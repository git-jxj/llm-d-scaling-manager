package dsetwriter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// A cluster can refuse a scale-down the writer asks for, at either of two
// levels. An admission policy or webhook on the DisaggregatedSet denies the
// write itself; one on the child LeaderWorkerSets lets the write through and
// denies the set controller's update of the child, so the set's spec says N-1
// while N groups keep running. Clusters that drain a group before removing it
// do this on purpose: any decrease without a drain is denied. This file finds
// both, reports them, and keeps the writer from lowering the role again.

// EventScaleDownRefused is the reason of the Warning event recorded when the
// cluster refuses a lower count for a role: the API server denied the write,
// or the role's child LeaderWorkerSets did not follow it within
// Writer.FollowTimeout.
const EventScaleDownRefused = "RoleScaleDownRefused"

// DefaultFollowTimeout is how long the child LeaderWorkerSets of a role may
// take to follow a lower count before the writer reports the scale-down as
// refused, when Writer.FollowTimeout is unset. The set controller updates the
// children within seconds; a rollout that defers the scale-down stops the
// clock.
const DefaultFollowTimeout = 2 * time.Minute

// DefaultDeniedRetry is the first wait before the writer asks again for a
// lower count the API server denied, when Writer.DeniedRetry is unset. Each
// further denial doubles it, up to MaxDeniedRetry.
const DefaultDeniedRetry = 5 * time.Minute

// MaxDeniedRetry is the longest wait between two tries of a denied scale-down.
const MaxDeniedRetry = time.Hour

// revisionLabelKey is the template revision a child LeaderWorkerSet of a
// DisaggregatedSet runs.
const revisionLabelKey = "disaggregatedset.x-k8s.io/revision"

// noMatch is the failure kind of a read whose kind the API server does not serve.
const noMatch = "NoMatch"

var lwsListGVK = schema.GroupVersionKind{Group: "leaderworkerset.x-k8s.io", Version: "v1", Kind: "LeaderWorkerSetList"}

// scaleDown is a lower count the writer wrote for one role, or tried to.
type scaleDown struct {
	// from and to are the role's count before and the count asked for.
	from, to int32
	// since is when the children should have started to follow: the write,
	// or the last pass that saw a rollout defer it.
	since time.Time
	// refused is set once the scale-down has been reported as refused.
	refused bool
	// denied is set when the API server denied the write itself; tries counts
	// the denials and retry is when the writer may ask again.
	denied bool
	tries  int
	retry  time.Time
	// reason is why it was refused, for the log of a held pass.
	reason string
}

// childReplicas is what the child LeaderWorkerSets of one role run.
type childReplicas struct {
	// replicas is the sum of spec.replicas over the role's children.
	replicas int32
	// revisions is the number of revisions with groups: more than one is a
	// rollout.
	revisions int
}

func (w *Writer) followTimeout() time.Duration {
	if w.FollowTimeout > 0 {
		return w.FollowTimeout
	}
	return DefaultFollowTimeout
}

func (w *Writer) deniedRetry(tries int) time.Duration {
	d := w.DeniedRetry
	if d <= 0 {
		d = DefaultDeniedRetry
	}
	for i := 1; i < tries && d < MaxDeniedRetry; i++ {
		d *= 2
	}
	return min(d, MaxDeniedRetry)
}

// followScaleDowns checks the scale-downs recorded for set's roles and
// returns the roles the writer must not lower this pass, each with why.
//
//   - A record whose role no longer has the count it was about (someone else
//     changed it, or the writer raised it) is dropped.
//   - A denied write is held until its retry is due.
//   - A written one is followed when the children run no more than it asked
//     for. Until then, a rollout that defers the scale-down restarts the
//     clock; past FollowTimeout without one, it is refused: reported once,
//     and held until the children follow or the count is raised.
func (w *Writer) followScaleDowns(ctx context.Context, set *unstructured.Unstructured, roles []Role, logger logr.Logger) map[string]string {
	key := types.NamespacedName{Namespace: set.GetNamespace(), Name: set.GetName()}
	now := w.now()
	held := map[string]string{}
	var children map[string]childReplicas
	var childErr error
	read := false
	for _, r := range roles {
		k := roleKey{key, r.Name}
		w.mu.Lock()
		sd := w.scaleDowns[k]
		w.mu.Unlock()
		if sd == nil {
			continue
		}
		if sd.denied {
			if r.Current != sd.from {
				w.dropScaleDown(k)
				continue
			}
			if now.Before(sd.retry) {
				held[r.Name] = sd.reason
			}
			continue
		}
		if r.Current != sd.to {
			w.dropScaleDown(k)
			continue
		}
		if !read {
			children, childErr = w.readChildren(ctx, set)
			read = true
			w.childReadFailure(key, childErr, logger)
		}
		if childErr != nil {
			// Unknown is neither followed nor refused: keep the record and
			// the clock, and hold a refusal already reported.
			if sd.refused {
				held[r.Name] = sd.reason
			}
			continue
		}
		c := children[r.Name]
		if c.replicas <= sd.to {
			if sd.refused {
				logger.Info("The cluster applied the role's scale-down it had refused: the role writer may lower it again",
					"role", r.Name, "replicas", sd.to)
			}
			w.dropScaleDown(k)
			continue
		}
		if c.revisions > 1 && scaleDownDeferred(set) {
			w.mu.Lock()
			sd.since = now
			w.mu.Unlock()
			if sd.refused {
				held[r.Name] = sd.reason
			}
			continue
		}
		if sd.refused {
			held[r.Name] = sd.reason
			continue
		}
		if now.Sub(sd.since) < w.followTimeout() {
			continue
		}
		reason := fmt.Sprintf("role %s: the writer set %d groups %s ago, and its child LeaderWorkerSets still run %d. "+
			"Something in the cluster refuses the decrease, such as an admission policy that requires a drain first%s. "+
			"The writer will not lower this role again until the children follow or the count is raised",
			r.Name, sd.to, now.Sub(sd.since).Round(time.Second), c.replicas, conditionsSay(set))
		w.mu.Lock()
		sd.refused, sd.reason = true, reason
		w.mu.Unlock()
		logger.Info("The cluster refused a role scale-down the writer wrote", "role", r.Name,
			"written", sd.to, "running", c.replicas, "waited", now.Sub(sd.since).Round(time.Second))
		w.event(set, corev1.EventTypeWarning, EventScaleDownRefused, reason)
		held[r.Name] = reason
	}
	return held
}

// recordWrite updates the scale-down records after an accepted write of plan:
// a lower count starts (or keeps) one, a higher one drops it.
func (w *Writer) recordWrite(set types.NamespacedName, roles []Role, plan map[string]int32) {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.scaleDowns == nil {
		w.scaleDowns = map[roleKey]*scaleDown{}
	}
	for _, r := range roles {
		k := roleKey{set, r.Name}
		n := plan[r.Name]
		switch {
		case n > r.Current:
			delete(w.scaleDowns, k)
		case n < r.Current:
			// A further decrease while the last one is still awaited keeps
			// the clock of the first: the children owe both.
			if sd := w.scaleDowns[k]; sd != nil && !sd.denied {
				sd.to = n
				continue
			}
			w.scaleDowns[k] = &scaleDown{from: r.Current, to: n, since: now}
		}
	}
}

// recordDenied records a write of plan the API server denied, for each role
// it lowered, and returns those roles. The next try of a role waits
// deniedRetry, doubled for each denial in a row.
func (w *Writer) recordDenied(set types.NamespacedName, roles []Role, plan map[string]int32, msg string) []string {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.scaleDowns == nil {
		w.scaleDowns = map[roleKey]*scaleDown{}
	}
	var lowered []string
	for _, r := range roles {
		if plan[r.Name] >= r.Current {
			continue
		}
		k := roleKey{set, r.Name}
		sd := w.scaleDowns[k]
		if sd == nil || !sd.denied || sd.from != r.Current {
			sd = &scaleDown{from: r.Current, denied: true}
			w.scaleDowns[k] = sd
		}
		sd.to = plan[r.Name]
		sd.tries++
		sd.refused = true
		sd.retry = now.Add(w.deniedRetry(sd.tries))
		sd.reason = fmt.Sprintf("role %s: the API server denied lowering it from %d to %d: %s", r.Name, r.Current, sd.to, msg)
		lowered = append(lowered, r.Name)
	}
	return lowered
}

// holdRefused keeps every held role of plan at least at its current count,
// then restores the all-zero-or-all-non-zero rule: if a held role keeps
// groups, a role planned to zero keeps its own count, or max(lo, 1) when it
// has none.
func holdRefused(roles []Role, plan map[string]int32, held map[string]string, lo int32) map[string]int32 {
	if len(held) == 0 {
		return plan
	}
	out := make(map[string]int32, len(plan))
	running := false
	for _, r := range roles {
		n := plan[r.Name]
		if _, ok := held[r.Name]; ok {
			n = max(n, r.Current)
		}
		out[r.Name] = n
		if n > 0 {
			running = true
		}
	}
	if running {
		for _, r := range roles {
			if out[r.Name] <= 0 {
				out[r.Name] = r.Current
				if out[r.Name] <= 0 {
					out[r.Name] = max(lo, 1)
				}
			}
		}
	}
	return out
}

// isDenial reports whether err is an admission policy or webhook denying a
// write: Forbidden or Invalid, with the "denied" of both messages
// ("ValidatingAdmissionPolicy ... denied request", "admission webhook ...
// denied the request"). A missing RBAC grant is Forbidden too, but says
// "cannot patch", not denied.
func isDenial(err error) bool {
	if !apierrors.IsForbidden(err) && !apierrors.IsInvalid(err) {
		return false
	}
	return strings.Contains(err.Error(), "denied")
}

// readChildren sums spec.replicas of the child LeaderWorkerSets of set by
// role, and counts the revisions of each role that run groups.
func (w *Writer) readChildren(ctx context.Context, set *unstructured.Unstructured) (map[string]childReplicas, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(lwsListGVK)
	if err := w.Client.List(ctx, list, client.InNamespace(set.GetNamespace()),
		client.MatchingLabels{constants.DisaggregatedSetNameLabelKey: set.GetName()}); err != nil {
		return nil, err
	}
	out := map[string]childReplicas{}
	revisions := map[string]map[string]bool{}
	for i := range list.Items {
		lws := &list.Items[i]
		role := lws.GetLabels()[constants.DisaggregatedSetRoleLabelKey]
		if role == "" || lws.GetDeletionTimestamp() != nil {
			continue
		}
		n, found, _ := unstructured.NestedInt64(lws.Object, "spec", "replicas")
		if !found {
			n = 1
		}
		c := out[role]
		c.replicas += clampInt32(n)
		if n > 0 {
			if revisions[role] == nil {
				revisions[role] = map[string]bool{}
			}
			revisions[role][lws.GetLabels()[revisionLabelKey]] = true
		}
		c.revisions = len(revisions[role])
		out[role] = c
	}
	return out, nil
}

// childReadFailure logs a failed read of a set's children once per kind of
// failure, and clears it on a read that worked.
func (w *Writer) childReadFailure(set types.NamespacedName, err error, logger logr.Logger) {
	w.mu.Lock()
	if w.childFailed == nil {
		w.childFailed = map[types.NamespacedName]string{}
	}
	if err == nil {
		delete(w.childFailed, set)
		w.mu.Unlock()
		return
	}
	kind := string(apierrors.ReasonForError(err))
	if meta.IsNoMatchError(err) {
		kind = noMatch
	}
	same := w.childFailed[set] == kind
	w.childFailed[set] = kind
	w.mu.Unlock()
	if !same {
		logger.Error(err, "Could not read the child LeaderWorkerSets of the set: the role writer cannot tell whether the cluster applied a scale-down it wrote. "+
			"It needs list on leaderworkersets, which the controller's role grants")
	}
}

// dropScaleDown forgets the scale-down record of one role.
func (w *Writer) dropScaleDown(k roleKey) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.scaleDowns, k)
}

// scaleDownDeferred reports whether set holds a scale-down through a rollout:
// spec.scalingPolicy.scaleDownDuringRollout is Deferred, the default.
func scaleDownDeferred(set *unstructured.Unstructured) bool {
	p, _, _ := unstructured.NestedString(set.Object, "spec", "scalingPolicy", "scaleDownDuringRollout")
	return p != "Immediate"
}

// conditionsSay is the set's status conditions that report a problem, as
// "; the set's conditions say: <type> <reason>: <message>", or "" when none
// does. A condition whose status is not True is taken as one, as is any
// condition whose type or reason names an error or a failure.
func conditionsSay(set *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(set.Object, "status", "conditions")
	var says []string
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		status, _ := m["status"].(string)
		reason, _ := m["reason"].(string)
		msg, _ := m["message"].(string)
		bad := strings.ToLower(typ + " " + reason)
		if status == "True" && !strings.Contains(bad, "error") && !strings.Contains(bad, "fail") {
			continue
		}
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		says = append(says, fmt.Sprintf("%s=%s %s: %s", typ, status, reason, msg))
	}
	if len(says) == 0 {
		return ""
	}
	return "; the set's conditions say: " + strings.Join(says, "; ")
}
