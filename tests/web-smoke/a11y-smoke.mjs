/**
 * A11y smoke (T0107, T1104): two independent strands of evidence —
 *
 *  A. axe-core scans over the representative routes at desktop and mobile
 *     widths, in two passes: the WCAG 2 A/AA tags, and the best-practice
 *     rules — the structural checks (duplicate landmarks, heading order,
 *     missing h1) that the WCAG tags leave out and that this harness must
 *     be able to turn red on. Zero violations on every route in both passes.
 *  B. A keyboard walkthrough in Chromium: the skip link is the first tab
 *     stop and is visibly exposed on focus, Tab reaches every header
 *     control in DOM order with a visible focus indicator, the search form
 *     submits by keyboard alone, nav links activate with Enter, and the
 *     mobile drawer opens/focuses/walks/closes entirely by keyboard.
 *
 * Usage: node a11y-smoke.mjs <base-url> [harness-ids.json]
 *
 *   With no second argument (the `run.sh` / `make browser-smoke` path) only
 *   the static routes are scanned. That is the "服务关" mode: run.sh's own
 *   note records that the smoke pages render with the services down, so a
 *   dynamic route served by a cold `next start` is a shell or an error
 *   state. Scanning a shell and calling it a core page is the one way this
 *   suite could pass while measuring nothing, so the dynamic routes are not
 *   scanned here at all rather than scanned and counted.
 *
 *   With a harness-ids.json (the `make a11y` / run-a11y.sh path) the dynamic
 *   docs/42 core pages are scanned against the real API, with the fixture
 *   ids that file carries. Each of those routes must then PROVE it has data:
 *   the fixture's own content marker has to be in the rendered DOM, and the
 *   shell's error/not-found markers must not be. A route that renders its
 *   error state fails the run instead of being counted as a pass.
 */
import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";
import axe from "axe-core";

const BASE = process.argv[2] ?? "http://127.0.0.1:31107";
const IDS_PATH = process.argv[3] ?? "";

const IDS = (() => {
  if (IDS_PATH === "") return null;
  try {
    return JSON.parse(fs.readFileSync(IDS_PATH, "utf8"));
  } catch (err) {
    console.error(`a11y-smoke: cannot read harness ids from ${IDS_PATH}: ${err.message}`);
    process.exit(2);
  }
})();

let fails = 0;
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
/* ok() takes a LABEL AND NOTHING ELSE — same guard, same reason as the copy
 * in visual-smoke.mjs (T1112): a second argument to a `(label) =>` signature
 * was silently discarded, and calls shaped like assertions therefore could
 * never fail. Any second argument is now reported as a failed check naming
 * the call. See that file for the full note. */
function ok(label, ...extra) {
  if (extra.length > 0) {
    fail(
      `ok(${JSON.stringify(label)}, …) called with ${extra.length + 1} arguments`,
      "ok takes a label only — a second argument is discarded, so this line could never fail. Write it as an if/else that calls fail(label, detail).",
    );
    return;
  }
  console.log(`ok   ${label}`);
}

const browser = await chromium.launch();

/* ---------- A. axe-core scans ---------- */

const WCAG_TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];
const STRUCTURAL_TAGS = ["best-practice"];

async function axeScan(page, label, pass, runOnly) {
  await page.addScriptTag({ content: axe.source });
  const violations = await page.evaluate(async (runOnly) => {
    const r = await window.axe.run(document, { runOnly });
    return r.violations.map((v) => ({
      id: v.id,
      impact: v.impact,
      help: v.help,
      targets: v.nodes.map((n) => n.target.join(" ")),
      // A selector alone ("#_r_0_") is not a diagnosis; the offending
      // element's own HTML is. Printed under the failure, never used to
      // decide whether the violation counts.
      html: (v.nodes[0]?.html ?? "").slice(0, 220),
    }));
  }, runOnly);
  if (violations.length === 0) {
    ok(`axe ${pass} ${label}: 0 violations`);
    return [];
  }
  fail(`axe ${pass} ${label}: ${violations.length} violation(s)`);
  for (const v of violations) {
    console.log(`     - ${v.id} [${v.impact}] ${v.help} @ ${v.targets.join(" | ")}`);
    if (v.html) console.log(`       html: ${v.html}`);
  }
  return violations;
}

// Scan result recorded per route for the coverage table.
const scanResults = [];

function routeURL(route, ids) {
  if (ids === null) return route;
  return route
    .replace(/{project_id}/g, ids.project_id)
    .replace(/{pr_number}/g, String(ids.pr_number))
    .replace(/{release_id}/g, ids.release_id)
    .replace(/{asset_pid}/g, ids.asset_pid)
    .replace(/{asset_version}/g, ids.asset_version)
    .replace(/{user_id}/g, ids.user_id);
}

/* The shell's own words for "this route did not render the page". They come
 * from project-shell.tsx (`data-project-notfound`, "Project not found",
 * "Project unavailable") — the states a cold `next start` with the API down
 * falls back to. A route that shows one of these has not been scanned as a
 * core page, whatever axe says about it. */
const SHELL_MARKERS = [
  "Project not found",
  "Project unavailable",
  "Loading project",
];

/* The search page is the case that makes a text marker useless on its own.
 * page.tsx renders `<span data-search-query>{query}</span>` from the URL
 * BEFORE any script runs — its own comment says the query "is the one part of
 * the page that is true without the API". So "catalyst" is on that page with
 * the API up, with it down, and with no session at all, and a check keyed on
 * it would have reported 有数据 for a page showing an error banner. The
 * answer itself is what proves the page: search-answer.tsx writes
 * `data-search-state="ready"` and `data-search-answer` only when an answer
 * came back. */
const SHELL_SELECTORS = ['[data-search-state="error"]'];

/* `data` is the marker that proves a route rendered the fixture's content —
 * not a shell. The strings are the harness's seeded titles
 * (tests/web-smoke/a11y-harness/main.go): they can only be on the page if the
 * page really fetched from the API, so the "有数据" column is a measurement
 * rather than a label this file asserts about itself.
 *
 * A route with `data: null` is static: it has no fixture to prove, and its
 * content (nav, header, search query) is there either way. */
const routeTemplates = [
  { route: "/", core: false, data: null },
  { route: "/projects", core: false, data: null },
  // The answer, not the query echo: `data-search-answer` exists only in the
  // ready state (see SHELL_SELECTORS above).
  { route: "/search?q=catalyst", core: true, data: { selector: "[data-search-answer]" } },
  { route: "/login", core: false, data: null },
  ...(IDS !== null ? [
    { route: "/projects/{project_id}", core: true, data: { text: "A11Y Test Project" } },
    { route: "/projects/{project_id}/research", core: true, data: { text: "A11Y Test Project" } },
    { route: "/projects/{project_id}/pulls/{pr_number}", core: true, data: { text: "A11Y Test PR" } },
    { route: "/projects/{project_id}/releases/{release_id}", core: true, data: { text: "A11Y Test Release" } },
    { route: "/assets/{asset_pid}/{asset_version}", core: true, data: { text: "A11Y Test Asset" } },
    { route: "/users/{user_id}", core: true, data: { text: "a11y-owner" } },
  ] : []),
];

const scans = [];
for (const t of routeTemplates) {
  const route = routeURL(t.route, IDS);
  scans.push([route, 1280, 800, t.core, t.data, t.route]);
  if (route === "/") {
    scans.push([route, 375, 667, t.core, t.data, t.route]);
  }
}

/* Proof that the route rendered the fixture's content rather than a shell.
 * Returns true only when the marker is present AND no shell marker is. A
 * false result is a failed check, so a route that silently degraded to its
 * error state cannot be counted as a core page that passed. */
async function hasContent(page, label, marker) {
  if (marker === null) return true;
  // page.evaluate takes ONE argument, so the two lists travel as one object.
  const probe = await page.evaluate(({ shellMarkers, shellSelectors }) => {
    const text = document.body.innerText;
    const hit = shellSelectors.find((s) => document.querySelector(s) !== null) ?? null;
    // The matched element's own attributes are the diagnosis (for search:
    // the wire code in data-search-error), so the failure names the reason
    // instead of only saying "there was an error".
    return {
      text,
      shellText: shellMarkers.find((m) => text.includes(m)) ?? null,
      shellSelector: hit,
      shellDetail: hit === null ? null : (document.querySelector(hit)?.outerHTML ?? "").slice(0, 300),
    };
  }, { shellMarkers: SHELL_MARKERS, shellSelectors: SHELL_SELECTORS });
  /* A shell state is NOT a failed check here, and the reason matters: the
   * axe scans below still run and are still required to be zero, and the
   * coverage table records this route as 服务关 rather than 有数据. Making
   * it a per-route failure would say the run found an accessibility problem
   * it did not find; making it a silent pass is the thing this whole file
   * exists to prevent. What guards against a page quietly degrading is the
   * FLOOR at the end of the run, which is a count over the routes that
   * reached 有数据 — if one of them stops, the count drops and the run goes
   * red naming it. */
  if (probe.shellText !== null || probe.shellSelector !== null) {
    const what = probe.shellText !== null ? JSON.stringify(probe.shellText) : probe.shellSelector;
    return { ok: false, reason: `${what} — ${probe.shellDetail ?? ""}` };
  }
  const described = marker.text !== undefined ? JSON.stringify(marker.text) : marker.selector;
  const present =
    marker.text !== undefined
      ? probe.text.includes(marker.text)
      : await page.evaluate((s) => document.querySelector(s) !== null, marker.selector);
  if (!present) {
    return { ok: false, reason: `content marker ${described} is absent` };
  }
  ok(`content ${label}: rendered ${described}`);
  return { ok: true, reason: null };
}

/* POST /api/v1/search runs behind the auth guard (specs/api's global
 * security), and the page's own client sends `credentials: "include"`. A
 * browser that has never signed in therefore gets 401 no matter how healthy
 * the API is, and /search would keep failing its content check for a reason
 * that has nothing to do with accessibility. Signing in here uses the
 * product's own login route with the fixture's real credentials — no test
 * backdoor, and the same session cookie the app's fetches already rely on.
 *
 * The fetch runs from a page ON the web origin, and `browser.newPage()` is a
 * fresh context, so it has to happen on the very page that will be scanned. */
async function signIn(page, ids) {
  await page.goto(BASE, { waitUntil: "load" });
  // A login that cannot even be attempted (API down, DNS, connection
  // refused) must be REPORTED as a failed check, not thrown: a throw here
  // aborts the run before it prints the per-route table, so an all-down
  // stack would look like a crashed script instead of a red gate.
  let res;
  try {
    res = await page.evaluate(
      async ({ api, email, password, key }) => {
        const r = await fetch(`${api}/api/v1/auth/login`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ email, password }),
        });
        const text = await r.text();
        let token = null;
        try {
          token = JSON.parse(text).csrf_token ?? null;
        } catch {
          token = null;
        }
        /* The session cookie alone is not enough: POST /search is a state
         * change and the guard requires the session-bound CSRF token in
         * X-CSRF-Token (lib/auth.ts sends it from sessionStorage under
         * "post.csrf", raw, no encoding). Without this the page answers
         * CSRF_FAILED and /search would look broken for a reason that has
         * nothing to do with accessibility. Writing the same value to the
         * same place is what the app's own login does. */
        if (token !== null) window.sessionStorage.setItem(key, token);
        return { status: r.status, body: text.slice(0, 200), token: token !== null };
      },
      { api: ids.api_base, email: ids.user_email, password: ids.password, key: "post.csrf" },
    );
  } catch (err) {
    res = { status: 0, body: `login request threw: ${err.message}`, token: false };
  }
  if (res.status !== 200 || !res.token) {
    fail("session: sign in before scanning /search", `POST /api/v1/auth/login -> ${res.status} ${res.body}`);
  }
  return res.status === 200 && res.token;
}

for (const [route, w, h, core, hasData, template] of scans) {
  const page = await browser.newPage({ viewport: { width: w, height: h } });
  if (IDS !== null && route.startsWith("/search")) await signIn(page, IDS);
  await page.goto(BASE + route, { waitUntil: "load" });
  // Let hydration finish so the client nav and session state are settled.
  await page.waitForTimeout(800);
  const content = await hasContent(page, `${route} @${w}x${h}`, hasData);
  const wcagV = await axeScan(page, `${route} @${w}x${h}`, "wcag", { type: "tag", values: WCAG_TAGS });
  const bpV = await axeScan(page, `${route} @${w}x${h}`, "best-practice", { type: "tag", values: STRUCTURAL_TAGS });
  scanResults.push({
    route,
    template,
    core,
    hasData,
    contentOK: content.ok,
    contentReason: content.reason,
    width: w,
    wcag: wcagV.length,
    bestPractice: bpV.length,
  });
  await page.close();
}

/* ---------- B. Keyboard walkthrough ---------- */

const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await page.goto(BASE, { waitUntil: "load" });
await page.waitForSelector(".global-nav-link", { state: "attached" });

const active = (pg) => pg.evaluate(() => {
  const el = document.activeElement;
  if (!el || el === document.body) return null;
  // Accessible name: aria-label, else the associated <label>, else text.
  const labelled =
    el.getAttribute("aria-label") ??
    (el.id
      ? document.querySelector(`label[for="${CSS.escape(el.id)}"]`)?.textContent
      : null) ??
    el.textContent ??
    "";
  return {
    tag: el.tagName,
    name: labelled.trim().slice(0, 40),
    href: el.getAttribute("href"),
    cls: el.className,
  };
});

// 1. Skip link is the first tab stop and becomes visible on focus.
await page.keyboard.press("Tab");
let a = await active(page);
if (a?.cls?.includes?.("skip-link") !== true) {
  fail("keyboard: first tab stop is the skip link", JSON.stringify(a));
} else {
  const exposed = await page.locator(".skip-link").evaluate((el) => {
    const s = getComputedStyle(el);
    return s.transform === "none" && s.visibility !== "hidden";
  });
  if (!exposed) fail("keyboard: skip link is visible while focused");
  else ok("keyboard: skip link is the first tab stop and visible on focus");
}

// 2. Activating the skip link lands on the main landmark.
await page.keyboard.press("Enter");
const onMain = await page.evaluate(() => {
  const el = document.activeElement;
  return el?.tagName === "MAIN" && el.id === "main";
});
if (!onMain) {
  fail("keyboard: Enter on skip link focuses #main", JSON.stringify(await active(page)));
} else {
  ok("keyboard: Enter on skip link focuses #main");
}

// 3. Tab order through the header, with a visible focus indicator each stop.
// A plain reload would keep the #main fragment from the previous step and
// restart the tab sequence inside <main>; a clean goto resets the focus
// sequence to the top of the document.
await page.goto(BASE, { waitUntil: "load" });
await page.waitForSelector(".global-nav-link", { state: "attached" });
// The account slot resolves asynchronously (session fetch); wait for the
// concrete end state so "Sign in" is in the DOM during the walk. The API is
// down by design in this harness, so the slot must settle to "Sign in".
//
// A slot that never settles is reported, not thrown: an unhandled timeout
// here ends the whole run with a Playwright stack trace naming no check at
// all — the failure mode an earlier round's log shows (a bare
// `page.waitForSelector` timeout as the last line, with the summary table
// never printed). A check that cannot say which check failed is worth
// nothing, so the timeout becomes a named FAIL and the walk below still
// runs and reports whatever it finds.
try {
  await page.waitForSelector(".global-nav-signin, .global-nav-user", { timeout: 15000 });
} catch {
  fail(
    "keyboard: the account slot settles to Sign in",
    "neither .global-nav-signin nor .global-nav-user was in the DOM after 15s; the header walk below ran anyway",
  );
}
const expectedOrder = [
  "Skip to content",
  "POST home",
  "Search POST",
  "Home",
  "Explore",
  "Projects",
  "Assets",
  "People",
  "Organizations",
  "Notifications",
  "Sign in",
];
const seen = [];
const focusVisible = [];
for (let i = 0; i < expectedOrder.length; i += 1) {
  await page.keyboard.press("Tab");
  a = await active(page);
  seen.push(a?.name ?? "(body)");
  focusVisible.push(
    await page.evaluate(() => {
      const el = document.activeElement;
      if (!el) return false;
      const s = getComputedStyle(el);
      return (s.outlineStyle !== "none" && parseFloat(s.outlineWidth) > 0) || s.boxShadow !== "none";
    }),
  );
}
const orderMismatch = expectedOrder.filter((name, i) => seen[i] !== name);
if (orderMismatch.length > 0) {
  fail("keyboard: tab order through the header", `expected ${JSON.stringify(expectedOrder)}, saw ${JSON.stringify(seen)}`);
} else {
  ok(`keyboard: tab order ${expectedOrder.join(" -> ")}`);
}
const invisibleFocus = seen.map((_, i) => i).filter((i) => !focusVisible[i]);
if (invisibleFocus.length > 0) {
  fail("keyboard: every header stop shows a visible focus indicator", `missing at ${invisibleFocus.map((i) => expectedOrder[i])}`);
} else {
  ok("keyboard: every header stop shows a visible focus indicator");
}

// 4. Search submits by keyboard alone.
const input = page.locator('input[type="search"][name="q"]');
await input.focus();
await page.keyboard.type("catalyst");
await page.keyboard.press("Enter");
await page.waitForURL(/\/search\?q=catalyst/);
const submitted = (await page.locator("[data-search-query]").textContent())?.trim();
if (submitted !== "catalyst") {
  fail("keyboard: search form submits to /search?q=catalyst", `page shows ${JSON.stringify(submitted)}`);
} else {
  ok("keyboard: search form submits to /search?q=catalyst");
}

// 5. A nav link activates with Enter.
await page.goto(BASE, { waitUntil: "load" });
await page.locator(".global-nav-links a", { hasText: "Projects" }).first().focus();
await page.keyboard.press("Enter");
await page.waitForURL(/\/projects$/);
const projectsHeading = (await page.locator("h1").textContent())?.trim();
if (projectsHeading !== "Projects") {
  fail("keyboard: Enter on the Projects link navigates", `h1 is ${JSON.stringify(projectsHeading)}, expected "Projects"`);
} else {
  ok("keyboard: Enter on the Projects link navigates");
}

// 6. No positive tabindex anywhere (focus order stays natural).
const positiveTabindex = await page.evaluate(() =>
  [...document.querySelectorAll("[tabindex]")]
    .map((el) => [el.tagName, el.getAttribute("tabindex")])
    .filter(([, v]) => Number(v) > 0),
);
if (positiveTabindex.length > 0) {
  fail("keyboard: no positive tabindex attributes", JSON.stringify(positiveTabindex));
} else {
  ok("keyboard: no positive tabindex attributes");
}

/* ---------- C. Mobile drawer by keyboard ---------- */

const mobile = await browser.newPage({ viewport: { width: 375, height: 667 } });
mobile.on("pageerror", (error) => fail("uncaught page error (mobile)", String(error).slice(0, 200)));
await mobile.goto(BASE, { waitUntil: "load" });

let reached = false;
for (let i = 0; i < 12 && !reached; i += 1) {
  await mobile.keyboard.press("Tab");
  const a2 = await active(mobile);
  if (a2?.name === "Global navigation menu") reached = true;
}
if (!reached) {
  fail("keyboard: mobile menu toggle is reachable by Tab");
} else {
  ok("keyboard: mobile menu toggle is reachable by Tab");
}

await mobile.waitForFunction(
  () => document.querySelectorAll('button[aria-label="Global navigation menu"]').length === 1,
  undefined,
  { timeout: 5000 },
);
const drawerToggle = mobile.locator('button[aria-label="Global navigation menu"]');

await drawerToggle.focus();
await mobile.keyboard.press("Enter");
const expanded = await drawerToggle.getAttribute("aria-expanded");
if (expanded !== "true") {
  fail("keyboard: Enter opens the drawer", `aria-expanded=${expanded}`);
} else {
  ok("keyboard: Enter opens the drawer (aria-expanded=true)");
}
let a2 = await active(mobile);
if (a2?.href !== "/search") {
  fail("keyboard: opening the drawer focuses the first item (Search)", JSON.stringify(a2));
} else {
  ok("keyboard: opening the drawer focuses the first item (Search)");
}
await mobile.keyboard.press("Tab");
a2 = await active(mobile);
if (a2?.href !== "/") {
  fail("keyboard: Tab walks the drawer items", JSON.stringify(a2));
} else {
  ok("keyboard: Tab walks the drawer items (Search -> Home)");
}
await mobile.keyboard.press("Escape");
const closed = await drawerToggle.getAttribute("aria-expanded");
a2 = await active(mobile);
if (closed !== "false" || a2?.name !== "Global navigation menu") {
  fail("keyboard: Escape closes the drawer and returns focus to the toggle", `expanded=${closed}, focus=${JSON.stringify(a2)}`);
} else {
  ok("keyboard: Escape closes the drawer and returns focus to the toggle");
}

await mobile.close();

/* ---------- C2. The auth layout's skip link (T1104) ----------
 * `/login` sits in the `(auth)` route group, which had no skip link while
 * `(main)` did — the one page where a reader is most likely to be arriving
 * with a keyboard and no context. T1104 mirrored the `(main)` shape; this
 * asserts the mirror holds, because nothing else covers /login's keyboard
 * path and a doc that claims it while nothing checks it is how the gap came
 * back the first time. */
{
  const auth = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  auth.on("pageerror", (error) => fail("uncaught page error (login)", String(error).slice(0, 200)));
  await auth.goto(`${BASE}/login`, { waitUntil: "load" });
  await auth.waitForTimeout(400);
  await auth.keyboard.press("Tab");
  const first = await auth.evaluate(() => {
    const el = document.activeElement;
    return { cls: el?.className ?? "", href: el?.getAttribute?.("href") ?? null };
  });
  if (typeof first.cls !== "string" || !first.cls.includes("skip-link")) {
    fail("keyboard: /login's first tab stop is the skip link", JSON.stringify(first));
  } else {
    await auth.keyboard.press("Enter");
    const landed = await auth.evaluate(() => {
      const el = document.activeElement;
      return el?.tagName === "MAIN" && el.id === "main" ? "main" : (el?.tagName ?? "none");
    });
    if (landed !== "main") {
      fail("keyboard: Enter on /login's skip link focuses #main", `focus landed on ${landed}`);
    } else {
      ok("keyboard: /login's skip link is the first tab stop and focuses #main");
    }
  }
  await auth.close();
}

/* ---------- D. PR tablist by keyboard (dynamic route only) ----------
 * `role="tablist"` is a promise about keyboard behaviour, not just a name:
 * one tab stop, arrow keys between the tabs, Home/End for the ends. The
 * page declared the role with none of that implemented, so the tabs were
 * reachable only by Tab — a widget announced to a screen reader that did
 * not behave as announced. This walks it for real; the fix it covers is in
 * pulls/[number]/page.tsx (roving tabindex + onTabKeyDown). */
if (IDS !== null) {
  const prPage = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  prPage.on("pageerror", (error) => fail("uncaught page error (pulls)", String(error).slice(0, 200)));
  await prPage.goto(`${BASE}${routeURL("/projects/{project_id}/pulls/{pr_number}", IDS)}`, { waitUntil: "load" });
  await prPage.waitForTimeout(800);
  /* A mutation run proved this section could crash the whole file with an
   * unhandled TimeoutError when the PR page rendered no tabs (the API down):
   * the run went red, but it died instead of reporting, taking the other
   * checks' results with it. A gate that cannot finish cannot tell you what
   * else broke, so the missing tablist is a failed CHECK, not a thrown one. */
  const tabCount = await prPage.evaluate(
    () => document.querySelectorAll('[data-pull-tabs] [role="tab"]').length,
  );
  if (tabCount === 0) {
    fail("keyboard: the PR tablist renders", "no [data-pull-tabs] [role=\"tab\"] in the DOM — the page is not the PR page");
  } else {
    await walkTablist(prPage);
  }
  await prPage.close();
}

/* The tablist walk, split out so the section above can report a missing
 * widget as a failure rather than dying on it. */
async function walkTablist(prPage) {
  // The tablist is exactly one tab stop: the selected tab only.
  const stops = await prPage.evaluate(() =>
    [...document.querySelectorAll('[data-pull-tabs] [role="tab"]')].map((el) => el.tabIndex),
  );
  const zeroStops = stops.filter((t) => t === 0).length;
  if (zeroStops !== 1) {
    fail("keyboard: the tablist is a single tab stop", `tabIndex values = ${JSON.stringify(stops)}`);
  } else {
    ok("keyboard: the tablist is a single tab stop (roving tabindex)");
  }

  const selectedId = () => prPage.evaluate(() => document.activeElement?.id ?? null);
  // The arrows only mean anything once a tab has focus; without this the
  // keydown lands on <body> and the check would "fail" for the wrong reason.
  await prPage.locator('[data-pull-tabs] [role="tab"][aria-selected="true"]').focus();
  const selectedTab = await selectedId();
  if (selectedTab === null || !selectedTab.startsWith("pull-tab-")) {
    fail("keyboard: the selected tab can take focus", `activeElement id = ${JSON.stringify(selectedTab)}`);
  }
  await prPage.keyboard.press("ArrowRight");
  const afterRight = await selectedId();
  if (afterRight === selectedTab) {
    fail("keyboard: ArrowRight moves to the next tab", `focus stayed on ${afterRight}`);
  } else {
    ok(`keyboard: ArrowRight moves to the next tab (${selectedTab} -> ${afterRight})`);
  }
  await prPage.keyboard.press("ArrowLeft");
  if ((await selectedId()) !== selectedTab) {
    fail("keyboard: ArrowLeft returns to the previous tab", `focus is ${await selectedId()}`);
  } else {
    ok("keyboard: ArrowLeft returns to the previous tab");
  }

  // The tab the arrow keys land on must actually be the selected one, else
  // the widget moved focus without switching panels.
  const selectedMatchesFocus = await prPage.evaluate(() => {
    const el = document.activeElement;
    return el?.getAttribute("role") === "tab" && el.getAttribute("aria-selected") === "true";
  });
  if (!selectedMatchesFocus) {
    fail("keyboard: the focused tab is the selected tab", "focus and aria-selected disagree");
  } else {
    ok("keyboard: the focused tab is the selected tab");
  }

  // End reaches the LAST tab (compared against the DOM's own order, so a
  // no-op End cannot pass by leaving focus wherever it already was), and
  // ArrowRight wraps from there back to the first (APG).
  const tabIds = await prPage.evaluate(() =>
    [...document.querySelectorAll('[data-pull-tabs] [role="tab"]')].map((el) => el.id),
  );
  const firstTabId = tabIds[0];
  const lastTabId = tabIds[tabIds.length - 1];
  await prPage.keyboard.press("End");
  const atEnd = await selectedId();
  if (atEnd !== lastTabId) {
    fail("keyboard: End moves focus to the last tab", `focus is ${atEnd}, last tab is ${lastTabId}`);
  } else {
    ok(`keyboard: End moves focus to the last tab (${lastTabId})`);
  }
  await prPage.keyboard.press("ArrowRight");
  const wrapped = await selectedId();
  if (wrapped !== firstTabId) {
    fail("keyboard: ArrowRight wraps from the last tab to the first", `focus is ${wrapped}, first tab is ${firstTabId}`);
  } else {
    ok("keyboard: ArrowRight wraps from the last tab to the first");
  }
}

/* ---------- E. prefers-reduced-motion actually takes effect ----------
 *
 * docs/28 asks the UI to respect the user's motion preference. The rule that
 * does it is a blanket `!important` override in globals.css (and in the
 * shared packages/ui layer), so the thing worth checking is not that the
 * text is present but that the browser APPLIES it — to any animated element,
 * including Primer's Spinner, whose own stylesheet carries a rotation
 * keyframes rule and no reduced-motion guard of its own.
 *
 * The probe injects its own animation and reads back the computed duration
 * under both media states. A missing or non-important rule would report 2s
 * here and fail, which is what makes this a measurement. */
{
  const motion = await browser.newPage({ viewport: { width: 1280, height: 800 } });
  await motion.goto(BASE, { waitUntil: "load" });

  const readProbe = async (reduced) => {
    await motion.emulateMedia({ reducedMotion: reduced ? "reduce" : "no-preference" });
    return motion.evaluate(() => {
      const style = document.createElement("style");
      style.textContent =
        "@keyframes t1104probe{from{opacity:1}to{opacity:0}} .t1104probe{animation:t1104probe 2s linear infinite}";
      document.head.appendChild(style);
      const el = document.createElement("div");
      el.className = "t1104probe";
      document.body.appendChild(el);
      const cs = getComputedStyle(el);
      const out = { duration: cs.animationDuration, iteration: cs.animationIterationCount };
      el.remove();
      style.remove();
      return out;
    });
  };

  const normal = await readProbe(false);
  const reduced = await readProbe(true);

  // The control: with no preference expressed the animation must run at its
  // authored 2s. If this is already 0.01ms the probe is measuring nothing.
  if (normal.duration !== "2s") {
    fail("motion: probe animates normally without the preference", `duration=${normal.duration}`);
  } else {
    ok("motion: probe animates normally without the preference (2s)");
  }
  const ms = parseFloat(reduced.duration);
  if (!(ms <= 0.01)) {
    fail("motion: prefers-reduced-motion suppresses animation", `duration=${reduced.duration}`);
  } else {
    ok(`motion: prefers-reduced-motion suppresses animation (${reduced.duration})`);
  }
  if (reduced.iteration !== "1") {
    fail("motion: prefers-reduced-motion collapses the iteration count", `iteration-count=${reduced.iteration}`);
  } else {
    ok("motion: prefers-reduced-motion collapses the iteration count to 1");
  }

  await motion.emulateMedia({ reducedMotion: null });
  await motion.close();
}

await browser.close();

/* ---------- Coverage table ----------
 *
 * `data` is decided by hasContent() above (the fixture marker is in the DOM
 * and no shell marker is), never by the intent of this file. A pane that
 * degraded to its error state therefore shows a FAIL and is not counted. */

console.log("\n== a11y-smoke: per-route coverage ==");
console.log("route                                    | core | data   | wcag | best-practice");
console.log("-----------------------------------------+------+--------+------+--------------");
let coreWithData = 0;
for (const r of scanResults) {
  const dataLabel = r.hasData === null ? "static" : r.contentOK ? "有数据" : "服务关";
  console.log(`${r.route.padEnd(40)} | ${r.core ? "yes" : "no "} | ${dataLabel.padEnd(6)} | ${String(r.wcag).padEnd(4)} | ${r.bestPractice}`);
  if (r.core && r.contentOK) coreWithData += 1;
}
console.log(`\ncore pages scanned with data: ${coreWithData}`);

/* Every core page that did NOT reach 有数据 is named here with its reason,
 * on every run — a shortfall that only ever appears in the RESULT of the
 * change that introduced it is a shortfall nobody reads again. */
const shelled = scanResults.filter((r) => r.core && r.hasData !== null && !r.contentOK);
if (shelled.length > 0) {
  console.log(`\ncore pages scanned 服务关 (NOT counted as core-page AA passes): ${shelled.length}`);
  for (const r of shelled) {
    console.log(`  - ${r.template}: ${r.contentReason ?? "no reason recorded"}`);
  }
}

/* THE FLOOR IS 7 — EVERY docs/42 CORE PAGE THAT HAS A ROUTE.
 *
 * docs/42 names nine core pages. SEVEN of them have a route in apps/web, and
 * this run drives SEVEN of those to their real content:
 *
 *   Project Overview        /projects/[id]                        driven
 *   Research Page           /projects/[id]/research               driven
 *   PR                      /projects/[id]/pulls/[number]         driven
 *   Release                 /projects/[id]/releases/[releaseId]   driven
 *   Asset Page              /assets/[pid]/[version]               driven
 *   Research Profile        /users/[id]                           driven
 *   Search Answer           /search                               driven
 *   Scientific Object Detail  (no route in apps/web)
 *   Knowledge Network Page    (no route in apps/web)
 *
 * `/assets/[pid]/[version]` is docs/42's ASSET PAGE, not Scientific Object
 * Detail: it renders origin/creators/metadata/dependencies/lineage/usages/
 * versions/network events, which is that section's list verbatim, and no
 * file under apps/web renders "Provenance" at all.
 *
 * `Search Answer` used to be the one core page with a route that was still
 * not scanned with data: POST /api/v1/search is a state change behind the
 * session guard (cmd/api/authhttp/auth_middleware.go checkCSRF) and
 * apps/web/lib/search.ts sent no X-CSRF-Token, so the page answered
 * `CSRF_FAILED` — the app's own client could not search while signed in
 * either. T1104 fixed the client (it now carries the token, like the other
 * five write clients), and this scan is the proof it works end to end:
 * `ok content /search?q=catalyst: rendered [data-search-answer]`.
 *
 * Asserting the count here means a route that quietly drops out of the table
 * (or a scan that degrades to a shell) fails the run instead of shrinking a
 * number nobody reads. The floor is every route-existing docs/42 core page
 * (docs/42 has nine sections; Scientific Object Detail and Knowledge Network
 * Page have no route in apps/web, which leaves exactly seven), so a page that
 * stops rendering its data reds this run rather than quietly leaving the
 * count. */
const CORE_PAGES_WITH_DATA_FLOOR = 7;
if (IDS !== null) {
  if (coreWithData < CORE_PAGES_WITH_DATA_FLOOR) {
    fail(
      "coverage: core pages scanned with data",
      `${coreWithData} < ${CORE_PAGES_WITH_DATA_FLOOR} — a docs/42 page that was being scanned with real content has stopped rendering it`,
    );
  } else {
    ok(`coverage: ${coreWithData} core pages scanned with data (>= ${CORE_PAGES_WITH_DATA_FLOOR})`);
  }
}

if (fails > 0) {
  console.log(`\na11y smoke: ${fails} check(s) FAILED`);
  process.exit(1);
}
console.log("\na11y smoke: all checks passed");
