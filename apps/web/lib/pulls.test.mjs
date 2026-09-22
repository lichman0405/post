/**
 * Unit tests for lib/pulls.ts (T0403, extended by T0408, T1105) — the
 * pull-request client the Pull requests tab renders with. Node's type
 * stripping runs this file directly
 * (node --test apps/web/lib/*.test.mjs via scripts/web-unit-tests.sh), so
 * it must stay free of runtime imports outside node:test / node:assert —
 * ./i18n.ts is the one exception, and it is itself import-free by
 * construction (see its header), which is what lets these tests resolve the
 * same sentences the pages render.
 *
 * The fake fetch mirrors lib/projects.test.mjs: a routes map keyed
 * "METHOD path" serves canned JSON, and every call is recorded so tests
 * can assert method + credentials on the wire.
 */

import test from "node:test";
import assert from "node:assert/strict";

import { translate } from "./i18n.ts";

import {
  ApiError,
  assessPullRisks,
  createPullsClient,
  DEFAULT_PULL_TAB,
  diffTotals,
  dimensionLabelKey,
  evidenceChanges,
  INTEGRITY_DIMENSIONS,
  isStaleReview,
  knowledgeChanges,
  latestHeadReview,
  pullRequestCodeKey,
  PULL_TABS,
  pullTabLabelKey,
  rawPatchUrl,
  relationTypeOf,
} from "./pulls.ts";

/** The translator the pages pass in, resolved against the REAL en catalog.
 *
 *  T1105 moved these sentences into the catalog, and a test asserting the old
 *  literals would be asserting copy that no longer exists. Resolving through
 *  translate("en", …) keeps the assertions about the sentence a reader sees AND
 *  adds a property the literals could not have: the key has to exist in the
 *  catalog, in both locales (translate renders ⟦missing:key⟧ otherwise, and
 *  lib/i18n.test.mjs pins locale parity). */
const en = (key, vars) => translate("en", key, vars);

/** A canned wire PR (cmd/api/pullrequestshttp prPayload). */
const PR = {
  id: "pr-00000000-0000-0000-0000-000000000001",
  number: 7,
  title: "Propose the claim",
  body: "Adds the first claim.",
  state: "open",
  source_branch_id: "branch-src",
  target_branch_id: "branch-main",
  base_state_id: "state-base",
  proposed_state_id: "state-prop",
  created_by: "alice",
  created_at: "2026-09-15T10:00:00Z",
};

/** A canned machine integrity report (internal/rsg/integrity Report). */
const REPORT = {
  kind: "integrity",
  project_id: "project-1",
  pr_number: 7,
  base_state_id: "state-base",
  proposed_state_id: "state-prop",
  verdict: "pass_with_warnings",
  results: [
    {
      dimension: "schema",
      check: "schema_registered",
      severity: "blocking",
      passed: true,
      subject: "claim",
      why: "schema claim/v1 is registered",
    },
    {
      dimension: "rights",
      check: "rights_pin_inherits_default",
      severity: "warning",
      passed: false,
      subject: "claim",
      detail: "no visibility policy pinned",
      why: "the version carries no pin, so the default policy applies",
    },
  ],
  explanation: "the proposal passes machine integrity with warnings",
  computed_at: "2026-09-15T10:05:00Z",
};

/**
 * Fake fetch: routes map of "METHOD path" -> { status?, body }. Records
 * every call so tests can assert method + credentials on the wire.
 */
function fakeFetch(routes) {
  const calls = [];
  const fn = async (input, init = {}) => {
    calls.push({ input, init });
    const key = `${init.method ?? "GET"} ${input}`;
    const route = routes[key];
    if (route === undefined) {
      throw new Error(`unrouted request: ${key}`);
    }
    return {
      status: route.status ?? 200,
      json: async () => route.body,
    };
  };
  return { fn, calls };
}

const API = "https://api.example.test";

/** The client over the standard canned routes. */
function client() {
  const { fn, calls } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests`]: { body: [PR] },
    [`GET ${API}/api/v1/projects/p1/pull-requests/7`]: { body: PR },
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/checks`]: { body: REPORT },
  });
  return { client: createPullsClient(API, { fetch: fn }), calls };
}

test("list returns the project's pull requests with the session cookie", async () => {
  const { client: c, calls } = client();
  const list = await c.list("p1");
  assert.equal(list.length, 1);
  assert.deepEqual(list[0], PR);
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.method, "GET");
  assert.equal(calls[0].init.credentials, "include");
});

test("get returns one pull request by number", async () => {
  const { client: c } = client();
  const pr = await c.get("p1", 7);
  assert.equal(pr.number, 7);
  assert.equal(pr.base_state_id, "state-base");
  assert.equal(pr.proposed_state_id, "state-prop");
});

test("checks returns the integrity report with parsed results", async () => {
  const { client: c } = client();
  const report = await c.checks("p1", 7);
  assert.equal(report.kind, "integrity");
  assert.equal(report.verdict, "pass_with_warnings");
  assert.equal(report.computed_at, "2026-09-15T10:05:00Z");
  assert.equal(report.results.length, 2);
  assert.equal(report.results[0].check, "schema_registered");
  assert.equal(report.results[0].severity, "blocking");
  assert.equal(report.results[1].check, "rights_pin_inherits_default");
  assert.equal(report.results[1].severity, "warning");
  assert.equal(report.results[1].passed, false);
});

test("a 404 with the stable envelope surfaces as ApiError with its code", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests/99`]: {
      status: 404,
      body: { code: "PULL_REQUEST_NOT_FOUND", message: "pull request 99 does not exist" },
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(
    () => c.get("p1", 99),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "PULL_REQUEST_NOT_FOUND");
      assert.equal(err.status, 404);
      assert.equal(err.retryable, false);
      return true;
    },
  );
});

test("a 503 defaults to retryable and keeps its code", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests`]: {
      status: 503,
      body: { code: "SERVICE_UNAVAILABLE", message: "down for maintenance" },
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(
    () => c.list("p1"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "SERVICE_UNAVAILABLE");
      assert.equal(err.retryable, true);
      return true;
    },
  );
});

test("a non-JSON error body becomes a generic ApiError", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests`]: {
      status: 502,
      body: "upstream exploded",
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(
    () => c.list("p1"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "UNKNOWN");
      assert.equal(err.status, 502);
      return true;
    },
  );
});

test("a non-array list response trips the shape guard", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests`]: { body: { pull_requests: [] } },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(
    () => c.list("p1"),
    /unexpected pull request list response shape/,
  );
});

test("a pull request missing its number trips the shape guard", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7`]: {
      body: { ...PR, number: "seven" },
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(() => c.get("p1", 7), /unexpected pull request response shape/);
});

test("a pull request missing a rendered field trips the shape guard", async () => {
  // The detail page renders body and created_by and keys on the branch
  // ids: a response omitting (or mistyping) any of them must be rejected
  // by the guard, not rendered as undefined.
  const fields = ["body", "created_by", "source_branch_id", "target_branch_id"];
  for (const field of fields) {
    const missing = { ...PR };
    delete missing[field];
    const cases = [
      ["missing", missing],
      ["not a string", { ...PR, [field]: 7 }],
    ];
    for (const [label, body] of cases) {
      const { fn } = fakeFetch({
        [`GET ${API}/api/v1/projects/p1/pull-requests`]: { body: [body] },
        [`GET ${API}/api/v1/projects/p1/pull-requests/7`]: { body },
      });
      const c = createPullsClient(API, { fetch: fn });
      await assert.rejects(
        () => c.get("p1", 7),
        /unexpected pull request response shape/,
        `get: ${field} ${label}`,
      );
      await assert.rejects(
        () => c.list("p1"),
        /unexpected pull request response shape/,
        `list: ${field} ${label}`,
      );
    }
  }
});

test("a report of the wrong kind trips the shape guard", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/checks`]: {
      body: { ...REPORT, kind: "opinion" },
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(() => c.checks("p1", 7), /unexpected integrity report shape/);
});

test("a check result with a non-boolean passed trips the shape guard", async () => {
  const { fn } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/checks`]: {
      body: { ...REPORT, results: [{ ...REPORT.results[0], passed: "yes" }] },
    },
  });
  const c = createPullsClient(API, { fetch: fn });
  await assert.rejects(() => c.checks("p1", 7), /unexpected check result shape/);
});

test("pullRequestCodeKey maps the stable codes, with a generic default", () => {
  assert.match(en(pullRequestCodeKey("PULL_REQUEST_NOT_FOUND")), /does not exist/);
  assert.match(en(pullRequestCodeKey("VALIDATION_FAILED")), /could not be processed/i);
  assert.match(en(pullRequestCodeKey("PROJECT_NOT_FOUND")), /does not exist/);
  assert.match(en(pullRequestCodeKey("SERVICE_UNAVAILABLE")), /temporarily unavailable/);
  assert.match(en(pullRequestCodeKey("SOMETHING_ELSE")), /something went wrong/i);
});

test("the canonical dimensions keep report order with human labels", () => {
  assert.deepEqual(INTEGRITY_DIMENSIONS, [
    "schema",
    "provenance",
    "dependency",
    "rights",
    "visibility",
    "blob",
  ]);
  assert.equal(en(dimensionLabelKey("schema")), "Schema");
  assert.equal(en(dimensionLabelKey("provenance")), "Provenance");
  assert.equal(en(dimensionLabelKey("dependency")), "Dependency");
  assert.equal(en(dimensionLabelKey("rights")), "Rights");
  assert.equal(en(dimensionLabelKey("visibility")), "Visibility");
  assert.equal(en(dimensionLabelKey("blob")), "Blob references");
  assert.equal(dimensionLabelKey("something-new"), null, "a dimension this client was not taught has no key; the page renders the raw value");
});

/* ==================== T0408: the diff, the reviews, the risks ==================== */

/** One object version (internal/rsg/manifest ObjectVersion). */
function objectVersion({ id, objectId, objectType, versionNo, stateId, title, visibilityPolicyId = null }) {
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
    created_by: "alice",
    created_at: "2026-09-15T10:00:00Z",
  };
}

/** One relation version (internal/rsg/manifest RelationVersion). */
function relationVersion({ id, relationId, relationType, versionNo, stateId }) {
  return {
    id,
    relation_id: relationId,
    version_no: versionNo,
    state_id: stateId,
    relation_type: relationType,
    source_object_version_id: "ver-evidence-1",
    target_object_version_id: "ver-claim-1",
    payload: {},
    integrity_hash: `hash-${id}`,
    created_by: "alice",
    created_at: "2026-09-15T10:00:00Z",
  };
}

/** One object change (internal/rsg/diff ObjectChange). */
function objectChange({
  objectId,
  objectType,
  kind,
  title,
  changedFields = [],
  targetMoved = false,
  visibilityPolicyId = null,
}) {
  return {
    object_id: objectId,
    object_type: objectType,
    kind,
    base_version: kind === "created" ? null : objectVersion({
      id: `ver-base-${objectId}`,
      objectId,
      objectType,
      versionNo: 1,
      stateId: "state-base",
      title,
    }),
    source_version: objectVersion({
      id: `ver-src-${objectId}`,
      objectId,
      objectType,
      versionNo: 2,
      stateId: "state-prop",
      title,
      visibilityPolicyId,
    }),
    target_version: null,
    target_moved: targetMoved,
    changed_fields: changedFields,
    target_changed_fields: [],
    new_versions: [],
  };
}

/** One relation change (internal/rsg/diff RelationChange). */
function relationChange({ relationId, relationType, kind, targetMoved = false }) {
  return {
    relation_id: relationId,
    kind,
    base_version: null,
    source_version: relationVersion({
      id: `ver-src-${relationId}`,
      relationId,
      relationType,
      versionNo: 1,
      stateId: "state-prop",
    }),
    target_version: null,
    target_moved: targetMoved,
    changed_fields: kind === "created" ? [] : ["payload"],
    target_changed_fields: [],
    new_versions: [],
  };
}

/**
 * A canned three-way diff with one change per bucket the page
 * distinguishes: a knowledge object, an evidence object, a structural
 * object, an evidence relation and a knowledge relation.
 */
const DIFF = {
  format_version: "v1",
  project_id: "p1",
  base: { id: "state-base", git_ref: "b00b00" },
  source: { id: "state-prop", git_ref: "c0ffee" },
  target: { id: "state-head", git_ref: "c0ffee02" },
  object_changes: [
    objectChange({
      objectId: "obj-claim",
      objectType: "claim",
      kind: "updated",
      title: "The alloy is single phase",
      changedFields: ["payload"],
    }),
    objectChange({
      objectId: "obj-evidence",
      objectType: "evidence_assertion",
      kind: "created",
      title: "Thermal run 7",
    }),
    objectChange({
      objectId: "obj-protocol",
      objectType: "protocol",
      kind: "updated",
      title: "Synthesis protocol",
      changedFields: ["payload"],
      targetMoved: true,
    }),
  ],
  relation_changes: [
    relationChange({ relationId: "rel-supports", relationType: "supports", kind: "created" }),
    relationChange({ relationId: "rel-addresses", relationType: "addresses_question", kind: "created" }),
  ],
  file_diff_refs: [
    { kind: "source", base_git_ref: "b00b00", head_git_ref: "c0ffee" },
    { kind: "target", base_git_ref: "b00b00", head_git_ref: "c0ffee02" },
  ],
  summary: {
    objects_created: 1,
    objects_updated: 2,
    objects_aborted: 0,
    objects_reopened: 0,
    relations_created: 2,
    relations_updated: 0,
  },
};

/** One recorded review (cmd/api/reviewhttp reviewPayload). */
function wireReview({ id, kind, decision, reviewedStateId, createdAt = "2026-09-15T11:00:00Z" }) {
  return {
    id,
    pull_request_id: PR.id,
    reviewer_id: "00000000-0000-4000-8000-000000000002",
    kind,
    decision,
    reviewed_state_id: reviewedStateId,
    responsibility: "scientific-lead",
    body: "read the change",
    created_at: createdAt,
  };
}

/** The client over the T0408 routes (the shared CSRF storage is injected). */
function diffClient(routes, storage) {
  const { fn, calls } = fakeFetch({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: { body: DIFF },
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: { body: [] },
    [`POST ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: {
      body: wireReview({
        id: "rev-1",
        kind: "scientific",
        decision: "approved",
        reviewedStateId: PR.proposed_state_id,
      }),
    },
    ...routes,
  });
  return {
    client: createPullsClient(API, {
      fetch: fn,
      storage: storage ?? memoryStorage("csrf-token-1"),
    }),
    calls,
  };
}

/** A TokenStorage over a plain variable (the tests never touch a browser). */
function memoryStorage(initial = null) {
  let token = initial;
  return {
    get: () => token,
    set: (next) => {
      token = next;
    },
    clear: () => {
      token = null;
    },
  };
}

test("diff returns the three-way document with its changes parsed", async () => {
  const { client: c } = diffClient();
  const diff = await c.diff("p1", 7);
  assert.equal(diff.base.id, "state-base");
  assert.equal(diff.base.git_ref, "b00b00");
  assert.equal(diff.source.id, "state-prop");
  assert.equal(diff.target.id, "state-head");
  assert.equal(diff.object_changes.length, 3);
  assert.equal(diff.object_changes[0].object_type, "claim");
  assert.equal(diff.object_changes[0].source_version.title, "The alloy is single phase");
  assert.equal(diff.relation_changes.length, 2);
  assert.equal(relationTypeOf(diff.relation_changes[0]), "supports");
  assert.equal(diff.file_diff_refs.length, 2);
  assert.equal(diff.file_diff_refs[0].kind, "source");
  assert.equal(diff.file_diff_refs[0].head_git_ref, "c0ffee");
  assert.deepEqual(diffTotals(diff), { objects: 3, relations: 2, files: 2 });
});

test("a diff missing a summary count trips the shape guard", async () => {
  const broken = { ...DIFF, summary: { ...DIFF.summary } };
  delete broken.summary.objects_aborted;
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: { body: broken },
  });
  await assert.rejects(() => c.diff("p1", 7), /unexpected diff response shape/);
});

test("a diff whose object change has no source version title trips the guard", async () => {
  // The change lists render the source version's labels: a row without
  // them would render empty, so the guard refuses it here.
  const change = { ...DIFF.object_changes[0], source_version: { ...DIFF.object_changes[0].source_version } };
  delete change.source_version.title;
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: {
      body: { ...DIFF, object_changes: [change] },
    },
  });
  await assert.rejects(() => c.diff("p1", 7), /unexpected object change shape: source version has no title/);
});

test("a diff whose relation change has no source version trips the guard", async () => {
  const change = { ...DIFF.relation_changes[0], source_version: null };
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: {
      body: { ...DIFF, relation_changes: [change] },
    },
  });
  await assert.rejects(() => c.diff("p1", 7), /unexpected relation change shape/);
});

test("diff surfaces a missing state as the API's own code", async () => {
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: {
      status: 404,
      body: { code: "STATE_NOT_FOUND", message: "one of the compared states does not exist" },
    },
  });
  await assert.rejects(
    () => c.diff("p1", 7),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "STATE_NOT_FOUND");
      return true;
    },
  );
});

test("reviews returns the recorded rounds oldest first", async () => {
  const rounds = [
    wireReview({ id: "rev-1", kind: "scientific", decision: "changes_requested", reviewedStateId: "state-older" }),
    wireReview({ id: "rev-2", kind: "integrity", decision: "approved", reviewedStateId: PR.proposed_state_id }),
  ];
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: { body: rounds },
  });
  const list = await c.reviews("p1", 7);
  assert.equal(list.length, 2);
  assert.equal(list[0].kind, "scientific");
  assert.equal(list[0].reviewed_state_id, "state-older");
  assert.equal(list[1].decision, "approved");
});

test("a non-array reviews response trips the shape guard", async () => {
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: { body: { reviews: [] } },
  });
  await assert.rejects(() => c.reviews("p1", 7), /unexpected review list response shape/);
});

test("a review missing a rendered field trips the shape guard", async () => {
  const broken = wireReview({
    id: "rev-1",
    kind: "scientific",
    decision: "approved",
    reviewedStateId: PR.proposed_state_id,
  });
  delete broken.reviewed_state_id;
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: { body: [broken] },
  });
  await assert.rejects(() => c.reviews("p1", 7), /unexpected review response shape/);
});

test("submitReview POSTs exactly the decision with the session's CSRF token", async () => {
  const { client: c, calls } = diffClient();
  const recorded = await c.submitReview("p1", 7, {
    kind: "scientific",
    decision: "changes_requested",
    body: "the ablation is missing",
  });
  assert.equal(recorded.id, "rev-1");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.credentials, "include");
  assert.equal(calls[0].init.headers["X-CSRF-Token"], "csrf-token-1");
  assert.equal(calls[0].init.headers["Content-Type"], "application/json");
  // Exactly the three reviewed fields: the reviewer identity and the
  // responsibility label are the API's to derive, never the client's.
  assert.deepEqual(JSON.parse(calls[0].init.body), {
    kind: "scientific",
    decision: "changes_requested",
    body: "the ablation is missing",
  });
});

test("the reads never carry the CSRF token", async () => {
  // A read has no business presenting a write token: the guard's write
  // path is keyed on the header, and a read that sent one would blur the
  // two paths.
  const { client: c, calls } = diffClient();
  await c.diff("p1", 7);
  await c.reviews("p1", 7);
  assert.equal(calls.length, 2);
  for (const call of calls) {
    assert.equal(call.init.headers["X-CSRF-Token"], undefined);
    assert.equal(call.init.body, undefined);
  }
});

test("submitReview without a stored token sends no CSRF header", async () => {
  // The client never invents a token: an unauthenticated tab sends the
  // write without one and the API's guard refuses it — which is the
  // answer the page renders.
  const { client: c, calls } = diffClient({}, memoryStorage(null));
  assert.equal(c.csrfToken(), null);
  await c.submitReview("p1", 7, { kind: "integrity", decision: "approved", body: "" });
  assert.equal(calls[0].init.headers["X-CSRF-Token"], undefined);
});

test("a refused review surfaces the API's own code", async () => {
  for (const [status, code] of [
    [403, "AUTH_FORBIDDEN"],
    [409, "REVIEW_ALREADY_SUBMITTED"],
    [409, "PR_TERMINAL"],
  ]) {
    const { client: c } = diffClient({
      [`POST ${API}/api/v1/projects/p1/pull-requests/7/reviews`]: {
        status,
        body: { code, message: "refused", request_id: "r-1", retryable: false },
      },
    });
    await assert.rejects(
      () => c.submitReview("p1", 7, { kind: "scientific", decision: "approved", body: "" }),
      (err) => {
        assert.ok(err instanceof ApiError);
        assert.equal(err.code, code);
        assert.equal(err.status, status);
        assert.equal(err.retryable, false);
        return true;
      },
      code,
    );
  }
});

test("the six tabs are docs/06 §6's, and the raw file view is never the default", () => {
  assert.deepEqual(PULL_TABS, [
    "summary",
    "scientific",
    "knowledge",
    "evidence",
    "checks",
    "raw",
  ]);
  assert.equal(DEFAULT_PULL_TAB, "summary");
  assert.notEqual(DEFAULT_PULL_TAB, "raw");
  assert.equal(en(pullTabLabelKey("summary")), "Summary");
  assert.equal(en(pullTabLabelKey("scientific")), "Scientific changes");
  assert.equal(en(pullTabLabelKey("knowledge")), "Knowledge changes");
  assert.equal(en(pullTabLabelKey("evidence")), "Evidence");
  assert.equal(en(pullTabLabelKey("checks")), "Checks");
  assert.equal(en(pullTabLabelKey("raw")), "Raw Files");
});

test("the knowledge and evidence tabs partition the knowledge layer", () => {
  const knowledge = knowledgeChanges(DIFF);
  const evidence = evidenceChanges(DIFF);
  // The claim is a knowledge object; the evidence assertion and the
  // supports edge are evidence; addresses_question is knowledge-shaped
  // but asserts no evidence (relationcatalog CategoryKnowledge).
  assert.deepEqual(knowledge.objects.map((c) => c.object_id), ["obj-claim"]);
  assert.deepEqual(knowledge.relations.map((c) => c.relation_id), ["rel-addresses"]);
  assert.deepEqual(evidence.objects.map((c) => c.object_id), ["obj-evidence"]);
  assert.deepEqual(evidence.relations.map((c) => c.relation_id), ["rel-supports"]);
  // The protocol is neither: it shows on the scientific tab only.
  const claimed = new Set([
    ...knowledge.objects.map((c) => c.object_id),
    ...evidence.objects.map((c) => c.object_id),
  ]);
  assert.equal(claimed.has("obj-protocol"), false);
  assert.equal(claimed.size, 2);
  // Every relation lands in exactly one of the two buckets (the
  // structural ones in neither).
  const relationIds = DIFF.relation_changes.map((c) => c.relation_id);
  assert.deepEqual(
    [...knowledge.relations, ...evidence.relations].map((c) => c.relation_id).sort(),
    relationIds.sort(),
  );
});

test("latestHeadReview keeps the dimensions apart and ignores older heads", () => {
  const reviews = [
    wireReview({ id: "rev-1", kind: "scientific", decision: "changes_requested", reviewedStateId: "state-older" }),
    wireReview({ id: "rev-2", kind: "integrity", decision: "approved", reviewedStateId: PR.proposed_state_id }),
    wireReview({
      id: "rev-3",
      kind: "scientific",
      decision: "approved",
      reviewedStateId: PR.proposed_state_id,
      createdAt: "2026-09-15T12:00:00Z",
    }),
  ];
  const scientific = latestHeadReview(reviews, "scientific", PR.proposed_state_id);
  assert.equal(scientific.id, "rev-3");
  assert.equal(latestHeadReview(reviews, "integrity", PR.proposed_state_id).id, "rev-2");
  // The older-head decision is a recorded fact, not this head's state.
  assert.equal(isStaleReview(reviews[0], PR.proposed_state_id), true);
  assert.equal(isStaleReview(reviews[1], PR.proposed_state_id), false);
  // A dimension with nothing on this head answers nothing.
  assert.equal(latestHeadReview([reviews[0]], "scientific", PR.proposed_state_id), null);
});

test("latestHeadReview reads the API's order, not the timestamp as a string", () => {
  // The list is ordered by (created_at, id) — oldest first — and Go
  // serializes timestamps with a variable number of fractional digits
  // (a zero fraction is omitted, trailing zeros are trimmed). Compared as
  // strings, the whole second sorts ABOVE its own fraction, so a string
  // comparison picks the OLDER review and the page shows the wrong
  // decision for the head.
  const wholeSecond = wireReview({
    id: "rev-whole",
    kind: "scientific",
    decision: "approved",
    reviewedStateId: PR.proposed_state_id,
    createdAt: "2026-09-15T12:00:00Z",
  });
  const halfSecondLater = wireReview({
    id: "rev-half",
    kind: "scientific",
    decision: "changes_requested",
    reviewedStateId: PR.proposed_state_id,
    createdAt: "2026-09-15T12:00:00.5Z",
  });
  assert.equal(
    "2026-09-15T12:00:00Z" >= "2026-09-15T12:00:00.5Z",
    true,
    "the premise: as strings the whole second sorts above its own .5",
  );
  assert.equal(
    Date.parse("2026-09-15T12:00:00Z") < Date.parse("2026-09-15T12:00:00.5Z"),
    true,
    "the premise: in time the .5 is later",
  );
  assert.equal(
    latestHeadReview([wholeSecond, halfSecondLater], "scientific", PR.proposed_state_id).id,
    "rev-half",
  );

  // Same trap one fraction deeper: .5Z sorts above .55Z as a string while
  // being the earlier instant.
  const trim = wireReview({
    id: "rev-5",
    kind: "scientific",
    decision: "approved",
    reviewedStateId: PR.proposed_state_id,
    createdAt: "2026-09-15T12:00:00.5Z",
  });
  const trimLater = wireReview({
    id: "rev-55",
    kind: "scientific",
    decision: "changes_requested",
    reviewedStateId: PR.proposed_state_id,
    createdAt: "2026-09-15T12:00:00.55Z",
  });
  assert.equal("2026-09-15T12:00:00.5Z" >= "2026-09-15T12:00:00.55Z", true);
  assert.equal(
    latestHeadReview([trim, trimLater], "scientific", PR.proposed_state_id).id,
    "rev-55",
  );

  // Same instant, different rows: the API's (created_at, id) order is a
  // total order, so the last one it sent still wins.
  const tied = [
    wireReview({ id: "rev-a", kind: "scientific", decision: "approved", reviewedStateId: PR.proposed_state_id }),
    wireReview({ id: "rev-b", kind: "scientific", decision: "changes_requested", reviewedStateId: PR.proposed_state_id }),
  ];
  assert.equal(latestHeadReview(tied, "scientific", PR.proposed_state_id).id, "rev-b");

  // A later review of another dimension, or of an older head, is not the
  // successor of this one.
  const otherDimension = wireReview({
    id: "rev-c",
    kind: "integrity",
    decision: "approved",
    reviewedStateId: PR.proposed_state_id,
    createdAt: "2026-09-15T12:00:09Z",
  });
  const olderHead = wireReview({
    id: "rev-d",
    kind: "scientific",
    decision: "changes_requested",
    reviewedStateId: "state-older",
    createdAt: "2026-09-15T12:00:09Z",
  });
  assert.equal(
    latestHeadReview([halfSecondLater, otherDimension, olderHead], "scientific", PR.proposed_state_id).id,
    "rev-half",
  );
});

/** The clean baseline: a passing report, one structural change, no reviews. */
const CLEAN_REPORT = {
  ...REPORT,
  verdict: "pass",
  results: [REPORT.results[0]],
};
const CLEAN_DIFF = {
  ...DIFF,
  object_changes: [objectChange({
    objectId: "obj-protocol",
    objectType: "protocol",
    kind: "updated",
    title: "Synthesis protocol",
    changedFields: ["payload"],
  })],
  relation_changes: [],
};

test("a clean proposal raises no risk at all", () => {
  // The banner is absent when there is nothing to warn about — never a
  // green "all clear" the page cannot vouch for.
  assert.deepEqual(
    assessPullRisks({
      report: CLEAN_REPORT,
      diff: CLEAN_DIFF,
      reviews: [],
      headStateId: PR.proposed_state_id,
    }, en),
    [],
  );
});

test("a failed blocking check is a blocking risk, never softened", () => {
  const report = {
    ...CLEAN_REPORT,
    verdict: "blocked",
    results: [
      { ...CLEAN_REPORT.results[0], passed: false, severity: "blocking", detail: "unknown schema" },
      {
        dimension: "rights",
        check: "rights_pin_inherits_default",
        severity: "warning",
        passed: false,
        subject: "protocol",
        why: "no pin",
      },
    ],
  };
  const risks = assessPullRisks({
    report,
    diff: CLEAN_DIFF,
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.equal(risks.length, 2);
  assert.equal(risks[0].severity, "blocking");
  assert.equal(risks[0].code, "RISK_BLOCKING_CHECK");
  assert.match(risks[0].title, /Schema check failed: schema_registered \(claim\)/);
  assert.equal(risks[0].detail, "unknown schema");
  assert.equal(risks[0].tab, "checks");
  assert.equal(risks[1].severity, "warning");
  assert.equal(risks[1].code, "RISK_WARNING_CHECK");
  // The machine's reason rides along when it gave no detail.
  assert.equal(risks[1].detail, "no pin");
});

test("a blocked verdict with no blocking row still leads with a blocking risk", () => {
  const risks = assessPullRisks({
    report: { ...CLEAN_REPORT, verdict: "blocked" },
    diff: CLEAN_DIFF,
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.equal(risks.length, 1);
  assert.equal(risks[0].severity, "blocking");
  assert.equal(risks[0].code, "RISK_INTEGRITY_VERDICT_BLOCKED");
});

test("a changes_requested review blocks, but only on the head proposed now", () => {
  const onHead = assessPullRisks({
    report: CLEAN_REPORT,
    diff: CLEAN_DIFF,
    reviews: [
      wireReview({
        id: "rev-1",
        kind: "scientific",
        decision: "changes_requested",
        reviewedStateId: PR.proposed_state_id,
      }),
    ],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.equal(onHead.length, 1);
  assert.equal(onHead[0].severity, "blocking");
  assert.equal(onHead[0].code, "RISK_CHANGES_REQUESTED");
  assert.equal(onHead[0].tab, "summary");

  // The same decision on an older head is stale: the author has since
  // moved the head, so it is not a risk about what is proposed now.
  const stale = assessPullRisks({
    report: CLEAN_REPORT,
    diff: CLEAN_DIFF,
    reviews: [
      wireReview({
        id: "rev-1",
        kind: "scientific",
        decision: "changes_requested",
        reviewedStateId: "state-older",
      }),
    ],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.deepEqual(stale, []);
});

test("the target branch having moved the same entries is a risk", () => {
  const risks = assessPullRisks({
    report: CLEAN_REPORT,
    diff: DIFF,
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  const moved = risks.find((r) => r.code === "RISK_TARGET_MOVED");
  assert.ok(moved, "expected a target-moved risk");
  assert.equal(moved.severity, "warning");
  assert.equal(moved.tab, "scientific");
  assert.match(moved.detail, /obj-protocol/);
  // Exactly the one entry moved on both sides.
  assert.equal(DIFF.object_changes.filter((c) => c.target_moved).length, 1);
});

test("aborts, schema changes and visibility changes each raise their own risk", () => {
  const diff = {
    ...DIFF,
    object_changes: [
      objectChange({ objectId: "obj-old", objectType: "claim", kind: "aborted", title: "Old claim" }),
      objectChange({
        objectId: "obj-claim",
        objectType: "claim",
        kind: "updated",
        title: "The alloy is single phase",
        changedFields: ["schema_ref", "visibility_policy_id"],
      }),
    ],
    relation_changes: [],
  };
  const risks = assessPullRisks({
    report: CLEAN_REPORT,
    diff,
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  const codes = risks.map((r) => r.code);
  assert.deepEqual(codes, ["RISK_OBJECTS_ABORTED", "RISK_SCHEMA_CHANGED", "RISK_VISIBILITY_CHANGED"]);
  for (const risk of risks) {
    assert.equal(risk.severity, "warning");
    assert.equal(risk.tab, "scientific");
  }
  assert.match(risks[0].detail, /Old claim/);
  // Every risk names a fact the API returned, never a judgement.
  for (const risk of risks) {
    assert.ok(risk.title.length > 0 && risk.detail.length > 0);
  }
});

test("an object CREATED with its own visibility policy is a visibility risk", () => {
  // The diff engine records no changed_fields for a creation — there is
  // no base value to differ from (internal/rsg/diff classifyObject
  // returns (created, nil)) — so the pin on the source version is the
  // only evidence that this merge decides who may read the object.
  const pinned = objectChange({
    objectId: "obj-new",
    objectType: "claim",
    kind: "created",
    title: "The second phase is metastable",
    visibilityPolicyId: "policy-restricted-1",
  });
  assert.deepEqual(pinned.changed_fields, [], "the premise: a creation carries no field diff");
  const risks = assessPullRisks({
    report: CLEAN_REPORT,
    diff: { ...DIFF, object_changes: [pinned], relation_changes: [] },
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.deepEqual(risks.map((r) => r.code), ["RISK_VISIBILITY_CHANGED"]);
  assert.equal(risks[0].severity, "warning");
  assert.equal(risks[0].tab, "scientific");
  assert.match(risks[0].detail, /The second phase is metastable/);
  assert.match(risks[0].detail, /policy-restricted-1/);

  // An ordinary creation — no pin, so it inherits the project default —
  // raises nothing: flagging every new object would bury the real risks.
  const inherited = objectChange({
    objectId: "obj-plain",
    objectType: "claim",
    kind: "created",
    title: "The second phase is metastable",
  });
  assert.deepEqual(
    assessPullRisks({
      report: CLEAN_REPORT,
      diff: { ...DIFF, object_changes: [inherited], relation_changes: [] },
      reviews: [],
      headStateId: PR.proposed_state_id,
    }, en),
    [],
  );

  // Both classes at once, in the diff's own object order.
  const changed = objectChange({
    objectId: "obj-moved",
    objectType: "protocol",
    kind: "updated",
    title: "Synthesis protocol",
    changedFields: ["visibility_policy_id"],
    visibilityPolicyId: "policy-open-2",
  });
  const both = assessPullRisks({
    report: CLEAN_REPORT,
    diff: { ...DIFF, object_changes: [pinned, changed], relation_changes: [] },
    reviews: [],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.deepEqual(both.map((r) => r.code), ["RISK_VISIBILITY_CHANGED"]);
  assert.match(both[0].detail, /born pinned to policy-restricted-1/);
  assert.match(both[0].detail, /policy inherited default → policy-open-2/);
});

test("an object change without the rights pin trips the guard", async () => {
  // The pin is a risk input. Read as "inherits the default", a missing
  // key would hide exactly the case the visibility risk exists for, so
  // the guard refuses the document instead of under-reporting.
  const change = { ...DIFF.object_changes[0], source_version: { ...DIFF.object_changes[0].source_version } };
  delete change.source_version.visibility_policy_id;
  const { client: c } = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: {
      body: { ...DIFF, object_changes: [change] },
    },
  });
  await assert.rejects(
    () => c.diff("p1", 7),
    /unexpected object change shape: source version has no visibility_policy_id/,
  );
  // null stays accepted: it is the API's own "no pin" value.
  const unpinned = { ...DIFF.object_changes[0], source_version: { ...DIFF.object_changes[0].source_version, visibility_policy_id: null } };
  const ok = diffClient({
    [`GET ${API}/api/v1/projects/p1/pull-requests/7/diff`]: {
      body: { ...DIFF, object_changes: [unpinned] },
    },
  });
  const diff = await ok.client.diff("p1", 7);
  assert.equal(diff.object_changes[0].source_version.visibility_policy_id, null);
});

test("blocking risks lead the list, whatever order they were found in", () => {
  const risks = assessPullRisks({
    report: {
      ...CLEAN_REPORT,
      verdict: "blocked",
      results: [{ ...CLEAN_REPORT.results[0], passed: false, severity: "blocking" }],
    },
    // A warning-shaped fact is found first (the abort) — the blocking
    // check still leads, because a reviewer reads the merge-stoppers
    // first.
    diff: {
      ...DIFF,
      object_changes: [
        objectChange({ objectId: "obj-old", objectType: "claim", kind: "aborted", title: "Old claim" }),
      ],
      relation_changes: [],
    },
    reviews: [
      wireReview({
        id: "rev-1",
        kind: "integrity",
        decision: "changes_requested",
        reviewedStateId: PR.proposed_state_id,
      }),
    ],
    headStateId: PR.proposed_state_id,
  }, en);
  assert.deepEqual(
    risks.map((r) => r.severity),
    ["blocking", "blocking", "warning"],
  );
});

test("rawPatchUrl targets the Files raw-diff channel with an encoded sha", () => {
  assert.equal(
    rawPatchUrl("https://api.example.test", "p/1", "c0ffee02"),
    "https://api.example.test/api/v1/projects/p%2F1/files/diff?sha=c0ffee02",
  );
});

test("pullRequestCodeKey covers the review surface's codes", () => {
  assert.match(en(pullRequestCodeKey("STATE_NOT_FOUND")), /no longer exists/);
  assert.match(en(pullRequestCodeKey("REVIEW_ALREADY_SUBMITTED")), /already recorded/);
  assert.match(en(pullRequestCodeKey("PR_TERMINAL")), /closed/);
  assert.match(en(pullRequestCodeKey("AUTH_FORBIDDEN")), /not permitted/);
  assert.match(en(pullRequestCodeKey("AUTH_UNAUTHENTICATED")), /sign in/i);
  assert.match(en(pullRequestCodeKey("CSRF_FAILED")), /cross-site/i);
});
