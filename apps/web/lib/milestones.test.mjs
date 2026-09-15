/**
 * Tests for the client milestones module (apps/web/lib/milestones.ts).
 *
 * Plain-ESM test file like releases.test.mjs: Node ≥ 23.6 runs the
 * imported .ts chain through type stripping (milestones.ts is import-free
 * by construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/milestones.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createMilestonesClient,
  isKnownMilestoneKind,
  kindDisplayName,
  mayCreateMilestone,
  messageForMilestoneCode,
  milestoneDisplayName,
} from "./milestones.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({
      method,
      path,
      credentials: init.credentials,
      headers: init.headers ?? {},
      body: init.body ?? null,
    });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => () => ({ status: 200, json: async () => payload });

const MILESTONE = {
  id: "11111111-2222-4333-8444-555555555555",
  project_id: "99999999-8888-4777-8666-555555555555",
  kind: "paper_submitted",
  label: "JACS 2026",
  occurred_at: "2026-09-14T10:00:00Z",
  release_id: "77777777-6666-4555-8444-555555555555",
  created_by: "33333333-2222-4111-8111-555555555555",
  created_at: "2026-09-15T09:00:00Z",
};

const PROJECT_ID = MILESTONE.project_id;
const MILESTONE_ID = MILESTONE.id;

test("list decodes the milestones envelope", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/milestones`]: ok({ milestones: [MILESTONE] }),
  });
  const client = createMilestonesClient(API, { fetch: fetchFn });

  const milestones = await client.list(PROJECT_ID);
  assert.equal(milestones.length, 1);
  assert.equal(milestones[0].kind, "paper_submitted");
  assert.equal(milestones[0].label, "JACS 2026");
  assert.equal(milestones[0].occurred_at, "2026-09-14T10:00:00Z");
  assert.equal(calls[0].credentials, "include");
});

test("list accepts a null label and a null release link", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/milestones`]: ok({
      milestones: [{ ...MILESTONE, label: null, release_id: null }],
    }),
  });
  const client = createMilestonesClient(API, { fetch: fetchFn });

  const milestones = await client.list(PROJECT_ID);
  assert.equal(milestones[0].label, null);
  assert.equal(milestones[0].release_id, null);
});

test("get decodes one milestone", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/milestones/${MILESTONE_ID}`]: ok(MILESTONE),
  });
  const client = createMilestonesClient(API, { fetch: fetchFn });

  const milestone = await client.get(PROJECT_ID, MILESTONE_ID);
  assert.equal(milestone.kind, "paper_submitted");
});

test("create posts the body with csrf and idempotency headers", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/milestones`]: () => ({
      status: 201,
      json: async () => MILESTONE,
    }),
  });
  const client = createMilestonesClient(API, {
    fetch: fetchFn,
    csrfToken: () => "csrf-token-1",
  });

  const milestone = await client.create(PROJECT_ID, {
    kind: "custom",
    label: "First external replicate",
    occurredAt: "2026-09-14T10:00:00Z",
    releaseId: MILESTONE.release_id,
    idempotencyKey: "key-create-m1",
  });
  assert.equal(milestone.id, MILESTONE.id);
  assert.equal(calls.length, 1);
  const call = calls[0];
  assert.equal(call.method, "POST");
  assert.equal(call.headers["X-CSRF-Token"], "csrf-token-1");
  assert.equal(call.headers["Idempotency-Key"], "key-create-m1");
  assert.deepEqual(JSON.parse(call.body), {
    kind: "custom",
    label: "First external replicate",
    occurred_at: "2026-09-14T10:00:00Z",
    release_id: MILESTONE.release_id,
  });
});

test("create omits the idempotency header when no key is supplied", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/milestones`]: () => ({
      status: 201,
      json: async () => MILESTONE,
    }),
  });
  const client = createMilestonesClient(API, { fetch: fetchFn });

  await client.create(PROJECT_ID, { kind: "patent_filed", occurredAt: "2026-09-14T10:00:00Z" });
  assert.ok(!("Idempotency-Key" in calls[0].headers));
  // The optional fields serialize away when absent — JSON.stringify
  // drops undefined values, so the wire body names only the sent fields.
  assert.deepEqual(JSON.parse(calls[0].body), {
    kind: "patent_filed",
    occurred_at: "2026-09-14T10:00:00Z",
  });
});

test("errors surface the stable envelope", async () => {
  const { fetchFn } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/milestones`]: () => ({
      status: 404,
      json: async () => ({
        code: "MILESTONE_RELEASE_NOT_FOUND",
        message: "the named release does not exist in this project",
      }),
    }),
  });
  const client = createMilestonesClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.create(PROJECT_ID, { kind: "custom", label: "x", occurredAt: "2026-09-14T10:00:00Z", releaseId: "foreign" }),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "MILESTONE_RELEASE_NOT_FOUND");
      assert.equal(err.status, 404);
      return true;
    },
  );
});

test("kindDisplayName covers the canonical kinds and falls back raw", () => {
  assert.equal(kindDisplayName("candidate_selected"), "Candidate selected");
  assert.equal(kindDisplayName("paper_submitted"), "Paper submitted");
  assert.equal(kindDisplayName("patent_filed"), "Patent filed");
  assert.equal(kindDisplayName("external_validation"), "External validation");
  assert.equal(kindDisplayName("custom"), "Custom");
  assert.equal(kindDisplayName("wobble"), "wobble");
});

test("milestoneDisplayName prefers the custom label", () => {
  assert.equal(milestoneDisplayName("custom", "First replicate"), "First replicate");
  assert.equal(milestoneDisplayName("custom", null), "Custom");
  assert.equal(milestoneDisplayName("paper_submitted", null), "Paper submitted");
  assert.equal(milestoneDisplayName("paper_submitted", "JACS 2026"), "JACS 2026");
  assert.equal(milestoneDisplayName("paper_submitted", ""), "Paper submitted");
});

test("isKnownMilestoneKind mirrors the API vocabulary", () => {
  for (const kind of ["candidate_selected", "paper_submitted", "patent_filed", "external_validation", "custom"]) {
    assert.equal(isKnownMilestoneKind(kind), true, `kind ${kind} should be known`);
  }
  // "completed" is deliberately NOT a milestone kind — milestones are
  // timeline facts, never lifecycle states.
  assert.equal(isKnownMilestoneKind("completed"), false);
  assert.equal(isKnownMilestoneKind(""), false);
});

test("mayCreateMilestone mirrors the matrix", () => {
  assert.equal(mayCreateMilestone("owner"), true);
  assert.equal(mayCreateMilestone("maintainer"), true);
  assert.equal(mayCreateMilestone("contributor"), false);
  assert.equal(mayCreateMilestone("viewer"), false);
  assert.equal(mayCreateMilestone(null), false);
});

test("messageForMilestoneCode covers every stable code", () => {
  for (const code of [
    "MILESTONE_PROJECT_NOT_FOUND",
    "MILESTONE_NOT_FOUND",
    "MILESTONE_RELEASE_NOT_FOUND",
    "MILESTONE_FORBIDDEN",
    "MILESTONE_VALIDATION_FAILED",
    "SERVICE_UNAVAILABLE",
  ]) {
    const message = messageForMilestoneCode(code);
    assert.ok(typeof message === "string" && message.length > 0, `no message for ${code}`);
  }
  assert.ok(messageForMilestoneCode("WAT").length > 0);
});
