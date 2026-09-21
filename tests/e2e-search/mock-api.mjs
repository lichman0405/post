/**
 * The Search Answer e2e harness's API origin (mock).
 *
 * WHY THIS IS A SERVER AND NOT `page.route`
 *
 * /search reads its answer with a POST from the BROWSER (the question needs
 * the session cookie and a JSON body, and the answer is a per-caller scoped
 * result the API answers `Cache-Control: no-store`), so Playwright *could*
 * route it. It is a real origin anyway, for two reasons:
 *
 *   1. A source's href is an API path in the answer's own namespace
 *      (internal/search/answer/locator.go), and clicking one navigates the
 *      browser to a second document. `page.route` can fake a response but it
 *      cannot stand in for "there is a real server at that address"; a real
 *      listener can, which is what makes the click assertion mean something.
 *   2. The wire shape is then exercised over a real TCP stack with real CORS
 *      (a credentialed cross-origin POST does preflight), which is the part
 *      of the page's contract a route handler would silently paper over.
 *
 * WHAT THIS FILE STANDS IN FOR, AND WHAT IT DOES NOT
 *
 * The response document is the JSON internal/search/answer.Answer renders
 * (answer.CanonicalJSON, pinned by tests/answer's golden fixtures) inside
 * cmd/api/searchhttp's `{search_id, answer}` envelope. This file's copy of
 * that shape is not authority: cmd/api/searchhttp's own tests and
 * tests/answer decide what an answer is. What THIS file decides is whether
 * the page renders one.
 *
 * Two deliberate properties:
 *
 *   - `search_id` is derived from the question, so a refresh of the same URL
 *     is the same id. The real API mints a new id per call (the record is a
 *     new search), so this is a HARNESS property, stated here so nobody reads
 *     more into it than it says: it makes "the URL, and only the URL, is where
 *     the question lives" observable.
 *   - The source read routes serve EXACTLY the addresses the fixture's
 *     sources carry, and nothing else. A page that composed a path of its own
 *     would 404 here — which is the failure the harness exists to catch.
 *
 * Modes (flipped by the spec via GET /__mode?set=...):
 *   answered            a grounded answer: 5 sources, 2 cited
 *   sparse              an answer whose lists are empty (conflicts, citations)
 *   fallback-<reason>   a fallback, one of the six closed reasons
 *   error-400           SEARCH_INVALID_REQUEST
 *   error-401           SEARCH_UNAUTHENTICATED
 *   error-503           SEARCH_UNAVAILABLE
 *   malformed           200, with an answer missing its required lists
 *   slow                a grounded answer, after a delay
 *
 * Usage: node mock-api.mjs <port>
 */

import { createHash } from "node:crypto";
import { createServer } from "node:http";

const PORT = Number(process.argv[2] ?? 0);
if (!Number.isInteger(PORT) || PORT <= 0) {
  console.error("mock-api: usage: node mock-api.mjs <port>");
  process.exit(2);
}

const PROJECT_ID = "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d";

/* ---------- The six fallback reasons, with the platform's own sentences ---------- */

/**
 * Each reason's sentence is the one internal/search/answer's derive.go writes
 * into the answer's first limitation (fallbackText), copied verbatim: the page
 * renders this text unchanged, so the fixture has to carry the real thing
 * rather than a paraphrase the page might have been tuned to.
 */
const FALLBACK_TEXT = {
  no_provider:
    "no answer model is configured on this deployment, so this answer is the structured result: the sources the search returned, and what the platform has recorded about them",
  no_sources: "the search returned no source, so there is nothing a written answer could cite",
  provider_error: "the answer model could not be reached, so this answer is the structured result",
  provider_timeout: "the answer model did not answer within the time allowed, so this answer is the structured result",
  invalid_answer:
    "the answer model returned a document that does not satisfy the answer schema, so it was refused and this answer is the structured result",
  ungrounded_citation:
    "the answer model cited or named an entity the search did not return, so its document was refused and this answer is the structured result",
};

const FALLBACK_REASONS = Object.keys(FALLBACK_TEXT);

/* ---------- The sources: five rows, two of them cited ---------- */

/**
 * Five sources in rank order. The CITED ones are deliberately ranks 2 and 3 —
 * not the first two — so a page that marked the leading rows (or the first N)
 * as cited fails here: `cited` is a per-row fact of the document, not a
 * position.
 *
 * Rank 2 has NO href on purpose. internal/search/answer/locator.go produces ""
 * for a traversed object version (no route serves one by id), and the page
 * must render that source as text rather than inventing an address for it.
 */
function sources() {
  return [
    {
      rank: 1,
      ref: "asset:AST-0001@2",
      kind: "document",
      entity_type: "asset",
      object_type: "material",
      title: "Mg-MOF-74 CO2 uptake at 298 K",
      version: "2",
      href: "/api/v1/assets/AST-0001?version=2",
      project_id: PROJECT_ID,
      labels: ["independently_reproduced"],
      factors: [
        { factor: "query_scope_match", level: "question", level_rank: 0, reason: "recalled by the question's own content (full_text)" },
        { factor: "evidence", level: "direct", level_rank: 0, reason: "3 evidence assertions target this version" },
        { factor: "conflict", level: "uncontested", level_rank: 0, reason: "nothing on the platform contradicts this version" },
      ],
      cited: false,
    },
    {
      rank: 2,
      ref: "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1",
      kind: "object_version",
      object_type: "finding",
      title: "Uptake falls by 4% after ten cycles",
      version: "1",
      // No href: no route serves an object version by id (locator.go).
      project_id: PROJECT_ID,
      labels: [],
      factors: [{ factor: "query_scope_match", level: "related", level_rank: 2, reason: "recalled by the question's graph neighbourhood" }],
      cited: true,
    },
    {
      rank: 3,
      ref: "document:KNW-0007@3",
      kind: "document",
      entity_type: "knowledge",
      title: "Screening protocol for CO2 isotherms",
      version: "3",
      href: "/api/v1/knowledge/KNW-0007",
      project_id: PROJECT_ID,
      labels: ["reviewed"],
      factors: [{ factor: "review", level: "approved", level_rank: 0, reason: "1 scientific review, 1 approved" }],
      cited: true,
    },
    {
      rank: 4,
      ref: "asset:AST-0044@1",
      kind: "document",
      entity_type: "asset",
      object_type: "dataset",
      title: "Activated carbon reference isotherm",
      version: "1",
      href: "/api/v1/assets/AST-0044?version=1",
      project_id: PROJECT_ID,
      labels: [],
      factors: [{ factor: "evidence", level: "asserted", level_rank: 2, reason: "1 evidence assertion targets this version" }],
      cited: false,
    },
    {
      rank: 5,
      ref: "release:REL-0011",
      kind: "document",
      entity_type: "release",
      title: "Sorbent screen 0.9",
      href: `/api/v1/projects/${PROJECT_ID}/releases/REL-0011`,
      project_id: PROJECT_ID,
      labels: [],
      factors: [{ factor: "version", level: "superseded", level_rank: 2, reason: "the release is not the project's newest" }],
      cited: false,
    },
  ];
}

/** The two refs the fixture marks cited, written out for the spec to check against. */
const CITED_REFS = ["object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1", "document:KNW-0007@3"];

/** The two refs the summary's citations name, in the provider's order. */
const CITATIONS = ["object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1", "document:KNW-0007@3"];

const SUMMARY =
  "Two sources bear directly on the question. Mg-MOF-74 is reported at 3.4 mmol/g CO2 uptake at 298 K, and a second finding records a 4% loss of uptake after ten cycles.";

const LIMITATIONS = [
  { text: "the vector signal did not run: no embedder is configured, so the question could not be embedded", origin: "platform" },
  {
    text: "1 source is not asserted about by anything the platform has recorded: its version is listed but nothing has been asserted for or against it",
    refs: ["asset:AST-0044@1"],
    origin: "platform",
  },
];

const CONFLICTS = [
  {
    text: "this source is contested: 1 evidence assertion contradicts it",
    refs: ["asset:AST-0001@2"],
    origin: "platform",
  },
];

/** A uuid-shaped, query-derived id: same question, same id (see the header). */
function searchID(query) {
  const d = createHash("sha1").update(query).digest("hex");
  return `${d.slice(0, 8)}-${d.slice(8, 12)}-${d.slice(12, 16)}-${d.slice(16, 20)}-${d.slice(20, 32)}`;
}

function answeredAnswer(query, { sparse = false } = {}) {
  const rows = sources();
  if (sparse) {
    return {
      answer_version: "1",
      status: "answered",
      query,
      answer_view: true,
      summary: SUMMARY,
      citations: [],
      limitations: [{ text: "the platform derived no limitation for this result's sources", origin: "platform" }],
      conflicts: [],
      sources: rows.map((row) => ({ ...row, labels: [], factors: [], cited: false })),
    };
  }
  return {
    answer_version: "1",
    status: "answered",
    query,
    answer_view: true,
    summary: SUMMARY,
    citations: CITATIONS,
    limitations: LIMITATIONS,
    conflicts: CONFLICTS,
    sources: rows,
  };
}

function fallbackAnswer(query, reason) {
  const rows = reason === "no_sources" ? [] : sources().map((row) => ({ ...row, cited: false }));
  return {
    answer_version: "1",
    status: "fallback",
    reason,
    query,
    answer_view: false,
    summary: "",
    citations: [],
    limitations: [{ text: FALLBACK_TEXT[reason] ?? "no written answer was produced for this search", origin: "platform" }],
    conflicts: [],
    sources: rows,
  };
}

/* ---------- The read routes the sources point at ---------- */

/**
 * The address of every source the fixture can hand a click to, built from the
 * fixture itself: whatever href the answer carries is what this origin can
 * serve, and nothing else is served. A front end that composed its own path
 * gets a 404, which is the point.
 */
function readRoutes() {
  const paths = new Set();
  for (const source of sources()) {
    if (!source.href) continue;
    paths.add(new URL(source.href, "http://127.0.0.1").pathname);
  }
  return paths;
}

/* ---------- The server ---------- */

let mode = "answered";
let searchCalls = 0;
let reads = [];
let lastQuery = null;

function send(res, status, body, origin) {
  const headers = { "content-type": "application/json", "cache-control": "no-store" };
  // The browser calls this origin cross-origin with credentials (the session
  // cookie rides along), so the mock must be CORS-correct: echo the origin,
  // never "*".
  if (origin) {
    headers["access-control-allow-origin"] = origin;
    headers["access-control-allow-credentials"] = "true";
    headers["vary"] = "Origin";
  }
  res.writeHead(status, headers);
  res.end(JSON.stringify(body));
}

function envelope(code, message, status) {
  return {
    code,
    message,
    request_id: "e2e-search",
    retryable: status === 503,
  };
}

const server = createServer(async (req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`);
  const origin = req.headers.origin;

  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "access-control-allow-origin": origin ?? "*",
      "access-control-allow-credentials": "true",
      "access-control-allow-methods": "GET, POST, OPTIONS",
      "access-control-allow-headers": "accept, content-type",
      vary: "Origin",
    });
    res.end();
    return;
  }

  // Control plane for the spec. Not part of any API: the real origin has no
  // such route and the web app never calls it.
  if (url.pathname === "/__mode") {
    const set = url.searchParams.get("set");
    if (set !== null) {
      mode = set;
      searchCalls = 0;
      reads = [];
      lastQuery = null;
    }
    return send(res, 200, { mode, searchCalls, lastQuery }, origin);
  }
  if (url.pathname === "/__stats") {
    return send(res, 200, { mode, searchCalls, reads, lastQuery, cited: CITED_REFS }, origin);
  }

  if (url.pathname === "/api/v1/search" && req.method === "POST") {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const raw = Buffer.concat(chunks).toString("utf8");
    let query = "";
    try {
      query = JSON.parse(raw).query ?? "";
    } catch {
      return send(res, 400, envelope("SEARCH_INVALID_REQUEST", "request body must be a JSON object", 400), origin);
    }
    searchCalls += 1;
    lastQuery = query;
    if (query.trim() === "") {
      return send(res, 400, envelope("SEARCH_INVALID_REQUEST", "`query` is required", 400), origin);
    }

    if (mode === "error-400") {
      return send(res, 400, envelope("SEARCH_INVALID_REQUEST", "the search request is not valid", 400), origin);
    }
    if (mode === "error-401") {
      return send(res, 401, envelope("SEARCH_UNAUTHENTICATED", "search requires an authenticated session", 401), origin);
    }
    if (mode === "error-503") {
      return send(res, 503, envelope("SEARCH_UNAVAILABLE", "search is temporarily unavailable", 503), origin);
    }
    if (mode === "malformed") {
      // A 200 whose answer is missing the lists the page iterates: a broken
      // document, which must render as a failure and never as an answer with
      // nothing in it (docs/51).
      return send(res, 200, { search_id: searchID(query), answer: { answer_version: "1", status: "answered" } }, origin);
    }
    if (mode === "slow") {
      await new Promise((resolve) => setTimeout(resolve, 1200));
    }
    if (mode.startsWith("fallback-")) {
      const reason = mode.slice("fallback-".length);
      const answer = FALLBACK_REASONS.includes(reason)
        ? fallbackAnswer(query, reason)
        : fallbackAnswer(query, "unknown_reason");
      return send(res, 200, { search_id: searchID(query), answer }, origin);
    }
    const sparse = mode === "sparse";
    return send(res, 200, { search_id: searchID(query), answer: answeredAnswer(query, { sparse }) }, origin);
  }

  // The source reads. Only the addresses the fixture's sources carry.
  if (req.method === "GET" && readRoutes().has(url.pathname)) {
    reads.push(url.pathname);
    return send(res, 200, { at: url.pathname, note: "the address the answer gave for this source" }, origin);
  }

  if (url.pathname === "/api/v1/auth/session") {
    return send(res, 401, envelope("UNAUTHENTICATED", "authentication required", 401), origin);
  }

  send(res, 404, envelope("NOT_FOUND", `no such route: ${url.pathname}`, 404), origin);
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`mock-api: listening on http://127.0.0.1:${PORT}`);
  for (const path of readRoutes()) console.log(`mock-api: source read route ${path}`);
});
