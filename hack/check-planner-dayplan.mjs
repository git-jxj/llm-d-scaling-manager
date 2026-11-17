// Runs the capacity planner page's day plan drawing
// (internal/whatifapi/page/dayplan.js) on the day plan the CLI's golden
// holds, and on that plan cut into pieces: the charts share one time axis,
// every series is drawn, a gap breaks the lines, and a bucket short of what
// it needed is shaded. Called by hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-dayplan.mjs [scenario.js dayplan.js plan.json]
import fs from "node:fs";
import vm from "node:vm";

const [scen, day, golden] = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden",
];
const ctx = vm.createContext({URLSearchParams});
for (const f of [scen, day]) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const {dayPlanSVG, dayBucketAt, dayTip, dayHoursTable, daySummaryText} = ctx;
const plan = JSON.parse(fs.readFileSync(golden, "utf8"));

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

// xs is every x coordinate of a path's d.
function xs(d) {
  return [...d.matchAll(/[ML](-?[\d.]+),/g)].map(m => +m[1]);
}

try {
  check("the golden plan has buckets", plan.buckets && plan.buckets.length > 5, plan.buckets && plan.buckets.length);
  const sized = plan.buckets.filter(b => b.status === "sized");
  check("the golden plan sizes some buckets", sized.length > 0);
  const g = dayPlanSVG(plan);
  const panels = attrs(g.markup, "g").filter(a => a.class === "panel");
  check("three panels", panels.length === 3, panels.map(p => p["data-panel"]).join(","));
  check("panels in order", panels.map(p => p["data-panel"]).join(",") === "rate,prefill,decode");
  const x0 = Date.parse(plan.from) / 1000, x1 = Date.parse(plan.to) / 1000;
  for (const p of panels) {
    check(`${p["data-panel"]} shares the x domain`, +p["data-x0"] === x0 && +p["data-x1"] === x1, `${p["data-x0"]}..${p["data-x1"]}`);
  }
  const series = new Set(attrs(g.markup, "path").map(a => a["data-series"]).concat(attrs(g.markup, "polygon").map(a => a["data-series"])));
  for (const s of ["rate-mean", "rate-p95", "prefill-needed", "prefill-actual", "decode-needed", "decode-actual"]) {
    check(`draws ${s}`, series.has(s), [...series].join(","));
  }
  // Every mark sits inside the plot, between the left margin and the labels.
  const L = g.L, R = g.L + g.pw;
  for (const p of attrs(g.markup, "path")) {
    const x = xs(p.d);
    check(`${p["data-series"]} inside the plot`, x.length > 0 && Math.min(...x) >= L - 0.1 && Math.max(...x) <= R + 0.1, `${Math.min(...x)}..${Math.max(...x)}`);
  }
  // The shading: one rectangle per bucket a role was short in.
  for (const role of ["prefill", "decode"]) {
    const want = plan.buckets.filter(b => b.needed && b.actual && b.actual[role] < b.needed[role]).length;
    const got = attrs(g.markup, "rect").filter(a => a["data-under"] === role).length;
    check(`${role} shaded where short`, got === want, `${got} shaded, ${want} short`);
  }
  // A bucket no fleet serves is greyed in both replica panels.
  const unmet = plan.buckets.filter(b => b.status === "unmet").length;
  check("unmet buckets greyed", attrs(g.markup, "rect").filter(a => a["data-unmet"]).length === 2 * unmet, unmet);
  // No label sits on another.
  const labels = attrs(g.markup, "text").filter(a => a.class === "lbl").map(a => ({x: +a.x, y: +a.y}));
  for (let i = 0; i < labels.length; i++) {
    for (let j = i + 1; j < labels.length; j++) {
      check(`labels ${i} and ${j} apart`, labels[i].x !== labels[j].x || Math.abs(labels[i].y - labels[j].y) >= 11.9, JSON.stringify([labels[i], labels[j]]));
    }
  }

  // A gap: a bucket with no data in the middle breaks the needed line into two.
  const gap = JSON.parse(JSON.stringify(plan));
  const mid = gap.buckets.findIndex((b, i) => i > 0 && b.status === "sized" && gap.buckets[i - 1].status === "sized" &&
    gap.buckets[i + 1] && gap.buckets[i + 1].status === "sized");
  check("the golden has three sized buckets in a row", mid > 0);
  const runs = s => attrs(dayPlanSVG(s).markup, "path").filter(a => a["data-series"] === "decode-needed").length;
  const before = runs(gap);
  gap.buckets[mid] = {start: gap.buckets[mid].start, samples: 0, requests: 0, shape: {source: "window"}, status: "noData"};
  check("a gap breaks the line", runs(gap) === before + 1, `${before} then ${runs(gap)}`);

  // The tooltip names every value of the bucket under the pointer.
  const b = sized[0], t = Date.parse(b.start) / 1000 + plan.step / 2;
  check("the bucket at a time", dayBucketAt(plan, t) === b);
  check("no bucket past the end", dayBucketAt(plan, x1 + 1) === null);
  const tip = dayTip(plan, b).rows.map(r => r[0]);
  for (const k of ["rate mean", "rate p95", "sized for", "needed", "had", "verdict"]) check(`the tip has ${k}`, tip.includes(k), tip.join(","));
  check("the table has every hour", (dayHoursTable(plan).match(/<tr>/g) || []).length === plan.hours.length + 1);
  check("the summary says the peak", daySummaryText(plan)[0].startsWith("Needed at peak"));
  // Markup from the plan is escaped: a flag is text, never markup.
  const evil = JSON.parse(JSON.stringify(plan));
  evil.buckets[0].flags = ["<img src=x onerror=1>"];
  const rows = dayTip(evil, evil.buckets[0]).rows;
  check("a flag is data", rows.some(r => r[1].includes("<img")));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL: ${failures.length} of ${passed + failures.length} day plan checks failed:`);
  for (const f of failures) console.error("  " + f);
  process.exit(1);
}
if (passed < 30) {
  console.error(`FAIL: only ${passed} day plan checks ran`);
  process.exit(1);
}
console.log(`ok: ${passed} day plan checks passed`);
