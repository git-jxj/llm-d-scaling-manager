package scalefromzero

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/collector/source"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	unittestutil "github.com/llm-d-incubation/llm-d-scaling-manager/test/utils"
)

// withRunningCanary adds a second variant of the fixture's model, running 3
// replicas: the canary a pool does not select.
func withRunningCanary(t *testing.T, f wakeFixture) string {
	t.Helper()
	canaryTarget, canary := f.target+"-canary", f.variant+"-canary"
	so := managedSO(f.ns, canary, canaryTarget, f.model)
	require.NoError(t, f.engine.client.Create(context.Background(),
		unittestutil.MakeDeployment(canaryTarget, f.ns, 3, map[string]string{"app": "canary"})))
	require.NoError(t, f.engine.client.Create(context.Background(), so))
	registerSO(f.engine.Variants, so)
	return canary
}

// The park-then-wake sequence through optimize(): the routed variant is at
// zero, the held canary runs 3. With the hold published, the canary does not
// cover the model and the routed variant wakes. Without it -- the model not
// routed -- the canary covers decode and nothing wakes, as before.
func TestARunningHeldCanaryDoesNotStopTheRoutedVariantWaking(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)

	f := newWakeFixture(t, "heldwake")
	canary := withRunningCanary(t, f)
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 3)}

	require.NoError(t, f.engine.optimize(context.Background()))
	_, published, _ := f.woken()
	require.False(t, published, "precondition: with no routing published, the running canary covers the model")

	decision.DefaultHeld.Publish(f.ns, f.model, []string{canary}, []string{f.variant}, time.Now())
	require.NoError(t, f.engine.optimize(context.Background()))
	replicas, published, _ := f.woken()
	require.True(t, published, "a held canary counted as the model's running decode: the routed variant never woke")
	require.Equal(t, int32(1), replicas)
}

// A held variant at zero is never the one woken.
func TestAHeldParkedVariantIsNotWoken(t *testing.T) {
	t.Cleanup(decision.DefaultHeld.Reset)

	f := newWakeFixture(t, "heldparked")
	f.src.values = []source.MetricValue{queueDepth(constants.EPPFlowControlQueueSize, f.model, 3)}
	decision.DefaultHeld.Publish(f.ns, f.model, []string{f.variant}, []string{"elsewhere"}, time.Now())
	require.NoError(t, f.engine.optimize(context.Background()))
	_, published, _ := f.woken()
	require.False(t, published, "a held variant was woken: the router cannot reach it")

	// A publication older than heldMaxAge no longer holds.
	decision.DefaultHeld.Publish(f.ns, f.model, []string{f.variant}, []string{"elsewhere"}, time.Now().Add(-heldMaxAge-time.Minute))
	require.NoError(t, f.engine.optimize(context.Background()))
	_, published, _ = f.woken()
	require.True(t, published, "a stale hold still kept the variant asleep")
}
