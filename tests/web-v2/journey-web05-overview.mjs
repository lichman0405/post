/**
 * WEB-05 overview journey (T1339): a project with NO main branch opens from
 * the directory without a page error, and a project WITH a main branch still
 * shows its accepted-main line (docs/70 G06, docs/42 page specs).
 *
 * The regression this journey is the evidence for: the overview payload
 * always carried `current_main` and serialised it as `{}` for a project that
 * has no main branch. An empty object is TRUTHY, so the project page's
 * `overview.current_main ? … : null` guard passed and the next property read
 * (`head_state_id.slice(0, 8)`) threw inside the render — a freshly created
 * project crashed the moment it was opened, which is exactly the real
 * journey T1338 walked into. The page now reads presence from
 * `head_state_id`, and `latest_commit` (absent until main has a commit) is
 * optional too.
 *
 * What this proves, and why each assertion is the one it is:
 *
 *   1. The account is established through the product's own /login surface
 *      by the browser, and the project is created through the visible create
 *      form with an explicit Private choice — the same route a reader takes.
 *      Project creation seeds no research state, so the new project has no
 *      main branch: the empty `current_main` is the state under test.
 *   2. Opening that project from the directory row renders the overview tab
 *      (which only exists once the overview read returned and React mounted
 *      it), shows the project's own purpose, shows the no-question empty
 *      state, and shows NO accepted-main line. Zero new page errors and a
 *      200 `GET …/overview` are asserted around it: a crash is caught even
 *      if some fallback markup happened to render.
 *   3. Signed in as the WEB-05 fixture owner (harness/fixture.go — a private
 *      project whose main branch has commits), the accepted-main line still
 *      renders: the "Accepted main" label, an 8-character head hash, the
 *      latest commit's message and the actor handle. This is the "unchanged
 *      when main exists" half of the fix — a guard that hid the line for
 *      every project would fail here.
 *
 * WHERE THE RESEARCH-CARRYING PROJECT COMES FROM. The fixture seeds it
 * through the same application services the API writes through
 * (harness/fixture.go, docs/70 §6.3); this journey only reads it, and the
 * fixture owner is signed in through the product's real login form. The
 * no-main project is created by the browser, through the product's form.
 *
 * Anti-bypass (docs/70 §6.2, scripts/tests/web-v2-bypass-scan.mjs): no
 * test-side request, SQL, CLI, cookie/storage injection or in-page script in
 * any step. Every assertion is a click, a typed value or a read of the
 * rendered DOM.
 *
 * Usage: node tests/web-v2/journey-web05-overview.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR,
 *        WEB_V2_API_ORIGIN, WEB_V2_ACCOUNT_EMAIL, WEB_V2_ACCOUNT_PASSWORD,
 *        WEB_V2_WEB05_EMAIL, WEB_V2_WEB05_PASSWORD
 */

import path from "node:path";

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey (WEB-05 overview): FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
// Its own subdirectory, like WEB-03, WEB-04 and the browse journey: a run
// writes one metadata/steps/trace series per journey, and two journeys
// sharing a directory would make neither reviewable.
const OUT_DIR = path.join(
  process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`,
  "web05-overview",
);
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";

// The account this journey registers, derived from the run id exactly as
// WEB-04 derives its own (a different prefix, so the two journeys never
// share an address). The password is never written to any evidence file.
const EMAIL = process.env.WEB_V2_ACCOUNT_EMAIL ?? `webv2-web05ov-${RUN_ID.toLowerCase()}@example.test`;
const PASSWORD = process.env.WEB_V2_ACCOUNT_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;

// The harness fixture owner (harness/main.go loadConfig, harness/fixture.go).
// The fixture project is PRIVATE and its main branch has commits, so the
// accepted-main line is the state the second half of the journey reads.
const OWNER_EMAIL = process.env.WEB_V2_WEB05_EMAIL ?? `webv2-web05-${RUN_ID.toLowerCase()}@example.test`;
const OWNER_PASSWORD = process.env.WEB_V2_WEB05_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;
const OWNER_HANDLE = "web05-owner";
const MAIN_PROJECT_SLUG = "isotherm-browse-lab";

// The project the browser creates. Fixed values, not generated: the reader
// recognises the row by name, and the purpose is what the overview must
// render back.
const PRIVATE_NAME = "Unpublished Mainless Draft";
const PRIVATE_SLUG = "mainless-draft";
const PRIVATE_PURPOSE =
  "A freshly created project whose research state has no main branch yet.";

const STEP_NAMES = [
  "signup-new-account",
  "create-a-private-project-that-has-no-main-branch",
  "open-the-new-project-from-the-directory-without-a-pageerror",
  "sign-out-and-sign-in-to-the-fixture-owner",
  "open-the-accepted-main-project-overview",
];

const steps = new JourneySteps("WEB-05-project-overview-current-main", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "WEB-05-project-overview-current-main",
  task: "T1339",
  role: "newly registered account -> WEB-05 fixture owner",
  accountEmail: EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

function log(message) {
  console.log(`web-v2 journey (WEB-05 overview): ${message}`);
}

function fail(message) {
  throw new Error(message);
}

async function requireVisible(page, selector, what) {
  const locator = page.locator(selector).first();
  try {
    await locator.waitFor({ state: "visible", timeout: 20000 });
  } catch {
    const count = await page.locator(selector).count().catch(() => 0);
    fail(`${what} is missing: expected a visible ${selector} on ${page.url()} (found ${count})`);
  }
  return locator;
}

async function requireGone(page, selector, what) {
  const count = await page.locator(selector).count();
  if (count !== 0) {
    fail(`${what}: expected no ${selector} on ${page.url()} (found ${count})`);
  }
}

/** The observed responses for `method` + URL suffix at or after `since`. */
function responsesSince(since, method, suffix) {
  return evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "response" && entry.method === method && entry.url.endsWith(suffix),
    );
}

/** Wait for a response by `method` + URL suffix, or answer null on timeout. */
async function waitForResponse(since, method, suffix, timeout = 15000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = responsesSince(since, method, suffix);
    if (found.length > 0) return found[found.length - 1];
    if (Date.now() >= deadline) return null;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

/**
 * The one console refusal this journey asks for by URL: the signed-out
 * session probe the navigation makes before sign-in. Any other console error
 * fails the run.
 */
function isExpectedRefusal(entry) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  if (!text.includes("401")) return false;
  return text.includes("/api/v1/auth/session") || location.includes("/api/v1/auth/session");
}

async function screenshot(page, label) {
  await evidence.screenshot(page, label);
}

/** The exact text of one element, trimmed. */
async function textOf(locator) {
  return ((await locator.textContent()) ?? "").trim();
}

/** Fail when this step observed a browser page error. */
function requireNoNewPageErrors(what, errorsBefore) {
  const fresh = evidence.pageErrors.slice(errorsBefore);
  if (fresh.length > 0) {
    fail(`${what}: the browser reported page error(s): ${fresh.map((entry) => entry.text).join(" | ")}`);
  }
}

async function run() {
  await evidence.init();
  log(`run ${RUN_ID} against ${BASE} (commit ${COMMIT})`);

  const browser = await chromium.launch({ headless: true });
  const browserVersion = browser.version();

  const observedPages = new WeakSet();
  function observePage(target) {
    if (observedPages.has(target)) return;
    observedPages.add(target);
    target.on("console", (message) => {
      const location = message.location();
      evidence.addConsole(
        message.type(),
        message.text(),
        location?.url ? `${location.url}:${location.lineNumber ?? 0}` : null,
      );
    });
    target.on("pageerror", (error) => evidence.addPageError(error.message, error.stack));
  }

  function observeContext(target) {
    target.on("request", (request) => evidence.addRequest(request));
    target.on("response", (response) => evidence.addResponse(response));
    target.on("requestfailed", (request) => evidence.addFailedRequest(request, request.failure()));
    target.on("page", observePage);
  }

  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  observeContext(context);
  await context.tracing.start({ screenshots: true, snapshots: true, sources: false });

  const page = await context.newPage();
  observePage(page);

  let result = "failed";
  let failure;
  /** The created project's own path, read from its directory row. */
  let projectHref = "";

  try {
    await steps.step("signup-new-account", async () => {
      const response = await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`home page did not load: HTTP ${response?.status() ?? "no response"} at ${BASE}/`);
      }
      const entry = await requireVisible(page, '[data-home-entry="sign-in"]', "the home sign-in entry");
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      await requireVisible(page, ".auth-card", "the sign-in card");
      const accountEntry = await requireVisible(
        page,
        ".auth-sub-after button",
        "the account-establishment entry (create an account)",
      );
      await accountEntry.click();
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const probe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      try {
        await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
      } catch {
        const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
        fail(`sign-up did not reach the signed-in home (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
      }
      await requireVisible(page, ".global-nav-user-handle", "the signed-in account handle");
      if (responsesSince(probe, "POST", "/api/v1/auth/signup").length !== 1) {
        fail("account establishment did not go through the product's signup endpoint");
      }
      await screenshot(page, "signed-in");
    });

    await steps.step("create-a-private-project-that-has-no-main-branch", async () => {
      // Entered through the signed-in home's own project entry: the route a
      // first-time account is offered, not a typed URL.
      const entry = await requireVisible(
        page,
        '[data-home-entry="project-workspaces"]',
        "the home project entry",
      );
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 15000 });
      await requireVisible(page, ".project-directory-title", "the project directory title");

      // The directory is not assumed empty: other journeys in this run
      // created PUBLIC projects, and a new account legitimately sees them.
      // The create control is the directory's own entry into the form.
      const start = await requireVisible(
        page,
        '[data-project-create="open"]',
        "the directory's create control",
      );
      await start.click();
      await requireVisible(page, '[data-project-create="form"]', "the create panel");

      // An explicit Private choice, as the acceptance criterion names it.
      await page.locator('[data-project-visibility="private"]').first().check();
      if (!(await page.locator('[data-project-visibility="private"]').first().isChecked())) {
        fail("selecting Private did not take effect; the reader must be able to choose it");
      }
      await page.locator('[data-project-field="name"]').fill(PRIVATE_NAME);
      await page.locator('[data-project-field="slug"]').fill(PRIVATE_SLUG);
      await page.locator('[data-project-field="purpose"]').fill(PRIVATE_PURPOSE);

      const probe = evidence.network.length;
      await page.locator('[data-project-create="submit"]').click();
      const created = await requireVisible(
        page,
        '[data-project-create="created"]',
        "the created-project confirmation",
      );
      if (!((await created.textContent()) ?? "").includes(PRIVATE_SLUG)) {
        fail("the confirmation does not name the new project's address");
      }
      const write = responsesSince(probe, "POST", "/api/v1/projects").at(-1);
      if (write === undefined || write.status !== 201) {
        fail(`creating the project did not answer 201 (observed ${write?.status ?? "no response"})`);
      }

      const row = await requireVisible(
        page,
        `[data-project-row="${PRIVATE_SLUG}"]`,
        "the new project's directory row",
      );
      await requireVisible(
        page,
        `[data-project-row="${PRIVATE_SLUG}"] [data-badge="private"]`,
        "the private visibility badge on the new project",
      );
      if (!((await row.textContent()) ?? "").includes(PRIVATE_NAME)) {
        fail("the directory row for the new project does not name it; the reader cannot recognise what they created");
      }
      projectHref = (await row.getAttribute("href")) ?? "";
      if (!/^\/projects\/[^/]+$/.test(projectHref)) {
        fail(`the new project's directory row links to ${JSON.stringify(projectHref)}, not a project page`);
      }
      await screenshot(page, "mainless-project-created");
    });

    await steps.step("open-the-new-project-from-the-directory-without-a-pageerror", async () => {
      const errorsBefore = evidence.pageErrors.length;
      const probe = evidence.network.length;

      const row = await requireVisible(
        page,
        `[data-project-row="${PRIVATE_SLUG}"]`,
        "the new project's directory row",
      );
      await row.click();
      await page.waitForURL((url) => url.pathname === projectHref, { timeout: 20000 });
      await requireVisible(page, "[data-project-shell]", "the project shell");

      // This container only renders once the overview read answered AND the
      // tab component mounted; the pre-fix crash threw before it existed.
      const overview = await requireVisible(
        page,
        '[data-project-tab-content="overview"]',
        "the project overview tab of a project without main",
      );

      const read = await waitForResponse(probe, "GET", "/overview");
      if (read === null || read.status !== 200) {
        fail(`the overview read did not answer 200 (observed ${read?.status ?? "no response"})`);
      }

      const purpose = await textOf(overview.locator(".demo-lead p").first());
      if (purpose !== PRIVATE_PURPOSE) {
        fail(`the overview shows ${JSON.stringify(purpose)} instead of the project's purpose ${JSON.stringify(PRIVATE_PURPOSE)}`);
      }

      // The empty research state renders its documented fallback — proof the
      // overview payload was consumed, not that a stale shell stood in. The
      // heading is copied from the catalog, so the assertion is "it says
      // something", not one language's sentence.
      const heading = await textOf(overview.locator(".demo-panel h3").first());
      if (heading === "") {
        fail("the overview's primary-question panel is visible but carries no heading");
      }

      // And a project with no main branch has NO accepted-main line. An
      // empty `current_main: {}` must not render one (nor throw reading it).
      await requireGone(
        page,
        '[data-project-tab-content="overview"] .demo-head-note',
        "a project without main must not show an accepted-main line",
      );

      requireNoNewPageErrors("opening a project without a main branch", errorsBefore);
      await screenshot(page, "mainless-project-overview");
    });

    await steps.step("sign-out-and-sign-in-to-the-fixture-owner", async () => {
      // Leave the new account through the header's own account menu, then
      // sign the fixture owner in through the real /login form.
      const accountMenu = await requireVisible(page, ".global-nav-user", "the account menu control");
      await accountMenu.click();
      const menuItems = page.locator('[role="menuitem"]');
      await menuItems.first().waitFor({ state: "visible", timeout: 10000 });
      if ((await menuItems.count()) < 2) {
        fail("the account menu opened but offers no sign-out item");
      }
      await menuItems.last().click();
      await requireVisible(page, ".global-nav-signin", "the signed-out state after sign-out");
      await requireGone(page, ".global-nav-user-handle", "sign-out must clear the signed-in header");

      // Back to the product's own home entry (a navigation reset, the same
      // route WEB-01's sign-in steps take), then in through /login.
      await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      const entry = await requireVisible(page, '[data-home-entry="sign-in"]', "the home sign-in entry");
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 20000 });
      await requireVisible(page, ".auth-card", "the sign-in card");
      await page.locator('form input[type="email"]').fill(OWNER_EMAIL);
      await page.locator('form input[type="password"]').fill(OWNER_PASSWORD);
      const probe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      try {
        await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
      } catch {
        const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
        fail(`the fixture owner's sign-in did not reach the signed-in home (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
      }
      await requireVisible(page, ".global-nav-user-handle", "the signed-in fixture owner's handle");
      const login = responsesSince(probe, "POST", "/api/v1/auth/login");
      if (login.length !== 1 || login[0].status !== 200) {
        fail(`the fixture owner's sign-in did not go through the product's login endpoint (observed ${JSON.stringify(login.map((entry) => entry.status))})`);
      }
      await screenshot(page, "signed-in-fixture-owner");
    });

    await steps.step("open-the-accepted-main-project-overview", async () => {
      const errorsBefore = evidence.pageErrors.length;

      const entry = await requireVisible(
        page,
        '[data-home-entry="project-workspaces"]',
        "the home project entry",
      );
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 20000 });
      await requireVisible(page, ".project-directory-title", "the project directory title");

      const row = await requireVisible(
        page,
        `[data-project-row="${MAIN_PROJECT_SLUG}"]`,
        "the seeded project's directory row",
      );
      const probe = evidence.network.length;
      await row.click();
      await page.waitForURL((url) => /^\/projects\/[^/]+$/.test(url.pathname), { timeout: 20000 });
      await requireVisible(page, "[data-project-shell]", "the project shell");

      const overview = await requireVisible(
        page,
        '[data-project-tab-content="overview"]',
        "the project overview tab of a project with main",
      );
      const read = await waitForResponse(probe, "GET", "/overview");
      if (read === null || read.status !== 200) {
        fail(`the overview read did not answer 200 (observed ${read?.status ?? "no response"})`);
      }

      // The accepted-main line is unchanged for a project that has one: the
      // label, the 8-character head hash, the latest commit's message and
      // the actor. A guard that hid this line for every project would fail
      // exactly here.
      const note = await requireVisible(
        page,
        '[data-project-tab-content="overview"] .demo-head-note',
        "the accepted-main line of a project with main",
      );
      const noteText = await textOf(note);
      const code = await textOf(note.locator("code").first());
      if (!/^[0-9a-f]{8}$/.test(code)) {
        fail(`the accepted-main line shows ${JSON.stringify(code)} as the head hash; 8 lowercase hex characters were expected`);
      }
      if (noteText.length <= code.length || !noteText.includes(code)) {
        fail(`the accepted-main line ${JSON.stringify(noteText)} does not carry the head hash it rendered`);
      }
      const expected = new RegExp(`^Accepted main [0-9a-f]{8} · .+ by ${OWNER_HANDLE}$`);
      if (!expected.test(noteText)) {
        fail(`the accepted-main line reads ${JSON.stringify(noteText)}; expected "Accepted main <8 hex> · <latest commit message> by ${OWNER_HANDLE}"`);
      }

      requireNoNewPageErrors("opening a project with a main branch", errorsBefore);
      await screenshot(page, "accepted-main-overview");
    });

    steps.assertComplete();
    result = "passed";
    log("all declared steps passed");
  } catch (err) {
    failure = err;
    log(`FAILED: ${errorText(err)}`);
    try {
      await screenshot(page, "failure-state");
    } catch (shotErr) {
      failure = new Error(`${errorText(err)}; evidence write failure: ${errorText(shotErr)}`);
    }
  } finally {
    let traceFailure;
    try {
      await context.tracing.stop({ path: tracePath });
      await evidence.verifyArtifact(tracePath, "the Playwright trace");
    } catch (err) {
      traceFailure = new Error(`evidence write failure: ${errorText(err)}`);
    }
    await browser.close();

    if (traceFailure !== undefined) {
      failure =
        failure === undefined
          ? traceFailure
          : new Error(`${errorText(failure)}; additionally, ${errorText(traceFailure)}`);
      result = "failed";
      log(`FAILED: ${errorText(traceFailure)}`);
    }

    if (result === "passed") {
      if (evidence.pageErrors.length > 0) {
        failure = new Error(`browser reported page error(s): ${evidence.pageErrors.map((e) => e.text).join(" | ")}`);
      } else if (evidence.failedRequests.length > 0) {
        failure = new Error(
          `browser request(s) failed: ${evidence.failedRequests.map((e) => `${e.method} ${e.url} (${e.errorText})`).join(" | ")}`,
        );
      } else if (evidence.serverErrors.length > 0) {
        failure = new Error(
          `API answered 5xx: ${evidence.serverErrors.map((e) => `${e.method} ${e.url} -> ${e.status}`).join(" | ")}`,
        );
      } else {
        const consoleErrors = evidence.console.filter(
          (entry) => entry.kind === "error" && !isExpectedRefusal(entry),
        );
        if (consoleErrors.length > 0) {
          failure = new Error(`browser console error(s): ${consoleErrors.map((e) => e.text).join(" | ")}`);
        }
      }
      if (failure !== undefined) {
        result = "failed";
        log(`FAILED after steps completed: ${errorText(failure)}`);
      }
    }

    const metadata = await evidence.finalize({
      steps,
      result,
      failure,
      browserVersion,
      playwrightVersion: process.env.WEB_V2_PLAYWRIGHT_VERSION,
      harness: process.env.WEB_V2_HARNESS_JSON ?? null,
    });
    log(`evidence: ${OUT_DIR} (result: ${metadata.result})`);
    if (result !== "passed") {
      process.exitCode = 1;
    }
  }
}

run().catch((err) => {
  console.error(`web-v2 journey (WEB-05 overview): FAILED — ${errorText(err)}`);
  process.exit(1);
});
