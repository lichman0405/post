/**
 * Tests for the web app's security response headers (apps/web/next.config.ts).
 *
 * Plain-ESM test file on purpose, like its neighbours: the module under test
 * is TypeScript, `tsc` rejects ".ts" import specifiers unless
 * allowImportingTsExtensions is set, and Node ≥ 23.6 runs the imported .ts
 * module directly through type stripping. next.config.ts imports `next` with
 * `import type` only, which stripping erases — so the config can be evaluated
 * without node_modules and without starting Next.
 *
 * The config is not a document, it is the thing that decides what every
 * document carries, so the assertions are on VALUES, not on presence: a CSP
 * that quietly loses `frame-ancestors`, or a connect-src that quietly widens
 * to a wildcard, is exactly the change this file exists to refuse.
 *
 * The mutation table at the bottom is the same idea one level down: each
 * entry breaks one property of the real header set and asserts the checker
 * reports it. Without that, a checker that returned "no problems" for
 * everything would make every assertion above vacuous.
 *
 * Run: node --test "apps/web/lib/security-headers.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import nextConfig from "../next.config.ts";

/** Comments in the policy string are joined with "; " — one separator. */
const SEPARATOR = "; ";

/**
 * The policy, directive by directive, as a map. Parsing it (rather than
 * substring-matching the raw string) is what makes "script-src lost
 * 'unsafe-eval'" and "script-src gained 'unsafe-eval'" different assertions.
 */
function directivesOf(csp) {
  const out = new Map();
  for (const part of csp.split(SEPARATOR)) {
    const [name, ...sources] = part.trim().split(/\s+/);
    out.set(name, sources);
  }
  return out;
}

/**
 * Every problem with a header set, as strings. An empty array is the pass
 * condition; the mutation table below depends on the failure direction being
 * real.
 */
function problemsWith(feeds, { apiOrigin }) {
  const problems = [];
  if (feeds.length !== 1) {
    problems.push(`expected one header feed, got ${feeds.length}`);
  }
  const feed = feeds[0]?.headers;
  if (feed === undefined) {
    problems.push("no header feed at all");
    return problems;
  }

  const byKey = new Map();
  for (const entry of feed) {
    if (byKey.has(entry.key)) {
      problems.push(`duplicate header ${entry.key}`);
    }
    byKey.set(entry.key, entry.value);
  }
  if (feed.length !== 5) {
    problems.push(`expected exactly 5 headers, got ${feed.length}`);
  }

  const csp = feed.find((h) => h.key === "Content-Security-Policy")?.value;
  if (csp === undefined) {
    problems.push("no Content-Security-Policy");
  } else {
    const want = new Map([
      ["default-src", ["'self'"]],
      ["script-src", ["'self'", "'unsafe-inline'"]],
      ["style-src", ["'self'", "'unsafe-inline'"]],
      ["img-src", ["'self'", "data:", "blob:"]],
      ["font-src", ["'self'"]],
      // The API origin is not optional: client components fetch it from the
      // browser, so a policy without it is a policy that breaks the product.
      ["connect-src", ["'self'", apiOrigin]],
      ["object-src", ["'none'"]],
      ["base-uri", ["'none'"]],
      ["form-action", ["'self'"]],
      ["frame-ancestors", ["'none'"]],
      ["frame-src", ["'none'"]],
    ]);
    const got = directivesOf(csp);
    for (const [name, sources] of want) {
      const actual = got.get(name);
      if (actual === undefined) {
        problems.push(`CSP is missing ${name}`);
        continue;
      }
      if (actual.join(" ") !== sources.join(" ")) {
        problems.push(
          `CSP ${name} is "${actual.join(" ")}", expected "${sources.join(" ")}"`,
        );
      }
    }
    for (const name of got.keys()) {
      if (!want.has(name)) {
        problems.push(`CSP carries an unexpected directive ${name}`);
      }
    }
    // Named separately from the directive map because both are the point of
    // the policy and both are one token away from being given up quietly.
    if (csp.includes("'unsafe-eval'")) {
      problems.push("CSP allows 'unsafe-eval'");
    }
    if (/(^|\s)\*(\s|;|$)/.test(csp)) {
      problems.push("CSP contains a wildcard source");
    }
    // A scheme with no host (https:, ws:) admits every origin of that
    // scheme. data: and blob: are not that: they name no origin at all, and
    // img-src's use of them is asserted in the directive map above.
    if (/(^|\s)(https?|wss?):(\s|;|$)/.test(csp)) {
      problems.push("CSP contains a bare scheme source");
    }
  }

  const exact = [
    ["X-Content-Type-Options", "nosniff"],
    ["X-Frame-Options", "DENY"],
    ["Referrer-Policy", "no-referrer"],
    [
      "Permissions-Policy",
      "accelerometer=(), autoplay=(), camera=(), display-capture=(), " +
        "encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), " +
        "magnetometer=(), microphone=(), midi=(), payment=(), " +
        "picture-in-picture=(), publickey-credentials-get=(), " +
        "screen-wake-lock=(), usb=(), xr-spatial-tracking=()",
    ],
  ];
  for (const [key, value] of exact) {
    const actual = byKey.get(key);
    if (actual === undefined) {
      problems.push(`missing header ${key}`);
    } else if (actual !== value) {
      problems.push(`${key} is "${actual}", expected "${value}"`);
    }
  }

  // The framework's own advertising header. It is suppressed through
  // poweredByHeader (asserted below), never by emitting an empty header —
  // a header with an empty value is a header a proxy may fill in.
  if (byKey.has("X-Powered-By")) {
    problems.push("X-Powered-By is present (poweredByHeader is the control)");
  }
  if (feeds[0]?.source !== "/:path*") {
    problems.push(
      `headers() applies to "${feeds[0]?.source}", which is not every path`,
    );
  }
  return problems;
}

/** The header set the config serves right now, for the API origin given. */
async function headerSetWith(apiBaseUrl) {
  const previous = process.env.API_BASE_URL;
  try {
    if (apiBaseUrl === undefined) {
      delete process.env.API_BASE_URL;
    } else {
      process.env.API_BASE_URL = apiBaseUrl;
    }
    return await nextConfig.headers();
  } finally {
    if (previous === undefined) {
      delete process.env.API_BASE_URL;
    } else {
      process.env.API_BASE_URL = previous;
    }
  }
}

const API_ORIGIN = "http://127.0.0.1:18080";

test("the config serves the full header set on every path", async () => {
  const entries = await headerSetWith(API_ORIGIN);
  assert.deepEqual(problemsWith(entries, { apiOrigin: API_ORIGIN }), []);

  const feed = entries[0];
  assert.equal(feed.source, "/:path*", "headers must cover the whole app");

  const csp = feed.headers.find(
    (h) => h.key === "Content-Security-Policy",
  ).value;
  assert.equal(
    csp,
    [
      "default-src 'self'",
      "script-src 'self' 'unsafe-inline'",
      "style-src 'self' 'unsafe-inline'",
      "img-src 'self' data: blob:",
      "font-src 'self'",
      `connect-src 'self' ${API_ORIGIN}`,
      "object-src 'none'",
      "base-uri 'none'",
      "form-action 'self'",
      "frame-ancestors 'none'",
      "frame-src 'none'",
    ].join(SEPARATOR),
  );
});

test("connect-src names exactly the API origin the browser fetches", async () => {
  const entries = await headerSetWith("http://127.0.0.1:18080");
  const csp = entries[0].headers.find(
    (h) => h.key === "Content-Security-Policy",
  ).value;
  assert.deepEqual(directivesOf(csp).get("connect-src"), [
    "'self'",
    "http://127.0.0.1:18080",
  ]);

  // A path on the variable must not leak into the policy: connect-src takes
  // origins, and a path source would be silently ignored by the browser.
  const withPath = await headerSetWith("https://api.example.test/v1/");
  const cspWithPath = withPath[0].headers.find(
    (h) => h.key === "Content-Security-Policy",
  ).value;
  assert.deepEqual(directivesOf(cspWithPath).get("connect-src"), [
    "'self'",
    "https://api.example.test",
  ]);
});

test("a missing API_BASE_URL leaves connect-src at 'self', not at a guess", async () => {
  const entries = await headerSetWith(undefined);
  const csp = entries[0].headers.find(
    (h) => h.key === "Content-Security-Policy",
  ).value;
  // The fail-closed direction: no variable means no extra origin. It never
  // means "allow anything", and it never means a hard-coded host that would
  // be wrong everywhere but the developer's machine.
  assert.deepEqual(directivesOf(csp).get("connect-src"), ["'self'"]);
});

test("every property of the header set is one mutation away from a refusal", async () => {
  const real = await headerSetWith(API_ORIGIN);

  /** The header entries of the single feed, cloned so mutations are local. */
  const clone = () => real[0].headers.map((h) => ({ ...h }));
  const replace = (headers, key, value) => {
    const found = headers.find((h) => h.key === key);
    assert.ok(found, `fixture: no ${key} to mutate`);
    found.value = value;
    return headers;
  };
  const patchCSP = (headers, from, to) => {
    const found = headers.find((h) => h.key === "Content-Security-Policy");
    assert.ok(found.value.includes(from), `fixture: CSP has no "${from}"`);
    found.value = found.value.replace(from, to);
    return headers;
  };

  const mutations = [
    ["the whole policy is dropped", (h) => h.filter((x) => x.key !== "Content-Security-Policy")],
    ["default-src widens to a wildcard", (h) => patchCSP(h, "default-src 'self'", "default-src *")],
    ["script-src gains 'unsafe-eval'", (h) => patchCSP(h, "script-src 'self' 'unsafe-inline'", "script-src 'self' 'unsafe-inline' 'unsafe-eval'")],
    ["script-src gains a remote origin", (h) => patchCSP(h, "script-src 'self' 'unsafe-inline'", "script-src 'self' 'unsafe-inline' https://cdn.example.test")],
    ["connect-src widens to a wildcard", (h) => patchCSP(h, `connect-src 'self' ${API_ORIGIN}`, "connect-src *")],
    ["connect-src loses the API origin", (h) => patchCSP(h, `connect-src 'self' ${API_ORIGIN}`, "connect-src 'self'")],
    ["frame-ancestors is dropped", (h) => patchCSP(h, "; frame-ancestors 'none'", "")],
    ["frame-ancestors is relaxed", (h) => patchCSP(h, "frame-ancestors 'none'", "frame-ancestors 'self'")],
    ["object-src is dropped", (h) => patchCSP(h, "; object-src 'none'", "")],
    ["base-uri is dropped", (h) => patchCSP(h, "; base-uri 'none'", "")],
    ["form-action widens to any origin", (h) => patchCSP(h, "form-action 'self'", "form-action *")],
    ["nosniff is deleted", (h) => h.filter((x) => x.key !== "X-Content-Type-Options")],
    ["nosniff is misspelled", (h) => replace(h, "X-Content-Type-Options", "sniff")],
    ["framing is allowed again", (h) => replace(h, "X-Frame-Options", "SAMEORIGIN")],
    ["referrers start leaking", (h) => replace(h, "Referrer-Policy", "strict-origin-when-cross-origin")],
    ["the camera is allowed again", (h) => replace(h, "Permissions-Policy", "camera=()")],
    ["X-Powered-By comes back", (h) => [...h, { key: "X-Powered-By", value: "Next.js" }]],
  ];

  for (const [name, mutate] of mutations) {
    const mutated = mutate(clone());
    const problems = problemsWith(
      [{ source: "/:path*", headers: mutated }],
      { apiOrigin: API_ORIGIN },
    );
    assert.ok(
      problems.length > 0,
      `the checker accepted a header set where ${name}`,
    );
  }

  // And the scoping control, which is not a header but decides which
  // responses carry them at all.
  assert.ok(
    problemsWith([{ source: "/", headers: clone() }], { apiOrigin: API_ORIGIN })
      .length > 0,
    "the checker accepted a header feed that does not cover every path",
  );
});

test("the framework's own header is suppressed in the config, not emptied", () => {
  assert.equal(nextConfig.poweredByHeader, false);
});
