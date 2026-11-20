package variantmeta

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// Why a variant's missing replicas have not arrived, read from its pods
// (ExplainMissing). Each is a value of the reason label of
// llmsm_replicas_not_arriving, so the set is closed and small.
const (
	// NotArrivingAdmissionRejected is a pod the kubelet refused at admission:
	// phase Failed with UnexpectedAdmissionError, OutOf<resource> or a node
	// affinity mismatch. The node took the pod and could not run it, usually
	// because it advertises devices it cannot allocate.
	NotArrivingAdmissionRejected = "admission-rejected"
	// NotArrivingCrashLoop is a container that starts and exits.
	NotArrivingCrashLoop = "crash-loop"
	// NotArrivingImagePull is an image that does not pull.
	NotArrivingImagePull = "image-pull"
	// NotArrivingContainerConfig is a container the kubelet cannot create
	// (a missing Secret or ConfigMap, a bad command).
	NotArrivingContainerConfig = "container-config"
	// NotArrivingUnschedulable is a pod the scheduler places nowhere.
	NotArrivingUnschedulable = "unschedulable"
	// NotArrivingSchedulingGated is a pod held at a scheduling gate (Kueue).
	NotArrivingSchedulingGated = "scheduling-gated"
	// NotArrivingFailed is any other pod that ended Failed (evicted, killed).
	NotArrivingFailed = "failed"
	// NotArrivingNotReady is a pod Running whose readiness probe has not
	// passed.
	NotArrivingNotReady = "not-ready"
	// NotArrivingStarting is a pod still on its way up (Pending and placed:
	// pulling, initializing) past the expected start.
	NotArrivingStarting = "starting"
	// NotArrivingNoPods is a shortfall with no pod to explain it: the
	// workload controller did not create them (a ResourceQuota, an admission
	// webhook). Its own events say why.
	NotArrivingNoPods = "no-pods"
)

// notArrivingOrder ranks the reasons from the most specific cause to the
// least: when a variant's pods show several, the first one with a pod wins.
// A rejection storm also leaves a fresh replacement Pending; the rejection is
// the cause and the Pending pod its symptom.
var notArrivingOrder = []string{
	NotArrivingAdmissionRejected,
	NotArrivingCrashLoop,
	NotArrivingImagePull,
	NotArrivingContainerConfig,
	NotArrivingUnschedulable,
	NotArrivingSchedulingGated,
	NotArrivingFailed,
	NotArrivingNotReady,
	NotArrivingStarting,
}

// maxCauseMessage bounds the pod message an ArrivalCause carries. A scheduler
// message lists every node group and can run to kilobytes.
const maxCauseMessage = 300

// ArrivalCause is why a variant's missing replicas have not arrived.
type ArrivalCause struct {
	// Reason is one of the NotArriving* values.
	Reason string
	// Pods is how many of the variant's pods show Reason.
	Pods int
	// Node is the node most of those pods were placed on, "" when none was
	// placed (unschedulable, gated) or when there are no pods.
	Node string
	// Detail is the pod's own words for it: the kubelet's or the scheduler's
	// message, or a container's waiting reason and last exit. From the newest
	// such pod, cut to maxCauseMessage bytes. May be empty.
	Detail string
}

// ExplainMissing reads why a variant's pods are not serving. pods are the
// variant's pods; Ready pods and pods being deleted are skipped.
//
// A pod still starting, or Running and not Ready, counts only when it was
// created before stalledBefore: a younger one is on its way, and a rollout
// or a scale-up that is making progress is not a fault. When such pods are
// all the variant has, the result's Reason is "" -- nothing to report.
//
// Pods that ended (Failed, Succeeded) count only when created at or after
// notBefore, so a pod evicted last week does not explain a shortfall that
// began today. A lone ended pod older than the newest live one has been
// replaced: it does not hide the cause the replacement shows.
//
// The result's Reason is NotArrivingNoPods when no pod explains anything.
func ExplainMissing(pods []corev1.Pod, notBefore, stalledBefore time.Time) ArrivalCause {
	type found struct {
		pods   int
		nodes  map[string]int
		newest time.Time
		detail string
	}
	byReason := map[string]*found{}
	progressing := 0
	var newestLive time.Time
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || PodReady(p) {
			continue
		}
		created := p.CreationTimestamp.Time
		terminal := p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded
		if terminal && created.Before(notBefore) {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		if !terminal && created.After(newestLive) {
			newestLive = created
		}
		reason, detail := classifyPod(p)
		if (reason == NotArrivingStarting || reason == NotArrivingNotReady) && !created.Before(stalledBefore) {
			progressing++
			continue
		}
		f := byReason[reason]
		if f == nil {
			f = &found{nodes: map[string]int{}}
			byReason[reason] = f
		}
		f.pods++
		if p.Spec.NodeName != "" {
			f.nodes[p.Spec.NodeName]++
		}
		if f.pods == 1 || created.After(f.newest) {
			f.newest = created
			if detail != "" {
				f.detail = detail
			}
		}
	}
	for _, reason := range notArrivingOrder {
		f := byReason[reason]
		if f == nil {
			continue
		}
		if endedReason(reason) && f.pods == 1 && f.newest.Before(newestLive) {
			continue
		}
		return ArrivalCause{Reason: reason, Pods: f.pods, Node: busiestNode(f.nodes), Detail: cut(f.detail)}
	}
	if progressing > 0 {
		return ArrivalCause{}
	}
	return ArrivalCause{Reason: NotArrivingNoPods}
}

// endedReason reports whether reason is read from pods that ended.
func endedReason(reason string) bool {
	return reason == NotArrivingAdmissionRejected || reason == NotArrivingFailed
}

// classifyPod names why one not-Ready, live pod is not serving, and its words
// for it.
func classifyPod(p *corev1.Pod) (reason, detail string) {
	if p.Status.Phase == corev1.PodFailed {
		detail = said(p.Status.Reason, p.Status.Message)
		if admissionRejected(p.Status.Reason) {
			return NotArrivingAdmissionRejected, detail
		}
		return NotArrivingFailed, detail
	}
	for _, c := range p.Status.Conditions {
		if c.Type != corev1.PodScheduled || c.Status != corev1.ConditionFalse {
			continue
		}
		switch c.Reason {
		case corev1.PodReasonUnschedulable:
			return NotArrivingUnschedulable, c.Message
		case corev1.PodReasonSchedulingGated:
			return NotArrivingSchedulingGated, c.Message
		}
	}
	for _, group := range [][]corev1.ContainerStatus{
		p.Status.InitContainerStatuses, p.Status.ContainerStatuses,
	} {
		for _, cs := range group {
			w := cs.State.Waiting
			if w == nil {
				continue
			}
			switch w.Reason {
			case "CrashLoopBackOff":
				return NotArrivingCrashLoop, crashDetail(cs)
			case "ImagePullBackOff", "ErrImagePull", "InvalidImageName":
				return NotArrivingImagePull, said(w.Reason, w.Message)
			case "CreateContainerError", "CreateContainerConfigError":
				return NotArrivingContainerConfig, said(w.Reason, w.Message)
			}
		}
	}
	// A container that exited and was restarted is crash-looping even while
	// it runs again: an engine that loads for minutes before it dies would
	// otherwise flip between not-ready and crash-loop on every restart.
	for _, cs := range p.Status.ContainerStatuses {
		if cs.RestartCount > 0 && cs.LastTerminationState.Terminated != nil {
			return NotArrivingCrashLoop, crashDetail(cs)
		}
	}
	if p.Status.Phase == corev1.PodRunning {
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady {
				return NotArrivingNotReady, c.Message
			}
		}
		return NotArrivingNotReady, ""
	}
	return NotArrivingStarting, ""
}

// admissionRejected reports whether a Failed pod's reason is the kubelet
// refusing it at admission. The kubelet sets UnexpectedAdmissionError when a
// device plugin cannot allocate, OutOf<resource> when the node is short of
// one, and NodeAffinity when the node no longer matches.
func admissionRejected(reason string) bool {
	switch {
	case reason == "UnexpectedAdmissionError", reason == "NodeAffinity":
		return true
	case strings.HasPrefix(reason, "OutOf"):
		return true
	}
	return false
}

// crashDetail is a crash-looping container's name and how it last exited.
// No restart count: it moves on every restart, and the detail lands in the
// controller-status line.
func crashDetail(cs corev1.ContainerStatus) string {
	t := cs.LastTerminationState.Terminated
	if t == nil {
		return fmt.Sprintf("container %s: CrashLoopBackOff", cs.Name)
	}
	return fmt.Sprintf("container %s restarting, last exit %s (code %d)", cs.Name, t.Reason, t.ExitCode)
}

// busiestNode is the node with the most pods, ties to the first by name so
// the answer does not move between cycles.
func busiestNode(nodes map[string]int) string {
	names := make([]string, 0, len(nodes))
	for n := range nodes {
		names = append(names, n)
	}
	sort.Strings(names)
	best := ""
	for _, n := range names {
		if best == "" || nodes[n] > nodes[best] {
			best = n
		}
	}
	return best
}

// CutDetail bounds a cause's detail as ExplainMissing does, for a caller
// that writes one itself.
func CutDetail(s string) string { return cut(s) }

// cut bounds s to maxCauseMessage bytes, on a rune boundary.
func cut(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxCauseMessage {
		return s
	}
	end := maxCauseMessage
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "..."
}

// said joins a reason and a message as "reason: message", leaving out
// whichever is empty.
func said(reason, message string) string {
	reason, message = strings.TrimSpace(reason), strings.TrimSpace(message)
	switch {
	case reason == "":
		return message
	case message == "":
		return reason
	}
	return reason + ": " + message
}

// ErrNoPodSelector is returned by ListPods for a scale target whose pods it
// cannot select: no group labels and no pod template labels.
var ErrNoPodSelector = errors.New("the scale target names no labels to select its pods by")

// ListPods lists a variant's pods: by the labels every pod of its group
// carries for a LeaderWorkerSet or a DisaggregatedSet role
// (scaletarget.GroupPodLabels), else by its pod template's labels, the way
// discovery does.
func ListPods(
	ctx context.Context,
	k8sClient client.Reader,
	namespace string,
	scaleTarget scaletarget.ScaleTargetAccessor,
) ([]corev1.Pod, error) {
	if k8sClient == nil || scaleTarget == nil {
		return nil, ErrNoPodSelector
	}
	labels := scaletarget.GroupPodLabels(scaleTarget)
	if len(labels) == 0 {
		if tmpl := scaleTarget.GetLeaderPodTemplateSpec(); tmpl != nil {
			labels = tmpl.Labels
		}
	}
	if len(labels) == 0 {
		return nil, ErrNoPodSelector
	}
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods,
		client.InNamespace(namespace), client.MatchingLabels(labels)); err != nil {
		return nil, err
	}
	return pods.Items, nil
}
