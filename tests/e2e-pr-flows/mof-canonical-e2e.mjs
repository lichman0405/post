/**
 * Gate I of docs/31_MASTER_ACCEPTANCE.md, the browser half.
 *
 * docs/31 Gate I: "完整 MOF workflow 从 Research Question 到外部项目贡献
 * Evidence 与 Profile credit，全程真实 DB/Git/blob/browser，非 mock 演示。"
 * This file is the `browser` in that sentence.
 *
 * It is the same machine as tests/e2e-pr-flows/pr-flows-e2e.mjs (real
 * Chromium, real CORS preflights, real session cookie, real CSRF token,
 * nothing intercepted) pointed at the OTHER stack: not that suite's Go
 * harness, but the gate's own `cmd/api` over the database
 * tests/acceptance/mof-canonical-workflow.sh seeded through T1201's builder,
 * with the built web app in front of it. It reuses this directory's npm
 * project (playwright 1.55.0, its own lockfile) rather than growing a second
 * one — a second harness is a second debt.
 *
 * What it adds over pr-flows-e2e.mjs is the end of the chain: the proposal it
 * drives is one the GATE opened on the SEEDED project (not one the browser
 * built from an empty fixture), so the merge it lands is a real state
 * transition on a project that already carries the demo's history — and the
 * non-member refusal it provokes is against THAT proposal.
 *
 * Every step prints `[NN]` and exactly one assertion, and a red step names
 * the step, what was expected and what arrived. Steps are numbered from
 * MOF_STEP_OFFSET so the gate's own numbering runs unbroken into this file.
 *
 * Three kinds of step, named in the log:
 *   [ui]     a real user action on a rendered page (fill / select / click)
 *   [fetch]  a request issued from the page's own context — the real
 *            browser, the real origin, the real CORS preflight, the real
 *            cookie, the session's CSRF token
 *   [git]    a real `git` invocation against the real Gitea remote. It is the
 *            one hop NO HTTP route makes: a scientist's `git push` is what
 *            puts content on a research branch, and the provider only merges
 *            a ref pair that really carries a commit. tests/e2e-pr-flows
 *            says the same thing about the same hop and reaches it through a
 *            test-owned endpoint (its harness's POST /harness/git/push, "a
 *            browser cannot run git"); this driver runs the git CLI itself
 *            instead, so the gate needs no endpoint of its own. The push is
 *            performed with the real credentials against the real
 *            repository, on a scratch clone the driver deletes afterwards.
 *
 * Usage: node mof-canonical-e2e.mjs <web-base> <context-json> <result-json>
 *   <context-json> is what the gate wrote: the API origin, the project and
 *   main branch ids, main's head state, a claim version id the finding must
 *   cite, the two accounts, and the provider repository a [git] step pushes
 *   to (its base URL, owner, name and service token).
 *   <result-json> is where the merge's own answer is written for the gate's
 *   psql-side assertions.
 */
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "";
const CTX_FILE = process.argv[3];
const RESULT_FILE = process.argv[4];
if (!BASE || !CTX_FILE || !RESULT_FILE) {
  console.error("mof-canonical-e2e: usage: node mof-canonical-e2e.mjs <web-base> <context-json> <result-json>");
  process.exit(2);
}
const CTX = JSON.parse(fs.readFileSync(CTX_FILE, "utf8"));
const API = CTX.api_base;
const PROJECT = CTX.project_id;
const MAIN = CTX.main_branch_id;

let stepNo = Number(process.env.MOF_STEP_OFFSET ?? 0);
let fails = 0;
const step = (name) => {
  stepNo += 1;
  console.log(`\n[${String(stepNo).padStart(2, "0")}] ${name}`);
  return stepNo;
};
const ok = (what) => console.log(`ok   ${what}`);
const fail = (what, detail) => {
  fails += 1;
  console.log(`FAIL ${what}${detail ? `: ${detail}` : ""}`);
};
/** One assertion, the only shape a step is allowed to end in. */
const check = (what, condition, detail) => (condition ? ok(what) : fail(what, detail));
const short = (s) => (typeof s === "string" && s.length > 8 ? s.slice(0, 8) : String(s ?? ""));

/* ---------- reading what MAIN carries --------------------------------------
 *
 * The one-object read route takes a branch id but does not use it for the
 * content: rsg.Service.GetObject checks that the branch exists (visibility)
 * and then answers the object's newest version PROJECT-WIDE
 * (internal/application/rsg/service.go: "GetLatestVersion(objectID)"). So
 * reading the gate's object "through main" says nothing about main — it
 * answers the same row through any branch of the project, before and after
 * the merge. The routes that DO answer what a branch carries are the
 * overview route (main's head state) and the query route pinned to it (the
 * as-of slice — the idiom tests/e2e-release/release-e2e.mjs uses for
 * exactly this question). Both are plain product reads, issued from the
 * page's own context like every other [fetch] step here.
 */

/** main's head state, as the overview route reports it. */
async function mainHead(page) {
  const res = await call(page, "GET /api/v1/projects/{id}/overview", "GET",
    `/api/v1/projects/${PROJECT}/overview`);
  if (res.status !== 200) throw new Error(`the overview route answered ${res.status}: ${why(res)}`);
  return res.body?.current_main?.head_state_id ?? "";
}

/* ---------- the scientist's push ------------------------------------------
 *
 * The one hop no HTTP route makes. Modelled on the git the pr-flows harness
 * runs (tests/e2e-pr-flows/harness/main.go, pushCommit), because the shape of
 * that push is the shape that works against this provider:
 *
 *   - the commit has to sit ON TOP OF main, not on an orphan history: the
 *     provider only merges a ref pair that shares an ancestor, so an
 *     unrelated root would make the merge fail at the git step with a
 *     conflict — a driver artefact masquerading as a product failure;
 *   - the credential rides GIT_CONFIG_VALUE_0/http.extraHeader, never the
 *     clone URL or a -c argument: argv is readable by every account on the
 *     machine, so a token in it is a token leaked.
 */
function gitPush(remote, branch, token, workdir, files) {
  const env = {
    ...process.env,
    HOME: workdir,
    GIT_TERMINAL_PROMPT: "0",
    GIT_CONFIG_COUNT: "1",
    GIT_CONFIG_KEY_0: "http.extraHeader",
    GIT_CONFIG_VALUE_0: `Authorization: token ${token}`,
  };
  const git = (...args) =>
    execFileSync("git", ["-C", workdir, ...args], { env, encoding: "utf8" }).trim();

  git("init", "-q", "-b", "scratch");
  git("config", "user.email", "gate@post.local");
  git("config", "user.name", "docs/31 Gate I");
  git("fetch", "--depth=1", remote, "main");
  git("checkout", "-q", "-b", branch, "FETCH_HEAD");
  for (const [name, content] of Object.entries(files)) {
    fs.writeFileSync(path.join(workdir, name), content);
  }
  git("add", "-A");
  git("commit", "-q", "-m", `research commit for ${branch} (docs/31 Gate I)`);
  const local = git("rev-parse", "HEAD");
  git("push", "-q", remote, `HEAD:refs/heads/${branch}`);
  // Read the provider back rather than trusting the push's exit status: the
  // ref is the provider's truth, and it is what the merge will merge.
  const ref = git("ls-remote", remote, `refs/heads/${branch}`).split(/\s+/)[0] ?? "";
  return { sha: local, providerSHA: ref };
}

/** The object ids the as-of slice pinned to one state carries. */
async function sliceObjectIDs(page, stateID) {
  const res = await call(page, "GET /api/v1/projects/{id}/query", "GET",
    `/api/v1/projects/${PROJECT}/query?state_id=${encodeURIComponent(stateID)}&object_type=finding`);
  if (res.status !== 200) throw new Error(`the query route answered ${res.status}: ${why(res)}`);
  return (res.body?.objects ?? []).map((o) => o.id);
}

/* ---------- one browser context per human ---------- */

async function openContext(browser, who) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const noise = { console: [], page: [], network: [], server: [] };
  page.on("console", (m) => {
    if (m.type() === "error") noise.console.push(m.text());
  });
  page.on("pageerror", (err) => noise.page.push(String(err?.message ?? err)));
  page.on("requestfailed", (req) => {
    if (!req.url().startsWith(API)) return;
    noise.network.push(`${req.method()} ${req.url()} — ${req.failure()?.errorText ?? "failed"}`);
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
  await page.fill('input[type="password"]', who.password);
  await page.click('button[type="submit"]');
  await page.waitForURL((url) => !url.pathname.startsWith("/login"), { timeout: 30000 });
  await page.waitForFunction(() => window.sessionStorage.getItem("post.csrf") !== null, null, { timeout: 30000 });
}

/** One request issued from the page's own context: the real browser, the real
 *  origin, the real preflight, the real cookie, the session's CSRF token. */
async function call(page, route, method, path, body, opts = {}) {
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
          method, headers, credentials: "include",
          body: body === undefined ? undefined : JSON.stringify(body),
        });
      } catch (err) {
        // The browser refused to make the request at all — the commonest cause
        // is CORS, which no Go client can notice.
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
  out.route = route;
  return out;
}
const why = (res) => (res.error ? `the browser refused the request: ${res.error}` : JSON.stringify(res.body)?.slice(0, 300));

/* ---------- the run ---------- */

async function main() {
  const browser = await chromium.launch();
  const owner = await openContext(browser, CTX.owner);
  const ext = await openContext(browser, CTX.external);
  const result = { steps: [] };

  try {
    /* --- the owner, through the login page ------------------------------ */
    await login(owner);
    step("the project owner signs in through the real login page [ui]");
    const csrf = await owner.page.evaluate(() => window.sessionStorage.getItem("post.csrf"));
    check("the app stored a CSRF token from the real login [ui]",
      typeof csrf === "string" && csrf.length > 0);

    // A document on the web origin before the first fetch, so every request
    // below is a real cross-origin request from the app's own origin.
    await owner.page.goto(BASE, { waitUntil: "domcontentloaded" });

    /* --- the proposal the gate's PR carries ----------------------------- */
    const branchName = `gate-canonical-${CTX.tag}`;
    const branchRes = await call(owner.page, "POST /api/v1/projects/{id}/branches", "POST",
      `/api/v1/projects/${PROJECT}/branches`,
      {
        name: branchName,
        visibility: "public",
        base_ref: CTX.main_base_state_id,
        purpose: "docs/31 Gate I: the replay PR the acceptance gate opens, reviews and merges",
      });
    step("the owner cuts a branch from main's head state [fetch]");
    check("the branch was created",
      branchRes.status === 201 && typeof branchRes.body?.id === "string" && branchRes.body.id !== "",
      `got ${branchRes.status}: ${why(branchRes)}`);
    const branchID = branchRes.body?.id;
    if (branchRes.status !== 201) throw new Error("the rest of the flow needs a branch");

    // One step, one assertion — a step that asserted two things at once would
    // name neither of them when it went red.
    step("the branch is cut from the state main is actually at [fetch]");
    const branchBase = branchRes.body?.base_state_id ?? branchRes.body?.state_id;
    check("the branch's base IS the state the gate read from main (not another branch's)",
      branchBase === CTX.main_base_state_id,
      `base ${short(branchBase)}, main's head ${short(CTX.main_base_state_id)}`);

    const objRes = await call(owner.page, "POST /api/v1/projects/{id}/branches/{id}/objects", "POST",
      `/api/v1/projects/${PROJECT}/branches/${branchID}/objects`,
      {
        object_type: "finding",
        payload: {
          // No `title`: it is a server-authoritative field, and GateMain
          // refuses a proposal that tries to carry one (authoritative_fields).
          statement: "Gate I: this finding was proposed, reviewed and accepted while the acceptance gate ran.",
          finding_type: "observation",
          // The domain rule the create route enforces: a finding pins at least
          // one claim version, taken from the seed's own finding.
          claim_version_refs: [CTX.claim_version_id],
          assessment: "preliminary",
          tags: ["synthetic", "acceptance-gate"],
          metadata: { seed_key: `gate-canonical-${CTX.tag}` },
        },
      });
    step("the branch carries one proposed object [fetch]");
    check("the object was created on the branch",
      objRes.status === 201 && typeof objRes.body?.id === "string",
      `got ${objRes.status}: ${why(objRes)}`);
    const objectID = objRes.body?.id;

    /* --- the Git half of the proposal: the scientist's push -------------- */
    const gitDir = fs.mkdtempSync(path.join(os.tmpdir(), "mof-canonical-git-"));
    let pushed = { sha: "", providerSHA: "" };
    let pushErr = "";
    try {
      pushed = gitPush(
        `${CTX.git.base.replace(/\/$/, "")}/${encodeURIComponent(CTX.git.owner)}/${encodeURIComponent(CTX.git.repo)}.git`,
        branchName, CTX.git.token, gitDir,
        {
          [`gate-canonical-${CTX.tag}.md`]: [
            "# docs/31 Gate I — the proposal's research commit",
            "",
            `The acceptance gate (tests/acceptance/mof-canonical-workflow.sh, run ${CTX.tag})`,
            "proposed one finding on this branch, and the proposal cites this commit as",
            `its work: the RSG object ${objectID} and this file are the same proposal.`,
            "",
          ].join("\n"),
        });
    } catch (err) {
      // The command line never carries the token (it rides the environment),
      // so the message is safe to print as it stands.
      pushErr = String(err?.stderr ?? err?.message ?? err).slice(0, 300);
    } finally {
      fs.rmSync(gitDir, { recursive: true, force: true });
    }
    step("the scientist pushes the branch's work to the provider [git]");
    check("the provider's ref for the branch carries the commit that was just made",
      pushErr === "" && pushed.sha !== "" && pushed.providerSHA === pushed.sha,
      `pushed ${short(pushed.sha)}, the provider's ref says ${short(pushed.providerSHA)}` +
        `${pushErr ? `: ${pushErr}` : ""}`);

    const openRes = await call(owner.page, "POST /api/v1/projects/{id}/pull-requests", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests`,
      {
        source_branch_id: branchID,
        target_branch_id: MAIN,
        title: `Gate I canonical replay ${CTX.tag}`,
        body: "Opened by tests/acceptance/mof-canonical-workflow.sh to drive review and merge in a real browser.",
      }, { key: `mof-canonical-open-${CTX.tag}` });
    step("the owner opens the proposal [fetch]");
    check("the open route returned a numbered, open proposal based on main's head",
      openRes.status === 201 && typeof openRes.body?.number === "number" &&
        openRes.body.state === "open" && openRes.body.base_state_id === CTX.main_base_state_id,
      `got ${openRes.status}: ${why(openRes)}`);
    const number = openRes.body?.number;
    if (openRes.status !== 201) throw new Error("the rest of the flow needs a proposal");

    const reqRes = await call(owner.page, "POST /api/v1/projects/{id}/pull-requests/{n}:request-review", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:request-review`, {},
      { key: `mof-canonical-review-${CTX.tag}` });
    step("the proposal is put in review [fetch]");
    check("the proposal moved to review_required",
      reqRes.status === 200 && reqRes.body?.state === "review_required",
      `got ${reqRes.status}: ${why(reqRes)}`);

    /* --- the proposal page, as the app renders it ----------------------- */
    const prURL = `${BASE}/projects/${PROJECT}/pulls/${number}`;
    await owner.page.goto(prURL, { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-pull-state]", { timeout: 30000 });
    step("the proposal page renders it [ui]");
    const rendered = await owner.page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
    const renderedNumber = await owner.page.locator("[data-pulls-detail]").first().getAttribute("data-pull-number");
    check("the page renders this proposal, in review_required [ui]",
      rendered === "review_required" && renderedNumber === String(number),
      `data-pull-state=${rendered} data-pull-number=${renderedNumber}, want review_required / ${number}`);

    /* --- the two judgments, through the real review form ---------------- */
    const submissions = [
      { kind: "scientific", decision: "approved", text: "Gate I: the proposed finding follows from the seeded measurements.", state: "review_required" },
      { kind: "integrity", decision: "approved", text: "Gate I: the proposal's pins and provenance are sound.", state: "merge_ready" },
    ];
    for (const [i, s] of submissions.entries()) {
      await owner.page.goto(prURL, { waitUntil: "domcontentloaded" });
      await owner.page.waitForSelector("[data-review-form]", { timeout: 30000 });
      await owner.page.selectOption("select[data-review-kind]", s.kind);
      await owner.page.selectOption("select[data-review-decision]", s.decision);
      await owner.page.fill("textarea[data-review-body]", s.text);
      await owner.page.click("button[data-review-submit]");
      await owner.page.waitForSelector("[data-review-saved]", { timeout: 30000 });
      step(`review ${i + 1} (${s.kind}, ${s.decision}) is recorded through the app's own form [ui]`);
      await owner.page.reload({ waitUntil: "domcontentloaded" });
      await owner.page.waitForSelector("[data-pull-state]", { timeout: 30000 });
      const state = await owner.page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
      check(`after review ${i + 1} the page renders the proposal as ${s.state} [ui]`,
        state === s.state, `data-pull-state=${state}, want ${s.state}`);
    }

    /* --- what main carries, before anything merges ----------------------- */
    const headBefore = await mainHead(owner.page);
    step("the overview route names main's head state [fetch]");
    check("main's head is the state the gate read from the database before the browser ran",
      headBefore !== "" && headBefore === CTX.main_base_state_id,
      `overview says ${short(headBefore)}, the gate seeded ${short(CTX.main_base_state_id)}`);

    const inMainBefore = await sliceObjectIDs(owner.page, headBefore);
    step("the proposal's object is not part of main yet [fetch]");
    check("main's slice does not carry the proposed object",
      inMainBefore.length > 0 && !inMainBefore.includes(objectID),
      `${inMainBefore.length} finding(s) in main's state, proposed object present=${inMainBefore.includes(objectID)}`);

    /* --- the negative control: someone who is not a member --------------- */
    //
    // The external group forks the project; it does not join it. docs/12's
    // merge authority is the target branch's, so the SAME request the owner
    // is about to make is refused for them — and this runs BEFORE the
    // owner's, so "it was already merged" cannot be the reason.
    await login(ext);
    step("the external contributor signs in through the real login page [ui]");
    const extCsrf = await ext.page.evaluate(() => window.sessionStorage.getItem("post.csrf"));
    check("the external session is its own, with its own CSRF token [ui]",
      typeof extCsrf === "string" && extCsrf.length > 0 && extCsrf !== csrf);
    await ext.page.goto(BASE, { waitUntil: "domcontentloaded" });

    const refused = await call(ext.page, "POST /api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`, { message: "not mine to merge" },
      { key: `mof-canonical-nonmember-${CTX.tag}` });
    step("NEGATIVE: a non-member's merge of the same proposal is refused [fetch]");
    check("the server refused it 403 AUTH_FORBIDDEN and reported no merge",
      refused.status === 403 && refused.body?.code === "AUTH_FORBIDDEN" &&
        refused.body?.state_id === undefined && refused.body?.git_sha === undefined,
      `got ${refused.status}: ${why(refused)}`);

    const stillReady = await call(ext.page, "GET /api/v1/projects/{id}/pull-requests/{n}", "GET",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}`);
    step("NEGATIVE: the refused merge left the proposal as it was [fetch]");
    check("the proposal is still merge_ready",
      stillReady.status === 200 && stillReady.body?.state === "merge_ready",
      `got ${stillReady.status} state ${stillReady.body?.state}`);

    const headAfterRefusal = await mainHead(ext.page);
    step("NEGATIVE: the refused merge moved main not at all [fetch]");
    check("main's head state is the one it had before the refusal",
      headAfterRefusal !== "" && headAfterRefusal === headBefore,
      `main is at ${short(headAfterRefusal)}, was ${short(headBefore)}`);

    /* --- the merge, by the owner ---------------------------------------- */
    //
    // The merge has two halves and they are deliberately not one: the state
    // transition is committed to PostgreSQL, and the provider-side pull
    // request is created and merged in Gitea. The assertion below demands
    // BOTH, so a merge that commits the database truth and fails its Git
    // half is a red step here — which is exactly what this step did before
    // the [git] push above existed, and the reason that push is in the flow.
    const merged = await call(owner.page, "POST /api/v1/projects/{id}/pull-requests/{n}:merge", "POST",
      `/api/v1/projects/${PROJECT}/pull-requests/${number}:merge`,
      { message: "docs/31 Gate I: the canonical replay lands" },
      { key: `mof-canonical-merge-${CTX.tag}` });
    step("the owner merges the proposal [fetch]");
    check("the merge committed the state AND advanced the provider's ref",
      merged.status === 200 &&
        typeof merged.body?.state_id === "string" && merged.body.state_id !== "" &&
        merged.body?.git_state === "updated" &&
        typeof merged.body?.git_sha === "string" && merged.body.git_sha !== "",
      `got ${merged.status} git_state=${merged.body?.git_state ?? "-"}` +
        `${merged.body?.git_error ? ` git_error=${merged.body.git_error}` : ""}: ${why(merged)}`);
    result.merge = merged.body ?? {};
    result.branch_id = branchID;
    result.branch_name = branchName;
    result.object_id = objectID;
    result.pr_number = number;
    // The commit the [git] step pushed. The gate compares it with what the
    // provider's own refs say after the merge, so "the Git half landed" is
    // read from Gitea rather than from the merge's own answer.
    result.pushed_sha = pushed.sha;

    await owner.page.goto(prURL, { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-pull-state]", { timeout: 30000 });
    step("the proposal page renders the merge [ui]");
    const finalState = await owner.page.locator("[data-pull-state]").first().getAttribute("data-pull-state");
    check("the page renders the proposal as merged [ui]", finalState === "merged", `data-pull-state=${finalState}`);

    const headAfter = await mainHead(owner.page);
    step("main's head is the state the merge named [fetch]");
    check("acceptance is a state transition on the target branch, not a flag on the proposal",
      headAfter !== "" && headAfter === result.merge.state_id && headAfter !== headBefore,
      `main is at ${short(headAfter)}, the merge wrote ${short(result.merge.state_id)}, was ${short(headBefore)}`);

    const inMainAfter = await sliceObjectIDs(owner.page, headAfter);
    step("the accepted version is what main now carries [fetch]");
    check("main's slice carries the object the gate proposed",
      inMainAfter.includes(objectID),
      `${inMainAfter.length} finding(s) in main's state, proposed object present=${inMainAfter.includes(objectID)}`);

    /* --- what the browser itself saw ------------------------------------ */
    step("the browser logged nothing it should not have [ui]");
    // The flows provoke exactly one browser-visible error on purpose — the
    // external context's refused merge is a 403 the browser logs like any
    // other failed resource. Any OTHER console error, exception, request the
    // API never received, or 5xx is a failure.
    const noisy = (m) => !/40[0-9] \(/.test(m);
    const unexpected = [
      ...owner.noise.console.filter(noisy).map((m) => `console: ${m}`),
      ...ext.noise.console.filter(noisy).map((m) => `external console: ${m}`),
      ...owner.noise.page.map((m) => `pageerror: ${m}`),
      ...owner.noise.network.map((m) => `requestfailed: ${m}`),
      ...owner.noise.server.map((m) => `5xx: ${m}`),
      ...ext.noise.page.map((m) => `external pageerror: ${m}`),
      ...ext.noise.network.map((m) => `external requestfailed: ${m}`),
      ...ext.noise.server.map((m) => `external 5xx: ${m}`),
    ];
    check("no uncaught exception, no failed API request, no 5xx — the provoked 403s aside",
      unexpected.length === 0, unexpected.slice(0, 3).join(" | "));
  } finally {
    // The last step number this file reached. The gate continues its own
    // numbering from here, so no two steps in the log share a number.
    result.last_step = stepNo;
    fs.writeFileSync(RESULT_FILE, JSON.stringify(result, null, 2));
    await browser.close();
  }

  if (fails > 0) {
    console.error(`\nmof-canonical-e2e: FAILED with ${fails} failure(s)`);
    process.exit(1);
  }
  console.log("\nmof-canonical-e2e: all passed");
}

main().catch((err) => {
  console.error(`mof-canonical-e2e: crashed: ${err?.stack ?? err}`);
  process.exit(1);
});
