/**
 * Visual regression (T1101 required test "visual regression").
 *
 * # What this is, and what it is not
 *
 * The project already had a "visual smoke" (tests/web-smoke/visual-smoke.mjs):
 * it renders the app and asserts computed styles and geometry, and it WRITES
 * screenshots to tests/web-smoke/shots/ as artifacts for a human to look at.
 * An artifact is not a regression test — nothing compares it to anything, so
 * no change to it can ever turn a run red.
 *
 * This harness does the other half. Every check below ends in a comparison
 * against a CHECKED-IN baseline image (tests/web-smoke/baseline/*.png), at a
 * fixed viewport, with a written tolerance. A pixel count past that tolerance
 * is a failure, and so is a missing baseline, a missing shot, and a page that
 * never reached its ready marker.
 *
 * The baselines are only worth what the run that wrote them is worth: they
 * have to be shot from a tree whose appearance has been established some
 * other way, or the comparison just agrees with itself. See
 * visual-regression-mutation-check.sh, which breaks the CSS on purpose — in
 * both directions, a shared component and a page layout — and requires the
 * comparison to name the page and count the pixels that moved.
 *
 * # The four sections
 *
 *   A  sitewide static CSS policy   every apps/web/app/**\/*.css plus the
 *                                   shared packages/ui/src/ui.css, comments
 *                                   stripped first
 *   B  the token manifest           packages/ui/src/tokens.ts is docs/41's
 *                                   fourteen roles and nothing else
 *   C  the shots                    render, wait for the ready marker, shoot
 *   D  the comparison               baseline vs shot, per the tolerance
 *   E  the rendered radius proof    measure what the browser actually drew
 *
 * `UPDATE_BASELINE=1` regenerates the baselines instead of comparing. It is
 * the only way to write that directory, and regenerating is a change a human
 * has to look at — the run says so, loudly, and does not claim to have
 * checked anything.
 *
 * Usage: node visual-regression.mjs <base-url> <repo-root>
 */
import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";
import pixelmatch from "pixelmatch";
import { PNG } from "pngjs";

import {
  ALICE_ID,
  ASSET_PID,
  installApiMock,
  PROFILE,
  PROJECT,
  RELEASE_ID,
  SEARCH_QUERY,
  TRIPLE,
} from "./visual-fixtures.mjs";

/** The conflicts comparison, as the query the page reads. */
const TRIPLE_QUERY = new URLSearchParams({
  base_state_id: TRIPLE.base_state_id,
  source_state_id: TRIPLE.source_state_id,
  target_state_id: TRIPLE.target_state_id,
}).toString();

/** The project the covered pages live under. The app's own directory links
 *  to `/projects/{id}` (the UUID), not to the slug, so the covered URLs are
 *  the ones a reader actually arrives at. */
const PROJECT_ID_URL = PROJECT.id;

const BASE = (process.argv[2] ?? "http://127.0.0.1:31150").replace(/\/+$/, "");
const ROOT = process.argv[3] ?? path.resolve(import.meta.dirname, "../..");
const WEB_DIR = path.join(ROOT, "apps/web");

const UPDATE = process.env.UPDATE_BASELINE === "1";
const BASELINE_DIR = path.join(import.meta.dirname, "baseline");
const CURRENT_DIR = path.join(import.meta.dirname, "current");
const DIFF_DIR = path.join(import.meta.dirname, "diff");

let fails = 0;
const pass = (label, detail) => console.log(`ok   ${label}${detail ? ` — ${detail}` : ""}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};

/* ======================================================================
 * THE TOLERANCE
 *
 * One number and one perceptual distance, written here rather than buried
 * in a call. A shot fails when MORE than TOLERANCE_PIXELS pixels differ at
 * a colour distance above PERCEPTUAL_THRESHOLD.
 *
 * Why not zero: subpixel text antialiasing is the one thing in a headless
 * Chromium render that is not bit-for-bit guaranteed across machines and
 * font builds, and a harness whose first red is a machine difference gets
 * deleted within a month. 64 pixels is about an 8x8 block — smaller than
 * any real change this harness exists to catch (a moved hairline is a few
 * thousand pixels; a changed font size is tens of thousands), and large
 * enough to absorb that noise.
 *
 * It is NOT headroom for a real difference: the unmodified tree measures
 * exactly 0 differing pixels on every shot, and the run prints the measured
 * count for every page, so a drift towards the budget is visible before it
 * is a failure.
 * ====================================================================== */
const TOLERANCE_PIXELS = 64;
const PERCEPTUAL_THRESHOLD = 0.1;

/* The viewport every baseline is shot at. One size, stated: the design
 * system's density is a desktop question (docs/06 §2), and a second size
 * would double the baseline set for a layout the CSS already collapses. */
const VIEWPORT = { width: 1280, height: 900 };

/* ======================================================================
 * A. Sitewide static CSS policy
 * ====================================================================== */

/** Strip `/* … *\/` so a comment ABOUT a gradient does not read as one. The
 *  pre-existing checks in web-smoke and e2e-shell do not do this: they
 *  scan raw bytes, which is why none of them could ever be pointed at the
 *  whole app — nine stylesheets carry the phrase "no gradients" in their
 *  own header comments. */
function stripComments(css) {
  return css.replace(/\/\*[\s\S]*?\*\//g, " ");
}

const FORBIDDEN_FX = [
  "linear-gradient",
  "radial-gradient",
  "conic-gradient",
  "repeating-linear-gradient",
  "repeating-radial-gradient",
  "backdrop-filter",
  "blur(",
];

/** docs/41:14 — no `brandGradient`, `purpleGlow`, `glassBackground`. The
 *  role names are checked in section B; here the point is that the three
 *  forbidden NAMES do not appear, in any file, as anything. */
const FORBIDDEN_TOKEN_NAMES = ["brandGradient", "purpleGlow", "glassBackground"];

const FORBIDDEN_HEX = [
  "#8957e5", "#6e40c9", "#8250df", "#8b5cf6", "#7c3aed", "#a855f7",
  "#9333ea", "#663399", "#5b21b6", "#4c1d95", "#d946ef", "#c026d3",
];

function walk(dir, out = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name === ".next") continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else if (entry.name.endsWith(".css")) out.push(full);
  }
  return out;
}

const cssFiles = [
  ...walk(path.join(WEB_DIR, "app")),
  path.join(ROOT, "packages/ui/src/ui.css"),
].sort();

if (cssFiles.length < 10) {
  fail("css policy: the stylesheet set is the whole app", `found ${cssFiles.length} files`);
}

/** Every `box-shadow` in the app, with the selector it sits under, so the
 *  "overlays only" rule (docs/41:18) is checked against a list rather than
 *  asserted in prose. */
const SHADOW_ALLOWLIST = [
  // The mobile navigation drawer: `position: absolute; z-index: 40`, drawn
  // BELOW the header and over the page. docs/41:18 permits a shadow on an
  // overlay/menu/dialog, and this is one — the desktop nav row beside it
  // has borders and no shadow.
  { file: "apps/web/app/nav/nav.css", selector: ".global-nav-drawer" },
];

/* The radius rule, as docs/41:17 and AC5 actually state it: no corner of
 * 24px or more ("card 不使用 24px+ 大圆角").
 *
 * This check used to allowlist four literal values (6px, 999px,
 * "6px 6px 0 0", 50%) — stricter than the spec, and the rework showed why
 * that is the wrong shape for this rule: the appearance-equivalence
 * requirement (AC3) forbids cosmetic changes to the pages this task did not
 * set out to change, and HEAD's own tree rounds two chip families at 4px
 * (`.assets-pid`, `.explore-row-*`) and draws `.coming-soon-icon` as a
 * circle. An invented allowlist cannot have it both ways: it either forces
 * those declarations to change (a visual regression, which AC3 reds) or it
 * lies about what the stylesheets say.
 *
 * So the check is the spec's own: a declared length of 24px or more fails,
 * and the four sentinels that mean "as round as the box allows" are named
 * here rather than being numbers. What the browser actually DRAWS is
 * measured separately at the end of this file (section E) and must stay
 * under 24px on every covered page — a sentinel on a large box fails there,
 * which is the half of the rule a static check cannot see. */
const RADIUS_SENTINELS = ["999px", "9999px", "2em", "50%"];
const MAX_DECLARED_RADIUS = 24;

function radiusOffence(value) {
  const single = value.trim();
  if (RADIUS_SENTINELS.includes(single)) return null;
  if (single === "6px 6px 0 0") return null;
  for (const part of single.split(/\s+/)) {
    const px = /^(\d+(?:\.\d+)?)px$/.exec(part);
    if (px === null) return `unparsed radius "${part}" (${value})`;
    if (Number(px[1]) >= MAX_DECLARED_RADIUS) return `${part} is ${MAX_DECLARED_RADIUS}px or more`;
  }
  return null;
}

const shadowSites = [];
const offenders = [];
for (const file of cssFiles) {
  const rel = path.relative(ROOT, file);
  const css = stripComments(fs.readFileSync(file, "utf8"));
  const lower = css.toLowerCase();

  for (const token of FORBIDDEN_FX) {
    if (lower.includes(token)) offenders.push(`${rel}: forbidden effect ${token}`);
  }
  for (const hex of FORBIDDEN_HEX) {
    if (lower.includes(hex)) offenders.push(`${rel}: purple ${hex}`);
  }
  for (const name of FORBIDDEN_TOKEN_NAMES) {
    if (css.includes(name)) offenders.push(`${rel}: forbidden token name ${name}`);
  }

  // Walk declarations with their enclosing selector, so a shadow can be
  // attributed to the rule that draws it.
  const ruleRe = /([^{}]+)\{([^{}]*)\}/g;
  let m;
  while ((m = ruleRe.exec(css)) !== null) {
    const selector = m[1].trim().replace(/\s+/g, " ").split("\n").pop().trim();
    const body = m[2];
    if (/box-shadow\s*:/.test(body)) shadowSites.push({ rel, selector });
    for (const decl of body.matchAll(/border-radius\s*:\s*([^;]+);/g)) {
      const offence = radiusOffence(decl[1]);
      if (offence !== null) offenders.push(`${rel}: border-radius ${offence} (${selector})`);
    }
  }
}

if (offenders.length === 0) {
  pass(`css policy: ${cssFiles.length} stylesheets — no gradient, no glass, no blur, no purple, no ${MAX_DECLARED_RADIUS}px+ corner`);
} else {
  for (const o of offenders) fail("css policy", o);
}

const unexplainedShadows = shadowSites.filter(
  (s) => !SHADOW_ALLOWLIST.some((a) => a.file === s.rel && s.selector.includes(a.selector)),
);
if (unexplainedShadows.length === 0) {
  pass(`css policy: box-shadow only on overlays (${shadowSites.length} site(s), all in the nav menu)`);
} else {
  for (const s of unexplainedShadows) {
    fail("css policy: box-shadow outside an overlay", `${s.rel} ${s.selector}`);
  }
}

/* ======================================================================
 * B. The token manifest
 * ====================================================================== */

/** docs/41:6-12, verbatim, in the order the document lists them. */
const DOC41_ROLES = [
  "canvas.default", "canvas.subtle",
  "fg.default", "fg.muted",
  "border.default", "border.muted",
  "accent.fg", "accent.emphasis",
  "success.fg", "success.emphasis",
  "attention.fg", "attention.emphasis",
  "danger.fg", "danger.emphasis",
];

const tokensSource = fs.readFileSync(path.join(ROOT, "packages/ui/src/tokens.ts"), "utf8");
const declared = [...tokensSource.matchAll(/^\s*"([a-z]+\.[a-z]+)":/gm)].map((m) => m[1]);

if (declared.length !== DOC41_ROLES.length) {
  fail("tokens: exactly the fourteen docs/41 roles", `found ${declared.length}: ${declared.join(", ")}`);
} else if (declared.join("|") !== DOC41_ROLES.join("|")) {
  fail("tokens: exactly the fourteen docs/41 roles", `declared ${declared.join(", ")}`);
} else {
  pass(`tokens: exactly the fourteen docs/41 roles, no sixteenth invented`);
}

/* Every token value must resolve to a Primer variable with a literal
 * fallback — no bare hex, and no value that is not a var() at all. */
const tokenValues = [...tokensSource.matchAll(/"[a-z]+\.[a-z]+":\s*"([^"]+)"/g)].map((m) => m[1]);
const badValues = tokenValues.filter((v) => !/^var\(--[A-Za-z-]+, #[0-9a-f]{6}\)$/.test(v));
if (badValues.length === 0) {
  pass(`tokens: all ${tokenValues.length} values are Primer vars with hex fallbacks`);
} else {
  fail("tokens: every value is a Primer var with a hex fallback", badValues.join(", "));
}

/* ======================================================================
 * C. The shots
 * ====================================================================== */

/**
 * The pages. `ready` is the marker that proves the page reached the state
 * the baseline was taken of — without it, a broken fixture or a crashing
 * page would baseline an error state and call it the design.
 *
 * Each entry names the page in docs/42 terms and the file it becomes.
 */
const PAGES = [
  { file: "projects-directory", label: "Projects directory", url: "/projects", ready: ".project-row" },
  { file: "project-overview", label: "Project Overview", url: `/projects/${PROJECT_ID_URL}`, ready: '[data-project-tab-content="overview"]' },
  // The research page was rewritten as a substantive workspace (T1202 +
  // hotfix/web-demo-regressions); the old [data-tab-placeholder] no longer
  // exists. The workspace container is the post-load marker.
  { file: "project-research", label: "Research Flow", url: `/projects/${PROJECT_ID_URL}/research`, ready: ".research-workspace" },
  { file: "project-files", label: "Repository Files", url: `/projects/${PROJECT_ID_URL}/files`, ready: "[data-files-entry]" },
  { file: "project-pulls", label: "Pull requests list", url: `/projects/${PROJECT_ID_URL}/pulls`, ready: '[data-pull-row]' },
  { file: "pull-detail", label: "PR Semantic Diff", url: `/projects/${PROJECT_ID_URL}/pulls/7`, ready: '[data-pull-panel="summary"]' },
  // Non-default PR tabs: the issue that prompted this rework was that the
  // shared Diff's chips looked different here than on the summary tab. The
  // tab state is client-side, so the harness clicks the tab before the shot.
  {
    file: "pull-detail-scientific",
    label: "PR Scientific tab",
    url: `/projects/${PROJECT_ID_URL}/pulls/7`,
    ready: '[data-pull-panel="scientific"]',
    action: async (p) => { await p.click('[data-pull-tab="scientific"]'); },
  },
  {
    file: "pull-detail-knowledge",
    label: "PR Knowledge tab",
    url: `/projects/${PROJECT_ID_URL}/pulls/7`,
    ready: '[data-pull-panel="knowledge"]',
    action: async (p) => { await p.click('[data-pull-tab="knowledge"]'); },
  },
  {
    file: "pull-detail-evidence",
    label: "PR Evidence tab",
    url: `/projects/${PROJECT_ID_URL}/pulls/7`,
    ready: '[data-pull-panel="evidence"]',
    action: async (p) => { await p.click('[data-pull-tab="evidence"]'); },
  },
  // The conflicts page reads the comparison from the query, not from its
  // own state picker (page.tsx:96) — without the triple it renders the
  // "supply a triple" explanation instead of a conflict card.
  { file: "project-conflicts", label: "Conflict resolution", url: `/projects/${PROJECT_ID_URL}/conflicts?${TRIPLE_QUERY}`, ready: "[data-conflict-card]" },
  { file: "project-activity", label: "Activity stream", url: `/projects/${PROJECT_ID_URL}/activity`, ready: "[data-activity-row]" },
  { file: "project-releases", label: "Releases list", url: `/projects/${PROJECT_ID_URL}/releases`, ready: "[data-release-row]" },
  { file: "release-detail", label: "Release detail", url: `/projects/${PROJECT_ID_URL}/releases/${RELEASE_ID}`, ready: "[data-release-detail]" },
  { file: "project-settings", label: "Settings", url: `/projects/${PROJECT_ID_URL}/settings`, ready: "[data-member-row]" },
  { file: "asset-page", label: "Asset Version page", url: `/assets/${ASSET_PID}/2.0`, ready: '[data-asset-page="ready"]' },
  { file: "search-answer", label: "Search Answer", url: `/search?q=${encodeURIComponent(SEARCH_QUERY)}`, ready: '[data-search-section="answer"]' },
  // Not `h1.profile-name`: that class is one THIS TASK introduced, so the
  // HEAD-tree equivalence run (head-tree-visual.sh) could never find it and
  // silently lost the page. The marker has to exist in both trees, and a
  // heading carrying the fixture's own display name is the stronger of the
  // two anyway — it proves the profile's data reached the DOM, not just
  // that a class was rendered.
  { file: "research-profile", label: "Research Profile", url: `/users/${ALICE_ID}`, ready: `h1:has-text("${PROFILE.display_name}")` },
];

fs.mkdirSync(CURRENT_DIR, { recursive: true });
if (UPDATE) fs.mkdirSync(BASELINE_DIR, { recursive: true });

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: 1 });
const page = await context.newPage();

const unmatched = [];
installApiMock(page, (what) => unmatched.push(what));

page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));

const shots = [];
for (const entry of PAGES) {
  await page.goto(`${BASE}${entry.url}`, { waitUntil: "load" });
  if (entry.action) {
    // Some pages keep their state in the DOM (tabs, drawers). The action
    // is executed before the ready-marker wait so the marker it waits for
    // is the one in the target state.
    await entry.action(page);
  }
  try {
    await page.waitForSelector(entry.ready, { state: "visible", timeout: 15000 });
  } catch {
    // "marker never appeared" alone sends the next reader back into the
    // fixtures by hand. The page's own visible text is what says whether
    // the fixture was wrong or the page was — so carry it in the failure.
    const seen = await page
      .evaluate(() => (document.body.innerText ?? "").replace(/\s+/g, " ").trim().slice(0, 300))
      .catch(() => "(page unreadable)");
    fail(
      `shot ${entry.file}: reached its ready state`,
      `marker ${entry.ready} never appeared at ${entry.url} — page said: ${seen}`,
    );
    continue;
  }
  // The final layout pass, then a screenshot with animations frozen: a
  // transition mid-flight is the classic source of an unreproducible diff.
  await page.evaluate(() => document.fonts.ready);
  await page.addStyleTag({ content: "*, *::before, *::after { animation: none !important; transition: none !important; caret-color: transparent !important; }" });
  const file = path.join(CURRENT_DIR, `${entry.file}.png`);
  await page.screenshot({ path: file, fullPage: true });
  shots.push({ ...entry, file });
  pass(`shot ${entry.file}`, `${entry.label} at ${VIEWPORT.width}x${VIEWPORT.height}`);
}

if (unmatched.length > 0) {
  // Not fatal by itself — a page may probe an endpoint it tolerates a 404
  // from — but it is always worth saying, because the next person's "the
  // baseline looks empty" is usually this.
  console.log(`note unmatched API calls (404 from the fixture router): ${[...new Set(unmatched)].join(", ")}`);
}

/* ======================================================================
 * E. The rendered radius proof
 *
 * Section A checks what the STYLESHEETS say. This checks what the BROWSER
 * DREW, which is the only thing a reader sees: `border-radius: 999px` on a
 * 20px-tall label is a sentinel, and the browser clamps the used value to
 * half the box. Measuring the used value is what turns "the pill is not a
 * big corner" from a claim in a comment into a check.
 *
 * The threshold is docs/41:17's own line: no 24px+ corners. Not on a card,
 * not on a label, not anywhere.
 * ====================================================================== */
const MAX_RADIUS_PX = 24;
const radiusOffenders = [];
for (const entry of PAGES) {
  await page.goto(`${BASE}${entry.url}`, { waitUntil: "load" });
  await page.waitForSelector(entry.ready, { state: "visible", timeout: 15000 }).catch(() => {});
  const found = await page.evaluate((max) => {
    const out = [];
    for (const el of document.querySelectorAll("*")) {
      const s = getComputedStyle(el);
      for (const corner of ["borderTopLeftRadius", "borderTopRightRadius", "borderBottomLeftRadius", "borderBottomRightRadius"]) {
        const declared = parseFloat(s[corner]);
        if (!Number.isFinite(declared) || declared === 0) continue;
        // `50%`-style corners compute to a percentage-computed px against
        // the box; the clamp below is what the reader actually sees.
        const box = el.getBoundingClientRect();
        const used = Math.min(declared, box.height / 2, box.width / 2);
        if (used > max) {
          out.push({ className: typeof el.className === "string" ? el.className : el.tagName, corner, declared, used: Math.round(used), w: Math.round(box.width), h: Math.round(box.height) });
        }
        break; // one corner per element is enough to name the offender
      }
    }
    return out;
  }, MAX_RADIUS_PX);
  for (const o of found) radiusOffenders.push(`${entry.file}: .${o.className} (${o.w}x${o.h}) declared ${o.declared}px → rendered ${o.used}px`);
}
if (radiusOffenders.length === 0) {
  pass(`radius: no rendered corner above ${MAX_RADIUS_PX}px on any covered page (docs/41:17)`);
} else {
  for (const o of radiusOffenders) fail("radius: rendered corner above 24px", o);
}

await browser.close();

/* ======================================================================
 * D. The comparison
 * ====================================================================== */

if (UPDATE) {
  // A page that never rendered has no shot, and writing the set without it
  // would check in a baseline list with a hole in it — the exact silent
  // hole this harness exists to close. So an update run refuses to write a
  // partial set, and it still ends through the shared failure report below
  // rather than claiming success.
  if (shots.length !== PAGES.length) {
    fail(
      "baseline update: every covered page rendered",
      `${shots.length}/${PAGES.length} shot(s) — refusing to write anything, so the checked-in set stays whole`,
    );
  } else {
    for (const shot of shots) {
      fs.copyFileSync(shot.file, path.join(BASELINE_DIR, `${shot.file.split("/").pop()}`));
    }
    console.log(`\nbaselines written: ${shots.length} shot(s) into ${path.relative(ROOT, BASELINE_DIR)}`);
    console.log("THIS RUN COMPARED NOTHING. A baseline-only change has to be reviewed by eye.");
  }
} else {
  // Comparing an update run against the baselines it just wrote would be
  // zero by construction — fourteen green lines that measured nothing.
  // The update path ends here and reports only what it actually did.

  fs.mkdirSync(DIFF_DIR, { recursive: true });

  const baselineNames = fs.existsSync(BASELINE_DIR)
    ? fs.readdirSync(BASELINE_DIR).filter((n) => n.endsWith(".png")).sort()
    : [];
  const shotNames = shots.map((s) => path.basename(s.file)).sort();
  const missingBaselines = shotNames.filter((n) => !baselineNames.includes(n));
  const orphanBaselines = baselineNames.filter((n) => !shotNames.includes(n));

  if (baselineNames.length === 0) {
    fail("baselines are checked in", `${path.relative(ROOT, BASELINE_DIR)} holds no PNG`);
  } else if (missingBaselines.length > 0) {
    fail("every shot has a baseline", `missing: ${missingBaselines.join(", ")}`);
  } else if (orphanBaselines.length > 0) {
    /* A baseline with no page is not harmless: it means a page was dropped
     * from the list and its protection went with it. */
    fail("every baseline has a page", `orphan: ${orphanBaselines.join(", ")}`);
  } else {
    pass(`baselines: ${baselineNames.length} checked-in PNG(s), one per covered page`);
  }

  for (const shot of shots) {
    const name = path.basename(shot.file);
    const baselinePath = path.join(BASELINE_DIR, name);
    if (!fs.existsSync(baselinePath)) continue;

    const baseline = PNG.sync.read(fs.readFileSync(baselinePath));
    const current = PNG.sync.read(fs.readFileSync(shot.file));

    if (baseline.width !== current.width || baseline.height !== current.height) {
      fail(
        `visual regression ${name}`,
        `page size changed: baseline ${baseline.width}x${baseline.height} vs current ${current.width}x${current.height}`,
      );
      continue;
    }

    const diff = new PNG({ width: baseline.width, height: baseline.height });
    const differing = pixelmatch(baseline.data, current.data, diff.data, baseline.width, baseline.height, {
      threshold: PERCEPTUAL_THRESHOLD,
    });

    if (differing > TOLERANCE_PIXELS) {
      fs.writeFileSync(path.join(DIFF_DIR, name), PNG.sync.write(diff));
      fail(
        `visual regression ${name}`,
        `${differing} pixel(s) differ, tolerance ${TOLERANCE_PIXELS} — diff image: ${path.relative(ROOT, path.join(DIFF_DIR, name))}`,
      );
    } else {
      pass(`visual regression ${name}`, `${differing} pixel(s) differ, tolerance ${TOLERANCE_PIXELS}`);
    }
  }
}

if (fails > 0) {
  console.log(`\nvisual regression: ${fails} check(s) FAILED`);
  process.exit(1);
}
console.log("\nvisual regression: all checks passed");
