/**
 * Tests for the client assets module (apps/web/lib/assets.ts).
 *
 * Plain-ESM test file like projects.test.mjs: Node ≥ 23.6 runs the
 * imported .ts chain through type stripping (assets.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/assets.test.mjs"
 *
 * What is worth testing here is not that a URL is a string. It is the two
 * things this module could get wrong in a way nobody would notice:
 *
 *   - the browse filter it sends, which decides whether the hub lists the
 *     right type or answers 400;
 *   - the refusal it renders, which for ASSET_NOT_FOUND must be ONE line —
 *     the API answers that code for an unknown pid, an unreadable project,
 *     an asset with nothing visible and a version the caller may not see,
 *     and a client that rendered four different messages would rebuild the
 *     oracle the single code exists not to be.
 */
import assert from "node:assert/strict";

import { translate } from "./i18n.ts";
import { test } from "node:test";

import {
  ApiError,
  ASSET_TYPES,
  assetHref,
  assetPageUrl,
  assetTypeEmptyKey,
  assetTypeLabelKey,
  assetVersionHref,
  browseUrl,
  createAssetsClient,
  creatorHandleLabel,
  assetCodeKey,
} from "./assets.ts";

/** The translator the pages pass in, resolved against the REAL en catalog.
 *
 *  T1105 moved these sentences into the catalog; a test asserting the old
 *  literals would now be asserting copy that no longer exists. Resolving
 *  through translate("en", …) keeps the assertions about the sentence a
 *  reader sees AND adds a property the literals could not have: every key
 *  has to exist in the catalog, in both locales. */
const en = (key, vars) => translate("en", key, vars);

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path(+query)" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const path = String(input).slice(API.length);
    calls.push({ path, init });
    const handler = routes[path];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "ASSET_NOT_FOUND", message: "asset not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

test("browse asks without a filter when none was given", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/assets": () => ({
      status: 200,
      json: async () => ({ type: null, types: [...ASSET_TYPES], assets: [] }),
    }),
  });
  const client = createAssetsClient(API, { fetch: fetchFn });
  await client.browse();
  await client.browse(null);
  assert.deepEqual(
    calls.map((c) => c.path),
    ["/api/v1/assets", "/api/v1/assets"],
  );
});

test("browse sends the type as the API's own value", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/assets?type=material_collection": () => ({
      status: 200,
      json: async () => ({ type: "material_collection", types: [...ASSET_TYPES], assets: [] }),
    }),
  });
  const client = createAssetsClient(API, { fetch: fetchFn });
  const list = await client.browse("material_collection");
  assert.equal(list.type, "material_collection");
  assert.equal(calls[0].path, "/api/v1/assets?type=material_collection");
});

test("every offered type is one the four-type set names", () => {
  assert.deepEqual([...ASSET_TYPES], ["dataset", "protocol", "material_collection", "benchmark"]);
  for (const type of ASSET_TYPES) {
    // The filter URL the page builds from the set is the one the API accepts.
    assert.equal(browseUrl(API, type), `${API}/api/v1/assets?type=${type}`);
  }
});

test("page asks for the newest version, or the one named", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/assets/mof-1": () => ({ status: 200, json: async () => ({ asset: { pid: "mof-1" } }) }),
    "/api/v1/assets/mof-1?version=1.0": () => ({
      status: 200,
      json: async () => ({ asset: { pid: "mof-1" }, version: { version: "1.0" } }),
    }),
  });
  const client = createAssetsClient(API, { fetch: fetchFn });
  await client.page("mof-1");
  const pinned = await client.page("mof-1", "1.0");
  assert.equal(pinned.version.version, "1.0");
  assert.deepEqual(
    calls.map((c) => c.path),
    ["/api/v1/assets/mof-1", "/api/v1/assets/mof-1?version=1.0"],
  );
});

test("the pid and the version are encoded into their own segments", () => {
  // A pid is a cuid2 and a label is [A-Za-z0-9._-], so neither is normally
  // escaped; the encoding is what keeps a value that came from a URL from
  // becoming a second path segment.
  assert.equal(assetHref("a/b"), "/assets/a%2Fb");
  assert.equal(assetVersionHref("mof-1", "1.0"), "/assets/mof-1/1.0");
  assert.equal(assetPageUrl(API, "mof-1"), `${API}/api/v1/assets/mof-1`);
  assert.equal(assetPageUrl(API, "mof-1", "2.0"), `${API}/api/v1/assets/mof-1?version=2.0`);
});

test("the session cookie rides along on both reads", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/assets": () => ({ status: 200, json: async () => ({ assets: [] }) }),
  });
  const client = createAssetsClient(API, { fetch: fetchFn });
  await client.browse();
  // Not a write: no CSRF header, no method override — but the cookie, so a
  // member sees their own project's private versions when the page is
  // loaded signed in.
  assert.equal(calls[0].init.credentials, "include");
  assert.equal(calls[0].init.method, undefined);
  assert.equal(calls[0].init.headers, undefined);
});

test("a refusal becomes an ApiError carrying the API's code", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/assets/unknown": () => ({
      status: 503,
      json: async () => ({ code: "ASSET_PAGE_UNAVAILABLE", message: "asset data is temporarily unavailable" }),
    }),
  });
  const client = createAssetsClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.page("unknown"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.status, 503);
      assert.equal(err.code, "ASSET_PAGE_UNAVAILABLE");
      assert.equal(err.retryable, true);
      return true;
    },
  );
});

test("every refusal the asset surface can answer has its own line, and the 404 has exactly one", () => {
  const notFound = en(assetCodeKey("ASSET_NOT_FOUND"));
  assert.notEqual(notFound, en(assetCodeKey("ASSET_PAGE_UNAVAILABLE")));
  assert.notEqual(notFound, en(assetCodeKey("ASSET_LIST_VALIDATION_FAILED")));
  assert.notEqual(notFound, en(assetCodeKey("SOMETHING_ELSE")));
  // The two validation codes are two surfaces: a bad type filter and a bad
  // version address are different mistakes, and the API answers them with
  // different codes (cmd/api/assetshttp). One line for both would tell the
  // reader their filter was wrong when their version address was.
  const badFilter = en(assetCodeKey("ASSET_LIST_VALIDATION_FAILED"));
  const badVersion = en(assetCodeKey("ASSET_PAGE_VALIDATION_FAILED"));
  assert.notEqual(badFilter, badVersion);
  assert.notEqual(badVersion, en(assetCodeKey("SOMETHING_ELSE")));
  assert.match(badVersion, /version/i);
  // The one line covers all four reasons the API answers this code for: the
  // page must not offer to distinguish "does not exist" from "not shared
  // with you".
  assert.match(notFound, /not shared with you/);
  assert.doesNotMatch(notFound, /private|forbidden|deleted/i);
});

test("an unknown type has no label key, so the page renders it verbatim", () => {
  // The set is closed on the server, so a value outside it is a value this
  // client does not know — and a key invented here would render as a
  // missing-key marker on a page that is merely reading a newer server. The
  // caller renders the raw value instead: `t(assetTypeLabelKey(x) ?? x)`.
  assert.equal(en(assetTypeLabelKey("dataset")), "Dataset");
  assert.equal(en(assetTypeLabelKey("benchmark")), "Benchmark");
  assert.equal(assetTypeLabelKey("model"), null);
  assert.equal(assetTypeLabelKey(""), null);
  // The browse list's empty line is per type for the same closed set.
  assert.equal(en(assetTypeEmptyKey("dataset")), "No published dataset assets yet.");
  assert.equal(assetTypeEmptyKey("model"), null);
});

test("a credited user is a mention and a credited organization is not", () => {
  // internal/assets.PageCreator documents `handle` as "a user's handle OR
  // an organization's slug". A mention prefix is the shape this platform
  // uses for PEOPLE, so an organization rendered with one would be rendered
  // as a user — the merge the two identity tables exist to prevent. The
  // user case is asserted first so a helper that never prefixed anything
  // could not pass by rendering both bare.
  assert.equal(creatorHandleLabel({ kind: "user", handle: "carol" }), "@carol");
  assert.equal(creatorHandleLabel({ kind: "organization", handle: "open-mof-lab" }), "open-mof-lab");
  assert.doesNotMatch(creatorHandleLabel({ kind: "organization", handle: "open-mof-lab" }), /@/);
});
