package preflight

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// dsetRoleScalerScale is the subresource KEDA drives, through the HPA, for
// one role of a DisaggregatedSet. LeaderWorkerSet serves it from v0.10.0.
const dsetRoleScalerScale = "disaggregatedsetrolescalers/scale"

// The LeaderWorkerSet controller as its Helm chart and its release manifests
// install it: the label every Deployment of the chart carries, and the name
// and namespace both use.
const (
	lwsControllerSelector   = "app.kubernetes.io/name=lws"
	lwsControllerDeployment = "lws-controller-manager"
	lwsControllerNamespace  = "lws-system"
)

// lwsRoleScalerVersion is scaletarget.LWSRoleScalerRelease as a semver.
var lwsRoleScalerVersion, _ = imageVersion("lws:" + scaletarget.LWSRoleScalerRelease)

// checkLWSDisaggregatedSet reads, by discovery, whether the cluster's
// LeaderWorkerSet serves DisaggregatedSet and, if it does, whether it serves
// the role scaler's /scale subresource that KEDA needs. Discovery needs no
// RBAC beyond what every authenticated account has. The capability is read
// from the API; discovery reads the CRDs only, so when the API serves the
// role scaler the controller's image tag is read too (lwsControllerVersion):
// CRDs upgraded ahead of the controller serve the API, and the old controller
// creates no role scalers.
func checkLWSDisaggregatedSet(ctx context.Context, s *state) ([]Result, string) {
	list, err := s.env.Kube.Discovery().ServerResourcesForGroupVersion(constants.DisaggregatedSetAPIVersion)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, "no DisaggregatedSet API: LeaderWorkerSet v0.9.0 or later is not installed"
		}
		return []Result{{Status: Unknown, Detail: "cannot read the API resources of " + constants.DisaggregatedSetAPIVersion + ": " + firstLine(err.Error())}}, ""
	}
	sets, scale := false, false
	for _, r := range list.APIResources {
		switch r.Name {
		case "disaggregatedsets":
			sets = true
		case dsetRoleScalerScale:
			scale = true
		}
	}
	switch {
	case !sets:
		return nil, "no DisaggregatedSet API: LeaderWorkerSet v0.9.0 or later is not installed"
	case !scale:
		return []Result{{Status: Warn, Detail: scaletarget.RoleScalerUnservedMessage,
			Fix: scaletarget.RoleScalerUnservedFix}}, ""
	}
	v, where, found := lwsControllerVersion(ctx, s)
	if found && v.less(lwsRoleScalerVersion) {
		return []Result{{Status: Warn, Detail: fmt.Sprintf("the API serves %s, but the LeaderWorkerSet controller is v%s (%s), older than %s: "+
			"the CRDs were upgraded without the controller. It creates no DisaggregatedSetRoleScaler, so a role in scaling.mode External "+
			"has nothing for KEDA to drive; in passive mode such a role is not observed at all, because KEDA never calls the trigger",
			dsetRoleScalerScale, v, where, scaletarget.LWSRoleScalerRelease),
			Fix: scaletarget.RoleScalerUnservedFix}}, ""
	}
	if found {
		return []Result{{Status: Pass, Detail: fmt.Sprintf("the API serves %s and the LeaderWorkerSet controller is v%s (%s): "+
			"KEDA can scale DisaggregatedSet roles in scaling.mode External", dsetRoleScalerScale, v, where)}}, ""
	}
	// The controller was not found or names no version: say what was read.
	return []Result{{Status: Pass, Detail: "the API serves " + dsetRoleScalerScale + ": KEDA can scale DisaggregatedSet roles in scaling.mode External, " +
		"provided the LeaderWorkerSet controller is " + scaletarget.LWSRoleScalerRelease + " or later too (new CRDs alone create no role scalers; " +
		where + ")"}}, ""
}

// lwsControllerVersion reads the LeaderWorkerSet controller's version from
// its image tag: the Deployments labelled lwsControllerSelector in every
// namespace, or, where those may not be listed, lws-controller-manager in
// lws-system by name. found is false when neither read finds a Deployment
// whose image names a version; where then says why, for the PASS detail.
func lwsControllerVersion(ctx context.Context, s *state) (v semver, where string, found bool) {
	var deps []appsv1.Deployment
	list, err := s.env.Kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: lwsControllerSelector})
	if err == nil {
		deps = list.Items
	}
	if len(deps) == 0 {
		d, gerr := s.env.Kube.AppsV1().Deployments(lwsControllerNamespace).Get(ctx, lwsControllerDeployment, metav1.GetOptions{})
		if gerr != nil {
			return semver{}, "its version was not read: no Deployment labelled " + lwsControllerSelector +
				" could be listed, nor " + lwsControllerNamespace + "/" + lwsControllerDeployment + " read", false
		}
		deps = []appsv1.Deployment{*d}
	}
	for _, d := range deps {
		for _, c := range d.Spec.Template.Spec.Containers {
			// The manager's image, not a sidecar's (an auth proxy has its own version).
			if !strings.HasPrefix(c.Image[strings.LastIndex(c.Image, "/")+1:], "lws:") {
				continue
			}
			if cv, ok := imageVersion(c.Image); ok {
				return cv, d.Namespace + "/" + d.Name + " " + c.Image, true
			}
		}
	}
	d := deps[0]
	return semver{}, fmt.Sprintf("its version was not read: %s/%s runs no lws image whose tag names a version", d.Namespace, d.Name), false
}
