// Regression tests for the dashboard.
//
// Four bugs these exist to prevent:
//   1. the 3-second status poll reloaded every tab (shared iframe + src reset);
//   2. switching tabs destroyed the AI chat (same cause);
//   3. embedding kiwix in an iframe broke link clicks - kiwix serves articles with
//      a CSP `sandbox`, and nesting produces nested sandboxes where links do
//      nothing and pages render blank;
//   4. window.open() was suppressed as a popup in Edge, so the cards did nothing.
//
// Sections are now plain <a target="_blank"> links, which no popup blocker refuses.
//
// Run:  node internal/launcher/ui/reload_test.js
"use strict";

const fs = require("fs");
const path = require("path");

const html = fs.readFileSync(path.join(__dirname, "index.html"), "utf8");
const match = html.match(/<script>([\s\S]*?)<\/script>/);
if (!match) {
  console.error("FAIL: no <script> block in index.html");
  process.exit(1);
}

function classList() {
  const set = new Set();
  return {
    add: (c) => set.add(c),
    remove: (c) => set.delete(c),
    toggle: (c, on) => (on ? set.add(c) : set.delete(c)),
    contains: (c) => set.has(c),
  };
}

function makeEl(id) {
  const attrs = {};
  return {
    id,
    textContent: "",
    className: "",
    title: "",
    dataset: {},
    _children: {},
    addEventListener() {},
    setAttribute: (k, v) => { attrs[k] = v; },
    getAttribute: (k) => attrs[k],
    removeAttribute: (k) => { delete attrs[k]; },
    hasAttribute: (k) => k in attrs,
    querySelector(sel) {
      return (this._children[sel] ||= makeEl(sel));
    },
    classList: classList(),
  };
}

const status = {
  starting: false,
  ram_gib: 16,
  model: "gemma-4-E4B",
  model_gib: 4.6,
  context: 8192,
  backend: "llama-vulkan",
  zims: 25,
  books: {
    wikipedia: "wikipedia_en_all_nopic_2026-06",
    medlineplus: "medlineplus.gov_en_all_2025-01",
    diy: "diy.stackexchange.com_en_all_2026-10",
  },
  kiwix: { up: true, url: "http://127.0.0.1:1111" },
  llama: { up: true, url: "http://127.0.0.1:2222" },
};

const els = {};
const views = ["ai", "wiki", "medical", "repair", "search"];
const cards = views.map((v) => {
  const c = makeEl(v);
  c.dataset.view = v;
  return c;
});

global.document = {
  getElementById: (id) => (els[id] ||= makeEl(id)),
  querySelectorAll: (sel) => (sel.includes(".card") ? cards : []),
};
global.setInterval = () => 0;
global.fetch = async () => ({ json: async () => status });

// eslint-disable-next-line no-eval
eval(match[1] + "\n;globalThis.__t = { refresh, target, chrome, state };");
const t = globalThis.__t;

function fail(msg) {
  console.error("FAIL: " + msg);
  process.exit(1);
}

(async () => {
  // --- no iframes: nesting kiwix breaks link clicks -------------------------
  if (/<iframe/i.test(html)) fail("index.html still embeds an iframe");

  // --- cards must be real links, not window.open ----------------------------
  // Strip comments: the source explains the old bug by name.
  const code = match[1].replace(/\/\/[^\n]*/g, "").replace(/\/\*[\s\S]*?\*\//g, "");
  if (/window\.open\s*\(/.test(code)) fail("dashboard still uses window.open (popup-blockable)");
  const cardTags = html.match(/<a class="card[^"]*"[^>]*>/g) || [];
  if (cardTags.length !== views.length) fail(`found ${cardTags.length} card links, want ${views.length}`);
  for (const tag of cardTags) {
    if (!/target="_blank"/.test(tag)) fail("card without target=_blank: " + tag);
    if (!/rel="[^"]*noopener/.test(tag)) fail("card without rel=noopener: " + tag);
  }

  await t.refresh();
  await t.refresh();
  await t.refresh();

  // --- every section resolves to a top-level URL, and the card carries it ---
  for (const view of views) {
    const r = t.target(view, status);
    if (!r.url) fail(`${view} has no target URL`);
    if (view === "search" && r.url.includes("/search?pattern=")) fail("Search points at an empty query");
    const card = cards.find((c) => c.dataset.view === view);
    if (card.getAttribute("href") !== r.url) fail(`${view} card href = ${card.getAttribute("href")}, want ${r.url}`);
    if (card.classList.contains("disabled")) fail(`${view} card is disabled but its service is up`);
  }

  // --- polling must not change the links under the user --------------------
  const before = cards.map((c) => c.getAttribute("href"));
  await t.refresh();
  const after = cards.map((c) => c.getAttribute("href"));
  if (before.join("|") !== after.join("|")) fail("status polling changed the card links");

  // --- AI down: its card is disabled, library cards still work -------------
  const offline = JSON.parse(JSON.stringify(status));
  offline.llama = { up: false, error: "vulkan failed" };
  t.state.status = offline;
  t.chrome();
  const aiCard = cards.find((c) => c.dataset.view === "ai");
  if (aiCard.getAttribute("href")) fail("AI card has a link while the AI is down");
  if (!aiCard.classList.contains("disabled")) fail("AI card not marked disabled");
  for (const view of ["wiki", "medical", "repair", "search"]) {
    const card = cards.find((c) => c.dataset.view === view);
    if (!card.getAttribute("href")) fail(`${view} lost its link because the AI is down`);
  }

  // --- library still loading: nothing is offered, nothing is broken --------
  const starting = JSON.parse(JSON.stringify(status));
  starting.starting = true;
  starting.kiwix = { up: false };
  starting.llama = { up: false };
  for (const view of ["wiki", "search"]) {
    const r = t.target(view, starting);
    if (r.url) fail(`${view} offered a link before the library was ready`);
    if (r.reason !== "starting") fail(`${view} reason = ${r.reason}, want starting`);
  }

  // --- AI on demand: the library never waits for the model -----------------
  const demand = JSON.parse(JSON.stringify(status));
  demand.llama = { up: false };
  demand.ai_on_demand = true;
  demand.ai_can_start = true;
  demand.ai_reason = "this machine has 16 GiB of RAM";
  t.state.status = demand;
  t.chrome();
  const demandCard = cards.find((c) => c.dataset.view === "ai");
  if (demandCard.getAttribute("href") !== "#start-ai") fail("AI card should offer to start the assistant");
  if (demandCard.classList.contains("disabled")) fail("AI card is disabled although it can start");
  if (demandCard.querySelector(".action").textContent !== "Start assistant") fail("AI card action text wrong");

  // The library must be unaffected by any AI state.
  for (const view of ["wiki", "medical", "repair", "search"]) {
    const card = cards.find((c) => c.dataset.view === view);
    if (!card.getAttribute("href")) fail(`${view} lost its link because the AI is deferred`);
  }

  console.log("PASS: links not popups; no iframes; poll is inert; AI on demand; library independent");
})();
