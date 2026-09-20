/**
 * Tests for the client discussions module (apps/web/lib/discussions.ts).
 *
 * Plain-ESM test file like milestones.test.mjs: Node ≥ 23.6 runs the
 * imported .ts chain through type stripping (discussions.ts is
 * import-free by construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/discussions.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  commentBodyForDisplay,
  createDiscussionsClient,
  isKnownPromotionKind,
  isKnownTargetKind,
  isTombstone,
  messageForDiscussionCode,
  parsePromotionRef,
  promotionDisplayName,
  promotionRef,
  targetDisplayName,
} from "./discussions.ts";

const API = "http://127.0.0.1:8080";
const PROJECT_ID = "99999999-8888-4777-8666-555555555555";
const THREAD_ID = "11111111-2222-4333-8444-555555555555";
const COMMENT_ID = "22222222-3333-4444-8555-666666666666";
const ALICE = "33333333-2222-4111-8111-555555555555";

const THREAD = {
  id: THREAD_ID,
  project_id: PROJECT_ID,
  target_type: "project",
  target_id: PROJECT_ID,
  created_by: ALICE,
  created_at: "2026-09-19T09:00:00Z",
  comment_count: 2,
  last_comment_at: "2026-09-19T09:05:00Z",
};

const LIVE_COMMENT = {
  id: COMMENT_ID,
  thread_id: THREAD_ID,
  project_id: PROJECT_ID,
  body: "Shall we try a lower synthesis temperature?",
  created_by: ALICE,
  created_at: "2026-09-19T09:01:00Z",
  deleted: false,
  deleted_at: null,
  deleted_by: null,
};

const WITHDRAWN_COMMENT = {
  ...LIVE_COMMENT,
  id: "44444444-5555-4666-8777-888888888888",
  body: null,
  deleted: true,
  deleted_at: "2026-09-19T09:10:00Z",
  deleted_by: ALICE,
};

const PROMOTION = {
  id: "55555555-6666-4777-8888-999999999999",
  project_id: PROJECT_ID,
  thread_id: THREAD_ID,
  comment_id: COMMENT_ID,
  promoted_kind: "hypothesis",
  promoted_ref: `hypothesis:66666666-7777-4888-8999-aaaaaaaaaaaa`,
  promoted_by: ALICE,
  promoted_at: "2026-09-19T09:20:00Z",
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
const created = (payload) => () => ({ status: 201, json: async () => payload });
const failWith = (status, payload) => () => ({ status, json: async () => payload });

test("listThreads renders the target query and decodes the envelope", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/discussions?target_type=project&target_id=${PROJECT_ID}`]: ok({
      threads: [THREAD],
    }),
  });
  const client = createDiscussionsClient(API, { fetch: fetchFn });

  const threads = await client.listThreads(PROJECT_ID, "project", PROJECT_ID);
  assert.equal(threads.length, 1);
  assert.equal(threads[0].target_type, "project");
  assert.equal(threads[0].comment_count, 2);
  assert.equal(calls[0].credentials, "include");
});

test("getThread keeps a withdrawn comment and renders its tombstone", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}`]: ok({
      thread: THREAD,
      comments: [LIVE_COMMENT, WITHDRAWN_COMMENT],
    }),
  });
  const client = createDiscussionsClient(API, { fetch: fetchFn });

  const detail = await client.getThread(PROJECT_ID, THREAD_ID);
  assert.equal(detail.comments.length, 2);
  assert.equal(commentBodyForDisplay(detail.comments[0]), LIVE_COMMENT.body);
  assert.equal(isTombstone(detail.comments[0]), false);
  assert.equal(isTombstone(detail.comments[1]), true);
  assert.equal(commentBodyForDisplay(detail.comments[1]), "This comment was withdrawn.");
  // The row is still there with the time it was withdrawn: nothing disappears.
  assert.equal(detail.comments[1].deleted_at, "2026-09-19T09:10:00Z");
});

test("openThread posts the target and the opening comment", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions`]: created({
      thread: THREAD,
      comments: [LIVE_COMMENT],
    }),
  });
  const client = createDiscussionsClient(API, {
    fetch: fetchFn,
    csrfToken: () => "csrf-1",
  });

  const detail = await client.openThread(PROJECT_ID, {
    targetType: "project",
    targetId: PROJECT_ID,
    body: LIVE_COMMENT.body,
  });
  assert.equal(detail.thread.id, THREAD_ID);
  assert.deepEqual(JSON.parse(calls[0].body), {
    target_type: "project",
    target_id: PROJECT_ID,
    body: LIVE_COMMENT.body,
  });
  assert.equal(calls[0].headers["X-CSRF-Token"], "csrf-1");
});

test("addComment and withdrawComment head the CSRF token", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments`]: created(LIVE_COMMENT),
    [`DELETE /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments/${COMMENT_ID}`]: ok(
      WITHDRAWN_COMMENT,
    ),
  });
  const client = createDiscussionsClient(API, {
    fetch: fetchFn,
    csrfToken: () => "csrf-2",
  });

  const comment = await client.addComment(PROJECT_ID, THREAD_ID, "another thought");
  assert.equal(comment.id, COMMENT_ID);
  assert.equal(calls[0].headers["X-CSRF-Token"], "csrf-2");
  assert.deepEqual(JSON.parse(calls[0].body), { body: "another thought" });

  const withdrawn = await client.withdrawComment(PROJECT_ID, THREAD_ID, COMMENT_ID);
  assert.equal(withdrawn.deleted, true);
  assert.equal(withdrawn.body, null);
  assert.equal(calls[1].method, "DELETE");
  assert.equal(calls[1].headers["X-CSRF-Token"], "csrf-2");
  assert.equal(calls[1].body, null);
});

test("promote renders one payload per kind and returns the provenance chain", async () => {
  const answer = {
    promotion: PROMOTION,
    thread: THREAD,
    comment: LIVE_COMMENT,
    scientific_object: {
      id: "66666666-7777-4888-8999-aaaaaaaaaaaa",
      project_id: PROJECT_ID,
      object_type: "hypothesis",
      version_id: "77777777-8888-4999-8aaa-bbbbbbbbbbbb",
      version_no: 1,
      state_id: "88888888-9999-4aaa-8bbb-cccccccccccc",
      title: "280K holds the same product",
      lifecycle_state: "active",
      payload: { statement: LIVE_COMMENT.body },
    },
  };
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments/${COMMENT_ID}/promotions`]:
      created(answer),
  });
  const client = createDiscussionsClient(API, {
    fetch: fetchFn,
    csrfToken: () => "csrf-3",
  });

  const result = await client.promote(PROJECT_ID, THREAD_ID, COMMENT_ID, {
    kind: "hypothesis",
    branchId: "99999999-aaaa-4bbb-8ccc-dddddddddddd",
    questionId: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
  });
  assert.equal(result.promotion.promoted_ref, `hypothesis:66666666-7777-4888-8999-aaaaaaaaaaaa`);
  assert.equal(result.scientific_object.version_no, 1);
  assert.equal(result.comment.created_by, ALICE);
  assert.deepEqual(JSON.parse(calls[0].body), {
    kind: "hypothesis",
    branch_id: "99999999-aaaa-4bbb-8ccc-dddddddddddd",
    hypothesis: { question_id: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee" },
  });

  // An Issue promotion carries its own declarations and no branch.
  const issueCalls = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments/${COMMENT_ID}/promotions`]:
      created({ ...answer, promotion: { ...PROMOTION, promoted_kind: "issue", promoted_ref: "issue:x" } }),
  });
  await createDiscussionsClient(API, { fetch: issueCalls.fetchFn }).promote(
    PROJECT_ID,
    THREAD_ID,
    COMMENT_ID,
    { kind: "issue", issueType: "question", title: "Does 280K hold?" },
  );
  assert.deepEqual(JSON.parse(issueCalls.calls[0].body), {
    kind: "issue",
    issue_type: "question",
    title: "Does 280K hold?",
  });

  // An external-evidence promotion carries the two version pins and the
  // assertion's vocabulary.
  const evidenceCalls = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments/${COMMENT_ID}/promotions`]:
      created({ ...answer, promotion: { ...PROMOTION, promoted_kind: "external_evidence" } }),
  });
  await createDiscussionsClient(API, { fetch: evidenceCalls.fetchFn }).promote(
    PROJECT_ID,
    THREAD_ID,
    COMMENT_ID,
    {
      kind: "external_evidence",
      branchId: "99999999-aaaa-4bbb-8ccc-dddddddddddd",
      evidence: {
        targetVersionRef: "object_version:aaaaaaaa-1111-4222-8333-444444444444",
        evidenceVersionRef: "bbbbbbbb-2222-4333-8444-555555555555",
        relation: "supports",
        evidenceType: "experimental",
        directness: "direct",
        inferenceNature: "causal",
      },
    },
  );
  assert.deepEqual(JSON.parse(evidenceCalls.calls[0].body), {
    kind: "external_evidence",
    branch_id: "99999999-aaaa-4bbb-8ccc-dddddddddddd",
    evidence: {
      target_version_ref: "object_version:aaaaaaaa-1111-4222-8333-444444444444",
      evidence_version_ref: "bbbbbbbb-2222-4333-8444-555555555555",
      relation: "supports",
      evidence_type: "experimental",
      directness: "direct",
      inference_nature: "causal",
    },
  });
});

test("promotionsForRef encodes the ref and decodes the list", async () => {
  const ref = PROMOTION.promoted_ref;
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/discussions/promotions?ref=${encodeURIComponent(ref)}`]: ok({
      promotions: [{ promotion: PROMOTION, thread: THREAD, comment: LIVE_COMMENT }],
    }),
  });
  const client = createDiscussionsClient(API, { fetch: fetchFn });

  const origins = await client.promotionsForRef(PROJECT_ID, ref);
  assert.equal(origins.length, 1);
  assert.equal(origins[0].promotion.id, PROMOTION.id);
  assert.equal(origins[0].comment.created_by, ALICE);
  assert.equal(calls[0].path.includes("promotions?ref="), true);
});

test("a refusal surfaces as ApiError with its stable code", async () => {
  const { fetchFn } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/discussions/${THREAD_ID}/comments/${COMMENT_ID}/promotions`]:
      failWith(403, {
        code: "DISCUSSION_FORBIDDEN",
        message: "the actor may not do this here",
        request_id: "req-1",
        retryable: false,
      }),
  });
  const client = createDiscussionsClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.promote(PROJECT_ID, THREAD_ID, COMMENT_ID, { kind: "issue", issueType: "question", title: "x" }),
    (err) => {
      assert.equal(err instanceof ApiError, true);
      assert.equal(err.code, "DISCUSSION_FORBIDDEN");
      assert.equal(err.status, 403);
      assert.equal(err.retryable, false);
      return true;
    },
  );
  // A 503 is retryable by default and reads as a plain sentence.
  assert.equal(
    messageForDiscussionCode("DISCUSSION_SERVICE_UNAVAILABLE"),
    "Discussion data is temporarily unavailable. Please try again later.",
  );
  assert.equal(
    messageForDiscussionCode("DISCUSSION_COMMENT_DELETED"),
    "This comment was withdrawn, so it cannot be promoted.",
  );
  assert.equal(
    messageForDiscussionCode("SOMETHING_NEW"),
    "Something went wrong while working with this discussion. Please try again.",
  );
});

test("kind helpers and ref parsing refuse what the API refuses", () => {
  assert.equal(isKnownTargetKind("project"), true);
  assert.equal(isKnownTargetKind("knowledge"), true);
  assert.equal(isKnownTargetKind("pull_request"), true);
  assert.equal(isKnownTargetKind("document"), false);
  assert.equal(targetDisplayName("pull_request"), "Pull request");
  assert.equal(targetDisplayName("document"), "document");

  assert.equal(isKnownPromotionKind("hypothesis"), true);
  assert.equal(isKnownPromotionKind("epic"), false);
  assert.equal(promotionDisplayName("external_evidence"), "External evidence proposal");

  assert.equal(promotionRef("issue", "abc"), "issue:abc");
  assert.deepEqual(parsePromotionRef("hypothesis:abc"), { kind: "hypothesis", id: "abc" });
  assert.equal(parsePromotionRef("epic:abc"), null);
  assert.equal(parsePromotionRef("issue"), null);
  assert.equal(parsePromotionRef("issue:"), null);
  assert.equal(parsePromotionRef(":abc"), null);
});
