package preflight

import (
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// secretRef is a valueFrom on a Secret key.
func secretRef(name string) *corev1.EnvVarSource {
	return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "v"}}
}

// The plain-HTTP check, the cases the second review asked for: a Deployment
// that cannot be read is not an absent one, a valueFrom on a boolean is not
// false, config.yaml keys are read case-insensitively, each fix is the
// command for where the setting lives, and an unauthenticated query refused
// over HTTP fails.
func TestPrometheusPlainHTTPReview(t *testing.T) {
	plain := func(f *fixture) {
		f.settings[keyPrometheusURL] = plainURL
		f.settings[keyAllowHTTP] = valueTrue
	}
	cases := []struct {
		name    string
		change  func(*fixture)
		want    Status
		detail  string
		fix     string
		notInFx string
	}{
		{"the Deployment cannot be read: UNKNOWN, not a missing Deployment", func(f *fixture) {
			plain(f)
			f.kubeReact = append(f.kubeReact, forbidden("get", "deployments"), forbidden("list", "deployments"))
		}, Unknown, "is not known: cannot read Deployment", "", ""},
		{"allow-HTTP from a Secret on the Deployment: UNKNOWN, not false", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.settings[keyAllowHTTP] = ""
			f.add(controllerWithEnv(corev1.EnvVar{Name: envAllowHTTP, ValueFrom: secretRef("flags")}))
		}, Unknown, envAllowHTTP + " on Deployment " + ControllerDeployment + " (from Secret flags)", "", ""},
		{"insecure from a Secret on the Deployment: UNKNOWN, not false", func(f *fixture) {
			plain(f)
			f.settings[keyPrometheusInsecure] = ""
			f.add(controllerWithEnv(corev1.EnvVar{Name: envInsecure, ValueFrom: secretRef("tls")}))
		}, Unknown, "TLS setting it refuses beside it is not known: " + envInsecure, "", ""},
		{"a lower-case token path in config.yaml", func(f *fixture) {
			plain(f)
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "prometheus_token_path: " + saTokenPath + "\n"}))
		}, Fail, envTokenPath + " in config.yaml of ConfigMap " + LegacyConfigMap, "kubectl edit configmap", ""},
		{"a mixed-case allow-HTTP in config.yaml", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.settings[keyAllowHTTP] = ""
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": "Prometheus_Allow_HTTP: \"true\"\n"}))
		}, Warn, "does not exist yet", "", ""},
		{"not allowed, no Deployment: the fix is the sed, not set env", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
		}, Fail, "unless " + keyAllowHTTP, tokenPathSed, "set env"},
		{"a live token path: set env, and the sed for the file", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: saTokenPath}))
		}, Fail, tokenEnvValue, "kubectl set env deployment/" + ControllerDeployment + " -n " + testNS + " " + envTokenPath + "=\n# and in the main file you apply, or the next apply brings it back: " + tokenPathSed, ""},
		{"a CA path in the settings: the patch that empties it", func(f *fixture) {
			plain(f)
			f.settings[keyPrometheusCA] = "/etc/ca.crt"
		}, Fail, "beside it", settingsPatch(testNS, keyPrometheusCA, ""), ""},
		{"insecure in the settings: the patch that sets it false", func(f *fixture) {
			plain(f)
			f.settings[keyPrometheusInsecure] = valueTrue
		}, Fail, "beside it", settingsPatch(testNS, keyPrometheusInsecure, "false"), ""},
		{"a server name on the Deployment: unset it", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envServerName, Value: "prom"}))
		}, Fail, "beside it", "kubectl set env deployment/" + ControllerDeployment + " -n " + testNS + " " + envServerName + "-", ""},
		{"a client key in config.yaml: edit it", func(f *fixture) {
			plain(f)
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envClientKey + ": /k\n"}))
		}, Fail, "beside it", "remove " + envClientKey + " from config.yaml", ""},
		{"refused unauthenticated over HTTP: FAIL", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv())
			f.prom.reachErr = &PromError{Kind: PromDenied, Err: errors.New("HTTP 401: unauthorized")}
		}, Fail, "refused an unauthenticated query", "https://prometheus.monitoring.svc:9090", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "prometheus.reachable")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) ||
				(tc.notInFx != "" && strings.Contains(r.Fix, tc.notInFx)) {
				t.Errorf("got %s %q fix %q, want %s containing %q, fix %q (without %q)", r.Status, r.Detail, r.Fix, tc.want, tc.detail, tc.fix, tc.notInFx)
			}
		})
	}
}

// tokenPathSed names the main file of every install, the OpenShift ones too.
func TestTokenPathSedNamesEveryMainFile(t *testing.T) {
	for _, f := range []string{"scaling-manager-namespaced.yaml", "scaling-manager-cluster.yaml",
		"scaling-manager-namespaced-openshift.yaml", "scaling-manager-cluster-openshift.yaml"} {
		if !strings.Contains(tokenPathSed, f) {
			t.Errorf("tokenPathSed does not name %s: %s", f, tokenPathSed)
		}
	}
}

// TestTokenPathEmptiedFromEnv: the Job variable smt install sets is read,
// and a value that is no boolean is an error naming it.
func TestTokenPathEmptiedFromEnv(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
		err   bool
	}{{"", false, false}, {"true", true, false}, {"false", false, false}, {" true ", true, false}, {"yes", false, true}} {
		t.Setenv(TokenPathEmptiedEnv, tc.value)
		got, err := tokenPathEmptiedFromEnv()
		if got != tc.want || (err != nil) != tc.err || (err != nil && !strings.Contains(err.Error(), TokenPathEmptiedEnv)) {
			t.Errorf("%s=%q: %v, %v", TokenPathEmptiedEnv, tc.value, got, err)
		}
	}
}
