package preflight

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/config"
	"github.com/llm-d-incubation/llm-d-scaling-manager/internal/constants"
)

// The settings keys (config/release/settings-configmap.yaml), as
// internal/config names them.
const (
	keyPrometheusURL      = config.SettingPrometheusURL
	keyPrometheusInsecure = config.SettingPrometheusInsecureSkipVerify
	keyPassive            = config.SettingPassive
	keyUtilizationShare   = config.SettingUtilizationShare
	keyLimiter            = config.SettingLimiter
	keyQuotas             = config.SettingQuotas
	keyQuotaScope         = config.SettingQuotaScope
	keyKueue              = config.SettingKueue
	keyKueueBorrow        = config.SettingKueueBorrow
	keyKueueNamespace     = config.SettingKueueNamespace
	keyScaleToZero        = config.SettingScaleToZero
	keyWatchNamespaces    = config.SettingWatchNamespaces
	keyLogVerbosity       = config.SettingLogVerbosity
	keyAllowHTTP          = config.SettingPrometheusAllowHTTP
	keyPrometheusCA       = config.SettingPrometheusCACertPath
)

// The controller's Prometheus loader keys the plain-HTTP check reads, as
// environment variables of its container and config.yaml keys: env beats
// config.yaml, and a setting beats both (internal/config loader).
const (
	envAllowHTTP   = "PROMETHEUS_ALLOW_HTTP"
	envTokenPath   = "PROMETHEUS_TOKEN_PATH"
	envBearerToken = "PROMETHEUS_BEARER_TOKEN"
	envInsecure    = "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY"
	envCACert      = "PROMETHEUS_CA_CERT_PATH"
	envServerName  = "PROMETHEUS_SERVER_NAME"
	envClientCert  = "PROMETHEUS_CLIENT_CERT_PATH"
	envClientKey   = "PROMETHEUS_CLIENT_KEY_PATH"
)

// promLoaderKeys are the loader keys above.
var promLoaderKeys = []string{envAllowHTTP, envTokenPath, envBearerToken, envInsecure, envCACert, envServerName, envClientCert, envClientKey}

// TokenPathEmptiedEnv is the preflight Job's environment variable that says
// the controller is about to be applied with PROMETHEUS_TOKEN_PATH empty
// (smt install --allow-http renders it so): the token path of the live
// Deployment is then not what the controller will start with.
const TokenPathEmptiedEnv = "LLMSM_PREFLIGHT_TOKEN_PATH_EMPTIED"

// settingsDefaults are the values the shipped settings file holds, used for a
// key neither the settings nor the legacy configuration holds.
var settingsDefaults = map[string]string{
	keyPrometheusURL:      "",
	keyPrometheusInsecure: "false",
	keyPrometheusCA:       "",
	keyPassive:          "true",
	keyUtilizationShare:   config.UtilizationShareSettingOff,
	keyLimiter:            "",
	keyQuotas:             "",
	keyQuotaScope:         string(config.QuotaScopeNamespace),
	keyKueue:              "false",
	keyKueueBorrow:        "true",
	keyKueueNamespace:     "kueue-system",
	keyScaleToZero:        "false",
	keyWatchNamespaces:    "",
	keyLogVerbosity:       "2",
	keyAllowHTTP:          "",
}

// Values the settings keys take.
const (
	shareActive  = config.UtilizationShareSettingActive
	limiterQuota = string(config.LimiterTypeQuota)
	valueTrue    = "true"
)

// legacyKeys maps a key to the config.yaml key of LegacyConfigMap the
// controller reads it from when the settings do not hold it: the loader key
// internal/config gives the setting.
func legacyKeys() map[string]string {
	out := map[string]string{}
	for _, k := range append(config.RestartSettingKeys(), keyPassive) {
		if loaderKey, ok := config.SettingLoaderKey(k); ok {
			out[k] = loaderKey
		}
	}
	return out
}

// limiterGroupKeys are the settings that build the limiters list together
// (config's limiterSettingKeys): when one is rejected, none is read.
var limiterGroupKeys = []string{keyLimiter, keyQuotas, keyQuotaScope, keyKueue, keyKueueBorrow, keyKueueNamespace}

// settingsState is the settings as the controller will read them.
type settingsState struct {
	// found: the settings ConfigMap exists. legacy: the legacy ConfigMap does.
	found, legacy bool
	// unread is why the settings could not be read, when they could not.
	unread string
	// set holds the keys the controller reads from the settings, and from the
	// legacy configuration for a key the settings do not hold. A key the
	// controller rejects is not in it: the next source down gives its value.
	set map[string]string
	// fromLegacy names the keys read from the legacy configuration.
	fromLegacy []string
	// parsed and report are the settings ConfigMap as the controller's parser,
	// config.ParseSettings, and its watchNamespaces restriction read it.
	parsed config.Settings
	report config.SettingsReport
	// unbounded is why the controller refuses to start on these settings,
	// where the limiter settings are read (budget): a limiter is declared and
	// the keys that build it are rejected.
	unbounded error
	// deployWatch is what the controller's Deployment names to watch, as
	// Config.DeploymentWatchNamespace reads it: "" for every namespace.
	// deployWhy is why the Deployment could not be read, when it could not.
	deployWatch, deployWhy string
	// watchUnchecked: watchNamespaces is set, and whether it only narrows
	// deployWatch is not known (deployWhy).
	watchUnchecked bool
	// watchWidens: watchNamespaces names a namespace deployWatch does not.
	watchWidens bool
	// watched are the namespaces the controller manages: watchNamespaces when
	// it narrows deployWatch, else deployWatch. Nil is every namespace.
	watched []string
	// deployFound: the controller's Deployment exists and was read.
	deployFound bool
	// deployEnv holds the promLoaderKeys the controller container sets to a
	// value the controller reads: a literal that is not empty, or
	// "from <source>" for valueFrom.
	deployEnv map[string]string
	// legacyProm holds the promLoaderKeys LegacyConfigMap's config.yaml sets
	// to a value that is not empty.
	legacyProm map[string]string
	// tokenPathEmptied: the preflight Job says the controller is about to be
	// applied with PROMETHEUS_TOKEN_PATH empty (TokenPathEmptiedEnv), so the
	// live Deployment's token path is not counted.
	tokenPathEmptied bool
}

// loaderValue is the value the controller's loader reads for loaderKey: the
// setting (settingKey, "" for none) when the settings ConfigMap holds it,
// else the container's environment, else config.yaml. source names where
// it came from; both are empty when it is unset.
func (st settingsState) loaderValue(loaderKey, settingKey string) (value, source string) {
	if settingKey != "" && !slices.Contains(st.fromLegacy, settingKey) {
		if v := strings.TrimSpace(st.set[settingKey]); v != "" {
			return v, settingKey + " in ConfigMap " + SettingsConfigMap
		}
	}
	if v, ok := st.deployEnv[loaderKey]; ok {
		return v, loaderKey + " on Deployment " + ControllerDeployment
	}
	if v, ok := st.legacyProm[loaderKey]; ok {
		return v, loaderKey + " in config.yaml of ConfigMap " + LegacyConfigMap
	}
	return "", ""
}

// allowHTTP is whether the controller allows a plain HTTP Prometheus URL, as
// its loader reads PROMETHEUS_ALLOW_HTTP.
func (st settingsState) allowHTTP() bool {
	v, _ := st.loaderValue(envAllowHTTP, keyAllowHTTP)
	b, _ := strconv.ParseBool(v)
	return b
}

// tlsBesideHTTP names the TLS settings the controller has that it refuses
// beside an http URL (internal/prometheus ValidateTLSConfig), with where each
// is set.
func (st settingsState) tlsBesideHTTP() []string {
	var out []string
	if v, src := st.loaderValue(envInsecure, keyPrometheusInsecure); src != "" {
		if b, _ := strconv.ParseBool(v); b {
			out = append(out, src)
		}
	}
	for _, k := range [][2]string{
		{envCACert, keyPrometheusCA},
		{envServerName, config.SettingPrometheusServerName},
		{envClientCert, ""},
		{envClientKey, ""},
	} {
		if _, src := st.loaderValue(k[0], k[1]); src != "" {
			out = append(out, src)
		}
	}
	return out
}

// bearerToken says where the controller gets a bearer token for Prometheus,
// as its loader reads PROMETHEUS_TOKEN_PATH and PROMETHEUS_BEARER_TOKEN: the
// container's environment, else config.yaml. where is empty when it gets
// none, or when that is not known (no Deployment yet). key is the variable;
// onDeployment says it is the container's.
func (st settingsState) bearerToken() (where, key string, onDeployment bool) {
	for _, k := range []string{envTokenPath, envBearerToken} {
		if v, ok := st.deployEnv[k]; ok {
			if k == envTokenPath && st.tokenPathEmptied {
				continue
			}
			return fmt.Sprintf("%s=%s on Deployment %s", k, v, ControllerDeployment), k, true
		}
	}
	for _, k := range []string{envTokenPath, envBearerToken} {
		if _, ok := st.legacyProm[k]; ok {
			return fmt.Sprintf("%s in config.yaml of ConfigMap %s", k, LegacyConfigMap), k, false
		}
	}
	return "", "", false
}

// controllerPromEnv reads promLoaderKeys off the controller container of d
// (managerContainer): the values the controller's loader takes. An empty
// literal is unset there (viper), so it is left out. A literal bearer token
// is a secret: it is recorded as "(a literal)", never copied.
func controllerPromEnv(d *appsv1.Deployment) map[string]string {
	out := map[string]string{}
	if d == nil {
		return out
	}
	c := managerContainer(*d)
	if c == nil {
		return out
	}
	for _, e := range c.Env {
		if !slices.Contains(promLoaderKeys, e.Name) {
			continue
		}
		switch {
		case e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil:
			out[e.Name] = "from Secret " + e.ValueFrom.SecretKeyRef.Name
		case e.ValueFrom != nil:
			out[e.Name] = "from a reference"
		case strings.TrimSpace(e.Value) != "" && e.Name == envBearerToken:
			out[e.Name] = "(a literal)"
		case strings.TrimSpace(e.Value) != "":
			out[e.Name] = e.Value
		default:
			delete(out, e.Name)
		}
	}
	return out
}

// legacyPromValues reads promLoaderKeys from config.yaml (old): those set to
// a value that is not empty.
func legacyPromValues(old map[string]any) map[string]string {
	out := map[string]string{}
	for _, k := range promLoaderKeys {
		if v, ok := old[k]; ok && v != nil {
			if s := strings.TrimSpace(fmt.Sprint(v)); s != "" {
				out[k] = s
			}
		}
	}
	return out
}

// value is the effective value of key: set, else the shipped default.
func (st settingsState) value(key string) string {
	if v, ok := st.set[key]; ok {
		return strings.TrimSpace(v)
	}
	return settingsDefaults[key]
}

// loadSettings reads the settings ConfigMap as the controller does: through
// its parser, with watchNamespaces held to the namespaces the controller's
// Deployment names, and LegacyConfigMap's config.yaml for a key the settings
// do not hold.
func (s *state) loadSettings(ctx context.Context) settingsState {
	s.settingsOnce.Do(func() {
		st := settingsState{set: map[string]string{}}
		cms := s.env.Kube.CoreV1().ConfigMaps(s.env.Namespace)
		var old map[string]any
		if legacy, err := cms.Get(ctx, LegacyConfigMap, metav1.GetOptions{}); err == nil {
			st.legacy = true
			if yaml.Unmarshal([]byte(legacy.Data["config.yaml"]), &old) != nil {
				old = nil
			}
		}
		var d *appsv1.Deployment
		st.deployWatch, st.deployWhy, d = s.deploymentWatch(ctx, old)
		st.deployFound = d != nil
		st.deployEnv = controllerPromEnv(d)
		st.legacyProm = legacyPromValues(old)
		st.tokenPathEmptied = s.env.TokenPathEmptied
		effective := st.deployWatch
		cm, err := cms.Get(ctx, SettingsConfigMap, metav1.GetOptions{})
		switch err {
		case nil:
			st.found = true
			effective = st.parseSettings(cm.Data)
			for k, v := range cm.Data {
				// A key the controller does not know is not read: the
				// next source down gives its value.
				if !slices.Contains(st.report.Unknown, k) {
					st.set[k] = v
				}
			}
			for k := range st.report.Rejected {
				delete(st.set, k)
				if slices.Contains(limiterGroupKeys, k) {
					for _, g := range limiterGroupKeys {
						delete(st.set, g)
					}
				}
			}
		default:
			absent, why := readVerdict(err, "ConfigMap "+SettingsConfigMap+" in "+s.env.Namespace)
			if !absent {
				st.unread = why
			}
		}
		for key, name := range legacyKeys() {
			if _, ok := st.set[key]; ok || key == keyWatchNamespaces {
				continue // WATCH_NAMESPACE is the Deployment's: deploymentWatch
			}
			if _, rejected := st.report.Rejected[key]; rejected && key == keyPassive {
				continue // a rejected passive fails closed, to the default true
			}
			if v, ok := old[name]; ok && v != nil {
				st.set[key] = fmt.Sprint(v)
				st.fromLegacy = append(st.fromLegacy, key)
			}
		}
		st.watched = config.ParseWatchNamespaces(effective)
		s.settings = st
	})
	return s.settings
}

// parseSettings runs the controller's parser on data, with the namespace
// quota key and the watchNamespaces restriction the controller applies
// (Config.SettingsQuotaKey, Config.RestrictSettings), and returns the
// namespaces the controller then watches.
func (st *settingsState) parseSettings(data map[string]string) string {
	effective := st.deployWatch
	st.parsed, st.report = config.ParseSettings(data, quotaKeyFor(effective))
	if ws, ok := st.parsed.Restart["WATCH_NAMESPACE"]; ok {
		switch err := config.NarrowsWatchNamespaces(st.deployWatch, ws); {
		case st.deployWhy != "":
			st.watchUnchecked = true
			effective = ws
		case err != nil:
			delete(st.parsed.Restart, "WATCH_NAMESPACE")
			st.report.Rejected[keyWatchNamespaces] = err.Error()
			st.watchWidens = true
		default:
			effective = ws
		}
		if effective == ws {
			// The quota key follows the namespaces watched.
			st.parsed, st.report = config.ParseSettings(data, quotaKeyFor(effective))
		}
	}
	// At start there is no value in force before: what KeepRejected and
	// RejectedLimiterUnbounded decide with an empty one is what the
	// controller does on its first read.
	st.parsed = st.parsed.KeepRejected(config.Settings{}, st.report.Rejected)
	st.unbounded = config.RejectedLimiterUnbounded(data, st.report.Rejected, config.Settings{})
	return effective
}

// quotaKeyFor is the namespace a namespace-scoped quota from the settings is
// keyed on when the controller watches watch, as Config.SettingsQuotaKey
// chooses it: the namespace when it watches exactly one, else "default".
func quotaKeyFor(watch string) string {
	if w := config.ParseWatchNamespaces(watch); len(w) == 1 {
		return w[0]
	}
	return config.QuotaLimiterReservedNamespaceKey
}

// deploymentWatch reads what the controller's Deployment names to watch, as
// the loader (viper) reads WATCH_NAMESPACE: the --watch-namespace flag when
// passed, an empty one meaning every namespace; else the container's
// WATCH_NAMESPACE when not empty, $(VAR) resolved; else config.yaml (old).
// Before the install, with no Deployment, it is what the install files name:
// the controller's namespace for a namespaced install, every namespace for a
// cluster-wide one.
// why says the Deployment could not be read; the value is then the install
// files' too. d is the Deployment, nil when it is absent or unread.
func (s *state) deploymentWatch(ctx context.Context, old map[string]any) (watch, why string, d *appsv1.Deployment) {
	files := ""
	if s.env.Scope != ScopeCluster {
		files = s.env.Namespace
	}
	deps := s.env.Kube.AppsV1().Deployments(s.env.Namespace)
	d, err := deps.Get(ctx, ControllerDeployment, metav1.GetOptions{})
	if apierrors.IsForbidden(err) {
		// The cluster-wide preflight may list Deployments, not get them.
		list, lerr := deps.List(ctx, metav1.ListOptions{FieldSelector: "metadata.name=" + ControllerDeployment})
		if lerr == nil {
			err = apierrors.NewNotFound(appsv1.Resource("deployments"), ControllerDeployment)
			for i := range list.Items {
				if list.Items[i].Name == ControllerDeployment {
					d, err = &list.Items[i], nil
				}
			}
		}
	}
	if err != nil {
		absent, why := readVerdict(err, "Deployment "+ControllerDeployment+" in "+s.env.Namespace)
		if absent {
			return files, "", nil
		}
		return files, why, nil
	}
	if v, set := watchFlag(*d); set {
		return config.NormalizeWatchNamespace(v), "", d
	}
	if c := managerContainer(*d); c != nil {
		expand := containerExpand(*d)
		for _, e := range c.Env {
			if e.Name == "WATCH_NAMESPACE" && e.ValueFrom == nil {
				if v := config.NormalizeWatchNamespace(expand(e.Value)); v != "" {
					return v, "", d
				}
			}
		}
	}
	if v, ok := old["WATCH_NAMESPACE"]; ok && v != nil {
		return config.NormalizeWatchNamespace(fmt.Sprint(v)), "", d
	}
	return "", "", d
}

// settingsBudget says whether the controller reads the limiter settings, as
// Config.SettingsBudgetApplies does once ResolvePolicyNamespace has run: not
// beside a separate policy namespace, which a controller that manages exactly
// its own namespace takes from that Namespace's policy-namespace label or
// from scaling-manager-policy when it exists. why names the separate
// namespace, or says what could not be read.
func (s *state) settingsBudget(ctx context.Context, st settingsState) (tri, string) {
	if st.deployWhy != "" {
		return triUnknown, st.deployWhy
	}
	ns := s.env.Namespace
	if config.NormalizeWatchNamespace(st.deployWatch) != ns {
		return triYes, ""
	}
	nss := s.env.Kube.CoreV1().Namespaces()
	n, err := nss.Get(ctx, ns, metav1.GetOptions{})
	switch {
	case err == nil:
		if p := n.Labels[constants.PolicyNamespaceLabelKey]; p != "" {
			if p == ns {
				return triYes, ""
			}
			return triNo, fmt.Sprintf("%s is labelled %s=%s", ns, constants.PolicyNamespaceLabelKey, p)
		}
	case !apierrors.IsNotFound(err):
		_, why := readVerdict(err, "namespace "+ns)
		return triUnknown, why
	}
	if _, err := nss.Get(ctx, config.WellKnownPolicyNamespace, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return triYes, ""
		}
		_, why := readVerdict(err, "namespace "+config.WellKnownPolicyNamespace)
		return triUnknown, why
	}
	if ns == config.WellKnownPolicyNamespace {
		return triYes, ""
	}
	return triNo, "namespace " + config.WellKnownPolicyNamespace + " exists"
}

// rejectedEffect says what the controller does with the rejected keys: a
// running one keeps the last good value of each; what its next start does
// depends on the key.
func (s *state) rejectedEffect(ctx context.Context, st settingsState) string {
	var start []string
	var others []string
	limiterDone := false
	for _, k := range slices.Sorted(maps.Keys(st.report.Rejected)) {
		switch {
		case k == keyPassive:
			start = append(start, "passive is true (a rejected passive fails closed)")
		case k == keyUtilizationShare:
			start = append(start, "utilizationShare is read from the scaling policy's default entry")
		case k == keyWatchNamespaces:
			start = append(start, "it watches the namespaces its Deployment names")
		case slices.Contains(limiterGroupKeys, k):
			if limiterDone {
				continue
			}
			limiterDone = true
			if st.unbounded == nil {
				start = append(start, "the limiters of the scaling policy's default entry apply")
				continue
			}
			switch budget, why := s.settingsBudget(ctx, st); budget {
			case triYes:
				start = append(start, "it refuses to start: limiter is set but the settings cannot build it")
			case triNo:
				start = append(start, "the limiter keys are not read beside the separate policy namespace ("+why+")")
			default:
				start = append(start, "it refuses to start unless a separate policy namespace supplies the limiters, which is not known ("+why+")")
			}
		default:
			others = append(others, k)
		}
	}
	if len(others) > 0 {
		start = append(start, strings.Join(others, ", ")+" from the next source down (config.yaml, the environment, the default)")
	}
	return "A running controller keeps the last good value of each; on its next start " + strings.Join(start, "; ")
}

// checkSettings reports whether the settings exist and the controller accepts
// every key: the verdict is config.ParseSettings', the controller's own, with
// watchNamespaces held to the namespaces the controller's Deployment names.
func checkSettings(ctx context.Context, s *state) ([]Result, string) {
	st := s.loadSettings(ctx)
	ns := s.env.Namespace
	create := fmt.Sprintf(`sed "s/__NAMESPACE__/%s/g" scaling-manager-settings.yaml | kubectl create -f -`, ns)
	if st.unread != "" {
		return []Result{{Status: Unknown, Detail: st.unread + "; every check below reads the shipped defaults"}}, ""
	}
	var bad []string
	for _, k := range slices.Sorted(maps.Keys(st.report.Rejected)) {
		bad = append(bad, k+": "+st.report.Rejected[k])
	}
	us := st.parsed.UtilizationShare
	if (us == config.UtilizationShareSettingShadow || us == shareActive) && st.value(keyLimiter) == "" {
		bad = append(bad, fmt.Sprintf("%s=%q needs a limiter: sharing divides a budget, and %s is empty", keyUtilizationShare, us, keyLimiter))
	}
	legacyNote := ""
	if len(st.fromLegacy) > 0 {
		legacyNote = fmt.Sprintf("; read %s from %s (config.yaml)", listText(st.fromLegacy, 10), LegacyConfigMap)
	}
	patch := fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"<key>":"<value>"}}'`, SettingsConfigMap, ns)
	switch {
	case len(bad) > 0:
		detail := "settings the controller rejects: " + strings.Join(bad, "; ")
		if len(st.report.Rejected) > 0 {
			detail += ". " + s.rejectedEffect(ctx, st)
		}
		fix := patch
		if st.watchWidens {
			fix = fmt.Sprintf(`kubectl patch configmap %s -n %s --type json -p '[{"op":"remove","path":"/data/%s"}]'`+
				"   # or set it to a subset of %s, what the controller's Deployment names\n",
				SettingsConfigMap, ns, keyWatchNamespaces, st.deployWatch) + patch
		}
		return []Result{{Status: Fail, Detail: detail, Fix: fix}}, ""
	case !st.found && !st.legacy:
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("ConfigMap %s does not exist in %s: the checks below read the shipped defaults, and prometheusURL has none", SettingsConfigMap, ns),
			Fix:    create + "   # after setting prometheusURL in the file"}}, ""
	case !st.found:
		return []Result{{Status: Warn,
			Detail: fmt.Sprintf("no ConfigMap %s in %s; read %s from %s (config.yaml), which the controller falls back to for one release",
				SettingsConfigMap, ns, listText(st.fromLegacy, 10), LegacyConfigMap),
			Fix: create + "   # with the values above"}}, ""
	case st.watchUnchecked:
		return []Result{{Status: Unknown,
			Detail: fmt.Sprintf("%s=%q: whether it only narrows the namespaces the controller's Deployment names, as the controller requires, is not known: %s",
				keyWatchNamespaces, st.value(keyWatchNamespaces), st.deployWhy) + legacyNote,
			Fix: fmt.Sprintf("let the preflight ServiceAccount get or list Deployment %s in %s, or run the preflight as a cluster admin",
				ControllerDeployment, ns)}}, ""
	case len(st.report.Unknown) > 0 || len(st.report.Warnings) > 0:
		var notes []string
		for _, k := range st.report.Unknown {
			note := k + " is no setting, and the controller ignores it"
			if hint := config.UnknownSettingHint(k); hint != "" {
				note += " (" + hint + ")"
			}
			notes = append(notes, note)
		}
		notes = append(notes, st.report.Warnings...)
		return []Result{{Status: Warn, Detail: strings.Join(notes, "; ") + legacyNote, Fix: patch}}, ""
	case len(st.fromLegacy) > 0:
		return []Result{{Status: Pass, Detail: fmt.Sprintf("read %s from %s", SettingsConfigMap, ns) + legacyNote}}, ""
	}
	return []Result{{Status: Pass, Detail: fmt.Sprintf("%s in %s parses: passive=%s, utilizationShare=%s, limiter=%q",
		SettingsConfigMap, ns, st.value(keyPassive), st.value(keyUtilizationShare), st.value(keyLimiter))}}, ""
}
