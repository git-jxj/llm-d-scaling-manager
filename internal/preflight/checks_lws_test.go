package preflight

import (
	"strings"
	"testing"
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
			if tc.want == Warn {
				for _, want := range []string{"DisaggregatedSetRoleScaler", "KEDA"} {
					if !strings.Contains(r.Detail, want) {
						t.Errorf("detail lacks %q: %s", want, r.Detail)
					}
				}
				for _, want := range []string{"v0.10.0 or later", "LWS_CHART_VERSION", "scaling.mode: External"} {
					if !strings.Contains(r.Fix, want) {
						t.Errorf("fix lacks %q: %s", want, r.Fix)
					}
				}
			}
		})
	}
}
