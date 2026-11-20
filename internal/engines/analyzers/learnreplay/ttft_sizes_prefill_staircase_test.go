package learnreplay

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"math"
	"slices"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/slo"
)

// sizesPrefillRun is what one replay of a staircase recording says of the
// slo analyzer's prefill ask: the TTFT reasons it published, the cycles it
// sized prefill on, and the largest count it asked for (role demand over
// per-replica rate, rounded up), with its outputs.
type sizesPrefillRun struct {
	reasons map[string]int
	sized   int
	largest float64
	outputs map[string][]byte
}

// sloAnalyzeKind is the kind of the slo analyzer's rows in a replay output.
const sloAnalyzeKind = "slo.Analyze"

// csvRows is a replay output's rows, header dropped.
func csvRows(t *testing.T, b []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(b)).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	return rows[1:]
}

// replaySizesPrefill replays a staircase recording with
// slo.SwitchTTFTSizesPrefill set to on.
func replaySizesPrefill(t *testing.T, raw []byte, on bool) sizesPrefillRun {
	t.Helper()
	rep, err := Replay(logr.NewContext(context.Background(), logr.Discard()), raw, Options{Mode: Normal,
		Switches: map[string]map[string]bool{PackageSLO: {slo.SwitchTTFTSizesPrefill: on}}})
	require.NoError(t, err)
	require.Empty(t, rep.Failed())
	run := sizesPrefillRun{reasons: map[string]int{}}
	run.outputs, err = Outputs(rep.Recording)
	require.NoError(t, err)

	demand := map[string]float64{}
	for _, row := range csvRows(t, run.outputs[OutputResults]) {
		if row[0] != sloAnalyzeKind {
			continue
		}
		var r struct {
			Result struct{ RoleDemand map[string]float64 }
		}
		require.NoError(t, json.Unmarshal([]byte(row[4]), &r))
		if d, ok := r.Result.RoleDemand[domain.RolePrefill]; ok {
			demand[row[1]+"|"+row[2]] = d
		}
	}
	for _, row := range csvRows(t, run.outputs[OutputVariantCapacities]) {
		if row[0] != sloAnalyzeKind {
			continue
		}
		var vc struct {
			Role               string
			PerReplicaCapacity float64
		}
		require.NoError(t, json.Unmarshal([]byte(row[4]), &vc))
		if vc.Role != domain.RolePrefill || !(vc.PerReplicaCapacity > 0) {
			continue
		}
		d, ok := demand[row[1]+"|"+row[2]]
		require.True(t, ok, "a sized prefill variant with no prefill demand at %s", row[1])
		run.sized++
		run.largest = max(run.largest, math.Ceil(d/vc.PerReplicaCapacity))
	}
	for _, row := range csvRows(t, run.outputs[OutputSLOPublished]) {
		// Only the reasons: the snapshot writes NaN as a string.
		var snap struct {
			TTFT []struct{ Reason string }
		}
		require.NoError(t, json.Unmarshal([]byte(row[4]), &snap))
		for _, r := range snap.TTFT {
			run.reasons[r.Reason]++
		}
	}
	return run
}

// sizesPrefillStaircase is, for each staircase recording, the slo analyzer's
// TTFT reasons with the switch on, the cycles it sizes prefill on, and the
// largest count it asks for, as measured. Off, every cycle P1 gave V_P was
// no-fixed-latency or no-decode-side (TestGoldens_TTFTStaircaseHoldsNoS2GateCycle),
// and none sized.
//
//   - a-off (1000/1000) is priced on every cycle with V_P, at one prefill
//     replica, as the saturation analyzer asks there (not asserted here).
//   - b-off (8000/200, flow control off) is the case of
//     llm-d-incubation/llm-d-scaling-manager#131. With a₀ as a it is priced
//     as far as the floor, and the 190 ms p95 is below it on every cycle:
//     0.19 to 0.27 s before the overload, the decode engine's measured TTFT
//     (0.06 to 0.13 s) and the prefill's own time at the assumed prompt cv
//     of 2. No prefill count meets it, so none is asked for. The recordings
//     carry no computed-token histogram. In a one-off replay that priced a
//     measured cv of 0.1, the fixed-length prompts the benchmark sent, the
//     steps that missed (30 to 35 r/s) asked a median of 2 to 4 prefill
//     replicas, at most 5, where the saturation analyzer asks 2; this test
//     does not hold that.
//   - b-on's last cycles hold a decode line and are below the floor too;
//     c-on, flow control on with no decode line, stays no-decode-side.
var sizesPrefillStaircase = []struct {
	fixture string
	reasons map[string]int
	sized   int
	largest float64
}{
	{"ttft-staircase-a-off", map[string]int{constants.SLOTTFTReasonPriced: 115,
		constants.SLOTTFTReasonNoPrefillCapacity: 12, constants.SLOTTFTReasonNoTraffic: 1}, 115, 1},
	{"ttft-staircase-b-off", map[string]int{constants.SLOTTFTReasonTargetBelowFloor: 273,
		constants.SLOTTFTReasonNoPrefillCapacity: 13, constants.SLOTTFTReasonNoTraffic: 3}, 0, 0},
	{"ttft-staircase-b-on", map[string]int{constants.SLOTTFTReasonNoDecodeSide: 251,
		constants.SLOTTFTReasonTargetBelowFloor: 32, constants.SLOTTFTReasonNoPrefillCapacity: 4,
		constants.SLOTTFTReasonNoTraffic: 2}, 0, 0},
	{"ttft-staircase-c-on", map[string]int{constants.SLOTTFTReasonNoDecodeSide: 288,
		constants.SLOTTFTReasonNoPrefillCapacity: 4}, 0, 0},
}

// On the four staircase recordings the switch prices what the joint a₀ lets
// it price and sizes prefill where that is priced; off, nothing is sized.
// Only the slo analyzer's own rows move: its results, variant capacities and
// published readings. The saturation analyzer's rows and the composite stay
// as they are, since the engine sizes from saturation alone.
func TestGoldens_TTFTSizesPrefillOnTheStaircases(t *testing.T) {
	t.Parallel() // reads the goldens, writes nowhere
	sloOutputs := []string{OutputResults, OutputVariantCapacities, OutputSLOPublished}
	for _, c := range sizesPrefillStaircase {
		raw := staircaseRecording(t, c.fixture)
		off, on := replaySizesPrefill(t, raw, false), replaySizesPrefill(t, raw, true)
		t.Logf("%s: on %v, %d cycles sized, at most %g; off %v", c.fixture, on.reasons, on.sized, on.largest, off.reasons)
		assert.Zero(t, off.sized, "%s: off sizes nothing", c.fixture)
		assert.Zero(t, off.reasons[constants.SLOTTFTReasonPriced], "%s: off prices nothing", c.fixture)
		assert.Equal(t, c.reasons, on.reasons, c.fixture)
		assert.Equal(t, c.sized, on.sized, c.fixture)
		assert.Equal(t, c.largest, on.largest, c.fixture)
		for f, b := range off.outputs {
			if !slices.Contains(sloOutputs, f) {
				d, err := compareOutput(f, b, on.outputs[f])
				require.NoError(t, err)
				assert.Nil(t, d, "%s: %s moved", c.fixture, f)
				continue
			}
			offRows, onRows := csvRows(t, b), csvRows(t, on.outputs[f])
			require.Len(t, onRows, len(offRows), "%s: %s", c.fixture, f)
			for i := range offRows {
				if offRows[i][0] != sloAnalyzeKind {
					assert.Equal(t, offRows[i], onRows[i], "%s: %s row %d is not the slo analyzer's", c.fixture, f, i)
				}
			}
		}
	}
}
