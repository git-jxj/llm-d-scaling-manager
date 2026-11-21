package steadystate

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// packingOf is the analyzer's packing for the variants given, nil ones
// left out.
func packingOf(packing map[string]*domain.DecodePacking) map[string]domain.DecodePacking {
	out := map[string]domain.DecodePacking{}
	for v, p := range packing {
		if p != nil {
			out[v] = *p
		}
	}
	return out
}

func packingVariants(names ...string) map[string]*llmdvariant.Variant {
	out := map[string]*llmdvariant.Variant{}
	for _, n := range names {
		out["ns/"+n] = &llmdvariant.Variant{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "ns"}}
	}
	return out
}

var (
	packingHeld     = &domain.DecodePacking{Bound: domain.DecodePackingBoundITL, Reason: "slo-tpot", K2Unbounded: 300, K2: 200}
	packingUnder    = &domain.DecodePacking{Bound: domain.DecodePackingBoundNone, Reason: domain.DecodePackingNotBinding, K2Unbounded: 100, K2: 100}
	packingBorrowed = &domain.DecodePacking{Bound: domain.DecodePackingBoundNone, Reason: "borrowed-line", K2Unbounded: 300, K2: 300}
	packingNoLine   = &domain.DecodePacking{Bound: domain.DecodePackingBoundNone, Reason: "no-line", K2Unbounded: 300, K2: 300}
)

// The series follow the saturation result: published for a variant that
// carries packing, and deleted when it stops.
// A model no longer reconciled loses them, and so does every model on a
// cycle that analyzes none.
func TestReportDecodePacking_Series(t *testing.T) {
	e, registry := analyzerSeriesEngine(t)
	now := time.Unix(1_700_000_000, 0)
	vars := packingVariants("d1", "d2")
	e.reportDecodePacking("ns", "m", packingOf(map[string]*domain.DecodePacking{"d1": packingHeld, "d2": packingUnder}), vars, now)
	assert.ElementsMatch(t, []string{"d1", "d2"},
		seriesLabels(t, registry, constants.LLMSMDecodePackingBound, constants.LabelVariantName))

	e.reportDecodePacking("ns", "m", packingOf(map[string]*domain.DecodePacking{"d1": packingHeld, "d2": nil}), vars, now)
	assert.ElementsMatch(t, []string{"d1"},
		seriesLabels(t, registry, constants.LLMSMDecodePackingBound, constants.LabelVariantName))
	assert.ElementsMatch(t, []string{"d1", "d1"},
		seriesLabels(t, registry, constants.LLMSMDecodePackingK2Tokens, constants.LabelVariantName))

	e.reportDecodePacking("ns", "m", packingOf(map[string]*domain.DecodePacking{"d1": packingHeld}), vars, now)
	e.reportDecodePacking("ns", "m2", packingOf(map[string]*domain.DecodePacking{"d2": packingHeld}), vars, now)
	e.pruneAnalyzerSeries(map[string]bool{"ns/m2": true})
	assert.ElementsMatch(t, []string{"d2"},
		seriesLabels(t, registry, constants.LLMSMDecodePackingBound, constants.LabelVariantName))
	e.evictAllAnalyzerSeries()
	assert.Empty(t, seriesLabels(t, registry, constants.LLMSMDecodePackingBound, constants.LabelVariantName))
	assert.Empty(t, e.decodePacking)
}

// The Warning is recorded once a TPOT target has bounded nothing for
// decodePackingInertHold, once per episode: not again while it stays inert,
// again after the bound applied in between, and from the start when the
// reason changes. A bound in force that holds nothing is not inert.
func TestReportDecodePacking_InertEvent(t *testing.T) {
	e, _ := analyzerSeriesEngine(t)
	rec := record.NewFakeRecorder(20)
	e.Recorder = rec
	vars := packingVariants("d1")
	t0 := time.Unix(1_700_000_000, 0)
	cycle := func(at time.Duration, p *domain.DecodePacking) []string {
		e.reportDecodePacking("ns", "m", packingOf(map[string]*domain.DecodePacking{"d1": p}), vars, t0.Add(at))
		return drainEvents(rec)
	}
	assert.Empty(t, cycle(0, packingNoLine))
	assert.Empty(t, cycle(decodePackingInertHold-time.Second, packingNoLine), "inside the hold")
	ev := cycle(decodePackingInertHold, packingNoLine)
	require.Len(t, ev, 1)
	assert.True(t, strings.HasPrefix(ev[0], "Warning "+constants.K8SEventDecodePackingBoundInert+" "), ev[0])
	assert.Contains(t, ev[0], "(no-line)")
	assert.Contains(t, ev[0], "minReplicaCount")
	assert.Empty(t, cycle(2*decodePackingInertHold, packingNoLine), "once per episode")

	// The reason changes: a new episode, its own hold.
	assert.Empty(t, cycle(2*decodePackingInertHold+time.Minute, packingBorrowed))
	ev = cycle(3*decodePackingInertHold+time.Minute, packingBorrowed)
	require.Len(t, ev, 1)
	assert.Contains(t, ev[0], "(borrowed-line)")

	// Bound in force, holding nothing: not inert, and it ends the episode.
	assert.Empty(t, cycle(4*decodePackingInertHold, packingUnder))
	assert.Empty(t, cycle(5*decodePackingInertHold, packingUnder))
	assert.Empty(t, cycle(5*decodePackingInertHold+time.Minute, packingBorrowed))
	require.Len(t, cycle(6*decodePackingInertHold+time.Minute, packingBorrowed), 1, "a new episode is reported")

	// Held by the target: never a Warning.
	assert.Empty(t, cycle(7*decodePackingInertHold, packingHeld))
	assert.Empty(t, cycle(9*decodePackingInertHold, packingHeld))
}

// Every reason the analyzer can give an inert bound has its own message,
// not the fallback.
func TestDecodePackingInertMessage(t *testing.T) {
	for _, r := range saturation.DecodePackingInertReasons() {
		assert.NotContains(t, decodePackingInertMessage(r), "k2-itl-bound line at --v=3", r)
	}
	assert.Contains(t, decodePackingInertMessage("something-new"), "k2-itl-bound line at --v=3")
}
