/**
 * Tests for the Explore index client (apps/web/lib/explore.ts).
 *
 * Plain-ESM test file like assets.test.mjs: Node ≥ 23.6 runs the imported
 * .ts chain through type stripping (explore.ts is import-free by
 * construction); the module is fully typechecked by
 * `pnpm --filter @post/web typecheck`.
 *
 * Run: node --test "apps/web/lib/explore.test.mjs"
 *
 * What is worth testing here is not that a URL is a string. It is the three
 * things this module could get wrong in a way nobody would notice:
 *
 *   - the SHAPE guard, which decides whether a malformed or truncated body
 *     is rendered as an index (a body with no `people` section is not a
 *     network with no people);
 *   - the section lookup, which decides which rows render under which tab —
 *     a lookup that fell back to another section would print one dimension's
 *     rows under another's heading;
 *   - the refusal, which must never turn a failed read into a short list
 *     (docs/51: empty and error are different states).
 */
import assert from "node:assert/strict";
import { test } from "node:test";

import {
  ApiError,
  EXPLORE_TABS,
  createExploreClient,
  isIndexShape,
  messageForExploreCode,
  publishedOn,
  sectionItems,
  tabId,
  tabLabel,
  tabPanelId,
} from "./explore.ts";

const API = "http://127.0.0.1:8080";

/** A scripted fake fetch: routes is a map "METHOD path" -> handler. */
function fakeFetch(routes) {
  const calls = [];
  const fetchFn = async (input, init = {}) => {
    const path = String(input).slice(API.length);
    calls.push({ path, init });
    const handler = routes[path];
    if (!handler) {
      return { status: 404, json: async () => ({ code: "NOT_FOUND", message: "not found" }) };
    }
    return handler();
  };
  return { fetchFn, calls };
}

/** An index body with every section present and empty. */
function emptyIndexBody() {
  const body = {};
  for (const tab of EXPLORE_TABS) body[tab] = { tab, items: [] };
  return body;
}

/** An index body with one row in the named section. */
function indexBodyWith(tab, item) {
  const body = emptyIndexBody();
  body[tab] = { tab, items: [item] };
  return body;
}

test("index reads the aggregate route and returns the body", async () => {
  const body = indexBodyWith("projects", {
    id: "p1",
    slug: "open-materials-lab",
    name: "Open Materials Lab",
    purpose: "MOF synthesis screening",
    activity_status: "active",
    url: "/projects/p1",
    created_at: "2026-09-01T10:00:00Z",
  });
  const { fetchFn, calls } = fakeFetch({
    "/api/v1/explore": () => ({ status: 200, json: async () => body }),
  });

  const index = await createExploreClient(API, { fetch: fetchFn }).index();

  assert.equal(calls.length, 1);
  assert.equal(calls[0].path, "/api/v1/explore");
  // The session cookie travels when the browser has one: the answer is the
  // same either way, and the server decides that, not this client.
  assert.equal(calls[0].init.credentials, "include");
  assert.equal(index.projects.items.length, 1);
  assert.equal(index.projects.items[0].name, "Open Materials Lab");
});

test("a 503 EXPLORE_UNAVAILABLE is a refusal, not an empty index", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/explore": () => ({
      status: 503,
      json: async () => ({ code: "EXPLORE_UNAVAILABLE", message: "the network index is temporarily unavailable" }),
    }),
  });

  await assert.rejects(
    () => createExploreClient(API, { fetch: fetchFn }).index(),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.status, 503);
      assert.equal(err.code, "EXPLORE_UNAVAILABLE");
      // The sentence says the whole index failed — never "there is nothing
      // here", which is the wrong answer a partial index would give.
      assert.match(err.message, /temporarily unavailable/);
      return true;
    },
  );
});

test("a body missing a section is refused, not rendered short", async () => {
  const body = emptyIndexBody();
  delete body.people;
  const { fetchFn } = fakeFetch({
    "/api/v1/explore": () => ({ status: 200, json: async () => body }),
  });

  await assert.rejects(
    () => createExploreClient(API, { fetch: fetchFn }).index(),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.code, "MALFORMED_INDEX");
      return true;
    },
  );
});

test("a body whose section has no items array is refused", async () => {
  const body = emptyIndexBody();
  body.contributions = { tab: "contributions" };
  const { fetchFn } = fakeFetch({
    "/api/v1/explore": () => ({ status: 200, json: async () => body }),
  });

  await assert.rejects(() => createExploreClient(API, { fetch: fetchFn }).index());
});

test("isIndexShape accepts a full index and refuses each truncated one", () => {
  assert.equal(isIndexShape(emptyIndexBody()), true);
  assert.equal(isIndexShape(null), false);
  assert.equal(isIndexShape([]), false);
  assert.equal(isIndexShape("{}"), false);
  // Removing ANY one of the six dimensions makes the body not-an-index: the
  // guard is by tab, not by "at least one section".
  for (const tab of EXPLORE_TABS) {
    const body = emptyIndexBody();
    delete body[tab];
    assert.equal(isIndexShape(body), false, `a body without ${tab} was accepted`);
  }
});

test("sectionItems reads the section the tab names, never a neighbour's", () => {
  const index = indexBodyWith("people", { id: "u1", handle: "ada", display_name: "Ada", bio: "", url: "/users/u1" });

  assert.equal(sectionItems(index, "people").length, 1);
  assert.equal(sectionItems(index, "people")[0].handle, "ada");
  // Every other tab is empty — not the people row under another heading.
  for (const tab of EXPLORE_TABS) {
    if (tab === "people") continue;
    assert.deepEqual(sectionItems(index, tab), [], `${tab} rendered the people row`);
  }
});

test("tab labels are the six dimensions, and an unknown tab renders verbatim", () => {
  assert.deepEqual(
    EXPLORE_TABS.map(tabLabel),
    ["Projects", "Assets", "Knowledge", "People", "Organizations", "Open Contributions"],
  );
  // Verbatim, not a nearest match and not "Other" (assets.ts's rule): an
  // unknown tab is a value this client does not know, not a value that
  // does not exist.
  assert.equal(tabLabel("models"), "models");
});

test("tab and panel ids are derived from the tab and distinct", () => {
  assert.equal(tabId("projects"), "explore-tab-projects");
  assert.equal(tabPanelId("projects"), "explore-panel-projects");
  for (const tab of EXPLORE_TABS) {
    assert.notEqual(tabId(tab), tabPanelId(tab));
  }
});

test("publishedOn renders the calendar day, never a time of day", () => {
  assert.equal(publishedOn("2026-09-01T10:00:00Z"), "2026-09-01");
  // An unparseable value renders empty rather than "Invalid Date": the row
  // keeps its place, and nothing is invented for it.
  assert.equal(publishedOn("not a date"), "");
});

test("the failure sentence never claims the network is empty", () => {
  assert.match(messageForExploreCode("EXPLORE_UNAVAILABLE"), /unavailable/);
  assert.doesNotMatch(messageForExploreCode("EXPLORE_UNAVAILABLE"), /no results|nothing published|empty/i);
  assert.doesNotMatch(messageForExploreCode("SOMETHING_NEW"), /no results|nothing published|empty/i);
});

test("a 503 with an unreadable body still refuses with the wire status", async () => {
  const { fetchFn } = fakeFetch({
    "/api/v1/explore": () => ({
      status: 503,
      json: async () => {
        throw new Error("not json");
      },
    }),
  });

  await assert.rejects(
    () => createExploreClient(API, { fetch: fetchFn }).index(),
    (err) => {
      assert.ok(err instanceof ApiError);
      assert.equal(err.status, 503);
      assert.equal(err.code, "UNKNOWN");
      return true;
    },
  );
});
