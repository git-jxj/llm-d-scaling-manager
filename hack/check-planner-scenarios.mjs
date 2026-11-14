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
  cleanName, delta, fleetDelta} = ctx;
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
} catch (e) {
  failures.push("a case threw, and the cases after it did not run: " + e.message);
}

if (failures.length) {
  console.error(`check-planner-scenarios: ${failures.length} failed, ${passed} passed (${file})`);
  for (const f of failures) console.error("  FAIL " + f);
  process.exit(1);
}
if (passed < 85) {
  console.error(`check-planner-scenarios: only ${passed} cases ran; expected at least 85`);
  process.exit(1);
}
console.log(`check-planner-scenarios: ${passed} cases passed (${file})`);
