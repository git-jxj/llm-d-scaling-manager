// Runs the capacity planner page's SLO target controls and its day plan
// (internal/whatifapi/page/planner.js: slider, fitSlider, the target boxes,
// runDay, dayStale, showView) against a fake DOM whose range inputs clamp
// their value to min and max as a browser does, and a fake server that
// answers a day plan at the target asked. A target typed in the box is the
// target, past the slider's old range too, and a later touch of the slider
// does not replace it; the slider reaches the floor the plan priced; a day
// plan says the targets it used, reads stale once they change, and is
// planned again when its view opens. Called by
// hack/check-planner-scenarios.sh.
//
// Usage: node hack/check-planner-targets.mjs [scenario.js dayplan.js backtest.js planner.js day-plan.json]
import fs from "node:fs";
import vm from "node:vm";

const files = [
  process.argv[2] || "internal/whatifapi/page/scenario.js",
  process.argv[3] || "internal/whatifapi/page/dayplan.js",
  process.argv[4] || "internal/whatifapi/page/backtest.js",
  process.argv[5] || "internal/whatifapi/page/planner.js",
];
const golden = JSON.parse(fs.readFileSync(process.argv[6] || "cmd/capacity-planner/testdata/day-plan-ols-sweep-json.golden", "utf8"));

// fakeElement is as much of a DOM element as these controls touch.
function fakeElement(tag) {
  return {
    tagName: tag, textContent: "", innerHTML: "", className: "", hidden: false, value: "", disabled: false, title: "", type: "",
    style: {}, dataset: {}, children: [], onclick: null, checked: false, open: false,
    classList: {
      set: new Set(),
      add(c) { this.set.add(c); }, remove(c) { this.set.delete(c); },
      toggle(c, on) { if (on === undefined ? !this.set.has(c) : on) this.set.add(c); else this.set.delete(c); },
      contains(c) { return this.set.has(c); },
    },
    appendChild(c) { this.children.push(c); return c; },
    replaceChildren(...cs) { this.children = cs; },
    querySelector() { return fakeElement("div"); },
    querySelectorAll() { return []; },
    addEventListener() {},
    setAttribute() {},
    focus() {},
  };
}

// fakeRange is an <input type="range">: its value reads clamped to min and
// max as they stand and snapped to min plus whole steps, the browser's value
// sanitization, with no attribute meaning 0 to 100 in steps of 1.
function fakeRange() {
  const e = fakeElement("input");
  let raw = "50";
  e.min = "";
  e.max = "";
  e.step = "";
  Object.defineProperty(e, "value", {
    get() {
      const lo = e.min === "" ? 0 : +e.min, hi = Math.max(lo, e.max === "" ? 100 : +e.max), st = e.step === "" ? 1 : +e.step;
      let v = lo + Math.round((Math.min(Math.max(+raw, lo), hi) - lo) / st) * st;
      if (v > hi) v -= st;
      return String(Number(v.toFixed(9)));
    },
    set(v) { raw = String(v); },
  });
  return e;
}

const byId = new Map([["ttft", fakeRange()], ["tpot", fakeRange()]]);
const document = {
  getElementById(id) { if (!byId.has(id)) byId.set(id, fakeElement("div")); return byId.get(id); },
  createElement: fakeElement,
  addEventListener() {},
  querySelector() { return fakeElement("div"); },
};

// The fake server answers a day plan at the SLO it was asked.
const calls = [];
function respond(status, doc) {
  return {ok: status < 300, status, statusText: "x", headers: {get: () => null}, json: async () => doc};
}
async function fakeFetch(path, opt) {
  const body = JSON.parse(opt.body || "null");
  calls.push({path, body});
  if (path === "v1/dayplan") {
    const d = JSON.parse(JSON.stringify(golden));
    d.ttft = Object.assign({}, d.ttft, {statistic: body.slo.ttft.statistic, target: body.slo.ttft.target});
    return respond(200, d);
  }
  return respond(404, {error: "no " + path});
}

const ctx = vm.createContext({document, location: {hash: ""}, history: {replaceState() {}}, fetch: fakeFetch, AbortController, DOMException,
  setTimeout, clearTimeout, setInterval, clearInterval, URLSearchParams, performance, console});
for (const f of files) vm.runInContext(fs.readFileSync(f, "utf8"), ctx, {filename: f});
const run = src => vm.runInContext(src, ctx);
// No plan is priced here: plan() is the exec check's.
run("plan = async () => {};");
const state = run("state");
const el = id => document.getElementById(id);

let passed = 0;
const failures = [];
function check(name, ok, detail) {
  if (ok) passed++;
  else failures.push(name + (detail === undefined ? "" : ": " + detail));
}
const until = async (cond, ms = 5000) => {
  const t0 = Date.now();
  while (!cond()) {
    if (Date.now() - t0 > ms) return false;
    await new Promise(r => setTimeout(r, 10));
  }
  return true;
};

const inputs = {inputsId: "sha256:x", model: "m",
  fleet: {prefill: {variant: "p", replicas: 2, gpusPerReplica: 8}, decode: {variant: "d", replicas: 2, gpusPerReplica: 8}},
  source: {kind: "recording", name: "r"}, terms: [], observed: {buckets: [], outputTokens: {value: 300}}};
// A plan whose TTFT floor, 2.92 s, is past four times the page's 1 s.
const plan = {ttft: {statistic: "p90", markers: {}, line: {target: 1, floor: {value: 2.92}, curve: []}}, tpot: {statistic: "mean", floor: {value: 0.012}, curve: []}};

try {
  run("wire()");
  state.inputs = inputs;
  state.plan = plan;
  state.mode = "plan";
  state.k = {ttftStat: "p90", ttftTarget: 1, tpotStat: "mean", tpotTarget: 0.05, ttftDefault: true, tpotDefault: true,
    headroom: 0.85, assume: {}, edges: "", perBucket: false, maxRep: 32, rate: 5, prefill: 2, decode: 2, priceAs: "mix", outputTokens: 300};
  el("dayrun").disabled = false;
  // The page at load: the slider around the 1 s target.
  run("slider(\"ttft\", 1000, 1)");
  run("slider(\"tpot\", 50, 0.1)");
  check("the slider starts at the target", +el("ttft").value === 1000, el("ttft").value);

  // A target typed past the slider's range is the target, and the slider
  // shows it rather than its end.
  el("ttftbox").value = "5000";
  el("ttftbox").onchange();
  check("the typed target is taken", state.k.ttftTarget === 5, state.k.ttftTarget);
  check("the slider shows the typed target, not its end", +el("ttft").value === 5000, `${el("ttft").value} in ${el("ttft").min}..${el("ttft").max}`);
  // The slider touched (released where it stands) keeps the typed target.
  el("ttft").onchange();
  check("touching the slider keeps the typed target", state.k.ttftTarget === 5, state.k.ttftTarget);
  // Below the range too.
  el("ttftbox").value = "20";
  el("ttftbox").onchange();
  el("ttft").onchange();
  check("a target typed below the range is kept", state.k.ttftTarget === 0.02, state.k.ttftTarget);
  el("tpotbox").value = "400";
  el("tpotbox").onchange();
  el("tpot").onchange();
  check("a TPOT typed past the range is kept", Math.abs(state.k.tpotTarget - 0.4) < 1e-9, state.k.tpotTarget);

  // The slider reaches the floor the plan priced, from a target below it.
  state.k.ttftTarget = 0.2;
  run("slider(\"ttft\", 200, 1)");
  run("if (typeof fitSlider === \"function\") fitSlider(\"ttft\", state.k.ttftTarget * 1000, 1, targetFloor(\"ttft\"))");
  check("the slider runs past the floor", +el("ttft").max > 2920, `max ${el("ttft").max}`);
  check("the target stays where it was", +el("ttft").value === 200 && state.k.ttftTarget === 0.2, el("ttft").value);
  el("ttft").value = el("ttft").max;
  el("ttft").onchange();
  check("dragged to the end, the target is past the floor", state.k.ttftTarget > 2.92, state.k.ttftTarget);
  // A floor that is no whole number of steps still leaves a typed whole
  // target whole: the slider touched does not snap 150 to 149.865.
  run("slider(\"ttft\", 200, 1, 37.3)");
  el("ttftbox").value = "150";
  el("ttftbox").onchange();
  el("ttft").onchange();
  check("an unaligned floor does not move a typed target", state.k.ttftTarget === 0.15, state.k.ttftTarget);
  run("slider(\"tpot\", 50, 0.1, 7.3)");
  el("tpotbox").value = "20.5";
  el("tpotbox").onchange();
  el("tpot").onchange();
  check("an unaligned TPOT floor does not move a typed target", Math.abs(state.k.tpotTarget - 0.0205) < 1e-12, state.k.tpotTarget);

  // The day plan: it says the targets it used.
  state.k.ttftTarget = 5;
  state.k.tpotTarget = 0.05;
  await run("runDay()");
  check("the day plan was asked at 5 s", calls.length === 1 && calls[0].body.slo.ttft.target === 5, JSON.stringify(calls.map(c => c.body.slo)));
  check("the day plan says the targets it used", /TTFT p90 5\.00 s/.test(el("dayused").textContent), el("dayused").textContent);
  check("a fresh day plan is not stale", !el("dayout").classList.contains("stale") && el("daystale").hidden === true);

  // A target changed in the box marks it stale, with what it used.
  el("ttftbox").value = "500";
  el("ttftbox").onchange();
  check("a changed target marks the day plan stale", el("dayout").classList.contains("stale"));
  check("the stale note names the old target", el("daystale").hidden === false && /5\.00 s/.test(el("daystale").textContent), el("daystale").textContent);
  // Opening the day view plans it again, at the new target.
  run("showView(\"day\")");
  check("opening the day view plans again", await until(() => calls.length === 2 && !state.dayBusy), calls.length);
  check("planned again at 500 ms", calls.length === 2 && calls[1].body.slo.ttft.target === 0.5, JSON.stringify(calls.map(c => c.body.slo.ttft.target)));
  check("the replanned day says 500 ms", /TTFT p90 500 ms/.test(el("dayused").textContent), el("dayused").textContent);
  check("the replanned day is not stale", !el("dayout").classList.contains("stale") && el("daystale").hidden === true);
  // Opening it again with nothing changed asks nothing.
  run("showView(\"answer\"); showView(\"day\")");
  await new Promise(r => setTimeout(r, 50));
  check("an unchanged day plan is not asked again", calls.length === 2, calls.length);
  // The bucket the day plan reads is a knob it uses too.
  el("daystep").value = "1h";
  el("daystep").onchange && el("daystep").onchange();
  check("a changed bucket marks the day plan stale", el("dayout").classList.contains("stale"));
} catch (e) {
  failures.push("threw: " + (e && e.stack || e));
}

if (failures.length) {
  console.error(`FAIL check-planner-targets: ${failures.length} of ${passed + failures.length}`);
  for (const f of failures) console.error("  - " + f);
  process.exit(1);
}
console.log(`ok check-planner-targets: ${passed} checks`);
