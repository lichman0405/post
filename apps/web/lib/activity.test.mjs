/**
 * Tests for the client Activity module (apps/web/lib/activity.ts).
 *
 * Plain-ESM test file like profile.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (activity.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/activity.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createActivityClient,
  messageForAuditCode,
} from "./activity.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({ method, path, credentials: init.credentials });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });

const ENTRY = {
  id: "0a0b0c0d-0000-4000-8000-000000000001",
  actor_id: "02040608-0a0c-4e10-9214-16181a1c1e20",
  actor_handle: "alice",
  actor_display_name: "Alice Researcher",
  via: "session",
  action: "org.created",
  target_ref: "organization:0a0b0c0d-1111-4000-8000-000000000002",
  project_id: null,
  organization_id: "0a0b0c0d-1111-4000-8000-000000000002",
  correlation_id: "corr-123",
  after_summary: { slug: "acme-labs", name: "Acme Research" },
  occurred_at: "2026-09-12T10:00:00Z",
};

test("projectActivity reads the feed with the session cookie, no CSRF header", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ok({ entries: [ENTRY], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.projectActivity("p1");
  assert.equal(page.entries.length, 1);
  assert.equal(page.entries[0].action, "org.created");
  assert.equal(page.entries[0].after_summary.slug, "acme-labs");
  assert.equal(page.next_cursor, null);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].credentials, "include");
});

test("orgActivity renders the organization feed path", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/organizations/o1/activity": () =>
      ok({ entries: [{ ...ENTRY, action: "org.member.invited" }], next_cursor: "tok" }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.orgActivity("o1");
  assert.equal(page.entries[0].action, "org.member.invited");
  assert.equal(page.next_cursor, "tok");
  assert.equal(calls[0].path, "/api/v1/organizations/o1/activity");
});

test("limit and cursor render as query parameters, cursor URL-encoded", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor": () =>
      ok({ entries: [], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.projectActivity("p1", { limit: 3, cursor: "cur+sor" });
  assert.deepEqual(page.entries, []);
  assert.equal(calls[0].path, "/api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor");
});

test("a denied feed surfaces the stable envelope as ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/organizations/o1/activity": () => ({
      status: 404,
      json: async () => ({ code: "ORG_NOT_FOUND", message: "organization not found" }),
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.orgActivity("o1"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "ORG_NOT_FOUND");
      assert.equal(err.status, 404);
      assert.equal(err.retryable, false);
      return true;
    },
  );
});

test("a non-envelope failure still throws ApiError with a generic code", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ({
      status: 500,
      json: async () => {
        throw new Error("not json");
      },
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    (err) => err instanceof ApiError && err.code === "UNKNOWN",
  );
});

test("a 3xx answer surfaces as a stable ApiError, never a shape error", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ({
      status: 302,
      json: async () => {
        throw new Error("no body on a redirect");
      },
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    (err) => err instanceof ApiError && err.code === "UNKNOWN" && err.status === 302,
  );
});

test("a malformed entry shape is rejected, not silently trusted", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () =>
      ok({ entries: [{ id: "x" }], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    /unexpected activity entry shape/,
  );
});

test("messageForAuditCode maps the stable codes to human text", () => {
  assert.equal(messageForAuditCode("PROJECT_NOT_FOUND"), "This project is not visible to you.");
  assert.equal(messageForAuditCode("ORG_NOT_FOUND"), "This organization is not visible to you.");
  assert.equal(messageForAuditCode("AUTH_UNAUTHENTICATED"), "Sign in to view this activity.");
  assert.equal(messageForAuditCode("METHOD_NOT_ALLOWED"), "The audit log is read-only.");
  assert.equal(messageForAuditCode("SERVICE_UNAVAILABLE"), "Activity is temporarily unavailable. Please try again later.");
  assert.equal(messageForAuditCode("NOPE"), "Something went wrong. Please try again.");
});
