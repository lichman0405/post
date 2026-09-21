/**
 * Tests for the client attestation module (apps/web/lib/attestations.ts).
 *
 * Plain-ESM test file like assets.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (attestations.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/attestations.test.mjs"
 *
 * What is worth testing here is not that a URL is a string. It is the three
 * things this module could get wrong in a way nobody would notice:
 *
 *   - what the page RENDERS, which is the client's only privacy-relevant
 *     decision: a page that rendered the whole response would render
 *     whatever a future API added, and the test feeds it a payload carrying
 *     the attesting project's id and the basis state to show that neither
 *     reaches the rendered facts;
 *   - the refusal it renders, which for ATTESTATION_NOT_FOUND must be ONE
 *     line, because the API answers that code for an unknown pid, a
 *     non-pid and an unreadable record alike;
 *   - the labels, which must render an unknown closed-set value VERBATIM
 *     rather than as "other" — the server's set is the closed one, and a
 *     label invented here would misreport the row it labels.
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  attestationFacts,
  attestationHref,
  attestationUrl,
  attributionLabel,
  fetchAttestation,
  messageForAttestationCode,
  readAttestation,
  validationResultLabel,
  validationResultTone,
  validationTypeLabel,
  WITHHELD_FACTS,
} from "./attestations.ts";

const API = "http://127.0.0.1:8080";
const PID = "01jq8zg9k0000000000000000a";

/** One attestation as the API answers it. */
function apiAttestation(overrides = {}) {
  return {
    pid: PID,
    validation_type: "reproduction",
    validation_result: "confirmed",
    created_at: "2026-09-21T06:00:00.000Z",
    target: {
      kind: "protocol",
      object_id: "77777777-7777-4777-8777-777777777777",
      version_id: "33333333-3333-4333-8333-333333333333",
      title: "A published digestion protocol",
    },
    attributed_by: { mode: "anonymous" },
    disclosure: {
      is_evidence: false,
      names_evidence: false,
      statement: "An attestation states that an organization validated the named version…",
    },
    ...overrides,
  };
}

/** A scripted fake fetch: one status and one body. */
function fakeFetch(status, body) {
  const calls = [];
  return {
    calls,
    fetchFn: async (input, init) => {
      calls.push({ url: String(input), init });
      return { status, json: async () => body };
    },
  };
}

test("the read addresses the pid, and never a project", () => {
  assert.equal(attestationUrl(API, PID), `${API}/api/v1/attestations/${PID}`);
  assert.equal(attestationHref(PID), `/attestations/${PID}`);
  // A pid is 26 Crockford characters, so encoding is a no-op — but the
  // segment is encoded anyway, because the alternative is a path a pid
  // cannot contain today becoming one it can tomorrow.
  assert.equal(attestationHref("a/b"), "/attestations/a%2Fb");
  assert.ok(!attestationHref(PID).includes("project"), "the page's URL names no project");
});

test("the page renders the published facts and nothing the API did not send", () => {
  const facts = attestationFacts(apiAttestation());
  assert.deepEqual(
    facts.map((f) => f.label),
    ["Validation", "Result", "Attributed by", "Made public"],
  );
  assert.equal(facts[0].value, "Reproduction");
  assert.equal(facts[1].value, "Confirmed");
  assert.equal(facts[2].value, "Anonymous");

  // The privacy assertion. This payload carries the private footing the
  // real API never sends — the attesting project, the basis state and the
  // internal review — and none of it may reach the rendered facts. A page
  // that spread the response would render all three.
  const leaky = apiAttestation({
    attesting_project_id: "11111111-1111-4111-8111-111111111111",
    basis_state_id: "44444444-4444-4444-8444-444444444444",
    internal_review_id: "55555555-5555-4555-8555-555555555555",
    attesting_organization_id: "66666666-6666-4666-8666-666666666666",
  });
  const rendered = JSON.stringify(attestationFacts(leaky));
  for (const privateID of [
    "11111111-1111-4111-8111-111111111111",
    "44444444-4444-4444-8444-444444444444",
    "55555555-5555-4555-8555-555555555555",
    "66666666-6666-4666-8666-666666666666",
  ]) {
    assert.ok(!rendered.includes(privateID), `the rendered facts leak ${privateID}`);
  }
});

test("an anonymous record says so, and the two anonymous cases read alike", () => {
  assert.equal(attributionLabel({ mode: "anonymous" }), "Anonymous");
  // The API omits the organization for an anonymous record (Disclosure
  // omitempty), so the two situations — no organization at all, and an
  // organization that may not be named — arrive identically and must
  // render identically. Distinguishing them would disclose which holds.
  assert.equal(attributionLabel({ mode: "anonymous", organization: null }), "Anonymous");
  assert.equal(
    attributionLabel({ mode: "named", organization: { id: "o", slug: "att-labs", name: "Attestation Labs" } }),
    "Named: Attestation Labs",
  );
  // A "named" mode with no organization is not a state the API can send;
  // if it ever did, the page must not invent a name for it.
  assert.equal(attributionLabel({ mode: "named" }), "Anonymous");
});

test("labels render a value outside the closed set verbatim", () => {
  assert.equal(validationTypeLabel("reproduction"), "Reproduction");
  assert.equal(validationTypeLabel("rights_review"), "Rights review");
  assert.equal(validationTypeLabel("peer_review"), "peer_review");
  assert.equal(validationResultLabel("inconclusive"), "Inconclusive");
  assert.equal(validationResultLabel("mostly"), "mostly");
});

test("three results, three tones, and no tone for a value outside the set", () => {
  assert.equal(validationResultTone("confirmed"), "success");
  assert.equal(validationResultTone("refuted"), "danger");
  assert.equal(validationResultTone("inconclusive"), "attention");
  assert.equal(validationResultTone("mostly"), "");
});

test("the withheld list names what an attestation structurally does not carry", () => {
  // Fixed, and deliberately not derived from the response: a list computed
  // from the payload would grow a line the moment the API disclosed
  // something, which is the wrong direction.
  assert.ok(WITHHELD_FACTS.length > 0);
  const joined = WITHHELD_FACTS.join(" | ").toLowerCase();
  for (const what of ["project", "state", "review", "evidence", "count"]) {
    assert.ok(joined.includes(what), `the withheld list does not name ${what}`);
  }
});

test("ATTESTATION_NOT_FOUND is one line, whatever caused it", () => {
  const line = messageForAttestationCode("ATTESTATION_NOT_FOUND");
  assert.equal(line, "No such attestation.");
  // No hint that there might be one the caller cannot see: that would be
  // the existence oracle the single wire code exists not to be.
  assert.ok(!/may not|permission|private|hidden/i.test(line));
  assert.notEqual(messageForAttestationCode("SERVICE_UNAVAILABLE"), line);
  assert.equal(messageForAttestationCode("WHO_KNOWS"), "Something went wrong loading this attestation.");
});

test("a 404 is an answer about the record; an outage is not an answer at all", async () => {
  // The three outcomes, over the three responses the API can give. The
  // decision that matters is the middle pair: a 404 and a 503 must NOT
  // collapse into one "not here", or a temporary outage is published as a
  // permanent statement that a record which may well exist does not.
  const ok = fakeFetch(200, apiAttestation());
  const found = await readAttestation(API, PID, ok.fetchFn);
  assert.equal(found.kind, "found");
  assert.equal(found.attestation.pid, PID);

  // Every 404 — unknown pid, a segment that is not a pid, a record this
  // caller may not learn about — is one outcome, because the API answers
  // them with one body on purpose.
  for (const code of ["ATTESTATION_NOT_FOUND", "PROJECT_NOT_FOUND"]) {
    const missing = fakeFetch(404, { code, message: "not found" });
    assert.deepEqual(await readAttestation(API, PID, missing.fetchFn), { kind: "missing" });
  }

  // A 5xx keeps the API's own code, so the page can render the copy that
  // code is written for (messageForAttestationCode names the retry).
  const down = fakeFetch(503, { code: "SERVICE_UNAVAILABLE", message: "temporarily unavailable" });
  const unavailable = await readAttestation(API, PID, down.fetchFn);
  assert.deepEqual(unavailable, { kind: "unavailable", code: "SERVICE_UNAVAILABLE" });
  assert.notEqual(unavailable.kind, "missing");

  // A fetch that never produced a response is the same situation from the
  // reader's side, and it must not be reported as a missing record either.
  const thrown = await readAttestation(API, PID, async () => {
    throw new TypeError("fetch failed");
  });
  assert.deepEqual(thrown, { kind: "unavailable", code: "SERVICE_UNAVAILABLE" });

  // A 500 is not 404 either: the status is what decides, not the word
  // "not found" in a message.
  const boom = fakeFetch(500, { code: "INTERNAL_ERROR", message: "not found" });
  assert.deepEqual(await readAttestation(API, PID, boom.fetchFn), {
    kind: "unavailable",
    code: "INTERNAL_ERROR",
  });
});

test("the outage copy names the retry, and is not the missing copy", () => {
  // The page renders what the API's code says. This is the sentence a
  // reader of an unreachable record sees, and it must not be the sentence a
  // reader of an absent record sees.
  const outage = messageForAttestationCode("SERVICE_UNAVAILABLE");
  assert.match(outage, /temporarily unavailable/);
  assert.match(outage, /[Tt]ry again/);
  assert.notEqual(outage, messageForAttestationCode("ATTESTATION_NOT_FOUND"));
});

test("fetchAttestation reads the route and surfaces the API's envelope", async () => {
  const ok = fakeFetch(200, apiAttestation());
  const got = await fetchAttestation(API, PID, ok.fetchFn);
  assert.equal(got.pid, PID);
  assert.equal(ok.calls.length, 1);
  assert.equal(ok.calls[0].url, `${API}/api/v1/attestations/${PID}`);
  assert.equal(ok.calls[0].init.credentials, "include");

  const missing = fakeFetch(404, { code: "ATTESTATION_NOT_FOUND", message: "attestation not found" });
  await assert.rejects(
    () => fetchAttestation(API, PID, missing.fetchFn),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "ATTESTATION_NOT_FOUND");
      assert.equal(err.status, 404);
      assert.equal(err.retryable, false);
      return true;
    },
  );

  const down = fakeFetch(503, { code: "SERVICE_UNAVAILABLE", message: "temporarily unavailable" });
  await assert.rejects(
    () => fetchAttestation(API, PID, down.fetchFn),
    (err) => {
      assert.equal(err.code, "SERVICE_UNAVAILABLE");
      assert.equal(err.retryable, true);
      return true;
    },
  );
});
