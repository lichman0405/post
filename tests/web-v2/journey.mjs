/**
 * WEB-01 initial journey (docs/70 §6-7, U01 login/session slice).
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

const STEP_NAMES = [
  "open-home-signed-out",
  "open-signin-entry",
  "open-account-establishment-entry",
  "signup-through-primary-button",
  "signup-session-survives-reload",
  "signup-session-shared-by-second-page",
  "signout-through-visible-control",
  "login-through-primary-button",
  "login-session-survives-reload",
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
async function waitSignedIn(page, action, sinceIndex) {
  try {
    await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
  } catch {
    const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
    fail(`${action}: primary button did not reach a signed-in home page (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
  }
  const handle = page.locator(".global-nav-user-handle").first();
  try {
    await handle.waitFor({ state: "visible", timeout: 20000 });
  } catch {
    fail(`${action}: primary button did not reach a signed-in home page (URL ${page.url()}, no account handle in the header)`);
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
 * A 401 from GET /auth/session is the product's signed-out state, not a
 * fault: the nav and the home strip both probe it on every anonymous page.
 * Chromium logs it as a console error because the response is non-2xx. Any
 * other console error remains a failure.
 */
function isExpectedSignedOutProbe(entry) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  return text.includes("401") && (location.includes("/api/v1/auth/session") || text.includes("/api/v1/auth/session"));
}

function observedRequest(method, suffix) {
  return evidence.network.some(
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
      const entry = await requireVisible(page, '.auth-status a[href="/login"]', "the home sign-in link");
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

    await steps.step("login-through-primary-button", async () => {
      const signIn = await requireVisible(page, ".global-nav-signin", "the sign-in link after sign-out");
      await signIn.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const sessionProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      const handle = await waitSignedIn(page, "sign-in", sessionProbe);
      if (!observedRequest("POST", "/api/v1/auth/login")) {
        fail("sign-in did not go through the product's login endpoint");
      }
      log(`signed in through the DOM as ${handle}`);
      await screenshot(page, "signed-in-after-login");
    });

    await steps.step("login-session-survives-reload", async () => {
      const sessionProbe = evidence.network.length;
      await page.reload({ waitUntil: "domcontentloaded" });
      await waitSignedIn(page, "reload after sign-in", sessionProbe);
      await screenshot(page, "signed-in-after-login-reload");
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
