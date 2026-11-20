// Runs the capacity planner page's "Check the planner" drawing
// (internal/whatifapi/page/backtest.js) on the backtest the CLI's golden
// holds, and on copies of it cut and altered: the two panels share one time
// axis, every predicted and measured series is drawn, a step not judged is
// greyed with its reason, a prediction above the axis is marked, the
// summary leads with the verdict, and everything taken from the answer is
// escaped. Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-backtest.mjs [scenario.js dayplan.js backtest.js backtest.json]
import fs from "node:fs";
import vm from "node:vm";

const [scen, day, bt, golden] = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "cmd/capacity-planner/testdata/backtest-q32-split-json.golden",
];
const ctx = vm.createContext({URLSearchParams});
for (const f of [scen, day, bt]) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const {btSVG, btSummaryText, btLegend, btStepAt, btTipHTML, btScoreTable, dayTime} = ctx;
const answer = JSON.parse(fs.readFileSync(golden, "utf8"));
const copy = x => JSON.parse(JSON.stringify(x));

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}

// attrs is every element of tag in markup, as its attributes.
function attrs(markup, tag) {
  const out = [];
  for (const m of markup.matchAll(new RegExp(`<${tag}\\b([^>]*)>`, "g"))) {
    const a = {};
    for (const p of m[1].matchAll(/([\w-]+)="([^"]*)"/g)) a[p[1]] = p[2];
    out.push(a);
  }
  return out;
}

try {
  check("the golden has steps", answer.steps && answer.steps.length > 5, answer.steps && answer.steps.length);
  const g = btSVG(answer);
  const panels = attrs(g.markup, "g").filter(a => a.class === "panel");
  check("two panels", panels.length === 2, panels.length);
  check("the panels share one x domain", new Set(panels.map(p => p["data-x0"] + "/" + p["data-x1"])).size === 1);
  const series = new Set([...attrs(g.markup, "path"), ...attrs(g.markup, "g")].map(a => a["data-series"]).filter(Boolean));
  for (const s of ["ttft-p50-predicted", "ttft-p90-predicted", "ttft-p50-measured", "ttft-p90-measured", "tpot-p50-measured", "tpot-p90-measured"]) {
    check("series " + s + " is drawn", series.has(s));
  }
  const excluded = answer.steps.filter(s => s.excluded);
  const greys = attrs(g.markup, "rect").filter(a => a.class === "btgrey");
  check("every step not judged is greyed in both panels", greys.length === 2 * excluded.length, `${greys.length} vs ${excluded.length}`);
  check("a greyed step names its reason", greys.every(a => a["data-excluded"]));
  const dots = (g.markup.match(/<circle class="dot/g) || []).length;
  check("measured figures are drawn as dots", dots > answer.steps.length, dots);

  // A prediction far above everything measured is clipped and marked.
  const big = copy(answer);
  const judged = big.steps.find(s => !s.excluded && s.ttft.find(p => p.statistic === "p90").predicted.value != null);
  judged.ttft.find(p => p.statistic === "p90").predicted.value = 1e6;
  const gb = btSVG(big);
  check("a prediction above the axis is marked", /data-clipped="ttft-p90"/.test(gb.markup));

  // The summary leads with the verdict, then what was not judged.
  const lines = btSummaryText(answer);
  check("the verdict comes first", lines[0] === answer.verdict, lines[0]);
  check("what was not judged comes second", lines[1] === answer.excluded, lines[1]);
  check("the legend names the band and the greyed steps", /upper bound/.test(btLegend()) && /not judged/.test(btLegend()));

  // The tooltip of a step says its inputs and each figure.
  const s0 = answer.steps[1];
  const at = btStepAt(answer, dayTime(s0.start) + 1);
  check("the step under the cursor is found", at === s0);
  const tip = btTipHTML(answer, s0);
  check("the tooltip names TTFT and TPOT", /TTFT p90/.test(tip) && /TPOT p90/.test(tip), tip);
  check("the score table has a row a statistic", (btScoreTable(answer).match(/<tr>/g) || []).length === 1 + 2 * answer.summary.ttft.length);

  // Everything taken from the answer is escaped.
  const evil = copy(answer);
  evil.verdict = "<script>alert(1)</script>";
  evil.fit.source = "<img src=x onerror=alert(1)>";
  evil.steps[0].excluded = "\"><script>x</script>";
  evil.steps[0].detail = "<b>bold</b>";
  const ge = btSVG(evil).markup + btTipHTML(evil, evil.steps[0]);
  check("an excluded reason is escaped in the chart and the tooltip", !/<script>/.test(ge), ge.slice(0, 200));
  check("the summary is text, escaped where it is put", btSummaryText(evil)[0] === "<script>alert(1)</script>");

  // A step with a refused prediction draws no predicted point and says why.
  const refused = copy(answer);
  for (const p of refused.steps[3].ttft) {
    p.predicted = {refused: {reason: "Saturated", detail: "past the knee"}};
  }
  check("a refused prediction is named in the tooltip", /Saturated/.test(btTipHTML(refused, refused.steps[3])));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-backtest: ${failures.length} failed, ${passed} passed`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}
if (passed < 15) {
  console.error(`FAIL check-planner-backtest: only ${passed} checks ran`);
  process.exit(1);
}
console.log(`ok check-planner-backtest: ${passed} checks`);
