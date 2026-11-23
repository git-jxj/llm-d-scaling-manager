// Runs the capacity planner page's first screen (internal/whatifapi/page/
// scenario.js: execView, refusalView, noticeText; dayplan.js: dayHeadline)
// on the report and backtest the CLI's goldens hold, and on small inputs
// and plans built here: the executive panel answers first, each line with
// one trust badge and no planner symbol; a prefill capacity the source could
// not learn is one card with page actions, never a CLI flag; a view says how
// far to trust its figures once; and the stylesheet keeps the page from
// scrolling sideways on a phone. Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-answer.mjs [scenario.js dayplan.js backtest.js planner.css report.json backtest.json]
import fs from "node:fs";
import vm from "node:vm";

const [scen, day, bt, css, reportFile, btFile] = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.css",
  process.argv[6] || "cmd/capacity-planner/testdata/at-replicas-staircase.json.golden",
  process.argv[7] || "cmd/capacity-planner/testdata/backtest-q32-split-json.golden",
];
const ctx = vm.createContext({URLSearchParams});
for (const f of [scen, day, bt]) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const {execView, refusalView, noticeText, execReportRequest, plainVerdict, csvField, curvesCSV} = ctx;
// A const of a script is no property of its context: read it by name.
const [EXEC_BADGES, EXPERIMENTAL_NOTICE] = ["EXEC_BADGES", "EXPERIMENTAL_NOTICE"].map(n => vm.runInContext(n, ctx));
const report = JSON.parse(fs.readFileSync(reportFile, "utf8"));
const backtest = JSON.parse(fs.readFileSync(btFile, "utf8"));
const sheet = fs.readFileSync(css, "utf8");
const copy = x => JSON.parse(JSON.stringify(x));

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}

// FIRST_SCREEN_BANNED are the words the first screen never shows (the
// planner's symbols and codes): they stay in tooltips, details and drawers.
const FIRST_SCREEN_BANNED = [/\brefused\b/i, /NoPrefillCapacity/, /\bV_P\b/, /\bknee\b/i, /\bcv\b/, /--assume/, /rank-mixture/,
  /renewal-lognormal/, /operating-point cap/, /flow-control onset/, /bootstrap band/, /\bcycle \d/, /\bpriced\b/i, /UNVALIDATED/];

const inputs = {inputsId: "sha256:x", model: "m", fleet: {prefill: {variant: "p", replicas: 3, gpusPerReplica: 8}, decode: {variant: "d", replicas: 6, gpusPerReplica: 8}},
  source: {kind: "recording", name: "r"}, terms: [], observed: {}};
const k = {ttftStat: "p90", ttftTarget: 1, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: true, tpotDefault: true};
const lbDetail = "V_P 18000 tokens/s is only the rate a prefill pod was seen to compute (it never queued for capacity): no TTFT is priced from it; pass --assume-vp for a flagged answer";
const refusedPlan = {limits: {prefill: {refused: {reason: "NoPrefillCapacity", detail: lbDetail, flags: ["VPLowerBound"]}},
  decode: {value: 40}, fleet: {refused: {reason: "NoPrefillCapacity", detail: lbDetail, flags: ["VPLowerBound"]}}, chosen: {rate: 22.2}},
  // The TTFT curve, its floor over the 1 s target, and a prefill knee known
  // only as a lower bound, as the chart reads them.
  ttft: {statistic: "p90", markers: {prefillKnee: {value: 20, flags: ["VPLowerBound"]}},
    line: {target: 1, floor: {value: 4.08}, curve: [{rate: 0, figure: {value: 4.1}}, {rate: 60, figure: {value: 7}}]}},
  tpot: {statistic: "mean", curve: [{rate: 0, figure: {value: 0.012}}, {rate: 60, figure: {value: 0.02}}]}};

try {
  // The executive panel: the fleet today first, each line one badge of three.
  const v = execView({inputs, plan: refusedPlan, report, bt: backtest, k});
  const badges = new Set(Object.values(EXEC_BADGES));
  check("the panel opens on the fleet today", v.lines[0].label === "Fleet today" && v.lines[0].text.startsWith("3 prefill + 6 decode: 9 pods, 72 GPUs (8 per pod)."), v.lines[0].text);
  check("one busiest load, its statistic named, the peak small", /The busiest load \(p95 of 1-minute cycles\): [0-9.]+ r\/s\.$/.test(v.lines[0].text) && /^peak /.test(v.lines[0].small),
    v.lines[0].text + " | " + v.lines[0].small);
  const line = name => v.lines.find(l => l.label === name) || {text: ""};
  check("decode leads with what it carries", /^6 replicas carry 40 r\/s at the TPOT target, [0-9.]+x the busiest load\.$/.test(line("Decode").text), line("Decode").text);
  check("prefill says at least, never not known", line("Prefill").text.startsWith("Speed not yet observed: at least 20 r/s (a lower bound") && !/not known/i.test(line("Prefill").text), line("Prefill").text);
  check("prefill's line carries the page actions", (line("Prefill").actions || []).map(a => a.act).join() === "usevp,busier" && line("Prefill").actions[0].vp === 18000);
  check("the verdict names the role that binds first", line("Verdict").text.startsWith("Prefill binds first"), line("Verdict").text);
  check("TTFT is stated against its target and its floor", /^About [0-9.]+ s p90 at the busiest load, above your 1 s target at every rate \(floor 4\.08 s\)\. The default target may not suit these prompts: set yours under SLO\.$/.test(line("TTFT").text), line("TTFT").text);
  check("users see TTFT and TPOT at three statistics", /TTFT mean .*p90 .*p95/.test(line("Users see").text) && /TPOT mean/.test(line("Users see").text), line("Users see").text);
  check("no line heads not known or not judged", v.lines.every(l => !/^(not known|not judged)/i.test(l.text)), v.lines.map(l => l.text.slice(0, 30)).join(" | "));
  check("the fleet today is measured", v.lines[0].badge === EXEC_BADGES.measured);
  check("every line carries one badge of three", v.lines.every(l => badges.has(l.badge)), v.lines.map(l => l.badge).join(","));
  for (const label of ["Right size", "+25% / +50% traffic", "Accuracy"]) {
    check(`the panel has ${label}`, v.lines.some(l => l.label === label), v.lines.map(l => l.label).join(", "));
  }
  check("the accuracy is checked on your data", v.lines.find(l => l.label === "Accuracy").badge === EXEC_BADGES.checked);
  check("the accuracy counts held-out steps within 20%", /within 20% in \d+ of \d+ five-minute steps/.test(v.lines.find(l => l.label === "Accuracy").text),
    v.lines.find(l => l.label === "Accuracy").text);
  check("the accuracy gives the range tested", /Tested range: .* r\/s; outside that is an extrapolation\./.test(v.lines.find(l => l.label === "Accuracy").text));
  check("the trust chip quotes the check", /^Checked on your traffic: TTFT \S+ \d+\/\d+ within 20%$/.test(v.trust), v.trust);
  check("the SLO used says a default is the page's", v.slo.includes("the page's default: set yours"), v.slo);
  check("growth not sized yet says pending", /^Pending/.test(v.lines.find(l => l.label === "+25% / +50% traffic").text));
  const grown = execView({inputs, plan: refusedPlan, report, k, growth: [{x: 1.25, rate: 25, need: "6 prefill + 12 decode"}, {x: 1.5, rate: 30, need: "at least 14 decode (prefill not sized)"}]});
  check("growth wired in reads each need", grown.lines.find(l => l.label === "+25% / +50% traffic").text.startsWith("+25% (25 r/s): 6 prefill + 12 decode; +50% (30 r/s): at least 14 decode"));
  // The held-out check the server ran at load is the headline: no backtest needed.
  const atLoad = copy(inputs);
  atLoad.ttftModel = {heldOut: {statistic: "mean", within: 130, steps: 144, medianRatio: 0.94, rateFrom: 10, rateTo: 20, prefill: 3, decode: 6}};
  const held = execView({inputs: atLoad, plan: refusedPlan, report, k});
  check("the held-out check at load is the accuracy", held.lines.find(l => l.label === "Accuracy").text.startsWith("On held-out steps of your traffic: TTFT mean within 20% in 130 of 144"),
    held.lines.find(l => l.label === "Accuracy").text);
  check("and the trust chip, naming its statistic", held.trust === "Checked on your traffic: TTFT mean 130/144 within 20%", held.trust);
  const pending = execView({inputs, plan: refusedPlan, k, reportPending: true, btPending: true});
  check("a panel still asking says so", pending.lines.some(l => /^Judging/.test(l.text)) && pending.lines.some(l => /^Checking/.test(l.text)));
  // The first screen speaks plainly.
  for (const l of v.lines.concat(held.lines)) {
    const bad = FIRST_SCREEN_BANNED.find(re => re.test(l.text));
    check(`"${l.label}" uses no planner symbol`, !bad, `${bad} in ${l.text}`);
  }
  check("a decode-only verdict is no fleet recommendation", ctx.execVerdict(inputs.fleet, {verdict: "NOT JUDGED", scope: "decode only", needed: {decodeOnly: true, decode: 4}})
    === "Decode only: 4-6 decode would hold TPOT. Prefill was not sized, so this is not a fleet recommendation.");
  check("plain words replace the symbols", plainVerdict("NOT JUDGED: past the operating point of a prefill knee priced from a lower bound of V_P; --assume-vp states a V_P to price it.") ===
    "past the safe load of a prefill limit estimated from a lower bound of prefill speed per pod", plainVerdict("NOT JUDGED: past the operating point of a prefill knee priced from a lower bound of V_P; --assume-vp states a V_P to price it."));
  check("the panel's report is judged at the load seen", JSON.stringify(execReportRequest("i", {headroom: 0.85})) === JSON.stringify({inputsId: "i", loads: ["seen"], headroom: 0.85}));

  // The refusal card: plain words, the speed seen as an action, the code apart.
  const r = refusalView(refusedPlan);
  check("a prefill capacity not learned is a card", r && r.title === "Prefill capacity unknown");
  check("it says the pods never queued and the speed seen", r.text.startsWith("The prefill pods never queued, so they handle at least 18,000 tokens/s per pod"), r.text);
  check("it says what decode alone serves", r.text.endsWith("Decode alone serves up to 40 r/s."), r.text);
  check("its action is the speed seen, a page action", r.vp === 18000 && !/--assume/.test(r.text));
  check("the code stands apart", r.code === "NoPrefillCapacity" && !r.text.includes("NoPrefillCapacity"));
  const none = copy(refusedPlan);
  none.limits.prefill.refused = {reason: "NoPrefillCapacity", detail: "V_P 0 tokens/s"};
  none.limits.fleet.refused = none.limits.prefill.refused;
  check("no speed learned offers no speed to use", refusalView(none).vp === null && refusalView(none).text.startsWith("The source learned no prefill speed"));
  check("a priced prefill shows no card", refusalView({limits: {prefill: {value: 30}, decode: {value: 40}, fleet: {value: 30}}}) === null);

  // One notice a view: experimental figures, or a TTFT model not validated.
  check("experimental figures get the one notice", noticeText([{flag: "RankMixtureTTFT"}], "") === EXPERIMENTAL_NOTICE);
  check("the notice says what the dagger means", EXPERIMENTAL_NOTICE.startsWith("Experimental model (checked on your data)") && EXPERIMENTAL_NOTICE.includes("\u2020"));
  check("a model not validated gets a notice without the banner's figures", /not validated on this fleet/.test(noticeText([{flag: "UnvalidatedTTFT"}], "TTFT model UNVALIDATED ... 3.15x")) &&
    !/3\.15x|UNVALIDATED/.test(noticeText([{flag: "UnvalidatedTTFT"}], "TTFT model UNVALIDATED ... 3.15x")));
  check("no flag, no notice", noticeText([], "") === "");

  // The headline over the day plan.
  const dayPlan = JSON.parse(fs.readFileSync("cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden", "utf8"));
  check("the day plan's headline names the peak and when", /^Peak needed .* at \S+ UTC \([0-9.]+ r\/s\)/.test(ctx.dayHeadline(dayPlan)), ctx.dayHeadline(dayPlan));

  // Exports: a field a spreadsheet would run is not a formula.
  check("a CSV field is quoted where it must be", csvField("a,b") === "\"a,b\"" && csvField("say \"x\"") === "\"say \"\"x\"\"\"");
  check("a CSV field is never a formula", csvField("=1+1") === "'=1+1" && csvField("-1.5") === "-1.5");
  check("the curves export a row a rate", curvesCSV({ttft: {statistic: "p90", line: {curve: [{rate: 1, figure: {value: 0.5}}, {rate: 2, figure: {refused: {reason: "Saturated"}}}]}},
    tpot: {statistic: "mean", curve: [{rate: 1, figure: {value: 0.01}}, {rate: 2, figure: {value: 0.02}}]}}).split("\n")[2] === "2,,Saturated,0.02,");

  // No sideways scroll on a phone: what the stylesheet must hold.
  for (const [name, re] of [["a select never runs past its column", /select[^{]*\{[^}]*max-width:\s*100%/],
    ["tables scroll in their own box", /#rmatrix[^{]*\.tablewrap[^{]*\{[^}]*overflow-x:\s*auto/],
    ["the panel stacks on a phone", /@media \(max-width: 640px\)[\s\S]*\.execlines \{ grid-template-columns: 1fr;/],
    ["forms stack on a phone", /@media \(max-width: 640px\)[\s\S]*\.source form, \.reportform, \.row[^{]*\{ flex-direction: column;/]]) {
    check(name, re.test(sheet));
  }
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-answer: ${failures.length} failed, ${passed} passed`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}
if (passed < 40) {
  console.error(`FAIL check-planner-answer: only ${passed} checks ran`);
  process.exit(1);
}
console.log(`ok check-planner-answer: ${passed} checks`);
