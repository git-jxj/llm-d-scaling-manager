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
// and the keys with a fixed set of values.
const LINK_NUMBERS = new Set(["cycle", "prefill", "decode", "rate", "ttft", "tpot", "ratio", "headroom", "out", "max",
  "vp", "a", "promptCV", "routerHop"]);
const LINK_NAMES = new Set(["recording", "namespace", "model", "prefillVariant", "decodeVariant", "priceAs"]);
const STAT_WORD = /^(mean|p[0-9]{1,2}(\.[0-9]{1,3})?)$/;
const LINK_WORDS = {mode: /^size$/, low: /^1$/, perBucket: /^1$/, ttftStat: STAT_WORD, tpotStat: STAT_WORD};

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

// delta is b less a as text: signed, in fmt's units, or "same" where the two
// print the same; empty where either is refused. cls is "up" or "down".
function delta(a, b, fmt) {
  if (a == null || b == null) return {text: "", cls: ""};
  if (fmt(a) === fmt(b)) return {text: "same", cls: ""};
  const d = b - a;
  return {text: (d > 0 ? "+" : "-") + fmt(Math.abs(d)), cls: d > 0 ? "up" : "down"};
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
