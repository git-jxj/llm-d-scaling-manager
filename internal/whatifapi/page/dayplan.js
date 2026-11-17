// The capacity planner page's day plan (POST /v1/dayplan), drawn as pure
// functions: the charts as SVG markup, the tooltip at a time, and the table
// view. No DOM and no browser API, so hack/check-planner-scenarios.sh runs
// them under node on fixtures. The page loads this file after scenario.js
// (esc) and before planner.js, which puts the markup on the page.
//
// Three small multiples share one time axis: the request rate (its mean,
// and a band from the mean to the p95), then needed against had for the
// prefill and then the decode replicas, each a step line. req/s and
// replicas are different units, so they never share a y axis. Where a role
// had fewer replicas than it needed, its panel is shaded.
"use strict";

// DAY_W is the charts' width in the SVG's units; DAY_L and DAY_R the margins
// for the y ticks and for the direct labels at the right.
const DAY_W = 960, DAY_L = 56, DAY_R = 140, DAY_T = 12;
// DAY_PANELS is each panel: its key, its title, and its height.
const DAY_PANELS = [
  {key: "rate", title: "Requests per second", h: 150},
  {key: "prefill", title: "Prefill replicas", h: 110},
  {key: "decode", title: "Decode replicas", h: 110},
];
// DAY_GAP is the space between panels, for the next panel's title.
const DAY_GAP = 30;
// DAY_TICK_STEPS are the time axis' tick spacings, seconds, smallest first.
const DAY_TICK_STEPS = [300, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400];

// dayTime is an RFC 3339 time as seconds since the epoch.
function dayTime(s) { return Date.parse(s) / 1000; }

// dayClock is t, seconds since the epoch, as HH:MM UTC.
function dayClock(t) { return new Date(t * 1000).toISOString().slice(11, 16); }

// dayNiceMax is a round top for a y axis over values up to v.
function dayNiceMax(v, integer) {
  if (!(v > 0)) return integer ? 2 : 1;
  const p = Math.pow(10, Math.floor(Math.log10(v)));
  for (const m of [1, 1.2, 1.5, 2, 2.5, 3, 4, 5, 6, 8, 10]) {
    const top = m * p;
    if (top >= v * 1.05 && (!integer || Number.isInteger(top))) return top;
  }
  return Math.ceil(v * 1.1);
}

// dayYTicks are about four round values from 0 to max: whole numbers on a
// replica axis.
function dayYTicks(max, integer) {
  let step = max / 4;
  const p = Math.pow(10, Math.floor(Math.log10(step)));
  const m = step / p;
  step = (m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10) * p;
  if (integer) step = Math.max(1, Math.round(step));
  const out = [];
  for (let v = 0; v <= max + step * 1e-6; v += step) out.push(Number(v.toPrecision(10)));
  return out;
}

// dayXTicks are the time axis' ticks: the smallest spacing that gives at
// most eight, on whole multiples of it in UTC.
function dayXTicks(x0, x1) {
  const span = x1 - x0;
  const step = DAY_TICK_STEPS.find(s => span / s <= 8) || DAY_TICK_STEPS[DAY_TICK_STEPS.length - 1];
  const out = [];
  for (let t = Math.ceil(x0 / step) * step; t <= x1; t += step) out.push(t);
  return out;
}

// dayFmt is a rate for a label: two significant decimals under 10.
function dayFmt(v) {
  return v == null ? "-" : v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2);
}

// dayLayout is where each panel sits, and the shared x scale: the first
// bucket's start to the last bucket's end.
function dayLayout(plan) {
  const x0 = dayTime(plan.from), x1 = dayTime(plan.to), pw = DAY_W - DAY_L - DAY_R;
  let y = DAY_T;
  const panels = DAY_PANELS.map(p => {
    const out = {key: p.key, title: p.title, top: y + 16, h: p.h};
    y = out.top + p.h + DAY_GAP;
    return out;
  });
  const H = y - DAY_GAP + 22;
  return {x0, x1, pw, H, panels, X: t => DAY_L + pw * (t - x0) / (x1 - x0)};
}

// dayRuns are the runs of consecutive buckets that have a value: a line
// breaks where a bucket has none.
function dayRuns(buckets, value) {
  const runs = [];
  let run = null;
  for (const b of buckets) {
    const v = value(b);
    if (v == null) { run = null; continue; }
    if (!run) { run = []; runs.push(run); }
    run.push({t: dayTime(b.start), v});
  }
  return runs;
}

// dayStep is a run as a step line's path: flat across each bucket, then
// straight up or down to the next.
function dayStep(run, step, X, Y) {
  let d = "";
  for (const [i, q] of run.entries()) {
    d += (i ? "L" : "M") + X(q.t).toFixed(1) + "," + Y(q.v).toFixed(1) + "L" + X(q.t + step).toFixed(1) + "," + Y(q.v).toFixed(1);
  }
  return d;
}

// dayLine is a run as a line through each bucket's middle.
function dayLine(run, step, X, Y) {
  return run.map((q, i) => (i ? "L" : "M") + X(q.t + step / 2).toFixed(1) + "," + Y(q.v).toFixed(1)).join("");
}

// dayRoleValue reads a role's count from a bucket's needed or actual.
function dayRoleValue(which, role) {
  return b => b[which] ? b[which][role] : null;
}

// dayPlanSVG is the day plan's charts: the SVG's inner markup, its viewBox,
// and the layout the hover reads. Every series is a path with
// data-series, each panel a group with data-panel and the x domain it is
// drawn on (data-x0, data-x1), so that a check can see they share one.
function dayPlanSVG(plan) {
  const lay = dayLayout(plan), X = lay.X, step = plan.step, b = plan.buckets;
  const xr = DAY_L + lay.pw;
  let s = "";
  for (const p of lay.panels) {
    const integer = p.key !== "rate";
    let vmax = 0;
    for (const k of b) {
      if (integer) {
        for (const w of ["needed", "actual"]) if (k[w]) vmax = Math.max(vmax, k[w][p.key]);
      } else if (k.rate) {
        vmax = Math.max(vmax, k.rate.p95);
      }
    }
    const ymax = dayNiceMax(vmax, integer);
    const Y = v => p.top + p.h * (1 - Math.min(v, ymax) / ymax);
    let g = `<text class="ptitle" x="${DAY_L}" y="${p.top - 6}">${esc(p.title)}</text>`;
    for (const v of dayYTicks(ymax, integer)) {
      if (v > ymax) continue;
      g += `<line class="${v ? "gridl" : "axisl"}" x1="${DAY_L}" x2="${xr}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/>` +
        `<text x="${DAY_L - 8}" y="${(Y(v) + 4).toFixed(1)}" text-anchor="end">${dayFmtTick(v)}</text>`;
    }
    const labels = [];
    if (p.key === "rate") {
      for (const run of dayRuns(b, k => k.rate ? [k.rate.mean, k.rate.p95] : null)) {
        const up = run.map(q => `${X(q.t + step / 2).toFixed(1)},${Y(q.v[1]).toFixed(1)}`);
        const down = run.slice().reverse().map(q => `${X(q.t + step / 2).toFixed(1)},${Y(q.v[0]).toFixed(1)}`);
        g += `<polygon class="dband" data-series="rate-p95" points="${up.concat(down).join(" ")}"/>`;
      }
      for (const run of dayRuns(b, k => k.rate ? k.rate.mean : null)) {
        g += `<path class="line c-0" data-series="rate-mean" d="${dayLine(run, step, X, Y)}"/>`;
      }
      const last = b.slice().reverse().find(k => k.rate);
      if (last) {
        labels.push({y: Y(last.rate.mean), text: "mean " + dayFmt(last.rate.mean)});
        labels.push({y: Y(last.rate.p95), text: "p95 " + dayFmt(last.rate.p95)});
      }
    } else {
      // Shaded where the role had fewer than it needed; greyed where no
      // fleet within the bound serves the bucket, so that the gap in the
      // needed line reads as a refusal, not as nothing needed.
      for (const k of b) {
        if (k.status === "unmet") {
          const t = dayTime(k.start);
          g += `<rect class="dunmet" data-unmet="${esc(p.key)}" x="${X(t).toFixed(1)}" y="${p.top}" width="${(X(t + step) - X(t)).toFixed(1)}" height="${p.h}"/>`;
        }
        if (k.needed && k.actual && k.actual[p.key] < k.needed[p.key]) {
          const t = dayTime(k.start);
          g += `<rect class="dunder" data-under="${esc(p.key)}" x="${X(t).toFixed(1)}" y="${p.top}" width="${(X(t + step) - X(t)).toFixed(1)}" height="${p.h}"/>`;
        }
      }
      for (const run of dayRuns(b, dayRoleValue("actual", p.key))) {
        g += `<path class="line c-had" data-series="${p.key}-actual" d="${dayStep(run, step, X, Y)}"/>`;
      }
      for (const run of dayRuns(b, dayRoleValue("needed", p.key))) {
        g += `<path class="line c-0" data-series="${p.key}-needed" d="${dayStep(run, step, X, Y)}"/>`;
      }
      const lastN = b.slice().reverse().find(k => k.needed), lastA = b.slice().reverse().find(k => k.actual);
      if (lastN) labels.push({y: Y(lastN.needed[p.key]), text: "needed " + lastN.needed[p.key]});
      if (lastA) labels.push({y: Y(lastA.actual[p.key]), text: "had " + lastA.actual[p.key]});
    }
    g += dayLabels(labels, xr + 6, p.top, p.top + p.h);
    s += `<g class="panel" data-panel="${p.key}" data-x0="${lay.x0}" data-x1="${lay.x1}">${g}</g>`;
  }
  const bottom = lay.panels[lay.panels.length - 1];
  const yb = bottom.top + bottom.h;
  for (const t of dayXTicks(lay.x0, lay.x1)) {
    s += `<line class="gridl" x1="${X(t).toFixed(1)}" x2="${X(t).toFixed(1)}" y1="${yb}" y2="${yb + 4}"/>` +
      `<text x="${X(t).toFixed(1)}" y="${yb + 16}" text-anchor="middle">${dayClock(t)}</text>`;
  }
  s += `<text x="${xr}" y="${yb + 16}" text-anchor="start" dx="8">UTC</text>`;
  s += `<line class="xh" x1="0" x2="0" y1="${lay.panels[0].top}" y2="${yb}" visibility="hidden"/>`;
  return {markup: s, viewBox: `0 0 ${DAY_W} ${lay.H}`, x0: lay.x0, x1: lay.x1, L: DAY_L, pw: lay.pw, W: DAY_W};
}

// dayFmtTick is a y tick: whole where it is.
function dayFmtTick(v) { return Number.isInteger(v) ? String(v) : String(Number(v.toPrecision(3))); }

// dayLabels are the direct labels at the right of a panel, top to bottom,
// each at its line's end and kept inside the panel. Two whose lines end
// closer than a line of text are one label, "needed 1, had 1", rather than
// one nudged off its line.
function dayLabels(labels, x, top, bottom) {
  const sorted = labels.slice().sort((a, b) => a.y - b.y);
  const placed = [];
  for (const l of sorted) {
    const y = Math.min(Math.max(l.y + 4, top + 10), bottom);
    const prev = placed[placed.length - 1];
    if (prev && y - prev.y < 12) prev.text += ", " + l.text;
    else placed.push({y, text: l.text});
  }
  return placed.map(l => `<text class="lbl" x="${x}" y="${l.y.toFixed(1)}">${esc(l.text)}</text>`).join("");
}

// DAY_STATUS is a bucket's status in words.
const DAY_STATUS = {sized: "sized", tooLittleData: "too little data", noData: "no data", unmet: "no fleet within the bound"};
// DAY_VERDICT is a verdict in words.
const DAY_VERDICT = {under: "under: SLO risk", over: "over: waste", match: "matches"};

// dayBucketAt is the bucket that holds t, seconds since the epoch, or null.
function dayBucketAt(plan, t) {
  for (const b of plan.buckets) {
    const s = dayTime(b.start);
    if (t >= s && t < s + plan.step) return b;
  }
  return null;
}

// dayTip is the tooltip of a bucket: every value at that time.
function dayTip(plan, b) {
  const rows = [];
  const row = (k, v) => rows.push([k, v]);
  row("status", DAY_STATUS[b.status] || b.status);
  if (b.rate) {
    row("rate mean", dayFmt(b.rate.mean) + " r/s");
    row("rate p95", dayFmt(b.rate.p95) + " r/s");
    row("rate max", dayFmt(b.rate.max) + " r/s");
  }
  if (b.sizedRate != null) row("sized for", dayFmt(b.sizedRate) + " r/s (" + plan.sizeFor + ")");
  row("requests", b.requests ? b.requests.toFixed(0) : "0");
  if (b.needed) row("needed", `${b.needed.prefill} prefill, ${b.needed.decode} decode`);
  if (b.actual) row("had", `${b.actual.prefill} prefill, ${b.actual.decode} decode`);
  if (b.verdict) row("verdict", DAY_VERDICT[b.verdict] || b.verdict);
  if (b.unmet) row("refused", b.unmet.reason);
  if (b.shape && b.shape.promptTokens != null) row("prompt, output", `${b.shape.promptTokens.toFixed(0)}, ${b.shape.outputTokens != null ? b.shape.outputTokens.toFixed(0) : "-"} tokens (${b.shape.source})`);
  if (b.flags && b.flags.length) row("flags", b.flags.map(f => f === "Extrapolated" ? "EXTRAPOLATED" : f).join(", "));
  return {title: dayClock(dayTime(b.start)) + " to " + dayClock(dayTime(b.start) + plan.step) + " UTC", rows};
}

// dayTipHTML is dayTip as the page's tooltip markup.
function dayTipHTML(plan, b) {
  const t = dayTip(plan, b);
  return `<div class="t">${esc(t.title)}</div>` + t.rows.map(([k, v]) => `<div class="r"><span>${esc(k)}</span><span>${esc(v)}</span></div>`).join("");
}

// dayHoursTable is the table view: one row per hour.
function dayHoursTable(plan) {
  const n = x => x == null ? "-" : String(x);
  let s = "<table><tr><th>hour (UTC)</th><th>rate mean</th><th>rate p95</th><th>needed P</th><th>needed D</th><th>had P</th><th>had D</th><th></th></tr>";
  for (const h of plan.hours) {
    s += `<tr><td>${esc(h.start.slice(0, 16).replace("T", " "))}</td><td>${h.rate ? dayFmt(h.rate.mean) : "-"}</td><td>${h.rate ? dayFmt(h.rate.p95) : "-"}</td>` +
      `<td>${n(h.needed && h.needed.prefill)}</td><td>${n(h.needed && h.needed.decode)}</td><td>${n(h.actual && h.actual.prefill)}</td><td>${n(h.actual && h.actual.decode)}</td>` +
      `<td>${esc(h.verdict ? DAY_VERDICT[h.verdict] : "not sized")}</td></tr>`;
  }
  return s + "</table>";
}

// daySummaryText is the summary in a few lines.
function daySummaryText(plan) {
  const s = plan.summary, c = s.counts, out = [];
  const hours = hs => hs.length ? hs.map(h => h.slice(11, 16)).join(", ") : "none";
  if (s.peakNeeded) out.push(`Needed at peak ${s.peakNeeded.prefill} prefill and ${s.peakNeeded.decode} decode; at least ${s.minNeeded.prefill} and ${s.minNeeded.decode}.`);
  if (s.neededGpuHours != null) out.push(`GPU-hours needed ${s.neededGpuHours.toFixed(1)}, used ${s.actualGpuHours.toFixed(1)}.`);
  else out.push(`Replica-hours needed ${(s.neededReplicaHours.prefill + s.neededReplicaHours.decode).toFixed(1)}, used ${(s.actualReplicaHours.prefill + s.actualReplicaHours.decode).toFixed(1)}.`);
  out.push(`Hours under (SLO risk): ${hours(s.underHours)}. Hours over (waste): ${hours(s.overHours)}.`);
  out.push(`${c.sized} buckets sized, ${c.unmet} with no fleet within the bound, ${c.tooLittleData} with too little data, ${c.noData} with no data.`);
  return out;
}
