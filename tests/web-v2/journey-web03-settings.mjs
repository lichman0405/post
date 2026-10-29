/**
 * WEB-03 (T1338, G03) real-UI journey: project Settings — adding a member by
 * handle, and who may do it.
 *
 * What this proves, with three real accounts established by the browser and
 * three real sessions in three isolated browser contexts:
 *
 *   1. A establishes an account through /login and creates a PRIVATE project
 *      through /projects' own form (WEB-04's form). Private on purpose: the
 *      project's appearance on B's Home afterwards can then only come from
 *      the membership A granted, never from the public list.
 *   2. B and C have their own accounts (created through the product's home
 *      create-account entry). They are established BEFORE the invite because
 *      V1 adds an existing account by handle — there are no pending
 *      invitations for addresses without one.
 *   3. A opens the project through the directory row and its Settings tab,
 *      which only an owner/maintainer is offered.
 *   4. An unknown handle is a FIELD error under the handle (the API answered
 *      404 USER_NOT_FOUND) and adds nobody.
 *   5. Submitting without choosing a role is a FIELD error under the role
 *      (the API answered 400 VALIDATION_FAILED for the role rule) and adds
 *      nobody.
 *   6. A invites B BY HANDLE as contributor: 201, B's row appears with that
 *      role, and the form starts empty for the next person.
 *   7. Adding B again is a FIELD error under the handle (409
 *      MEMBER_ALREADY_EXISTS) — never a silent role change: B's row is still
 *      contributor.
 *   8. B reloads Home and the private project is listed (a 200
 *      GET /api/v1/projects names it), and B opens the project page: the
 *      membership is real, not just A's screen state.
 *   9. B (a contributor) is offered no Settings tab; the Settings URL it
 *      types renders the not-authorized answer, and no invite or role control
 *      is reachable — B can neither invite nor promote itself.
 *  10. C (a non-member) cannot see the private project on Home, its project
 *      URL answers the neutral not-found (the API's existence-hiding 404),
 *      and its Settings URL offers no invite form — a non-member cannot
 *      invite.
 *
 * Anti-bypass (docs/70 §6.2): every business step above is a normal visible
 * control in a real browser. No test-side request, SQL, CLI, seeded account,
 * cookie/storage write or intercepted response participates. Accounts and the
 * project are created by the pages themselves; the invitations and refusals
 * are issued by the Settings form. Requests are OBSERVED only.
 *
 * Usage: node tests/web-v2/journey-web03-settings.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR, WEB_V2_API_ORIGIN
 */

import path from "node:path";

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey (WEB-03 settings): FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
// Its own subdirectory: each journey writes its own steps/metadata/trace, and
// one journey overwriting another's evidence would leave a green record
// pointing at somebody else's run.
const OUT_DIR = path.join(
  process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`,
  "web03-settings",
);
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";

const ID = RUN_ID.toLowerCase().replace(/[^a-z0-9-]/g, "-");
const OWNER_EMAIL = `web03-owner-${ID}@example.test`;
const CONTRIBUTOR_EMAIL = `web03-b-${ID}@example.test`;
const NONMEMBER_EMAIL = `web03-c-${ID}@example.test`;
const PASSWORD = `webv2-synthetic-${RUN_ID}-password`;

const PROJECT_NAME = "Settings Invite Lab";
const PROJECT_SLUG = `settings-invite-${ID}`.slice(0, 64);
const PROJECT_PURPOSE =
  "Exercise the Settings member invitation against the real project API.";

const STEP_NAMES = [
  "signup-owner-a",
  "create-private-project-through-the-web-form",
  "signup-contributor-b",
  "signup-nonmember-c",
  "owner-opens-project-settings",
  "invite-unknown-handle-shows-field-error",
  "invite-without-role-shows-field-error",
  "owner-invites-b-as-contributor",
  "duplicate-member-shows-field-error",
  "b-sees-project-on-home-after-invite",
  "contributor-cannot-invite-or-promote",
  "nonmember-cannot-reach-invite",
];

const steps = new JourneySteps("WEB-03-project-settings-invite", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "WEB-03-project-settings-invite",
  task: "T1338",
  role: "owner A; contributor B; non-member C (three real sessions)",
  accountEmail: OWNER_EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

/** The project's id, once the directory row has told us where it lives. */
let projectID = "";
let projectHref = "";
let handleA = "";
let handleB = "";
let handleC = "";

function log(message) {
  console.log(`web-v2 journey (WEB-03 settings): ${message}`);
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
    .some(
      (entry) => entry.event === "request" && entry.method === method && entry.url.endsWith(suffix),
    );
}

/** Any write (POST/PUT/PATCH/DELETE) observed at or after `since`. */
function observedWriteSince(since) {
  return evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "request" &&
        ["POST", "PUT", "PATCH", "DELETE"].includes(entry.method),
    );
}

/**
 * Wait until the network log holds a response for `method` + URL suffix at or
 * after `since`, or answer null once `timeout` ms have passed. The click's
 * promise resolves before the response reaches the listener, so reading the
 * log on the next line is a race; the loop orders the observation without
 * softening it (the caller still asserts the exact status).
 */
async function waitForResponse(since, method, suffix, timeout = 20000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = lastResponseSince(since, method, suffix);
    if (found !== null) return found;
    if (Date.now() >= deadline) return null;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

async function screenshot(page, label) {
  await evidence.screenshot(page, label);
}

/**
 * Establish a fresh account through the product's home create-account entry
 * (/login?mode=signup — the same /login surface). Returns the handle the API
 * assigned, as the header shows it.
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
  try {
    await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
  } catch {
    const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
    fail(`${label}: sign-up did not reach the signed-in home (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
  }
  const handleLocator = await requireVisible(
    page,
    ".global-nav-user-handle",
    `${label}: the signed-in account handle`,
  );
  const handle = ((await handleLocator.textContent()) ?? "").trim();
  if (handle === "") {
    fail(`${label}: the header reports a session but no handle`);
  }
  if (!observedRequestSince(probe, "POST", "/api/v1/auth/signup")) {
    fail(`${label}: account establishment did not go through the product's signup endpoint`);
  }
  const sessionOk = evidence.network
    .slice(probe)
    .some(
      (entry2) =>
        entry2.event === "response" &&
        entry2.method === "GET" &&
        entry2.status === 200 &&
        entry2.url.endsWith("/api/v1/auth/session"),
    );
  if (!sessionOk) {
    fail(`${label}: no 200 GET /api/v1/auth/session was observed after the sign-up`);
  }
  return handle;
}

/**
 * The refusals this journey asks for on purpose, by URL and status. Each one
 * is asserted by the step that triggers it, so allowing its console line does
 * not excuse a silent failure:
 *
 *   - 401 GET /api/v1/auth/session: the signed-out probe each new context's
 *     first page makes before it has an account;
 *   - 404 POST .../members: the unknown-handle invite;
 *   - 400 POST .../members: the invite with no role chosen (the API's role
 *     rule refuses it);
 *   - 409 POST .../members: the duplicate-member invite;
 *   - 404 GET /api/v1/projects/<id> and .../membership: C's existence-hiding
 *     read of the private project (the shell reads both in parallel).
 *
 * Any other console error fails the run.
 */
function isExpectedRefusal(entry) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  const named = (suffix) => text.includes(suffix) || location.includes(suffix);
  if (text.includes("401")) return named("/api/v1/auth/session");
  if (projectID === "") return false;
  const membersURL = `/api/v1/projects/${projectID}/members`;
  if (text.includes("404")) {
    return named(membersURL) || named(`/api/v1/projects/${projectID}`);
  }
  if (text.includes("400") || text.includes("409")) return named(membersURL);
  return false;
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

  const contextA = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  const contextB = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  const contextC = await browser.newContext({
    viewport: { width: 1280, height: 900 },
    locale: "en-US",
    timezoneId: "UTC",
  });
  observeContext(contextA);
  observeContext(contextB);
  observeContext(contextC);

  const pageA = await contextA.newPage();
  observePage(pageA);
  const pageB = await contextB.newPage();
  observePage(pageB);
  const pageC = await contextC.newPage();
  observePage(pageC);

  await contextA.tracing.start({ screenshots: true, snapshots: true, sources: false });

  let result = "failed";
  let failure;
  const settingsHref = () => `${BASE}/projects/${projectID}/settings`;
  const membersSuffix = () => `/api/v1/projects/${projectID}/members`;
  const memberRows = (page) => page.locator("[data-member-row]");

  try {
    await steps.step("signup-owner-a", async () => {
      handleA = await signUp(pageA, OWNER_EMAIL, "owner A sign-up");
      log(`owner A established through the DOM as ${handleA}`);
      await screenshot(pageA, "owner-signed-in");
    });

    await steps.step("create-private-project-through-the-web-form", async () => {
      // Entered through the signed-in home's own project entry, then the
      // directory's own create form — the path a reader is actually offered.
      const entry = await requireVisible(
        pageA,
        '[data-home-entry="project-workspaces"]',
        "the home project entry",
      );
      await entry.click();
      await pageA.waitForURL((url) => url.pathname === "/projects", { timeout: 15000 });
      // The directory has two create entries and offers exactly one: the
      // header button while it lists any project, and the empty-state entry
      // while it lists none. This journey runs after the ones that create
      // PUBLIC projects (WEB-04, T1339's overview journey), so A's directory
      // is normally non-empty — clicking whichever the page offers keeps the
      // step honest in both orders instead of assuming an empty directory.
      const start = pageA
        .locator('[data-project-create="empty-entry"], [data-project-create="open"]')
        .first();
      try {
        await start.waitFor({ state: "visible", timeout: 20000 });
      } catch {
        fail(
          `the directory offers no create entry: expected a visible [data-project-create="open"] or [data-project-create="empty-entry"] on ${pageA.url()}`,
        );
      }
      await start.click();
      await requireVisible(pageA, '[data-project-create="form"]', "the create panel");

      // Private is this journey's choice, not a default: the panel arrives
      // on Public, and the reader switches it deliberately.
      const publicRadio = pageA.locator('[data-project-visibility="public"]').first();
      if (!(await publicRadio.isChecked())) {
        fail("the create panel does not preselect Public; WEB-04's default changed");
      }
      await pageA.locator('[data-project-visibility="private"]').first().check();
      await pageA.locator('[data-project-field="name"]').fill(PROJECT_NAME);
      await pageA.locator('[data-project-field="slug"]').fill(PROJECT_SLUG);
      await pageA.locator('[data-project-field="purpose"]').fill(PROJECT_PURPOSE);

      const probe = evidence.network.length;
      await pageA.locator('[data-project-create="submit"]').click();
      await requireVisible(
        pageA,
        '[data-project-create="created"]',
        "the created-project confirmation",
      );
      const write = await waitForResponse(probe, "POST", "/api/v1/projects");
      if (write === null || write.status !== 201) {
        fail(`creating the private project did not answer 201 (observed ${write?.status ?? "no response"})`);
      }
      const row = await requireVisible(
        pageA,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "the new private project's directory row",
      );
      await requireVisible(
        pageA,
        `[data-project-row="${PROJECT_SLUG}"] [data-badge="private"]`,
        "the private visibility badge on the new project",
      );
      projectHref = (await row.getAttribute("href")) ?? "";
      if (!/^\/projects\/[^/]+$/.test(projectHref)) {
        fail(`the directory row links to ${JSON.stringify(projectHref)}, not a project page`);
      }
      projectID = projectHref.split("/").pop() ?? "";
      if (!((await row.textContent()) ?? "").includes(PROJECT_NAME)) {
        fail("the directory row does not name the project just created");
      }
      await screenshot(pageA, "private-project-created");
    });

    await steps.step("signup-contributor-b", async () => {
      handleB = await signUp(pageB, CONTRIBUTOR_EMAIL, "contributor B sign-up");
      if (handleB === handleA) {
        fail("B's account resolved to the same handle as A's");
      }
      log(`contributor B established through the DOM as ${handleB}`);
      await screenshot(pageB, "contributor-signed-in");
    });

    await steps.step("signup-nonmember-c", async () => {
      handleC = await signUp(pageC, NONMEMBER_EMAIL, "non-member C sign-up");
      if (handleC === handleA || handleC === handleB) {
        fail("C's account resolved to another account's handle");
      }
      log(`non-member C established through the DOM as ${handleC}`);
      await screenshot(pageC, "nonmember-signed-in");
    });

    await steps.step("owner-opens-project-settings", async () => {
      await pageA.bringToFront();
      const row = await requireVisible(
        pageA,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "the project's directory row",
      );
      await row.click();
      await pageA.waitForURL((url) => url.pathname === projectHref, { timeout: 20000 });
      const probe = evidence.network.length;
      await requireVisible(
        pageA,
        `[data-project-shell="${PROJECT_SLUG}"]`,
        "the project shell",
      );
      // The owner is offered the Settings tab; the page behind it is the
      // invite surface.
      const tab = await requireVisible(
        pageA,
        '[data-project-tab="settings"]',
        "the Settings tab for the project owner",
      );
      await tab.click();
      await pageA.waitForURL((url) => url.pathname === `/projects/${projectID}/settings`, {
        timeout: 15000,
      });
      const inviteSection = await requireVisible(
        pageA,
        "[data-invite-section]",
        "the add-member section on Settings",
      );
      if (((await inviteSection.textContent()) ?? "").trim() === "") {
        fail("the add-member section is visible but says nothing");
      }
      // The member list the invite writes into is the API's list, and A is
      // its only member so far.
      await requireVisible(pageA, "[data-member-row]", "the member list");
      const ownerSelect = pageA.locator("[data-role-select]");
      if ((await ownerSelect.count()) !== 0) {
        fail("the owner's own role is editable; the page must not offer a self role change");
      }
      if ((await memberRows(pageA).count()) !== 1) {
        fail(`the member list shows ${await memberRows(pageA).count()} rows before any invite; the owner alone was expected`);
      }
      const listRead = lastResponseSince(probe, "GET", membersSuffix());
      if (listRead === null || listRead.status !== 200) {
        fail(`the Settings tab did not read the member list with a 200 (observed ${listRead?.status ?? "no response"})`);
      }
      await screenshot(pageA, "owner-opens-settings");
    });

    await steps.step("invite-unknown-handle-shows-field-error", async () => {
      const before = await memberRows(pageA).count();
      await pageA.locator("[data-invite-handle]").fill(`nobody-${ID}`);
      await pageA.locator("[data-invite-role]").selectOption("contributor");

      const probe = evidence.network.length;
      await pageA.locator("[data-invite-submit]").click();

      // The field error can only render after the API's refusal, which also
      // settles the network read below.
      const fieldError = await requireVisible(
        pageA,
        '[data-invite-field-error="handle"]',
        "the unknown-handle error under the handle field",
      );
      const text = ((await fieldError.textContent()) ?? "").trim();
      if (!/no account uses that handle/i.test(text)) {
        fail(`the unknown-handle error said ${JSON.stringify(text)}, which does not explain the handle`);
      }
      const invalid = await pageA.locator('[data-invite-handle][aria-invalid="true"]').count();
      if (invalid !== 1) {
        fail(`the handle input is not marked invalid after an unknown handle (aria-invalid count ${invalid})`);
      }
      await requireGone(
        pageA,
        '[data-invite-field-error="role"]',
        "an unknown handle must not report the role field",
      );
      if (!observedRequestSince(probe, "POST", membersSuffix())) {
        fail("the unknown-handle invite never reached POST .../members; the refusal must be the API's");
      }
      const write = await waitForResponse(probe, "POST", membersSuffix());
      if (write === null || write.status !== 404) {
        fail(`the unknown-handle invite did not answer 404 (observed ${write?.status ?? "no response"})`);
      }
      // Nobody was added.
      if ((await memberRows(pageA).count()) !== before) {
        fail("a refused invite changed the member list");
      }
      await screenshot(pageA, "unknown-handle-field-error");
    });

    await steps.step("invite-without-role-shows-field-error", async () => {
      const before = await memberRows(pageA).count();
      // The role control starts at its placeholder after the refusal above
      // is cleared by the next submit; choose nothing on purpose.
      await pageA.locator("[data-invite-handle]").fill(handleB);
      await pageA.locator("[data-invite-role]").selectOption("");

      const probe = evidence.network.length;
      await pageA.locator("[data-invite-submit]").click();

      const fieldError = await requireVisible(
        pageA,
        '[data-invite-field-error="role"]',
        "the invalid-role error under the role field",
      );
      const text = ((await fieldError.textContent()) ?? "").trim();
      if (!/not a valid role/i.test(text)) {
        fail(`the invalid-role error said ${JSON.stringify(text)}, which does not explain the role`);
      }
      const invalid = await pageA.locator('[data-invite-role][aria-invalid="true"]').count();
      if (invalid !== 1) {
        fail(`the role select is not marked invalid after an invalid role (aria-invalid count ${invalid})`);
      }
      await requireGone(
        pageA,
        '[data-invite-field-error="handle"]',
        "an invalid role must not report the handle field",
      );
      // The refusal is the API's own role rule: the request went out and the
      // answer was 400 VALIDATION_FAILED.
      if (!observedRequestSince(probe, "POST", membersSuffix())) {
        fail("the invalid-role invite never reached POST .../members; the refusal must be the API's");
      }
      const write = await waitForResponse(probe, "POST", membersSuffix());
      if (write === null || write.status !== 400) {
        fail(`the invalid-role invite did not answer 400 (observed ${write?.status ?? "no response"})`);
      }
      if ((await memberRows(pageA).count()) !== before) {
        fail("a refused invite changed the member list");
      }
      await screenshot(pageA, "invalid-role-field-error");
    });

    await steps.step("owner-invites-b-as-contributor", async () => {
      await pageA.locator("[data-invite-handle]").fill(handleB);
      await pageA.locator("[data-invite-role]").selectOption("contributor");

      const probe = evidence.network.length;
      await pageA.locator("[data-invite-submit]").click();

      // The success notice and B's row both render only after the 201 and
      // the member-list re-read, so awaiting them also guarantees the
      // response this step asserts is already in the network record.
      const notice = await requireVisible(
        pageA,
        '[data-invite-notice="success"]',
        "the invite success notice",
      );
      const noticeText = ((await notice.textContent()) ?? "").trim();
      if (!noticeText.includes(`@${handleB}`)) {
        fail(`the success notice said ${JSON.stringify(noticeText)}, which does not name the added account`);
      }
      const row = pageA.locator("[data-member-row]", { hasText: `@${handleB}` });
      try {
        await row.waitFor({ state: "visible", timeout: 20000 });
      } catch {
        fail(`B's member row did not appear after the invite (member rows: ${await memberRows(pageA).count()})`);
      }
      const roleValue = await row.locator("[data-role-select]").inputValue();
      if (roleValue !== "contributor") {
        fail(`B's role control shows ${JSON.stringify(roleValue)} after the invite; contributor was chosen`);
      }
      const write = await waitForResponse(probe, "POST", membersSuffix());
      if (write === null || write.status !== 201) {
        fail(`the invite did not answer 201 (observed ${write?.status ?? "no response"})`);
      }
      // The form starts empty for the next person: the role is a deliberate
      // choice per member, never the previous invite's role repeated.
      if ((await pageA.locator("[data-invite-handle]").inputValue()) !== "") {
        fail("the invite handle field kept its value after a successful add");
      }
      if ((await pageA.locator("[data-invite-role]").inputValue()) !== "") {
        fail("the invite role did not return to its placeholder after a successful add");
      }
      if ((await memberRows(pageA).count()) !== 2) {
        fail(`the member list shows ${await memberRows(pageA).count()} rows after inviting B; two were expected`);
      }
      await screenshot(pageA, "b-invited-as-contributor");
    });

    await steps.step("duplicate-member-shows-field-error", async () => {
      await pageA.locator("[data-invite-handle]").fill(handleB);
      await pageA.locator("[data-invite-role]").selectOption("viewer");

      const probe = evidence.network.length;
      await pageA.locator("[data-invite-submit]").click();

      const fieldError = await requireVisible(
        pageA,
        '[data-invite-field-error="handle"]',
        "the duplicate-member error under the handle field",
      );
      const text = ((await fieldError.textContent()) ?? "").trim();
      if (!/already a member/i.test(text)) {
        fail(`the duplicate-member error said ${JSON.stringify(text)}, which does not explain the duplicate`);
      }
      const write = await waitForResponse(probe, "POST", membersSuffix());
      if (write === null || write.status !== 409) {
        fail(`the duplicate invite did not answer 409 (observed ${write?.status ?? "no response"})`);
      }
      // "Add" and "change role" are different actions: the duplicate was
      // refused, not silently applied.
      const row = pageA.locator("[data-member-row]", { hasText: `@${handleB}` });
      const roleValue = await row.locator("[data-role-select]").inputValue();
      if (roleValue !== "contributor") {
        fail(`B's role changed to ${JSON.stringify(roleValue)} on a refused duplicate; it must stay contributor`);
      }
      if ((await memberRows(pageA).count()) !== 2) {
        fail("a refused duplicate changed the member list");
      }
      await screenshot(pageA, "duplicate-member-field-error");
    });

    await steps.step("b-sees-project-on-home-after-invite", async () => {
      await pageB.bringToFront();
      const probe = evidence.network.length;
      const response = await pageB.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`B's home page did not load (HTTP ${response?.status() ?? "no response"})`);
      }
      await requireVisible(pageB, ".global-nav-user-handle", "B's signed-in account handle");
      // The private project is on B's Home only because the invite granted a
      // membership — the public list never contains a private project.
      const row = await requireVisible(
        pageB,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "the invited project on B's Home",
      );
      const href = (await row.getAttribute("href")) ?? "";
      if (href !== projectHref) {
        fail(`B's Home links the project to ${JSON.stringify(href)}, not ${JSON.stringify(projectHref)}`);
      }
      const listRead = lastResponseSince(probe, "GET", "/api/v1/projects");
      if (listRead === null || listRead.status !== 200) {
        fail(`B's Home did not read the project list with a 200 (observed ${listRead?.status ?? "no response"})`);
      }
      // Open it: the project page is the membership's read authorization,
      // not just a row in a list.
      const projectProbe = evidence.network.length;
      await row.click();
      await pageB.waitForURL((url) => url.pathname === `/projects/${projectID}`, {
        timeout: 20000,
      });
      const shell = await requireVisible(
        pageB,
        `[data-project-shell="${PROJECT_SLUG}"]`,
        "the project shell for the invited member",
      );
      if (!((await shell.textContent()) ?? "").includes(PROJECT_NAME)) {
        fail("the project shell for B does not name the project");
      }
      const read = lastResponseSince(projectProbe, "GET", `/api/v1/projects/${projectID}`);
      if (read === null || read.status !== 200) {
        fail(`B's project read did not answer 200 (observed ${read?.status ?? "no response"})`);
      }
      await screenshot(pageB, "b-sees-project-in-home-and-open-it");
    });

    await steps.step("contributor-cannot-invite-or-promote", async () => {
      // The tab bar carries only the tabs B's role permits: no Settings.
      await requireGone(
        pageB,
        '[data-project-tab="settings"]',
        "a contributor must not be offered the Settings tab",
      );
      // A typed deep link is the reader's own attempt: the page must answer
      // with the not-authorized state and offer no control at all.
      const probe = evidence.network.length;
      await pageB.goto(settingsHref(), { waitUntil: "domcontentloaded" });
      await requireVisible(
        pageB,
        "[data-settings-unauthorized]",
        "the not-authorized answer for a contributor at the Settings URL",
      );
      await requireGone(pageB, "[data-invite-section]", "a contributor must not reach the invite form");
      await requireGone(pageB, "[data-invite-handle]", "a contributor must not reach the invite handle field");
      await requireGone(pageB, "[data-invite-submit]", "a contributor must not reach an invite submit");
      await requireGone(pageB, "[data-role-select]", "a contributor must not reach a role control");
      const writes = observedWriteSince(probe);
      if (writes.length > 0) {
        fail(`a contributor's Settings visit issued ${writes.map((w) => `${w.method} ${w.url}`).join(", ")}; it must only read`);
      }
      await screenshot(pageB, "contributor-settings-unauthorized");
    });

    await steps.step("nonmember-cannot-reach-invite", async () => {
      await pageC.bringToFront();
      // C is signed in but belongs to no project. Home lists nothing for it.
      await pageC.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      await requireVisible(pageC, ".global-nav-user-handle", "C's signed-in account handle");
      await requireGone(
        pageC,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "a non-member's Home must not list the private project",
      );

      // The project URL answers the neutral not-found: the API hid the
      // project's existence (404), not merely its content (403).
      const projectProbe = evidence.network.length;
      const response = await pageC.goto(`${BASE}${projectHref}`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`the non-member's visit to the project URL did not load the product shell (HTTP ${response?.status() ?? "no response"})`);
      }
      await requireVisible(pageC, "[data-project-notfound]", "the neutral not-found state for a non-member");
      await requireGone(pageC, "[data-project-shell]", "a non-member must not be given the project shell");
      const read = lastResponseSince(projectProbe, "GET", `/api/v1/projects/${projectID}`);
      if (read === null || read.status !== 404) {
        fail(`the non-member's project read answered ${read?.status ?? "no response"}, not the existence-hiding 404`);
      }
      const projectText = (await pageC.locator("body").textContent()) ?? "";
      if (projectText.includes(PROJECT_NAME)) {
        fail(`the non-member's page contains the private project's name ${JSON.stringify(PROJECT_NAME)}`);
      }

      // The Settings URL offers no invite form either.
      const settingsProbe = evidence.network.length;
      await pageC.goto(settingsHref(), { waitUntil: "domcontentloaded" });
      await requireVisible(pageC, "[data-project-notfound]", "the not-found state at the Settings URL");
      await requireGone(pageC, "[data-invite-section]", "a non-member must not reach the invite form");
      await requireGone(pageC, "[data-invite-handle]", "a non-member must not reach an invite handle field");
      await requireGone(pageC, "[data-invite-submit]", "a non-member must not reach an invite submit");
      const settingsRead = lastResponseSince(settingsProbe, "GET", `/api/v1/projects/${projectID}`);
      if (settingsRead === null || settingsRead.status !== 404) {
        fail(`the non-member's Settings read answered ${settingsRead?.status ?? "no response"}, not 404`);
      }
      const writes = observedWriteSince(settingsProbe);
      if (writes.length > 0) {
        fail(`a non-member's Settings visit issued ${writes.map((w) => `${w.method} ${w.url}`).join(", ")}; it must only read`);
      }
      await screenshot(pageC, "nonmember-project-hidden");
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
      await contextA.tracing.stop({ path: tracePath });
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
  console.error(`web-v2 journey (WEB-03 settings): FAILED — ${errorText(err)}`);
  process.exit(1);
});
