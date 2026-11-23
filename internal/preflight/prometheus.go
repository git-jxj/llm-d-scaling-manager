package preflight

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// The vLLM series the controller sizes from; a model server that exports none
// of them cannot be sized.
var requiredModelMetrics = []string{"vllm:kv_cache_usage_perc", "vllm:num_requests_waiting", "vllm:num_requests_running"}

// schemeHTTPS is the only scheme the token is sent over.
const schemeHTTPS = "https"

// thanosURL is the Thanos Querier the OpenShift install reads.
const thanosURL = "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"

// Prometheus answers instant queries.
type Prometheus interface {
	// Query runs an instant PromQL query and returns its vector.
	Query(ctx context.Context, query string) ([]Sample, error)
}

// Sample is one element of an instant vector.
type Sample struct {
	Labels map[string]string
	Value  float64
}

// PromErrorKind classifies a failed Prometheus query.
type PromErrorKind int

const (
	// PromUnreachable: no connection (refused, DNS, timeout).
	PromUnreachable PromErrorKind = iota
	// PromTLS: the certificate did not verify.
	PromTLS
	// PromDenied: Prometheus answered 401 or 403 for this account.
	PromDenied
	// PromBad: any other answer that is not a vector.
	PromBad
)

// PromError is a failed Prometheus query.
type PromError struct {
	Kind PromErrorKind
	Err  error
}

func (e *PromError) Error() string { return e.Err.Error() }
func (e *PromError) Unwrap() error { return e.Err }

// PrometheusConfig is what the client is built from.
type PrometheusConfig struct {
	URL                string
	InsecureSkipVerify bool
	// CA is the PEM of PrometheusCASecret's ca.crt, when there is one.
	CA []byte
}

// In-pod files the client reads: the account's token, sent as the controller
// sends its own, and on OpenShift the service CA the controller trusts.
const (
	tokenFile     = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	serviceCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"
)

// NewHTTPPrometheus is the client the Job uses: the system roots, the
// Prometheus CA and the service CA. It sends the Pod's ServiceAccount token
// only over https with the certificate verified: never over plain http and
// never with InsecureSkipVerify, where whoever answers at the URL the settings
// name would receive it.
func NewHTTPPrometheus(cfg PrometheusConfig) (Prometheus, error) {
	base, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parsing %q: %w", cfg.URL, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if len(cfg.CA) > 0 && !pool.AppendCertsFromPEM(cfg.CA) {
		return nil, fmt.Errorf("the mounted CA (Secret %s, key ca.crt) holds no PEM certificate", PrometheusCASecret)
	}
	if ca, err := os.ReadFile(serviceCAFile); err == nil {
		pool.AppendCertsFromPEM(ca)
	}
	token := ""
	if sendsToken(base, cfg.InsecureSkipVerify) {
		if b, err := os.ReadFile(tokenFile); err == nil {
			token = strings.TrimSpace(string(b))
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, InsecureSkipVerify: cfg.InsecureSkipVerify, MinVersion: tls.VersionTLS12} //nolint:gosec // the operator's prometheusInsecureSkipVerify, as the controller reads it
	return &httpPrometheus{base: base, token: token, client: &http.Client{Transport: tr, Timeout: 15 * time.Second}}, nil
}

// sendsToken reports whether the account's token may go to base: only over
// https, with the server's certificate verified.
func sendsToken(base *url.URL, insecureSkipVerify bool) bool {
	return base.Scheme == schemeHTTPS && !insecureSkipVerify
}

type httpPrometheus struct {
	base   *url.URL
	token  string
	client *http.Client
}

// Query runs an instant query against /api/v1/query.
func (p *httpPrometheus) Query(ctx context.Context, query string) ([]Sample, error) {
	u := *p.base
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/v1/query"
	u.RawQuery = url.Values{"query": {query}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, &PromError{Kind: PromBad, Err: err}
	}
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		var verr *tls.CertificateVerificationError
		var uerr x509.UnknownAuthorityError
		var herr x509.HostnameError
		if errors.As(err, &verr) || errors.As(err, &uerr) || errors.As(err, &herr) {
			return nil, &PromError{Kind: PromTLS, Err: err}
		}
		return nil, &PromError{Kind: PromUnreachable, Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, &PromError{Kind: PromDenied, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(string(body)))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &PromError{Kind: PromBad, Err: fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(string(body)))}
	}
	var r struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Value  []any             `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.Status != "success" {
		return nil, &PromError{Kind: PromBad, Err: fmt.Errorf("not a Prometheus query answer: %s", firstLine(string(body)))}
	}
	var out []Sample
	for _, e := range r.Data.Result {
		s := Sample{Labels: e.Metric}
		if len(e.Value) == 2 {
			if str, ok := e.Value[1].(string); ok {
				s.Value, _ = strconv.ParseFloat(str, 64)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// checkPrometheusReachable: prometheusURL is set, answers, and its
// certificate verifies against the CA the controller will mount.
func checkPrometheusReachable(ctx context.Context, s *state) ([]Result, string) {
	ns := s.env.Namespace
	st := s.loadSettings(ctx)
	raw := st.value(keyPrometheusURL)
	setURL := func(u string) string {
		return fmt.Sprintf(`kubectl patch configmap %s -n %s --type merge -p '{"data":{"%s":"%s"}}'`, SettingsConfigMap, ns, keyPrometheusURL, u)
	}
	s.promWhy = "Prometheus is not reachable (see prometheus.reachable)"
	if st.unread != "" {
		s.promWhy = "the settings cannot be read, so which Prometheus is not known (see settings)"
		return []Result{{Status: Unknown, Detail: s.promWhy}}, ""
	}
	if raw == "" {
		fix := setURL("https://<prometheus service>.<namespace>.svc:9090")
		if osh, _ := s.isOpenShift(); osh == triYes {
			fix = setURL(thanosURL)
		} else if cands := prometheusCandidates(ctx, s); cands != "" {
			fix += "\n# Prometheus resources here: " + cands
		}
		return []Result{{Status: Fail, Detail: keyPrometheusURL + " is not set, and the controller has nothing to read metrics from", Fix: fix}}, ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s=%q is not a URL with a host", keyPrometheusURL, raw), Fix: setURL("https://...")}}, ""
	}
	// pass is the result for a Prometheus that answered. Before the
	// controller's Deployment exists, a plain HTTP URL is a WARN unless smt
	// install said it applies the controller without a token: the release
	// files set PROMETHEUS_TOKEN_PATH, which the controller refuses over HTTP.
	pass := func(detail string) []Result {
		return []Result{{Status: Pass, Detail: detail}}
	}
	plain := u.Scheme == "http"
	if plain {
		if r, stop := plainHTTPResult(st, raw, ns, setURL("https://"+u.Host+u.Path)); stop {
			return []Result{r}, ""
		}
		switch {
		case st.deployWhy != "":
			// Forbidden is not absent: the token check was not made.
			pass = func(detail string) []Result {
				return []Result{{Status: Unknown,
					Detail: detail + "; whether the controller's Deployment sets a token, which it refuses to send over plain HTTP, is not known: " + st.deployWhy}}
			}
		case !st.deployFound && !st.tokenPathEmptied:
			pass = func(detail string) []Result {
				return []Result{{Status: Warn,
					Detail: detail + fmt.Sprintf("; the controller's Deployment does not exist yet, and the release files set %s on it, which the controller refuses over plain HTTP", envTokenPath),
					Fix: "before applying the controller, empty it in the main file: " + tokenPathSed +
						"\n# smt install --allow-http does it for you"}}
			}
		}
	}
	ca := s.env.PrometheusCA
	insecure := st.value(keyPrometheusInsecure) == valueTrue
	prom, err := s.env.NewPrometheus(PrometheusConfig{URL: raw, InsecureSkipVerify: insecure, CA: ca})
	if err != nil {
		return []Result{{Status: Fail, Detail: raw + ": " + err.Error(),
			Fix: fmt.Sprintf("kubectl create secret generic %s -n %s --from-file=ca.crt=<file> --dry-run=client -o yaml | kubectl apply -f -", PrometheusCASecret, ns)}}, ""
	}
	start := time.Now()
	_, err = prom.Query(ctx, "vector(1)")
	took := time.Since(start).Round(time.Millisecond)
	note := ""
	if insecure || u.Scheme != schemeHTTPS {
		note = "; certificate NOT verified, so the preflight's token was not sent"
	}
	var perr *PromError
	switch {
	case err == nil:
		s.prom, s.promUsable = prom, true
		return pass(fmt.Sprintf("%s answered in %s%s", raw, took, note)), ""
	case errors.As(err, &perr) && perr.Kind == PromDenied && plain:
		s.promWhy = fmt.Sprintf("Prometheus refused an unauthenticated query (%v): the metrics are not checked", err)
		return []Result{{Status: Fail,
			Detail: fmt.Sprintf("%s refused an unauthenticated query (%v); over plain HTTP the controller sends no token either, so its queries are refused too", raw, err),
			Fix:    "let this endpoint answer queries without a token, or use an https URL, where the controller sends its ServiceAccount token: " + setURL("https://"+u.Host+u.Path)}}, ""
	case errors.As(err, &perr) && perr.Kind == PromDenied:
		s.promWhy = fmt.Sprintf("Prometheus refused the preflight's account (%v): the metrics are not checked", err)
		return pass(fmt.Sprintf("%s answered (%v for the preflight's account: network and TLS work; the controller queries with its own)%s", raw, err, note)), ""
	case errors.As(err, &perr) && perr.Kind == PromTLS:
		fix := fmt.Sprintf("kubectl create secret generic %s -n %s --from-file=ca.crt=<the CA that signs Prometheus's certificate>", PrometheusCASecret, ns)
		if ca != nil {
			fix = fmt.Sprintf("kubectl create secret generic %s -n %s --from-file=ca.crt=<file> --dry-run=client -o yaml | kubectl apply -f -   # the mounted CA does not sign it", PrometheusCASecret, ns)
		}
		fix += "\nthen delete and re-apply the Job: it mounts the Secret at start"
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s: %v", raw, err), Fix: fix}}, ""
	case errors.As(err, &perr) && perr.Kind == PromBad:
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s answered, but not as Prometheus: %v", raw, err), Fix: setURL("<the Prometheus or Thanos Querier base URL>")}}, ""
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return []Result{{Status: Fail, Detail: fmt.Sprintf("%s did not answer within the check's time: %v", raw, err), Fix: "check the URL, and that no NetworkPolicy blocks " + ns + " from reaching it"}}, ""
	}
	return []Result{{Status: Fail, Detail: fmt.Sprintf("%s: %v", raw, err), Fix: "check the URL, and that no NetworkPolicy blocks " + ns + " from reaching it"}}, ""
}

// tokenPathSedExpr is the sed program the install guides run on a release
// main file to empty PROMETHEUS_TOKEN_PATH (hack/check-release-manifests.sh
// checks the guides use it and that it changes exactly one line).
const tokenPathSedExpr = `s#value: /var/run/secrets/kubernetes.io/serviceaccount/token$#value: ""#`

// tokenPathSed is that sed, on the main file of each install.
const tokenPathSed = `sed -i '` + tokenPathSedExpr + `' scaling-manager-namespaced.yaml   # or scaling-manager-cluster.yaml, scaling-manager-namespaced-openshift.yaml, scaling-manager-cluster-openshift.yaml`

// settingsPatch is the kubectl patch that sets one key of the settings.
func settingsPatch(ns, key, value string) string {
	return fmt.Sprintf("kubectl patch configmap %s -n %s --type merge -p '{\"data\":{\"%s\":\"%s\"}}'", SettingsConfigMap, ns, key, value)
}

// tokenFix is how to stop the controller sending the token key over plain
// HTTP, where the Deployment sets it: in the file it is applied from (or the
// next apply brings it back), and on the live Deployment when there is one.
func tokenFix(st settingsState, ns, key string) string {
	if key != envTokenPath {
		fix := fmt.Sprintf("remove %s from the controller container in the main file you apply", key)
		if st.deployFound {
			fix = fmt.Sprintf("kubectl set env deployment/%s -n %s %s-\n# and %s, or the next apply brings it back", ControllerDeployment, ns, key, fix)
		}
		return fix
	}
	if !st.deployFound {
		return tokenPathSed
	}
	return fmt.Sprintf("kubectl set env deployment/%s -n %s %s=", ControllerDeployment, ns, key) +
		"\n# and in the main file you apply, or the next apply brings it back: " + tokenPathSed
}

// tlsFix is how to remove one TLS setting: a patch of the settings, an unset
// on the Deployment, or an edit of config.yaml.
func tlsFix(ns string, t tlsSource) string {
	switch {
	case strings.HasPrefix(t.source, t.settingKey+" in ConfigMap ") && t.settingKey != "":
		v := ""
		if t.settingKey == keyPrometheusInsecure {
			v = settingsDefaults[keyPrometheusInsecure]
		}
		return settingsPatch(ns, t.settingKey, v)
	case strings.Contains(t.source, " on Deployment "):
		return fmt.Sprintf("kubectl set env deployment/%s -n %s %s-   # and in the main file you apply", ControllerDeployment, ns, t.loaderKey)
	}
	return fmt.Sprintf("kubectl edit configmap %s -n %s   # remove %s from config.yaml", LegacyConfigMap, ns, t.loaderKey)
}

// plainHTTPResult is the result for an http:// Prometheus URL raw the
// controller refuses to start on (internal/prometheus ValidateTLSConfig): one
// it is not told to allow, one it would send a bearer token to, or one with
// a TLS setting beside it; UNKNOWN when a valueFrom the preflight cannot
// read decides. stop is false when it starts on it. toHTTPS is the patch to
// an https URL.
func plainHTTPResult(st settingsState, raw, ns, toHTTPS string) (r Result, stop bool) {
	allowed, unknown := st.allowHTTP()
	if unknown != "" {
		return Result{Status: Unknown,
			Detail: fmt.Sprintf("%s is plain HTTP, and whether the controller allows it is not known: %s comes from a reference the preflight does not read", raw, unknown)}, true
	}
	if !allowed {
		token := tokenFix(st, ns, envTokenPath)
		return Result{Status: Fail,
			Detail: fmt.Sprintf("%s is plain HTTP, which the controller refuses unless %s is true", raw, keyAllowHTTP),
			Fix: toHTTPS +
				"\n# or, to read it over plain HTTP (unencrypted, no token): smt install ... --prometheus-url " + raw + " --allow-http" +
				"\n# by hand: " + settingsPatch(ns, keyAllowHTTP, valueTrue) +
				"\n#   and empty the token path (the controller sends no token over plain HTTP): " + strings.ReplaceAll(token, "\n# ", "\n#   ")}, true
	}
	set, unknownTLS := st.tlsBesideHTTP()
	if len(set) > 0 {
		var names, fixes []string
		for _, t := range set {
			names = append(names, t.source)
			fixes = append(fixes, tlsFix(ns, t))
		}
		return Result{Status: Fail,
			Detail: fmt.Sprintf("%s is plain HTTP, and the controller refuses TLS settings beside it: %s", raw, strings.Join(names, "; ")),
			Fix:    strings.Join(fixes, "\n") + "\n# or use an https URL: " + toHTTPS}, true
	}
	tok, key, onDeployment := st.bearerToken()
	if tok != "" {
		fix := fmt.Sprintf("kubectl edit configmap %s -n %s   # remove %s from config.yaml", LegacyConfigMap, ns, key)
		if onDeployment {
			fix = tokenFix(st, ns, key) +
				"\n# smt install ... --allow-http applies the controller with PROMETHEUS_TOKEN_PATH empty, so a re-run keeps it so"
		}
		return Result{Status: Fail,
			Detail: fmt.Sprintf("%s is plain HTTP and the controller has a bearer token for it (%s): it refuses to send one over plain HTTP, and does not start", raw, tok),
			Fix:    fix + "\n# or use an https URL: " + toHTTPS}, true
	}
	if len(unknownTLS) > 0 {
		return Result{Status: Unknown,
			Detail: fmt.Sprintf("%s is plain HTTP, and whether the controller has a TLS setting it refuses beside it is not known: %s comes from a reference the preflight does not read", raw, strings.Join(unknownTLS, "; "))}, true
	}
	return Result{}, false
}

// prometheusCandidates names the Prometheus resources in the cluster, for a
// URL suggestion; empty when there are none or they cannot be read.
func prometheusCandidates(ctx context.Context, s *state) string {
	list, err := s.env.Dynamic.Resource(prometheusGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil || len(list.Items) == 0 {
		return ""
	}
	var out []string
	for _, p := range list.Items {
		out = append(out, fmt.Sprintf("%s/%s (Service prometheus-operated.%s.svc:9090)", p.GetNamespace(), p.GetName(), p.GetNamespace()))
	}
	return listText(out, 5)
}

// checkModelMetrics: Prometheus holds the vLLM series for the model servers
// of every managed namespace.
func checkModelMetrics(ctx context.Context, s *state) ([]Result, string) {
	pods, podsWhy := s.modelServers(ctx)
	nss, named := s.managed()
	if !named {
		if pods == nil {
			return []Result{{Status: Unknown, Detail: "which namespaces run model servers is not known: " + podsWhy}}, ""
		}
		for ns := range pods {
			nss = append(nss, ns)
		}
		if len(nss) == 0 {
			return []Result{{Status: Warn, Detail: "no model-server pod (" + modelServerSelector + ") runs in any namespace yet, so the metrics are not checked"}}, ""
		}
	}
	if !s.promUsable {
		return []Result{{Status: Unknown, Detail: s.promWhy}}, ""
	}
	sel := strings.Join(requiredModelMetrics, "|")
	q := fmt.Sprintf(`count by (namespace, __name__) ({__name__=~%q, namespace=~%q})`, sel, strings.Join(nss, "|"))
	got, err := s.prom.Query(ctx, q)
	if err != nil {
		var perr *PromError
		if errors.As(err, &perr) && perr.Kind == PromDenied {
			return []Result{{Status: Unknown, Detail: "the metrics query was denied: " + err.Error()}}, ""
		}
		return []Result{{Status: Unknown, Detail: "the metrics query failed: " + err.Error()}}, ""
	}
	have := map[string]map[string]bool{}
	for _, smp := range got {
		ns, name := smp.Labels["namespace"], smp.Labels["__name__"]
		if have[ns] == nil {
			have[ns] = map[string]bool{}
		}
		have[ns][name] = true
	}
	var fails, warns, unknowns, ok []string
	for _, ns := range nss {
		switch {
		case len(have[ns]) == 0 && pods != nil && len(pods[ns]) == 0:
			warns = append(warns, ns+": no model-server pod runs here yet, so nothing to check")
		case len(have[ns]) == 0 && pods == nil:
			// Whether model servers run there is not known: a refused
			// pod list is not "none", and not "some" either.
			unknowns = append(unknowns, ns)
		case len(have[ns]) == 0:
			if fresh := freshPodMonitor(ctx, s, ns, pods[ns]); fresh != "" {
				warns = append(warns, ns+": no vllm: series yet, and "+fresh)
				continue
			}
			fails = append(fails, ns)
		case len(have[ns]) < len(requiredModelMetrics):
			var missing []string
			for _, m := range requiredModelMetrics {
				if !have[ns][m] {
					missing = append(missing, m)
				}
			}
			warns = append(warns, ns+": missing "+strings.Join(missing, ", "))
		default:
			ok = append(ok, ns)
		}
	}
	if len(fails) > 0 {
		return []Result{{Status: Fail,
			Detail: "no vllm: series in Prometheus for " + listText(fails, 8) + ": nothing scrapes the model servers there, so the controller has nothing to size from",
			Fix:    `sed -e "s/__NAMESPACE__/` + s.env.Namespace + `/g" -e "s/__MODEL_NAMESPACE__/<model namespace>/g" scaling-manager-model-namespace.yaml | kubectl apply -f -   # the model-server PodMonitor; see podmonitor.port`}}, ""
	}
	if len(unknowns) > 0 {
		return []Result{{Status: Unknown, Detail: "no vllm: series for " + listText(unknowns, 8) + ", and whether model servers run there is not known: " + podsWhy}}, ""
	}
	if len(warns) > 0 {
		return []Result{{Status: Warn, Detail: strings.Join(warns, "; ")}}, ""
	}
	if podsWhy != "" {
		return []Result{{Status: Pass, Detail: "vllm: series present in " + listText(ok, 8) + " (model-server pods not listed: " + podsWhy + ")"}}, ""
	}
	return []Result{{Status: Pass, Detail: "vllm: series present in " + listText(ok, 8)}}, ""
}

// podMonitorScrapeGrace is how long Prometheus may take to pick up a new
// PodMonitor and scrape it once, before one scrape interval: the operator
// reloads the configuration, then the first scrape lands.
const podMonitorScrapeGrace = 2 * time.Minute

// freshPodMonitor names a PodMonitor in ns that selects one of the model
// servers and is younger than podMonitorScrapeGrace plus its scrape
// interval, with its age; empty when there is none. An install right after
// admin-setup created the PodMonitor finds no series only because
// Prometheus has not scraped it yet, which is not a missing scrape.
func freshPodMonitor(ctx context.Context, s *state, ns string, pods []corev1.Pod) string {
	if len(pods) == 0 {
		return ""
	}
	list, err := s.env.Dynamic.Resource(podMonitorGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return ""
	}
	now := s.env.Now()
	for i := range list.Items {
		pm := &list.Items[i]
		sel, ok := podMonitorSelector(pm)
		if !ok {
			continue
		}
		selects := false
		for _, p := range pods {
			if sel.Matches(labels.Set(p.Labels)) {
				selects = true
				break
			}
		}
		if !selects {
			continue
		}
		grace := podMonitorScrapeGrace + podMonitorInterval(pm)
		age := now.Sub(pm.GetCreationTimestamp().Time)
		if age >= 0 && age < grace {
			return fmt.Sprintf("PodMonitor %s is %s old: Prometheus has not scraped the new PodMonitor yet; re-run in ~2 min (it FAILs once it is %s old and still no series arrive)",
				pm.GetName(), age.Round(time.Second), grace.Round(time.Second))
		}
	}
	return ""
}

// podMonitorInterval is the longest scrape interval of a PodMonitor's
// endpoints; 0 when none is set or none parses (Prometheus's default then
// applies, which this does not know).
func podMonitorInterval(pm *unstructured.Unstructured) time.Duration {
	eps, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	var longest time.Duration
	for _, e := range eps {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		v, _ := m["interval"].(string)
		if d, err := time.ParseDuration(v); err == nil && d > longest {
			longest = d
		}
	}
	return longest
}

// checkPodMonitorPort: in each namespace that runs model servers, a
// PodMonitor selects them and names a port they declare, and its targets are
// up. The port rule is PR #64's (llmsm_modelserver_port_check): a PodMonitor
// naming a port no container declares yields no target at all.
func checkPodMonitorPort(ctx context.Context, s *state) ([]Result, string) {
	pods, why := s.modelServers(ctx)
	if pods == nil {
		return []Result{{Status: Unknown, Detail: "the model-server pods cannot be listed: " + why}}, ""
	}
	if len(pods) == 0 {
		return []Result{{Status: Warn, Detail: "no model-server pod (" + modelServerSelector + ") runs in a managed namespace yet, so the port the PodMonitor scrapes is not checked"}}, ""
	}
	var results []Result
	for _, ns := range sortedKeys(pods) {
		results = append(results, podMonitorInNamespace(ctx, s, ns, pods[ns]))
	}
	return results, ""
}

func podMonitorInNamespace(ctx context.Context, s *state, ns string, pods []corev1.Pod) Result {
	fix := `sed -e "s/__NAMESPACE__/` + s.env.Namespace + `/g" -e "s/__MODEL_NAMESPACE__/` + ns + `/g" scaling-manager-model-namespace.yaml | kubectl apply -f -`
	list, err := s.env.Dynamic.Resource(podMonitorGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		absent, why := readVerdict(err, "PodMonitors in "+ns)
		if absent {
			return Result{Status: Warn, Detail: ns + ": the PodMonitor API is not served (no Prometheus Operator), so how the model servers are scraped is not checked; metrics.model says whether their series arrive"}
		}
		return Result{Status: Unknown, Detail: ns + ": " + why}
	}
	type pmMatch struct {
		name   string
		ports  []string
		labels map[string]string
		pods   []corev1.Pod
	}
	var matches []pmMatch
	var ours *unstructured.Unstructured
	for i := range list.Items {
		pm := &list.Items[i]
		if pm.GetLabels()["app.kubernetes.io/component"] == "modelserver-metrics" {
			ours = pm
		}
		sel, ok := podMonitorSelector(pm)
		if !ok {
			continue
		}
		var selected []corev1.Pod
		for _, p := range pods {
			if sel.Matches(labels.Set(p.Labels)) {
				selected = append(selected, p)
			}
		}
		if len(selected) > 0 {
			matches = append(matches, pmMatch{name: pm.GetName(), ports: podMonitorPorts(pm), labels: pm.GetLabels(), pods: selected})
		}
	}
	if len(matches) == 0 {
		if ours != nil {
			return Result{Status: Fail, Detail: fmt.Sprintf("%s: PodMonitor %s selects none of the %d model-server pods (%s), so nothing is scraped",
				ns, ours.GetName(), len(pods), podNames(pods)), Fix: "give the model-server pods the labels it selects (llm-d.ai/role: decode or prefill), or add a PodMonitor of your own that selects them"}
		}
		return Result{Status: Fail, Detail: fmt.Sprintf("%s: no PodMonitor selects the %d model-server pods (%s), so nothing scrapes them", ns, len(pods), podNames(pods)), Fix: fix}
	}
	var bad, good []string
	for _, m := range matches {
		s.podMonitors = append(s.podMonitors, podMonitorRef{namespace: ns, name: m.name, labels: m.labels})
		if len(m.ports) == 0 {
			// Endpoints by number (targetPort, portNumber): nothing to name.
			good = append(good, m.name)
			continue
		}
		declared := false
		seen := make([]string, 0, len(m.pods))
		for _, p := range m.pods {
			names := podPortNames(p)
			seen = append(seen, p.Name+" ("+strings.Join(names, ", ")+")")
			for _, n := range names {
				for _, want := range m.ports {
					if n == want {
						declared = true
					}
				}
			}
		}
		if declared {
			good = append(good, m.name)
		} else {
			bad = append(bad, fmt.Sprintf("PodMonitor %s scrapes the port named %s, and no model-server pod declares it: %s", m.name, strings.Join(m.ports, ","), listText(seen, 5)))
		}
	}
	if len(good) == 0 {
		return Result{Status: Fail,
			Detail: ns + ": " + strings.Join(bad, "; ") + ". A PodMonitor that names a port no container declares has no target at all: nothing is scraped, and nothing reports an error",
			Fix:    modelServerPortFix(s, pods)}
	}
	if !s.promUsable {
		return Result{Status: Pass, Detail: fmt.Sprintf("%s: PodMonitor %s selects the model servers on a port they declare (targets not checked: %s)", ns, strings.Join(good, ", "), s.promWhy)}
	}
	var down []string
	for _, name := range good {
		got, err := s.prom.Query(ctx, fmt.Sprintf(`up{job=%q}`, ns+"/"+name))
		if err != nil {
			return Result{Status: Unknown, Detail: fmt.Sprintf("%s: whether the targets of PodMonitor %s are up is not known: %v", ns, name, err)}
		}
		for _, smp := range got {
			if smp.Value != 1 {
				down = append(down, smp.Labels["instance"])
			}
		}
	}
	if len(down) > 0 {
		return Result{Status: Fail, Detail: fmt.Sprintf("%s: targets of PodMonitor %s are down: %s", ns, strings.Join(good, ", "), listText(down, 5)),
			Fix: "kubectl get --raw /api/v1/namespaces/" + ns + "/pods/<pod>:<port>/proxy/metrics   # does the model server answer on /metrics?"}
	}
	return Result{Status: Pass, Detail: fmt.Sprintf("%s: PodMonitor %s selects the model servers on a port they declare", ns, strings.Join(good, ", "))}
}

// modelServerPortFix is the fix for model servers whose port the PodMonitor
// does not scrape: the cluster admin renders the release PodMonitor for the
// port they declare (smt --modelserver-port), named when the pods agree on
// one; or the model server names its port as the PodMonitor scrapes it.
func modelServerPortFix(s *state, pods []corev1.Pod) string {
	port := commonPort(pods)
	if port == "" {
		port = "<the metrics port, by name or number>"
	}
	cmd := "smt admin-setup -n " + s.env.Namespace + " --modelserver-port " + port
	if s.env.Scope == ScopeCluster {
		cmd = "smt install --scope cluster <the flags it was installed with> --modelserver-port " + port
	}
	return cmd + "   # cluster admin: the PodMonitor scrapes that port. Or name the model server's metrics port as the PodMonitor scrapes it"
}

// commonPort is the one container port every pod declares, by name, or by
// number where it has no name; "metrics" when that is one of several; empty
// otherwise.
func commonPort(pods []corev1.Pod) string {
	var common map[string]bool
	for _, p := range pods {
		have := map[string]bool{}
		for _, c := range append(append([]corev1.Container(nil), p.Spec.Containers...), p.Spec.InitContainers...) {
			for _, port := range c.Ports {
				k := port.Name
				if k == "" {
					k = strconv.Itoa(int(port.ContainerPort))
				}
				have[k] = true
			}
		}
		if common == nil {
			common = have
			continue
		}
		for k := range common {
			if !have[k] {
				delete(common, k)
			}
		}
	}
	if len(common) == 1 {
		for k := range common {
			return k
		}
	}
	if common["metrics"] {
		return "metrics"
	}
	return ""
}

// podMonitorSelector is a PodMonitor's pod selector; false when it has none
// or it does not parse.
func podMonitorSelector(pm *unstructured.Unstructured) (labels.Selector, bool) {
	raw, ok, _ := unstructured.NestedMap(pm.Object, "spec", "selector")
	if !ok {
		return nil, false
	}
	sel, err := selectorOf(raw)
	if err != nil || sel.Empty() {
		return nil, false
	}
	return sel, true
}

// podMonitorPorts are the port names a PodMonitor's endpoints scrape.
func podMonitorPorts(pm *unstructured.Unstructured) []string {
	eps, _, _ := unstructured.NestedSlice(pm.Object, "spec", "podMetricsEndpoints")
	var out []string
	for _, e := range eps {
		if m, ok := e.(map[string]any); ok {
			if p, _ := m["port"].(string); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// podPortNames lists a pod's container port names, init (sidecar) containers
// included: a sidecar is discovered like any other container.
func podPortNames(p corev1.Pod) []string {
	containers := append(append([]corev1.Container(nil), p.Spec.Containers...), p.Spec.InitContainers...)
	n := 0
	for _, c := range containers {
		n += len(c.Ports)
	}
	out := make([]string, 0, n)
	for _, c := range containers {
		for _, port := range c.Ports {
			name := port.Name
			if name == "" {
				name = "unnamed " + strconv.Itoa(int(port.ContainerPort))
			}
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		out = []string{"no ports"}
	}
	return out
}

func podNames(pods []corev1.Pod) string {
	names := make([]string, len(pods))
	for i, p := range pods {
		names[i] = p.Name
	}
	return listText(names, 5)
}

// checkPrometheusSelects: a Prometheus resource selects each PodMonitor
// podmonitor.port found, by its PodMonitor and namespace selectors.
func checkPrometheusSelects(ctx context.Context, s *state) ([]Result, string) {
	if len(s.podMonitors) == 0 {
		return nil, "no PodMonitor selects a model server (see podmonitor.port)"
	}
	list, err := s.env.Dynamic.Resource(prometheusGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		absent, why := readVerdict(err, "Prometheus resources in every namespace")
		if absent {
			return []Result{{Status: Warn, Detail: "the Prometheus API is not served (no Prometheus Operator), so which Prometheus scrapes the PodMonitors is not checked"}}, ""
		}
		return []Result{{Status: Unknown, Detail: why}}, ""
	}
	if len(list.Items) == 0 {
		return []Result{{Status: Fail, Detail: "no Prometheus resource in the cluster: nothing reads the PodMonitors", Fix: "install a Prometheus that selects PodMonitors, e.g. kube-prometheus-stack"}}, ""
	}
	nsLabels := map[string]map[string]string{}
	var results []Result
	for _, pm := range s.podMonitors {
		if _, ok := nsLabels[pm.namespace]; !ok {
			n, err := s.env.Kube.CoreV1().Namespaces().Get(ctx, pm.namespace, metav1.GetOptions{})
			if err != nil {
				results = append(results, unknown("namespace "+pm.namespace, err))
				continue
			}
			nsLabels[pm.namespace] = n.Labels
		}
		var by, wants []string
		for i := range list.Items {
			p := &list.Items[i]
			ok, want := prometheusSelects(p, pm, nsLabels[pm.namespace])
			if ok {
				by = append(by, p.GetNamespace()+"/"+p.GetName())
			} else if want != "" {
				wants = append(wants, want)
			}
		}
		if len(by) > 0 {
			results = append(results, Result{Status: Pass, Detail: fmt.Sprintf("PodMonitor %s/%s is selected by Prometheus %s", pm.namespace, pm.name, listText(by, 3))})
			continue
		}
		results = append(results, Result{Status: Fail,
			Detail: fmt.Sprintf("no Prometheus resource selects PodMonitor %s/%s: %s", pm.namespace, pm.name, listText(wants, 3)),
			Fix:    fmt.Sprintf("kubectl label podmonitor %s -n %s <the label the Prometheus selects, above>", pm.name, pm.namespace)})
	}
	return results, ""
}

// prometheusSelects reports whether Prometheus p selects pm, and when not,
// what it would need. A nil podMonitorSelector selects nothing; a nil
// namespace selector means the Prometheus's own namespace only.
func prometheusSelects(p *unstructured.Unstructured, pm podMonitorRef, nsLabels map[string]string) (bool, string) {
	name := p.GetNamespace() + "/" + p.GetName()
	raw, ok, _ := unstructured.NestedMap(p.Object, "spec", "podMonitorSelector")
	if !ok {
		return false, name + " selects no PodMonitor (podMonitorSelector unset)"
	}
	sel, err := selectorOf(raw)
	if err != nil {
		return false, name + ": podMonitorSelector does not parse"
	}
	if !sel.Matches(labels.Set(pm.labels)) {
		return false, fmt.Sprintf("%s selects PodMonitors by %s", name, sel.String())
	}
	nsRaw, ok, _ := unstructured.NestedMap(p.Object, "spec", "podMonitorNamespaceSelector")
	if !ok {
		if p.GetNamespace() == pm.namespace {
			return true, ""
		}
		return false, name + " reads PodMonitors in its own namespace only (podMonitorNamespaceSelector unset)"
	}
	nsSel, err := selectorOf(nsRaw)
	if err != nil {
		return false, name + ": podMonitorNamespaceSelector does not parse"
	}
	if !nsSel.Matches(labels.Set(nsLabels)) {
		return false, fmt.Sprintf("%s reads PodMonitors in namespaces labelled %s, and %s is not", name, nsSel.String(), pm.namespace)
	}
	return true, ""
}

func selectorOf(raw map[string]any) (labels.Selector, error) {
	var ls metav1.LabelSelector
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(&ls)
}
