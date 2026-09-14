/**
 * Tests for the client files module (apps/web/lib/files.ts).
 *
 * Plain-ESM test file like projects.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (files.ts imports only lib/projects.ts,
 * which is import-free by construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/files.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  createFilesClient,
  messageForFileCode,
} from "./files.ts";

const API = "http://127.0.0.1:8080";
const PID = "11111111-2222-4333-8444-555555555555";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({ method, path, credentials: init.credentials, body: init.body ?? null });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });

const LISTING = {
  ref: "main",
  path: "",
  sha: "tree-sha-1",
  entries: [
    { name: "docs", path: "docs", type: "tree", mode: "040000", size: 0, sha: "t1" },
    { name: "README.md", path: "README.md", type: "blob", mode: "100644", size: 12, sha: "s1" },
  ],
};

const FILE = {
  name: "README.md",
  path: "README.md",
  type: "blob",
  mode: "100644",
  size: 12,
  sha: "s1",
  kind: "text",
  content: "hello world\n",
  truncated: false,
};

const HISTORY = [
  {
    sha: "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0",
    author: "Alice",
    author_email: "alice@example.com",
    date: "2026-09-14T10:00:00Z",
    message: "add readme",
  },
];

test("tree lists the root of main with the session cookie", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PID}/files/tree`]: async () => ok(LISTING),
  });
  const client = createFilesClient(API, { fetch: fetchFn });
  const listing = await client.tree(PID, {});
  assert.equal(listing.sha, "tree-sha-1");
  assert.equal(listing.entries.length, 2);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].credentials, "include");
});

test("tree encodes the ref and path into the query", async () => {
  const { fetchFn, calls } = fakeFetch({
    ["GET /api/v1/projects/" + PID + "/files/tree?ref=feature%2Fx&path=docs%2Fnotes"]:
      async () => ok({ ...LISTING, path: "docs/notes", ref: "feature/x" }),
  });
  const client = createFilesClient(API, { fetch: fetchFn });
  await client.tree(PID, { path: "docs/notes", ref: "feature/x" });
  assert.equal(calls.length, 1);
});

test("content reads one file's preview", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PID}/files/content?path=README.md`]: async () => ok(FILE),
  });
  const client = createFilesClient(API, { fetch: fetchFn });
  const view = await client.content(PID, { path: "README.md" });
  assert.equal(view.kind, "text");
  assert.equal(view.content, "hello world\n");
  assert.equal(view.truncated, false);
});

test("history reads the commit list for one path with the page size", async () => {
  const { fetchFn, calls } = fakeFetch({
    [`GET /api/v1/projects/${PID}/files/history?path=README.md&limit=20`]: async () =>
      ok({ ref: "main", path: "README.md", entries: HISTORY }),
  });
  const client = createFilesClient(API, { fetch: fetchFn });
  const entries = await client.history(PID, { path: "README.md", limit: 20 });
  assert.equal(entries.length, 1);
  assert.equal(entries[0].sha, HISTORY[0].sha);
  assert.equal(calls[0].method, "GET");
});

test("reads surface the stable envelope as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    [`GET /api/v1/projects/${PID}/files/tree`]: async () => ({
      status: 409,
      json: async () => ({ code: "PROJECT_NOT_PROVISIONED", message: "not provisioned" }),
    }),
  });
  const client = createFilesClient(API, { fetch: fetchFn });
  await assert.rejects(
    client.tree(PID, {}),
    (err) => err instanceof ApiError && err.code === "PROJECT_NOT_PROVISIONED" && err.status === 409,
  );
});

test("rawUrl and diffUrl build absolute GET links on the API origin (never a fetch)", async () => {
  const { fetchFn, calls } = fakeFetch({});
  const client = createFilesClient(API, { fetch: fetchFn });
  assert.equal(client.rawUrl(PID, { ref: "main", path: "data/raw.csv" }),
    `${API}/api/v1/projects/${PID}/files/raw?ref=main&path=data%2Fraw.csv`);
  assert.equal(client.rawUrl(PID, { path: "f.bin" }),
    `${API}/api/v1/projects/${PID}/files/raw?path=f.bin`);
  assert.equal(client.diffUrl(PID, "a1b2c3d4e5f6a7b8"),
    `${API}/api/v1/projects/${PID}/files/diff?sha=a1b2c3d4e5f6a7b8`);
  assert.equal(calls.length, 0, "URL builders must not fetch");
});

test("messageForFileCode maps the files vocabulary", () => {
  assert.match(messageForFileCode("PROJECT_NOT_PROVISIONED"), /provision/);
  assert.match(messageForFileCode("FILES_NOT_FOUND"), /No such file/);
  assert.match(messageForFileCode("WHATEVER"), /try again/);
});
