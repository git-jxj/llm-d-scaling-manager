package config

import (
	"strings"
	"testing"
)

// The settings keys the install from the one release manifest adds
// (proposal PR #404, appendix B).
func TestSettingsInstallKeys(t *testing.T) {
	const enabled = "true"
	cases := []struct {
		name     string
		data     map[string]string
		rejected string
		check    func(t *testing.T, s Settings)
	}{
		{name: "namespaceSelector valid", data: map[string]string{
			SettingNamespaceSelector: NamespaceOptInLabel + "=true,kubernetes.io/metadata.name in (team-a,team-b)"},
			check: func(t *testing.T, s Settings) {
				if s.NamespaceSelector == nil || !strings.Contains(*s.NamespaceSelector, "team-b") {
					t.Fatalf("NamespaceSelector %v", s.NamespaceSelector)
				}
			}},
		{name: "namespaceSelector unset", data: map[string]string{},
			check: func(t *testing.T, s Settings) {
				if s.NamespaceSelector != nil {
					t.Fatalf("NamespaceSelector %q", *s.NamespaceSelector)
				}
			}},
		{name: "namespaceSelector not a selector", data: map[string]string{SettingNamespaceSelector: "name in (team-a"},
			rejected: SettingNamespaceSelector},
		{name: "caFromSecret true", data: map[string]string{SettingCAFromSecret: enabled},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != PrometheusCAFromSecretPath {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q", got)
				}
			}},
		{name: "caFromSecret false", data: map[string]string{SettingCAFromSecret: "false"},
			check: func(t *testing.T, s Settings) {
				if got, ok := s.Restart["PROMETHEUS_CA_CERT_PATH"]; ok {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q set by caFromSecret false", got)
				}
			}},
		{name: "caFromSecret with the same path", data: map[string]string{SettingCAFromSecret: enabled,
			SettingPrometheusCACertPath: PrometheusCAFromSecretPath},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != PrometheusCAFromSecretPath {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q", got)
				}
			}},
		{name: "caFromSecret with another path", data: map[string]string{SettingCAFromSecret: enabled,
			SettingPrometheusCACertPath: "/etc/other/ca.crt"},
			rejected: SettingCAFromSecret,
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["PROMETHEUS_CA_CERT_PATH"]; got != "/etc/other/ca.crt" {
					t.Fatalf("PROMETHEUS_CA_CERT_PATH %q; the explicit path must stay", got)
				}
			}},
		{name: "caFromSecret yes", data: map[string]string{SettingCAFromSecret: "yes"}, rejected: SettingCAFromSecret},
		{name: "dsetRoleWriter true", data: map[string]string{SettingDSetRoleWriter: enabled},
			check: func(t *testing.T, s Settings) {
				if got := s.Restart["DSET_ROLE_WRITER"]; got != enabled {
					t.Fatalf("DSET_ROLE_WRITER %q", got)
				}
			}},
		{name: "dsetRoleWriter maybe", data: map[string]string{SettingDSetRoleWriter: "maybe"}, rejected: SettingDSetRoleWriter},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, rep := ParseSettings(c.data, "team-a")
			if len(rep.Unknown) != 0 {
				t.Fatalf("unknown keys %v", rep.Unknown)
			}
			_, rejected := rep.Rejected[c.rejected]
			if c.rejected != "" && !rejected {
				t.Fatalf("%s not rejected: %v", c.rejected, rep.Rejected)
			}
			if c.rejected == "" && len(rep.Rejected) != 0 {
				t.Fatalf("rejected %v", rep.Rejected)
			}
			if c.check != nil {
				c.check(t, s)
			}
		})
	}
}

// A rejected namespaceSelector keeps the one in force before, so a typo
// never widens what the controller manages to every labelled namespace.
func TestNamespaceSelectorRejectedKeepsPrevious(t *testing.T) {
	prevText := "kubernetes.io/metadata.name in (team-a)"
	prev, _ := ParseSettings(map[string]string{SettingNamespaceSelector: prevText}, "team-a")
	next, rep := ParseSettings(map[string]string{SettingNamespaceSelector: "kubernetes.io/metadata.name in (team-a"}, "team-a")
	kept := next.KeepRejected(prev, rep.Rejected)
	if kept.NamespaceSelector == nil || *kept.NamespaceSelector != prevText {
		t.Fatalf("NamespaceSelector %v, want the previous %q", kept.NamespaceSelector, prevText)
	}
	c := NewTestConfig()
	c.ApplySettings(kept, true, "2", rep)
	if got := c.NamespaceSelector(); got != prevText {
		t.Fatalf("Config.NamespaceSelector %q", got)
	}
}

// The live keys include namespaceSelector; the restart keys include
// dsetRoleWriter; caFromSecret is known but has no loader key of its own.
func TestInstallKeysClassified(t *testing.T) {
	live := false
	for _, k := range LiveSettingKeys {
		live = live || k == SettingNamespaceSelector
	}
	if !live {
		t.Error("namespaceSelector is not a live key")
	}
	if k, ok := SettingLoaderKey(SettingDSetRoleWriter); !ok || k != "DSET_ROLE_WRITER" {
		t.Errorf("dsetRoleWriter loader key %q, %v", k, ok)
	}
	if _, ok := SettingLoaderKey(SettingCAFromSecret); ok {
		t.Error("caFromSecret has a loader key of its own; it sets PROMETHEUS_CA_CERT_PATH")
	}
}
