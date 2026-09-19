/**
 * T0410's required test — label: `playwright pr flows`.
 *
 * Two flows of the seed project (docs/34) driven in a REAL Chromium against
 * the REAL stack: the routes cmd/api mounts, in front of the real auth
 * guard, over a real PostgreSQL and a real Gitea. There is no network mock
 * here — the other browser suites (tests/e2e-pulls, tests/e2e-conflicts)
 * intercept the API and pin the wire shapes; this one lets the browser talk
 * to the API itself, which is the only way the parts a browser owns — the
 * CORS preflight of every write, the session cookie riding a cross-origin
 * fetch, the CSRF token the app stores at login, the rendering of each
 * state the machine reaches — are exercised at all.
 *
 *   Flow 1 — docs/34's seed flow, conflict-free: batch create (main + the
 *   four research branches) → objects → push → open pull request → request
 *   review → three review submissions → merge. Nothing on this path
 *   constructs state: every write is a request the product serves, and the
 *   only test-owned endpoint is the /harness/ stand-in for the one
 *   transition this build has no route for (see below).
 *
 *   Flow 2 — one scientific conflict: both sides move the same protocol
 *   parameter to different values, the merge refuses to choose (409
 *   MERGE_BLOCKED), a human records `keep_both` on the conflicts page, and
 *   only then does the same merge land — carrying the disagreement instead
 *   of resolving it. The positive control is the pair: the SAME merge
 *   request is refused before the decision and accepted after it, so the
 *   test cannot pass by the merge simply working.
 *
 * Per step the log names HOW the step ran, because the distinction is the
 * task's authenticity floor:
 *   [ui]      a real user action on a rendered page (click / fill / submit)
 *   [fetch]   a request issued from the page's own context — the real
 *             browser, the real origin, the real CORS preflight, the real
 *             cookies. The browser's fetch performs the request; a human
 *             did not click anything.
 *   [harness] one of the two test-owned endpoints, the ONLY steps that are
 *             not product surface:
 *     POST /harness/pull-requests/{n}/request-review — docs/43's
 *     open → review_required. The contract declares no request-review
 *     operation (specs/api/openapi.yaml) and specs/policies/
 *     permissions-matrix.csv has no request_review cell, so there is
 *     nothing to issue a product request TO. The endpoint calls the
 *     production pullrequests.Service.RequestReview — the very call the
 *     missing route would make — and never touches a state column.
 *     POST /harness/git/push — a scientist's `git push`, the transport
 *     that puts a commit on the research ref (a browser cannot push, and
 *     the provider only merges a ref pair that carries a real commit).
 *
 * Usage: node pr-flows-e2e.mjs <web-base> <ready-json-file>
 *   <ready-json-file> is the JSON the Go harness prints once its fixture is
 *   up (api_base, project_id, users, password, …).
 */
import fs from "node:fs";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31160";
const READY_FILE = process.argv[3];
if (!READY_FILE) {
  console.error("pr-flows-e2e: usage: node pr-flows-e2e.mjs <web-base> <ready-json-file>");
  process.exit(2);
}
const READY = JSON.parse(fs.readFileSync(READY_FILE, "utf8"));
const API = READY.api_base;
const PROJECT = READY.project_id;
const PASSWORD = READY.password;
const SHOTS = process.env.PR_FLOWS_SHOT_DIR ?? "";

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
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/* ---------- the route inventory: every request these flows make ---------- */

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
  const noise = { console: [], page: [], network: [], server: [] };
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
  // not a request the flows depend on.
  page.on("requestfailed", (req) => {
    if (!req.url().startsWith(API)) return;
    const failure = req.failure();
    noise.network.push(`${req.method()} ${req.url()} — ${failure?.errorText ?? "failed"}`);
  });
  page.on("response", (res) => {
    if (res.status() >= 500 && res.url().startsWith(API)) {
      noise.server.push(`${res.status()} ${res.request().method()} ${res.url()}`);
    }
  });
  return { context, page, who, noise };
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
      // Every write carries the session's CSRF token, body or not: the
      // guard rejects a state change without it, and the merge route takes
      // no body at all.
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
    const why = res.error ? `the browser refused the request: ${res.error}` : JSON.stringify(res.body)?.slice(0, 400);
    fail(label, `got ${res.status}: ${why}`);
    return res;
  }
  if (opts.expect) opts.expect(res.body);
  else ok(label);
  return res;
}

const short = (s) => (typeof s === "string" && s.length > 8 ? s.slice(0, 8) : String(s ?? ""));

/* ---------- the fixture's product shapes ---------- */

const protocolPayload = (purpose, temperature) => ({
  purpose,
  domain: "materials",
  steps: [{ id: "s1", action: "heat" }],
  parameters: { temperature },
  requirements: ["dry glovebox"],
});

const claimPayload = (statement) => ({
  statement,
  claim_type: "descriptive",
  subject_ref: "material-0001",
  property: "band_gap",
  scope: { system: "fixture" },
  assessment: "preliminary",
});

/** A finding has to pin at least one claim version (the RSG domain rule the
 *  create route enforces), so the main-line claim's version id is threaded
 *  into it. */
const findingPayload = (statement, claimVersionID) => ({
  statement,
  finding_type: "observation",
  claim_version_refs: [claimVersionID],
  assessment: "preliminary",
});

/* ---------- the flows ---------- */

async function main() {
  const browser = await chromium.launch();
  const alice = await openContext(browser, USERS["flow-owner"]);
  const bob = await openContext(browser, USERS["flow-maintainer"]);
  const carol = await openContext(browser, USERS["flow-reviewer"]);

  try {
    banner("the three humans sign in through the real login page");
    for (const [name, ctx] of [
      ["alice (owner)", alice],
      ["bob (maintainer)", bob],
      ["carol (viewer)", carol],
    ]) {
      await login(ctx);
      const csrf = await ctx.page.evaluate(() => window.sessionStorage.getItem("post.csrf"));
      hit("both", "POST", "/api/v1/auth/login (sign-in page)", 200);
      check(`${name} signed in [ui]: the app stored a CSRF token`,
        typeof csrf === "string" && csrf.length > 0);
    }

    // A document on the web origin before any fetch goes out, so every
    // request below is a real cross-origin request from the app's own
    // origin — and therefore carries a real preflight.
    await alice.page.goto(BASE, { waitUntil: "domcontentloaded" });

    const one = await flowOne(alice, bob, carol);
    if (one === null) {
      fail("flow 2: not run", "flow 1 did not produce the branches and branches' objects flow 2 builds on");
    } else {
      await flowTwo(alice, bob, carol, one);
    }

    // The browser's own preflights, read back from the SERVER: Playwright's
    // network events do not surface a preflight, and the question — did the
    // API admit the headers the browser asked to send — is one only the
    // server's own record can answer.
    banner("the preflights the browser itself performed (the server's record)");
    const served = (await (await fetch(`${API}/harness/requests`)).json()).requests ?? [];
    const preflights = served.filter((r) => r.method === "OPTIONS");
    check("preflight: the browser asked before its first cross-origin write",
      preflights.length > 0, `${preflights.length} preflight(s), ${served.length} request(s) served`);
    const idem = preflights.find((p) => (p.asked_headers ?? "").toLowerCase().includes("idempotency-key"));
    check("preflight: a write asked to send the contract's Idempotency-Key",
      idem !== undefined,
      `asks: ${[...new Set(preflights.map((p) => p.asked_headers))].join(" | ")}`);
    if (idem) {
      check("preflight: the API's allow-list admitted it — the write could leave the browser",
        (idem.allowed_headers ?? "").toLowerCase().includes("idempotency-key"),
        `allow-headers ${idem.allowed_headers}`);
      check("preflight: the API answered the app's own origin, 204",
        idem.allowed_origin === BASE && idem.status === 204,
        `allow-origin ${idem.allowed_origin}, status ${idem.status}`);
    }
    const csrfPreflight = preflights.find((p) => (p.asked_headers ?? "").toLowerCase().includes("x-csrf-token"));
    check("preflight: a write asked to send the session's CSRF token, and the API admitted it",
      csrfPreflight !== undefined &&
        (csrfPreflight.allowed_headers ?? "").toLowerCase().includes("x-csrf-token"),
      csrfPreflight ? `asked ${csrfPreflight.asked_headers} allowed ${csrfPreflight.allowed_headers}` : "none");
    const writesThatArrived = served.filter(
      (r) => r.method === "POST" && r.path.endsWith("/pull-requests"),
    );
    check("preflight: the write the preflight preceded did reach the API",
      writesThatArrived.length > 0,
      `${writesThatArrived.length} creation request(s) served`);

  } finally {
    banner("what the browser itself saw");
    // The flows provoke exactly one console error on purpose: flow 2's
    // positive control asks the API to merge an undecided conflict, and the
    // browser logs every 4xx resource it loads. Any OTHER console error —
    // an exception, a hydration mismatch, a 5xx — is a failure.
    const provoked = /409 \(Conflict\)/;
    for (const [name, ctx] of [["alice", alice], ["bob", bob], ["carol", carol]]) {
      const n = ctx.noise;
      const unexpected = n.console.filter((m) => !provoked.test(m));
      check(`${name}'s pages logged no unexpected console error [ui] (the provoked 409 aside)`,
        unexpected.length === 0, unexpected.slice(0, 3).join(" | "));
      check(`${name}'s pages raised no uncaught exception [ui]`, n.page.length === 0,
        n.page.slice(0, 3).join(" | "));
      check(`${name}'s requests all reached the API [ui]`, n.network.length === 0,
        n.network.slice(0, 3).join(" | "));
      check(`${name}'s API answers were not 5xx [ui]`, n.server.length === 0,
        n.server.slice(0, 3).join(" | "));
    }

    if (fails > 0 && SHOTS) {
      for (const [name, ctx] of [["alice", alice], ["bob", bob], ["carol", carol]]) {
        try {
          await ctx.page.screenshot({ path: `${SHOTS}/${name}.png`, fullPage: true });
        } catch {
          /* the page may already be gone; the failure log is the evidence */
        }
      }
      console.log(`\npr-flows-e2e: failure screenshots written to ${SHOTS}`);
    }
    banner("the route inventory: every request these flows made");
    for (const [key, row] of [...routes.entries()].sort()) {
      console.log(`  ${key}  x${row.n}  -> ${[...row.statuses].sort().join(",")}`);
    }
    await browser.close();
  }

  if (fails > 0) {
    console.error(`\npr-flows-e2e: FAILED with ${fails} failure(s)`);
    process.exit(1);
  }
  console.log("\npr-flows-e2e: all passed");
}

/* ================= flow 1: docs/34's seed flow, conflict-free ============ */

async function flowOne(alice, bob, carol) {
  const page = alice.page;
  const flow = "flow1";

  banner("flow 1 — batch create → objects → pull request → review → merge");

  // ---- main, then the four research branches cut from main's head: the
  // "batch create" half of docs/34. Each is a product write.
  const main = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches", "POST",
      `/api/v1/projects/${PROJECT}/branches`,
      { name: "main", base_ref: "", visibility: "private" }, 201)
  ).body;

  const claim = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects`,
      { object_type: "claim", payload: claimPayload("MOF-X selectivity holds at 40% RH") }, 201)
  ).body;
  const protocol = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects`,
      { object_type: "protocol", payload: protocolPayload("annealing protocol", "300 K") }, 201)
  ).body;
  const finding = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects`,
      {
        object_type: "finding",
        payload: findingPayload("water uptake tracks the selectivity loss", claim.version_id),
      }, 201)
  ).body;

  // The fork point: the state the research branches are cut from — read
  // back from the finding's own record, never invented.
  const forkPoint = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${finding.id}`, undefined, 200)
  ).body.state_id;
  check("flow 1: the main-line objects produced the fork point the branches cut from",
    typeof forkPoint === "string" && forkPoint !== "");

  const branchNames = ["humidity-40rh", "humidity-70rh", "mechanism-water-binding", "protocol-activation-180c"];
  const branches = {};
  for (const name of branchNames) {
    const created = (
      await callOk(page, flow, "/api/v1/projects/{id}/branches", "POST",
        `/api/v1/projects/${PROJECT}/branches`,
        { name, base_ref: forkPoint, visibility: "private" }, 201)
    ).body;
    branches[name] = created;
  }
  check(`flow 1: the batch created the four research branches (${branchNames.join(", ")})`,
    branchNames.every((n) => branches[n]?.id));
  const source = branches["humidity-40rh"];

  // ---- the branch's own work: what the proposal carries.
  const branchClaim = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${source.id}/objects`,
      { object_type: "claim", payload: claimPayload("selectivity is retained at 40% RH") }, 201)
  ).body;
  await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
    `/api/v1/projects/${PROJECT}/branches/${source.id}/objects`,
    { object_type: "protocol", payload: protocolPayload("40 RH activation protocol", "320 K") }, 201);

  // ---- the research commit. The provider will not merge a ref pair with
  // nothing to merge, and the push is the transport that puts the work on
  // the ref — a browser cannot push, so the harness performs the real
  // `git push` against the real Gitea.
  await pushBranch(page, flow, source.id);

  // ---- the proposal, through the contract's own open route.
  const OPEN_KEY = "browser-flow-one-open-0001";
  const openBody = {
    source_branch_id: source.id,
    target_branch_id: main.id,
    title: "40 RH activation protocol",
    body: "docs/34 seed flow",
  };
  const opened = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests`, openBody, 201,
      { key: OPEN_KEY, label: "flow 1: POST /api/v1/projects/{id}/pull-requests = 201 [fetch]" })
  ).body;
  if (typeof opened?.number !== "number") {
    fail("flow 1: the open route must answer with the new proposal's number",
      `${JSON.stringify(opened)?.slice(0, 200)} — the rest of flow 1 cannot run`);
    return null;
  }
  const number = opened.number;
  ok("flow 1: the open route assigned a number");
  check("flow 1: the new proposal is open", opened.state === "open", `state ${opened.state}`);
  check("flow 1: it pins the fork point as its base", opened.base_state_id === forkPoint,
    `base ${short(opened.base_state_id)}, fork point ${short(forkPoint)}`);

  // The same request again with the same Idempotency-Key: the contract's
  // replay. A second proposal would mean the key was ignored.
  const replay = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests#replay", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests`, openBody, 201,
      { key: OPEN_KEY, label: "flow 1: the same request with the same Idempotency-Key = 201 [fetch]" })
  ).body;
  check("flow 1: the replayed creation returned the first proposal, not a second one",
    replay.number === number, `number ${replay.number}, first ${number}`);

  // ---- open → review_required. No product route serves this transition.
  const requested = (
    await callOk(page, flow, "/harness/pull-requests/{n}/request-review", "POST",
      `/harness/pull-requests/${number}/request-review`, { project_id: PROJECT }, 200,
      { label: "flow 1: the proposal is put in review [harness]" })
  ).body;
  check("flow 1: the proposal is in review_required",
    requested.state === "review_required", `state ${requested.state}`);

  // ---- the pull request page: the proposal as the app renders it.
  const prURL = `${BASE}/projects/${PROJECT}/pulls/${number}`;
  await page.goto(prURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-pull-state]", { timeout: 30000 });
  const rendered = await page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
  check(`flow 1: the pull request page renders #${number} in review_required [ui]`,
    rendered === "review_required", `data-pull-state=${rendered}`);
  check("flow 1: the page is the proposal's own page [ui]",
    (await page.locator("[data-pulls-detail]").first().getAttribute("data-pull-number")) === String(number));

  // ---- the three judgments, each through the real review form. Who may
  // submit which dimension is the API's routing decision, not this test's:
  // the protocol change is routed to bob's label, the claim change to
  // carol's, and the whole-proposal integrity judgment to bob.
  const submissions = [
    { ctx: bob, name: "bob", kind: "scientific", decision: "approved", text: "the protocol's numbers check out", state: "review_required" },
    { ctx: carol, name: "carol", kind: "scientific", decision: "approved", text: "the claim follows from the measurements", state: "review_required" },
    { ctx: bob, name: "bob", kind: "integrity", decision: "approved", text: "sources and pins are sound", state: "merge_ready" },
  ];
  for (const [i, s] of submissions.entries()) {
    await s.ctx.page.goto(prURL, { waitUntil: "domcontentloaded" });
    await s.ctx.page.waitForSelector("[data-review-form]", { timeout: 30000 });
    await s.ctx.page.selectOption("select[data-review-kind]", s.kind);
    await s.ctx.page.selectOption("select[data-review-decision]", s.decision);
    await s.ctx.page.fill("textarea[data-review-body]", s.text);
    await s.ctx.page.click("button[data-review-submit]");
    await s.ctx.page.waitForSelector("[data-review-saved]", { timeout: 30000 });
    hit("flow1", "POST", "/api/v1/projects/{id}/pull-requests/{n}/reviews", 201);
    ok(`flow 1: review ${i + 1} (${s.name}, ${s.kind}, ${s.decision}) recorded through the form [ui]`);

    await s.ctx.page.reload({ waitUntil: "domcontentloaded" });
    await s.ctx.page.waitForSelector("[data-pull-state]", { timeout: 30000 });
    const state = await s.ctx.page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
    hit("flow1", "GET", "/api/v1/projects/{id}/pull-requests/{n}", 200);
    check(`flow 1: after submission ${i + 1} the page shows the proposal as ${s.state} [ui]`,
      state === s.state, `data-pull-state=${state}, want ${s.state}`);
  }

  // ---- the merge. Not a UI action: the app has no merge control, so the
  // maintainer issues it against the API (the real browser still performs
  // the cross-origin write, preflight and all).
  const merged = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`, undefined, 200,
      { key: "browser-flow-one-merge-0001", label: "flow 1: POST /api/v1/projects/{id}/pull-requests/{n}:merge = 200 [fetch]",
        expect: (b) => {
          check("flow 1: the merge reports the provider step as updated, naming the commit it produced",
            b.git_state === "updated" && typeof b.git_sha === "string" && b.git_sha !== "",
            `git_state=${b.git_state} git_sha=${b.git_sha} error=${b.git_error ?? ""}`);
          check("flow 1: the merge applied the branch's two objects",
            b.applied === 2, `applied ${b.applied}`);
        } })
  ).body;

  await page.goto(prURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-pull-state]", { timeout: 30000 });
  const finalState = await page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
  check("flow 1: the pull request page renders the merged proposal [ui]",
    finalState === "merged", `data-pull-state=${finalState}`);

  // The accepted state, read back the way a reader sees it: the object the
  // merge wrote now has a version in main's history, and that version lives
  // in the state the merge named. (This build has no route that returns a
  // branch's head state; the write's own result is the evidence.)
  const accepted = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${main.id}/objects/${branchClaim.id}`, undefined, 200)
  ).body;
  check("flow 1: main's accepted state IS the state the merge committed",
    accepted.state_id === merged.state_id,
    `the branch's claim lives in ${short(accepted.state_id)}, the merge committed ${short(merged.state_id)}`);
  check("flow 1: the claim the branch proposed is what main accepted",
    accepted.payload?.statement === "selectivity is retained at 40% RH",
    `statement ${accepted.payload?.statement}`);

  return { main, source, branches, protocolID: protocol.id, protocolOnMain: protocol, number };
}

/* ================= flow 2: one scientific conflict ====================== */

async function flowTwo(alice, bob, carol, one) {
  const page = alice.page;
  const flow = "flow2";

  banner("flow 2 — a scientific conflict, refused until a human decides");

  const branchID = one.branches["protocol-activation-180c"].id;
  const protocolID = one.protocolID;
  const mainID = one.main.id;

  // ---- the researcher's commit, then the branch's edit of the SAME
  // parameter, plus a claim the merge will be able to land (a plan with
  // nothing to apply is refused, and the point here is that the CONFLICT is
  // not resolved automatically — not that nothing else may merge).
  await pushBranch(page, flow, branchID);

  const onBranch = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${branchID}/objects/${protocolID}`, undefined, 200)
  ).body;
  await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}:version", "POST",
    `/api/v1/projects/${PROJECT}/branches/${branchID}/objects/${protocolID}:version`,
    { expected_version: onBranch.current_version, patch: { parameters: { temperature: "350 K" } } }, 201,
    { label: "flow 2: the branch moves the protocol to 350 K [fetch]" });
  const appliedClaim = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${branchID}/objects`,
      { object_type: "claim", payload: claimPayload("activation at 180 C needs a re-run") }, 201)
  ).body;

  // ---- the proposal, opened and driven to merge_ready exactly as flow
  // 1's was (the review route is the one flow 1 exercised through its form;
  // here the submissions are issued from the page's context).
  const opened = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests`,
      { source_branch_id: branchID, target_branch_id: mainID, title: "180 C activation protocol", body: "T0410 flow two" },
      201, { key: "browser-flow-two-open-0001" })
  ).body;
  if (typeof opened?.number !== "number") {
    fail("flow 2: the open route must answer with the new proposal's number",
      `${JSON.stringify(opened)?.slice(0, 200)} — the rest of flow 2 cannot run`);
    return;
  }
  const number = opened.number;
  await callOk(page, flow, "/harness/pull-requests/{n}/request-review", "POST",
    `/harness/pull-requests/${number}/request-review`, { project_id: PROJECT }, 200,
    { label: "flow 2: the proposal is put in review [harness]" });
  for (const s of [
    { ctx: bob, kind: "scientific" },
    { ctx: carol, kind: "scientific" },
    { ctx: bob, kind: "integrity" },
  ]) {
    await callOk(s.ctx.page, flow, "/api/v1/projects/{id}/pull-requests/{n}/reviews", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}/reviews`,
      { kind: s.kind, decision: "approved", body: "reviewed for the conflict flow" }, 201);
  }
  const ready = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests/{n}", "GET",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}`, undefined, 200)
  ).body;
  check("flow 2: the proposal reached merge_ready", ready.state === "merge_ready", `state ${ready.state}`);

  // ---- main moves the SAME parameter to a different value: the divergence
  // a merge must not settle. Neither 350 K nor 400 K is the merge's to
  // choose, and an average is a value nobody proposed.
  await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}:version", "POST",
    `/api/v1/projects/${PROJECT}/branches/${mainID}/objects/${protocolID}:version`,
    { expected_version: onBranch.current_version + 1, patch: { parameters: { temperature: "400 K" } } }, 201,
    { label: "flow 2: main moves the same protocol to 400 K [fetch]" });

  const targetHead = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${mainID}/objects/${protocolID}`, undefined, 200)
  ).body.state_id;

  // ---- the positive control, first half: with the conflict undecided the
  // SAME merge request is refused, and nothing moves.
  const refused = await call(page, flow, "/api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
    `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`, undefined,
    { key: "browser-flow-two-merge-0001" });
  check("flow 2: the merge over an undecided scientific conflict is refused 409 MERGE_BLOCKED",
    refused.status === 409 && refused.body?.code === "MERGE_BLOCKED",
    `status ${refused.status} body ${JSON.stringify(refused.body)?.slice(0, 200)}`);
  check("flow 2: the refusal carries no merge record",
    refused.body?.state_id === undefined && refused.body?.git_sha === undefined);
  const afterRefusal = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${mainID}/objects/${protocolID}`, undefined, 200)
  ).body.state_id;
  check("flow 2: main did not move on the refused merge", afterRefusal === targetHead,
    `main ${short(afterRefusal)}, was ${short(targetHead)}`);
  const stillReady = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests/{n}", "GET",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}`, undefined, 200)
  ).body;
  check("flow 2: the proposal is still merge_ready after the refusal",
    stillReady.state === "merge_ready", `state ${stillReady.state}`);

  // ---- the conflict as the app shows it: the detector's verdict, no
  // decision yet, and the choices a human has.
  const conflictsURL =
    `${BASE}/projects/${PROJECT}/conflicts` +
    `?base_state_id=${opened.base_state_id}` +
    `&source_state_id=${opened.proposed_state_id}` +
    `&target_state_id=${targetHead}`;
  await page.goto(conflictsURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-conflicts-page]", { timeout: 30000 });
  await page.waitForSelector("[data-conflict-card]", { timeout: 30000 });
  hit("flow2", "GET", "/api/v1/projects/{id}/conflicts", 200);

  const card = page.locator(`[data-conflict-card="${protocolID}"]`).first();
  const cardCount = await page.locator("[data-conflict-card]").count();
  check("flow 2: the conflicts page reports a conflict on the contested protocol [ui]",
    (await card.count()) === 1, `${cardCount} card(s)`);
  const code = await card.getAttribute("data-conflict-code");
  const category = ((await card.locator("[data-conflict-category]").first().textContent()) ?? "").trim();
  check("flow 2: the detector classified it as a scientific field divergence [ui]",
    code === "SCIENTIFIC_FIELD_DIVERGES" && category === "scientific",
    `code ${code}, category ${category}`);
  check("flow 2: no decision is recorded yet — the machine picked no side [ui]",
    (await card.locator("[data-conflict-saved]").count()) === 0);
  check("flow 2: the human is offered the choices, keep_both among them [ui]",
    (await card.locator('input[data-resolution-kind="keep_both"]').count()) === 1);

  // ---- the human decision, through the form.
  await card.locator('input[data-resolution-kind="keep_both"]').check();
  await card.locator("textarea[data-conflict-note]").fill(
    "both readings stand until the follow-up experiment reports",
  );
  await card.locator("button[data-conflict-save]").click();
  await page.waitForSelector("[data-conflict-saved]", { timeout: 30000 });
  hit("flow2", "PUT", "/api/v1/projects/{id}/resolutions", 200);
  const savedKind = ((await card.locator("[data-conflict-saved]").first().textContent()) ?? "").trim();
  check("flow 2: the decision (keep_both) was recorded through the form [ui]",
    savedKind.includes("keep_both"), savedKind);

  // ---- the positive control, second half: the SAME merge request now
  // lands, carrying the disagreement instead of resolving it.
  const landed = (
    await callOk(page, flow, "/api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`, undefined, 200,
      { key: "browser-flow-two-merge-0002",
        label: "flow 2: the SAME merge request after the human decision = 200 [fetch]",
        expect: (b) => {
          check("flow 2: the merge carried exactly one conflict instead of resolving it",
            b.carried === 1, `carried ${b.carried}`);
          check("flow 2: it applied the one change that was safe to apply",
            b.applied === 1, `applied ${b.applied}`);
          const wroteProtocol = (b.written_versions ?? []).some((v) => v.target_id === protocolID);
          check("flow 2: it wrote no version of the contested protocol — no side won",
            !wroteProtocol, JSON.stringify(b.written_versions ?? []).slice(0, 300));
        } })
  ).body;

  const accepted = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${mainID}/objects/${protocolID}`, undefined, 200)
  ).body;
  check("flow 2: main's accepted reading of the contested parameter is still its own 400 K",
    accepted.payload?.parameters?.temperature === "400 K",
    `temperature ${accepted.payload?.parameters?.temperature}`);

  const claimInState = (
    await callOk(page, flow, "/api/v1/projects/{id}/branches/{id}/objects/{id}", "GET",
      `/api/v1/projects/${PROJECT}/branches/${mainID}/objects/${appliedClaim.id}`, undefined, 200)
  ).body;
  check("flow 2: the proposal's other work did land in the accepted state",
    claimInState.state_id === landed.state_id,
    `claim in ${short(claimInState.state_id)}, accepted ${short(landed.state_id)}`);

  // The decision is part of the record the page renders, not just of the
  // response the form read.
  await page.goto(conflictsURL, { waitUntil: "domcontentloaded" });
  await page.waitForSelector("[data-conflict-saved]", { timeout: 30000 });
  ok("flow 2: the conflicts page renders the recorded decision after a reload [ui]");
}

/* ---------- the shared transport step ---------- */

/** pushBranch performs the real `git push` through the harness endpoint,
 *  retrying while the production branch-ref syncer has not yet given the
 *  branch its ref — a wait on the stack's own tick, not a retry of a
 *  failure. */
async function pushBranch(page, flow, branchID) {
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const res = await call(page, flow, "/harness/git/push", "POST", "/harness/git/push", {
      branch_id: branchID,
    });
    if (res.status === 200) {
      ok(`${flow}: the research commit is on the provider's ref [harness]`);
      return;
    }
    if (res.status === 409 && JSON.stringify(res.body).includes("NOT_SYNCED")) {
      await sleep(500);
      continue;
    }
    fail(`${flow}: POST /harness/git/push [harness] = 200`,
      `got ${res.status}: ${JSON.stringify(res.body)}`);
    return;
  }
  fail(`${flow}: POST /harness/git/push [harness] = 200`, "the branch ref never synced");
}

main().catch((err) => {
  console.error(`pr-flows-e2e: crashed: ${err?.stack ?? err}`);
  process.exit(1);
});
