package config

import "testing"

// prometheusAllowHTTP is the settings key for PROMETHEUS_ALLOW_HTTP: it sets
// the loader key above the environment and config.yaml, and only true or
// false is taken.
func TestSettingsPrometheusAllowHTTP(t *testing.T) {
	t.Setenv("PROMETHEUS_BASE_URL", "")
	t.Setenv("PROMETHEUS_ALLOW_HTTP", "false")
	c, err := LoadWithSettings(nil, "", map[string]string{
		SettingPrometheusURL: "http://prometheus.monitoring.svc:9090", SettingPrometheusAllowHTTP: "true"})
	if err != nil {
		t.Fatal(err)
	}
	if !c.PrometheusAllowHTTP() {
		t.Error("prometheusAllowHTTP true in the settings did not override PROMETHEUS_ALLOW_HTTP=false in the environment")
	}
	if got, ok := SettingLoaderKey(SettingPrometheusAllowHTTP); !ok || got != "PROMETHEUS_ALLOW_HTTP" {
		t.Errorf("loader key %q %v, want PROMETHEUS_ALLOW_HTTP", got, ok)
	}
	_, rep := ParseSettings(map[string]string{SettingPrometheusAllowHTTP: "maybe"}, "default")
	if _, rejected := rep.Rejected[SettingPrometheusAllowHTTP]; !rejected {
		t.Errorf("prometheusAllowHTTP maybe was not rejected: %v", rep.Rejected)
	}
	if hint := UnknownSettingHint("PROMETHEUS_ALLOW_HTTP"); hint != "PROMETHEUS_ALLOW_HTTP is now the prometheusAllowHTTP setting" {
		t.Errorf("hint for PROMETHEUS_ALLOW_HTTP: %q", hint)
	}
}
