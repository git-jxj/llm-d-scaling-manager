package learnreplay

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// decodeTTFTRun is what one replay of a staircase recording says of the
// decode engine's TTFT S1 holds: the figure on each cycle's slo-ttft-variant
// line, and the replay's outputs.
type decodeTTFTRun struct {
	figures []float64
	outputs map[string][]byte
}

// replayDecodeTTFT replays a staircase recording with
// slo.SwitchTTFTLowLoadDecodeTTFT set to on.
func replayDecodeTTFT(t *testing.T, raw []byte, on bool) decodeTTFTRun {
	t.Helper()
	var run decodeTTFTRun
	logger := funcr.NewJSON(func(obj string) {
		if !strings.Contains(obj, `"msg":"slo-ttft-variant"`) {
			return
		}
		v, err := strconv.ParseFloat(logField(t, obj, "decodeTTFT"), 64)
		require.NoError(t, err)
		run.figures = append(run.figures, v)
	}, funcr.Options{Verbosity: 10})
	rep, err := Replay(logr.NewContext(context.Background(), logger), raw, Options{Mode: Normal,
		Switches: map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTLowLoadDecodeTTFT: on}}})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	run.outputs, err = Outputs(rep.Recording)
	require.NoError(t, err)
	return run
}

// queueFloor is the decode TTFT, in seconds, above which a figure is a
// queue's wait and not the engine's: at low load the staircases' decode
// engines read about 0.12 s, under 0.3 s calm.
const queueFloor = 1.0

// lowLoadStaircase is, for each staircase recording, the cycles S1 logs, how
// many of them hold a decode TTFT above queueFloor with the switch off and
// on, the largest figure either way, and the cycles the switch changes with
// the largest figure it holds on them, as measured.
//
// b-off and c-on end in an overload. Off, the figure climbs from the first
// queued cycle (250 and 249) to 17-21 s and 6-12 s. On, the calm figure,
// 0.154 s and 0.228 s, is held, on 23 cycles each: prefill.HeldDecodeAge
// after the last cycle whose window held a calm reading. The overload then
// outlasts the hold to the recording's end, and the window's largest stands
// again, as off: 17 and 20 cycles. a-off and b-on never queue on decode.
var lowLoadStaircase = []struct {
	fixture             string
	cycles              int
	queuedOff, queuedOn int
	largestOff          float64
	largestOn           float64
	changed             int
	heldLargest         float64
}{
	{"ttft-staircase-a-off", 128, 0, 0, 0.040, 0.040, 0, 0},
	{"ttft-staircase-b-off", 289, 38, 17, 20.614, 20.614, 23, 0.154},
	{"ttft-staircase-b-on", 289, 0, 0, 0.205, 0.205, 0, 0},
	{"ttft-staircase-c-on", 292, 41, 20, 11.961, 11.961, 23, 0.228},
}

// On the four staircase recordings S1's decode engine TTFT reads the queue's
// wait after an overload with the switch off, and a calm figure with it on.
// No output of the replay moves: the figure prices only a fleet with no
// decode line, and no cycle of these reaches a priced fleet
// (TestGoldens_TTFTStaircaseHoldsNoS2GateCycle).
func TestGoldens_TTFTLowLoadDecodeTTFTOnTheStaircases(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	for _, c := range lowLoadStaircase {
		raw := staircaseRecording(t, c.fixture)
		off, on := replayDecodeTTFT(t, raw, false), replayDecodeTTFT(t, raw, true)
		require.Len(t, on.figures, len(off.figures), c.fixture)
		var queuedOff, queuedOn, changed int
		var largestOff, largestOn, heldLargest float64
		for i := range off.figures {
			if off.figures[i] > queueFloor {
				queuedOff++
			}
			if on.figures[i] > queueFloor {
				queuedOn++
			}
			largestOff, largestOn = max(largestOff, off.figures[i]), max(largestOn, on.figures[i])
			if on.figures[i] != off.figures[i] {
				changed++
				heldLargest = max(heldLargest, on.figures[i])
			}
			assert.LessOrEqual(t, on.figures[i], off.figures[i], "%s cycle %d: on never reads above off", c.fixture, i)
		}
		t.Logf("%s: %d cycles; above %.1f s: %d off, %d on; largest %.3f s off, %.3f s on; %d cycles changed, to at most %.3f s",
			c.fixture, len(off.figures), queueFloor, queuedOff, queuedOn, largestOff, largestOn, changed, heldLargest)
		assert.Equal(t, c.cycles, len(off.figures), c.fixture)
		assert.Equal(t, c.queuedOff, queuedOff, c.fixture)
		assert.Equal(t, c.queuedOn, queuedOn, c.fixture)
		assert.InDelta(t, c.largestOff, largestOff, 5e-4, c.fixture)
		assert.InDelta(t, c.largestOn, largestOn, 5e-4, c.fixture)
		assert.Equal(t, c.changed, changed, c.fixture)
		assert.InDelta(t, c.heldLargest, heldLargest, 5e-4, c.fixture)
		for f, b := range off.outputs {
			d, err := compareOutput(f, b, on.outputs[f])
			require.NoError(t, err)
			assert.Nil(t, d, "%s: %s moved", c.fixture, f)
		}
	}
}
