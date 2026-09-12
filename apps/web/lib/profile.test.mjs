/**
 * Tests for the client profile module (apps/web/lib/profile.ts).
 *
 * Plain-ESM test file like auth.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (profile.ts imports ./auth.ts, which is
 * import-free by construction); the modules are fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/profile.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createProfileClient,
  messageForProfileCode,
} from "./profile.ts";

const API = "http://127.0.0.1:8080";

/** A memory TokenStorage for tests (same shape as auth.test.mjs). */
function memoryStorage() {
  let token = null;
  return {
    get: () => token,
    set: (t) => { token = t; },
    clear: () => { token = null; },
  };
}

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({ method, path, headers: init.headers ?? {}, body: init.body, credentials: init.credentials });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler({ headers: init.headers ?? {}, body: init.body });
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });

const PROFILE = {
  id: "02040608-0a0c-4e10-9214-16181a1c1e20",
  handle: "alice",
  display_name: "Alice Researcher",
  bio: "alloy design",
  created_at: "2026-09-01T12:00:00Z",
};

test("get reads the public profile by id without any CSRF header", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/users/02040608-0a0c-4e10-9214-16181a1c1e20/profile": async ({ headers }) => {
      assert.equal(headers["X-CSRF-Token"], undefined, "public reads carry no CSRF header");
      return ok(PROFILE);
    },
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage: memoryStorage() });
  const p = await client.get(PROFILE.id);
  assert.equal(p.handle, "alice");
  assert.equal(p.bio, "alloy design");
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].credentials ?? "include", "include");
});

test("getByHandle resolves the handle lookup path", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/users/by-handle/alice-r/profile": async () => ok({ ...PROFILE, handle: "alice-r" }),
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage: memoryStorage() });
  const p = await client.getByHandle("alice-r");
  assert.equal(p.handle, "alice-r");
  assert.equal(calls[0].path, "/api/v1/users/by-handle/alice-r/profile");
});

test("a 404 profile surfaces the stable envelope as ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/users/00000000-0000-4000-8000-000000000000/profile": async () => ({
      status: 404,
      json: async () => ({ code: "USER_NOT_FOUND", message: "no such user", retryable: false }),
    }),
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage: memoryStorage() });
  await assert.rejects(
    client.get("00000000-0000-4000-8000-000000000000"),
    (err) => err instanceof ApiError && err.code === "USER_NOT_FOUND" && err.status === 404,
  );
});

test("update PATCHes the editable fields with the stored CSRF token", async () => {
  const storage = memoryStorage();
  storage.set("tok-9");
  const { fetchFn, calls } = fakeFetch({
    "PATCH /api/v1/users/02040608-0a0c-4e10-9214-16181a1c1e20/profile": async ({ headers, body }) => {
      const parsed = JSON.parse(body);
      assert.equal(parsed.display_name, "Alice Two");
      assert.equal(parsed.bio, "ML potentials");
      assert.equal(parsed.handle, "alice-r");
      assert.equal(headers["X-CSRF-Token"], "tok-9");
      assert.equal(headers["Content-Type"], "application/json");
      return ok({ ...PROFILE, display_name: "Alice Two", bio: "ML potentials", handle: "alice-r" });
    },
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage });
  const p = await client.update(PROFILE.id, {
    handle: "alice-r",
    display_name: "Alice Two",
    bio: "ML potentials",
  });
  assert.equal(p.display_name, "Alice Two");
  assert.equal(calls[0].method, "PATCH");
  assert.equal(calls[0].credentials ?? "include", "include");
});

test("a foreign update surfaces AUTH_FORBIDDEN as ApiError", async () => {
  const storage = memoryStorage();
  storage.set("tok-bob");
  const { fetchFn } = fakeFetch({
    "PATCH /api/v1/users/02040608-0a0c-4e10-9214-16181a1c1e20/profile": async () => ({
      status: 403,
      json: async () => ({ code: "AUTH_FORBIDDEN", message: "only the profile owner may edit these fields", retryable: false }),
    }),
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage });
  await assert.rejects(
    client.update(PROFILE.id, { bio: "x" }),
    (err) => err instanceof ApiError && err.code === "AUTH_FORBIDDEN" && err.status === 403,
  );
});

test("a taken handle surfaces HANDLE_ALREADY_TAKEN as ApiError", async () => {
  const storage = memoryStorage();
  storage.set("tok-a");
  const { fetchFn } = fakeFetch({
    "PATCH /api/v1/users/02040608-0a0c-4e10-9214-16181a1c1e20/profile": async () => ({
      status: 409,
      json: async () => ({ code: "HANDLE_ALREADY_TAKEN", message: "this handle is already taken", retryable: false }),
    }),
  });
  const client = createProfileClient(API, { fetch: fetchFn, storage });
  await assert.rejects(
    client.update(PROFILE.id, { handle: "bob" }),
    (err) => err instanceof ApiError && err.code === "HANDLE_ALREADY_TAKEN",
  );
});

test("messageForProfileCode covers every stable profile code with a generic fallback", () => {
  for (const code of [
    "USER_NOT_FOUND", "AUTH_FORBIDDEN", "AUTH_UNAUTHENTICATED",
    "VALIDATION_FAILED", "HANDLE_ALREADY_TAKEN", "CSRF_FAILED",
    "SERVICE_UNAVAILABLE",
  ]) {
    const msg = messageForProfileCode(code);
    assert.ok(msg.length > 0, `message for ${code}`);
    assert.ok(!msg.includes("undefined"), `no undefined text for ${code}`);
  }
  assert.ok(messageForProfileCode("SOMETHING_NEW").length > 0, "unknown codes get a generic message");
  // The known codes map to distinct, useful lines (not the generic one).
  const generic = messageForProfileCode("SOMETHING_NEW");
  assert.notEqual(messageForProfileCode("HANDLE_ALREADY_TAKEN"), generic);
});
