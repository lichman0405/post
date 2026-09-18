/**
 * Tests for the client inbox module (apps/web/lib/inbox.ts).
 *
 * Plain-ESM test file like milestones.test.mjs: Node ≥ 23.6 runs the
 * imported .ts chain through type stripping (inbox.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/inbox.test.mjs"
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  aggregationLabel,
  createInboxClient,
  entryTargetLabel,
  eventTypeDisplayName,
  messageForInboxCode,
  windowLabel,
} from "./inbox.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const method = init.method ?? "GET";
    const path = String(input).slice(API.length);
    calls.push({
      method,
      path,
      credentials: init.credentials,
      headers: init.headers ?? {},
      body: init.body ?? null,
    });
    const handler = routes[`${method} ${path}`];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

const ok = (payload) => () => ({ status: 200, json: async () => payload });

const ENTRY = {
  target_type: "project",
  target_id: "99999999-8888-4777-8666-555555555555",
  target_label: "MOF Screening",
  event_type: "state.committed",
  window_start: "2026-09-16T10:00:00Z",
  count: 40,
  unread: 40,
  read: false,
  first_at: "2026-09-16T10:00:01Z",
  last_at: "2026-09-16T10:00:40Z",
  latest_event_id: "11111111-2222-4333-8444-555555555555",
  latest_delivery_id: "77777777-6666-4555-8444-555555555555",
  url: "/projects/99999999-8888-4777-8666-555555555555/activity",
};

test("list decodes the page and carries the session cookie", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/inbox?filter=unread": ok({ entries: [ENTRY], unread_count: 3, filter: "unread" }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  const page = await client.list();

  assert.equal(page.entries.length, 1);
  assert.equal(page.entries[0].count, 40);
  assert.equal(page.entries[0].latest_delivery_id, ENTRY.latest_delivery_id);
  assert.equal(page.unreadCount, 3);
  assert.equal(page.filter, "unread");
  assert.equal(calls.length, 1);
  assert.equal(calls[0].method, "GET");
  // The inbox is one person's: the cookie must ride along, or the API
  // answers 401 and the page renders "sign in" for a signed-in user.
  assert.equal(calls[0].credentials, "include");
});

test("list passes the view and the page size through", async () => {
  const { fetchFn, calls } = fakeFetch({
    "GET /api/v1/inbox?filter=all&limit=10": ok({ entries: [], unread_count: 0, filter: "all" }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await client.list("all", 10);

  assert.equal(calls[0].path, "/api/v1/inbox?filter=all&limit=10");
});

test("list surfaces the API envelope as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/inbox?filter=unread": () => ({
      status: 401,
      json: async () => ({ code: "AUTH_UNAUTHENTICATED", message: "authentication required" }),
    }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.list(),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "AUTH_UNAUTHENTICATED");
      assert.equal(err.status, 401);
      assert.equal(err.retryable, false);
      return true;
    },
  );
});

test("list refuses a page whose entries are not entries", async () => {
  const { fetchFn } = fakeFetch({
    // The envelope is right but one row is missing its counters: rendering
    // it would show an entry with no count, which is the one thing the
    // aggregation is for.
    "GET /api/v1/inbox?filter=unread": ok({
      entries: [{ target_type: "project", target_id: "p" }],
      unread_count: 1,
      filter: "unread",
    }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(() => client.list(), /unexpected inbox entry shape/);
});

test("list refuses a body that is not a page", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/inbox?filter=unread": ok({ entries: [] }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(() => client.list(), /unexpected inbox response shape/);
});

test("markRead posts the anchors with the CSRF token and returns the count", async () => {
  const { fetchFn, calls } = fakeFetch({
    "POST /api/v1/inbox/read": ok({ read: 40 }),
  });
  const client = createInboxClient(API, { fetch: fetchFn, csrfToken: () => "tok-123" });

  const marked = await client.markRead([ENTRY.latest_delivery_id]);

  assert.equal(marked, 40);
  assert.equal(calls[0].method, "POST");
  assert.equal(calls[0].path, "/api/v1/inbox/read");
  assert.equal(calls[0].headers["X-CSRF-Token"], "tok-123");
  assert.equal(calls[0].credentials, "include");
  assert.deepEqual(JSON.parse(calls[0].body), { deliveries: [ENTRY.latest_delivery_id] });
});

test("markRead with an empty selection calls nothing", async () => {
  const { fetchFn, calls } = fakeFetch({});
  const client = createInboxClient(API, { fetch: fetchFn });

  assert.equal(await client.markRead([]), 0);
  assert.equal(calls.length, 0);
});

test("markRead surfaces a stale anchor as an ApiError", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/inbox/read": () => ({
      status: 404,
      json: async () => ({ code: "INBOX_ENTRY_NOT_FOUND", message: "inbox entry not found" }),
    }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.markRead([ENTRY.latest_delivery_id]),
    (err) => err instanceof ApiError && err.code === "INBOX_ENTRY_NOT_FOUND" && err.status === 404,
  );
});

test("markAllRead posts and reports the count", async () => {
  const { fetchFn, calls } = fakeFetch({
    "POST /api/v1/inbox/read-all": ok({ read: 5 }),
  });
  const client = createInboxClient(API, { fetch: fetchFn, csrfToken: () => "tok-123" });

  assert.equal(await client.markAllRead(), 5);
  assert.equal(calls[0].path, "/api/v1/inbox/read-all");
  assert.equal(calls[0].body, null);
  assert.equal(calls[0].headers["X-CSRF-Token"], "tok-123");
});

test("a failed mark is retryable when the API says so", async () => {
  const { fetchFn } = fakeFetch({
    "POST /api/v1/inbox/read-all": () => ({
      status: 503,
      json: async () => ({
        code: "SERVICE_UNAVAILABLE",
        message: "inbox data is temporarily unavailable",
        retryable: true,
      }),
    }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.markAllRead(),
    (err) => err instanceof ApiError && err.retryable === true && err.status === 503,
  );
});

test("a non-JSON failure is an ApiError, not a crash", async () => {
  const { fetchFn } = fakeFetch({
    "GET /api/v1/inbox?filter=unread": () => ({
      status: 502,
      json: async () => {
        throw new Error("Unexpected token < in JSON");
      },
    }),
  });
  const client = createInboxClient(API, { fetch: fetchFn });

  await assert.rejects(
    () => client.list(),
    (err) => err instanceof ApiError && err.code === "UNKNOWN" && err.status === 502,
  );
});

test("aggregationLabel speaks only when an entry stands for several", () => {
  assert.equal(aggregationLabel(40), "40 events");
  assert.equal(aggregationLabel(2), "2 events");
  // One notification is one row: a "1 event" badge on every line is noise.
  assert.equal(aggregationLabel(1), null);
  assert.equal(aggregationLabel(0), null);
  assert.equal(aggregationLabel(Number.NaN), null);
});

test("eventTypeDisplayName names canonical types and echoes the rest", () => {
  assert.equal(eventTypeDisplayName("state.committed"), "Commit");
  assert.equal(eventTypeDisplayName("release.published"), "Release published");
  // An event type this build does not know renders raw: the page must not
  // invent a name for something it does not understand.
  assert.equal(eventTypeDisplayName("future.thing_happened"), "future.thing_happened");
  assert.equal(eventTypeDisplayName(""), "");
});

test("entryTargetLabel falls back to the kind and a short id", () => {
  assert.equal(entryTargetLabel(ENTRY), "MOF Screening");
  // "" is the API saying it has no naming query for that target KIND, not
  // that the caller may not read the target (those entries are not served
  // at all). The row names the kind and a short id rather than rendering
  // an empty link — and never an "access denied" state.
  assert.equal(
    entryTargetLabel({ ...ENTRY, target_label: "" }),
    `project ${ENTRY.target_id.slice(0, 8)}`,
  );
  // A short id is not truncated into ambiguity.
  assert.equal(entryTargetLabel({ ...ENTRY, target_label: "", target_id: "p-1" }), "project p-1");
});

test("windowLabel renders the clock hour the API binned into", () => {
  assert.equal(windowLabel("2026-09-16T10:00:00Z"), "2026-09-16 10:00–11:00 UTC");
  // The window is UTC by construction; the label says so rather than
  // shifting it into the viewer's timezone (a shifted label would name an
  // hour the deliveries are not in).
  assert.equal(windowLabel("2026-09-16T10:30:00Z"), "2026-09-16 10:00–11:00 UTC");
  // Crossing midnight keeps the end hour right.
  assert.equal(windowLabel("2026-09-16T23:00:00Z"), "2026-09-16 23:00–00:00 UTC");
  assert.equal(windowLabel("not a timestamp"), "");
});

test("messageForInboxCode answers every code the surface can show", () => {
  for (const code of [
    "AUTH_UNAUTHENTICATED",
    "INBOX_ENTRY_NOT_FOUND",
    "VALIDATION_FAILED",
    "SERVICE_UNAVAILABLE",
  ]) {
    const message = messageForInboxCode(code);
    assert.equal(typeof message, "string");
    assert.notEqual(message, "");
    // A code the page knows never falls through to the generic line: the
    // point of mapping codes is that the user reads what happened.
    assert.notEqual(message, messageForInboxCode("SOMETHING_ELSE"));
  }
  assert.equal(
    messageForInboxCode("SOMETHING_ELSE"),
    "Something went wrong while reading the inbox. Please try again.",
  );
});
