package config

import "testing"

// The DisaggregatedSet role writer ships off; DSET_ROLE_WRITER=true turns it on from the
// environment, and the settings ConfigMap takes the same key, true or false.
func TestDSetRoleWriterFlag(t *testing.T) {
	t.Run("off when unset", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if cfg.DSetRoleWriterEnabled() {
			t.Error("the writer is on with DSET_ROLE_WRITER unset; it ships off")
		}
	})

	t.Run("on from the environment", func(t *testing.T) {
		t.Setenv("PROMETHEUS_BASE_URL", "https://prometheus:9090")
		t.Setenv("DSET_ROLE_WRITER", "true")
		cfg, err := Load(nil, "")
		if err != nil {
			t.Fatalf("Load() failed: %v", err)
		}
		if !cfg.DSetRoleWriterEnabled() {
			t.Error("DSET_ROLE_WRITER=true was not read")
		}
	})

	t.Run("settings ConfigMap", func(t *testing.T) {
		s, rep := ParseSettings(map[string]string{"DSET_ROLE_WRITER": "true"}, "default")
		if len(rep.Rejected) != 0 || len(rep.Unknown) != 0 {
			t.Fatalf("rejected %v unknown %v, want none", rep.Rejected, rep.Unknown)
		}
		if s.Restart["DSET_ROLE_WRITER"] != "true" {
			t.Errorf("restart %v, want DSET_ROLE_WRITER=true", s.Restart)
		}
		_, rep = ParseSettings(map[string]string{"DSET_ROLE_WRITER": "sometimes"}, "default")
		if _, bad := rep.Rejected["DSET_ROLE_WRITER"]; !bad {
			t.Errorf("rejected %v, want DSET_ROLE_WRITER=sometimes refused", rep.Rejected)
		}
	})
}
