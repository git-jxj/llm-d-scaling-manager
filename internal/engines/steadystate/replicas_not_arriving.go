package steadystate

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/domain"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/allocation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/analyzers/saturation"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/engines/variantmeta"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/logging"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/metrics"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
	llmdvariant "github.com/llm-d-incubation/llm-d-scaling-manager/internal/variant"
)

// Replicas a variant asked for that never arrive (issue #224).
//
// A node that advertises devices it cannot allocate takes a pod, refuses it at
// admission, and the ReplicaSet replaces it on the same node, over and over.
// The node's health is the platform's to fix: the device plugin, the GPU
// operator, node remediation. What the scaling manager owes is to not count
// those replicas as supply and to say why they are missing, read from the
// pods, so the person on call goes to the node and not to the scaler.
//
// Ready supply already counts only replicas that serve. What this adds is the
// report, and one change to anticipated supply: a pod not Ready after twice
// its role's known start time is no longer counted as on its way
// (withholdOverdueArrivals).

const (
	// notArrivingStartFactor times the role's start time is how long a
	// shortfall lasts before it is reported, and how old a starting pod is
	// before it stops counting as on its way. The start time is a running
	// estimate, so a start a little over it is ordinary; twice it is not.
	notArrivingStartFactor = 2.0
	// notArrivingFloor is the least that wait can be, so a fast-starting
	// model does not report a rollout in progress.
	notArrivingFloor = 5 * time.Minute
	// notArrivingEventEvery is how often the Warning event repeats while the
	// cause stays the same. A new cause is reported at once.
	notArrivingEventEvery = 15 * time.Minute
	// notArrivingUnknown is the reason when the variant's pods could not be
	// listed.
	notArrivingUnknown = "unknown"
)

// notArrivingTracker is the state the report keeps between cycles.
type notArrivingTracker struct {
	// variants is keyed namespace/variant.
	variants map[string]*notArrivingVariant
	// startByRole is each role's start time in seconds from the last cycle
	// that analysed its model (domain.AnalyzerResult.RoleStartSeconds),
	// keyed namespace/model/role. A role with neither a measurement nor a
	// seed is absent.
	startByRole map[string]float64
}

// notArrivingVariant is one variant whose ask is not met.
type notArrivingVariant struct {
	namespace, name string
	// since is when the shortfall was first seen. Kept in memory only: after
	// a restart a shortfall waits out the threshold again.
	since time.Time
	// reported is set once the shortfall passed the threshold.
	reported bool
	// reason is the cause last reported.
	reason string
	// lastEvent is when the last Warning event was recorded.
	lastEvent time.Time
	// line is the variant's controller-status line.
	line string
}

func (e *Engine) notArrivingState() *notArrivingTracker {
	if e.notArriving == nil {
		e.notArriving = &notArrivingTracker{
			variants:    map[string]*notArrivingVariant{},
			startByRole: map[string]float64{},
		}
	}
	return e.notArriving
}

func startRoleKey(namespace, modelID, role string) string {
	return namespace + "/" + modelID + "/" + domain.CanonicalRole(role)
}

// noteRoleStarts keeps each analysed role's start time for the next cycle.
func (t *notArrivingTracker) noteRoleStarts(requests []allocation.ModelScalingRequest) {
	for _, req := range requests {
		if req.CompositeSignal.Result == nil {
			continue
		}
		for role, s := range req.CompositeSignal.Result.RoleStartSeconds {
			if s > 0 {
				t.startByRole[startRoleKey(req.Namespace, req.ModelID, role)] = s
			}
		}
	}
}

// threshold is how long a shortfall or a starting pod of a role may last
// before the replica is taken as not arriving. known is false when the role's
// start time is neither measured nor seeded; the default start time is used
// then.
func (t *notArrivingTracker) threshold(namespace, modelID, role string) (d time.Duration, known bool) {
	s, known := t.startByRole[startRoleKey(namespace, modelID, role)]
	if !known {
		s = saturation.DefaultReplicaStartSeconds
	}
	return max(notArrivingFloor, time.Duration(notArrivingStartFactor*s*float64(time.Second))), known
}

// withholdOverdueArrivals stops counting as on its way a pod that has not
// become Ready in twice its role's start time: it moves the pod's age out of
// PendingAges and counts it in StuckReplicas, which anticipated supply
// subtracts. Only when the role's start time is known: with only the default,
// a slow model that was never measured would have its starting replicas
// ordered twice.
//
// A LeaderWorkerSet reports no pod ages (variantmeta.pendingAgeSeconds), so
// its groups are left as they were.
func (e *Engine) withholdOverdueArrivals(ctx context.Context, modelID string, metas []domain.VariantMetadata) {
	if e.notArriving == nil {
		return
	}
	for i := range metas {
		m := &metas[i]
		if len(m.PendingAges) == 0 {
			continue
		}
		limit, known := e.notArriving.threshold(m.Namespace, modelID, m.Role)
		if !known {
			continue
		}
		kept := make([]float64, 0, len(m.PendingAges))
		overdue := 0
		for _, age := range m.PendingAges {
			if age > limit.Seconds() {
				overdue++
				continue
			}
			kept = append(kept, age)
		}
		if overdue == 0 {
			continue
		}
		m.PendingAges = kept
		m.StuckReplicas += overdue
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Starting pods past twice their start time are not counted as arriving",
			"namespace", m.Namespace, "variant", m.VariantName, "overdue", overdue, "afterSeconds", limit.Seconds())
	}
}

// reportReplicasNotArriving compares, for every variant of the cycle, what
// its scale target asks for with what is Ready, and reports a shortfall that
// outlasts the threshold: a Warning event (on a new cause, then every
// notArrivingEventEvery), llmsm_replicas_not_arriving, and a line in the
// controller-status ConfigMap. A Normal event follows when the replicas
// arrive.
//
// scaleTargets is the cycle's scale targets by namespace/model, then by
// namespace/target; a variant not in it is read through the client. A scale
// target that cannot be read leaves the variant's state as it was.
func (e *Engine) reportReplicasNotArriving(
	ctx context.Context,
	modelGroups map[string][]llmdvariant.Variant,
	scaleTargets map[string]map[string]scaletarget.ScaleTargetAccessor,
	requests []allocation.ModelScalingRequest,
	now time.Time,
) {
	t := e.notArrivingState()
	t.noteRoleStarts(requests)
	seen := map[string]bool{}
	for _, modelVAs := range modelGroups {
		for i := range modelVAs {
			va := &modelVAs[i]
			key := utils.GetNamespacedKey(va.Namespace, va.Name)
			seen[key] = true
			st := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.Spec.ModelID)][utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
			if st == nil {
				if e.client == nil {
					continue
				}
				var err error
				st, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
				if err != nil {
					continue
				}
			}
			e.observeArrival(ctx, t, va, st, now)
		}
	}
	for key, v := range t.variants {
		if seen[key] {
			continue
		}
		if v.reported {
			metrics.ClearReplicasNotArriving(v.namespace, v.name)
		}
		delete(t.variants, key)
	}
	var lines []string
	for _, v := range t.variants {
		if v.reported {
			lines = append(lines, v.line)
		}
	}
	e.Status.SetReplicasNotArriving(lines)
}

// observeArrival updates one variant's shortfall and reports it.
func (e *Engine) observeArrival(
	ctx context.Context,
	t *notArrivingTracker,
	va *llmdvariant.Variant,
	st scaletarget.ScaleTargetAccessor,
	now time.Time,
) {
	key := utils.GetNamespacedKey(va.Namespace, va.Name)
	ask := 0
	if r := st.GetReplicas(); r != nil {
		ask = int(*r)
	}
	ready := int(st.GetStatusReadyReplicas())
	v := t.variants[key]
	if ask <= ready {
		if v != nil && v.reported {
			metrics.ClearReplicasNotArriving(va.Namespace, va.Name)
			e.recordArrivalEvent(va, corev1.EventTypeNormal, constants.K8SEventReplicasArrived,
				fmt.Sprintf("%d of %d replicas Ready, after %s short", ready, ask, now.Sub(v.since).Round(time.Second)))
		}
		delete(t.variants, key)
		return
	}
	if v == nil {
		v = &notArrivingVariant{namespace: va.Namespace, name: va.Name, since: now}
		t.variants[key] = v
	}
	role := variantmeta.RoleFromScaleTarget(st)
	limit, _ := t.threshold(va.Namespace, va.Spec.ModelID, role)
	short := now.Sub(v.since)
	if short < limit {
		return
	}
	cause := variantmeta.ArrivalCause{Reason: notArrivingUnknown}
	pods, err := variantmeta.ListPods(ctx, e.client, va.Namespace, st)
	if err != nil {
		cause.Detail = "its pods could not be listed: " + err.Error()
	} else {
		// Pods that ended count from one threshold before the shortfall
		// began: the rejections that started it, not last week's evictions.
		cause = variantmeta.ExplainMissing(pods, v.since.Add(-limit))
	}
	metrics.SetReplicasNotArriving(va.Namespace, va.Name, role, cause.Reason, ask-ready)
	v.line = notArrivingLine(va, role, ready, ask, v.since, cause)
	if !v.reported || cause.Reason != v.reason || now.Sub(v.lastEvent) >= notArrivingEventEvery {
		e.recordArrivalEvent(va, corev1.EventTypeWarning, constants.K8SEventReplicasNotArriving,
			notArrivingMessage(ready, ask, short, limit, cause))
		v.lastEvent = now
		ctrl.LoggerFrom(ctx).Info("Replicas asked for have not arrived",
			"namespace", va.Namespace, "variant", va.Name, "role", role, "ready", ready, "asked", ask,
			"shortSeconds", int(short.Seconds()), "reason", cause.Reason, "pods", cause.Pods,
			"node", cause.Node, "detail", cause.Detail)
	}
	v.reported, v.reason = true, cause.Reason
}

// recordArrivalEvent records straight on the recorder rather than through
// recordEvent, which keeps one event per variant per cycle: this report runs
// before the decisions, and would take the slot of the cycle's ScaledUp.
func (e *Engine) recordArrivalEvent(va *llmdvariant.Variant, eventType, reason, message string) {
	if e.Recorder == nil {
		return
	}
	e.Recorder.Event(llmdvariant.EventTarget(va), eventType, reason, message)
}

// notArrivingLine is a variant's controller-status line. It carries nothing
// that moves every cycle -- no age, no pod count -- so the ConfigMap is not
// rewritten every cycle while the shortfall lasts.
func notArrivingLine(va *llmdvariant.Variant, role string, ready, ask int, since time.Time, c variantmeta.ArrivalCause) string {
	line := fmt.Sprintf("%s/%s (%s): %d/%d Ready since %s: %s",
		va.Namespace, va.Name, role, ready, ask, since.UTC().Format(time.RFC3339), c.Reason)
	if c.Node != "" {
		line += " on node " + c.Node
	}
	if c.Detail != "" {
		line += ": " + c.Detail
	}
	return line
}

// notArrivingMessage is the Warning event's text: the shortfall, then the
// cause in the pods' words.
func notArrivingMessage(ready, ask int, short, limit time.Duration, c variantmeta.ArrivalCause) string {
	head := fmt.Sprintf("%d of %d replicas Ready for %s, longer than the %s a replica should take: ",
		ready, ask, short.Round(time.Second), limit.Round(time.Second))
	on := ""
	if c.Node != "" {
		on = " on node " + c.Node
	}
	detail := ""
	if c.Detail != "" {
		detail = " (" + c.Detail + ")"
	}
	switch c.Reason {
	case variantmeta.NotArrivingAdmissionRejected:
		return head + fmt.Sprintf("%s rejected by the kubelet at admission%s%s. "+
			"The node offers resources it cannot allocate; its health is for the platform "+
			"(device plugin, GPU operator, node remediation) to fix. The scaling manager does not move pods off a node.",
			podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingUnschedulable:
		return head + fmt.Sprintf("%s unschedulable%s.", podCount(c.Pods), detail)
	case variantmeta.NotArrivingSchedulingGated:
		return head + fmt.Sprintf("%s held at a scheduling gate, such as Kueue admission%s.", podCount(c.Pods), detail)
	case variantmeta.NotArrivingImagePull:
		return head + fmt.Sprintf("%s cannot pull the image%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingCrashLoop:
		return head + fmt.Sprintf("%s crash-looping%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingContainerConfig:
		return head + fmt.Sprintf("%s whose container cannot be created%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingFailed:
		return head + fmt.Sprintf("%s failed%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingNotReady:
		return head + fmt.Sprintf("%s Running but not Ready%s%s.", podCount(c.Pods), on, detail)
	case variantmeta.NotArrivingStarting:
		return head + fmt.Sprintf("%s still starting%s.", podCount(c.Pods), on)
	case variantmeta.NotArrivingNoPods:
		return head + "no pod exists for the missing replicas. The workload controller did not create them; " +
			"its FailedCreate events say why (often a ResourceQuota)."
	}
	return head + "the cause is not known" + detail + "."
}

// podCount is "1 pod" or "<n> pods".
func podCount(n int) string {
	if n == 1 {
		return "1 pod"
	}
	return fmt.Sprintf("%d pods", n)
}
