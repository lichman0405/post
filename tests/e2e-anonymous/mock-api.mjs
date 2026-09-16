/**
 * The Go API, mocked at the network layer for the anonymous-pages e2e
 * (T0801 required test "anonymous e2e").
 *
 * Unlike the other harnesses in tests/ (which intercept the API inside the
 * browser), this one is a REAL HTTP server, because the pages under test
 * are read twice: once by the Next SERVER (the <head> of T0801's
 * generateMetadata, and /sitemap.xml) and once by the browser (the client
 * components the routes render). A browser-level intercept could not see
 * the server-side read at all, and the whole point of the task is what a
 * crawler — i.e. a server-side, cookie-less reader — gets.
 *
 * # What this mock IS
 *
 * It is the ANONYMOUS answer of the real API, which is the only reader
 * that exists here: no session is ever established, so every request is
 * anonymous and the mock never has to decide anything per-caller. The
 * answers it gives are therefore the responses cmd/api actually produces
 * for an anonymous caller, copied from the Go payload structs:
 *
 *   - a public project/release/asset/profile: 200, the payload
 *     (cmd/api/projectshttp projectPayload; cmd/api/releasehttp
 *     releasePayload; internal/assets.AssetPage; cmd/api/profilehttp
 *     publicProfilePayload);
 *   - a private project, and everything under it: the existence-hiding 404
 *     with the SAME code and message an unknown id gets (docs/45; the
 *     projects service answers both with PROJECT_NOT_FOUND, T0106);
 *   - the caller's own membership: 404 PROJECT_MEMBERSHIP_NOT_FOUND (the
 *     "no role" answer the project shell renders for a non-member);
 *   - /auth/session: 401 (no session), which is what the nav expects.
 *
 * # What this mock is NOT
 *
 * It is not a second implementation of any rule: it does not decide who
 * may see what, it only distinguishes the two fixture entities it was
 * given (one public, one private). The rules themselves are pinned against
 * the real guard and real PostgreSQL by tests/e2e/anonymous_e2e_test.go,
 * tests/e2e/privacy_e2e_test.go and the integration suite; the browser
 * checklist then proves the PAGES honour the answer.
 *
 * Usage: node mock-api.mjs <port>
 */
import { createServer } from "node:http";

import {
  ALICE,
  ASSET_PID,
  ASSET_SLUG,
  ASSET_TITLE,
  PRIVATE_PROJECT,
  PROFILE,
  PUBLIC_PROJECT,
  RELEASE,
} from "./fixtures.mjs";

const PORT = Number(process.argv[2] ?? 0);
if (!Number.isInteger(PORT) || PORT <= 0) {
  console.error("mock-api: usage: node mock-api.mjs <port>");
  process.exit(2);
}

/**
 * The mutation switch, OFF unless the environment asks for it, and used by
 * exactly one caller: mutation-check.sh, which proves the checklist can
 * fail. With it on, this server answers the private project to an
 * anonymous caller — the single most dangerous thing a real API could do
 * here — and the checklist must catch it and name the assertion.
 *
 * It changes the MOCK's answer only; the web app is untouched, so a run
 * that catches the mutation is a run that proves the pages were really
 * being tested. A run with the switch on is never a pass: the checklist is
 * supposed to fail under it.
 */
const LEAK_PRIVATE = process.env.MOCK_LEAK_PRIVATE === "1";
if (LEAK_PRIVATE) {
  console.error("mock-api: MOCK_LEAK_PRIVATE=1 — this server is lying about a private project on purpose");
}

/* ---------- payload builders ---------- */

function assetPage(version) {
  const versions = [
    {
      version: "1.0",
      url: `/assets/${ASSET_PID}/1.0`,
      visibility: "public",
      integrity_hash: "sha256:1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f",
      published_at: "2026-09-01T10:00:00Z",
      current: false,
    },
    {
      version: "2.0",
      url: `/assets/${ASSET_PID}/2.0`,
      visibility: "public",
      integrity_hash: "sha256:2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f",
      published_at: "2026-09-10T10:00:00Z",
      current: true,
    },
  ];
  const rendered = versions.find((v) => v.version === version) ?? versions[1];
  return {
    asset: {
      pid: ASSET_PID,
      type: "material_collection",
      title: ASSET_TITLE,
      slug: ASSET_SLUG,
      origin_project: {
        id: PUBLIC_PROJECT.id,
        name: PUBLIC_PROJECT.name,
        slug: PUBLIC_PROJECT.slug,
        visibility: "public",
      },
      created_at: "2026-08-20T09:00:00Z",
    },
    version: {
      version: rendered.version,
      url: rendered.url,
      visibility: rendered.visibility,
      integrity_hash: rendered.integrity_hash,
      published_at: rendered.published_at,
      published_by: ALICE,
    },
    origin: [
      {
        ref: `project:${PUBLIC_PROJECT.id}`,
        kind: "project",
        resolved: true,
        title: PUBLIC_PROJECT.name,
        project_id: PUBLIC_PROJECT.id,
        link: `/projects/${PUBLIC_PROJECT.id}`,
      },
    ],
    rights: {
      version: 1,
      standard_license_id: "CC-BY-4.0",
      custom_agreement_ref: null,
      usage: {
        commercial_use: "allowed",
        derivatives: "allowed_with_attribution",
        redistribution: "allowed",
        model_training: "unspecified",
        attribution: "required",
        patent_grant: "not_granted",
      },
      visibility: { metadata: "project_policy", data_access: "restricted" },
      notes: "Synthesis parameters are published; precursor batches are not.",
    },
    creators: [{ ...ALICE, role: "publisher" }],
    metadata: [{ key: "cell_chemistry", value: "Zn4O(BDC)3" }],
    dependencies: [],
    lineage: [],
    used_by: [],
    versions: versions.map((v) => ({ ...v, current: v.version === rendered.version })),
    events: [],
  };
}

const BROWSE = {
  type: null,
  types: ["dataset", "protocol", "material_collection", "benchmark"],
  assets: [
    {
      pid: ASSET_PID,
      type: "material_collection",
      title: ASSET_TITLE,
      slug: ASSET_SLUG,
      url: `/assets/${ASSET_PID}`,
      origin_project: {
        id: PUBLIC_PROJECT.id,
        name: PUBLIC_PROJECT.name,
        slug: PUBLIC_PROJECT.slug,
        visibility: "public",
      },
      public_versions: 2,
      latest_version: "2.0",
      latest_url: `/assets/${ASSET_PID}/2.0`,
      latest_published_at: "2026-09-10T10:00:00Z",
    },
  ],
};

function envelope(code, message) {
  return { code, message, request_id: "e2e-anonymous", retryable: false };
}

const PROJECT_NOT_FOUND = envelope("PROJECT_NOT_FOUND", "project not found");
const ASSET_NOT_FOUND = envelope("ASSET_NOT_FOUND", "asset not found");
const RELEASE_NOT_FOUND = envelope("RELEASE_NOT_FOUND", "release not found");
const USER_NOT_FOUND = envelope("USER_NOT_FOUND", "no such user");
const MEMBERSHIP_NOT_FOUND = envelope("PROJECT_MEMBERSHIP_NOT_FOUND", "you are not a member of this project");
const UNAUTHENTICATED = envelope("UNAUTHENTICATED", "authentication required");

/* ---------- routing ---------- */

/** The anonymous answer for one request: {status, body}. */
function answer(method, pathname, query) {
  if (method !== "GET" && method !== "HEAD") {
    // The guard's rule for an unauthenticated write (cmd/api/authhttp).
    return { status: 401, body: UNAUTHENTICATED };
  }

  // --- auth surface (the nav asks for the session on every page) ---
  if (pathname === "/api/v1/auth/session") return { status: 401, body: UNAUTHENTICATED };

  // --- projects ---
  if (pathname === "/api/v1/projects") {
    // The list an anonymous caller gets: the public projects only (T0106).
    return { status: 200, body: { projects: [publicProjectPayload()] } };
  }
  const project = pathname.match(/^\/api\/v1\/projects\/([^/]+)$/);
  if (project) {
    const payload = readableProject(project[1]);
    return payload === null ? { status: 404, body: PROJECT_NOT_FOUND } : { status: 200, body: payload };
  }
  const membership = pathname.match(/^\/api\/v1\/projects\/([^/]+)\/membership$/);
  if (membership) {
    // A project the caller may read but is not a member of answers the
    // same 404 the shell renders as "no role" (T0108).
    if (readableProject(membership[1]) === null) return { status: 404, body: PROJECT_NOT_FOUND };
    return { status: 404, body: MEMBERSHIP_NOT_FOUND };
  }

  // --- releases ---
  const releases = pathname.match(/^\/api\/v1\/projects\/([^/]+)\/releases$/);
  if (releases) {
    if (readableProject(releases[1]) === null) return { status: 404, body: PROJECT_NOT_FOUND };
    return { status: 200, body: { releases: [RELEASE] } };
  }
  const release = pathname.match(/^\/api\/v1\/projects\/([^/]+)\/releases\/([^/]+)(\/manifest)?$/);
  if (release) {
    if (readableProject(release[1]) === null) return { status: 404, body: PROJECT_NOT_FOUND };
    if (release[2] !== RELEASE.id) return { status: 404, body: RELEASE_NOT_FOUND };
    if (release[3]) {
      return { status: 200, body: { manifest: "canonical", version: RELEASE.version } };
    }
    return { status: 200, body: RELEASE };
  }

  // --- assets ---
  if (pathname === "/api/v1/assets") return { status: 200, body: BROWSE };
  const asset = pathname.match(/^\/api\/v1\/assets\/([^/]+)$/);
  if (asset) {
    // The published asset, and nothing else — the private asset's pid
    // (fixtures.PRIVATE_ASSET_PID) lands in this same branch on purpose:
    // the API answers a pid whose versions the caller may not see with the
    // one hiding 404, exactly like a pid that names nothing.
    if (asset[1] !== ASSET_PID) return { status: 404, body: ASSET_NOT_FOUND };
    return { status: 200, body: assetPage(query.get("version")) };
  }

  // --- profiles ---
  const profile = pathname.match(/^\/api\/v1\/users\/([^/]+)\/profile$/);
  if (profile) {
    if (profile[1] !== PROFILE.id) return { status: 404, body: USER_NOT_FOUND };
    return { status: 200, body: PROFILE };
  }

  return { status: 404, body: envelope("NOT_FOUND", "no such route") };
}

function publicProjectPayload() {
  return { ...PUBLIC_PROJECT };
}

/**
 * The project a caller may read, or null. The only rule in this file: the
 * public fixture is readable and everything else answers the one
 * existence-hiding 404 — the same answer the real API gives an anonymous
 * caller for a private project AND for an id that names nothing.
 *
 * The second branch exists only under the mutation switch (see above) and
 * is a bug by construction: it is the one line that makes this server
 * disclose an entity the caller may not see.
 */
function readableProject(id) {
  if (id === PUBLIC_PROJECT.id) return publicProjectPayload();
  if (LEAK_PRIVATE && id === PRIVATE_PROJECT.id) {
    return { ...PUBLIC_PROJECT, ...PRIVATE_PROJECT, visibility: "private" };
  }
  return null;
}

/* ---------- the server ---------- */

/**
 * Every request this server answered, in order. The checklist reads it to
 * prove WHAT the pages asked for — in particular that the metadata reads
 * went out with no session (no Cookie header), which is the property that
 * makes "indexable" mean "public" rather than "public to me".
 */
const log = [];

const server = createServer((req, res) => {
  const url = new URL(req.url ?? "/", `http://127.0.0.1:${PORT}`);

  // Harness control endpoint, not part of any API surface: the wire log.
  if (url.pathname === "/__requests") {
    const payload = JSON.stringify({ requests: log });
    res.writeHead(200, { "content-type": "application/json", "content-length": Buffer.byteLength(payload) });
    res.end(payload);
    return;
  }

  const preflight = req.method === "OPTIONS";
  const { status, body } = preflight
    ? { status: 204, body: null }
    : answer(req.method ?? "GET", url.pathname, url.searchParams);

  log.push({
    method: req.method ?? "GET",
    path: url.pathname + (url.search === "" ? "" : url.search),
    cookie: req.headers.cookie ?? null,
    authorization: req.headers.authorization ?? null,
    origin: req.headers.origin ?? null,
    user_agent: req.headers["user-agent"] ?? null,
    status,
  });

  // The production guard reflects the configured web origin and allows
  // credentials (cmd/api/authhttp applyCORS); the browser here IS the web
  // origin, and the client fetches send credentials: "include".
  const origin = req.headers.origin;
  const headers = {
    "content-type": "application/json",
    "cache-control": "no-store",
  };
  if (typeof origin === "string" && origin !== "") {
    headers["access-control-allow-origin"] = origin;
    headers["access-control-allow-credentials"] = "true";
    headers.vary = "Origin";
  }
  if (preflight) {
    headers["access-control-allow-methods"] = "GET, POST, PUT, PATCH, DELETE, OPTIONS";
    headers["access-control-allow-headers"] = "Content-Type, X-CSRF-Token, X-Correlation-ID";
    res.writeHead(204, headers);
    res.end();
    return;
  }

  const payload = JSON.stringify(body);
  headers["content-length"] = Buffer.byteLength(payload);
  res.writeHead(status, headers);
  res.end(req.method === "HEAD" ? undefined : payload);
});

server.listen(PORT, "127.0.0.1", () => {
  // The harness reads this line to learn the port when it asked for 0.
  console.log(`mock-api listening on http://127.0.0.1:${PORT}`);
});
