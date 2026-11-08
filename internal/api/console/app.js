// Forge console. Everything here is a client of the bearer-token API; this
// file holds no job data. Every value the API returns is put into the page
// as text (textContent, or a string passed to append), never parsed as
// HTML: build output is hostile, and the token is in this page.
"use strict";

const PAGE = 25;
const POLL_MS = 2000;
const TOKEN_KEY = "forge-token";

const $ = (id) => document.getElementById(id);

const view = {
  state: "",       // the list's state filter
  before: 0,       // the cursor of the page on screen, 0 for the newest
  history: [],     // cursors of the pages before this one, for Newer
  next: 0,         // the cursor of the following page, 0 if none
  selected: 0,     // the job in Table 2
  follow: null,    // AbortController of the log being followed
  timer: 0,
  gen: 0,          // bumped by each refresh; a slower, older response is dropped
};

// el builds an element; children that are strings become text nodes.
function el(tag, props, ...children) {
  const node = document.createElement(tag);
  if (props && props.className) node.className = props.className;
  node.append(...children.map((c) => (c === null || c === undefined ? "" : c)));
  return node;
}

function readToken() {
  try { return sessionStorage.getItem(TOKEN_KEY) || ""; } catch { return ""; }
}

function writeToken(value) {
  try {
    if (value) sessionStorage.setItem(TOKEN_KEY, value);
    else sessionStorage.removeItem(TOKEN_KEY);
  } catch { /* storage blocked: the token lives only as long as this page */ }
  memoryToken = value;
  showTokenState();
}

let memoryToken = readToken();

function showTokenState() {
  $("token-state").textContent = memoryToken ? "set for this tab" : "not set";
  $("forget").hidden = !memoryToken;
}

class Unauthorized extends Error {}

// api fetches path with the token in the Authorization header. A 401 opens
// the token prompt; with auth off at the API one never comes, so a proxy
// deployment never sees the prompt.
async function api(path, options = {}) {
  const headers = new Headers(options.headers || {});
  if (memoryToken) headers.set("Authorization", "Bearer " + memoryToken);
  let resp;
  try {
    resp = await fetch(path, { ...options, headers, cache: "no-store", referrerPolicy: "no-referrer" });
  } catch (err) {
    if (err.name === "AbortError") throw err;
    throw new Error("The API is unreachable: " + err.message);
  }
  if (resp.status === 401) {
    askForToken(memoryToken ? "The API rejected this token." : "");
    throw new Unauthorized("unauthorized");
  }
  return resp;
}

function askForToken(reason) {
  const form = $("token-form");
  if (reason) say(reason);
  if (form.hidden) {
    form.hidden = false;
    $("token").focus();
  }
}

function say(message) { $("status").textContent = message; }

async function errorText(resp) {
  const body = (await resp.text()).trim();
  return body ? body : resp.status + " " + resp.statusText;
}

// stateOf is the one word the State column shows: done jobs say how.
function stateOf(job) {
  if (job.state !== "done") return job.state;
  if (job.oom_killed) return "done · OOM";
  if (job.error) return "done · " + job.error;
  return "done";
}

function failed(job) {
  return job.state === "done" && (job.exit_code !== 0 || job.oom_killed);
}

const fmt = (n) => (n === undefined || n === null ? "–" : Number(n).toLocaleString("en-US").replace(/,/g, " "));

function jobRow(job) {
  const open = el("button", { className: "link id" }, String(job.id));
  open.type = "button";
  open.setAttribute("aria-label", "Open job " + job.id);
  open.addEventListener("click", () => select(job.id));
  const state = el("td", { className: "state s-" + job.state + (failed(job) ? " bad" : "") }, stateOf(job));
  const row = el("tr", null,
    el("td", null, open),
    state,
    el("td", { className: "cmd wide" }, (job.args || []).join(" ")),
    el("td", { className: "n" + (failed(job) ? " bad" : "") }, job.state === "done" ? String(job.exit_code) : "–"),
    el("td", { className: "n opt-narrow" }, String(job.attempt)),
    el("td", { className: "opt worker" }, job.worker || "–"),
    el("td", { className: "n opt" }, fmt(job.log_bytes)),
    el("td", { className: "n opt" }, job.artifact_bytes ? fmt(job.artifact_bytes) : "–"));
  if (job.id === view.selected) row.className = "selected";
  return row;
}

async function loadList(gen) {
  const q = new URLSearchParams({ limit: String(PAGE) });
  if (view.state) q.set("state", view.state);
  if (view.before) q.set("before", String(view.before));
  const resp = await api("/jobs?" + q);
  if (!resp.ok) throw new Error("Listing jobs failed: " + (await errorText(resp)));
  const page = await resp.json();
  if (gen !== view.gen) return;
  view.next = page.next || 0;
  // Rebuilding the rows would drop keyboard focus every poll; put it back.
  const focused = $("jobs").contains(document.activeElement) ? document.activeElement.textContent : "";
  $("jobs").replaceChildren(...page.jobs.map(jobRow));
  if (focused) {
    const again = [...$("jobs").querySelectorAll(".id")].find((b) => b.textContent === focused);
    if (again) again.focus();
  }
  const ids = page.jobs.map((j) => j.id);
  $("span").textContent = ids.length ? "· #" + ids[0] + " to #" + ids[ids.length - 1] : "";
  const empty = $("empty");
  empty.hidden = ids.length > 0;
  empty.textContent = view.state
    ? "No " + view.state + " jobs on this page. Choose All to see every state."
    : "No jobs yet. Submit one with Submit job; it appears here as queued.";
  $("newer").disabled = view.history.length === 0;
  $("older").disabled = !view.next;
}

// loadCounts reads the queue depth from the authenticated /metrics text.
async function loadCounts() {
  const resp = await api("/metrics");
  if (!resp.ok) return;
  const text = await resp.text();
  for (const state of ["queued", "running", "done"]) {
    const m = text.match(new RegExp('^forge_queue_jobs\\{state="' + state + '"\\} (\\d+)$', "m"));
    $("count-" + state).textContent = m ? fmt(m[1]) : "–";
  }
}

async function refresh() {
  clearTimeout(view.timer);
  const gen = ++view.gen;
  try {
    await Promise.all([loadList(gen), loadCounts(), view.selected ? loadJob(view.selected) : null]);
    if ($("status").textContent.startsWith("The API is unreachable")) say("");
  } catch (err) {
    if (!(err instanceof Unauthorized)) say(err.message);
  }
  // Polling stops while the page is hidden or waiting for a token. Only the
  // latest refresh schedules the next, so overlapping calls keep one chain.
  if (gen === view.gen && document.visibilityState === "visible" && $("token-form").hidden) {
    view.timer = setTimeout(refresh, POLL_MS);
  }
}

function setFilter(state) {
  view.state = state;
  view.before = 0;
  view.history = [];
  for (const b of document.querySelectorAll(".seg button")) {
    b.setAttribute("aria-pressed", String(b.dataset.state === state));
  }
  refresh();
}

function page(older) {
  if (older && !view.next) return;
  $("older").disabled = $("newer").disabled = true;
  if (older) {
    view.history.push(view.before);
    view.before = view.next;
  } else {
    view.before = view.history.pop() || 0;
  }
  refresh();
}

const ARGV0 = ["/bin/sh", "-c"];

function argsFor(command) { return [...ARGV0, command]; }

function showArgs() {
  $("args-preview").textContent = JSON.stringify({ args: argsFor($("command").value) }, null, 1);
}

function toggleSubmit(open) {
  $("submit-form").hidden = !open;
  $("open-submit").setAttribute("aria-expanded", String(open));
  if (open) {
    showArgs();
    $("command").focus();
  } else {
    $("open-submit").focus();
  }
}

async function submit(event) {
  event.preventDefault();
  const error = $("submit-error");
  error.textContent = "";
  try {
    const resp = await api("/jobs", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ args: argsFor($("command").value) }),
    });
    if (!resp.ok) {
      error.textContent = "The API refused the job: " + (await errorText(resp));
      return;
    }
    const { id } = await resp.json();
    $("command").value = "";
    toggleSubmit(false);
    say("Job " + id + " submitted.");
    setFilter("");
    select(id);
  } catch (err) {
    if (!(err instanceof Unauthorized)) error.textContent = err.message;
  }
}

function prop(name, value, unit, bad) {
  return el("tr", null, el("th", { className: "pname" }, name),
    el("td", { className: bad ? "bad" : "" }, value), el("td", { className: "unit" }, unit || ""));
}

function showJob(job) {
  $("d-id").textContent = String(job.id);
  const rows = [
    prop("State", job.state),
    prop("Attempt", String(job.attempt), "n"),
    prop("Worker", job.worker || "–"),
    prop("Exit code", job.state === "done" ? String(job.exit_code) : "–", "", failed(job)),
    prop("OOM killed", job.oom_killed ? "yes" : "no", "", job.oom_killed),
    prop("Cancel requested", job.cancel_requested ? "yes" : "no"),
    prop("Error", job.error || "–", "", Boolean(job.error)),
    prop("Log", fmt(job.log_bytes), "bytes"),
    prop("Artifact", job.artifact_bytes ? fmt(job.artifact_bytes) : "none", job.artifact_bytes ? "bytes" : ""),
  ];
  $("props").replaceChildren(...rows);
  $("cancel").hidden = job.state === "done";
  const download = $("download");
  download.hidden = !(job.state === "done" && job.artifact_bytes > 0);
  download.textContent = "Download artifact (" + fmt(job.artifact_bytes) + " bytes)";
}

async function loadJob(id, signal) {
  const resp = await api("/jobs/" + id, { signal });
  if (!resp.ok) throw new Error("Job " + id + ": " + (await errorText(resp)));
  const job = await resp.json();
  if (id === view.selected) showJob(job);
  return job;
}

// select opens a job in Table 2. Each selection owns one AbortController
// from its first request, so opening another job, or closing this one,
// stops every request and stream this selection started: nothing late can
// write into another job's pane.
async function select(id) {
  if (view.follow) view.follow.abort();
  const ctl = new AbortController();
  view.follow = ctl;
  view.selected = id;
  $("detail").hidden = false;
  $("detail-status").textContent = "";
  $("log-state").textContent = "";
  $("log").replaceChildren();
  for (const row of $("jobs").rows) {
    row.classList.toggle("selected", row.querySelector(".id").textContent === String(id));
  }
  try {
    await loadJob(id, ctl.signal);
    const still = matchMedia("(prefers-reduced-motion: reduce)").matches;
    $("detail").scrollIntoView({ block: "start", behavior: still ? "auto" : "smooth" });
    await followLog(id, ctl);
  } catch (err) {
    if (ctl.signal.aborted || err instanceof Unauthorized) return;
    $("detail-status").textContent = err.message;
    $("log-state").textContent = "· stopped";
  }
}

// appendLog adds output as plain text, following the bottom if the reader
// was there. The API's "[forge: ...]" markers arrive in the same byte stream
// as the build's output, so a build can print a forged one; the page must not
// style any line as if the API wrote it.
function appendLog(pre, text) {
  const stuck = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 4;
  pre.append(text);
  if (stuck) pre.scrollTop = pre.scrollHeight;
}

// followLog reads the streamed /logs body as it arrives. EventSource cannot
// send the Authorization header, and the endpoint is plain text, not SSE.
async function followLog(id, ctl) {
  $("log-state").textContent = "· following";
  const resp = await api("/jobs/" + id + "/logs", { signal: ctl.signal });
  if (!resp.ok) throw new Error("Log: " + (await errorText(resp)));
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    appendLog($("log"), decoder.decode(value, { stream: true }));
  }
  appendLog($("log"), decoder.decode());
  // The API ends the stream when the job is done, but a proxy timeout or a
  // restart ends it too; only the job's own state says which.
  const job = await loadJob(id, ctl.signal);
  if (view.follow === ctl) view.follow = null;
  $("log-state").textContent = job.state === "done"
    ? "· complete"
    : "· disconnected while " + job.state + "; open the job again to resume";
  refresh();
}

const CANCEL_RESULT = {
  200: "Cancelled before it started: the job is done with exit code -1.",
  202: "Cancel requested. The job is still running; its worker kills it at the next heartbeat.",
  404: "No such job.",
  409: "The job had already finished; its result stands.",
};

// busy runs action with button disabled, and reports its message only if
// the same job is still open when it finishes.
async function busy(button, action) {
  const id = view.selected;
  const report = (text) => { if (view.selected === id) $("detail-status").textContent = text; };
  button.disabled = true;
  try {
    report(await action(id));
  } catch (err) {
    if (!(err instanceof Unauthorized)) report(err.message);
  } finally {
    button.disabled = false;
  }
}

async function cancel(id) {
  const resp = await api("/jobs/" + id + "/cancel", { method: "POST" });
  const message = CANCEL_RESULT[resp.status] || "Cancel failed: " + (await errorText(resp));
  refresh();
  return message;
}

// download fetches the tar with the header and saves it from a blob: the
// browser never navigates to the artifact URL, which needs the token.
async function download(id) {
  const resp = await api("/jobs/" + id + "/artifact");
  if (!resp.ok) return "Artifact: " + (await errorText(resp));
  const url = URL.createObjectURL(await resp.blob());
  const a = el("a");
  a.href = url;
  a.download = "job-" + id + ".tar";
  document.body.append(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
  return "Saved job-" + id + ".tar. Treat its contents as untrusted: it is the build's output.";
}

function closeDetail() {
  if (view.follow) view.follow.abort();
  view.follow = null;
  view.selected = 0;
  $("detail").hidden = true;
  for (const row of $("jobs").rows) row.classList.remove("selected");
}

function wire() {
  $("token-form").addEventListener("submit", (event) => {
    event.preventDefault();
    writeToken($("token").value.trim());
    $("token").value = "";
    $("token-form").hidden = true;
    say("");
    refresh();
    if (view.selected) select(view.selected);
  });
  $("forget").addEventListener("click", () => {
    writeToken("");
    closeDetail();
    $("jobs").replaceChildren();
    say("Token forgotten for this tab.");
    refresh();
  });
  for (const b of document.querySelectorAll(".seg button")) {
    b.addEventListener("click", () => setFilter(b.dataset.state));
  }
  $("older").addEventListener("click", () => page(true));
  $("newer").addEventListener("click", () => page(false));
  $("open-submit").addEventListener("click", () => toggleSubmit($("submit-form").hidden));
  $("close-submit").addEventListener("click", () => toggleSubmit(false));
  $("command").addEventListener("input", showArgs);
  $("submit-form").addEventListener("submit", submit);
  $("cancel").addEventListener("click", () => busy($("cancel"), cancel));
  $("download").addEventListener("click", () => busy($("download"), download));
  $("close-detail").addEventListener("click", closeDetail);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") refresh();
    else clearTimeout(view.timer);
  });
}

wire();
showTokenState();
refresh();
