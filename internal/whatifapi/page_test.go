package whatifapi

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// get sends a GET of path to h, with the authorization header when given.
func get(h http.Handler, path, auth string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	h.ServeHTTP(rec, req)
	return rec
}

// pageText is an embedded file of the page, its line ends as LF: a Windows
// checkout embeds CRLF.
func pageText(t *testing.T, name string) string {
	t.Helper()
	b, err := pageFS.ReadFile("page/" + name)
	require.NoError(t, err)
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// The page is served from the binary at / with its files under /page/, each
// with its own content type and the page's security headers; any other file
// is a 404.
func TestPageServed(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	for path, want := range map[string]struct{ ct, contains string }{
		"/":                 {"text/html; charset=utf-8", `<script src="page/planner.js" defer></script>`},
		"/page/planner.js":  {"text/javascript; charset=utf-8", "FLAG_WORDS"},
		"/page/scenario.js": {"text/javascript; charset=utf-8", "function linkProblem"},
		"/page/planner.css": {"text/css; charset=utf-8", "prefers-color-scheme: dark"},
	} {
		rec := get(h, path, "")
		require.Equal(t, http.StatusOK, rec.Code, path)
		assert.Equal(t, want.ct, rec.Header().Get("Content-Type"), path)
		assert.Equal(t, pageCSP, rec.Header().Get("Content-Security-Policy"), path)
		assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"), path)
		assert.Equal(t, "no-cache", rec.Header().Get("Cache-Control"), path)
		assert.Contains(t, rec.Body.String(), want.contains, path)
	}
	assert.Contains(t, pageText(t, "index.html"), `href="page/planner.css"`)

	// The policy's directives, read one by one: nothing but the page's own
	// origin, and no inline script or style.
	rec := get(h, "/", "")
	directives := map[string]string{}
	for _, d := range strings.Split(rec.Header().Get("Content-Security-Policy"), ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
		directives[name] = value
	}
	for name, want := range map[string]string{"default-src": "'none'", "script-src": "'self'", "style-src": "'self'",
		"connect-src": "'self'", "frame-ancestors": "'none'", "base-uri": "'none'", "form-action": "'none'"} {
		assert.Equal(t, want, directives[name], "Content-Security-Policy %s", name)
	}
	assert.NotContains(t, rec.Header().Get("Content-Security-Policy"), "unsafe-")
	assert.Equal(t, "no-referrer", rec.Header().Get("Referrer-Policy"))
	for _, path := range []string{"/page/", "/page/index.html", "/page/page.go", "/index.html"} {
		assert.Equal(t, http.StatusNotFound, get(h, path, "").Code, path)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}")))
	assert.Equal(t, http.StatusNotFound, rec.Code, "the page is GET only")
}

// The page sits behind the API's own authentication: with Kubernetes auth no
// token is 401, a user not allowed get on /whatif/ is 403, and an allowed one
// gets the page; with none, a foreign Host is 403.
func TestPageBehindAuth(t *testing.T) {
	h := New(Config{Resolver: &stubResolver{}}).Handler()
	allowed := NewAuthenticator(newCluster("/whatif/", ""), nil, 0, nil).Wrap(h)
	assert.Equal(t, http.StatusUnauthorized, get(allowed, "/", "").Code)
	assert.Equal(t, http.StatusUnauthorized, get(allowed, "/page/planner.js", "Bearer bad").Code)
	rec := get(allowed, "/", "Bearer good")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "Capacity planner")

	planOnly := NewAuthenticator(newCluster("/whatif/v1/plan", ""), nil, 0, nil).Wrap(h)
	assert.Equal(t, http.StatusForbidden, get(planOnly, "/", "Bearer good").Code)
	assert.Equal(t, http.StatusForbidden, get(planOnly, "/page/planner.js", "Bearer good").Code)

	loop := LoopbackOnly(h)
	for host, want := range map[string]int{"127.0.0.1:8080": http.StatusOK, "localhost:8080": http.StatusOK, "rebound.example:8080": http.StatusForbidden} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
		loop.ServeHTTP(rec, req)
		assert.Equal(t, want, rec.Code, host)
	}
}

// The page loads nothing from another origin and runs no inline script or
// style, so its Content-Security-Policy holds and it works air-gapped.
func TestPageLoadsNothingExternal(t *testing.T) {
	entries, err := pageFS.ReadDir("page")
	require.NoError(t, err)
	require.Len(t, entries, len(pageFiles), "every embedded file is served, and no more")
	for _, e := range entries {
		// A reference to an element of the page itself (the graphs' clip
		// paths) is no load.
		text := strings.ReplaceAll(pageText(t, e.Name()), "url(#", "")
		for _, bad := range []string{"http:", "https:", `src="//`, `href="//`, "url(", "@import", " style=\"", "javascript:", "eval("} {
			assert.NotContains(t, text, bad, "%s", e.Name())
		}
	}
	html := pageText(t, "index.html")
	// Two scripts by src, the scenario logic first: planner.js calls it.
	assert.Equal(t, 2, strings.Count(html, "<script"), "two scripts, by src")
	assert.Less(t, strings.Index(html, `<script src="page/scenario.js" defer></script>`), strings.Index(html, `<script src="page/planner.js" defer></script>`))
	assert.GreaterOrEqual(t, strings.Index(html, `<script src="page/scenario.js" defer></script>`), 0)
	// The page reaches its files and the API by paths relative to itself, so
	// that a proxy may serve it under a prefix.
	for _, name := range []string{"planner.js", "scenario.js"} {
		js := pageText(t, name)
		for _, abs := range []string{`"/v1/`, "`/v1/", `'/v1/`, `"/page/`} {
			assert.NotContains(t, js, abs, "an absolute path in %s", name)
		}
	}
	assert.NotContains(t, html, `="/page/`, "an absolute path")
	assert.NotContains(t, html, "<style")
}

// Every element the script reads by id is on the page.
func TestPageIDsExist(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	ids := regexp.MustCompile(`el\("([^"]+)"\)`).FindAllStringSubmatch(js, -1)
	require.Greater(t, len(ids), 50, "found too few ids")
	for _, m := range ids {
		assert.Contains(t, html, `id="`+m[1]+`"`, "the script reads #%s", m[1])
	}
	// The ids the script reads through a variable: the steppers and the
	// targets' sliders and boxes.
	for _, id := range []string{"prefill", "decode", "ttft", "ttftbox", "tpot", "tpotbox"} {
		assert.Contains(t, html, `id="`+id+`"`)
	}
}

// The page asks both questions: a toggle between them, a size posted to
// v1/size by a relative path with no fleet in it (the fleet is the answer),
// the steppers read only in size mode, and the mode in the link.
func TestPageAsksSize(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	for _, want := range []string{`id="mplan"`, `id="msize"`, "How much does this fleet serve?", "What fleet does this traffic need?",
		`id="sizebox"`, `id="maxrep"`, `id="sizelow"`, `id="sizeanswer" aria-live="polite"`} {
		assert.Contains(t, html, want)
	}
	for _, want := range []string{`api("POST", "v1/size", sizeRequest(), signal)`, "state.abort = new AbortController();", "state.abort.abort();",
		`'raise "Search up to" (at most 64)'`, "More replicas do not lower a floor", `under "Assume a term the source did not learn"`,
		"delete req.fleet;", `p.set("mode", "size")`,
		`req.sizeOn = "low";`, "modeSeq !== state.modeSeq", `el("perbucket").checked = k.perBucket;`,
		`el("prefill").disabled = el("pdec").disabled = el("pinc").disabled = !state.inputs.fleet.prefill || sizing;`} {
		assert.Contains(t, js, want)
	}
}

// The page asks again only where retryDelay (scenario.js, checked under node
// by hack/check-planner-scenarios.sh) says to: a busy server's 503, never a
// plan stopped at the server's plan budget.
func TestPageRetriesOnlyABusyServer(t *testing.T) {
	js := pageText(t, "planner.js")
	assert.Contains(t, js, `const wait = retryDelay(resp.status, resp.headers.get("Retry-After"), attempt);`)
	assert.NotContains(t, js, "resp.status === 503", "planner.js decides a retry on its own")
	assert.Contains(t, pageText(t, "scenario.js"), "function retryDelay(status, retryAfter, attempt)")
}

// The page saves, exports, imports and compares scenarios: every control is
// on the page and wired to its handler, the store and the file carry the
// link, every key the link writes is one an imported link may carry, a
// name read back reaches the page only through esc, and a pinned plan is a
// copy, not priced again.
func TestPageSavesAndCompares(t *testing.T) {
	html, js := pageText(t, "index.html"), pageText(t, "planner.js")
	for id, wire := range map[string]string{
		"scensave":   `el("scensave").onclick = saveScenario;`,
		"scenopen":   `el("scenopen").onclick =`,
		"scendelete": `el("scendelete").onclick = deleteScenario;`,
		"scenexport": `el("scenexport").onclick = exportScenario;`,
		"scenimport": `el("scenimport").onchange = () => importScenario(el("scenimport").files[0]);`,
		"pina":       `el("pina").onclick = pinA;`,
		"unpin":      `el("unpin").onclick = unpin;`,
	} {
		assert.Contains(t, html, `id="`+id+`"`)
		assert.Contains(t, js, wire, "#%s is not wired", id)
	}
	for _, id := range []string{"scenname", "scenlist", "scencount", "pinnote", "comparebox", "comparewhat", "comparetable"} {
		assert.Contains(t, html, `id="`+id+`"`)
	}
	assert.Contains(t, html, `<input type="file" id="scenimport"`)

	// One format: the store and the file hold linkParams' text, and writeLink
	// puts the same text in the address. The checks, the store's edits and
	// the names are scenario.js's, run on fixtures by
	// hack/check-planner-scenarios.sh; here, that the page calls them.
	sjs := pageText(t, "scenario.js")
	for _, want := range []string{`"#" + linkParams().toString()`, `link: linkParams().toString()`,
		`localStorage.setItem(SCENARIO_STORE`, "readScenarioFile(await file.text())", "file.size > MAX_FILE",
		"putScenario(readStore(),", "dropScenario(readStore(), sc.name)", "importPlan(readStore(),",
		"visibleScenarios(readStore())", "new Option(", "state.link = linkAtLoadStart(state.link, seq);",
		"linkAtLoadEnd(state.link, seq, state.seq, true)", "linkAtLoadEnd(state.link, seq, state.seq, false)", "beginOpen(pageBefore(), sc)",
		"sourceProblem(sc.link, state.sources)", "compareRows(pin.plan, cur,", "sameCurve(p, pts(current))", "state.priceFailed = true;"} {
		assert.Contains(t, js, want)
	}
	assert.Contains(t, sjs, `const SCENARIO_KIND = "capacity-planner-scenario";`)
	assert.NotContains(t, js, `el("scenlist").innerHTML`, "the saved names are listed as nodes, not markup")

	// Every key linkParams writes is one linkProblem admits, so an exported
	// scenario imports.
	start, end := strings.Index(js, "function linkParams()"), strings.Index(js, "// ---- The plan ----")
	require.True(t, start >= 0 && end > start, "linkParams not found")
	keys := regexp.MustCompile(`p\.set\("([A-Za-z]+)"`).FindAllStringSubmatch(js[start:end], -1)
	require.Greater(t, len(keys), 15, "found too few link keys")
	require.Contains(t, js[start:end], "p.set(a.key, k.assume[a.key])")
	astart := strings.Index(js, "const ASSUMABLE = [")
	require.GreaterOrEqual(t, astart, 0, "ASSUMABLE not found")
	assumed := regexp.MustCompile(`key: "([A-Za-z]+)"`).FindAllStringSubmatch(js[astart:astart+strings.Index(js[astart:], "];")], -1)
	require.Len(t, assumed, 4, "the assumed terms' keys")
	lstart, lend := strings.Index(sjs, "const LINK_NUMBERS"), strings.Index(sjs, "// CONTROL is")
	require.True(t, lstart >= 0 && lend > lstart, "the link's keys not found")
	admitted := sjs[lstart:lend]
	for _, k := range append(keys, assumed...) {
		assert.True(t, strings.Contains(admitted, `"`+k[1]+`"`) || strings.Contains(admitted, k[1]+": "), "an imported link may not carry %s", k[1])
	}

	// A name read back is untrusted: no line of either script that builds
	// markup puts one in raw. (The status line and the notes take names as
	// textContent, and the list builds Option nodes.)
	raw := regexp.MustCompile(`\$\{(x|sc|pin|state\.pin|same)\.(name|what|source|link|saved)`)
	markup := regexp.MustCompile("innerHTML|<[a-z/]")
	scanned := 0
	for name, text := range map[string]string{"planner.js": js, "scenario.js": sjs} {
		for i, line := range strings.Split(text, "\n") {
			if markup.MatchString(line) {
				scanned++
				assert.NotRegexp(t, raw, line, "%s line %d puts a name into markup unescaped", name, i+1)
			}
		}
	}
	require.Greater(t, scanned, 100, "found too few markup lines")

	// A pinned plan is a copy of the plan on screen; the graphs draw it dashed.
	for _, want := range []string{"plan: JSON.parse(JSON.stringify(p))", `cls: "c-pin", pin: true`, `${sr.pin ? " pin" : ""}`} {
		assert.Contains(t, js, want)
	}
	css := pageText(t, "planner.css")
	assert.Contains(t, css, "svg .line.pin { stroke-dasharray:")
}

// isFlagType reports whether e is the type whatif.Flag, as its own package or
// another names it.
func isFlagType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "Flag"
	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		return ok && pkg.Name == "whatif" && x.Sel.Name == "Flag"
	}
	return false
}

// The page names every flag a plan can carry in plain words: the engine's
// (internal/signals/whatif), the SLO analyzer's and the CLI's, whose code
// prices the API's figures. A flag added there fails here until the page says
// what it means.
func TestPageNamesEveryFlag(t *testing.T) {
	files := make([]string, 0, 64)
	for _, dir := range [][]string{{"..", "signals", "whatif"}, {"..", "engines", "analyzers", "slo"}, {"..", "..", "cmd", "capacity-planner"}} {
		got, err := filepath.Glob(filepath.Join(append(dir, "*.go")...))
		require.NoError(t, err)
		require.NotEmpty(t, got, "%v", dir)
		files = append(files, got...)
	}
	var flags []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err)
		ast.Inspect(f, func(n ast.Node) bool {
			vs, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if !isFlagType(vs.Type) {
				return true
			}
			for _, v := range vs.Values {
				if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					s, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					flags = append(flags, s)
				}
			}
			return true
		})
	}
	require.Greater(t, len(flags), 40, "found too few flags")
	js := pageText(t, "planner.js")
	for _, flag := range flags {
		assert.Contains(t, js, "\n  "+flag+": [", "FLAG_WORDS has no words for %s", flag)
	}
}

// listingResolver is a stub resolver whose listing answers sources or err.
type listingResolver struct {
	stubResolver
	sources SourcesResponse
	err     error
}

func (r *listingResolver) Sources(context.Context) (SourcesResponse, error) { return r.sources, r.err }

// A listing that fails is answered by its class, as any error is: the
// caller's is 400, a forbidden one 403, any other 500, never a listing.
func TestSourcesListingFails(t *testing.T) {
	for err, code := range map[error]int{
		errors.New("the server could not list its recordings directory"): http.StatusInternalServerError,
		InputErrorf("bad"):                    http.StatusBadRequest,
		ForbiddenError{Err: errors.New("no")}: http.StatusForbidden,
	} {
		rec := get(New(Config{Resolver: &listingResolver{err: err}}).Handler(), "/v1/sources", "")
		assert.Equal(t, code, rec.Code, "%v", err)
		assert.Contains(t, rec.Body.String(), `"error"`)
		assert.NotContains(t, rec.Body.String(), `"recordings"`)
	}
	// A listing's nil lists are written empty, and the schema version is the
	// server's.
	rec := get(New(Config{Resolver: &listingResolver{sources: SourcesResponse{SchemaVersion: 99, Prometheus: true}}}).Handler(), "/v1/sources", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"schemaVersion": 1, "prometheus": true, "namespaces": [], "recordings": []}`, rec.Body.String())
}

// A resolver that cannot list its sources answers GET /v1/sources with none,
// as empty lists rather than nulls.
func TestSourcesWithoutLister(t *testing.T) {
	rec := get(New(Config{Resolver: &stubResolver{}}).Handler(), "/v1/sources", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"schemaVersion": 1, "prometheus": false, "namespaces": [], "recordings": []}`, rec.Body.String())
}

// The page judges the fleet: the shape rows, the loads and the growth of the
// load seen are on the page, a report is posted to v1/report by a relative
// path with the body reportRequest builds from them (its cases run under node
// in hack/check-planner-scenarios.sh), a server with no report route is said
// in words, and the matrix carries a legend whose marks are words, not
// colour alone. The report's strings reach the page as text.
func TestPageJudgesTheFleet(t *testing.T) {
	html, js, scen := pageText(t, "index.html"), pageText(t, "planner.js"), pageText(t, "scenario.js")
	for _, want := range []string{`id="rshapes"`, `id="rshapeadd"`, `id="rloads"`, `id="rgrowth"`, `id="rrun"`, `id="rmatrix"`,
		`id="rstatus" class="hint" aria-live="polite"`, `id="rlegend"`, `<span class="rmark met">ok</span>`, `<span class="rmark missed">MISS</span>`,
		`<span class="rmark saturated">SAT</span>`, `<span class="rmark unknown">?</span>`} {
		assert.Contains(t, html, want)
	}
	for _, want := range []string{`api("POST", "v1/report", asked.body)`,
		`reportRequest({inputsId: state.inputs.inputsId, fleet: plan.fleet, shapes: shapeRows(),`,
		`loads: el("rloads").value, growth: el("rgrowth").value, slo: plan.slo, assume: plan.assume})`,
		`reportMissing(status, e.message)`, "This server has no fleet report (POST /v1/report)", `" EXTRAPOLATED"`} {
		assert.Contains(t, js, want)
	}
	for _, want := range []string{"function reportRequest(o)", "function reportMatrix(r)", "function reportMissing(status, error)"} {
		assert.Contains(t, scen, want)
	}
	start := strings.Index(js, "function renderReport(r)")
	require.GreaterOrEqual(t, start, 0)
	assert.NotContains(t, js[start:], "innerHTML", "the report is drawn as text nodes")
}
