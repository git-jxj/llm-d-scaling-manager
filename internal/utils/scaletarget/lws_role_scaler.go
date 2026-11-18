package scaletarget

import (
	"sync"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
)

// LWSRoleScalerRelease is the first LeaderWorkerSet release that serves
// DisaggregatedSetRoleScaler, the object with a /scale subresource that KEDA
// drives for one role of a DisaggregatedSet. LeaderWorkerSet v0.9.x serves
// DisaggregatedSet without it. Only messages use it: the capability itself is
// read from the API server, never from a version string.
const LWSRoleScalerRelease = "v0.10.0"

// RoleScalerUnservedMessage says what an LWS without
// DisaggregatedSetRoleScaler cannot do.
const RoleScalerUnservedMessage = "LeaderWorkerSet in this cluster serves DisaggregatedSet but not DisaggregatedSetRoleScaler " +
	"(LeaderWorkerSet older than " + LWSRoleScalerRelease + "), so KEDA cannot scale DisaggregatedSet roles: " +
	"they have nothing with a /scale subresource for the HPA to drive. Until it is upgraded, only the role writer " +
	"(DSET_ROLE_WRITER=true) scales them"

// RoleScalerUnservedFix is the fix for RoleScalerUnservedMessage.
const RoleScalerUnservedFix = "upgrade LeaderWorkerSet to " + LWSRoleScalerRelease + " or later. Helm does not upgrade CRDs, so apply them first: " +
	"helm pull oci://registry.k8s.io/lws/charts/lws --version 0.10.0 --untar && " +
	"kubectl apply --server-side --force-conflicts -f lws/crds && " +
	"helm upgrade lws oci://registry.k8s.io/lws/charts/lws --version 0.10.0 -n lws-system. " +
	"Then set scaling.mode: External on each role and re-run make scaledobjects-plan. " +
	"(The install deploys LWS_CHART_VERSION only where no LeaderWorkerSet is installed.)"

// RoleScalerWarner logs RoleScalerUnservedMessage once per process: the
// first time the controller finds that DisaggregatedSetRoleScaler is not
// served, never every cycle after.
type RoleScalerWarner struct {
	once sync.Once
}

// DefaultRoleScalerWarner is the warner the controller shares, so the
// collector and the scale-target reads log the warning once between them.
var DefaultRoleScalerWarner = &RoleScalerWarner{}

// Observe logs the warning, once, when err says DisaggregatedSetRoleScaler is
// not served (a NoMatch from the API server's REST mapping). Any other error,
// NotFound included, says nothing about the kind, and nil means it is served.
// It reports whether err was that NoMatch.
func (w *RoleScalerWarner) Observe(log logr.Logger, err error, namespace, target string) bool {
	if err == nil || !meta.IsNoMatchError(err) {
		return false
	}
	w.once.Do(func() {
		log.Info("WARNING: "+RoleScalerUnservedMessage,
			"namespace", namespace, "target", target, "fix", RoleScalerUnservedFix)
	})
	return true
}

// observeRoleScaler is DefaultRoleScalerWarner.Observe on the controller's
// log; a variable so tests can give it a warner and a logger of their own.
var observeRoleScaler = func(err error, namespace, target string) bool {
	return DefaultRoleScalerWarner.Observe(ctrl.Log.WithName("lws"), err, namespace, target)
}
