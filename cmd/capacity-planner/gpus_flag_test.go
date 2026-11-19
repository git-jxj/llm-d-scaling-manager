package main

import (
	"io"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --gpus-per-replica is unset by default, standing in with 1 only where
// kube-state-metrics gives no count; given, it is the replica's GPUs and
// replaces kube-state-metrics' count (whatifsource.FlagGPUsOverride). Its
// help says it counts a whole group, so a LeaderWorkerSet's is not given as
// one Pod's. A count that is no number, or 0, is a usage error
// (TestCapture_UsageErrors).
func TestCapture_GPUsPerReplicaFlag(t *testing.T) {
	options := func(extra ...string) (int, bool) {
		t.Helper()
		fs := newFlags("slo-to-rate", io.Discard)
		f := answerFlags{question: "slo-to-rate", warn: io.Discard}
		f.register(fs)
		ok, err := parse(fs, slices.Concat(sweepWindow("http://prom.example:9090"), extra))
		require.True(t, ok)
		require.NoError(t, err)
		require.NoError(t, f.check(fs))
		opts, err := f.windowOptions()
		require.NoError(t, err)
		return opts.GPUsPerReplica, opts.GPUsPerReplicaSet
	}

	gpus, set := options()
	assert.Equal(t, 1, gpus, "the stand-in where kube-state-metrics gives none")
	assert.False(t, set, "unset by default: kube-state-metrics' count stands")

	gpus, set = options("--gpus-per-replica", "32")
	assert.Equal(t, 32, gpus)
	assert.True(t, set, "given, it overrides kube-state-metrics")

	gpus, set = options("--gpus-per-replica", "1")
	assert.Equal(t, 1, gpus)
	assert.True(t, set, "given as the default's value, it still overrides")

	fs := newFlags("capture", io.Discard)
	var f captureFlags
	f.registerWindow(fs)
	usage := fs.Lookup("gpus-per-replica").Usage
	assert.Contains(t, usage, "the whole group's for a LeaderWorkerSet")
}
