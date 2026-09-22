/**
 * T0608's required test — label: `playwright release flow`.
 *
 * ONE flow, driven in a REAL Chromium against the REAL stack: the routes
 * cmd/api mounts, in front of the real auth guard, over a real PostgreSQL.
 * There is no network mock: the browser talks to the API itself, which is
 * the only way the parts a browser owns — the CORS preflight of every
 * write, the session cookie riding a cross-origin fetch, the CSRF token the
 * app stores at login, the rendering of each state the machine reaches —
 * are exercised at all.
 *
 * The chain, in the order the task book names it:
 *
 *   policy requires reviewers → merge → release → object abort (through
 *   T0602's product path) → the old release is byte-identical.
 *
 * The last leg is the point of the whole flow, and it is NOT "the release
 * shows a warning". A release is an immutable snapshot that renders from
 * its own row (internal/domain/release.go: "the release renders from the
 * row alone, never from live state, so an old release cannot drift"), and
 * the `releases` table carries an append-only trigger. The spec's "later
 * status warning" belongs to the ASSET VERSION (docs/11 §7 Abort/Supersede,
 * docs/43:19), and the asset-side warning is delivered by no task at all —
 * docs/42's Release and Asset page inventories do not carry it. So this
 * flow asserts what the product actually promises: after the abort, the
 * stored release row AND the stored asset version row are BYTE-IDENTICAL,
 * and so is the manifest export the release serves.
 *
 * Every leg has a positive/negative control, so the flow cannot pass by the
 * machine merely working:
 *
 *   1. the SAME release create (the app's own form, same version string) is
 *      refused before the merge and accepted after it;
 *   2. after ONE reviewer has approved BOTH dimensions, the proposal is
 *      still review_required (the policy counts PEOPLE: two are required);
 *      only a SECOND, distinct approver moves it to merge_ready;
 *   3. the row probe is required to REPORT A CHANGE across the abort on a
 *      subject that really does change (the aborted object's version log),
 *      so an instrument that cannot say "different" cannot be mistaken for
 *      one that said "identical".
 *
 * Per step the log names HOW the step ran, because the distinction is the
 * task's authenticity floor:
 *   [ui]      a real user action on a rendered page (click / fill / submit)
 *   [fetch]   a request issued from the page's own context — the real
 *             browser, the real origin, the real CORS preflight, the real
 *             cookies. The browser's fetch performs the request; a human
 *             did not click anything.
 *   [harness] one of the test-owned endpoints, the ONLY steps that are not
 *             product surface:
 *     POST /harness/pull-requests/{n}/request-review — docs/43's
 *     open → review_required. The contract declares no request-review
 *     operation and the permissions matrix has no request_review cell, so
 *     there is nothing to issue a product request TO. The endpoint calls
 *     the production pullrequests.Service.RequestReview — the very call the
 *     missing route would make — and never touches a state column.
 *     GET /harness/rows and GET /harness/object-rows — read-only row
 *     snapshots (SELECT to_jsonb(row)). "The stored row is byte-identical"
 *     is not a question any product route answers: every product read
 *     renders a row for a caller, and a rendering is not the row.
 *     GET /harness/asset-candidate — the manifest document a publisher
 *     authors, computed with the production internal/assets model, so the
 *     bytes the browser posts are the bytes the product's gate re-derives.
 *     GET /harness/requests — the server's own record of what it was asked,
 *     including the preflights Playwright does not surface.
 *
 * Usage: node release-e2e.mjs <web-base> <ready-json-file>
 *   <ready-json-file> is the JSON the Go harness prints once its fixture is
 *   up (api_base, project_id, users, password, …).
 */
import fs from "node:fs";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31170";
const READY_FILE = process.argv[3];
if (!READY_FILE) {
  console.error("release-e2e: usage: node release-e2e.mjs <web-base> <ready-json-file>");
  process.exit(2);
}
const READY = JSON.parse(fs.readFileSync(READY_FILE, "utf8"));
const API = READY.api_base;
const PROJECT = READY.project_id;
const PASSWORD = READY.password;
const SHOTS = process.env.RELEASE_SHOT_DIR ?? "";

const USERS = Object.fromEntries(READY.users.map((u) => [u.handle, u]));

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
const check = (label, condition, detail) => {
  if (condition) ok(label);
  else fail(label, detail);
};
const banner = (text) => console.log(`\n=== ${text}`);

/* ---------- the route inventory: every request this flow makes ---------- */

const routes = new Map();
const hit = (flow, method, route, status) => {
  const key = `${flow} ${method} ${route}`;
  const row = routes.get(key) ?? { n: 0, statuses: new Set() };
  row.n += 1;
  row.statuses.add(status);
  routes.set(key, row);
};

/* ---------- one browser context per human ---------- */

async function openContext(browser, who) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const noise = { console: [], page: [], network: [], server: [], seen: [] };
  // docs/67's browser rule: a browser E2E also watches what the browser
  // itself saw — console errors, uncaught exceptions, requests that never
  // arrived and answers in the 5xx range. A flow that "passes" while the
  // page is throwing is not passing.
  page.on("console", (msg) => {
    if (msg.type() === "error") noise.console.push(msg.text());
  });
  page.on("pageerror", (err) => noise.page.push(String(err?.message ?? err)));
  // Only the API's traffic counts as a failed request: the app aborts its
  // own RSC prefetches when a navigation supersedes them (net::ERR_ABORTED
  // on the web origin), which is the framework's navigation behaviour and
  // not a request the flow depends on.
  page.on("requestfailed", (req) => {
    if (!req.url().startsWith(API)) return;
    const failure = req.failure();
    noise.network.push(`${req.method()} ${req.url()} — ${failure?.errorText ?? "failed"}`);
  });
  page.on("response", async (res) => {
    if (!res.url().startsWith(API)) return;
    // What the BROWSER observed, as opposed to what this script assumed.
    // The app's own form submits are the only writes whose status the flow
    // cannot read from a return value, and recording them here is what
    // makes those assertions observations.
    const entry = {
      method: res.request().method(),
      path: res.url().slice(API.length),
      status: res.status(),
      body: null,
    };
    noise.seen.push(entry);
    if (res.status() >= 400) {
      // A refusal the app renders as one generic sentence: the API's own
      // explanation (the gate report's, for a blocked release) is in the
      // body, and a flow that fails without it is a flow nobody can debug.
      try {
        entry.body = (await res.text()).slice(0, 800);
      } catch {
        entry.body = null;
      }
    }
    if (res.status() >= 500) noise.server.push(`${res.status()} ${res.request().method()} ${res.url()}`);
  });
  return { context, page, who, noise };
}

/** The status the browser observed for the last request to `path`. */
function seenStatus(ctx, method, path) {
  const hits = ctx.noise.seen.filter((r) => r.method === method && r.path === path);
  return hits.length === 0 ? null : hits[hits.length - 1].status;
}

/** The last response body the browser observed for `path`, once the
 *  response event has finished reading it. */
async function seenBody(ctx, method, path, timeoutMs = 3000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const hits = ctx.noise.seen.filter(
      (r) => r.method === method && r.path === path && r.body !== null,
    );
    if (hits.length > 0) return hits[hits.length - 1].body;
    if (Date.now() > deadline) return null;
    await new Promise((r) => setTimeout(r, 50));
  }
}

/** The last request the browser made whose path ENDS WITH `suffix` — for the
 *  routes whose path carries an id this flow does not want to rebuild. */
function seenLast(ctx, method, suffix) {
  const hits = ctx.noise.seen.filter((r) => r.method === method && r.path.endsWith(suffix));
  return hits.length === 0 ? null : hits[hits.length - 1];
}

/** Wait until any of `selectors` is on the page; returns the one that
 *  arrived, or null on timeout. The form a user submits settles in exactly
 *  one of two ways (recorded / refused), and which one arrived is the
 *  observation — a timeout that only says "neither" hides the refusal. */
async function waitForAnyOf(page, selectors, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    for (const selector of selectors) {
      if ((await page.locator(selector).count()) > 0) return selector;
    }
    if (Date.now() > deadline) return null;
    await new Promise((r) => setTimeout(r, 100));
  }
}

/** Sign in through the REAL login page: fill, submit, let the app store its
 *  CSRF token and set the session cookie. */
async function login({ page, who }) {
  await page.goto(`${BASE}/login`, { waitUntil: "domcontentloaded" });
  await page.fill('input[type="email"]', who.email);
  await page.fill('input[type="password"]', PASSWORD);
  await page.click('button[type="submit"]');
  await page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: 30000 });
  await page.waitForFunction(() => window.sessionStorage.getItem("post.csrf") !== null, null, {
    timeout: 30000,
  });
}

/** One request issued from the page's own context, with the headers the app
 *  itself sends: the session cookie rides `credentials: "include"`, every
 *  write carries the CSRF token the login stored, and a creation carries
 *  the contract's Idempotency-Key. */
async function call(page, flow, route, method, path, body, opts = {}) {
  const out = await page.evaluate(
    async ({ apiBase, method, path, body, key }) => {
      const headers = {};
      if (body !== undefined) headers["Content-Type"] = "application/json";
      if (!["GET", "HEAD", "OPTIONS"].includes(method)) {
        const csrf = window.sessionStorage.getItem("post.csrf");
        if (csrf) headers["X-CSRF-Token"] = csrf;
      }
      if (key) headers["Idempotency-Key"] = key;
      let res;
      try {
        res = await fetch(apiBase + path, {
          method,
          headers,
          credentials: "include",
          body: body === undefined ? undefined : JSON.stringify(body),
        });
      } catch (err) {
        // The browser refused to make the request at all. The commonest
        // cause is CORS: a header the preflight did not admit makes fetch
        // reject before anything is sent, which no Go client can notice.
        return { status: 0, body: null, error: `${err.name}: ${err.message}` };
      }
      const text = await res.text();
      let parsed = null;
      try {
        parsed = text === "" ? null : JSON.parse(text);
      } catch {
        parsed = text;
      }
      return { status: res.status, body: parsed, error: "" };
    },
    { apiBase: API, method, path, body, key: opts.key },
  );
  hit(flow, method, route, out.status);
  return out;
}

/** One request, one checked status. */
async function callOk(page, flow, route, method, path, body, want, opts = {}) {
  const res = await call(page, flow, route, method, path, body, opts);
  const label = opts.label ?? `${flow}: ${method} ${route} = ${want}`;
  if (res.status !== want) {
    const why = res.error
      ? `the browser refused the request: ${res.error}`
      : JSON.stringify(res.body)?.slice(0, 400);
    fail(label, `got ${res.status}: ${why}`);
    return res;
  }
  if (opts.expect) opts.expect(res.body);
  else ok(label);
  return res;
}

/** One GET through the page's context, returning the response BYTES as the
 *  browser received them: the sha256 of the body, its length, and its text.
 *  Byte-identity is asserted on the digest, not on a re-rendered object. */
async function fetchBytes(page, flow, route, path) {
  const out = await page.evaluate(
    async ({ apiBase, path }) => {
      const res = await fetch(apiBase + path, { credentials: "include" });
      const buf = await res.arrayBuffer();
      const digest = await crypto.subtle.digest("SHA-256", buf);
      const hex = [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
      return { status: res.status, sha256: hex, length: buf.byteLength, text: new TextDecoder().decode(buf) };
    },
    { apiBase: API, path },
  );
  hit(flow, "GET", route, out.status);
  return out;
}

const short = (s) => (typeof s === "string" && s.length > 8 ? s.slice(0, 8) : String(s ?? ""));

/* ---------- the fixture's product shapes ---------- */

const claimPayload = (statement) => ({
  statement,
  claim_type: "descriptive",
  subject_ref: "material-0001",
  property: "band_gap",
  scope: { system: "fixture" },
  assessment: "preliminary",
});

/** The project policy this flow puts in force: main is protected (a merge
 *  needs an explicit approval chain) and every merge into main needs TWO
 *  distinct approving reviewers. */
const POLICY_VERSION = "v1";
const POLICY_DOC = { main_protected: true, release_min_reviewers: 2 };
const RELEASE_VERSION = "v1.0.0";
const RELEASE_TITLE = "MOF-X 40% RH selectivity";

/* ---------- the flow ---------- */

async function main() {
  const browser = await chromium.launch();
  const alice = await openContext(browser, USERS["release-owner"]);
  const bob = await openContext(browser, USERS["release-reviewer-a"]);
  const carol = await openContext(browser, USERS["release-reviewer-b"]);

  try {
    banner("the three humans sign in through the real login page");
    for (const [name, ctx] of [
      ["alice (owner)", alice],
      ["bob (maintainer, Data Reviewer)", bob],
      ["carol (viewer, Data Reviewer)", carol],
    ]) {
      await login(ctx);
      const csrf = await ctx.page.evaluate(() => window.sessionStorage.getItem("post.csrf"));
      hit("flow", "POST", "/api/v1/auth/login (sign-in page)", 200);
      check(
        `${name} signed in [ui]: the app stored a CSRF token`,
        typeof csrf === "string" && csrf.length > 0,
      );
    }

    // A document on the web origin before any fetch goes out, so every
    // request below is a real cross-origin request from the app's own
    // origin — and therefore carries a real preflight.
    await alice.page.goto(BASE, { waitUntil: "domcontentloaded" });

    const chain = await theChain(alice, bob, carol);
    if (chain === null) fail("the chain: not completed", "an earlier leg returned null");

    banner("the preflights the browser itself performed (the server's record)");
    const served = (await (await fetch(`${API}/harness/requests`)).json()).requests ?? [];
    const preflights = served.filter((r) => r.method === "OPTIONS");
    check(
      "preflight: the browser asked before its first cross-origin write",
      preflights.length > 0,
      `${preflights.length} preflight(s), ${served.length} request(s) served`,
    );
    const idem = preflights.find((p) =>
      (p.asked_headers ?? "").toLowerCase().includes("idempotency-key"),
    );
    check(
      "preflight: a write asked to send the contract's Idempotency-Key",
      idem !== undefined,
      `asks: ${[...new Set(preflights.map((p) => p.asked_headers))].join(" | ")}`,
    );
    if (idem) {
      check(
        "preflight: the API's allow-list admitted it — the write could leave the browser",
        (idem.allowed_headers ?? "").toLowerCase().includes("idempotency-key"),
        `allow-headers ${idem.allowed_headers}`,
      );
      check(
        "preflight: the API answered the app's own origin, 204",
        idem.allowed_origin === BASE && idem.status === 204,
        `allow-origin ${idem.allowed_origin}, status ${idem.status}`,
      );
    }
    const csrfPreflight = preflights.find((p) =>
      (p.asked_headers ?? "").toLowerCase().includes("x-csrf-token"),
    );
    check(
      "preflight: a write asked to send the session's CSRF token, and the API admitted it",
      csrfPreflight !== undefined &&
        (csrfPreflight.allowed_headers ?? "").toLowerCase().includes("x-csrf-token"),
      csrfPreflight
        ? `asked ${csrfPreflight.asked_headers} allowed ${csrfPreflight.allowed_headers}`
        : "none",
    );
    const abortCalls = served.filter(
      (r) => r.method === "POST" && r.path.endsWith(":abort-proposal"),
    );
    check(
      "preflight: the abort write the preflight preceded did reach the API",
      abortCalls.length > 0,
      `${abortCalls.length} abort request(s) served`,
    );
  } finally {
    banner("what the browser itself saw");
    // The flow provokes exactly one console error on purpose: the release
    // form is submitted before the merge and the API refuses it (409), and
    // the browser logs every 4xx resource it loads. Any OTHER console error
    // — an exception, a hydration mismatch, a 5xx — is a failure.
    const provoked = /409 \(Conflict\)/;
    for (const [name, ctx] of [
      ["alice", alice],
      ["bob", bob],
      ["carol", carol],
    ]) {
      const n = ctx.noise;
      const unexpected = n.console.filter((m) => !provoked.test(m));
      check(
        `${name}'s pages logged no unexpected console error [ui] (the provoked 409 aside)`,
        unexpected.length === 0,
        unexpected.slice(0, 3).join(" | "),
      );
      check(
        `${name}'s pages raised no uncaught exception [ui]`,
        n.page.length === 0,
        n.page.slice(0, 3).join(" | "),
      );
      check(
        `${name}'s requests all reached the API [ui]`,
        n.network.length === 0,
        n.network.slice(0, 3).join(" | "),
      );
      check(
        `${name}'s API answers were not 5xx [ui]`,
        n.server.length === 0,
        n.server.slice(0, 3).join(" | "),
      );
    }

    if (fails > 0 && SHOTS) {
      for (const [name, ctx] of [
        ["alice", alice],
        ["bob", bob],
        ["carol", carol],
      ]) {
        try {
          await ctx.page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: true });
        } catch {
          /* the page may already be gone; the failure log is the evidence */
        }
      }
      console.log(`\nrelease-e2e: failure screenshots written to ${SHOTS}`);
    }
    banner("the route inventory: every request this flow made");
    for (const [key, row] of [...routes.entries()].sort()) {
      console.log(`  ${key}  x${row.n}  -> ${[...row.statuses].sort().join(",")}`);
    }
    await browser.close();
  }

  if (fails > 0) {
    console.error(`\nrelease-e2e: FAILED with ${fails} failure(s)`);
    process.exit(1);
  }
  console.log("\nrelease-e2e: all passed");
}

/* ================= the chain ============================================ */

async function theChain(alice, bob, carol) {
  const page = alice.page;
  const flow = "flow";

  /* ---- leg 1: the policy, through the product's own policy route. ---- */
  banner("leg 1 — the project policy requires two reviewers");

  const policy = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/policy",
      "PUT",
      `/api/v1/projects/${PROJECT}/policy`,
      { version: POLICY_VERSION, policy: POLICY_DOC },
      201,
      { label: "leg 1: PUT /api/v1/projects/{id}/policy = 201 [fetch]" },
    )
  ).body;
  check(
    "leg 1: the policy version the API stored is the one this flow set",
    policy?.version === POLICY_VERSION,
    JSON.stringify(policy)?.slice(0, 200),
  );

  const effective = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/policy/effective",
      "GET",
      `/api/v1/projects/${PROJECT}/policy/effective`,
      undefined,
      200,
    )
  ).body;
  check(
    "leg 1: the effective policy read names the project version this flow published",
    effective?.project_policy?.version === POLICY_VERSION,
    `project_policy: ${JSON.stringify(effective?.project_policy)?.slice(0, 200)}`,
  );
  // effective_policy is the EVALUATED document the merge and review paths
  // read — the same rules object they consult, not a summary of it.
  const rules = effective?.effective_policy ?? null;
  check(
    "leg 1: the policy IN FORCE for the project carries release_min_reviewers = 2",
    rules?.release_min_reviewers === 2,
    `effective policy: ${JSON.stringify(effective)?.slice(0, 300)}`,
  );
  check(
    "leg 1: the policy IN FORCE carries main_protected = true",
    rules?.main_protected === true,
    `effective policy: ${JSON.stringify(effective)?.slice(0, 300)}`,
  );

  /* ---- leg 2: main, the research branch, and the proposal. ---- */
  banner("leg 2 — a research proposal into main");

  const main = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches",
      "POST",
      `/api/v1/projects/${PROJECT}/branches`,
      { name: "main", base_ref: "", visibility: "private" },
      201,
    )
  ).body;
  check("leg 2: the project has its main branch", main?.name === "main", JSON.stringify(main));

  // One object on main: the project's accepted state, so a release has
  // something to snapshot and the aborted object has a main-line version.
  const mainClaim = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches/{id}/objects",
      "POST",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects`,
      { object_type: "claim", payload: claimPayload("MOF-X selectivity holds at 40% RH") },
      201,
      { label: "leg 2: the main-line claim is created = 201 [fetch]" },
    )
  ).body;

  const forkPoint = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches/{id}/objects/{id}",
      "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${mainClaim.id}`,
      undefined,
      200,
    )
  ).body.state_id;

  const research = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches",
      "POST",
      `/api/v1/projects/${PROJECT}/branches`,
      { name: "humidity-40rh", base_ref: forkPoint, visibility: "private" },
      201,
    )
  ).body;

  const branchClaim = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches/{id}/objects",
      "POST",
      `/api/v1/projects/${PROJECT}/branches/${research.id}/objects`,
      { object_type: "claim", payload: claimPayload("retention at 40% RH is reproducible") },
      201,
      { label: "leg 2: the research branch's claim is created = 201 [fetch]" },
    )
  ).body;

  const opened = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/pull-requests",
      "POST",
      `/api/v1/projects/${PROJECT}/pull-requests`,
      {
        source_branch_id: research.id,
        target_branch_id: main.id,
        title: "40 RH retention claim",
        body: "T0608 release/abort/policy e2e",
      },
      201,
      { key: "release-e2e-open-0001", label: "leg 2: POST /api/v1/projects/{id}/pull-requests = 201 [fetch]" },
    )
  ).body;
  if (typeof opened?.number !== "number") {
    fail(
      "leg 2: the open route must answer with the new proposal's number",
      `${JSON.stringify(opened)?.slice(0, 200)} — the rest of the chain cannot run`,
    );
    return null;
  }
  const number = opened.number;
  ok(`leg 2: the open route assigned a number (#${number})`);

  const requested = (
    await callOk(
      page,
      flow,
      "/harness/pull-requests/{n}/request-review",
      "POST",
      `/harness/pull-requests/${number}/request-review`,
      { project_id: PROJECT },
      200,
      { label: "leg 2: the proposal is put in review [harness]" },
    )
  ).body;
  check(
    "leg 2: the proposal is in review_required",
    requested.state === "review_required",
    `state ${requested.state}`,
  );

  /* ---- leg 3: the release form is REFUSED before the merge. ---- */
  banner("leg 3 — the release gate refuses before the reviews are merged");
  const releaseURL = `${BASE}/projects/${PROJECT}/releases`;
  let refusalExplanation = null;

  await page.goto(releaseURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-release-version-input]", { timeout: 30000 });
  const emptyState = await page.locator("[data-releases-empty]").count();
  check("leg 3: the project has no release yet [ui]", emptyState === 1, "an empty list was expected");

  await page.fill("[data-release-version-input]", RELEASE_VERSION);
  await page.fill("[data-release-title-input]", RELEASE_TITLE);
  await page.click("[data-release-create]");
  await page.waitForSelector('[data-release-notice="error"]', { timeout: 30000 });
  const refusedStatus = seenStatus(alice, "POST", `/api/v1/projects/${PROJECT}/releases`);
  hit("flow", "POST", "/api/v1/projects/{id}/releases", refusedStatus ?? 0);
  check(
    "leg 3: the API answered the form's create with 409 Conflict",
    refusedStatus === 409,
    `the browser observed ${refusedStatus}`,
  );
  const refusal = (await page.locator('[data-release-notice="error"]').first().innerText()).trim();
  check(
    "leg 3: the app's own release form is refused before the merge [ui]",
    refusal.length > 0,
    "the form showed no refusal text",
  );
  console.log(`     (the refusal the app rendered: ${refusal})`);
  refusalExplanation = await seenBody(alice, "POST", `/api/v1/projects/${PROJECT}/releases`);
  console.log(`     (the API's own explanation: ${refusalExplanation})`);
  check(
    "leg 3: no release row appeared after the refusal [ui]",
    (await page.locator("[data-release-row]").count()) === 0,
    "a row appeared for a refused release",
  );

  /* ---- leg 4: the policy holds the proposal until TWO people approve. ---- */
  banner("leg 4 — the policy counts reviewers, not dimensions");

  const prURL = (n) => `${BASE}/projects/${PROJECT}/pulls/${n}`;
  /**
   * One reviewer's decision, typed into the rendered form. Returns the
   * proposal's lifecycle state after the page is reloaded, or null when the
   * API refused the review — a refusal is REPORTED with the API's own words
   * rather than thrown: a thrown timeout ends the whole flow and tells the
   * reader nothing about which leg broke or why.
   */
  const submitReview = async (ctx, who, n, kind, text) => {
    await ctx.page.goto(prURL(n), { waitUntil: "domcontentloaded" });
    await ctx.page.waitForSelector("[data-review-form]", { timeout: 30000 });
    await ctx.page.selectOption("select[data-review-kind]", kind);
    await ctx.page.selectOption("select[data-review-decision]", "approved");
    await ctx.page.fill("textarea[data-review-body]", text);
    await ctx.page.click("button[data-review-submit]");
    const settled = await waitForAnyOf(
      ctx.page,
      ["[data-review-saved]", "[data-review-error]"],
      30000,
    );
    const observed = seenLast(ctx, "POST", "/reviews");
    if (settled !== "[data-review-saved]") {
      const rendered =
        settled === "[data-review-error]"
          ? (await ctx.page.locator("[data-review-error]").first().innerText()).trim()
          : "the form showed neither an acknowledgement nor a refusal";
      fail(
        `${who}'s ${kind} review of #${n} is recorded [ui]`,
        `${rendered} — the browser saw ${observed?.status ?? "no"} POST .../reviews` +
          `${observed?.body ? `; the API said: ${observed.body}` : ""}`,
      );
      return null;
    }
    ok(`${who}'s ${kind} review of #${n} is recorded [ui]`);
    hit("flow", "POST", "/api/v1/projects/{id}/pull-requests/{n}/reviews", observed?.status ?? 201);
    await ctx.page.reload({ waitUntil: "domcontentloaded" });
    await ctx.page.waitForSelector("[data-pull-state]", { timeout: 30000 });
    return ctx.page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
  };

  let state = await submitReview(bob, "bob", number, "scientific", "the retention numbers follow from the runs");
  check(
    "leg 4: one reviewer's scientific approval alone does not advance the proposal [ui]",
    state === "review_required",
    `data-pull-state=${state}`,
  );

  state = await submitReview(bob, "bob", number, "integrity", "sources and pins are sound");
  check(
    "leg 4: the SAME reviewer approving the second dimension still does not advance it [ui] " +
      "(release_min_reviewers = 2 counts distinct people)",
    state === "review_required",
    `data-pull-state=${state} — with one approver recorded the proposal must stay in review`,
  );

  state = await submitReview(carol, "carol", number, "scientific", "the 40% RH series is consistent");
  check(
    "leg 4: a SECOND distinct approver advances the proposal to merge_ready [ui]",
    state === "merge_ready",
    `data-pull-state=${state}`,
  );

  /* ---- leg 5: the merge. ---- */
  banner("leg 5 — the merge");

  const merged = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/pull-requests/{n}:merge",
      "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`,
      undefined,
      200,
      {
        key: "release-e2e-merge-0001",
        label: "leg 5: POST /api/v1/projects/{id}/pull-requests/{n}:merge = 200 [fetch]",
        expect: (b) => {
          check(
            "leg 5: the merge committed a new accepted state on main",
            typeof b.state_id === "string" && b.state_id !== "",
            JSON.stringify(b)?.slice(0, 300),
          );
          check("leg 5: the merge applied the branch's claim", b.applied === 1, `applied ${b.applied}`);
          // This suite runs the database half only (no git provider is
          // wired into this harness) — recorded, not glossed over.
          console.log(
            `     (git_state=${b.git_state}, git_error=${b.git_error ?? ""})`,
          );
        },
      },
    )
  ).body;

  const accepted = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/branches/{id}/objects/{id}",
      "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${branchClaim.id}`,
      undefined,
      200,
    )
  ).body;
  check(
    "leg 5: the object the branch proposed is what main now carries",
    accepted?.payload?.statement === "retention at 40% RH is reproducible",
    `statement ${accepted?.payload?.statement}`,
  );
  check(
    "leg 5: that object's main-line version lives in the state the merge committed",
    accepted?.state_id === merged.state_id,
    `object is in ${short(accepted?.state_id)}, the merge committed ${short(merged.state_id)}`,
  );
  check(
    "leg 5: the merged version is active",
    accepted?.lifecycle_state === "active",
    `lifecycle_state=${accepted?.lifecycle_state}`,
  );

  /* ---- leg 6: the release, through the app's own Releases tab. ---- */
  banner("leg 6 — the release, cut from the merged state");

  await page.goto(releaseURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-release-version-input]", { timeout: 30000 });
  await page.fill("[data-release-version-input]", RELEASE_VERSION);
  await page.fill("[data-release-title-input]", RELEASE_TITLE);
  await page.click("[data-release-create]");
  // The gate's answer, whichever way it goes: the create is the same
  // request in both legs, so a refusal here must say what changed.
  await page.waitForSelector(
    '[data-release-notice="success"], [data-release-notice="error"]',
    { timeout: 30000 },
  );
  if ((await page.locator('[data-release-notice="error"]').count()) > 0) {
    const again = await seenBody(alice, "POST", `/api/v1/projects/${PROJECT}/releases`);
    fail(
      "leg 6: the release the API refused before the merge is still refused after it",
      `the API said: ${again} (before the merge it said: ${refusalExplanation})`,
    );
    // Why the review fact is still false: the gate reads the reviews of the
    // research PRs whose PROPOSAL is an ancestor of the released state
    // (ListReleaseReviews), and this probe reports what that reading finds
    // for the state the merge just accepted.
    const lineage = await call(
      page, flow, "/harness/review-lineage", "GET",
      `/harness/review-lineage?state_id=${merged.state_id}`, undefined,
    );
    console.log(`     (the release gate's own reading of ${merged.state_id}: ${JSON.stringify(lineage.body)})`);
    check(
      "leg 6: the state main just accepted has a review record, read the way the release gate reads one",
      (lineage.body?.matching_reviews ?? -1) > 0,
      `matching_reviews=${lineage.body?.matching_reviews}; the proposal #${number} is not among the state's ancestors`,
    );
    return null;
  }
  const createdStatus = seenStatus(alice, "POST", `/api/v1/projects/${PROJECT}/releases`);
  hit("flow", "POST", "/api/v1/projects/{id}/releases", createdStatus ?? 0);
  check(
    "leg 6: the SAME release version the API refused before the merge is accepted after it [ui]",
    createdStatus === 201,
    `the browser observed ${createdStatus}`,
  );

  await page.waitForSelector(`[data-release-row="${RELEASE_VERSION}"]`, { timeout: 30000 });
  const releaseHref = await page
    .locator(`[data-release-link="${RELEASE_VERSION}"]`)
    .first()
    .getAttribute("href");
  const releaseID = (releaseHref ?? "").split("/").filter(Boolean).pop();
  check(
    "leg 6: the new release is listed on the Releases tab, with an id of its own [ui]",
    typeof releaseID === "string" && releaseID.length === 36,
    `href ${releaseHref}`,
  );
  check(
    "leg 6: the release row links to its manifest export [ui]",
    (await page.locator(`[data-release-manifest="${RELEASE_VERSION}"]`).count()) === 1,
    "no manifest link rendered",
  );
  console.log(`     (release ${RELEASE_VERSION} = ${releaseID})`);

  const releaseBefore = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/releases/{id}",
      "GET",
      `/api/v1/projects/${PROJECT}/releases/${releaseID}`,
      undefined,
      200,
    )
  ).body;
  check(
    "leg 6: the release pins the state the merge committed",
    releaseBefore?.state_id === merged.state_id,
    `release pins ${short(releaseBefore?.state_id)}, the merge committed ${short(merged.state_id)}`,
  );
  check(
    "leg 6: the release pins the project policy version in force",
    typeof releaseBefore?.policy_version_id === "string" && releaseBefore.policy_version_id !== "",
    `policy_version_id=${releaseBefore?.policy_version_id}`,
  );

  const manifestPath = `/api/v1/projects/${PROJECT}/releases/${releaseID}/manifest`;
  const manifestBefore = await fetchBytes(
    page,
    flow,
    "/api/v1/projects/{id}/releases/{id}/manifest",
    manifestPath,
  );
  check(
    "leg 6: the manifest export is served",
    manifestBefore.status === 200 && manifestBefore.length > 0,
    `status ${manifestBefore.status}, ${manifestBefore.length} bytes`,
  );
  let manifestDoc = null;
  try {
    manifestDoc = JSON.parse(manifestBefore.text);
  } catch {
    manifestDoc = null;
  }
  check(
    "leg 6: the exported manifest carries the row's manifest hash",
    manifestDoc?.manifest_hash === releaseBefore?.manifest_hash,
    `doc ${manifestDoc?.manifest_hash} vs row ${releaseBefore?.manifest_hash}`,
  );
  check(
    "leg 6: the exported manifest names the released state",
    manifestDoc?.state_id === merged.state_id,
    `doc state ${short(manifestDoc?.state_id)} vs ${short(merged.state_id)}`,
  );
  // The review record travels INSIDE the immutable bytes (docs/11 §1: a
  // release fixes the review/approval record). This is the policy leg
  // landing in the snapshot: two distinct people, both dimensions
  // approved — the same counts the review leg forced by hand above.
  const recordedApprovals = (manifestDoc?.reviews ?? []).flatMap((rec) =>
    (rec.reviews ?? [])
      .filter((r) => r.decision === "approved")
      .map((r) => ({ reviewer: r.reviewer_id, kind: r.review_kind })),
  );
  const approvers = new Set(recordedApprovals.map((a) => a.reviewer));
  const approvedKinds = new Set(recordedApprovals.map((a) => a.kind));
  check(
    "leg 6: the release snapshot records approvals from TWO distinct reviewers",
    approvers.size === 2,
    `${approvers.size} distinct approver(s): ${JSON.stringify(recordedApprovals)}`,
  );
  check(
    "leg 6: the release snapshot records both dimensions approved",
    approvedKinds.has("scientific") && approvedKinds.has("integrity"),
    `approved kinds: ${[...approvedKinds].join(", ")}`,
  );
  check(
    "leg 6: the snapshot's review record names the proposal those approvals were given on",
    (manifestDoc?.reviews ?? []).some((rec) => rec.pull_request_number === number),
    `records: ${JSON.stringify((manifestDoc?.reviews ?? []).map((r) => r.pull_request_number))}, want #${number}`,
  );

  const releaseRowBefore = (
    await callOk(page, flow, "/harness/rows", "GET",
      `/harness/rows?release_id=${releaseID}`, undefined, 200,
      { label: "leg 6: the stored release row is read verbatim [harness]" })
  ).body.release;
  check(
    "leg 6: the stored release row was read (the probe found it)",
    typeof releaseRowBefore === "string" && releaseRowBefore.length > 0,
    `${releaseRowBefore}`,
  );

  /* ---- leg 7: the asset version, published through the product route. ---- */
  banner("leg 7 — the released state is published as an asset version");

  const candidate = (
    await callOk(page, flow, "/harness/asset-candidate", "GET",
      `/harness/asset-candidate?project_id=${PROJECT}&release_id=${releaseID}` +
        `&object_version_id=${accepted.version_id}`,
      undefined, 200,
      { label: "leg 7: the publish candidate document is read [harness]" })
  ).body;
  check(
    "leg 7: the candidate pins the release it was published from",
    Array.isArray(candidate.origin_refs) &&
      candidate.origin_refs.includes(`release:${releaseID}`),
    JSON.stringify(candidate?.origin_refs),
  );
  check(
    "leg 7: the candidate pins the object version it carries",
    Array.isArray(candidate.origin_refs) &&
      candidate.origin_refs.includes(`object_version:${accepted.version_id}`),
    JSON.stringify(candidate?.origin_refs),
  );

  const published = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/assets:publish",
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      candidate,
      201,
      { key: "release-e2e-publish-0001", label: "leg 7: POST /api/v1/projects/{id}/assets:publish = 201 [fetch]" },
    )
  ).body;
  check(
    "leg 7: the publish minted the new asset a pid",
    typeof published?.asset_pid === "string" && published.asset_pid.length > 0,
    JSON.stringify(published)?.slice(0, 300),
  );
  check(
    "leg 7: the stored version's integrity hash is the one that covers the manifest",
    published?.integrity_hash === candidate.integrity_hash,
    `stored ${published?.integrity_hash} vs candidate ${candidate.integrity_hash}`,
  );
  check(
    "leg 7: the version is stored with the release pin it was published from",
    Array.isArray(published?.origin_refs) && published.origin_refs.includes(`release:${releaseID}`),
    JSON.stringify(published?.origin_refs),
  );
  const assetPID = published.asset_pid;
  const assetVersion = published.version;
  console.log(`     (asset ${assetPID} version ${assetVersion})`);

  const assetRowBefore = (
    await callOk(page, flow, "/harness/rows", "GET",
      `/harness/rows?asset_pid=${assetPID}&asset_version=${assetVersion}`, undefined, 200,
      { label: "leg 7: the stored asset version row is read verbatim [harness]" })
  ).body.asset_version;
  check(
    "leg 7: the stored asset version row was read (the probe found it)",
    typeof assetRowBefore === "string" && assetRowBefore.length > 0,
    `${assetRowBefore}`,
  );

  if (process.env.RELEASE_CHAIN_OUT) {
    fs.writeFileSync(
      process.env.RELEASE_CHAIN_OUT,
      JSON.stringify({
        number,
        releaseID,
        assetPID,
        assetVersion,
        acceptedVersionID: accepted.version_id,
        objectID: accepted.id,
      }),
    );
  }

  /* ---- leg 8: the abort, through T0602's product path. ---- */
  banner("leg 8 — the object is aborted through the product's abort route");

  const beforeAbort = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${branchClaim.id}`, undefined, 200)
  ).body;
  check(
    "leg 8: the object to abort is active on main",
    beforeAbort?.lifecycle_state === "active",
    `lifecycle_state=${beforeAbort?.lifecycle_state}`,
  );

  // Two instruments for "main has not moved", because the object read above
  // is NOT one: GetObject answers with the object's newest version overall
  // (internal/application/rsg/service.go:517, GetLatestVersion), and the
  // branch in its path is checked for existence, never used to scope the
  // version. So it reports what the abort wrote — on the PROPOSAL branch —
  // the moment that branch exists. What actually answers the question is
  // main's head state (the overview route) and the as-of view pinned to it
  // (the query route's slice pinning).
  const overviewMainHead = async () =>
    (
      await callOk(page, flow, "/api/v1/projects/{id}/overview", "GET",
        `/api/v1/projects/${PROJECT}/overview`, undefined, 200)
    ).body?.current_main?.head_state_id;
  const asOf = async (stateID) =>
    (
      await callOk(page, flow, "/api/v1/projects/{id}/query", "GET",
        `/api/v1/projects/${PROJECT}/query?state_id=${stateID}&object_type=claim`, undefined, 200)
    ).body?.objects?.find((o) => o.id === branchClaim.id) ?? null;

  const mainHeadBefore = await overviewMainHead();
  check(
    "leg 8: the overview route names main's head state",
    typeof mainHeadBefore === "string" && mainHeadBefore.length > 0,
    `current_main.head_state_id=${mainHeadBefore}`,
  );

  const versionRowsBefore = (
    await callOk(page, flow, "/harness/object-rows", "GET",
      `/harness/object-rows?object_id=${branchClaim.id}`, undefined, 200,
      { label: "leg 8: the object's version log is read verbatim [harness]" })
  ).body;

  const abortPath = `/api/v1/projects/${PROJECT}/objects/${branchClaim.id}:abort-proposal`;
  const abort = (
    await callOk(
      page,
      flow,
      "/api/v1/projects/{id}/objects/{id}:abort-proposal",
      "POST",
      abortPath,
      {
        object_version_ref: `object_version:${beforeAbort.version_id}`,
        reason_code: "measurement_retracted",
        explanation: "the 40% RH series was re-measured and the drift was not corrected",
      },
      201,
      { key: "release-e2e-abort-0001", label: "leg 8: POST /api/v1/projects/{id}/objects/{id}:abort-proposal = 201 [fetch]" },
    )
  ).body;
  check(
    "leg 8: the abort wrote a version in lifecycle 'aborted'",
    abort?.lifecycle_state === "aborted",
    `lifecycle_state=${abort?.lifecycle_state}`,
  );
  check(
    "leg 8: the abort opened a proposal instead of touching main",
    typeof abort?.pull_request_number === "number" && abort.pull_request_number > 0,
    `pull_request_number=${abort?.pull_request_number}`,
  );
  check(
    "leg 8: the abort recorded the reason and the human explanation docs/46 requires",
    abort?.reason_code === "measurement_retracted" && (abort?.explanation ?? "").length > 0,
    `reason_code=${abort?.reason_code}`,
  );

  check(
    "leg 8: the abort wrote its version on a proposal branch, not on main",
    abort?.branch_id !== undefined && abort.branch_id !== main.id && abort.branch_name !== "main",
    `branch_name=${abort?.branch_name}, branch_id=${short(abort?.branch_id)} vs main ${short(main.id)}`,
  );
  const mainHeadAfterProposal = await overviewMainHead();
  check(
    "leg 8: main's head state does NOT move while the abort is only a proposal",
    mainHeadAfterProposal === mainHeadBefore,
    `main's head was ${short(mainHeadBefore)}, an unmerged proposal left it ${short(mainHeadAfterProposal)}` +
      " — docs/09 §4: frozen main moves only through a Research PR merge",
  );
  const asOfMain = await asOf(mainHeadBefore);
  check(
    "leg 8: the state main is at still carries the object as active",
    asOfMain !== null && asOfMain.lifecycle_state === "active" &&
      asOfMain.version_no === beforeAbort.version_no,
    asOfMain === null
      ? "the state-pinned slice did not return the object at all"
      : `as of main's head the object is ${asOfMain.lifecycle_state} at version ${asOfMain.version_no}` +
        ` (main carried version ${beforeAbort.version_no} before the abort)`,
  );

  const abortNumber = abort.pull_request_number;
  const abortURL = `${BASE}/projects/${PROJECT}/pulls/${abortNumber}`;
  await page.goto(abortURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-pull-state]", { timeout: 30000 });
  check(
    "leg 8: the abort proposal's page renders the proposal [ui]",
    (await page.locator("[data-pulls-detail]").first().getAttribute("data-pull-number")) ===
      String(abortNumber),
    `#${abortNumber}`,
  );
  const abortRequested = (
    await callOk(page, flow, "/harness/pull-requests/{n}/request-review", "POST",
      `/harness/pull-requests/${abortNumber}/request-review`, { project_id: PROJECT }, 200,
      { label: "leg 8: the abort proposal is put in review [harness]" })
  ).body;
  check(
    "leg 8: the abort proposal is in review_required",
    abortRequested.state === "review_required",
    `state ${abortRequested.state}`,
  );

  let abortState = await submitReview(bob, "bob", abortNumber, "scientific", "the retraction is justified");
  abortState = await submitReview(bob, "bob", abortNumber, "integrity", "the record is complete");
  check(
    "leg 8: the abort proposal is held by the SAME two-reviewer policy [ui]",
    abortState === "review_required",
    `data-pull-state=${abortState}`,
  );
  abortState = await submitReview(carol, "carol", abortNumber, "scientific", "agreed, the series is withdrawn");
  check(
    "leg 8: the abort proposal reaches merge_ready with a second approver [ui]",
    abortState === "merge_ready",
    `data-pull-state=${abortState}`,
  );
  if (abortState !== "merge_ready") {
    // The abort cannot merge, so legs 8's tail and 9 have nothing to read.
    // Stopping here is the honest end: the refusal above is already reported
    // with the API's own words, and a cascade of "the merge failed" lines
    // would bury it.
    fail(
      "leg 8: the abort chain stops here",
      `the abort proposal is ${abortState ?? "without recorded reviews"}, so merging it and the ` +
        "immutability leg that follows it were not reached",
    );
    return null;
  }

  const abortMerged = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${abortNumber}:merge`, undefined, 200,
      { key: "release-e2e-abort-merge-0001",
        label: "leg 8: the abort proposal is merged = 200 [fetch]" })
  ).body;
  check(
    "leg 8: the abort merge committed a new accepted state on main",
    typeof abortMerged?.state_id === "string" && abortMerged.state_id !== "",
    JSON.stringify(abortMerged)?.slice(0, 300),
  );

  const afterAbort = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${branchClaim.id}`, undefined, 200)
  ).body;
  check(
    "leg 8: the object is aborted on main once the proposal merges [fetch]",
    afterAbort?.lifecycle_state === "aborted",
    `lifecycle_state=${afterAbort?.lifecycle_state}`,
  );
  check(
    "leg 8: the abort landed a NEW version on main rather than deleting one",
    afterAbort?.version_no > beforeAbort?.version_no,
    `version_no ${beforeAbort?.version_no} → ${afterAbort?.version_no}`,
  );
  const mainHeadAfterMerge = await overviewMainHead();
  check(
    "leg 8: main's head moved at the merge and at nothing else",
    typeof mainHeadAfterMerge === "string" && mainHeadAfterMerge !== mainHeadBefore,
    `main's head was ${short(mainHeadBefore)} before the abort proposal and is ` +
      `${short(mainHeadAfterMerge)} after the merge (the merge committed ${short(abortMerged?.state_id)})`,
  );
  const asOfMerged = await asOf(mainHeadAfterMerge);
  check(
    "leg 8: main carries the aborted version, and carries it through the merge",
    asOfMerged !== null && asOfMerged.lifecycle_state === "aborted",
    asOfMerged === null
      ? "the state-pinned slice did not return the object at all"
      : `as of main's accepted state the object is ${asOfMerged.lifecycle_state} at version ${asOfMerged.version_no}`,
  );

  /* ---- leg 9: nothing that was already immutable moved. ---- */
  banner("leg 9 — the old release and the old asset version are byte-identical");

  // The instrument's own control. The version log of the aborted object DID
  // change; if this probe reports that it did not, the probe is blind and
  // every "identical" verdict below is worthless.
  const versionRowsAfter = (
    await callOk(page, flow, "/harness/object-rows", "GET",
      `/harness/object-rows?object_id=${branchClaim.id}`, undefined, 200,
      { label: "leg 9: the object's version log is read again [harness]" })
  ).body;
  check(
    "leg 9: the row probe REPORTS A CHANGE on a subject that changed (instrument control)",
    versionRowsAfter.rows.join("\n") !== versionRowsBefore.rows.join("\n") &&
      versionRowsAfter.count > versionRowsBefore.count,
    `${versionRowsBefore.count} version row(s) before, ${versionRowsAfter.count} after`,
  );

  const releaseRowAfter = (
    await callOk(page, flow, "/harness/rows", "GET",
      `/harness/rows?release_id=${releaseID}`, undefined, 200,
      { label: "leg 9: the stored release row is read again [harness]" })
  ).body.release;
  check(
    "leg 9: the stored release row is BYTE-IDENTICAL after the abort",
    releaseRowAfter === releaseRowBefore,
    `before ${releaseRowBefore}\n     after  ${releaseRowAfter}`,
  );

  const assetRowAfter = (
    await callOk(page, flow, "/harness/rows", "GET",
      `/harness/rows?asset_pid=${assetPID}&asset_version=${assetVersion}`, undefined, 200,
      { label: "leg 9: the stored asset version row is read again [harness]" })
  ).body.asset_version;
  check(
    "leg 9: the stored asset version row is BYTE-IDENTICAL after the abort",
    assetRowAfter === assetRowBefore,
    `before ${assetRowBefore}\n     after  ${assetRowAfter}`,
  );

  const releaseAfter = (
    await callOk(page, flow, "/api/v1/projects/{id}/releases/{id}", "GET",
      `/api/v1/projects/${PROJECT}/releases/${releaseID}`, undefined, 200)
  ).body;
  check(
    "leg 9: the release the API serves is the same document after the abort",
    JSON.stringify(releaseAfter) === JSON.stringify(releaseBefore),
    `before ${JSON.stringify(releaseBefore)}\n     after  ${JSON.stringify(releaseAfter)}`,
  );

  const manifestAfter = await fetchBytes(
    page, flow, "/api/v1/projects/{id}/releases/{id}/manifest", manifestPath,
  );
  check(
    "leg 9: the manifest export is BYTE-IDENTICAL after the abort",
    manifestAfter.sha256 === manifestBefore.sha256,
    `sha256 ${manifestBefore.sha256} (${manifestBefore.length} bytes) → ` +
      `${manifestAfter.sha256} (${manifestAfter.length} bytes)`,
  );
  check(
    "leg 9: the release still answers its own manifest export route",
    manifestAfter.status === 200,
    `status ${manifestAfter.status}`,
  );

  // The asset page is a read of the SAME stored version through the product
  // surface: it must resolve to the version that was published, and it must
  // not have grown a field nobody promised.
  const assetPage = (
    await callOk(page, flow, "/api/v1/assets/{pid}", "GET",
      `/api/v1/assets/${assetPID}?version=${assetVersion}`, undefined, 200)
  ).body;
  check(
    "leg 9: the asset page still resolves the published version to the release it came from",
    JSON.stringify(assetPage).includes(`release:${releaseID}`),
    `the page does not name release:${releaseID}`,
  );
  console.log(
    "     (the asset-side 'later status warning' docs/11 §7 promises for a version whose " +
      "subject was later aborted is delivered by no task: docs/42's Release and Asset page " +
      "inventories do not carry it, and this page has no such field. This flow asserts the " +
      "version row and its content stay immutable — the warning itself is a gap, not a bug here.)",
  );

  return { number, releaseID, assetPID, assetVersion, abortNumber, acceptedVersionID: accepted.version_id, objectID: accepted.id };
}

await main();
