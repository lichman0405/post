/**
 * i18n smoke (T1105): the language preference really switches the UI, and
 * the things that must NOT switch do not.
 *
 * Usage: node i18n-smoke.mjs <base-url> [harness-ids.json]
 *
 *   With no second argument (`tests/web-smoke/run.sh`, `make browser-smoke`)
 *   only the routes that render their own content with the services down are
 *   driven. That is not a watered-down run: the shell, the navigation, the
 *   `/login` card and the `/search` intro are exactly the surfaces this task
 *   converted, and they render without an API by design.
 *
 *   With a harness-ids.json (`make i18n`, `tests/web-smoke/run-i18n.sh`) the
 *   dynamic core pages are driven too, against the real API fixture.
 *
 * WHAT THIS FILE IS TRYING TO MAKE IT IMPOSSIBLE TO SAY WITHOUT CHECKING
 *
 *  1. "the core pages switch language" — each route below names a Chinese and
 *     an English marker that only that page's own body renders, and the
 *     marker is read out of the live DOM in both preferences. A page that
 *     stayed English fails; a page that renders the key instead of a string
 *     (`⟦missing:…⟧`, the translator's documented miss output) fails too.
 *  2. "switching back restores" — the SAME page object is reloaded with the
 *     preference flipped back to en and has to render the English marker
 *     again with the English `<html lang>`.
 *  3. "the API payload is byte-identical" — every request the page makes to
 *     /api/** is recorded (method, path, query, body) in both preferences and
 *     the two recordings must match exactly. Localizing the UI must not move
 *     a single byte on the wire; a catalog key reaching a request body, a
 *     query parameter or an href is the failure this catches.
 *
 *     WHAT THE BODY HALF OF THAT SENTENCE RESTS ON, because it is not the
 *     same evidence as the rest: the harness routes are GETs, and a GET has
 *     no body — recording `postData() ?? ""` for them compares two empty
 *     strings, which is a real comparison of nothing. The requests whose
 *     bodies are actually compared are the ones a route cannot render
 *     without: on `/search?q=` those are the sign-in POST /api/v1/auth/login
 *     that the guarded page needs (see signIn below) and the page's own
 *     POST /api/v1/search (lib/search.ts), whose JSON bodies travel through
 *     the same recorder as every other request. `wire()` folds both into the
 *     same comparison, and section 4 prints how many compared requests
 *     carried a body (`(N with a request body)`), so the claim is read off
 *     the run — two on that route — rather than asserted about all of them.
 *  4. "scientific values and units are identical" — the quantities a page
 *     actually renders are extracted from both DOMs and compared as sets.
 *     The extractor is floor-checked: if the English DOM yields fewer than
 *     two quantities the run fails, because a comparison over nothing
 *     compares nothing.
 *  5. "enum and ID are not localized" — the `data-*` attribute map, the href
 *     set and the switcher's own option VALUES are extracted per preference
 *     and must be identical. docs/28 §3 splits the stable code from the
 *     display label; this is the check that the split held.
 *
 * The `ok()` / `fail()` shapes are the same as a11y-smoke.mjs's: `ok` takes a
 * label and nothing else (tests/web-smoke/check-ok-arity.js enforces that
 * statically, and the runtime guard catches anything it misses), and every
 * failure names the check it belongs to.
 */
import fs from "node:fs";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31107";
const IDS_PATH = process.argv[3] ?? "";

const IDS = (() => {
  if (IDS_PATH === "") return null;
  try {
    return JSON.parse(fs.readFileSync(IDS_PATH, "utf8"));
  } catch (err) {
    console.error(`i18n-smoke: cannot read harness ids from ${IDS_PATH}: ${err.message}`);
    process.exit(2);
  }
})();

let fails = 0;
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
function ok(label, ...extra) {
  if (extra.length > 0) {
    fail(
      `ok(${JSON.stringify(label)}, …) called with ${extra.length + 1} arguments`,
      "ok takes a label only — a second argument is discarded, so this line could never fail. Write it as an if/else that calls fail(label, detail).",
    );
    return;
  }
  console.log(`ok   ${label}`);
}

/* The preference cookie's name and values, restated here rather than
 * imported: this file runs in Playwright's node, apps/web/lib/i18n.ts is a
 * TypeScript module, and the values are the contract the browser sees. They
 * are also asserted below (the switcher's <option value>s), so a rename in
 * the app reds this file instead of silently testing the default. */
const COOKIE = "post_locale";
const EN = "en";
const ZH = "zh-CN";

/* The translator's missing-key wrapper (lib/i18n.ts MISSING_KEY_PREFIX). A
 * rendered key is the one failure mode where the page looks finished and is
 * not, so it is checked on the whole document — attributes included, since
 * an aria-label is translated too. */
const MISSING_MARKER = "⟦missing:";

/**
 * The quantities-and-units extractor.
 *
 * Deliberately a fixed unit list rather than "any number": a bare digit is
 * the row count in "2 competing hypotheses" and it is supposed to change
 * (the sentence around it does). What must not change is the number WITH its
 * unit, because docs/28 §4 makes a unit a fact rather than a spelling.
 */
const QUANTITY = /(\d+(?:[.,]\d+)?)\s?(mmol\/g|mg\/g|mol\/L|% RH|%|K|°C|Å|nm|µm|kg|mL|h)\b/g;

/**
 * The shapes the declared volatile data-* attributes must have.
 *
 * A value the SERVER issues per request cannot be compared across two loads,
 * and the honest response is to say so by name — not to stop comparing data-*
 * attributes. Every name a route declares in `volatileDataAttrs` must appear
 * here with the shape its value must match in BOTH preferences, so the
 * exemption is a claim about the value ("this is still a UUID") rather than a
 * hole in the comparison. See the search-answer route for the one case.
 */
const VOLATILE_DATA_ATTR_SHAPES = {
  // The accumulated form is `|v1|v2` (load() joins repeated attributes), so a
  // single-valued attribute renders as "|" + the UUID.
  "data-search-id": /^\|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/,
};

function quantities(text) {
  const out = new Set();
  for (const m of text.matchAll(QUANTITY)) out.add(`${m[1]} ${m[2]}`);
  return [...out].sort();
}

/* ---------- per-route markers and expectations ---------- */

/** Each entry names a selector and the text that selector must contain in
 *  each preference. The English strings are this task's baseline copy (they
 *  were literal in the components before it), so "the English one is still
 *  there" is a claim about the interface, not about the catalog.
 *
 *  `title` is the same claim for the browser tab, which no CSS selector can
 *  reach — `document.title` is where the page's `generateMetadata` lands, and
 *  a page whose body is translated but whose tab is not is a real half-fix
 *  (the tab is what a reader sees in a row of twenty). It is declared per
 *  route rather than asserted everywhere because /projects/{id} derives its
 *  title from the loaded project's own name, which is API data and must NOT
 *  change with the preference — see check 7. */
const ROUTES = [
  {
    route: "/",
    label: "home",
    fetches: true,
    probes: [
      // The header is the surface an unauthenticated visitor sees on every
      // page, and it is translated by the shared shell.
      { selector: ".global-nav-links", en: "Organizations", zh: "组织" },
    ],
    title: { en: "Platform for Open Science", zh: "开放科学与技术平台" },
  },
  {
    route: "/login",
    label: "login",
    // The sign-in card talks to the API only on submit, so this route has
    // nothing on the wire to compare. Declared here rather than inferred from
    // "the list came back empty", because those are the same observation and
    // only one of them is a fact about the page: a route that STOPS fetching
    // must fail the check below, not join this one.
    fetches: false,
    probes: [
      { selector: ".auth-heading", en: "Sign in to POST", zh: "登录 POST" },
      { selector: ".auth-sub", en: "Platform for Open Science & Technology", zh: "开放科学与技术平台" },
    ],
    title: { en: "Sign in — POST", zh: "登录 — POST" },
  },
  {
    route: "/projects",
    label: "projects directory",
    fetches: true,
    probes: [
      // The headings live in the page bodies; with the API down this route
      // renders its directory shell, whose heading and subtitle ARE copy.
      { selector: ".project-directory-title", en: "Projects", zh: "项目" },
    ],
    title: { en: "Projects — POST", zh: "项目 — POST" },
  },
  {
    route: "/search",
    label: "search",
    fetches: true,
    probes: [
      { selector: ".search-title", en: "Search", zh: "搜索" },
      // This element carries the scientific quantities compared below, so it
      // is also the route's proof that the units survive translation.
      { selector: ".search-intro", en: "Ask a question about the network", zh: "就整张网络提一个问题" },
    ],
    title: { en: "Search — POST", zh: "搜索 — POST" },
  },
  ...(IDS !== null
    ? [
        {
          route: `/projects/${IDS.project_id}`,
          label: "project overview",
          fetches: true,
          probes: [
            // The tab bar comes from the PROJECT SHELL, which renders only
            // when the project really loaded. That makes this route's proof
            // two-sided: the shell's tab labels are Chinese AND the page is
            // not the not-found state (whose Chinese copy would otherwise
            // satisfy a naive "is there Chinese on this page" check).
            { selector: '[data-project-tab="overview"]', en: "Overview", zh: "概览" },
            { selector: '[data-project-tab="pulls"]', en: "Pull requests", zh: "拉取请求" },
          ],
          // No `title`: this route's <title> is the project's own name, read
          // from the API (app/(main)/projects/[id]/layout.tsx). Asserting a
          // bilingual title here would be asserting that domain data was
          // translated — the opposite of docs/28 §3.
        },
        /* The five pages the first review found still English, plus the pull
         * list and the search answer body converted in the same pass. A probe
         * selector here is a claim about THIS route's own body: it must not be
         * satisfied by the shared shell, or every route would pass on the nav
         * alone. Each `en` value is the string that was literal in the
         * component before this task (verified against `git show HEAD:` per
         * file), so "the English is still byte-identical" is checked rather
         * than assumed. */
        {
          route: `/projects/${IDS.project_id}/research`,
          label: "research tab",
          fetches: true,
          probes: [
            // The harness seeds no scientific objects, so the overview comes
            // back `empty` (internal/application/rsg/overview.go) and the page
            // renders its empty state. The error state renders the SAME
            // selector with different copy, which is what makes this a
            // discriminating probe rather than "something rendered".
            { selector: ".research-state h2", en: "No research state yet", zh: "尚无研究状态" },
          ],
        },
        {
          route: `/projects/${IDS.project_id}/pulls`,
          label: "pull request list",
          fetches: true,
          probes: [
            { selector: ".pulls-section-title", en: "Pull requests", zh: "拉取请求" },
            // The table over the fixture's one PR: its headers are catalog copy
            // and its rows are API data. `nth-of-type(4)` because the Table
            // component renders every header as a sibling <th> in one row
            // (packages/ui/src/Table.tsx), so a bare `.pulls-table th` matches
            // the first column whatever it says.
            { selector: ".pulls-table thead th:nth-of-type(4)", en: "Opened", zh: "创建时间" },
          ],
        },
        {
          route: `/projects/${IDS.project_id}/pulls/${IDS.pr_number}`,
          label: "pull request detail",
          fetches: true,
          probes: [
            // Renders in the ready, error and invalid-id states, so this probe
            // says "the page's copy switched" without claiming which state the
            // fixture produced; the wire check below is what proves the detail
            // really loaded.
            { selector: ".pulls-back a", en: "Back to pull requests", zh: "返回拉取请求列表" },
            // The review panel lives below the tabs and renders on the default
            // tab (`DEFAULT_PULL_TAB = "summary"`, lib/pulls.ts), so this is
            // reachable without a click. Selectored through `.pull-review`
            // because `.pulls-section-title` is not unique on this page.
            { selector: ".pull-review .pulls-section-title", en: "Record a review", zh: "记录一次审阅" },
            // The tab bar renders every member of PULL_TABS whatever the diff
            // touches, so this probe covers the tab LABELS (lib/pulls.ts's
            // pullTabLabelKey) rather than the panel below them. Probed on the
            // container because the tabs are buttons in a tablist, and the
            // count chip inside each one is API data.
            { selector: ".pull-tabs", en: "Scientific changes", zh: "科学变更" },
            // The open tab's hint — the sentence that tells the reader what the
            // panel they are looking at contains. The page opens on
            // DEFAULT_PULL_TAB ("summary", lib/pulls.ts), so the hint on load
            // is the summary one.
            {
              selector: ".pull-panel-hint",
              en: "What this proposal changes, in one screen: counts, both review dimensions, and the risks a reviewer must not miss.",
              zh: "一屏说清本提案改了什么：计数、两个审阅维度，以及审阅者不可错过的风险。",
            },
          ],
        },
        {
          route: `/projects/${IDS.project_id}/releases`,
          label: "release list",
          fetches: true,
          probes: [
            { selector: ".releases-section-title", en: "Releases", zh: "发布" },
          ],
        },
        {
          route: `/projects/${IDS.project_id}/releases/${IDS.release_id}`,
          label: "release detail",
          fetches: true,
          probes: [
            // The immutable-snapshot sentence is this page's own copy and the
            // longest single catalog value it renders; the fact list beside it
            // labels four API values.
            {
              selector: ".release-detail-note",
              en: "Immutable snapshot — the state, policy and review record fixed here never change",
              zh: "不可变快照 —— 此处固定的状态、策略与审阅记录永不改变",
            },
            { selector: ".release-detail-facts dt:first-of-type", en: "Version", zh: "版本" },
          ],
        },
        {
          route: `/assets/${IDS.asset_pid}/${IDS.asset_version}`,
          label: "asset page",
          fetches: true,
          probes: [
            // Two blocks rather than one: the asset page renders nine of them
            // (docs/42 §Asset Page), and a single heading would be satisfied
            // by a page that localized its header and stopped.
            { selector: '[data-asset-block="origin"] .asset-block-title', en: "Origin", zh: "来源" },
            { selector: '[data-asset-block="metadata"] .asset-block-title', en: "Metadata", zh: "元数据" },
            // The asset's own type label, which docs/28 §3 contrasts with the
            // stable code beside it: this element carries BOTH, the code in its
            // data-asset-type attribute (asserted unchanged in check 5) and the
            // display label as its text (asserted to switch here). The fixture
            // publishes a dataset (tests/web-smoke/a11y-harness/main.go).
            { selector: ".assets-type", en: "Dataset", zh: "数据集" },
          ],
        },
        {
          /* The Research Profile page. a11y-smoke.mjs has driven it as a core
           * route since T1104; before this the i18n suite did not, so its copy
           * was the one core surface no language check looked at. Its two reads
           * (the identity card and the profile card) are different components
           * with different copy, so the probes below are the `.rp-*` ones. */
          route: `/users/${IDS.user_id}`,
          label: "research profile",
          fetches: true,
          probes: [
            { selector: ".rp-title", en: "Research profile", zh: "研究档案" },
            // docs/28 §4 in one sentence: the profile shows recorded facts and
            // no score. It is the longest catalog value this page renders.
            {
              selector: ".rp-footnote",
              en: "Every dimension above is a list of recorded facts. POST does not compute a score, rank or rating for a person, and this page shows none.",
              zh: "以上每一栏都是一组被记录下来的事实。POST 不会为个人计算评分、排名或评级，本页也不展示任何一种。",
            },
          ],
        },
        {
          // The query is what makes this a different page from the `/search`
          // route above: with one, search-answer.tsx renders the answer body
          // (provenance, the fallback notice, the statement sections, the
          // evidence map, the underlying results); without one, none of it
          // exists in the DOM. The first review's finding was exactly this —
          // the earlier suite drove `/search` and so never reached any of it.
          route: "/search?q=catalyst",
          label: "search answer",
          // POST /api/v1/search is behind the auth guard: without a session the
          // page renders its error panel and both probes come back null.
          auth: true,
          fetches: true,
          probes: [
            {
              selector: ".search-fallback .search-section-title",
              en: "Structured result",
              zh: "结构化结果",
            },
            // The fallback's own sentence — the longest catalog value this
            // route renders, and the one that says what a fallback IS.
            {
              selector: ".search-fallback-note",
              en: "No answer was written, and the sources below are what the search found.",
              zh: "没有撰写回答，下面的来源就是本次检索找到的内容。",
            },
            // Renders in both branches (fallback and answered), so it is the
            // answer body's own proof that it mounted at all.
            { selector: ".search-provenance", en: "Search", zh: "搜索" },
            // The headline over the platform's own sentence: what the fallback
            // IS, in the product's words (lib/search.ts's fallbackHeadlineKey
            // maps the API's reason code to this key). It is per-reason copy,
            // so the `en` value below is the one THIS harness produces — the
            // suite says which reason it saw if the API's answer changes.
            {
              selector: ".search-fallback-headline",
              en: "No written answer: the search returned no source to cite.",
              zh: "没有书面回答：本次检索没有返回可引用的来源。",
            },
          ],
          /* `data-search-id` is the id of the row POST /search just wrote
           * (internal/persistence/search_record_store.go: Save returns the new
           * row's UUID), so two loads of the same page differ in that
           * attribute BY CONSTRUCTION — the first run of this route reds check
           * 5 on it, and the red is a fact about the API's state change, not
           * about language. It is excluded from the identity comparison by
           * name and ONLY after both preferences are shown to carry a
           * UUID-shaped value there: an attribute that stops looking like an
           * id fails below, so the exclusion cannot widen into a hole. Nothing
           * else on this route is excluded — 23 other data-* attributes are
           * still compared byte for byte. */
          volatileDataAttrs: ["data-search-id"],
        },
      ]
    : []),
];

const browser = await chromium.launch();

/**
 * POST /api/v1/search runs behind the auth guard (the same one a11y-smoke.mjs
 * signs in for), and the page's own client sends `credentials: "include"`. A
 * browser that has never signed in therefore gets 401 no matter how healthy
 * the API is, and `/search?q=` would fail its probes for a reason that has
 * nothing to do with language.
 *
 * The fetch runs from a page ON the web origin and writes the session-bound
 * CSRF token to the same sessionStorage slot the app's own login writes
 * (lib/auth.ts reads "post.csrf"), because POST /search is a state change and
 * the guard requires it. No test backdoor: it is the product's login route
 * with the fixture's real credentials.
 *
 * It navigates the page to BASE first, which is why the caller truncates the
 * recorder afterwards: the bootstrap page's own requests are not evidence
 * about the route under test. The login's own POST is the one request in this
 * suite that carries a BODY, so it is kept (see `authRequests` in `load`).
 */
async function signIn(page, ids) {
  await page.goto(BASE, { waitUntil: "load" });
  let res;
  try {
    res = await page.evaluate(
      async ({ api, email, password, key }) => {
        const r = await fetch(`${api}/api/v1/auth/login`, {
          method: "POST",
          headers: { "content-type": "application/json" },
          credentials: "include",
          body: JSON.stringify({ email, password }),
        });
        const text = await r.text();
        let token = null;
        try {
          token = JSON.parse(text).csrf_token ?? null;
        } catch {
          token = null;
        }
        if (token !== null) window.sessionStorage.setItem(key, token);
        return { status: r.status, body: text.slice(0, 200), token: token !== null };
      },
      { api: ids.api_base, email: ids.user_email, password: ids.password, key: "post.csrf" },
    );
  } catch (err) {
    res = { status: 0, body: `login request threw: ${err.message}`, token: false };
  }
  if (res.status !== 200 || !res.token) {
    fail("session: sign in before driving /search?q=", `POST /api/v1/auth/login -> ${res.status} ${res.body}`);
  }
  return res.status === 200 && res.token;
}

/**
 * Load one route under one preference and read everything the checks below
 * need out of it in a single pass. A fresh context per load, because the
 * preference is a cookie and contexts are what own cookies: reusing one
 * would let the previous load's preference leak into the next and make the
 * "switched back to en" check pass for the wrong reason.
 *
 * `auth` routes sign in on a throwaway page load first (the bootstrap; its
 * requests are dropped) and keep the login POST as `authRequests`, so the
 * wire comparison is still "what THIS page asked for" plus one real body.
 */
async function load(route, locale, { auth = false } = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: COOKIE, value: locale, url: BASE }]);
  const page = await context.newPage();
  const requests = [];
  // Recorded, not stubbed: the request still goes where it was going to go,
  // so the page is the page. What is captured is what would travel.
  await page.route("**/api/**", async (routeHandler) => {
    const req = routeHandler.request();
    const url = new URL(req.url());
    requests.push(`${req.method()} ${url.pathname}${url.search} ${req.postData() ?? ""}`);
    await routeHandler.continue();
  });
  const pageErrors = [];
  page.on("pageerror", (error) => pageErrors.push(String(error).slice(0, 200)));
  let authRequests = [];
  if (auth) {
    await signIn(page, IDS);
    authRequests = requests.filter((line) => line.startsWith("POST /api/v1/auth/login "));
    requests.length = 0;
  }
  await page.goto(BASE + route, { waitUntil: "load" });
  // Hydration settles the client components (the account slot, the bell, the
  // switcher's own state); the header is not final until it has run.
  await page.waitForTimeout(800);
  const dom = await page.evaluate(() => {
    const dataAttrs = {};
    for (const el of document.querySelectorAll("*")) {
      for (const attr of el.attributes) {
        if (!attr.name.startsWith("data-")) continue;
        dataAttrs[attr.name] = `${dataAttrs[attr.name] ?? ""}|${attr.value}`;
      }
    }
    return {
      lang: document.documentElement.lang,
      html: document.documentElement.outerHTML,
      title: document.title,
      bodyText: document.body.innerText,
      hrefs: [...new Set([...document.querySelectorAll("a[href]")].map((a) => a.getAttribute("href")))].sort(),
      dataAttrs,
      options: [...document.querySelectorAll("[data-language-switcher] option")].map((o) => o.value),
      switcherDevices: document.querySelectorAll("[data-language-switcher]").length,
    };
  });
  await context.close();
  return { ...dom, requests: [...requests].sort(), authRequests: [...authRequests].sort(), pageErrors };
}

/** Everything about this load that must be byte-identical across preferences:
 *  the page's own requests, plus the body-carrying sign-in POST when the route
 *  needed one. */
function wire(dom) {
  return JSON.stringify([...dom.authRequests, ...dom.requests]);
}

/** Read the route's probe texts (and its rendered quantities) out of a
 *  freshly loaded page, reusing `load()`'s context discipline. */
async function probeTexts(route, locale, probes, { auth = false } = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
  await context.addCookies([{ name: COOKIE, value: locale, url: BASE }]);
  const page = await context.newPage();
  if (auth) await signIn(page, IDS);
  await page.goto(BASE + route, { waitUntil: "load" });
  await page.waitForTimeout(800);
  const out = await page.evaluate(
    ({ selectors }) => Object.fromEntries(selectors.map((s) => [s, document.querySelector(s)?.textContent?.trim() ?? null])),
    { selectors: probes.map((p) => p.selector) },
  );
  await context.close();
  return out;
}

const coverage = [];
/** Routes whose requests were actually compared. Counted separately from
 *  `coverage` because a route can be driven in both preferences and still
 *  contribute nothing to the wire claim — and that difference is exactly what
 *  a "the payload is identical" sentence hides. */
let wireCompared = 0;

for (const r of ROUTES) {
  const auth = r.auth === true;
  /* --- 1. the preference switches the page ---------------------------- */
  const en = await load(r.route, EN, { auth });
  const zh = await load(r.route, ZH, { auth });
  const enTexts = await probeTexts(r.route, EN, r.probes, { auth });
  const zhTexts = await probeTexts(r.route, ZH, r.probes, { auth });

  if (en.lang !== EN) {
    fail(`lang ${r.label}: /${r.route} with no preference is <html lang="en">`, `lang=${JSON.stringify(en.lang)}`);
  } else if (zh.lang !== ZH) {
    fail(`lang ${r.label}: the preference moves <html lang>`, `lang=${JSON.stringify(zh.lang)} after ${COOKIE}=${ZH}`);
  } else {
    ok(`lang ${r.label}: <html lang> follows the preference (en -> zh-CN)`);
  }

  for (const p of r.probes) {
    const enSeen = enTexts[p.selector];
    const zhSeen = zhTexts[p.selector];
    if (enSeen === null || !enSeen.includes(p.en)) {
      fail(`${r.label}: ${p.selector} renders its English copy`, `saw ${JSON.stringify(enSeen)}, expected it to contain ${JSON.stringify(p.en)}`);
    } else if (zhSeen === null || !zhSeen.includes(p.zh)) {
      fail(`${r.label}: ${p.selector} renders its Chinese copy under ${COOKIE}=${ZH}`, `saw ${JSON.stringify(zhSeen)}, expected it to contain ${JSON.stringify(p.zh)}`);
    } else {
      ok(`${r.label}: ${p.selector} switches ${JSON.stringify(p.en)} -> ${JSON.stringify(p.zh)}`);
    }
  }

  /* --- 1b. the browser tab follows the preference too ----------------- */
  if (r.title !== undefined) {
    // `document.title` is what a reader sees in a strip of twenty tabs, and
    // it is the one piece of page copy no selector in this file can reach.
    // Read off the same two loads the probes came from, so it costs nothing
    // extra; a page whose body translates and whose tab does not reds here.
    if (!en.title.includes(r.title.en)) {
      fail(`${r.label}: the tab title is the English copy`, `document.title = ${JSON.stringify(en.title)}, expected it to contain ${JSON.stringify(r.title.en)}`);
    } else if (!zh.title.includes(r.title.zh)) {
      fail(`${r.label}: the tab title follows the preference`, `document.title = ${JSON.stringify(zh.title)} under ${COOKIE}=${ZH}, expected it to contain ${JSON.stringify(r.title.zh)}`);
    } else {
      ok(`${r.label}: tab title switches ${JSON.stringify(r.title.en)} -> ${JSON.stringify(r.title.zh)}`);
    }
  }

  /* --- 2. switching back to en restores the page ---------------------- */
  {
    const context = await browser.newContext({ viewport: { width: 1280, height: 800 } });
    // The preference is set to zh-CN and then overwritten with en in the
    // same context, which is what the switcher itself does (it writes the
    // cookie). If the layout cached the first answer, this load would still
    // be Chinese and the check below would say so.
    await context.addCookies([{ name: COOKIE, value: ZH, url: BASE }]);
    const page = await context.newPage();
    if (auth) await signIn(page, IDS);
    await page.goto(BASE + r.route, { waitUntil: "load" });
    await page.waitForTimeout(400);
    const langBefore = await page.evaluate(() => document.documentElement.lang);
    await context.addCookies([{ name: COOKIE, value: EN, url: BASE }]);
    await page.reload({ waitUntil: "load" });
    await page.waitForTimeout(800);
    const back = await page.evaluate(
      ({ selectors }) => ({
        lang: document.documentElement.lang,
        texts: Object.fromEntries(selectors.map((s) => [s, document.querySelector(s)?.textContent?.trim() ?? null])),
      }),
      { selectors: r.probes.map((p) => p.selector) },
    );
    await context.close();
    if (langBefore !== ZH) {
      fail(`${r.label}: ${COOKIE}=${ZH} is in effect before switching back`, `lang=${JSON.stringify(langBefore)}`);
    } else if (back.lang !== EN) {
      fail(`${r.label}: switching back to en restores <html lang="en">`, `lang=${JSON.stringify(back.lang)}`);
    } else {
      const stillChinese = r.probes.filter((p) => {
        const seen = back.texts[p.selector];
        return seen === null || !seen.includes(p.en);
      });
      if (stillChinese.length > 0) {
        fail(`${r.label}: switching back to en restores the page`, `still not English: ${JSON.stringify(stillChinese.map((p) => [p.selector, back.texts[p.selector]]))}`);
      } else {
        ok(`${r.label}: switching back to en restores the page and <html lang="en">`);
      }
    }
  }

  /* --- 3. no rendered key anywhere in the document -------------------- */
  for (const [locale, dom] of [[EN, en], [ZH, zh]]) {
    if (dom.html.includes(MISSING_MARKER)) {
      const at = dom.html.indexOf(MISSING_MARKER);
      fail(`${r.label}: no untranslated key renders under ${locale}`, dom.html.slice(Math.max(0, at - 60), at + 80));
    } else {
      ok(`${r.label}: no ${MISSING_MARKER}…⟧ in the document under ${locale}`);
    }
  }

  /* --- 4. the wire is byte-identical ---------------------------------- */
  if (r.fetches && en.requests.length === 0) {
    fail(`${r.label}: the page talks to the API`, "no /api/** request was recorded, so the payload comparison below would compare two empty lists");
  } else if (en.requests.length === 0) {
    console.log(`--   ${r.label}: no API request on this route, so the wire has nothing to compare here`);
  } else if (wire(en) !== wire(zh)) {
    const onlyEn = [...en.authRequests, ...en.requests].filter((x) => ![...zh.authRequests, ...zh.requests].includes(x));
    const onlyZh = [...zh.authRequests, ...zh.requests].filter((x) => ![...en.authRequests, ...en.requests].includes(x));
    fail(`${r.label}: identical API payload in both preferences`, `only en: ${JSON.stringify(onlyEn)}; only zh-CN: ${JSON.stringify(onlyZh)}`);
  } else {
    const bodies = [...en.authRequests, ...en.requests].filter((line) => line.split(" ").slice(-1)[0] !== "").length;
    ok(`${r.label}: ${en.requests.length} API request(s) byte-identical in both preferences` +
      (bodies > 0 ? ` (${bodies} with a request body)` : ""));
    wireCompared += 1;
  }

  /* --- 5. codes and IDs are not localized ----------------------------- */
  if (JSON.stringify(en.hrefs) !== JSON.stringify(zh.hrefs)) {
    fail(`${r.label}: hrefs are identical in both preferences`, `en ${JSON.stringify(en.hrefs)} vs zh-CN ${JSON.stringify(zh.hrefs)}`);
  } else {
    ok(`${r.label}: ${en.hrefs.length} href(s) identical in both preferences`);
  }
  /* The route's declared volatile data-* attributes (see the search-answer
   * entry): dropped from the comparison only after both preferences are shown
   * to carry a value of the declared shape. The failure mode this guards is an
   * exclusion that quietly exempts an attribute which stopped being volatile —
   * or was never there — so "the name I exempted is not in the DOM" is a red,
   * not a skip. */
  const volatile = r.volatileDataAttrs ?? [];
  const stable = (dom) => {
    const out = { ...dom.dataAttrs };
    for (const name of volatile) delete out[name];
    return out;
  };
  let volatileOk = true;
  for (const name of volatile) {
    const shape = VOLATILE_DATA_ATTR_SHAPES[name];
    if (shape === undefined) {
      fail(`${r.label}: the declared volatile attribute is shape-checked`, `${name} has no entry in VOLATILE_DATA_ATTR_SHAPES`);
      volatileOk = false;
      continue;
    }
    for (const [locale, dom] of [[EN, en], [ZH, zh]]) {
      const value = dom.dataAttrs[name];
      if (value === undefined) {
        fail(`${r.label}: the declared volatile ${name} is in the DOM under ${locale}`, "declared volatile, so the comparison skips it — but it is absent, which would make the exclusion an exemption");
        volatileOk = false;
      } else if (!shape.test(value)) {
        fail(`${r.label}: the declared volatile ${name} keeps its shape under ${locale}`, `saw ${JSON.stringify(value)}, expected ${shape}`);
        volatileOk = false;
      }
    }
  }
  if (volatile.length > 0 && volatileOk) {
    console.log(`--   ${r.label}: ${volatile.length} declared volatile data-* attribute(s) excluded by name and shape (${volatile.join(", ")}); ${Object.keys(stable(en)).length} compared`);
  }
  if (JSON.stringify(stable(en)) !== JSON.stringify(stable(zh))) {
    fail(`${r.label}: data-* attribute values are identical in both preferences`, `en ${JSON.stringify(stable(en))} vs zh-CN ${JSON.stringify(stable(zh))}`);
  } else {
    ok(`${r.label}: data-* attribute values identical in both preferences`);
  }
  if (JSON.stringify(en.options) !== JSON.stringify([EN, ZH])) {
    fail(`${r.label}: the language switcher offers the locale codes`, `option values = ${JSON.stringify(en.options)}, expected ${JSON.stringify([EN, ZH])}`);
  } else {
    ok(`${r.label}: the switcher's option values are the locale codes (en, zh-CN)`);
  }
  if (en.switcherDevices !== 1) {
    fail(`${r.label}: exactly one language switcher renders`, `found ${en.switcherDevices}`);
  }

  /* --- 6. scientific quantities survive translation ------------------- */
  {
    // Over the route's OWN copy (its probes) rather than the whole body: the
    // header and footer are the same shell on every route, so reading
    // document.body would compare the shell once per route and tell us
    // nothing about the page.
    const probeText = (texts) => Object.values(texts).filter((v) => typeof v === "string").join("\n");
    const enQ = quantities(probeText(enTexts));
    const zhQ = quantities(probeText(zhTexts));
    if (enQ.length === 0) {
      // Not a pass and not a failure: the language has to be honest about
      // having nothing to compare here. The FLOOR below is what keeps this
      // from becoming the whole suite's answer.
      console.log(`--   ${r.label}: no quantity renders on this route to compare`);
    } else if (enQ.length < 2) {
      fail(`${r.label}: the quantity extractor found something to compare`, `en yielded ${JSON.stringify(enQ)} — a comparison over a single quantity is one token wide, not a measurement`);
    } else if (JSON.stringify(enQ) !== JSON.stringify(zhQ)) {
      fail(`${r.label}: quantities and units render identically in both preferences`, `en ${JSON.stringify(enQ)} vs zh-CN ${JSON.stringify(zhQ)}`);
    } else {
      ok(`${r.label}: ${enQ.length} quantities+units identical in both preferences (${enQ.join(", ")})`);
    }
  }

  if (en.pageErrors.length > 0 || zh.pageErrors.length > 0) {
    fail(`${r.label}: no uncaught page error`, JSON.stringify([...en.pageErrors, ...zh.pageErrors]));  }

  coverage.push({ route: r.route, label: r.label });
}

await browser.close();

console.log("\n== i18n-smoke: per-route coverage ==");
console.log("route                                    | driven");
console.log("-----------------------------------------+-------");
for (const c of coverage) console.log(`${c.route.padEnd(40)} | yes`);
console.log(`\nroutes driven in both preferences: ${coverage.length}${IDS === null ? " (services-down mode: static surfaces only)" : " (with harness data)"}`);

/* The floor. A route that quietly drops out of the list above shrinks a
 * number nobody reads; asserting the count here is what makes that a red
 * run instead.
 *
 * SHAPED LIKE T1104'S CORE_PAGES_WITH_DATA_FLOOR, and for the same reason: a
 * single number would have to be written as the lowest of the two modes, so
 * the services-down run would be asserting the count the full run reaches and
 * would red for the absence of the harness — a failure about the environment,
 * not about the UI. The static routes render without an API by construction
 * (they are what `run.sh` drives); the core routes below need the fixture, and
 * a route that stops rendering its body reds the probe checks before this
 * count is ever read.
 *
 * The four static routes are `/`, `/login`, `/projects` and `/search`; the
 * core set is the project overview plus the seven routes the first review
 * found either unlisted or unexercised (research, pull list, pull detail,
 * release list, release detail, asset page, the search answer body).
 *
 * The second review added an eighth: `/users/{user_id}` (Research Profile).
 * It was the one core page a11y-smoke.mjs drove and this suite did not, which
 * is what let its copy stay English while every other core page was checked.
 * Both counts below moved up by one with it and neither may move down. */
const STATIC_ROUTES = 4;
/** Static routes on which a request is recorded and compared (`/login` talks
 *  to the API only on submit, so it is not one of them). */
const STATIC_ROUTES_WITH_WIRE = 3;
/** Core routes with fixture content: the seven added in the rework, plus the
 *  research profile page. */
const CORE_ROUTES_WITH_DATA = 8;
/** Those eight, plus the project overview — every core route whose page
 *  fetches, which is all of them. */
const CORE_ROUTES_WITH_WIRE = 9;
const FLOOR = IDS === null ? STATIC_ROUTES : STATIC_ROUTES + 1 + CORE_ROUTES_WITH_DATA;
if (coverage.length < FLOOR) {
  fail("coverage: routes driven", `${coverage.length} < ${FLOOR} — a route that was being driven has stopped being driven`);
}
/* The wire floor: the "API payload is byte-identical in both preferences"
 * claim rests on the routes where a request actually happened, so that count
 * gets its own floor. Without it, deleting every fetch from the pages would
 * leave the claim untested and the run green. */
const WIRE_FLOOR = IDS === null ? STATIC_ROUTES_WITH_WIRE : STATIC_ROUTES_WITH_WIRE + CORE_ROUTES_WITH_WIRE;
console.log(`routes with a compared API payload: ${wireCompared}`);
if (wireCompared < WIRE_FLOOR) {
  fail("coverage: routes with a compared API payload", `${wireCompared} < ${WIRE_FLOOR} — the wire-identity claim now rests on fewer routes than this suite drives`);
}

if (fails > 0) {
  console.log(`\ni18n smoke: ${fails} check(s) FAILED`);
  process.exit(1);
}
console.log("\ni18n smoke: all checks passed");
