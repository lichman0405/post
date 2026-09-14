/**
 * Tests for the client conflicts module (apps/web/lib/conflicts.ts).
 *
 * Plain-ESM test file like files.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (conflicts.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/conflicts.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  RESOLUTION_KINDS,
  createConflictsClient,
  messageForConflictCode,
} from "./conflicts.ts";

const API = "http://127.0.0.1:8080";
const PID = "11111111-2222-4333-8444-555555555555";
const TRIPLE = {
  base_state_id: "bbbbbbbb-0000-4000-8000-000000000001",
  source_state_id: "bbbbbbbb-0000-4000-8000-000000000002",
  target_state_id: "bbbbbbbb-0000-4000-8000-000000000003",
};

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({
      method,
      path,
      headers: init.headers ?? null,
      body: init.body ?? null,
      credentials: init.credentials ?? null,
    });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });
const err = (status, code, message) => ({ status, json: async () => ({ code, message }) });

/** A minimal but shape-true report (cmd/api/conflicthttp + T0405 wire). */
const REPORT = {
  format_version: "v1",
  project_id: PID,
  diff: {
    format_version: "v1",
    project_id: PID,
    base: { id: TRIPLE.base_state_id, git_ref: "b00" },
    source: { id: TRIPLE.source_state_id, git_ref: "a11" },
    target: { id: TRIPLE.target_state_id, git_ref: "c22" },
    object_changes: [
      {
        object_id: "obj-1",
        object_type: "protocol",
        kind: "updated",
        base_version: {
          id: "v0",
          object_id: "obj-1",
          object_type: "protocol",
          version_no: 1,
          state_id: TRIPLE.base_state_id,
          branch_id: null,
          schema_ref: { id: "sch", version: "1" },
          title: "Synthesis protocol",
          lifecycle_state: "active",
          payload: { purpose: "synthesize", steps: [{ id: "s1", temperature: 300 }] },
          visibility_policy_id: null,
          integrity_hash: "h0",
          created_by: "u1",
          created_at: "2026-09-01T00:00:00Z",
        },
        source_version: {
          id: "v1",
          object_id: "obj-1",
          object_type: "protocol",
          version_no: 2,
          state_id: TRIPLE.source_state_id,
          branch_id: null,
          schema_ref: { id: "sch", version: "1" },
          title: "Synthesis protocol",
          lifecycle_state: "active",
          payload: { purpose: "synthesize", steps: [{ id: "s1", temperature: 350 }] },
          visibility_policy_id: null,
          integrity_hash: "h1",
          created_by: "u1",
          created_at: "2026-09-02T00:00:00Z",
        },
        target_version: {
          id: "v2",
          object_id: "obj-1",
          object_type: "protocol",
          version_no: 2,
          state_id: TRIPLE.target_state_id,
          branch_id: null,
          schema_ref: { id: "sch", version: "1" },
          title: "Synthesis protocol",
          lifecycle_state: "active",
          payload: { purpose: "synthesize", steps: [{ id: "s1", temperature: 400 }] },
          visibility_policy_id: null,
          integrity_hash: "h2",
          created_by: "u1",
          created_at: "2026-09-03T00:00:00Z",
        },
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
      object_id: "obj-1",
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
          detail: "both branches moved the same protocol step to different temperatures",
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
};

const VIEW = {
  report: REPORT,
  evidence: [
    {
      object_id: "obj-1",
      source_evidence: [],
      target_evidence: [
        {
          relation_type: "supports",
          evidence_type: "measurement",
          directness: "direct",
          reasoning_note: null,
          review_state: "accepted",
          evidence_object_id: "ev-1",
          evidence_object_type: "measurement",
          evidence_title: "Thermal run 7",
          evidence_object_version_no: 2,
        },
      ],
    },
  ],
  resolutions: [],
};

const DECISION = {
  target_kind: "object",
  target_id: "obj-1",
  code: "SCIENTIFIC_FIELD_DIVERGES",
  fields: ["payload"],
  payload_keys: ["steps"],
  other_object_id: null,
  kind: "accept_target",
  note: "thermal run 7 backs 400",
};

const RECORD = {
  id: "res-1",
  project_id: PID,
  base_state_id: TRIPLE.base_state_id,
  source_state_id: TRIPLE.source_state_id,
  target_state_id: TRIPLE.target_state_id,
  target_kind: "object",
  target_id: "obj-1",
  code: "SCIENTIFIC_FIELD_DIVERGES",
  fields: ["payload"],
  payload_keys: ["steps"],
  other_object_id: null,
  kind: "accept_target",
  note: "thermal run 7 backs 400",
  decided_by: "u1",
  decided_at: "2026-09-14T10:00:00Z",
  updated_at: "2026-09-14T10:00:00Z",
};

const GET_PATH =
  `/api/v1/projects/${PID}/conflicts` +
  `?base_state_id=${TRIPLE.base_state_id}` +
  `&source_state_id=${TRIPLE.source_state_id}` +
  `&target_state_id=${TRIPLE.target_state_id}`;

test("get requests the exact conflicts route with the pinned triple", async () => {
  const { fetchFn, calls } = fakeFetch({ [`GET ${GET_PATH}`]: () => ok(VIEW) });
  const client = createConflictsClient(API, { fetch: fetchFn });

  const view = await client.get(PID, TRIPLE);
  assert.equal(view.report.object_verdicts[0].conflicts[0].code, "SCIENTIFIC_FIELD_DIVERGES");
  assert.equal(view.evidence[0].target_evidence[0].evidence_title, "Thermal run 7");
  assert.deepEqual(view.resolutions, []);

  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].path, GET_PATH);
  assert.equal(calls[0].credentials, "include");
});

test("get decodes the stable envelope into ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`GET ${GET_PATH}`]: () => err(400, "VALIDATION_FAILED", "triple required"),
  });
  const client = createConflictsClient(API, { fetch: fetchFn });

  await assert.rejects(
    client.get(PID, TRIPLE),
    (e) => e instanceof ApiError && e.code === "VALIDATION_FAILED" && e.status === 400,
  );
});

test("get rejects a response missing the report/evidence/resolutions shape", async () => {
  const { fetchFn } = fakeFetch({ [`GET ${GET_PATH}`]: () => ok({ nope: true }) });
  const client = createConflictsClient(API, { fetch: fetchFn });

  await assert.rejects(client.get(PID, TRIPLE), /unexpected conflicts response shape/);
});

test("save PUTs the triple and decisions with the CSRF token", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`PUT /api/v1/projects/${PID}/resolutions`]: () => ok({ resolutions: [RECORD] }),
  });
  let stored = null;
  const storage = { get: () => stored, set: (t) => (stored = t), clear: () => (stored = null) };
  stored = "csrf-token-1";
  const client = createConflictsClient(API, { fetch: fetchFn, storage });

  const plan = await client.save(PID, TRIPLE, [DECISION]);
  assert.equal(plan.length, 1);
  assert.equal(plan[0].kind, "accept_target");
  assert.equal(plan[0].decided_by, "u1");

  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[0].path, `/api/v1/projects/${PID}/resolutions`);
  assert.equal(calls[0].headers["X-CSRF-Token"], "csrf-token-1");
  assert.equal(calls[0].headers["Content-Type"], "application/json");
  assert.equal(calls[0].credentials, "include");
  const body = JSON.parse(calls[0].body);
  assert.equal(body.base_state_id, TRIPLE.base_state_id);
  assert.equal(body.source_state_id, TRIPLE.source_state_id);
  assert.equal(body.target_state_id, TRIPLE.target_state_id);
  assert.deepEqual(body.resolutions, [DECISION]);
});

test("save sends no CSRF header when no token is stored", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`PUT /api/v1/projects/${PID}/resolutions`]: () => ok({ resolutions: [] }),
  });
  const client = createConflictsClient(API, { fetch: fetchFn });

  await client.save(PID, TRIPLE, [DECISION]);
  assert.equal(calls[0].headers["X-CSRF-Token"], undefined);
});

test("save surfaces the API's refusal codes", async () => {
  const { fetchFn } = fakeFetch({
    [`PUT /api/v1/projects/${PID}/resolutions`]: () =>
      err(409, "CONFLICT_NOT_FOUND", "stale report"),
  });
  const client = createConflictsClient(API, { fetch: fetchFn });

  await assert.rejects(
    client.save(PID, TRIPLE, [DECISION]),
    (e) => e instanceof ApiError && e.code === "CONFLICT_NOT_FOUND" && e.status === 409,
  );
});

test("RESOLUTION_KINDS is exactly the five human decisions — no average exists", () => {
  assert.deepEqual(RESOLUTION_KINDS, [
    "accept_source",
    "accept_target",
    "keep_both",
    "validation_branch",
    "unresolved",
  ]);
  assert.equal(RESOLUTION_KINDS.length, 5);
  for (const kind of RESOLUTION_KINDS) {
    assert.ok(!/avg|average|mean|merge|auto/i.test(kind), `kind ${kind} must not compute a value`);
  }
});

test("messageForConflictCode maps the stable codes", () => {
  assert.match(messageForConflictCode("CONFLICT_NOT_FOUND"), /changed since this page loaded/);
  assert.match(messageForConflictCode("AUTH_FORBIDDEN"), /not allowed/);
  assert.match(messageForConflictCode("CSRF_FAILED"), /cross-site/);
  assert.match(messageForConflictCode("STATE_NOT_FOUND"), /does not exist/);
  assert.match(messageForConflictCode("PROJECT_NOT_FOUND"), /does not exist/);
  assert.match(messageForConflictCode("VALIDATION_FAILED"), /not valid/);
  assert.match(messageForConflictCode("SERVICE_UNAVAILABLE"), /temporarily unavailable/);
  assert.match(messageForConflictCode("AUTH_UNAUTHENTICATED"), /Sign in/);
  assert.match(messageForConflictCode("???"), /Something went wrong/);
});
