/**
 * Tests for the search answer client (apps/web/lib/search.ts).
 *
 * Plain-ESM test file like assets.test.mjs: Node ≥ 23.6 runs the imported .ts
 * chain through type stripping (search.ts is import-free by construction);
 * the module is fully typechecked by `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/search.test.mjs"
 *
 * What is worth testing here is not that a string is a string. It is the
 * three things this module could get wrong in a way the page would show:
 *
 *   - the request it sends, which decides whether the answer comes back at
 *     all (the cookie, the JSON body, the question);
 *   - the refusals it turns into codes, because a body that is not an answer
 *     must be an ERROR and never an answer with nothing in it (docs/51:
 *     failure is not emptiness);
 *   - the ONE projection it performs — statements' refs onto sources —
 *     which must never attach a claim to a source the answer did not return.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  FALLBACK_REASONS,
  citedSources,
  createSearchClient,
  evidenceRows,
  fallbackHeadline,
  isSearchResponse,
  messageForSearchCode,
  sourceHref,
  sourceKindLabel,
  sourceLabel,
} from "./search.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: one handler per call, recorded. */
function fakeFetch(handlers) {
  const calls = [];
  let i = 0;
  const fetchFn = async (input, init = {}) => {
    calls.push({ url: String(input), init });
    const handler = handlers[Math.min(i, handlers.length - 1)];
    i += 1;
    return handler();
  };
  return { fetchFn, calls };
}

function jsonResponse(status, body) {
  return { status, json: async () => body };
}

/** A minimal well-formed answer, with the five sources the e2e fixture uses. */
function answerBody(overrides = {}) {
  return {
    search_id: "01j9z6k3m4n5p6q7r8s9t0v1w9001",
    answer: {
      answer_version: "1",
      status: "answered",
      query: "which MOF materials show CO2 uptake above 3 mmol/g?",
      answer_view: true,
      summary: "Mg-MOF-74 is the reported uptake leader at 298 K [1].",
      citations: ["asset:AST-0001@2"],
      limitations: [],
      conflicts: [],
      sources: [
        {
          rank: 1,
          ref: "asset:AST-0001@2",
          kind: "document",
          entity_type: "asset",
          title: "Mg-MOF-74 CO2 uptake at 298 K",
          version: "2",
          href: "/api/v1/assets/AST-0001?version=2",
          project_id: "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d",
          labels: ["independently_reproduced"],
          factors: [],
          cited: true,
        },
      ],
      ...overrides,
    },
  };
}

test("search posts the question as JSON, with the session cookie", async () => {
  const { fetchFn, calls } = fakeFetch([() => jsonResponse(200, answerBody())]);
  await createSearchClient(API, { fetch: fetchFn }).search("CO2 uptake");

  assert.equal(calls.length, 1);
  assert.equal(calls[0].url, `${API}/api/v1/search`);
  assert.equal(calls[0].init.method, "POST");
  assert.equal(calls[0].init.credentials, "include");
  assert.equal(calls[0].init.headers["content-type"], "application/json");
  assert.deepEqual(JSON.parse(calls[0].init.body), { query: "CO2 uptake" });
});

test("a refusal becomes an ApiError carrying the wire code and request id", async () => {
  const { fetchFn } = fakeFetch([
    () =>
      jsonResponse(503, {
        code: "SEARCH_UNAVAILABLE",
        message: "search is temporarily unavailable",
        request_id: "req-42",
        retryable: true,
      }),
  ]);
  await assert.rejects(
    () => createSearchClient(API, { fetch: fetchFn }).search("q"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "SEARCH_UNAVAILABLE");
      assert.equal(err.status, 503);
      assert.equal(err.requestId, "req-42");
      assert.equal(err.retryable, true);
      return true;
    },
  );
});

test("a 200 whose body is not an answer is an error, not an empty answer", async () => {
  // The failure this guards against: a truncated or renamed document
  // rendered as "this search found nothing".
  const { fetchFn } = fakeFetch([() => jsonResponse(200, { search_id: "x", answer: { status: "answered" } })]);
  await assert.rejects(
    () => createSearchClient(API, { fetch: fetchFn }).search("q"),
    (err) => {
      assert.equal(err.code, "MALFORMED_ANSWER");
      return true;
    },
  );
});

test("a fetch that throws is unreachable, not an empty answer", async () => {
  const fetchFn = async () => {
    throw new TypeError("fetch failed");
  };
  await assert.rejects(
    () => createSearchClient(API, { fetch: fetchFn }).search("q"),
    (err) => {
      assert.equal(err.code, "UNREACHABLE");
      return true;
    },
  );
});

test("a fallback answer is a well-formed answer", () => {
  const body = answerBody({
    status: "fallback",
    reason: "no_provider",
    answer_view: false,
    summary: "",
    citations: [],
  });
  assert.equal(isSearchResponse(body), true);
});

test("a status outside the vocabulary is refused", () => {
  const body = answerBody({ status: "maybe" });
  assert.equal(isSearchResponse(body), false);
});

test("an answer with no sources key is refused (an absent list is not an empty one)", () => {
  const body = answerBody();
  delete body.answer.sources;
  assert.equal(isSearchResponse(body), false);
});

test("sourceHref resolves the answer's own path and leaves nothing else", () => {
  assert.equal(sourceHref(API, "/api/v1/assets/AST-0001"), `${API}/api/v1/assets/AST-0001`);
  assert.equal(sourceHref(`${API}/`, "/api/v1/knowledge/KNW-1"), `${API}/api/v1/knowledge/KNW-1`);
  assert.equal(sourceHref(API, ""), "");
  assert.equal(sourceHref(API, undefined), "");
  // An already-absolute address is never re-prefixed.
  assert.equal(sourceHref(API, "https://example.test/x"), "https://example.test/x");
});

test("citedSources is the subset the answer marked, not a re-recipe", () => {
  const body = answerBody({
    sources: [
      { rank: 1, ref: "a", kind: "document", labels: [], factors: [], cited: true },
      { rank: 2, ref: "b", kind: "document", labels: [], factors: [], cited: false },
      { rank: 3, ref: "c", kind: "document", labels: [], factors: [], cited: true },
    ],
    citations: ["a", "c"],
  });
  assert.deepEqual(
    citedSources(body.answer).map((s) => s.ref),
    ["a", "c"],
  );
});

test("evidenceRows aligns a statement's refs with the answer's sources", () => {
  const body = answerBody({
    sources: [
      { rank: 1, ref: "a", kind: "document", labels: [], factors: [], cited: true },
      { rank: 2, ref: "b", kind: "document", labels: [], factors: [], cited: false },
    ],
    limitations: [{ text: "the vector signal did not run", origin: "platform" }],
    conflicts: [{ text: "a and b disagree", refs: ["a", "b"], origin: "platform" }],
  });
  const rows = evidenceRows(body.answer);
  assert.equal(rows.length, 2);
  assert.deepEqual(rows[0].refs, []);
  assert.deepEqual(rows[0].sources, []);
  assert.deepEqual(
    rows[1].sources.map((s) => s.ref),
    ["a", "b"],
  );
  assert.equal(rows[1].section, "conflict");
});

test("evidenceRows keeps a ref that names no source instead of inventing one", () => {
  const body = answerBody({
    sources: [{ rank: 1, ref: "a", kind: "document", labels: [], factors: [], cited: true }],
    conflicts: [{ text: "a and z disagree", refs: ["a", "z"], origin: "platform" }],
  });
  const rows = evidenceRows(body.answer);
  assert.deepEqual(rows[0].refs, ["a", "z"]);
  assert.deepEqual(
    rows[0].sources.map((s) => s.ref),
    ["a"],
  );
});

test("every fallback reason has its own sentence", () => {
  const headlines = FALLBACK_REASONS.map((reason) => fallbackHeadline(reason));
  assert.equal(new Set(headlines).size, FALLBACK_REASONS.length);
  for (const headline of headlines) {
    assert.ok(headline.length > 0);
    assert.ok(!/something went wrong/i.test(headline));
  }
  // A reason this page has not been taught is still a fallback with a line.
  assert.ok(fallbackHeadline("brand_new_reason").length > 0);
});

test("every code this page can be handed has its own line", () => {
  const codes = [
    "SEARCH_INVALID_REQUEST",
    "SEARCH_UNAUTHENTICATED",
    "SEARCH_UNAVAILABLE",
    "SEARCH_RECORD_FAILED",
    "MALFORMED_ANSWER",
    "UNREACHABLE",
  ];
  const lines = codes.map(messageForSearchCode);
  assert.equal(new Set(lines).size, codes.length);
});

test("a source's label falls back to its ref only when it has no title", () => {
  assert.equal(sourceLabel({ ref: "a", title: "T" }), "T");
  assert.equal(sourceLabel({ ref: "a", title: "  " }), "a");
  assert.equal(sourceLabel({ ref: "a" }), "a");
  assert.equal(sourceKindLabel({ kind: "document", entity_type: "asset" }), "document · asset");
  assert.equal(sourceKindLabel({ kind: "object_version", object_type: "finding" }), "object_version · finding");
  assert.equal(sourceKindLabel({ kind: "object_version" }), "object_version");
});
