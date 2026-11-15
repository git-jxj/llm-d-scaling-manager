// The capacity planner page's saved scenarios, as pure functions: escaping,
// the checks on a scenario read back from a file or the store, the store's
// edits, and the compare table's changes. No DOM and no browser API, so
// hack/check-planner-scenarios.sh runs them under node on fixtures. The page
// loads this file before planner.js.
//
// A scenario is the page's link, the text after the # that planner.js's
// linkParams writes: the source and the knobs. A saved scenario is that text
// under a name in the browser's localStorage; an exported one is a small JSON
// file holding the same text. A file or a store read back is untrusted.
"use strict";

// esc is s safe to put in HTML.
function esc(s) {
  return String(s).replace(/[&<>"']/g, c => ({"&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"}[c]));
}

const SCENARIO_KIND = "capacity-planner-scenario";
const SCENARIO_VERSION = 1;
const SCENARIO_STORE = "capacity-planner.scenarios";
const MAX_SCENARIOS = 50, MAX_NAME = 80, MAX_LINK = 4096, MAX_FILE = 65536;

// LINK_NUMBERS, LINK_NAMES and LINK_WORDS are every key linkParams writes:
// the numbers (the assumed terms' keys among them, ASSUMABLE in planner.js),
// the free names (a recording, a namespace, a model, a variant, a bucket),
// and the keys with a fixed set of values or a form of their own (edges).
const LINK_NUMBERS = new Set(["cycle", "prefill", "decode", "rate", "ttft", "tpot", "ratio", "headroom", "out", "max",
  "vp", "a", "promptCV", "routerHop"]);
const LINK_NAMES = new Set(["recording", "namespace", "model", "prefillVariant", "decodeVariant", "priceAs"]);
const STAT_WORD = /^(mean|p[0-9]{1,2}(\.[0-9]{1,3})?)$/;
const LINK_WORDS = {mode: /^size$/, low: /^1$/, perBucket: /^1$/, ttftStat: STAT_WORD, tpotStat: STAT_WORD,
  edges: {test: edgesInLink}};

// MAX_EDGES and MAX_EDGE are the server's limits on bucketEdges
// (internal/whatifapi/scenario.go): at most 8 edges, each at most 1024k
// tokens.
const MAX_EDGES = 8, MAX_EDGE = 1 << 20;

// bucketName is a bucket's name as the server gives it (bucketName in
// internal/whatifapi/scenario.go): its edge in k where it is a whole number
// of 1024 tokens, else in tokens. So 2048 is "2k" and 2000 is "2000".
function bucketName(tokens) {
  return tokens % 1024 === 0 ? `${tokens / 1024}k` : String(tokens);
}

// parseEdges reads bucket edges as the page's box takes them: token counts
// or Nk (N x 1024 tokens), split by commas or spaces, ascending, by the
// server's rules. It returns {edges, text}, text the canonical form the link
// carries: each edge by its bucket's name, so 2048 is "2k" and the box, the
// link and the line's label agree ("2k,16k,64k"); blank is no edges, the
// default. Anything else throws, saying which edge and why.
function parseEdges(s) {
  const words = String(s).split(/[\s,]+/).filter(w => w !== "").map(w => w.toLowerCase());
  if (words.length > MAX_EDGES) throw new Error(`${words.length} edges; give at most ${MAX_EDGES}`);
  const edges = [];
  for (const w of words) {
    if (!/^[0-9]{1,8}k?$/.test(w)) throw new Error(`${w} is not a token count such as 2048 or 16k`);
    const e = parseInt(w, 10) * (w.endsWith("k") ? 1024 : 1);
    if (e < 1 || e > MAX_EDGE) throw new Error(`${w} is outside 1 to ${MAX_EDGE / 1024}k tokens`);
    if (edges.length && e <= edges[edges.length - 1]) throw new Error(`${w} is not above the edge before it: give the edges smallest first, each once`);
    edges.push(e);
  }
  return {edges, text: edges.map(bucketName).join(",")};
}

// linkedEdges is what a link's edges= gives the page: {edges, note}. Edges
// that parse are used in their canonical form (the page then rewrites the
// link), however they were typed; edges that do not parse are not used, and
// note says so, why, and that the default buckets are priced in their place.
// The page never prices other edges than the link names in silence.
function linkedEdges(v) {
  if (!v) return {edges: "", note: null};
  try {
    return {edges: parseEdges(v).text, note: null};
  } catch (e) {
    return {edges: "", note: `The link's bucket edges "${v}" were not used (${e.message}): the default buckets are priced.`};
  }
}

// edgesInLink reports whether v is edges a link may carry: one or more that
// parseEdges reads, in any form it reads.
function edgesInLink(v) {
  try {
    return parseEdges(v).edges.length > 0;
  } catch (e) {
    return false;
  }
}

// edgesBoxProblem is what the bucket edges box says under itself: "" while
// its text reads as the edges in use (inUse, parseEdges' text, "" for the
// default), else why the lines on screen are not what it says. It stays
// until the box and the chart agree, whatever the status line says since.
function edgesBoxProblem(raw, inUse) {
  const shown = inUse ? `the lines show ${inUse.replace(/,/g, ", ")}` : "the lines show the default buckets";
  let r;
  try {
    r = parseEdges(raw);
  } catch (e) {
    return `Not used: ${e.message}; ${shown}.`;
  }
  return r.text === inUse ? "" : `Not used yet; ${shown}.`;
}

// EDGE_BEYOND is the flag a bucket line past the context window carries.
const EDGE_BEYOND = "EdgeBeyondContext";

// beyondContext reports whether a bucket line (a plan's ttft.perBucket[i])
// is flagged past the engines' context window, on either point.
function beyondContext(l) {
  const has = f => !!f && ((f.flags || []).includes(EDGE_BEYOND) || (!!f.refused && (f.refused.flags || []).includes(EDGE_BEYOND)));
  return has(l.atRate && l.atRate.figure) || has(l.atTarget && l.atTarget.rate);
}

// contextNote is what a bucket line past the window says of itself, with
// window the inputs' maxModelLen in tokens, or null where not known.
function contextNote(window) {
  return window ? `past the ${bucketName(window)}-token context window` : "past the context window";
}

// bucketSeries is the classes the n-th bucket line (from 0) is drawn with:
// one of five colours, a dash pattern from the sixth on so a repeated colour
// stays told apart, and dimmed and dotted past the context window.
function bucketSeries(n, beyond) {
  return {cls: "c-" + (n % 5), extra: (n >= 5 ? " rep" : "") + (beyond ? " beyond" : "")};
}

// edgesText is a plan's buckets as the compare table names them.
function edgesText(p) {
  const e = p && p.scenario && p.scenario.bucketEdges;
  return e && e.length ? e.map(bucketName).join(", ") : "1k, 4k, 8k, 32k, 128k (default)";
}

// CONTROL is a control character: in a name, the HTML parser would change
// it, and the name could never be opened or deleted again.
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/;

// linkProblem is what is wrong with a link read from a file or the store, or
// "" when it is one linkParams could have written.
function linkProblem(link) {
  if (typeof link !== "string") return "the link is not text";
  if (link.length > MAX_LINK) return `the link is longer than ${MAX_LINK} characters`;
  const p = new URLSearchParams(link), seen = new Set();
  for (const [key, v] of p) {
    if (seen.has(key)) return `the link names ${key} twice`;
    seen.add(key);
    if (LINK_NUMBERS.has(key)) {
      if (v === "" || !isFinite(+v) || +v < 0) return `${key} is not a number: ${v}`;
    } else if (LINK_NAMES.has(key)) {
      if (!v || v.length > 253 || CONTROL.test(v)) return `${key} is not a name`;
    } else if (Object.hasOwn(LINK_WORDS, key)) {
      if (!LINK_WORDS[key].test(v)) return `${key} cannot be ${v}`;
    } else {
      return `the page sets no ${key}`;
    }
  }
  if (!p.get("recording") && !p.get("namespace")) return "the link names no source";
  return "";
}

// nameProblem is what is wrong with a scenario's name, or "".
function nameProblem(name) {
  if (typeof name !== "string" || !name.trim() || name.length > MAX_NAME) return `a scenario's name is text of 1 to ${MAX_NAME} characters`;
  if (CONTROL.test(name)) return "a scenario's name has a control character";
  return "";
}

// checkScenario is a scenario read back, {name, link, saved}, or an error
// saying why it is not one.
function checkScenario(x) {
  if (!x || typeof x !== "object" || Array.isArray(x)) throw new Error("not a scenario");
  const bad = nameProblem(x.name) || linkProblem(x.link);
  if (bad) throw new Error(bad);
  return {name: x.name.trim(), link: x.link, saved: typeof x.saved === "string" && !CONTROL.test(x.saved) ? x.saved.slice(0, 40) : ""};
}

// checked is checkScenario's answer, or null for an entry that is not one.
function checked(x) {
  try { return checkScenario(x); } catch (e) { return null; }
}

// readScenarioFile is an exported scenario's JSON, checked.
function readScenarioFile(text) {
  if (typeof text !== "string" || text.length > MAX_FILE) throw new Error(`the file is larger than ${MAX_FILE / 1024} KiB`);
  let doc;
  try { doc = JSON.parse(text); } catch (e) { throw new Error("the file is not JSON"); }
  if (!doc || doc.kind !== SCENARIO_KIND) throw new Error(`the file is not a ${SCENARIO_KIND}`);
  if (doc.version !== SCENARIO_VERSION) throw new Error(`the file is version ${String(doc.version).slice(0, 20)}; this page reads version ${SCENARIO_VERSION}`);
  return checkScenario(doc);
}

// parseStore is the store's raw entries, every one kept: an entry this page
// cannot read may be another version's, and is written back as it was.
function parseStore(text) {
  let raw;
  try { raw = JSON.parse(text || "[]"); } catch (e) { return []; }
  return Array.isArray(raw) ? raw : [];
}

// visibleScenarios is the entries that check, the first MAX_SCENARIOS of
// them: the list the page shows.
function visibleScenarios(raw) {
  return raw.map(checked).filter(x => x).slice(0, MAX_SCENARIOS);
}

// putScenario is raw with entry saved under its name: a scenario of that
// name that checks is replaced, every other entry kept. A new name past
// MAX_SCENARIOS is an error.
function putScenario(raw, entry) {
  const sc = checkScenario(entry);
  const same = x => { const c = checked(x); return c !== null && c.name === sc.name; };
  const replaced = raw.some(same);
  if (!replaced && visibleScenarios(raw).length >= MAX_SCENARIOS) throw new Error(`this page keeps at most ${MAX_SCENARIOS} scenarios: delete one first`);
  return {raw: raw.filter(x => !same(x)).concat([{name: sc.name, link: sc.link, saved: sc.saved}]), replaced};
}

// dropScenario is raw without the scenario named; entries that do not check
// are kept.
function dropScenario(raw, name) {
  return raw.filter(x => { const c = checked(x); return c === null || c.name !== name; });
}

// freeName is name, or where a scenario in taken has it, the first of
// "name (2)", "name (3)" ... that none has, cut to MAX_NAME characters.
function freeName(name, taken) {
  const used = new Set(taken);
  if (!used.has(name)) return name;
  for (let n = 2; ; n++) {
    const suffix = ` (${n})`, candidate = name.slice(0, MAX_NAME - suffix.length) + suffix;
    if (!used.has(candidate)) return candidate;
  }
}

// cleanName is a typed or generated name made fit to save: control
// characters as spaces, trimmed, at most MAX_NAME characters.
function cleanName(s) {
  return String(s).replace(new RegExp(CONTROL.source, "g"), " ").trim().slice(0, MAX_NAME).trim();
}

// scenarioTitle is a name made of clauses, most telling first, joined by
// commas: whole clauses are dropped from the end until it fits MAX_NAME,
// never cut mid-word unless the first alone is too long.
function scenarioTitle(clauses) {
  const parts = clauses.map(cleanName).filter(Boolean);
  while (parts.length > 1 && parts.join(", ").length > MAX_NAME) parts.pop();
  return cleanName(parts.join(", "));
}

// sourceProblem is why a link's source cannot be opened against a server
// that lists sources (GET /v1/sources), or "" where it can. No source is
// substituted for one the server does not read.
function sourceProblem(link, sources) {
  if (!sources) return "the page has not listed the server's sources yet: try again in a moment";
  const p = new URLSearchParams(link), rec = p.get("recording"), ns = p.get("namespace");
  if (rec) return sources.recordings.includes(rec) ? "" : `it reads the recording ${rec}, which this server does not list`;
  if (ns) {
    if (!sources.prometheus) return `it reads namespace ${ns} from Prometheus, which this server does not read`;
    return sources.namespaces.includes(ns) ? "" : `it reads namespace ${ns}, which this server may not read`;
  }
  return "it names no source";
}

// The link's knobs are applied by one load: link is {seq, restore}, or null
// once applied or given up. A load that starts takes the link over, so a
// Load pressed before the link's load returns still applies it.
// linkAtLoadStart is the link as a load numbered seq starts.
function linkAtLoadStart(link, seq) {
  return link ? {seq, restore: link.restore} : null;
}

// linkAtLoadEnd is what a load numbered seq does with the link when it ends,
// ok or not, with currentSeq the newest load: apply the knobs, put back what
// was before (restore, for a scenario opened), or wait for the next load
// (the page's own link). A load a newer one overtook does nothing.
function linkAtLoadEnd(link, seq, currentSeq, ok) {
  if (!link || link.seq !== seq || seq !== currentSeq) return {link, apply: false, restore: null};
  if (ok) return {link: null, apply: true, restore: null};
  if (link.restore) return {link: null, apply: false, restore: link.restore};
  return {link: {seq: null, restore: null}, apply: false, restore: null};
}

// beginOpen is opening sc over the page as it stands (before: the link's
// text, the address, the source form and the name box): what to put on the
// page, and the link to hand to the load, which puts before back if it fails.
function beginOpen(before, sc) {
  return {next: {linked: sc.link, hash: "#" + sc.link, name: sc.name}, link: {seq: null, restore: before}};
}

// importPlan is what importing sc does to the store raw: a scenario saved
// under the same name with the same link is opened as it is (save false);
// another under that name makes this one "name (2)" and so on. raw itself is
// never changed: the page writes the answer once, or nothing.
function importPlan(raw, sc) {
  const list = visibleScenarios(raw);
  const same = list.find(x => x.name === sc.name);
  if (same && same.link === sc.link) return {raw, name: same.name, save: false};
  const name = freeName(sc.name, list.map(x => x.name));
  return {raw: putScenario(raw, {name, link: sc.link, saved: sc.saved}).raw, name, save: true};
}

// sameCurve reports whether two curves (points {x, y}, y null where refused)
// are the same line: then A's is drawn under the current one, unseen.
function sameCurve(a, b) {
  if (!a || !b || a.length !== b.length || !a.length) return false;
  const near = (u, v) => u === v || (u != null && v != null && Math.abs(u - v) <= 1e-9 * Math.max(1, Math.abs(u)));
  return a.every((q, i) => near(q.x, b[i].x) && near(q.y, b[i].y));
}

// figValue is a figure's value, or null where it is refused or absent.
function figValue(f) {
  return f && !f.refused && typeof f.value === "number" ? f.value : null;
}

// delta is b less a as text: signed, in fmt's units, or "same" where the two
// print the same; empty where either is missing. cls is "up" or "down". The
// change is the change of the figures as printed, so that 10.1 to 10.2 is
// +0.10, not the +0.16 the unrounded figures differ by.
function delta(a, b, fmt) {
  if (a == null || b == null) return {text: "", cls: ""};
  const ta = String(fmt(a)), tb = String(fmt(b));
  if (ta === tb) return {text: "same", cls: ""};
  const num = t => parseFloat(t.replace(/,/g, "")), unit = t => t.replace(/^[-+0-9.,]+/, "");
  const shown = isFinite(num(ta)) && isFinite(num(tb)) && unit(ta) === unit(tb);
  const d = shown ? num(tb) - num(ta) : b - a;
  return {text: (d > 0 ? "+" : "-") + fmt(Math.abs(d)), cls: d > 0 ? "up" : "down"};
}

// figDelta is delta of two figures, saying where either is refused: "both
// refused", "now refused" (A priced it), "now priced" (A refused it).
function figDelta(fa, fc, fmt) {
  const va = figValue(fa), vc = figValue(fc);
  if (!fa || !fc) return {text: "", cls: ""};
  if (va == null && vc == null) return {text: "both refused", cls: ""};
  if (va == null) return {text: "now priced", cls: ""};
  if (vc == null) return {text: "now refused", cls: ""};
  return delta(va, vc, fmt);
}

// compareRows is the compare table: A, the current plan and the change, row
// by row. f formats: rate (r/s), fleet (counts), fig (a figure in r/s, or its
// refusal), bind (what binds a plan), slo (statistic, target) and shape (what
// a plan was priced at). Every text is escaped here.
function compareRows(a, cur, f) {
  const rows = [];
  const row = (name, at, ct, d) => rows.push(`<tr><th scope="row">${esc(name)}</th><td>${esc(at)}</td><td>${esc(ct)}</td>` +
    `<td class="${d.cls ? "d-" + d.cls : ""}">${esc(d.text)}</td></tr>`);
  const same = (x, y) => ({text: x === y ? "same" : "changed", cls: ""});
  row("Fleet", f.fleet(a.scenario.fleet), f.fleet(cur.scenario.fleet), {text: fleetDelta(a.scenario.fleet, cur.scenario.fleet), cls: ""});
  // A higher rate set is neither better nor worse: its change is not coloured.
  const ra = a.limits.chosen.rate, rc = cur.limits.chosen.rate, rateSame = f.rate(ra) === f.rate(rc);
  row("Rate you set", f.rate(ra), f.rate(rc), Object.assign(delta(ra, rc, f.rate), {cls: ""}));
  for (const [key, name] of [["fleet", "Fleet limit"], ["prefill", "Prefill limit (TTFT)"], ["decode", "Decode limit (TPOT)"]]) {
    const xa = a.limits[key], xc = cur.limits[key];
    if (!xa && !xc) continue;
    row(name, xa ? f.fig(xa) : "none", xc ? f.fig(xc) : "none", figDelta(xa, xc, f.rate));
  }
  // Headroom at another rate is not better or worse: coloured only where the
  // rate is the same.
  const hd = figDelta(a.limits.chosen.headroom, cur.limits.chosen.headroom, f.rate);
  row("Headroom at the rate", f.fig(a.limits.chosen.headroom), f.fig(cur.limits.chosen.headroom), rateSame ? hd : Object.assign(hd, {cls: ""}));
  const ba = f.bind(a), bc = f.bind(cur);
  row("Binds", ba, bc, same(ba, bc));
  const ta = f.slo(a.ttft.statistic, a.ttft.line.target), tc = f.slo(cur.ttft.statistic, cur.ttft.line.target);
  row("TTFT target", ta, tc, same(ta, tc));
  if (a.tpot || cur.tpot) {
    const pa = a.tpot ? f.slo(a.tpot.statistic, a.tpot.target) : "none", pc = cur.tpot ? f.slo(cur.tpot.statistic, cur.tpot.target) : "none";
    row("TPOT target", pa, pc, same(pa, pc));
  }
  const sa = f.shape(a), sc = f.shape(cur);
  row("Priced at", sa, sc, same(sa, sc));
  // The buckets, only where A's and the current plan's edges differ: the
  // per-bucket lines are drawn for the current plan alone.
  const ea = edgesText(a), ec = edgesText(cur);
  if (ea !== ec) row("Buckets", ea, ec, {text: "changed", cls: ""});
  return `<table><tr><th></th><th>A (dashed)</th><th>Current</th><th>Change</th></tr>${rows.join("")}</table>`;
}

// MAX_RETRIES is how many times the page asks a busy server again.
const MAX_RETRIES = 5;

// retryDelay is how long, in milliseconds, the page waits before asking again
// after a response of status with the Retry-After header retryAfter (null when
// absent), on its attempt-th retry from 0; -1 is not to ask again. Only a busy
// server's 503 is retried: it alone sets Retry-After. A 503 without it is a
// plan stopped at the server's plan budget (PlanBudgetExceeded), which would
// be priced from the start again, or a request that ended. The wait is the
// header's seconds, at least 1 and at most 30.
function retryDelay(status, retryAfter, attempt) {
  if (status !== 503 || attempt >= MAX_RETRIES) return -1;
  if (retryAfter === null || retryAfter === undefined || String(retryAfter).trim() === "") return -1;
  const s = Number(retryAfter);
  return 1000 * (Number.isFinite(s) ? Math.min(Math.max(s, 1), 30) : 1);
}

// fleetDelta is the change in replicas from a to b, a role at a time.
function fleetDelta(a, b) {
  const parts = [];
  for (const role of ["prefill", "decode"]) {
    const d = (b[role] || 0) - (a[role] || 0);
    if (d) parts.push(`${d > 0 ? "+" : ""}${d} ${role}`);
  }
  return parts.length ? parts.join(", ") : "same";
}
