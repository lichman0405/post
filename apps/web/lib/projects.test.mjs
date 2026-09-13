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
    calls.push({ method, path, credentials: init.credentials, headers: init.headers ?? {} });
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

test("messageForProjectCode maps the stable codes", () => {
  assert.match(messageForProjectCode("PROJECT_NOT_FOUND"), /does not exist/);
  assert.match(messageForProjectCode("PROJECT_MEMBERSHIP_NOT_FOUND"), /not a member/);
  assert.match(messageForProjectCode("WHATEVER"), /went wrong/);
});
