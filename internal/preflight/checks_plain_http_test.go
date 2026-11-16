package preflight

import (
	"context"
	"strconv"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

const (
	plainURL      = "http://prometheus.monitoring.svc:9090"
	saTokenPath   = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	tokenEnvValue = envTokenPath + "=" + saTokenPath
)

// controllerWithEnv is the controller's Deployment with env on its container.
func controllerWithEnv(env ...corev1.EnvVar) *appsv1.Deployment {
	d := controllerDeployment(testNS)
	d.Spec.Template.Spec.Containers[0].Env = env
	return d
}

// A plain HTTP Prometheus passes only when the controller starts on it
// (internal/prometheus ValidateTLSConfig): allowed, no bearer token from the
// container's environment or config.yaml, and no TLS setting.
func TestPrometheusPlainHTTP(t *testing.T) {
	plain := func(f *fixture) {
		f.settings[keyPrometheusURL] = plainURL
		f.settings[keyAllowHTTP] = valueTrue
	}
	cases := []struct {
		name   string
		change func(*fixture)
		want   Status
		detail string
		fix    string
	}{
		{"allowed, no controller yet: WARN naming the sed", plain, Warn, "does not exist yet", "sed -i 's#value: /var/run/secrets/kubernetes.io/serviceaccount/token$#value: \"\"#'"},
		{"allowed, no controller yet, smt empties the token path", func(f *fixture) {
			plain(f)
			f.tokenPathEmptied = true
		}, Pass, "answered", ""},
		{"allowed, a live token path smt is about to empty", func(f *fixture) {
			plain(f)
			f.tokenPathEmptied = true
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: saTokenPath}))
		}, Pass, "answered", ""},
		{"allowed, smt empties the token path, not a bearer token", func(f *fixture) {
			plain(f)
			f.tokenPathEmptied = true
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: saTokenPath}, corev1.EnvVar{Name: envBearerToken, Value: "x"}))
		}, Fail, envBearerToken + "=(a literal)", "kubectl set env"},
		{"allowed, smt empties the token path, not the one in config.yaml", func(f *fixture) {
			plain(f)
			f.tokenPathEmptied = true
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envTokenPath + ": " + saTokenPath + "\n"}))
		}, Fail, envTokenPath + " in config.yaml", "kubectl edit configmap"},
		{"the token path on the manager container, a sidecar first", func(f *fixture) {
			plain(f)
			d := controllerDeployment(testNS)
			d.Spec.Template.Spec.Containers = []corev1.Container{{Name: "sidecar"},
				{Name: "manager", Env: []corev1.EnvVar{{Name: envTokenPath, Value: saTokenPath}}}}
			f.add(d)
		}, Fail, tokenEnvValue + " on Deployment", "kubectl set env"},
		{"TLS: insecure on the Deployment, the setting empty", func(f *fixture) {
			plain(f)
			f.settings[keyPrometheusInsecure] = ""
			f.add(controllerWithEnv(corev1.EnvVar{Name: envInsecure, Value: "true"}))
		}, Fail, "beside it: " + envInsecure + " on Deployment", "https://"},
		{"TLS: the setting false beats insecure on the Deployment", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envInsecure, Value: "true"}))
		}, Pass, "answered", ""},
		{"TLS: insecure false on the Deployment is no TLS setting", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envInsecure, Value: "false"}))
		}, Pass, "answered", ""},
		{"TLS: a CA path on the Deployment", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envCACert, Value: "/etc/ca.crt"}))
		}, Fail, "beside it: " + envCACert + " on Deployment", ""},
		{"TLS: a server name on the Deployment", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envServerName, Value: "prom"}))
		}, Fail, "beside it: " + envServerName + " on Deployment", ""},
		{"TLS: a client certificate on the Deployment", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envClientCert, Value: "/etc/c.crt"}))
		}, Fail, "beside it: " + envClientCert + " on Deployment", ""},
		{"TLS: a client key in config.yaml", func(f *fixture) {
			plain(f)
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envClientKey + ": /etc/c.key\n"}))
		}, Fail, "beside it: " + envClientKey + " in config.yaml", ""},
		{"TLS: a CA path in the settings", func(f *fixture) {
			plain(f)
			f.settings["prometheusCACertPath"] = "/etc/ca.crt"
		}, Fail, "beside it: prometheusCACertPath in ConfigMap " + SettingsConfigMap, ""},
		{"allowed, the release Deployment's token path", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: saTokenPath}))
		}, Fail, tokenEnvValue + " on Deployment " + ControllerDeployment, "kubectl set env deployment/" + ControllerDeployment + " -n " + testNS + " " + envTokenPath + "="},
		{"allowed, token path emptied", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: ""}))
		}, Pass, "answered", ""},
		{"allowed, bearer token from a Secret", func(f *fixture) {
			plain(f)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envBearerToken, ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "prom-token"}, Key: "token"}}}))
		}, Fail, envBearerToken + "=from Secret prom-token", "kubectl set env"},
		{"allowed, token path in config.yaml", func(f *fixture) {
			plain(f)
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envTokenPath + ": " + saTokenPath + "\n"}))
		}, Fail, envTokenPath + " in config.yaml of ConfigMap " + LegacyConfigMap, "kubectl edit configmap " + LegacyConfigMap},
		{"allowed by the container's environment", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.add(controllerWithEnv(corev1.EnvVar{Name: envAllowHTTP, Value: "true"}, corev1.EnvVar{Name: envTokenPath, Value: ""}))
		}, Pass, "answered", ""},
		{"allowed by config.yaml, the setting empty", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.settings[keyAllowHTTP] = ""
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envAllowHTTP + ": \"true\"\n"}))
		}, Warn, "answered", ""},
		{"the setting false beats the environment", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.settings[keyAllowHTTP] = strconv.FormatBool(false)
			f.add(controllerWithEnv(corev1.EnvVar{Name: envAllowHTTP, Value: "true"}))
		}, Fail, "unless " + keyAllowHTTP + " is true", "--allow-http"},
		{"the environment beats config.yaml", func(f *fixture) {
			f.settings[keyPrometheusURL] = plainURL
			f.add(cm(testNS, LegacyConfigMap, map[string]string{"config.yaml": envAllowHTTP + ": \"true\"\n"}))
			f.add(controllerWithEnv(corev1.EnvVar{Name: envAllowHTTP, Value: strconv.FormatBool(false)}))
		}, Fail, "unless " + keyAllowHTTP + " is true", "--allow-http"},
		{"allowed, insecureSkipVerify", func(f *fixture) {
			plain(f)
			f.settings[keyPrometheusInsecure] = valueTrue
		}, Fail, "refuses TLS settings beside it: " + keyPrometheusInsecure, "https://prometheus.monitoring.svc:9090"},
		{"https is not held to it", func(f *fixture) {
			f.settings[keyAllowHTTP] = valueTrue
			f.add(controllerWithEnv(corev1.EnvVar{Name: envTokenPath, Value: saTokenPath}))
		}, Pass, "answered", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			tc.change(f)
			r := only(t, f.run(t), "prometheus.reachable")
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Errorf("got %s %q fix %q, want %s containing %q, fix %q", r.Status, r.Detail, r.Fix, tc.want, tc.detail, tc.fix)
			}
		})
	}
}

// The Deployment's watch namespace is read off the manager container, not
// the first one.
func TestDeploymentWatchReadsTheManagerContainer(t *testing.T) {
	f := newFixture(t)
	d := controllerDeployment(testNS)
	d.Spec.Template.Spec.Containers = []corev1.Container{
		{Name: "sidecar", Args: []string{"--watch-namespace=team-z"}},
		{Name: "manager", Env: []corev1.EnvVar{{Name: "WATCH_NAMESPACE", Value: otherNS}}}}
	f.add(d)
	s := &state{env: f.env(t)}
	if got := s.loadSettings(context.Background()).deployWatch; got != otherNS {
		t.Errorf("deployWatch %q, want %q from the manager container", got, otherNS)
	}
}

// A literal bearer token on the Deployment FAILs a plain HTTP URL, and the
// preflight prints where it is set, never the token.
func TestPrometheusPlainHTTPHidesLiteralToken(t *testing.T) {
	const secret = "s3cret-token-value"
	f := newFixture(t)
	f.settings[keyPrometheusURL] = plainURL
	f.settings[keyAllowHTTP] = valueTrue
	f.add(controllerWithEnv(corev1.EnvVar{Name: envBearerToken, Value: secret}))
	r := only(t, f.run(t), "prometheus.reachable")
	if r.Status != Fail || !strings.Contains(r.Detail, envBearerToken+"=(a literal)") ||
		!strings.Contains(r.Fix, "kubectl set env deployment/"+ControllerDeployment+" -n "+testNS+" "+envBearerToken+"=") {
		t.Errorf("got %s %q fix %q", r.Status, r.Detail, r.Fix)
	}
	if strings.Contains(r.Detail+r.Fix, secret) {
		t.Errorf("the token is printed: %q %q", r.Detail, r.Fix)
	}
}
