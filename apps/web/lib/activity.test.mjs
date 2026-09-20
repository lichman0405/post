/**
 * Tests for the client Activity module (apps/web/lib/activity.ts).
 *
 * Plain-ESM test file like profile.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (activity.ts is import-free by
 * construction); the whole chain is typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * This test imports TWO lib modules: activity.ts, and inbox.ts for the
 * event-type registry activityTitle takes as an argument. That is the point
 * of the argument — the audit vocabulary (internal/domain/audit.go) and the
 * event vocabulary (specs/events/event-types.yaml) are separate lists, so
 * the test is where their labels are checked against each other.
 *
 * T0607 added the source filter and the rendering rules, so the negative
 * cases here are the point: an unknown filter, an unknown source on the
 * wire, and an action/via/target the maps do not know must each be visible
 * as themselves rather than silently rendered as something else.
 *
 * Run: node --test "apps/web/lib/activity.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ACTIVITY_SOURCES,
  ApiError,
  activityActorHref,
  activityActorName,
  activityDetail,
  activityFamily,
  activityFilterLabel,
  activitySourceFromParam,
  activityTarget,
  activityTimestamp,
  activityTitle,
  activityVisibility,
  createActivityClient,
  messageForAuditCode,
  shortHash,
  viaLabel,
} from "./activity.ts";
import { eventTypeDisplayName } from "./inbox.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({ method, path, credentials: init.credentials });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => ({ status: 200, json: async () => payload });

/** One governance row (the audit_log half of the feed). */
const ENTRY = {
  id: "0a0b0c0d-0000-4000-8000-000000000001",
  source: "governance",
  actor_id: "02040608-0a0c-4e10-9214-16181a1c1e20",
  actor_handle: "alice",
  actor_display_name: "Alice Researcher",
  via: "session",
  action: "org.created",
  target_ref: "organization:0a0b0c0d-1111-4000-8000-000000000002",
  project_id: null,
  organization_id: "0a0b0c0d-1111-4000-8000-000000000002",
  correlation_id: "corr-123",
  after_summary: { slug: "acme-labs", name: "Acme Research" },
  occurred_at: "2026-09-12T10:00:00Z",
};

/** One research row (the research_events half): no target_ref, no
 *  summaries, a payload and a visibility instead. */
const RESEARCH_ENTRY = {
  id: "0a0b0c0d-0000-4000-8000-0000000000aa",
  source: "research",
  actor_id: "02040608-0a0c-4e10-9214-16181a1c1e20",
  actor_handle: "alice",
  actor_display_name: "Alice Researcher",
  via: "claude_code",
  action: "scientific_object.aborted",
  target_ref: null,
  project_id: "0a0b0c0d-2222-4000-8000-000000000003",
  organization_id: null,
  correlation_id: "corr-456",
  payload: {
    object_id: "0a0b0c0d-3333-4000-8000-000000000004",
    object_type: "structure",
    version_no: 4,
    aborted_version_no: 3,
    reason_code: "contaminated",
  },
  visibility: "project",
  occurred_at: "2026-09-12T11:00:00Z",
};

/** One governance row for the same abort: the audit half, which carries the
 *  free-text explanation the event deliberately does not. */
const ABORT_AUDIT_ENTRY = {
  ...ENTRY,
  id: "0a0b0c0d-0000-4000-8000-0000000000bb",
  action: "scientific_object.aborted",
  target_ref: "object:0a0b0c0d-3333-4000-8000-000000000004",
  before_summary: { lifecycle_state: "active" },
  after_summary: {
    lifecycle_state: "aborted",
    reason_code: "contaminated",
    explanation: "the sample was contaminated during transfer",
    version_no: 4,
  },
};

/* ------------------------------------------------------------------ wire */

test("projectActivity reads the feed with the session cookie, no CSRF header", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ok({ entries: [ENTRY], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.projectActivity("p1");
  assert.equal(page.entries.length, 1);
  assert.equal(page.entries[0].action, "org.created");
  assert.equal(page.entries[0].source, "governance");
  assert.equal(page.entries[0].after_summary.slug, "acme-labs");
  assert.equal(page.next_cursor, null);
  assert.equal(calls[0].method, "GET");
  assert.equal(calls[0].credentials, "include");
});

test("orgActivity renders the organization feed path", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/organizations/o1/activity": () =>
      ok({ entries: [{ ...ENTRY, action: "org.member.invited" }], next_cursor: "tok" }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.orgActivity("o1");
  assert.equal(page.entries[0].action, "org.member.invited");
  assert.equal(page.next_cursor, "tok");
  assert.equal(calls[0].path, "/api/v1/organizations/o1/activity");
});

test("limit and cursor render as query parameters, cursor URL-encoded", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor": () =>
      ok({ entries: [], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  const page = await client.projectActivity("p1", { limit: 3, cursor: "cur+sor" });
  assert.deepEqual(page.entries, []);
  assert.equal(calls[0].path, "/api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor");
});

test("projectActivity renders ?source=, and no source means no parameter", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity?source=governance": () =>
      ok({ entries: [ENTRY], next_cursor: null }),
    "GET /api/v1/projects/p1/activity?source=research": () =>
      ok({ entries: [RESEARCH_ENTRY], next_cursor: null }),
    "GET /api/v1/projects/p1/activity": () =>
      ok({ entries: [ENTRY, RESEARCH_ENTRY], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });

  const gov = await client.projectActivity("p1", { source: "governance" });
  assert.equal(gov.entries[0].action, "org.created");
  const res = await client.projectActivity("p1", { source: "research" });
  assert.equal(res.entries[0].action, "scientific_object.aborted");
  const both = await client.projectActivity("p1");
  assert.equal(both.entries.length, 2);

  // The three requests are three different URLs: the unfiltered read must
  // NOT send source=all (the API has no such value) and must not send an
  // empty source= (which would read as the absence of a filter anyway, but
  // by accident rather than by contract).
  assert.deepEqual(calls.map((c) => c.path), [
    "/api/v1/projects/p1/activity?source=governance",
    "/api/v1/projects/p1/activity?source=research",
    "/api/v1/projects/p1/activity",
  ]);
});

test("source composes with limit and cursor in one query string", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor&source=research": () =>
      ok({ entries: [], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await client.projectActivity("p1", { limit: 3, cursor: "cur+sor", source: "research" });
  assert.equal(calls[0].path, "/api/v1/projects/p1/activity?limit=3&cursor=cur%2Bsor&source=research");
});

test("orgActivity never sends a source, even when the caller passes one", async () => {
  // The organization feed carries governance records only (the API refuses
  // source=research there), so the client drops the parameter rather than
  // forwarding a filter that can hold one value.
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/organizations/o1/activity": () => ok({ entries: [ENTRY], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await client.orgActivity("o1", { source: "research" });
  assert.equal(calls[0].path, "/api/v1/organizations/o1/activity");
});

test("an entry without a source is refused, not rendered as governance", async () => {
  // Every rendering rule branches on source (payload vs. summaries, the
  // visibility line), so a row that lost the field must not be rendered by
  // whichever branch the code happened to fall into.
  const { source, ...noSource } = ENTRY;
  assert.equal(source, "governance");
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ok({ entries: [noSource], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    /unexpected activity entry shape/,
  );
});

test("an unknown source value is refused too", async () => {
  for (const bogus of ["audit", "events", "all", "Governance", ""]) {
    const { fetchFn } = fakeFetch({
      "GET /api/v1/projects/p1/activity": () =>
        ok({ entries: [{ ...ENTRY, source: bogus }], next_cursor: null }),
    });
    const client = createActivityClient(API, { fetch: fetchFn });
    await assert.rejects(
      () => client.projectActivity("p1"),
      /unexpected activity entry shape/,
      `source ${JSON.stringify(bogus)} must not be accepted`,
    );
  }
});

test("a denied feed surfaces the stable envelope as ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/organizations/o1/activity": () => ({
      status: 404,
      json: async () => ({ code: "ORG_NOT_FOUND", message: "organization not found" }),
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.orgActivity("o1"),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "ORG_NOT_FOUND");
      assert.equal(err.status, 404);
      assert.equal(err.retryable, false);
      return true;
    },
  );
});

test("an out-of-contract source surfaces as the API's stable error", async () => {
  // The page takes its filter from the URL, so a crafted ?source=audit can
  // reach the client at runtime even though the type forbids it. The API
  // answers 400 rather than an empty page, and the client must surface that
  // as the stable code — an empty page would read as "this project has no
  // such history".
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/projects/p1/activity?source=audit": () => ({
      status: 400,
      json: async () => ({ code: "VALIDATION_FAILED", message: "source must be governance or research (or absent for both)" }),
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1", { source: "audit" }),
    (err) => err instanceof ApiError && err.code === "VALIDATION_FAILED" && err.status === 400,
  );
  assert.equal(calls[0].path, "/api/v1/projects/p1/activity?source=audit");
});

test("a non-envelope failure still throws ApiError with a generic code", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ({
      status: 500,
      json: async () => {
        throw new Error("not json");
      },
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    (err) => err instanceof ApiError && err.code === "UNKNOWN",
  );
});

test("a 3xx answer surfaces as a stable ApiError, never a shape error", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () => ({
      status: 302,
      json: async () => {
        throw new Error("no body on a redirect");
      },
    }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    (err) => err instanceof ApiError && err.code === "UNKNOWN" && err.status === 302,
  );
});

test("a malformed entry shape is rejected, not silently trusted", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/projects/p1/activity": () =>
      ok({ entries: [{ id: "x" }], next_cursor: null }),
  });
  const client = createActivityClient(API, { fetch: fetchFn });
  await assert.rejects(
    () => client.projectActivity("p1"),
    /unexpected activity entry shape/,
  );
});

/* ------------------------------------------------------- presentation */

test("activityFilterLabel names the chips, and null is the unfiltered one", () => {
  assert.deepEqual(ACTIVITY_SOURCES, ["governance", "research"]);
  assert.equal(activityFilterLabel(null), "All");
  assert.equal(activityFilterLabel("governance"), "Governance");
  assert.equal(activityFilterLabel("research"), "Research events");
  // The labels are distinct: two chips reading the same would be one filter
  // with two buttons.
  assert.equal(new Set(ACTIVITY_SOURCES.map(activityFilterLabel)).size, ACTIVITY_SOURCES.length);
  assert.ok(!ACTIVITY_SOURCES.map(activityFilterLabel).includes(activityFilterLabel(null)));
});

test("activityTitle names a row from its OWN registry, never the other one", () => {
  // A governance row: this module's audit vocabulary.
  assert.equal(activityTitle(ENTRY, eventTypeDisplayName), "Organization created");
  assert.equal(
    activityTitle({ ...ENTRY, action: "project.schema_profile_registered" }, eventTypeDisplayName),
    "Schema profile registered",
  );
  // A research row: a research row's action IS an event type, so its name
  // comes from the event registry the caller passes in.
  assert.equal(activityTitle(RESEARCH_ENTRY, eventTypeDisplayName), "Object aborted");
  assert.equal(
    activityTitle({ ...RESEARCH_ENTRY, action: "state.committed" }, eventTypeDisplayName),
    "Commit",
  );
  // The two are NOT interchangeable: a governance row the audit vocabulary
  // does not know must not borrow the event registry's name for the same
  // dotted string — internal/domain/audit.go: "neither list is derived from
  // the other".
  assert.equal(
    activityTitle(
      { ...ENTRY, action: "something.new_thing" },
      (t) => `EVENT:${t}`,
    ),
    "something.new_thing",
  );
  // …and a research row never reads the audit table, even for a name that is
  // in it: the injected registry is the only one consulted.
  assert.equal(
    activityTitle({ ...RESEARCH_ENTRY, action: "org.created" }, (t) => `EVENT:${t}`),
    "EVENT:org.created",
  );
});

test("names the two vocabularies spell alike carry ONE label", () => {
  // These five names are audit actions AND event types — audit.go calls
  // that "a coincidence of two separate registries, not a shared identity".
  // The registries are not derived from each other, so nothing but this
  // test keeps their labels equal; the page renders both halves of one
  // timeline, and two labels for one act would be the feed disagreeing with
  // itself.
  for (const name of [
    "project.created",
    "project.main_frozen",
    "pull_request.merged",
    "scientific_object.aborted",
    "knowledge.version_published",
  ]) {
    const governance = activityTitle({ ...ENTRY, action: name }, eventTypeDisplayName);
    const research = activityTitle({ ...RESEARCH_ENTRY, action: name }, eventTypeDisplayName);
    assert.equal(governance, research, `${name}: the two registries disagree`);
    // Neither may be the raw name: that would mean one registry lost the
    // entry and the row rendered by accident.
    assert.notEqual(governance, name, `${name} rendered raw`);
  }
});

test("an action with no label renders its raw dotted name on either source", () => {
  assert.equal(
    activityTitle({ ...ENTRY, action: "something.new_thing" }, eventTypeDisplayName),
    "something.new_thing",
  );
  assert.equal(
    activityTitle({ ...RESEARCH_ENTRY, action: "something.new_thing" }, eventTypeDisplayName),
    "something.new_thing",
  );
  // A reopen GOVERNANCE row renders raw today: only the event exists (T0610
  // writes the audit action), and guessing its name would put a word in the
  // vocabulary that internal/domain/audit.go does not have.
  assert.equal(
    activityTitle({ ...ENTRY, action: "scientific_object.reopened" }, eventTypeDisplayName),
    "scientific_object.reopened",
  );
  // …while the reopen EVENT, which does exist, is named.
  assert.equal(
    activityTitle({ ...RESEARCH_ENTRY, action: "scientific_object.reopened" }, eventTypeDisplayName),
    "Object reopened",
  );
});

test("activityFamily names only the families with rendering of their own", () => {
  assert.equal(activityFamily({ ...ENTRY, action: "scientific_object.aborted" }), "abort");
  assert.equal(activityFamily({ ...ENTRY, action: "scientific_object.reopened" }), "reopen");
  assert.equal(activityFamily({ ...ENTRY, action: "release.created" }), "release");
  assert.equal(activityFamily({ ...ENTRY, action: "release.published" }), "release");
  assert.equal(activityFamily(ENTRY), "other");
  // branch.aborted is a discarded proposal branch, not a retracted research
  // object: it must not render as an abort.
  assert.equal(activityFamily({ ...ENTRY, action: "branch.aborted" }), "other");
});

test("actor name, and the link only where there is an actor to link", () => {
  assert.equal(activityActorName(ENTRY), "Alice Researcher");
  assert.equal(activityActorName({ ...ENTRY, actor_display_name: null }), "alice");
  assert.equal(
    activityActorName({ ...ENTRY, actor_display_name: null, actor_handle: null }),
    "Unknown actor",
  );
  assert.equal(
    activityActorHref(ENTRY),
    "/users/02040608-0a0c-4e10-9214-16181a1c1e20",
  );
  // A row ABOUT nobody (an unauthenticated sign-in attempt) links nowhere:
  // pointing it at the reader's own profile would be a lie about who acted.
  assert.equal(activityActorHref({ ...ENTRY, actor_id: null }), null);
  // An id with a slash must not escape its path segment.
  assert.equal(activityActorHref({ ...ENTRY, actor_id: "a/b" }), "/users/a%2Fb");
});

test("viaLabel covers both vocabularies and renders unknown values raw", () => {
  // Audit rows record how the action reached the API.
  assert.equal(viaLabel("session"), "Web session");
  assert.equal(viaLabel("password"), "Password");
  assert.equal(viaLabel("oidc"), "SSO");
  // Research rows record the write path that produced them.
  assert.equal(viaLabel("web"), "Web UI");
  assert.equal(viaLabel("mcp"), "MCP");
  assert.equal(viaLabel("claude_code"), "Claude Code");
  // Unknown values render raw rather than blank.
  assert.equal(viaLabel("carrier_pigeon"), "carrier_pigeon");
  assert.equal(viaLabel(""), "");
});

test("release rows show the version and the short manifest hash", () => {
  const entry = {
    ...ENTRY,
    action: "release.created",
    target_ref: "release:0a0b0c0d-4444-4000-8000-000000000005",
    after_summary: {
      version: "v1.2.0",
      state_id: "0a0b0c0d-5555-4000-8000-000000000006",
      manifest_hash: "9f2c1ab34d5e6f708192a3b4c5d6e7f80912a3b4c5d6e7f80912a3b4c5d6e7f8",
    },
  };
  assert.deepEqual(activityDetail(entry), [
    { label: "Version", value: "v1.2.0" },
    { label: "Manifest", value: "9f2c1ab34d5e" },
  ]);
  assert.equal(shortHash("abc"), "abc");
  assert.equal(shortHash(""), "");
});

test("a research release row reads its payload, not its summaries", () => {
  // The event body is the payload; a research row has no after_summary, and
  // reading the wrong half must not silently produce "no version".
  const entry = {
    ...RESEARCH_ENTRY,
    action: "release.published",
    payload: { release_id: "0a0b0c0d-4444-4000-8000-000000000005", version: "v2.0.0", manifest_hash: "abcdef0123456789" },
  };
  assert.deepEqual(activityDetail(entry), [
    { label: "Version", value: "v2.0.0" },
    { label: "Manifest", value: "abcdef012345" },
  ]);
  // …and a governance row never reads a payload, even if one is present.
  const sneaky = { ...entry, source: "governance", after_summary: { version: "v3.0.0", manifest_hash: "ffffffffffffffff" } };
  assert.deepEqual(activityDetail(sneaky), [
    { label: "Version", value: "v3.0.0" },
    { label: "Manifest", value: "ffffffffffff" },
  ]);
});

test("abort rows show the reason, and only the governance half has the explanation", () => {
  // The governance (audit) row: the explanation is here.
  const gov = activityDetail(ABORT_AUDIT_ENTRY);
  assert.deepEqual(gov, [
    { label: "Reason code", value: "contaminated" },
    { label: "Version", value: "4" },
    { label: "Explanation", value: "the sample was contaminated during transfer" },
    { label: "State", value: "active → aborted" },
  ]);
  // The research (event) row of the SAME abort: an empty "Explanation" line
  // would claim the event says there is none, which is a different claim
  // from "the event does not carry the free text" — so the line is absent.
  const res = activityDetail(RESEARCH_ENTRY);
  assert.deepEqual(res, [
    { label: "Reason code", value: "contaminated" },
    { label: "Version", value: "4" },
  ]);
  assert.ok(!res.some((d) => d.label === "Explanation"));
  // A research row has no before/after summaries, so no State line.
  assert.ok(!res.some((d) => d.label === "State"));
});

test("reopen rows render the same family, and a state line needs both sides", () => {
  const reopened = { ...ABORT_AUDIT_ENTRY, action: "scientific_object.reopened", after_summary: { lifecycle_state: "active", reason_code: "reopened", explanation: "wrong sample" } };
  // The row inherits before_summary {lifecycle_state: "active"}, and its
  // after_summary says active too — a no-op transition is not a move, so no
  // State line. There is no Version line either: this fixture's summary
  // carries no version_no.
  assert.deepEqual(activityDetail(reopened), [
    { label: "Reason code", value: "reopened" },
    { label: "Explanation", value: "wrong sample" },
  ]);
  // One side only (the row records where it went, not where it came from)
  // is no transition, and a fabricated "? → active" would be a claim about
  // the before-state that the row does not make.
  assert.ok(!activityDetail({ ...reopened, before_summary: {} }).some((d) => d.label === "State"));
  assert.ok(!activityDetail({ ...reopened, before_summary: undefined }).some((d) => d.label === "State"));
  // With a real move recorded on both sides, the line is rendered.
  assert.ok(
    activityDetail({
      ...reopened,
      before_summary: { lifecycle_state: "aborted" },
      after_summary: { lifecycle_state: "active" },
    }).some((d) => d.label === "State" && d.value === "aborted → active"),
  );
});

test("a malformed payload renders nothing rather than a broken fact", () => {
  // A payload that is not an object, an array, or a field of the wrong type
  // must not appear as "[object Object]" or "NaN".
  for (const payload of [null, "text", 42, [1, 2, 3], { version_no: { nested: true } }, { version_no: Number.NaN }]) {
    const detail = activityDetail({ ...RESEARCH_ENTRY, payload });
    assert.deepEqual(detail, [], `payload ${JSON.stringify(payload)} must render nothing`);
  }
  // "other" rows render no facts at all: their summaries are unlabelled, and
  // a page printing the raw jsonb would be showing the wire, not history.
  assert.deepEqual(activityDetail({ ...ENTRY, after_summary: { slug: "x" } }), []);
});

test("activityTarget links the refs that have a page and only those", () => {
  const project = "0a0b0c0d-2222-4000-8000-000000000003";
  const release = activityTarget(project, {
    ...ENTRY,
    target_ref: "release:0a0b0c0d-4444-4000-8000-000000000005",
  });
  assert.deepEqual(release, {
    label: "Release 0a0b0c0d",
    href: `/projects/${project}/releases/0a0b0c0d-4444-4000-8000-000000000005`,
  });
  assert.deepEqual(activityTarget(project, { ...ENTRY, target_ref: "user:02040608-0a0c-4e10" }), {
    label: "User 02040608",
    href: "/users/02040608-0a0c-4e10",
  });
  // An abort's target is an object, and this app has no object page: the ref
  // renders as text, never as a link to a 404.
  assert.deepEqual(activityTarget(project, ABORT_AUDIT_ENTRY), {
    label: "Object 0a0b0c0d",
    href: null,
  });
  assert.deepEqual(activityTarget(project, ENTRY), {
    label: "Organization 0a0b0c0d",
    href: null,
  });
  // A research row names no target at all — and its payload is not mined for
  // one, so a row whose payload holds an object_id still shows none.
  assert.equal(activityTarget(project, RESEARCH_ENTRY), null);
  assert.equal(activityTarget(project, { ...RESEARCH_ENTRY, target_ref: null }), null);
  // Unknown/odd refs render as themselves rather than being dropped.
  assert.deepEqual(activityTarget(project, { ...ENTRY, target_ref: "someday:xyz" }), {
    label: "someday:xyz",
    href: null,
  });
  assert.deepEqual(activityTarget(project, { ...ENTRY, target_ref: "release:" }), {
    label: "release",
    href: null,
  });
  assert.deepEqual(activityTarget(project, { ...ENTRY, target_ref: "nocolon" }), {
    label: "nocolon",
    href: null,
  });
});

test("activityVisibility is a research-row fact only", () => {
  assert.equal(activityVisibility(RESEARCH_ENTRY), "project");
  assert.equal(activityVisibility({ ...RESEARCH_ENTRY, visibility: "" }), null);
  assert.equal(activityVisibility({ ...RESEARCH_ENTRY, visibility: undefined }), null);
  // A governance row has no visibility of its own: who may read it is the
  // scope's gate, and that is the same gate for both sources.
  assert.equal(activityVisibility({ ...ENTRY, visibility: "project" }), null);
});

test("activityTimestamp renders the instant in UTC, labelled", () => {
  assert.equal(activityTimestamp("2026-09-12T10:00:00Z"), "2026-09-12 10:00 UTC");
  assert.equal(activityTimestamp("2026-09-12T10:00:45.123456Z"), "2026-09-12 10:00 UTC");
  // A different zone is the same instant: the row must not be shifted into
  // the reader's zone, or the server and the browser would disagree.
  assert.equal(activityTimestamp("2026-09-12T12:00:00+02:00"), "2026-09-12 10:00 UTC");
  // Midnight and single-digit fields are padded, so the column lines up.
  assert.equal(activityTimestamp("2026-01-02T03:04:05Z"), "2026-01-02 03:04 UTC");
  // An unparseable value renders as it arrived: "" would claim the row has
  // no instant, and a made-up date would be a fact the row does not carry.
  assert.equal(activityTimestamp("not-a-time"), "not-a-time");
  assert.equal(activityTimestamp(""), "");
});

test("activitySourceFromParam reads the URL filter, and refuses another value", () => {
  assert.equal(activitySourceFromParam(null), null);
  assert.equal(activitySourceFromParam(""), null);
  assert.equal(activitySourceFromParam("governance"), "governance");
  assert.equal(activitySourceFromParam("research"), "research");
  // Not "no filter": the API refuses a filter it does not have rather than
  // answering a subset, so the page refuses to send one rather than reading
  // everything a mistyped URL asked it not to read.
  for (const bad of ["all", "audit", "events", "Governance", "research "]) {
    assert.throws(
      () => activitySourceFromParam(bad),
      /unknown activity source/,
      `${JSON.stringify(bad)} must be refused`,
    );
  }
});

test("messageForAuditCode maps the stable codes to human text", () => {
  assert.equal(messageForAuditCode("PROJECT_NOT_FOUND"), "This project is not visible to you.");
  assert.equal(messageForAuditCode("ORG_NOT_FOUND"), "This organization is not visible to you.");
  assert.equal(messageForAuditCode("AUTH_UNAUTHENTICATED"), "Sign in to view this activity.");
  assert.equal(messageForAuditCode("METHOD_NOT_ALLOWED"), "The audit log is read-only.");
  assert.equal(messageForAuditCode("SERVICE_UNAVAILABLE"), "Activity is temporarily unavailable. Please try again later.");
  assert.equal(messageForAuditCode("NOPE"), "Something went wrong. Please try again.");
});
