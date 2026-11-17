package whatif

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// The calibration envelope: where the planner's figures were measured
// against a client, and how far off they read there
// (docs/reference/capacity-planner-accuracy.md). Each row is one kind of
// figure on one shape class, from one measured run: the region the run
// covered and the signed error it read over it. A figure inside a row's
// region carries that row's error; any other figure reads OUTSIDE VALIDATED
// REGION, with what put it outside. The rows are data (envelope.json), so
// that a new run adds rows without code.

// FigureKind is a kind of figure the planner prints.
type FigureKind string

// The kinds of figure. A kind no row names was never measured, and every
// figure of it is outside.
const (
	// FigureTTFT is the fleet's end-to-end TTFT at a rate, at a statistic.
	// Its knee share is the rate over the fleet's lower knee.
	FigureTTFT FigureKind = "ttft"
	// FigureMeanTPOT is decode's mean TPOT at a rate. Its knee share is the
	// rate over decode's knee.
	FigureMeanTPOT FigureKind = "mean-tpot"
	// FigurePercentileTPOT is decode's TPOT at a per-request percentile.
	FigurePercentileTPOT FigureKind = "percentile-tpot"
	// FigurePrefillKnee is the prefill knee: the rate the prefill pods
	// saturate at.
	FigurePrefillKnee FigureKind = "prefill-knee"
	// FigureDecodeKnee is decode's knee.
	FigureDecodeKnee FigureKind = "decode-knee"
	// FigureRateAtTTFT is the rate at a TTFT target. Its knee share is the
	// rate over the fleet's lower knee.
	FigureRateAtTTFT FigureKind = "rate-at-ttft"
	// FigureRateAtTPOT is decode's rate at a TPOT target.
	FigureRateAtTPOT FigureKind = "rate-at-tpot"
	// FigurePrefillLatency is the prefill pods' own latency, without the
	// decode side (TTFTParts.OwnPrefill and EngineWait): a client measures
	// TTFT end to end only.
	FigurePrefillLatency FigureKind = "prefill-latency"
)

// The topologies a class names.
const (
	// TopologyPD is a fleet of prefill and decode pods.
	TopologyPD = "pd"
	// TopologyAggregated is one pod doing both.
	TopologyAggregated = "aggregated"
)

// The flow-control states a class names.
const (
	FlowControlNameOff = "off"
	FlowControlNameOn  = "on"
)

// FigureClass is what a figure was priced at, as the envelope's regions are
// drawn: the fleet, its accelerator and its traffic. A zero count or length
// is unknown, and an unknown value is outside any region that bounds it.
type FigureClass struct {
	Topology        string
	Accelerator     string
	PrefillReplicas int
	DecodeReplicas  int
	PromptTokens    float64
	OutputTokens    float64
	// ArrivalCV2 is the arrivals' Ca^2; 0 reads as Poisson (1).
	ArrivalCV2  float64
	FlowControl string
}

// FleetClass is the class of a P/D fleet's figures, less the accelerator,
// which the fleet does not carry.
func FleetClass(fl TTFTFleet) FigureClass {
	c := FigureClass{Topology: TopologyPD, PrefillReplicas: fl.Prefill.Replicas, PromptTokens: fl.Prefill.PromptTokens,
		ArrivalCV2: fl.Prefill.ArrivalCV2, FlowControl: FlowControlNameOff}
	if fl.FlowControl.State == FlowControlOn {
		c.FlowControl = FlowControlNameOn
	}
	if fl.Decode != nil {
		c.DecodeReplicas, c.OutputTokens = fl.Decode.Replicas, fl.Decode.OutputTokens
	}
	return c
}

// Figure is one figure to judge.
type Figure struct {
	Kind FigureKind
	// Percentile is the statistic, q in (0, 1); 0 is the mean.
	Percentile float64
	Class      FigureClass
	// KneeShare is the figure's rate over its knee (FigureKind says which);
	// 0 where it has none.
	KneeShare float64
	// Flags are the figure's own: some put it outside every region
	// (Envelope.OutsideFlags).
	Flags []Flag
}

// Span is a closed range, Lo to Hi.
type Span struct {
	Lo float64 `json:"lo"`
	Hi float64 `json:"hi"`
}

// holds reports whether the span holds v.
func (s Span) holds(v float64) bool { return v >= s.Lo && v <= s.Hi }

// Region is where a row was measured. An empty list or a nil span bounds
// nothing.
type Region struct {
	Topology        string    `json:"topology"`
	Accelerators    []string  `json:"accelerators,omitempty"`
	PrefillReplicas []int     `json:"prefillReplicas,omitempty"`
	DecodeReplicas  []int     `json:"decodeReplicas,omitempty"`
	PromptTokens    *Span     `json:"promptTokens,omitempty"`
	OutputTokens    *Span     `json:"outputTokens,omitempty"`
	ArrivalCV2      *Span     `json:"arrivalCV2,omitempty"`
	FlowControl     []string  `json:"flowControl,omitempty"`
	Percentiles     []float64 `json:"percentiles,omitempty"`
	// MaxKneeShare is the highest knee share measured; 0 bounds nothing.
	MaxKneeShare float64 `json:"maxKneeShare,omitempty"`
}

// EnvelopeRow is one kind of figure on one shape class, from one run.
type EnvelopeRow struct {
	Figure FigureKind `json:"figure"`
	// Shape names the shape class in words; Source the run.
	Shape  string `json:"shape"`
	Source string `json:"source"`
	Region Region `json:"region"`
	// ErrorLo and ErrorHi are the signed error, the model's figure over the
	// measured one less 1: the 10th and the 90th percentile of the
	// readings, or, where a reading is a bracket (a knee, a rate at a
	// target), the lowest and the highest end of any. Nil is unbounded on
	// that side: a measured rate that was only bounded.
	ErrorLo *float64 `json:"errorLo,omitempty"`
	ErrorHi *float64 `json:"errorHi,omitempty"`
	// Readings is how many readings the error rests on.
	Readings int `json:"readings"`
}

// Envelope is the planner's measured accuracy.
type Envelope struct {
	Rows []EnvelopeRow `json:"rows"`
	// OutsideFlags are the flags that put a figure outside every region,
	// with what each says: the runs never priced them.
	OutsideFlags map[Flag]string `json:"outsideFlags"`
	// Never says, by kind, why a kind no row names has none.
	Never map[FigureKind]string `json:"never"`
}

//go:embed envelope.json
var envelopeJSON []byte

var defaultEnvelope = sync.OnceValues(func() (Envelope, error) { return ParseEnvelope(envelopeJSON) })

// DefaultEnvelope is the envelope the repository's measured runs draw
// (envelope.json). It panics on a file that does not parse, which a test
// holds.
func DefaultEnvelope() Envelope {
	e, err := defaultEnvelope()
	if err != nil {
		panic(err)
	}
	return e
}

// ParseEnvelope reads an envelope, refusing a row that judges nothing.
func ParseEnvelope(raw []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return Envelope{}, fmt.Errorf("envelope: %w", err)
	}
	for i, r := range e.Rows {
		switch {
		case r.Figure == "" || r.Source == "" || r.Shape == "":
			return Envelope{}, fmt.Errorf("envelope: row %d: figure, shape and source are required", i)
		case r.Region.Topology == "":
			return Envelope{}, fmt.Errorf("envelope: row %d: no topology", i)
		case r.Readings < 1:
			return Envelope{}, fmt.Errorf("envelope: row %d: no readings", i)
		case r.ErrorLo == nil && r.ErrorHi == nil:
			return Envelope{}, fmt.Errorf("envelope: row %d: no error on either side", i)
		}
	}
	return e, nil
}

// Validation is what the envelope says of one figure: the row whose region
// holds it, or why none does.
type Validation struct {
	Row *EnvelopeRow
	// Outside is why no row holds the figure; "" when one does.
	Outside string
}

// Validated reports whether a row holds the figure.
func (v Validation) Validated() bool { return v.Row != nil }

// String is the figure's label: "validated: error -2..+16% (q32 staircase)"
// or "OUTSIDE VALIDATED REGION (why)".
func (v Validation) String() string {
	if v.Row == nil {
		return "OUTSIDE VALIDATED REGION (" + v.Outside + ")"
	}
	return "validated: error " + ErrorText(v.Row.ErrorLo, v.Row.ErrorHi) + " (" + v.Row.Source + ")"
}

// ErrorText prints a signed error range in percent: "-2..+16%", or one side
// of it, "at most -6%", where the other is unbounded.
func ErrorText(lo, hi *float64) string {
	pct := func(x float64) string {
		r := math.Round(100 * x)
		if r == 0 {
			r = 0 // no "-0"
		}
		return fmt.Sprintf("%+.0f", r)
	}
	switch {
	case lo != nil && hi != nil:
		return pct(*lo) + ".." + pct(*hi) + "%"
	case hi != nil:
		return "at most " + pct(*hi) + "%"
	case lo != nil:
		return "at least " + pct(*lo) + "%"
	}
	return "unbounded"
}

// Judge says whether a row holds f, and why not when none does: the reason
// a flag gives, or the first miss of the row it misses least.
func (e Envelope) Judge(f Figure) Validation {
	if f.Class.Topology == TopologyAggregated {
		return Validation{Outside: "aggregated variants: never measured"}
	}
	for _, fl := range f.Flags {
		if why, ok := e.OutsideFlags[fl]; ok {
			return Validation{Outside: why}
		}
	}
	best, bestMisses := "", math.MaxInt
	for i := range e.Rows {
		r := &e.Rows[i]
		if r.Figure != f.Kind {
			continue
		}
		misses := r.Region.misses(f)
		if len(misses) == 0 {
			return Validation{Row: r}
		}
		if len(misses) < bestMisses {
			best, bestMisses = misses[0], len(misses)
		}
	}
	if best == "" {
		if why, ok := e.Never[f.Kind]; ok {
			return Validation{Outside: why}
		}
		return Validation{Outside: string(f.Kind) + ": never measured"}
	}
	return Validation{Outside: best}
}

// misses is what of f the region does not hold, in words, in a fixed order.
func (g Region) misses(f Figure) []string {
	var out []string
	c := f.Class
	if c.Topology != g.Topology {
		out = append(out, fmt.Sprintf("topology %s, measured %s", orUnknown(c.Topology), g.Topology))
	}
	if len(g.Percentiles) > 0 && !slices.Contains(g.Percentiles, f.Percentile) {
		out = append(out, fmt.Sprintf("%s, measured %s", statisticText(f.Percentile), statisticsText(g.Percentiles)))
	}
	if len(g.Accelerators) > 0 && !slices.Contains(g.Accelerators, c.Accelerator) {
		out = append(out, fmt.Sprintf("accelerator %s, measured %s", orUnknown(c.Accelerator), strings.Join(g.Accelerators, ", ")))
	}
	if len(g.PrefillReplicas) > 0 && !slices.Contains(g.PrefillReplicas, c.PrefillReplicas) {
		out = append(out, fmt.Sprintf("%s prefill pods, measured %s", countText(c.PrefillReplicas), intsText(g.PrefillReplicas)))
	}
	if len(g.DecodeReplicas) > 0 && !slices.Contains(g.DecodeReplicas, c.DecodeReplicas) {
		out = append(out, fmt.Sprintf("%s decode pods, measured %s", countText(c.DecodeReplicas), intsText(g.DecodeReplicas)))
	}
	if s := g.PromptTokens; s != nil && !(c.PromptTokens > 0 && s.holds(c.PromptTokens)) {
		out = append(out, fmt.Sprintf("prompt %s tokens, measured %s", amountText(c.PromptTokens), spanText(*s, amountText)))
	}
	if s := g.OutputTokens; s != nil && !(c.OutputTokens > 0 && s.holds(c.OutputTokens)) {
		out = append(out, fmt.Sprintf("output %s tokens, measured %s", amountText(c.OutputTokens), spanText(*s, amountText)))
	}
	ca2 := c.ArrivalCV2
	if ca2 == 0 {
		ca2 = 1
	}
	if s := g.ArrivalCV2; s != nil && !s.holds(ca2) {
		out = append(out, fmt.Sprintf("arrivals Ca^2 %s, measured %s", ratioText(ca2), spanText(*s, ratioText)))
	}
	if len(g.FlowControl) > 0 && !slices.Contains(g.FlowControl, c.FlowControl) {
		out = append(out, fmt.Sprintf("flow control %s, measured %s", orUnknown(c.FlowControl), strings.Join(g.FlowControl, ", ")))
	}
	if g.MaxKneeShare > 0 && f.KneeShare > g.MaxKneeShare {
		out = append(out, fmt.Sprintf("at %.2f of the knee, measured up to %.2f", f.KneeShare, g.MaxKneeShare))
	}
	return out
}

// unknownText stands for a class value no input gave.
const unknownText = "unknown"

func orUnknown(s string) string {
	if s == "" {
		return unknownText
	}
	return s
}

func countText(n int) string {
	if n <= 0 {
		return unknownText
	}
	return strconv.Itoa(n)
}

func intsText(xs []int) string {
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = strconv.Itoa(x)
	}
	return strings.Join(parts, ", ")
}

func amountText(x float64) string {
	if x <= 0 {
		return unknownText
	}
	return strconv.FormatFloat(math.Round(x), 'f', -1, 64)
}

func ratioText(x float64) string { return strconv.FormatFloat(x, 'g', 3, 64) }

func spanText(s Span, text func(float64) string) string {
	return text(s.Lo) + " to " + text(s.Hi)
}

// statisticText names a statistic: "mean", or "p90".
func statisticText(q float64) string {
	if q == 0 {
		return "mean"
	}
	return "p" + strconv.FormatFloat(math.Round(1e6*q)/1e4, 'f', -1, 64)
}

func statisticsText(qs []float64) string {
	parts := make([]string, len(qs))
	for i, q := range qs {
		parts[i] = statisticText(q)
	}
	return strings.Join(parts, ", ")
}
