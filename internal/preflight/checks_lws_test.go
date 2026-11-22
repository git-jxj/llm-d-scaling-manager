package preflight

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// The DisaggregatedSet check reads what LeaderWorkerSet serves: v0.9.x
// serves disaggregatedsets alone, v0.10.0 adds the role scaler and its
// /scale subresource, which KEDA needs. No DisaggregatedSet API: the check
// does not apply.
func TestLWSDisaggregatedSet(t *testing.T) {
	for _, tc := range []struct {
		name      string
		discovery []string
		want      Status // "" for not applicable
	}{
		{"no LeaderWorkerSet DisaggregatedSet API", nil, ""},
		{"LWS v0.9.x: sets, no role scaler", []string{"disaggregatedsets", "disaggregatedsets/status"}, Warn},
		{"role scaler without /scale", []string{"disaggregatedsets", "disaggregatedsetrolescalers"}, Warn},
		{"LWS v0.10.0 or later", []string{"disaggregatedsets", "disaggregatedsets/status",
			"disaggregatedsetrolescalers", "disaggregatedsetrolescalers/scale", "disaggregatedsetrolescalers/status"}, Pass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.lwsDiscovery = tc.discovery
			rep := f.run(t)
			if tc.want == "" {
				if !notApplicable(rep, "lws.disaggregatedset") {
					t.Fatalf("want not applicable, got %v", results(rep, "lws.disaggregatedset"))
				}
				return
			}
			r := only(t, rep, "lws.disaggregatedset")
			if r.Status != tc.want {
				t.Fatalf("got %s %q", r.Status, r.Detail)
			}
			// Discovery sees CRDs, not the controller: a PASS says what it saw
			// and that the controller must match.
			if tc.want == Pass && (!strings.HasPrefix(r.Detail, "the API serves ") || !strings.Contains(r.Detail, "controller is v0.10.0 or later")) {
				t.Errorf("pass detail claims more than discovery read: %s", r.Detail)
			}
			if tc.want == Warn {
				for _, want := range []string{"DisaggregatedSetRoleScaler", "KEDA"} {
					if !strings.Contains(r.Detail, want) {
						t.Errorf("detail lacks %q: %s", want, r.Detail)
					}
				}
				for _, want := range []string{"v0.10.0 or later", "through its operator", "If it was installed with the upstream Helm chart", "LWS_CHART_VERSION", "scaling.mode: External",
					"role writer"} {
					if !strings.Contains(r.Fix, want) {
						t.Errorf("fix lacks %q: %s", want, r.Fix)
					}
				}
				// In passive mode such a role is not read at all, which is
				// worse than "not scaled": the warning must say it.
				if !strings.Contains(r.Detail, "not observed at all") {
					t.Errorf("detail does not say a passive role goes unobserved: %s", r.Detail)
				}
			}
		})
	}
}

// Discovery reads the CRDs. CRDs upgraded to v0.10.0 with the controller
// left at v0.9.x serve the role scaler's /scale, and the old controller
// creates no role scaler: the check reads the controller's image tag too.
func TestLWSDisaggregatedSetControllerVersion(t *testing.T) {
	served := []string{"disaggregatedsets", "disaggregatedsetrolescalers", "disaggregatedsetrolescalers/scale"}
	chartLabels := map[string]string{"app.kubernetes.io/name": "lws", "control-plane": "controller-manager"}
	for _, tc := range []struct {
		name       string
		dep        runtime.Object
		listDenied bool
		want       Status
		detail     string
	}{
		{"old controller, chart labels", deployment("lws-system", "lws-controller-manager", chartLabels, "registry.k8s.io/lws/lws:v0.9.1"),
			false, Warn, "controller is v0.9.1"},
		{"old controller, read by name where Deployments may not be listed",
			deployment("lws-system", "lws-controller-manager", nil, "registry.k8s.io/lws/lws:v0.9.0"),
			true, Warn, "controller is v0.9.0"},
		{"current controller", deployment("lws-ops", "lws-controller-manager", chartLabels, "registry.k8s.io/lws/lws:v0.10.0"),
			false, Pass, "controller is v0.10.0 (lws-ops/lws-controller-manager"},
		{"newer controller", deployment("lws-system", "lws-controller-manager", chartLabels, "registry.k8s.io/lws/lws:v1.0.0"),
			false, Pass, "controller is v1.0.0"},
		{"image by digest names no version", deployment("lws-system", "lws-controller-manager", chartLabels, "registry.k8s.io/lws/lws@sha256:0123"),
			false, Pass, "provided the LeaderWorkerSet controller is v0.10.0 or later too"},
		{"no controller found", nil, false, Pass, "its version was not read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.lwsDiscovery = served
			if tc.dep != nil {
				f.kube = append(f.kube, tc.dep)
			}
			if tc.listDenied {
				f.kubeReact = append(f.kubeReact, reaction{"list", "deployments", func(a k8stesting.Action) (bool, runtime.Object, error) {
					if a.GetNamespace() != "" {
						return false, nil, nil
					}
					return forbidden("list", "deployments").fn(a)
				}})
			}
			r := only(t, f.run(t), "lws.disaggregatedset")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) {
				t.Fatalf("got %s %q, want %s containing %q", r.Status, r.Detail, tc.want, tc.detail)
			}
			if tc.want == Warn {
				for _, want := range []string{"not observed at all", "older than v0.10.0"} {
					if !strings.Contains(r.Detail, want) {
						t.Errorf("detail lacks %q: %s", want, r.Detail)
					}
				}
				if !strings.Contains(r.Fix, "upgrade LeaderWorkerSet") {
					t.Errorf("fix lacks the upgrade: %s", r.Fix)
				}
			}
		})
	}
}
