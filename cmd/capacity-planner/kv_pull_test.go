package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/whatif"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/whatifapi"
)

// The KV pull is named as the larger wait in TTFT only where the decode
// side's wait beats the prefill side's and the pull is its larger part
// (issue #301: decode idle by its KV fill, yet the decode count sets TTFT).
func TestKVPull_SetsTTFTOnlyAsTheLargerWait(t *testing.T) {
	for _, c := range []struct {
		name string
		p    whatif.TTFTParts
		want bool
	}{
		{"pull beats prefill and admission", whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.05, DecodeWait: 0.001}, true},
		{"prefill is the larger wait", whatif.TTFTParts{EngineWait: 0.08, KVPullWait: 0.05}, false},
		{"admission is the decode side's larger part", whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.02, DecodeWait: 0.03}, false},
		{"the router's wait counts on the prefill side", whatif.TTFTParts{EngineWait: 0.01, RouterWait: 0.03, BurstWait: 0.02, KVPullWait: 0.05}, false},
		{"no pull", whatif.TTFTParts{EngineWait: 0.01}, false},
	} {
		assert.Equal(t, c.want, pullSetsTTFT(c.p), c.name)
	}
}

// A missed TTFT names the pull, not decode admission, where the pull is the
// decode side's larger wait.
func TestKVPull_ARoleWordNamesThePull(t *testing.T) {
	role, why := ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.05, DecodeWait: 0.001}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role)
	assert.True(t, strings.HasPrefix(why, "the KV pull on the decode pods"), why)
	role, why = ttftRole(whatif.TTFT{Parts: whatif.TTFTParts{EngineWait: 0.02, KVPullWait: 0.01, DecodeWait: 0.03}}, fleetEval{})
	assert.Equal(t, roleDecodeWord, role)
	assert.True(t, strings.HasPrefix(why, "decode admission"), why)
}

// Decode's utilization carries a KV pull row where the pull is priced, and
// says when it sets TTFT; with none priced there is no row.
func TestKVPull_TheUtilizationRow(t *testing.T) {
	var s whatifapi.ReportShape
	s.Utilization.Decode.KVPull = &whatifapi.ReportKVPull{Utilization: 0.62, Wait: 0.05, SetsTTFT: true}
	var b strings.Builder
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait 50.0 ms: the larger wait in TTFT, so the decode count sets TTFT\n")

	s.Utilization.Decode.KVPull.SetsTTFT = false
	b.Reset()
	shapeText(&b, s)
	assert.Contains(t, b.String(), "  KV pull       utilization 0.620 per decode pod, mean wait 50.0 ms\n")

	s.Utilization.Decode.KVPull = nil
	b.Reset()
	shapeText(&b, s)
	assert.NotContains(t, b.String(), "KV pull")
}

// On the 0.6B staircase at 9 prefill (#301's case), at 30 r/s, decode admits
// every request at once while the pull on 2 decode pods is the larger wait
// in TTFT; at 4 the pull's utilization halves.
func TestKVPull_TheStaircaseNamesThePullWhileDecodeIdles(t *testing.T) {
	use := func(nd int) whatifapi.ReportDecodeUse {
		q := stairQuery(9, nd, 30)
		q.TTFT = &whatifapi.ReportTarget{Seconds: 0.2, Percentile: 0.95}
		s := stairReport(t, q).Shapes[0]
		require.NotNil(t, s.Utilization.Decode.KVPull, "%d decode: %s; %+v", nd, s.Verdict.Summary, s.Table)
		return s.Utilization.Decode
	}
	two, four := use(2), use(4)
	require.NotNil(t, two.WaitProbability.Value)
	assert.Less(t, *two.WaitProbability.Value, 0.01, "decode admits every request at once")
	assert.True(t, two.KVPull.SetsTTFT)
	assert.Greater(t, two.KVPull.Wait, four.KVPull.Wait)
	assert.InEpsilon(t, two.KVPull.Utilization/2, four.KVPull.Utilization, 1e-9)
}
