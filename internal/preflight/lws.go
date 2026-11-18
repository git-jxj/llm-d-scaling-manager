package preflight

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/utils/scaletarget"
)

// dsetRoleScalerScale is the subresource KEDA drives, through the HPA, for
// one role of a DisaggregatedSet. LeaderWorkerSet serves it from v0.10.0.
const dsetRoleScalerScale = "disaggregatedsetrolescalers/scale"

// checkLWSDisaggregatedSet reads, by discovery, whether the cluster's
// LeaderWorkerSet serves DisaggregatedSet and, if it does, whether it serves
// the role scaler's /scale subresource that KEDA needs. Discovery needs no
// RBAC beyond what every authenticated account has. The capability is read,
// never the version: the version only names the release to upgrade to.
func checkLWSDisaggregatedSet(_ context.Context, s *state) ([]Result, string) {
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
	// Discovery reads the CRDs, not the controller: CRDs applied ahead of a
	// controller upgrade read the same, so the detail says what it saw.
	return []Result{{Status: Pass, Detail: "the API serves " + dsetRoleScalerScale + ": KEDA can scale DisaggregatedSet roles in scaling.mode External, " +
		"provided the LeaderWorkerSet controller is " + scaletarget.LWSRoleScalerRelease + " or later too (new CRDs alone create no role scalers)"}}, ""
}
