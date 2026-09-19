/**
 * Tests for the client Research Profile module
 * (apps/web/lib/research-profile.ts).
 *
 * Plain-ESM test file like profile.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping, and the module is import-free by
 * construction so the chain is one file. Types are checked separately by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/research-profile.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ResearchProfileError,
  createResearchProfileClient,
  describeRelation,
  describeVia,
  formatAffiliationWindow,
  formatDay,
  messageForResearchProfileCode,
  parseOrganizationProfile,
  parsePersonProfile,
  printableEntity,
} from "./research-profile.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes maps "path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const path = String(input).slice(API.length);
    calls.push({ method: init.method ?? "GET", path, credentials: init.credentials });
    const handler = routes[path];
    if (!handler) return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => () => ({ status: 200, json: async () => payload });

const PROJECT = {
  id: "0effccc8-3ba6-4abc-87de-f1b95920aa8f",
  slug: "open-lab",
  name: "Open Lab",
  url: "/projects/0effccc8-3ba6-4abc-87de-f1b95920aa8f",
};

/** A full Research Profile payload, as the API sends one. */
function personPayload(overrides = {}) {
  return {
    person: {
      id: "02040608-0a0c-4e10-9214-16181a1c1e20",
      handle: "alice",
      display_name: "Alice Researcher",
      bio: "zeolite screening",
      url: "/users/02040608-0a0c-4e10-9214-16181a1c1e20",
    },
    affiliations: [
      {
        organization: { id: "e8367850-91e3-414a-a848-2995bc34d28d", slug: "institute", name: "Institute", url: "/organizations/institute" },
        role: "contributor",
        affiliation_start: "2024-01-15",
        affiliation_end: "2025-06-30",
        verified: true,
      },
    ],
    contributions: [
      {
        event_type: "research_state.merged",
        role_codes: ["author"],
        occurred_at: "2026-01-10T10:00:00Z",
        accepted_context: true,
        released_context: true,
        via: "web",
        project: PROJECT,
      },
    ],
    assets: [
      {
        pid: "c2f8565a56ed41f2b037f3c0c6",
        title: "Open Set",
        asset_type: "dataset",
        version: "1.0",
        url: "/assets/c2f8565a56ed41f2b037f3c0c6/1.0",
        role: "creator",
        project: PROJECT,
        published_at: "2026-01-12T10:00:00Z",
      },
    ],
    reuse: [],
    reproductions: [
      { relation: "fails_to_reproduce", review_state: "unreviewed", created_at: "2026-02-25T10:00:00Z", project: null },
    ],
    projects: [{ id: PROJECT.id, slug: PROJECT.slug, name: PROJECT.name, url: PROJECT.url }],
    ...overrides,
  };
}

function orgPayload(overrides = {}) {
  return {
    organization: {
      id: "e8367850-91e3-414a-a848-2995bc34d28d",
      slug: "institute",
      name: "Institute",
      description: "heterogeneous catalysis",
      url: "/organizations/institute",
    },
    projects: [
      {
        id: PROJECT.id,
        slug: PROJECT.slug,
        name: PROJECT.name,
        purpose: "screen zeolites",
        activity_status: "active",
        url: PROJECT.url,
      },
    ],
    activity: [],
    assets: [],
    ...overrides,
  };
}

test("person reads the research-profile route anonymously", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/users/02040608-0a0c-4e10-9214-16181a1c1e20/research-profile": ok(personPayload()),
  });
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  const profile = await client.person("02040608-0a0c-4e10-9214-16181a1c1e20");

  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "GET");
  assert.equal(profile.person.handle, "alice");
  assert.equal(profile.contributions.length, 1);
  assert.equal(profile.contributions[0].project.slug, "open-lab");
});

test("organization reads the profile route and keeps the slug", async () => {
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/organizations/institute/profile": ok(orgPayload()),
  });
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  const profile = await client.organization("institute");

  assert.equal(calls[0].path, "/api/v1/organizations/institute/profile");
  assert.equal(profile.organization.slug, "institute");
  assert.equal(profile.projects[0].activity_status, "active");
});

test("a slug is encoded into the path, not interpolated raw", async () => {
  const { fetchFn, calls } = fakeFetch({});
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  await client.organization("a/../b").catch(() => {});
  assert.equal(calls[0].path, "/api/v1/organizations/a%2F..%2Fb/profile");
});

test("a 404 becomes the API's own code, and is not retryable", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/users/gone/research-profile": () => ({
      status: 404,
      json: async () => ({ code: "USER_NOT_FOUND", message: "no such user" }),
    }),
  });
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.person("gone"),
    (err) => {
      assert.ok(err instanceof ResearchProfileError);
      assert.equal(err.code, "USER_NOT_FOUND");
      assert.equal(err.status, 404);
      assert.equal(err.retryable, false);
      return true;
    },
  );
});

test("a 503 stays retryable and carries the API's message", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/organizations/institute/profile": () => ({
      status: 503,
      json: async () => ({ code: "SERVICE_UNAVAILABLE", message: "profiles are temporarily unavailable" }),
    }),
  });
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.organization("institute"),
    (err) => {
      assert.ok(err instanceof ResearchProfileError);
      assert.equal(err.code, "SERVICE_UNAVAILABLE");
      assert.equal(err.retryable, true);
      return true;
    },
  );
});

test("a body that is not JSON becomes an UNKNOWN error, not a crash", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/users/x/research-profile": () => ({
      status: 502,
      json: async () => {
        throw new Error("not json");
      },
    }),
  });
  const client = createResearchProfileClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.person("x"),
    (err) => {
      assert.equal(err.code, "UNKNOWN");
      assert.equal(err.status, 502);
      return true;
    },
  );
});

// ---------------------------------------------------------------------------
// The no-score rule, at the rendering boundary

test("a payload carrying a score field is refused, not rendered", () => {
  // The control FIRST: the same payload without the field parses, so the
  // refusal below is about the field and not about the fixture being
  // unparseable in general.
  const clean = personPayload();
  assert.equal(parsePersonProfile(clean).person.handle, "alice");

  for (const key of ["research_score", "rank", "total_contributions", "weight", "reputation", "impact"]) {
    const dirty = personPayload();
    dirty.person[key] = 1;
    assert.throws(
      () => parsePersonProfile(dirty),
      new RegExp(`\\.${key}`),
      `${key} must be refused at the rendering boundary`,
    );
  }
});

test("a score field nested inside a row is refused too", () => {
  const dirty = personPayload();
  const nested = personPayload();
  nested.contributions[0].metrics = { quality: 0.9 };
  dirty.contributions = nested.contributions;
  assert.throws(() => parsePersonProfile(dirty), /\.metrics/);
});

test("legitimate numeric and version fields are NOT mistaken for scores", () => {
  // The rule is about field NAMES, and this is the control that keeps it from
  // becoming "refuse anything with a number in it": a version label, a year
  // inside a date, an ongoing-reproduction count of zero rows.
  const profile = parsePersonProfile(personPayload());
  assert.equal(profile.assets[0].version, "1.0"); // a version, not a score
  assert.equal(profile.contributions[0].occurred_at, "2026-01-10T10:00:00Z");
  assert.deepEqual(profile.reuse, []);
});

test("the organization payload is held to the same rule", () => {
  assert.equal(parseOrganizationProfile(orgPayload()).organization.slug, "institute");
  const dirty = orgPayload();
  dirty.organization.rating = 5;
  assert.throws(() => parseOrganizationProfile(dirty), /\.rating/);
});

// ---------------------------------------------------------------------------
// What the parser preserves

test("a withheld project stays null — the payload never says 'unknown'", () => {
  const payload = personPayload();
  payload.assets[0].project = null;
  payload.reproductions[0].project = null;
  const profile = parsePersonProfile(payload);
  assert.equal(profile.assets[0].project, null);
  assert.equal(profile.reproductions[0].project, null);
  // ... and the row itself survives: a withheld identity is not a dropped row.
  assert.equal(profile.assets[0].title, "Open Set");
  assert.equal(profile.reproductions[0].relation, "fails_to_reproduce");
});

test("a reuse row names its project, and a payload that withholds one is refused", () => {
  // The control: a reuse row as the API builds it — the model drops a usage
  // whose project may not be named, so the row that survives always carries
  // the project it is a usage BY.
  const payload = personPayload();
  payload.reuse = [
    {
      pid: "c2f8565a56ed41f2b037f3c0c6",
      title: "Open Set",
      version: "1.0",
      url: "/assets/c2f8565a56ed41f2b037f3c0c6/1.0",
      asset_type: "dataset",
      dependency_type: "derived_from",
      declared_at: "2026-03-02T10:00:00Z",
      project: PROJECT,
    },
  ];
  const profile = parsePersonProfile(payload);
  assert.equal(profile.reuse[0].project.name, PROJECT.name);

  // And the shape the model cannot produce: a reuse row with no project. It is
  // refused rather than rendered as a placeholder — "used by a project not
  // named here" would state a fact about a project the reader can never
  // resolve, and the API's rule makes such a row not exist at all.
  const withheld = personPayload();
  withheld.reuse = [{ ...payload.reuse[0], project: null }];
  assert.throws(() => parsePersonProfile(withheld), /reuse names no project/);
});

test("an ENDED affiliation keeps both dates through the parser", () => {
  const profile = parsePersonProfile(personPayload());
  const [affiliation] = profile.affiliations;
  assert.equal(affiliation.affiliation_start, "2024-01-15");
  assert.equal(affiliation.affiliation_end, "2025-06-30");
  assert.equal(affiliation.role, "contributor");
  assert.equal(affiliation.verified, true);
});

test("an affiliation with no organization keeps the person's own row", () => {
  const payload = personPayload();
  payload.affiliations[0].organization = null;
  const profile = parsePersonProfile(payload);
  assert.equal(profile.affiliations[0].organization, null);
  assert.equal(profile.affiliations[0].role, "contributor");
  assert.equal(profile.affiliations[0].affiliation_start, "2024-01-15");
});

test("a person's own contributions carry no actor; an organization's do", () => {
  const own = parsePersonProfile(personPayload());
  assert.equal(own.contributions[0].actor, undefined);

  const activity = orgPayload();
  activity.activity = [
    {
      event_type: "research_state.merged",
      role_codes: ["author"],
      occurred_at: "2026-01-10T10:00:00Z",
      accepted_context: true,
      released_context: false,
      via: "web",
      project: PROJECT,
      actor: { id: "02040608-0a0c-4e10-9214-16181a1c1e20", handle: "alice", display_name: "Alice Researcher", url: "/users/02040608-0a0c-4e10-9214-16181a1c1e20" },
    },
  ];
  const org = parseOrganizationProfile(activity);
  assert.equal(org.activity[0].actor.handle, "alice");
});

test("a missing required field is refused rather than rendered as undefined", () => {
  const payload = personPayload();
  delete payload.person.handle;
  assert.throws(() => parsePersonProfile(payload), /handle/);
});

test("an asset without a role (the organization surface) parses", () => {
  const payload = orgPayload();
  payload.assets = [
    {
      pid: "c2f8565a56ed41f2b037f3c0c6",
      title: "Open Set",
      asset_type: "dataset",
      version: "1.0",
      url: "/assets/c2f8565a56ed41f2b037f3c0c6/1.0",
      project: PROJECT,
      published_at: "2026-01-12T10:00:00Z",
    },
  ];
  const org = parseOrganizationProfile(payload);
  assert.equal(org.assets[0].role, undefined);
  assert.equal(org.assets[0].project.slug, "open-lab");
});

test("a withheld identity prints NOTHING — never a placeholder naming the absence", () => {
  // printableEntity is where every row decides whether it has an entity to
  // print, and the affiliation row is the one that used to answer wrongly:
  // it rendered the string "Organization not named" where the name goes. That
  // sentence is a positive claim on an anonymous page — the model nulls an
  // affiliation's organization only when the employer has been DEACTIVATED
  // (the other condition drops the whole row), so a reader who knows the
  // person could read the deactivation straight out of the placeholder.

  // The named case is the control: whatever is withheld must not make the
  // function stop returning names, or "null for null" would be satisfied by a
  // function that always returns null.
  assert.deepEqual(printableEntity({ name: "Open Lab", url: "/projects/p1" }), {
    url: "/projects/p1",
    label: "Open Lab",
  });
  assert.deepEqual(printableEntity({ name: "Institute", url: "/organizations/institute" }), {
    url: "/organizations/institute",
    label: "Institute",
  });

  // The withheld case: the answer is "print no entity at all".
  assert.equal(printableEntity(null), null);
  // An omitted field is the same fact as null (the parser normalizes the two).
  assert.equal(printableEntity(undefined), null);
  // A name-less identity is no identity: it must not become a blank label
  // standing where a name belongs.
  assert.equal(printableEntity({ name: "", url: "" }), null);

  // And the row's own facts are not this function's business — it says
  // nothing about whether the row survives, which is the point: the caller
  // keeps role, dates and verification (see the affiliation test above).
});

// ---------------------------------------------------------------------------
// Formatting helpers (the API sends facts, the client writes the sentence)

test("formatAffiliationWindow renders all four window shapes", () => {
  assert.equal(formatAffiliationWindow("2024-01-15", "2025-06-30"), "2024-01-15 – 2025-06-30");
  assert.equal(formatAffiliationWindow("2024-01-15", null), "2024-01-15 – present");
  assert.equal(formatAffiliationWindow(null, "2025-06-30"), "until 2025-06-30");
  assert.equal(formatAffiliationWindow(null, null), "dates not recorded");
});

test("formatDay keeps the calendar day the API sent", () => {
  assert.equal(formatDay("2026-01-12T10:00:00Z"), "2026-01-12");
  assert.equal(formatDay("not a date"), "not a date");
});

test("via and relation vocabularies render as words, unknown values verbatim", () => {
  assert.equal(describeVia("claude_code"), "agent");
  assert.equal(describeVia("git_compat"), "Git");
  assert.equal(describeVia("carrier_pigeon"), "carrier_pigeon");
  assert.equal(describeRelation("reproduces"), "Reproduced");
  assert.equal(describeRelation("fails_to_reproduce"), "Failed to reproduce");
  assert.equal(describeRelation("supports"), "supports");
});

test("every stable code has a human line", () => {
  for (const code of ["USER_NOT_FOUND", "ORG_NOT_FOUND", "SERVICE_UNAVAILABLE", "UNEXPECTED"]) {
    assert.notEqual(messageForResearchProfileCode(code), messageForResearchProfileCode("SOMETHING_ELSE"));
  }
  assert.match(messageForResearchProfileCode("USER_NOT_FOUND"), /no public research profile/);
  assert.match(messageForResearchProfileCode("ORG_NOT_FOUND"), /no public profile/);
});
