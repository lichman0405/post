/**
 * WEB-06a journey (T1307): branches and structured authoring — the draft
 * half of U03 (docs/70 G06, U04; D15).
 *
 * What this proves, and why each assertion is the one it is:
 *
 *   1. "A branch is created and switched, and a minimal draft is saved."
 *      A fresh account creates its project through the visible form, then
 *      creates TWO research branches BEFORE any write (a fresh project has
 *      no `main`, so the branch form is genuinely the first step). Both are
 *      forked from the same state on purpose: the branch-switch assertions
 *      later can then tell isolation from timing.
 *
 *   2. "A minimal draft is saved, left, and continued after a reload and a
 *      sign-in." The question draft carries only its statement — the form's
 *      save is enabled with that one field (D15). The journey signs OUT
 *      through the header's account menu, signs back in through /login, and
 *      returns to the branch URL it saved before leaving — the address bar
 *      is the reader's own bookmark, so this is "come back to the page",
 *      not a hidden state. WHILE SIGNED OUT it also visits those same pages
 *      anonymously: the Public project still reads, and it carries no
 *      authoring form, no save control, no editor and no relation form — the
 *      shell's `role: null` must gate the affordances, not be read as "not a
 *      viewer, therefore a writer". The draft is then continued by a NEW
 *      immutable version whose fields are the v1 payload plus what was
 *      typed, and the v1 row is still in the version log afterwards.
 *
 *   3. "The four slice-a types each have their own form, and a failed result
 *      stays a failed result." Question, Hypothesis, Experiment and
 *      Calculation are authored through their own field sets on the SAME
 *      branch. The experiment records a failure: the outcome control says
 *      `failed`, the stored payload keeps it under `metadata.result_outcome`
 *      (the schemas' free-form `metadata` object; the key itself is this
 *      slice's documented convention, not a schema field), and the object's
 *      header and inventory row both read `failed` after a reload — a failure
 *      is never re-rendered as success. The other eight canonical types are
 *      absent from the kind switch.
 *
 *   4. "The required fields are checked before review, and nothing claims a
 *      submission." The check panel runs the server's own pr gate and lists
 *      the gaps the server names (a minimal hypothesis is missing the
 *      schema-required question link), together with its warnings. The panel
 *      says, in words, that nothing was submitted — this slice has no submit
 *      action, and the journey asserts the note is on screen rather than
 *      taking the absence of a button on faith. On the second branch, whose
 *      only object is complete, the same check reports no blocking gap: the
 *      verdict is the server's, not a hardcoded state.
 *
 *   5. "Versions and relations survive a reload and a branch switch." A
 *      typed relation (addresses_question) and the hypothesis's second
 *      version are added by clicking, then read back after a reload. A
 *      second version of the EXPERIMENT and of the CALCULATION follows, each
 *      changing only its result: every other field of the payload (objective,
 *      operators, conditions, method, software, parameters, and the recorded
 *      outcome) must still be there, so a patch cannot silently revert what
 *      the form did not show. An old version is read-only — the editor is
 *      absent on version 1 of an object at version 2 and the page links to
 *      the head instead — and a second relation TYPE (tests_hypothesis, from
 *      the experiment to the hypothesis) is added and read back. The
 *      calculation's software is then CLEARED and saved: because the patch
 *      is a shallow merge, a clear that is merely omitted keeps the old
 *      value, so the journey reloads and asserts the field really is empty
 *      (and that the fields it did not clear survived). The branch switch
 *      then shows the other branch holding only its own object, and switching
 *      back shows the four objects at their version logs (the calculation at
 *      v3 after the clear) and both relations still there.
 *
 * Anti-bypass (docs/70 §6.2, scripts/tests/web-v2-bypass-scan.mjs): no
 * test-side request, SQL, CLI, cookie/storage injection or in-page script in
 * any step. Every object, version and relation in this run is created by a
 * click or a keystroke in the product's own forms, and every assertion reads
 * the rendered DOM.
 *
 * Usage: node tests/web-v2/journey-web06.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR,
 *        WEB_V2_API_ORIGIN, WEB_V2_ACCOUNT_EMAIL, WEB_V2_ACCOUNT_PASSWORD
 */

import path from "node:path";

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey (WEB-06a): FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
// Its own subdirectory, like WEB-03/04/05: five journeys write five step
// records, five traces and five screenshot series.
const OUT_DIR = path.join(
  process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`,
  "web06",
);
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";
const EMAIL = process.env.WEB_V2_ACCOUNT_EMAIL ?? `webv2-web06-${RUN_ID.toLowerCase()}@example.test`;
const PASSWORD = process.env.WEB_V2_ACCOUNT_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;

// The project and every value this run authors. Fixed strings, not
// generated: every later assertion locates an object by the words the
// reader typed, exactly as a reader would.
const PROJECT_NAME = "Isotherm Authoring Lab";
const PROJECT_SLUG = "isotherm-authoring-lab";
const PROJECT_PURPOSE =
  "Author the question, hypothesis, experiment and calculation of a CO2 isotherm study on one research branch.";

const BRANCH_A = "isotherm-a";
const BRANCH_A_PURPOSE = "The path that carries the incomplete drafts.";
const BRANCH_B = "isotherm-b";
const BRANCH_B_PURPOSE = "A second path, created before any write, for the completeness check.";

const QUESTION_STATEMENT = "Does the material adsorb CO2 above 1 bar at 298 K?";
const QUESTION_PURPOSE = "Compare adsorption across the two candidate samples.";
const QUESTION_STATE = "open";

const QUESTION_ON_B = "Which sample reaches saturation first at 298 K?";

const HYPOTHESIS_STATEMENT = "The high-surface-area sample saturates first.";
const HYPOTHESIS_TYPE = "mechanistic";

const EXPERIMENT_OBJECTIVE = "Measure the CO2 isotherm at 298 K between 1 and 40 bar";
const EXPERIMENT_OPERATORS = "operator-alpha, operator-beta";
const EXPERIMENT_RESULT =
  "The run failed: the microbalance lost calibration before the isotherm completed.";
const EXPERIMENT_RESULT_V2 =
  "The run failed again after recalibration: the isotherm stopped at 12 bar, so no uptake curve was recorded.";
const EXPERIMENT_CONDITIONS = '{"temperature_k": 298, "pressure_range_bar": "1-40"}';

const CALCULATION_OBJECTIVE = "Fit the Langmuir model to the measured isotherm";
const CALCULATION_METHOD = "Least-squares fit of the Langmuir isotherm to the measured uptake points";
const CALCULATION_SOFTWARE = "python-scipy 1.14";
const CALCULATION_PARAMETERS = '{"qmax": 7.2, "k": 0.4}';
const CALCULATION_RESULT = "Inconclusive: the failed experiment left too few points to fit.";
const CALCULATION_RESULT_V2 =
  "Inconclusive: the refit on the repaired run still has fewer than five points.";

const STEP_NAMES = [
  "sign-up-and-create-the-project",
  "create-two-research-branches-before-any-write",
  "save-a-minimal-question-draft",
  "leave-sign-in-again-and-continue-the-draft",
  "the-four-slice-a-forms-save-their-own-objects",
  "the-review-check-names-the-gaps-and-submits-nothing",
  "versions-and-relations-survive-a-reload",
  "a-second-experiment-and-calculation-version-keeps-every-field",
  "clearing-a-nullable-field-is-saved-and-reads-back-empty",
  "branch-switch-isolates-the-branches-and-keeps-history",
];

const steps = new JourneySteps("WEB-06a-branch-authoring", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "WEB-06a-branch-authoring",
  task: "T1307",
  role: "a newly registered account, owner of the project it creates",
  accountEmail: EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

function log(message) {
  console.log(`web-v2 journey (WEB-06a): ${message}`);
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

/** Wait until `selector` has an option whose label is `label`, then select it. */
async function selectByLabel(page, selector, label, what) {
  const select = await requireVisible(page, selector, what);
  const deadline = Date.now() + 20000;
  for (;;) {
    const option = select.locator("option", { hasText: label }).first();
    if ((await option.count()) > 0) break;
    if (Date.now() >= deadline) {
      const labels = await select.locator("option").allTextContents();
      fail(`${what} offers no option labelled ${JSON.stringify(label)} (options: ${JSON.stringify(labels)})`);
    }
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
  await select.selectOption({ label });
}

/** The exact text of one element, trimmed. */
async function textOf(locator) {
  return ((await locator.textContent()) ?? "").trim();
}

function responsesSince(since, method, suffix) {
  return evidence.network
    .slice(since)
    .filter(
      (entry) =>
        entry.event === "response" && entry.method === method && entry.url.endsWith(suffix),
    );
}

/**
 * Wait until the network log holds a response for `method` + URL suffix at or
 * after `since`, or answer null once `timeout` ms have passed. Same helper,
 * same reason as WEB-05: the rendered result settles before the response
 * listener sees the response, so reading the log on the next line is a race.
 */
async function waitForResponse(since, method, suffix, timeout = 20000) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const found = responsesSince(since, method, suffix);
    if (found.length > 0) return found[found.length - 1];
    if (Date.now() >= deadline) return null;
    await new Promise((resolve) => setTimeout(resolve, 50));
  }
}

/**
 * The responses this journey asks for by URL and status, so letting their
 * console lines through does not excuse a silent failure:
 *
 *   - 401 GET /api/v1/auth/session: the signed-out probe the nav makes on
 *     every anonymous page (the home page before sign-up, and again after
 *     the deliberate sign-out).
 *   - 401 GET /api/v1/projects/<this run's project>/membership: the shell's
 *     "what am I in this project" probe, asked while the journey is
 *     deliberately signed out. 401 is the designed answer for an anonymous
 *     caller, and it is what makes the reader's role null — the very state
 *     the anonymous read-only check asserts. Scoped to THIS project's id so
 *     no other 401 can hide behind it.
 *
 * Any other console error fails the run.
 */
function isExpectedRefusal(entry, projectId) {
  if (entry.kind !== "error") return false;
  const text = entry.text ?? "";
  const location = entry.location ?? "";
  if (!text.includes("401")) return false;
  const sessionProbe = (value) => value.includes("/api/v1/auth/session");
  if (sessionProbe(text) || sessionProbe(location)) return true;
  if (projectId === "") return false;
  const membership = `/api/v1/projects/${projectId}/membership`;
  return text.includes(membership) || location.includes(membership);
}

async function screenshot(page, label) {
  await evidence.screenshot(page, label);
}

/** The object id of the page the browser is on (the web's own route). */
function objectIdOf(url) {
  const match = /\/research\/objects\/([^/?#]+)/.exec(new URL(url).pathname);
  return match === null ? "" : decodeURIComponent(match[1]);
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

  let projectId = "";
  let branchAId = "";
  let branchBId = "";
  let branchAUrl = "";
  let questionId = "";

  /** Open the project's Research tab from the project shell. */
  async function openResearchTab() {
    const tab = await requireVisible(page, '[data-project-tab="research"]', "the project's Research tab");
    await tab.click();
    await page.waitForURL((url) => url.pathname.endsWith("/research"), { timeout: 20000 });
    await requireVisible(page, "[data-research-authoring]", "the research authoring panel");
  }

  /** Create one research branch through the panel's own form. */
  async function createBranch(name, purpose) {
    const toggle = await requireVisible(page, "[data-authoring-new-branch-toggle]", "the new-branch control");
    await toggle.click();
    const form = await requireVisible(page, "[data-authoring-new-branch]", "the new-branch form");
    await page.locator("[data-authoring-branch-name]").fill(name);
    await page.locator("[data-authoring-branch-purpose]").fill(purpose);
    // The branch BEFORE this click: the wait must observe a CHANGE, not the
    // branch the page was already showing (the second creation happens while
    // a branch is selected, and "some branch is in the URL" would be true
    // before the POST answered).
    const previousBranchId = new URL(page.url()).searchParams.get("branch") ?? "";
    const probe = evidence.network.length;
    await form.locator("[data-authoring-branch-submit]").click();
    await page.waitForURL(
      (url) => {
        const selected = url.searchParams.get("branch") ?? "";
        return selected !== "" && selected !== previousBranchId;
      },
      { timeout: 20000 },
    );
    const created = await waitForResponse(probe, "POST", "/branches");
    if (created === null || created.status !== 201) {
      fail(`creating the branch ${name} did not answer 201 (observed ${created?.status ?? "no response"})`);
    }
    const branchId = new URL(page.url()).searchParams.get("branch") ?? "";
    if (branchId === "") fail(`the URL after creating ${name} names no branch: ${page.url()}`);
    await requireVisible(page, "[data-authoring-branch-select]", "the branch selector after creating a branch");
    await screenshot(page, `branch-created-${name}`);
    return branchId;
  }

  /** Author one object through the draft form and return its id. */
  async function saveDraft(kind, fields, expectedVersion = 1) {
    const probe = evidence.network.length;
    const saveButton = await requireVisible(page, `[data-authoring-save="${kind}"]`, `the ${kind} save control`);
    if (await saveButton.isDisabled()) {
      fail(`the ${kind} save control is disabled with an identity field typed`);
    }
    await saveButton.click();
    const created = await waitForResponse(probe, "POST", "/objects");
    if (created === null || created.status !== 201) {
      fail(`saving the ${kind} draft did not answer 201 (observed ${created?.status ?? "no response"})`);
    }
    const saved = await requireVisible(page, `[data-authoring-saved]`, `the ${kind} saved confirmation`);
    const savedText = await textOf(saved);
    if (!savedText.includes(`version ${expectedVersion}`)) {
      fail(`the ${kind} saved confirmation reads ${JSON.stringify(savedText)}, which does not name version ${expectedVersion}`);
    }
    const objectId = (await saved.getAttribute("data-authoring-saved")) ?? "";
    if (objectId === "") fail(`the ${kind} saved confirmation names no object`);
    const row = await requireVisible(
      page,
      `[data-authoring-object="${objectId}"][data-object-type="${kind}"]`,
      `the ${kind} inventory row`,
    );
    const version = await row.locator("[data-authoring-object-version]").getAttribute("data-authoring-object-version");
    if (version !== String(expectedVersion)) {
      fail(`the ${kind} inventory row reports version ${JSON.stringify(version)}; ${expectedVersion} was saved`);
    }
    const rowText = await textOf(row);
    const identity = fields[Object.keys(fields)[0]];
    if (identity !== undefined && !rowText.includes(identity)) {
      fail(`the ${kind} inventory row reads ${JSON.stringify(rowText)}, which does not name ${JSON.stringify(identity)}`);
    }
    return objectId;
  }

  /** Fill one authoring field by its schema name. */
  async function fillAuthoringField(name, value) {
    const input = await requireVisible(page, `[data-authoring-input="${name}"]`, `the authoring input ${name}`);
    await input.fill(value);
  }

  async function selectAuthoringField(name, label) {
    await selectByLabel(page, `[data-authoring-input="${name}"]`, label, `the authoring select ${name}`);
  }

  /** The inventory rows as `{id, type, version, outcome}`. */
  async function inventory() {
    const rows = page.locator("[data-authoring-objects] li[data-authoring-object]");
    const count = await rows.count();
    const out = [];
    for (let index = 0; index < count; index += 1) {
      const row = rows.nth(index);
      out.push({
        id: (await row.getAttribute("data-authoring-object")) ?? "",
        type: (await row.getAttribute("data-object-type")) ?? "",
        version: (await row.locator("[data-authoring-object-version]").getAttribute("data-authoring-object-version")) ?? "",
        outcome:
          (await row.locator("[data-authoring-object-outcome]").getAttribute("data-authoring-object-outcome").catch(() => null)) ??
          "",
      });
    }
    return out;
  }

  /**
   * The inventory once it has settled on `expectCount` rows. The panel (and
   * the select that carries the branch) renders before the branch slice
   * arrives, so reading the rows the moment the panel is visible can see the
   * still-loading zero — a race that passes or fails on timing alone.
   */
  async function inventoryWhen(expectCount, what) {
    const deadline = Date.now() + 20000;
    for (;;) {
      const rows = await inventory();
      if (rows.length === expectCount) return rows;
      if (Date.now() >= deadline) {
        fail(`${what}: the inventory settled at ${rows.length} object(s) (${JSON.stringify(rows)}); ${expectCount} were expected`);
      }
      await new Promise((resolve) => setTimeout(resolve, 50));
    }
  }

  async function openInventoryObject(objectType, what) {
    const row = await requireVisible(
      page,
      `[data-authoring-objects] li[data-object-type="${objectType}"]`,
      what,
    );
    const id = (await row.getAttribute("data-authoring-object")) ?? "";
    if (id === "") fail(`${what} names no object`);
    await row.locator("[data-authoring-object-open]").click();
    await page.waitForURL((url) => url.pathname.includes(`/research/objects/${id}`), { timeout: 20000 });
    await requireVisible(page, "[data-research-object]", `the ${objectType} object page`);
    return id;
  }

  async function backToResearch() {
    const link = await requireVisible(page, '[data-back-to="research"]', "the breadcrumb back to the research outline");
    await link.click();
    await page.waitForURL((url) => url.pathname.endsWith("/research"), { timeout: 20000 });
    await requireVisible(page, "[data-research-authoring]", "the research authoring panel after returning");
  }

  /** Run the review check and return the observed verdict attribute. */
  async function runReviewCheck() {
    const panel = await requireVisible(page, "[data-gate-check-panel]", "the review-check panel");
    const probe = evidence.network.length;
    await panel.locator("[data-gate-check]").click();
    const report = await requireVisible(page, "[data-gate-report]", "the review-check report");
    const call = await waitForResponse(probe, "POST", ":validate");
    if (call === null || call.status !== 200) {
      fail(`the review check did not answer 200 (observed ${call?.status ?? "no response"})`);
    }
    return (await report.getAttribute("data-gate-verdict")) ?? "";
  }

  try {
    await steps.step("sign-up-and-create-the-project", async () => {
      const response = await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`home page did not load: HTTP ${response?.status() ?? "no response"} at ${BASE}/`);
      }
      const entry = await requireVisible(page, '[data-home-entry="sign-in"]', "the home sign-in entry");
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 20000 });
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

      // The project, through the directory's own entry (WEB-04's route).
      const projects = await requireVisible(page, '[data-home-entry="project-workspaces"]', "the home project entry");
      await projects.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 20000 });
      // Either entry creates a project: a fresh account's empty directory
      // shows the empty-state entry, and a directory that already lists a
      // public project another journey created shows the header's create
      // control. This journey must not depend on which ran first.
      const start = page.locator('[data-project-create="empty-entry"], [data-project-create="open"]').first();
      try {
        await start.waitFor({ state: "visible", timeout: 20000 });
      } catch {
        fail(`no entry into creating a project is visible on ${page.url()}`);
      }
      await start.click();
      const form = await requireVisible(page, '[data-project-create="form"]', "the create panel");
      await page.locator('[data-project-field="name"]').fill(PROJECT_NAME);
      await page.locator('[data-project-field="slug"]').fill(PROJECT_SLUG);
      await page.locator('[data-project-field="purpose"]').fill(PROJECT_PURPOSE);
      const createProbe = evidence.network.length;
      await form.locator('[data-project-create="submit"]').click();
      await requireVisible(page, '[data-project-create="created"]', "the created confirmation");
      const created = await waitForResponse(createProbe, "POST", "/api/v1/projects");
      if (created === null || created.status !== 201) {
        fail(`creating the project did not answer 201 (observed ${created?.status ?? "no response"})`);
      }
      const row = await requireVisible(
        page,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "the new project's directory row",
      );
      await row.click();
      await page.waitForURL((url) => /^\/projects\/[^/]+$/.test(url.pathname), { timeout: 20000 });
      await requireVisible(page, "[data-project-shell]", "the project shell");
      projectId = new URL(page.url()).pathname.split("/")[2] ?? "";
      if (projectId === "") fail(`the project page URL ${page.url()} names no project`);
      await screenshot(page, "project-shell");
    });

    await steps.step("create-two-research-branches-before-any-write", async () => {
      await openResearchTab();
      // A fresh project has no branch at all: the selector offers only the
      // "no branch yet" entry, and the branch form is the way forward.
      const select = await requireVisible(page, "[data-authoring-branch-select]", "the branch selector");
      // Wait for the LOADED empty state, not the loading placeholder: both
      // render one disabled option, so "one option" alone would pass while
      // the branch list is still in flight and read "Loading…" as the state.
      await page.waitForFunction(
        () => {
          const node = document.querySelector("[data-authoring-branch-select]");
          if (!(node instanceof HTMLSelectElement) || node.options.length !== 1 || !node.disabled) return false;
          return /no branch/i.test(node.options[0]?.textContent ?? "");
        },
        undefined,
        { timeout: 20000 },
      );
      const options = await select.locator("option").allTextContents();
      if (options.length !== 1) {
        fail(`a new project's branch selector offers ${JSON.stringify(options)}; exactly the "no branch" entry was expected`);
      }
      if (!/no branch/i.test(options[0])) {
        fail(`the branch selector reads ${JSON.stringify(options[0])}; a "no branch yet" state was expected`);
      }
      await screenshot(page, "research-no-branch-yet");

      branchAId = await createBranch(BRANCH_A, BRANCH_A_PURPOSE);
      branchBId = await createBranch(BRANCH_B, BRANCH_B_PURPOSE);
      if (branchAId === branchBId) {
        fail(`both branches were created with the same id ${branchAId}`);
      }
      const labels = await select.locator("option").allTextContents();
      if (!labels.includes(BRANCH_A) || !labels.includes(BRANCH_B)) {
        fail(`the branch selector lists ${JSON.stringify(labels)}; both created branches were expected`);
      }
      // Switch back to the first branch by choosing it, not by typing a URL.
      await selectByLabel(page, "[data-authoring-branch-select]", BRANCH_A, "the branch selector");
      await page.waitForURL((url) => url.searchParams.get("branch") === branchAId, { timeout: 20000 });
      await requireVisible(page, "[data-authoring-inventory-empty]", "the empty inventory of the new branch");
      await requireGone(page, "[data-authoring-object]", "a branch created before any write must hold no object");
      branchAUrl = page.url();
      await screenshot(page, "branch-a-selected");
    });

    await steps.step("save-a-minimal-question-draft", async () => {
      // The question form is the default kind, and its save needs only the
      // statement (D15). Everything else is left blank on purpose.
      const questionTab = await requireVisible(page, '[data-authoring-kind="research_question"]', "the question kind");
      const pressed = await questionTab.getAttribute("aria-pressed");
      if (pressed !== "true") fail(`the question kind is not the selected form (aria-pressed=${JSON.stringify(pressed)})`);
      await requireVisible(page, 'form[data-authoring-form="research_question"]', "the question draft form");
      for (const name of ["statement", "purpose", "question_state", "scope"]) {
        await requireVisible(page, `[data-authoring-input="${name}"]`, `the question field ${name}`);
      }
      await fillAuthoringField("statement", QUESTION_STATEMENT);
      questionId = await saveDraft("research_question", { statement: QUESTION_STATEMENT }, 1);

      const savedLink = await requireVisible(page, "[data-authoring-open-draft]", "the link to continue the draft");
      const savedHref = (await savedLink.getAttribute("href")) ?? "";
      if (!savedHref.includes("/research/objects/")) {
        fail(`the continue-the-draft link reads ${JSON.stringify(savedHref)}, not a research object page`);
      }
      if (!savedHref.includes(`branch=${branchAId}`)) {
        fail(`the continue-the-draft link ${JSON.stringify(savedHref)} does not carry the branch it was saved on`);
      }
      const rows = await inventory();
      if (rows.length !== 1 || rows[0].type !== "research_question" || rows[0].version !== "1") {
        fail(`after saving the question draft the branch holds ${JSON.stringify(rows)}; exactly one question at version 1 was expected`);
      }
      await screenshot(page, "minimal-question-draft-saved");
    });

    await steps.step("leave-sign-in-again-and-continue-the-draft", async () => {
      // Leave: sign out through the header's account menu.
      await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      const accountMenu = await requireVisible(page, ".global-nav-user", "the account menu control");
      await accountMenu.click();
      const menuItems = page.locator('[role="menuitem"]');
      await menuItems.first().waitFor({ state: "visible", timeout: 10000 });
      await menuItems.last().click();
      await requireVisible(page, ".global-nav-signin", "the signed-out state");
      await requireGone(page, ".global-nav-user-handle", "sign-out must clear the signed-in header");
      await screenshot(page, "signed-out");

      // WHILE SIGNED OUT the project is still readable (it is Public) but
      // offers NO write affordance. The shell hands an anonymous reader
      // `role: null`, and a gate written as "not a viewer" is true for that
      // caller — the affordance would fail open. This visits the very pages
      // the signed-in reader will use, anonymously.
      await page.goto(branchAUrl, { waitUntil: "domcontentloaded" });
      await requireVisible(page, "[data-research-authoring]", "the research page for an anonymous reader");
      await requireGone(page, "[data-authoring-form]", "an authoring form for an anonymous reader");
      await requireGone(page, "[data-authoring-save]", "a save-a-draft control for an anonymous reader");
      await requireGone(page, "[data-authoring-new-branch-toggle]", "a create-branch control for an anonymous reader");
      await page.goto(
        `${BASE}/projects/${projectId}/research/objects/${questionId}?branch=${encodeURIComponent(branchAId)}`,
        { waitUntil: "domcontentloaded" },
      );
      await requireVisible(page, "[data-research-object]", "the object page for an anonymous reader");
      await requireGone(page, "[data-object-edit-form]", "an object editor for an anonymous reader");
      await requireGone(page, "[data-relation-form]", "a relation form for an anonymous reader");
      await requireGone(page, "[data-gate-check-panel]", "the review-check panel for an anonymous reader");
      await requireVisible(page, "[data-object-readonly]", "the read-only notice for an anonymous reader");
      await screenshot(page, "anonymous-reader-has-no-write-affordance");
      await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      await requireVisible(page, ".global-nav-signin", "the signed-out header before signing in again");

      // Come back: sign in through /login, then return to the branch page
      // saved before leaving.
      await page.locator('[data-home-entry="sign-in"]').first().click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 20000 });
      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const loginProbe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
      await requireVisible(page, ".global-nav-user-handle", "the signed-in account handle after signing in again");
      const login = responsesSince(loginProbe, "POST", "/api/v1/auth/login");
      if (login.length !== 1 || login[0].status !== 200) {
        fail(`signing in again did not go through the product's login endpoint (observed ${JSON.stringify(login.map((entry) => entry.status))})`);
      }

      const reopened = await page.goto(branchAUrl, { waitUntil: "domcontentloaded" });
      if (reopened === null || reopened.status() >= 400) {
        fail(`the saved branch page did not load after signing in again: HTTP ${reopened?.status() ?? "no response"} at ${branchAUrl}`);
      }
      await requireVisible(page, "[data-research-authoring]", "the authoring panel on the saved branch page");
      const select = await requireVisible(page, "[data-authoring-branch-select]", "the branch selector after returning");
      if ((await select.inputValue()) !== branchAId) {
        fail(`returning to the saved page selected ${JSON.stringify(await select.inputValue())}, not the branch ${branchAId}`);
      }
      const rowsAfterReturn = await inventoryWhen(1, "the saved branch page after returning");
      if (rowsAfterReturn[0].id !== questionId) {
        fail(`the saved branch page holds ${JSON.stringify(rowsAfterReturn)} after returning; the draft ${questionId} was expected`);
      }
      await screenshot(page, "returned-to-the-draft");

      // Continue it: open the draft from its own inventory row and add the
      // purpose as a NEW version. The v1 statement must already be in the
      // form — a blank form would silently overwrite the draft on save.
      const openedId = await openInventoryObject("research_question", "the question draft's inventory row");
      if (openedId !== questionId) {
        fail(`the inventory opened ${openedId}, not the draft ${questionId}`);
      }
      if (new URL(page.url()).searchParams.get("branch") !== branchAId) {
        fail(`the object page ${page.url()} does not carry the branch it was opened from`);
      }
      const selectedBefore = await textOf(page.locator("[data-selected-version]").first());
      if (selectedBefore !== "1") {
        fail(`the draft opens on version ${JSON.stringify(selectedBefore)}; it has only version 1`);
      }
      const headerText = await textOf(page.locator(".research-object-header h2").first());
      if (headerText !== QUESTION_STATEMENT) {
        fail(`the draft page is headed ${JSON.stringify(headerText)}, not its statement ${JSON.stringify(QUESTION_STATEMENT)}`);
      }
      const statementInput = await requireVisible(page, '[data-object-edit-input="statement"]', "the editor's statement field");
      if ((await statementInput.inputValue()) !== QUESTION_STATEMENT) {
        fail(`the editor opened with ${JSON.stringify(await statementInput.inputValue())} in the statement field; the saved draft was expected`);
      }
      await page.locator('[data-object-edit-input="purpose"]').fill(QUESTION_PURPOSE);
      await page.locator("[data-object-edit-save]").click();
      await requireVisible(page, '[data-research-object][data-object-version="2"]', "the question at version 2 after continuing it");
      const selectedAfter = await textOf(page.locator("[data-selected-version]").first());
      if (selectedAfter !== "2") {
        fail(`after continuing the draft the page reports selected version ${JSON.stringify(selectedAfter)}`);
      }
      const savedNotice = await requireVisible(page, "[data-object-edit-saved]", "the new-version confirmation");
      if (!(await textOf(savedNotice)).includes("version 2")) {
        fail(`the new-version confirmation reads ${JSON.stringify(await textOf(savedNotice))}`);
      }
      await requireVisible(page, '[data-version-option="1"]', "the draft's version 1 in the version log");
      const currentMark = await page.locator('[data-version-current="true"]').first().getAttribute("data-version-option");
      if (currentMark !== "2") {
        fail(`the version log marks version ${JSON.stringify(currentMark)} as current; the new version is 2`);
      }

      // A reload keeps it: the version log, the version on screen and the
      // typed purpose all come from the server.
      await page.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(page, '[data-research-object][data-object-version="2"]', "the question at version 2 after a reload");
      const payloadText = await textOf(page.locator("[data-object-payload]").first());
      if (!payloadText.includes(QUESTION_PURPOSE)) {
        fail(`after a reload the question's payload does not carry the purpose ${JSON.stringify(QUESTION_PURPOSE)}`);
      }
      await screenshot(page, "continued-draft-after-reload");
      await backToResearch();
      const rowsAfterContinue = await inventoryWhen(1, "the outline after continuing the draft");
      if (rowsAfterContinue[0].version !== "2") {
        fail(`the outline reads ${JSON.stringify(rowsAfterContinue)} after continuing the draft; one question at version 2 was expected`);
      }
    });

    await steps.step("the-four-slice-a-forms-save-their-own-objects", async () => {
      // Each type through its own form. The fields asserted here are the
      // schema's own field names: a form that silently dropped one would
      // fail before anything was typed.
      const forms = [
        {
          kind: "hypothesis",
          fields: ["statement", "question_id", "hypothesis_type", "assessment", "scope"],
        },
        {
          kind: "experiment",
          fields: ["objective", "operator_ids", "sample_ids", "protocol_version_id", "conditions", "result_summary", "result_outcome"],
        },
        {
          kind: "calculation",
          fields: ["objective", "method", "software", "input_object_ids", "protocol_version_id", "parameters", "result_summary", "result_outcome"],
        },
      ];
      for (const { kind, fields } of forms) {
        await requireVisible(page, `[data-authoring-kind="${kind}"]`, `the ${kind} kind tab`);
        await page.locator(`[data-authoring-kind="${kind}"]`).click();
        await requireVisible(page, `form[data-authoring-form="${kind}"]`, `the ${kind} draft form`);
        for (const name of fields) {
          await requireVisible(page, `[data-authoring-input="${name}"]`, `the ${kind} field ${name}`);
        }
      }
      // A kind the reader has not switched to is not rendered: the four
      // families are forms, not one generic JSON box.
      await requireGone(page, 'form[data-authoring-form="research_question"]', "the question form must be gone after switching kind");

      // Hypothesis: a minimal draft (statement only) on purpose — the review
      // check below names the question link the schema requires.
      await page.locator('[data-authoring-kind="hypothesis"]').click();
      await fillAuthoringField("statement", HYPOTHESIS_STATEMENT);
      const hypothesisId = await saveDraft("hypothesis", { statement: HYPOTHESIS_STATEMENT }, 1);

      // Experiment: a REAL failed run. The failure is the result, and the run
      // still names the people who made it.
      await page.locator('[data-authoring-kind="experiment"]').click();
      await fillAuthoringField("objective", EXPERIMENT_OBJECTIVE);
      await fillAuthoringField("operator_ids", EXPERIMENT_OPERATORS);
      await fillAuthoringField("conditions", EXPERIMENT_CONDITIONS);
      await fillAuthoringField("result_summary", EXPERIMENT_RESULT);
      await selectAuthoringField("result_outcome", "failed");
      const experimentId = await saveDraft("experiment", { objective: EXPERIMENT_OBJECTIVE }, 1);
      const experimentRow = await requireVisible(
        page,
        `[data-authoring-object="${experimentId}"]`,
        "the experiment inventory row",
      );
      const experimentOutcome = await experimentRow
        .locator("[data-authoring-object-outcome]")
        .getAttribute("data-authoring-object-outcome");
      if (experimentOutcome !== "failed") {
        fail(`the failed experiment is listed with outcome ${JSON.stringify(experimentOutcome)}; a failure is a result and must read as one`);
      }

      // Calculation: inconclusive, with the model it ran (software and
      // parameters) filled — the second version below proves that filling the
      // result does not drop the rest.
      await page.locator('[data-authoring-kind="calculation"]').click();
      await fillAuthoringField("objective", CALCULATION_OBJECTIVE);
      await fillAuthoringField("method", CALCULATION_METHOD);
      await fillAuthoringField("software", CALCULATION_SOFTWARE);
      await fillAuthoringField("parameters", CALCULATION_PARAMETERS);
      await fillAuthoringField("result_summary", CALCULATION_RESULT);
      await selectAuthoringField("result_outcome", "inconclusive");
      const calculationId = await saveDraft("calculation", { objective: CALCULATION_OBJECTIVE }, 1);

      const rows = await inventory();
      const byType = Object.fromEntries(rows.map((row) => [row.type, row]));
      for (const type of ["research_question", "hypothesis", "experiment", "calculation"]) {
        if (byType[type] === undefined) {
          fail(`the branch does not hold a ${type} after authoring all four (inventory: ${JSON.stringify(rows)})`);
        }
      }
      if (rows.length !== 4) {
        fail(`the branch holds ${rows.length} objects (${JSON.stringify(rows)}); the four authored families were expected`);
      }
      if (byType.research_question.version !== "2" || byType.hypothesis.version !== "1") {
        fail(`the inventory reports question v${byType.research_question.version} and hypothesis v${byType.hypothesis.version}; 2 and 1 were expected`);
      }
      if (byType.hypothesis.id !== hypothesisId) {
        fail("the inventory's hypothesis is not the one the journey saved");
      }
      await screenshot(page, "four-objects-on-the-branch");
    });

    await steps.step("the-review-check-names-the-gaps-and-submits-nothing", async () => {
      // Run the check on the minimal HYPOTHESIS, the object the schema says
      // is incomplete (it has no question_id yet).
      await openInventoryObject("hypothesis", "the hypothesis inventory row");
      const verdict = await runReviewCheck();
      if (verdict !== "blocked") {
        fail(`the pr gate on an incomplete branch reports verdict ${JSON.stringify(verdict)}; the server refuses a hypothesis without its question`);
      }
      const blocked = await requireVisible(page, "[data-gate-blocked]", "the blocking-gap summary");
      const blockedText = await textOf(blocked);
      const gapCount = Number(/^[^0-9]*(\d+)/.exec(blockedText)?.[1] ?? "0");
      if (!(gapCount > 0)) {
        fail(`the blocking summary reads ${JSON.stringify(blockedText)}, which names no gap count`);
      }
      const blockingRows = page.locator('[data-gate-row][data-gate-severity="blocking"]');
      if ((await blockingRows.count()) === 0) {
        fail("the report is blocked but lists no blocking row");
      }
      const schemaRow = await requireVisible(
        page,
        '[data-gate-row="schema_typed"][data-gate-severity="blocking"]',
        "the schema_typed blocking row",
      );
      const schemaRowText = await textOf(schemaRow);
      if (!schemaRowText.toLowerCase().includes("hypothesis")) {
        fail(`the blocking schema row reads ${JSON.stringify(schemaRowText)}, which names no hypothesis`);
      }
      const details = await page.locator("[data-gate-row][data-gate-severity='blocking'] small").allTextContents();
      if (!details.some((detail) => detail.trim() !== "")) {
        fail("no blocking row explains its gap (every `why` is empty)");
      }
      const warnings = await page.locator("[data-gate-warnings]").count();
      if (warnings !== 1) {
        fail(`the report shows ${warnings} warning summaries; the incomplete question/experiment/calculation owe at least one warning`);
      }
      // The honest half: the panel says it only checked. There is no submit
      // action in this slice and the page must not imply one happened.
      const notSubmitted = await requireVisible(page, "[data-gate-not-submitted]", "the not-submitted notice");
      const notice = await textOf(notSubmitted);
      if (!/nothing was submitted/i.test(notice)) {
        fail(`the check panel reads ${JSON.stringify(notice)}; a check-only run must say nothing was submitted`);
      }
      await screenshot(page, "review-check-blocked");
      await backToResearch();
    });

    await steps.step("versions-and-relations-survive-a-reload", async () => {
      // The typed relation, added from the hypothesis to the question by
      // choosing both ends — source is the version on screen.
      const hypothesisId = await openInventoryObject("hypothesis", "the hypothesis inventory row");
      const relationType = await requireVisible(page, "[data-authoring-relation-type]", "the relation type control");
      if ((await relationType.inputValue()) !== "addresses_question") {
        fail(`the relation form opens on ${JSON.stringify(await relationType.inputValue())}, not addresses_question`);
      }
      await selectByLabel(page, "[data-authoring-relation-target]", QUESTION_STATEMENT, "the relation target control");
      const relationProbe = evidence.network.length;
      await page.locator("[data-relation-add]").click();
      await requireVisible(page, "[data-relation-saved]", "the relation-added confirmation");
      const relationCall = await waitForResponse(relationProbe, "POST", "/relations");
      if (relationCall === null || relationCall.status !== 201) {
        fail(`adding the relation did not answer 201 (observed ${relationCall?.status ?? "no response"})`);
      }
      await requireVisible(page, '[data-relation-list] [data-relation-type="addresses_question"]', "the relation in the object's relation list");

      // Continue the hypothesis in a second version, choosing its question
      // from the same list the create form offers.
      await selectByLabel(page, '[data-object-edit-input="question_id"]', QUESTION_STATEMENT, "the editor's question select");
      await page.locator('[data-object-edit-input="hypothesis_type"]').fill(HYPOTHESIS_TYPE);
      await page.locator("[data-object-edit-save]").click();
      await requireVisible(page, '[data-research-object][data-object-version="2"]', "the hypothesis at version 2");
      await requireVisible(page, '[data-version-option="1"]', "the hypothesis's version 1 in the version log");

      // Reload: the version, the reference it stores and the relation are
      // all read back from the server.
      await page.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(page, '[data-research-object][data-object-id="' + hypothesisId + '"][data-object-version="2"]', "the hypothesis at version 2 after a reload");
      const questionSelect = await requireVisible(page, '[data-object-edit-input="question_id"]', "the question select after a reload");
      if ((await questionSelect.inputValue()) !== questionId) {
        fail(`after a reload the hypothesis's question reference is ${JSON.stringify(await questionSelect.inputValue())}, not ${questionId}`);
      }
      const payloadText = await textOf(page.locator("[data-object-payload]").first());
      if (!payloadText.includes(HYPOTHESIS_TYPE)) {
        fail(`after a reload the hypothesis's payload does not carry hypothesis_type ${JSON.stringify(HYPOTHESIS_TYPE)}`);
      }
      const relationAfterReload = await requireVisible(
        page,
        '[data-relation-list] [data-relation-type="addresses_question"]',
        "the relation after a reload",
      );
      if (!(await textOf(relationAfterReload)).includes(QUESTION_STATEMENT)) {
        fail("the relation row after a reload does not name the question it was added for");
      }
      await screenshot(page, "hypothesis-v2-with-relation");
      await backToResearch();
    });

    await steps.step("a-second-experiment-and-calculation-version-keeps-every-field", async () => {
      // A second version is a PATCH: only the result changes, and every other
      // field of the version it continues must survive. The editor is seeded
      // from the head version for exactly this reason; an editor offered on an
      // OLD version would write that old state over the head.
      const experimentId = await openInventoryObject("experiment", "the experiment inventory row");
      const outcomeSelect = await requireVisible(
        page,
        '[data-object-edit-input="result_outcome"]',
        "the experiment outcome select",
      );
      if ((await outcomeSelect.inputValue()) !== "failed") {
        fail(`the experiment editor opens on outcome ${JSON.stringify(await outcomeSelect.inputValue())}; the recorded failure must not be rewritten as a success`);
      }
      await page.locator('[data-object-edit-input="result_summary"]').fill(EXPERIMENT_RESULT_V2);
      await page.locator("[data-object-edit-save]").click();
      await requireVisible(page, '[data-research-object][data-object-version="2"]', "the experiment at version 2");
      await requireVisible(page, '[data-object-edit-saved="2"]', "the experiment saved-version-2 confirmation");

      // Nothing but the summary moved.
      const experimentPayload = await textOf(page.locator("[data-object-payload]").first());
      const mustSurvive = [
        ["the objective", EXPERIMENT_OBJECTIVE],
        ["the operators", "operator-alpha"],
        ["the conditions", "temperature_k"],
        ["the new summary", EXPERIMENT_RESULT_V2],
      ];
      for (const [what, needle] of mustSurvive) {
        if (!experimentPayload.includes(needle)) {
          fail(`the experiment's version-2 payload lost ${what} (${JSON.stringify(needle)}): ${JSON.stringify(experimentPayload)}`);
        }
      }
      // The outcome is still the recorded failure, not blanked by the patch.
      const outcomeAfterSave = await requireVisible(
        page,
        '[data-object-edit-input="result_outcome"]',
        "the experiment outcome select after the second version",
      );
      if ((await outcomeAfterSave.inputValue()) !== "failed") {
        fail(`the experiment's second version left outcome ${JSON.stringify(await outcomeAfterSave.inputValue())}, not failed`);
      }

      // An OLD version is for reading: no editor is offered on version 1, and
      // the page names the head it would have to continue instead.
      await page.locator('[data-version-option="1"]').click();
      await requireVisible(page, '[data-research-object][data-object-version="1"]', "the experiment at version 1");
      const stale = await requireVisible(page, "[data-object-edit-stale]", "the old-version read-only notice");
      if ((await stale.getAttribute("data-object-edit-stale")) !== "2") {
        fail(`the old-version notice names current version ${JSON.stringify(await stale.getAttribute("data-object-edit-stale"))}, not 2`);
      }
      await requireGone(page, "[data-object-edit-form]", "an editor on an old version");
      await screenshot(page, "old-version-is-read-only");
      const openCurrent = await requireVisible(page, "[data-object-edit-current]", "the link to the current version");
      await openCurrent.click();
      await page.waitForURL((url) => url.searchParams.get("version") === "2", { timeout: 20000 });
      await requireVisible(page, "[data-object-edit-form]", "the editor back on the current version");

      // A second relation TYPE, from a different family: the experiment tests
      // the hypothesis (the hypothesis page keeps the addresses_question one).
      await selectByLabel(page, "[data-authoring-relation-type]", "tests hypothesis", "the relation type control");
      await selectByLabel(page, "[data-authoring-relation-target]", HYPOTHESIS_STATEMENT, "the relation target control");
      const relationProbe = evidence.network.length;
      await page.locator("[data-relation-add]").click();
      await requireVisible(page, "[data-relation-saved]", "the second relation's confirmation");
      const relationCall = await waitForResponse(relationProbe, "POST", "/relations");
      if (relationCall === null || relationCall.status !== 201) {
        fail(`adding the tests_hypothesis relation did not answer 201 (observed ${relationCall?.status ?? "no response"})`);
      }
      await page.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        page,
        '[data-research-object][data-object-id="' + experimentId + '"][data-object-version="2"]',
        "the experiment at version 2 after a reload",
      );
      const relationRow = await requireVisible(
        page,
        '[data-relation-list] [data-relation-type="tests_hypothesis"]',
        "the tests_hypothesis relation after a reload",
      );
      if (!(await textOf(relationRow)).includes(HYPOTHESIS_STATEMENT)) {
        fail("the tests_hypothesis relation row does not name the hypothesis it was added for");
      }
      const payloadAfterReload = await textOf(page.locator("[data-object-payload]").first());
      for (const [what, needle] of mustSurvive) {
        if (!payloadAfterReload.includes(needle)) {
          fail(`after a reload the experiment's version-2 payload lost ${what} (${JSON.stringify(needle)})`);
        }
      }
      await screenshot(page, "experiment-v2-with-second-relation");
      await backToResearch();

      // The same second version on a CALCULATION: the result changes, the
      // model it ran (method, software, parameters) does not.
      const calculationId = await openInventoryObject("calculation", "the calculation inventory row");
      const calculationOnScreen = await page.locator("[data-research-object]").getAttribute("data-object-id");
      if (calculationOnScreen !== calculationId) {
        fail(`the calculation page shows ${JSON.stringify(calculationOnScreen)}, not the inventory's ${calculationId}`);
      }
      await page.locator('[data-object-edit-input="result_summary"]').fill(CALCULATION_RESULT_V2);
      await page.locator("[data-object-edit-save]").click();
      await requireVisible(page, '[data-research-object][data-object-version="2"]', "the calculation at version 2");
      const calculationPayload = await textOf(page.locator("[data-object-payload]").first());
      for (const [what, needle] of [
        ["the method", CALCULATION_METHOD],
        ["the software", CALCULATION_SOFTWARE],
        ["the parameters", "qmax"],
        ["the new result", CALCULATION_RESULT_V2],
      ]) {
        if (!calculationPayload.includes(needle)) {
          fail(`the calculation's version-2 payload lost ${what} (${JSON.stringify(needle)}): ${JSON.stringify(calculationPayload)}`);
        }
      }
      const calculationOutcome = await requireVisible(
        page,
        '[data-object-edit-input="result_outcome"]',
        "the calculation outcome select after the second version",
      );
      if ((await calculationOutcome.inputValue()) !== "inconclusive") {
        fail(`the calculation's second version left outcome ${JSON.stringify(await calculationOutcome.inputValue())}, not inconclusive`);
      }
      await screenshot(page, "calculation-v2-keeps-its-model");
      await backToResearch();
      const rowsAfterSecondVersions = await inventoryWhen(4, "the outline after the second versions");
      const versionsByType = Object.fromEntries(rowsAfterSecondVersions.map((row) => [row.type, row.version]));
      for (const type of ["research_question", "hypothesis", "experiment", "calculation"]) {
        if (versionsByType[type] !== "2") {
          fail(`the outline reports ${type} v${versionsByType[type]} after the second versions; every one of the four families should be at version 2`);
        }
      }
    });

    await steps.step("clearing-a-nullable-field-is-saved-and-reads-back-empty", async () => {
      // A version patch is a SHALLOW merge: a cleared field left OUT of the
      // patch keeps its old value, so "Saved as version N" would report a
      // clear that never happened. The clear is sent explicitly — `null` for
      // the fields the object schema types ["string", "null"] — and a reload
      // proves the stored value is gone rather than merely untyped.
      const calculationId = await openInventoryObject("calculation", "the calculation inventory row");
      const software = await requireVisible(page, '[data-object-edit-input="software"]', "the calculation software input");
      if ((await software.inputValue()) !== CALCULATION_SOFTWARE) {
        fail(`the calculation's software field opens with ${JSON.stringify(await software.inputValue())}, not the stored ${JSON.stringify(CALCULATION_SOFTWARE)}`);
      }
      await software.fill("");
      await page.locator("[data-object-edit-save]").click();
      await requireVisible(page, '[data-research-object][data-object-version="3"]', "the calculation at version 3 after clearing its software");
      await requireVisible(page, '[data-object-edit-saved="3"]', "the saved-version-3 confirmation");
      await page.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        page,
        '[data-research-object][data-object-id="' + calculationId + '"][data-object-version="3"]',
        "the calculation at version 3 after a reload",
      );
      const clearedSoftware = await requireVisible(page, '[data-object-edit-input="software"]', "the software input after the clear and a reload");
      if ((await clearedSoftware.inputValue()) !== "") {
        fail(`after clearing the software and reloading, the field reads ${JSON.stringify(await clearedSoftware.inputValue())}; the stored value must be gone, not re-seeded`);
      }
      const clearedPayload = await textOf(page.locator("[data-object-payload]").first());
      if (clearedPayload.includes(CALCULATION_SOFTWARE)) {
        fail(`after clearing the software the payload still shows ${JSON.stringify(CALCULATION_SOFTWARE)}: ${JSON.stringify(clearedPayload)}`);
      }
      // Everything the form did not clear is still there.
      for (const [what, needle] of [
        ["the method", CALCULATION_METHOD],
        ["the parameters", "qmax"],
        ["the result", CALCULATION_RESULT_V2],
      ]) {
        if (!clearedPayload.includes(needle)) {
          fail(`clearing the software also lost ${what} (${JSON.stringify(needle)}): ${JSON.stringify(clearedPayload)}`);
        }
      }
      await screenshot(page, "cleared-field-reads-back-empty");
      await backToResearch();
    });

    await steps.step("branch-switch-isolates-the-branches-and-keeps-history", async () => {
      // Switch by choosing the other branch. It was created before any
      // write, so it must hold nothing — and no object of branch A may leak
      // into it.
      await selectByLabel(page, "[data-authoring-branch-select]", BRANCH_B, "the branch selector");
      await page.waitForURL((url) => url.searchParams.get("branch") === branchBId, { timeout: 20000 });
      await requireVisible(page, "[data-authoring-inventory-empty]", "the other branch's empty inventory");
      await requireGone(page, "[data-authoring-object]", "the second branch must not show the first branch's objects");
      await screenshot(page, "branch-b-is-empty");

      // A COMPLETE question on branch B: every field the pr gate asks for.
      await page.locator('[data-authoring-kind="research_question"]').click();
      await fillAuthoringField("statement", QUESTION_ON_B);
      await fillAuthoringField("purpose", "Decide which sample the next isotherm run should start with.");
      await selectAuthoringField("question_state", "open");
      await saveDraft("research_question", { statement: QUESTION_ON_B }, 1);

      // The same check on a branch whose only object is complete reports no
      // blocking gap: the verdict is the server's answer, not a page state.
      const completeId = await openInventoryObject("research_question", "the complete question on branch B");
      const verdictB = await runReviewCheck();
      if (verdictB === "blocked") {
        fail(`the pr gate on a complete branch reports ${JSON.stringify(verdictB)}`);
      }
      await requireVisible(page, "[data-gate-passed]", "the passing summary on the complete branch");
      await requireGone(page, "[data-gate-blocked]", "a passing branch must not render a blocking summary");
      await screenshot(page, "review-check-passes-on-the-complete-branch");

      // Back to branch A: its four objects, their versions and its relation
      // are still there.
      await page.goto(branchAUrl, { waitUntil: "domcontentloaded" });
      await requireVisible(page, "[data-research-authoring]", "branch A's research page");
      const rowsA = await inventoryWhen(4, "branch A after the branch switch");
      const byType = Object.fromEntries(rowsA.map((row) => [row.type, row]));
      // Every authored family keeps its version log across the switch: the
      // question, hypothesis and experiment are at their second version, and
      // the calculation at its third (the clear that followed).
      const expectedVersions = { research_question: "2", hypothesis: "2", experiment: "2", calculation: "3" };
      for (const [type, expected] of Object.entries(expectedVersions)) {
        if (byType[type].version !== expected) {
          fail(`after the branch switch branch A reports ${type} v${byType[type].version}; version ${expected} was expected`);
        }
      }
      if (byType.experiment.outcome !== "failed") {
        fail(`after the branch switch the experiment's outcome reads ${JSON.stringify(byType.experiment.outcome)}, not failed`);
      }
      const reopenedHypothesis = await openInventoryObject("hypothesis", "the hypothesis on branch A after the branch switch");
      if (reopenedHypothesis !== byType.hypothesis.id) {
        fail("the reopened hypothesis is not the one the branch holds");
      }
      // Both relations, of both types, from both directions.
      await requireVisible(
        page,
        '[data-relation-list] [data-relation-type="addresses_question"]',
        "the addresses_question relation after the branch switch",
      );
      await requireVisible(
        page,
        '[data-relation-list] [data-relation-direction="incoming"][data-relation-type="tests_hypothesis"]',
        "the incoming tests_hypothesis relation after the branch switch",
      );
      const selected = await textOf(page.locator("[data-selected-version]").first());
      if (selected !== "2") {
        fail(`the hypothesis opens on version ${JSON.stringify(selected)} after the branch switch; version 2 was expected`);
      }
      await screenshot(page, "branch-a-history-intact");
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
          (entry) => entry.kind === "error" && !isExpectedRefusal(entry, projectId),
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
  console.error(`web-v2 journey (WEB-06a): FAILED — ${errorText(err)}`);
  process.exit(1);
});
