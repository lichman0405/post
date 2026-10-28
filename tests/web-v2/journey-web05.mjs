/**
 * WEB-05 journey (T1306): browsing a project's research objects — the
 * research map, the outline list and one object's version log (docs/70 G06,
 * U03, D14).
 *
 * What this proves, and why each assertion is the one it is:
 *
 *   1. U03 — "reaches a Question, an Experiment and a Finding version by
 *      clicking, without pasting an API URL". The journey starts at the
 *      product's own Home, signs in through the real /login form, enters
 *      /projects through the home project entry, opens the project row and
 *      the Research tab, and then drills down by CLICKING an outline row.
 *      Every object page URL is the web's own route (`/projects/…/research/
 *      objects/…`); no step navigates to an `/api/` address and no object id
 *      is ever typed — the reader recognises an object by its statement, and
 *      so does this journey.
 *   2. "Old version and return path keep the selected object". Selecting
 *      version 1 is a click, the page re-renders THAT version's own words
 *      (the header and the payload, not a version badge alone), the address
 *      bar carries `?version=1`, and a reload renders the same version — the
 *      selection is in the URL, not in hidden component state. Following the
 *      relation to another object leaves a return link naming the object it
 *      came from, and that link comes back to the SAME object at the SAME
 *      version.
 *   3. "Research map and list agree on the object set". Both surfaces render
 *      one traversal, so this asserts that on the live DOM in BOTH views:
 *      the ids on the map equal the ids in the list, no id appears twice, and
 *      the set is not trivially empty (each view names the objects its own
 *      axis adds — the questions view the experiment, the findings view the
 *      claim — so a view that silently rendered the other one cannot pass).
 *
 * WHERE THE RESEARCH STATE COMES FROM. WEB-05 is the BROWSE half of G06: the
 * web has no surface yet for authoring a question, a version or a relation
 * (that is a later work package). The harness therefore seeds one PRIVATE
 * project with its research state through the same application services the
 * API writes through (harness/fixture.go), and this journey only READS it.
 * docs/70 §6.3 allows exactly that seed; §6.2 forbids it from standing in for
 * a business step, and none is claimed here — no step says the reader created
 * or versioned anything. The account is the fixture's owner, signed in
 * through the product's own login form; no session is planted.
 *
 * Anti-bypass (docs/70 §6.2, scripts/tests/web-v2-bypass-scan.mjs): no
 * test-side request, SQL, CLI, cookie/storage injection or in-page script in
 * any step. Every assertion is a click, a keystroke or a read of the rendered
 * DOM and its attributes.
 *
 * Usage: node tests/web-v2/journey-web05.mjs <web-base-url>
 * Env:   WEB_V2_RUN_ID, WEB_V2_COMMIT, WEB_V2_EVIDENCE_DIR,
 *        WEB_V2_API_ORIGIN, WEB_V2_WEB05_EMAIL, WEB_V2_WEB05_PASSWORD
 */

import path from "node:path";

import { chromium } from "playwright";

import { Evidence } from "./lib/evidence.mjs";
import { JourneySteps, errorText } from "./lib/steps.mjs";

const BASE = (process.argv[2] ?? process.env.WEB_V2_BASE_URL ?? "").replace(/\/$/, "");
if (BASE === "") {
  console.error("web-v2 journey (WEB-05): FAILED — no web base URL was provided; a journey with no target is not a pass");
  process.exit(1);
}

const RUN_ID = process.env.WEB_V2_RUN_ID ?? `adhoc-${Date.now()}`;
const COMMIT = process.env.WEB_V2_COMMIT ?? "unknown";
// Its own subdirectory, like WEB-03 and WEB-04: four journeys write four
// step records, four traces and four screenshot series, and merging them
// into one run-id would make none of them reviewable.
const OUT_DIR = path.join(
  process.env.WEB_V2_EVIDENCE_DIR ?? `tests/web-v2/evidence/${RUN_ID}`,
  "web05",
);
const API_ORIGIN = process.env.WEB_V2_API_ORIGIN ?? "";
// The fixture owner's synthetic credentials, derived from the run id exactly
// as the harness derives them (harness/main.go loadConfig). The password is
// never written to any evidence file or log.
const EMAIL = process.env.WEB_V2_WEB05_EMAIL ?? `webv2-web05-${RUN_ID.toLowerCase()}@example.test`;
const PASSWORD = process.env.WEB_V2_WEB05_PASSWORD ?? `webv2-synthetic-${RUN_ID}-password`;

// The harness fixture's fixed identity (harness/fixture.go). The titles are
// the CURRENT versions — what the outline shows — and the OLD titles are the
// first versions, which the object page must render once version 1 is
// selected. They differ, so "old version" is observable content and not only
// a number.
const PROJECT_SLUG = "isotherm-browse-lab";
const PROJECT_NAME = "Isotherm Browse Lab";
const QUESTION_CURRENT = "Does the material adsorb CO2 above 1 bar at 298 K?";
const QUESTION_OLD = "Does the material adsorb CO2 at 298 K?";
const EXPERIMENT_CURRENT = "Measure the CO2 isotherm at 298 K between 1 and 40 bar";
const EXPERIMENT_OLD = "Measure the CO2 isotherm at 298 K";
const FINDING_CURRENT = "Uptake peaks at 32 bar";
const FINDING_OLD = "Uptake peaks at 30 bar";

const STEP_NAMES = [
  "sign-in-to-the-browse-fixture",
  "reach-the-project-research-surface",
  "research-map-and-list-agree-in-both-views",
  "reach-question-experiment-and-finding-old-versions",
  "old-version-and-return-path-keep-the-selected-object",
];

const steps = new JourneySteps("WEB-05-browse-research-objects", STEP_NAMES);
const evidence = new Evidence({
  runId: RUN_ID,
  outDir: OUT_DIR,
  commit: COMMIT,
  apiOrigin: API_ORIGIN,
  webOrigin: BASE,
  entryUrl: `${BASE}/`,
  journeyName: "WEB-05-browse-research-objects",
  task: "T1306",
  role: "project owner (seeded browse fixture), signed in through /login",
  accountEmail: EMAIL,
  expectedSteps: STEP_NAMES,
});

const tracePath = `${OUT_DIR}/trace.zip`;

function log(message) {
  console.log(`web-v2 journey (WEB-05): ${message}`);
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

/**
 * Wait until the network log holds a response for `method` + URL suffix at or
 * after `since`, or answer null once `timeout` ms have passed.
 *
 * A click's promise and a rendered title both settle before the response the
 * click triggered reaches Playwright's response listener, so reading the log
 * on the line after navigation is a race — it reported "no response" for a
 * read that had in fact answered 200 a millisecond later. The loop orders the
 * observation; it does not soften it: the caller still asserts the exact
 * status, and null after the timeout is still a failure.
 */
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
 * The responses this journey asks for by URL and status, so letting their
 * console lines through does not excuse a silent failure:
 *
 *   - 401 GET /api/v1/auth/session: the signed-out probe the nav makes on
 *     every anonymous page. The only anonymous page here is the first one,
 *     before the sign-in step; every later read runs with a session.
 *
 * Any other console error fails the run.
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

/** Every value of `attr` over the matched elements, in DOM order. */
async function attributeValues(page, selector, attr) {
  const out = [];
  const count = await page.locator(selector).count();
  for (let index = 0; index < count; index += 1) {
    const value = await page.locator(selector).nth(index).getAttribute(attr);
    if (value === null || value === "") {
      fail(`an element matching ${selector} has no ${attr} at index ${index} on ${page.url()}`);
    }
    out.push(value);
  }
  return out;
}

function sortedCsv(values) {
  return [...values].sort().join(",");
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

  /** Click the Research tab from wherever the project shell is. */
  async function openResearchTab() {
    const tab = await requireVisible(page, '[data-project-tab="research"]', "the project's Research tab");
    await tab.click();
    await page.waitForURL((url) => url.pathname.endsWith("/research"), { timeout: 20000 });
    await requireVisible(page, "[data-research-workspace]", "the research workspace");
    await requireVisible(page, "[data-research-list]", "the outline list");
  }

  /** Go back to the outline from an object page, through the breadcrumb. */
  async function backToResearch() {
    const link = await requireVisible(page, '[data-back-to="research"]', "the breadcrumb back to the research outline");
    await link.click();
    await page.waitForURL((url) => url.pathname.endsWith("/research"), { timeout: 20000 });
    await requireVisible(page, "[data-research-list]", "the outline list after returning");
  }

  /**
   * One drill-down: click the outline row of `objectType`, then click the
   * first version in the version log. Returns nothing; every intermediate
   * fact is asserted where it is observed so a failure names the object it
   * was looking at.
   */
  async function openRowAndSelectOldVersion(objectType, currentTitle, oldTitle) {
    const selector = `[data-research-list] [data-research-row][data-object-type="${objectType}"]`;
    const row = await requireVisible(page, selector, `the ${objectType} row of the outline list`);
    const rowCount = await page.locator(selector).count();
    if (rowCount !== 1) {
      fail(`the outline list shows ${rowCount} ${objectType} rows; the fixture has exactly one`);
    }
    const rowText = await textOf(row);
    if (!rowText.includes(currentTitle)) {
      fail(`the ${objectType} row reads ${JSON.stringify(rowText)}, which does not name ${JSON.stringify(currentTitle)}`);
    }
    const href = (await row.getAttribute("href")) ?? "";
    if (!/^\/projects\/[^/]+\/research\/objects\/[^/?#]+$/.test(href)) {
      fail(`the ${objectType} row links to ${JSON.stringify(href)}, not a research object page`);
    }
    const rowObjectID = objectIdOf(new URL(href, BASE).toString());
    if (rowObjectID === "") {
      fail(`the ${objectType} row's link ${JSON.stringify(href)} names no object`);
    }

    await row.click();
    await page.waitForURL((url) => url.pathname.includes(`/research/objects/${rowObjectID}`), { timeout: 20000 });
    const currentUrl = new URL(page.url());
    if (!currentUrl.pathname.startsWith("/projects/") || currentUrl.pathname.includes("/api/")) {
      fail(`the object page URL ${page.url()} is not the web's own project route`);
    }
    // Wait for THIS object's page, not the page the browser was on a moment
    // ago: the click's URL change lands before the detail read does.
    const object = await requireVisible(
      page,
      `[data-research-object][data-object-id="${rowObjectID}"]`,
      `the ${objectType} object page`,
    );
    const gotType = await object.getAttribute("data-object-type");
    if (gotType !== objectType) {
      fail(`clicking the ${objectType} row opened a ${JSON.stringify(gotType)} page`);
    }

    // The page opens on the object's newest version, and says so.
    const header = page.locator(".research-object-header h2").first();
    await header.waitFor({ state: "visible", timeout: 20000 });
    const headerText = await textOf(header);
    if (headerText !== currentTitle) {
      fail(`the ${objectType} page opens on ${JSON.stringify(headerText)}, not the newest version ${JSON.stringify(currentTitle)}`);
    }
    const selectedBefore = await textOf(page.locator("[data-selected-version]").first());
    if (selectedBefore !== "2") {
      fail(`the ${objectType} page reports selected version ${JSON.stringify(selectedBefore)} before any click; the fixture head is 2`);
    }

    // Selecting the OLD version is a click on the version log. The wait is on
    // the RENDERED version, not only the address bar: the URL changes before
    // the version's own read lands, and reading the page in between would
    // assert against the version the reader just left.
    const option = await requireVisible(page, '[data-version-option="1"]', `the version-1 option of the ${objectType}`);
    await option.click();
    await page.waitForURL((url) => url.searchParams.get("version") === "1", { timeout: 20000 });
    await requireVisible(
      page,
      `[data-research-object][data-object-id="${rowObjectID}"][data-object-version="1"]`,
      `the ${objectType} page rendering version 1`,
    );

    const selectedAfter = await textOf(page.locator("[data-selected-version]").first());
    if (selectedAfter !== "1") {
      fail(`after clicking version 1 the ${objectType} page reports selected version ${JSON.stringify(selectedAfter)}`);
    }
    const headerAfter = await textOf(header);
    if (headerAfter !== oldTitle) {
      fail(`version 1 of the ${objectType} renders ${JSON.stringify(headerAfter)}, not its own content ${JSON.stringify(oldTitle)}`);
    }
    const payloadValues = await page.locator("[data-object-payload] dd").allTextContents();
    if (!payloadValues.some((value) => value.trim() === oldTitle)) {
      fail(`version 1's payload of the ${objectType} does not carry its own statement ${JSON.stringify(oldTitle)} (fields: ${JSON.stringify(payloadValues)})`);
    }
    const currentMarks = await page.locator('[data-version-current="true"]').count();
    if (currentMarks !== 1) {
      fail(`the ${objectType} version log marks ${currentMarks} options as current; exactly one version is the head`);
    }
    const currentOption = await page.locator('[data-version-current="true"]').first().getAttribute("data-version-option");
    if (currentOption !== "2") {
      fail(`the ${objectType} version log marks version ${JSON.stringify(currentOption)} as current; the fixture head is 2`);
    }
  }

  try {
    await steps.step("sign-in-to-the-browse-fixture", async () => {
      const response = await page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
      if (response === null || response.status() >= 400) {
        fail(`home page did not load: HTTP ${response?.status() ?? "no response"} at ${BASE}/`);
      }
      const entry = await requireVisible(page, '[data-home-entry="sign-in"]', "the home sign-in entry");
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/login", { timeout: 20000 });
      await requireVisible(page, ".auth-card", "the sign-in card");

      await page.locator('form input[type="email"]').fill(EMAIL);
      await page.locator('form input[type="password"]').fill(PASSWORD);
      const probe = evidence.network.length;
      await page.locator('form button[type="submit"]').click();
      try {
        await page.waitForURL((url) => url.pathname === "/", { timeout: 20000 });
      } catch {
        const flash = await page.locator('[role="alert"]').first().textContent().catch(() => null);
        fail(`sign-in did not reach the signed-in home (URL ${page.url()}${flash ? `; page reported: ${flash.trim()}` : ""})`);
      }
      await requireVisible(page, ".global-nav-user-handle", "the signed-in account handle");

      const login = responsesSince(probe, "POST", "/api/v1/auth/login");
      if (login.length !== 1 || login[0].status !== 200) {
        fail(`sign-in did not go through the product's login endpoint (observed ${JSON.stringify(login.map((entry) => entry.status))})`);
      }
      await screenshot(page, "signed-in");
    });

    await steps.step("reach-the-project-research-surface", async () => {
      // Entered through the signed-in home's own project entry, then the
      // project row, then the project's Research tab: the route a reader is
      // offered, not a typed URL.
      const entry = await requireVisible(
        page,
        '[data-home-entry="project-workspaces"]',
        "the home project entry",
      );
      const listProbe = evidence.network.length;
      await entry.click();
      await page.waitForURL((url) => url.pathname === "/projects", { timeout: 20000 });
      await requireVisible(page, ".project-directory-title", "the project directory title");
      const listRead = await waitForResponse(listProbe, "GET", "/api/v1/projects");
      if (listRead === null || listRead.status !== 200) {
        fail(`the project directory read did not answer 200 (${JSON.stringify(listRead)})`);
      }

      const row = await requireVisible(
        page,
        `[data-project-row="${PROJECT_SLUG}"]`,
        "the seeded project's directory row",
      );
      if (!(await textOf(row)).includes(PROJECT_NAME)) {
        fail(`the directory row for ${PROJECT_SLUG} does not name ${JSON.stringify(PROJECT_NAME)}`);
      }
      const rowHref = (await row.getAttribute("href")) ?? "";
      if (!/^\/projects\/[^/]+$/.test(rowHref)) {
        fail(`the project row links to ${JSON.stringify(rowHref)}, not a project page`);
      }
      await row.click();
      await page.waitForURL((url) => /^\/projects\/[^/]+$/.test(url.pathname), { timeout: 20000 });
      await requireVisible(page, "[data-project-shell]", "the project shell");

      const outlineProbe = evidence.network.length;
      await openResearchTab();
      const outlineRead = await waitForResponse(outlineProbe, "GET", "/research");
      if (outlineRead === null || outlineRead.status !== 200) {
        fail(`the research outline read did not answer 200 (${JSON.stringify(outlineRead)})`);
      }
      const hero = await textOf(page.locator(".research-hero").first());
      if (!hero.includes(PROJECT_NAME)) {
        fail(`the research page does not name its project ${JSON.stringify(PROJECT_NAME)}`);
      }
      await screenshot(page, "research-workspace");
    });

    await steps.step("research-map-and-list-agree-in-both-views", async () => {
      async function assertAgreement(view) {
        const mapIds = await attributeValues(
          page,
          "[data-research-map] [data-research-map-node]",
          "data-research-map-node",
        );
        const listIds = await attributeValues(
          page,
          "[data-research-list] [data-research-row]",
          "data-research-row",
        );
        if (mapIds.length === 0 || listIds.length === 0) {
          fail(`the ${view} view renders an empty object set (map ${mapIds.length}, list ${listIds.length}); an empty agreement proves nothing`);
        }
        if (new Set(mapIds).size !== mapIds.length) {
          fail(`the ${view} map renders a duplicate node id: ${mapIds.join(", ")}`);
        }
        if (new Set(listIds).size !== listIds.length) {
          fail(`the ${view} list renders a duplicate row id: ${listIds.join(", ")}`);
        }
        if (sortedCsv(mapIds) !== sortedCsv(listIds)) {
          fail(`the ${view} view disagrees about its objects — map [${sortedCsv(mapIds)}] vs list [${sortedCsv(listIds)}]`);
        }
        return listIds;
      }

      // The questions view, the outline's default axis.
      const questions = await assertAgreement("questions");
      const questionTypes = await attributeValues(
        page,
        "[data-research-list] [data-research-row]",
        "data-object-type",
      );
      for (const type of ["research_question", "experiment", "finding"]) {
        if (!questionTypes.includes(type)) {
          fail(`the questions view does not show a ${type} (types: ${[...new Set(questionTypes)].join(", ")})`);
        }
      }
      if (questions.length < 3) {
        fail(`the questions view shows ${questions.length} objects; the fixture's question, experiment and finding are at least three`);
      }
      await screenshot(page, "map-and-list-questions");

      // The findings view: the other axis. Its own discriminator is the
      // claim the finding cites — an object the questions view does not
      // render — so a page that silently kept the other view cannot pass.
      const findingsLink = await requireVisible(
        page,
        '[data-research-view-link="findings"]',
        "the findings view switch",
      );
      await findingsLink.click();
      await page.waitForURL((url) => url.searchParams.get("view") === "findings", { timeout: 20000 });
      await requireVisible(page, '[data-research-list] [data-object-type="claim"]', "the claim node the findings view adds");
      await requireGone(page, '[data-research-list] [data-object-type="experiment"]', "the findings view must not keep the questions view's experiment node");

      await assertAgreement("findings");
      await screenshot(page, "map-and-list-findings");

      // Back to the questions view: its experiment node returns, its claim
      // node leaves. The switch is a link both ways.
      const questionsLink = await requireVisible(
        page,
        '[data-research-view-link="questions"]',
        "the questions view switch",
      );
      await questionsLink.click();
      await page.waitForURL((url) => url.searchParams.get("view") === "questions", { timeout: 20000 });
      await requireVisible(page, '[data-research-list] [data-object-type="experiment"]', "the experiment node after switching back");
      await requireGone(page, '[data-research-list] [data-object-type="claim"]', "the questions view must not keep the findings view's claim node");
    });

    await steps.step("reach-question-experiment-and-finding-old-versions", async () => {
      // U03: each of the three object types, entered by clicking its outline
      // row, drilled to its FIRST version by clicking the version log.
      for (const [type, currentTitle, oldTitle, label] of [
        ["research_question", QUESTION_CURRENT, QUESTION_OLD, "question"],
        ["experiment", EXPERIMENT_CURRENT, EXPERIMENT_OLD, "experiment"],
        ["finding", FINDING_CURRENT, FINDING_OLD, "finding"],
      ]) {
        await requireVisible(page, "[data-research-list]", "the outline list");
        await openRowAndSelectOldVersion(type, currentTitle, oldTitle);
        await screenshot(page, `${label}-old-version`);
        await backToResearch();
      }
    });

    await steps.step("old-version-and-return-path-keep-the-selected-object", async () => {
      // Open the question from the outline again, and select its old version.
      await openRowAndSelectOldVersion("research_question", QUESTION_CURRENT, QUESTION_OLD);
      const questionID = objectIdOf(page.url());
      if (questionID === "") {
        fail(`the question page URL ${page.url()} names no object`);
      }
      if (new URL(page.url()).searchParams.get("version") !== "1") {
        fail(`the selected version is not in the address bar: ${page.url()}`);
      }

      // A reload renders the same version with the same content: the
      // selection lives in the URL, not in component state (docs/70 §6.1.3).
      await page.reload({ waitUntil: "domcontentloaded" });
      await requireVisible(
        page,
        `[data-research-object][data-object-id="${questionID}"][data-object-version="1"]`,
        "the question page after a reload",
      );
      const selectedAfterReload = await textOf(page.locator("[data-selected-version]").first());
      if (selectedAfterReload !== "1") {
        fail(`after a reload the question page reports selected version ${JSON.stringify(selectedAfterReload)}`);
      }
      const headerAfterReload = await textOf(page.locator(".research-object-header h2").first());
      if (headerAfterReload !== QUESTION_OLD) {
        fail(`after a reload the question page renders ${JSON.stringify(headerAfterReload)}, not version 1's ${JSON.stringify(QUESTION_OLD)}`);
      }

      // Follow the relation to the finding. The question is addressed by
      // BOTH the finding and the experiment (that is why the questions view
      // shows both), so the journey names the FINDING's relation by its
      // object type instead of trusting list order. The link must carry the
      // object (and version) it was followed from — that is what makes the
      // return path possible at all.
      await requireVisible(page, "[data-relation-list]", "the question's relations");
      const relation = await requireVisible(
        page,
        '[data-relation-list] [data-relation-target][data-relation-object-type="finding"]',
        "the relation link to the linked finding",
      );
      const relationHref = (await relation.getAttribute("href")) ?? "";
      const relationQuery = new URL(relationHref, BASE).searchParams;
      if (relationQuery.get("from") !== questionID) {
        fail(`the relation link reads from=${JSON.stringify(relationQuery.get("from"))}, not the question ${questionID} it was followed from`);
      }
      if (relationQuery.get("fromVersion") !== "1") {
        fail(`the relation link reads fromVersion=${JSON.stringify(relationQuery.get("fromVersion"))}, not the selected version 1`);
      }
      const relationTargetID = objectIdOf(new URL(relationHref, BASE).toString());
      if (relationTargetID === "" || relationTargetID === questionID) {
        fail(`the relation link ${JSON.stringify(relationHref)} does not point at another research object`);
      }

      await relation.click();
      await page.waitForURL((url) => url.pathname.includes(`/research/objects/${relationTargetID}`), { timeout: 20000 });
      await requireVisible(page, "[data-research-object]", "the related object's page");
      const relationType = await page.locator("[data-research-object]").first().getAttribute("data-object-type");
      if (relationType !== "finding") {
        fail(`the question's relation opened a ${JSON.stringify(relationType)} instead of the linked finding`);
      }
      await screenshot(page, "finding-page-with-return-link");

      // The return link names the object it came from, carries its version,
      // and goes back to THAT object at THAT version.
      const back = await requireVisible(page, '[data-back-to="object"]', "the return link to the object the relation was followed from");
      const backHref = (await back.getAttribute("href")) ?? "";
      if (objectIdOf(new URL(backHref, BASE).toString()) !== questionID) {
        fail(`the return link ${JSON.stringify(backHref)} does not point back at the question ${questionID}`);
      }
      if (new URL(backHref, BASE).searchParams.get("version") !== "1") {
        fail(`the return link ${JSON.stringify(backHref)} does not carry the selected version 1`);
      }
      if (!(await textOf(back)).includes(QUESTION_CURRENT)) {
        fail(`the return link reads ${JSON.stringify(await textOf(back))}, which does not name the question`);
      }

      await back.click();
      await page.waitForURL(
        (url) => url.pathname.includes(`/research/objects/${questionID}`) && url.searchParams.get("version") === "1",
        { timeout: 20000 },
      );
      await requireVisible(page, "[data-research-object]", "the question page after returning");
      const backType = await page.locator("[data-research-object]").first().getAttribute("data-object-type");
      if (backType !== "research_question") {
        fail(`the return link landed on a ${JSON.stringify(backType)} page, not the question`);
      }
      const selectedAfterReturn = await textOf(page.locator("[data-selected-version]").first());
      if (selectedAfterReturn !== "1") {
        fail(`returning to the question reports selected version ${JSON.stringify(selectedAfterReturn)}, not 1`);
      }
      const headerAfterReturn = await textOf(page.locator(".research-object-header h2").first());
      if (headerAfterReturn !== QUESTION_OLD) {
        fail(`returning to the question renders ${JSON.stringify(headerAfterReturn)}, not version 1's ${JSON.stringify(QUESTION_OLD)}`);
      }
      await screenshot(page, "back-on-question-old-version");

      // And the outline remembers which object the reader came from: the
      // breadcrumb focuses it, in both surfaces.
      await backToResearch();
      const focused = await attributeValues(
        page,
        "[data-research-list] [data-research-row][data-research-focus]",
        "data-object-type",
      );
      if (focused.length !== 1 || focused[0] !== "research_question") {
        fail(`the outline marks ${JSON.stringify(focused)} as the focused object after returning; exactly the question was expected`);
      }
      const focusedMap = await page.locator("[data-research-map] [data-research-map-node][data-research-focus]").count();
      if (focusedMap !== 1) {
        fail(`the map marks ${focusedMap} nodes as focused after returning; exactly one was expected`);
      }
      const focusedRow = page.locator("[data-research-list] [data-research-row][data-research-focus]").first();
      if (!(await textOf(focusedRow)).includes(QUESTION_CURRENT)) {
        fail("the focused outline row is not the question the reader returned from");
      }
      await screenshot(page, "outline-focused-on-returned-object");
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
  console.error(`web-v2 journey (WEB-05): FAILED — ${errorText(err)}`);
  process.exit(1);
});
