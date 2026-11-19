package learnreplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"

	clocktesting "k8s.io/utils/clock/testing"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/throughput"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/learnrec"
)

// Mismatch is one place a replay diverged from its recording.
type Mismatch struct {
	// Seq and Kind are the recorded event the divergence is at (Seq 0 for a
	// recording-wide one, such as a count of events).
	Seq  uint64
	Kind string
	// What says what diverged: "store" for the check before an Analyze,
	// "line" for an event the replay recorded differently, "evict" for a
	// store eviction that removed another count, "router" for a router asked
	// when the run did not ask it, or the reverse, "settle-no-cycles" for
	// settle events the replay cannot retain by (settleNoCycles, reported
	// only).
	What string
	// Detail is a readable account: the differing fields, or the two lines
	// around their first difference.
	Detail string
	// Fatal is whether the mismatch fails the replay in its mode. An evict
	// count is a check only a Fidelity replay is held to (2.4): elsewhere
	// LearnedAt follows the replay's own writes, and it is reported only.
	Fatal bool
}

// String is the mismatch on one line: where, what, and the detail.
func (m Mismatch) String() string {
	return fmt.Sprintf("seq %d %s: %s: %s", m.Seq, m.Kind, m.What, m.Detail)
}

// Report is what a replay did.
type Report struct {
	// Events is how many recorded events were read, the start included, and
	// Applied how many the replay called (check events are not applied).
	Events, Applied int
	// Analyzes counts the Analyze calls replayed, per kind.
	Analyzes map[string]int
	// Ignored counts, per kind, the events of owners this replay does not
	// drive (the engine-level layer ev-shindin/llm-scaling-manager#345 adds to the same recording, say):
	// skipped, and left out of the line comparison.
	Ignored map[string]int
	// TrimmedChecks is how many check events at the recording's end were
	// left out: a cap or a crash cut off the Analyze they belonged to.
	TrimmedChecks int
	// TornTail is a last line cut off mid-write, dropped (learnrec.Read).
	TornTail string
	// StoreChecks is how many store checks ran (one per recorded variant
	// record, before each Analyze that reads the store).
	StoreChecks int
	// Recording is the replay's own recording.
	Recording []byte
	// Mismatches is every divergence found, in sequence order.
	Mismatches []Mismatch
	// Paths is how often each learner path ran in the replay, by
	// saturation.PathNames, the settle tracker's verdicts, slo.PathNames
	// when the run built the slo analyzer, throughput.PathNames when it
	// built the throughput analyzer, and the engine's paths
	// (analyzer-learner.md, 2.5).
	Paths map[string]int64
}

// Failed is the mismatches that fail the replay in its mode.
func (r *Report) Failed() []Mismatch {
	var out []Mismatch
	for _, m := range r.Mismatches {
		if m.Fatal {
			out = append(out, m)
		}
	}
	return out
}

// replayer holds one replay's analyzers and their shared clock.
type replayer struct {
	opts   Options
	clock  *clocktesting.FakePassiveClock
	store  *capacity.Store
	sat    *saturation.SaturationAnalyzer
	slo    *slo.Analyzer
	router *recordedRouter
	tp     *throughput.ThroughputAnalyzer
	rep    *Report
	// engine replays the engine events, nil when the replay has none; and
	// results is each analyzer's last replayed result per model, for it.
	engine  EngineReplayer
	results map[string]*domain.AnalyzerResult
	// policies is each model's last saturation Analyze's policy, for it.
	policies map[string]*config.ScalingPolicy
	// rec is the replay's own recorder.
	rec *learnrec.Recorder
	// settle is the replay's settle tracker (settle.go).
	settle *settleReplay
}

// owners is the kinds' owners a replay drives, by the prefix of their kind.
var owners = []string{"saturation.", "slo.", "throughput.", "store.", "settle."}

// driven reports whether a replay applies, or checks, events of kind.
func (r *replayer) driven(kind string) bool {
	if kind == learnrec.KindStart || r.engine != nil && r.engine.Owns(kind) {
		return true
	}
	for _, o := range owners {
		if strings.HasPrefix(kind, o) {
			return true
		}
	}
	return false
}

// Replay replays recording. An error is a recording that cannot be replayed
// at all (no start, an unknown event of an owner it drives, an event for an
// analyzer the run did not have, a cancelled ctx); a divergence is a
// Mismatch in the report. In Fidelity mode the replay's own recording is
// compared with the original event by event.
//
// In Generate mode recording is a capture instead (generate.go), and the
// report's Recording is the run generated from it.
func Replay(ctx context.Context, recording []byte, opts Options) (*Report, error) {
	if opts.Mode == Generate {
		return generate(ctx, recording, opts)
	}
	src := BytesSource(recording)
	// A recording that does not read fails before any event is applied.
	if err := check(src); err != nil {
		return nil, err
	}
	return replayFrom(ctx, src, opts)
}

// replayFrom is Replay of a recording that reads (check), streamed from src:
// only the events a Fidelity replay compares are held. With opts.stop the
// replay ends at the event it stops after, and the report counts only the
// events read until then.
func replayFrom(ctx context.Context, src Source, opts Options) (*Report, error) {
	rc, err := src()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	s := learnrec.NewScanner(rc)
	if !s.Scan() || s.Event().Kind != learnrec.KindStart {
		if err := s.Err(); err != nil {
			return nil, err
		}
		return nil, errors.New("learnreplay: the recording has no start line; a replay cannot build its analyzers without one")
	}
	first := s.Event()
	var start learnrec.Start
	if err := first.DecodeData(&start); err != nil {
		return nil, fmt.Errorf("learnreplay: start: %w", err)
	}
	var buf bytes.Buffer
	var sink io.Writer = &buf
	if opts.discardRecording {
		sink = io.Discard
	}
	rec := learnrec.New(sink, 0)
	r, err := build(start, first.Now, opts, rec)
	if err != nil {
		return nil, err
	}
	rec.Record(learnrec.KindStart, first.Now, start)
	r.rep.Events = 1

	// kept is the driven events, for a Fidelity replay's comparison.
	fidelity := opts.Mode == Fidelity
	var kept []learnrec.RawEvent
	if fidelity {
		kept = append(kept, first)
	}
	// Check events are written inside the Analyze they belong to, before it.
	// At the end of a recording, with no Analyze after them, a cap or a crash
	// cut that Analyze off; the replay cannot make them, so they go: pending
	// holds them until an event that is no check follows.
	var pending []learnrec.RawEvent
	stopped := false
	applyOne := func(ev learnrec.RawEvent) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("learnreplay: %w", err)
		}
		if err := r.apply(ctx, ev); err != nil {
			return fmt.Errorf("learnreplay: seq %d %s: %w", ev.Seq, ev.Kind, err)
		}
		stopped = opts.stop != nil && opts.stop(r, ev)
		return nil
	}
	for !stopped && s.Scan() {
		ev := s.Event()
		r.rep.Events++
		if !r.driven(ev.Kind) {
			r.rep.Ignored[ev.Kind]++
			continue
		}
		if fidelity {
			kept = append(kept, ev)
		}
		if checkKinds[ev.Kind] {
			pending = append(pending, ev)
			continue
		}
		for _, p := range append(pending, ev) {
			if err := applyOne(p); err != nil {
				return nil, err
			}
			if stopped {
				break
			}
		}
		pending = pending[:0]
	}
	if !stopped {
		if err := s.Err(); err != nil {
			return nil, err
		}
		r.rep.TrimmedChecks = len(pending)
		if fidelity {
			kept = kept[:len(kept)-len(pending)]
		}
	}
	r.rep.TornTail = s.TornTail()
	if err := rec.Err(); err != nil {
		return nil, fmt.Errorf("learnreplay: the replay's own recording failed: %w", err)
	}
	if !opts.discardRecording {
		r.rep.Recording = buf.Bytes()
	}
	r.rep.Paths = r.sat.PathCounts()
	maps.Copy(r.rep.Paths, r.settle.pathCounts())
	if r.slo != nil {
		maps.Copy(r.rep.Paths, r.slo.PathCounts())
	}
	if r.tp != nil {
		maps.Copy(r.rep.Paths, r.tp.PathCounts())
	}
	if r.engine != nil {
		maps.Copy(r.rep.Paths, r.engine.PathCounts())
	}
	if fidelity {
		r.rep.Mismatches = append(r.rep.Mismatches, compareEvents(kept, r.rep.Recording)...)
	}
	return r.rep, nil
}
