package whatifsource

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

// --gpus-per-replica, given, replaces what kube-state-metrics reads, and says
// so.
func TestCapture_GPUsPerReplicaGivenOverridesKubeStateMetrics(t *testing.T) {
	d := lwsFleetCapture(t, decodeRole(everyPod), prefillRole(everyPod), func(o *Options) {
		o.GPUsPerReplica, o.GPUsPerReplicaSet = 16, true
	})
	assert.Equal(t, 16, d.window.Variants[0].GpuCount)
	assert.Contains(t, d.window.Flags, fmt.Sprintf(FlagGPUsOverride, "m-decode", 16, 32))

	// Not given, the default stands in only where kube-state-metrics has
	// nothing.
	d = lwsFleetCapture(t, decodeRole(everyPod), prefillRole(everyPod), func(o *Options) { o.GPUsPerReplica = 16 })
	assert.Equal(t, 32, d.window.Variants[0].GpuCount)
}
