/**
 * Tests for the client publish module (apps/web/lib/publish.ts).
 *
 * Plain-ESM test file like releases.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (publish.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/publish.test.mjs"
 *
 * Four things here are checked rather than assumed, because each of them
 * fails silently in the product:
 *
 *   - the candidate encoder, which has to produce EXACTLY the bytes Go's
 *     canonical manifest JSON produces. The publish gate compares the
 *     candidate's integrity hash to the digest of Go's own canonical bytes
 *     (internal/assets/gate.go validateIntegrityHash), so a difference of
 *     one escape is the difference between a publishable manifest and
 *     ASSET_INTEGRITY_HASH_MISMATCH. The golden vector below is the answer
 *     the production model gives for one document, printed by a throwaway
 *     `go run` over internal/assets.Manifest.CanonicalJSON/Hash;
 *   - the base64url round trip, which carries the whole candidate through a
 *     URL and whose failure mode is a page that cannot decode its own link;
 *   - the integrity comparison, which is the fail-closed check that stops
 *     this client from publishing a NARROWER document than the one it says
 *     it is publishing. Both directions are asserted: equal digests build a
 *     candidate, differing digests build none;
 *   - the link budget, whose whole point is that a real document can exceed
 *     it (so the guard is not dead code) while the documents this page
 *     actually offers do not.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  MAX_CANDIDATE_QUERY_LENGTH,
  buildCandidateFromStoredVersion,
  decodeCandidateQuery,
  encodeCandidateQuery,
  messageForPublishCode,
} from "./publish.ts";

/**
 * The production model's answer for one manifest, verbatim:
 *
 *   go run ./tmp: m.CanonicalJSON() and m.Hash() over the same document,
 *   where m is assets.Manifest{Version: assets.ManifestFormatVersion,
 *   AssetType: assets.TypeDataset, Metadata: {...}, DependencyPins: {...}}.
 *
 * It pins every rule the encoder below has to agree with: the struct's field
 * order, the metadata map sorted ascending, compact separators, Go's default
 * escaping of `<`, `>` and `&`, a non-ASCII character left alone, the nested
 * string map sorted too, and a dependency pin written as a BARE STRING
 * (internal/assets.DependencyPin is `type DependencyPin string`).
 */
const GOLDEN = {
  manifest: {
    version: 1,
    asset_type: "dataset",
    metadata: {
      access_level: "restricted",
      blob_ids: ["blob-fixture-0001"],
      data_type: "adsorption isotherm",
      purpose: "compare <100 °C & hold",
      quality_notes: { drift: "corrected", operator: "a & b" },
    },
    dependency_pins: [
      { pin: "tdwqgb4t3aaaaaaaaaaaaaaaaaa@1.0" },
      { pin: "abcde12345abcdefghijklmnop@2.0" },
    ],
  },
  canonical:
    '{"version":1,"asset_type":"dataset","metadata":{"access_level":"restricted",' +
    '"blob_ids":["blob-fixture-0001"],"data_type":"adsorption isotherm",' +
    '"purpose":"compare \\u003c100 °C \\u0026 hold","quality_notes":' +
    '{"drift":"corrected","operator":"a \\u0026 b"}},"dependency_pins":' +
    '["tdwqgb4t3aaaaaaaaaaaaaaaaaa@1.0","abcde12345abcdefghijklmnop@2.0"]}',
  hash: "8e2a0e596fbd81a7642ff0f21a617baed50751358834e44a33c53ec95b62d29a",
};

/** One stored version, as the asset page renders it. */
function stored(overrides = {}) {
  return {
    asset: { pid: "tdwqgb4t3aaaaaaaaaaaaaaaaaa", type: "dataset", origin_project: { id: "p-1" } },
    version: {
      version: "1.0",
      visibility: "private",
      integrityHash: "",
      ...(overrides.version ?? {}),
    },
    origin: [{ ref: "project:p-1" }, { ref: "release:r-1" }],
    rights: { version: 1, notes: "the published declaration" },
    metadata: [{ key: "purpose", value: "the isotherm series" }],
    dependencies: [],
    creators: [{ kind: "user", partyId: "u-1" }],
    ...overrides.rest,
  };
}

test("the rebuilt manifest hashes to the digest the production model gives", async () => {
  // The document is rebuilt from the page's blocks (a metadata list and a
  // pin list), not read back, so the encoder is exercised end to end: build
  // it with the stored digest unset, then compare the digest it produced to
  // Go's.
  const build = await buildCandidateFromStoredVersion({
    stored: stored({
      rest: {
        metadata: GOLDEN.manifest.metadata.purpose
          ? Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value }))
          : [],
        dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
        version: { version: "1.0", visibility: "private", integrityHash: "" },
      },
    }),
  });
  // integrityHash is empty, so this build is withheld — its rebuiltHash is
  // still the digest of the document it built.
  assert.equal(build.kind, "integrity-absent");
  assert.equal(build.rebuiltHash, GOLDEN.hash);
  assert.equal(build.pins, 2);
  assert.equal(build.metadataKeys, 5);

  // The golden bytes are the document Go digested: checked here so the
  // constant above cannot drift into a comment nobody can verify, and so the
  // digest's meaning (it IS the digest of those bytes) is stated.
  const { createHash } = await import("node:crypto");
  assert.equal(createHash("sha256").update(GOLDEN.canonical).digest("hex"), GOLDEN.hash);

  // And with that digest stored, the very same document is offered: the
  // encoder's bytes and Go's bytes are the same bytes, since nothing else
  // digests to that string.
  const refreshed = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: GOLDEN.hash },
      rest: {
        metadata: Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value })),
        dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
      },
    }),
  });
  assert.equal(refreshed.kind, "ready");
  assert.equal(refreshed.candidate.integrity_hash, GOLDEN.hash);
  assert.deepEqual(refreshed.candidate.manifest, GOLDEN.manifest);
});

test("a manifest whose digest differs is not offered a candidate", async () => {
  // The direction that matters: the page rendered FEWER pins than the stored
  // document declares (internal/assets' dependenciesOf drops a pin into a
  // non-public version), so the rebuild digests differently. The build must
  // refuse rather than hand back the narrower document.
  const withPin = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: "" },
      rest: { dependencies: [{ pin: "abc@1.0" }] },
    }),
  });
  assert.equal(withPin.kind, "integrity-absent");

  const withoutPin = await buildCandidateFromStoredVersion({
    stored: stored({
      // The stored digest covers a manifest WITH the pin; the page renders
      // none, so the rebuild covers a document without it.
      version: { version: "1.0", visibility: "private", integrityHash: withPin.rebuiltHash },
      rest: { dependencies: [] },
    }),
  });
  assert.equal(withoutPin.kind, "integrity-mismatch");
  assert.equal(withoutPin.declaredHash, withPin.rebuiltHash);
  assert.notEqual(withoutPin.rebuiltHash, withPin.rebuiltHash);
  assert.equal(withoutPin.pins, 0);
  // The sentence names the difference the reader can act on.
  assert.match(withoutPin.detail, /does not cover/);
  assert.match(withoutPin.detail, /0 dependency pins/);
  assert.equal(withoutPin.candidate, undefined);

  // The agreeing direction: the same document digests the same way twice.
  const agree = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: withoutPin.rebuiltHash },
      rest: { dependencies: [] },
    }),
  });
  assert.equal(agree.kind, "ready");
});

test("a version with no rights declaration publishes no rights declaration", async () => {
  // The stored version carries none (internal/assets' rightsOf answers null).
  // Filling the gap with a default document would render a declaration the
  // version never had AND replace the gate's own ASSET_MISSING_RIGHTS refusal
  // with a publication that claims rights nobody declared.
  const build = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: GOLDEN.hash },
      rest: {
        rights: null,
        metadata: Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value })),
        dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
      },
    }),
  });
  assert.equal(build.kind, "ready");
  assert.equal("rights" in build.candidate, false);
  assert.equal(build.candidate.rights, undefined);
  // Absent, not null: the route distinguishes "no document" from "a document
  // that is JSON null", and only the first is the honest refusal.
  assert.equal(
    JSON.parse(JSON.stringify(build.candidate)).rights,
    undefined,
  );

  // A stored declaration is carried verbatim, byte for byte.
  const declared = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: GOLDEN.hash },
      rest: {
        metadata: Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value })),
        dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
      },
    }),
  });
  assert.deepEqual(declared.candidate.rights, { version: 1, notes: "the published declaration" });
});

test("the stored digest is compared over its hex digits, not its spelling", async () => {
  const base = {
    rest: {
      metadata: Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value })),
      dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
    },
  };
  for (const spelling of [
    GOLDEN.hash,
    GOLDEN.hash.toUpperCase(),
    `sha256:${GOLDEN.hash}`,
    `  ${GOLDEN.hash}  `,
  ]) {
    const build = await buildCandidateFromStoredVersion({
      stored: stored({ ...base, version: { version: "1.0", visibility: "private", integrityHash: spelling } }),
    });
    assert.equal(build.kind, "ready", `expected ${JSON.stringify(spelling)} to be comparable`);
  }
  // One digit different is not a spelling difference.
  const altered = GOLDEN.hash.replace(/.$/, (c) => (c === "a" ? "b" : "a"));
  const mismatched = await buildCandidateFromStoredVersion({
    stored: stored({ ...base, version: { version: "1.0", visibility: "private", integrityHash: altered } }),
  });
  assert.equal(mismatched.kind, "integrity-mismatch");
});

test("a candidate survives the URL parameter it is carried in", async () => {
  const build = await buildCandidateFromStoredVersion({
    stored: stored({
      version: { version: "1.0", visibility: "private", integrityHash: GOLDEN.hash },
      rest: {
        metadata: Object.entries(GOLDEN.manifest.metadata).map(([key, value]) => ({ key, value })),
        dependencies: GOLDEN.manifest.dependency_pins.map((p) => ({ pin: p.pin })),
      },
    }),
  });
  const query = encodeCandidateQuery(build.candidate);
  // URL-safe: no '+', '/' or '=' may survive into the query string, and no
  // percent-encoding is needed to carry it.
  assert.match(query, /^[A-Za-z0-9_-]+$/);
  assert.equal(encodeURIComponent(query), query);
  // The bytes that come back are the bytes that went in, angle brackets and
  // all.
  const decoded = decodeCandidateQuery(query);
  assert.deepEqual(decoded, JSON.parse(JSON.stringify(build.candidate)));
  assert.equal(decoded.manifest.metadata.purpose, "compare <100 °C & hold");
});

test("every base64url length is reversible, including the padded cases", () => {
  // 1, 2 and 3-byte remainders take the three different padding paths.
  for (const text of ["a", "ab", "abc", "abcd", "à", "àé", "àéü", "日本語テキスト"]) {
    const query = encodeCandidateQuery({ marker: text });
    assert.equal(decodeCandidateQuery(query).marker, text, `round trip of ${JSON.stringify(text)}`);
  }
});

test("a candidate that does not fit the link budget is over it", () => {
  // The guard this module's MAX_CANDIDATE_QUERY_LENGTH exists for is only
  // reachable if a real document can exceed it — and the request line the
  // browser sends must still fit the web server's own limit, which is Node's
  // default header size.
  assert.ok(MAX_CANDIDATE_QUERY_LENGTH < 16384);

  const pad = (chars) => ({
    marker: "x".repeat(chars),
  });
  const small = encodeCandidateQuery(pad(200));
  const large = encodeCandidateQuery(pad(MAX_CANDIDATE_QUERY_LENGTH + 2000));
  assert.ok(small.length <= MAX_CANDIDATE_QUERY_LENGTH);
  assert.ok(large.length > MAX_CANDIDATE_QUERY_LENGTH);

  // And the documents this page actually offers ARE under it: the chain's
  // own candidate (five metadata keys, three origin refs, no pins) encoded
  // to well under a third of the budget in the browser e2e. A guard that
  // withheld legitimate links would be a different bug.
  const realistic = encodeCandidateQuery({
    asset_pid: "tdwqgb4t3aaaaaaaaaaaaaaaaaa",
    asset_type: "dataset",
    version: "1.0.1789000000-public",
    manifest: GOLDEN.manifest,
    rights: { version: 1 },
    origin_refs: ["project:p-1", "release:r-1", "object_version:o-1"],
    visibility: "public",
    integrity_hash: GOLDEN.hash,
    creator_ids: ["u-1"],
  });
  assert.ok(realistic.length < MAX_CANDIDATE_QUERY_LENGTH / 2, `realistic link is ${realistic.length} characters`);
});

test("a code the API really answers is never the generic fallback", () => {
  // The codes cmd/api/assetshttp writes for the publish and preview routes.
  // A reader who is told "something went wrong" when the API actually named
  // the cause has been told a falsehood: for a signed-out reader the cause
  // is the one thing they can fix.
  const codes = [
    "ASSET_PUBLISH_BLOCKED",
    "AUTH_UNAUTHENTICATED",
    "PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL",
    "PROJECT_FORBIDDEN",
    "ASSET_PUBLISH_AGENT_DENIED",
    "VALIDATION_FAILED",
    "ASSET_PREVIEW_VALIDATION_FAILED",
    "ASSET_PREVIEW_UNAVAILABLE",
    "ASSET_PUBLISH_PROJECT_NOT_FOUND",
    "PROJECT_NOT_FOUND",
    "ASSET_NOT_FOUND",
    "ASSET_ALREADY_EXISTS",
    "ASSET_VERSION_IMMUTABLE",
    "IDEMPOTENCY_CONFLICT",
    "RIGHTS_POLICY_BLOCKS_ACTION",
    "SERVICE_UNAVAILABLE",
  ];
  const seen = new Map();
  for (const code of codes) {
    const message = messageForPublishCode(code);
    assert.notEqual(message, messageForPublishCode("NO_SUCH_CODE"), `${code} fell through to the default`);
    assert.ok(!message.includes(code), `${code} is rendered as its own code`);
    assert.match(message, /^[A-Z].*\.$/, `${code}'s message is not a sentence: ${message}`);
    assert.ok(message.length > 30, `${code}'s message says too little: ${message}`);
    seen.set(code, message);
  }
  // Distinct causes get distinct sentences, so a reader can tell them apart.
  // The one intended pair is the two project-not-found codes: the preview
  // route and the publish route spell the same fact with different codes
  // (projects.CodeProjectNotFound, assetpublish.CodeProjectNotFound), and a
  // reader does not need two sentences for it.
  assert.equal(
    seen.get("ASSET_PUBLISH_PROJECT_NOT_FOUND"),
    seen.get("PROJECT_NOT_FOUND"),
  );
  assert.equal(new Set(seen.values()).size, codes.length - 1);

  // The version-label path is named, because it is the one refusal a reader
  // can act on by choosing another label.
  assert.match(messageForPublishCode("VALIDATION_FAILED"), /version label/i);
  // The anonymous path says what to do about it.
  assert.match(messageForPublishCode("AUTH_UNAUTHENTICATED"), /sign in/i);

  // An unknown code still has to answer with something, and it is the only
  // case that may be generic.
  assert.match(messageForPublishCode("SOMETHING_NEW"), /something went wrong/i);
});
