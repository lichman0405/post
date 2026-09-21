/**
 * Search Answer browser e2e (T0907).
 *
 * Drives the REAL web app — a production `next build` served by `next start`
 * — in real Chromium, with the answer coming over HTTP from the harness's API
 * origin (mock-api.mjs; see its header for why the answer, and the source
 * addresses it names, are a server and not a `page.route`).
 *
 * What this file is responsible for proving (docs/05 §5, docs/42 §Search
 * Answer, ADR-010):
 *
 *   1. The four states exist and are told apart. `answered` shows the summary,
 *      its View label and the citations it leans on; `fallback` — all six of
 *      the closed reasons, each with its own sentence — shows the structured
 *      result and is NOT drawn as an error; `loading` is rendered while the
 *      question is out; an error is an alert with the contract's code.
 *   2. `sources` and `cited` agree, row by row and by ref: the rows the page
 *      marks as cited are exactly the ones the document marked, which are
 *      exactly the refs the summary's citations name — checked against the
 *      fixture, not against the page's own opinion.
 *   3. The URL, and only the URL, is where the question lives: a direct open
 *      (no form, no click) renders the answer; a reload and a fresh tab
 *      render the same search_id; a different question renders a different
 *      one; and the request that reached the API carried the URL's question.
 *   4. Every source is reachable: the address a source links to is the one
 *      the answer gave for it (never a path this app composed), a click lands
 *      on a real 200 there, and a source the answer gave no address for is
 *      rendered as text rather than linked somewhere invented.
 *   5. Nothing on the page is invented: with the lists empty, the sections
 *      are ABSENT (not filled with a placeholder), and the app's own source
 *      files contain none of the fixture's strings.
 *   6. Keyboard: the source links are in the tab order, in order, and Enter
 *      opens one.
 *   7. a11y: axe (WCAG 2 A/AA + best-practice) finds nothing on the answered
 *      state or the fallback state — both of which only exist with a live API
 *      and so are not covered by tests/web-smoke.
 *
 * Usage: node search-e2e.mjs <web-base> <api-base>
 */

import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";
import axe from "axe-core";

const BASE = (process.argv[2] ?? "").replace(/\/$/, "");
const API = (process.argv[3] ?? "").replace(/\/$/, "");
if (!BASE || !API) {
  console.error("search-e2e: usage: node search-e2e.mjs <web-base> <api-base>");
  process.exit(2);
}

/* ---------- Assertions ---------- */

let failures = 0;
let checks = 0;

function ok(where, what) {
  checks += 1;
  console.log(`  ok   ${where}: ${what}`);
}

function fail(what, detail) {
  failures += 1;
  console.error(`  FAIL ${what}${detail ? ` — ${detail}` : ""}`);
}

function check(where, what, condition, detail) {
  if (condition) ok(where, what);
  else fail(`${where}: ${what}`, detail);
}

function eq(where, what, got, want) {
  check(where, what, JSON.stringify(got) === JSON.stringify(want), `got ${JSON.stringify(got)}, want ${JSON.stringify(want)}`);
}

/** Compare two lists as SETS, for the assertions that must not depend on order. */
function setEq(where, what, got, want) {
  const a = [...got].sort();
  const b = [...want].sort();
  eq(where, what, a, b);
}

/* ---------- The expected answer, written out ---------- */

/**
 * The fixture, duplicated from mock-api.mjs on purpose: the spec states what
 * it expects to see rather than asking the mock what it sent, so a change to
 * the fixture that this file does not know about fails instead of silently
 * agreeing.
 */
const Q = "which MOF materials show CO2 uptake above 3 mmol/g at 298 K, and has that been reproduced?";
const Q2 = "what limits lithium-sulfur cycling stability?";

const SUMMARY =
  "Two sources bear directly on the question. Mg-MOF-74 is reported at 3.4 mmol/g CO2 uptake at 298 K, and a second finding records a 4% loss of uptake after ten cycles.";

const REFS = [
  "asset:AST-0001@2",
  "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1",
  "document:KNW-0007@3",
  "asset:AST-0044@1",
  "release:REL-0011",
];

/** The two the document marks cited — deliberately NOT the first two. */
const CITED_REFS = [
  "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1",
  "document:KNW-0007@3",
];

const CITATIONS = [
  "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1",
  "document:KNW-0007@3",
];

/** rank -> the address the answer gives, and the address it does NOT (rank 2). */
const HREFS = {
  1: "/api/v1/assets/AST-0001?version=2",
  3: "/api/v1/knowledge/KNW-0007",
  4: "/api/v1/assets/AST-0044?version=1",
  5: "/api/v1/projects/3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d/releases/REL-0011",
};
const SOURCE_WITHOUT_ADDRESS = "object_version:1d2c3b4a-5e6f-4071-8293-a4b5c6d7e8f9@1";

const LIMITATIONS = [
  "the vector signal did not run: no embedder is configured, so the question could not be embedded",
  "1 source is not asserted about by anything the platform has recorded: its version is listed but nothing has been asserted for or against it",
];
const CONFLICTS = ["this source is contested: 1 evidence assertion contradicts it"];

/** The platform's own sentence per reason (internal/search/answer/derive.go). */
const FALLBACK_REASONS = [
  "no_provider",
  "no_sources",
  "provider_error",
  "provider_timeout",
  "invalid_answer",
  "ungrounded_citation",
];
const FALLBACK_TEXT = {
  no_provider:
    "no answer model is configured on this deployment, so this answer is the structured result: the sources the search returned, and what the platform has recorded about them",
  no_sources: "the search returned no source, so there is nothing a written answer could cite",
  provider_error: "the answer model could not be reached, so this answer is the structured result",
  provider_timeout: "the answer model did not answer within the time allowed, so this answer is the structured result",
  invalid_answer:
    "the answer model returned a document that does not satisfy the answer schema, so it was refused and this answer is the structured result",
  ungrounded_citation:
    "the answer model cited or named an entity the search did not return, so its document was refused and this answer is the structured result",
};

/* ---------- Harness plumbing ---------- */

const WEB_DIR = path.resolve(import.meta.dirname, "../../apps/web");

async function setMode(mode) {
  const res = await fetch(`${API}/__mode?set=${encodeURIComponent(mode)}`);
  if (!res.ok) throw new Error(`setMode(${mode}) -> ${res.status}`);
  return res.json();
}

async function stats() {
  const res = await fetch(`${API}/__stats`);
  if (!res.ok) throw new Error(`stats -> ${res.status}`);
  return res.json();
}

async function open(page, query) {
  await page.goto(`${BASE}/search?q=${encodeURIComponent(query)}`, { waitUntil: "load" });
}

async function texts(page, selector) {
  return page.locator(selector).allTextContents().then((all) => all.map((t) => t.replace(/\s+/g, " ").trim()));
}

async function attrs(page, selector, attribute) {
  return page.locator(selector).evaluateAll(
    (nodes, attr) => nodes.map((node) => node.getAttribute(attr)),
    attribute,
  );
}

/** The page's own rendered text, for the "nothing invented" assertions. */
async function bodyText(page) {
  return (await page.locator("main").innerText()).replace(/\s+/g, " ");
}

/** What is carrying role=alert, for a failure that has to be diagnosable. */
async function alertDump(page) {
  const nodes = await page.locator('[role="alert"]').evaluateAll((all) =>
    all.map((n) => `${n.tagName.toLowerCase()}.${n.className || "-"}: ${(n.textContent || "").slice(0, 60)}`),
  );
  return JSON.stringify(nodes);
}

async function axeScan(page, label) {
  await page.addScriptTag({ content: axe.source });
  const violations = await page.evaluate(async () => {
    const r = await window.axe.run(document, {
      runOnly: { type: "tag", values: ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa", "best-practice"] },
    });
    return r.violations.map((v) => ({ id: v.id, impact: v.impact, targets: v.nodes.map((n) => n.target.join(" ")) }));
  });
  if (violations.length === 0) {
    ok(`axe ${label}`, "0 violations");
    return;
  }
  for (const v of violations) {
    fail(`axe ${label}: ${v.id} [${v.impact}]`, v.targets.join(" | "));
  }
}

/* ---------- The run ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
const page = await context.newPage();

try {
  /* ===== 1. the answered state ===== */

  console.log("\n== answered ==");
  await setMode("answered");
  await open(page, Q);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });

  const t = (s) => `[data-search-state="ready"] ${s}`;
  eq("answered", "the answer is the answered one", await page.locator("[data-search-answer]").getAttribute("data-status"), "answered");
  check("answered", "the question is rendered from the URL before any script runs",
    (await page.locator("[data-search-query]").textContent())?.trim() === Q);
  eq("answered", "the summary is the document's, verbatim", (await page.locator(t("[data-search-summary]")).textContent())?.trim(), SUMMARY);
  eq("answered", "the summary carries the View label", (await page.locator(t("[data-search-view-badge]")).textContent())?.trim(), "View");
  eq("answered", "the citations are rendered in the document's order", await texts(page, t("[data-search-citation]")), CITATIONS);

  // The cited rows: by ref, not by position.
  const citedRows = await attrs(page, t("[data-search-cited-ref]"), "data-search-cited-ref");
  setEq("answered", "the cited sources are exactly the document's cited refs", citedRows, CITED_REFS);
  const flagged = await page.locator(t('[data-search-source-ref][data-cited="true"]')).evaluateAll((n) => n.map((e) => e.getAttribute("data-search-source-ref")));
  setEq("answered", "the same rows carry cited=true in the results", flagged, CITED_REFS);
  eq("answered", "exactly two sources are cited", citedRows.length, 2);

  // The underlying results: every source, in rank order, unre-ordered.
  const listed = await attrs(page, t("[data-search-source-ref]"), "data-search-source-ref");
  eq("answered", "every source is listed, in the ranking's order", listed, REFS);
  eq("answered", "the result count is the list's own length", (await page.locator(t('[data-search-section="results"] .search-count')).textContent())?.trim(), String(REFS.length));

  // Limitations and conflicts, verbatim.
  eq("answered", "the limitations are the document's, verbatim", await texts(page, t('[data-search-section="limitations"] .search-statement-text')), LIMITATIONS);
  eq("answered", "the conflicts are the document's, verbatim", await texts(page, t('[data-search-section="conflicts"] .search-statement-text')), CONFLICTS);

  // The evidence map, projected from the statements' own refs.
  const evidenceRows = await page.locator(t('[data-search-section="evidence-map"] .search-row')).count();
  eq("answered", "the evidence map has one row per statement that named a source", evidenceRows, 2);
  const evidenceRefs = await attrs(page, t('[data-search-section="evidence-map"] [data-search-source-link]'), "data-search-source-link");
  setEq("answered", "the evidence map points at the sources the statements named", evidenceRefs, ["asset:AST-0044@1", "asset:AST-0001@2"]);

  // A source with no address is text, never a link.
  const linkRefs = await attrs(page, t("[data-search-source-link]"), "data-search-source-link");
  check("answered", "the source the answer gave no address for is not a link", !linkRefs.includes(SOURCE_WITHOUT_ADDRESS), JSON.stringify(linkRefs));

  // Every href is the answer's own address, resolved against the API origin.
  for (const [rank, href] of Object.entries(HREFS)) {
    const ref = REFS[Number(rank) - 1];
    const got = await page
      .locator(t(`[data-search-section="results"] [data-search-source-link="${ref}"]`))
      .getAttribute("href");
    eq("answered", `source #${rank} links to the address the answer gave it`, got, `${API}${href}`);
  }

  await axeScan(page, "/search (answered)");

  /* ===== 2. the URL is where the question lives ===== */

  console.log("\n== the URL is the question's address ==");
  const id1 = await page.locator("[data-search-answer]").getAttribute("data-search-id");
  check("url", "the answer names the search it came from", typeof id1 === "string" && id1.length > 0, String(id1));
  const afterOpen = await stats();
  eq("url", "the question the API was asked is the one in the URL", afterOpen.lastQuery, Q);
  await page.reload({ waitUntil: "load" });
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  const id2 = await page.locator("[data-search-answer]").getAttribute("data-search-id");
  eq("url", "a reload renders the same search's answer", id2, id1);

  const otherTab = await context.newPage();
  await otherTab.goto(`${BASE}/search?q=${encodeURIComponent(Q)}`, { waitUntil: "load" });
  await otherTab.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  const id3 = await otherTab.locator("[data-search-answer]").getAttribute("data-search-id");
  eq("url", "a new tab on the same URL renders the same search's answer", id3, id1);
  await otherTab.close();

  await open(page, Q2);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  const id4 = await page.locator("[data-search-answer]").getAttribute("data-search-id");
  check("url", "a different question is a different search", id4 !== id1, `both ${id1}`);
  eq("url", "the second question is the one the API was asked", (await stats()).lastQuery, Q2);
  check("url", "the second question is rendered on the page", (await page.locator("[data-search-query]").textContent())?.trim() === Q2);

  /* ===== 3. every source is reachable ===== */

  console.log("\n== a source opens ==");
  await setMode("answered");
  await open(page, Q);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  const clickRef = REFS[0];
  const link = page.locator(`[data-search-section="results"] [data-search-source-link="${clickRef}"]`);
  const [response] = await Promise.all([
    page.waitForNavigation({ waitUntil: "load", timeout: 15000 }),
    link.click(),
  ]);
  eq("source click", "clicking a source lands on a 200", response?.status(), 200);
  const landed = new URL(page.url());
  eq("source click", "it lands on the address the answer gave", `${landed.pathname}${landed.search}`, HREFS[1]);
  check("source click", "the API origin really served that read", (await stats()).reads.includes(landed.pathname), JSON.stringify((await stats()).reads));

  /* ===== 4. keyboard ===== */

  // The tab walk starts at the first source link of the underlying results
  // (which sit below the cited sources and the evidence map, so the links the
  // walk picks up are that section's), and Tab is pressed from there. Nothing
  // is asserted about ADJACENCY: the row's ranking-factors disclosure is a
  // tab stop between two rows, and a walk that only passed on a lucky layout
  // would be a walk that proves nothing. What is asserted is that every
  // source link is reached, in the document's order.
  console.log("\n== keyboard ==");
  await open(page, Q);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  const resultsLinks = page.locator('[data-search-section="results"] [data-search-source-link]');
  const linkCount = await resultsLinks.count();
  check("keyboard", "the addressed sources are links", linkCount === 4, `got ${linkCount} links`);
  await resultsLinks.nth(0).focus();
  const walked = [await page.evaluate(() => document.activeElement?.getAttribute("data-search-source-link"))];
  for (let i = 0; i < 16; i += 1) {
    await page.keyboard.press("Tab");
    const ref = await page.evaluate(() => document.activeElement?.getAttribute("data-search-source-link"));
    if (ref !== null && ref !== undefined && !walked.includes(ref)) walked.push(ref);
  }
  eq("keyboard", "Tab reaches every source link, in order", walked, [REFS[0], REFS[2], REFS[3], REFS[4]]);
  await resultsLinks.nth(1).focus();
  const [kbResponse] = await Promise.all([
    page.waitForNavigation({ waitUntil: "load", timeout: 15000 }),
    page.keyboard.press("Enter"),
  ]);
  eq("keyboard", "Enter opens a source", kbResponse?.status(), 200);

  /* ===== 5. the six fallbacks ===== */

  console.log("\n== fallback (the platform declining, not failing) ==");
  const headlines = [];
  for (const reason of FALLBACK_REASONS) {
    await setMode(`fallback-${reason}`);
    await open(page, Q);
    await page.waitForSelector(`[data-search-fallback="${reason}"]`, { timeout: 15000 });
    const where = `fallback ${reason}`;
    const headline = (await page.locator("[data-search-fallback-headline]").textContent())?.replace(/\s+/g, " ").trim();
    headlines.push(headline);
    check(where, "it has a sentence of its own", typeof headline === "string" && headline.length > 0, String(headline));
    eq(where, "the reason code is named", (await page.locator("[data-search-fallback-reason]").textContent())?.trim(), reason);
    eq(where, "the platform's own sentence is rendered verbatim", await texts(page, '[data-search-section="limitations"] .search-statement-text'), [FALLBACK_TEXT[reason]]);
    check(where, "no summary is drawn", (await page.locator('[data-search-section="answer"]').count()) === 0);
    // Scoped to <main>: Next's App Router keeps its own route announcer in
    // the body (role="alert"), which is about navigation and says nothing
    // about this answer.
    const alerts = await page.locator('main [role="alert"]').count();
    check(where, "a fallback is NOT an error", alerts === 0, await alertDump(page));
    check(where, "the fallback itself carries no alert role", (await page.locator("[data-search-fallback][role='alert']").count()) === 0);
    check(where, "no error state is drawn", (await page.locator('[data-search-state="error"]').count()) === 0);
    const expectedSources = reason === "no_sources" ? 0 : REFS.length;
    eq(where, "the structured result is the whole result", await page.locator('[data-search-section="results"] [data-search-source-ref]').count(), expectedSources);
    eq(where, "no source is cited", await page.locator('[data-search-cited-mark]').count(), 0);
  }
  eq("fallback", "the six reasons are six different sentences", new Set(headlines).size, FALLBACK_REASONS.length);
  const doubled = headlines.filter((h, i) => headlines.indexOf(h) !== i);
  check("fallback", "no two reasons share a sentence", doubled.length === 0, JSON.stringify(doubled));

  await setMode("fallback-no_provider");
  await open(page, Q);
  await page.waitForSelector('[data-search-fallback="no_provider"]', { timeout: 15000 });
  await axeScan(page, "/search (fallback)");

  /* ===== 6. errors ===== */

  console.log("\n== errors ==");
  const errorCases = [
    ["error-503", "SEARCH_UNAVAILABLE", true],
    ["error-401", "SEARCH_UNAUTHENTICATED", false],
    ["error-400", "SEARCH_INVALID_REQUEST", false],
  ];
  const errorTexts = [];
  for (const [mode, code, retryable] of errorCases) {
    await setMode(mode);
    await open(page, Q);
    await page.waitForSelector('[data-search-state="error"]', { timeout: 15000 });
    const where = `error ${code}`;
    eq(where, "the state is an alert carrying the contract's code", await page.locator("[data-search-error]").getAttribute("data-search-error"), code);
    check(where, "it is announced as an alert", (await page.locator('main [role="alert"]').count()) === 1, await alertDump(page));
    check(where, "no answer is drawn", (await page.locator("[data-search-answer]").count()) === 0);
    const retry = await page.locator(".search-retry").count();
    eq(where, "the retry offer follows the envelope's retryable flag", retry, retryable ? 1 : 0);
    errorTexts.push((await page.locator("[data-search-error]").textContent())?.replace(/\s+/g, " ").trim());
  }
  eq("errors", "each code gets its own sentence", new Set(errorTexts).size, errorCases.length);

  /* ===== 7. loading ===== */

  console.log("\n== loading ==");
  await setMode("slow");
  await open(page, Q);
  const loadingSeen = await page
    .waitForSelector("[data-search-loading]", { timeout: 5000 })
    .then(() => true)
    .catch(() => false);
  check("loading", "the question is shown as being searched before an answer exists", loadingSeen);
  check("loading", "the loading state is a status, not an error", (await page.locator('[data-search-loading][role="status"]').count()) === 1);
  check("loading", "the question is on the page while loading", (await page.locator("[data-search-query]").textContent())?.trim() === Q);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  ok("loading", "the answer replaces the loading state");

  /* ===== 8. nothing is invented ===== */

  console.log("\n== nothing is invented ==");
  await setMode("sparse");
  await open(page, Q);
  await page.waitForSelector('[data-search-state="ready"]', { timeout: 15000 });
  eq("sparse", "an empty conflicts list renders NO conflicts section", await page.locator('[data-search-section="conflicts"]').count(), 0);
  eq("sparse", "a statement with no refs renders NO evidence map", await page.locator('[data-search-section="evidence-map"]').count(), 0);
  eq("sparse", "with nothing cited there is NO cited-sources section", await page.locator('[data-search-section="sources"]').count(), 0);
  eq("sparse", "no source is marked cited", await page.locator('[data-search-cited-mark]').count(), 0);
  eq("sparse", "an empty labels list renders no label chips", await page.locator(".search-label").count(), 0);
  eq("sparse", "no citations are rendered", await page.locator("[data-search-citation]").count(), 0);
  const sparseText = await bodyText(page);
  for (const placeholder of ["No conflicts", "No limitations", "No sources", "not available", "undefined", "null", "TBD", "0 results"]) {
    check("sparse", `the page does not say "${placeholder}"`, !sparseText.includes(placeholder), sparseText.slice(0, 200));
  }
  check("sparse", "the answer itself is still rendered", (await page.locator("[data-search-answer]").count()) === 1);

  // The app's own source files must not contain the fixture's data: a page
  // that had baked a title, a ref or a number into itself would still pass
  // the checks above on the day the mock happened to agree with it.
  const ROOT = path.resolve(import.meta.dirname, "../..");
  const appFiles = [
    "apps/web/lib/search.ts",
    "apps/web/app/(main)/search/page.tsx",
    "apps/web/app/(main)/search/search-answer.tsx",
  ].map((rel) => path.join(ROOT, rel));
  const fixtureStrings = [
    "Mg-MOF-74",
    "AST-0001",
    "KNW-0007",
    "AST-0044",
    "REL-0011",
    "3.4 mmol/g",
    "4% loss",
    "independently_reproduced",
    "3f2a5b7c-1d4e-4a6b-8c9d-0e1f2a3b4c5d",
  ];
  for (const file of appFiles) {
    const src = fs.readFileSync(file, "utf8");
    const found = fixtureStrings.filter((s) => src.includes(s));
    check("source scan", `${path.relative(WEB_DIR, file)} carries no fixture data`, found.length === 0, JSON.stringify(found));
  }

  /* ===== 9. no question, still a page ===== */

  console.log("\n== /search with no question ==");
  // setMode resets the mock's counters, so "no request was made" is a fact
  // about this navigation rather than about the whole run.
  await setMode("answered");
  await page.goto(`${BASE}/search`, { waitUntil: "load" });
  eq("bare", "the page is served", (await page.locator("h1").first().textContent())?.trim(), "Search");
  eq("bare", "no answer is requested without a question", (await stats()).searchCalls, 0);
  check("bare", "no error is drawn for a page that asked nothing", (await page.locator('main [role="alert"]').count()) === 0);

  /* ===== 10. a failure is not emptiness ===== */

  console.log("\n== a truncated answer is a failure ==");
  await setMode("malformed");
  await open(page, Q);
  // Waited for by settlement rather than by the expected selector: the
  // question is which state the page ended in, and a wait for the error state
  // would report its own timeout instead of the state that was actually
  // rendered.
  const malformedState = await page
    .waitForFunction(
      () => {
        const state = document.querySelector("[data-search-state]")?.getAttribute("data-search-state");
        return state === "error" || state === "ready" ? state : false;
      },
      undefined,
      { timeout: 8000 },
    )
    .then((handle) => handle.jsonValue())
    .catch(() => null);
  eq("malformed", "an answer missing its lists is a failure, not an empty answer", malformedState, "error");
  const malformedCode =
    (await page.locator("[data-search-error]").count()) === 1
      ? await page.locator("[data-search-error]").getAttribute("data-search-error")
      : null;
  eq("malformed", "the failure is the shape guard's", malformedCode, "MALFORMED_ANSWER");
  eq("malformed", "no results section is drawn for it", await page.locator('[data-search-section="results"]').count(), 0);
} catch (err) {
  fail("harness", err && err.stack ? err.stack : String(err));
} finally {
  await browser.close();
}

console.log(`\nsearch-e2e: ${checks} checks, ${failures} failure(s)`);
process.exit(failures === 0 ? 0 : 1);
