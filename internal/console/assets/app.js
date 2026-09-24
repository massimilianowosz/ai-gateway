/* ============================================================================
   Ubiquum AI Gateway — appliance console

   One page, seven views, no build step. The traffic view is the only one that
   keeps a connection open: it subscribes to /v1/traffic/stream and patches its
   rows in place as the capture pipeline progresses.
   ========================================================================== */

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const esc = (v) =>
  String(v ?? "").replace(/[&<>'"]/g, (c) =>
    ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", "'": "&#39;", '"': "&quot;" }[c]));

const money = (v) =>
  new Intl.NumberFormat(undefined, { style: "currency", currency: "USD", maximumFractionDigits: 4 })
    .format(Number(v || 0));

const num = (v) => new Intl.NumberFormat().format(Number(v || 0));

const compact = (v) =>
  new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 1 })
    .format(Number(v || 0));

const clock = (v) => new Date(v || Date.now()).toLocaleTimeString([], { hour12: false });
const stamp = (v) => new Date(v || Date.now()).toLocaleString();

const ms = (v) => {
  const n = Number(v || 0);
  return n >= 1000 ? `${(n / 1000).toFixed(1)}s` : `${Math.round(n)}ms`;
};

// Derived session ids are 32 hex characters and unreadable in full; a client
// that named its own session usually fits, so only the long ones are cut.
// A session id is a UUID or a hash: unreadable, and two of them look alike
// whichever agent produced them. Naming the agent first makes a row something
// an operator can say out loud, and the short id keeps it unique. The full id
// stays in the row's tooltip for anyone who needs to correlate it.
const sessionLabel = (s) => {
  const agent = String(s.agent || "session").toLowerCase().replace(/[^a-z0-9]+/g, "-");
  const id = String(s.session_id || "");
  return `${agent}-${id.slice(0, 8)}`;
};

const listField = (v) =>
  String(v || "").split(",").map((x) => x.trim()).filter(Boolean);

let csrf = "";
let currentPage = "overview";

const state = {
  keys: [],
  spend: [],
  connections: [],
  models: [],
  sessions: [],
  events: [],
  // Alias -> { provider_model, provider, provider_type }. Traffic records the
  // alias a client asked for; this is what it actually was.
  modelCatalog: {},
  // Label -> pattern, so a watchlist finding can show which part of the
  // captured value actually matched.
  watchPatterns: {},
  // Turn ids already counted, so a capture message followed by its enriched
  // twin does not increment a provisional session twice.
  seenTurns: new Set(),
};

/* --- Transport ------------------------------------------------------------ */

async function api(url, options = {}) {
  // expectAuthError belongs to the caller, not to fetch: on the login request a
  // 401 means the password was wrong, and routing it through the
  // session-expired handler below would report the wrong cause.
  const { expectAuthError = false, ...init } = options;

  init.headers = { "Content-Type": "application/json", ...(init.headers || {}) };
  if (!["GET", "HEAD"].includes(init.method || "GET") && csrf) {
    init.headers["X-Ubiquum-CSRF"] = csrf;
  }
  const response = await fetch(url, init);
  if (response.status === 401 && !expectAuthError) {
    showLogin();
    throw new Error("Session expired");
  }
  if (!response.ok) {
    let body = {};
    try { body = await response.json(); } catch { /* non-JSON error page */ }
    throw new Error(body.error || `Request failed (${response.status})`);
  }
  return response.status === 204 ? null : response.json();
}

const safe = async (path, fallback) => {
  try { return await api(path); } catch { return fallback; }
};

/* --- Session -------------------------------------------------------------- */

function showLogin() {
  closeTrafficStream();
  $("#app").classList.add("hidden");
  $("#login").classList.remove("hidden");
}

function showApp() {
  $("#login").classList.add("hidden");
  $("#app").classList.remove("hidden");
}

// The brand sprite is a separate file because it is large and rarely changes.
// It is injected rather than referenced as `brands.svg#id`, because external
// <use> does not inherit currentColor consistently across browsers.
async function loadBrandSprite() {
  if ($("#brand-sprite")) return;
  try {
    const svg = await (await fetch("brands.svg")).text();
    const host = document.createElement("div");
    host.id = "brand-sprite";
    host.className = "icon-sprite";
    host.innerHTML = svg;
    document.body.prepend(host);
  } catch { /* icons degrade to the neutral fallback mark */ }
}

async function bootstrap() {
  try {
    const session = await api("/console/api/session", { expectAuthError: true });
    csrf = session.csrf;
    showApp();
    await Promise.all([loadHealth(), loadBrandSprite()]);
    navigate(location.hash.slice(1) || "overview");
  } catch {
    showLogin();
  }
}

async function loadHealth() {
  const pill = $(".health-pill");
  try {
    const response = await fetch("/health");
    const health = await response.json();
    const ok = response.ok && health.status === "ok";
    $("#health-text").textContent = ok ? "Gateway online" : "Gateway unavailable";
    pill.classList.toggle("offline", !ok);
  } catch {
    $("#health-text").textContent = "Gateway unavailable";
    pill.classList.add("offline");
  }
}

async function loadData() {
  const [keys, spend, connections, models, watchlist, catalog] = await Promise.all([
    safe("/v1/key/list", {}),
    safe("/v1/spend/records?page_size=100", {}),
    safe("/console/api/connections", {}),
    safe("/console/api/models", {}),
    safe("/v1/watchlist", { data: [] }),
    safe("/console/api/model-catalog", { data: [] }),
  ]);
  state.keys = keys.keys || keys.data || [];
  state.spend = spend.data || spend.records || spend.spend_logs || [];
  state.connections = connections.connections || [];
  state.models = models.data || models.models || [];
  state.watchPatterns = Object.fromEntries(
    (watchlist.data || []).map((t) => [t.label, t.pattern]));
  state.modelCatalog = Object.fromEntries(
    (catalog.data || []).map((m) => [m.name, m]));
}

/* --- Router --------------------------------------------------------------- */

const PAGES = {
  overview: { title: "Dashboard", render: renderOverview },
  traffic: { title: "AI traffic", render: renderTraffic },
  connections: { title: "AI connections", render: renderConnections },
  agents: { title: "Agents & applications", render: renderAgents },
  policies: { title: "Policies & budgets", render: renderPolicies },
  watchlist: { title: "Sensitive data", render: renderWatchlist },
  system: { title: "System", render: renderSystem },
};

async function navigate(page) {
  // The request ledger became the Requests view of AI traffic; old links and
  // bookmarks still land on it.
  if (page === "activity") {
    trafficView = "requests";
    page = "traffic";
  }
  const view = PAGES[page] ? page : "overview";
  currentPage = view;

  // The page lives in the URL so a refresh comes back to it. Assigning the
  // hash fires hashchange, which would navigate a second time, so it is only
  // written when it actually differs.
  if (location.hash.slice(1) !== view) location.hash = view;

  $$("nav button").forEach((b) => b.classList.toggle("active", b.dataset.page === view));
  $("#page-title").textContent = PAGES[view].title;
  $("#topbar-slot").innerHTML = "";
  $(".sidebar").classList.remove("open");
  $("#nav-scrim").classList.remove("open");
  $("#content").innerHTML = `<div class="card empty">Loading appliance data…</div>`;

  if (view !== "traffic") closeTrafficStream();

  await loadData();
  await PAGES[view].render();
  bindGo();
}

function bindGo() {
  $$("[data-go]").forEach((b) => (b.onclick = () => navigate(b.dataset.go)));
}

/* --- Shared fragments ----------------------------------------------------- */

const statusBadge = (status, message) => {
  const code = Number(status || 0);
  const title = message ? ` title="${esc(message)}"` : "";
  if (!code) return `<span class="badge neutral">—</span>`;
  if (code >= 500) return `<span class="badge danger"${title}>${code}</span>`;
  if (code >= 400) return `<span class="badge warning"${title}>${code}</span>`;
  // A stream can fail after its 200 was sent; the message is the only sign.
  if (message) return `<span class="badge danger"${title}>${code} !</span>`;
  return `<span class="badge">${code}</span>`;
};

const emptyState = (title, hint) =>
  `<div class="empty"><strong>${esc(title)}</strong>${hint ? esc(hint) : ""}</div>`;

const totalSpend = () => state.spend.reduce((sum, r) => sum + Number(r.cost ?? r.total_cost ?? 0), 0);
const totalTokens = () => state.spend.reduce((sum, r) => sum + Number(r.total_tokens || 0), 0);

// A total of zero is ambiguous when most calls were flat-billed, so the note
// says how many were left out rather than letting the figure read as "nothing
// was spent" or as a recording failure.
const flatNote = () => {
  const flat = state.spend.filter((r) => r.billing_mode === "flat").length;
  return flat
    ? `Recorded by providers · ${num(flat)} flat-billed call${flat === 1 ? "" : "s"} excluded`
    : "Recorded by providers";
};

/* --- Dashboard ------------------------------------------------------------ */

// Proportional sizes are snapped to one of the 5% step classes: the console is
// served under style-src 'self', which rejects a style attribute and the CSSOM
// write that would produce one alike.
const sizeClass = (axis, percent) => {
  const step = Math.min(100, Math.max(5, Math.round(Number(percent || 0) / 5) * 5));
  return `${axis}-${step}`;
};

function renderOverview() {
  const failed = state.spend.filter((r) => Number(r.status) >= 400).length;
  const active = state.keys.filter((k) => k.active !== false).length;

  $("#content").innerHTML = `
    <div class="page-intro">
      <div>
        <p>Usage and configuration reported by this appliance.</p>
      </div>
      <span class="badge neutral" id="overview-scope">Latest ${num(state.spend.length)} records</span>
    </div>

    <div class="stack">
      <div class="grid stats" id="overview-stats">
        <div class="stat"><span>Requests</span><strong>${num(state.spend.length)}</strong><small>Loaded ledger records</small></div>
        <div class="stat ok"><span>Token volume</span><strong>${compact(totalTokens())}</strong><small>Input + output</small></div>
        <div class="stat"><span>Observed cost</span><strong>${money(totalSpend())}</strong><small>${flatNote()}</small></div>
        <div class="stat ${failed ? "error" : "ok"}"><span>Failed</span><strong>${num(failed)}</strong><small>HTTP 400 or higher</small></div>
      </div>

      <div class="grid split-2-1">
        <div class="card">
          <div class="card-head">
            <div><h2>Token volume over time</h2><p>Agent traffic, last 24 hours</p></div>
            <span class="muted sub">${Math.round(ANALYTICS_RANGE.bucket / 60e3)}m buckets</span>
          </div>
          <div id="volume-chart">${emptyState("Loading agent traffic…")}</div>
        </div>
        <div class="card">
          <div class="card-head"><h2>Configuration</h2></div>
          <div class="metric-list">
            <div class="metric-row"><span>Identities<small>Enabled / total</small></span><strong>${num(active)} / ${num(state.keys.length)}</strong></div>
            <div class="metric-row"><span>Connections<small>Visible BYOK providers</small></span><strong>${num(state.connections.length)}</strong></div>
            <div class="metric-row"><span>Runtime models</span><strong>${num(state.models.length)}</strong></div>
            <div class="metric-row"><span>Budgets configured</span><strong>${num(state.keys.filter((k) => Number(k.budget) > 0).length)}</strong></div>
            <div class="metric-row"><span>Model allowlists</span><strong>${num(state.keys.filter((k) => (k.models || []).length > 0).length)}</strong></div>
          </div>
        </div>
      </div>

      <div class="grid thirds" id="overview-insights"></div>
    </div>`;

  renderVolumeChart();
}

// The series comes from hivetrace rather than the spend ledger, so it is
// fetched after the page is on screen: the rest of the dashboard should not
// wait on it, and an appliance with hivetrace off says so in place of the
// chart instead of failing the whole page.
async function renderVolumeChart() {
  const slot = $("#volume-chart");
  if (!slot) return;

  const since = new Date(Date.now() - ANALYTICS_RANGE.ms).toISOString();
  const [events, sessions] = await Promise.all([
    safe(`/v1/traffic/events?page_size=1000&since=${encodeURIComponent(since)}`, null),
    safe(`/v1/traffic/sessions?page_size=500&since=${encodeURIComponent(since)}`, null),
  ]);
  if (!slot.isConnected) return;

  if (!events) {
    slot.innerHTML = emptyState("AI traffic observability is disabled",
      "Set hivetrace.enabled: true in gateway.yaml to chart agent traffic here.");
    return;
  }

  state.events = events.data || [];
  slot.innerHTML = timeSeriesChart(ANALYTICS_RANGE);
  renderOverviewInsights(state.events, (sessions && sessions.data) || []);
  paintBars();
}

// The dashboard reads the same day of traffic as its chart. The spend ledger
// figures it starts with are a capped slice of records, not a period, so they
// are replaced as soon as the traffic arrives.
function renderOverviewInsights(events, sessions = []) {
  const stats = $("#overview-stats");
  const insights = $("#overview-insights");
  if (!stats || !insights) return;

  const sum = summarise(events);
  const failed = events.filter((e) => Number(e.status) >= 400);
  const cached = events.reduce((a, e) => a + Number(e.cached_prompt_tokens || 0), 0);
  const flat = events.filter((e) => e.billing_mode === "flat").length;
  // One agent per session, or a session whose first turns carried no
  // User-Agent is counted twice and the card adds up to more sessions than
  // there are. An identified agent wins over "Unidentified".
  const unknown = agentName(null, null);
  const bySession = new Map();
  for (const e of events) {
    const name = agentName(e.agent, e.agent_via);
    const seen = bySession.get(e.session_id);
    if (!seen || (seen.name === unknown && name !== unknown)) {
      bySession.set(e.session_id, { name, agent: e.agent, via: e.agent_via });
    }
  }
  const agents = new Map();
  for (const a of bySession.values()) {
    if (!agents.has(a.name)) agents.set(a.name, { sessions: 0, agent: a.agent, via: a.via });
    agents.get(a.name).sessions++;
  }

  const failures = new Map();
  for (const e of failed) {
    const who = providerName(e.model) || e.provider || "gateway";
    const key = `${failureKind(Number(e.status))} \u00b7 ${who}`;
    failures.set(key, (failures.get(key) || 0) + 1);
  }
  const worstFailure = [...failures.entries()].sort((a, b) => b[1] - a[1])[0];

  const scope = $("#overview-scope");
  if (scope) scope.textContent = "Last 24 hours";

  stats.innerHTML = `
    <div class="stat"><span>Sessions</span><strong>${num(sum.sessions.size)}</strong><small>${num(agents.size)} agent${agents.size === 1 ? "" : "s"} \u00b7 last 24 hours</small></div>
    <div class="stat"><span>Requests</span><strong>${num(sum.turns)}</strong><small>${num(sum.toolCalls)} tool calls</small></div>
    <div class="stat ok"><span>Token volume</span><strong>${compact(sum.inputTokens + sum.outputTokens)}</strong><small>${
      sum.inputTokens && cached ? `${Math.round((100 * cached) / sum.inputTokens)}% of input served from cache` : "Input + output"}</small></div>
    <div class="stat"><span>Observed cost</span><strong>${money(sum.cost)}</strong><small>${
      flat ? `${num(flat)} flat-billed call${flat === 1 ? "" : "s"} not charged` : "Recorded by providers"}</small></div>
    <div class="stat ${failed.length ? "error" : "ok"}"><span>Failed</span><strong>${num(failed.length)}</strong><small>${
      worstFailure ? `Mostly ${esc(worstFailure[0].charAt(0).toLowerCase() + worstFailure[0].slice(1))}` : "No failed calls"}</small></div>`;

  const agentRows = [...agents.entries()]
    .map(([label, a]) => ({ label, value: a.sessions, mark: agentMark(a.agent, a.via) }))
    .sort((a, b) => b.value - a.value)
    .slice(0, 6);

  const findingTotals = new Map();
  const findingKind = new Map();
  for (const e of events) {
    for (const f of e.findings || []) {
      const label = findingLabel(f);
      findingTotals.set(label, (findingTotals.get(label) || 0) + Number(f.occurrences || 0));
      findingKind.set(label, f.kind);
    }
  }
  const tone = { secret: "danger", pii: "warning", watchlist: "info" };
  const findingRows = topRows(findingTotals, 5,
    (label) => `<i class="sev-dot ${tone[findingKind.get(label)] || "info"}"></i>`);

  insights.innerHTML = `
    <div class="card">${breakdownCard("Sessions by agent", agentRows)}</div>
    <div class="card">${breakdownCard("Sensitive data", findingRows)}</div>
    <div class="card">${failures.size
      ? breakdownCard("Failed requests", topRows(failures, 5))
      : `<div class="card-head"><div><h2>Failed requests</h2></div><strong>0</strong></div>${emptyState("No failed calls in the last 24 hours")}`}</div>
    <div class="card">${modelFitCard(sessions)}</div>`;

  $$("[data-open-session]", insights).forEach((li) => (li.onclick = () => openSession(li.dataset.openSession)));
}

// Sessions whose model tier did not match the measured difficulty, worst
// mismatch first: the hardest undersized, then the easiest oversized.
function modelFitCard(sessions) {
  const misfit = sessions.filter((s) => MODEL_FIT[s.model_fit])
    .sort((a, b) => (a.model_fit === b.model_fit ? 0 : a.model_fit === "undersized" ? -1 : 1)
      || (a.model_fit === "undersized" ? b.difficulty - a.difficulty : a.difficulty - b.difficulty));
  const over = misfit.filter((s) => s.model_fit === "oversized").length;
  const under = misfit.length - over;
  return `
    <div class="card-head"><div><h2>Model fit</h2><p>${num(over)} oversized \u00b7 ${num(under)} undersized</p></div><strong>${num(misfit.length)}</strong></div>
    ${misfit.length ? `<ul class="breakdown fit-list">${misfit.slice(0, 6).map((s) => `
      <li class="clickable" data-open-session="${esc(s.session_id)}" title="${esc(MODEL_FIT[s.model_fit][1])}">
        <span class="truncate">${agentMark(s.agent, s.agent_via)}${esc(modelName(s.primary_model))}</span>
        <span class="fit-tag ${esc(s.model_fit)}">${s.model_fit === "oversized" ? "Oversized" : "Undersized"}</span>
        <b title="Difficulty">${num(s.difficulty)}/5</b>
      </li>`).join("")}</ul>`
      : emptyState("Every model matched its task", "No top-tier model on a trivial session, no light model on a hard one.")}`;
}

/* --- Request ledger ------------------------------------------------------- */

// A flat-billed call costs the appliance nothing by policy — the caller's own
// subscription paid the provider. Printing 0,00 USD for it reads as a failure
// to record the price rather than as the answer.
function costCell(row) {
  if (row.billing_mode === "flat") {
    return `<span class="muted" title="Flat-billed: the caller's own provider subscription paid for this call, so the appliance records no charge.">flat</span>`;
  }
  const amount = Number(row.cost ?? row.total_cost ?? 0);
  // Nothing was ever priced when no provider served the call — rate limited,
  // rejected, over budget. 0,00 USD would claim it ran and happened to be
  // free. The row shape differs between a turn and a session, so both are
  // checked. A failed turn now names the provider that refused it, so the
  // status has to rule it out too.
  const served = !(Number(row.status) >= 400) && (row.provider || (row.providers || []).length);
  if (!served && !amount) {
    return `<span class="muted" title="No provider served this call, so it was never priced.">\u2014</span>`;
  }
  return money(amount);
}

// Legacy fallback: the raw spend ledger has neither tools, files, nor an
// agent, so this is only ever shown when hivetrace is off and there is
// nothing richer to draw.
function ledgerTable(items) {
  if (!items.length) return emptyState("No traffic recorded yet", "Requests appear here once the gateway proxies a call.");
  const rows = items.map((r) => `
    <tr>
      <td class="mono">${stamp(r.created_at)}</td>
      <td>${esc(r.model)}</td>
      <td class="muted">${esc(r.provider)}</td>
      <td class="num">${num(r.total_tokens)}</td>
      <td class="num">${costCell(r)}</td>
      <td>${statusBadge(r.status)}</td>
    </tr>`).join("");

  return `<div class="table-wrap"><table>
    <thead><tr><th>Time</th><th>Model</th><th>Provider</th><th class="num">Tokens</th><th class="num">Cost</th><th>Status</th></tr></thead>
    <tbody>${rows}</tbody></table></div>`;
}

// One row per call. Clicking it opens that call, selected inside its session
// so the turns around it are one scroll away.
function requestRow(e) {
  return `
    <tr class="clickable" data-session="${esc(e.session_id || "")}" data-request="${esc(e.id || "")}">
      <td class="mono">${clock(e.created_at)}</td>
      <td>
        <div class="session-name">
          ${agentMark(e.agent, e.agent_via)}
          <div class="session-lines">
            <strong class="mono">${esc(sessionLabel({ session_id: e.session_id, agent: e.agent, agent_via: e.agent_via }))}</strong>
            ${e.model ? `<span class="session-model">${esc(modelName(e.model))}${
              providerName(e.model) ? `<span class="session-provider"> \u00b7 ${esc(providerName(e.model))}</span>` : ""}</span>` : ""}
          </div>
        </div>
      </td>
      <td class="num">${Number(e.status) >= 400 ? `<span class="muted">\u2014</span>` : tokenCell(e)}</td>
      <td class="num">${costCell(e)}</td>
      <td class="num">${ms(e.ttfb_ms || e.duration_ms)}</td>
      <td>${toolCell(turnScope(e))}</td>
      <td>${fileCell(turnScope(e))}</td>
      <td>${sensitiveCell(e)}</td>
      <td>${Number(e.status) >= 400 || e.error_message
        ? statusBadge(e.status, e.error_message)
        : `<span class="muted mono">${esc(e.status || "\u2014")}</span>`}</td>
    </tr>`;
}

function visibleRequests() {
  const q = sessionQuery.trim().toLowerCase();
  return [...(state.events || [])]
    .sort((a, b) => new Date(b.created_at || 0) - new Date(a.created_at || 0))
    .filter((e) => {
      const failed = Number(e.status) >= 400 || Boolean(e.error_message);
      if (modelFilter && e.model !== modelFilter) return false;
      if (statusFilter === "error" && !failed) return false;
      if (statusFilter === "ok" && failed) return false;
      if (statusFilter === "flagged" && !(e.findings || []).length) return false;
      if (!q) return true;
      return [e.session_id, agentName(e.agent, e.agent_via), e.model, modelName(e.model), e.provider, e.error_message]
        .join(" ").toLowerCase().includes(q);
    });
}

function requestTable() {
  if (!(state.events || []).length) {
    return emptyState("No requests captured in this range", "Requests appear here once the gateway proxies a call.");
  }
  const rows = visibleRequests();
  if (!rows.length) return emptyState("No request matches these filters");
  return `<div class="table-wrap"><table class="session-list">
    <thead><tr>
      <th>Time</th><th>Session</th><th class="num">Tokens in / out</th><th class="num">Cost</th>
      <th class="num">TTFB</th><th>Tools &amp; MCP</th><th>Files</th><th>Sensitive</th><th>Status</th>
    </tr></thead>
    <tbody>${rows.map(requestRow).join("")}</tbody></table></div>`;
}

/* --- AI traffic ----------------------------------------------------------- */

let trafficStream = null;
let trafficLive = false;
let activityTimer = null;


// How long after its last turn a session still counts as active.
//
// This is a heuristic, not a fact: the gap between turns is bimodal — around
// 5s while an agent loops, minutes while it waits for a person — and the two
// overlap, so no threshold separates "still working" from "finished". 45s
// covers three quarters of in-loop pauses and keeps a stopped agent from
// claiming to run for a minute and a half, which is what made a restarted
// client show up as two live sessions at once.
const ACTIVE_WINDOW_MS = 45000;

// sessionId -> epoch ms of the last turn seen on the live stream.
const activity = new Map();

function markActive(sessionId) {
  if (sessionId) activity.set(sessionId, Date.now());
}

function isActive(sessionId) {
  const last = activity.get(sessionId);
  return Boolean(last) && Date.now() - last < ACTIVE_WINDOW_MS;
}

const activeCount = () => state.sessions.filter((s) => isActive(s.session_id)).length;

// Ranges the console offers. Each is a window ending now; the same length
// immediately before it is fetched too, because a number without a comparison
// says nothing about whether anything changed.
const RANGES = [
  { id: "1h", label: "Last 1 hour", ms: 3600e3, bucket: 5 * 60e3 },
  { id: "6h", label: "Last 6 hours", ms: 6 * 3600e3, bucket: 20 * 60e3 },
  { id: "24h", label: "Last 24 hours", ms: 24 * 3600e3, bucket: 60 * 60e3 },
  { id: "7d", label: "Last 7 days", ms: 7 * 24 * 3600e3, bucket: 6 * 3600e3 },
];

// A day, not an hour: an operator opening the console after lunch was shown
// an empty page and reasonably concluded the gateway had lost their traffic.
// The window has to be long enough that "nothing here" means it.
let rangeId = "24h";
let trafficView = "sessions";
let sessionQuery = "";
let modelFilter = "";
let statusFilter = "";

const currentRange = () => RANGES.find((r) => r.id === rangeId) || RANGES[0];

// The dashboard is not driven by the traffic page's selector, so it fixes its
// own window: a day is long enough to show a working pattern.
const ANALYTICS_RANGE = RANGES.find((r) => r.id === "24h");

async function renderTraffic() {
  const range = currentRange();
  const since = new Date(Date.now() - range.ms).toISOString();

  const [sessions, events] = await Promise.all([
    safe(`/v1/traffic/sessions?page_size=200&since=${encodeURIComponent(since)}`, null),
    safe(`/v1/traffic/events?page_size=1000&since=${encodeURIComponent(since)}`, { data: [] }),
  ]);

  // A null answer means the endpoint said 503: hivetrace is off, which is a
  // configuration state to explain rather than an error to show.
  if (sessions === null) {
    // Without hivetrace the spend ledger is the only record of calls there is.
    $("#content").innerHTML = `
      <div class="notice">
        AI traffic observability is disabled, so sessions, tool and MCP usage, file access and
        PII or secret detections are not captured. Set <code>hivetrace.enabled: true</code> in gateway.yaml to enable it.
      </div>
      <div class="card flush">
        <div class="card-head"><div><h2>Requests</h2><p>Provider, tokens, cost and status from the spend ledger</p></div></div>
        ${ledgerTable(state.spend)}
      </div>`;
    return;
  }

  state.sessions = sessions.data || [];
  state.events = (events && events.data) || [];
  state.findingSamples = Boolean(sessions.finding_samples);

  $("#topbar-slot").innerHTML = `
    <div class="topbar-tools">
      <select id="range-select" class="range-select" aria-label="Time range">
        ${RANGES.map((r) => `<option value="${r.id}" ${r.id === rangeId ? "selected" : ""}>${esc(r.label)}</option>`).join("")}
      </select>
      <span class="live-pill" id="live-pill"><span class="pulse"></span><span id="live-text">Connecting…</span></span>
    </div>`;

  $("#content").innerHTML = `
    <div class="page-intro">
      <div><p>Live view of all agent sessions, model usage, tool calls and data access.</p></div>
    </div>

    ${state.findingSamples ? `
      <div class="notice warning">
        <strong>Detected values are being stored.</strong>
        hivetrace.finding_samples is on, so the text each detector matched — secrets
        and personal data included — is kept beside every finding and shown below.
        Turn it off once the detectors have been checked; values already written stay
        until the traces expire.
      </div>` : ""}

    <div class="stack">
      <div class="grid thirds">
        <div class="card">${breakdownCard("Models used", modelBreakdown())}</div>
        <div class="card">
          <div class="card-head"><div><h2>Tool calls by type</h2></div></div>
          ${toolMixChart()}
        </div>
        <div class="card">${breakdownCard("MCP servers", serverBreakdown())}</div>
      </div>

      <div class="card flush">
        <div class="card-head">
          <div class="view-head">
            <div class="view-switch" role="tablist">
              <button type="button" role="tab" data-traffic-view="sessions" class="${trafficView === "sessions" ? "active" : ""}">Sessions <b>${num(state.sessions.length)}</b></button>
              <button type="button" role="tab" data-traffic-view="requests" class="${trafficView === "requests" ? "active" : ""}">Requests <b>${num(state.events.length)}</b></button>
            </div>
            <p id="traffic-view-hint">${trafficView === "sessions" ? "Updating live \u00b7 click a session for its turns" : "Every call, newest first \u00b7 click one to open it"}</p>
          </div>
          <div class="table-filters">
            <input id="session-search" class="filter-input" type="search" placeholder="Search\u2026" value="${esc(sessionQuery)}">
            <select id="model-filter" class="filter-input">
              <option value="">All models</option>
              ${(() => {
                const opts = modelOptions();
                const names = opts.map(modelName);
                // Two aliases of one model read identically, so the alias tells them apart.
                return opts.map((m, i) => {
                  const label = names.indexOf(names[i]) !== names.lastIndexOf(names[i]) ? `${names[i]} (${m})` : names[i];
                  return `<option value="${esc(m)}" ${m === modelFilter ? "selected" : ""}>${esc(label)}</option>`;
                }).join("");
              })()}
            </select>
            <select id="status-filter" class="filter-input">
              <option value="">All statuses</option>
              <option value="ok" ${statusFilter === "ok" ? "selected" : ""}>Successful</option>
              <option value="error" ${statusFilter === "error" ? "selected" : ""}>With errors</option>
              <option value="flagged" ${statusFilter === "flagged" ? "selected" : ""}>Sensitive data</option>
            </select>
          </div>
        </div>
        <div id="session-table">${trafficView === "sessions" ? sessionTable() : requestTable()}</div>
      </div>
    </div>`;

  paintBars();
  bindSessionRows();
  bindTrafficControls();
  openTrafficStream();
}

function bindTrafficControls() {
  const range = $("#range-select");
  if (range) range.onchange = () => { rangeId = range.value; navigate("traffic"); };

  const search = $("#session-search");
  if (search) search.oninput = () => { sessionQuery = search.value; repaintSessions(); };

  const model = $("#model-filter");
  if (model) model.onchange = () => { modelFilter = model.value; repaintSessions(); };

  const status = $("#status-filter");
  if (status) status.onchange = () => { statusFilter = status.value; repaintSessions(); };

  // Two views of the same window and filters: sessions answer "what did this
  // agent do", requests answer "which calls, across everything".
  $$("[data-traffic-view]").forEach((b) => (b.onclick = () => {
    trafficView = b.dataset.trafficView;
    $$("[data-traffic-view]").forEach((x) => x.classList.toggle("active", x === b));
    const hint = $("#traffic-view-hint");
    if (hint) hint.textContent = trafficView === "sessions"
      ? "Updating live \u00b7 click a session for its turns"
      : "Every call, newest first \u00b7 click one to open it";
    repaintSessions();
  }));
}

// Proportional widths come from classes, so every bar has to be painted after
// its markup lands rather than carrying an inline style.
function paintBars() {
  $$("[data-bar]").forEach((bar) => bar.classList.add(sizeClass("w", bar.dataset.bar)));
  $$("[data-height]").forEach((bar) => bar.classList.add(sizeClass("h", bar.dataset.height)));
}

/* --- Agent, model and MCP identity ---------------------------------------- */

// Agents get a mark and a label. The gateway decides which agent a session
// belongs to; this only says how to draw it, and falls back to a neutral mark
// rather than inventing one for an agent it has not been taught.
const AGENTS = {
  "claude-code": { label: "Claude Code", icon: "b-claude", tone: "amber" },
  codex: { label: "Codex", icon: "b-openai", tone: "slate" },
  copilot: { label: "Copilot", icon: "b-copilot", tone: "indigo" },
  cursor: { label: "Cursor", icon: "b-cursor", tone: "slate" },
  cline: { label: "Cline", icon: "a-cline", tone: "teal" },
  kilocode: { label: "Kilo Code", icon: "a-cline", tone: "teal" },
  "gemini-cli": { label: "Gemini CLI", icon: "b-gemini", tone: "blue" },
  windsurf: { label: "Windsurf", icon: "b-windsurf", tone: "teal" },
  aider: { label: "Aider", icon: "a-aider", tone: "green" },
  opencode: { label: "OpenCode", icon: "a-opencode", tone: "violet" },
  goose: { label: "Goose", icon: "a-goose", tone: "green" },
  continue: { label: "Continue", icon: "a-opencode", tone: "violet" },
  zed: { label: "Zed", icon: "b-zed", tone: "slate" },
  openhands: { label: "OpenHands", icon: "a-goose", tone: "green" },
  crush: { label: "Crush", icon: "a-terminal", tone: "violet" },
  "modelhive-cli": { label: "ModelHive CLI", icon: "a-terminal", tone: "indigo" },
  curl: { label: "curl", icon: "a-terminal", tone: "slate" },
  wget: { label: "wget", icon: "a-terminal", tone: "slate" },
  postman: { label: "Postman", icon: "b-postman", tone: "amber" },
  insomnia: { label: "Insomnia", icon: "b-insomnia", tone: "violet" },
};

// A bespoke caller is named by its runtime, because that is all the traffic
// says. Calling it "Custom (httpx)" is honest where "httpx" alone reads like a
// product and "unknown" throws away what we do know.
const RUNTIMES = {
  "openai-python": { label: "OpenAI SDK (Python)", icon: "b-python" },
  "openai-node": { label: "OpenAI SDK (Node)", icon: "b-node" },
  "anthropic-python": { label: "Anthropic SDK (Python)", icon: "b-anthropic" },
  "anthropic-go": { label: "Anthropic SDK (Go)", icon: "b-go" },
  "anthropic-sdk": { label: "Anthropic SDK", icon: "b-anthropic" },
  langchain: { label: "LangChain", icon: "b-langchain" },
  langgraph: { label: "LangGraph", icon: "b-langchain" },
  llamaindex: { label: "LlamaIndex", icon: "a-custom" },
  litellm: { label: "LiteLLM", icon: "a-custom" },
  haystack: { label: "Haystack", icon: "b-python" },
  autogen: { label: "AutoGen", icon: "b-python" },
  crewai: { label: "CrewAI", icon: "b-python" },
  "python-urllib": { label: "Python urllib", icon: "b-python" },
  "python-requests": { label: "Python requests", icon: "b-python" },
  "python-httpx": { label: "Python httpx", icon: "b-python" },
  "python-aiohttp": { label: "Python aiohttp", icon: "b-python" },
  "node-fetch": { label: "Node fetch", icon: "b-node" },
  axios: { label: "axios", icon: "b-node" },
  undici: { label: "undici", icon: "b-node" },
  "go-http": { label: "Go http", icon: "b-go" },
  okhttp: { label: "OkHttp", icon: "a-custom" },
};

// Marks whose id starts with b- are real brand logos, nearly all of them in
// full colour. A tinted tile would fight those colours, so they sit on a
// neutral one; the geometric a-* and c-* glyphs keep the tone that makes an
// unbranded service recognisable.
function markClass(icon, tone) {
  return `agent-mark tone-${tone}${String(icon).startsWith("b-") ? " brand" : ""}`;
}

function agentLook(agent, via) {
  const known = AGENTS[agent];
  if (known) return known;
  if (agent && via === "library") {
    const runtime = RUNTIMES[agent];
    return {
      label: `Custom \u00b7 ${runtime ? runtime.label : agent}`,
      icon: runtime ? runtime.icon : "a-custom",
      tone: "slate",
    };
  }
  if (agent) return { label: agent, icon: "a-custom", tone: "slate" };
  return { label: "Unidentified", icon: "a-unknown", tone: "slate" };
}

// `via` is shown as a tooltip rather than a badge: an operator who cares can
// find out that the agent was inferred, and one who does not is never told a
// guess looks like a fact.
const VIA_TITLE = {
  "user-agent": "identified by the client's User-Agent",
  tools: "inferred from the toolset the client declared",
  credential: "inferred from the subscription the caller presented",
  library: "only the HTTP library is known \u2014 a bespoke caller",
};

function agentMark(agent, via) {
  const look = agentLook(agent, via);
  const title = VIA_TITLE[via] || "no identifying signal in the traffic";
  return `<span class="${markClass(look.icon, look.tone)}" title="${esc(look.label)} \u2014 ${esc(title)}">
    <svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg>
  </span>`;
}

function agentName(agent, via) {
  return agentLook(agent, via).label;
}

// Servers name themselves freely, so the same service arrives as "postgres",
// "postgresql" or "claude_ai_Google_Calendar". Each entry lists the substrings
// that identify it, checked against the name with everything but letters and
// digits stripped. The catalogue mirrors the modelhive MCP registry and the
// Claude connector directory so a server is called the same thing everywhere.
const MCP_CATALOGUE = [
  { match: ["postgres", "pg"], label: "PostgreSQL", icon: "b-postgresql", category: "database" },
  { match: ["mysql", "mariadb"], label: "MySQL", icon: "b-mysql", category: "database" },
  { match: ["sqlite"], label: "SQLite", icon: "b-sqlite", category: "database" },
  { match: ["mongodb", "mongo"], label: "MongoDB", icon: "b-mongodb", category: "database" },
  { match: ["redis"], label: "Redis", icon: "b-redis", category: "database" },
  { match: ["elasticsearch", "opensearch"], label: "Elasticsearch", icon: "b-elasticsearch", category: "database" },
  { match: ["snowflake"], label: "Snowflake", icon: "b-snowflake", category: "database" },
  { match: ["bigquery"], label: "BigQuery", icon: "b-bigquery", category: "database" },
  { match: ["supabase"], label: "Supabase", icon: "b-supabase", category: "database" },

  { match: ["github"], label: "GitHub", icon: "b-github", category: "developer_tools" },
  { match: ["gitlab"], label: "GitLab", icon: "b-gitlab", category: "developer_tools" },
  { match: ["bitbucket"], label: "Bitbucket", icon: "b-bitbucket", category: "developer_tools" },
  { match: ["docker"], label: "Docker", icon: "b-docker", category: "developer_tools" },
  { match: ["jenkins"], label: "Jenkins", icon: "b-jenkins", category: "developer_tools" },
  { match: ["terraform"], label: "Terraform", icon: "b-terraform", category: "developer_tools" },
  { match: ["sentry"], label: "Sentry", icon: "b-sentry", category: "observability" },

  { match: ["cloudflare"], label: "Cloudflare", icon: "b-cloudflare", category: "cloud_infra" },
  { match: ["googlecloud", "gcp"], label: "Google Cloud", icon: "b-googlecloud", category: "cloud_infra" },
  { match: ["aws", "amazon"], label: "AWS", icon: "b-aws", category: "cloud_infra" },
  { match: ["azure"], label: "Azure", icon: "b-azure", category: "cloud_infra" },
  { match: ["netlify"], label: "Netlify", icon: "b-netlify", category: "cloud_infra" },
  { match: ["vercel"], label: "Vercel", icon: "b-vercel", category: "cloud_infra" },

  { match: ["slack"], label: "Slack", icon: "b-slack", category: "collaboration" },
  { match: ["discord"], label: "Discord", icon: "b-discord", category: "collaboration" },
  { match: ["microsoft365", "m365", "office365"], label: "Microsoft 365", icon: "b-microsoft", category: "productivity" },
  { match: ["sharepoint"], label: "SharePoint", icon: null, category: "knowledge" },
  { match: ["onedrive"], label: "OneDrive", icon: "b-onedrive", category: "knowledge" },
  { match: ["outlook"], label: "Outlook", icon: null, category: "communication" },
  { match: ["teams"], label: "Microsoft Teams", icon: "b-teams", category: "collaboration" },
  { match: ["zoom"], label: "Zoom", icon: "b-zoom", category: "collaboration" },

  { match: ["notion"], label: "Notion", icon: "b-notion", category: "knowledge" },
  { match: ["confluence"], label: "Confluence", icon: "b-confluence", category: "knowledge" },
  { match: ["googledrive", "gdrive"], label: "Google Drive", icon: "b-googledrive", category: "knowledge" },
  { match: ["dropbox"], label: "Dropbox", icon: "b-dropbox", category: "knowledge" },
  { match: ["obsidian"], label: "Obsidian", icon: "b-obsidian", category: "knowledge" },
  { match: ["box"], label: "Box", icon: "b-box", category: "knowledge" },

  { match: ["datadog"], label: "Datadog", icon: "b-datadog", category: "observability" },
  { match: ["grafana"], label: "Grafana", icon: "b-grafana", category: "observability" },
  { match: ["pagerduty"], label: "PagerDuty", icon: "b-pagerduty", category: "observability" },
  { match: ["newrelic"], label: "New Relic", icon: "b-newrelic", category: "observability" },

  { match: ["googlecalendar", "gcal", "calendar"], label: "Google Calendar", icon: "b-googlecalendar", category: "productivity" },
  { match: ["googlesheets", "sheets"], label: "Google Sheets", icon: "b-googlesheets", category: "productivity" },
  { match: ["googledocs", "docs"], label: "Google Docs", icon: "b-googledocs", category: "productivity" },
  { match: ["airtable"], label: "Airtable", icon: "b-airtable", category: "productivity" },
  { match: ["todoist"], label: "Todoist", icon: "b-todoist", category: "productivity" },
  { match: ["zapier"], label: "Zapier", icon: "b-zapier", category: "productivity" },

  { match: ["atlassian"], label: "Atlassian", icon: "b-atlassian", category: "project_management" },
  { match: ["jira"], label: "Jira", icon: "b-jira", category: "project_management" },
  { match: ["linear"], label: "Linear", icon: "b-linear", category: "project_management" },
  { match: ["asana"], label: "Asana", icon: "b-asana", category: "project_management" },
  { match: ["trello"], label: "Trello", icon: "b-trello", category: "project_management" },
  { match: ["clickup"], label: "ClickUp", icon: "b-clickup", category: "project_management" },
  { match: ["monday"], label: "monday.com", icon: "b-monday", category: "project_management" },

  { match: ["salesforce"], label: "Salesforce", icon: "b-salesforce", category: "crm" },
  { match: ["hubspot"], label: "HubSpot", icon: "b-hubspot", category: "crm" },
  { match: ["pipedrive"], label: "Pipedrive", icon: null, category: "crm" },
  { match: ["zendesk"], label: "Zendesk", icon: "b-zendesk", category: "support" },
  { match: ["intercom"], label: "Intercom", icon: "b-intercom", category: "support" },
  { match: ["freshdesk"], label: "Freshdesk", icon: null, category: "support" },

  { match: ["stripe"], label: "Stripe", icon: "b-stripe", category: "finance" },
  { match: ["quickbooks"], label: "QuickBooks", icon: "b-quickbooks", category: "finance" },
  { match: ["xero"], label: "Xero", icon: "b-xero", category: "finance" },
  { match: ["plaid"], label: "Plaid", icon: null, category: "finance" },
  { match: ["shopify"], label: "Shopify", icon: "b-shopify", category: "finance" },
  { match: ["freshbooks"], label: "FreshBooks", icon: null, category: "finance" },

  { match: ["bravesearch", "brave"], label: "Brave Search", icon: "b-brave", category: "search" },
  { match: ["algolia"], label: "Algolia", icon: "b-algolia", category: "search" },
  { match: ["mailchimp"], label: "Mailchimp", icon: "b-mailchimp", category: "marketing" },
  { match: ["googleanalytics"], label: "Google Analytics", icon: "b-googleanalytics", category: "marketing" },
  { match: ["segment"], label: "Segment", icon: "b-segment", category: "marketing" },
  { match: ["figma"], label: "Figma", icon: "b-figma", category: "design" },
  { match: ["canva"], label: "Canva", icon: null, category: "design" },
  { match: ["miro"], label: "Miro", icon: "b-miro", category: "design" },
  { match: ["lucid"], label: "Lucid", icon: "b-lucid", category: "design" },
  { match: ["adobe"], label: "Adobe", icon: null, category: "design" },

  { match: ["gmail"], label: "Gmail", icon: "b-gmail", category: "communication" },
  { match: ["twilio"], label: "Twilio", icon: "b-twilio", category: "communication" },
  { match: ["sendgrid"], label: "SendGrid", icon: "b-sendgrid", category: "communication" },
  { match: ["spotify"], label: "Spotify", icon: "b-spotify", category: "communication" },

  { match: ["claudeinchrome", "chrome"], label: "Claude in Chrome", icon: "b-chrome", category: "browser_automation" },
  { match: ["playwright"], label: "Playwright", icon: "b-playwright", category: "browser_automation" },
  { match: ["puppeteer"], label: "Puppeteer", icon: "b-puppeteer", category: "browser_automation" },
  { match: ["1password", "onepassword"], label: "1Password", icon: "b-1password", category: "security" },
  { match: ["vault", "hashicorp"], label: "HashiCorp Vault", icon: "b-vault", category: "security" },

  { match: ["graphql"], label: "GraphQL", icon: "b-graphql", category: "api" },
  { match: ["indeed"], label: "Indeed", icon: "b-indeed", category: "search" },
  { match: ["filesystem", "files"], label: "Filesystem", icon: null, category: "developer_tools" },
];

// Not every brand has a mark in either icon set, so the category carries its
// own glyph and tone. A server we cannot name still gets a shape that says
// what kind of thing it is.
const MCP_CATEGORIES = {
  database: { label: "Database", icon: "c-database", tone: "blue" },
  developer_tools: { label: "Developer tools", icon: "c-code", tone: "slate" },
  cloud_infra: { label: "Cloud", icon: "c-cloud", tone: "indigo" },
  collaboration: { label: "Collaboration", icon: "c-chat", tone: "violet" },
  knowledge: { label: "Knowledge", icon: "c-doc", tone: "amber" },
  observability: { label: "Observability", icon: "c-pulse", tone: "green" },
  api: { label: "API", icon: "c-code", tone: "slate" },
  productivity: { label: "Productivity", icon: "c-calendar", tone: "blue" },
  project_management: { label: "Project management", icon: "c-board", tone: "violet" },
  crm: { label: "CRM", icon: "c-people", tone: "teal" },
  support: { label: "Support", icon: "c-lifebuoy", tone: "teal" },
  finance: { label: "Finance", icon: "c-card", tone: "green" },
  search: { label: "Search", icon: "c-search", tone: "amber" },
  marketing: { label: "Marketing", icon: "c-megaphone", tone: "amber" },
  design: { label: "Design", icon: "c-pen", tone: "violet" },
  communication: { label: "Communication", icon: "c-mail", tone: "blue" },
  browser_automation: { label: "Browser automation", icon: "c-browser", tone: "slate" },
  security: { label: "Security", icon: "c-lock", tone: "green" },
  unknown: { label: "MCP server", icon: "b-mcp", tone: "slate" },
};

function mcpLook(server) {
  // Connector platforms prefix the server with their own name, so the same
  // service arrives as "Miro" from one client and "claude_ai_Miro" from
  // another. Dropping the prefix keeps them one entry and one label.
  const bare = String(server || "").replace(/^(claude[_.-]?ai|mcp|server)[_.-]+/i, "");
  const key = bare.toLowerCase().replace(/[^a-z0-9]/g, "");
  const hit = MCP_CATALOGUE.find((entry) => entry.match.some((m) => key.includes(m)));
  const category = MCP_CATEGORIES[hit ? hit.category : "unknown"];
  return {
    label: hit ? hit.label : bare || server,
    icon: hit && hit.icon ? hit.icon : category.icon,
    tone: category.tone,
    category: hit ? category.label : "Unrecognised server",
  };
}

function mcpMark(server) {
  const look = mcpLook(server);
  return `<span class="${markClass(look.icon, look.tone)}" title="${esc(look.label)} \u2014 ${esc(look.category)}">
    <svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg>
  </span>`;
}

// A deployment name says nothing about who built the model, and the same
// family arrives under many names, so the vendor is read off the name.
const MODEL_VENDORS = [
  { match: ["claude", "sonnet", "opus", "haiku", "anthropic"], label: "Anthropic", icon: "b-anthropic", tone: "amber" },
  { match: ["gemini", "bison", "gecko", "palm"], label: "Google", icon: "b-gemini", tone: "blue" },
  { match: ["deepseek"], label: "DeepSeek", icon: "b-deepseek", tone: "blue" },
  { match: ["qwen", "glm"], label: "Qwen", icon: "b-qwen", tone: "violet" },
  { match: ["mistral", "mixtral", "codestral", "magistral"], label: "Mistral", icon: "b-mistral", tone: "amber" },
  { match: ["llama"], label: "Meta", icon: "b-meta", tone: "blue" },
  { match: ["kimi", "moonshot"], label: "Moonshot", icon: "b-moonshot", tone: "slate" },
  { match: ["sonar", "perplexity"], label: "Perplexity", icon: "b-perplexity", tone: "teal" },
  { match: ["codex", "gpt", "davinci", "o1-", "o3-", "o4-", "chatgpt", "openai"], label: "OpenAI", icon: "b-openai", tone: "green" },
];

function modelLook(model) {
  const key = String(model || "").toLowerCase();
  const hit = MODEL_VENDORS.find((v) => v.match.some((m) => key.includes(m)));
  if (!hit) return { label: null, icon: "c-model", tone: "slate" };
  return hit;
}

// Words a plain capitalisation gets wrong.
const MODEL_WORDS = { gpt: "GPT", glm: "GLM", deepseek: "DeepSeek", openai: "OpenAI", qwen: "Qwen", llama: "Llama" };

// modelName turns an alias into what people call the model. The alias is what
// a client asked for, often wrapped in a routing prefix — claude-code-sonnet-5
// — so the name comes from the provider model underneath: claude-sonnet-5,
// read as Claude Sonnet 5. A version split across tokens (haiku-4-5) is
// rejoined, and a trailing date snapshot is dropped.
function modelName(alias) {
  if (!alias) return "";
  const raw = state.modelCatalog[alias]?.provider_model || alias;
  // OpenAI's reasoning line is written lowercase: o3-mini, not O3 Mini.
  if (/^o\d/.test(raw)) return raw;
  const parts = raw.split("-").filter((p) => !/^\d{8}$/.test(p));
  const out = [];
  for (const p of parts) {
    const prev = out[out.length - 1];
    if (/^\d+$/.test(p) && prev && /^\d+(\.\d+)*$/.test(prev) && p.length === 1) {
      out[out.length - 1] = `${prev}.${p}`;
    } else if (prev === "GPT" && /^\d/.test(p)) {
      out[out.length - 1] = `GPT-${p}`;
    } else {
      const word = MODEL_WORDS[p.toLowerCase()];
      out.push(word || (/^v?\d/.test(p) ? p.toUpperCase() : p.charAt(0).toUpperCase() + p.slice(1)));
    }
  }
  return out.join(" ");
}

const PROVIDER_TYPES = {
  anthropic: "Anthropic", chatgpt_codex: "ChatGPT", openai: "OpenAI", azure_openai: "Azure OpenAI",
  vertex: "Vertex AI", gemini: "Google", bedrock: "AWS Bedrock", mistral: "Mistral",
  deepseek: "DeepSeek", ollama: "Ollama", echo: "Local echo",
};

// providerName says who served a model. A BYOK connection is named by the
// operator, so its own name beats the generic "OpenAI-compatible".
function providerName(alias) {
  const entry = state.modelCatalog[alias];
  if (!entry) return "";
  const byok = entry.provider.match(/^byok-(.+)$/);
  if (byok) {
    const conn = state.connections.find((c) => c.id === byok[1]);
    if (conn) return conn.name;
  }
  return PROVIDER_TYPES[entry.provider_type] || entry.provider;
}

function modelChip(model) {
  if (!model) return `<span class="muted">\u2014</span>`;
  const look = modelLook(model);
  const via = providerName(model);
  const title = [model, via || look.label].filter(Boolean).join(" \u2014 ");
  return `<span class="model-chip" title="${esc(title)}">
    <span class="${markClass(look.icon, look.tone)}"><svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg></span>
    <span class="truncate">${esc(modelName(model))}</span>
  </span>`;
}

// highlightSample marks the part of a captured value that actually matched.
//
// A watchlist term with an outer wildcard matches the whole token it lands in,
// and that token is often a file path: without this the operator is shown a
// path and left to work out which word put it there. Only the literal parts
// of the pattern are marked, since the wildcards are by definition whatever
// happened to surround them.
//
// The value comes from captured traffic, so every slice is escaped
// individually and the markup is only ever the marks this function adds.
function highlightSample(value, pattern) {
  const literals = (pattern || "").split("*").filter(Boolean);
  if (!literals.length) return esc(value);

  const haystack = value.toLowerCase();
  const ranges = [];
  for (const literal of literals) {
    const needle = literal.toLowerCase();
    for (let at = haystack.indexOf(needle); at !== -1; at = haystack.indexOf(needle, at + needle.length)) {
      ranges.push([at, at + needle.length]);
    }
  }
  if (!ranges.length) return esc(value);
  ranges.sort((a, b) => a[0] - b[0]);

  let out = "";
  let cursor = 0;
  for (const [start, end] of ranges) {
    if (start < cursor) continue; // literals that overlap each other
    out += esc(value.slice(cursor, start));
    out += `<mark>${esc(value.slice(start, end))}</mark>`;
    cursor = end;
  }
  return out + esc(value.slice(cursor));
}

/* --- Aggregation ---------------------------------------------------------- */

// Every panel is derived from the same window of turns, so one pass produces
// all of them and the numbers cannot disagree with each other.
function summarise(events) {
  const out = {
    turns: events.length, inputTokens: 0, outputTokens: 0, cost: 0, errors: 0,
    toolCalls: 0, mcpCalls: 0, nativeCalls: 0, findings: 0, secrets: 0, pii: 0,
    reads: 0, writes: 0, searches: 0,
    models: new Map(), servers: new Map(), sessions: new Set(),
  };
  for (const e of events) {
    out.inputTokens += Number(e.prompt_tokens || 0);
    out.outputTokens += Number(e.completion_tokens || 0);
    out.cost += Number(e.cost || 0);
    if (Number(e.status) >= 400) out.errors += 1;
    if (e.session_id) out.sessions.add(e.session_id);
    if (e.model) out.models.set(e.model, (out.models.get(e.model) || 0) + 1);

    for (const t of e.tools || []) {
      out.toolCalls += 1;
      if (t.source === "mcp") {
        out.mcpCalls += 1;
        if (t.server) out.servers.set(t.server, (out.servers.get(t.server) || 0) + 1);
      } else {
        out.nativeCalls += 1;
      }
    }
    for (const f of e.files || []) {
      if (f.operation === "write") out.writes += 1;
      else if (f.operation === "search") out.searches += 1;
      else out.reads += 1;
    }
    for (const f of e.findings || []) {
      const n = Number(f.occurrences || 0);
      out.findings += n;
      if (f.kind === "secret") out.secrets += n; else out.pii += n;
    }
  }
  return out;
}

// Buckets the window for the sparklines and the time series. Empty buckets are
// kept: a gap in traffic is information, and dropping it would stretch the
// remaining points into a shape the traffic never had.
function bucketEvents(events, range) {
  const now = Date.now();
  const count = Math.max(1, Math.round(range.ms / range.bucket));
  const start = now - count * range.bucket;
  const buckets = Array.from({ length: count }, (_, i) => ({
    at: start + i * range.bucket, input: 0, output: 0, cost: 0, turns: 0,
    tools: 0, findings: 0, sessions: new Set(),
  }));
  for (const e of events) {
    const t = new Date(e.created_at || e.started_at || 0).getTime();
    const i = Math.floor((t - start) / range.bucket);
    if (i < 0 || i >= count) continue;
    const b = buckets[i];
    b.turns += 1;
    b.input += Number(e.prompt_tokens || 0);
    b.output += Number(e.completion_tokens || 0);
    b.cost += Number(e.cost || 0);
    b.tools += (e.tools || []).length;
    b.findings += (e.findings || []).reduce((a, f) => a + Number(f.occurrences || 0), 0);
    if (e.session_id) b.sessions.add(e.session_id);
  }
  return buckets;
}

/* --- Charts --------------------------------------------------------------- */

// Input and output tokens stacked per bucket. Cost is deliberately not drawn
// here: the headline card already carries the total, and on an appliance where
// most traffic is flat-billed the line is a rule along zero that reads as a
// broken chart.
function timeSeriesChart(range) {
  const buckets = bucketEvents(state.events || [], range);
  if (!buckets.some((b) => b.turns)) {
    return emptyState("No traffic in this range");
  }
  const peakTokens = Math.max(...buckets.map((b) => b.input + b.output), 1);

  return `
    <div class="chart-legend">
      <span class="key in">Input tokens</span>
      <span class="key out">Output tokens</span>
    </div>
    <div class="series">
      <div class="series-bars">
        ${buckets.map((b) => `
          <div class="series-col" title="${esc(clock(new Date(b.at).toISOString()))} · ${num(b.input)} in · ${num(b.output)} out">
            <i class="seg out" data-height="${Math.round((b.output / peakTokens) * 100)}"></i>
            <i class="seg in" data-height="${Math.round((b.input / peakTokens) * 100)}"></i>
          </div>`).join("")}
      </div>
    </div>
    <div class="chart-labels">
      <span>${esc(clock(new Date(buckets[0].at).toISOString()))}</span>
      <span>Now</span>
    </div>`;
}

// A donut split by what the call actually did. File reads and writes are
// counted from the file accesses a turn produced, not from the tool names, so
// a shell command that read a file lands in the same slice as a Read tool.
function toolMixChart() {
  const s = summarise(state.events || []);
  const slices = [
    { label: "MCP", value: s.mcpCalls, tone: "blue" },
    { label: "Native", value: s.nativeCalls, tone: "green" },
    { label: "File read", value: s.reads, tone: "violet" },
    { label: "File write", value: s.writes, tone: "amber" },
    { label: "File search", value: s.searches, tone: "teal" },
  ].filter((x) => x.value > 0);

  const total = slices.reduce((a, x) => a + x.value, 0);
  if (!total) return emptyState("No tool activity in this range");

  const r = 42;
  const circumference = 2 * Math.PI * r;
  let offset = 0;
  const rings = slices.map((x) => {
    const len = (x.value / total) * circumference;
    const ring = `<circle class="ring tone-${x.tone}" cx="60" cy="60" r="${r}"
      stroke-dasharray="${len.toFixed(2)} ${(circumference - len).toFixed(2)}"
      stroke-dashoffset="${(-offset).toFixed(2)}"/>`;
    offset += len;
    return ring;
  }).join("");

  return `
    <div class="donut-wrap">
      <svg class="donut" viewBox="0 0 120 120" aria-hidden="true">
        <circle class="ring track" cx="60" cy="60" r="${r}"/>
        ${rings}
      </svg>
      <div class="donut-centre"><strong>${num(total)}</strong><small>calls</small></div>
      <ul class="donut-legend">
        ${slices.map((x) => `
          <li><span class="swatch tone-${x.tone}"></span>${esc(x.label)}
            <b>${num(x.value)}</b><em>${Math.round((x.value / total) * 100)}%</em></li>`).join("")}
      </ul>
    </div>`;
}

/* --- Breakdowns ----------------------------------------------------------- */

function breakdownCard(title, rows) {
  const total = rows.reduce((a, r) => a + r.value, 0);
  return `
    <div class="card-head"><div><h2>${esc(title)}</h2></div><strong>${num(total)}</strong></div>
    ${rows.length ? `<ul class="breakdown">
      ${rows.map((r) => `
        <li>
          <span class="truncate" title="${esc(r.label)}">${r.mark || ""}${esc(r.label)}</span>
          <span class="meter"><i data-bar="${total ? Math.round((r.value / total) * 100) : 0}"></i></span>
          <b>${num(r.value)}</b>
        </li>`).join("")}
    </ul>` : emptyState("Nothing recorded in this range")}`;
}

function topRows(map, limit, mark, rename) {
  const rows = [...map.entries()]
    .map(([key, value]) => ({ label: rename ? rename(key) : key, value, mark: mark ? mark(key) : "" }))
    .sort((a, b) => b.value - a.value);
  if (rows.length <= limit) return rows;
  const rest = rows.slice(limit).reduce((a, r) => a + r.value, 0);
  return [...rows.slice(0, limit), { label: "other", value: rest, mark: "" }];
}

function modelBreakdown() {
  return topRows(summarise(state.events || []).models, 5, (m) => {
    const look = modelLook(m);
    return `<span class="${markClass(look.icon, look.tone)}"><svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg></span>`;
  });
}

function serverBreakdown() {
  return topRows(summarise(state.events || []).servers, 5,
    (name) => mcpMark(name), (name) => mcpLook(name).label);
}

// Created and deleted are deliberately absent: FileAccess records read, write
// and search only, so the console would have to invent the difference between
// a file that was created and one that was overwritten.
// Taken from the sessions themselves, so the filter offers exactly the models
// the table can show.
function modelOptions() {
  return [...new Set(state.sessions.flatMap((s) => s.models || []))].sort();
}

// Active sessions sort to the top, most recently heard from first, so a run
// that just started is where the eye already is. The rest fall back to when
// they last produced a turn.
function sortedSessions() {
  const recency = (s) => new Date(s.ended_at || s.started_at || 0).getTime();
  return [...state.sessions].sort((a, b) => {
    const aLive = isActive(a.session_id);
    const bLive = isActive(b.session_id);
    if (aLive !== bLive) return aLive ? -1 : 1;
    if (aLive && bLive) return activity.get(b.session_id) - activity.get(a.session_id);
    return recency(b) - recency(a);
  });
}

// The filters narrow what is already loaded rather than refetching: the range
// selector owns the query, and a search box that round-trips makes the table
// jump under the cursor on every keystroke.
function visibleSessions() {
  const q = sessionQuery.trim().toLowerCase();
  return sortedSessions().filter((s) => {
    if (modelFilter && !(s.models || []).includes(modelFilter)) return false;
    if (statusFilter === "error" && !Number(s.errors)) return false;
    if (statusFilter === "ok" && Number(s.errors)) return false;
    if (statusFilter === "flagged" && !(s.findings || []).length) return false;
    if (!q) return true;
    return [s.session_id, agentName(s.agent, s.agent_via), ...(s.models || [])]
      .join(" ").toLowerCase().includes(q);
  });
}

function sessionTable() {
  if (!state.sessions.length) {
    return emptyState("No sessions captured in this range",
      "A session appears as soon as its first turn goes through.");
  }
  const rows = visibleSessions();
  if (!rows.length) return emptyState("No session matches these filters");

  return `<div class="table-wrap"><table class="session-list">
    <thead><tr>
      <th>Session</th><th class="num">Turns</th><th class="num">Tokens in / out</th>
      <th class="num">Cost</th><th>Tools &amp; MCP</th><th>Files</th><th>Sensitive</th>
      <th class="num">Started</th><th></th>
    </tr></thead>
    <tbody>${rows.map(sessionRow).join("")}</tbody></table></div>`;
}

// The caller is named where the network can name it. A resolved name is a
// convenience, so the address it came from stays in the tooltip: a PTR record
// is set by whoever owns the reverse zone and is not evidence on its own.
function sessionRow(s) {
  const live = isActive(s.session_id);
  return `
    <tr class="clickable ${live ? "live" : ""} ${openSessionId === s.session_id ? "focused" : ""}" data-session="${esc(s.session_id)}">
      <td title="${esc(s.session_id)}${s.client_host || s.client_ip ? ` \u2014 from ${esc(s.client_host || s.client_ip)}` : ""}">
        <div class="session-name">
          <span class="dot ${live ? "dot-live" : ""}" title="${live ? "active" : "idle"}"></span>
          ${agentMark(s.agent, s.agent_via)}
          <div class="session-lines">
            <strong class="mono">${esc(sessionLabel(s))}</strong>
            ${sessionModel(s)}
          </div>
        </div>
      </td>
      <td class="num">${num(s.requests)}${s.provisional ? `<span class="muted" title="awaiting analysis">+</span>` : ""}</td>
      <td class="num">${tokenCell(s)}</td>
      <td class="num">${costCell(s)}</td>
      <td>${toolCell(s)}</td>
      <td>${fileCell(s)}</td>
      <td>${sensitiveCell(s)}</td>
      <td class="num mono">${clock(s.started_at)}</td>
      <td class="chevron-cell"><span class="chevron">\u203a</span></td>
    </tr>`;
}

const TASK_VIA = {
  files: "from the files it wrote or read",
  tools: "from the tools and MCP servers it used",
  model: "by the task classifier, from the opening request",
};

// Difficulty is measured, not judged: served turns, tool calls and files
// written. The type is only shown when that evidence names one.
function taskFacts(s) {
  const d = Number(s.difficulty || 0);
  if (!s.task_type && !d) return [];
  const type = s.task_type ? s.task_type.charAt(0).toUpperCase() + s.task_type.slice(1) : "";
  const meter = d
    ? `<span class="diff-fact" title="Difficulty ${d} of 5, measured from turns served, tool calls and files written"><span class="fact-label">Difficulty</span><span class="diff-meter" aria-label="${d} of 5">${
      [1, 2, 3, 4, 5].map((i) => `<i class="${i <= d ? "on" : ""}"></i>`).join("")}</span></span>`
    : "";
  return [
    type ? `<span title="Task type, ${esc(TASK_VIA[s.task_via] || "inferred")}"><span class="fact-label">Task</span>${esc(type)}</span>` : "",
    meter,
    s.model_tier ? `<span title="Ranked by list output price: top from $20 per million tokens, light under $8"><span class="fact-label">Model tier</span>${esc(MODEL_TIERS[s.model_tier] || s.model_tier)}</span>` : "",
    fitBadge(s),
  ];
}

const MODEL_TIERS = { top: "Top", mid: "Mid", light: "Light" };

const MODEL_FIT = {
  oversized: ["Oversized model", "A top-tier model on a difficulty-1 session: a light model would likely have done."],
  undersized: ["Undersized model", "A light model on a difficulty-4+ session: worth checking the result, or moving it up a tier."],
};

function fitBadge(s) {
  const fit = MODEL_FIT[s.model_fit];
  if (!fit) return "";
  const model = s.primary_model ? `${modelName(s.primary_model)} \u2014 ` : "";
  return `<span class="badge warning fit-badge" title="${esc(model + fit[1])}">${esc(fit[0])}</span>`;
}

// The model rides under the session name rather than in its own column: it is
// what the session ran on, not a figure to compare down the table.
function sessionModel(s) {
  const models = s.models || [];
  if (!models.length) return "";
  const more = models.length > 1 ? ` +${models.length - 1}` : "";
  const via = providerName(models[0]);
  const title = models.map((m) => `${m} \u2192 ${modelName(m)}${providerName(m) ? ` on ${providerName(m)}` : ""}`).join("\n");
  return `<span class="session-model" title="${esc(title)}">${esc(modelName(models[0]))}${more}${
    via ? `<span class="session-provider"> \u00b7 ${esc(via)}</span>` : ""}${fitBadge(s)}</span>`;
}

// Input and output are read very differently — one is context you pay to
// resend, the other is what the model actually produced — so the row shows
// both rather than a single total.
function tokenCell(s) {
  const out = Number(s.completion_tokens || 0);
  const inp = Number(s.prompt_tokens || 0);
  const cached = Number(s.cached_prompt_tokens || 0);
  const title = [
    `${num(inp)} input tokens`,
    cached ? `${num(cached)} of them served from cache` : null,
    `${num(out)} output tokens`,
    `${num(s.total_tokens)} total`,
  ].filter(Boolean).join("\n");
  return `<span class="tokens" title="${esc(title)}">
    <span>${compact(inp)}</span><span class="tok-sep">/</span><span class="tok-out">${compact(out)}</span>
  </span>`;
}

// The cell answers "which services did this session touch" at a glance: the
// marks are the MCP servers, and the tooltip names their functions, so nobody
// has to open the panel to find out what "3 mcp" meant.
function toolCell(s) {
  const calls = Number(s.tool_calls || 0);
  if (!calls) return `<span class="muted">\u2014</span>`;

  const tools = s.tools || [];
  const native = tools.filter((t) => t.source !== "mcp");
  const servers = s.mcp_servers || [];

  const marks = servers.slice(0, 4).map((m) => {
    const fns = tools.filter((t) => t.source === "mcp" && t.server === m.server)
      .map((t) => `  ${t.tool} \u00d7${t.calls}`).join("\n");
    const look = mcpLook(m.server);
    const title = `${look.label} (${look.category})\n${num(m.calls)} call${m.calls === 1 ? "" : "s"}${fns ? "\n" + fns : ""}`;
    return `<span class="${markClass(look.icon, look.tone)}" title="${esc(title)}">
      <svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg></span>`;
  }).join("");

  const nativeTitle = native.map((t) => `  ${t.tool} \u00d7${t.calls}`).join("\n");
  return `<div class="tool-cell">
    <span class="tool-count" title="${esc(native.length ? "Built-in tools:\n" + nativeTitle : "")}">${num(calls)}</span>
    <span class="mark-row">${marks}${servers.length > 4 ? `<span class="badge neutral">+${servers.length - 4}</span>` : ""}</span>
  </div>`;
}

function fileCell(s) {
  // The same cleaning as the detail panel, or the row and the panel it opens
  // would disagree about how many files were touched.
  const files = normaliseFiles(s.files_read || [], s.files_written || []);
  const label = (e) => `${e.outside ? e.path : [e.dir, e.name].filter(Boolean).join("/")}${e.isDir ? "/" : ""}`;
  const read = files.read.map(label);
  const written = files.write.map(label);
  if (!read.length && !written.length) return `<span class="muted">\u2014</span>`;
  const list = (label, paths) => paths.length
    ? `${label}:\n${paths.slice(0, 12).map((p) => "  " + p).join("\n")}${paths.length > 12 ? `\n  \u2026 ${paths.length - 12} more` : ""}`
    : "";
  const title = [list("Read", read), list("Written", written)].filter(Boolean).join("\n\n");
  const count = (icon, n, cls) => n
    ? `<span class="icon-count ${cls}"><svg aria-hidden="true"><use href="#${icon}"/></svg>${num(n)}</span>`
    : "";
  return `<span class="file-cell" title="${esc(title)}">${
    count("s-read", read.length, "read")}${count("s-write", written.length, "write")}</span>`;
}

function sensitiveCell(s) {
  const list = s.findings || [];
  if (!list.length) return `<span class="muted" title="No secret or personal data detected in either direction">\u2014</span>`;
  const title = list.map((f) =>
    `${f.type} \u00d7${f.occurrences} (${f.kind}, ${f.origin === "response" ? "model response" : "request"})`).join("\n");
  const total = list.reduce((a, f) => a + Number(f.occurrences || 0), 0);
  // One number, coloured by the worst thing in it: a leaked key outranks a
  // hundred e-mail addresses, and the breakdown is one hover away.
  const kinds = new Set(list.map((f) => f.kind));
  const tone = kinds.has("secret") ? "danger" : kinds.has("pii") ? "warning" : "info";
  return `<span class="icon-count sensitive ${tone}" title="${esc(title)}"><svg aria-hidden="true"><use href="#s-shield"/></svg>${num(total)}</span>`;
}

function bindSessionRows() {
  $$("[data-session]").forEach((row) => {
    if (row.dataset.session) row.onclick = () => openSession(row.dataset.session, row.dataset.request || null);
  });
}

/* --- Live stream ---------------------------------------------------------- */

function setLive(on, text) {
  trafficLive = on;
  const pill = $("#live-pill");
  if (!pill) return;
  pill.classList.toggle("on", on);
  pill.classList.toggle("off", !on);
  $("#live-text").textContent = text;
}

function openTrafficStream() {
  closeTrafficStream();
  if (!window.EventSource) {
    setLive(false, "Live updates unsupported");
    return;
  }

  // EventSource cannot carry an Authorization header, so this relies on the
  // console session cookie the admin middleware already accepts.
  trafficStream = new EventSource("/v1/traffic/stream");

  trafficStream.onopen = () => setLive(true, "Live");
  trafficStream.onerror = () => setLive(false, "Reconnecting…");

  trafficStream.addEventListener("capture", (e) => onTurn(readMessage(e)?.event, "pending"));
  trafficStream.addEventListener("event", (e) => onTurn(readMessage(e)?.event, "enriched"));
  trafficStream.addEventListener("session", (e) => onSession(readMessage(e)?.session));

  // Nothing arrives when a session goes quiet, so its light has to be expired
  // on a timer rather than by an event. Only the lights are touched: rebuilding
  // the table every ten seconds would drop hover, text selection and scroll
  // position under the reader's hands.
  clearInterval(activityTimer);
  activityTimer = setInterval(() => {
    if (currentPage === "traffic") refreshLiveDots();
  }, 10000);
}

function refreshLiveDots() {
  let changed = false;
  $$("[data-session]").forEach((row) => {
    const live = isActive(row.dataset.session);
    if (row.classList.contains("live") !== live) changed = true;
    row.classList.toggle("live", live);
    const dot = $(".dot", row);
    if (dot) {
      dot.classList.toggle("dot-live", live);
      dot.title = live ? "active" : "idle";
    }
  });
}

function closeTrafficStream() {
  if (trafficStream) {
    trafficStream.close();
    trafficStream = null;
  }
  clearInterval(activityTimer);
  activityTimer = null;
  trafficLive = false;
}

function readMessage(e) {
  try { return JSON.parse(e.data); } catch { return null; }
}

function onTurn(event, kind) {
  if (!event || currentPage !== "traffic") return;

  // The Requests view lists turns directly, so it needs each one as it lands.
  const known = state.events.findIndex((e) => e.id === event.id);
  if (known >= 0) state.events[known] = event;
  else state.events.unshift(event);
  if (trafficView === "requests") repaintSessions();

  touchSession(event, !state.seenTurns.has(event.id));
  state.seenTurns.add(event.id);

  // The open panel is a live view of one session, so a turn belonging to it
  // lands there as it arrives rather than on the next manual reopen.
  if (openSessionId && event.session_id === openSessionId) {
    const at = openEvents.findIndex((e) => e.id === event.id);
    if (at >= 0) openEvents[at] = event;
    else openEvents.push(event);
    markTurnFresh(event.id);
    repaintPanel();
  }
}

// touchSession lights a session up the moment one of its turns lands.
//
// A session the analyzer has not aggregated yet gets a provisional row built
// from the turn itself, rather than staying invisible for a whole analyze
// interval: a run that just started is exactly the one an operator is looking
// for. The analyzer's own message replaces it with the authoritative numbers.
function touchSession(event, isNewTurn) {
  const id = event.session_id;
  if (!id) return;

  markActive(id);

  const existing = state.sessions.find((s) => s.session_id === id);
  if (existing) {
    existing.ended_at = event.created_at || existing.ended_at;
    if (existing.provisional && isNewTurn) {
      existing.requests += 1;
      existing.cost = Number(existing.cost || 0) + Number(event.cost || 0);
      existing.tool_calls += (event.tools || []).length;
    }
  } else {
    state.sessions.unshift({
      session_id: id,
      client_product: event.client_product,
      agent: event.agent,
      agent_via: event.agent_via,
      key_prefix: event.key_prefix,
      models: event.model ? [event.model] : [],
      providers: event.provider ? [event.provider] : [],
      // Without this a flat-billed session reads as 0,00 USD until the
      // analyzer replaces the row — the whole window in which it is live and
      // being watched.
      billing_mode: event.billing_mode,
      started_at: event.created_at,
      ended_at: event.created_at,
      requests: 1,
      errors: Number(event.status) >= 400 ? 1 : 0,
      cost: Number(event.cost || 0),
      total_tokens: Number(event.total_tokens || 0),
      tool_calls: (event.tools || []).length,
      files_read: [],
      files_written: [],
      findings: event.findings || [],
      provisional: true,
    });
    repaintSessions();
    return;
  }

  // An existing row only needs its own cells redrawn, and it has to move to
  // the top now that it is live, so the row is relocated rather than the whole
  // table rebuilt.
  const row = $(`[data-session="${CSS.escape(id)}"]`);
  const body = row && row.parentElement;
  if (!row) {
    repaintSessions();
    return;
  }
  row.outerHTML = sessionRow(existing);
  const moved = $(`[data-session="${CSS.escape(id)}"]`);
  if (moved && body && body.firstElementChild !== moved) body.prepend(moved);
  bindSessionRows();
}

function repaintSessions() {
  const table = $("#session-table");
  if (!table) return;
  table.innerHTML = trafficView === "sessions" ? sessionTable() : requestTable();
  bindSessionRows();
  const count = $("[data-traffic-view=requests] b");
  if (count) count.textContent = num(state.events.length);
}
function onSession(session) {
  if (!session || currentPage !== "traffic") return;

  const index = state.sessions.findIndex((s) => s.session_id === session.session_id);
  const known = index >= 0;
  if (known) state.sessions[index] = session;
  else state.sessions.unshift(session);

  if (openSessionId === session.session_id) {
    openSession_ = session;
    repaintPanel();
  }

  // A session that was already listed keeps its place: swapping one row in is
  // invisible, while re-rendering the table moves everything under the cursor.
  const row = known && $(`[data-session="${CSS.escape(session.session_id)}"]`);
  if (row) {
    row.outerHTML = sessionRow(session);
    bindSessionRows();
    return;
  }
  repaintSessions();
}

/* --- Session panel -------------------------------------------------------- */

// The panel is a live view of one session, not a snapshot, so its state lives
// outside the render: turns arriving over SSE are folded into openEvents and
// the panel repaints itself.
let openSessionId = null;
let openSession_ = null;
let openEvents = [];
let selectedTurn = null;
let filesTab = "read";
// Chosen once when a session opens, then left alone: a panel that switches tab
// under the reader because a live turn arrived is worse than a dense one.
let asideTab = null;
let openToken = 0;
const freshTurns = new Set();

function markTurnFresh(id) {
  freshTurns.add(id);
  setTimeout(() => freshTurns.delete(id), 2000);
}

async function openSession(sessionId, turnId = null) {
  const panel = $("#session-detail");
  const token = ++openToken;
  openSessionId = sessionId;
  selectedTurn = turnId;
  asideTab = null;
  openEvents = [];
  openSession_ = state.sessions.find((x) => x.session_id === sessionId) || {};

  panel.innerHTML = `<div class="empty">Loading session\u2026</div>`;
  $("#session-dialog").showModal();

  try {
    repaintSessions();

    const detail = await safe(`/v1/traffic/session?session_id=${encodeURIComponent(sessionId)}`, null);
    // A token rather than the session id: the id is also cleared when the
    // drawer closes, which made a perfectly good response look stale and left
    // the panel on its loading message.
    if (token !== openToken) return;
    if (!detail) {
      panel.innerHTML = emptyState("Session unavailable");
      return;
    }

    openSessionId = sessionId;
    openSession_ = detail.session || openSession_;
    openEvents = detail.events || [];
    repaintPanel();
    // Opened from one request: that turn is what the reader came for.
    if (turnId) {
      $(`[data-turn="${CSS.escape(turnId)}"]`, panel)?.scrollIntoView({ block: "center" });
    }
  } catch (err) {
    // Without this the drawer sat on "Loading session…" for ever whenever a
    // render threw, which tells the operator nothing and hides the fault.
    panel.innerHTML = emptyState("Could not open this session", String((err && err.message) || err));
    throw err;
  }
}

function closeSession() {
  openSessionId = null;
  openSession_ = null;
  openEvents = [];
  selectedTurn = null;
  repaintSessions();
}

function repaintPanel() {
  const panel = $("#session-detail");
  if (!panel || !openSessionId || !$("#session-dialog").open) return;

  const s = openSession_ || {};
  // Repainting replaces the markup, which resets every scroll position: a click
  // on a turn, or a live turn arriving, threw the reader back to the top.
  const scrolled = [".panel-body", ".panel-main .table-wrap", ".aside-scroll"].map((sel) => [sel, $(sel, panel)?.scrollTop || 0]);
  // Newest first: a live session keeps producing turns, and the one that just
  // arrived is the one being watched.
  const events = [...openEvents].sort(
    (a, b) => new Date(b.created_at || 0) - new Date(a.created_at || 0));
  const live = isActive(openSessionId);
  const models = s.models || [];
  const singleModel = new Set(events.map((e) => e.model).filter(Boolean)).size <= 1;
  // Columns that would repeat one value on every row are said once, above.
  const allFlat = events.length > 0 && events.every((e) => e.billing_mode === "flat" || Number(e.status) >= 400);
  const cols = { model: !singleModel, cost: !allFlat };
  const facts = [
    models.length ? `${esc(modelName(models[0]))}${models.length > 1 ? ` +${models.length - 1}` : ""}` : "",
    models.length && providerName(models[0]) ? esc(providerName(models[0])) : "",
    s.started_at ? `${clock(s.started_at)}${s.ended_at ? ` \u2013 ${clock(s.ended_at)}` : ""}` : "",
    s.client_host || s.client_ip
      // The machine name, not the ISP's domain it resolved under.
      ? `from <span title="${esc([s.client_host, s.client_ip].filter(Boolean).join(" \u2014 "))}">${
        esc(s.client_host ? s.client_host.split(".")[0] : s.client_ip)}</span>`
      : "",
    ...taskFacts(s),
  ].filter(Boolean);

  panel.innerHTML = `
    <header class="panel-head">
      <div class="panel-title">
        ${agentMark(s.agent, s.agent_via)}
        <div>
          <h2>${esc(agentName(s.agent, s.agent_via))}
            ${live ? `<span class="badge live-badge"><span class="dot-live"></span>live</span>` : ""}
          </h2>
          <div class="panel-facts">
            <button type="button" class="session-id-chip mono" data-copy="${esc(openSessionId)}" title="${esc(openSessionId)} \u2014 click to copy">${esc(String(openSessionId).slice(0, 8))}</button>
            ${facts.map((f) => `<span>${f}</span>`).join("")}
          </div>
        </div>
      </div>
      <button class="icon-button" data-close aria-label="Close">&times;</button>
    </header>

    <div class="panel-stats">
      ${[
        ["s-turns", "Turns", num(s.requests || events.length), "Turns in this session"],
        ["s-in", "Input", compact(s.prompt_tokens), "Input tokens"],
        ["s-out", "Output", compact(s.completion_tokens), "Output tokens"],
        ["s-cost", "Cost", costCell(s), "Cost"],
        ["s-tools", "Tools", num(s.tool_calls), "Tool calls"],
        ["s-clock", "p95", ms(s.latency_p95_ms), "95th percentile latency"],
        ["s-alert", "Errors", num(s.errors), "Failed turns"],
      ].map(([icon, label, value, hint]) => `
        <div class="ps-item${icon === "s-alert" && Number(s.errors) ? " has-errors" : ""}" title="${hint}">
          <span class="ps-label"><svg class="ps-icon" aria-hidden="true"><use href="#${icon}"/></svg><span class="ps-text">${label}</span></span>
          <strong>${value}</strong>
        </div>`).join("")}
    </div>

    <div class="panel-body">
      <div class="panel-main">
        <div class="detail-section">
          <h3>Turns${live ? ` <span class="muted sub">streaming</span>` : ""}</h3>
          <div class="table-wrap"><table class="turn-table">
            <thead><tr><th>Time</th>${cols.model ? "<th>Model</th>" : ""}<th class="num">In / out</th>${cols.cost ? `<th class="num">Cost</th>` : ""}<th class="num">TTFB</th><th>Tools</th><th>Status</th><th>Sensitive</th></tr></thead>
            <tbody>${events.map((e) => turnTableRow(e, cols)).join("") || `<tr><td colspan="8">${emptyState("No turns yet")}</td></tr>`}</tbody>
          </table></div>
        </div>
      </div>
      <aside class="panel-aside" id="session-aside">${asidePanel(s, events, selectedTurn)}</aside>
    </div>`;

  bindDialogClose(panel);
  for (const [sel, top] of scrolled) {
    const el = $(sel, panel);
    if (el) el.scrollTop = top;
  }
  $$("[data-copy]", panel).forEach((b) => (b.onclick = async () => {
    try {
      await navigator.clipboard.writeText(b.dataset.copy);
      b.classList.add("copied");
      setTimeout(() => b.classList.remove("copied"), 1200);
    } catch { /* clipboard needs a secure context; the tooltip still has it */ }
  }));
  $$("[data-bar]", panel).forEach((bar) => {
    bar.classList.add(sizeClass("w", bar.dataset.bar));
  });
  $$("[data-turn]", panel).forEach((row) => (row.onclick = () => {
    selectedTurn = selectedTurn === row.dataset.turn ? null : row.dataset.turn;
    repaintPanel();
  }));
  // Toggled in place: the panel repaints on every live turn, and a full
  // repaint here would also throw away the list's scroll position.
  $$("[data-files-tab]", panel).forEach((b) => (b.onclick = () => {
    filesTab = b.dataset.filesTab;
    $$("[data-files-tab]", panel).forEach((x) => x.classList.toggle("active", x === b));
    $$("[data-files-list]", panel).forEach((l) => l.classList.toggle("hidden", l.dataset.filesList !== filesTab));
  }));
  $$("[data-scope-back]", panel).forEach((b) => (b.onclick = () => {
    selectedTurn = null;
    repaintPanel();
  }));
  $$("[data-aside-tab]", panel).forEach((b) => (b.onclick = () => {
    asideTab = b.dataset.asideTab;
    $$("[data-aside-tab]", panel).forEach((x) => {
      x.classList.toggle("active", x === b);
      x.setAttribute("aria-selected", String(x === b));
    });
    $$("[data-aside-pane]", panel).forEach((p) => p.classList.toggle("hidden", p.dataset.asidePane !== asideTab));
    $("[data-files-filter]", panel)?.classList.toggle("hidden", asideTab !== "files");
  }));
}

function turnTableRow(e, cols = { model: true, cost: true }) {
  const tools = e.tools || [];
  const mcp = tools.filter((t) => t.source === "mcp");
  const marks = [...new Set(mcp.map((t) => t.server))].slice(0, 3).map((server) => {
    const look = mcpLook(server);
    return `<span class="${markClass(look.icon, look.tone)}" title="${esc(look.label)}">
      <svg class="icon" aria-hidden="true"><use href="#${look.icon}"/></svg></span>`;
  }).join("");
  const toolTitle = tools.map((t) => `  ${t.server ? t.server + " / " : ""}${t.tool}`).join("\n");
  const failed = Number(e.status) >= 400;
  const code = Number(e.status);

  // A failed turn has no tokens, no cost and no tools: four cells of zeros that
  // say nothing, where the one thing worth reading is why it failed.
  const middle = failed
    ? `<td colspan="${cols.cost ? 4 : 3}" class="turn-fail" title="${esc(e.error_message || "")}">
        <strong>${esc(failureKind(code))}${e.provider ? ` \u00b7 ${esc(providerName(e.model) || e.provider)}` : ""}</strong>
        ${e.error_message ? `<span>${esc(e.error_message)}</span>` : ""}
      </td>`
    : `<td class="num">${tokenCell(e)}</td>
      ${cols.cost ? `<td class="num">${costCell(e)}</td>` : ""}
      <td class="num">${ms(e.ttfb_ms || e.duration_ms)}</td>
      <td>${tools.length
        ? `<div class="tool-cell"><span class="tool-count" title="${esc(toolTitle)}">${num(tools.length)}</span><span class="mark-row">${marks}</span></div>`
        : `<span class="muted">\u2014</span>`}</td>`;

  return `
    <tr class="clickable ${selectedTurn === e.id ? "selected" : ""} ${freshTurns.has(e.id) ? "fresh" : ""} ${failed ? "failed" : ""}" data-turn="${esc(e.id)}">
      <td class="mono">${clock(e.created_at)}</td>
      ${cols.model ? `<td>${modelChip(e.model)}</td>` : ""}
      ${middle}
      <td>${failed
        ? `<span class="mono fail-code">${code}</span>`
        : e.error_message ? statusBadge(e.status, e.error_message) : `<span class="muted mono">${code || "\u2014"}</span>`}</td>
      <td>${sensitiveCell(e)}</td>
    </tr>`;
}

function failureKind(code) {
  if (code === 429) return "Rate limited";
  if (code === 401 || code === 403) return "Refused";
  if (code === 404) return "Not found";
  if (code === 400 || code === 422) return "Rejected request";
  if (code === 408 || code === 504) return "Timed out";
  if (code >= 500) return "Provider error";
  return `HTTP ${code}`;
}

// turnScope reshapes one turn into the same fields a session summary carries,
// so the right-hand panel has a single renderer for both scopes.
function turnScope(e) {
  const counts = new Map();
  for (const t of e.tools || []) {
    const key = `${t.source}\u0000${t.server || ""}\u0000${t.tool}`;
    const row = counts.get(key) || { tool: t.tool, server: t.server, source: t.source, calls: 0 };
    row.calls += 1;
    counts.set(key, row);
  }
  const servers = new Map();
  for (const row of counts.values()) {
    if (row.source !== "mcp" || !row.server) continue;
    servers.set(row.server, (servers.get(row.server) || 0) + row.calls);
  }
  return {
    models: e.model ? [e.model] : [],
    providers: e.provider ? [e.provider] : [],
    tools: [...counts.values()],
    mcp_servers: [...servers].map(([server, calls]) => ({ server, calls })),
    files_read: (e.files || []).filter((f) => f.operation !== "write").map((f) => f.path),
    files_written: (e.files || []).filter((f) => f.operation === "write").map((f) => f.path),
    findings: e.findings || [],
  };
}

function asidePanel(session, events, turnId) {
  const turn = turnId ? events.find((e) => e.id === turnId) : null;
  const scope = turn ? turnScope(turn) : session;
  const files = normaliseFiles(scope.files_read || [], scope.files_written || []);
  const reads = files.read;
  const writes = files.write;
  // A write is the rarer and weightier event, so an empty read list must not
  // hide it behind a tab that shows nothing.
  const tab = filesTab === "write" || !reads.length ? "write" : "read";
  const fileList = (op, entries) => `
    <div class="file-groups${tab === op ? "" : " hidden"}" data-files-list="${op}">
      ${entries.length ? fileGroups(entries) : `<p class="muted sub">Nothing ${op === "read" ? "read" : "written"}</p>`}
    </div>`;
  // Titled like the Turns column opposite, so the two read as a pair. A
  // selected turn gets a way back: clicking the row a second time was the only
  // exit, and nothing said so.
  const header = turn
    ? `<h3 class="aside-title">Turn <span class="mono">${clock(turn.created_at)}</span>
        <button type="button" class="scope-back" data-scope-back>\u2190 Session</button></h3>`
    : `<h3 class="aside-title">Session</h3>`;

  const findings = scope.findings || [];
  const sensitiveCount = findings.reduce((a, f) => a + Number(f.occurrences || 0), 0);
  const toolCount = (scope.tools || []).reduce((a, t) => a + Number(t.calls || 0), 0);
  const fileCount = reads.length + writes.length;
  if (!asideTab) asideTab = sensitiveCount ? "sensitive" : "tools";

  const tabs = [
    ["sensitive", "Sensitive", sensitiveCount],
    ["tools", "Tools", toolCount],
    ["files", "Files", fileCount],
  ];
  // The read/write filter lives on the tab row, so it stays in view with the
  // tabs and only shows while Files is open.
  const filesFilter = fileCount ? `
    <div class="tab-filter${asideTab === "files" ? "" : " hidden"}" data-files-filter>
      <button type="button" class="${tab === "read" ? "active" : ""}" data-files-tab="read">Read <b>${num(reads.length)}</b></button>
      <button type="button" class="${tab === "write" ? "active" : ""}" data-files-tab="write">Write <b>${num(writes.length)}</b></button>
    </div>` : "";
  const tabBar = `
    <div class="aside-tabs" role="tablist">
      ${tabs.map(([id, label, count]) => `
        <button type="button" role="tab" aria-selected="${asideTab === id}" data-aside-tab="${id}"
          class="${asideTab === id ? "active" : ""}${count ? "" : " empty"}">
          ${label} <b>${num(count)}</b>
        </button>`).join("")}
      ${filesFilter}
    </div>`;
  const pane = (id, html) => `<div class="aside-pane${asideTab === id ? "" : " hidden"}" data-aside-pane="${id}">${html}</div>`;

  const toolsHtml = `${mcpSection(scope)}${nativeToolSection(scope)}`;
  const filesHtml = fileCount ? `
      ${fileList("read", reads)}
      ${fileList("write", writes)}` : "";
  const sensitiveHtml = sensitiveGroups(findings);
  const where = turn ? "in this turn" : "in this session";

  return `
    ${header}
    ${turn && turn.error_message ? `
      <div class="turn-error">
        <strong>${esc(failureKind(Number(turn.status)))} · ${esc(turn.provider ? providerName(turn.model) || turn.provider : "gateway")} · HTTP ${num(turn.status)}</strong>
        <p>${esc(turn.error_message)}</p>
      </div>` : ""}
    ${tabBar}
    <div class="aside-scroll">
      ${pane("sensitive", sensitiveHtml || emptyState(`No sensitive data ${where}`))}
      ${pane("tools", toolsHtml || emptyState(`No tools called ${where}`))}
      ${pane("files", filesHtml || emptyState(`No files touched ${where}`))}
    </div>`;
}

// mcpSection answers "which MCP servers did this session reach, and what did it
// call on them". The per-tool rows already carry their server, so the grouping
// is done here rather than asking the gateway for a second shape of the same
// data.
function mcpSection(s) {
  const byServer = new Map();
  for (const t of s.tools || []) {
    if (t.source !== "mcp" || !t.server) continue;
    if (!byServer.has(t.server)) byServer.set(t.server, []);
    byServer.get(t.server).push(t);
  }
  // A server can show up in the call totals without any tool row surviving, so
  // list it anyway: knowing it was reached matters even if the detail is gone.
  for (const m of s.mcp_servers || []) {
    if (!byServer.has(m.server)) byServer.set(m.server, []);
  }
  if (!byServer.size) return "";

  const totals = new Map((s.mcp_servers || []).map((m) => [m.server, m.calls]));
  const servers = [...byServer.entries()].sort(
    (a, b) => (totals.get(b[0]) || 0) - (totals.get(a[0]) || 0),
  );

  const html = servers.map(([server, tools]) => {
    const calls = totals.get(server) || tools.reduce((a, t) => a + Number(t.calls || 0), 0);
    const peak = Math.max(1, ...tools.map((t) => Number(t.calls || 0)));
    const sorted = [...tools].sort((a, b) => Number(b.calls || 0) - Number(a.calls || 0));
    const look = mcpLook(server);
    return `
      <div class="mcp-server">
        <div class="mcp-server-head">
          ${mcpMark(server)}
          <div class="mcp-server-name">
            <strong>${esc(look.label)}</strong>
            <span class="muted sub">${num(calls)} call${calls === 1 ? "" : "s"} · ${num(sorted.length)} function${sorted.length === 1 ? "" : "s"}</span>
          </div>
        </div>
        ${sorted.length ? `<div class="mcp-tools">${sorted.map((t) => `
          <div class="mcp-tool">
            <span class="mono truncate" title="${esc(t.tool)}">${esc(t.tool)}</span>
            <span class="mcp-bar"><i data-bar="${Math.round((100 * Number(t.calls || 0)) / peak)}"></i></span>
            <b>${num(t.calls)}</b>
          </div>`).join("")}</div>` : `<p class="muted sub">Functions not recorded for this server.</p>`}
      </div>`;
  }).join("");

  return detailSection("MCP servers & functions", html, true);
}

function nativeToolSection(s) {
  const native = (s.tools || []).filter((t) => t.source !== "mcp");
  if (!native.length) return "";
  const sorted = [...native].sort((a, b) => Number(b.calls || 0) - Number(a.calls || 0));
  const peak = Math.max(1, ...sorted.map((t) => Number(t.calls || 0)));
  const calls = sorted.reduce((a, t) => a + Number(t.calls || 0), 0);
  // Same row and bar as an MCP server's functions: the question is the same,
  // so the answer should read the same way.
  return detailSection("Built-in tools", `
    <div class="mcp-server">
      <div class="mcp-server-head">
        <span class="${markClass("c-code", "slate")}"><svg class="icon" aria-hidden="true"><use href="#c-code"/></svg></span>
        <div class="mcp-server-name">
          <strong>Agent runtime</strong>
          <span class="muted sub">${num(calls)} call${calls === 1 ? "" : "s"} \u00b7 ${num(sorted.length)} tool${sorted.length === 1 ? "" : "s"}</span>
        </div>
      </div>
      <div class="mcp-tools">${sorted.map((t) => `
        <div class="mcp-tool">
          <span class="mono truncate" title="${esc(t.tool)}">${esc(t.tool)}</span>
          <span class="mcp-bar"><i data-bar="${Math.round((100 * Number(t.calls || 0)) / peak)}"></i></span>
          <b>${num(t.calls)}</b>
        </div>`).join("")}</div>
    </div>`, true);
}

/* --- Sensitive data ------------------------------------------------------- */

const FINDING_GROUPS = [
  ["secret", "Secrets", "danger"],
  ["pii", "Personal data", "warning"],
  ["watchlist", "Watchlist", "info"],
];

// Words a detector id spells that plain sentence case would get wrong.
const FINDING_WORDS = { IP: "IP", IBAN: "IBAN", URL: "URL", API: "API", AWS: "AWS", JWT: "JWT", SSH: "SSH", GITHUB: "GitHub", GITLAB: "GitLab", OPENAI: "OpenAI", PEM: "PEM", US: "US", SSN: "SSN", IVA: "IVA" };

// EMAIL_ADDRESS reads as "Email address". A watchlist finding is already named
// by the operator, so it is left exactly as they wrote it.
function findingLabel(f) {
  if (f.kind === "watchlist") return f.type;
  return String(f.type).split("_").map((w, i) => {
    const up = w.toUpperCase();
    if (FINDING_WORDS[up]) return FINDING_WORDS[up];
    const lower = w.toLowerCase();
    return i === 0 ? lower.charAt(0).toUpperCase() + lower.slice(1) : lower;
  }).join(" ");
}

// Grouped by severity, worst first, so a leaked key is never below a hundred
// e-mail addresses. "In request" is the common case and goes unsaid; a value
// the model produced itself is the one worth flagging.
function sensitiveGroups(findings) {
  return FINDING_GROUPS.map(([kind, title, tone]) => {
    const rows = findings.filter((f) => f.kind === kind)
      .sort((a, b) => Number(b.occurrences || 0) - Number(a.occurrences || 0));
    if (!rows.length) return "";
    const total = rows.reduce((a, f) => a + Number(f.occurrences || 0), 0);
    return `
      <div class="sens-group">
        <div class="sens-group-head"><i class="sev-dot ${tone}"></i>${title}<b>${num(total)}</b></div>
        ${rows.map((f) => {
          const values = f.samples || [];
          const line = `
            <span class="sens-name" title="${esc(f.type)}">${esc(findingLabel(f))}</span>
            ${f.origin === "response" ? `<span class="sens-origin">model response</span>` : ""}
            <b>${num(f.occurrences)}</b>`;
          if (!values.length) return `<div class="sens-row">${line}<span class="sens-caret"></span></div>`;
          return `
            <details class="sens-row-wrap">
              <summary class="sens-row">${line}<span class="sens-caret">\u25b8</span></summary>
              <div class="finding-values">${values.map((v) => `<code>${
                kind === "watchlist" ? highlightSample(v, state.watchPatterns[f.type]) : esc(v)}</code>`).join("")}</div>
            </details>`;
        }).join("")}
      </div>`;
  }).join("");
}

/* --- Files ---------------------------------------------------------------- */

// normaliseFiles turns what the agent's tools named into a list a person can
// read. Agents mix absolute and project-relative paths for the same file, and
// shell commands leak things that are not files at all (2>/dev/null). The
// project root is inferred from where the relative paths start, stripped, and
// the duplicates it exposes are merged. Anything absolute that is not under it
// is kept apart: an agent reaching outside its project is worth seeing.
function normaliseFiles(readPaths, writePaths) {
  const junk = (p) => !p || /[<>|;&]/.test(p) || p.startsWith("/dev/") || p === "null";
  const all = [...readPaths, ...writePaths].filter((p) => !junk(p));
  const starts = new Set(all.filter((p) => !p.startsWith("/") && !p.startsWith("~"))
    .map((p) => p.replace(/^\.\//, "").split("/")[0]).filter(Boolean));
  const votes = new Map();
  for (const p of all.filter((x) => x.startsWith("/"))) {
    const segs = p.split("/");
    const at = segs.findIndex((s, i) => i > 1 && starts.has(s));
    if (at > 0) {
      const root = segs.slice(0, at).join("/");
      votes.set(root, (votes.get(root) || 0) + 1);
    }
  }
  const root = [...votes.entries()].sort((a, b) => b[1] - a[1])[0]?.[0] || "";

  const shape = (paths) => {
    const seen = new Map();
    for (const raw of paths) {
      if (junk(raw)) continue;
      let rel = raw.replace(/^\.\//, "");
      let outside = false;
      if (root && rel.startsWith(root + "/")) rel = rel.slice(root.length + 1);
      else if (rel.startsWith("/") || rel.startsWith("~")) outside = true;
      const isDir = rel.endsWith("/");
      const clean = rel.replace(/\/+$/, "");
      const cut = clean.lastIndexOf("/");
      const key = `${outside}\u0000${clean}`;
      if (!seen.has(key)) {
        seen.set(key, {
          path: raw, outside, isDir,
          isPattern: /[*?]/.test(clean),
          dir: cut < 0 ? "" : clean.slice(0, cut),
          name: cut < 0 ? clean : clean.slice(cut + 1),
        });
      }
    }
    return [...seen.values()];
  };
  return { read: shape(readPaths), write: shape(writePaths), root };
}

function fileGroups(entries) {
  const groups = new Map();
  for (const e of entries) {
    const key = `${e.outside ? 1 : 0}\u0000${e.dir}`;
    if (!groups.has(key)) groups.set(key, { dir: e.dir, outside: e.outside, items: [] });
    groups.get(key).items.push(e);
  }
  const ordered = [...groups.values()].sort((a, b) =>
    (a.outside - b.outside) || (a.dir === "" ? -1 : b.dir === "" ? 1 : a.dir.localeCompare(b.dir)));
  let outsideShown = false;
  return ordered.map((g) => {
    const lead = g.outside && !outsideShown
      ? (outsideShown = true, `<div class="file-outside-head" title="Absolute paths outside the project the agent was working in">Outside the project</div>`)
      : "";
    const items = g.items.sort((a, b) => (b.isDir - a.isDir) || a.name.localeCompare(b.name));
    return `${lead}
      <div class="file-group${g.outside ? " outside" : ""}">
        <div class="file-group-head mono" title="${esc(g.dir || "project root")}"><bdi>${esc(g.dir || "project root")}</bdi></div>
        ${items.map((e) => `
          <div class="file-item mono${e.isDir ? " dir" : ""}${e.isPattern ? " pattern" : ""}" title="${esc(e.path)}">${
            esc(e.name)}${e.isDir ? "/" : ""}</div>`).join("")}
      </div>`;
  }).join("");
}

function detailSection(title, html, show) {
  if (!show) return "";
  return `<div class="detail-section"><h3>${esc(title)}</h3>${html}</div>`;
}

/* --- Connections ---------------------------------------------------------- */

function renderConnections() {
  const rows = state.connections.map((c) => `
    <div class="connection-row">
      <div class="provider-icon">${esc(c.provider_type.slice(0, 2).toUpperCase())}</div>
      <div>
        <h3>${esc(c.name)}</h3>
        <p>${esc(c.provider_type)} · ${c.models.length} model${c.models.length === 1 ? "" : "s"} · ${c.has_api_key ? "credential installed" : "no credential"}</p>
      </div>
      <button class="danger-button" data-delete-connection="${esc(c.id)}">Remove</button>
    </div>`).join("");

  $("#content").innerHTML = `
    <div class="page-intro">
      <div><p>Bring your own keys. Secrets are encrypted locally and never returned by the API.</p></div>
      <button class="primary" id="add-connection">Add connection</button>
    </div>
    <div class="card">${rows || emptyState("No BYOK connections yet", "Add the first provider to expose its models.")}</div>`;

  $("#add-connection").onclick = () => $("#connection-dialog").showModal();
  $$("[data-delete-connection]").forEach((b) => (b.onclick = async () => {
    if (!await confirmDialog({
      title: "Remove this connection?",
      message: "Its encrypted credential and runtime models are deleted.",
      confirm: "Remove", danger: true,
    })) return;
    await api(`/console/api/connections/${b.dataset.deleteConnection}`, { method: "DELETE" });
    navigate("connections");
  }));
}

/* --- Agents --------------------------------------------------------------- */

function renderAgents() {
  const rows = state.keys.map((k) => {
    const ref = k.id || k.gateway_key_id || k.key_prefix;
    return `
    <tr>
      <td><strong>${esc(k.name || "Unnamed identity")}</strong><div class="mono muted">${esc(k.key_prefix)}</div></td>
      <td>${k.agent_id ? `<span class="badge accent">agent</span>` : `<span class="badge neutral">application</span>`}</td>
      <td class="muted">${esc((k.models || []).join(", ") || "All public models")}</td>
      <td>${k.active !== false ? `<span class="badge">active</span>` : `<span class="badge warning">disabled</span>`}</td>
      <td><button class="danger-button" data-remove-agent="${esc(ref)}">Remove</button></td>
    </tr>`;
  }).join("");

  $("#content").innerHTML = `
    <div class="page-intro">
      <div><p>Every agent or application receives an isolated key, policy and spend identity.</p></div>
      <button class="primary" id="add-identity">Create identity</button>
    </div>
    <div class="card flush">
      ${rows
        ? `<div class="table-wrap"><table><thead><tr><th>Identity</th><th>Type</th><th>Model access</th><th>Status</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : emptyState("No identities configured", "Create one to start issuing governed credentials.")}
    </div>`;

  $("#add-identity").onclick = () => $("#identity-dialog").showModal();
  $$("[data-remove-agent]").forEach((b) => (b.onclick = async () => {
    const ref = b.dataset.removeAgent;
    const k = state.keys.find((x) => (x.id || x.gateway_key_id || x.key_prefix) === ref);
    if (!await confirmDialog({
      title: `Remove ${k?.name || "this identity"}?`,
      message: "Its key stops working immediately.",
      confirm: "Remove", danger: true,
    })) return;
    try {
      await api("/v1/key/delete", { method: "POST", body: JSON.stringify({ key: ref }) });
      navigate("agents");
    } catch (err) {
      await noticeDialog("Could not remove the identity", err.message || "");
    }
  }));
}

/* --- Policies ------------------------------------------------------------- */

function renderPolicies() {
  const rows = state.keys.map((k) => `
    <tr>
      <td><strong>${esc(k.name || k.key_prefix)}</strong></td>
      <td class="num">${money(k.spend)} / ${Number(k.budget) ? money(k.budget) : "no limit"}</td>
      <td class="num">${Number(k.rate_limit) ? `${num(k.rate_limit)} RPM` : "no limit"}</td>
      <td class="muted">${esc((k.allowed_providers || []).join(", ") || "any allowed")}</td>
      <td>${k.require_eu_residency ? `<span class="badge info">EU only</span>` : `<span class="badge neutral">global</span>`}</td>
      <td><button class="table-action" data-edit-policy="${esc(k.id || k.gateway_key_id || k.key_prefix)}">Manage</button></td>
    </tr>`).join("");

  $("#content").innerHTML = `
    <div class="page-intro">
      <div><p>Values currently enforced for each runtime identity.</p></div>
    </div>
    <div class="card flush">
      ${rows
        ? `<div class="table-wrap"><table><thead><tr><th>Identity</th><th class="num">Budget</th><th class="num">Rate</th><th>Providers</th><th>Residency</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
        : emptyState("No policies to show", "Create an identity to attach budgets and access rules.")}
    </div>`;

  $$("[data-edit-policy]").forEach((b) => (b.onclick = () => openPolicy(b.dataset.editPolicy)));
}

function openPolicy(ref) {
  const k = state.keys.find((x) => (x.id || x.gateway_key_id || x.key_prefix) === ref);
  if (!k) return;
  const f = $("#budget-form");
  f.elements.key.value = ref;
  f.elements.budget.value = k.budget || 0;
  f.elements.rate_limit.value = k.rate_limit || 0;
  f.elements.models.value = (k.models || []).join(", ");
  f.elements.active.checked = k.active !== false;
  $("#budget-dialog").showModal();
}

/* --- Watchlist ------------------------------------------------------------ */

// The watchlist answers a question the generic detectors cannot. PERSON tells
// an operator that somebody was named; it cannot tell them that *their* chief
// executive was named, which is the alert worth raising. The terms are
// declared here, so there is nothing to infer and nothing to get wrong.
const PII_HINTS = {
  CODICE_FISCALE: "Italian tax code, check character verified",
  PARTITA_IVA: "Italian VAT number",
  IBAN_CODE: "Bank account in IBAN form",
  CREDIT_CARD: "Card numbers that pass the Luhn check",
  CRYPTO: "Bitcoin wallet addresses",
  US_SSN: "US social security numbers",
  US_BANK_NUMBER: "US bank account numbers",
  EMAIL_ADDRESS: "Email addresses",
  PHONE_NUMBER: "Italian and international phone numbers",
  IP_ADDRESS: "IPv4 and IPv6 addresses",
  PERSON: "Names introduced in a sentence (\u201cmi chiamo\u2026\u201d, \u201cdott.ssa\u2026\u201d)",
  LOCATION: "Places introduced in a sentence (\u201cresidente a\u2026\u201d)",
};

async function renderWatchlist() {
  const [terms, detectors] = await Promise.all([
    safe("/v1/watchlist", { data: [] }).then((r) => r.data || []),
    safe("/v1/detectors", { data: [] }).then((r) => r.data || []),
  ]);

  const piiRows = detectors.map((d) => `
    <label class="detector-row ${d.enabled ? "" : "off"}">
      <span><strong>${esc(findingLabel(d))}</strong><small>${esc(PII_HINTS[d.type] || "")}${
        d.default ? "" : " \u00b7 off by default"}</small></span>
      <input type="checkbox" role="switch" class="toggle" data-detector="${esc(d.type)}" ${d.enabled ? "checked" : ""} />
    </label>`).join("");

  const rows = terms.map((t) => `
    <tr class="${t.enabled ? "" : "row-muted"}">
      <td><strong>${esc(t.label)}</strong></td>
      <td><code>${esc(t.pattern)}</code></td>
      <td><input type="checkbox" role="switch" class="toggle" data-toggle-term="${t.id}" ${t.enabled ? "checked" : ""}
        aria-label="${t.enabled ? "Watching" : "Paused"}: ${esc(t.label)}" title="${t.enabled ? "Watching" : "Paused"}" /></td>
      <td class="row-actions">
        <button class="table-action danger" data-delete-term="${t.id}" data-label="${esc(t.label)}">Remove</button>
      </td>
    </tr>`).join("");

  $("#content").innerHTML = `
    <div class="page-intro">
      <div>
        <p>What AI traffic reports as personal data, plus the names, addresses and numbers this organisation wants to hear about.</p>
      </div>
    </div>

    <div class="card">
      <div class="card-head"><div><h2>Standard personal data</h2><p>Turned-off types are no longer reported, but are still masked in stored traffic.</p></div>
        <span class="muted sub">${num(detectors.filter((d) => d.enabled).length)} of ${num(detectors.length)} reported</span></div>
      ${piiRows ? `<div class="detector-grid">${piiRows}</div>` : emptyState("Detector settings unavailable")}
      <p id="detector-error" class="error"></p>
    </div>

    <div class="grid split-2-1">
      <div class="card flush">
        <div class="card-head"><h2>Watched terms</h2><span class="muted sub">${num(terms.length)} configured</span></div>
        ${rows
          ? `<div class="table-wrap"><table><thead><tr><th>Reported as</th><th>Pattern</th><th>Watching</th><th></th></tr></thead><tbody>${rows}</tbody></table></div>`
          : emptyState("Nothing on the watchlist", "Add a term and it will be reported in AI traffic from the next turn.")}
      </div>

      <div class="card">
        <div class="card-head"><h2>Add a term</h2></div>
        <form id="watchlist-form" class="stack-form">
          <label>Reported as
            <input name="label" required maxlength="60" placeholder="CEO mobile" />
            <span class="field-hint">The finding is shown under this name, so the value itself stays out of the console.</span>
          </label>
          <label>Pattern
            <input name="pattern" required maxlength="200" placeholder="+39 333 1234567" />
            <span class="field-hint">Matched as written, case-insensitive. Use <code>*</code> for any run of characters within a word.</span>
          </label>
          <div class="pattern-help">
            <div><code>*assimilia*</code><span>anywhere inside a word</span></div>
            <div><code>*.pippo@relatech.com</code><span>any prefix on that address</span></div>
            <div><code>Mario Rossi</code><span>the whole phrase only</span></div>
          </div>
          <p id="watchlist-error" class="error"></p>
          <button type="submit" class="primary">Add to watchlist</button>
        </form>
      </div>
    </div>`;

  bindWatchlist();
}

function bindWatchlist() {
  $$("[data-detector]").forEach((box) => (box.onchange = async () => {
    box.disabled = true;
    try {
      await api("/v1/detectors/update", {
        method: "POST",
        body: JSON.stringify({ type: box.dataset.detector, enabled: box.checked }),
      });
    } catch (err) {
      box.checked = !box.checked;
      box.disabled = false;
      $("#detector-error").textContent = err.message || "Could not save that change";
      return;
    }
    await renderWatchlist();
  }));

  $("#watchlist-form").onsubmit = async (e) => {
    e.preventDefault();
    const f = new FormData(e.target);
    $("#watchlist-error").textContent = "";
    try {
      await api("/v1/watchlist", {
        method: "POST",
        body: JSON.stringify({ label: f.get("label"), pattern: f.get("pattern") }),
      });
      await renderWatchlist();
    } catch (err) {
      $("#watchlist-error").textContent = err.message || "Could not add that term";
    }
  };

  $$("[data-toggle-term]").forEach((b) => (b.onchange = async () => {
    b.disabled = true;
    await api(`/v1/watchlist/update?id=${encodeURIComponent(b.dataset.toggleTerm)}`, {
      method: "POST",
      body: JSON.stringify({ enabled: b.checked }),
    });
    await renderWatchlist();
  }));

  $$("[data-delete-term]").forEach((b) => (b.onclick = async () => {
    if (!await confirmDialog({
      title: `Stop watching for “${b.dataset.label}”?`,
      message: "The term is deleted. Findings already recorded stay in the traffic.",
      confirm: "Remove", danger: true,
    })) return;
    await api(`/v1/watchlist/delete?id=${encodeURIComponent(b.dataset.deleteTerm)}`, { method: "POST" });
    await renderWatchlist();
  }));
}

/* --- System --------------------------------------------------------------- */

async function renderSystem() {
  const ready = await fetch("/ready")
    .then(async (r) => ({ ok: r.ok, ...(await r.json()) }))
    .catch(() => ({ ok: false, status: "unavailable" }));

  $("#content").innerHTML = `
    <div class="page-intro">
      <div><p>Runtime status and integration endpoints.</p></div>
      <span class="badge ${ready.ok ? "" : "warning"}">${ready.ok ? "ready" : "attention"}</span>
    </div>
    <div class="grid two-col">
      <div class="card">
        <div class="card-head"><h2>Status</h2></div>
        <div class="connection-row">
          <div class="provider-icon">GW</div>
          <div><h3>Data plane</h3><p>${esc(ready.status)} · ${num(ready.models || 0)} runtime models</p></div>
        </div>
        <div class="connection-row">
          <div class="provider-icon">DB</div>
          <div><h3>Governance store</h3><p>${ready.ok ? "Reachable and schema-ready" : "Review readiness response"}</p></div>
        </div>
      </div>
      <div class="card">
        <div class="card-head"><h2>Network integration</h2></div>
        <p class="muted">Allow clients to reach this appliance on the gateway port. Permit provider egress only from the appliance security zone.</p>
        <div class="metric-list">
          <div class="metric-row"><span>OpenAI-compatible base URL</span><strong class="mono">${esc(location.origin)}/v1</strong></div>
          <div class="metric-row"><span>Health probe</span><strong class="mono">${esc(location.origin)}/ready</strong></div>
          <div class="metric-row"><span>Metrics</span><strong class="mono">${esc(location.origin)}/metrics</strong></div>
        </div>
      </div>
    </div>`;
}

/* --- Bindings ------------------------------------------------------------- */

// In-page replacements for confirm() and alert(). Resolves true only on the
// confirm button; Esc, the backdrop and Cancel all mean no.
function confirmDialog({ title, message = "", confirm = "Confirm", danger = false, cancel = "Cancel" }) {
  const d = $("#confirm-dialog");
  $("#confirm-title").textContent = title;
  $("#confirm-message").textContent = message;
  $("#confirm-message").hidden = !message;
  const ok = $("#confirm-ok");
  ok.textContent = confirm;
  ok.className = danger ? "danger-solid" : "primary";
  const no = $("#confirm-cancel");
  no.hidden = cancel === null;
  no.textContent = cancel || "";
  d.returnValue = "";
  d.showModal();
  (cancel === null ? ok : no).focus();
  return new Promise((resolve) => d.addEventListener("close", () => resolve(d.returnValue === "ok"), { once: true }));
}

function noticeDialog(title, message = "") {
  return confirmDialog({ title, message, confirm: "OK", cancel: null });
}

$("#confirm-dialog").addEventListener("click", (event) => {
  if (event.target === event.currentTarget) event.currentTarget.close();
});

function bindDialogClose(root = document) {
  $$("[data-close]", root).forEach((b) => (b.onclick = () => b.closest("dialog").close()));
}

// Esc and backdrop clicks close the drawer without going through a button, so
// the panel state is reset on the dialog's own close event.
$("#session-dialog").addEventListener("close", closeSession);

// A <dialog> does not close when the backdrop is clicked. The backdrop is the
// dialog element itself, so a click landing on it rather than on the panel
// inside means the pointer was outside.
$("#session-dialog").addEventListener("click", (event) => {
  if (event.target === event.currentTarget) event.currentTarget.close();
});

$$("nav button").forEach((b) => (b.onclick = () => navigate(b.dataset.page)));

$("#mobile-menu").onclick = () => {
  $(".sidebar").classList.add("open");
  $("#nav-scrim").classList.add("open");
};

$("#nav-scrim").onclick = () => {
  $(".sidebar").classList.remove("open");
  $("#nav-scrim").classList.remove("open");
};

$("#refresh").onclick = () => navigate(currentPage);

$("#logout").onclick = async () => {
  await api("/console/api/session", { method: "DELETE" });
  csrf = "";
  showLogin();
};

$("#login-form").onsubmit = async (e) => {
  e.preventDefault();
  $("#login-error").textContent = "";
  try {
    const session = await api("/console/api/session", {
      method: "POST",
      expectAuthError: true,
      body: JSON.stringify({ password: $("#password").value }),
    });
    csrf = session.csrf;
    $("#password").value = "";
    showApp();
    await loadHealth();
    navigate("overview");
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
};

bindDialogClose();

$("#connection-form").onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const models = String(f.get("models"))
    .split("\n")
    .map((x) => x.trim())
    .filter(Boolean)
    .map((line) => {
      const [name, providerModel] = line.split("=").map((x) => x.trim());
      return { name, provider_model: providerModel || name };
    });

  const body = {
    name: f.get("name"),
    provider_type: f.get("provider_type"),
    api_base: f.get("api_base"),
    api_key: f.get("api_key"),
    region: f.get("region"),
    api_version: f.get("api_version"),
    models,
  };

  $("#connection-error").textContent = "";
  try {
    await api("/console/api/connections", { method: "POST", body: JSON.stringify(body) });
    $("#connection-dialog").close();
    e.target.reset();
    navigate("connections");
  } catch (err) {
    $("#connection-error").textContent = err.message;
  }
};

$("#identity-form").onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const models = listField(f.get("models"));
  const body = {
    name: f.get("name"),
    budget: Number(f.get("budget")) || 0,
    rate_limit: Number(f.get("rate_limit")) || 0,
  };
  // Omitted, not empty: the API reads an absent list as "no whitelist" and an
  // empty one as "deny every model". Sending [] here revoked the access the
  // field's own hint promised to leave open.
  if (models.length) body.models = models;

  $("#identity-error").textContent = "";
  try {
    const created = await api("/v1/key/generate", { method: "POST", body: JSON.stringify(body) });
    $("#identity-dialog").close();
    e.target.reset();
    if (created && created.key) {
      $("#new-secret").textContent = created.key;
      $("#secret-dialog").showModal();
    }
    navigate("agents");
  } catch (err) {
    $("#identity-error").textContent = err.message;
  }
};

$("#budget-form").onsubmit = async (e) => {
  e.preventDefault();
  const f = new FormData(e.target);
  const models = listField(f.get("models"));
  // /v1/key/update takes {key, params}. Sending the fields at the top level
  // decodes into an empty params and answers "updated" without changing
  // anything, which is worse than an error.
  const params = {
    budget: Number(f.get("budget")) || 0,
    rate_limit: Number(f.get("rate_limit")) || 0,
    active: f.get("active") === "on",
  };
  // Same nil-versus-empty rule as on creation: an empty field leaves the
  // current list alone rather than revoking every model.
  if (models.length) params.models = models;

  $("#budget-error").textContent = "";
  try {
    await api("/v1/key/update", {
      method: "POST",
      body: JSON.stringify({ key: f.get("key"), params }),
    });
    $("#budget-dialog").close();
    navigate("policies");
  } catch (err) {
    $("#budget-error").textContent = err.message;
  }
};

$("#copy-secret").onclick = async () => {
  try {
    await navigator.clipboard.writeText($("#new-secret").textContent);
    $("#copy-secret").textContent = "Copied";
    setTimeout(() => ($("#copy-secret").textContent = "Copy key"), 1600);
  } catch { /* clipboard blocked; the value stays selectable */ }
};

// A console left open on the traffic page holds an SSE connection; drop it
// when the tab goes away rather than leaving the server fanning out to nobody.
window.addEventListener("pagehide", closeTrafficStream);

// A browser allows about six connections per host on HTTP/1.1, and an SSE
// stream holds one for as long as the tab lives. Several console tabs left
// open therefore consume every slot and any further request — including the
// console's own — queues for ever. A hidden tab has nothing to show anyway, so
// it gives its stream back and takes a fresh one when it returns.
document.addEventListener("visibilitychange", () => {
  if (document.hidden) {
    closeTrafficStream();
    setLive(false, "Paused");
  } else if (currentPage === "traffic") {
    navigate("traffic");
  }
});

// Back and forward move between pages rather than out of the console.
window.addEventListener("hashchange", () => {
  const view = location.hash.slice(1) || "overview";
  if (view !== currentPage) navigate(view);
});

bootstrap();
