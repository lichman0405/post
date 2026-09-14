/**
 * Unit tests for lib/pulls.ts (T0403) — the pull-request client the
 * Pull requests tab renders with. Node's type stripping runs this file
 * directly (node --test apps/web/lib/*.test.mjs via scripts/web-unit-tests.sh),
 * so it must stay import-free outside node:test / node:assert.
 *
 * The fake fetch mirrors lib/projects.test.mjs: a routes map keyed
 * "METHOD path" serves canned JSON, and every call is recorded so tests
 * can assert method + credentials on the wire.
 */

import test from "node:test";
import assert from "node:assert/strict";

import {
  ApiError,
  createPullsClient,
  dimensionLabel,
  INTEGRITY_DIMENSIONS,
  messageForPullRequestCode,
} from "./pulls.ts";

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

test("messageForPullRequestCode maps the stable codes and falls back", () => {
  assert.match(messageForPullRequestCode("PULL_REQUEST_NOT_FOUND"), /does not exist/);
  assert.match(messageForPullRequestCode("VALIDATION_FAILED"), /could not be processed/i);
  assert.match(messageForPullRequestCode("PROJECT_NOT_FOUND"), /does not exist/);
  assert.match(messageForPullRequestCode("SERVICE_UNAVAILABLE"), /temporarily unavailable/);
  assert.match(messageForPullRequestCode("SOMETHING_ELSE"), /something went wrong/i);
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
  assert.equal(dimensionLabel("schema"), "Schema");
  assert.equal(dimensionLabel("provenance"), "Provenance");
  assert.equal(dimensionLabel("dependency"), "Dependency");
  assert.equal(dimensionLabel("rights"), "Rights");
  assert.equal(dimensionLabel("visibility"), "Visibility");
  assert.equal(dimensionLabel("blob"), "Blob references");
  assert.equal(dimensionLabel("something-new"), "something-new");
});
