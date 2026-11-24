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
  // The executive panel: the fleet today first; a badge only where a row is
  // measured or checked, the rest estimates, as one note says.
  const measuredHour = {from: "2026-11-17T13:00:00Z", to: "2026-11-17T14:00:00Z", rate: 24, requests: 80000,
    ttft: [{statistic: "mean", value: 3.6}, {statistic: "p90", bucket: {lo: 3, hi: 5}}, {statistic: "p95", why: "above the top bucket"}],
    tpot: [{statistic: "mean", value: 0.016}, {statistic: "p90", bucket: {lo: 0.01, hi: 0.02}}, {statistic: "p95", bucket: {lo: 0.01, hi: 0.02}}]};
  const withHour = Object.assign(copy(inputs), {measured: measuredHour,
    ttftModel: {heldOut: {statistic: "mean", within: 130, steps: 144, medianRatio: 0.94, rateFrom: 10, rateTo: 20, prefill: 3, decode: 6}}});
  const v = execView({inputs: withHour, plan: refusedPlan, report, k});
  check("the panel opens on the fleet today", v.lines[0].label === "Fleet today" && v.lines[0].text.startsWith("3 prefill + 6 decode: 9 pods, 72 GPUs (8 per pod)."), v.lines[0].text);
  check("one busiest load, its statistic named, the peak small", /The busiest load \(p95 of 1-minute cycles\): [0-9.]+ r\/s\.$/.test(v.lines[0].text) && /^peak /.test(v.lines[0].small),
    v.lines[0].text + " | " + v.lines[0].small);
  check("at most seven rows, in order", v.lines.map(l => l.label).join() === "Fleet today,Verdict,Decode,Prefill,TTFT / TPOT,+25% / +50% traffic,Accuracy",
    v.lines.map(l => l.label).join());
  const line = name => v.lines.find(l => l.label === name) || {text: ""};
  check("decode leads with what it carries", /^6 replicas carry 40 r\/s at the TPOT target, [0-9.]+x the busiest load\.$/.test(line("Decode").text), line("Decode").text);
  // The busiest load (41.7 r/s) was carried without queuing: prefill's
  // lower bound (20 r/s) is never shown under it.
  check("prefill is never shown under the load it carried", line("Prefill").text.startsWith("Not measured yet: at least 41.7 r/s (1.0x the busiest load; carried it without queuing). Each pod computed at least 18,000 tokens/s.") &&
    !/not known/i.test(line("Prefill").text), line("Prefill").text);
  const S = (pk, rate) => JSON.stringify(ctx.prefillAtLeast(pk, {rate: {value: rate}}));
  check("a lower bound under the load is raised to it", S({value: 20, flags: ["VPLowerBound"]}, 41.7) === JSON.stringify({value: 41.7, lb: true, raised: true}));
  check("a lower bound over the load stands", S({value: 50, flags: ["VPLowerBound"]}, 41.7) === JSON.stringify({value: 50, lb: true, raised: false}));
  check("a measured limit is never raised", S({value: 20}, 41.7) === JSON.stringify({value: 20, lb: false, raised: false}));
  const lbKnee = {value: 20, flags: ["VPLowerBound"]};
  check("fewer prefill replicas than carried the load are not raised to it",
    JSON.stringify(ctx.prefillAtLeast(lbKnee, {rate: {value: 41.7}}, 1, 2)) === JSON.stringify({value: 20, lb: true, raised: false}) &&
    ctx.prefillAtLeast(lbKnee, {rate: {value: 41.7}}, 2, 2).raised && ctx.prefillAtLeast(lbKnee, {rate: {value: 41.7}}, 3, 2).raised);
  check("the panel raises only the fleet that carried the load",
    !ctx.prefillAtLeastOf({ttft: {markers: {prefillKnee: lbKnee}}, scenario: {fleet: {prefill: 1, decode: 6}}}, {load: {rate: {value: 41.7}}, fleet: {prefill: 2}}).raised &&
    ctx.prefillAtLeastOf({ttft: {markers: {prefillKnee: lbKnee}}, scenario: {fleet: {prefill: 2, decode: 6}}}, {load: {rate: {value: 41.7}}, fleet: {prefill: 2}}).raised);
  // A limit bar says binds only where the limit binding was measured.
  const C = ctx.limitChip;
  check("a lower-bound prefill limit never binds", C("prefill", {value: 26.7, flags: ["VPLowerBound"]}, ["prefill"], true) === "limit not measured" &&
    C("prefill", {refused: {reason: "NoPrefillCapacity"}}, ["prefill"], true) === "limit not measured");
  check("decode binds only where it binds, measured", C("decode", {value: 30}, ["prefill"], true) === "" &&
    C("decode", {value: 30}, ["decode"], true) === "binds" && C("decode", {refused: {reason: "NoDecodeLine"}}, ["decode"], false) === "");
  check("a measured prefill limit that binds says so", C("prefill", {value: 26.7}, ["prefill"], false) === "binds" && C("prefill", {value: 26.7}, ["decode"], false) === "");
  check("a ratio under 1 keeps two decimals", ctx.execTimes1(0.98) === "0.98x" && ctx.execTimes1(1.6) === "1.6x");
  check("prefill's line carries the page actions", (line("Prefill").actions || []).map(a => a.act).join() === "usevp,busier" && line("Prefill").actions[0].vp === 18000);
  check("a lower-bound prefill is never called the constraint", line("Verdict").text.startsWith("Prefill's limit is not measured yet: the pods carried the busiest load without queuing, so it is at least that. Decode carries 0.96x the busiest load.") &&
    !/constraint/.test(line("Verdict").text), line("Verdict").text);
  const tt = line("TTFT / TPOT");
  check("one line: users saw, at the SLO's statistics, then the target", tt.text === "Users saw TTFT p90 3 s-5 s, TPOT mean 16 ms (busiest hour). The 1 s target is below this model's floor (4.08 s) at any load: set a realistic target under SLO.", tt.text);
  const more = (tt.more || []).join(" | ");
  check("the drawer holds the hour, its buckets and the prediction", more.includes("Users saw (measured, busiest hour 13:00-14:00 UTC, 24 r/s): TTFT mean 3.6 s, p90 3 s-5 s (one histogram bucket)") && more.includes("Predicted at "), more);
  check("the measured row is badged measured", tt.badge === EXEC_BADGES.measured);
  check("a percentile not given says why, once", more.includes("TTFT p95: above the top bucket") && more.includes("TPOT p90/p95: the router's 10 ms-20 ms bucket cannot resolve them"), more);
  check("the prediction past the tested range says extrapolated", /Predicted at [0-9.]+ r\/s \(above the tested 20 r\/s, extrapolated\): TTFT p90 about [0-9.]+ s; at the top of the tested range, 20 r\/s, about [0-9.]+ s\./.test(more), more);
  check("no line heads not known or not judged", v.lines.every(l => !/^(not known|not judged)/i.test(l.text)), v.lines.map(l => l.text.slice(0, 30)).join(" | "));
  check("the fleet today is measured", v.lines[0].badge === EXEC_BADGES.measured);
  check("estimates carry no badge, and a note says so", v.lines.filter(l => !l.badge).map(l => l.label).join() === "Verdict,Decode,Prefill,+25% / +50% traffic" && v.note === "Rows are estimates unless marked.",
    v.lines.map(l => l.label + ":" + l.badge).join(","));
  check("the accuracy is checked on your data", line("Accuracy").badge === EXEC_BADGES.checked);
  check("the quick check says the full check at the SLO's percentile has not run", line("Accuracy").text.includes("The full check, at TTFT p90, has not been run yet: Check accuracy on this recording runs it."), line("Accuracy").text);
  check("and offers it", JSON.stringify(line("Accuracy").actions) === JSON.stringify([{act: "check", label: "Check accuracy on this recording"}]), JSON.stringify(line("Accuracy").actions));
  check("the SLO used says a default is the page's", v.slo.includes("the page's default: set yours"), v.slo);
  check("growth not sized yet says pending", /^Pending/.test(line("+25% / +50% traffic").text));
  const grown = execView({inputs, plan: refusedPlan, report, k, growth: [{x: 1.25, rate: 25, need: "6 prefill + 12 decode"}, {x: 1.5, rate: 30, need: "at least 14 decode (prefill not sized)"}]});
  check("growth wired in reads each need", grown.lines.find(l => l.label === "+25% / +50% traffic").text.startsWith("+25% (25 r/s): 6 prefill + 12 decode; +50% (30 r/s): at least 14 decode"));
  // With the page's own check, the panel quotes its score at the SLO's
  // statistic, the one the check view shows, and says why steps went unscored.
  const checked = execView({inputs: withHour, plan: refusedPlan, report, bt: backtest, k});
  const acc = checked.lines.find(l => l.label === "Accuracy");
  check("the check's score is at the SLO's percentile", /TTFT p90 within 20% in \d+ of \d+ five-minute steps/.test(acc.text), acc.text);
  check("the check's unscored steps say why", /TTFT p90 not scored in \d+ \(/.test(acc.text), acc.text);
  check("the accuracy gives the range tested", /Tested range: .* r\/s; outside that is an extrapolation\./.test(acc.text), acc.text);
  check("the trust chip quotes the check, its statistic named", /^Checked on your traffic: TTFT p90 \d+\/\d+ within 20%$/.test(checked.trust), checked.trust);
  // Without the page's check, the server's held-out mean.
  // The server's quick check is named apart from the page's full check.
  check("the quick check at load is the accuracy, named as such", line("Accuracy").text.startsWith("Quick check on held-out steps of your traffic: TTFT mean within 20% in 130 of 144"), line("Accuracy").text);
  check("and the trust chip, naming the quick check and its statistic", v.trust === "Quick check on your traffic: TTFT mean 130/144 within 20%", v.trust);
  const held = v;
  const pending = execView({inputs, plan: refusedPlan, k, reportPending: true, btPending: true});
  check("a panel still asking says so", pending.lines.some(l => /^Checking/.test(l.text)));
  check("a check running is not offered again", pending.lines.every(l => !(l.actions || []).some(a => a.act === "check")));
  // The page's check runs only when asked: before it, with no quick check
  // from the server, the panel says plainly that nothing was checked and
  // offers the check; it shows no score, no checked badge and no chip
  // quoting one.
  const unchecked = execView({inputs, plan: refusedPlan, report, k});
  const notRun = unchecked.lines.find(l => l.label === "Accuracy");
  check("before a check, the accuracy says it has not run", !!notRun && notRun.text.startsWith("Not checked yet: the planner has not been checked on this recording."), notRun && notRun.text);
  check("and shows no score", !!notRun && !/within 20%|\d+ of \d+/.test(notRun.text), notRun && notRun.text);
  check("and no checked badge", !!notRun && notRun.badge === "", notRun && notRun.badge);
  check("and offers the check", !!notRun && JSON.stringify(notRun.actions) === JSON.stringify([{act: "check", label: "Check accuracy on this recording"}]), notRun && JSON.stringify(notRun.actions));
  check("the header chip says not checked", unchecked.trust === "Accuracy not checked yet", unchecked.trust);
  const selfChecked = execView({inputs: Object.assign(copy(inputs), {selfCheck: {line: "Self-check: tested on the last 12 h of this recording."}}), plan: refusedPlan, report, k});
  const sc = selfChecked.lines.find(l => l.label === "Accuracy");
  check("the server's self-check shows, with the full check offered", !!sc && sc.text.startsWith("Self-check:") && (sc.actions || []).some(a => a.act === "check"), sc && sc.text);
  check("and the chip says the full check has not run", selfChecked.trust === "Self-check only: full accuracy check not run yet", selfChecked.trust);
  const aggOnly =Object.assign(copy(inputs), {fleet: {decode: inputs.fleet.decode}});
  check("a fleet with no prefill, which the check cannot price, is offered no check", !execView({inputs: aggOnly, plan: refusedPlan, report, k}).lines.some(l => l.label === "Accuracy"));
  // The first screen speaks plainly.
  for (const l of v.lines.concat(held.lines, unchecked.lines)) {
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
  check("the day plan's headline names the peak and when", /Peak needed .* at \S+ UTC \([0-9.]+ r\/s\)/.test(ctx.dayHeadline(dayPlan)), ctx.dayHeadline(dayPlan));

  // A plan sized on decode alone never reads as holding the SLO: the
  // headline counts the intervals whose TTFT was measured over its target.
  const decodeOnly = JSON.parse(fs.readFileSync("cmd/capacity-planner/testdata/day-plan-staircase-b-on-json.golden", "utf8"));
  const dh = ctx.dayHeadline(decodeOnly);
  check("the day plan's headline leads with the measured TTFT misses, as risk", dh.startsWith("SLO risk: measured TTFT p95 was over 0.19 s in every interval (prefill not sized). Peak needed"), dh);
  check("and never says no hour missed the SLO", !/No hour missed/.test(dh), dh);

  // Exports: a field a spreadsheet would run is not a formula.
  check("a CSV field is quoted where it must be", csvField("a,b") === "\"a,b\"" && csvField("say \"x\"") === "\"say \"\"x\"\"\"");
  check("a CSV field is never a formula", csvField("=1+1") === "'=1+1" && csvField("-1.5") === "-1.5");
  check("a CSV field opening with a tab or a carriage return is never a formula",
    csvField("\t=1+1") === "'\t=1+1" && csvField("\r=1+1") === "\"'\r=1+1\"");
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
