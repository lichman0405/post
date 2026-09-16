/**
 * Tests for the public-entity metadata module (apps/web/lib/entity-meta.ts,
 * T0801).
 *
 * Plain-ESM test file like correlation.test.mjs: Node ≥ 23.6 runs the
 * imported .ts through type stripping (entity-meta.ts is import-free except
 * for the erased `import type`); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * What is pinned here, and why each one is pinned rather than assumed:
 *
 *   - the ANONYMOUS ask. Every fetch must go out with `credentials:
 *     "omit"` and no cookie header: that is the whole mechanism by which a
 *     reader's own session can never make a private entity indexable. A
 *     test that only checked the outcome could not see a later change that
 *     forwarded cookies and still passed against a public fixture.
 *   - the FAIL-CLOSED direction. A refusal, a timeout, a thrown transport
 *     error and a malformed payload all mean "not public" — never "index
 *     it anyway" and never a thrown render.
 *   - the INDISTINGUISHABILITY of the unindexable head: a private entity
 *     and an unknown id produce the same <head> byte for byte. If they
 *     ever diverge, a crawler (or anyone) can enumerate private ids by
 *     asking.
 *   - the ORIGIN rules (resolvePublicOrigin): a configured origin wins,
 *     the request-derived fallback is validated, and a Host header that is
 *     not a host builds nothing.
 *   - the SITEMAP source: entries are exactly the rows the anonymous
 *     public reads returned, at the entity's own path.
 *
 * Run: node --test "apps/web/lib/entity-meta.test.mjs"
 */
import { test } from "node:test";
import assert from "node:assert/strict";

import {
  HIDDEN_PAGE_DESCRIPTION,
  HIDDEN_PAGE_TITLE,
  fetchPublicAssetMeta,
  fetchPublicIndex,
  fetchPublicProfileMeta,
  fetchPublicProjectMeta,
  fetchPublicReleaseMeta,
  hiddenPageMetadata,
  oneLine,
  publicEntityPath,
  publicIndexEntries,
  publicPageMetadata,
  resolvePublicOrigin,
} from "./entity-meta.ts";

const API = "http://api.test";

/** Replaces global fetch for one test; returns the calls it recorded. */
function stubFetch(handler) {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, init) => {
    calls.push({ url: String(url), init: init ?? {} });
    return handler(String(url), init ?? {});
  };
  return {
    calls,
    restore() {
      globalThis.fetch = original;
    },
  };
}

function jsonResponse(status, body) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

/** The API's standard refusal envelope (docs/45): no entity, one code. */
function notFound() {
  return jsonResponse(404, { code: "PROJECT_NOT_FOUND", message: "project not found" });
}

// ---------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------

test("oneLine collapses whitespace and trims", () => {
  assert.equal(oneLine("  a \n b\t\tc  "), "a b c");
});

test("oneLine cuts long text at the limit with one ellipsis", () => {
  const long = "x".repeat(400);
  const cut = oneLine(long);
  assert.equal(cut.length, 160);
  assert.ok(cut.endsWith("…"), cut.slice(-5));
  assert.equal(oneLine("short"), "short");
});

test("publicEntityPath spells every public entity route", () => {
  assert.equal(publicEntityPath("project", "p-1"), "/projects/p-1");
  assert.equal(publicEntityPath("release", "p-1", "r-9"), "/projects/p-1/releases/r-9");
  assert.equal(publicEntityPath("asset", "pid-1"), "/assets/pid-1");
  assert.equal(publicEntityPath("asset", "pid-1", undefined, "1.0"), "/assets/pid-1/1.0");
  assert.equal(publicEntityPath("profile", "u-1"), "/users/u-1");
});

test("publicEntityPath encodes what a URL must encode", () => {
  assert.equal(publicEntityPath("asset", "a/b c"), "/assets/a%2Fb%20c");
  assert.equal(publicEntityPath("asset", "pid", undefined, "v 1/2"), "/assets/pid/v%201%2F2");
});

// ---------------------------------------------------------------------
// The two heads
// ---------------------------------------------------------------------

test("a public entity is indexable, canonical and named in its own words", () => {
  const meta = publicPageMetadata(
    { name: "Open Materials Lab", description: "Screening MOFs for CO2 capture." },
    "https://post.example/projects/p-1",
  );
  assert.equal(meta.title, "Open Materials Lab — POST");
  assert.equal(meta.description, "Screening MOFs for CO2 capture.");
  assert.deepEqual(meta.robots, { index: true, follow: true });
  assert.deepEqual(meta.alternates, { canonical: "https://post.example/projects/p-1" });
  assert.equal(meta.openGraph.url, "https://post.example/projects/p-1");
  assert.equal(meta.openGraph.title, meta.title);
  assert.equal(meta.openGraph.description, meta.description);
  assert.equal(meta.openGraph.siteName, "POST");
});

test("a public entity with no words of its own gets a page-level description", () => {
  const meta = publicPageMetadata({ name: "X", description: "   " }, "https://post.example/projects/x");
  assert.equal(meta.description, "Public research on POST.");
});

test("a long entity description is cut to one meta line", () => {
  const meta = publicPageMetadata(
    { name: "X", description: "purpose ".repeat(60) },
    "https://post.example/projects/x",
  );
  assert.ok(meta.description.length <= 160, `description length ${meta.description.length}`);
  assert.ok(!meta.description.includes("\n"));
});

test("the unindexable head is not indexed and names nothing", () => {
  const meta = hiddenPageMetadata();
  assert.deepEqual(meta.robots, { index: false, follow: false });
  assert.equal(meta.title, HIDDEN_PAGE_TITLE);
  assert.equal(meta.description, HIDDEN_PAGE_DESCRIPTION);
  assert.equal(meta.alternates, undefined);
  assert.equal(meta.openGraph.url, undefined);
});

test("a private entity and an unknown id produce the same head", async () => {
  // The private project and the id that names nothing both answer 404 to
  // the anonymous read (T0106 existence hiding), so both heads must be
  // built from the same call, with nothing of the request in them.
  const stub = stubFetch(() => notFound());
  try {
    const priv = await fetchPublicProjectMeta(API, "11111111-1111-4111-8111-111111111111");
    const unknown = await fetchPublicProjectMeta(API, "99999999-9999-4999-8999-999999999999");
    assert.equal(priv, null);
    assert.equal(unknown, null);
    assert.deepEqual(hiddenPageMetadata(), hiddenPageMetadata());
  } finally {
    stub.restore();
  }
});

test("a metadata name cannot smuggle markup or newlines into the title", () => {
  const meta = publicPageMetadata(
    { name: "A\n</title><script>alert(1)</script> B", description: "d" },
    "https://post.example/projects/x",
  );
  assert.ok(!meta.title.includes("\n"), meta.title);
  // The value is JSON-encoded by Next when it renders the tag; what this
  // pins is that the module does not pre-format markup of its own.
  assert.ok(meta.title.startsWith("A </title><script>alert(1)</script> B — POST"), meta.title);
});

// ---------------------------------------------------------------------
// The origin
// ---------------------------------------------------------------------

test("a configured origin wins and is normalized", () => {
  assert.equal(
    resolvePublicOrigin("https://post.example/", { host: "internal.local:3000" }),
    "https://post.example",
  );
});

test("an unusable configured origin falls back to the request, not to itself", () => {
  assert.equal(
    resolvePublicOrigin("not a url", { host: "post.example" }),
    "http://post.example",
  );
  assert.equal(
    resolvePublicOrigin("ftp://post.example", { host: "post.example" }),
    "http://post.example",
  );
});

test("the request origin honours the proxy headers a deployment sets", () => {
  assert.equal(
    resolvePublicOrigin("", {
      host: "127.0.0.1:3000",
      forwardedHost: "post.example",
      forwardedProto: "https",
    }),
    "https://post.example",
  );
});

test("a Host header that is not a host builds nothing", () => {
  for (const host of ["post.example/evil", "evil.com:80/../x", "a b", "user@host", "", "x".repeat(260)]) {
    assert.equal(resolvePublicOrigin("", { host }), "", `host ${JSON.stringify(host)}`);
  }
});

test("a request with no usable origin yields the empty string, never a guess", () => {
  assert.equal(resolvePublicOrigin("", {}), "");
});

// ---------------------------------------------------------------------
// The anonymous reads
// ---------------------------------------------------------------------

test("every read is anonymous: no credentials, no cookie, no auth header", async () => {
  const stub = stubFetch(() => jsonResponse(200, { name: "N" }));
  try {
    await fetchPublicProjectMeta(API, "p-1");
    assert.equal(stub.calls.length, 1);
    const init = stub.calls[0].init;
    assert.equal(init.credentials, "omit");
    const headers = init.headers ?? {};
    const names = Object.keys(headers).map((k) => k.toLowerCase());
    assert.ok(!names.includes("cookie"), `headers ${names.join(",")}`);
    assert.ok(!names.includes("authorization"), `headers ${names.join(",")}`);
    assert.equal(init.cache, "no-store");
  } finally {
    stub.restore();
  }
});

test("a refusal is not an entity — for every kind", async () => {
  const stub = stubFetch(() => notFound());
  try {
    assert.equal(await fetchPublicProjectMeta(API, "p-1"), null);
    assert.equal(await fetchPublicReleaseMeta(API, "p-1", "r-1"), null);
    assert.equal(await fetchPublicAssetMeta(API, "pid-1"), null);
    assert.equal(await fetchPublicAssetMeta(API, "pid-1", "1.0"), null);
    assert.equal(await fetchPublicProfileMeta(API, "u-1"), null);
  } finally {
    stub.restore();
  }
});

test("a transport failure is not an entity either", async () => {
  const stub = stubFetch(() => {
    throw new Error("ECONNREFUSED");
  });
  try {
    assert.equal(await fetchPublicProjectMeta(API, "p-1"), null);
    assert.deepEqual(await fetchPublicIndex(API), { projects: [], assets: [] });
  } finally {
    stub.restore();
  }
});

test("a payload that is not JSON, or is not an object, is not an entity", async () => {
  const stub = stubFetch(() => new Response("<html>502</html>", { status: 200 }));
  try {
    assert.equal(await fetchPublicProjectMeta(API, "p-1"), null);
  } finally {
    stub.restore();
  }
});

test("a project without a name cannot be described", async () => {
  const stub = stubFetch(() => jsonResponse(200, { id: "p-1", purpose: "p" }));
  try {
    assert.equal(await fetchPublicProjectMeta(API, "p-1"), null);
  } finally {
    stub.restore();
  }
});

test("a public project reads its name and purpose", async () => {
  const stub = stubFetch(() =>
    jsonResponse(200, {
      id: "p-1",
      name: "Open Materials Lab",
      slug: "open-materials",
      visibility: "public",
      purpose: "Screen MOFs for CO2 capture.",
    }),
  );
  try {
    assert.deepEqual(await fetchPublicProjectMeta(API, "p-1"), {
      name: "Open Materials Lab",
      description: "Screen MOFs for CO2 capture.",
    });
    assert.equal(stub.calls[0].url, `${API}/api/v1/projects/p-1`);
  } finally {
    stub.restore();
  }
});

test("a release reads its project and its version", async () => {
  const stub = stubFetch((url) => {
    if (url.endsWith("/api/v1/projects/p-1")) return jsonResponse(200, { name: "Open Materials Lab" });
    return jsonResponse(200, { id: "r-1", version: "1.0", title: "Screening results" });
  });
  try {
    assert.deepEqual(await fetchPublicReleaseMeta(API, "p-1", "r-1"), {
      name: "Screening results",
      description: "Release 1.0 of Open Materials Lab — an immutable research snapshot.",
    });
    assert.equal(stub.calls[1].url, `${API}/api/v1/projects/p-1/releases/r-1`);
  } finally {
    stub.restore();
  }
});

test("a release under a project the caller cannot read is not described", async () => {
  const stub = stubFetch(() => notFound());
  try {
    assert.equal(await fetchPublicReleaseMeta(API, "p-1", "r-1"), null);
    // The release itself is never asked for: the project gate answers first.
    assert.equal(stub.calls.length, 1, stub.calls.map((c) => c.url).join(" "));
  } finally {
    stub.restore();
  }
});

test("an asset with a withheld origin names only itself", async () => {
  // The API withholds origin_project when the reader may not see the
  // project (internal/assets/page.go). The description must not claim one.
  const stub = stubFetch(() =>
    jsonResponse(200, {
      asset: { pid: "pid-1", type: "material_collection", title: "MOF-5 Collection", origin_project: null },
      version: { version: "2.0" },
    }),
  );
  try {
    assert.deepEqual(await fetchPublicAssetMeta(API, "pid-1"), {
      name: "MOF-5 Collection",
      description: "Research asset on POST.",
    });
  } finally {
    stub.restore();
  }
});

test("a public asset names its origin and, per version, the version", async () => {
  const stub = stubFetch(() =>
    jsonResponse(200, {
      asset: {
        pid: "pid-1",
        type: "material_collection",
        title: "MOF-5 Collection",
        origin_project: { id: "p-1", name: "Open Materials Lab", slug: "open-materials", visibility: "public" },
      },
      version: { version: "2.0" },
    }),
  );
  try {
    assert.deepEqual(await fetchPublicAssetMeta(API, "pid-1"), {
      name: "MOF-5 Collection",
      description: "Research asset published by Open Materials Lab on POST.",
    });
    assert.deepEqual(await fetchPublicAssetMeta(API, "pid-1", "2.0"), {
      name: "MOF-5 Collection",
      description: "Version 2.0 of a research asset published by Open Materials Lab on POST.",
    });
    assert.equal(stub.calls[1].url, `${API}/api/v1/assets/pid-1?version=2.0`);
  } finally {
    stub.restore();
  }
});

test("a profile reads its display name, handle and bio", async () => {
  const stub = stubFetch(() =>
    jsonResponse(200, { id: "u-1", handle: "alice", display_name: "Alice Guo", bio: "MOF chemist." }),
  );
  try {
    assert.deepEqual(await fetchPublicProfileMeta(API, "u-1"), {
      name: "Alice Guo (@alice)",
      description: "MOF chemist.",
    });
  } finally {
    stub.restore();
  }
});

// ---------------------------------------------------------------------
// The sitemap source
// ---------------------------------------------------------------------

test("the sitemap lists the public index, at the entity's own path", () => {
  const entries = publicIndexEntries("https://post.example", {
    projects: [{ id: "p-1", created_at: "2026-09-01T10:00:00Z" }],
    assets: [{ pid: "pid-1", created_at: "" }],
  });
  assert.deepEqual(
    entries.map((e) => e.url),
    [
      "https://post.example/",
      "https://post.example/projects",
      "https://post.example/assets",
      "https://post.example/projects/p-1",
      "https://post.example/assets/pid-1",
    ],
  );
  assert.ok(entries[3].lastModified instanceof Date);
  assert.equal(entries[4].lastModified, undefined);
});

test("the public index reads both lists, and a refusal yields no rows", async () => {
  const ok = stubFetch((url) =>
    url.endsWith("/api/v1/projects")
      ? jsonResponse(200, { projects: [{ id: "p-1", created_at: "2026-09-01T10:00:00Z" }, { nope: 1 }] })
      : jsonResponse(200, {
          type: null,
          types: ["dataset"],
          assets: [{ pid: "pid-1", latest_published_at: "2026-09-02T10:00:00Z" }, { pid: "" }],
        }),
  );
  try {
    assert.deepEqual(await fetchPublicIndex(API), {
      projects: [{ id: "p-1", created_at: "2026-09-01T10:00:00Z" }],
      assets: [{ pid: "pid-1", created_at: "2026-09-02T10:00:00Z" }],
    });
    assert.equal(ok.calls[0].init.credentials, "omit");
  } finally {
    ok.restore();
  }

  const down = stubFetch(() => {
    throw new Error("down");
  });
  try {
    assert.deepEqual(await fetchPublicIndex(API), { projects: [], assets: [] });
  } finally {
    down.restore();
  }
});
