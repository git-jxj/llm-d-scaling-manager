// Runs the capacity planner page's scenario logic
// (internal/whatifapi/page/scenario.js) on fixtures: the checks on a
// scenario read back from a file or the store, the store's edits, the names
// and the compare table's changes. Called by hack/check-planner-scenarios.sh.
//
// The page runs this file in a browser; here it runs in a node vm context
// with nothing but what the page's script uses (URLSearchParams), so a
// browser API the logic came to rely on fails here first.
//
// Usage: node hack/check-planner-scenarios.mjs [path/to/scenario.js]
import fs from "node:fs";
import vm from "node:vm";

const file = process.argv[2] || "internal/whatifapi/page/scenario.js";
const ctx = vm.createContext({URLSearchParams});
vm.runInContext(fs.readFileSync(file, "utf8"), ctx, {filename: file});
const S = name => vm.runInContext(name, ctx);
const {esc, linkProblem, checkScenario, readScenarioFile, parseStore, visibleScenarios, putScenario, dropScenario, freeName,
  cleanName, delta, fleetDelta, scenarioTitle, sourceProblem, linkAtLoadStart, linkAtLoadEnd, beginOpen, importPlan, sameCurve, figDelta,
  compareRows, parseEdges, linkedEdges, edgesBoxProblem, beyondContext, contextNote, bucketSeries, retryDelay} = ctx;
const MAX_NAME = S("MAX_NAME"), MAX_SCENARIOS = S("MAX_SCENARIOS"), MAX_LINK = S("MAX_LINK");

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}
function throws(name, fn, want) {
  try {
    fn();
    check(name, false, "did not throw");
  } catch (e) {
    check(name, !want || e.message.includes(want), e.message);
  }
}

// A case that throws where it should not ends the run as a failure, said as
// one, with the cases before it counted.
try {
// esc: the five characters HTML reads specially, and nothing else.
check("esc escapes markup", esc(`<img src=x onerror="a('b')">&`) === "&lt;img src=x onerror=&quot;a(&#39;b&#39;)&quot;&gt;&amp;", esc(`<img src=x onerror="a('b')">&`));
check("esc leaves text", esc("model-02, 2 prefill") === "model-02, 2 prefill");
check("esc of a number", esc(5) === "5");

// linkProblem: every link linkParams writes is admitted ...
const good = "recording=run.jsonl.gz&cycle=3&model=m&prefillVariant=p&decodeVariant=d&prefill=2&decode=1&rate=6.5&ttftStat=p90&ttft=0.8" +
  "&tpotStat=mean&tpot=0.05&ratio=1.3&headroom=0.85&priceAs=1k&out=1000&vp=30000&a=0.02&promptCV=1.2&routerHop=0.003&perBucket=1";
for (const link of [good, "namespace=llm&headroom=0.85", "namespace=llm&mode=size&max=40&low=1&rate=2&headroom=0.85",
  "recording=r.jsonl.gz&ttftStat=p99.9&ttft=1"]) {
  check(`admits ${link.slice(0, 50)}`, linkProblem(link) === "", linkProblem(link));
}
// ... and every other is refused, with the reason.
for (const [link, want] of [
  ["recording=r&onerror=1", "the page sets no onerror"],
  ["recording=r&constructor=1", "the page sets no constructor"],
  ["recording=r&__proto__=1", "the page sets no __proto__"],
  ["recording=r&toString=x", "the page sets no toString"],
  ["recording=r&rate=abc", "rate is not a number"],
  ["recording=r&rate=", "rate is not a number"],
  ["recording=r&rate=-1", "rate is not a number"],
  ["recording=r&rate=Infinity", "rate is not a number"],
  ["recording=r&rate=1&rate=2", "names rate twice"],
  ["recording=r&ttftStat=p90;x", "ttftStat cannot be"],
  ["recording=r&ttftStat=max", "ttftStat cannot be"],
  ["recording=r&mode=plan", "mode cannot be"],
  ["recording=r&low=true", "low cannot be"],
  ["recording=r&model=", "model is not a name"],
  ["recording=r&model=a%0Db", "model is not a name"],
  ["recording=r&model=" + "m".repeat(254), "model is not a name"],
  ["rate=1", "names no source"],
  ["", "names no source"],
  ["recording=r&model=" + "m".repeat(MAX_LINK), "longer than"],
]) {
  const got = linkProblem(link);
  check(`refuses ${link.slice(0, 40)}`, got !== "" && got.includes(want), JSON.stringify(got));
}
check("refuses a link that is not text", linkProblem(42) !== "" && linkProblem(null) !== "");

// checkScenario and nameProblem: the name.
const sc = checkScenario({name: "  two prefill ", link: good, saved: "2026-11-14 21:08"});
check("checkScenario trims the name", sc.name === "two prefill", sc.name);
check("checkScenario keeps the link", sc.link === good);
check("checkScenario keeps the time", sc.saved === "2026-11-14 21:08");
check("checkScenario drops a time with a control character", checkScenario({name: "n", link: good, saved: "a\nb"}).saved === "");
check("a name of MAX_NAME characters", checkScenario({name: "n".repeat(MAX_NAME), link: good}).name.length === MAX_NAME);
check("a hostile name is kept as text", checkScenario({name: "<img src=x onerror=alert(1)>", link: good}).name === "<img src=x onerror=alert(1)>");
for (const [what, x, want] of [
  ["no object", null, "not a scenario"],
  ["an array", [], "not a scenario"],
  ["a string", "x", "not a scenario"],
  ["no name", {link: good}, "name is text"],
  ["an empty name", {name: "   ", link: good}, "name is text"],
  ["a long name", {name: "n".repeat(MAX_NAME + 1), link: good}, "name is text"],
  ["a number name", {name: 5, link: good}, "name is text"],
  ["a CR in the name", {name: "a\rb", link: good}, "control character"],
  ["a NUL in the name", {name: "a\u0000b", link: good}, "control character"],
  ["a tab in the name", {name: "a\tb", link: good}, "control character"],
  ["a DEL in the name", {name: "a\u007fb", link: good}, "control character"],
  ["a C1 control in the name", {name: "a\u0085b", link: good}, "control character"],
  ["a bad link", {name: "n", link: "recording=r&x=1"}, "the page sets no x"],
  ["no link", {name: "n"}, "not text"],
]) {
  throws(`checkScenario refuses ${what}`, () => checkScenario(x), want);
}

// readScenarioFile: the exported file.
const file1 = JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "a", link: good});
check("reads an exported file", readScenarioFile(file1).link === good);
throws("refuses a file that is not JSON", () => readScenarioFile("not json"), "not JSON");
throws("refuses another kind", () => readScenarioFile(JSON.stringify({kind: "other", version: 1, name: "a", link: good})), "not a capacity-planner-scenario");
throws("refuses another version", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 2, name: "a", link: good})), "version 2");
throws("refuses JSON null", () => readScenarioFile("null"), "not a capacity-planner-scenario");
throws("refuses a file with a bad link", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "a", link: "recording=r&rate=x"})), "rate");
throws("refuses a file too large", () => readScenarioFile(" ".repeat(70000)), "larger than");

// The store: entries that do not check are kept, and hidden.
check("parseStore of nothing", parseStore(null).length === 0);
check("parseStore of junk", parseStore("{").length === 0 && parseStore('{"a":1}').length === 0);
const foreign = {name: "from another version", link: "recording=r&future=1"};
const hasForeign = r => r.some(x => x && x.name === foreign.name && x.link === foreign.link);
let raw = parseStore(JSON.stringify([{name: "a", link: good}, foreign, 42, {name: "a\rb", link: good}]));
check("visibleScenarios hides what does not check", visibleScenarios(raw).map(x => x.name).join("|") === "a", visibleScenarios(raw).map(x => x.name).join("|"));
let put = putScenario(raw, {name: "b", link: good});
check("putScenario adds", !put.replaced && visibleScenarios(put.raw).map(x => x.name).join("|") === "a|b");
check("putScenario keeps entries that do not check", hasForeign(put.raw) && put.raw.includes(42) && put.raw.length === 5, put.raw.length);
put = putScenario(put.raw, {name: "a", link: "namespace=llm"});
check("putScenario replaces the same name", put.replaced && visibleScenarios(put.raw).filter(x => x.name === "a").length === 1 &&
  visibleScenarios(put.raw).find(x => x.name === "a").link === "namespace=llm");
check("putScenario replacing keeps the rest", hasForeign(put.raw) && put.raw.length === 5, put.raw.length);
throws("putScenario refuses a bad entry", () => putScenario([], {name: "a\u0000", link: good}), "control character");
raw = dropScenario(put.raw, "a");
check("dropScenario drops the one named", visibleScenarios(raw).map(x => x.name).join("|") === "b");
check("dropScenario keeps entries that do not check", hasForeign(raw) && raw.includes(42) && raw.length === 4, raw.length);
check("dropScenario of a name not saved", dropScenario(raw, "zzz").length === raw.length);
// A foreign entry with a visible one's name is not dropped with it.
const twin = {name: "b", link: "recording=r&future=1"};
check("dropScenario keeps a foreign twin", dropScenario(raw.concat([twin]), "b").includes(twin));

// The cap: MAX_SCENARIOS that check; a new name past it is refused, a
// replacement is not, and entries that do not check do not count.
let full = [foreign];
for (let i = 0; i < MAX_SCENARIOS; i++) full = putScenario(full, {name: "s" + i, link: good}).raw;
check("the store holds MAX_SCENARIOS", visibleScenarios(full).length === MAX_SCENARIOS);
throws("a new name past the cap is refused", () => putScenario(full, {name: "one more", link: good}), "at most");
check("a replacement at the cap is not", putScenario(full, {name: "s0", link: "namespace=llm"}).replaced);
check("visibleScenarios shows at most MAX_SCENARIOS", visibleScenarios(full.concat([{name: "extra", link: good}])).length === MAX_SCENARIOS);

// freeName: three imports of one 80-character name are three names.
const long = "n".repeat(MAX_NAME);
const taken = [long];
for (let i = 0; i < 3; i++) taken.push(freeName(long, taken));
check("freeName gives distinct names", new Set(taken).size === 4, taken.map(n => n.slice(-6)).join(","));
check("freeName stays within MAX_NAME", taken.every(n => n.length <= MAX_NAME), taken.map(n => n.length).join(","));
check("freeName suffixes", taken[1].endsWith(" (2)") && taken[2].endsWith(" (3)") && taken[3].endsWith(" (4)"), taken.slice(1).map(n => n.slice(-4)).join(","));
check("freeName of a free name", freeName("x", ["y"]) === "x");
check("freeName of a short name", freeName("x", ["x", "x (2)"]) === "x (3)", freeName("x", ["x", "x (2)"]));
// Saved under the names freeName gives, three imports are three entries,
// and deleting one leaves the others.
let imports = [{name: long, link: good}];
for (let i = 0; i < 3; i++) {
  const n = freeName(long, visibleScenarios(imports).map(x => x.name));
  imports = putScenario(imports, {name: n, link: "namespace=llm&rate=" + i}).raw;
}
check("three imports are four entries", visibleScenarios(imports).length === 4, visibleScenarios(imports).length);
check("deleting one leaves three", visibleScenarios(dropScenario(imports, visibleScenarios(imports)[1].name)).length === 3);

// cleanName: control characters out, trimmed, at most MAX_NAME.
check("cleanName drops control characters", cleanName(" a\r\nb\u0000 ") === "a  b", JSON.stringify(cleanName(" a\r\nb\u0000 ")));
check("cleanName cuts", cleanName("x".repeat(200)).length === MAX_NAME);
check("cleanName gives a name that checks", checkScenario({name: cleanName("m\t" + "y".repeat(100)), link: good}).name.length <= MAX_NAME);

// delta and fleetDelta: the compare table's change column.
const f2 = v => v.toFixed(2);
for (const [a, b, text, cls] of [[1, 2, "+1.00", "up"], [2, 1, "-1.00", "down"], [1, 1.001, "same", ""], [null, 1, "", ""], [1, null, "", ""], [0, 0.5, "+0.50", "up"]]) {
  const d = delta(a, b, f2);
  check(`delta(${a}, ${b})`, d.text === text && d.cls === cls, JSON.stringify(d));
}
check("fleetDelta of one role", fleetDelta({prefill: 2, decode: 1}, {prefill: 4, decode: 1}) === "+2 prefill");
check("fleetDelta of both", fleetDelta({prefill: 2, decode: 3}, {prefill: 1, decode: 4}) === "-1 prefill, +1 decode", fleetDelta({prefill: 2, decode: 3}, {prefill: 1, decode: 4}));
check("fleetDelta of none", fleetDelta({prefill: 1, decode: 1}, {prefill: 1, decode: 1}) === "same");
// A decode-only fleet has no prefill count: it counts as 0.
check("fleetDelta adding prefill to a decode-only fleet", fleetDelta({decode: 2}, {prefill: 1, decode: 2}) === "+1 prefill");
check("fleetDelta of a decode-only change", fleetDelta({decode: 2}, {decode: 3}) === "+1 decode", fleetDelta({decode: 2}, {decode: 3}));

// delta reads the figures as printed: 10.06 and 10.22 print 10.1 and 10.2,
// and the change is +0.10 r/s, not the +0.16 between the raw values.
const fr = v => v.toFixed(Math.abs(v) >= 10 ? 1 : 2) + " r/s";
check("delta of the printed figures", delta(10.06, 10.22, fr).text === "+0.10 r/s", delta(10.06, 10.22, fr).text);
check("delta of the printed figures, down", delta(10.22, 10.06, fr).text === "-0.10 r/s" && delta(10.22, 10.06, fr).cls === "down");
check("delta across units falls back to the values", delta(0.9, 1.2, v => v < 1 ? (v * 1000).toFixed(0) + " ms" : v.toFixed(2) + " s").text === "+300 ms");

// figDelta says where a side is refused.
const fig = v => ({value: v}), refused = {refused: {reason: "Saturated"}};
check("figDelta both refused", figDelta(refused, refused, fr).text === "both refused");
check("figDelta now refused", figDelta(fig(2), refused, fr).text === "now refused");
check("figDelta now priced", figDelta(refused, fig(2), fr).text === "now priced");
check("figDelta of two values", figDelta(fig(2), fig(3), fr).text === "+1.00 r/s");
check("figDelta with a side missing", figDelta(undefined, fig(3), fr).text === "");

// compareRows: every text escaped, the refusals said, headroom coloured only
// at the same rate, the header and row names.
const hostile = '<img src=x onerror="alert(1)">';
const plan = (o = {}) => ({
  scenario: {fleet: {prefill: 2, decode: 1}},
  limits: {fleet: fig(o.fleet ?? 11.2), prefill: fig(11.6), decode: o.decode === null ? undefined : fig(o.decode ?? 11.2),
    chosen: {rate: o.rate ?? 10, headroom: o.headroom ?? fig(1.2)}},
  ttft: {statistic: o.stat ?? "p90", line: {target: 0.5}}, tpot: {statistic: "mean", target: 0.004},
});
const fmts = {rate: fr, fleet: n => `${n.prefill} prefill ${hostile}`, fig: f => figValue(f) != null ? fr(figValue(f)) : "refused " + hostile,
  bind: () => "decode binds " + hostile, slo: (s, t) => `${s} ${t}`, shape: () => "a mix " + hostile};
const figValue = f => f && !f.refused && typeof f.value === "number" ? f.value : null;
const html = compareRows(plan({stat: hostile}), plan({fleet: 12.3, headroom: refused}), fmts);
const tagless = html.replace(/<\/?(table|tr|th|td)( scope="row"| class="[a-z-]*")?>/g, "");
check("compareRows lets no markup through", !tagless.includes("<") && !tagless.includes(">"), tagless.slice(0, 200));
check("compareRows escapes the texts", html.includes("&lt;img src=x onerror=&quot;alert(1)&quot;&gt;"));
// The change cell: a rate formatter whose text carries markup.
const hostileRate = v => fr(v) + " <b>";
const changes = compareRows(plan(), plan({fleet: 12.3}), {...fmts, rate: hostileRate})
  .replace(/<\/?(table|tr|th|td)( scope="row"| class="[a-z-]*")?>/g, "");
check("compareRows escapes the change cell", !changes.includes("<b>") && changes.includes("+1.10 r/s &lt;b&gt;"), changes.slice(0, 160));
// The row names are the page's own words; the row template escapes them
// anyway, and every other text it puts in a cell.
const rowTemplate = (compareRows.toString().match(/const row = [\s\S]*?\);\r?\n/) || [""])[0];
const holes = rowTemplate.match(/\$\{[^}]*\}/g) || [];
check("the row template has its holes", holes.length === 5, holes.join(" "));
check("every hole in the row template is escaped", holes.every(h => h.startsWith("${esc(") || h === '${d.cls ? "d-" + d.cls : ""}'), holes.join(" "));
check("compareRows headers", html.includes("<th>A (dashed)</th><th>Current</th><th>Change</th>"));
check("compareRows names the rate row", html.includes(">Rate you set<") && !html.includes(">Rate set<"));
check("compareRows says now refused", html.includes(">now refused<"));
check("compareRows colours a limit that rose", /Fleet limit<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="d-up">\+1.10 r\/s</.test(html), html.match(/Fleet limit.*?<\/tr>/)?.[0]);
const sameRate = compareRows(plan(), plan({headroom: fig(2.2)}), fmts);
const otherRate = compareRows(plan(), plan({headroom: fig(2.2), rate: 9}), fmts);
const headroomCell = h => (h.match(/Headroom at the rate<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="([^"]*)">/) || [])[1];
check("headroom coloured at the same rate", headroomCell(sameRate) === "d-up", headroomCell(sameRate));
check("headroom not coloured at another rate", headroomCell(otherRate) === "", headroomCell(otherRate));
check("rate change is never coloured", /Rate you set<\/th><td>[^<]*<\/td><td>[^<]*<\/td><td class="">-1.00 r\/s</.test(otherRate), otherRate.match(/Rate you set.*?<\/tr>/)?.[0]);
check("compareRows drops a role neither side has", !compareRows(plan({decode: null}), plan({decode: null}), fmts).includes("Decode limit"));

// scenarioTitle: whole clauses dropped from the end, headroom first.
const short = scenarioTitle(["m2", "1 decode", "2.00 r/s", "TTFT p90 1 s", "TPOT mean 50 ms", "headroom 0.85"]);
check("scenarioTitle keeps every clause that fits", short === "m2, 1 decode, 2.00 r/s, TTFT p90 1 s, TPOT mean 50 ms, headroom 0.85", short);
// 87 characters in all: the headroom goes, whole.
const title = scenarioTitle(["model-02", "2 prefill, 1 decode", "11.2 r/s", "TTFT p90 500 ms", "TPOT mean 4.0 ms", "headroom 0.85"]);
check("scenarioTitle drops the headroom first", title === "model-02, 2 prefill, 1 decode, 11.2 r/s, TTFT p90 500 ms, TPOT mean 4.0 ms", title);
const t2 = scenarioTitle(["m".repeat(40), "2 prefill, 1 decode", "11.2 r/s", "TTFT p90 500 ms", "TPOT mean 4.0 ms", "headroom 0.85"]);
check("scenarioTitle drops whole clauses until it fits", t2 === "m".repeat(40) + ", 2 prefill, 1 decode, 11.2 r/s", t2);
check("scenarioTitle cuts only a first clause too long", scenarioTitle(["x".repeat(100), "y"]).length === MAX_NAME);

// sourceProblem: what a server that lists sources can open.
const recs = {prometheus: false, namespaces: [], recordings: ["a.jsonl.gz"]};
const prom = {prometheus: true, namespaces: ["llm"], recordings: []};
check("sourceProblem: a listed recording", sourceProblem("recording=a.jsonl.gz", recs) === "");
check("sourceProblem: an unlisted recording", sourceProblem("recording=b.jsonl.gz", recs).includes("does not list"));
check("sourceProblem: a namespace on a recordings-only server", sourceProblem("namespace=llm&rate=1", recs).includes("does not read"));
check("sourceProblem: an allowed namespace", sourceProblem("namespace=llm", prom) === "");
check("sourceProblem: a namespace not allowed", sourceProblem("namespace=other", prom).includes("may not read"));
check("sourceProblem: sources not listed yet", sourceProblem("namespace=llm", null).includes("not listed"));
check("sourceProblem: no source", sourceProblem("rate=1", prom) === "it names no source");

// The link handshake: the page's link, a scenario opened, loads overtaken.
let link = {seq: null, restore: null};
link = linkAtLoadStart(link, 1);
let end = linkAtLoadEnd(link, 1, 1, true);
check("the first load applies the page's link", end.apply && end.link === null);
end = linkAtLoadEnd(linkAtLoadStart({seq: null, restore: null}, 1), 1, 1, false);
check("a failed first load keeps the link for the next", !end.apply && end.link && end.link.seq === null && !end.restore);
check("the next load takes it", linkAtLoadStart(end.link, 2).seq === 2);
// Load pressed before the first load returns: the newer load applies it.
link = linkAtLoadStart(linkAtLoadStart({seq: null, restore: null}, 1), 2);
check("an overtaken load does nothing", !linkAtLoadEnd(link, 1, 2, true).apply);
check("the overtaking load applies the link", linkAtLoadEnd(link, 2, 2, true).apply);
// A scenario opened, whose load fails: put back, and no later load applies it.
const before = {linked: "recording=a.jsonl.gz&prefill=2", hash: "#recording=a.jsonl.gz&prefill=2", form: {rec: "a.jsonl.gz", prom: false}, name: "mine"};
const open = beginOpen(before, {name: "dead", link: "recording=a.jsonl.gz&model=none&prefill=5"});
check("beginOpen puts the scenario on the page", open.next.linked === "recording=a.jsonl.gz&model=none&prefill=5" &&
  open.next.hash === "#recording=a.jsonl.gz&model=none&prefill=5" && open.next.name === "dead");
link = linkAtLoadStart(open.link, 7);
end = linkAtLoadEnd(link, 7, 7, false);
check("a failed scenario load restores what was before", end.restore === before && !end.apply && end.link === null);
check("what is restored is the page before", end.restore.linked === before.linked && end.restore.hash === before.hash &&
  end.restore.form.rec === "a.jsonl.gz" && end.restore.name === "mine");
check("no later load applies a failed scenario", linkAtLoadStart(end.link, 8) === null);
check("a scenario load that succeeds applies it", linkAtLoadEnd(linkAtLoadStart(beginOpen(before, {name: "x", link: "namespace=llm"}).link, 9), 9, 9, true).apply);
// A Load pressed while the scenario loads takes it over, rollback included.
link = linkAtLoadStart(linkAtLoadStart(open.link, 3), 4);
check("an overtaking load carries the rollback", linkAtLoadEnd(link, 4, 4, false).restore === before);

// importPlan: the import branches; raw is never changed.
const store0 = [{name: "a", link: good, saved: "t0"}, foreign];
const snapshot = JSON.stringify(store0);
let imp = importPlan(store0, {name: "a", link: good, saved: "t1"});
check("same name, same link: opened, not saved", !imp.save && imp.name === "a" && imp.raw === store0);
imp = importPlan(store0, {name: "a", link: "namespace=llm", saved: "t1"});
check("same name, another link: suffixed and saved", imp.save && imp.name === "a (2)" &&
  visibleScenarios(imp.raw).map(x => x.name).join("|") === "a|a (2)", imp.name);
imp = importPlan(store0, {name: "new", link: good, saved: "t1"});
check("a new name: saved as it is", imp.save && imp.name === "new" && hasForeign(imp.raw));
check("importPlan never changes the store it read", JSON.stringify(store0) === snapshot);
throws("importPlan at the cap refuses, saving nothing", () => importPlan(full, {name: "one more", link: good}), "at most");

// The saved time is kept, and cut at 40 characters.
check("putScenario keeps .saved", visibleScenarios(putScenario([], {name: "t", link: good, saved: "2026-11-14 21:08"}).raw)[0].saved === "2026-11-14 21:08");
check("putScenario cuts .saved at 40", visibleScenarios(putScenario([], {name: "t", link: good, saved: "s".repeat(60)}).raw)[0].saved.length === 40);

// sameCurve: A drawn under the current curve.
const c1 = [{x: 1, y: 0.1}, {x: 2, y: null}, {x: 3, y: 0.3}];
check("sameCurve of one curve", sameCurve(c1, c1.map(q => ({...q}))));
check("sameCurve within rounding", sameCurve(c1, [{x: 1, y: 0.1 + 1e-13}, {x: 2, y: null}, {x: 3, y: 0.3}]));
check("sameCurve of another curve", !sameCurve(c1, [{x: 1, y: 0.1}, {x: 2, y: 0.2}, {x: 3, y: 0.3}]));
check("sameCurve of other lengths", !sameCurve(c1, c1.slice(1)) && !sameCurve([], []));

// parseEdges: the bucket edges box, by the server's rules
// (internal/whatifapi/scenario.go): token counts or Nk, at most 8,
// ascending, each from 1 to 1024k. The text is the link's canonical form:
// each edge by its bucket's name, as the server names the line.
const edgesOf = s => { const r = parseEdges(s); return JSON.stringify(r.edges) + " " + r.text; };
for (const [s, want] of [
  ["", "[] "],
  ["2048", "[2048] 2k"],
  ["2000", "[2000] 2000"],
  ["2k, 16K,64k", "[2048,16384,65536] 2k,16k,64k"],
  ["1000 2048\t8k", "[1000,2048,8192] 1000,2k,8k"],
  ["1,2,3,4,5,6,7,8", "[1,2,3,4,5,6,7,8] 1,2,3,4,5,6,7,8"],
  ["1048576", "[1048576] 1024k"],
]) {
  let got;
  try { got = edgesOf(s); } catch (e) { got = "threw " + e.message; }
  check(`parseEdges reads ${JSON.stringify(s)}`, got === want, got);
}
for (const [s, want] of [
  ["1,2,3,4,5,6,7,8,9", "9 edges; give at most 8"],
  ["0", "outside 1 to 1024k"],
  ["1025k", "outside 1 to 1024k"],
  ["1048577", "outside 1 to 1024k"],
  ["4k,1k", "1k is not above the edge before it"],
  ["2k,2048", "2048 is not above the edge before it"],
  ["1.5k", "not a token count"],
  ["-5", "not a token count"],
  ["16m", "not a token count"],
  ["2k;16k", "not a token count"],
  ["1e3", "not a token count"],
]) {
  throws(`parseEdges refuses ${JSON.stringify(s)}`, () => parseEdges(s), want);
}

// A link may carry edges in any form parseEdges reads, so a hand-edited
// edges=2048 or 2K,16k is used; edges that do not parse are refused.
for (const link of ["recording=r&perBucket=1&edges=2k,16k,64k", "recording=r&edges=1000", "namespace=llm&perBucket=1&edges=1,2,3,4,5,6,7,8",
  "recording=r&edges=2048", "recording=r&edges=2K,16k", "recording=r&edges=2k,%2016k"]) {
  check(`admits ${link}`, linkProblem(link) === "", linkProblem(link));
}
for (const link of ["recording=r&edges=", "recording=r&edges=4k,1k", "recording=r&edges=0",
  "recording=r&edges=1,2,3,4,5,6,7,8,9", "recording=r&edges=2k%3Cb%3E", "recording=r&edges=2k&edges=4k", "recording=r&edges=%20"]) {
  const got = linkProblem(link);
  check(`refuses ${link}`, got !== "", JSON.stringify(got));
}

// linkedEdges: a link's edges are used in canonical form, so the page
// rewrites the link; edges that do not parse are not used, and said.
for (const [v, edges, note] of [
  [null, "", null],
  ["", "", null],
  ["2048", "2k", null],
  ["2K,16k", "2k,16k", null],
  ["2000 16384", "2000,16k", null],
  ["4k,1k", "", `The link's bucket edges "4k,1k" were not used (1k is not above the edge before it: give the edges smallest first, each once): the default buckets are priced.`],
  ["1,2,3,4,5,6,7,8,9", "", `The link's bucket edges "1,2,3,4,5,6,7,8,9" were not used (9 edges; give at most 8): the default buckets are priced.`],
  ["lots", "", `The link's bucket edges "lots" were not used (lots is not a token count such as 2048 or 16k): the default buckets are priced.`],
]) {
  const got = linkedEdges(v);
  check(`linkedEdges of ${JSON.stringify(v)}`, got.edges === edges && got.note === note, JSON.stringify(got));
}

// edgesBoxProblem: the box is marked while its text is not the edges in use,
// and clear once it is.
for (const [raw, inUse, want] of [
  ["2k, 16k", "2k,16k", ""],
  ["", "", ""],
  ["2048,16384", "2k,16k", ""],
  ["4k,1k", "2k,16k", "Not used: 1k is not above the edge before it: give the edges smallest first, each once; the lines show 2k, 16k."],
  ["lots", "", "Not used: lots is not a token count such as 2048 or 16k; the lines show the default buckets."],
  ["2k,64k", "2k,16k", "Not used yet; the lines show 2k, 16k."],
  ["", "2k,16k", "Not used yet; the lines show 2k, 16k."],
]) {
  const got = edgesBoxProblem(raw, inUse);
  check(`edgesBoxProblem of ${JSON.stringify(raw)} with ${JSON.stringify(inUse)}`, got === want, JSON.stringify(got));
}

// beyondContext and contextNote: a line flagged on either point, answered
// or refused, is past the window, and says which.
const lineWith = (rateFlags, targetRefusedFlags) => ({atRate: {figure: {value: 1, flags: rateFlags}},
  atTarget: {rate: targetRefusedFlags ? {refused: {reason: "TargetBelowFloor", flags: targetRefusedFlags}} : {value: 2}}});
check("beyondContext on point A", beyondContext(lineWith(["EdgeBeyondContext"], null)));
check("beyondContext on a refused point B", beyondContext(lineWith([], ["EdgeBeyondContext"])));
check("beyondContext of a line within the window", !beyondContext(lineWith(["AssumedVP"], ["AssumedVP"])));
check("beyondContext of a bare line", !beyondContext({}));
check("contextNote with the window", contextNote(16384) === "past the 16k-token context window", contextNote(16384));
check("contextNote without it", contextNote(null) === "past the context window");

// bucketSeries: five colours, dashed from the sixth line on, dimmed past the
// window.
check("bucketSeries of the first", JSON.stringify(bucketSeries(0, false)) === '{"cls":"c-0","extra":""}');
check("bucketSeries of the sixth", JSON.stringify(bucketSeries(5, false)) === '{"cls":"c-0","extra":" rep"}', JSON.stringify(bucketSeries(5, false)));
check("bucketSeries of the eighth past the window", JSON.stringify(bucketSeries(7, true)) === '{"cls":"c-2","extra":" rep beyond"}');
check("bucketSeries of the fifth past the window", JSON.stringify(bucketSeries(4, true)) === '{"cls":"c-4","extra":" beyond"}');

// An imported scenario: edges in the form the page writes are read back; any
// other form refuses the file, naming the edges.
const withEdges = good + "&edges=2k,16k,64k";
check("reads an exported file with edges", readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e", link: withEdges})).link === withEdges);
throws("refuses a file with edges out of order", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e", link: good + "&edges=64k,2k"})), "edges cannot be");
throws("refuses a file with too many edges", () => readScenarioFile(JSON.stringify({kind: "capacity-planner-scenario", version: 1, name: "e",
  link: good + "&edges=" + Array.from({length: 9}, (_, i) => i + 1).join(",")})), "edges cannot be");
const impEdges = importPlan([], {name: "e", link: withEdges, saved: "t"});
check("importPlan saves a scenario with edges", impEdges.save && visibleScenarios(impEdges.raw)[0].link === withEdges);

// Compare: A pinned with other edges than the current plan. The limits do
// not move; a Buckets row says the edges changed, and is absent where they
// are the same.
const withLines = (p, edges) => Object.assign(p, {scenario: Object.assign({}, p.scenario, edges ? {bucketEdges: edges} : {}),
  ttft: Object.assign({}, p.ttft, {perBucket: (edges || [1024, 4096]).map(e => ({bucket: String(e), curve: []}))})});
const aEdges = withLines(plan(), [2048, 16384]), curEdges = withLines(plan(), null);
const edgeTable = compareRows(aEdges, curEdges, fmts);
check("compareRows with other edges says the buckets changed",
  edgeTable.includes(`<th scope="row">Buckets</th><td>2k, 16k</td><td>1k, 4k, 8k, 32k, 128k (default)</td><td class="">changed</td>`), edgeTable);
check("compareRows with other edges: the limits read the same", (edgeTable.match(/>same</g) || []).length >= 4, edgeTable);
const sameEdges = compareRows(withLines(plan(), [2048]), withLines(plan(), [2048]), fmts);
check("compareRows with the same edges has no Buckets row", !sameEdges.includes(">Buckets<"), sameEdges);

// retryDelay: only a busy server's 503, which sets Retry-After, is asked
// again; a plan stopped at its budget (503, no Retry-After) never is, or the
// page would price it from the start up to five more times.
check("retryDelay waits Retry-After on a busy 503", retryDelay(503, "1", 0) === 1000, retryDelay(503, "1", 0));
check("retryDelay waits 2 s on Retry-After 2", retryDelay(503, "2", 4) === 2000, retryDelay(503, "2", 4));
check("retryDelay never retries a 503 without Retry-After (PlanBudgetExceeded)", retryDelay(503, null, 0) === -1, retryDelay(503, null, 0));
check("retryDelay never retries a 503 with an empty Retry-After", retryDelay(503, " ", 0) === -1, retryDelay(503, " ", 0));
check("retryDelay stops after five retries", retryDelay(503, "1", 5) === -1, retryDelay(503, "1", 5));
check("retryDelay reads an unreadable Retry-After as 1 s", retryDelay(503, "soon", 0) === 1000, retryDelay(503, "soon", 0));
check("retryDelay waits at most 30 s", retryDelay(503, "3600", 0) === 30000, retryDelay(503, "3600", 0));
check("retryDelay never retries a 200", retryDelay(200, "1", 0) === -1, retryDelay(200, "1", 0));
check("retryDelay never retries a 500", retryDelay(500, "1", 0) === -1, retryDelay(500, "1", 0));

// The fleet report: the body the page posts to v1/report from its controls,
// the shapes and loads it refuses before asking, the summary it draws, and a
// server with no report route told apart from a dropped inputsId.
{
  const {reportShapeProblem, shapeText, parseReportLoads, reportRequest, reportMatrix, reportMissing} = ctx;
  check("measured is a shape", reportShapeProblem("measured") === "");
  check("IL/OL is a shape", reportShapeProblem("8k/200") === "");
  check("IL/OL/hit is a shape", reportShapeProblem("1k/4k/0.3") === "");
  check("one length is not a shape", reportShapeProblem("8k") !== "");
  check("a hit of 1 is not a shape", reportShapeProblem("1k/1k/1") !== "");
  check("a length past 1024k is refused", reportShapeProblem("2000k/200") !== "");
  check("blank boxes are the measured shape", shapeText({il: "", ol: " ", hit: ""}) === "measured");
  check("a row without a hit", shapeText({il: "8k", ol: "200"}) === "8k/200");
  check("a row with a hit", shapeText({il: "1k", ol: "4k", hit: "0.3"}) === "1k/4k/0.3");
  check("empty loads are the server's default", parseReportLoads("").loads.length === 0 && parseReportLoads("").problem === "");
  check("loads are read in order", JSON.stringify(parseReportLoads("50%, 5r/s ,seen").loads) === JSON.stringify(["50%", "5r/s", "seen"]));
  check("a share past 100% is refused", parseReportLoads("150%").problem !== "");
  check("a load with no unit is refused", parseReportLoads("5").problem !== "");

  const asked = reportRequest({inputsId: "sha256:x", fleet: {decode: 3}, shapes: [{}, {il: "1k", ol: "1k"}, {il: "16k", ol: "200", hit: "0.3"}],
    loads: "50%, 80%, 5r/s, seen", growth: "1.2", slo: {ttft: {statistic: "p90", target: 2}}, assume: {vp: 200000}});
  const want = {inputsId: "sha256:x", shapes: ["measured", "1k/1k", "16k/200/0.3"], fleet: {decode: 3}, loads: ["50%", "80%", "5r/s", "seen"],
    loadSeenX: 1.2, slo: {ttft: {statistic: "p90", target: 2}}, assume: {vp: 200000}};
  check("the body carries every control", asked.problem === "" && JSON.stringify(asked.body) === JSON.stringify(want), JSON.stringify(asked));
  check("no rows asks for the measured shape", JSON.stringify(reportRequest({inputsId: "i"}).body) === JSON.stringify({inputsId: "i", shapes: ["measured"]}));
  check("a growth of 1 is left out", !("loadSeenX" in reportRequest({inputsId: "i", growth: "1"}).body));
  check("a bad growth is refused", reportRequest({inputsId: "i", growth: "-2"}).body === null);
  check("a bad shape is refused before asking", reportRequest({inputsId: "i", shapes: [{il: "8k"}]}).body === null);
  check("seven shapes are refused", reportRequest({inputsId: "i", shapes: Array(7).fill({il: "1k", ol: "1k"})}).body === null);
  check("a bad load is refused before asking", reportRequest({inputsId: "i", loads: "fast"}).body === null);

  const m = reportMatrix({loads: ["50%", "seen x1.2"], shapes: [
    {label: "measured", verdict: {verdict: "RIGHT-SIZED"}, table: [
      {load: "50%", rate: 19.07, cell: {ttft: {value: 0.6367}, tpot: {value: 0.01641}, mark: "met"}},
      {load: "seen x1.2", rate: 37.91, cell: {ttft: {refused: {reason: "AboveDecodeOnset"}}, tpot: {value: 0.0636}, mark: "saturated"}}]},
    {label: "1k/1k", extrapolated: true, extrapolatedWhy: "IL 1024 outside", verdict: {verdict: "UNDER-PROVISIONED"}, table: [
      {load: "50%", rate: 18.27, cell: {ttft: {value: 0.134}, tpot: {value: 0.0164}, mark: "missed"}}]}]});
  check("the matrix has a column per load", m.loads.length === 2 && m.rows[0].cells.length === 2);
  check("a met cell reads ok", m.rows[0].cells[0].cls === "met" && m.rows[0].cells[0].word === "ok" && m.rows[0].cells[0].text === "636.7 / 16.4",
    JSON.stringify(m.rows[0].cells[0]));
  check("a refused figure is -", m.rows[0].cells[1].text === "- / 63.6" && m.rows[0].cells[1].word === "SAT");
  check("a missed cell says MISS", m.rows[1].cells[0].cls === "missed" && m.rows[1].cells[0].word === "MISS");
  check("a load with no row is -", m.rows[1].cells[1].text === "-" && m.rows[1].cells[1].cls === "none");
  check("an extrapolated shape is marked", m.rows[1].extrapolated && !m.rows[0].extrapolated && m.rows[1].why === "IL 1024 outside");
  check("each row carries its verdict", m.rows[1].verdict === "UNDER-PROVISIONED");
  check("a server with no report route", reportMissing(404, "404: POST /v1/report: no such endpoint; the server serves ..."));
  check("a dropped inputsId is not a missing route", !reportMissing(404, "404: unknown inputsId: resolve the source"));
  check("a 400 is not a missing route", !reportMissing(400, "no such endpoint"));
}
} catch (e) {
  failures.push("a case threw, and the cases after it did not run: " + e.message);
}

if (failures.length) {
  console.error(`check-planner-scenarios: ${failures.length} failed, ${passed} passed (${file})`);
  for (const f of failures) console.error("  FAIL " + f);
  process.exit(1);
}
// EXPECTED_CASES is how many cases this file runs: a case that silently
// stops running (an early return, a renamed function) fails the check.
const EXPECTED_CASES = 247;
if (passed !== EXPECTED_CASES) {
  console.error(`check-planner-scenarios: ${passed} cases ran; expected ${EXPECTED_CASES}`);
  process.exit(1);
}
console.log(`check-planner-scenarios: ${passed} cases passed (${file})`);
