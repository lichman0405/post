/**
 * The fixtures the visual-regression baselines are shot from (T1101).
 *
 * Every shape here is copied from the harness that already owns it —
 * tests/e2e-shell, tests/e2e-pulls, tests/e2e-conflicts, tests/e2e-activity,
 * tests/e2e-assets, tests/e2e-search, tests/e2e-files — which in turn take
 * them from the Go handlers and are locked against real PostgreSQL by the
 * integration suites. Nothing here is invented: the point of a baseline is
 * that the page renders its REAL shape, and a fixture that only satisfies
 * the page's own parser would let a broken page produce a clean image.
 *
 * # Determinism
 *
 * A baseline is only as good as its determinism. Three rules:
 *
 *   1. Every date is a literal. No `new Date()` reaches a fixture, so a
 *      run tomorrow produces the same bytes as a run today.
 *   2. Every id is a literal, and the ones a page sorts by are chosen so
 *      that the sort order is the order written here.
 *   3. The lists are complete where the page paginates: `/activity` gets
 *      one window with no next cursor, so the "load more" control is in
 *      its settled state rather than mid-stream.
 *
 * # What an unmatched request does
 *
 * Nothing. `installApiMock` answers 404 with the API's own error envelope,
 * exactly as the other harnesses do. That is deliberate and it is the one
 * thing this file must not do quietly: a missing fixture leaves a page in
 * an error state, and the checklist's per-page "ready marker" assertion
 * then FAILS rather than baselining the error state as the design.
 */

/* ---------- The origin the web app is built against ---------- */

export const API = "http://api.e2e.test";

export const PROJECT_ID = "11111111-2222-4333-8444-555555555555";
export const SOLAR_ID = "22222222-3333-4444-8555-666666666666";
export const ALICE_ID = "00000000-0000-4000-8000-000000000001";
export const BOB_ID = "00000000-0000-4000-8000-000000000002";

const CSRF_TOKEN = "csrf-e2e-token";

const SESSION = {
  user: { id: ALICE_ID, handle: "alice", email: "alice@example.com", display_name: "Alice Guo" },
  csrf_token: CSRF_TOKEN,
};

const ERROR = (code, message) => ({ code, message, request_id: "vr-req", retryable: false });

/* ---------- The project ---------- */

export const PROJECT = {
  id: PROJECT_ID,
  organization_id: null,
  program_id: null,
  slug: "alloy-lab",
  name: "Alloy Lab",
  purpose: "Design high-entropy alloys with reproducible synthesis routes.",
  activity_status: "planning",
  visibility: "public",
  main_frozen: true,
  git_repository_external_id: null,
  provision_status: "pending",
  created_by: ALICE_ID,
  created_at: "2026-09-10T08:00:00Z",
};

/**
 * Project overview / research workspace data (T1202 demo). Both pages read
 * `/api/v1/projects/{id}/overview`; without it the research page renders an
 * error state and the overview page renders the demo skeleton with empty
 * counts. This fixture gives them stable content for the baseline.
 */
export const OVERVIEW = {
  counts: { questions: 3, findings: 2, hypotheses: 4, claims: 5, other_objects: 1 },
  key_questions: [
    {
      object_id: "Q-001",
      statement: "Can MOF-X separate ethylene under realistic humidity?",
      question_state: "open",
      hypotheses: [
        { object_id: "H-001", object_type: "hypothesis", title: "Binding affinity remains above threshold at 40% RH" },
        { object_id: "H-002", object_type: "hypothesis", title: "Humidity displaces weakly bound guest molecules" },
      ],
      findings: [],
    },
  ],
  key_findings: [
    {
      object_id: "F-001",
      statement: "Breakthrough experiments show capacity drops above 40% RH.",
      assessment: "supported",
      finding_type: "experimental",
      claims: [
        { object_id: "C-001", version_id: "C-001-v1", title: "Capacity retention >80% below 40% RH", resolved: true },
      ],
    },
  ],
  branches: {
    active: [
      {
        id: "BR-001",
        name: "humidity-reactivation",
        purpose: "Test reactivation protocols after humidity exposure",
        head_state_id: "state-abc123",
      },
    ],
  },
  current_main: {
    head_state_id: "state-main-001",
    latest_commit: { message: "Add humidity breakthrough data", actor: "Alice Guo" },
  },
};

/** The private project: rendered in the directory, never entered. */
const SOLAR = {
  ...PROJECT,
  id: SOLAR_ID,
  slug: "solar-lab",
  name: "Solar Lab",
  visibility: "private",
  main_frozen: false,
};

/** A third row, so the directory is a list rather than a pair. */
const COPPER = {
  ...PROJECT,
  id: "33333333-4444-4555-8666-777777777777",
  slug: "copper-lab",
  name: "Copper Lab",
  main_frozen: false,
  provision_status: "ready",
};

const OWNER_MEMBERSHIP = {
  project_id: PROJECT_ID,
  user_id: ALICE_ID,
  role: "owner",
  created_at: "2026-09-10T08:00:00Z",
};

const MEMBERS = [
  { user_id: ALICE_ID, handle: "alice", display_name: "Alice Guo", role: "owner", joined_at: "2026-09-10" },
  { user_id: BOB_ID, handle: "bob", display_name: "Bob Chen", role: "maintainer", joined_at: "2026-09-11" },
  { user_id: "00000000-0000-4000-8000-000000000003", handle: "carol", display_name: "Carol Ndiaye", role: "contributor", joined_at: "2026-09-12" },
  { user_id: "00000000-0000-4000-8000-000000000004", handle: "dmitri", display_name: "Dmitri Volkov", role: "viewer", joined_at: "2026-09-13" },
];

/* ---------- The Files tab ---------- */

const TREE_ROOT = {
  ref: "main",
  path: "",
  sha: "tree-root-sha-001",
  entries: [
    { name: "README.md", path: "README.md", type: "blob", mode: "100644", size: 34, sha: "blob-readme-1" },
    { name: "docs", path: "docs", type: "tree", mode: "040000", size: 0, sha: "tree-docs-1" },
    { name: "data", path: "data", type: "tree", mode: "040000", size: 0, sha: "tree-data-1" },
    { name: "plot.png", path: "plot.png", type: "blob", mode: "100644", size: 4200, sha: "blob-plot-1" },
  ],
};

/* ---------- The pull request ---------- */

const BASE_STATE = "bbbbbbbb-0000-4000-8000-000000000001";
const HEAD_STATE = "dddddddd-0000-4000-8000-000000000002";
const TARGET_STATE = "cccccccc-0000-4000-8000-000000000003";
const BASE_SHA = "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0";
const SOURCE_SHA = "a11a11a11a11a11a11a11a11a11a11a11a11a11";
const TARGET_SHA = "c22c22c22c22c22c22c22c22c22c22c22c22c22";

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
  created_by: ALICE_ID,
  created_at: "2026-09-14T09:00:00Z",
};

/** A second, closed proposal so the list is not a single row. */
const PR_CLOSED = {
  ...PR,
  id: "pr-00000000-0000-4000-8000-000000000006",
  number: 6,
  title: "Retire the 300 K reading",
  body: "The sample was contaminated in transit.",
  state: "merged",
  created_at: "2026-09-13T09:00:00Z",
};

function objectVersion(id, objectId, objectType, versionNo, stateId, title) {
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
    visibility_policy_id: null,
    integrity_hash: `hash-${id}`,
    created_by: ALICE_ID,
    created_at: "2026-09-14T09:00:00Z",
  };
}

function relationVersion(id, relationId, relationType, source, target) {
  return {
    id,
    relation_id: relationId,
    version_no: 1,
    state_id: HEAD_STATE,
    relation_type: relationType,
    source_object_version_id: source,
    target_object_version_id: target,
    payload: {},
    integrity_hash: `hash-${id}`,
    created_by: ALICE_ID,
    created_at: "2026-09-14T09:00:00Z",
  };
}

const CHECKS = {
  kind: "integrity",
  project_id: PROJECT_ID,
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

const DIFF = {
  format_version: "v1",
  project_id: PROJECT_ID,
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
      source_version: relationVersion("ver-rel-supports", "rel-supports-1", "supports", "ver-evidence-1", "ver-claim-1"),
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
      source_version: relationVersion("ver-rel-addresses", "rel-addresses-1", "addresses_question", "ver-claim-1", "ver-question-1"),
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

/** One recorded review, as cmd/api/reviewhttp's reviewPayload writes it —
 *  the shape tests/e2e-pulls keeps in sync with the Go side. lib/pulls.ts
 *  asReview polices pull_request_id and responsibility, not pr_id. */
const REVIEWS = [
  {
    id: "rev-1",
    pull_request_id: PR.id,
    reviewer_id: BOB_ID,
    kind: "scientific",
    decision: "approve",
    reviewed_state_id: HEAD_STATE,
    responsibility: "scientific-lead",
    body: "The evidence assertion supports the claim it is attached to.",
    created_at: "2026-09-15T11:00:00Z",
  },
];

/* ---------- The conflicts page ---------- */

export const TRIPLE = {
  base_state_id: "aaaaaaaa-0000-4000-8000-000000000001",
  source_state_id: "aaaaaaaa-0000-4000-8000-000000000002",
  target_state_id: "aaaaaaaa-0000-4000-8000-000000000003",
};

function protocolVersion(id, versionNo, stateId, temperature) {
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
    created_by: ALICE_ID,
    created_at: "2026-09-10T08:00:00Z",
  };
}

const CONFLICTS_VIEW = {
  report: {
    format_version: "v1",
    project_id: PROJECT_ID,
    diff: {
      format_version: "v1",
      project_id: PROJECT_ID,
      base: { id: TRIPLE.base_state_id, git_ref: "b00" },
      source: { id: TRIPLE.source_state_id, git_ref: "a11" },
      target: { id: TRIPLE.target_state_id, git_ref: "c22" },
      object_changes: [
        {
          object_id: "obj-protocol-1",
          object_type: "protocol",
          kind: "updated",
          base_version: protocolVersion("ver-base-1", 1, TRIPLE.base_state_id, 300),
          source_version: protocolVersion("ver-src-1", 2, TRIPLE.source_state_id, 350),
          target_version: protocolVersion("ver-tgt-1", 2, TRIPLE.target_state_id, 400),
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
      source_evidence: [
        {
          relation_type: "supports",
          evidence_type: "measurement",
          directness: "direct",
          reasoning_note: "the 350 K reading is the one the source branch recorded",
          review_state: "accepted",
          evidence_object_id: "obj-evidence-9",
          evidence_object_type: "measurement",
          evidence_title: "Thermal run 9",
          evidence_object_version_no: 1,
        },
      ],
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

/* ---------- The activity stream ---------- */

const ACTOR = { actor_id: ALICE_ID, actor_handle: "alice", actor_display_name: "Alice Guo" };
const OBJECT_ID = "0a0b0c0d-3333-4000-8000-000000000004";
export const RELEASE_ID = "0a0b0c0d-4444-4000-8000-000000000005";

const ACTIVITY = {
  entries: [
    {
      id: "0a0b0c0d-0000-4000-8000-0000000000a4",
      source: "research",
      actor_id: null,
      actor_handle: null,
      actor_display_name: null,
      via: "mcp",
      action: "scientific_object.reopened",
      target_ref: null,
      project_id: PROJECT_ID,
      organization_id: null,
      correlation_id: "corr-reopen",
      payload: { object_id: OBJECT_ID, version_no: 5, reason_code: "wrong_sample" },
      visibility: "private",
      occurred_at: "2026-09-15T08:00:00Z",
    },
    {
      id: "0a0b0c0d-0000-4000-8000-0000000000a2",
      source: "research",
      ...ACTOR,
      via: "claude_code",
      action: "scientific_object.aborted",
      target_ref: null,
      project_id: PROJECT_ID,
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
    },
    {
      id: "0a0b0c0d-0000-4000-8000-0000000000a1",
      source: "governance",
      ...ACTOR,
      via: "session",
      action: "scientific_object.aborted",
      target_ref: `object:${OBJECT_ID}`,
      project_id: PROJECT_ID,
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
    },
    {
      id: "0a0b0c0d-0000-4000-8000-0000000000a3",
      source: "governance",
      ...ACTOR,
      via: "web",
      action: "release.created",
      target_ref: `release:${RELEASE_ID}`,
      project_id: PROJECT_ID,
      organization_id: null,
      correlation_id: "corr-release",
      before_summary: null,
      after_summary: {
        version: "v1.2.0",
        state_id: "0a0b0c0d-9999-4000-8000-00000000000a",
        manifest_hash: "9f2c1ab34d5e6f708192a3b4c5d6e7f80912a3b4c5d6e7f80912a3b4c5d6e7f8",
      },
      occurred_at: "2026-09-13T12:00:00Z",
    },
    {
      id: "0a0b0c0d-0000-4000-8000-0000000000a5",
      source: "governance",
      ...ACTOR,
      via: "web",
      action: "something.new_thing",
      target_ref: null,
      project_id: PROJECT_ID,
      organization_id: null,
      correlation_id: "corr-unknown",
      before_summary: null,
      after_summary: null,
      occurred_at: "2026-09-12T12:00:00Z",
    },
  ],
  next_cursor: null,
};

/* ---------- The releases ---------- */

const RELEASE = {
  id: RELEASE_ID,
  project_id: PROJECT_ID,
  version: "v1.2.0",
  title: "MOF-X 40% RH selectivity",
  state_id: "0a0b0c0d-9999-4000-8000-00000000000a",
  policy_version_id: "policy-v3",
  org_policy_version_id: null,
  manifest_hash: "9f2c1ab34d5e6f708192a3b4c5d6e7f80912a3b4c5d6e7f80912a3b4c5d6e7f8",
  created_by: ALICE_ID,
  created_at: "2026-09-13T12:00:00Z",
};

const RELEASE_OLDER = {
  ...RELEASE,
  id: "0a0b0c0d-4444-4000-8000-000000000006",
  version: "v1.1.0",
  title: "First reproducible uptake curve",
  manifest_hash: "1a2b3c4d5e6f708192a3b4c5d6e7f80912a3b4c5d6e7f80912a3b4c5d6e7f801",
  created_at: "2026-09-11T12:00:00Z",
};

/* ---------- The asset page ---------- */

export const ASSET_PID = "01j9z6k3m4n5p6q7r8s9t0v1w01";
const OPEN_LAB = { id: "11111111-1111-4111-8111-111111111111", name: "Open Materials Lab", slug: "open-materials", visibility: "public" };
const CAROL = { user_id: "00000000-0000-4000-8000-000000000003", handle: "carol", display_name: "Carol Ndiaye" };

const V10 = {
  version: "1.0",
  url: `/assets/${ASSET_PID}/1.0`,
  visibility: "public",
  integrity_hash: "sha256:1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f",
  published_at: "2026-09-01T10:00:00Z",
  current: false,
};
const V20 = {
  version: "2.0",
  url: `/assets/${ASSET_PID}/2.0`,
  visibility: "public",
  integrity_hash: "sha256:2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f",
  published_at: "2026-09-10T10:00:00Z",
  current: true,
};

const ASSET_PAGE = {
  asset: {
    pid: ASSET_PID,
    type: "material_collection",
    title: "MOF-5 Synthesis Collection",
    slug: "mof5-synthesis",
    origin_project: OPEN_LAB,
    created_at: "2026-08-20T09:00:00Z",
  },
  version: {
    version: V20.version,
    url: V20.url,
    visibility: V20.visibility,
    integrity_hash: V20.integrity_hash,
    published_at: V20.published_at,
    published_by: { user_id: ALICE_ID, handle: "alice", display_name: "Alice Guo" },
  },
  origin: [
    { ref: `project:${OPEN_LAB.id}`, kind: "project", resolved: true, title: OPEN_LAB.name, project_id: OPEN_LAB.id, link: `/projects/${OPEN_LAB.id}` },
  ],
  rights: {
    version: 1,
    standard_license_id: "CC-BY-4.0",
    custom_agreement_ref: null,
    usage: {
      commercial_use: "allowed",
      derivatives: "allowed_with_attribution",
      redistribution: "allowed",
      model_training: "unspecified",
      attribution: "required",
      patent_grant: "not_granted",
    },
    visibility: { metadata: "project_policy", data_access: "restricted" },
    notes: "Synthesis parameters are published; precursor batches are not.",
  },
  creators: [
    { kind: "user", party_id: CAROL.user_id, handle: CAROL.handle, display_name: CAROL.display_name, role: "creator" },
  ],
  metadata: [
    { key: "blob_ids", value: ["sha256:9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"] },
    { key: "cell_chemistry", value: "Zn4O(BDC)3" },
    { key: "sample_count", value: 128 },
  ],
  dependencies: [
    { pin: "01j9z6k3m4n5p6q7r8s9t0v1w02@1.4", resolved: true, public: true, title: "BDC Linker Protocol", type: "protocol", url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w02/1.4" },
    { pin: "01j9z6k3m4n5p6q7r8s9t0v1w03@0.1", resolved: false, public: false },
  ],
  lineage: [
    { relation: "derived_from", direction: "parent", pid: "01j9z6k3m4n5p6q7r8s9t0v1w04", version: "1.0", url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w04/1.0", title: "MOF-5 Reference Dataset" },
  ],
  used_by: [
    { project_id: "33333333-3333-4333-8333-333333333333", project_name: "Catalysis Group", project_slug: "catalysis-group", dependency_type: "derived_from", created_at: "2026-09-11T08:30:00Z" },
  ],
  versions: [{ ...V20 }, { ...V10 }],
  events: [
    { type: "research_asset.version_published", occurred_at: "2026-09-10T10:00:00Z", version: "2.0", actor: { user_id: ALICE_ID, handle: "alice", display_name: "Alice Guo" } },
  ],
};

/* ---------- The search answer ---------- */

export const SEARCH_QUERY = "which MOF materials show CO2 uptake above 3 mmol/g at 298 K?";

const SEARCH_ANSWER = {
  answer_version: "1",
  status: "answered",
  query: SEARCH_QUERY,
  answer_view: true,
  summary:
    "Two sources bear directly on the question. Mg-MOF-74 is reported at 3.4 mmol/g CO2 uptake at 298 K, and a second finding records a 4% loss of uptake after ten cycles.",
  citations: ["object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1", "document:KNW-0007@3"],
  limitations: [
    { text: "the vector signal did not run: no embedder is configured, so the question could not be embedded", origin: "platform" },
    {
      text: "1 source is not asserted about by anything the platform has recorded: its version is listed but nothing has been asserted for or against it",
      refs: ["asset:AST-0044@1"],
      origin: "platform",
    },
  ],
  conflicts: [
    { text: "this source is contested: 1 evidence assertion contradicts it", refs: ["asset:AST-0001@2"], origin: "platform" },
  ],
  sources: [
    {
      rank: 1,
      ref: "asset:AST-0001@2",
      kind: "document",
      entity_type: "asset",
      object_type: "material",
      title: "Mg-MOF-74 CO2 uptake at 298 K",
      version: "2",
      href: "/api/v1/assets/AST-0001?version=2",
      project_id: PROJECT_ID,
      labels: ["independently_reproduced"],
      factors: [
        { factor: "query_scope_match", level: "question", level_rank: 0, reason: "recalled by the question's own content (full_text)" },
        { factor: "evidence", level: "direct", level_rank: 0, reason: "3 evidence assertions target this version" },
        { factor: "conflict", level: "uncontested", level_rank: 0, reason: "nothing on the platform contradicts this version" },
      ],
      cited: false,
    },
    {
      rank: 2,
      ref: "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1",
      kind: "object_version",
      object_type: "finding",
      title: "Uptake falls by 4% after ten cycles",
      version: "1",
      project_id: PROJECT_ID,
      labels: [],
      factors: [{ factor: "query_scope_match", level: "related", level_rank: 2, reason: "recalled by the question's graph neighbourhood" }],
      cited: true,
    },
    {
      rank: 3,
      ref: "document:KNW-0007@3",
      kind: "document",
      entity_type: "knowledge",
      title: "Screening protocol for CO2 isotherms",
      version: "3",
      href: "/api/v1/knowledge/KNW-0007",
      project_id: PROJECT_ID,
      labels: ["reviewed"],
      factors: [{ factor: "review", level: "approved", level_rank: 0, reason: "1 scientific review, 1 approved" }],
      cited: true,
    },
    {
      rank: 4,
      ref: "asset:AST-0044@1",
      kind: "document",
      entity_type: "asset",
      object_type: "dataset",
      title: "Activated carbon reference isotherm",
      version: "1",
      href: "/api/v1/assets/AST-0044?version=1",
      project_id: PROJECT_ID,
      labels: [],
      factors: [{ factor: "evidence", level: "asserted", level_rank: 2, reason: "1 evidence assertion targets this version" }],
      cited: false,
    },
  ],
};

/* ---------- The research profile ---------- */

export const PROFILE = {
  id: ALICE_ID,
  handle: "alice",
  display_name: "Alice Guo",
  bio: "Computational materials scientist. Reproducibility, phase diagrams, and the occasional alloy.",
  created_at: "2026-08-01T09:00:00Z",
};

/* ---------- The route table ---------- */

/**
 * Every route the covered pages read, and nothing else. A page that grows
 * a new read gets a 404 here and its ready marker fails, which is the
 * point: the baseline must not silently become an error-state baseline.
 */
export function respond(method, pathname, url, request) {
  const json = (body, status = 200) => ({ status, body });

  if (method === "GET" && pathname === "/api/v1/auth/session") return json(SESSION);

  /* Directory + shell */
  if (method === "GET" && pathname === "/api/v1/projects") {
    return json({ projects: [PROJECT, COPPER, SOLAR] });
  }
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}`) return json(PROJECT);
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/membership`) return json(OWNER_MEMBERSHIP);
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/members`) return json({ members: MEMBERS });

  /* Overview / research workspace (T1202 demo) */
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/overview`) return json(OVERVIEW);

  /* Files */
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/files/tree`) {
    if ((url?.searchParams.get("path") ?? "") !== "") {
      return json(ERROR("FILE_NOT_FOUND", "path not found"), 404);
    }
    return json(TREE_ROOT);
  }

  /* Pull requests */
  // The list and the reviews answer BARE ARRAYS, not envelopes: lib/pulls.ts
  // list() and reviews() both `Array.isArray(body)` on the decoded body and
  // throw on anything else (pulls.ts:564, pulls.ts:600).
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/pull-requests`) {
    return json([PR, PR_CLOSED]);
  }
  const prPath = `/api/v1/projects/${PROJECT_ID}/pull-requests/${PR_NUMBER}`;
  if (method === "GET" && pathname === prPath) return json(PR);
  if (method === "GET" && pathname === `${prPath}/checks`) return json(CHECKS);
  if (method === "GET" && pathname === `${prPath}/diff`) return json(DIFF);
  if (method === "GET" && pathname === `${prPath}/reviews`) return json(REVIEWS);

  /* Conflicts */
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/conflicts`) return json(CONFLICTS_VIEW);

  /* Activity */
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/activity`) return json(ACTIVITY);

  /* Releases */
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/releases`) {
    return json({ releases: [RELEASE, RELEASE_OLDER] });
  }
  if (method === "GET" && pathname === `/api/v1/projects/${PROJECT_ID}/releases/${RELEASE_ID}`) return json(RELEASE);

  /* Asset page */
  if (method === "GET" && pathname === `/api/v1/assets/${ASSET_PID}`) return json(ASSET_PAGE);

  /* Search answer (a POST from the browser, behind the session cookie) */
  if (method === "POST" && pathname === "/api/v1/search") {
    const query = request?.postDataJSON?.()?.query ?? SEARCH_QUERY;
    return json({ search_id: "vr-search-1", answer: { ...SEARCH_ANSWER, query } });
  }

  /* Research profile */
  if (method === "GET" && pathname === `/api/v1/users/${ALICE_ID}/profile`) return json(PROFILE);
  if (method === "GET" && pathname === `/api/v1/users/${ALICE_ID}/research-profile`) {
    return json({ user_id: ALICE_ID, handle: "alice", display_name: "Alice Guo", entries: [] });
  }

  return json(ERROR("NOT_FOUND", `no fixture for ${method} ${pathname}`), 404);
}

/** Install the mock on a page (context-level: nothing escapes to real DNS). */
export function installApiMock(page, onUnmatched) {
  return page.context().route(`${API}/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const answer = respond(request.method(), url.pathname, url, request);
    if (answer.status === 404 && url.pathname !== "/api/v1/auth/session") {
      onUnmatched?.(`${request.method()} ${url.pathname}`);
    }
    return route.fulfill({
      status: answer.status,
      contentType: "application/json",
      body: JSON.stringify(answer.body),
    });
  });
}
