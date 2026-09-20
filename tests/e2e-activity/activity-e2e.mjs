/**
 * Activity timeline e2e (T0607 required test "activity e2e"): a real
 * Chromium drives the BUILT web app (next start, production layer) through
 * the project's Activity tab. The Go API is mocked at the network layer
 * with the exact wire shapes cmd/api/audithttp produces —
 * activityEntryPayload with its T0607 fields (source, payload, visibility),
 * the keyset cursor and the stable error envelope. The integration suite
 * (tests/integration/activity_sources_test.go) locks those shapes against
 * real PostgreSQL, so the two halves cover the real contract without the
 * browser test depending on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - filter governance/research events: the three chips read the feed
 *     three ways — the filter travels as ?source=, the unfiltered chips
 *     send no source at all, and the rows the page renders are the ones
 *     that answer contains;
 *   - actor/via links: each row links its actor to the profile route and
 *     names the channel the change arrived through, in words;
 *   - abort/reopen/release display: the abort renders its reason code, the
 *     human explanation and the lifecycle move it recorded — the research
 *     half of the same abort renders the event body it was emitted with
 *     and claims NO explanation (the platform does not put the free text
 *     there); a release renders its version and short manifest hash and
 *     links the release page; a reopening renders as its own family;
 *   - pagination: "Load more" walks the API's cursor and appends one
 *     window, never re-rendering the first page;
 *   - the failure shapes: a refused ?source= value renders the page's own
 *     refusal without any feed request, a malformed row is refused by the
 *     client (error state, not a silently rendered row), and a denied feed
 *     renders the stable message.
 *
 * Usage: node activity-e2e.mjs <base-url> [apps-web-dir]
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31110";

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
const check = (label, condition, detail) => (condition ? ok(label) : fail(label, detail));

/* ---------- A. Fixtures (the exact wire shapes of cmd/api/audithttp) ---- */

const API = "http://api.e2e.test";

const PROJECT = {
  id: "11111111-2222-4333-8444-555555555555",
  organization_id: "99999999-2222-4333-8444-555555555555",
  program_id: null,
  slug: "mof-lab",
  name: "MOF Lab",
  purpose: "Screen MOFs for carbon capture.",
  activity_status: "active",
  visibility: "public",
  main_frozen: true,
  git_repository_external_id: null,
  provision_status: "ready",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-10T08:00:00Z",
};

const MEMBERSHIP = {
  project_id: PROJECT.id,
  user_id: "00000000-0000-4000-8000-000000000001",
  role: "owner",
  created_at: "2026-09-10T08:00:00Z",
};

const ALICE = {
  actor_id: "00000000-0000-4000-8000-000000000001",
  actor_handle: "alice",
  actor_display_name: "Alice Guo",
};

const OBJECT_ID = "0a0b0c0d-3333-4000-8000-000000000004";
const RELEASE_ID = "0a0b0c0d-4444-4000-8000-000000000005";

/** The abort's GOVERNANCE row: the audit half, which carries the free-text
 *  explanation and the lifecycle move (internal/application/aborts's
 *  auditEntry). */
const ABORT_AUDIT = {
  id: "0a0b0c0d-0000-4000-8000-0000000000a1",
  source: "governance",
  ...ALICE,
  via: "session",
  action: "scientific_object.aborted",
  target_ref: `object:${OBJECT_ID}`,
  project_id: PROJECT.id,
  organization_id: null,
  correlation_id: "corr-abort",
  before_summary: { object_id: OBJECT_ID, version_no: 3, lifecycle_state: "active" },
  after_summary: {
    object_id: OBJECT_ID,
    object_type: "structure",
    aborted_version_id: "0a0b0c0d-6666-4000-8000-000000000007",
    aborted_version_no: 3,
    version_no: 4,
    lifecycle_state: "aborted",
    reason_code: "contaminated",
    explanation: "the sample was contaminated in transit",
  },
  occurred_at: "2026-09-14T09:30:00Z",
};

/** The abort's RESEARCH row: the event half, which carries identity and
 *  reference only — no explanation (internal/application/aborts/events.go). */
const ABORT_EVENT = {
  id: "0a0b0c0d-0000-4000-8000-0000000000a2",
  source: "research",
  ...ALICE,
  via: "claude_code",
  action: "scientific_object.aborted",
  target_ref: null,
  project_id: PROJECT.id,
  organization_id: null,
  correlation_id: "corr-abort-event",
  payload: {
    object_id: OBJECT_ID,
    object_type: "structure",
    version_no: 4,
    state_id: "0a0b0c0d-7777-4000-8000-000000000008",
    branch_id: "0a0b0c0d-8888-4000-8000-000000000009",
    aborted_version_no: 3,
    reason_code: "contaminated",
  },
  visibility: "private",
  occurred_at: "2026-09-14T09:30:01Z",
};

/** A RELEASE's governance row: the release page link comes from target_ref. */
const RELEASE_AUDIT = {
  id: "0a0b0c0d-0000-4000-8000-0000000000a3",
  source: "governance",
  ...ALICE,
  via: "web",
  action: "release.created",
  target_ref: `release:${RELEASE_ID}`,
  project_id: PROJECT.id,
  organization_id: null,
  correlation_id: "corr-release",
  before_summary: null,
  after_summary: {
    version: "v1.2.0",
    state_id: "0a0b0c0d-9999-4000-8000-00000000000a",
    manifest_hash: "9f2c1ab34d5e6f708192a3b4c5d6e7f80912a3b4c5d6e7f80912a3b4c5d6e7f8",
  },
  occurred_at: "2026-09-13T12:00:00Z",
};

/** A REOPENING's research row: its own family, so the page's reopen
 *  rendering is covered without waiting for the governance action T0610
 *  owns (nothing writes an audit row named scientific_object.reopened yet). */
const REOPEN_EVENT = {
  id: "0a0b0c0d-0000-4000-8000-0000000000a4",
  source: "research",
  actor_id: null,
  actor_handle: null,
  actor_display_name: null,
  via: "mcp",
  action: "scientific_object.reopened",
  target_ref: null,
  project_id: PROJECT.id,
  organization_id: null,
  correlation_id: "corr-reopen",
  payload: { object_id: OBJECT_ID, version_no: 5, reason_code: "wrong_sample" },
  visibility: "private",
  occurred_at: "2026-09-15T08:00:00Z",
};

/** A governance row whose action no vocabulary knows: it renders RAW. */
const UNKNOWN_ACTION_AUDIT = {
  ...RELEASE_AUDIT,
  id: "0a0b0c0d-0000-4000-8000-0000000000a5",
  action: "something.new_thing",
  target_ref: null,
  after_summary: null,
  occurred_at: "2026-09-12T12:00:00Z",
};

/** Newest first, one window: the two registries interleaved. */
const FIRST_PAGE = [REOPEN_EVENT, ABORT_EVENT, ABORT_AUDIT, RELEASE_AUDIT, UNKNOWN_ACTION_AUDIT];
/** The second window, reached only by the cursor the first page returns. */
const SECOND_PAGE = [
  {
    id: "0a0b0c0d-0000-4000-8000-0000000000a6",
    source: "governance",
    ...ALICE,
    via: "internal",
    action: "project.main_frozen",
    target_ref: `project:${PROJECT.id}`,
    project_id: PROJECT.id,
    organization_id: null,
    correlation_id: "corr-freeze",
    before_summary: { main_frozen: false },
    after_summary: { main_frozen: true },
    occurred_at: "2026-09-10T08:05:00Z",
  },
];

const DENIED = {
  code: "PROJECT_NOT_FOUND",
  message: "project not found",
  request_id: "e2e-req",
  retryable: false,
};

const UNAUTHENTICATED = {
  code: "UNAUTHENTICATED",
  message: "authentication required",
  request_id: "e2e-req",
  retryable: false,
};

/**
 * Mutable mock state. `feed` is what the activity endpoint answers; the
 * scenario object lets a test flip one thing (a denied read, a malformed
 * row) and reload.
 */
const scenario = {
  feed: "both", // "both" | "denied" | "malformed"
  /** When set, the feed answers nothing for that source — a project whose
   *  filtered history is genuinely empty. */
  emptySource: null, // null | "governance" | "research"
  requests: [],
};

const CURSOR_SECOND = "Y3Vyc29yLXNlY29uZA";

function activityPayload(url) {
  const source = url.searchParams.get("source");
  const cursor = url.searchParams.get("cursor");
  if (cursor !== null) {
    // The only cursor the mock issues is the first page's.
    if (cursor !== CURSOR_SECOND) {
      return { status: 404, body: DENIED };
    }
    const rest = SECOND_PAGE.filter((e) => source === null || e.source === source);
    return { status: 200, body: { entries: rest, next_cursor: null } };
  }
  // The empty-history scenario is per SOURCE: a null emptySource is "not
  // armed", so it can never match the unfiltered read's absent parameter.
  if (scenario.emptySource !== null && source === scenario.emptySource) {
    return { status: 200, body: { entries: [], next_cursor: null } };
  }
  const rows = FIRST_PAGE.filter((e) => source === null || e.source === source);
  return { status: 200, body: { entries: rows, next_cursor: CURSOR_SECOND } };
}

function installApiMock(page) {
  return page.route(`${API}/**`, async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    const pathname = url.pathname;

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      return route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify(UNAUTHENTICATED) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PROJECT) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/membership`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(MEMBERSHIP) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/activity`) {
      scenario.requests.push(url.pathname + url.search);
      if (scenario.feed === "denied") {
        return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(DENIED) });
      }
      if (scenario.feed === "malformed") {
        // A row without its source: the client must refuse it rather than
        // render it as whichever registry the code falls into.
        const { source, ...noSource } = FIRST_PAGE[0];
        if (source !== "research") throw new Error("fixture drift");
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ entries: [noSource], next_cursor: null }),
        });
      }
      const answer = activityPayload(url);
      return route.fulfill({
        status: answer.status,
        contentType: "application/json",
        body: JSON.stringify(answer.body),
      });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(DENIED) });
  });
}

/* ---------- B. Helpers ---------- */

const ACTIVITY_URL = `${BASE}/projects/${PROJECT.id}/activity`;

async function rows(page) {
  return page.locator("[data-activity-row]").count();
}

/** One row's text content, for the assertions that read several fields. */
async function rowText(page, id) {
  return (await page.locator(`[data-activity-row="${id}"]`).textContent()) ?? "";
}

/* ---------- C. The checklist ---------- */

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 300)));
await installApiMock(page);

/* 1. The timeline renders both registries, newest first. */

await page.goto(ACTIVITY_URL, { waitUntil: "load" });
await page.waitForSelector("[data-activity-list]");
if ((await rows(page)) !== FIRST_PAGE.length) {
  fail("timeline: row count", `got ${await rows(page)}, want ${FIRST_PAGE.length}`);
} else {
  ok(`timeline: renders all ${FIRST_PAGE.length} rows of the first window`);
}
check(
  "timeline: the unfiltered read sends NO source parameter",
  scenario.requests.includes(`/api/v1/projects/${PROJECT.id}/activity?limit=25`),
  `requests: ${JSON.stringify(scenario.requests)}`,
);

// The rendered order is the API's order, not a re-sorted one: the ids must
// come back in the sequence the feed served.
const renderedOrder = await page.locator("[data-activity-row]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-activity-row")),
);
check(
  "timeline: the rows keep the feed's newest-first order",
  renderedOrder.join(",") === FIRST_PAGE.map((e) => e.id).join(","),
  `rendered ${renderedOrder.join(",")}`,
);

// Every row says which registry it came from.
const sources = await page.locator("[data-activity-row]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-activity-row-source")),
);
check(
  "timeline: every row carries its registry",
  sources.length === FIRST_PAGE.length && sources.every((s) => s === "governance" || s === "research"),
  `sources: ${sources.join(",")}`,
);

/* 2. actor/via links. */

const actorLink = page.locator(`[data-activity-row="${ABORT_AUDIT.id}"] [data-activity-actor]`);
check(
  "actor: the row links the actor to their profile",
  (await actorLink.getAttribute("href")) === `/users/${ALICE.actor_id}` &&
    (await actorLink.textContent()) === "Alice Guo",
  `href=${await actorLink.getAttribute("href")}`,
);
const via = await page.locator(`[data-activity-row="${ABORT_AUDIT.id}"] [data-activity-via]`).textContent();
check("via: the governance row names its channel in words", via === "Web session", `got ${via}`);
const viaResearch = await page.locator(`[data-activity-row="${ABORT_EVENT.id}"] [data-activity-via]`).textContent();
check("via: the research row names its write path in words", viaResearch === "Claude Code", `got ${viaResearch}`);
const correlation = await page
  .locator(`[data-activity-row="${ABORT_AUDIT.id}"] [data-activity-correlation]`)
  .textContent();
check("row: the correlation id is rendered", correlation === "corr-abort", `got ${correlation}`);
// A row with no actor (a platform event) says so and links nowhere.
const unknownActor = page.locator(`[data-activity-row="${REOPEN_EVENT.id}"] [data-activity-actor]`);
check(
  "actor: a row with no actor renders the stated absence, not a broken link",
  (await unknownActor.textContent()) === "Unknown actor" &&
    (await page.locator(`[data-activity-row="${REOPEN_EVENT.id}"] a[data-activity-actor]`).count()) === 0,
  `text=${await unknownActor.textContent()}`,
);

/* 3. abort / reopen / release display. */

const abortGovTitle = await page.locator(`[data-activity-row="${ABORT_AUDIT.id}"] [data-activity-title]`).textContent();
check("abort: the governance row is titled by the audit vocabulary", abortGovTitle === "Object aborted", `got ${abortGovTitle}`);
const abortGovText = await rowText(page, ABORT_AUDIT.id);
check(
  "abort: the governance row shows the reason code, the explanation and the lifecycle move",
  abortGovText.includes("contaminated") &&
    abortGovText.includes("the sample was contaminated in transit") &&
    abortGovText.includes("active → aborted"),
  `row text: ${abortGovText}`,
);
check(
  "abort: the governance row's target renders as text (objects have no page)",
  (await page.locator(`[data-activity-row="${ABORT_AUDIT.id}"] a[data-activity-target]`).count()) === 0 &&
    (await page.locator(`[data-activity-row="${ABORT_AUDIT.id}"] [data-activity-target]`).textContent()) ===
      "Object 0a0b0c0d",
);
const abortEventText = await rowText(page, ABORT_EVENT.id);
check(
  "abort: the event half shows the version pair it carries",
  abortEventText.includes("contaminated") && abortEventText.includes("4"),
  `row text: ${abortEventText}`,
);
check(
  "abort: the event half claims NO explanation (the platform does not put the free text there)",
  !abortEventText.includes("the sample was contaminated in transit") &&
    (await page.locator(`[data-activity-row="${ABORT_EVENT.id}"] [data-activity-detail="Explanation"]`).count()) === 0,
  `row text: ${abortEventText}`,
);
check(
  "abort: the event half shows the visibility the event was recorded with",
  (await page.locator(`[data-activity-row="${ABORT_EVENT.id}"] [data-activity-visibility]`).textContent()) === "private",
);

const releaseTitle = await page.locator(`[data-activity-row="${RELEASE_AUDIT.id}"] [data-activity-title]`).textContent();
check("release: the row is titled by the audit vocabulary", releaseTitle === "Release created", `got ${releaseTitle}`);
const releaseText = await rowText(page, RELEASE_AUDIT.id);
check(
  "release: the version and the short manifest hash render",
  releaseText.includes("v1.2.0") && releaseText.includes("9f2c1ab34d5e") && !releaseText.includes("9f2c1ab34d5e6f70"),
  `row text: ${releaseText}`,
);
const releaseLink = page.locator(`[data-activity-row="${RELEASE_AUDIT.id}"] a[data-activity-target]`);
check(
  "release: the row links the release page",
  (await releaseLink.getAttribute("href")) === `/projects/${PROJECT.id}/releases/${RELEASE_ID}`,
  `href=${await releaseLink.getAttribute("href")}`,
);

const reopenTitle = await page.locator(`[data-activity-row="${REOPEN_EVENT.id}"] [data-activity-title]`).textContent();
check("reopen: the event is titled by the event registry", reopenTitle === "Object reopened", `got ${reopenTitle}`);
check(
  "reopen: the row carries its own family marker",
  (await page.locator(`[data-activity-row="${REOPEN_EVENT.id}"]`).getAttribute("data-activity-row-family")) === "reopen",
);
// The reopen row shows the reason it carries, and no State line: a research
// row has no before/after summaries.
check(
  "reopen: the reason code renders while the explanation line is absent",
  (await rowText(page, REOPEN_EVENT.id)).includes("wrong_sample") &&
    (await page.locator(`[data-activity-row="${REOPEN_EVENT.id}"] [data-activity-detail="Explanation"]`).count()) === 0,
);

// An action no vocabulary knows renders raw — the platform's own word.
const unknownTitle = await page
  .locator(`[data-activity-row="${UNKNOWN_ACTION_AUDIT.id}"] [data-activity-title]`)
  .textContent();
check(
  "unknown action: the dotted name renders raw rather than a coined label",
  unknownTitle === "something.new_thing",
  `got ${unknownTitle}`,
);

/* 4. filter governance/research events. */

scenario.requests.length = 0;
await page.click('[data-activity-filter="governance"]');
await page.waitForURL((u) => u.search === "?source=governance");
await page.waitForSelector("[data-activity-list]");
check(
  "filter: the chip writes the filter into the URL",
  page.url().endsWith("/activity?source=governance"),
  page.url(),
);
check(
  "filter: the read carries source=governance",
  scenario.requests.includes(`/api/v1/projects/${PROJECT.id}/activity?limit=25&source=governance`),
  `requests: ${JSON.stringify(scenario.requests)}`,
);
const governedIDs = await page.locator("[data-activity-row]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-activity-row")),
);
const wantGoverned = FIRST_PAGE.filter((e) => e.source === "governance").map((e) => e.id);
check(
  "filter: only governance rows render",
  governedIDs.join(",") === wantGoverned.join(",") &&
    (await page.locator('[data-activity-row-source="research"]').count()) === 0,
  `rendered ${governedIDs.join(",")}`,
);
check(
  "filter: the selected chip is marked for assistive tech",
  (await page.getAttribute('[data-activity-filter="governance"]', "aria-current")) === "true" &&
    (await page.getAttribute('[data-activity-filter="all"]', "data-activity-filter-selected")) === "false",
);

scenario.requests.length = 0;
await page.click('[data-activity-filter="research"]');
await page.waitForURL((u) => u.search === "?source=research");
await page.waitForSelector("[data-activity-list]");
check(
  "filter: the read carries source=research",
  scenario.requests.includes(`/api/v1/projects/${PROJECT.id}/activity?limit=25&source=research`),
  `requests: ${JSON.stringify(scenario.requests)}`,
);
const researchIDs = await page.locator("[data-activity-row]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-activity-row")),
);
const wantResearch = FIRST_PAGE.filter((e) => e.source === "research").map((e) => e.id);
check(
  "filter: only research rows render",
  researchIDs.join(",") === wantResearch.join(",") &&
    (await page.locator('[data-activity-row-source="governance"]').count()) === 0,
  `rendered ${researchIDs.join(",")}`,
);
check(
  "filter: the three answers add up (governance + research = all)",
  governedIDs.length + researchIDs.length === FIRST_PAGE.length,
  `${governedIDs.length} + ${researchIDs.length} != ${FIRST_PAGE.length}`,
);

// Back to All: the parameter is REMOVED, not set to "all" (the API has no
// such value, and the unfiltered read is the absence of the parameter).
scenario.requests.length = 0;
await page.click('[data-activity-filter="all"]');
await page.waitForURL((u) => !u.search.includes("source="));
await page.waitForSelector("[data-activity-list]");
check(
  "filter: All removes the parameter and reads both registries",
  scenario.requests.includes(`/api/v1/projects/${PROJECT.id}/activity?limit=25`) &&
    !scenario.requests.some((r) => r.includes("source=")),
  `requests: ${JSON.stringify(scenario.requests)}`,
);
check("filter: the unfiltered rows are back", (await rows(page)) === FIRST_PAGE.length, `got ${await rows(page)}`);

// A reload keeps the filter: the URL is the state.
await page.click('[data-activity-filter="research"]');
await page.waitForURL((u) => u.search === "?source=research");
await page.reload({ waitUntil: "load" });
await page.waitForSelector("[data-activity-list]");
check(
  "filter: a reload of ?source=research keeps the filter and its rows",
  (await page.locator('[data-activity-row-source="governance"]').count()) === 0 &&
    (await rows(page)) === wantResearch.length,
  `rows ${await rows(page)}, url ${page.url()}`,
);

/* 5. pagination: the cursor walks the feed and appends. */

scenario.requests.length = 0;
await page.goto(ACTIVITY_URL, { waitUntil: "load" });
await page.waitForSelector("[data-activity-list]");
await page.waitForSelector("[data-activity-load-more]");
await page.click("[data-activity-load-more]");
await page.waitForSelector(`[data-activity-row="${SECOND_PAGE[0].id}"]`);
check(
  "pagination: Load more sends the API's cursor back",
  scenario.requests.includes(`/api/v1/projects/${PROJECT.id}/activity?limit=25&cursor=${encodeURIComponent(CURSOR_SECOND)}`),
  `requests: ${JSON.stringify(scenario.requests)}`,
);
const afterMore = await page.locator("[data-activity-row]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-activity-row")),
);
check(
  "pagination: the second window is APPENDED, not a replacement",
  afterMore.join(",") === [...FIRST_PAGE.map((e) => e.id), SECOND_PAGE[0].id].join(","),
  `rendered ${afterMore.join(",")}`,
);
check(
  "pagination: the last window ends the feed (no further cursor)",
  (await page.locator("[data-activity-load-more]").count()) === 0,
);

/* 6. The failure shapes. */

// A refused ?source= value: the page says so and requests nothing — the API
// refuses a filter it does not have, and the page must not widen the read.
scenario.requests.length = 0;
await page.goto(`${ACTIVITY_URL}?source=audit`, { waitUntil: "load" });
// The wait is capped and its absence is NOT fatal: a page that widened a
// refused filter into "read everything" must be reported as a failed check
// (the two below read it), not crash the run before it says which one.
await page.waitForSelector("[data-activity-filter-refused]", { timeout: 5000 }).catch(() => {});
check(
  "refusal: an unknown ?source= renders the page's own refusal, with no feed request",
  scenario.requests.length === 0,
  `requests: ${JSON.stringify(scenario.requests)}`,
);
// The counts guard the reads: on a page that rendered no refusal at all
// (the mutated case), an unguarded attribute read would time out and end
// the run before it says WHICH assertion the page failed.
const refusedEl = page.locator("[data-activity-filter-refused]");
const showAllLink = page.locator("[data-activity-filter-all]");
check(
  "refusal: the refused value is named, and the way out is a link to the unfiltered feed",
  (await refusedEl.count()) === 1 &&
    (await refusedEl.getAttribute("data-activity-filter-refused")) === "audit" &&
    (await showAllLink.count()) === 1 &&
    (await showAllLink.getAttribute("href")) === `/projects/${PROJECT.id}/activity`,
  `refused=${(await refusedEl.count()) === 1 ? await refusedEl.getAttribute("data-activity-filter-refused") : "(no refusal state)"}`,
);
check(
  "refusal: no row is rendered from the refused filter",
  (await page.locator("[data-activity-row]").count()) === 0,
);
if ((await showAllLink.count()) === 1) {
  await page.click("[data-activity-filter-all]");
  await page.waitForSelector("[data-activity-list]");
  check("refusal: the way out leads to the unfiltered timeline", (await rows(page)) === FIRST_PAGE.length);
} else {
  fail("refusal: the way out leads to the unfiltered timeline", "there is no 'Show all activity' link to follow");
}

// A row missing its source: the client refuses it (the rendering rules
// branch on the source), so the page shows an error instead of a row that
// silently claimed one of the two registries.
scenario.feed = "malformed";
await page.goto(ACTIVITY_URL, { waitUntil: "load" });
await page.waitForSelector("[data-activity-error]");
check(
  "malformed row: the feed is refused, not half-rendered",
  (await page.locator("[data-activity-row]").count()) === 0 &&
    (await page.locator("[data-activity-empty]").count()) === 0,
);

// A denied read: the stable message, never an empty timeline.
scenario.feed = "denied";
await page.goto(ACTIVITY_URL, { waitUntil: "load" });
await page.waitForSelector("[data-activity-error]");
check(
  "denied read: the page shows the stable message, not an empty timeline",
  (await page.textContent("[data-activity-error]")).includes("This project is not visible to you.") &&
    (await page.locator("[data-activity-empty]").count()) === 0,
  await page.textContent("[data-activity-error]"),
);

// A filter that answers nothing: the empty state names the filter, so an
// empty page cannot be read as "this project has no history".
scenario.feed = "both";
scenario.emptySource = "research";
await page.goto(`${ACTIVITY_URL}?source=research`, { waitUntil: "load" });
await page.waitForSelector("[data-activity-empty]");
check(
  "empty: the empty state names the filter it is empty for",
  (await page.textContent("[data-activity-empty]")).includes("research events"),
  await page.textContent("[data-activity-empty]"),
);

await browser.close();

if (fails > 0) {
  console.error(`e2e-activity: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("e2e-activity: all checks passed");
