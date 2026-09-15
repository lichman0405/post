/**
 * PR Research State Diff UI e2e (T0408 required test "pr ui e2e"): a real
 * Chromium drives the BUILT web app (next start, production layer)
 * through the pull request detail page. The Go API is mocked at the
 * network layer with the exact wire shapes cmd/api/pullrequestshttp (the
 * PR, the checks report and the three-way diff), cmd/api/reviewhttp (the
 * review list and the submission) and internal/rsg/diff produce — the Go
 * handler suites lock those shapes against the real guard, so the halves
 * cover the real contract without the browser test depending on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - 默认首屏非 raw diff: the page opens on the Summary of the Research
 *     State Diff (docs/06 §6 — the raw Git diff lives on a secondary
 *     tab). Six tabs exist in the documented order, Summary is the
 *     selected one on the first frame, and the raw file panel is not even
 *     in the DOM until the reader asks for it; asking for it renders both
 *     sides' recorded refs with the raw patch links of the Files channel.
 *   - 重要风险明显: the risk banner sits ABOVE the tab bar (so no tab can
 *     hide it), leads with the machine's own blocking failures at their
 *     own severity, and is computed — with a clean proposal it disappears
 *     entirely rather than showing a green "all clear".
 *   - The tabs are the required ones — Summary / Scientific changes /
 *     Knowledge changes / Evidence / Checks / Raw Files — and each holds
 *     its own bucket: knowledge objects and knowledge relations apart
 *     from evidence objects and the docs/10 §4 evidence relations.
 *   - Review controls: the two dimensions (scientific, integrity) are
 *     never flattened; the form submits nothing until the human chooses a
 *     decision; the POST carries exactly the chosen decision and the
 *     session's CSRF token; the page shows the recorded decision; a
 *     refused submission (403) renders the API's own message and changes
 *     nothing; and a decision recorded on an OLDER head is shown as such
 *     and never counted as the current head's state or risk.
 *
 * Usage: node pulls-e2e.mjs <base-url> [apps-web-dir]
 */
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31120";
const APPS_WEB = process.argv[3] ?? path.resolve(import.meta.dirname, "../../apps/web");

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
/** Assert a boolean with one line of evidence either way. */
const check = (label, condition, detail) => {
  if (condition) ok(label);
  else fail(label, detail);
};

/* ---------- A. Fixtures (the exact wire shapes of cmd/api) ---------- */

const API = "http://api.e2e.test";

const PROJECT = {
  id: "11111111-2222-4333-8444-555555555555",
  organization_id: null,
  program_id: null,
  slug: "alloy-lab",
  name: "Alloy Lab",
  purpose: "Design high-entropy alloys with reproducible synthesis routes.",
  activity_status: "planning",
  visibility: "public",
  main_frozen: true,
  git_repository_external_id: null,
  provision_status: "ready",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-10T08:00:00Z",
};

const OWNER_MEMBERSHIP = {
  project_id: PROJECT.id,
  user_id: "00000000-0000-4000-8000-000000000001",
  role: "owner",
  created_at: "2026-09-10T08:00:00Z",
};

const CSRF_TOKEN = "csrf-e2e-token";

const SESSION = {
  user: {
    id: "00000000-0000-4000-8000-000000000001",
    handle: "alice",
    email: "alice@example.com",
    display_name: "Alice Guo",
  },
  csrf_token: CSRF_TOKEN,
};

const NOT_FOUND = {
  code: "PROJECT_NOT_FOUND",
  message: "project not found",
  request_id: "e2e-req",
  retryable: false,
};

// The four states, deliberately distinct in their first eight characters:
// every assertion below reads the prefix the page renders, so an
// assertion that could be satisfied by ANY of them would measure nothing.
const BASE_STATE = "bbbbbbbb-0000-4000-8000-000000000001";
const HEAD_STATE = "dddddddd-0000-4000-8000-000000000002";
const TARGET_STATE = "cccccccc-0000-4000-8000-000000000003";
const OLDER_STATE = "eeeeeeee-0000-4000-8000-000000000004";

const PR_NUMBER = 7;
const PR = {
  id: "pr-00000000-0000-4000-8000-000000000007",
  number: PR_NUMBER,
  title: "Propose the single-phase claim",
  body: "Adds the claim, the evidence assertion behind it and the supports edge.",
  state: "open",
  source_branch_id: "branch-proposal",
  target_branch_id: "branch-main",
  base_state_id: BASE_STATE,
  proposed_state_id: HEAD_STATE,
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-14T09:00:00Z",
};

const PR_PATH = `/api/v1/projects/${PROJECT.id}/pull-requests/${PR_NUMBER}`;
const REVIEWS_PATH = `${PR_PATH}/reviews`;

/** The git refs of the two sides (internal/rsg/diff FileDiffRef). */
const BASE_SHA = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0";
const SOURCE_SHA = "a11a11a11a11a11a11a11a11a11a11a11a11a11";
const TARGET_SHA = "c22c22c22c22c22c22c22c22c22c22c22c22c22";

/** One scientific object version (internal/rsg/manifest ObjectVersion). */
function objectVersion(id, objectId, objectType, versionNo, stateId, title, visibilityPolicyId = null) {
  return {
    id,
    object_id: objectId,
    object_type: objectType,
    version_no: versionNo,
    state_id: stateId,
    branch_id: null,
    schema_ref: { id: `sch-${objectType}`, version: "1" },
    title,
    lifecycle_state: "active",
    payload: { note: title },
    visibility_policy_id: visibilityPolicyId,
    integrity_hash: `hash-${id}`,
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-14T09:00:00Z",
  };
}

/** One relation version (internal/rsg/manifest RelationVersion). */
function relationVersion(id, relationId, relationType) {
  return {
    id,
    relation_id: relationId,
    version_no: 1,
    state_id: HEAD_STATE,
    relation_type: relationType,
    source_object_version_id: "ver-evidence-1",
    target_object_version_id: "ver-claim-1",
    payload: {},
    integrity_hash: `hash-${id}`,
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-14T09:00:00Z",
  };
}

/**
 * The machine integrity report (internal/rsg/integrity Report): blocked,
 * with one failing blocking check (the schema change) and one failing
 * warning check.
 */
const BLOCKED_REPORT = {
  kind: "integrity",
  project_id: PROJECT.id,
  pr_number: PR_NUMBER,
  base_state_id: BASE_STATE,
  proposed_state_id: HEAD_STATE,
  verdict: "blocked",
  results: [
    {
      dimension: "schema",
      check: "schema_change_is_backward_compatible",
      severity: "blocking",
      passed: false,
      subject: "claim",
      detail: "claim/v1 requires the new field `phase_fraction`, which no stored payload carries",
      why: "a schema change that old payloads cannot satisfy must be migrated, not merged",
    },
    {
      dimension: "rights",
      check: "rights_pin_inherits_default",
      severity: "warning",
      passed: false,
      subject: "evidence_assertion",
      detail: "no visibility policy pinned",
      why: "the version carries no pin, so the project default applies",
    },
    {
      dimension: "provenance",
      check: "provenance_edge_present",
      severity: "blocking",
      passed: true,
      subject: "evidence_assertion",
      why: "the evidence assertion has a provenance edge",
    },
  ],
  explanation: "the proposal is blocked by one schema check",
  computed_at: "2026-09-15T10:05:00Z",
};

/** A clean report: every check passes (used to prove the banner is computed). */
const CLEAN_REPORT = {
  ...BLOCKED_REPORT,
  verdict: "pass",
  results: BLOCKED_REPORT.results.map((result) => ({ ...result, passed: true })),
  explanation: "the proposal passes machine integrity",
};

/** The three-way Research State Diff (internal/rsg/diff Document). */
const DIFF = {
  format_version: "v1",
  project_id: PROJECT.id,
  base: { id: BASE_STATE, git_ref: BASE_SHA },
  source: { id: HEAD_STATE, git_ref: SOURCE_SHA },
  target: { id: TARGET_STATE, git_ref: TARGET_SHA },
  object_changes: [
    {
      object_id: "obj-claim-1",
      object_type: "claim",
      kind: "updated",
      base_version: objectVersion("ver-claim-base", "obj-claim-1", "claim", 1, BASE_STATE, "The alloy is single phase"),
      source_version: objectVersion("ver-claim-1", "obj-claim-1", "claim", 2, HEAD_STATE, "The alloy is single phase"),
      target_version: null,
      target_moved: false,
      changed_fields: ["schema_ref"],
      target_changed_fields: [],
      new_versions: [],
    },
    {
      object_id: "obj-evidence-1",
      object_type: "evidence_assertion",
      kind: "created",
      base_version: null,
      source_version: objectVersion("ver-evidence-1", "obj-evidence-1", "evidence_assertion", 1, HEAD_STATE, "Thermal run 7"),
      target_version: null,
      target_moved: false,
      changed_fields: [],
      target_changed_fields: [],
      new_versions: [],
    },
    {
      object_id: "obj-protocol-1",
      object_type: "protocol",
      kind: "updated",
      base_version: objectVersion("ver-protocol-base", "obj-protocol-1", "protocol", 1, BASE_STATE, "Synthesis protocol"),
      source_version: objectVersion("ver-protocol-1", "obj-protocol-1", "protocol", 2, HEAD_STATE, "Synthesis protocol"),
      target_version: null,
      target_moved: true,
      changed_fields: ["payload"],
      target_changed_fields: ["payload"],
      new_versions: [],
    },
    {
      object_id: "obj-old-claim",
      object_type: "claim",
      kind: "aborted",
      base_version: objectVersion("ver-old-base", "obj-old-claim", "claim", 1, BASE_STATE, "Older phase claim"),
      source_version: objectVersion("ver-old-1", "obj-old-claim", "claim", 2, HEAD_STATE, "Older phase claim"),
      target_version: null,
      target_moved: false,
      changed_fields: [],
      target_changed_fields: [],
      new_versions: [],
    },
  ],
  relation_changes: [
    {
      relation_id: "rel-supports-1",
      kind: "created",
      base_version: null,
      source_version: relationVersion("ver-rel-supports", "rel-supports-1", "supports"),
      target_version: null,
      target_moved: false,
      changed_fields: [],
      target_changed_fields: [],
      new_versions: [],
    },
    {
      relation_id: "rel-addresses-1",
      kind: "created",
      base_version: null,
      source_version: relationVersion("ver-rel-addresses", "rel-addresses-1", "addresses_question"),
      target_version: null,
      target_moved: false,
      changed_fields: [],
      target_changed_fields: [],
      new_versions: [],
    },
  ],
  file_diff_refs: [
    { kind: "source", base_git_ref: BASE_SHA, head_git_ref: SOURCE_SHA },
    { kind: "target", base_git_ref: BASE_SHA, head_git_ref: TARGET_SHA },
  ],
  summary: {
    objects_created: 1,
    objects_updated: 2,
    objects_aborted: 1,
    objects_reopened: 0,
    relations_created: 2,
    relations_updated: 0,
  },
};

/** The clean diff: one structural object change, nothing else. */
const CLEAN_DIFF = {
  ...DIFF,
  object_changes: [
    {
      object_id: "obj-protocol-1",
      object_type: "protocol",
      kind: "updated",
      base_version: objectVersion("ver-protocol-base", "obj-protocol-1", "protocol", 1, BASE_STATE, "Synthesis protocol"),
      source_version: objectVersion("ver-protocol-1", "obj-protocol-1", "protocol", 2, HEAD_STATE, "Synthesis protocol"),
      target_version: null,
      target_moved: false,
      changed_fields: ["payload"],
      target_changed_fields: [],
      new_versions: [],
    },
  ],
  relation_changes: [],
  summary: {
    objects_created: 0,
    objects_updated: 1,
    objects_aborted: 0,
    objects_reopened: 0,
    relations_created: 0,
    relations_updated: 0,
  },
};

/** One recorded review (cmd/api/reviewhttp reviewPayload). */
function wireReview(id, kind, decision, reviewedStateId, body, createdAt = "2026-09-14T12:00:00Z") {
  return {
    id,
    pull_request_id: PR.id,
    reviewer_id: "00000000-0000-4000-8000-000000000002",
    kind,
    decision,
    reviewed_state_id: reviewedStateId,
    responsibility: "scientific-lead",
    body,
    created_at: createdAt,
  };
}

/** The mutable fixtures the route handler serves. */
const state = {
  report: BLOCKED_REPORT,
  diff: DIFF,
  reviews: [
    // A decision on an OLDER head: recorded, shown as such, and never
    // counted as the current head's state (or as a risk about it).
    wireReview("rev-old-1", "scientific", "changes_requested", OLDER_STATE, "the ablation is missing"),
  ],
  refuseReviews: false,
};

/* ---------- B. The network-layer mock ---------- */

const apiTraffic = [];
const postedReviews = [];

const qs = (url) => Object.fromEntries(new URL(url).searchParams.entries());

function installApiMock(page) {
  // Context-level routing, not page-level: any future popup/tab the page
  // opens is intercepted too, so no request can escape to real DNS.
  return page.context().route(`${API}/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const method = request.method();
    const pathname = url.pathname;
    apiTraffic.push({
      method,
      pathname,
      query: qs(request.url()),
      csrf: request.headers()["x-csrf-token"] ?? null,
    });

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(SESSION),
      });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PROJECT) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/membership`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(OWNER_MEMBERSHIP),
      });
    }
    if (method === "GET" && pathname === PR_PATH) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PR) });
    }
    if (method === "GET" && pathname === `${PR_PATH}/checks`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(state.report),
      });
    }
    if (method === "GET" && pathname === `${PR_PATH}/diff`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(state.diff),
      });
    }
    if (method === "GET" && pathname === REVIEWS_PATH) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(state.reviews),
      });
    }
    if (method === "POST" && pathname === REVIEWS_PATH) {
      const body = request.postDataJSON();
      postedReviews.push(body);
      if (state.refuseReviews) {
        return route.fulfill({
          status: 403,
          contentType: "application/json",
          body: JSON.stringify({
            code: "AUTH_FORBIDDEN",
            message: "you are not permitted to submit a review in this project",
            request_id: "e2e-req",
            retryable: false,
          }),
        });
      }
      // The API derives the reviewer and the responsibility label; the
      // wire answer is the recorded review.
      const recorded = wireReview(
        `rev-${state.reviews.length + 1}`,
        body.kind,
        body.decision,
        PR.proposed_state_id,
        body.body ?? "",
      );
      state.reviews = [...state.reviews, recorded];
      return route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify(recorded),
      });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- C. Browser scenarios ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

const PR_URL = `${BASE}/projects/${PROJECT.id}/pulls/${PR_NUMBER}`;

/** Load the PR page and wait for the tab bar. */
async function openPullPage() {
  await page.goto(PR_URL, { waitUntil: "load" });
  await page.waitForSelector('[data-project-shell="alloy-lab"]');
  await page.waitForSelector("[data-pull-tabs]");
}

/* ---------- 1. 默认首屏非 raw diff ---------- */

await openPullPage();

const tabNames = await page
  .locator("[data-pull-tab]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-pull-tab")));
check(
  "tabs: the six required views, in the docs/06 §6 order",
  JSON.stringify(tabNames) ===
    JSON.stringify(["summary", "scientific", "knowledge", "evidence", "checks", "raw"]),
  `got ${JSON.stringify(tabNames)}`,
);

const tabLabels = await page.locator("[data-pull-tab]").allTextContents();
const wantedLabels = [
  "Summary",
  "Scientific changes",
  "Knowledge changes",
  "Evidence",
  "Checks",
  "Raw Files",
];
check(
  "tabs: labeled Summary / Scientific changes / Knowledge changes / Evidence / Checks / Raw Files",
  wantedLabels.every((label, i) => (tabLabels[i] ?? "").startsWith(label)),
  `got ${JSON.stringify(tabLabels)}`,
);

const selected = await page
  .locator('[data-pull-tab][aria-selected="true"]')
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-pull-tab")));
check(
  "first screen: Summary is the selected tab",
  JSON.stringify(selected) === JSON.stringify(["summary"]),
  `got ${JSON.stringify(selected)}`,
);

const rawPanels = await page.locator('[data-pull-panel="raw"], [data-raw-files], [data-raw-patch]').count();
check(
  "first screen: the raw file view is NOT in the DOM (默认首屏非 raw diff)",
  rawPanels === 0,
  `found ${rawPanels} raw elements`,
);

const summaryPanel = page.locator('[data-pull-panel="summary"]');
await summaryPanel.waitFor();
const totalsText = await summaryPanel.locator("[data-pull-totals]").textContent();
const statesText = await summaryPanel.locator("[data-pull-states]").textContent();
check(
  "first screen: the semantic diff is what a reader lands on (counts, states, machine verdict)",
  totalsText.includes("Objects created") &&
    totalsText.includes("Objects aborted") &&
    statesText.includes(BASE_STATE.slice(0, 8)) &&
    statesText.includes(HEAD_STATE.slice(0, 8)) &&
    statesText.includes(TARGET_STATE.slice(0, 8)) &&
    (await summaryPanel.locator('[data-pull-integrity-verdict="blocked"]').count()) === 1,
  `totals: ${totalsText} / states: ${statesText}`,
);
const counts = await summaryPanel
  .locator("[data-count-label]")
  .evaluateAll((els) =>
    els.map((el) => [el.getAttribute("data-count-label"), el.querySelector(".pull-count-value").textContent]),
  );
check(
  "first screen: the counts are the diff's own summary",
  JSON.stringify(counts) ===
    JSON.stringify([
      ["Objects created", "1"],
      ["Objects updated", "2"],
      ["Objects aborted", "1"],
      ["Objects reopened", "0"],
      ["Relations created", "2"],
      ["Relations updated", "0"],
    ]),
  `got ${JSON.stringify(counts)}`,
);

/* ---------- 2. 重要风险明显 ---------- */

// Read the banner without auto-waiting: a missing banner must FAIL
// here with a line of evidence, not abort the run on a timeout.
const banner = page.locator("[data-pull-risks]");
const bannerCount = await banner.count();
const riskBlocking = bannerCount === 1 ? await banner.getAttribute("data-risk-blocking") : null;
const riskCount = bannerCount === 1 ? await banner.getAttribute("data-risk-count") : null;
check(
  "risks: the banner shows a blocking risk",
  bannerCount === 1 && Number(riskBlocking) >= 1,
  `banners=${bannerCount} data-risk-blocking=${riskBlocking}, data-risk-count=${riskCount}`,
);

// Above the tab bar: no tab can hide the risks.
const aboveTabs = await page.evaluate(() => {
  const risks = document.querySelector("[data-pull-risks]");
  const tabs = document.querySelector("[data-pull-tabs]");
  if (risks === null || tabs === null) return "missing";
  const position = risks.compareDocumentPosition(tabs);
  return (position & Node.DOCUMENT_POSITION_FOLLOWING) !== 0 ? "above" : "below";
});
check("risks: the banner sits above the tab bar, so no tab hides it", aboveTabs === "above", aboveTabs);

const blockingRisks = page.locator('[data-pull-risk="RISK_BLOCKING_CHECK"]');
const blockingCount = await blockingRisks.count();
const blockingRisk = blockingRisks.first();
const blockingText = blockingCount === 0 ? "" : await blockingRisk.textContent();
const blockingSeverity =
  blockingCount === 0 ? null : await blockingRisk.getAttribute("data-risk-severity");
check(
  "risks: the machine's blocking check is rendered with its own severity and reason",
  blockingCount === 1 &&
    blockingText.includes("blocking") &&
    blockingText.includes("schema_change_is_backward_compatible") &&
    blockingText.includes("no stored payload carries") &&
    blockingSeverity === "blocking",
  `count=${blockingCount} severity=${blockingSeverity} text=${blockingText}`,
);
const blockingBox = blockingCount === 1 ? await blockingRisk.boundingBox() : null;
check(
  "risks: the blocking risk is visible, not a collapsed row",
  blockingBox !== null && blockingBox.width > 0 && blockingBox.height > 0,
  JSON.stringify(blockingBox),
);
const warningRisk = page.locator('[data-pull-risk="RISK_WARNING_CHECK"]').first();
check(
  "risks: a failed warning check never escalates to blocking",
  (await warningRisk.getAttribute("data-risk-severity")) === "warning",
);

// A decision recorded on an OLDER head is a fact, not a risk about the
// head proposed now.
check(
  "risks: a changes_requested decision on an older head is not a risk",
  (await page.locator('[data-pull-risk="RISK_CHANGES_REQUESTED"]').count()) === 0,
);

// The target branch moved the same object: an important, non-blocking risk.
const movedRisk = page.locator('[data-pull-risk="RISK_TARGET_MOVED"]').first();
const movedText = await movedRisk.textContent();
check(
  "risks: the target branch having moved the same entry is flagged",
  movedText.includes("obj-protocol-1") && (await movedRisk.getAttribute("data-risk-severity")) === "warning",
  movedText,
);

// A risk leads to its evidence: the link switches to the tab that holds it.
await movedRisk.locator('[data-risk-tab="scientific"]').click();
await page.waitForSelector('[data-pull-panel="scientific"]');
check(
  "risks: opening a risk switches to the tab that holds its evidence",
  (await page.locator('[data-pull-tab="scientific"][aria-selected="true"]').count()) === 1,
);

/* ---------- 3. Each tab holds its own bucket ---------- */

const scientificObjects = await page
  .locator("[data-scientific-object]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-scientific-object")));
check(
  "scientific changes: every object change of the diff, created and aborted included",
  JSON.stringify(scientificObjects) ===
    JSON.stringify(["obj-claim-1", "obj-evidence-1", "obj-protocol-1", "obj-old-claim"]),
  `got ${JSON.stringify(scientificObjects)}`,
);
const kinds = await page
  .locator("[data-scientific-object]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-change-kind")));
check(
  "scientific changes: the change kinds render (created / updated / aborted)",
  JSON.stringify(kinds) === JSON.stringify(["updated", "created", "updated", "aborted"]),
  `got ${JSON.stringify(kinds)}`,
);
check(
  "scientific changes: the target-side move is marked on the row",
  (await page.locator('[data-scientific-object="obj-protocol-1"] [data-change-moved]').count()) === 1,
);

await page.locator('[data-pull-tab="knowledge"]').click();
await page.waitForSelector('[data-pull-panel="knowledge"]');
const knowledgeObjects = await page
  .locator("[data-knowledge-object]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-knowledge-object")));
const knowledgeRelations = await page
  .locator("[data-knowledge-relation]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-knowledge-relation")));
check(
  "knowledge changes: the knowledge objects and knowledge relations, and nothing else",
  JSON.stringify(knowledgeObjects) === JSON.stringify(["obj-claim-1", "obj-old-claim"]) &&
    JSON.stringify(knowledgeRelations) === JSON.stringify(["rel-addresses-1"]),
  `objects=${JSON.stringify(knowledgeObjects)} relations=${JSON.stringify(knowledgeRelations)}`,
);
check(
  "knowledge changes: the evidence bucket is not duplicated here",
  (await page.locator("[data-pull-panel='knowledge'] [data-evidence-object]").count()) === 0 &&
    (await page.locator("[data-pull-panel='knowledge'] [data-evidence-relation]").count()) === 0,
);

await page.locator('[data-pull-tab="evidence"]').click();
await page.waitForSelector('[data-pull-panel="evidence"]');
const evidenceObjects = await page
  .locator("[data-evidence-object]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-evidence-object")));
const evidenceRelations = await page
  .locator("[data-evidence-relation]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-evidence-relation")));
const evidenceTypes = await page
  .locator("[data-evidence-relation]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-relation-type")));
check(
  "evidence: the evidence assertion and the docs/10 §4 evidence relation",
  JSON.stringify(evidenceObjects) === JSON.stringify(["obj-evidence-1"]) &&
    JSON.stringify(evidenceRelations) === JSON.stringify(["rel-supports-1"]) &&
    JSON.stringify(evidenceTypes) === JSON.stringify(["supports"]),
  `objects=${JSON.stringify(evidenceObjects)} relations=${JSON.stringify(evidenceRelations)} types=${JSON.stringify(evidenceTypes)}`,
);

await page.locator('[data-pull-tab="checks"]').click();
await page.waitForSelector('[data-pull-panel="checks"]');
const failedRows = await page
  .locator("[data-check-row][data-check-passed='false']")
  .evaluateAll((els) =>
    els.map((el) => [el.getAttribute("data-check-row"), el.getAttribute("data-check-severity")]),
  );
check(
  "checks: the failing rows carry the machine's own severity",
  JSON.stringify(failedRows) ===
    JSON.stringify([
      ["schema-schema_change_is_backward_compatible", "blocking"],
      ["rights-rights_pin_inherits_default", "warning"],
    ]),
  `got ${JSON.stringify(failedRows)}`,
);
check(
  "checks: the verdict and its explanation render",
  (await page.locator('[data-pull-panel="checks"] [data-verdict="blocked"]').count()) === 1,
);

await page.locator('[data-pull-tab="raw"]').click();
await page.waitForSelector('[data-pull-panel="raw"]');
const rawSides = await page
  .locator("[data-file-diff-ref]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-file-diff-ref")));
check(
  "raw files: both sides of the three-way are listed on the secondary tab",
  JSON.stringify(rawSides) === JSON.stringify(["source", "target"]),
  `got ${JSON.stringify(rawSides)}`,
);
const rawLinks = await page
  .locator("[data-raw-patch]")
  .evaluateAll((els) => els.map((el) => [el.getAttribute("data-raw-patch"), el.getAttribute("href")]));
// Both sides pin the same base commit, so the base ref appears twice.
const wantedRefs = [BASE_SHA, SOURCE_SHA, BASE_SHA, TARGET_SHA];
check(
  "raw files: every recorded ref links to its own raw patch",
  JSON.stringify(rawLinks.map(([sha]) => sha)) === JSON.stringify(wantedRefs) &&
    rawLinks.every(
      ([sha, href]) =>
        typeof href === "string" &&
        href === `${API}/api/v1/projects/${PROJECT.id}/files/diff?sha=${sha}`,
    ),
  `got ${JSON.stringify(rawLinks)}`,
);
check(
  "raw files: the page says the raw view is secondary to the semantic diff",
  (await page.locator('[data-pull-panel="raw"]').textContent()).includes("secondary by design"),
);

/* ---------- 4. The review controls ---------- */

await page.locator('[data-pull-tab="summary"]').click();
await page.waitForSelector("[data-pull-panel='summary']");
await page.waitForSelector("[data-pull-review]");

check(
  "review: the page sends NO write before the human submits",
  apiTraffic.filter((c) => c.method === "POST").length === 0,
  `found ${apiTraffic.filter((c) => c.method === "POST").length} POST(s)`,
);
check(
  "review: the submit button is disabled until a decision is chosen",
  await page.locator("[data-review-submit]").isDisabled(),
);

const headLine = page.locator('[data-review-state-dimension="scientific"]');
check(
  "review: a dimension whose only decision judged an older head reads as none for this head",
  (await headLine.getAttribute("data-review-state-decision")) === "none",
  await headLine.textContent(),
);
const integrityLine = page.locator('[data-review-state-dimension="integrity"]');
check(
  "review: the integrity dimension has no decision for this head either",
  (await integrityLine.getAttribute("data-review-state-decision")) === "none",
);
const earlierText = await page.locator("[data-pull-earlier-reviews]").textContent();
check(
  "review: the older-head decision is still shown, named with the state it judged",
  earlierText.includes("scientific") &&
    earlierText.includes("changes_requested") &&
    earlierText.includes(OLDER_STATE.slice(0, 8)) &&
    earlierText.includes("not the head proposed now"),
  earlierText,
);

// The human decides: one dimension, one verdict, one line of reasoning.
await page.locator("[data-review-kind]").selectOption("integrity");
await page.locator("[data-review-decision]").selectOption("changes_requested");
await page.locator("[data-review-body]").fill("the schema change has to be migrated first");
await page.locator("[data-review-submit]").click();
await page.waitForSelector("[data-review-saved]");

check(
  "review: exactly one POST reaches the API",
  postedReviews.length === 1,
  `got ${postedReviews.length}`,
);
check(
  "review: the POST body is exactly the human's decision",
  JSON.stringify(postedReviews[0]) ===
    JSON.stringify({
      kind: "integrity",
      decision: "changes_requested",
      body: "the schema change has to be migrated first",
    }),
  JSON.stringify(postedReviews[0]),
);
const reviewPosts = apiTraffic.filter((c) => c.method === "POST" && c.pathname === REVIEWS_PATH);
check(
  "review: the POST carries the session's CSRF token",
  reviewPosts.length === 1 && reviewPosts[0].csrf === CSRF_TOKEN,
  JSON.stringify(reviewPosts),
);
check(
  "review: the recorded decision renders on its own dimension",
  (await integrityLine.getAttribute("data-review-state-decision")) === "changes_requested",
  await integrityLine.textContent(),
);
check(
  "review: the other dimension is untouched by it (docs/09 §5: dimensions never flatten)",
  (await headLine.getAttribute("data-review-state-decision")) === "none",
);
check(
  "risks: the decision now blocks the current head",
  (await page.locator('[data-pull-risk="RISK_CHANGES_REQUESTED"]').count()) === 1 &&
    (await page
      .locator('[data-pull-risk="RISK_CHANGES_REQUESTED"]')
      .getAttribute("data-risk-severity")) === "blocking",
);
const severities = await page
  .locator("[data-pull-risks] [data-risk-severity]")
  .evaluateAll((els) => els.map((el) => el.getAttribute("data-risk-severity")));
check(
  "risks: the banner leads with blocking risks",
  severities.length > 0 && severities[0] === "blocking",
  `severities=${JSON.stringify(severities)}`,
);

// A refused submission shows the API's own message and changes nothing.
state.refuseReviews = true;
await page.locator("[data-review-kind]").selectOption("scientific");
await page.locator("[data-review-decision]").selectOption("approved");
await page.locator("[data-review-body]").fill("looks fine to me");
await page.locator("[data-review-submit]").click();
await page.waitForSelector("[data-review-error]");
const refusalText = await page.locator("[data-review-error]").textContent();
check(
  "review: a 403 renders the API's own refusal, not a client-side guess",
  refusalText.includes("not permitted to submit a review"),
  refusalText,
);
check(
  "review: a refused submission records nothing",
  (await headLine.getAttribute("data-review-state-decision")) === "none" &&
    (await page.locator('[data-pull-risk="RISK_CHANGES_REQUESTED"]').count()) === 1,
);

/* ---------- 5. The risk banner is computed, not decorative ---------- */

// Swap in a proposal with nothing to warn about: every check passes, no
// review judged this head, one structural change, no file refs.
state.report = CLEAN_REPORT;
state.diff = CLEAN_DIFF;
state.reviews = [];
state.refuseReviews = false;
await openPullPage();

check(
  "risks: a clean proposal shows no risk banner at all (no green all-clear)",
  (await page.locator("[data-pull-risks]").count()) === 0,
  `found ${await page.locator("[data-pull-risks]").count()}`,
);
check(
  "first screen: the Summary tab is still the default after a reload",
  (await page.locator('[data-pull-tab="summary"][aria-selected="true"]').count()) === 1 &&
    (await page.locator('[data-pull-panel="raw"], [data-raw-patch]').count()) === 0,
);

await page.locator('[data-pull-tab="knowledge"]').click();
await page.waitForSelector("[data-pull-panel='knowledge']");
check(
  "knowledge changes: an empty bucket says so instead of rendering nothing",
  (await page.locator("[data-pull-bucket-empty]").count()) === 1 &&
    (await page.locator("[data-pull-panel='knowledge']").textContent()).includes("No knowledge changes"),
);
await page.locator('[data-pull-tab="evidence"]').click();
await page.waitForSelector("[data-pull-panel='evidence']");
check(
  "evidence: an empty bucket says so too",
  (await page.locator("[data-pull-panel='evidence']").textContent()).includes("No evidence changes"),
);

/* ---------- 6. Rework: the two ways the page could lie ---------- */

// 6a. An object CREATED already carrying its own rights policy. The diff
// engine records no changed_fields for a creation — there is no base
// value to differ from (internal/rsg/diff classifyObject returns
// (created, nil)) — so the only thing that says "this merge decides who
// may read this object" is the pin on the source version.
const PINNED_POLICY = "policy-restricted-t0408";
const createdChange = (objectId, objectType, title, visibilityPolicyId = null) => ({
  object_id: objectId,
  object_type: objectType,
  kind: "created",
  base_version: null,
  source_version: objectVersion(`ver-${objectId}`, objectId, objectType, 1, HEAD_STATE, title, visibilityPolicyId),
  target_version: null,
  target_moved: false,
  changed_fields: [],
  target_changed_fields: [],
  new_versions: [],
});

state.report = CLEAN_REPORT; // no machine check can raise the risk below
state.reviews = [];
state.diff = {
  ...DIFF,
  object_changes: [
    createdChange("obj-restricted-1", "research_question", "Is the second phase metastable?", PINNED_POLICY),
    createdChange("obj-plain-1", "claim", "The second phase is metastable"),
  ],
  relation_changes: [],
  summary: {
    objects_created: 2,
    objects_updated: 0,
    objects_aborted: 0,
    objects_reopened: 0,
    relations_created: 0,
    relations_updated: 0,
  },
};
await openPullPage();

// Read without auto-waiting: a missing risk must report itself.
const visibilityRisks = page.locator('[data-pull-risk="RISK_VISIBILITY_CHANGED"]');
const visibilityCount = await visibilityRisks.count();
const visibilityRisk = visibilityRisks.first();
const visibilityText = visibilityCount === 0 ? "" : await visibilityRisk.textContent();
check(
  "risks: an object CREATED already pinned to its own visibility policy is flagged",
  visibilityCount === 1 &&
    visibilityText.includes("Is the second phase metastable?") &&
    visibilityText.includes(PINNED_POLICY),
  `count=${visibilityCount} text=${visibilityText}`,
);
check(
  "risks: an ordinary creation (no pin — it inherits the project default) is not flagged",
  visibilityCount === 1 && !visibilityText.includes("The second phase is metastable"),
  `count=${visibilityCount} text=${visibilityText}`,
);
check(
  "risks: the visibility risk warns and points at the scientific tab",
  visibilityCount === 1 &&
    (await visibilityRisk.getAttribute("data-risk-severity")) === "warning" &&
    (await visibilityRisk.locator('[data-risk-tab="scientific"]').count()) === 1,
  `count=${visibilityCount} severity=${visibilityCount === 0 ? null : await visibilityRisk.getAttribute("data-risk-severity")}`,
);
if (visibilityCount === 1) {
  await visibilityRisk.locator('[data-risk-tab="scientific"]').click();
  await page.waitForSelector('[data-pull-panel="scientific"]');
  check(
    "risks: the visibility risk leads to the created object on the scientific tab",
    (await page.locator('[data-scientific-object="obj-restricted-1"]').count()) === 1 &&
      (await page.locator('[data-pull-tab="scientific"][aria-selected="true"]').count()) === 1,
  );
} else {
  // No banner to open: report that as the failure it is rather than
  // letting a Playwright timeout end the run before the rest of the
  // checks have had their say.
  check(
    "risks: the visibility risk leads to the created object on the scientific tab",
    false,
    `no RISK_VISIBILITY_CHANGED banner to open (count=${visibilityCount})`,
  );
}

// 6b. Two decisions inside the same second, the later one written with a
// fraction Go would serialize as "…T12:00:00.5Z". Ordered by
// (created_at, id), the list is oldest first, so the LAST one is the
// head's state — while as plain strings "…T12:00:00Z" sorts above its own
// fraction and "…12:00:00.5Z" above "…12:00:00.55Z".
state.reviews = [
  wireReview("rev-same-a", "scientific", "approved", HEAD_STATE, "the earlier one", "2026-09-15T12:00:00Z"),
  wireReview("rev-same-b", "scientific", "changes_requested", HEAD_STATE, "the later one", "2026-09-15T12:00:00.5Z"),
];
await openPullPage();
const sameSecondLine = page.locator('[data-review-state-dimension="scientific"]');
const sameSecondDecision = await sameSecondLine.getAttribute("data-review-state-decision");
check(
  "review: of two same-second decisions the LATER one is the head's state (…:00Z vs …:00.5Z)",
  sameSecondDecision === "changes_requested",
  `got ${sameSecondDecision}: ${await sameSecondLine.textContent()}`,
);
check(
  "risks: the banner follows the later decision too",
  (await page.locator('[data-pull-risk="RISK_CHANGES_REQUESTED"]').count()) === 1,
);

// The same trap one fraction deeper: ".5Z" sorts above ".55Z" as a
// string while being the earlier instant.
state.reviews = [
  wireReview("rev-frac-a", "scientific", "approved", HEAD_STATE, "", "2026-09-15T12:00:00.5Z"),
  wireReview("rev-frac-b", "scientific", "changes_requested", HEAD_STATE, "", "2026-09-15T12:00:00.55Z"),
];
await openPullPage();
const deeperLine = page.locator('[data-review-state-dimension="scientific"]');
const deeperDecision = await deeperLine.getAttribute("data-review-state-decision");
check(
  "review: and one fraction deeper (…:00.5Z vs …:00.55Z)",
  deeperDecision === "changes_requested",
  `got ${deeperDecision}: ${await deeperLine.textContent()}`,
);

await browser.close();

if (fails > 0) {
  console.error(`pulls-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("pulls-e2e: all passed");
