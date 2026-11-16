package preflight

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// settingsDefaults are the values the shipped settings file holds, key for
// key: a key the file leaves empty is empty here too.
func TestSettingsDefaultsAreTheShippedFile(t *testing.T) {
	b, err := os.ReadFile("../../config/release/settings-configmap.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(b, &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Data) == 0 {
		t.Fatal("the shipped settings file holds no keys")
	}
	for k, want := range cm.Data {
		got, ok := settingsDefaults[k]
		if !ok {
			t.Errorf("%s: in the shipped file, not in settingsDefaults", k)
			continue
		}
		if got != want {
			t.Errorf("%s: settingsDefaults %q, the shipped file %q", k, got, want)
		}
	}
	for k := range settingsDefaults {
		if _, ok := cm.Data[k]; !ok {
			t.Errorf("%s: in settingsDefaults, not in the shipped file", k)
		}
	}
}
