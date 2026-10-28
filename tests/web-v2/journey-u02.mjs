/**
 * WEB-03 (T1304, G03) real-UI journey: U02 — organizations, members and
 * roles (docs/70 §6-7, docs/70's U02 line).
 *
 * What this proves, with three real accounts established by the browser and
 * three real sessions in three isolated browser contexts:
 *
 *   1. A creates an organization through /organizations' own create form and
 *      becomes its owner (the create response and the member page agree).
 *   2. B and V have their own accounts (created through the product's home
 *      create-account entry) and A invites them BY HANDLE through the invite
 *      form as Contributor and Viewer respectively.
 *   3. V, a Viewer, selects "Owner" on its own row and saves: the API refuses
 *      the write (403, ORG_FORBIDDEN) and the refusal is on the page in the
 *      reader's words; the stored role is unchanged and stays unchanged
 *      across a reload.
 *   4. A changes B's role Contributor -> Maintainer through the member page;
 *      the change survives A's reload AND B's own page load/reload, and B's
 *      affiliation start date — the membership history — is not rewritten.
 *   5. V leaves the organization through its own "Leave organization"
 *      control; the list it can read no longer contains the organization,
 *      while A's member list still shows V's row — ended, with the role it
 *      held, a "Left <date>" departure date and no live role control
 *      (离职不删除历史). This step does not compare the affiliation start
 *      date; the start-date comparison belongs to the role-change step (4).
 *
 * Anti-bypass (docs/70 §6.2): every business step above is a normal visible
 * control in a real browser. No test-side request, SQL, CLI, seeded account,
 * cookie/storage write or intercepted response participates. The accounts are
 * created through /login; the organization, invitations, role changes and
 * leave are issued by the pages themselves. Requests are OBSERVED only.
 *
 * Usage: node tests/web-v2/journey-u02.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR, WEB_V2_API_ORIGIN
 */

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey U02: FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
const OUT_DIR = process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`;
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";

const ID = RUN_ID.toLowerCase().replace(/[^a-z0-9-]/g, "-");
const OWNER_EMAIL = `u02-owner-${ID}@example.test`;
const CONTRIBUTOR_EMAIL = `u02-b-${ID}@example.test`;
const VIEWER_EMAIL = `u02-v-${ID}@example.test`;
const PASSWORD = `webv2-synthetic-${RUN_ID}-password`;
const ORG_SLUG = `catalysis-${ID}`.slice(0, 64);
const ORG_NAME = "Institute of Synthetic Catalysis";

const STEP_NAMES = [
  "open-organizations-signed-out",
  "signup-owner-a",
  "create-organization-through-web-form",
  "owner-sees-self-as-member",
  "signup-contributor-b",
  "signup-viewer-v",
  "invite-b-as-contributor",
  "invite-v-as-viewer",
  "viewer-cannot-promote-itself",
  "owner-changes-b-role-and-reload-persists",
  "b-sees-role-change-after-reload",
  "viewer-leaves-and-history-is-kept",
];

const steps = new JourneySteps("U02-organization-members", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/organizations`,
  journeyName: "U02-organization-members",
  task: "T1304",
  role: "owner A; contributor B; viewer V (three real sessions)",
  accountEmail: OWNER_EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

function log(message) {
  console.log(`web-v2 U02: ${message}`);
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

async function screenshot(page, label) {
  await evidence.screenshot(page, label);
}

/** Responses at or after `since` whose method matches and URL contains `fragment`. */
function responsesSince(since, method, fragment) {
  return evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "response" &&
        entry.method === method &&
        entry.url.includes(fragment),
    );
}

function lastResponseSince(since, method, fragment) {
  const rows = responsesSince(since, method, fragment);
  return rows.length === 0 ? null : rows[rows.length - 1];
}

function observedRequestSince(since, method, fragment) {
  return evidence.network
    .slice(since)
    .some(
      (entry) =>
        entry.event === "request" &&
        entry.method === method &&
        entry.url.includes(fragment),
    );
}

/**
 * Wait until the header shows a signed-in account and that specific action
 * produced a 200 GET /auth/session. The network position `since` keeps a
 * response from an earlier phase from satisfying this one.
 */
async function waitSignedIn(page, action, since, expectedPath) {
  try {
    await page.waitForURL((url) => url.pathname === expectedPath, { timeout: 20000 });
  } catch {
    const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
    fail(
      `${action}: the form did not reach ${expectedPath} (URL ${page.url()}${
        flash ? `; page reported: ${flash.trim()}` : ""
      })`,
    );
  }
  const handle = page.locator(".global-nav-user-handle").first();
  try {
    await handle.waitFor({ state: "visible", timeout: 20000 });
  } catch {
    fail(`${action}: reached ${expectedPath} but the header reports no account`);
  }
  const text = ((await handle.textContent()) ?? "").trim();
  if (text === "") {
    fail(`${action}: the header reports a session but no handle`);
  }
  const sessionOk = evidence.network
    .slice(since)
    .some(
      (entry) =>
        entry.event === "response" &&
        entry.method === "GET" &&
        entry.status === 200 &&
        entry.url.endsWith("/api/v1/auth/session"),
    );
  if (!sessionOk) {
    fail(`${action}: no 200 GET /api/v1/auth/session was observed after the action`);
  }
  return text;
}

/**
 * Establish a fresh account through the product's own home create-account
 * entry (/login?mode=signup — the same /login surface, not a second auth
 * page). Returns the page and the handle the API assigned.
 */
async function signUp(page, email, label) {
  const response = await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
  if (response === null || response.status() >= 400) {
    fail(`${label}: the home page did not load (HTTP ${response?.status() ?? "no response"})`);
  }
  const entry = await requireVisible(
    page,
    '[data-home-entry="create-account"]',
    `${label}: the home create-account entry`,
  );
  await entry.click();
  await page.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
  if (new URL(page.url()).searchParams.get("mode") !== "signup") {
    fail(`${label}: the create-account entry did not open /login?mode=signup`);
  }
  await requireVisible(
    page,
    'form input[type="password"][autocomplete="new-password"]',
    `${label}: the create-account password field`,
  );
  await page.locator('form input[type="email"]').fill(email);
  await page.locator('form input[type="password"]').fill(PASSWORD);
  const probe = evidence.network.length;
  await page.locator('form button[type="submit"]').click();
  const handle = await waitSignedIn(page, label, probe, "/");
  if (!observedRequestSince(probe, "POST", "/api/v1/auth/signup")) {
    fail(`${label}: the create-account form did not submit to the product's signup endpoint`);
  }
  return handle;
}

async function run() {
  await evidence.init();
  log(`run ${RUN_ID} against ${BASE} (commit ${COMMIT})`);

  const browser = await chromium.launch({ headless: true });
  const browserVersion = browser.version();

  const contextOwner = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  const contextB = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  const contextV = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });

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
  function observeContext(context) {
    context.on("request", (request) => evidence.addRequest(request));
    context.on("response", (response) => evidence.addResponse(response));
    context.on("requestfailed", (request) => evidence.addFailedRequest(request, request.failure()));
    context.on("page", observePage);
  }
  observeContext(contextOwner);
  observeContext(contextB);
  observeContext(contextV);

  let result = "failed";
  let failure;
  let handleA = "";
  let handleB = "";
  let handleV = "";
  const pageA = await contextOwner.newPage();
  observePage(pageA);
  const pageB = await contextB.newPage();
  observePage(pageB);
  const pageV = await contextV.newPage();
  observePage(pageV);

  await contextOwner.tracing.start({ screenshots: true, snapshots: true, sources: false });

  try {
    await steps.step("open-organizations-signed-out", async () => {
      const response = await pageA.goto(`${BASE}/organizations`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`/organizations did not load: HTTP ${response?.status() ?? "no response"}`);
      }
      await requireVisible(pageA, ".org-index-title", "the organizations heading");
      const signIn = await requireVisible(
        pageA,
        "[data-org-signin]",
        "the anonymous organizations workspace sign-in entry",
      );
      const href = await signIn.getAttribute("href");
      if (href !== "/login?next=%2Forganizations") {
        fail(`the anonymous workspace sign-in links to ${JSON.stringify(href)}, not /login?next=%2Forganizations`);
      }
      await screenshot(pageA, "organizations-signed-out");
    });

    await steps.step("signup-owner-a", async () => {
      const signIn = await requireVisible(pageA, "[data-org-signin]", "the workspace sign-in entry");
      await signIn.click();
      await pageA.waitForURL((url) => url.pathname === "/login", { timeout: 15000 });
      const next = new URL(pageA.url()).searchParams.get("next");
      if (next !== "/organizations") {
        fail(`the sign-in page received next=${JSON.stringify(next)}, not /organizations`);
      }
      // The same /login card, switched to create-account mode by its own
      // entry — A is a brand-new account, established by the browser.
      const toggle = await requireVisible(
        pageA,
        ".auth-sub-after button",
        "the create-an-account entry on /login",
      );
      await toggle.click();
      await requireVisible(
        pageA,
        'form input[type="password"][autocomplete="new-password"]',
        "the create-account password field",
      );
      await pageA.locator('form input[type="email"]').fill(OWNER_EMAIL);
      await pageA.locator('form input[type="password"]').fill(PASSWORD);
      const probe = evidence.network.length;
      await pageA.locator('form button[type="submit"]').click();
      handleA = await waitSignedIn(pageA, "owner sign-up", probe, "/organizations");
      if (!observedRequestSince(probe, "POST", "/api/v1/auth/signup")) {
        fail("A's sign-up did not go through the product's signup endpoint");
      }
      log(`owner A established through the DOM as ${handleA}`);
      await screenshot(pageA, "owner-signed-in");
    });

    await steps.step("create-organization-through-web-form", async () => {
      await requireVisible(pageA, "[data-org-create]", "the create-organization form");
      await pageA.locator("[data-org-create-name]").fill(ORG_NAME);
      await pageA.locator("[data-org-create-slug]").fill(ORG_SLUG);
      await pageA
        .locator("[data-org-create-description]")
        .fill("A team studying solid-state catalysis and its reproducibility.");
      const probe = evidence.network.length;
      await pageA.locator("[data-org-create-submit]").click();
      await pageA.waitForURL(
        (url) => url.pathname === `/organizations/${ORG_SLUG}/members`,
        { timeout: 20000 },
      );
      const created = lastResponseSince(probe, "POST", "/api/v1/organizations");
      if (created === null || created.status !== 201) {
        fail(`the create form did not get a 201 from POST /api/v1/organizations (observed ${created?.status ?? "no response"})`);
      }
      await requireVisible(pageA, "[data-org-members-page]", "the member page after creation");
      const title = ((await pageA.locator(".org-members-title").textContent()) ?? "").trim();
      if (title !== ORG_NAME) {
        fail(`the member page shows ${JSON.stringify(title)}, not the organization just created`);
      }
      await screenshot(pageA, "organization-created");
    });

    await steps.step("owner-sees-self-as-member", async () => {
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleA}"][data-member-role="owner"][data-member-status="active"]`,
        "A's own owner membership row",
      );
      const myRole = await requireVisible(
        pageA,
        '[data-org-my-role="owner"]',
        "the page's report of the reader's own role",
      );
      if (!((await myRole.textContent()) ?? "").includes("Owner")) {
        fail("the role badge says owner in its data attribute but not in words");
      }
      await screenshot(pageA, "owner-member-row");
    });

    await steps.step("signup-contributor-b", async () => {
      handleB = await signUp(pageB, CONTRIBUTOR_EMAIL, "contributor B sign-up");
      if (handleB === handleA) {
        fail("B's account resolved to the same handle as A's");
      }
      log(`contributor B established through the DOM as ${handleB}`);
      await screenshot(pageB, "contributor-signed-in");
    });

    await steps.step("signup-viewer-v", async () => {
      handleV = await signUp(pageV, VIEWER_EMAIL, "viewer V sign-up");
      if (handleV === handleA || handleV === handleB) {
        fail("V's account resolved to another account's handle");
      }
      log(`viewer V established through the DOM as ${handleV}`);
      await screenshot(pageV, "viewer-signed-in");
    });

    await steps.step("invite-b-as-contributor", async () => {
      await pageA.bringToFront();
      // The verified flag is the owner's own statement. The form must not
      // make it for them: with no deliberate check, the invitation is
      // recorded UNVERIFIED (the backend's default too).
      const verifiedBox = pageA.locator("[data-org-invite-verified]");
      await verifiedBox.waitFor({ state: "visible", timeout: 15000 });
      if (await verifiedBox.isChecked()) {
        fail("the invite form arrives with 'verified' already checked; an invitation must not assert a verification the owner did not make");
      }
      await pageA.locator("[data-org-invite-handle]").fill(handleB);
      await pageA.locator("[data-org-invite-role]").selectOption("contributor");
      const probe = evidence.network.length;
      await pageA.locator("[data-org-invite-submit]").click();
      // The row (and the success notice) render only after the POST resolved
      // and the list was re-read, so waiting on them also guarantees the
      // response this step asserts is already in the network record.
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleB}"][data-member-role="contributor"][data-member-status="active"]`,
        "B's contributor membership row",
      );
      await requireVisible(pageA, '[data-org-notice="success"]', "the invite success notice");
      const invited = lastResponseSince(probe, "POST", "/members");
      if (invited === null || invited.status !== 201) {
        fail(`the invite form did not get a 201 from POST .../members (observed ${invited?.status ?? "no response"})`);
      }
      // The record says unverified, and the row says so in words.
      const bVerified = pageA.locator(`tr[data-member-handle="${handleB}"] [data-member-verified]`);
      const bFlag = await bVerified.getAttribute("data-member-verified");
      if (bFlag !== "false") {
        fail(`the default invite recorded verified=${JSON.stringify(bFlag)} for B; an unchecked invite must record the affiliation unverified`);
      }
      const bFlagText = ((await bVerified.textContent()) ?? "").trim();
      if (bFlagText !== "Unverified") {
        fail(`B's row shows ${JSON.stringify(bFlagText)} for an unverified invitation`);
      }
      await screenshot(pageA, "b-invited-as-contributor-unverified");
    });

    await steps.step("invite-v-as-viewer", async () => {
      await pageA.locator("[data-org-invite-handle]").fill(handleV);
      await pageA.locator("[data-org-invite-role]").selectOption("viewer");
      // The one path to verified=true: the owner checks the box themselves.
      await pageA.locator("[data-org-invite-verified]").check();
      const probe = evidence.network.length;
      await pageA.locator("[data-org-invite-submit]").click();
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleV}"][data-member-role="viewer"][data-member-status="active"]`,
        "V's viewer membership row",
      );
      const invited = lastResponseSince(probe, "POST", "/members");
      if (invited === null || invited.status !== 201) {
        fail(`the viewer invite did not get a 201 from POST .../members (observed ${invited?.status ?? "no response"})`);
      }
      const vVerified = pageA.locator(`tr[data-member-handle="${handleV}"] [data-member-verified]`);
      const vFlag = await vVerified.getAttribute("data-member-verified");
      if (vFlag !== "true") {
        fail(`the explicitly verified invite recorded verified=${JSON.stringify(vFlag)} for V; the checked box must record the affiliation verified`);
      }
      const vFlagText = ((await vVerified.textContent()) ?? "").trim();
      if (vFlagText !== "Verified") {
        fail(`V's row shows ${JSON.stringify(vFlagText)} for an explicitly verified invitation`);
      }
      // The statement is per-invitation: it must not stay checked behind
      // the owner and silently verify the next person they invite.
      if (await pageA.locator("[data-org-invite-verified]").isChecked()) {
        fail("the verified box stayed checked after the invite; the next invitation would be verified without a new statement");
      }
      await screenshot(pageA, "v-invited-as-viewer-verified");
    });

    await steps.step("viewer-cannot-promote-itself", async () => {
      // Navigate through the product: home -> the header's Organizations
      // destination -> this organization's Members link. V is not handed a
      // typed deep link.
      await pageV.bringToFront();
      const navLink = await requireVisible(
        pageV,
        '.global-nav-links a[href="/organizations"]',
        "the header Organizations destination for V",
      );
      await navLink.click();
      await pageV.waitForURL((url) => url.pathname === "/organizations", { timeout: 15000 });
      await requireVisible(pageV, '[data-org-list="list"]', "V's organization list");
      const manage = await requireVisible(
        pageV,
        `[data-org-manage="${ORG_SLUG}"]`,
        "the member-page link for V's organization",
      );
      await manage.click();
      await pageV.waitForURL(
        (url) => url.pathname === `/organizations/${ORG_SLUG}/members`,
        { timeout: 15000 },
      );
      await requireVisible(pageV, '[data-org-my-role="viewer"]', "V's own viewer role");
      await requireVisible(
        pageV,
        `tr[data-member-handle="${handleV}"][data-member-role="viewer"]`,
        "V's own viewer membership row",
      );

      // The attempt: select Owner on V's own row and save. The API must
      // refuse it; the page must say so.
      const probe = evidence.network.length;
      await pageV.locator(`[data-role-select="${handleV}"]`).selectOption("owner");
      await pageV.locator(`[data-role-save="${handleV}"]`).click();
      const notice = await requireVisible(
        pageV,
        '[data-org-notice="error"]',
        "the visible refusal of V's self-promotion",
      );
      const text = ((await notice.textContent()) ?? "").trim();
      if (!/only organization owners may perform this action/i.test(text)) {
        fail(`the refusal said ${JSON.stringify(text)}, which does not tell the reader the action needs an owner`);
      }
      const refused = lastResponseSince(probe, "PATCH", "/members/");
      if (refused === null || refused.status !== 403) {
        fail(`V's self-promotion did not get a 403 from PATCH .../members/{id} (observed ${refused?.status ?? "no response"})`);
      }
      // Nothing changed: the control is back on the stored role and the
      // row still says viewer.
      const selected = await pageV.locator(`[data-role-select="${handleV}"]`).inputValue();
      if (selected !== "viewer") {
        fail(`V's role control shows ${JSON.stringify(selected)} after the refused change; the stored role is viewer`);
      }
      const rowRole = await pageV
        .locator(`tr[data-member-handle="${handleV}"]`)
        .getAttribute("data-member-role");
      if (rowRole !== "viewer") {
        fail(`V's row reports role ${JSON.stringify(rowRole)} after the refusal; expected viewer`);
      }

      // Reload: the refusal was not a client-side illusion — the server's
      // stored role is still viewer.
      const reloadProbe = evidence.network.length;
      await pageV.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(pageV, '[data-org-my-role="viewer"]', "V's role after reload");
      await requireVisible(
        pageV,
        `tr[data-member-handle="${handleV}"][data-member-role="viewer"]`,
        "V's viewer row after reload",
      );
      const reloaded = lastResponseSince(reloadProbe, "GET", `/organizations/`);
      if (reloaded === null || reloaded.status !== 200) {
        fail(`the reload did not re-read the member list successfully (observed ${reloaded?.status ?? "no response"})`);
      }
      await screenshot(pageV, "viewer-promotion-refused");
    });

    await steps.step("owner-changes-b-role-and-reload-persists", async () => {
      await pageA.bringToFront();
      const startCell = pageA.locator(
        `tr[data-member-handle="${handleB}"] [data-member-start]`,
      );
      await startCell.waitFor({ state: "visible", timeout: 15000 });
      const startBefore = ((await startCell.textContent()) ?? "").trim();
      if (startBefore === "") {
        fail("B's affiliation start date is not rendered; the history claim cannot be checked");
      }

      const probe = evidence.network.length;
      await pageA.locator(`[data-role-select="${handleB}"]`).selectOption("maintainer");
      await pageA.locator(`[data-role-save="${handleB}"]`).click();
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleB}"][data-member-role="maintainer"]`,
        "B's maintainer row after the change",
      );
      await requireVisible(pageA, '[data-org-notice="success"]', "the role-change success notice");
      const changed = lastResponseSince(probe, "PATCH", "/members/");
      if (changed === null || changed.status !== 200) {
        fail(`the owner's role change did not get a 200 from PATCH .../members/{id} (observed ${changed?.status ?? "no response"})`);
      }

      // Reload A's page: the change is the server's, so it survives.
      const reloadProbe = evidence.network.length;
      await pageA.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleB}"][data-member-role="maintainer"][data-member-status="active"]`,
        "B's maintainer row after A's reload",
      );
      const reloaded = lastResponseSince(reloadProbe, "GET", `/organizations/`);
      if (reloaded === null || reloaded.status !== 200) {
        fail(`A's reload did not re-read the member list successfully (observed ${reloaded?.status ?? "no response"})`);
      }
      const startAfter = (
        (await pageA.locator(`tr[data-member-handle="${handleB}"] [data-member-start]`).textContent()) ?? ""
      ).trim();
      if (startAfter !== startBefore) {
        fail(`the role change rewrote B's affiliation start (${startBefore} -> ${startAfter}); history must be kept`);
      }
      await screenshot(pageA, "b-role-changed-after-reload");
    });

    await steps.step("b-sees-role-change-after-reload", async () => {
      // The affected member's own session sees the new role: the change is
      // not just the owner's screen state.
      await pageB.bringToFront();
      const navLink = await requireVisible(
        pageB,
        '.global-nav-links a[href="/organizations"]',
        "the header Organizations destination for B",
      );
      await navLink.click();
      await pageB.waitForURL((url) => url.pathname === "/organizations", { timeout: 15000 });
      const manage = await requireVisible(
        pageB,
        `[data-org-manage="${ORG_SLUG}"]`,
        "the member-page link for B's organization",
      );
      await manage.click();
      await pageB.waitForURL(
        (url) => url.pathname === `/organizations/${ORG_SLUG}/members`,
        { timeout: 15000 },
      );
      await requireVisible(pageB, '[data-org-my-role="maintainer"]', "B's own maintainer role");
      const probe = evidence.network.length;
      await pageB.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        pageB,
        `tr[data-member-handle="${handleB}"][data-member-role="maintainer"]`,
        "B's own maintainer row after reload",
      );
      const reloaded = lastResponseSince(probe, "GET", `/organizations/`);
      if (reloaded === null || reloaded.status !== 200) {
        fail(`B's reload did not re-read the member list successfully (observed ${reloaded?.status ?? "no response"})`);
      }
      await screenshot(pageB, "b-sees-maintainer");
    });

    await steps.step("viewer-leaves-and-history-is-kept", async () => {
      await pageV.bringToFront();
      // V leaves through the member page's own control (any member may end
      // their own affiliation; the last owner could not).
      await requireVisible(pageV, `[data-affiliation-start="${handleV}"]`, "V's leave control");
      const probe = evidence.network.length;
      await pageV.locator(`[data-affiliation-start="${handleV}"]`).click();
      await pageV.locator(`[data-affiliation-confirm="${handleV}"]`).click();
      await pageV.waitForURL((url) => url.pathname === "/organizations", { timeout: 20000 });
      const left = lastResponseSince(probe, "DELETE", "/members/");
      if (left === null || left.status !== 204) {
        fail(`leaving did not get a 204 from DELETE .../members/{id} (observed ${left?.status ?? "no response"})`);
      }
      // The organization is no longer one of V's: the list it can read is
      // answered and empty, not stale.
      await requireVisible(pageV, '[data-org-list="empty"]', "V's answered empty organization list");
      if ((await pageV.locator(`[data-org-row="${ORG_SLUG}"]`).count()) !== 0) {
        fail("the organization V just left is still listed for V");
      }
      await screenshot(pageV, "viewer-left");

      // A's record keeps V's row: ended status, the role it held, a
      // "Left <date>" departure date, and no live role control — those are
      // the four things this step measures. It does NOT compare the
      // affiliation start date (that check belongs to the role change).
      // Nothing was deleted (离职不删除历史).
      await pageA.bringToFront();
      const reloadProbe = evidence.network.length;
      await pageA.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        pageA,
        `tr[data-member-handle="${handleV}"][data-member-status="ended"]`,
        "V's historical membership row after leaving",
      );
      const endedRole = await pageA
        .locator(`tr[data-member-handle="${handleV}"]`)
        .getAttribute("data-member-role");
      if (endedRole !== "viewer") {
        fail(`V's kept row reports role ${JSON.stringify(endedRole)}; the historical role is viewer`);
      }
      const statusText = (
        (await pageA.locator(`tr[data-member-handle="${handleV}"] [data-member-status-text]`).textContent()) ?? ""
      ).trim();
      if (!/^Left \d{4}-\d{2}-\d{2}$/.test(statusText)) {
        fail(`V's kept row status reads ${JSON.stringify(statusText)}, not the departure date`);
      }
      if (
        (await pageA.locator(`tr[data-member-handle="${handleV}"] [data-role-select]`).count()) !== 0
      ) {
        fail("V's ended row still offers a role control; an ended affiliation holds no role to change");
      }
      const reloaded = lastResponseSince(reloadProbe, "GET", `/organizations/`);
      if (reloaded === null || reloaded.status !== 200) {
        fail(`A's reload did not re-read the member list successfully (observed ${reloaded?.status ?? "no response"})`);
      }
      await screenshot(pageA, "viewer-history-kept");
    });

    steps.assertComplete();
    result = "passed";
    log("all declared steps passed");
  } catch (err) {
    failure = err;
    log(`FAILED: ${errorText(err)}`);
    try {
      await screenshot(pageA, "failure-state");
    } catch (shotErr) {
      failure = new Error(`${errorText(err)}; evidence write failure: ${errorText(shotErr)}`);
    }
  } finally {
    let traceFailure;
    try {
      await contextOwner.tracing.stop({ path: tracePath });
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
      const consoleErrors = evidence.console.filter(
        (entry) => entry.kind === "error" && !isExpectedRefusal(entry),
      );
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
      } else if (consoleErrors.length > 0) {
        failure = new Error(`browser console error(s): ${consoleErrors.map((e) => e.text).join(" | ")}`);
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

/**
 * The two non-2xx answers this journey produces on purpose:
 *
 *   - GET /auth/session 401 is the signed-out probe on the pages each new
 *     context visits before its account exists;
 *   - PATCH .../organizations/{id}/members/{userId} 403 is V's refusal, which
 *     has its own step asserting both the status and the visible message.
 *
 * Anything else stays a run failure.
 */
function isExpectedRefusal(entry) {
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  if (text.includes("401")) {
    return location.includes("/api/v1/auth/session") || text.includes("/api/v1/auth/session");
  }
  if (text.includes("403")) {
    return (
      (location.includes("/api/v1/organizations/") && location.includes("/members/")) ||
      (text.includes("/api/v1/organizations/") && text.includes("/members/"))
    );
  }
  return false;
}

run().catch((err) => {
  console.error(`web-v2 journey U02: FAILED — ${errorText(err)}`);
  process.exit(1);
});
