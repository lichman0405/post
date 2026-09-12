/**
 * Tests for the client auth module (apps/web/lib/auth.ts).
 *
 * Plain-ESM test file like config.test.mjs: Node ≥ 23.6 runs the imported
 * .ts through type stripping; the module itself is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/auth.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createAuthClient,
  messageForCode,
  messageForOIDCError,
  sessionTokenStorage,
} from "./auth.ts";

const API = "http://127.0.0.1:8080";

/** A memory TokenStorage for tests. */
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
    calls.push({ method, path, headers: init.headers ?? {}, body: init.body });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler({ headers: init.headers ?? {}, body: init.body });
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });

test("signup posts JSON and stores the CSRF token from the response", async () => {
  const { fetchFn, calls } = fakeFetch({
    "POST /api/v1/auth/signup": async ({ headers, body }) => {
      const parsed = JSON.parse(body);
      assert.equal(parsed.email, "alice@example.com");
      assert.equal(parsed.password, "long-enough-password");
      assert.equal(headers["Content-Type"], "application/json");
      assert.equal(headers["X-CSRF-Token"], undefined, "signup carries no CSRF yet");
      return ok({ user: { id: "u1", handle: "alice", email: "alice@example.com", display_name: "alice" }, csrf_token: "tok-1" });
    },
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  const session = await client.signup({ email: "alice@example.com", password: "long-enough-password" });
  assert.equal(session.csrf_token, "tok-1");
  assert.equal(client.csrfToken(), "tok-1");
  assert.equal(calls[0].credentials ?? "include", "include");
});

test("login failure surfaces the stable envelope as ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/auth/login": async () => ({
      status: 401,
      json: async () => ({ code: "AUTH_INVALID_CREDENTIALS", message: "invalid email or password", retryable: false }),
    }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  await assert.rejects(
    client.login("a@b.co", "wrong-password"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "AUTH_INVALID_CREDENTIALS");
      assert.equal(err.status, 401);
      assert.equal(err.message, "invalid email or password");
      return true;
    },
  );
});

test("session: 401 resolves null and clears the stored token", async () => {
  const storage = memoryStorage();
  storage.set("stale-token");
  const { fetchFn } = fakeFetch({
    "GET /api/v1/auth/session": async () => ({ status: 401, json: async () => ({ code: "AUTH_UNAUTHENTICATED", message: "no active session" }) }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage });
  assert.equal(await client.session(), null);
  assert.equal(client.csrfToken(), null, "a dead session clears the CSRF token");
});

test("session: 200 refreshes the CSRF token", async () => {
  const storage = memoryStorage();
  const { fetchFn } = fakeFetch({
    "GET /api/v1/auth/session": async () => ok({ user: { id: "u1", handle: "alice", email: "a@b.co", display_name: "Alice" }, csrf_token: "tok-2" }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage });
  const session = await client.session();
  assert.equal(session.user.email, "a@b.co");
  assert.equal(client.csrfToken(), "tok-2");
});

test("logout sends the CSRF token and clears it afterwards", async () => {
  const storage = memoryStorage();
  storage.set("tok-3");
  const { fetchFn, calls } = fakeFetch({
    "POST /api/v1/auth/logout": async ({ headers }) => {
      assert.equal(headers["X-CSRF-Token"], "tok-3");
      return { status: 204, json: async () => null };
    },
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage });
  await client.logout();
  assert.equal(client.csrfToken(), null);
  assert.equal(calls[0].method, "POST");
});

test("logout without a session (401) is still treated as signed out", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/auth/logout": async () => ({ status: 401, json: async () => ({ code: "AUTH_UNAUTHENTICATED", message: "authentication required" }) }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  await client.logout(); // must not throw
});

test("rate-limit 429 surfaces Retry-After semantics via ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/auth/login": async () => ({
      status: 429,
      json: async () => ({ code: "RATE_LIMITED", message: "too many attempts; try again later", retryable: true }),
    }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  await assert.rejects(
    client.login("a@b.co", "whatever-password"),
    (err) => err instanceof ApiError && err.code === "RATE_LIMITED" && err.status === 429,
  );
});

test("oidcAuthorizeUrl returns the provider URL", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/auth/oidc/authorize-url": async () => ok({ authorize_url: "https://id.example.com/authorize?state=s1" }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  assert.equal(
    await client.oidcAuthorizeUrl(),
    "https://id.example.com/authorize?state=s1",
  );
});

test("a non-JSON error body becomes a generic ApiError, never a crash", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/auth/login": async () => ({ status: 502, json: async () => { throw new Error("not json"); } }),
  });
  const client = createAuthClient(API, { fetch: fetchFn, storage: memoryStorage() });
  await assert.rejects(
    client.login("a@b.co", "whatever-password"),
    (err) => err instanceof ApiError && err.code === "UNKNOWN",
  );
});

test("messageForCode covers every stable auth code with a generic fallback", () => {
  for (const code of [
    "AUTH_INVALID_CREDENTIALS", "EMAIL_ALREADY_REGISTERED", "VALIDATION_FAILED",
    "RATE_LIMITED", "CSRF_FAILED", "SERVICE_UNAVAILABLE", "OIDC_NOT_CONFIGURED",
    "OIDC_PROVIDER_FAILED", "OIDC_EMAIL_NOT_VERIFIED",
  ]) {
    const msg = messageForCode(code);
    assert.ok(msg.length > 0, `message for ${code}`);
    assert.ok(!msg.includes("undefined"), `no undefined text for ${code}`);
  }
  assert.ok(messageForCode("SOMETHING_NEW").length > 0, "unknown codes get a generic message");
});

test("messageForOIDCError covers the callback failure codes the API redirects", () => {
  for (const code of ["state_mismatch", "no_code", "email_not_verified", "provider_failed"]) {
    const msg = messageForOIDCError(code);
    assert.ok(msg.length > 0, `message for ${code}`);
    assert.ok(!msg.includes("undefined"), `no undefined text for ${code}`);
  }
  assert.ok(messageForOIDCError("something_else").length > 0, "unknown codes get a generic message");
  // The known codes map to distinct, useful lines (not the generic one).
  const generic = messageForOIDCError("something_else");
  assert.notEqual(messageForOIDCError("email_not_verified"), generic);
});

test("sessionTokenStorage falls back to memory outside the browser", () => {
  // In Node there is no window: the storage must still work (tests, SSR
  // render passes) instead of throwing.
  const storage = sessionTokenStorage();
  assert.equal(storage.get(), null);
  storage.set("tok-x");
  assert.equal(storage.get(), "tok-x");
  storage.clear();
  assert.equal(storage.get(), null);
});
