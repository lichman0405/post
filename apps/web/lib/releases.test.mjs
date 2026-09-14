/**
 * Tests for the client releases module (apps/web/lib/releases.ts).
 *
 * Plain-ESM test file like projects.test.mjs: Node ≥ 23.6 runs the
 * imported .ts chain through type stripping (releases.ts is import-free
 * by construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/releases.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createReleasesClient,
  mayCreateRelease,
  messageForReleaseCode,
  releaseManifestHref,
} from "./releases.ts";

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

const RELEASE = {
  id: "11111111-2222-4333-8444-555555555555",
  project_id: "99999999-8888-4777-8666-555555555555",
  version: "v1.0.0",
  title: "First release",
  state_id: "77777777-6666-4555-8444-555555555555",
  policy_version_id: "66666666-5555-4444-8333-555555555555",
  org_policy_version_id: "55555555-4444-4333-8222-555555555555",
  manifest_hash: "sha256:abcd",
  created_by: "33333333-2222-4111-8111-555555555555",
  created_at: "2026-09-14T10:00:00Z",
};

const PROJECT_ID = RELEASE.project_id;
const RELEASE_ID = RELEASE.id;

test("list decodes the releases envelope", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/releases`]: ok({ releases: [RELEASE] }),
  });
  const client = createReleasesClient(API, { fetch: fetchFn });

  const releases = await client.list(PROJECT_ID);
  assert.equal(releases.length, 1);
  assert.equal(releases[0].version, "v1.0.0");
  assert.equal(releases[0].manifest_hash, "sha256:abcd");
  assert.equal(calls[0].credentials, "include");
});

test("get decodes one release", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/releases/${RELEASE_ID}`]: ok(RELEASE),
  });
  const client = createReleasesClient(API, { fetch: fetchFn });

  const release = await client.get(PROJECT_ID, RELEASE_ID);
  assert.equal(release.title, "First release");
});

test("create posts the body with csrf and idempotency headers", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/releases`]: () => ({
      status: 201,
      json: async () => RELEASE,
    }),
  });
  const client = createReleasesClient(API, {
    fetch: fetchFn,
    csrfToken: () => "csrf-token-1",
  });

  const release = await client.create(PROJECT_ID, {
    version: "v1.0.0",
    title: "First release",
    idempotencyKey: "key-create-v1",
  });
  assert.equal(release.version, "v1.0.0");
  assert.equal(calls.length, 1);
  const call = calls[0];
  assert.equal(call.method, "POST");
  assert.equal(call.headers["X-CSRF-Token"], "csrf-token-1");
  assert.equal(call.headers["Idempotency-Key"], "key-create-v1");
  assert.equal(JSON.parse(call.body).version, "v1.0.0");
  assert.equal(JSON.parse(call.body).title, "First release");
});

test("create omits the idempotency header when no key is supplied", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/releases`]: () => ({
      status: 201,
      json: async () => RELEASE,
    }),
  });
  const client = createReleasesClient(API, { fetch: fetchFn });

  await client.create(PROJECT_ID, { version: "v1.0.0" });
  assert.ok(!("Idempotency-Key" in calls[0].headers));
});

test("errors surface the stable envelope", async () => {
  const { fetchFn } = fakeFetch({
    [`POST /api/v1/projects/${PROJECT_ID}/releases`]: () => ({
      status: 409,
      json: async () => ({ code: "RELEASE_VERSION_TAKEN", message: "this version already exists" }),
    }),
  });
  const client = createReleasesClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.create(PROJECT_ID, { version: "v1.0.0" }),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "RELEASE_VERSION_TAKEN");
      assert.equal(err.status, 409);
      return true;
    },
  );
});

test("manifest returns the parsed document", async () => {
  const doc = { format: "open-rd/release-manifest", format_version: 1 };
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PROJECT_ID}/releases/${RELEASE_ID}/manifest`]: ok(doc),
  });
  const client = createReleasesClient(API, { fetch: fetchFn });

  const manifest = await client.manifest(PROJECT_ID, RELEASE_ID);
  assert.equal(manifest.format, "open-rd/release-manifest");
});

test("mayCreateRelease mirrors the matrix", () => {
  assert.equal(mayCreateRelease("owner"), true);
  assert.equal(mayCreateRelease("maintainer"), true);
  assert.equal(mayCreateRelease("contributor"), false);
  assert.equal(mayCreateRelease("viewer"), false);
  assert.equal(mayCreateRelease(null), false);
});

test("messageForReleaseCode covers every stable code", () => {
  for (const code of [
    "RELEASE_PROJECT_NOT_FOUND",
    "RELEASE_NOT_FOUND",
    "RELEASE_FORBIDDEN",
    "RELEASE_VERSION_TAKEN",
    "RELEASE_NO_MAIN_STATE",
    "RELEASE_GATE_BLOCKED",
    "VALIDATION_FAILED",
    "SERVICE_UNAVAILABLE",
  ]) {
    const message = messageForReleaseCode(code);
    assert.ok(typeof message === "string" && message.length > 0, `no message for ${code}`);
  }
  assert.ok(messageForReleaseCode("WAT").length > 0);
});

test("releaseManifestHref encodes both ids", () => {
  const href = releaseManifestHref(API, "p id", "r/id");
  assert.equal(href, `${API}/api/v1/projects/p%20id/releases/r%2Fid/manifest`);
});
