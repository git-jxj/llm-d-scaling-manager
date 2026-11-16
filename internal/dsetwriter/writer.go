// Package dsetwriter scales the Static roles of a DisaggregatedSet by writing
// spec.roles[].spec.replicas on the set itself.
//
// Every other scale target is actuated by KEDA through a /scale subresource.
// A DisaggregatedSet has none on the set, and a role has one only in
// scaling.mode External, through its DisaggregatedSetRoleScaler -- which LWS
// before v0.10.0 does not have at all. KEDA cannot target a Static role, so
// it never calls the scaler about one, and the role is never registered. This
// writer is the second path for such a role:
//
//   - it registers each Static role of a set that opts in (OptInAnnotation) in
//     the registry, as KEDA's calls register a ScaledObject, so the engines
//     size it like any other variant (target kind DisaggregatedSetRoleScaler,
//     name <set>-<role>, the name a role scaler would have);
//   - it reads the decisions the engines publish for those roles and applies
//     them to the set with one server-side apply, field manager FieldManager.
//
// External roles are never written: their count is the role scaler's, and the
// set controller ignores spec.replicas for them. KEDA plus the role scaler is
// the preferred path; this one exists for sets that cannot take it.
//
// Off unless DSET_ROLE_WRITER is true, and on the leader only. It needs patch
// on disaggregatedsets, granted by a separate, admin-applied component.
// See docs/proposals/disaggregatedset.md.
package dsetwriter

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/decision"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/registry"
)

// Annotations on a DisaggregatedSet that configure the writer for its Static
// roles. They take the place of the ScaledObject a KEDA-scaled role has.
const (
	// OptInAnnotation opts the set in: "true" writes when no other field
	// manager owns a role's replicas; "force" takes them over (see Writer.apply).
	OptInAnnotation = "scaling-manager.llmd.ai/role-writer"
	// ModelIDAnnotation is the model the set serves, as the engine reports it
	// (--served-model-name). Required.
	ModelIDAnnotation = "scaling-manager.llmd.ai/model-id"
	// MinReplicasAnnotation and MaxReplicasAnnotation bound every Static
	// role, in groups. Defaults 1 and 10, the plan's own.
	MinReplicasAnnotation = "scaling-manager.llmd.ai/min-replicas"
	MaxReplicasAnnotation = "scaling-manager.llmd.ai/max-replicas"
	// ScalingPolicyAnnotation names the scaling policy tier, as the trigger's
	// scalingPolicy does. Optional.
	ScalingPolicyAnnotation = "scaling-manager.llmd.ai/scaling-policy"
)

// FieldManager is the server-side apply field manager the writer applies as.
const FieldManager = "scaling-manager-role-writer"

// Event reasons on the DisaggregatedSet.
const (
	EventScaled   = "RoleReplicasWritten"
	EventConflict = "RoleReplicasConflict"
	EventRefused  = "RoleWriterRefused"
)

// Defaults.
const (
	DefaultInterval    = 15 * time.Second
	DefaultDecisionTTL = 3 * time.Minute
	defaultMin         = 1
	defaultMax         = 10
)

var setGVK = schema.FromAPIVersionAndKind(constants.DisaggregatedSetAPIVersion, constants.DisaggregatedSetKind)

// DecisionReader is the part of the decision store the writer reads.
type DecisionReader interface {
	Get(namespace, name string) (decision.Decision, bool)
}

// Writer registers and actuates the Static roles of opted-in sets.
type Writer struct {
	// Client reads (uncached) and applies the sets.
	Client client.Client
	// Registry is where the roles are registered, as KEDA calls register a
	// ScaledObject.
	Registry *registry.Registry
	// Decisions is the store the engines publish into.
	Decisions DecisionReader
	// Recorder records events on the set; may be nil.
	Recorder record.EventRecorder
	// Namespaces to scan; empty means every namespace.
	Namespaces []string
	// Interval between passes, and DecisionTTL, past which a positive
	// decision no longer speaks for its role (the external scaler's rule).
	Interval, DecisionTTL time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// NeedLeaderElection makes the writer run on the leader only.
func (w *Writer) NeedLeaderElection() bool { return true }

// Start runs a pass every Interval until ctx is done.
func (w *Writer) Start(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	logger := ctrl.LoggerFrom(ctx).WithName("dset-role-writer")
	logger.Info("DisaggregatedSet role writer is on: Static roles of sets annotated "+OptInAnnotation+" are scaled by writing spec.roles[].spec.replicas",
		"namespaces", w.Namespaces, "interval", interval)
	ctx = ctrl.LoggerInto(ctx, logger)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		w.Pass(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Pass scans the sets once: registers their Static roles and applies the
// decisions published for them.
func (w *Writer) Pass(ctx context.Context) {
	logger := ctrl.LoggerFrom(ctx)
	namespaces := w.Namespaces
	if len(namespaces) == 0 {
		namespaces = []string{""}
	}
	for _, ns := range namespaces {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(setGVK.GroupVersion().WithKind(constants.DisaggregatedSetKind + "List"))
		if err := w.Client.List(ctx, list, client.InNamespace(ns)); err != nil {
			if !meta.IsNoMatchError(err) {
				logger.Error(err, "Could not list DisaggregatedSets", "namespace", ns)
			}
			continue
		}
		for i := range list.Items {
			w.reconcile(ctx, &list.Items[i])
		}
	}
}

// Role is one Static role of a set as the writer sees it.
type Role struct {
	Name    string
	Current int32
}

// StaticRoles returns the roles of set the writer may write, in spec order,
// and why none may be written when that is so: an External role is the role
// scaler's, a partitioned role's count is its sub-roles', and with slices
// above 1 a role's count is per slice while the engines size its total.
func StaticRoles(set *unstructured.Unstructured) ([]Role, string) {
	if slices, found, _ := unstructured.NestedInt64(set.Object, "spec", "slices"); found && slices > 1 {
		return nil, fmt.Sprintf("spec.slices is %d; the role writer scales only sets of one slice", slices)
	}
	roles, _, _ := unstructured.NestedSlice(set.Object, "spec", "roles")
	var out []Role
	for _, r := range roles {
		m, ok := r.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		mode, _, _ := unstructured.NestedString(m, "scaling", "mode")
		subs, _, _ := unstructured.NestedSlice(m, "subRoles")
		if name == "" || mode == "External" || len(subs) > 0 {
			continue
		}
		replicas, found, _ := unstructured.NestedInt64(m, "spec", "replicas")
		if !found {
			replicas = 1
		}
		out = append(out, Role{Name: name, Current: int32(replicas)})
	}
	return out, ""
}

// Plan returns the replicas to write for roles, given the decision for each
// role that has one, clamped to [lo, hi]. A role with no decision keeps its
// count.
//
// It honours the DisaggregatedSet rule that every Static role is above zero
// or every one is zero (a CEL rule on the spec): a role is zeroed only when
// every role would be. Otherwise a role decided down to zero keeps
// max(lo, 1) groups, so one role is never parked alone -- the set would
// reject the write, and a prefill with no decode serves nothing.
func Plan(roles []Role, decided map[string]int32, lo, hi int32) map[string]int32 {
	out := make(map[string]int32, len(roles))
	allZero := len(roles) > 0
	for _, r := range roles {
		n := r.Current
		if d, ok := decided[r.Name]; ok {
			n = d
			if n > 0 {
				n = min(max(n, lo), hi)
			}
		}
		out[r.Name] = n
		if n > 0 {
			allZero = false
		}
	}
	if !allZero {
		for name, n := range out {
			if n <= 0 {
				out[name] = max(lo, 1)
			}
		}
	}
	return out
}

// ApplyConfiguration is the server-side apply document that sets the replicas
// of the named roles of set and nothing else. spec.roles is a list map keyed
// by name, so the apply owns only spec.roles[name=<role>].spec.replicas of
// each role it names.
func ApplyConfiguration(set *unstructured.Unstructured, replicas map[string]int32) *unstructured.Unstructured {
	names := make([]string, 0, len(replicas))
	for n := range replicas {
		names = append(names, n)
	}
	sort.Strings(names)
	roles := make([]any, 0, len(names))
	for _, n := range names {
		roles = append(roles, map[string]any{"name": n, "spec": map[string]any{"replicas": int64(replicas[n])}})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": constants.DisaggregatedSetAPIVersion,
		"kind":       constants.DisaggregatedSetKind,
		"metadata":   map[string]any{"name": set.GetName(), "namespace": set.GetNamespace()},
		"spec":       map[string]any{"roles": roles},
	}}
}

// TargetName is the scale-target name of role in set: <set>-<role>.
func TargetName(set, role string) string { return set + "-" + role }

// EntryName is the registry entry of role in set, as a ScaledObject the plan
// would write for it is named: <set>-<role>-llmsm.
func EntryName(set, role string) string { return TargetName(set, role) + "-llmsm" }

func (w *Writer) reconcile(ctx context.Context, set *unstructured.Unstructured) {
	mode := set.GetAnnotations()[OptInAnnotation]
	if mode != "true" && mode != "force" {
		return
	}
	logger := ctrl.LoggerFrom(ctx).WithValues("namespace", set.GetNamespace(), "disaggregatedSet", set.GetName())
	model := set.GetAnnotations()[ModelIDAnnotation]
	if model == "" {
		w.event(set, corev1.EventTypeWarning, EventRefused, "no "+ModelIDAnnotation+" annotation: the role writer cannot attribute this set to a model")
		return
	}
	roles, why := StaticRoles(set)
	if why != "" {
		w.event(set, corev1.EventTypeWarning, EventRefused, why)
		return
	}
	lo := annotationInt(set, MinReplicasAnnotation, defaultMin)
	hi := max(annotationInt(set, MaxReplicasAnnotation, defaultMax), max(lo, 1))

	decided := map[string]int32{}
	for _, r := range roles {
		w.register(set, r.Name, model, lo, hi)
		if d, ok := w.Decisions.Get(set.GetNamespace(), TargetName(set.GetName(), r.Name)); ok && w.honours(d) {
			decided[r.Name] = d.DesiredReplicas
		}
	}
	plan := Plan(roles, decided, lo, hi)
	changed := map[string]int32{}
	for _, r := range roles {
		if plan[r.Name] != r.Current {
			changed[r.Name] = plan[r.Name]
		}
	}
	if len(changed) == 0 {
		return
	}
	// Every Static role is written together, so the CEL rule is checked
	// against one consistent document, and a rejected write leaves no role
	// half-applied.
	opts := []client.PatchOption{client.FieldOwner(FieldManager)}
	if mode == "force" {
		opts = append(opts, client.ForceOwnership)
	}
	// Patch with the Apply patch type rather than Client.Apply: the document is
	// unstructured, for a kind this module has no apply configuration for.
	if err := w.Client.Patch(ctx, ApplyConfiguration(set, plan), client.Apply, opts...); err != nil { //nolint:staticcheck // SA1019: see above

		if apierrors.IsConflict(err) {
			w.event(set, corev1.EventTypeWarning, EventConflict,
				"another field manager owns spec.roles[].spec.replicas (GitOps, kubectl apply): remove replicas from that source, or set "+OptInAnnotation+"=force: "+err.Error())
			return
		}
		logger.Error(err, "Could not write the role replicas", "replicas", plan)
		return
	}
	logger.Info("Wrote DisaggregatedSet role replicas", "replicas", changed)
	w.event(set, corev1.EventTypeNormal, EventScaled, fmt.Sprintf("role replicas %v", changed))
}

// register records role of set in the registry, as a KEDA call records a
// ScaledObject, and sets its target. Repeated every pass, it keeps the entry
// live; a set that opts out simply stops refreshing it and it expires.
func (w *Writer) register(set *unstructured.Unstructured, role, model string, lo, hi int32) {
	if w.Registry == nil {
		return
	}
	md := map[string]string{
		registry.ModelIDKey: model,
		registry.SourceKey:  registry.SourceRoleWriter,
	}
	if p := set.GetAnnotations()[ScalingPolicyAnnotation]; p != "" {
		md[registry.ScalingPolicyKey] = p
	}
	ns, name := set.GetNamespace(), EntryName(set.GetName(), role)
	w.Registry.Observe(ns, name, md)
	w.Registry.SetTarget(ns, name, registry.Target{
		APIVersion:  constants.DisaggregatedSetAPIVersion,
		Kind:        constants.DisaggregatedSetRoleScalerKind,
		Name:        TargetName(set.GetName(), role),
		MinReplicas: &lo,
		MaxReplicas: &hi,
		Labels:      set.GetLabels(),
	})
}

// honours applies the external scaler's staleness rule: a decision to park
// (0) never expires, a positive one only speaks for its role while fresh.
func (w *Writer) honours(d decision.Decision) bool {
	if d.DesiredReplicas <= 0 {
		return true
	}
	ttl := w.DecisionTTL
	if ttl <= 0 {
		ttl = DefaultDecisionTTL
	}
	now := time.Now
	if w.Now != nil {
		now = w.Now
	}
	return now().Sub(d.UpdatedAt) <= ttl
}

func (w *Writer) event(set *unstructured.Unstructured, kind, reason, msg string) {
	if w.Recorder != nil {
		w.Recorder.Event(set, kind, reason, msg)
	}
}

func annotationInt(set *unstructured.Unstructured, key string, def int32) int32 {
	v, ok := set.GetAnnotations()[key]
	if !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 0 {
		return def
	}
	return int32(n)
}
