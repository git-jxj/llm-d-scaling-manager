package dsetwriter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// refusedNS is the namespace of the refused scale-down envtest.
const refusedNS = "dset-refused"

// guardLabel selects the objects the test's admission policies guard.
const guardLabel = "drain-guard"

// The reasons and waits below are spelled out, not taken from the package, so
// this test also builds against a writer that has none of them: run against
// the parent commit, it shows what the writer did before.
const (
	reasonRefused = "RoleScaleDownRefused"
	reasonFailed  = "RoleReplicasWriteFailed"
	pastFollow    = 10 * time.Minute // past the 2-minute follow bound
)

var lwsGVK = schema.GroupVersionKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSet"}

// TestWriterScaleDownRefusedOnAnAPIServer runs the writer against an API
// server that refuses scale-downs the way a cluster that drains before it
// removes a group does: a ValidatingAdmissionPolicy denies any lower
// spec.replicas without a drain. There is no DisaggregatedSet controller in
// envtest, so the test plays it: controller() copies each role's count to
// the role's child LeaderWorkerSet, as the set controller does, and returns
// what the API server answered.
func TestWriterScaleDownRefusedOnAnAPIServer(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set: this test needs envtest (make setup-envtest)")
	}
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("testdata", "crd")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	require.NoError(t, err)
	defer func() { _ = env.Stop() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	c, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme.Scheme})
	require.NoError(t, err)
	require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: refusedNS}}))

	// A child LeaderWorkerSet may not go lower; nor may a Static role of a
	// guarded set.
	guard(ctx, t, c, "lws-drain-guard", "leaderworkerset.x-k8s.io", "leaderworkersets",
		"object.spec.replicas >= oldObject.spec.replicas",
		"lowering spec.replicas needs a drain first")
	guard(ctx, t, c, "dset-drain-guard", "disaggregatedset.x-k8s.io", "disaggregatedsets",
		"object.spec.roles.all(r, !has(r.spec.replicas) || oldObject.spec.roles.exists(o, o.name == r.name && (!has(o.spec.replicas) || r.spec.replicas >= o.spec.replicas)))",
		"lowering a role needs a drain first")
	probe := child("probe", "decode", 2)
	require.NoError(t, c.Create(ctx, probe))
	waitDenied(ctx, t, func() error {
		p := probe.DeepCopy()
		require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(p), p))
		require.NoError(t, unstructured.SetNestedField(p.Object, int64(1), "spec", "replicas"))
		return c.Update(ctx, p, client.DryRunAll)
	})

	t0 := time.Now()
	clock := t0
	newWriter := func(store *decision.Store, applies *int) (*Writer, *record.FakeRecorder) {
		counting := interceptor.NewClient(c, interceptor.Funcs{
			Apply: func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
				*applies++
				return cl.Apply(ctx, obj, opts...)
			},
		})
		rec := record.NewFakeRecorder(100)
		return &Writer{
			Client: counting, Registry: registry.New(0), Decisions: store, Recorder: rec, Passive: passiveModels{},
			Namespaces: []string{refusedNS}, ScaleDownWindow: -1, DecisionTTL: 24 * time.Hour,
			Now: func() time.Time { return clock },
		}, rec
	}
	// controller is the DisaggregatedSet controller's part: it sets each
	// Static role's child to the role's count.
	controller := func(t *testing.T, name string) error {
		t.Helper()
		want := replicasOf(t, getIn(ctx, t, c, name))
		for _, role := range []string{"prefill", "decode"} {
			lws := &unstructured.Unstructured{}
			lws.SetGroupVersionKind(lwsGVK)
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: refusedNS, Name: childName(name, role)}, lws))
			require.NoError(t, unstructured.SetNestedField(lws.Object, want[role], "spec", "replicas"))
			if err := c.Update(ctx, lws); err != nil {
				return err
			}
		}
		return nil
	}

	t.Run("a scale-down the child LeaderWorkerSet refuses is reported, and the role is not lowered again", func(t *testing.T) {
		const name = "child-refuses"
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(refusedSet(name, nil)), client.FieldOwner("gitops")))
		for _, role := range []string{"prefill", "decode"} {
			require.NoError(t, c.Create(ctx, child(name, role, 1)))
		}
		store := decision.NewStore()
		applies := 0
		w, rec := newWriter(store, &applies)
		clock = t0

		store.Set(refusedNS, TargetName(name, "decode"), 4)
		w.Pass(ctx)
		require.Equal(t, int64(4), replicasOf(t, getIn(ctx, t, c, name))["decode"])
		require.NoError(t, controller(t, name), "a raise is allowed")

		store.Set(refusedNS, TargetName(name, "decode"), 2)
		w.Pass(ctx)
		require.Equal(t, int64(2), replicasOf(t, getIn(ctx, t, c, name))["decode"], "the set takes the lower count")
		err := controller(t, name)
		require.Error(t, err, "the child may not follow it")
		require.Contains(t, err.Error(), "needs a drain first")
		assert.True(t, hasEvent(rec, EventScaled), "the write itself was accepted")

		clock = t0.Add(30 * time.Second)
		w.Pass(ctx)
		assert.False(t, hasEvent(rec, reasonRefused), "inside the follow bound nothing is reported yet")

		clock = t0.Add(pastFollow)
		w.Pass(ctx)
		events := drain(rec)
		assert.Equal(t, 1, countReason(events, reasonRefused),
			"past the bound, the refusal is reported on the set (events: %v)", events)
		assert.Contains(t, strings.Join(events, "\n"), "still run 4")

		// The engines ask for less again: the writer does not lower a role
		// the cluster refused to lower.
		before := applies
		store.Set(refusedNS, TargetName(name, "decode"), 1)
		for i := range 3 {
			clock = t0.Add(pastFollow + time.Duration(i+1)*time.Minute)
			w.Pass(ctx)
		}
		assert.Equal(t, int64(2), replicasOf(t, getIn(ctx, t, c, name))["decode"], "the refused role is not lowered again")
		assert.Equal(t, before, applies, "nor written at all")
		assert.Equal(t, 0, countReason(drain(rec), reasonRefused), "and the refusal is reported once")

		// A raise is written, and resets the hold: the next lower count is
		// asked for again.
		store.Set(refusedNS, TargetName(name, "decode"), 5)
		w.Pass(ctx)
		require.Equal(t, int64(5), replicasOf(t, getIn(ctx, t, c, name))["decode"])
		require.NoError(t, controller(t, name))
		store.Set(refusedNS, TargetName(name, "decode"), 3)
		w.Pass(ctx)
		assert.Equal(t, int64(3), replicasOf(t, getIn(ctx, t, c, name))["decode"], "after a raise, a lower count is written again")
	})

	t.Run("a scale-down the API server denies on the set is reported, and retried with a backoff", func(t *testing.T) {
		const name = "set-refuses"
		s := refusedSet(name, nil)
		s.SetLabels(map[string]string{guardLabel: "on"})
		require.NoError(t, c.Apply(ctx, client.ApplyConfigurationFromUnstructured(s), client.FieldOwner("gitops")))
		store := decision.NewStore()
		applies := 0
		w, rec := newWriter(store, &applies)
		clock = t0

		store.Set(refusedNS, TargetName(name, "decode"), 3)
		w.Pass(ctx)
		require.Equal(t, int64(3), replicasOf(t, getIn(ctx, t, c, name))["decode"], "a raise is allowed")
		waitDenied(ctx, t, func() error {
			lower := ApplyConfiguration(getIn(ctx, t, c, name), map[string]int32{"prefill": 1, "decode": 1})
			return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(lower), client.FieldOwner(FieldManager), client.DryRunAll)
		})
		drain(rec)

		store.Set(refusedNS, TargetName(name, "decode"), 1)
		w.Pass(ctx)
		events := drain(rec)
		assert.Equal(t, 1, countReason(events, reasonRefused), "the denial is reported as a refused scale-down (events: %v)", events)
		assert.Equal(t, 0, countReason(events, reasonFailed), "not as a failed write")
		assert.Contains(t, strings.Join(events, "\n"), "lowering a role needs a drain first", "with the API server's message")
		first := applies

		for _, at := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
			clock = t0.Add(at)
			w.Pass(ctx)
		}
		assert.Equal(t, first, applies, "a denied scale-down is not asked for every pass")

		clock = t0.Add(6 * time.Minute)
		w.Pass(ctx)
		assert.Equal(t, first+1, applies, "it is asked for again after the backoff")
		clock = t0.Add(8 * time.Minute)
		w.Pass(ctx)
		assert.Equal(t, first+1, applies, "and the backoff doubles")

		// Another role goes up meanwhile: written without the held role.
		store.Set(refusedNS, TargetName(name, "prefill"), 2)
		clock = t0.Add(9 * time.Minute)
		w.Pass(ctx)
		assert.Equal(t, map[string]int64{"prefill": 2, "decode": 3, "encode": 1}, replicasOf(t, getIn(ctx, t, c, name)),
			"a denied decrease does not block a raise of another role")
	})
}

// guard creates a ValidatingAdmissionPolicy, and its Deny binding, that
// applies expression to every UPDATE of resource labelled guardLabel=on.
func guard(ctx context.Context, t *testing.T, c client.Client, name, group, resource, expression, message string) {
	t.Helper()
	fail := admissionv1.Fail
	selector := &metav1.LabelSelector{MatchLabels: map[string]string{guardLabel: "on"}}
	require.NoError(t, c.Create(ctx, &admissionv1.ValidatingAdmissionPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionv1.ValidatingAdmissionPolicySpec{
			FailurePolicy: &fail,
			MatchConstraints: &admissionv1.MatchResources{
				ObjectSelector: selector,
				ResourceRules: []admissionv1.NamedRuleWithOperations{{RuleWithOperations: admissionv1.RuleWithOperations{
					Operations: []admissionv1.OperationType{admissionv1.Update},
					Rule:       admissionv1.Rule{APIGroups: []string{group}, APIVersions: []string{"v1"}, Resources: []string{resource}},
				}}},
			},
			Validations: []admissionv1.Validation{{Expression: expression, Message: message}},
		},
	}))
	require.NoError(t, c.Create(ctx, &admissionv1.ValidatingAdmissionPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: admissionv1.ValidatingAdmissionPolicyBindingSpec{
			PolicyName:        name,
			ValidationActions: []admissionv1.ValidationAction{admissionv1.Deny},
		},
	}))
}

// waitDenied retries try, a dry-run decrease, until the API server denies it:
// a new policy takes effect a moment after it is created.
func waitDenied(ctx context.Context, t *testing.T, try func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := try()
		if apierrors.IsForbidden(err) || apierrors.IsInvalid(err) {
			return
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("the admission policy never denied the probe decrease; last answer: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// refusedSet is envSet in the refused test's namespace, opted in to the
// writer.
func refusedSet(name string, replicas map[string]int64) *unstructured.Unstructured {
	s := envSet(name, replicas)
	s.SetNamespace(refusedNS)
	s.SetAnnotations(map[string]string{OptInAnnotation: "true", ModelIDAnnotation: "org/GLM"})
	return s
}

func childName(set, role string) string { return fmt.Sprintf("%s-0-r1-%s", set, role) }

// child is the child LeaderWorkerSet of role in set at replicas, labelled as
// the set controller labels it, and guarded.
func child(set, role string, replicas int64) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"replicas": replicas}}}
	u.SetGroupVersionKind(lwsGVK)
	u.SetNamespace(refusedNS)
	u.SetName(childName(set, role))
	u.SetLabels(map[string]string{
		constants.DisaggregatedSetNameLabelKey: set,
		constants.DisaggregatedSetRoleLabelKey: role,
		"disaggregatedset.x-k8s.io/revision":   "r1",
		guardLabel:                             "on",
	})
	return u
}

func getIn(ctx context.Context, t *testing.T, c client.Client, name string) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(setGVK)
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: refusedNS, Name: name}, u))
	return u
}
