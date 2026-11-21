package whatifapi

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relaxation time of the queueing review's table (issue #305): one
// Qwen3-32B 8k prefill pod, S = 0.47 s, at rho 0.8, 0.85, 0.9 and 0.95,
// with the measured spread (cv 0.06) and the assumed one (cv 2), random
// arrivals. Pooled over N pods it is N times shorter.
func TestRelaxationSeconds(t *testing.T) {
	const s = 0.47
	knee := 1 / s
	for _, c := range []struct {
		rho, cv, want float64
	}{
		{0.8, 0.06, 19}, {0.85, 0.06, 36}, {0.9, 0.06, 85}, {0.95, 0.06, 358},
		{0.8, 2, 94}, {0.85, 2, 178}, {0.9, 2, 423}, {0.95, 2, 1786},
	} {
		got := relaxationSeconds(c.rho*knee, knee, 1, 1, c.cv)
		assert.InDelta(t, c.want, got, 0.02*c.want+0.5, "rho %.2f cv %.2f", c.rho, c.cv)
	}
	// Two pods, pooled: the knee doubles, a pod's service time stays.
	assert.InDelta(t, relaxationSeconds(0.9*knee, knee, 1, 1, 0.06)/2, relaxationSeconds(0.9*2*knee, 2*knee, 2, 1, 0.06), 1e-9)
	// Bursty arrivals (Ca^2 4) relax slower.
	assert.InDelta(t, relaxationSeconds(0.9*knee, knee, 1, 1, 0.06)*(4+0.0036)/(1+0.0036), relaxationSeconds(0.9*knee, knee, 1, 4, 0.06), 1e-9)
	assert.True(t, math.IsInf(relaxationSeconds(knee, knee, 1, 1, 0.06), 1), "at the knee it never settles")
	assert.Zero(t, relaxationSeconds(1, 0, 2, 1, 0.06), "no knee priced")
	assert.Zero(t, relaxationSeconds(0, knee, 2, 1, 0.06), "no traffic")
	// A 60 s window takes a relaxation time of up to 12 s: one pod at rho
	// 0.75 with the measured spread settles, rho 0.85 does not; two pods at
	// 0.8 do, and with the assumed spread they do not.
	assert.True(t, settles(relaxationSeconds(0.75*knee, knee, 1, 1, 0.06), 60))
	assert.False(t, settles(relaxationSeconds(0.85*knee, knee, 1, 1, 0.06), 60))
	assert.True(t, settles(relaxationSeconds(0.8*2*knee, 2*knee, 2, 1, 0.06), 60))
	assert.False(t, settles(relaxationSeconds(0.8*2*knee, 2*knee, 2, 1, 2), 60))
}

// A fluid backlog: what runs past the knee in one step is queued at the
// next, drains at the knee less the rate, and adds its wait over the step's
// measurement window, the last part of the step.
func TestFluidBacklog(t *testing.T) {
	var f fluid
	m, most := f.step(3, 4, 300, 300)
	assert.Zero(t, m, "under the knee with nothing queued")
	assert.Zero(t, most)
	// 5 r/s against 4 for 300 s queues 300 requests; nothing was carried in.
	m, _ = f.step(5, 4, 300, 300)
	assert.Zero(t, m)
	assert.InDelta(t, 300, f.q, 1e-9)
	// At 2 r/s it drains at 2 r/s, empty at 150 s. Over the whole step the
	// mean backlog is 300 x 150 / 600 = 75 requests, 18.75 s at 4 r/s; the
	// most, at the step's start, 75 s.
	g := f
	m, most = g.step(2, 4, 300, 300)
	assert.InDelta(t, 300, g.carried, 1e-9)
	assert.InDelta(t, 75.0/4, m, 1e-9)
	assert.InDelta(t, 75, most, 1e-9)
	assert.Zero(t, g.q)
	// Measured over the step's last 60 s, the backlog is already gone.
	g = f
	m, most = g.step(2, 4, 300, 60)
	assert.Zero(t, m)
	assert.Zero(t, most)
	// Over its last 200 s (100 to 300 s) it holds 100 requests at 100 s and
	// empties at 150 s: a mean of 100 x 50 / 2 / 200 = 12.5, over 4 r/s.
	g = f
	m, most = g.step(2, 4, 300, 200)
	assert.InDelta(t, 12.5/4, m, 1e-9)
	assert.InDelta(t, 25, most, 1e-9)
	// Past the knee the backlog grows through the step: over the last 60 s
	// of 100 + 0.5 t, the mean is 100 + 0.5 x 270 and the most at the end.
	f = fluid{q: 100}
	m, most = f.step(4.5, 4, 300, 60)
	assert.InDelta(t, (100+0.5*270)/4, m, 1e-9)
	assert.InDelta(t, (100+0.5*300)/4, most, 1e-9)
	// A step with no requests drains at the knee last served.
	f = fluid{q: 300, mu: 4}
	f.idle(60)
	assert.InDelta(t, 60, f.q, 1e-9)
	// A rate that is no number breaks the chain.
	f = fluid{q: 100, mu: 4}
	m, _ = f.step(math.NaN(), 4, 300, 60)
	assert.Zero(t, m)
	assert.Zero(t, f.q)
	f.reset()
	assert.Zero(t, f.q)
	m, _ = backlogWait(10, 1, 0, 300, 60)
	assert.Zero(t, m, "no knee")
}

// A step whose prefill queue does not settle in its window has its TTFT
// unscored (unsettled), its TPOT scored.
func TestScoreStepUnsettled(t *testing.T) {
	one := 1.0
	st := BacktestStep{Unsettled: true, TTFT: []BacktestPoint{predictedPoint(0.9, Figure{Value: &one}, nil)},
		TPOT: []BacktestPoint{predictedPoint(0.9, Figure{Value: &one}, nil)}}
	m := measured{ttft: []*float64{&one}, tpot: []*float64{&one}, ttftWhy: []string{""}, tpotWhy: []string{""}, requests: 1000, tpotRequests: 1000}
	scoreStep(&st, m, nil)
	assert.Equal(t, UnscoredUnsettled, st.TTFT[0].Unscored)
	assert.Nil(t, st.TTFT[0].Ratio)
	require.NotNil(t, st.TPOT[0].Ratio)
	assert.InDelta(t, 1, *st.TPOT[0].Ratio, 1e-12)
}
