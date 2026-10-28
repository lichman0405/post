/**
 * WEB-04 journey (T1305): creating a project from the web, and who may then
 * see it (docs/70 G04, D16; U01/U02).
 *
 * What this proves, and why each assertion is the one it is:
 *
 *   1. The account is established through the product's own /login surface by
 *      the browser. The harness seeds no account and no project, so every
 *      project in this run is created by a visible form (docs/70 §6.2).
 *   2. A new account's /projects is an ANSWERED empty state that offers the
 *      first research path — the entry into creating a project (D16). The
 *      empty state is accepted only together with the 200 that produced it,
 *      because the same container is the error container.
 *   3. D16's "默认 Public": the visibility control is checked Public BEFORE
 *      the journey touches it and Private is one click away. The public
 *      project is created through that default, so "default Public" is
 *      measured, not asserted from a constant.
 *   4. The private project is created by deliberately switching the radio,
 *      and the directory row carries the private badge — the visibility the
 *      reader chose is the visibility the project holds.
 *   5. A duplicate address is a FIELD error under the address control, the
 *      API answered 409, the panel stays open so the reader can correct it,
 *      and the directory still holds exactly the projects it held before
 *      (no second project was created).
 *   6. In a FRESH browser context — no session, nothing carried over — the
 *      private project's URL renders the neutral not-found state, the API
 *      answered the existence-hiding 404 for it, the page never contains the
 *      project's name or purpose, and the anonymous directory lists the
 *      public project while omitting the private one.
 *
 * Anti-bypass (docs/70 §6.2, scripts/tests/web-v2-bypass-scan.mjs): no
 * test-side request, SQL, CLI, cookie/storage injection or in-page script in
 * any step. Every business step is a click, a typed value or a read of the
 * rendered DOM.
 *
 * Usage: node tests/web-v2/journey-web04.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR,
 *        WEB_V2_API_ORIGIN, WEB_V2_ACCOUNT_EMAIL, WEB_V2_ACCOUNT_PASSWORD
 */

import path from "node:path";

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey (WEB-04): FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
// Its own subdirectory: WEB-01's journey.mjs owns the run's top-level
// metadata/steps/trace, and one journey overwriting another's evidence would
// leave a green record pointing at somebody else's run.
const OUT_DIR = path.join(
  process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`,
  "web04",
);
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";
const EMAIL = process.env.WEB_V2_ACCOUNT_EMAIL ?? `webv2-web04-${RUN_ID.toLowerCase()}@example.test`;
const PASSWORD = process.env.WEB_V2_ACCOUNT_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;

// The two projects this journey creates. Fixed values, not generated: the
// duplicate step needs to re-use the first address on purpose, and the
// anonymous step needs to search the rendered page for the private project's
// name — a name the reader can recognise.
const PUBLIC_NAME = "Protein Folding Study";
const PUBLIC_SLUG = "protein-folding-study";
const PUBLIC_PURPOSE =
  "Determine whether the molten-globule intermediate is populated before the native state at 310 K.";
const PRIVATE_NAME = "Unpublished Membrane Draft";
const PRIVATE_SLUG = "membrane-draft";
const PRIVATE_PURPOSE =
  "Draft analyses of membrane permeability that are not ready to be read outside the project.";

const STEP_NAMES = [
  "signup-new-account",
  "directory-empty-state-offers-first-research-path",
  "create-public-project-through-the-default-visibility",
  "create-private-project-through-an-explicit-choice",
  "duplicate-address-is-a-field-error-and-creates-nothing",
  "anonymous-cannot-see-the-private-project",
];

const steps = new JourneySteps("WEB-04-create-project", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "WEB-04-create-project",
  role: "anonymous visitor -> newly registered account -> anonymous visitor",
  accountEmail: EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

/**
 * The private project's id, once the directory has told us where it lives.
 * The console-refusal filter below needs it: the only membership probe this
 * journey may excuse is the one for the project it deliberately opens
 * anonymously.
 */
let privateProjectID = "";

function log(message) {
  console.log(`web-v2 journey (WEB-04): ${message}`);
}

function fail(message) {
  throw new Error(message);
}

async function requireVisible(page, selector, what) {
  const locator = page.locator(selector).first();
  try {
    await locator.waitFor({ state: "visible", timeout: 15000 });
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

/** The last observed response for `method` + URL suffix at or after `since`. */
function lastResponseSince(since, method, suffix) {
  const responses = evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "response" && entry.method === method && entry.url.endsWith(suffix),
    );
  return responses.length === 0 ? null : responses[responses.length - 1];
}

function observedRequestSince(since, method, suffix) {
  return evidence.network
    .slice(since)
    .some((entry) => entry.event === "request" && entry.method === method && entry.url.endsWith(suffix));
}

/**
 * Wait until the network log holds a response for `method` + URL suffix at or
 * after `since`, or answer null once `timeout` ms have passed.
 *
 * The click's promise resolves when the DOM event is dispatched, which is
 * BEFORE the response the click triggers reaches Playwright's response
 * listener — so reading the log on the line after `click()` is a race, and it
 * is the one that made the restored run report "no response" for a refusal
 * the page had in fact received. The loop orders the observation; it does not
 * soften it: the caller still asserts the exact status, and null after the
 * timeout is still a failure.
 */
async function waitForResponse(since, method, suffix, timeout = 15000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = lastResponseSince(since, method, suffix);
    if (found !== null) return found;
    if (Date.now() >= deadline) return null;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

/**
 * The refusals this journey asks for, by URL and status. Each one is the
 * answer the step that triggered it asserts, so allowing its console line
 * does not excuse a silent failure:
 *
 *   - 401 GET /api/v1/auth/session: the signed-out probe the nav makes on
 *     every anonymous page (and on the fresh context in the last step);
 *   - 409 POST /api/v1/projects: the duplicate-address refusal the duplicate
 *     step submits on purpose and then asserts field-by-field;
 *   - 401 GET /api/v1/projects/<id>/membership for the ONE private project
 *     the last step opens anonymously. The shell reads the project and the
 *     caller's membership in parallel and treats a membership failure as
 *     "no role" (project-shell.tsx), so an anonymous visitor produces this
 *     401 by design; the assertion that matters is the 404 next to it,
 *     which is where the existence hiding happens;
 *   - 404 GET /api/v1/projects/<id>: that existence-hiding answer. The
 *     signed-in steps never read a project by id, so neither allowance can
 *     mask a broken read elsewhere in this journey.
 *
 * Any other console error fails the run.
 */
function isExpectedRefusal(entry) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  const named = (suffix) => text.includes(suffix) || location.includes(suffix);
  if (text.includes("401")) {
    return (
      named("/api/v1/auth/session") ||
      (privateProjectID !== "" && named(`/api/v1/projects/${privateProjectID}/membership`))
    );
  }
  if (text.includes("409")) return named("/api/v1/projects");
  if (text.includes("404")) return named("/api/v1/projects/");
  return false;
}

async function screenshot(page, label) {
  await evidence.screenshot(page, label);
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
  /** The created projects' own paths, read from the directory rows. */
  let publicHref = "";
  let privateHref = "";

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
      if (!observedRequestSince(probe, "POST", "/api/v1/auth/signup")) {
        fail("account establishment did not go through the product's signup endpoint");
      }
      await screenshot(page, "signed-in");
    });

    await steps.step("directory-empty-state-offers-first-research-path", async () => {
      // Entered through the signed-in home's own project entry, not a typed
      // URL: this is the route a first-time account is actually offered.
      const entry = await requireVisible(
        page,
        '[data-home-entry="project-workspaces"]',
        "the home project entry",
      );
      const probe = evidence.network.length;
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 15000 });
      await requireVisible(page, ".project-directory-title", "the project directory title");

      const empty = await requireVisible(page, "[data-project-empty]", "the answered empty state");
      const listRead = lastResponseSince(probe, "GET", "/api/v1/projects");
      if (listRead === null || listRead.status !== 200) {
        fail(`the empty state was not backed by a 200 GET /api/v1/projects (observed ${listRead?.status ?? "no response"})`);
      }
      const text = ((await empty.textContent()) ?? "").trim();
      if (!/^No projects yet\./i.test(text)) {
        fail(`the directory showed ${JSON.stringify(text)}; a new account's answered empty state was expected`);
      }

      // D16: the empty state itself is where the first research path starts.
      const start = await requireVisible(
        page,
        '[data-project-create="empty-entry"]',
        "the empty state's entry into creating the first project",
      );
      const label = ((await start.textContent()) ?? "").trim();
      if (label === "") {
        fail("the empty state's create entry is visible but carries no label");
      }
      await screenshot(page, "directory-empty-state");
    });

    await steps.step("create-public-project-through-the-default-visibility", async () => {
      const start = await requireVisible(
        page,
        '[data-project-create="empty-entry"]',
        "the empty state's create entry",
      );
      await start.click();
      const form = await requireVisible(page, '[data-project-create="form"]', "the create panel");

      // D16's default is MEASURED before the journey touches the control:
      // Public checked, Private not. A form that only defaulted correctly
      // after being clicked would pass a weaker test and fail a reader.
      const publicRadio = page.locator('[data-project-visibility="public"]').first();
      const privateRadio = page.locator('[data-project-visibility="private"]').first();
      await publicRadio.waitFor({ state: "visible", timeout: 10000 });
      if (!(await publicRadio.isChecked())) {
        fail("the create panel does not preselect Public; D16 requires the default to be Public");
      }
      if (await privateRadio.isChecked()) {
        fail("the create panel has Private preselected; D16 requires the default to be Public");
      }

      await page.locator('[data-project-field="name"]').fill(PUBLIC_NAME);
      await page.locator('[data-project-field="purpose"]').fill(PUBLIC_PURPOSE);
      // The address is derived from the name until the reader edits it.
      const derived = await page.locator('[data-project-field="slug"]').inputValue();
      if (derived !== PUBLIC_SLUG) {
        fail(`the address derived from ${JSON.stringify(PUBLIC_NAME)} was ${JSON.stringify(derived)}, not ${JSON.stringify(PUBLIC_SLUG)}`);
      }

      // G04's personal/team entry: a fresh account belongs to no
      // organization, and the product says so instead of silently dropping
      // the choice. The read behind it must have been a real 200.
      const orgNote = await requireVisible(
        page,
        '[data-project-organizations="none"]',
        "the organization situation on a fresh account",
      );
      if (((await orgNote.textContent()) ?? "").trim() === "") {
        fail("the organization note is visible but says nothing");
      }
      const orgRead = lastResponseSince(0, "GET", "/api/v1/organizations");
      if (orgRead === null || orgRead.status !== 200) {
        fail(`the create panel's organization read did not answer 200 (observed ${orgRead?.status ?? "no response"})`);
      }
      await screenshot(page, "create-public-filled");

      const probe = evidence.network.length;
      await page.locator('[data-project-create="submit"]').click();
      const created = await requireVisible(
        page,
        '[data-project-create="created"]',
        "the created-project confirmation",
      );
      const createdText = ((await created.textContent()) ?? "").trim();
      if (!createdText.includes(PUBLIC_SLUG)) {
        fail(`the confirmation said ${JSON.stringify(createdText)}, which does not name the new project's address`);
      }
      const write = lastResponseSince(probe, "POST", "/api/v1/projects");
      if (write === null || write.status !== 201) {
        fail(`creating the public project did not answer 201 (observed ${write?.status ?? "no response"})`);
      }
      await requireGone(page, '[data-project-create="form"]', "the panel must close after a successful create");

      const row = await requireVisible(
        page,
        `[data-project-row="${PUBLIC_SLUG}"]`,
        "the new public project's directory row",
      );
      await requireVisible(
        page,
        `[data-project-row="${PUBLIC_SLUG}"] [data-badge="public"]`,
        "the public visibility badge on the new project",
      );
      const rowText = ((await row.textContent()) ?? "").trim();
      if (!rowText.includes(PUBLIC_NAME)) {
        fail(`the directory row for ${PUBLIC_SLUG} does not name the project (${JSON.stringify(rowText)})`);
      }
      publicHref = (await row.getAttribute("href")) ?? "";
      if (!/^\/projects\/[^/]+$/.test(publicHref)) {
        fail(`the directory row links to ${JSON.stringify(publicHref)}, not a project page`);
      }
      await screenshot(page, "public-project-created");
    });

    await steps.step("create-private-project-through-an-explicit-choice", async () => {
      const open = await requireVisible(
        page,
        '[data-project-create="open"]',
        "the directory's create control",
      );
      await open.click();
      await requireVisible(page, '[data-project-create="form"]', "the create panel");

      // The default is per-project, not a sticky last choice: this panel
      // opens on Public again, and Private is the reader's own click.
      const publicRadio = page.locator('[data-project-visibility="public"]').first();
      if (!(await publicRadio.isChecked())) {
        fail("the second create panel did not preselect Public; D16's default is per project");
      }
      await page.locator('[data-project-visibility="private"]').first().check();
      if (!(await page.locator('[data-project-visibility="private"]').first().isChecked())) {
        fail("selecting Private did not take effect; the reader must be able to choose it");
      }

      await page.locator('[data-project-field="name"]').fill(PRIVATE_NAME);
      await page.locator('[data-project-field="slug"]').fill(PRIVATE_SLUG);
      await page.locator('[data-project-field="purpose"]').fill(PRIVATE_PURPOSE);
      await screenshot(page, "create-private-filled");

      const probe = evidence.network.length;
      await page.locator('[data-project-create="submit"]').click();
      const created = await requireVisible(
        page,
        '[data-project-create="created"]',
        "the created-project confirmation for the private project",
      );
      if (!((await created.textContent()) ?? "").includes(PRIVATE_SLUG)) {
        fail("the confirmation does not name the private project's address");
      }
      const write = lastResponseSince(probe, "POST", "/api/v1/projects");
      if (write === null || write.status !== 201) {
        fail(`creating the private project did not answer 201 (observed ${write?.status ?? "no response"})`);
      }

      const row = await requireVisible(
        page,
        `[data-project-row="${PRIVATE_SLUG}"]`,
        "the new private project's directory row",
      );
      await requireVisible(
        page,
        `[data-project-row="${PRIVATE_SLUG}"] [data-badge="private"]`,
        "the private visibility badge on the new project",
      );
      if (!((await row.textContent()) ?? "").includes(PRIVATE_NAME)) {
        fail("the directory row for the private project does not name it; the reader cannot recognise what they created");
      }
      privateHref = (await row.getAttribute("href")) ?? "";
      if (!/^\/projects\/[^/]+$/.test(privateHref)) {
        fail(`the private project's directory row links to ${JSON.stringify(privateHref)}, not a project page`);
      }
      privateProjectID = privateHref.split("/").pop() ?? "";
      const rows = await page.locator("[data-project-row]").count();
      if (rows !== 2) {
        fail(`the directory shows ${rows} project rows after two creates; two were expected`);
      }
      await screenshot(page, "private-project-created");
    });

    await steps.step("duplicate-address-is-a-field-error-and-creates-nothing", async () => {
      const before = await page.locator("[data-project-row]").count();
      const open = await requireVisible(
        page,
        '[data-project-create="open"]',
        "the directory's create control",
      );
      await open.click();
      await requireVisible(page, '[data-project-create="form"]', "the create panel");

      await page.locator('[data-project-field="name"]').fill(`${PUBLIC_NAME} Copy`);
      await page.locator('[data-project-field="purpose"]').fill("A second attempt that must not be created.");
      await page.locator('[data-project-field="slug"]').fill(PUBLIC_SLUG);

      const probe = evidence.network.length;
      await page.locator('[data-project-create="submit"]').click();

      // The sentence belongs to the address control: same field container,
      // and the input itself is marked invalid. It is awaited FIRST because
      // it is the product's answer to the duplicate, and because it can only
      // render after the API has refused — which also settles the network
      // read below (the click's promise resolves before the response is
      // recorded; reading the log on the next line is the race that made the
      // restored run report "no response" for a refusal the page received).
      const fieldError = await requireVisible(
        page,
        '.project-create-field:has(#project-create-slug) [data-project-field-error="slug"]',
        "the duplicate address error under the address field",
      );
      // The refusal came from the API, not from the form guessing or from a
      // client-side check that never asked: the request went out, and the
      // answer was 409.
      if (!observedRequestSince(probe, "POST", "/api/v1/projects")) {
        fail("the duplicate address never reached POST /api/v1/projects; the refusal has to come from the API's slug rule rather than from a client-side guess");
      }
      const write = await waitForResponse(probe, "POST", "/api/v1/projects");
      if (write === null || write.status !== 409) {
        fail(`the duplicate address did not answer 409 from POST /api/v1/projects (observed ${write?.status ?? "no response"})`);
      }
      const errorText = ((await fieldError.textContent()) ?? "").trim();
      if (!/already exists/i.test(errorText)) {
        fail(`the address field error said ${JSON.stringify(errorText)}, which does not explain the duplicate address`);
      }
      const invalid = await page.locator('#project-create-slug[aria-invalid="true"]').count();
      if (invalid !== 1) {
        fail(`the address input is not marked invalid after a duplicate refusal (aria-invalid count ${invalid})`);
      }
      // A field error is not a banner: the form-level message must stay empty.
      await requireGone(page, '[data-project-create="error"]', "a duplicate address must be reported on its field, not as a form-level banner");
      // The panel stays open so the reader can correct the address.
      await requireVisible(page, '[data-project-create="form"]', "the create panel after a duplicate refusal");
      await requireGone(page, '[data-project-create="created"]', "a refused create must not report success");
      await screenshot(page, "duplicate-address-field-error");

      // Nothing was created: the panel closes and the directory is unchanged.
      await page.locator('[data-project-create="cancel"]').click();
      await requireGone(page, '[data-project-create="form"]', "the panel after cancelling");
      const after = await page.locator("[data-project-row]").count();
      if (after !== before) {
        fail(`the directory held ${before} project rows before the duplicate attempt and ${after} after; no project was created`);
      }
      if ((await page.locator(`[data-project-row="${PUBLIC_SLUG}"]`).count()) !== 1) {
        fail(`the address ${PUBLIC_SLUG} does not appear exactly once after the duplicate attempt`);
      }
      await screenshot(page, "directory-unchanged-after-duplicate");
    });

    await steps.step("anonymous-cannot-see-the-private-project", async () => {
      if (privateHref === "" || publicHref === "") {
        fail("the project paths were not captured; the anonymous step cannot check a page it does not know");
      }
      // A FRESH context: no session cookie, no storage, no reuse of the
      // signed-in browser. This is a stranger arriving at the URL.
      const anonymous = await browser.newContext({
        viewport: { width: 1280, height: 900 },
        locale: "en-US",
        timezoneId: "UTC",
      });
      observeContext(anonymous);
      const anonPage = await anonymous.newPage();
      observePage(anonPage);
      try {
        const probe = evidence.network.length;
        const response = await anonPage.goto(`${BASE}${privateHref}`, { waitUntil: "domcontentloaded" });
        if (response === null || response.status() >= 400) {
          fail(`the anonymous visit to the private project's URL did not load the product shell (HTTP ${response?.status() ?? "no response"})`);
        }
        await requireGone(anonPage, ".global-nav-user-handle", "the anonymous context must not hold a session");
        await requireVisible(anonPage, "[data-project-notfound]", "the neutral not-found state for a private project");

        // The API hid the project's EXISTENCE, not merely its content: 404,
        // never 403 (a 403 would confirm the project is there).
        const read = lastResponseSince(probe, "GET", `/api/v1/projects/${privateHref.split("/").pop()}`);
        if (read === null || read.status !== 404) {
          fail(`the anonymous read of the private project answered ${read?.status ?? "no response"}, not the existence-hiding 404`);
        }

        const pageText = (await anonPage.locator("body").textContent()) ?? "";
        if (pageText.includes(PRIVATE_NAME)) {
          fail(`the anonymous page contains the private project's name ${JSON.stringify(PRIVATE_NAME)}`);
        }
        if (pageText.includes(PRIVATE_PURPOSE)) {
          fail("the anonymous page contains the private project's purpose");
        }
        if (pageText.includes(PRIVATE_SLUG)) {
          fail(`the anonymous page contains the private project's address ${JSON.stringify(PRIVATE_SLUG)}`);
        }
        await screenshot(anonPage, "anonymous-private-not-found");

        // And the directory does not announce it either: the public project
        // is listed (it is Public), the private one is absent.
        const directoryProbe = evidence.network.length;
        await anonPage.goto(`${BASE}/projects`, { waitUntil: "domcontentloaded" });
        await requireVisible(anonPage, ".project-directory-title", "the anonymous project directory");
        await requireVisible(
          anonPage,
          `[data-project-row="${PUBLIC_SLUG}"]`,
          "the public project in the anonymous directory",
        );
        await requireGone(
          anonPage,
          `[data-project-row="${PRIVATE_SLUG}"]`,
          "the anonymous directory must not list the private project",
        );
        const listRead = lastResponseSince(directoryProbe, "GET", "/api/v1/projects");
        if (listRead === null || listRead.status !== 200) {
          fail(`the anonymous directory read did not answer 200 (observed ${listRead?.status ?? "no response"})`);
        }
        const directoryText = (await anonPage.locator("body").textContent()) ?? "";
        if (directoryText.includes(PRIVATE_NAME)) {
          fail(`the anonymous directory contains the private project's name ${JSON.stringify(PRIVATE_NAME)}`);
        }
        await screenshot(anonPage, "anonymous-directory-public-only");
      } finally {
        await anonymous.close();
      }
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
  console.error(`web-v2 journey (WEB-04): FAILED — ${errorText(err)}`);
  process.exit(1);
});
