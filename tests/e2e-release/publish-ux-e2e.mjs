/**
 * T1103 required test — label: `publish ux e2e`.
 *
 * The publish/visibility confirmation page, driven in real Chromium against
 * the real stack. The harness has already run release-e2e and written the
 * chain state (RELEASE_CHAIN_OUT); this script uses that release and object
 * version to exercise the product's publish confirmation UI.
 *
 * Every assertion that matters is made through the browser: the page renders
 * the six-category impact list docs/06 §8 requires, the confirm action
 * succeeds only when the preview is publishable, a blocking private
 * dependency and a broken provenance pin both make the confirm path
 * unreachable, and a non-owner or anonymous actor is refused by the API
 * itself — not merely by a hidden button.
 *
 * Two kinds of assertion are made in both directions, because one direction
 * of each is satisfiable by a page that does nothing:
 *
 *   - the entry point exists BECAUSE the document the page rebuilds hashes to
 *     the digest the stored version declares (the same document, rebuilt,
 *     offers the confirmation), and is WITHHELD when the rebuild is narrower
 *     than what was stored (a pin the page may not render). The fixture for
 *     the second is published for real, so the stored row can be read back
 *     and the page's omission named as an omission;
 *   - a state that moved between the preview and the click is refused by the
 *     server-side re-check, and the page renders that refusal's own report.
 *
 * Nothing here is a mock: the browser talks to the real API over real CORS,
 * real session cookies and real CSRF tokens, and the refusals asserted below
 * are the API's own answers. The /harness routes are the suite's own
 * instruments — fixtures and read-only row snapshots, never product
 * surfaces: reads are made from Node (no session needed), and a fixture that
 * STORES a row is made from the signed-in page like every other write, so it
 * passes the same guard. No fixture decides anything about a publication;
 * the product's own preview, gate and publish route decide all of that.
 */
import fs from "node:fs";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31170";
const READY_FILE = process.argv[3];
const CHAIN_FILE = process.argv[4];
if (!READY_FILE || !CHAIN_FILE) {
  console.error(
    "publish-ux-e2e: usage: node publish-ux-e2e.mjs <web-base> <ready-json-file> <chain-json-file>",
  );
  process.exit(2);
}
const READY = JSON.parse(fs.readFileSync(READY_FILE, "utf8"));
const CHAIN = JSON.parse(fs.readFileSync(CHAIN_FILE, "utf8"));
const API = READY.api_base;
const PROJECT = READY.project_id;
const PASSWORD = READY.password;

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

async function openContext(browser, who) {
  const context = await browser.newContext();
  const page = await context.newPage();
  const noise = { console: [], page: [], network: [], server: [] };
  page.on("console", (msg) => {
    if (msg.type() === "error") noise.console.push(msg.text());
  });
  page.on("pageerror", (err) => noise.page.push(String(err?.message ?? err)));
  page.on("requestfailed", (req) => {
    if (!req.url().startsWith(API)) return;
    const failure = req.failure();
    noise.network.push(`${req.method()} ${req.url()} — ${failure?.errorText ?? "failed"}`);
  });
  page.on("response", (res) => {
    if (!res.url().startsWith(API)) return;
    if (res.status() >= 500) noise.server.push(`${res.status()} ${res.request().method()} ${res.url()}`);
  });
  return { context, page, who, noise, label: who?.handle ?? "anonymous" };
}

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

/** One API call made from inside the browser's own session. */
async function call(page, method, path, body, opts = {}) {
  return page.evaluate(
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
}

async function callOk(page, method, path, body, want, opts = {}) {
  const res = await call(page, method, path, body, opts);
  const label = opts.label ?? `${method} ${path} = ${want}`;
  if (res.status !== want) {
    const why = res.error ? `browser refused: ${res.error}` : JSON.stringify(res.body)?.slice(0, 400);
    fail(label, `got ${res.status}: ${why}`);
  } else {
    ok(label);
  }
  return res;
}

/**
 * One call to a /harness route, from Node rather than from the browser.
 *
 * The harness routes are the suite's own instruments — fixtures and
 * read-only row snapshots — and are never product surfaces. Calling them
 * from Node keeps them out of the page's origin: a page fetch would be a
 * cross-origin request the product's CORS policy has no reason to allow.
 */
async function harness(method, path, body) {
  const res = await fetch(API + path, {
    method,
    headers: body === undefined ? {} : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await res.text();
  let parsed = null;
  try {
    parsed = text === "" ? null : JSON.parse(text);
  } catch {
    parsed = text;
  }
  return { status: res.status, body: parsed, text };
}

/**
 * A harness fixture that WRITES, made from the browser's own session.
 *
 * The harness's write routes sit behind the same guard the API's do
 * (authhttp's session + CSRF check wraps write methods), so a fixture that
 * stores a row is made the way any other write in this suite is: from the
 * signed-in page, over real CORS, with the real CSRF token. Reads need no
 * session, which is why `harness` above reaches them from Node.
 */
async function harnessWrite(page, method, path, body) {
  const res = await call(page, method, path, body);
  return { status: res.status, body: res.body, text: JSON.stringify(res.body ?? res.error) };
}

function encodeCandidate(candidate) {
  return Buffer.from(JSON.stringify(candidate), "utf8").toString("base64url");
}

function decodeCandidate(param) {
  return JSON.parse(Buffer.from(param, "base64url").toString("utf8"));
}

function confirmPageUrl(candidate) {
  return confirmPageUrlFor(PROJECT, candidate);
}

function confirmPageUrlFor(projectId, candidate) {
  return `${BASE}/projects/${projectId}/assets/publish?candidate=${encodeCandidate(candidate)}`;
}

/**
 * The harness's own candidate document, authored through the ONE route that
 * computes it with the production model (internal/assets.Manifest), so the
 * integrity hash a fixture carries is the digest of the document it carries.
 *
 * `params` are the harness route's optional authoring knobs (version,
 * visibility, title, slug, dependency_pins, blob_ids, metadata_pad); the
 * required three identify the release and object version the candidate pins.
 */
async function harnessCandidate(params = {}) {
  const query = new URLSearchParams({
    project_id: PROJECT,
    release_id: CHAIN.releaseID,
    object_version_id: CHAIN.acceptedVersionID,
    ...params,
  });
  const res = await harness("GET", `/harness/asset-candidate?${query}`);
  if (res.status !== 200) {
    fail("the harness authored a publish candidate [harness]", `got ${res.status}: ${res.text.slice(0, 200)}`);
    return null;
  }
  return res.body;
}

/**
 * The same fixture document, published as a new VERSION of an existing
 * asset.
 *
 * The publish route creates an asset when the candidate names no pid, and
 * `title`/`slug` are exactly the display fields of an asset it creates
 * ("required when the request names no asset, and refused when it does" —
 * cmd/api/assetshttp/publish.go). Naming a pid is also what makes the
 * PREVIEW a statement about this publication: a candidate with an empty pid
 * is previewed as `ASSET_INVALID_PID`, because a preview cannot know that
 * the publish would mint one.
 */
function newVersionOf(candidate, pid) {
  const c = JSON.parse(JSON.stringify(candidate));
  c.asset_pid = pid;
  delete c.title;
  delete c.slug;
  return c;
}

/** The stored row of one asset version, verbatim (the row's own JSON text). */
async function storedVersionRow(pid, version) {
  const res = await harness(
    "GET",
    `/harness/rows?asset_pid=${encodeURIComponent(pid)}&asset_version=${encodeURIComponent(version)}`,
  );
  const text = res.body?.asset_version;
  return typeof text === "string" && text !== "" ? JSON.parse(text) : null;
}

/** The candidate the confirmation page is currently showing. */
function candidateFromPage(page) {
  const param = new URL(page.url()).searchParams.get("candidate");
  return param ? decodeCandidate(param) : null;
}

let versionCounter = 0;
/**
 * A fresh version label per candidate: the publish stores versions under
 * (asset, version), so a candidate reused verbatim is a taken label.
 */
function freshVersion() {
  versionCounter += 1;
  return `1.0.${Math.floor(Date.now() / 1000)}${versionCounter}`;
}

/**
 * The candidate as the publish route accepts it, with a version label no
 * earlier leg has taken.
 *
 * The publish takes title/slug ONLY when it creates the asset and refuses
 * them when the candidate names one ("required when the request names no
 * asset, and refused when it does" — cmd/api/assetshttp/publish.go), so the
 * display fields follow `asset_pid` rather than being set unconditionally.
 */
function freshCandidate(base) {
  const c = JSON.parse(JSON.stringify(base));
  c.version = freshVersion();
  if (c.asset_pid) {
    delete c.title;
    delete c.slug;
  } else {
    c.title = c.title ?? "Publish UX candidate";
    c.slug = `publish-ux-${Date.now()}-${versionCounter}`;
  }
  return c;
}

/** The assets page URL the product lands on after a publish. */
function assetURL(pid, version) {
  return `${BASE}/assets/${encodeURIComponent(pid)}/${encodeURIComponent(version)}`;
}

async function main() {
  const browser = await chromium.launch();
  const owner = await openContext(browser, USERS["release-owner"]);
  const maintainer = await openContext(browser, USERS["release-reviewer-a"]);
  const anon = await openContext(browser, null);

  try {
    banner("sign in owner and maintainer through the real login page");
    await login(owner);
    await login(maintainer);

    /* ------------------------------------------------------------------ *
     * 1. Reaching the confirmation PAGE from the product, not a modal.
     * ------------------------------------------------------------------ */
    banner("owner opens the asset version and reaches the confirmation page");
    // The product's way in is the asset version page. It is the only surface
    // that holds an asset's pid, and a candidate the preview route accepts
    // must name one: the preview handler mints no pid (the publish command
    // does), so a candidate without one is refused before anything is
    // previewed. Nothing here is a guess about what the page will show —
    // the two facts the page needs are asserted over the API first, so a
    // missing fact is named here instead of being blamed on the page.
    const chainPage = await call(
      owner.page,
      "GET",
      `/api/v1/assets/${encodeURIComponent(CHAIN.assetPID)}?version=${encodeURIComponent(
        CHAIN.assetVersion,
      )}`,
    );
    check(
      "the owner can read the version the chain published [fetch]",
      chainPage.status === 200 && chainPage.body?.asset?.pid === CHAIN.assetPID,
      `status ${chainPage.status}, body = ${JSON.stringify(chainPage.body).slice(0, 200)}`,
    );
    check(
      "the published version carries the release it came from [fetch]",
      (chainPage.body?.origin ?? []).some((o) => o.ref === `release:${CHAIN.releaseID}`),
      `origin = ${JSON.stringify(chainPage.body?.origin)}`,
    );

    await owner.page.goto(assetURL(CHAIN.assetPID, CHAIN.assetVersion), {
      waitUntil: "domcontentloaded",
    });
    await owner.page.waitForSelector("[data-asset-publish]", { timeout: 30000 });
    check(
      "the asset version offers a publish entry point [ui]",
      (await owner.page.locator("[data-asset-publish]").count()) === 1,
    );

    const entryHref = await owner.page.getAttribute("[data-asset-publish]", "href");
    check(
      "the entry point is a link to a page of its own, not a dialog [ui]",
      typeof entryHref === "string" &&
        entryHref.startsWith(`/projects/${PROJECT}/assets/publish?candidate=`),
      String(entryHref).slice(0, 100),
    );

    await owner.page.click("[data-asset-publish]");
    await owner.page.waitForURL((url) => url.pathname.endsWith("/assets/publish"), {
      timeout: 30000,
    });
    check(
      "the publish action navigates to a page of its own, with its own URL [ui]",
      new URL(owner.page.url()).pathname === `/projects/${PROJECT}/assets/publish`,
      owner.page.url(),
    );
    await owner.page.waitForSelector("[data-publish-section='summary']", { timeout: 30000 });

    // "A page of its own, back-navigable, directly addressable": the browser
    // has to be able to leave it and return to it, with the same candidate
    // still in the address. A dialog cannot do this, which is why the check
    // is the browser's own history and not the presence of a close button.
    const baseCandidateParam = new URL(owner.page.url()).searchParams.get("candidate");
    check(
      "the confirmation page links back to the asset version it is about [ui]",
      (await owner.page.getAttribute("[data-publish-back-asset]", "href")) ===
        `/assets/${CHAIN.assetPID}`,
      String(await owner.page.getAttribute("[data-publish-back-asset]", "href")),
    );
    await owner.page.goBack({ waitUntil: "domcontentloaded" });
    check(
      "the browser's Back button returns to the asset version [ui]",
      new URL(owner.page.url()).pathname ===
        `/assets/${CHAIN.assetPID}/${CHAIN.assetVersion}`,
      owner.page.url(),
    );
    await owner.page.goForward({ waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-section='summary']", { timeout: 30000 });
    check(
      "Forward returns to the confirmation page, still carrying its candidate [ui]",
      new URL(owner.page.url()).searchParams.get("candidate") === baseCandidateParam &&
        baseCandidateParam !== null,
      owner.page.url(),
    );

    const baseCandidate = candidateFromPage(owner.page);
    check(
      "the confirmation page loaded with a publish candidate [ui]",
      baseCandidate !== null,
      "no candidate in the URL",
    );
    const baseObjectRef = (baseCandidate?.origin_refs ?? []).find((r) =>
      String(r).startsWith("object_version:"),
    );
    check(
      "the candidate pins this release and the released object version [ui]",
      Array.isArray(baseCandidate?.origin_refs) &&
        baseCandidate.origin_refs.includes(`release:${CHAIN.releaseID}`) &&
        baseCandidate.origin_refs.includes(`project:${PROJECT}`) &&
        typeof baseObjectRef === "string",
      JSON.stringify(baseCandidate?.origin_refs),
    );
    check(
      "the candidate names the asset whose version the reader was on [ui]",
      baseCandidate?.asset_pid === CHAIN.assetPID,
      `asset_pid = ${JSON.stringify(baseCandidate?.asset_pid)}, expected ${CHAIN.assetPID}`,
    );
    check(
      "the candidate would publish the next version of that asset [ui]",
      baseCandidate?.version === `${CHAIN.assetVersion}-public`,
      `version = ${JSON.stringify(baseCandidate?.version)}`,
    );

    /* ------------------------------------------------------------------ *
     * 2. The six categories docs/06 §8 names, each locatable, and the
     *    impact list carrying NAMES rather than counts.
     * ------------------------------------------------------------------ */
    banner("the confirmation page renders the six impact categories");
    for (const [category, section] of [
      ["objects that would become public", "objects"],
      ["metadata that would become public", "metadata"],
      ["blob access", "blobs"],
      ["dependencies and origin refs", "dependencies"],
      ["rights / license", "rights"],
      ["private evidence / attestation", "attestation"],
    ]) {
      check(
        `the page has a ${category} block [ui]`,
        (await owner.page.locator(`[data-publish-section='${section}']`).count()) === 1,
      );
    }

    const pageText = await owner.page.locator("[data-publish-confirm-page]").innerText();
    check(
      "the page names the concrete object version it would publish, not a count [ui]",
      pageText.includes(String(baseObjectRef).slice("object_version:".length)),
      `the page text does not name ${baseObjectRef}`,
    );
    check(
      "the page names the concrete release ref it would publish, not a count [ui]",
      pageText.includes(`release:${CHAIN.releaseID}`),
      "the release origin ref is absent from the page text",
    );

    // The sixth category is a statement about the platform, not a list of
    // findings: there is no attestation feature to enumerate
    // (internal/application/researchprofile/doc.go). An empty list would read
    // as "checked, found none", which this page cannot claim.
    const attestation = owner.page.locator("[data-publish-section='attestation']");
    check(
      "the attestation category renders a status, not a list [ui]",
      (await attestation.locator("ul, ol, li").count()) === 0,
      "the attestation section renders list elements",
    );
    check(
      "the attestation status says the platform has no such feature [ui]",
      /no private-evidence \/ attestation feature/i.test(await attestation.innerText()),
      await attestation.innerText(),
    );

    /* ------------------------------------------------------------------ *
     * 3. The happy path: confirm here, and the version is published.
     * ------------------------------------------------------------------ */
    banner("owner confirms and the asset is published through the page");
    const positive = freshCandidate(baseCandidate);
    const positivePreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      positive,
    );
    check(
      "the API preview says the positive candidate is publishable [fetch]",
      positivePreview.status === 200 && positivePreview.body?.publishable === true,
      `status ${positivePreview.status}, publishable ${positivePreview.body?.publishable}, blockers ${JSON.stringify(
        positivePreview.body?.publish_blockers,
      )?.slice(0, 300)}`,
    );

    await owner.page.goto(confirmPageUrl(positive), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-confirm]", { timeout: 30000 });
    await owner.page.click("[data-publish-confirm]");
    await owner.page.waitForURL((url) => url.pathname.startsWith("/assets/"), { timeout: 30000 });
    const landings = new URL(owner.page.url()).pathname.split("/").filter(Boolean);
    check(
      "the publish succeeded and the browser landed on the published version [ui]",
      landings.length === 3 && landings[0] === "assets",
      `ended at ${owner.page.url()}`,
    );
    const publishedPID = landings[1];
    const publishedVersion = landings[2];

    /* ------------------------------------------------------------------ *
     * 3b. The positive direction of the integrity check: the entry point
     *     exists because the document the page can rebuild hashes to the
     *     digest the stored version declares.
     *
     *     Both legs check the same thing over two versions whose documents
     *     differ (a private asset version and the public one the page just
     *     published), and both compare the page's rendering AND the link's
     *     own candidate against the ROW — not against the page, which would
     *     only prove the page agrees with itself.
     * ------------------------------------------------------------------ */
    banner("the entry link is offered where the rebuilt document hashes to the stored digest");
    for (const [what, pid, version, alreadyPublic] of [
      ["the chain's own version", CHAIN.assetPID, CHAIN.assetVersion, false],
      ["the version this page published", publishedPID, publishedVersion, true],
    ]) {
      const row = await storedVersionRow(pid, version);
      check(
        `the stored row of ${what} declares an integrity hash [harness]`,
        typeof row?.integrity_hash === "string" && row.integrity_hash.length === 64,
        `row = ${JSON.stringify(row?.integrity_hash)}`,
      );

      await owner.page.goto(assetURL(pid, version), { waitUntil: "domcontentloaded" });
      await owner.page.waitForSelector("[data-asset-publish]", { timeout: 30000 });
      const renderedHash = (await owner.page.locator("[data-asset-hash]").innerText()).trim();
      check(
        `the page of ${what} states the digest its own row stores [ui]`,
        renderedHash === row?.integrity_hash,
        `page says ${renderedHash}, the row says ${row?.integrity_hash}`,
      );

      const href = await owner.page.getAttribute("[data-asset-publish]", "href");
      const candidate = decodeCandidate(new URL(href, BASE).searchParams.get("candidate"));
      check(
        `the entry link of ${what} is offered because its document hashes to that digest [ui]`,
        candidate.integrity_hash === row?.integrity_hash,
        `candidate hash ${candidate.integrity_hash}, row hash ${row?.integrity_hash}`,
      );
      check(
        `the entry link of ${what} publishes a new PUBLIC version of the same asset [ui]`,
        candidate.asset_pid === pid && candidate.visibility === "public",
        `asset_pid ${candidate.asset_pid} (want ${pid}), visibility ${candidate.visibility}`,
      );
      // The wording follows what the version is: a version that is already
      // public is offered a NEW public version, not told it is about to
      // become public (which would be false for it).
      const linkText = (await owner.page.locator("[data-asset-publish]").innerText()).trim();
      check(
        `the entry link of ${what} says what it would do (${alreadyPublic ? "already public" : "private"}) [ui]`,
        alreadyPublic
          ? /new public version/i.test(linkText)
          : /^Publish this version publicly$/.test(linkText),
        `link text: ${linkText}`,
      );
    }

    /* ------------------------------------------------------------------ *
     * 4. A blocker is hard: the confirm path does not exist, and the API
     *    refuses the very same candidate.
     * ------------------------------------------------------------------ */
    banner("a broken provenance pin makes the confirm path unreachable");
    const brokenPin = freshCandidate(baseCandidate);
    brokenPin.origin_refs = [
      ...(brokenPin.origin_refs ?? []),
      "object_version:00000000-0000-0000-0000-000000000000",
    ];
    const brokenPreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      brokenPin,
    );
    check(
      "the API preview refuses the broken-pin candidate [fetch]",
      brokenPreview.status === 200 && brokenPreview.body?.publishable === false,
      `status ${brokenPreview.status}, publishable ${brokenPreview.body?.publishable}`,
    );
    await owner.page.goto(confirmPageUrl(brokenPin), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-section='blockers']", { timeout: 30000 });
    check(
      "a broken pin shows the blockers section [ui]",
      (await owner.page.locator("[data-publish-section='blockers']").count()) === 1,
    );
    check(
      "a broken pin leaves no confirm action on the page at all [ui]",
      (await owner.page.locator("[data-publish-confirm]").count()) === 0,
      "a confirm control was rendered for a refused publication",
    );
    const brokenPublish = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      brokenPin,
      { key: `publish-ux-broken-${Date.now()}` },
    );
    check(
      "the API refuses the broken-pin candidate with ASSET_PUBLISH_BLOCKED [fetch]",
      brokenPublish.status === 409 && brokenPublish.body?.code === "ASSET_PUBLISH_BLOCKED",
      `got ${brokenPublish.status}: ${JSON.stringify(brokenPublish.body)?.slice(0, 200)}`,
    );

    /* ------------------------------------------------------------------ *
     * 5. A blocking PRIVATE DEPENDENCY: the case docs/23 §4 exists for.
     *    The rights declaration claims open data access for a blob that is
     *    not open (internal/assets/preview.go: a blob blocks exactly there),
     *    which is a private dependency — not a candidate-check failure and
     *    not a rights blocker — so it must be named as one.
     * ------------------------------------------------------------------ */
    banner("a blocking private dependency makes the confirm path unreachable");
    const blockingDep = freshCandidate(baseCandidate);
    blockingDep.rights = JSON.parse(JSON.stringify(blockingDep.rights ?? {}));
    blockingDep.rights.visibility = {
      ...(blockingDep.rights.visibility ?? {}),
      data_access: "open",
    };
    const depPreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      blockingDep,
    );
    const blockingDeps = (depPreview.body?.private_dependencies ?? []).filter((d) => d.blocking);
    check(
      "the API preview reports a blocking private dependency [fetch]",
      depPreview.status === 200 && blockingDeps.length > 0,
      `status ${depPreview.status}, private_dependencies ${JSON.stringify(
        depPreview.body?.private_dependencies,
      )?.slice(0, 300)}`,
    );
    check(
      "the blocking private dependency is a blob, not a candidate check [fetch]",
      blockingDeps.some((d) => d.kind === "blob"),
      JSON.stringify(blockingDeps),
    );

    await owner.page.goto(confirmPageUrl(blockingDep), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-section='blockers']", { timeout: 30000 });
    const blockerKinds = await owner.page
      .locator("[data-publish-blocker-kind]")
      .evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-publish-blocker-kind")));
    check(
      "the page names the blocking private dependency as such [ui]",
      blockerKinds.includes("private_dependency"),
      JSON.stringify(blockerKinds),
    );
    const namedBlockerRefs = await owner.page
      .locator("[data-publish-blocker-kind='private_dependency']")
      .evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-publish-blocker-ref")));
    check(
      "the page names the concrete blob that blocks, not just a count [ui]",
      namedBlockerRefs.length > 0 && namedBlockerRefs.every((r) => typeof r === "string" && r.length > 0),
      JSON.stringify(namedBlockerRefs),
    );
    check(
      "a blocking private dependency leaves no confirm action on the page [ui]",
      (await owner.page.locator("[data-publish-confirm]").count()) === 0,
      "a confirm control was rendered for a publication refused over a private dependency",
    );
    const depPublish = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      blockingDep,
      { key: `publish-ux-dep-${Date.now()}` },
    );
    check(
      "the API refuses the blocking-private-dependency candidate with ASSET_PUBLISH_BLOCKED [fetch]",
      depPublish.status === 409 && depPublish.body?.code === "ASSET_PUBLISH_BLOCKED",
      `got ${depPublish.status}: ${JSON.stringify(depPublish.body)?.slice(0, 200)}`,
    );

    /* ------------------------------------------------------------------ *
     * 5b. The rebuild check, BOTH directions, with one dependency pin as
     *     the only difference between the two fixtures.
     *
     *     A pin is rendered on a version page only when the pinned version
     *     and its project are both public (internal/assets mayLinkVersion,
     *     page.go), while the manifest the row stores keeps every pin. So
     *     "the page renders fewer pins than the document declares" is a
     *     reachable state, and this leg reaches it for real:
     *
     *       - it first creates a PUBLIC project and publishes one public
     *         version into it, purely as something a pin may link to;
     *       - then it publishes a version that pins THAT version (rendered,
     *         so the rebuilt document still equals the stored one — the
     *         entry point is offered) and a version that pins one of the
     *         harness project's own private versions (dropped, so the
     *         rebuild is narrower — no entry point, and the page says what
     *         differed).
     *
     *     Each direction is checked against the stored ROW as well as the
     *     page, so the page is never its own witness.
     * ------------------------------------------------------------------ */
    banner("a pin the page can render, and one it cannot: the rebuild check in both directions");

    const publicProject = await call(owner.page, "POST", "/api/v1/projects", {
      slug: `publish-ux-public-${Date.now()}`,
      name: "Publish UX public project",
      purpose: "hold one public asset version a dependency pin may link to (T1103 e2e fixture)",
      visibility: "public",
      organization_id: READY.org_id,
    });
    check(
      "the owner created a PUBLIC project through the API [fetch]",
      publicProject.status === 201 && typeof publicProject.body?.project?.id === "string",
      `got ${publicProject.status}: ${JSON.stringify(publicProject.body)?.slice(0, 300)}`,
    );
    const publicProjectID = publicProject.body?.project?.id ?? "";
    check(
      "the new project really is public [fetch]",
      publicProject.body?.project?.visibility === "public",
      `visibility = ${JSON.stringify(publicProject.body?.project?.visibility)}`,
    );

    // The version a pin may link to. Its provenance is a state IN the public
    // project: a publication must pin a release or a state
    // (ASSET_UNPINNED_SOURCE, internal/assets validateProvenance — a project
    // ref is not enough, and this leg learned that from the API's own
    // refusal), and a ref into the harness project's private state would be
    // a private dependency that blocks a public publication
    // (internal/assets refDependency). The integrity hash covers the
    // manifest, which does not carry the refs.
    const publicState = await harnessWrite(owner.page, "POST", "/harness/state", {
      project_id: publicProjectID,
    });
    check(
      "the harness stored a state in the public project, and named its ref [harness]",
      publicState.status === 201 && /^state:[0-9a-f-]{36}$/.test(publicState.body?.ref ?? ""),
      `got ${publicState.status}: ${publicState.text.slice(0, 200)}`,
    );
    const publicSkeleton = await harnessCandidate({
      visibility: "public",
      version: freshVersion(),
      title: "A public version another version may pin",
      slug: `publish-ux-pinnable-${Date.now()}`,
    });
    publicSkeleton.origin_refs = [publicState.body?.ref];
    const publicVersion = await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${publicProjectID}/assets:publish`,
      publicSkeleton,
      201,
      {
        key: `publish-ux-public-${Date.now()}`,
        label: "a public version is published into the public project [fetch]",
      },
    );
    const pinnablePID = publicVersion.body?.asset_pid;
    const pinnableLabel = publicVersion.body?.version;
    check(
      "the pinnable version is stored as public [fetch]",
      publicVersion.body?.visibility === "public" && typeof pinnablePID === "string",
      `visibility = ${JSON.stringify(publicVersion.body?.visibility)}, asset_pid = ${JSON.stringify(pinnablePID)}`,
    );
    const pinnableRef = `${pinnablePID}@${pinnableLabel}`;

    // ---- 5b-i. The pin IS rendered, and the entry point exists because the
    // rebuilt document still carries it.
    const renderablePin = newVersionOf(
      await harnessCandidate({
        version: freshVersion(),
        dependency_pins: pinnableRef,
      }),
      CHAIN.assetPID,
    );
    check(
      "the renderable-pin fixture declares the pin it will be read back for [harness]",
      (renderablePin.manifest?.dependency_pins ?? []).includes(pinnableRef),
      `pins = ${JSON.stringify(renderablePin.manifest?.dependency_pins)}`,
    );
    const renderablePreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      renderablePin,
    );
    check(
      "the API preview allows a pin into a public version of a public project [fetch]",
      renderablePreview.status === 200 && renderablePreview.body?.publishable === true,
      `status ${renderablePreview.status}, blockers ${JSON.stringify(renderablePreview.body?.publish_blockers)?.slice(0, 200)}`,
    );
    const renderablePublished = await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      renderablePin,
      201,
      { key: `publish-ux-renderable-${Date.now()}`, label: "the renderable-pin version is published [fetch]" },
    );
    const renderablePID = renderablePublished.body?.asset_pid;
    const renderableLabel = renderablePublished.body?.version;
    const renderableRow = await storedVersionRow(renderablePID, renderableLabel);
    check(
      "the stored row of the renderable-pin version declares the pin [harness]",
      (renderableRow?.manifest?.dependency_pins ?? []).includes(pinnableRef),
      `row pins = ${JSON.stringify(renderableRow?.manifest?.dependency_pins)}`,
    );
    await owner.page.goto(assetURL(renderablePID, renderableLabel), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-asset-publish]", { timeout: 30000 });
    const renderableText = await owner.page.locator("[data-asset-page='ready']").innerText();
    check(
      "the page renders the pin it can link to [ui]",
      renderableText.includes(pinnableRef),
      `the page does not name ${pinnableRef}`,
    );
    const renderableHref = await owner.page.getAttribute("[data-asset-publish]", "href");
    const renderableCandidate = decodeCandidate(new URL(renderableHref, BASE).searchParams.get("candidate"));
    check(
      "the entry point is offered for a version whose rebuild still carries the pin [ui]",
      renderableCandidate.integrity_hash === renderableRow?.integrity_hash &&
        (renderableCandidate.manifest?.dependency_pins ?? []).length === 1,
      `candidate hash ${renderableCandidate.integrity_hash}, row hash ${renderableRow?.integrity_hash}, ` +
        `pins ${JSON.stringify(renderableCandidate.manifest?.dependency_pins)}`,
    );

    // ---- 5b-ii. The pin is NOT rendered: same fixture, one pinning a
    // private version instead. The stored document still declares it, the
    // page cannot render it, and the two hashes therefore differ.
    const privatePinRef = `${CHAIN.assetPID}@${CHAIN.assetVersion}`;
    const withheldPin = newVersionOf(
      await harnessCandidate({
        version: freshVersion(),
        dependency_pins: privatePinRef,
      }),
      CHAIN.assetPID,
    );
    check(
      "the withheld-pin fixture declares a pin into a version that is not public [harness]",
      (withheldPin.manifest?.dependency_pins ?? []).includes(privatePinRef),
      `pins = ${JSON.stringify(withheldPin.manifest?.dependency_pins)}`,
    );
    const withheldPreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      withheldPin,
    );
    check(
      "the API preview allows a private pin in a publication that would not widen it [fetch]",
      withheldPreview.status === 200 && withheldPreview.body?.publishable === true,
      `status ${withheldPreview.status}, publishable ${withheldPreview.body?.publishable}, blockers ${JSON.stringify(
        withheldPreview.body?.publish_blockers,
      )?.slice(0, 200)}`,
    );
    const withheldPublished = await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      withheldPin,
      201,
      { key: `publish-ux-withheld-${Date.now()}`, label: "the withheld-pin version is published [fetch]" },
    );
    const withheldPID = withheldPublished.body?.asset_pid;
    const withheldLabel = withheldPublished.body?.version;
    const withheldRow = await storedVersionRow(withheldPID, withheldLabel);
    check(
      "the stored row of the withheld-pin version still declares the pin [harness]",
      (withheldRow?.manifest?.dependency_pins ?? []).includes(privatePinRef),
      `row pins = ${JSON.stringify(withheldRow?.manifest?.dependency_pins)}, row id = ${JSON.stringify(withheldRow?.id)}`,
    );
    await owner.page.goto(assetURL(withheldPID, withheldLabel), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-asset-publish-withheld]", { timeout: 30000 });
    check(
      "no publication is offered from a document this page cannot rebuild [ui]",
      (await owner.page.locator("[data-asset-publish]").count()) === 0,
      "an entry link was rendered for a version whose rebuild differs from the stored document",
    );
    const withheldNotice = owner.page.locator("[data-asset-publish-withheld]");
    check(
      "the withheld entry names the failure it is (an integrity mismatch) [ui]",
      (await withheldNotice.getAttribute("data-asset-publish-withheld")) === "integrity-mismatch",
      String(await withheldNotice.getAttribute("data-asset-publish-withheld")),
    );
    const withheldText = (await withheldNotice.innerText()).replace(/\s+/g, " ");
    check(
      "the notice states what differs: the pins the page can render [ui]",
      /does not cover/.test(withheldText) && /0 dependency pins/.test(withheldText),
      withheldText,
    );
    const withheldPageText = await owner.page.locator("[data-asset-page='ready']").innerText();
    check(
      "the page renders no pin the network cannot open [ui]",
      !withheldPageText.includes(privatePinRef),
      `the page names ${privatePinRef}, which the dependency block withholds`,
    );
    check(
      "the page shows the digest it checked its rebuild against [ui]",
      (await owner.page.locator("[data-asset-hash]").innerText()).trim() === withheldRow?.integrity_hash,
      `page says ${(await owner.page.locator("[data-asset-hash]").innerText()).trim()}, row says ${withheldRow?.integrity_hash}`,
    );

    /* ------------------------------------------------------------------ *
     * 6. Roles: the server is the boundary. The page may explain, the API
     *    decides.
     * ------------------------------------------------------------------ */
    banner("a maintainer is refused by the API, not only by the page");
    const maintainerCandidate = freshCandidate(baseCandidate);
    await maintainer.page.goto(confirmPageUrl(maintainerCandidate), { waitUntil: "domcontentloaded" });
    await maintainer.page.waitForSelector("[data-publish-role-notice]", { timeout: 30000 });
    check(
      "the maintainer sees why the confirm action is owner-only [ui]",
      (await maintainer.page.locator("[data-publish-role-notice]").count()) === 1,
    );
    check(
      "the maintainer is shown no confirm action [ui]",
      (await maintainer.page.locator("[data-publish-confirm]").count()) === 0,
    );
    const maintainerPublish = await call(
      maintainer.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      maintainerCandidate,
      { key: `publish-ux-maintainer-${Date.now()}` },
    );
    check(
      "the publish API refuses the maintainer (403), whatever the page shows [fetch]",
      maintainerPublish.status === 403,
      `got ${maintainerPublish.status}: ${JSON.stringify(maintainerPublish.body)?.slice(0, 200)}`,
    );

    banner("an anonymous actor is refused by the API");
    // The anonymous browser has loaded nothing yet, so it has no document
    // origin to make a same-origin call from. It opens the public site
    // first — with no session, which is the actor under test.
    await anon.page.goto(`${BASE}/`, { waitUntil: "domcontentloaded" });
    const anonCandidate = freshCandidate(baseCandidate);
    const anonPublish = await call(
      anon.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      anonCandidate,
      { key: `publish-ux-anon-${Date.now()}` },
    );
    check(
      "the publish API refuses an anonymous actor (401 or 403) [fetch]",
      anonPublish.status === 401 || anonPublish.status === 403,
      `got ${anonPublish.status}: ${JSON.stringify(anonPublish.body)?.slice(0, 200)}`,
    );
    const anonPreview = await call(
      anon.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      anonCandidate,
    );
    check(
      "the preview API refuses an anonymous actor (401 or 403) [fetch]",
      anonPreview.status === 401 || anonPreview.status === 403,
      `got ${anonPreview.status}: ${JSON.stringify(anonPreview.body)?.slice(0, 200)}`,
    );
    await anon.page.goto(confirmPageUrl(anonCandidate), { waitUntil: "domcontentloaded" });
    await anon.page.waitForLoadState("networkidle");
    check(
      "an anonymous visitor is shown no confirm action on the confirmation page [ui]",
      (await anon.page.locator("[data-publish-confirm]").count()) === 0,
    );
    // The confirmation page of a PRIVATE project is not merely preview-less
    // for a stranger: the project shell answers with the same existence-
    // hiding not-found every unauthorized reader sees, and the publish UI is
    // never mounted. A page that rendered "this publication was refused"
    // instead would be telling the stranger that the project exists.
    check(
      "an anonymous visitor gets the neutral project not-found, not a publish refusal [ui]",
      (await anon.page.locator("[data-project-notfound]").count()) === 1 &&
        (await anon.page.locator("[data-publish-confirm-page]").count()) === 0,
      `not-found ${await anon.page.locator("[data-project-notfound]").count()}, ` +
        `publish page ${await anon.page.locator("[data-publish-confirm-page]").count()}`,
    );
    check(
      "the not-found page says nothing about publications [ui]",
      !/publish/i.test(await anon.page.locator("[data-project-notfound]").innerText()),
      await anon.page.locator("[data-project-notfound]").innerText(),
    );

    // A PUBLIC project is the one an anonymous visitor CAN reach a
    // confirmation page in, and there the refusal has to be readable: the
    // API says AUTH_UNAUTHENTICATED, and the page owed the reader the one
    // fact it can act on rather than "something went wrong".
    await anon.page.goto(confirmPageUrlFor(publicProjectID, freshCandidate(anonCandidate)), {
      waitUntil: "domcontentloaded",
    });
    await anon.page.waitForSelector("[data-publish-error]", { timeout: 30000 });
    check(
      "an anonymous visitor reaches the confirmation page of a public project [ui]",
      (await anon.page.locator("[data-publish-confirm-page]").count()) === 1 &&
        (await anon.page.locator("[data-publish-section='summary']").count()) === 0,
      await anon.page.url(),
    );
    const anonNotice = (await anon.page.locator("[data-publish-error]").first().innerText())
      .replace(/\s+/g, " ")
      .trim();
    check(
      "the anonymous reader is told to sign in, in a sentence [ui]",
      /sign in/i.test(anonNotice),
      `notice: ${anonNotice}`,
    );
    check(
      "the anonymous reader is not told the generic fallback [ui]",
      !/something went wrong/i.test(anonNotice),
      `notice: ${anonNotice}`,
    );
    check(
      "the anonymous reader is offered no confirm action in a public project [ui]",
      (await anon.page.locator("[data-publish-confirm]").count()) === 0,
    );

    /* ------------------------------------------------------------------ *
     * 7. The state moved between the preview and the click: the refusal
     *    must name the cause, not fall back to a generic failure.
     * ------------------------------------------------------------------ */
    banner("a state change between preview and confirm is refused by name");
    const race = freshCandidate(baseCandidate);
    race.asset_pid = publishedPID;
    delete race.title;
    delete race.slug;
    await owner.page.goto(confirmPageUrl(race), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-confirm]", { timeout: 30000 });

    // Take the version label out from under the page: publish the identical
    // candidate through the API first. The page's own preview said this was
    // publishable; the publish re-runs its checks over the state at that
    // moment and now finds the version stored.
    await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      race,
      201,
      { key: `publish-ux-race-${Date.now()}`, label: "the identical candidate is published through the API first [fetch]" },
    );

    await owner.page.click("[data-publish-confirm]");
    await owner.page.waitForSelector("[data-publish-error]", { timeout: 30000 });
    const refusalText = (await owner.page.locator("[data-publish-error]").first().innerText()).trim();
    check(
      "the refusal names the cause instead of a generic failure [ui]",
      /already taken|version label/i.test(refusalText),
      `refusal text: ${refusalText}`,
    );
    check(
      "the refusal is not the generic fallback [ui]",
      !/something went wrong/i.test(refusalText),
      `refusal text: ${refusalText}`,
    );
    check(
      "the page stayed on the confirmation page for the reader to re-read [ui]",
      new URL(owner.page.url()).pathname === `/projects/${PROJECT}/assets/publish`,
      owner.page.url(),
    );

    /* ------------------------------------------------------------------ *
     * 7b. The same shape, moved on a different axis: a BLOB's access is
     *     not part of the candidate at all, so the reader's own page cannot
     *     see it change. The preview says the publication is clean, the
     *     blob's attachment stops being open, and the publish's own
     *     re-check inside its transaction is the only thing that can catch
     *     it (cmd/api/assetshttp blockedEnvelope: the refusal carries the
     *     complete report, and the page renders that report).
     * ------------------------------------------------------------------ */
    banner("a blob whose access moves under the page is refused by the server-side re-check");
    const blobFixture = await harnessWrite(owner.page, "POST", "/harness/blob", {
      object_version_id: CHAIN.acceptedVersionID,
      access_level: "open",
    });
    check(
      "the harness attached an OPEN blob to the released object version [harness]",
      blobFixture.status === 201 && typeof blobFixture.body?.blob_id === "string",
      `got ${blobFixture.status}: ${blobFixture.text.slice(0, 200)}`,
    );
    const blobID = blobFixture.body?.blob_id ?? "";

    // The candidate names that blob and promises open data access for it:
    // the one state in which a blob is publishable at preview time
    // (internal/assets previewBlobs: promised open + openly attached =
    // nothing to report), for a PUBLIC target, so the same blob blocks the
    // moment its access moves.
    const movedBlob = newVersionOf(
      await harnessCandidate({
        version: freshVersion(),
        visibility: "public",
        blob_ids: blobID,
      }),
      CHAIN.assetPID,
    );
    movedBlob.rights.visibility = { ...(movedBlob.rights.visibility ?? {}), data_access: "open" };
    const cleanPreview = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish-preview`,
      movedBlob,
    );
    check(
      "the API preview finds the openly attached blob publishable [fetch]",
      cleanPreview.status === 200 && cleanPreview.body?.publishable === true,
      `status ${cleanPreview.status}, publishable ${cleanPreview.body?.publishable}, blockers ${JSON.stringify(
        cleanPreview.body?.publish_blockers,
      )?.slice(0, 200)}`,
    );

    await owner.page.goto(confirmPageUrl(movedBlob), { waitUntil: "domcontentloaded" });
    await owner.page.waitForSelector("[data-publish-confirm]", { timeout: 30000 });
    check(
      "the page offers the confirm action while the preview is clean [ui]",
      (await owner.page.locator("[data-publish-confirm]").count()) === 1,
    );

    const movedAccess = await harnessWrite(owner.page, "POST", "/harness/blob-access", {
      blob_id: blobID,
      access_level: "restricted",
    });
    check(
      "the blob's attachment is no longer open [harness]",
      movedAccess.status === 200 && movedAccess.body?.attachments === 1,
      `got ${movedAccess.status}: ${movedAccess.text.slice(0, 200)}`,
    );

    await owner.page.click("[data-publish-confirm]");
    await owner.page.waitForSelector("[data-publish-error]", { timeout: 30000 });
    const movedNotice = (await owner.page.locator("[data-publish-error]").first().innerText())
      .replace(/\s+/g, " ")
      .trim();
    check(
      "the refusal says it was found when the publish ran, not at preview time [ui]",
      /refused when it was executed|server-side re-check/i.test(movedNotice),
      `notice: ${movedNotice}`,
    );
    check(
      "the refusal is not the generic fallback [ui]",
      !/something went wrong/i.test(movedNotice),
      `notice: ${movedNotice}`,
    );
    const movedBlockerRefs = await owner.page
      .locator("[data-publish-blocker-kind='private_dependency']")
      .evaluateAll((nodes) => nodes.map((n) => n.getAttribute("data-publish-blocker-ref")));
    check(
      "the refusal names the concrete blob that moved, not a count [ui]",
      movedBlockerRefs.includes(blobID),
      `blocker refs ${JSON.stringify(movedBlockerRefs)}, blob ${blobID}`,
    );
    check(
      "the blockers section is the one the refusal carried [ui]",
      (await owner.page.locator("[data-publish-section='blockers']").count()) === 1,
    );
    check(
      "the confirm action is gone once the server refused [ui]",
      (await owner.page.locator("[data-publish-confirm]").count()) === 0,
    );
    check(
      "the page states that the publication cannot be confirmed [ui]",
      (await owner.page.locator("[data-publish-blocked-notice]").count()) === 1,
    );

    /* ------------------------------------------------------------------ *
     * 7c. A version label past the API's bound. The page's entry works
     *     (the stored version is sound), and the version it would publish
     *     (<label>-public) is one character too long: 64 is the bound, and
     *     a 64-character label is storable — which is what makes this
     *     reachable at all.
     *
     *     The preview reads the label off the candidate
     *     (ASSET_INVALID_VERSION_LABEL, internal/assets gate), so the page
     *     refuses it BEFORE anything is clicked and names it; the publish
     *     route refuses the same candidate again with its own code
     *     (VALIDATION_FAILED) if it is ever sent. Both are asserted: the
     *     reader is owed the cause, not "something went wrong".
     * ------------------------------------------------------------------ */
    banner("a version label past the API's bound is refused by name");
    // Exactly internal/assets.MaxVersionLen, so the label is storable and
    // the version the page would publish from it (<label>-public) is not.
    const maxLabel = `1.0.${"9".repeat(60)}`;
    const maxLabelFixture = await harnessCandidate({
      version: maxLabel,
      title: "A version at the label bound",
      slug: `publish-ux-maxlabel-${Date.now()}`,
    });
    const maxLabelVersion = await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      maxLabelFixture,
      201,
      { key: `publish-ux-maxlabel-${Date.now()}`, label: "a version with a 64-character label is published [fetch]" },
    );
    check(
      "the stored label is exactly at the API's own bound [fetch]",
      maxLabelVersion.body?.version === maxLabel && maxLabel.length === 64,
      `version = ${JSON.stringify(maxLabelVersion.body?.version)} (${maxLabel.length} characters)`,
    );
    await owner.page.goto(
      assetURL(maxLabelVersion.body?.asset_pid, maxLabelVersion.body?.version),
      { waitUntil: "domcontentloaded" },
    );
    await owner.page.waitForSelector("[data-asset-publish]", { timeout: 30000 });
    await owner.page.click("[data-asset-publish]");
    await owner.page.waitForSelector("[data-publish-section='blockers']", { timeout: 30000 });
    const maxLabelCandidate = candidateFromPage(owner.page);
    check(
      "the entry asks for a version label the API will refuse [ui]",
      typeof maxLabelCandidate?.version === "string" && maxLabelCandidate.version.length > 64,
      `version = ${JSON.stringify(maxLabelCandidate?.version)} (${maxLabelCandidate?.version?.length} characters)`,
    );
    const labelBlockers = await owner.page
      .locator("[data-publish-blocker-kind='publish']")
      .evaluateAll((nodes) => nodes.map((n) => n.innerText.replace(/\s+/g, " ").trim()));
    check(
      "the blocker names the version label as the cause [ui]",
      labelBlockers.some((t) => /version label/i.test(t)),
      JSON.stringify(labelBlockers).slice(0, 400),
    );
    check(
      "the page offers no confirm action for a label the API refuses [ui]",
      (await owner.page.locator("[data-publish-confirm]").count()) === 0,
    );
    check(
      "the page states that the publication cannot be confirmed [ui]",
      (await owner.page.locator("[data-publish-blocked-notice]").count()) === 1,
    );
    const maxLabelAPI = await call(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      maxLabelCandidate,
      { key: `publish-ux-maxlabel-api-${Date.now()}` },
    );
    check(
      "the API refuses that label with VALIDATION_FAILED [fetch]",
      maxLabelAPI.status === 400 && maxLabelAPI.body?.code === "VALIDATION_FAILED",
      `got ${maxLabelAPI.status}: ${JSON.stringify(maxLabelAPI.body)?.slice(0, 200)}`,
    );

    /* ------------------------------------------------------------------ *
     * 7d. A document too large for the link. The candidate is a query
     *     parameter, so a metadata-heavy version would produce a link the
     *     web server refuses to even parse (Node's default 16 KiB header
     *     block). The page withholds the entry rather than offering one
     *     that cannot be opened, and states both numbers.
     * ------------------------------------------------------------------ */
    banner("a document too large for the link is not offered as one");
    const paddedFixture = await harnessCandidate({
      version: freshVersion(),
      metadata_pad: "9000",
      title: "A metadata-heavy version",
      slug: `publish-ux-padded-${Date.now()}`,
    });
    check(
      "the padded fixture really carries the padding in its manifest [harness]",
      typeof paddedFixture.manifest?.metadata?.harness_pad === "string" &&
        paddedFixture.manifest.metadata.harness_pad.length === 9000,
      `harness_pad = ${JSON.stringify(paddedFixture.manifest?.metadata?.harness_pad)?.slice(0, 60)}`,
    );
    const paddedVersion = await callOk(
      owner.page,
      "POST",
      `/api/v1/projects/${PROJECT}/assets:publish`,
      paddedFixture,
      201,
      { key: `publish-ux-padded-${Date.now()}`, label: "the metadata-heavy version is published [fetch]" },
    );
    await owner.page.goto(
      assetURL(paddedVersion.body?.asset_pid, paddedVersion.body?.version),
      { waitUntil: "domcontentloaded" },
    );
    await owner.page.waitForSelector("[data-asset-publish-withheld]", { timeout: 30000 });
    check(
      "no entry link is offered for a document too large to carry [ui]",
      (await owner.page.locator("[data-asset-publish]").count()) === 0,
      "an entry link was rendered for a candidate that cannot fit the request line",
    );
    const paddedNotice = owner.page.locator("[data-asset-publish-withheld]");
    check(
      "the entry is withheld over the link budget, not over the document's contents [ui]",
      (await paddedNotice.getAttribute("data-asset-publish-withheld")) === "candidate-too-long",
      String(await paddedNotice.getAttribute("data-asset-publish-withheld")),
    );
    const paddedText = (await paddedNotice.innerText()).replace(/\s+/g, " ");
    const statedLength = Number(/(\d+) characters once encoded/.exec(paddedText)?.[1]);
    const statedBudget = Number(/(\d+)-character budget/.exec(paddedText)?.[1]);
    check(
      "the notice states the length and the budget it exceeds, and they agree [ui]",
      Number.isFinite(statedLength) &&
        Number.isFinite(statedBudget) &&
        statedLength > statedBudget &&
        statedBudget > 0 &&
        statedBudget < 16384,
      `notice: ${paddedText}`,
    );

    /* ------------------------------------------------------------------ *
     * 8. The published version is a real one.
     * ------------------------------------------------------------------ */
    banner("the version the page published is readable from the product");
    const stored = await call(
      owner.page,
      "GET",
      `/api/v1/assets/${encodeURIComponent(publishedPID)}?version=${encodeURIComponent(
        publishedVersion,
      )}`,
    );
    check(
      "the published version is readable at the pid@version the page landed on [fetch]",
      stored.status === 200 && stored.body?.version?.version === publishedVersion,
      `got ${stored.status} for ${assetURL(publishedPID, publishedVersion)}: ${JSON.stringify(
        stored.body,
      )?.slice(0, 200)}`,
    );
    check(
      "the version the page published is the PUBLIC one it promised [fetch]",
      stored.body?.version?.visibility === "public",
      `visibility = ${JSON.stringify(stored.body?.version?.visibility)}`,
    );
    check(
      "the published version carries the provenance the candidate declared [fetch]",
      (stored.body?.origin ?? []).some((o) => o.ref === `release:${CHAIN.releaseID}`),
      `origin = ${JSON.stringify(stored.body?.origin)}`,
    );

    /* ------------------------------------------------------------------ *
     * 9. No console noise, no uncaught exceptions, no 5xx.
     * ------------------------------------------------------------------ */
    for (const ctx of [owner, maintainer]) {
      const unexpectedConsole = ctx.noise.console.filter((m) => !/\b(400|401|403|409)\b/.test(m));
      check(
        `${ctx.label}'s pages logged no unexpected console error [ui]`,
        unexpectedConsole.length === 0,
        unexpectedConsole.slice(0, 3).join(" | "),
      );
      check(
        `${ctx.label}'s pages raised no uncaught exception [ui]`,
        ctx.noise.page.length === 0,
        ctx.noise.page.slice(0, 3).join(" | "),
      );
      check(
        `${ctx.label}'s requests all reached the API [ui]`,
        ctx.noise.network.length === 0,
        ctx.noise.network.slice(0, 3).join(" | "),
      );
    }
    for (const ctx of [owner, maintainer, anon]) {
      check(
        `${ctx.label} saw no 5xx from the API [ui]`,
        ctx.noise.server.length === 0,
        ctx.noise.server.slice(0, 3).join(" | "),
      );
    }

    await browser.close();
  } catch (err) {
    fails += 1;
    console.error("publish-ux-e2e: unexpected failure:", err);
    try {
      fs.writeFileSync(
        "/tmp/publish-ux-e2e-failure.html",
        await owner.page.content(),
      );
      console.error("publish-ux-e2e: page HTML written to /tmp/publish-ux-e2e-failure.html");
    } catch {
      /* the page may already be gone */
    }
    await browser.close();
  }

  if (fails > 0) {
    console.error(`\npublish-ux-e2e: FAILED with ${fails} failure(s)`);
    process.exit(1);
  }
  console.log("\npublish-ux-e2e: all passed");
}

await main();
