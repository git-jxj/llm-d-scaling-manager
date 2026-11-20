package steadystate

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/signals/capacity"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// The report runs in the cycle, and before its early return: a cycle in which
// no model produced a request -- here, no scaling policy is loaded -- still
// starts the shortfall's clock. A model none of whose replicas ever arrived
// is exactly the one that produces no request.
func TestReplicasNotArrivingIsWiredIntoTheCycle(t *testing.T) {
	c, _ := stormFixture(t, 3, time.Now())
	e := &Engine{
		Config:        config.NewTestConfig(),
		optimizer:     allocation.NewCostAwareOptimizer(),
		analyzers:     testAnalyzers(plainAnalyzer{}),
		capacityStore: capacity.NewStore(),
		client:        c,
		Recorder:      record.NewFakeRecorder(10),
	}
	e.optimizeV2(context.Background(), map[string][]llmdvariant.Variant{"m|ns": {decodeVariant()}}, nil)

	require.NotNil(t, e.notArriving, "the cycle never reached the report")
	assert.Contains(t, e.notArriving.variants, utils.GetNamespacedKey("ns", "decode"),
		"1 of 2 Ready: the shortfall's clock must have started")
}

// Discovery's metadata passes through withholdOverdueArrivals before the
// analyzers see it: a pod Pending and placed for 20 minutes, against a known
// start time of 2 minutes, is stuck rather than arriving.
func TestOverdueArrivalsWithheldInModelData(t *testing.T) {
	now := time.Now()
	c, d := stormFixture(t, 0, now)
	require.NoError(t, c.Create(context.Background(), &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "decode-slow", Namespace: "ns", Labels: decodeLabels,
			CreationTimestamp: metav1.NewTime(now.Add(-20 * time.Minute))},
		Spec: corev1.PodSpec{NodeName: "gpu-node-2"},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}},
	}))
	e := &Engine{Config: config.NewTestConfig(), client: c}
	e.notArrivingState().noteRoleStarts([]allocation.ModelScalingRequest{{Namespace: "ns", ModelID: "m",
		CompositeSignal: allocation.NamedAnalyzerResult{Result: &domain.AnalyzerResult{
			RoleStartSeconds: map[string]float64{"decode": 120}}}}})
	va := decodeVariant()
	targets := map[string]scaletarget.ScaleTargetAccessor{
		utils.GetNamespacedKey("ns", "decode"): scaletarget.NewDeploymentAccessor(d),
	}

	data := e.assembleModelData(context.Background(), "m", "ns", []llmdvariant.Variant{va}, targets,
		map[string]*llmdvariant.Variant{utils.GetNamespacedKey("ns", "decode"): &va}, nil, c)

	require.Len(t, data.variantMetadata, 1)
	m := data.variantMetadata[0]
	assert.Equal(t, 1, m.PendingReplicas)
	assert.Equal(t, 1, m.StuckReplicas, "20 minutes is past max(5m, 2x120s)")
	assert.Empty(t, m.PendingAges)
	require.Len(t, data.variantStates, 1)
	assert.Equal(t, 1, data.variantStates[0].StuckReplicas, "the analyzers' copy carries it too")
}
