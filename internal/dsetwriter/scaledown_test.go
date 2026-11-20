package dsetwriter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// followRig is a writer over a fake API server that stores what the writer
// applies, and the child LeaderWorkerSets the test sets.
type followRig struct {
	t       *testing.T
	w       *Writer
	c       client.WithWatch
	rec     *record.FakeRecorder
	store   *decision.Store
	clock   time.Time
	t0      time.Time
	applies int
	// deny, when set, is returned by the next applies instead of applying.
	deny error
	// listErr, when set, fails every list of LeaderWorkerSets.
	listErr error
}

// newFollowRig starts with prefill at 2 groups and decode at 4, each child
// LeaderWorkerSet running as many.
func newFollowRig(t *testing.T) *followRig {
	t.Helper()
	const decode = 4
	sch := runtime.NewScheme()
	gv := schema.GroupVersion{Group: setGVK.Group, Version: setGVK.Version}
	sch.AddKnownTypeWithName(setGVK, &unstructured.Unstructured{})
	sch.AddKnownTypeWithName(gv.WithKind(constants.DisaggregatedSetKind+"List"), &unstructured.UnstructuredList{})
	sch.AddKnownTypeWithName(lwsListGVK.GroupVersion().WithKind("LeaderWorkerSet"), &unstructured.Unstructured{})
	sch.AddKnownTypeWithName(lwsListGVK, &unstructured.UnstructuredList{})
	r := &followRig{t: t, store: decision.NewStore(), t0: time.Now()}
	r.clock = r.t0
	r.c = fake.NewClientBuilder().WithScheme(sch).WithObjects(set(optIn("true"), "", decode)).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if u, ok := list.(*unstructured.UnstructuredList); ok && u.GroupVersionKind() == lwsListGVK && r.listErr != nil {
				return r.listErr
			}
			return c.List(ctx, list, opts...)
		},
		Apply: func(ctx context.Context, c client.WithWatch, o runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			r.applies++
			if r.deny != nil {
				return r.deny
			}
			doc := o.(interface{ UnstructuredContent() map[string]any }).UnstructuredContent()
			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(setGVK)
			require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "glm"}, live))
			roles, _, _ := unstructured.NestedSlice(live.Object, "spec", "roles")
			for _, a := range rolesOf(doc) {
				am := a.(map[string]any)
				for _, l := range roles {
					lm := l.(map[string]any)
					if lm["name"] == am["name"] {
						n, _, _ := unstructured.NestedInt64(am, "spec", "replicas")
						require.NoError(t, unstructured.SetNestedField(lm, n, "spec", "replicas"))
					}
				}
			}
			require.NoError(t, unstructured.SetNestedSlice(live.Object, roles, "spec", "roles"))
			return c.Update(ctx, live)
		},
	}).Build()
	r.rec = record.NewFakeRecorder(100)
	r.w = &Writer{Client: r.c, Registry: registry.New(0), Decisions: r.store, Recorder: r.rec, Passive: passiveModels{},
		AllNamespaces: true, ScaleDownWindow: -1, DecisionTTL: 24 * time.Hour, Now: func() time.Time { return r.clock }}
	r.child("decode", "r1", decode)
	r.child("prefill", "r1", 2)
	return r
}

// child creates or sets the child LeaderWorkerSet of role at revision.
func (r *followRig) child(role, revision string, replicas int64) {
	r.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(lwsListGVK.GroupVersion().WithKind("LeaderWorkerSet"))
	name := "glm-0-" + revision + "-" + role
	err := r.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, u)
	if apierrors.IsNotFound(err) {
		u.SetNamespace(ns)
		u.SetName(name)
		u.SetLabels(map[string]string{
			constants.DisaggregatedSetNameLabelKey: "glm",
			constants.DisaggregatedSetRoleLabelKey: role,
			revisionLabelKey:                       revision,
		})
		require.NoError(r.t, unstructured.SetNestedField(u.Object, replicas, "spec", "replicas"))
		require.NoError(r.t, r.c.Create(context.Background(), u))
		return
	}
	require.NoError(r.t, err)
	require.NoError(r.t, unstructured.SetNestedField(u.Object, replicas, "spec", "replicas"))
	require.NoError(r.t, r.c.Update(context.Background(), u))
}

func (r *followRig) decode() int64 {
	r.t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(setGVK)
	require.NoError(r.t, r.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "glm"}, u))
	return replicasOf(r.t, u)["decode"]
}

func (r *followRig) passAt(d time.Duration, decode int32) {
	r.store.Set(ns, "glm-decode", decode)
	r.clock = r.t0.Add(d)
	r.w.Pass(context.Background())
}

// A lower count the children do not follow is reported once the bound has
// passed, not before; the role is then held until the children follow.
func TestScaleDownRefusedAfterTheBound(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	require.Equal(t, int64(2), r.decode())

	r.passAt(DefaultFollowTimeout-time.Second, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused), "inside the bound")

	r.passAt(DefaultFollowTimeout, 2)
	events := drain(r.rec)
	require.Equal(t, 1, countReason(events, EventScaleDownRefused), "at the bound: %v", events)

	n := r.applies
	r.passAt(DefaultFollowTimeout+time.Minute, 1)
	assert.Equal(t, int64(2), r.decode(), "held")
	assert.Equal(t, n, r.applies)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused), "reported once")

	// The operator drains, the controller lowers the child: the hold lifts
	// and the next lower count is written.
	r.child("decode", "r1", 2)
	r.passAt(DefaultFollowTimeout+2*time.Minute, 1)
	assert.Equal(t, int64(1), r.decode(), "followed: the writer lowers again")
}

// A scale-down that the children follow in time is never reported.
func TestScaleDownFollowedIsNotReported(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.child("decode", "r1", 2)
	r.passAt(time.Hour, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused))
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	assert.Empty(t, r.w.scaleDowns, "the record is dropped once followed")
}

// A rollout that defers the scale-down (two revisions with groups, the
// default Deferred) stops the clock; the bound counts from its end.
func TestScaleDownDeferredByARolloutIsNotRefused(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.child("decode", "r2", 1) // the new revision rolls in
	r.passAt(time.Hour, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused), "held by the rollout")

	r.child("decode", "r2", 0) // the rollout ends; r1 still runs 4
	r.passAt(time.Hour+DefaultFollowTimeout-time.Second, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused), "the bound counts from the rollout's end")
	r.passAt(time.Hour+DefaultFollowTimeout, 2)
	assert.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused))
}

// With scaleDownDuringRollout Immediate a rollout stops the clock too: the
// old and new revisions run side by side, so the sum is above the count
// while it lasts.
func TestScaleDownImmediateRolloutStopsTheClockToo(t *testing.T) {
	r := newFollowRig(t)
	r.update(func(live *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedField(live.Object, "Immediate", "spec", "scalingPolicy", "scaleDownDuringRollout"))
	})
	r.passAt(0, 2)
	r.child("decode", "r2", 1)
	r.passAt(DefaultFollowTimeout, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused))
}

// update changes the stored set with f.
func (r *followRig) update(f func(*unstructured.Unstructured)) {
	r.t.Helper()
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(setGVK)
	require.NoError(r.t, r.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "glm"}, live))
	f(live)
	require.NoError(r.t, r.c.Update(context.Background(), live))
}

func (r *followRig) records() int {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	return len(r.w.scaleDowns)
}

// A raise resets a refused role: it is written at once, and a lower count
// after it is asked for again.
func TestScaleDownRefusedResetByARaise(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.passAt(DefaultFollowTimeout, 2)
	require.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused))

	r.passAt(DefaultFollowTimeout+time.Minute, 5)
	require.Equal(t, int64(5), r.decode(), "a raise is written at once")
	assert.Equal(t, 0, r.records(), "and drops the record when written, not on the next pass")
	r.child("decode", "r1", 5)
	r.passAt(DefaultFollowTimeout+2*time.Minute, 3)
	assert.Equal(t, int64(3), r.decode(), "after the raise a lower count is written again")
}

// A count someone else sets (the operator, after a drain) drops the record.
func TestScaleDownRecordDroppedWhenTheCountMoves(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.passAt(DefaultFollowTimeout, 2)
	require.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused))
	live := &unstructured.Unstructured{}
	live.SetGroupVersionKind(setGVK)
	require.NoError(t, r.c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "glm"}, live))
	roles, _, _ := unstructured.NestedSlice(live.Object, "spec", "roles")
	for _, x := range roles {
		if m := x.(map[string]any); m["name"] == "decode" {
			require.NoError(t, unstructured.SetNestedField(m, int64(4), "spec", "replicas"))
		}
	}
	require.NoError(t, unstructured.SetNestedSlice(live.Object, roles, "spec", "roles"))
	require.NoError(t, r.c.Update(context.Background(), live))
	r.passAt(DefaultFollowTimeout+time.Minute, 2)
	assert.Equal(t, int64(2), r.decode(), "the record no longer holds the role")
}

// A denial on the set itself is retried after DefaultDeniedRetry, then after
// twice that, never more than MaxDeniedRetry apart; a write that lowers
// nothing is not held by it.
func TestScaleDownDeniedBacksOff(t *testing.T) {
	r := newFollowRig(t)
	r.deny = apierrors.NewForbidden(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "glm",
		assertErr("ValidatingAdmissionPolicy 'guard' with binding 'guard' denied request: drain first"))
	r.passAt(0, 2)
	require.Equal(t, 1, r.applies)
	events := drain(r.rec)
	assert.Equal(t, 1, countReason(events, EventScaleDownRefused), "%v", events)
	assert.Equal(t, 0, countReason(events, EventFailed))

	r.passAt(DefaultDeniedRetry-time.Second, 2)
	assert.Equal(t, 1, r.applies, "held until the retry")
	r.passAt(DefaultDeniedRetry, 2)
	assert.Equal(t, 2, r.applies, "retried")
	assert.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused), "each denial is reported")
	r.passAt(DefaultDeniedRetry+2*DefaultDeniedRetry-time.Second, 2)
	assert.Equal(t, 2, r.applies, "the wait doubled")
	r.passAt(3*DefaultDeniedRetry, 2)
	assert.Equal(t, 3, r.applies)

	assert.Equal(t, MaxDeniedRetry, r.w.deniedRetry(20), "capped")

	// The policy lets it through after a drain: accepted, then followed.
	r.deny = nil
	drain(r.rec)
	at := 3*DefaultDeniedRetry + 4*DefaultDeniedRetry
	r.passAt(at, 2)
	assert.Equal(t, int64(2), r.decode())
	// It is now a written scale-down: the children are followed. They do
	// not follow, so it is refused past the bound.
	r.passAt(at+DefaultFollowTimeout, 2)
	assert.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused))
	r.child("decode", "r1", 2)
	r.passAt(at+DefaultFollowTimeout+time.Minute, 2)
	assert.Equal(t, 0, r.records(), "followed")
}

// A denied record is dropped once the engines no longer want the lower
// count, so a later denial starts the backoff afresh.
func TestScaleDownDeniedSettlesWhenNoLongerWanted(t *testing.T) {
	r := newFollowRig(t)
	r.deny = apierrors.NewForbidden(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "glm",
		assertErr("admission webhook \"w\" denied the request: drain first"))
	r.passAt(0, 2)
	r.passAt(DefaultDeniedRetry, 2)
	require.Equal(t, 2, r.applies)
	r.passAt(DefaultDeniedRetry+time.Minute, 4)
	assert.Equal(t, 0, r.records(), "the engines want 4 again")
	r.passAt(DefaultDeniedRetry+2*time.Minute, 2)
	require.Equal(t, 3, r.applies)
	r.passAt(DefaultDeniedRetry+2*time.Minute+DefaultDeniedRetry, 2)
	assert.Equal(t, 4, r.applies, "the first wait again, not the doubled one")
}

// A further decrease while one is awaited keeps the first write's clock.
func TestScaleDownFurtherDecreaseKeepsTheClock(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 3)
	r.passAt(time.Minute, 2)
	require.Equal(t, int64(2), r.decode())
	r.passAt(DefaultFollowTimeout, 2)
	events := drain(r.rec)
	require.Equal(t, 1, countReason(events, EventScaleDownRefused), "two minutes after the first write: %v", events)
	assert.Contains(t, events[len(events)-1], "spec.replicas is 2")
}

// A failed read of the children is unknown: no refusal, the clock kept, and
// a refusal already reported still held.
func TestScaleDownChildReadFailure(t *testing.T) {
	r := newFollowRig(t)
	lines := &logLines{}
	ctx := ctrl.LoggerInto(context.Background(), lines.logger())
	pass := func(d time.Duration, decode int32) {
		r.store.Set(ns, "glm-decode", decode)
		r.clock = r.t0.Add(d)
		r.w.Pass(ctx)
	}
	pass(0, 2)
	r.listErr = errors.New("connection refused")
	pass(DefaultFollowTimeout, 2)
	pass(DefaultFollowTimeout+time.Second, 2)
	assert.Equal(t, 0, countReason(drain(r.rec), EventScaleDownRefused), "unknown is not refused")
	assert.Equal(t, 1, lines.count("Could not read the child LeaderWorkerSets"), "logged once")
	assert.Equal(t, 0, lines.count("needs list on leaderworkersets"), "a timeout is not blamed on RBAC")

	r.listErr = nil
	pass(DefaultFollowTimeout+2*time.Second, 2)
	assert.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused), "the clock was kept: refused at once")

	r.listErr = apierrors.NewForbidden(schema.GroupResource{Group: "leaderworkerset.x-k8s.io", Resource: "leaderworkersets"}, "", errors.New("no"))
	pass(DefaultFollowTimeout+time.Minute, 1)
	assert.Equal(t, int64(2), r.decode(), "a refused role stays held while the read fails")
	assert.Equal(t, 1, lines.count("needs list on leaderworkersets"), "Forbidden names the grant")
}

// The refusal event carries the set's failing conditions.
func TestScaleDownRefusedEventSaysTheConditions(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.update(func(live *unstructured.Unstructured) {
		require.NoError(t, unstructured.SetNestedSlice(live.Object, []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "UpdateFailed", "message": "denied request: drain first"},
		}, "status", "conditions"))
	})
	r.passAt(DefaultFollowTimeout, 2)
	events := drain(r.rec)
	require.Equal(t, 1, countReason(events, EventScaleDownRefused))
	assert.Contains(t, events[len(events)-1], "Ready=False UpdateFailed: denied request: drain first")
}

// A restart loses the records: the first pass compares the children with
// the count and starts one, so a refusal from before the restart is still
// reported.
func TestScaleDownRefusedAcrossARestart(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	r.w = &Writer{Client: r.c, Registry: registry.New(0), Decisions: r.store, Recorder: r.rec, Passive: passiveModels{},
		AllNamespaces: true, ScaleDownWindow: -1, DecisionTTL: 24 * time.Hour, Now: func() time.Time { return r.clock }}
	// The engines still want 2: the new writer writes nothing, so only the
	// first-sight check can notice the children never followed.
	r.passAt(time.Minute, 2)
	r.passAt(time.Minute+DefaultFollowTimeout, 2)
	assert.Equal(t, 1, countReason(drain(r.rec), EventScaleDownRefused))
	n := r.applies
	r.passAt(time.Minute+DefaultFollowTimeout+time.Second, 1)
	assert.Equal(t, n, r.applies, "and the role is held")
}

// A passive model and an opted-out set drop their records.
func TestScaleDownRecordsDroppedWhenPassiveOrGone(t *testing.T) {
	r := newFollowRig(t)
	r.passAt(0, 2)
	require.Equal(t, 1, r.records())
	r.w.Passive = passiveModels{"org/GLM": true}
	r.passAt(time.Minute, 2)
	assert.Equal(t, 0, r.records(), "passive")

	r.w.Passive = passiveModels{}
	r.child("decode", "r1", 4)
	r.passAt(2*time.Minute, 2)
	require.Equal(t, 1, r.records(), "seen again: children above the count")
	r.update(func(live *unstructured.Unstructured) { live.SetAnnotations(nil) })
	r.passAt(3*time.Minute, 2)
	assert.Equal(t, 0, r.records(), "opted out")
}

// An RBAC Forbidden is a failed write, not a refused scale-down: it is
// retried every pass, as before.
func TestScaleDownRBACForbiddenIsAFailure(t *testing.T) {
	r := newFollowRig(t)
	r.deny = apierrors.NewForbidden(schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}, "glm",
		assertErr(`User "system:serviceaccount:x:y" cannot patch resource "disaggregatedsets"`))
	r.passAt(0, 2)
	r.passAt(time.Second, 2)
	events := drain(r.rec)
	assert.Equal(t, 0, countReason(events, EventScaleDownRefused))
	assert.Equal(t, 1, countReason(events, EventFailed))
	assert.Equal(t, 2, r.applies)
}

func TestIsDenial(t *testing.T) {
	gr := schema.GroupResource{Group: setGVK.Group, Resource: "disaggregatedsets"}
	assert.True(t, isDenial(apierrors.NewForbidden(gr, "glm", assertErr("ValidatingAdmissionPolicy 'p' with binding 'b' denied request: x"))))
	assert.True(t, isDenial(apierrors.NewInvalid(setGVK.GroupKind(), "glm", field.ErrorList{field.Forbidden(field.NewPath("spec"), `admission webhook "w" denied the request: x`)})))
	assert.False(t, isDenial(apierrors.NewForbidden(gr, "glm", assertErr(`User "u" cannot patch resource "disaggregatedsets"`))))
	assert.False(t, isDenial(apierrors.NewConflict(gr, "glm", assertErr("denied"))))
}

// A held role keeps its groups, so a role planned to zero beside it keeps
// its own: the set forbids one Static role at zero while another runs.
func TestHoldRefusedKeepsAllZeroOrNone(t *testing.T) {
	roles := []Role{{Name: "prefill", Current: 2}, {Name: "decode", Current: 3}}
	got := holdRefused(roles, map[string]int32{"prefill": 0, "decode": 0}, map[string]string{"decode": "refused"}, 1)
	assert.Equal(t, map[string]int32{"prefill": 2, "decode": 3}, got)
	got = holdRefused(roles, map[string]int32{"prefill": 4, "decode": 1}, map[string]string{"decode": "refused"}, 1)
	assert.Equal(t, map[string]int32{"prefill": 4, "decode": 3}, got, "a held role is not lowered; the other moves")
	got = holdRefused([]Role{{Name: "prefill", Current: 0}, {Name: "decode", Current: 3}},
		map[string]int32{"prefill": 0, "decode": 0}, map[string]string{"decode": "refused"}, 0)
	assert.Equal(t, map[string]int32{"prefill": 1, "decode": 3}, got, "a role with no groups beside a held one gets max(lo, 1)")
	got = holdRefused(roles, map[string]int32{"prefill": 1, "decode": 1}, nil, 1)
	assert.Equal(t, map[string]int32{"prefill": 1, "decode": 1}, got, "nothing held, nothing changed")
}

func TestConditionsSay(t *testing.T) {
	s := set(nil, "", 1)
	assert.Empty(t, conditionsSay(s))
	require.NoError(t, unstructured.SetNestedSlice(s.Object, []any{
		map[string]any{"type": "Available", "status": "True", "reason": "Ok", "message": "fine"},
		map[string]any{"type": "Progressing", "status": "False", "reason": "UpdateFailed", "message": "admission denied"},
	}, "status", "conditions"))
	assert.Equal(t, "; the set's conditions say: Progressing=False UpdateFailed: admission denied", conditionsSay(s))
}
