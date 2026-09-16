/**
 * Explore surface browser e2e (T0802).
 *
 * Drives the REAL web app — a production `next build` served by `next start`
 * — in real Chromium, with the index coming over HTTP from the harness's API
 * origin (mock-api.mjs; see its header for why the Explore mock is a server
 * and not a `page.route`: the page reads the index server-side).
 *
 * What this file is responsible for proving:
 *
 *   1. The six tabs of docs/05 §6 exist, in order, and switch panels — by
 *      click and by the WAI-ARIA keyboard pattern — with exactly one panel
 *      visible at a time and focus following the selection.
 *   2. All six panels are in the SERVED HTML, with their rows, before any
 *      script runs: the index is a document, not a client-side fetch.
 *   3. The private project appears NOWHERE — not in the served HTML, not in
 *      the DOM, not as a placeholder where a withheld project would have
 *      been (docs/23 §3/§5).
 *   4. Nothing ranks by popularity: no like/star/score vocabulary, no badge
 *      on a tab, no count or total anywhere in the surface, and the rows are
 *      in the order the API sent (a client-side re-sort would fail here).
 *   5. Failure is not emptiness (docs/51): a 503 and a truncated 200 both
 *      render the error panel, never six empty tabs; a genuinely empty index
 *      renders the empty lines and not the error.
 *
 * Usage: node explore-e2e.mjs <web-base> <api-base>
 */

import { chromium } from "playwright";

const BASE = (process.argv[2] ?? "").replace(/\/$/, "");
const API = (process.argv[3] ?? "").replace(/\/$/, "");
if (!BASE || !API) {
  console.error("explore-e2e: usage: node explore-e2e.mjs <web-base> <api-base>");
  process.exit(2);
}

/* ---------- Assertions ---------- */

let failures = 0;
let checks = 0;

function ok(where, what) {
  checks += 1;
  console.log(`  ok   ${where}: ${what}`);
}

function fail(what, detail) {
  failures += 1;
  console.error(`  FAIL ${what}${detail ? ` — ${detail}` : ""}`);
}

function check(where, what, condition, detail) {
  if (condition) ok(where, what);
  else fail(`${where}: ${what}`, detail);
}

function eq(where, what, got, want) {
  check(where, what, JSON.stringify(got) === JSON.stringify(want), `got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
}

/* ---------- The expected index, written out ---------- */

/**
 * The public project's identity — asserted PRESENT, and absent again for the
 * private project below. These constants are duplicated from mock-api.mjs on
 * purpose: the spec states what it expects to see rather than asking the
 * mock what it sent, so a change to the fixture that the spec does not know
 * about fails instead of silently agreeing.
 */
const OPEN = {
  id: "01j9z6k3m4n5p6q7r8s9t0v1w01",
  slug: "open-materials-lab",
  name: "Open Materials Lab",
};

/** The private project: none of these three strings may appear anywhere. */
const HIDDEN = {
  id: "01j9z6k3m4n5p6q7r8s9t0v1w02",
  slug: "withheld-internal-lab",
  name: "Withheld Internal Lab",
};

const TABS = [
  ["projects", "Projects"],
  ["assets", "Assets"],
  ["knowledge", "Knowledge"],
  ["people", "People"],
  ["organizations", "Organizations"],
  ["contributions", "Open Contributions"],
];

/** Each panel's rows, in the API's order (newest first), as the DOM exposes them. */
const ROW_ORDER = [
  ["projects", "[data-explore-project-row]", ["catalyst-screening-group", "open-materials-lab"]],
  ["assets", "[data-explore-asset-row]", ["01j9z6k3m4n5p6q7r8s9t0v1w11", "01j9z6k3m4n5p6q7r8s9t0v1w12"]],
  ["knowledge", "[data-explore-knowledge-row]", ["01j9z6k3m4n5p6q7r8s9t0v1w21", "01j9z6k3m4n5p6q7r8s9t0v1w22"]],
  ["people", "[data-explore-person-row]", ["alice-chen", "bob-ioannidis"]],
  ["organizations", "[data-explore-organization-row]", ["helix-institute", "open-foundry"]],
  [
    "contributions",
    "[data-explore-contribution-row]",
    ["01j9z6k3m4n5p6q7r8s9t0v1w61", "01j9z6k3m4n5p6q7r8s9t0v1w62"],
  ],
];

/**
 * The words that would turn "this row has no project" into "this row's
 * project is hidden" — the sentence docs/23 §3 keeps off the wire, which a
 * front end must not invent out of a null. Checked against the RENDERED row
 * text and the surface's own HTML, never against the whole document: the
 * product does use words like "restricted" elsewhere (rights tokens on an
 * asset page), and a check that cannot tell those apart from a leak here is
 * a check that will be relaxed the first time it cries wolf.
 *
 * The bare em dash is in the list because a dash is the classic gap filler
 * for a withheld field. It is checked in rows only, for the same reason.
 */
const NO_PLACEHOLDER = ["hidden project", "private project", "withheld", "undisclosed"];
const NO_GAP_FILLER = ["—", "–", "n/a", "not available"];

/**
 * The vocabulary of a popularity ranking (docs/05 §6 forbids 点赞数 as the
 * core ranking). Checked against rendered values and attribute NAMES — the
 * two places such a number would have to appear, whether rendered or hidden
 * for a client-side sort.
 */
const POPULARITY = /\b(likes?|stars?|upvotes?|downvotes?|votes?|popularity|trending|score|ranking|hot|favorites?|bookmarks?)\b/i;

/* ---------- Mock control ---------- */

async function setMode(mode) {
  const res = await fetch(`${API}/__mode?set=${mode}`);
  if (!res.ok) throw new Error(`mock mode ${mode}: HTTP ${res.status}`);
  return res.json();
}

async function stats() {
  const res = await fetch(`${API}/__stats`);
  if (!res.ok) throw new Error(`mock stats: HTTP ${res.status}`);
  return res.json();
}

/* ---------- The run ---------- */

console.log(`explore-e2e: web ${BASE}, api ${API}`);

await setMode("ok");

/* --- 1. The served HTML: a document, and no private identity in it --- */

const served = await (await fetch(`${BASE}/explore`)).text();

check("served HTML", "the index is rendered server-side (public project named)", served.includes(OPEN.name));
check(
  "served HTML",
  "all six panels are in the HTML before any script runs",
  TABS.every(([tab]) => served.includes(`data-explore-panel="${tab}"`)),
  `missing one of ${TABS.map(([t]) => t).join(", ")}`,
);
check(
  "served HTML",
  "every section's rows are in the HTML",
  ["catalyst-screening-group", "01j9z6k3m4n5p6q7r8s9t0v1w12", "alice-chen", "helix-institute", "01j9z6k3m4n5p6q7r8s9t0v1w62"].every(
    (needle) => served.includes(needle),
  ),
);
for (const [what, needle] of [
  ["name", HIDDEN.name],
  ["slug", HIDDEN.slug],
  ["id", HIDDEN.id],
]) {
  check("served HTML", `the private project's ${what} is not in the served HTML`, !served.includes(needle), needle);
}
// The served HTML is the whole document (chrome, RSC payload and all), so
// the placeholder vocabulary is checked against the surface's own markup
// further down, where the scope is exact.

const beforeLoad = (await stats()).exploreReads;
check("served HTML", "the page really read the index over HTTP", beforeLoad >= 1, `reads=${beforeLoad}`);

/* --- 2. The browser --- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 1000 } });
const page = await context.newPage();
// A hydration crash must fail the run loudly rather than leave the page
// server-rendered and inert (the tabs would still "look" right).
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));

await page.goto(`${BASE}/explore`, { waitUntil: "load" });
await page.waitForSelector('[role="tablist"]');

const tablist = page.locator('[role="tablist"]');
eq(
  "tabs",
  "the six dimensions are tabs, in the spec's order and with its labels",
  await tablist.locator('[role="tab"]').evaluateAll((els) => els.map((el) => el.getAttribute("data-explore-tab"))),
  TABS.map(([tab]) => tab),
);
eq(
  "tabs",
  "each tab's label is the dimension name, and a tab carries no badge",
  await tablist.locator('[role="tab"]').allTextContents().then((texts) => texts.map((t) => t.trim())),
  TABS.map(([, label]) => label),
);
eq(
  "tabs",
  "all six panels are present in the DOM",
  await page.locator("[data-explore-panel]").evaluateAll((els) => els.map((el) => el.getAttribute("data-explore-panel"))),
  TABS.map(([tab]) => tab),
);

/** The one visible panel, or null when the exclusivity invariant is broken. */
async function visiblePanels() {
  return page.evaluate(() =>
    [...document.querySelectorAll("[data-explore-panel]")]
      .filter((el) => !el.hasAttribute("hidden") && el.offsetParent !== null)
      .map((el) => el.getAttribute("data-explore-panel")),
  );
}

async function assertOnly(where, tab) {
  eq(where, `only the ${tab} panel is visible`, await visiblePanels(), [tab]);
  eq(
    where,
    `only the ${tab} tab is selected`,
    await page.locator('[role="tab"][aria-selected="true"]').evaluateAll((els) =>
      els.map((el) => el.getAttribute("data-explore-tab")),
    ),
    [tab],
  );
}

// The ARIA tab pattern's wiring: a tab points at its panel, and the panel
// points back — the two directions that make a tablist navigable at all.
const wiring = await page.evaluate(() =>
  [...document.querySelectorAll('[role="tab"]')].map((tab) => {
    const panel = document.getElementById(tab.getAttribute("aria-controls"));
    return {
      controlsExists: panel !== null,
      labelsBack: panel !== null && panel.getAttribute("aria-labelledby") === tab.id,
      role: panel !== null ? panel.getAttribute("role") : null,
    };
  }),
);
check(
  "tabs",
  "each tab controls a panel that labels itself back with the tab",
  wiring.every((w) => w.controlsExists && w.labelsBack && w.role === "tabpanel"),
  JSON.stringify(wiring),
);

await assertOnly("default", "projects");
check(
  "default",
  "the projects panel holds the rows the API sent, in its order",
  (await page.locator("[data-explore-project-row]").count()) === 2,
);

/* --- 3. Clicking switches panels, exclusively, in both directions --- */

for (const [tab] of TABS) {
  await page.click(`[data-explore-tab="${tab}"]`);
  await assertOnly("click", tab);
}
// Back the other way: a tablist that only ever moves forward is broken in a
// way a one-directional loop cannot see.
for (const [tab] of [...TABS].reverse()) {
  await page.click(`[data-explore-tab="${tab}"]`);
  await assertOnly("click (reverse)", tab);
}

/* --- 4. The keyboard pattern, with focus following the selection --- */

await page.focus('[data-explore-tab="projects"]');
await page.keyboard.press("ArrowRight");
await assertOnly("keyboard", "assets");
eq(
  "keyboard",
  "focus follows the selection",
  await page.evaluate(() => document.activeElement?.getAttribute("data-explore-tab")),
  "assets",
);
await page.keyboard.press("End");
await assertOnly("keyboard", "contributions");
eq(
  "keyboard",
  "End moves focus to the last tab",
  await page.evaluate(() => document.activeElement?.getAttribute("data-explore-tab")),
  "contributions",
);
await page.keyboard.press("ArrowRight");
await assertOnly("keyboard", "projects");
await page.keyboard.press("Home");
await assertOnly("keyboard", "projects");
await page.keyboard.press("ArrowLeft");
await assertOnly("keyboard", "contributions");
await page.keyboard.press("ArrowLeft");
await assertOnly("keyboard", "organizations");

/* --- 5. Rows: order, withheld projects, and no ranking vocabulary --- */

for (const [tab, selector, expected] of ROW_ORDER) {
  const got = await page.locator(selector).evaluateAll((els) =>
    els.map((el) => el.getAttribute([...el.attributes].find((a) => a.name.startsWith("data-explore-"))?.name ?? "")),
  );
  eq(`rows:${tab}`, "the panel renders the API's rows in the API's order", got, expected);
}

// A project is named only where the API named one: the asset, the knowledge
// row and the opportunity of the PUBLIC project. The two withheld rows carry
// no project element at all.
eq(
  "rows:projects named",
  "a project is rendered exactly where the API sent one",
  await page.locator("[data-explore-project]").evaluateAll((els) => els.map((el) => el.getAttribute("data-explore-project"))),
  [OPEN.slug, OPEN.slug, OPEN.slug],
);

// The ROWS, not the prose: the surface's own subtitle says "no popularity
// anywhere in it", and a check that fails on the copy that makes the promise
// is a check that will be loosened rather than fixed. What must be absent is
// a popularity VALUE on a row — and a field one could hide in.
const rowText = await page.locator(".explore-row").allInnerTexts();
check("ranking", "the rows carry no popularity vocabulary", rowText.length > 0 && !POPULARITY.test(rowText.join("\n")), rowText.join(" | ").slice(0, 300));
const attributeNames = await page.locator(".explore [data-explore-panel], .explore [data-explore-tab], .explore-row").evaluateAll((els) => [
  ...new Set(els.flatMap((el) => [...el.attributes].map((a) => a.name))),
]);
check(
  "ranking",
  "no element in the surface carries a popularity field",
  !attributeNames.some((name) => POPULARITY.test(name)),
  attributeNames.join(", "),
);
check(
  "ranking",
  "no count or total is rendered",
  (await page.locator(".explore [data-explore-count], .explore [data-explore-total]").count()) === 0,
);
check(
  "ranking",
  "the surface offers no sort control",
  (await page.locator(".explore select, .explore [data-explore-sort]").count()) === 0,
);

const domText = await page.evaluate(() => document.documentElement.outerHTML);
for (const [what, needle] of [
  ["name", HIDDEN.name],
  ["slug", HIDDEN.slug],
  ["id", HIDDEN.id],
]) {
  check("ranking", `the private project's ${what} is not in the rendered DOM`, !domText.includes(needle), needle);
}
const surfaceHTML = (await page.locator(".explore").evaluate((el) => el.outerHTML)).toLowerCase();
check(
  "withheld",
  "nothing in the surface says a project was withheld",
  !NO_PLACEHOLDER.some((needle) => surfaceHTML.includes(needle)),
  NO_PLACEHOLDER.filter((needle) => surfaceHTML.includes(needle)).join(", "),
);
check(
  "withheld",
  "the withheld rows are rendered whole, with no gap filler where a project would be",
  rowText.length > 0 && !NO_GAP_FILLER.some((needle) => rowText.join("\n").toLowerCase().includes(needle)),
  rowText.join(" | ").slice(0, 300),
);
// The two rows whose project the API withheld: they must be present (the
// entity is public — only its project is not) and must carry no project.
eq(
  "withheld",
  "the withheld rows exist, and carry no project element at all",
  await page.locator('[data-explore-project]').count(),
  3,
);

/* --- 6. Failure is not emptiness (docs/51) --- */

await setMode("unavailable");
const readsBefore503 = (await stats()).exploreReads;
await page.goto(`${BASE}/explore`, { waitUntil: "load" });
/**
 * The error panel, or an empty reading of it when there is none — read
 * through this helper rather than directly, so a MISSING panel fails the
 * checks below instead of throwing and aborting the rest of the run. A
 * failure state that stops the harness at its first symptom hides the other
 * four things that are also wrong with it.
 */
async function errorPanel() {
  const alerts = await page.locator('[role="alert"][data-explore-error]').count();
  if (alerts !== 1) return { alerts, code: "", message: "" };
  const panel = page.locator('[role="alert"][data-explore-error]');
  return { alerts, code: (await panel.getAttribute("data-explore-error")) ?? "", message: (await panel.innerText()) ?? "" };
}

const alert503 = await errorPanel();
check("503", "a failed index renders an error panel", alert503.alerts === 1, `found ${alert503.alerts}`);
eq("503", "the error names the API's wire code", alert503.code, "EXPLORE_UNAVAILABLE");
check("503", "the error is a real sentence, not an empty box", alert503.message.trim().length > 20, alert503.message);
check(
  "503",
  "the failure is NOT drawn as six empty tabs",
  (await page.locator("[data-explore-panel]").count()) === 0 && !(await page.content()).includes("No public projects yet."),
);
check(
  "503",
  "the page re-read the index instead of serving a cached one",
  (await stats()).exploreReads > readsBefore503,
  `reads=${(await stats()).exploreReads} before=${readsBefore503}`,
);
// Two identical loads must read twice: `no-store` is what keeps a
// publication from being invisible until the cache expires.
await page.goto(`${BASE}/explore`, { waitUntil: "load" });
check("503", "the index is read per request, never cached across requests", (await stats()).exploreReads > readsBefore503);

// A truncated 200 (a section missing) is a failure too — the shape guard's
// whole purpose: an absent section must not render as an empty one.
await setMode("malformed");
await page.goto(`${BASE}/explore`, { waitUntil: "load" });
const alertMalformed = await errorPanel();
check("malformed", "a body missing a section renders the error panel", alertMalformed.alerts === 1, `found ${alertMalformed.alerts}`);
eq(
  "malformed",
  "a truncated answer is a failure, not five tabs and one empty one",
  alertMalformed.code,
  "MALFORMED_INDEX",
);
check("malformed", "no panel is rendered for a truncated answer", (await page.locator("[data-explore-panel]").count()) === 0);

// ... and the other direction: an index that really is empty is rendered as
// empty, never as an error.
await setMode("empty");
await page.goto(`${BASE}/explore`, { waitUntil: "load" });
await page.waitForSelector("[data-explore-panel]");
const alertsInSurface = await page.locator('.explore [role="alert"]').count();
const alertsOutside = await page
  .locator('[role="alert"]')
  .evaluateAll((els) => els.filter((el) => !el.closest(".explore")).map((el) => el.outerHTML.slice(0, 120)));
check("empty", "an empty index is not an error", alertsInSurface === 0, `alerts in the surface: ${alertsInSurface}`);
// Reported either way: the check below is scoped to the surface, and the
// scope is only honest if the log says what it excluded.
console.log(`  info empty: role="alert" outside the surface: ${alertsOutside.length === 0 ? "none" : alertsOutside.join(" | ")}`);
check(
  "empty",
  "no alert outside the surface claims the index failed",
  alertsOutside.every((html) => !html.includes("data-explore-error")),
  alertsOutside.join(" | "),
);
eq("empty", "an empty index still has all six panels", await page.locator("[data-explore-panel]").count(), 6);
const emptyLines = await page.locator(".explore-empty").allTextContents();
eq("empty", "every section says so in words", emptyLines.length, 6);
check(
  "empty",
  "the empty sections say nothing was published, not that something failed",
  emptyLines.every((line) => /no .*(yet|right now)/i.test(line)),
  JSON.stringify(emptyLines),
);

/* ---------- Done ---------- */

await browser.close();

console.log(`explore-e2e: ${checks - failures}/${checks} checks passed`);
if (failures > 0) {
  console.error(`explore-e2e: ${failures} FAILED`);
  process.exit(1);
}
console.log("explore-e2e: all passed");
