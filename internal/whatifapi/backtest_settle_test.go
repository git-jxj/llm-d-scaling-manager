package whatifapi

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The relaxation time of the queueing review's table (issue #305): a
// Qwen3-32B 8k prefill, S = 0.47 s a pod, at rho 0.8, 0.85, 0.9 and 0.95,
// with the measured spread (cv 0.06) and the assumed one (cv 2).
func TestRelaxationSeconds(t *testing.T) {
	const s = 0.47
	knee := 2 / s // two pods
	for _, c := range []struct {
		rho, cv, want float64
	}{
		{0.8, 0.06, 19}, {0.85, 0.06, 36}, {0.9, 0.06, 85}, {0.95, 0.06, 358},
		{0.8, 2, 94}, {0.85, 2, 178}, {0.9, 2, 423}, {0.95, 2, 1786},
	} {
		got := relaxationSeconds(c.rho*knee, knee, 2, c.cv)
		assert.InDelta(t, c.want, got, 0.02*c.want+0.5, "rho %.2f cv %.2f", c.rho, c.cv)
	}
	assert.True(t, math.IsInf(relaxationSeconds(knee, knee, 2, 0.06), 1), "at the knee it never settles")
	assert.Zero(t, relaxationSeconds(1, 0, 2, 0.06), "no knee priced")
	assert.Zero(t, relaxationSeconds(0, knee, 2, 0.06), "no traffic")
}

// A fluid backlog: what runs past the knee in one step is queued at the
// next, drains at the knee less the rate, and adds its mean over mu to the
// wait.
func TestFluidBacklog(t *testing.T) {
	var f fluid
	assert.Zero(t, f.step(3, 4, 300), "under the knee with nothing queued")
	assert.Zero(t, f.q)
	// 5 r/s against 4 for 300 s queues 300 requests; nothing was carried in.
	assert.Zero(t, f.step(5, 4, 300))
	assert.InDelta(t, 300, f.q, 1e-9)
	// At 2 r/s it drains at 2 r/s, in 150 s of the 300: the mean backlog is
	// 300 x 150 / 600 = 75 requests, 18.75 s at 4 r/s.
	w := f.step(2, 4, 300)
	assert.InDelta(t, 300, f.carried, 1e-9)
	assert.InDelta(t, 75.0/4, w, 1e-9)
	assert.Zero(t, f.q)
	// Past the knee the backlog grows through the step.
	f = fluid{q: 100}
	assert.InDelta(t, (100+0.5*300/2)/4, f.step(4.5, 4, 300), 1e-9)
	f.reset()
	assert.Zero(t, f.q)
	assert.Zero(t, backlogWait(10, 1, 0, 300), "no knee")
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
