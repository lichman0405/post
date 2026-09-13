/**
 * Tests for the client projects module (apps/web/lib/projects.ts).
 *
 * Plain-ESM test file like auth.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (projects.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/projects.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createProjectsClient,
  mayManageSettings,
  messageForProjectCode,
} from "./projects.ts";

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

const ok = (payload) => ({ status: 200, json: async () => payload });

const PROJECT = {
  id: "11111111-2222-4333-8444-555555555555",
  organization_id: null,
  program_id: null,
  slug: "alloy-lab",
  name: "Alloy Lab",
  purpose: "Design high-entropy alloys",
  activity_status: "planning",
  visibility: "private",
  main_frozen: true,
  git_repository_external_id: null,
  provision_status: "pending",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-13T08:00:00Z",
};

const MEMBERSHIP = {
  project_id: PROJECT.id,
  user_id: "00000000-0000-4000-8000-000000000001",
  role: "owner",
  created_at: "2026-09-13T08:00:00Z",
};

test("get reads one project with the session cookie (no CSRF on reads)", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}`]: async () => ok(PROJECT),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  const p = await client.get(PROJECT.id);
  assert.equal(p.slug, "alloy-lab");
  assert.equal(p.main_frozen, true);
  assert.equal(calls[0].credentials ?? "include", "include");
  assert.equal(calls[0].headers["X-CSRF-Token"], undefined);
});

test("get surfaces the existence-hiding envelope as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}`]: async () => ({
      status: 404,
      json: async () => ({ code: "PROJECT_NOT_FOUND", message: "project not found" }),
    }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.get(PROJECT.id),
    (err) => err instanceof ApiError && err.code === "PROJECT_NOT_FOUND" && err.status === 404,
  );
});

test("list decodes the projects envelope and rejects malformed shapes", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects": async () => ok({ projects: [PROJECT] }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  const list = await client.list();
  assert.equal(list.length, 1);
  assert.equal(list[0].name, "Alloy Lab");

  const broken = createProjectsClient(API, {
    fetch: fakeFetch({ "GET /api/v1/projects": async () => ok({ nope: [] }) }).fetchFn,
  });
  await assert.rejects(() => broken.list(), /unexpected project list response shape/);
});

test("myMembership returns the caller's role", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}/membership`]: async () => ok(MEMBERSHIP),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  const m = await client.myMembership(PROJECT.id);
  assert.equal(m.role, "owner");
  assert.equal(calls[0].credentials ?? "include", "include");
});

test("myMembership maps the 404 (no role) to null, not an error", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}/membership`]: async () => ({
      status: 404,
      json: async () => ({ code: "PROJECT_MEMBERSHIP_NOT_FOUND", message: "not a member" }),
    }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  assert.equal(await client.myMembership(PROJECT.id), null);
});

test("myMembership still surfaces other failures as ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}/membership`]: async () => ({
      status: 503,
      json: async () => ({ code: "SERVICE_UNAVAILABLE", message: "down" }),
    }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.myMembership(PROJECT.id),
    (err) => err instanceof ApiError && err.code === "SERVICE_UNAVAILABLE",
  );
});

test("mayManageSettings gates the Settings tab to owner/maintainer", () => {
  assert.equal(mayManageSettings("owner"), true);
  assert.equal(mayManageSettings("maintainer"), true);
  assert.equal(mayManageSettings("contributor"), false);
  assert.equal(mayManageSettings("viewer"), false);
  assert.equal(mayManageSettings(null), false);
});

const MEMBERS = [
  {
    user_id: "00000000-0000-4000-8000-000000000001",
    handle: "alice",
    display_name: "Alice Guo",
    role: "owner",
    joined_at: "2026-09-10",
  },
  {
    user_id: "00000000-0000-4000-8000-000000000002",
    handle: "bob",
    display_name: "Bob Chen",
    role: "viewer",
    joined_at: "2026-09-12",
  },
];

test("listMembers decodes the member envelope and rejects malformed shapes", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}/members`]: async () => ok({ members: MEMBERS }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  const members = await client.listMembers(PROJECT.id);
  assert.equal(members.length, 2);
  assert.equal(members[0].handle, "alice");
  assert.equal(members[0].role, "owner");
  assert.equal(members[0].joined_at, "2026-09-10");
  assert.equal(members[1].display_name, "Bob Chen");

  const broken = createProjectsClient(API, {
    fetch: fakeFetch({
      [`GET /api/v1/projects/${PROJECT.id}/members`]: async () => ok({ nope: [] }),
    }).fetchFn,
  });
  await assert.rejects(() => broken.listMembers(PROJECT.id), /unexpected member list response shape/);
});

test("listMembers surfaces SETTINGS_FORBIDDEN as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT.id}/members`]: async () => ({
      status: 403,
      json: async () => ({ code: "SETTINGS_FORBIDDEN", message: "no" }),
    }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.listMembers(PROJECT.id),
    (err) => err instanceof ApiError && err.code === "SETTINGS_FORBIDDEN" && err.status === 403,
  );
});

test("setMemberRole PUTs the role with the CSRF token from the provider", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`PUT /api/v1/projects/${PROJECT.id}/members/${MEMBERS[1].user_id}`]: async () =>
      ok({ ...MEMBERSHIP, user_id: MEMBERS[1].user_id, role: "maintainer" }),
  });
  const client = createProjectsClient(API, {
    fetch: fetchFn,
    csrfToken: () => "session-token-1",
  });
  const updated = await client.setMemberRole(PROJECT.id, MEMBERS[1].user_id, "maintainer");
  assert.equal(updated.role, "maintainer");
  assert.equal(calls[0].method, "PUT");
  assert.equal(calls[0].headers["X-CSRF-Token"], "session-token-1");
  assert.deepEqual(JSON.parse(calls[0].body), { role: "maintainer" });
});

test("setMemberRole without a token provider sends no CSRF header", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`PUT /api/v1/projects/${PROJECT.id}/members/${MEMBERS[1].user_id}`]: async () =>
      ok({ ...MEMBERSHIP, user_id: MEMBERS[1].user_id, role: "viewer" }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  await client.setMemberRole(PROJECT.id, MEMBERS[1].user_id, "viewer");
  assert.equal(calls[0].headers["X-CSRF-Token"], undefined);
});

test("updateSettings PATCHes only the fields the caller sent", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`PATCH /api/v1/projects/${PROJECT.id}`]: async () =>
      ok({ ...PROJECT, purpose: "A sharper goal.", activity_status: "active" }),
  });
  const client = createProjectsClient(API, {
    fetch: fetchFn,
    csrfToken: () => "session-token-2",
  });
  const updated = await client.updateSettings(PROJECT.id, { purpose: "A sharper goal." });
  assert.equal(updated.purpose, "A sharper goal.");
  assert.equal(calls[0].method, "PATCH");
  assert.equal(calls[0].headers["X-CSRF-Token"], "session-token-2");
  assert.deepEqual(JSON.parse(calls[0].body), { purpose: "A sharper goal." });
});

test("updateSettings surfaces VISIBILITY_CHANGE_NOT_SUPPORTED as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`PATCH /api/v1/projects/${PROJECT.id}`]: async () => ({
      status: 400,
      json: async () => ({ code: "VISIBILITY_CHANGE_NOT_SUPPORTED", message: "preview only" }),
    }),
  });
  const client = createProjectsClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.updateSettings(PROJECT.id, { purpose: "x" }),
    (err) => err instanceof ApiError && err.code === "VISIBILITY_CHANGE_NOT_SUPPORTED",
  );
});

test("messageForProjectCode maps the stable codes", () => {
  assert.match(messageForProjectCode("PROJECT_NOT_FOUND"), /does not exist/);
  assert.match(messageForProjectCode("PROJECT_MEMBERSHIP_NOT_FOUND"), /not a member/);
  assert.match(messageForProjectCode("SETTINGS_FORBIDDEN"), /owners and maintainers/);
  assert.match(messageForProjectCode("MEMBER_NOT_FOUND"), /no longer part/);
  assert.match(messageForProjectCode("LAST_OWNER"), /at least one owner/);
  assert.match(messageForProjectCode("SELF_ROLE_CHANGE_FORBIDDEN"), /your own role/);
  assert.match(messageForProjectCode("OWNER_ROLE_CHANGE_FORBIDDEN"), /grant or revoke the owner role/);
  assert.match(messageForProjectCode("VISIBILITY_CHANGE_NOT_SUPPORTED"), /not available yet/);
  assert.match(messageForProjectCode("CSRF_FAILED"), /Reload the page/);
  assert.match(messageForProjectCode("WHATEVER"), /went wrong/);
});
