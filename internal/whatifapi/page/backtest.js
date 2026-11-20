// The capacity planner page's "Check the planner" view (POST /v1/backtest),
// drawn as pure functions: the charts as SVG markup, the summary lines, the
// legend and the table view. No DOM and no browser API, so
// hack/check-planner-backtest.mjs runs them under node on a fixture. The
// page loads this file after scenario.js (esc) and dayplan.js (the time
// axis helpers) and before planner.js, which puts the markup on the page.
//
// Two panels share one time axis: TTFT, then TPOT, each in seconds. Per
// panel, the predicted p50 and p90 are step lines, the band from the p90 to
// its upper bound is a wash of the p90's hue, and the measured p50 and p90
// are dots. A step not judged is greyed and says why on hover. A predicted
// figure far above what was measured is clipped at the top of the panel
// and marked, so one saturated step does not flatten the rest.
"use strict";

// BT_PANELS is each panel: the key of its figures and its title.
const BT_PANELS = [
  {key: "ttft", title: "TTFT, seconds", h: 170},
  {key: "tpot", title: "TPOT, seconds", h: 150},
];
// BT_STATS are the statistics drawn, each with its class.
const BT_STATS = [{stat: "p50", cls: "c-0"}, {stat: "p90", cls: "c-1"}];
// BT_CLIP is how far above the highest measured figure a panel's axis
// reaches before predictions are clipped.
const BT_CLIP = 3;
// BT_EXCLUDED is a step's reason not to be judged, in words.
const BT_EXCLUDED = {noTraffic: "no traffic", noMeasurement: "no measured requests", scaling: "replicas changing",
  warmUp: "warm-up", notPriced: "not priced"};

// btPoint is the step's point at stat in key ("ttft" or "tpot"), or null.
function btPoint(step, key, stat) {
  return (step[key] || []).find(p => p.statistic === stat) || null;
}

// btLayout is where each panel sits, and the x scale from the first step's
// start to the last step's end.
function btLayout(bt) {
  const steps = bt.steps;
  const x0 = dayTime(steps[0].start), x1 = dayTime(steps[steps.length - 1].start) + bt.step;
  const pw = DAY_W - DAY_L - DAY_R;
  let y = DAY_T;
  const panels = BT_PANELS.map(p => {
    const out = {key: p.key, title: p.title, top: y + 16, h: p.h};
    y = out.top + p.h + DAY_GAP;
    return out;
  });
  return {x0, x1, pw, H: y - DAY_GAP + 22, panels, X: t => DAY_L + pw * (t - x0) / (x1 - x0)};
}

// btPanelMax is a panel's axis top: the highest measured figure drawn,
// times BT_CLIP, and no higher than the highest prediction.
function btPanelMax(bt, key) {
  let meas = 0, pred = 0;
  for (const s of bt.steps) {
    for (const {stat} of BT_STATS) {
      const p = btPoint(s, key, stat);
      if (!p) continue;
      if (p.measured != null && isFinite(p.measured)) meas = Math.max(meas, p.measured);
      if (p.predicted && p.predicted.value != null) pred = Math.max(pred, p.predicted.value);
    }
  }
  const top = meas > 0 ? Math.min(Math.max(meas, pred), meas * BT_CLIP) : pred;
  return dayNiceMax(top, false);
}

// btSummaryText is the answer in sentences: the verdict, what was not
// judged, the windows, the configuration and what was measured.
function btSummaryText(bt) {
  const out = [bt.verdict, bt.excluded];
  const at = s => s.slice(0, 16).replace("T", " ");
  out.push(`Fitted on ${bt.fit.source} from ${at(bt.fit.from)} to ${at(bt.fit.to)} UTC; tested on ${bt.test.source} from ` +
    `${at(bt.test.from)} to ${at(bt.test.to)} UTC${bt.split ? " (one source split: " + bt.split + ")" : ""}.`);
  if (bt.config.match) {
    out.push("Configuration: the same in every field both sources record.");
  } else {
    out.push("CONFIGURATION DIFFERS, priced anyway: " + bt.config.differences.map(d => `${d.field} (fit ${d.fit}, test ${d.test})`).join("; ") + ".");
  }
  out.push("Measured: " + bt.measuredNote + ".");
  for (const a of bt.assumed) out.push("Assumed: " + a + ".");
  return out;
}

// btSVG is the charts: the SVG's inner markup, its viewBox, and the layout
// the hover reads. Every series is a path or group with data-series, every
// greyed step a rect with data-excluded.
function btSVG(bt) {
  const lay = btLayout(bt), X = lay.X, step = bt.step, xr = DAY_L + lay.pw;
  const days = bt.steps.length && bt.steps[0].start.slice(0, 10) !== bt.steps[bt.steps.length - 1].start.slice(0, 10);
  let s = "";
  for (const p of lay.panels) {
    const ymax = btPanelMax(bt, p.key);
    const Y = v => p.top + p.h * (1 - Math.min(v, ymax) / ymax);
    let g = `<text class="ptitle" x="${DAY_L}" y="${p.top - 6}">${esc(p.title)}</text>`;
    for (const k of bt.steps) {
      if (!k.excluded) continue;
      const t = dayTime(k.start);
      g += `<rect class="btgrey" data-excluded="${esc(k.excluded)}" x="${X(t).toFixed(1)}" y="${p.top}" ` +
        `width="${(X(t + step) - X(t)).toFixed(1)}" height="${p.h}"/>`;
    }
    for (const v of dayYTicks(ymax, false)) {
      if (v > ymax) continue;
      g += `<line class="${v ? "gridl" : "axisl"}" x1="${DAY_L}" x2="${xr}" y1="${Y(v).toFixed(1)}" y2="${Y(v).toFixed(1)}"/>` +
        `<text x="${DAY_L - 8}" y="${(Y(v) + 4).toFixed(1)}" text-anchor="end">${btFmt(v)}</text>`;
    }
    const p90 = k => {
      const q = btPoint(k, p.key, "p90");
      return q && q.predicted.value != null && q.bound != null ? [q.predicted.value, q.bound] : null;
    };
    for (const run of dayRuns(bt.steps, p90)) {
      g += `<polygon class="btband" data-series="${p.key}-p90-bound" points="${dayStepBand(run, step, X, Y)}"/>`;
    }
    for (const {stat, cls} of BT_STATS) {
      const pred = k => { const q = btPoint(k, p.key, stat); return q && q.predicted.value != null ? q.predicted.value : null; };
      for (const run of dayRuns(bt.steps, pred)) {
        g += `<path class="line ${cls}" data-series="${p.key}-${stat}-predicted" d="${dayStep(run, step, X, Y)}"/>`;
      }
      let dots = "";
      for (const k of bt.steps) {
        const q = btPoint(k, p.key, stat);
        if (!q || q.measured == null || !isFinite(q.measured)) continue;
        dots += `<circle class="dot ${cls}" cx="${X(dayTime(k.start) + step / 2).toFixed(1)}" cy="${Y(q.measured).toFixed(1)}" r="3"/>`;
      }
      g += `<g data-series="${p.key}-${stat}-measured">${dots}</g>`;
      for (const k of bt.steps) {
        const q = btPoint(k, p.key, stat);
        if (q && q.predicted.value != null && q.predicted.value > ymax) {
          g += `<text class="btclip" data-clipped="${p.key}-${stat}" x="${X(dayTime(k.start) + step / 2).toFixed(1)}" y="${p.top + 10}" ` +
            `text-anchor="middle">&#9650;</text>`;
        }
      }
    }
    g += dayLabels(BT_STATS.map(({stat}) => {
      for (let i = bt.steps.length - 1; i >= 0; i--) {
        const q = btPoint(bt.steps[i], p.key, stat);
        if (q && q.predicted.value != null) return {y: Y(q.predicted.value), text: stat};
      }
      return null;
    }).filter(Boolean), xr + 6, p.top, p.top + p.h);
    s += `<g class="panel" data-panel="${p.key}" data-x0="${lay.x0}" data-x1="${lay.x1}">${g}</g>`;
  }
  const bottom = lay.panels[lay.panels.length - 1], yb = bottom.top + bottom.h;
  for (const t of dayXTicks(lay.x0, lay.x1)) {
    s += `<line class="gridl" x1="${X(t).toFixed(1)}" x2="${X(t).toFixed(1)}" y1="${yb}" y2="${yb + 4}"/>` +
      `<text x="${X(t).toFixed(1)}" y="${yb + 16}" text-anchor="middle">${dayClock(t, days)}</text>`;
  }
  s += `<text x="${xr}" y="${yb + 16}" text-anchor="start" dx="8">UTC</text>`;
  s += `<line class="xh" x1="0" x2="0" y1="${lay.panels[0].top}" y2="${yb}" visibility="hidden"/>`;
  return {markup: s, viewBox: `0 0 ${DAY_W} ${lay.H}`, x0: lay.x0, x1: lay.x1, L: DAY_L, pw: lay.pw, W: DAY_W};
}

// btFmt is seconds for a tick or a cell: ms under 10 s.
function btFmt(v) {
  if (v == null) return "-";
  if (!isFinite(v)) return "above the top bucket";
  return v >= 10 ? v.toFixed(1) + " s" : (v * 1000 >= 100 ? (v * 1000).toFixed(0) : Number((v * 1000).toPrecision(2))) + " ms";
}

// btLegend is the charts' legend.
function btLegend() {
  const sw = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><line class="line ${cls}" x1="1" x2="17" y1="5" y2="5"/></svg>`;
  const dot = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><circle class="dot ${cls}" cx="9" cy="5" r="3"/></svg>`;
  const box = cls => `<svg viewBox="0 0 18 10" aria-hidden="true"><rect class="${cls}" x="1" y="1" width="16" height="8"/></svg>`;
  return `<span>${sw("c-0")}p50 predicted</span><span>${dot("c-0")}p50 measured</span>` +
    `<span>${sw("c-1")}p90 predicted</span><span>${dot("c-1")}p90 measured</span>` +
    `<span>${box("btband")}p90 up to its upper bound</span><span>${box("btgrey")}not judged</span>` +
    `<span>&#9650; predicted above the axis</span>`;
}

// btStepAt is the step that holds t, seconds since the epoch, or null.
function btStepAt(bt, t) {
  for (const s of bt.steps) {
    const a = dayTime(s.start);
    if (t >= a && t < a + bt.step) return s;
  }
  return null;
}

// btTipHTML is the tooltip of one step: its inputs, why it was not judged,
// and each statistic predicted and measured.
function btTipHTML(bt, s) {
  let h = `<b>${esc(s.start.slice(11, 16))} UTC</b>: ${esc(s.rate.toFixed(2))} r/s, ${s.prefill} prefill + ${s.decode} decode`;
  if (s.kneeShare != null) h += `, ${esc((s.kneeShare * 100).toFixed(0))}% of the knee`;
  if (s.excluded) h += `<br>not judged: ${esc(BT_EXCLUDED[s.excluded] || s.excluded)}${s.detail ? " (" + esc(s.detail) + ")" : ""}`;
  for (const key of ["ttft", "tpot"]) {
    for (const p of s[key] || []) {
      const pred = p.predicted.value != null ? btFmt(p.predicted.value) : "no figure (" + (p.predicted.refused ? p.predicted.refused.reason : "-") + ")";
      h += `<br>${key.toUpperCase()} ${esc(p.statistic)}: predicted ${esc(pred)}, measured ${esc(btFmt(p.measured))}`;
    }
  }
  return h;
}

// btScoreTable is the scores as a table: each statistic over every judged
// step, and over those inside the validated region.
function btScoreTable(bt) {
  const pct = v => v == null ? "-" : (v * 100).toFixed(0) + "%";
  const num = v => v == null ? "-" : v.toFixed(2);
  let h = `<table class="t"><thead><tr><th>figure</th><th>steps</th><th>median ratio</th><th>within 20%</th><th>median error</th>` +
    `<th>p90 error</th><th>at or under the bound</th><th>in region: steps</th><th>in region: within 20%</th></tr></thead><tbody>`;
  for (const key of ["ttft", "tpot"]) {
    for (const sc of bt.summary[key]) {
      const a = sc.all, r = sc.inRegion;
      h += `<tr><td>${key.toUpperCase()} ${esc(sc.statistic)}</td><td>${a.steps}</td><td>${num(a.bias)}</td><td>${pct(a.within)}</td>` +
        `<td>${pct(a.medianError)}</td><td>${pct(a.p90Error)}</td><td>${pct(a.coverage)}</td><td>${r.steps}</td><td>${pct(r.within)}</td></tr>`;
    }
  }
  return h + "</tbody></table>";
}
