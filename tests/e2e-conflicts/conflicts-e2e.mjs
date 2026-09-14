/**
 * Scientific Conflict Resolution UI e2e (T0407 required test
 * "conflict e2e"): a real Chromium drives the BUILT web app (next start,
 * production layer) through the Conflicts page. The Go API is mocked at
 * the network layer with the exact wire shapes cmd/api/conflicthttp
 * produces/consumes — conflictViewPayload (the T0405 report with the
 * T0401 three-way diff riding along), the save request/answer — the Go
 * handler suite and the e2e Go test (tests/e2e/conflict_e2e_test.go)
 * lock those shapes against the real guard, so the halves cover the real
 * contract without the browser test depending on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - 用户可明确处理冲突: the page renders, per classified conflict, the
 *     three-way context (base value, source = A value, target = B value),
 *     the per-side evidence context, the detector's explanation labeled
 *     仅建议 (advisory only), and the decision form with exactly five
 *     options — Accept A (source) / Accept B (target) / Keep both
 *     versions / Create validation branch / Keep unresolved — and the
 *     save is the human's explicit act: the button is disabled until a
 *     kind is chosen, and the PUT carries exactly the chosen kind + note
 *     with the session's CSRF token;
 *   - 不存在自动平均参数: no element anywhere on the page offers or
 *     mentions an averaged/merged/computed value, the five options are
 *     the exhaustive set (no sixth "average" option exists), and the
 *     page sends NO write before the human presses Save.
 *
 * Usage: node conflicts-e2e.mjs <base-url> [apps-web-dir]
 */
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31110";
const APPS_WEB = process.argv[3] ?? path.resolve(import.meta.dirname, "../../apps/web");

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
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

const SESSION = {
  user: {
    id: "00000000-0000-4000-8000-000000000001",
    handle: "alice",
    email: "alice@example.com",
    display_name: "Alice Guo",
  },
  csrf_token: "csrf-e2e-token",
};

const NOT_FOUND = {
  code: "PROJECT_NOT_FOUND",
  message: "project not found",
  request_id: "e2e-req",
  retryable: false,
};

const TRIPLE = {
  base_state_id: "aaaaaaaa-0000-4000-8000-000000000001",
  source_state_id: "aaaaaaaa-0000-4000-8000-000000000002",
  target_state_id: "aaaaaaaa-0000-4000-8000-000000000003",
};

/** One scientific object version (internal/rsg/manifest ObjectVersion). */
function objectVersion(id, versionNo, stateId, temperature) {
  return {
    id,
    object_id: "obj-protocol-1",
    object_type: "protocol",
    version_no: versionNo,
    state_id: stateId,
    branch_id: null,
    schema_ref: { id: "sch-protocol", version: "1" },
    title: "Synthesis protocol",
    lifecycle_state: "active",
    payload: { purpose: "synthesize", steps: [{ id: "s1", temperature }] },
    visibility_policy_id: null,
    integrity_hash: `hash-${temperature}`,
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-10T08:00:00Z",
  };
}

const BASE_VERSION = objectVersion("ver-base-1", 1, TRIPLE.base_state_id, 300);
const SOURCE_VERSION = objectVersion("ver-src-1", 2, TRIPLE.source_state_id, 350);
const TARGET_VERSION = objectVersion("ver-tgt-1", 2, TRIPLE.target_state_id, 400);

/** The GET conflicts answer (cmd/api/conflicthttp conflictViewPayload). */
const CONFLICTS_VIEW = {
  report: {
    format_version: "v1",
    project_id: PROJECT.id,
    diff: {
      format_version: "v1",
      project_id: PROJECT.id,
      base: { id: TRIPLE.base_state_id, git_ref: "b00" },
      source: { id: TRIPLE.source_state_id, git_ref: "a11" },
      target: { id: TRIPLE.target_state_id, git_ref: "c22" },
      object_changes: [
        {
          object_id: "obj-protocol-1",
          object_type: "protocol",
          kind: "updated",
          base_version: BASE_VERSION,
          source_version: SOURCE_VERSION,
          target_version: TARGET_VERSION,
          target_moved: true,
          changed_fields: ["payload"],
          target_changed_fields: ["payload"],
          new_versions: [],
        },
      ],
      relation_changes: [],
      file_diff_refs: [],
      summary: {
        objects_created: 0,
        objects_updated: 1,
        objects_aborted: 0,
        objects_reopened: 0,
        relations_created: 0,
        relations_updated: 0,
      },
    },
    object_verdicts: [
      {
        object_id: "obj-protocol-1",
        object_type: "protocol",
        kind: "updated",
        auto_mergeable: false,
        conflicts: [
          {
            category: "scientific",
            code: "SCIENTIFIC_FIELD_DIVERGES",
            fields: ["payload"],
            payload_keys: ["steps"],
            other_object_id: "",
            detail:
              "both branches moved the same protocol step s1 to different temperatures — a scientific conflict no machine may resolve",
          },
        ],
      },
    ],
    relation_verdicts: [],
    auto_mergeable: false,
    summary: {
      objects_auto_mergeable: 0,
      objects_conflicted: 1,
      relations_auto_mergeable: 0,
      relations_conflicted: 0,
      conflicts_by_category: [{ category: "scientific", count: 1 }],
    },
  },
  evidence: [
    {
      object_id: "obj-protocol-1",
      source_evidence: [],
      target_evidence: [
        {
          relation_type: "supports",
          evidence_type: "measurement",
          directness: "direct",
          reasoning_note: "backs the 400 K reading",
          review_state: "accepted",
          evidence_object_id: "obj-evidence-7",
          evidence_object_type: "measurement",
          evidence_title: "Thermal run 7",
          evidence_object_version_no: 2,
        },
      ],
    },
  ],
  resolutions: [],
};

const PUT_RESOLUTIONS_PATH = `/api/v1/projects/${PROJECT.id}/resolutions`;

/** The recorded API traffic (methods + bodies), for the network-level
 *  assertions: no write before the human decides, the PUT carries the
 *  CSRF token, and the body is exactly the chosen decision. */
const apiTraffic = [];
/** The PUT bodies actually received, most recent last. */
const putBodies = [];

function qs(url) {
  return Object.fromEntries(new URL(url).searchParams.entries());
}

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
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/conflicts`) {
      const q = qs(request.url());
      if (
        q.base_state_id === TRIPLE.base_state_id &&
        q.source_state_id === TRIPLE.source_state_id &&
        q.target_state_id === TRIPLE.target_state_id
      ) {
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(CONFLICTS_VIEW),
        });
      }
      return route.fulfill({ status: 400, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    if (method === "PUT" && pathname === PUT_RESOLUTIONS_PATH) {
      const body = request.postDataJSON();
      putBodies.push(body);
      if (
        body.base_state_id !== TRIPLE.base_state_id ||
        body.source_state_id !== TRIPLE.source_state_id ||
        body.target_state_id !== TRIPLE.target_state_id ||
        !Array.isArray(body.resolutions) ||
        body.resolutions.length === 0
      ) {
        return route.fulfill({
          status: 400,
          contentType: "application/json",
          body: JSON.stringify({ code: "VALIDATION_FAILED", message: "bad plan" }),
        });
      }
      // The answer is the full updated plan (resolutionPayload list).
      const now = "2026-09-14T12:00:00Z";
      const records = body.resolutions.map((d, i) => ({
        id: `res-${i + 1}`,
        project_id: PROJECT.id,
        base_state_id: body.base_state_id,
        source_state_id: body.source_state_id,
        target_state_id: body.target_state_id,
        target_kind: d.target_kind,
        target_id: d.target_id,
        code: d.code,
        fields: d.fields ?? [],
        payload_keys: d.payload_keys ?? [],
        other_object_id: d.other_object_id ?? null,
        kind: d.kind,
        note: d.note ?? "",
        decided_by: "00000000-0000-4000-8000-000000000001",
        decided_at: now,
        updated_at: now,
      }));
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ resolutions: records }),
      });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- B. Browser scenarios ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

const CONFLICTS =
  `${BASE}/projects/${PROJECT.id}/conflicts` +
  `?base_state_id=${TRIPLE.base_state_id}` +
  `&source_state_id=${TRIPLE.source_state_id}` +
  `&target_state_id=${TRIPLE.target_state_id}`;

/* ---------- 0. A bare visit without the triple explains itself ---------- */

await page.goto(`${BASE}/projects/${PROJECT.id}/conflicts`, { waitUntil: "load" });
await page.waitForSelector("[data-conflicts-missing]");
const missingText = await page.locator("[data-conflicts-missing]").textContent();
for (const want of ["base_state_id", "source_state_id", "target_state_id"]) {
  if (!missingText.includes(want)) {
    fail("missing triple: the page explains the required query parameters", `missing ${want}`);
  }
}
ok("missing triple: the page explains the required query parameters");
if (apiTraffic.some((c) => c.pathname.includes("/conflicts"))) {
  fail("missing triple: no conflicts API call happens without a triple");
} else {
  ok("missing triple: no conflicts API call happens without a triple");
}

/* ---------- 1. The page renders inside the project shell ---------- */

await page.goto(CONFLICTS, { waitUntil: "load" });
await page.waitForSelector('[data-project-shell="alloy-lab"]');
await page.waitForSelector("[data-conflicts-page]");
ok("conflicts page renders inside the project shell");

await page.waitForSelector("[data-conflict-card]");
const tripleText = await page.locator("[data-conflicts-triple]").textContent();
for (const want of [
  TRIPLE.base_state_id.slice(0, 8),
  TRIPLE.source_state_id.slice(0, 8),
  TRIPLE.target_state_id.slice(0, 8),
]) {
  if (!tripleText.includes(want)) {
    fail("triple: the header shows the three compared states", `missing ${want} (got ${tripleText})`);
  }
}
ok("triple: the header shows base, source (A) and target (B)");

const summaryText = await page.locator("[data-conflicts-summary]").textContent();
if (!summaryText.includes("1 conflict needs a human decision") || !summaryText.includes("scientific×1")) {
  fail("summary: conflicted count and category roll-up render", `got ${summaryText}`);
} else {
  ok("summary: conflicted count and category roll-up render");
}

/* ---------- 2. The conflict card: identity, explanation, three-way ---------- */

const card = page.locator('[data-conflict-card="obj-protocol-1"]').first();
const cardText = await card.textContent();
for (const want of ["scientific", "SCIENTIFIC_FIELD_DIVERGES", "protocol"]) {
  if (!cardText.includes(want)) {
    fail("card: category, code and target render", `missing ${want} (got ${cardText})`);
  }
}
ok("card: category, code and target render");

// The detector's explanation is advisory only (仅建议) — the requirement:
// the agent explanation never decides.
const advisory = await card.locator("[data-conflict-advisory-badge]").textContent();
if (!advisory.includes("仅建议") || !advisory.includes("advisory only")) {
  fail("advisory: the explanation is labeled 仅建议 (advisory only)", `got ${advisory}`);
} else {
  ok("advisory: the explanation is labeled 仅建议 (advisory only)");
}
const advisoryText = await card.locator("[data-conflict-advisory]").textContent();
if (!advisoryText.includes("never applies this on its own")) {
  fail("advisory: the page states the machine never auto-applies the explanation");
} else {
  ok("advisory: the page states the machine never auto-applies the explanation");
}

// The three-way context: base value, source = A value, target = B value.
await card.locator("[data-conflicts-threeway]").waitFor();
const fieldRow = card.locator("[data-conflicts-threeway] tbody tr").first();
const baseCell = await fieldRow.locator('td[data-side="base"]').textContent();
const sourceCell = await fieldRow.locator('td[data-side="source"]').textContent();
const targetCell = await fieldRow.locator('td[data-side="target"]').textContent();
if (!baseCell.includes("300") || !sourceCell.includes("350") || !targetCell.includes("400")) {
  fail(
    "three-way: base / source (A) / target (B) values render per diverged field",
    `base=${baseCell}, source=${sourceCell}, target=${targetCell}`,
  );
} else {
  ok("three-way: base=300, source (A)=350, target (B)=400 render per diverged field");
}
const fieldLabel = await fieldRow.locator("th[scope=row]").textContent();
if (!fieldLabel.includes("payload.steps")) {
  fail("three-way: the diverged payload key is named", `got ${fieldLabel}`);
} else {
  ok("three-way: the diverged payload key (payload.steps) is named");
}

/* ---------- 3. Evidence context ---------- */

await card.locator("[data-conflicts-evidence]").waitFor();
const evidenceText = await card.locator("[data-conflicts-evidence]").textContent();
for (const want of ["Thermal run 7", "supports", "direct", "accepted", "backs the 400 K reading"]) {
  if (!evidenceText.includes(want)) {
    fail("evidence: the target-side evidence context renders", `missing ${want}`);
  }
}
if (!evidenceText.includes("No evidence recorded on this side")) {
  fail("evidence: the empty source side is shown as no evidence recorded");
} else {
  ok("evidence: per-side evidence context renders (target evidence; source has none)");
}

/* ---------- 4. Exactly five options; no averaging anywhere ---------- */

const radios = card.locator("[data-resolution-kind]");
const kinds = await radios.evaluateAll((els) => els.map((el) => el.value));
const WANT_KINDS = ["accept_source", "accept_target", "keep_both", "validation_branch", "unresolved"];
if (JSON.stringify(kinds) !== JSON.stringify(WANT_KINDS)) {
  fail("options: exactly the five decision options, in order", `got ${JSON.stringify(kinds)}`);
} else {
  ok("options: exactly five — accept A / accept B / keep both / validation branch / unresolved");
}

// 不存在自动平均参数: no element offers or mentions an averaged, merged or
// computed value anywhere on the page, and no sixth option exists.
const pageText = await page.locator("[data-conflicts-page]").textContent();
const AVERAGE_RE = /average|averag|平均|折中|均值|compute[ds]? value|auto[- ]?merge/i;
if (AVERAGE_RE.test(pageText)) {
  fail("no averaging: no text anywhere mentions an averaged/computed value", `page text matched ${AVERAGE_RE}`);
} else {
  ok("no averaging: no text anywhere mentions an averaged/computed value");
}
const averageControls = await page
  .locator('[data-resolution-kind="average"], [data-resolution-kind="mean"], [data-resolution-kind="auto"]')
  .count();
if (averageControls !== 0) {
  fail("no averaging: no averaging option exists in the decision form", `found ${averageControls}`);
} else {
  ok("no averaging: no averaging option exists in the decision form");
}

/* ---------- 5. No write before the human decides ---------- */

const putsBefore = apiTraffic.filter((c) => c.method === "PUT");
if (putsBefore.length !== 0) {
  fail("explicit: the page sends no resolution write before the human saves", `found ${putsBefore.length} PUT(s)`);
} else {
  ok("explicit: the page sends no resolution write before the human saves");
}

// The save button is disabled until a kind is chosen — the decision is
// the human's explicit act.
const disabledBefore = await card.locator("[data-conflict-save]").isDisabled();
if (!disabledBefore) {
  fail("explicit: the save button is disabled until a kind is chosen");
} else {
  ok("explicit: the save button is disabled until a kind is chosen");
}

/* ---------- 6. The human decides: the PUT is exactly their choice ---------- */

await card.locator('[data-resolution-kind="accept_target"]').check();
await card.locator("[data-conflict-note]").fill("Thermal run 7 backs 400 K");
await card.locator("[data-conflict-save]").click();

await page.waitForSelector("[data-conflict-saved]");
const savedBadge = await card.locator("[data-conflict-saved]").textContent();
if (!savedBadge.includes("accept_target")) {
  fail("save: the saved state renders the chosen decision", `got ${savedBadge}`);
} else {
  ok("save: the saved state renders the chosen decision (accept_target)");
}

if (putBodies.length !== 1) {
  fail("save: exactly one PUT reaches the API", `got ${putBodies.length}`);
} else {
  const body = putBodies[0];
  if (
    body.base_state_id !== TRIPLE.base_state_id ||
    body.source_state_id !== TRIPLE.source_state_id ||
    body.target_state_id !== TRIPLE.target_state_id
  ) {
    fail("save: the PUT carries the pinned triple", `got ${JSON.stringify(body).slice(0, 200)}`);
  } else {
    ok("save: the PUT carries the pinned triple");
  }
  const decision = body.resolutions[0];
  if (
    decision.target_kind !== "object" ||
    decision.target_id !== "obj-protocol-1" ||
    decision.code !== "SCIENTIFIC_FIELD_DIVERGES" ||
    decision.kind !== "accept_target" ||
    decision.note !== "Thermal run 7 backs 400 K" ||
    JSON.stringify(decision.fields) !== JSON.stringify(["payload"]) ||
    JSON.stringify(decision.payload_keys) !== JSON.stringify(["steps"])
  ) {
    fail("save: the decision is exactly the human's choice", `got ${JSON.stringify(decision)}`);
  } else {
    ok("save: the decision body is exactly the human's choice (kind + note + classifier key)");
  }
}

// The PUT rides the session's CSRF token.
const lastPut = apiTraffic.filter((c) => c.method === "PUT" && c.pathname === PUT_RESOLUTIONS_PATH);
if (lastPut.length === 0 || !(lastPut[0].csrf ?? false)) {
  fail("save: the PUT carries the X-CSRF-Token of the session");
} else {
  ok("save: the PUT carries the X-CSRF-Token of the session");
}

/* ---------- 7. The decision survives a reload (recorded, not ephemeral) ---------- */

CONFLICTS_VIEW.resolutions = [
  {
    id: "res-1",
    project_id: PROJECT.id,
    base_state_id: TRIPLE.base_state_id,
    source_state_id: TRIPLE.source_state_id,
    target_state_id: TRIPLE.target_state_id,
    target_kind: "object",
    target_id: "obj-protocol-1",
    code: "SCIENTIFIC_FIELD_DIVERGES",
    fields: ["payload"],
    payload_keys: ["steps"],
    other_object_id: null,
    kind: "accept_target",
    note: "Thermal run 7 backs 400 K",
    decided_by: "00000000-0000-4000-8000-000000000001",
    decided_at: "2026-09-14T12:00:00Z",
    updated_at: "2026-09-14T12:00:00Z",
  },
];
await page.reload({ waitUntil: "load" });
await page.waitForSelector("[data-conflict-card]");
const reloadedBadge = await card.locator("[data-conflict-saved]").textContent();
if (!reloadedBadge.includes("accept_target")) {
  fail("reload: the recorded decision re-renders", `got ${reloadedBadge}`);
} else {
  ok("reload: the recorded decision re-renders from the API's plan");
}
const reloadedRadio = await card.locator('[data-resolution-kind="accept_target"]').isChecked();
if (!reloadedRadio) {
  fail("reload: the recorded decision pre-selects its option");
} else {
  ok("reload: the recorded decision pre-selects its option");
}

await browser.close();

if (fails > 0) {
  console.error(`conflicts-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("conflicts-e2e: all passed");
