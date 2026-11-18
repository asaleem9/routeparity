const $ = (id) => document.getElementById(id);
const labels = {"envoy-gateway": "Envoy Gateway", "nginx-gateway-fabric": "NGINX Gateway Fabric", istio: "Istio"};
const state = {view: "literal", runs: [], run: null, data: [], selection: null, request: 0};
const el = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};
const date = (value) => new Intl.DateTimeFormat("en", {dateStyle: "medium", timeStyle: "short", timeZone: "UTC"}).format(new Date(value)) + " UTC";
const reports = () => state.data.map((artifact) => artifact.views[state.view]);
const coverage = (report, id) => report.coverage.find((c) => c.ruleID === id) || {ruleID: id, status: "not-exercised", probes: 0, diffs: 0};
const limitations = (report) => [...new Map((report.unverified || []).map((u) => [JSON.stringify(u), u])).values()];
const statusText = (status) => ({differs: "Differs", matches: "Matches", "not-exercised": "Not exercised"})[status] || "Not exercised";
function localURL(path) {
  const url = new URL(path, window.location.href);
  if (url.origin !== window.location.origin || !["http:", "https:"].includes(url.protocol)) throw new Error("Invalid evidence link");
  return url.href;
}
function download(path, text) {
  const a = el("a", "text-link", text);
  a.href = localURL(path);
  return a;
}
function rawPath(implementation, suffix = "") {
  return state.run.artifacts + implementation + (suffix ? "-" + state.view + suffix : "") + ".json";
}
function pill(status) { return el("span", "status " + status, statusText(status)); }
function renderSummary() {
  const cards = $("summary-cards"); cards.replaceChildren();
  for (const report of reports()) {
    const id = report.candidate.implementation;
    const s = report.summary;
    const card = el("article", "summary-card");
    const title = el("div", "card-title", labels[id] || id);
    title.append(el("span", "version", report.candidate.versions[id]));
    const count = el("div", "card-count"); count.append(el("strong", "", s.probes), el("span", "", "requests checked"));
    const breakdown = el("p", "card-breakdown");
    breakdown.append(el("span", "match", `${s.match} match`), el("span", "diff", `${s.diff} differ`));
    if (s.unrouted + s.unverified + s.error) breakdown.append(el("span", "", `${s.unrouted + s.unverified + s.error} incomplete`));
    const bottom = el("div", "card-bottom");
    const unknown = limitations(report).length;
    bottom.append(el("span", unknown ? "unverified-indicator" : "", `${unknown} unverified feature${unknown === 1 ? "" : "s"}`), download(rawPath(id), "Raw evidence ↗"));
    card.append(title, count, breakdown, el("p", "card-recorded", "Recorded " + date(report.candidate.recordedAt)), bottom); cards.append(card);
  }
}
function ruleRows() {
  const rows = new Map();
  for (const report of reports()) for (const c of report.coverage) if (!rows.has(c.ruleID)) rows.set(c.ruleID, c);
  const search = $("rule-search").value.trim().toLowerCase();
  const filter = $("status-filter").value;
  return [...rows.values()].sort((a,b) => a.ruleID.localeCompare(b.ruleID)).filter((c) =>
    (!search || `${c.ruleID} ${c.title}`.toLowerCase().includes(search)) &&
    (filter === "all" || reports().some((r) => coverage(r,c.ruleID).status === filter)));
}
function renderTable() {
  const head = $("comparison-table").querySelector("thead");
  const body = $("comparison-table").querySelector("tbody");
  head.replaceChildren(); body.replaceChildren();
  const tr = el("tr"); tr.append(el("th", "", "Ingress-nginx behavior"));
  for (const r of reports()) tr.append(el("th", "", labels[r.candidate.implementation] || r.candidate.implementation));
  for (const th of tr.children) th.scope = "col";
  head.append(tr);
  const rows = ruleRows();
  $("no-results").hidden = rows.length > 0;
  if (!rows.length) state.selection = null;
  if (rows.length && (!state.selection || !rows.some((r) => r.ruleID === state.selection.rule))) {
    state.selection = {rule: rows[0].ruleID, implementation: reports()[0].candidate.implementation};
  }
  for (const row of rows) {
    const tr = el("tr");
    const name = el("td"); name.append(el("span", "rule-name", row.title || row.ruleID), el("span", "rule-id", row.ruleID)); tr.append(name);
    for (const report of reports()) {
      const id = report.candidate.implementation, c = coverage(report,row.ruleID);
      const td = el("td"), button = el("button", "result-button");
      button.type = "button";
      button.setAttribute("aria-label", `${row.ruleID}, ${labels[id] || id}: ${statusText(c.status)}. Inspect evidence.`);
      button.setAttribute("aria-pressed", String(state.selection?.rule === row.ruleID && state.selection?.implementation === id));
      button.append(pill(c.status), el("span", "cell-detail", c.status === "not-exercised" ? "no baseline hits" : c.status === "differs" ? `${c.diffs} / ${c.probes} probes differ` : `${c.probes} probes checked`));
      button.addEventListener("click", () => {state.selection = {rule: row.ruleID, implementation: id}; renderTable(); $("comparison-table").querySelector('[aria-pressed="true"]')?.focus({preventScroll: true});});
      td.append(button); tr.append(td);
    }
    body.append(tr);
  }
  renderEvidence();
}
function renderEvidence() {
  const body = $("evidence-body"); body.replaceChildren();
  if (!state.selection) { $("evidence-title").textContent = "Select a result"; $("evidence-meta").textContent = ""; return; }
  const report = reports().find((r) => r.candidate.implementation === state.selection.implementation);
  if (!report) return;
  const c = coverage(report,state.selection.rule), id = report.candidate.implementation;
  $("evidence-title").textContent = state.selection.rule;
  $("evidence-meta").textContent = `${labels[id] || id} ${report.candidate.versions[id]} · ${statusText(c.status)}`;
  const diffs = report.comparisons.filter((item) => item.verdict !== "MATCH" &&
    (item.probe.origin?.ruleID === c.ruleID || item.finding?.ruleID === c.ruleID));
  const summary = c.status === "not-exercised" ? "The baseline did not exercise this behavior. No parity conclusion is available." : diffs.length ? `${diffs.length} attributed difference${diffs.length === 1 ? "" : "s"}. Values show ingress-nginx first, then the Gateway.` : "No attributed differences for this rule. The full comparison retains every checked request.";
  body.append(el("p", "evidence-summary", summary));
  for (const item of diffs) {
    const request = el("article", "request");
    request.append(el("code", "", `${item.probe.method} ${item.probe.scheme}://${item.probe.host}${item.probe.rawTarget}`));
    for (const d of item.deltas || []) {
      const delta = el("div", "delta"); delta.append(el("span", "delta-field", d.field));
      for (const [value, className, sign] of [[d.baseline, "before", "−"], [d.candidate, "after", "+"]]) {
        const line = el("div", "delta-value " + className); line.append(el("b", "", sign), el("span", "", value || "(none)")); delta.append(line);
      }
      request.append(delta);
    }
    if (item.reason) request.append(el("p", "finding-message", item.reason));
    if (item.finding?.message) request.append(el("p", "finding-message", item.finding.message));
    body.append(request);
  }
  body.append(download(rawPath(id, ""), "Download full comparison bundle ↓"));
}
function renderLimitations() {
  const container = $("limitations-list"); container.replaceChildren(); let total = 0;
  for (const report of reports()) {
    const unknown = limitations(report), id = report.candidate.implementation; total += unknown.length;
    const details = el("details", "limitation-group"), summary = el("summary", "", `${labels[id] || id} · ${unknown.length} unverified feature${unknown.length === 1 ? "" : "s"}`);
    const ul = el("ul");
    for (const u of unknown) {
      const li = el("li"); li.append(el("span", "reason-tag", u.reason), el("strong", "", u.feature));
      if (u.object) li.append(el("p", "mono", `${u.object.kind}/${u.object.namespace || ""}/${u.object.name}`));
      if (u.detail) li.append(el("p", "", u.detail)); ul.append(li);
    }
    if (!unknown.length) ul.append(el("li", "", "No unverified features were reported for this run."));
    details.append(summary,ul); container.append(details);
  }
  $("limitations-count").textContent = `${total} reported limitations`;
}
function render() {
  for (const button of document.querySelectorAll("[data-view]")) { const active = button.dataset.view === state.view; button.setAttribute("aria-selected", String(active)); button.tabIndex = active ? 0 : -1; }
  $("results").setAttribute("aria-labelledby", `tab-${state.view}`);
  $("source-description").textContent = state.view === "literal" ? "Each Ingress path copied into an HTTPRoute, with no compensation for controller defaults." : "Ingresses translated by ingress2gateway using the standard emitter.";
  renderSummary(); renderTable(); renderLimitations();
}
async function loadRun(id) {
  const run = state.runs.find((run) => run.id === id), request = ++state.request;
  if (!run) throw new Error("Unknown measurement run");
  const response = await fetch(localURL(run.matrix));
  if (!response.ok) throw new Error("Measurement data is unavailable");
  const data = await response.json();
  if (data.schemaVersion !== 1 || !Array.isArray(data.artifacts) || data.artifacts.length !== 3) throw new Error("Incomplete measurement data");
  if (request !== state.request) return;
  state.run = run;
  state.data = data.artifacts;
  $("run-meta").textContent = `baseline: ingress-nginx ${reports()[0].baseline.versions["ingress-nginx"]} · ${state.run.commit?.slice(0,8) || "local build"}`;
  $("download-report").href = localURL(state.run.report); $("download-report").hidden = false;
  $("content").hidden = false; $("load-error").hidden = true;
  render();
}
function fail(error) { $("load-error").textContent = `${error.message}. Download the workflow artifacts or try again after the next successful measurement.`; $("load-error").hidden = false; $("content").hidden = true; }
for (const button of document.querySelectorAll("[data-view]")) button.addEventListener("click", () => {state.view = button.dataset.view; render();});
for (const button of document.querySelectorAll("[data-view]")) button.addEventListener("keydown", (event) => { if (["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) { event.preventDefault(); state.view = event.key === "Home" ? "literal" : event.key === "End" ? "converted" : state.view === "literal" ? "converted" : "literal"; render(); $(`tab-${state.view}`).focus(); } });
$("rule-search").addEventListener("input",renderTable);
$("status-filter").addEventListener("change",renderTable);
$("run-select").addEventListener("change", (event) => loadRun(event.target.value).catch(fail));
try {
  const response = await fetch("runs.json"); if (!response.ok) throw new Error("No measurement index is available");
  const index = await response.json(); if (index.schemaVersion !== 1 || !index.runs?.length) throw new Error("No complete measurements are available");
  state.runs = index.runs;
  $("run-select").replaceChildren(...state.runs.map((run,i) => {const option = el("option", "", `${date(run.recordedAt)}${i === 0 ? " · latest" : ""}`); option.value = run.id; return option;}));
  $("run-select").disabled = false;
  await loadRun(state.runs[0].id);
} catch (error) { fail(error); }
