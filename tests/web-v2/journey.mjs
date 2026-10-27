/**
 * WEB-01 initial journey (docs/70 §6-7, U01), extended by T1303 (WEB-02).
 *
 * What this proves:
 *   1. An account is established through the product's own /login sign-up
 *      entry — the harness seeds no account (docs/70 §6.2).
 *   2. Signing in is performed by filling the form and clicking the primary
 *      submit control in a real browser. No test-side request, SQL, CLI or
 *      storage write participates in any business step.
 *   3. The session really persists: it survives a reload and a second page
 *      in the same browser context.
 *   4. Signing out and signing back in work through the visible controls,
 *      and the new session persists across a reload too.
 *
 * WEB-02 (T1303) adds the rest of U01:
 *   5. The signed-in home is a workspace: it exposes the project
 *      (start/continue research) entry and the entry lands on the project
 *      directory ANSWERED by the API — an empty project list is only accepted
 *      together with the 200 response that produced it, so an error container
 *      that happens to share the class cannot pass.
 *   6. The home's create-account entry opens /login?mode=signup and a second
 *      account is established through that deep link, on the same /login
 *      surface (no second authentication page).
 *   7. A wrong password is reported in the reader's words with the form
 *      still usable — and the API really answered 401 for it.
 *   8. Signing in from a product page returns the reader to that page
 *      (`?next=`), instead of dropping every visitor on Home.
 *   9. A session the API no longer accepts (ended by signing out in another
 *      tab, through the product's own menu) renders a recoverable prompt with
 *      a sign-in link to the current page, and that link restores the session.
 *      On Home the current page is "/", the default destination, so the
 *      href is plain /login; the journey asserts that exact value.
 *
 * Every request the browser makes is observed (method/URL/status) for the
 * evidence record; nothing is intercepted, stubbed or fulfilled by the
 * harness. Screenshots and a Playwright trace are written to the run's
 * evidence directory.
 *
 * Required anti-bypass rule: if the page cannot establish an account (the
 * sign-up entry is missing) or the primary button does not act, this script
 * exits non-zero with the reason. It never skips.
 *
 * Usage: node tests/web-v2/journey.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR,
 *        WEB_V2_API_ORIGIN, WEB_V2_ACCOUNT_EMAIL, WEB_V2_ACCOUNT_PASSWORD
 */

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey: FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
const OUT_DIR = process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`;
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";
const EMAIL = process.env.WEB_V2_ACCOUNT_EMAIL ?? `webv2-${RUN_ID.toLowerCase()}@example.test`;
const PASSWORD = process.env.WEB_V2_ACCOUNT_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;
// The deep-link sign-up establishes a SECOND account: reusing EMAIL would be
// rejected as EMAIL_ALREADY_REGISTERED and the step would prove nothing about
// the signup mode of /login.
const ALT_EMAIL = `webv2-alt-${RUN_ID.toLowerCase()}@example.test`;

const STEP_NAMES = [
  "open-home-signed-out",
  "open-signin-entry",
  "open-account-establishment-entry",
  "signup-through-primary-button",
  "home-exposes-project-entry",
  "signup-session-survives-reload",
  "signup-session-shared-by-second-page",
  "signout-through-visible-control",
  "signup-via-create-account-deep-link",
  "signout-after-deep-link-signup",
  "return-to-task-entry-carries-next",
  "wrong-password-shows-recoverable-error",
  "login-returns-to-original-task",
  "login-session-survives-reload",
  "expired-session-offers-recovery",
  "recover-session-through-signin",
];

const steps = new JourneySteps("U01-login-session", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "U01-login-session",
  role: "anonymous visitor -> newly registered account A",
  accountEmail: EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

function log(message) {
  console.log(`web-v2 journey: ${message}`);
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

/**
 * Wait until the header shows a signed-in account, or fail with the page's
 * own words. `sinceIndex` is the network log's length captured immediately
 * before the action: only a session confirmation observed AFTER that point
 * counts, so a 200 from the sign-up phase cannot satisfy the later sign-in
 * assertion (and vice versa).
 */
async function waitSignedIn(page, action, sinceIndex, expectedPath = "/") {
  // The wording is kept for the sign-up action verbatim: mutation-check.sh
  // requires a disconnected primary button to fail with this sentence, and a
  // check that only greps "something failed" would accept any red.
  const destination =
    expectedPath === "/" ? "a signed-in home page" : `a signed-in page at ${expectedPath}`;
  try {
    await page.waitForURL((url) => url.pathname === expectedPath, { timeout: 20000 });
  } catch {
    const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
    fail(`${action}: primary button did not reach ${destination} (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
  }
  const handle = page.locator(".global-nav-user-handle").first();
  try {
    await handle.waitFor({ state: "visible", timeout: 20000 });
  } catch {
    fail(`${action}: primary button did not reach ${destination} (URL ${page.url()}, no account handle in the header)`);
  }
  const text = (await handle.textContent())?.trim() ?? "";
  if (text === "") {
    fail(`${action}: the header reports a session but no handle; the signed-in state is not readable`);
  }
  // The API must have confirmed the session, not just the client's local
  // state, and that confirmation must belong to this action.
  const sessionOk = evidence.network.slice(sinceIndex).some(
    (entry) =>
      entry.event === "response" &&
      entry.method === "GET" &&
      entry.status === 200 &&
      entry.url.endsWith("/api/v1/auth/session"),
  );
  if (!sessionOk) {
    fail(`${action}: the header shows a user but no successful GET /api/v1/auth/session was observed after this action`);
  }
  return text;
}

/**
 * Two 401s are part of the product, not faults:
 *
 *   - GET /auth/session answering 401 is the signed-out state: the nav and
 *     the home workbench probe it on every anonymous page, and on a reload
 *     after the session ended (the expired-session step below);
 *   - POST /auth/login answering 401 is the rejected sign-in the
 *     wrong-password step below performs on purpose.
 *
 * Chromium logs both as console errors because the response is non-2xx. The
 * rejection is asserted by its own step (the error flash and the 401 on that
 * exact request), so letting its console line through here does not excuse a
 * silent failure. Any other console error remains a run failure.
 */
function isExpectedSignedOutProbe(entry) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  if (!text.includes("401")) return false;
  return (
    location.includes("/api/v1/auth/session") ||
    text.includes("/api/v1/auth/session") ||
    location.includes("/api/v1/auth/login")
  );
}

/** The last observed response for `method` + URL suffix at or after `since`,
 *  or null. `since` is a slice of evidence.network captured before an action,
 *  so a response from an earlier phase cannot satisfy a later assertion. */
function lastResponseSince(since, method, suffix) {
  const responses = evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "response" &&
        entry.method === method &&
        entry.url.endsWith(suffix),
    );
  return responses.length === 0 ? null : responses[responses.length - 1];
}

/** The last observed response for `method` + URL suffix, or null. */
function lastResponse(method, suffix) {
  const responses = evidence.network.filter(
    (entry) =>
      entry.event === "response" &&
      entry.method === method &&
      entry.url.endsWith(suffix),
  );
  return responses.length === 0 ? null : responses[responses.length - 1];
}

function observedRequest(method, suffix) {
  return evidence.network.some(
    (entry) => entry.event === "request" && entry.method === method && entry.url.endsWith(suffix),
  );
}

function observedRequestSince(since, method, suffix) {
  return evidence.network
    .slice(since)
    .some(
      (entry) => entry.event === "request" && entry.method === method && entry.url.endsWith(suffix),
    );
}

// A key-step screenshot is evidence, not decoration: if it cannot be
// written, the run fails. Swallowing this would let a green result point at
// a screenshot that does not exist.
async function screenshot(page, label) {
  await evidence.screenshot(page, label);
}

async function run() {
  await evidence.init();
  log(`run ${RUN_ID} against ${BASE} (commit ${COMMIT})`);

  const browser = await chromium.launch({ headless: true });
  const browserVersion = browser.version();
  const context = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  let result = "failed";
  let failure;

  // Observe, never intercept. context events cover every page in this run.
  context.on("request", (request) => evidence.addRequest(request));
  context.on("response", (response) => evidence.addResponse(response));
  context.on("requestfailed", (request) => evidence.addFailedRequest(request, request.failure()));
  await context.tracing.start({ screenshots: true, snapshots: true, sources: false });

  // Every page in this context is observed, not only the first: the
  // journey opens a second page to prove the session is shared, and a
  // console error or page error there must fail the run too.
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
  context.on("page", observePage);

  const page = await context.newPage();
  observePage(page);

  try {
    await steps.step("open-home-signed-out", async () => {
      const response = await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`home page did not load: HTTP ${response?.status() ?? "no response"} at ${BASE}/`);
      }
      await requireVisible(page, ".global-nav-signin", "the signed-out sign-in entry");
      await requireGone(page, ".global-nav-user-handle", "a fresh browser context must start signed out");
      await screenshot(page, "home-signed-out");
    });

    await steps.step("open-signin-entry", async () => {
      // Normal entry from the product's own home page, not a typed deep link.
      // The signed-out home renders the sign-in entry itself (docs/05 §2: the
      // anonymous home is product content with a sign-in entry, not a
      // marketing page), so this is the entry a first-time visitor sees.
      const entry = await requireVisible(
        page,
        '[data-home-entry="sign-in"]',
        "the home sign-in entry",
      );
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      await requireVisible(page, ".auth-card", "the sign-in card");
      await screenshot(page, "login-page");
    });

    await steps.step("open-account-establishment-entry", async () => {
      await requireVisible(page, 'form input[type="email"]', "the sign-in email field");
      await requireVisible(page, 'form input[type="password"]', "the sign-in password field");
      await requireVisible(page, 'form button[type="submit"]', "the primary sign-in button");
      const accountEntry = await requireVisible(
        page,
        ".auth-sub-after button",
        "the account-establishment entry (create an account)",
      );
      await accountEntry.click();
      // The mode switch is confirmed by the API call it enables: the next
      // submit must be POST /auth/signup, not POST /auth/login. That is
      // checked in the next step, where the result is observable.
      await page.waitForTimeout(100);
      await screenshot(page, "signup-entry");
    });

    await steps.step("signup-through-primary-button", async () => {
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      await screenshot(page, "signup-filled");
      const sessionProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const handle = await waitSignedIn(page, "sign-up", sessionProbe);
      if (!observedRequest("POST", "/api/v1/auth/signup")) {
        fail("sign-up did not go through the product's signup endpoint; the account-establishment entry did not switch the form");
      }
      // The browser's storage is deliberately NOT read here: the session is
      // proven by behaviour (a 200 /auth/session and the reload/second-page
      // steps), not by pulling a cookie out of the context. Chromium does
      // not expose Set-Cookie on this fetch to the page or to the protocol
      // event we record, which is why the evidence notes the request/status
      // pair rather than a cookie name.
      log(`signed up through the DOM as ${handle}`);
      await screenshot(page, "signed-in-after-signup");
    });

    await steps.step("home-exposes-project-entry", async () => {
      // WEB-02's G02 half: the signed-in home is a workspace with the entry
      // into starting or continuing research (docs/05 §2). The entry must be
      // visible after logging in — not typed as a URL — and it must lead to
      // the project workspace surface. A new account has no projects, so the
      // honest answer there is the empty state, which this step also checks:
      // an entry that lands on an error page is not a working entry.
      await requireVisible(page, "#home-projects-heading", "the home My Projects section");
      // The home's own list must be ANSWERED, not erroring: the empty/list
      // marker is rendered only after GET /api/v1/projects resolves, while
      // the error branch renders .home-empty with no marker at all. A 200
      // for that read is the second half of the same claim.
      await requireVisible(
        page,
        '[data-home-projects="empty"], [data-home-projects="list"]',
        "the home project list's answered state",
      );
      const homeList = lastResponse("GET", "/api/v1/projects");
      if (homeList === null || homeList.status !== 200) {
        fail(`the home project list did not come from a 200 GET /api/v1/projects (observed ${homeList?.status ?? "no response"})`);
      }
      const entry = await requireVisible(
        page,
        '[data-home-entry="project-workspaces"]',
        "the home's project (start/continue research) entry",
      );
      const href = await entry.getAttribute("href");
      if (href !== "/projects") {
        fail(`the home project entry points at ${JSON.stringify(href)}, not the project workspace /projects`);
      }
      const label = ((await entry.textContent()) ?? "").trim();
      if (label === "") {
        fail("the home project entry is visible but carries no label; the reader cannot know what it does");
      }
      const directoryProbe = evidence.network.length;
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 15000 });
      await requireVisible(page, ".project-directory-title", "the project directory after the home entry");
      const listState = page.locator(".project-list, .project-list-empty").first();
      try {
        await listState.waitFor({ state: "visible", timeout: 15000 });
      } catch {
        fail(`the project directory did not finish loading after the home entry (URL ${page.url()})`);
      }
      // .project-list-empty is ALSO the error container (project-directory.tsx
      // renders loadError through it), so "a container is visible" proves
      // nothing. The directory is accepted only when the list came from a 200
      // for this navigation AND the text is the answered empty state — a
      // 401/network failure renders an error sentence here and fails.
      const directoryList = lastResponseSince(directoryProbe, "GET", "/api/v1/projects");
      if (directoryList === null || directoryList.status !== 200) {
        fail(`the project directory did not get a 200 from GET /api/v1/projects (observed ${directoryList?.status ?? "no response"})`);
      }
      const directoryText = ((await listState.textContent()) ?? "").trim();
      if (!/^No projects yet\./i.test(directoryText)) {
        fail(`the project directory showed ${JSON.stringify(directoryText)}; a new account's answered empty state was expected (an error container renders the same class)`);
      }
      await screenshot(page, "home-project-entry");
      // Back to the home page through its own wordmark control: the next
      // steps assert the signed-in home and its session, not the directory.
      const logo = await requireVisible(page, ".global-nav-logo", "the global navigation wordmark");
      await logo.click();
      await page.waitForURL((url) => url.pathname === "/", { timeout: 15000 });
      await requireVisible(page, ".global-nav-user-handle", "the account handle after returning home");
    });

    await steps.step("signup-session-survives-reload", async () => {
      const sessionProbe = evidence.network.length;
      await page.reload({ waitUntil: "domcontentloaded" });
      await waitSignedIn(page, "reload after sign-up", sessionProbe);
      await screenshot(page, "signed-in-after-reload");
    });

    await steps.step("signup-session-shared-by-second-page", async () => {
      const second = await context.newPage();
      try {
        const response = await second.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
        if (response === null || response.status() >= 400) {
          fail(`second page did not load: HTTP ${response?.status() ?? "no response"}`);
        }
        await requireVisible(second, ".global-nav-user-handle", "the account handle on a second page");
      } finally {
        await second.close();
      }
    });

    await steps.step("signout-through-visible-control", async () => {
      await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      // Sign out through the header's account menu: the global navigation
      // owns the shared session state every page sees, so this is the
      // control whose effect the next step can observe on the same page.
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
      if (!observedRequest("POST", "/api/v1/auth/logout")) {
        fail("sign-out did not call the product's logout endpoint");
      }
      await screenshot(page, "signed-out-after-signout");
    });

    await steps.step("signup-via-create-account-deep-link", async () => {
      // The home's create-account entry opens the SAME /login surface in its
      // create-account mode (`?mode=signup`) — there is no second auth page.
      // This step uses that entry end to end, so the deep link is exercised
      // rather than assumed; the in-card toggle was already exercised above.
      const entry = await requireVisible(
        page,
        '[data-home-entry="create-account"]',
        "the home create-account entry",
      );
      const href = await entry.getAttribute("href");
      if (href !== "/login?mode=signup") {
        fail(`the home create-account entry points at ${JSON.stringify(href)}, not the create-account mode of /login`);
      }
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      if (new URL(page.url()).searchParams.get("mode") !== "signup") {
        fail(`the create-account entry landed on ${page.url()} without mode=signup`);
      }
      // Prove the card really opened in create-account mode BEFORE typing:
      // the password field asks for a new password (autocomplete) and the
      // primary control offers to create an account.
      await requireVisible(
        page,
        'form input[type="password"][autocomplete="new-password"]',
        "the create-account password field on /login?mode=signup",
      );
      const submitLabel = ((await page.locator('form button[type="submit"]').textContent()) ?? "").trim();
      if (submitLabel !== "Create account") {
        fail(`the primary control on /login?mode=signup says ${JSON.stringify(submitLabel)}, not the create-account action`);
      }
      await page.locator('form input[type="email"]').fill(ALT_EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const sessionProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const handle = await waitSignedIn(page, "sign-up via ?mode=signup", sessionProbe);
      if (!observedRequestSince(sessionProbe, "POST", "/api/v1/auth/signup")) {
        fail("the ?mode=signup form did not submit to the product's signup endpoint");
      }
      log(`established a second account through /login?mode=signup as ${handle}`);
      await screenshot(page, "signed-in-via-signup-deeplink");
    });

    await steps.step("signout-after-deep-link-signup", async () => {
      // Back to the signed-out state the return-to-task steps need, through
      // the same visible control.
      const accountMenu = await requireVisible(page, ".global-nav-user", "the account menu after the deep-link sign-up");
      await accountMenu.click();
      const menuItems = page.locator('[role="menuitem"]');
      await menuItems.first().waitFor({ state: "visible", timeout: 10000 });
      if ((await menuItems.count()) < 2) {
        fail("the account menu after the deep-link sign-up offers no sign-out item");
      }
      await menuItems.last().click();
      await requireVisible(page, ".global-nav-signin", "the signed-out state after the second sign-out");
      await requireGone(page, ".global-nav-user-handle", "the second sign-out must clear the signed-in header");
      await screenshot(page, "signed-out-after-deeplink-signout");
    });

    await steps.step("return-to-task-entry-carries-next", async () => {
      // "Return to the task you were on" (WEB-02): a reader who signs in
      // from a product page is sent back to THAT page, not dropped on Home.
      // The unsigned-in header link carries the current path; this checks
      // the value it produces and that the sign-in form honours it.
      const response = await page.goto(`${BASE}/projects`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`the project directory did not load while signed out: HTTP ${response?.status() ?? "no response"}`);
      }
      const signIn = await requireVisible(page, ".global-nav-signin", "the signed-out header sign-in entry");
      const href = await signIn.getAttribute("href");
      if (href !== "/login?next=%2Fprojects") {
        fail(`the signed-out header on /projects links to ${JSON.stringify(href)}, not /login?next=%2Fprojects — signing in would lose the task`);
      }
      await signIn.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      const next = new URL(page.url()).searchParams.get("next");
      if (next !== "/projects") {
        fail(`the sign-in page received next=${JSON.stringify(next)}, not the originating /projects`);
      }
      await requireVisible(page, 'form input[type="email"]', "the sign-in email field");
      await screenshot(page, "return-to-task-signin");
    });

    await steps.step("wrong-password-shows-recoverable-error", async () => {
      // U01: a rejected credential is reported in the reader's words and the
      // form stays usable — a recoverable error, not a dead end and not a
      // silent no-op. The 401 on POST /auth/login is the assertion that the
      // API really rejected it; the flash is the assertion that the reader
      // was told.
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(`${PASSWORD}-wrong`);
      const before = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const alert = await requireVisible(page, '.auth-card [role="alert"]', "the rejected sign-in message");
      const text = ((await alert.textContent()) ?? "").trim();
      if (!/invalid email or password/i.test(text)) {
        fail(`the rejected sign-in said ${JSON.stringify(text)}, which does not tell the reader the credential was wrong`);
      }
      if (new URL(page.url()).pathname !== "/login") {
        fail(`a rejected sign-in navigated to ${page.url()}; the reader must stay on the form to correct it`);
      }
      const rejected = evidence.network
        .slice(before)
        .filter(
          (entry) =>
            entry.event === "response" &&
            entry.method === "POST" &&
            entry.url.endsWith("/api/v1/auth/login"),
        )
        .pop();
      if (rejected === undefined || rejected.status !== 401) {
        fail(`the wrong-password attempt did not get a 401 from POST /api/v1/auth/login (observed ${rejected?.status ?? "no response"})`);
      }
      // Recoverable means the reader can try again: the fields and the
      // primary control are still there and enabled.
      await requireVisible(page, 'form input[type="email"]', "the email field after a rejected sign-in");
      const submit = page.locator('form button[type="submit"]');
      if (!(await submit.isEnabled())) {
        fail("the primary sign-in button is disabled after a rejected sign-in; the reader cannot retry");
      }
      await screenshot(page, "wrong-password-error");
    });

    await steps.step("login-returns-to-original-task", async () => {
      // Retry on the same form with the right credential. The reader was on
      // /projects when they asked to sign in, so that is where they land.
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const sessionProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const handle = await waitSignedIn(page, "sign-in", sessionProbe, "/projects");
      if (!observedRequest("POST", "/api/v1/auth/login")) {
        fail("sign-in did not go through the product's login endpoint");
      }
      log(`signed in through the DOM as ${handle}, returned to /projects`);
      await screenshot(page, "signed-in-returned-to-task");
    });

    await steps.step("login-session-survives-reload", async () => {
      // The session persists on the page it returned the reader to.
      const sessionProbe = evidence.network.length;
      await page.reload({ waitUntil: "domcontentloaded" });
      await waitSignedIn(page, "reload after sign-in", sessionProbe, "/projects");
      await screenshot(page, "signed-in-after-login-reload");
    });

    await steps.step("expired-session-offers-recovery", async () => {
      // U01's second half: a session that is no longer accepted is reported
      // as exactly that, with a way back — not a silent sign-out.
      //
      // The expiry is produced through the product's own UI: signing out in
      // a second tab revokes the shared session while the first tab still
      // holds its per-tab CSRF token. On reload the first tab's session probe
      // answers 401 with a token still present, which is the "session ended"
      // state (the server is never touched by the test).
      await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      await requireVisible(page, ".global-nav-user-handle", "the signed-in home before the session ends");
      const second = await context.newPage();
      try {
        const response = await second.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
        if (response === null || response.status() >= 400) {
          fail(`the second tab did not load: HTTP ${response?.status() ?? "no response"}`);
        }
        const menu = await requireVisible(second, ".global-nav-user", "the account menu in the second tab");
        await menu.click();
        const items = second.locator('[role="menuitem"]');
        await items.first().waitFor({ state: "visible", timeout: 10000 });
        if ((await items.count()) < 2) {
          fail("the account menu in the second tab offers no sign-out item");
        }
        await items.last().click();
        await requireVisible(second, ".global-nav-signin", "the signed-out state in the second tab");
      } finally {
        await second.close();
      }

      const probe = evidence.network.length;
      await page.reload({ waitUntil: "domcontentloaded" });
      // Wait for the prompt first: the session probe is a client fetch that
      // resolves after the document is loaded, and the prompt only renders
      // once that 401 has been answered.
      const recover = await requireVisible(
        page,
        '[data-home-entry="recover-session"]',
        "the recoverable session-ended prompt",
      );
      const sessionProbe = evidence.network
        .slice(probe)
        .filter(
          (entry) =>
            entry.event === "response" &&
            entry.method === "GET" &&
            entry.url.endsWith("/api/v1/auth/session"),
        )
        .pop();
      if (sessionProbe === undefined || sessionProbe.status !== 401) {
        fail(`the reload after the session ended did not probe GET /api/v1/auth/session to a 401 (observed ${sessionProbe?.status ?? "no response"})`);
      }
      const flash = ((await page.locator(".home-flash").first().textContent()) ?? "").trim();
      if (!/session has ended/i.test(flash)) {
        fail(`the session-ended state said ${JSON.stringify(flash)}, which does not tell the reader the session ended`);
      }
      if ((await page.locator(".global-nav-user-handle").count()) !== 0) {
        fail("the header still claims a signed-in account after the session ended");
      }
      // MEASURED behaviour, not an assumption: the prompt is on Home, whose
      // path is safeNextPath's default destination "/", so loginHref produces
      // plain /login and NO next= parameter. The step asserts that exact href;
      // the destination after recovery is asserted in the next step ("/").
      const recoverHref = await recover.getAttribute("href");
      if (recoverHref !== "/login") {
        fail(`the recovery prompt links to ${JSON.stringify(recoverHref)}, not /login (Home is the default destination, so no ?next= is produced)`);
      }
      await screenshot(page, "session-ended-recoverable");
    });

    await steps.step("recover-session-through-signin", async () => {
      // Recovery is real: the prompt's own sign-in control restores a
      // session and returns the reader to the page they were on.
      const recover = await requireVisible(page, '[data-home-entry="recover-session"]', "the recovery sign-in link");
      await recover.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const sessionProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const handle = await waitSignedIn(page, "recovery sign-in", sessionProbe);
      const sessionOk = evidence.network.slice(sessionProbe).some(
        (entry) =>
          entry.event === "response" &&
          entry.method === "GET" &&
          entry.status === 200 &&
          entry.url.endsWith("/api/v1/auth/session"),
      );
      if (!sessionOk) {
        fail("recovery sign-in did not produce a confirmed session after the action");
      }
      log(`recovered the session as ${handle}`);
      await screenshot(page, "session-recovered");
    });

    steps.assertComplete();
    result = "passed";
    log("all declared steps passed");
  } catch (err) {
    failure = err;
    log(`FAILED: ${errorText(err)}`);
    // One screenshot of the failing state is the most useful artifact. If
    // even that cannot be written, the run is still failed for its original
    // reason and additionally reports the evidence write failure.
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

    // A page error, a failed request or a server error is never waved
    // through: docs/70 §6.1 requires the console/network fault to fail the
    // run, not decorate it.
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
          (entry) => entry.kind === "error" && !isExpectedSignedOutProbe(entry),
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
  console.error(`web-v2 journey: FAILED — ${errorText(err)}`);
  process.exit(1);
});
