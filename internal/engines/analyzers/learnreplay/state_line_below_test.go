package learnreplay

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/itl"
)

// belowRow is one pod of variant at fill k reading a mean ITL of itl.
func belowRow(pod, variant string, k, itl float64) domain.ReplicaMetrics {
	return domain.ReplicaMetrics{PodName: pod, VariantName: variant, Ready: true,
		KvUsageMean: k, KvUsageMeanKnown: true, AvgITL: itl, AvgITLKnown: true}
}

// belowInput is one cycle: two decode pods at a fill of 0.05 reading itl and
// itl2, and the rows the floor must not read: a prefill pod, a decode pod at
// no fill, one at the band's bottom, one with no ITL, one reading +Inf, and
// one not ready, each reading a mean ITL far under the decode pods'.
func belowInput(itl, itl2 float64) domain.AnalyzerInput {
	notReady := belowRow("d-notready", "dec", 0.05, 0.001)
	notReady.Ready = false
	noITL := belowRow("d-noitl", "dec", 0.05, 0.001)
	noITL.AvgITLKnown = false
	return domain.AnalyzerInput{
		Config:        &config.ScalingPolicy{},
		VariantStates: []domain.VariantReplicaState{{VariantName: "dec", Role: "decode"}, {VariantName: "pf", Role: "prefill"}},
		ReplicaMetrics: []domain.ReplicaMetrics{
			belowRow("d-a", "dec", 0.05, itl),
			belowRow("d-b", "dec", 0.05, itl2),
			belowRow("p-a", "pf", 0.05, 0.001),
			belowRow("d-zero", "dec", 0, 0.001),
			belowRow("d-bottom", "dec", 0.15, 0.001),
			belowRow("d-inf", "dec", 0.05, math.Inf(1)),
			notReady,
			noITL,
		},
	}
}

// The floor below the band is a low percentile of the decode pods' readings
// off a ramp: a prefill pod, a fill of 0, a fill at the band's bottom, a
// missing or infinite ITL and a pod not ready are not read, and neither is a
// pod's first two minutes.
func TestLineSamples_TheFloorBelowTheBandReadsOnlyDecodeOffARamp(t *testing.T) {
	l := noteCycles(lineT0, repeated(30, belowInput(0.011, 0.012))...)
	// Cycles 9 to 30 are off a ramp: 22 cycles of two pods.
	assert.Len(t, l.below["dec"], 44)
	assert.Empty(t, l.below["pf"])
	assert.InDelta(t, 0.011, l.leastBelowBand("dec"), 1e-15)
}

// One window that read low does not set the floor: the 10th percentile passes
// over it where the least would not.
func TestLineSamples_OneLowWindowDoesNotSetTheFloor(t *testing.T) {
	in := repeated(30, belowInput(0.011, 0.012))
	in[20] = belowInput(0.001, 0.012)
	l := noteCycles(lineT0, in...)
	assert.InDelta(t, 0.001, slicesMin(l.below["dec"]), 1e-15, "the outlier was read")
	assert.InDelta(t, 0.011, l.leastBelowBand("dec"), 1e-15, "and passed over")
}

// Too few readings below the band is no floor, and the line is then not read
// below the band at all (whatif.LowestFill).
func TestLineSamples_TooFewReadingsBelowTheBandIsNoFloor(t *testing.T) {
	// Cycles 9 to 12 are off a ramp: 8 readings, under itl.BelowBandMinReadings.
	l := noteCycles(lineT0, repeated(12, belowInput(0.011, 0.012))...)
	assert.Len(t, l.below["dec"], 8)
	assert.Zero(t, l.leastBelowBand("dec"))
	assert.Zero(t, itl.BelowBandFloor(make([]float64, itl.BelowBandMinReadings-1)))
}

// The floor is read from the latest itl.BelowBandWindow readings, so a floor read
// under an older engine ages out.
func TestLineSamples_TheFloorAgesOut(t *testing.T) {
	in := repeated(30, belowInput(0.005, 0.005))
	in = append(in, repeated(itl.BelowBandWindow/2+10, belowInput(0.011, 0.012))...)
	l := noteCycles(lineT0, in...)
	assert.Len(t, l.below["dec"], itl.BelowBandWindow)
	assert.InDelta(t, 0.011, l.leastBelowBand("dec"), 1e-15)
}

func slicesMin(xs []float64) float64 {
	m := math.Inf(1)
	for _, x := range xs {
		m = min(m, x)
	}
	return m
}
