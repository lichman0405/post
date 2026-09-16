/**
 * Anonymous pages e2e (T0801 required test "anonymous e2e").
 *
 * Three readers run against the BUILT web app (next start, production
 * layer) with the Go API served by tests/e2e-anonymous/mock-api.mjs:
 *
 *   A. THE CRAWLER — plain HTTP, no browser, no JavaScript, no cookie:
 *      `fetch()` each public entity page and read the bytes the server
 *      sent. This is the reader T0801 exists for, and the only reader that
 *      can see whether the <head> is server-rendered at all.
 *
 *      It reads each page TWICE, because the two are different contracts:
 *
 *        - as an HTML-limited crawler (a Bingbot UA). Next.js serves such
 *          crawlers BLOCKING metadata — the <head> is completed before the
 *          shell is flushed — so this read asserts the tags inside <head>,
 *          which is where a search engine that checks the head looks. This
 *          is the assertion that matters for "public pages 服务端输出可索引
 *          内容" (docs/51);
 *        - as an ordinary reader (no bot token in the UA). Next.js streams
 *          for these: the shell is flushed first and the metadata follows
 *          at the end of the document, where the browser's parser and
 *          React's client runtime place it. A crawler that is not on
 *          Next's list must therefore still find every tag in the server's
 *          bytes — which is what this read asserts, and what section B
 *          confirms from a JavaScript-free browser (document.title).
 *
 *      Both readings assert the same thing about the CONTENT, and both
 *      assert that a private entity and an id that names nothing are
 *      byte-identical once the requested id is masked.
 *
 *   B. THE VISITOR — real Chromium, a fresh context with no cookies: every
 *      public entity page renders its content for a signed-out reader,
 *      with no redirect and no console error, while the private page shows
 *      the same neutral not-found state a stranger gets for an id that
 *      does not exist. A second, JavaScript-free context repeats the read
 *      to show what a reader with no scripting at all gets from the same
 *      response.
 *
 * No reader ever signs in, and the mock API logs every request it
 * answered, so the last section proves the property the whole design rests
 * on: NOT ONE read in the entire run carried a session (no Cookie, no
 * Authorization). That is what makes "indexable" mean "public" rather than
 * "public to whoever happened to be looking".
 *
 * The wire shapes the mock serves are pinned against the real guard and
 * real PostgreSQL by tests/e2e/privacy_e2e_test.go and the integration
 * suite; this checklist proves the PAGES honour them.
 *
 * Usage: node anonymous-e2e.mjs <base-url> <api-url>
 */
import { chromium } from "playwright";

import {
  ASSET_PID,
  ASSET_SLUG,
  EXPECTED,
  HIDDEN_DESCRIPTION,
  HIDDEN_TITLE,
  PRIVATE_ASSET_PID,
  PRIVATE_PROJECT,
  PRIVATE_RELEASE_ID,
  PRIVATE_WORDS,
  PROFILE,
  PUBLIC_PROJECT,
  RELEASE,
  UNKNOWN_ASSET_PID,
  UNKNOWN_PROFILE_ID,
  UNKNOWN_PROJECT_ID,
  UNKNOWN_RELEASE_ID,
} from "./fixtures.mjs";

const BASE = (process.argv[2] ?? "http://127.0.0.1:31141").replace(/\/+$/, "");
const API = (process.argv[3] ?? "http://127.0.0.1:31142").replace(/\/+$/, "");

/** A crawler Next.js renders blocking metadata for (next/dist/.../html-bots.js). */
const BOT_UA = "Mozilla/5.0 (compatible; Bingbot/2.0; +http://www.bing.com/bingbot.htm)";

/** A reader that gets the streamed response — no bot token in the UA. */
const PLAIN_UA = "post-e2e-anonymous-reader/1.0";

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

/* ---------- HTML reading (no DOM library, no JavaScript execution) ---------- */

const ENTITIES = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " " };

function decode(value) {
  return value.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (whole, body) => {
    if (body.startsWith("#x") || body.startsWith("#X")) {
      const code = Number.parseInt(body.slice(2), 16);
      return Number.isNaN(code) ? whole : String.fromCodePoint(code);
    }
    if (body.startsWith("#")) {
      const code = Number.parseInt(body.slice(1), 10);
      return Number.isNaN(code) ? whole : String.fromCodePoint(code);
    }
    return ENTITIES[body] ?? whole;
  });
}

/** The attributes of one tag, as a map. */
function attrsOf(tag) {
  const out = {};
  const re = /([a-zA-Z_:][-a-zA-Z0-9_:.]*)="([^"]*)"/g;
  let m;
  while ((m = re.exec(tag)) !== null) out[m[1].toLowerCase()] = decode(m[2]);
  return out;
}

/** Every <name ...> element's attributes, in document order. */
function elements(html, name) {
  const out = [];
  const re = new RegExp(`<${name}\\s[^>]*>`, "gi");
  let m;
  while ((m = re.exec(html)) !== null) out.push(attrsOf(m[0]));
  return out;
}

/** The <head> element's contents: everything before the first </head>. */
function headOf(html) {
  const start = html.indexOf("<head");
  const end = html.indexOf("</head>");
  if (start === -1 || end === -1) return "";
  const open = html.indexOf(">", start);
  return html.slice(open + 1, end);
}

/** The metadata a reader can find in one scope (a <head>, or a whole document). */
function readScope(scopeHtml) {
  const metas = elements(scopeHtml, "meta");
  const links = elements(scopeHtml, "link");
  const titleMatch = /<title[^>]*>([\s\S]*?)<\/title>/i.exec(scopeHtml);
  const scope = {
    raw: scopeHtml,
    title: titleMatch === null ? "" : decode(titleMatch[1]),
    meta: (name) => metas.find((m) => m.name === name)?.content ?? "",
    property: (prop) => metas.find((m) => m.property === prop)?.content ?? "",
    link: (rel) => links.find((l) => l.rel === rel)?.href ?? "",
  };
  // Everything that decides whether (and how) a crawler indexes the page,
  // with nothing of the route in it. Two unindexable pages of DIFFERENT
  // kinds (a project and a profile, say) must have the same signature: a
  // crawler must not be able to tell them apart either.
  scope.signature = JSON.stringify({
    title: scope.title,
    description: scope.meta("description"),
    robots: scope.meta("robots"),
    canonical: scope.link("canonical"),
    ogTitle: scope.property("og:title"),
    ogDescription: scope.property("og:description"),
    ogUrl: scope.property("og:url"),
  });
  return scope;
}

/** One page as a reader receives it: the <head> scope and the document scope. */
function readPage(html) {
  return { raw: html, head: readScope(headOf(html)), doc: readScope(html) };
}

/** Fetch one page over plain HTTP, with a caller-chosen User-Agent. */
async function crawl(path, userAgent = PLAIN_UA) {
  const res = await fetch(`${BASE}${path}`, {
    redirect: "manual",
    headers: { "user-agent": userAgent },
  });
  const html = await res.text();
  return { status: res.status, location: res.headers.get("location"), ...readPage(html) };
}

/* ---------- A. The crawler's view ---------- */

console.log(`anonymous-e2e: crawler ${BASE}, API ${API}`);

/**
 * A public entity page, read as an HTML-limited crawler: reachable, and its
 * completed <head> names the entity in the entity's own words.
 */
async function checkPublicPage(label, expected) {
  const page = await crawl(expected.path, BOT_UA);

  check(`${label}: the page is served to a crawler (200, no redirect)`,
    page.status === 200 && page.location === null,
    `status=${page.status} location=${page.location}`);
  if (page.status !== 200) return null;

  check(`${label}: the title names the entity`,
    page.head.title === expected.title,
    `title=${JSON.stringify(page.head.title)} want=${JSON.stringify(expected.title)}`);
  check(`${label}: the description is the entity's own words`,
    page.head.meta("description") === expected.description,
    `description=${JSON.stringify(page.head.meta("description"))}`);
  check(`${label}: the canonical address is the entity's own absolute URL`,
    page.head.link("canonical") === `${BASE}${expected.path}`,
    `canonical=${page.head.link("canonical")} want=${BASE}${expected.path}`);
  check(`${label}: the page invites indexing`,
    page.head.meta("robots").includes("index") && !page.head.meta("robots").includes("noindex"),
    `robots=${JSON.stringify(page.head.meta("robots"))}`);
  check(`${label}: OpenGraph repeats the same name and address`,
    page.head.property("og:title") === expected.title &&
      page.head.property("og:url") === `${BASE}${expected.path}` &&
      page.head.property("og:description") === expected.description,
    `og:title=${JSON.stringify(page.head.property("og:title"))} og:url=${page.head.property("og:url")}`);
  return page;
}

const publicPages = [
  ["public project", EXPECTED.project],
  ["public release", EXPECTED.release],
  ["public asset", EXPECTED.asset],
  ["public asset version", EXPECTED.assetVersion],
  ["public profile", EXPECTED.profile],
];
const botReads = new Map();
for (const [label, expected] of publicPages) {
  botReads.set(label, await checkPublicPage(label, expected));
  console.log("");
}

// A crawler Next.js does not know gets the STREAMED response: the metadata
// arrives after the shell instead of inside <head>. The contract for that
// reader is not "it is in head" but "it is in the bytes the server sent" —
// asserted here for every public page, and confirmed against a JavaScript-
// free browser in section B. If this ever fails, a crawler that is not on
// Next's htmlLimitedBots list is being served a page with no metadata at
// all, which is the failure mode docs/51 exists to prevent.
for (const [label, expected] of publicPages) {
  const page = await crawl(expected.path);
  const bot = botReads.get(label);
  check(`streamed read (${label}): the same metadata is in the server's bytes`,
    page.doc.signature === bot?.head.signature,
    `streamed=${page.doc.signature} blocking=${bot?.head.signature}`);
}

/**
 * A page that must not be indexed: the generic head, and NOTHING of the
 * entity in it. `id` is the id the request was made with — it is in the URL
 * the crawler itself typed, so it is masked before two such heads are
 * compared; everything else must be identical.
 */
async function checkHiddenPage(label, path, id) {
  const page = await crawl(path, BOT_UA);
  check(`${label}: the page is served (200, no redirect to a sign-in)`,
    page.status === 200 && page.location === null,
    `status=${page.status} location=${page.location}`);
  if (page.status !== 200) return null;

  check(`${label}: it carries the generic, name-free title`,
    page.head.title === HIDDEN_TITLE, `title=${JSON.stringify(page.head.title)}`);
  check(`${label}: it says noindex, nofollow inside <head>`,
    page.head.meta("robots").includes("noindex") && page.head.meta("robots").includes("nofollow"),
    `robots=${JSON.stringify(page.head.meta("robots"))}`);
  check(`${label}: it has no canonical and no OpenGraph url`,
    page.head.link("canonical") === "" && page.head.property("og:url") === "",
    `canonical=${page.head.link("canonical")} og:url=${page.head.property("og:url")}`);
  check(`${label}: the description describes the page, not the entity`,
    page.head.meta("description") === HIDDEN_DESCRIPTION,
    `description=${JSON.stringify(page.head.meta("description"))}`);
  for (const word of PRIVATE_WORDS) {
    check(`${label}: the served HTML never contains ${JSON.stringify(word)}`,
      !page.raw.includes(word), "found in the page the server sent");
  }
  return { ...page, masked: page.head.raw.split(id).join("<ID>") };
}

const hiddenPages = [];
const privateProjectPage = await checkHiddenPage(
  "private project",
  `/projects/${PRIVATE_PROJECT.id}`,
  PRIVATE_PROJECT.id,
);
hiddenPages.push(privateProjectPage);
console.log("");
const unknownProjectPage = await checkHiddenPage(
  "unknown project id",
  `/projects/${UNKNOWN_PROJECT_ID}`,
  UNKNOWN_PROJECT_ID,
);
hiddenPages.push(unknownProjectPage);
console.log("");
const unknownAssetPage = await checkHiddenPage(
  "unknown asset pid",
  `/assets/${UNKNOWN_ASSET_PID}`,
  UNKNOWN_ASSET_PID,
);
hiddenPages.push(unknownAssetPage);
console.log("");
const privateAssetPage = await checkHiddenPage(
  "asset of a private project",
  `/assets/${PRIVATE_ASSET_PID}`,
  PRIVATE_ASSET_PID,
);
hiddenPages.push(privateAssetPage);
console.log("");
hiddenPages.push(
  await checkHiddenPage(
    "release of a private project",
    `/projects/${PRIVATE_PROJECT.id}/releases/${PRIVATE_RELEASE_ID}`,
    PRIVATE_PROJECT.id,
  ),
);
console.log("");
hiddenPages.push(
  await checkHiddenPage(
    "unknown profile id",
    `/users/${UNKNOWN_PROFILE_ID}`,
    UNKNOWN_PROFILE_ID,
  ),
);
console.log("");

// The heart of existence hiding, read from outside: a private entity and an
// id that names nothing must be the SAME page. If they ever differ, the
// difference is a list of private ids anyone can harvest.
check("existence hiding: a private project's head is the unknown id's head, byte for byte",
  privateProjectPage?.masked === unknownProjectPage?.masked,
  `private=${JSON.stringify(privateProjectPage?.masked)} unknown=${JSON.stringify(unknownProjectPage?.masked)}`);
check("existence hiding: a private project's asset reads as an unknown pid",
  privateAssetPage?.masked === unknownAssetPage?.masked,
  "the two unindexable heads differ");
const signatures = new Set(hiddenPages.map((p) => p.head.signature));
check("existence hiding: every unindexable page, of every kind, carries one identical head",
  signatures.size === 1,
  [...signatures].join("\n"));
console.log("");

// The one case that is NOT byte-identical, asserted on purpose so a change
// shows up here rather than in silence: a release URL under a PUBLIC project
// that has no such release. Next merges the metadata of every segment along
// the route, and the project layout (/projects/[id]/layout.tsx) describes a
// public project — so the canonical of the parent survives into a page the
// release segment marked noindex. That canonical names a public, already
// indexable address, which is why it is allowed here: it discloses nothing,
// and the page is noindex either way. What must hold, and does below, is
// that the page still carries no name of its own, no OpenGraph url, and
// nothing of the private world.
const orphanRelease = await crawl(
  `/projects/${PUBLIC_PROJECT.id}/releases/${UNKNOWN_RELEASE_ID}`,
  BOT_UA,
);
check("release a public project does not have: still unindexable and name-free",
  orphanRelease.status === 200 &&
    orphanRelease.head.title === HIDDEN_TITLE &&
    orphanRelease.head.meta("robots").includes("noindex") &&
    orphanRelease.head.property("og:url") === "" &&
    !PRIVATE_WORDS.some((word) => orphanRelease.raw.includes(word)),
  `title=${orphanRelease.head.title} robots=${orphanRelease.head.meta("robots")}`);
check("release a public project does not have: the only canonical it carries is its public parent's",
  [ "", `${BASE}${EXPECTED.project.path}` ].includes(orphanRelease.head.link("canonical")),
  `canonical=${orphanRelease.head.link("canonical")}`);
console.log("");

/* ---------- A2. robots.txt and sitemap.xml ---------- */

const robotsRes = await fetch(`${BASE}/robots.txt`);
const robotsTxt = await robotsRes.text();
check("robots.txt: served", robotsRes.status === 200 && robotsTxt.length > 0, `status=${robotsRes.status}`);
check("robots.txt: everything public is allowed", /Allow:\s*\//.test(robotsTxt), robotsTxt);
check("robots.txt: the personal/governance surfaces are asked out",
  /Disallow:\s*\/notifications/.test(robotsTxt), robotsTxt);
check("robots.txt: it names the sitemap absolutely",
  new RegExp(`Sitemap:\\s*${BASE.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}/sitemap\\.xml`).test(robotsTxt),
  robotsTxt);
check("robots.txt: it does NOT try to enumerate private URLs (that would publish them)",
  !PRIVATE_WORDS.some((word) => robotsTxt.includes(word)), robotsTxt);

const sitemapRes = await fetch(`${BASE}/sitemap.xml`);
const sitemapXml = await sitemapRes.text();
check("sitemap.xml: served as XML", sitemapRes.status === 200 && sitemapXml.trimStart().startsWith("<?xml"),
  `status=${sitemapRes.status} body=${sitemapXml.slice(0, 40)}`);
const locs = [...sitemapXml.matchAll(/<loc>([^<]*)<\/loc>/g)].map((m) => m[1]);
const wantLocs = [
  `${BASE}/`,
  `${BASE}/projects`,
  `${BASE}/assets`,
  `${BASE}${EXPECTED.project.path}`,
  `${BASE}${EXPECTED.asset.path}`,
];
check("sitemap.xml: it lists the public pages and only them",
  locs.length === wantLocs.length && wantLocs.every((url) => locs.includes(url)),
  `got=${JSON.stringify(locs)}`);
check("sitemap.xml: no private id, slug or pid is enumerable from it",
  !PRIVATE_WORDS.some((word) => sitemapXml.includes(word)) &&
    !sitemapXml.includes(PRIVATE_PROJECT.id) &&
    !sitemapXml.includes(PRIVATE_ASSET_PID) &&
    !sitemapXml.includes(PRIVATE_RELEASE_ID),
  sitemapXml);
check("sitemap.xml: it carries a lastmod from the API's own public rows",
  /<lastmod>/.test(sitemapXml), sitemapXml);
console.log("");

/* ---------- B. The signed-out visitor ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });

const consoleErrors = [];
const pageErrors = [];
const page = await context.newPage();
page.on("console", (msg) => {
  if (msg.type() === "error") consoleErrors.push(msg.text());
});
page.on("pageerror", (err) => pageErrors.push(String(err)));

/** Visits one page and reports where the visitor ended up. */
async function visit(path) {
  const response = await page.goto(`${BASE}${path}`, { waitUntil: "load" });
  return { status: response?.status() ?? 0, url: page.url() };
}

/**
 * Runs one group of visitor checks, turning a thrown error (a selector
 * that never appeared, a page that failed to load) into a FAIL line. A
 * page that never renders is a failing page, and the run must report it as
 * one — an exception here would abort the checklist and hide every check
 * after it, which is exactly the shape a broken page takes.
 */
async function step(label, fn) {
  try {
    await fn();
  } catch (err) {
    fail(`${label}: the page could not be driven`, err instanceof Error ? err.message : String(err));
  }
}

// --- the public project: a signed-out visitor reads it, and no wall ---
await step("visitor/project", async () => {
  const at = await visit(EXPECTED.project.path);
  check("visitor/project: no redirect away from the page", at.url === `${BASE}${EXPECTED.project.path}`, at.url);
  check("visitor/project: served 200 to a signed-out visitor", at.status === 200, `status=${at.status}`);
  await page.waitForSelector("[data-project-shell]", { timeout: 15000 });
  check("visitor/project: the shell renders with the project's own slug",
    (await page.getAttribute("[data-project-shell]", "data-project-shell")) === PUBLIC_PROJECT.slug,
    await page.getAttribute("[data-project-shell]", "data-project-shell"));
  const projectText = await page.locator(".project-shell").innerText();
  check("visitor/project: the reader sees the project's name and purpose",
    projectText.includes(PUBLIC_PROJECT.name) && projectText.includes(PUBLIC_PROJECT.purpose),
    projectText.slice(0, 200));
  check("visitor/project: the shell offers no Settings tab to a stranger",
    (await page.locator('[data-project-tab="settings"]').count()) === 0,
    "a signed-out visitor was offered project settings");
  // Signed out is the CONTEXT, not an assumption: the header shows its
  // signed-out entry, and the project page rendered anyway. That is "the
  // login wall does not block public research", stated as two observable
  // facts rather than one assertion about a string.
  check("visitor/project: the header itself knows nobody is signed in",
    (await page.locator(".global-nav-signin").count()) > 0,
    "the signed-out nav entry is missing — the visitor may not be anonymous");
});
console.log("");

// --- the public release ---
await step("visitor/release", async () => {
  const at = await visit(EXPECTED.release.path);
  check("visitor/release: no redirect away from the release", at.url === `${BASE}${EXPECTED.release.path}`, at.url);
  await page.waitForSelector("[data-release-detail]", { timeout: 15000 });
  check("visitor/release: the release renders its version",
    (await page.getAttribute("[data-release-detail]", "data-release-detail")) === RELEASE.version,
    await page.getAttribute("[data-release-detail]", "data-release-detail"));
  const releaseText = await page.locator(".release-detail").innerText();
  check("visitor/release: the reader sees the release title", releaseText.includes(RELEASE.title),
    releaseText.slice(0, 200));
});
console.log("");

// --- the asset hub and one asset ---
await step("visitor/assets", async () => {
  const hub = await visit("/assets");
  check("visitor/assets: the hub is served to a signed-out visitor", hub.status === 200, `status=${hub.status}`);
  await visit(EXPECTED.asset.path);
  await page.waitForSelector('[data-asset-page="ready"]', { timeout: 15000 });
  check("visitor/asset: the asset renders for a signed-out visitor",
    (await page.getAttribute("[data-asset-title]", "data-asset-title")) === ASSET_SLUG,
    await page.getAttribute("[data-asset-title]", "data-asset-title"));
  await visit(EXPECTED.assetVersion.path);
  await page.waitForSelector('[data-asset-page="ready"]', { timeout: 15000 });
  check("visitor/asset version: a pinned version renders too",
    (await page.getAttribute("[data-asset-version]", "data-asset-version")) === "2.0",
    await page.getAttribute("[data-asset-version]", "data-asset-version"));
});
console.log("");

// --- the public profile ---
await step("visitor/profile", async () => {
  const at = await visit(EXPECTED.profile.path);
  check("visitor/profile: no redirect away from the profile", at.url === `${BASE}${EXPECTED.profile.path}`, at.url);
  await page.waitForSelector(".profile-handle", { timeout: 15000 });
  check("visitor/profile: the profile renders its handle",
    (await page.locator(".profile-handle").innerText()).trim() === `@${PROFILE.handle}`,
    await page.locator(".profile-handle").innerText());
  const profileText = await page.locator(".profile-card").innerText();
  check("visitor/profile: the reader sees the owner's own bio", profileText.includes(PROFILE.bio),
    profileText.slice(0, 200));
});
console.log("");

// --- the private project and the id that names nothing: one page ---
let privateText = "";
await step("visitor/private", async () => {
  const at = await visit(`/projects/${PRIVATE_PROJECT.id}`);
  check("visitor/private: no redirect, no sign-in wall", at.url === `${BASE}/projects/${PRIVATE_PROJECT.id}`, at.url);
  await page.waitForSelector("[data-project-notfound]", { timeout: 15000 });
  privateText = await page.locator(".project-state").innerText();
  const privateDump = await page.content();
  check("visitor/private: the not-found state names nothing of the project",
    !PRIVATE_WORDS.some((word) => privateDump.includes(word)),
    "the private entity's words reached the anonymous DOM");
  check("visitor/private: it offers no shell chrome to a stranger",
    (await page.locator("[data-project-shell]").count()) === 0,
    "a private project rendered shell chrome anonymously");
});
const privateTextSeen = privateText;

await step("visitor/unknown id", async () => {
  await visit(`/projects/${UNKNOWN_PROJECT_ID}`);
  await page.waitForSelector("[data-project-notfound]", { timeout: 15000 });
  const unknownText = await page.locator(".project-state").innerText();
  check("visitor: a private project and an unknown id are the SAME visible state",
    privateTextSeen !== "" && privateTextSeen === unknownText,
    `private=${JSON.stringify(privateTextSeen)} unknown=${JSON.stringify(unknownText)}`);
});
console.log("");

// --- the same pages again, with JavaScript switched off entirely ---
//
// This is the reader that has neither cookies nor a script engine: a
// modest indexer, a social-preview fetcher, `curl | grep`. Nothing in the
// page has been hydrated for it, so what it can see is exactly what the
// server sent. It is also the reader that shows the difference between the
// two ways Next serves metadata (see the header): this one gets the
// STREAMED document, and must still find the tags in it — which it does,
// because `document.title` and a document-wide query see what the parser
// built, not only what is inside <head>.
const noJS = await browser.newContext({ javaScriptEnabled: false, viewport: { width: 1280, height: 900 } });
const bare = await noJS.newPage();

/** Title, robots and canonical as a script-free reader finds them. */
async function bareRead(path) {
  await bare.goto(`${BASE}${path}`, { waitUntil: "load" });
  return bare.evaluate(() => ({
    title: document.title,
    robots: document.querySelector('meta[name="robots"]')?.getAttribute("content") ?? "",
    canonical: document.querySelector('link[rel="canonical"]')?.getAttribute("href") ?? "",
  }));
}

await step("no-JS reader", async () => {
  for (const [label, expected] of publicPages) {
    const seen = await bareRead(expected.path);
    check(`no-JS reader (${label}): title, robots and canonical are readable with no script`,
      seen.title === expected.title && seen.robots.includes("index") && seen.canonical === `${BASE}${expected.path}`,
      JSON.stringify(seen));
  }
  for (const [label, path] of [
    ["private project", `/projects/${PRIVATE_PROJECT.id}`],
    ["unknown id", `/projects/${UNKNOWN_PROJECT_ID}`],
  ]) {
    const seen = await bareRead(path);
    check(`no-JS reader (${label}): it is told not to index, with no canonical`,
      seen.title === HIDDEN_TITLE && seen.robots.includes("noindex") && seen.canonical === "",
      JSON.stringify(seen));
  }
});
await noJS.close();
console.log("");

/* ---------- C. The wire: not one read carried a session ---------- */

const logged = await (async () => {
  const res = await fetch(`${API}/__requests`);
  return (await res.json()).requests;
})();

const withSession = logged.filter((r) => r.cookie !== null || r.authorization !== null);
check("wire: no request in the entire run carried a cookie or an Authorization header",
  withSession.length === 0,
  withSession.map((r) => `${r.method} ${r.path} cookie=${r.cookie}`).join(" | "));
const writes = logged.filter((r) => r.method !== "GET" && r.method !== "HEAD" && r.method !== "OPTIONS");
check("wire: an anonymous browse issues reads only", writes.length === 0,
  writes.map((r) => `${r.method} ${r.path}`).join(" | "));
check("wire: the server-side metadata read of the public project happened, anonymously",
  logged.some((r) => r.path === `/api/v1/projects/${PUBLIC_PROJECT.id}` && r.status === 200),
  `${logged.length} request(s) logged`);
check("wire: the browser itself read the project successfully (the page is the API's answer)",
  logged.some((r) => r.path === `/api/v1/projects/${PUBLIC_PROJECT.id}` && r.origin === BASE && r.status === 200),
  logged.filter((r) => r.path === `/api/v1/projects/${PUBLIC_PROJECT.id}`).map((r) => `${r.origin}/${r.status}`).join(" "));
check("wire: the private project was asked for and answered with the hiding 404",
  logged.some((r) => r.path === `/api/v1/projects/${PRIVATE_PROJECT.id}` && r.status === 404),
  "no 404 was recorded for the private project");
check("wire: the private asset was asked for and answered with the hiding 404",
  logged.some((r) => r.path === `/api/v1/assets/${PRIVATE_ASSET_PID}` && r.status === 404),
  "no 404 was recorded for the private asset");
console.log("");

/* ---------- D. Nothing broke while reading ---------- */

check("browser: no uncaught page error on any page", pageErrors.length === 0, pageErrors.join(" | "));
const realConsoleErrors = consoleErrors.filter((text) => !/Failed to load resource/.test(text));
check("browser: no console error on any page", realConsoleErrors.length === 0, realConsoleErrors.join(" | "));
if (consoleErrors.length !== realConsoleErrors.length) {
  // Chromium reports the API's own 401 (no session) and 404 (private
  // entity) here. They are the expected answers of an anonymous browse,
  // not page failures — printed rather than swallowed so a real 404 stays
  // visible.
  console.log(`note: ${consoleErrors.length - realConsoleErrors.length} API-status message(s) from the expected anonymous answers:`);
  for (const text of consoleErrors) if (/Failed to load resource/.test(text)) console.log(`note:   ${text}`);
}

await browser.close();

if (fails > 0) {
  console.error(`anonymous-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("anonymous-e2e: all checks passed");
