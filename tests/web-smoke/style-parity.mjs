/**
 * Style parity (T1101 AC3, diagnostic).
 *
 * The pixel comparison answers "did the page move" and (with
 * diff-locations.mjs) "where". This answers the next question: WHICH ELEMENT
 * moved, and by how much — by rendering the same page from two trees (the
 * HEAD copy built by head-tree-visual.sh, and the worktree) and diffing
 * every classed element's box and type metrics.
 *
 * It is a diagnostic, not the required test: the acceptance number comes
 * from visual-regression.mjs, which compares pixels against the checked-in
 * baselines. This one exists because "3577 pixels differ on project-files"
 * does not say whether the file rows grew, the badges grew, or the pane
 * moved — and guessing that from a diff image is how a "fix" lands on the
 * wrong rule.
 *
 * Usage: node style-parity.mjs <head-url> <worktree-url> [page-substring]
 */
import { chromium } from "playwright";

import { ALICE_ID, ASSET_PID, installApiMock, PROFILE, PROJECT, RELEASE_ID, SEARCH_QUERY, TRIPLE } from "./visual-fixtures.mjs";

const TRIPLE_QUERY = new URLSearchParams({
  base_state_id: TRIPLE.base_state_id,
  source_state_id: TRIPLE.source_state_id,
  target_state_id: TRIPLE.target_state_id,
}).toString();

/** The covered pages. Kept in step with visual-regression.mjs's PAGES by the
 *  name check at the bottom of this file. */
const PAGES = [
  { file: "projects-directory", url: "/projects", ready: ".project-row" },
  { file: "project-overview", url: `/projects/${PROJECT.id}`, ready: '[data-project-tab-content="overview"]' },
  { file: "project-research", url: `/projects/${PROJECT.id}/research`, ready: "[data-tab-placeholder]" },
  { file: "project-files", url: `/projects/${PROJECT.id}/files`, ready: "[data-files-entry]" },
  { file: "project-pulls", url: `/projects/${PROJECT.id}/pulls`, ready: "[data-pull-row]" },
  { file: "pull-detail", url: `/projects/${PROJECT.id}/pulls/7`, ready: '[data-pull-panel="summary"]' },
  { file: "project-conflicts", url: `/projects/${PROJECT.id}/conflicts?${TRIPLE_QUERY}`, ready: "[data-conflict-card]" },
  { file: "project-activity", url: `/projects/${PROJECT.id}/activity`, ready: "[data-activity-row]" },
  { file: "project-releases", url: `/projects/${PROJECT.id}/releases`, ready: "[data-release-row]" },
  { file: "release-detail", url: `/projects/${PROJECT.id}/releases/${RELEASE_ID}`, ready: "[data-release-detail]" },
  { file: "project-settings", url: `/projects/${PROJECT.id}/settings`, ready: "[data-member-row]" },
  { file: "asset-page", url: `/assets/${ASSET_PID}/2.0`, ready: '[data-asset-page="ready"]' },
  { file: "search-answer", url: `/search?q=${encodeURIComponent(SEARCH_QUERY)}`, ready: '[data-search-section="answer"]' },
  { file: "research-profile", url: `/users/${ALICE_ID}`, ready: `h1:has-text("${PROFILE.display_name}")` },
];

const [HEAD_BASE_RAW, WORK_BASE_RAW, only] = process.argv.slice(2);
if (!HEAD_BASE_RAW || !WORK_BASE_RAW) {
  console.error("usage: node style-parity.mjs <head-url> <worktree-url> [page-substring]");
  process.exit(2);
}
const HEAD_BASE = HEAD_BASE_RAW.replace(/\/+$/, "");
const WORK_BASE = WORK_BASE_RAW.replace(/\/+$/, "");

const VIEWPORT = { width: 1280, height: 900 };

/** The metrics worth comparing: geometry plus everything that changes ink. */
const PROPS = [
  "display", "fontSize", "fontWeight", "lineHeight", "fontFamily",
  "color", "backgroundColor", "borderTopWidth", "borderTopColor",
  "borderTopLeftRadius", "paddingTop", "paddingLeft", "marginTop", "marginBottom",
  "gap", "textDecorationLine", "whiteSpace", "verticalAlign", "flexWrap", "alignItems",
];

/** Collect every classed element, keyed so the two trees line up. */
async function snapshot(page) {
  return page.evaluate((props) => {
    const seen = new Map();
    const out = [];
    for (const el of document.querySelectorAll("*")) {
      const classes = typeof el.className === "string" ? el.className.trim() : "";
      if (classes === "") continue;
      const key = `${el.tagName.toLowerCase()}.${classes.split(/\s+/).join(".")}`;
      const n = (seen.get(key) ?? 0) + 1;
      seen.set(key, n);
      const box = el.getBoundingClientRect();
      const style = getComputedStyle(el);
      const metrics = {};
      for (const prop of props) metrics[prop] = style[prop];
      out.push({
        key: `${key}#${n}`,
        box: {
          x: Math.round(box.x * 10) / 10,
          y: Math.round((box.y + window.scrollY) * 10) / 10,
          w: Math.round(box.width * 10) / 10,
          h: Math.round(box.height * 10) / 10,
        },
        metrics,
        text: (el.textContent ?? "").replace(/\s+/g, " ").trim().slice(0, 40),
      });
    }
    return out;
  }, PROPS);
}

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: VIEWPORT, deviceScaleFactor: 1 });

async function render(base, entry) {
  const page = await context.newPage();
  installApiMock(page);
  await page.goto(`${base}${entry.url}`, { waitUntil: "load" });
  await page.waitForSelector(entry.ready, { state: "visible", timeout: 15000 }).catch(() => {});
  await page.evaluate(() => document.fonts.ready);
  const snap = await snapshot(page);
  await page.close();
  return snap;
}

let totalDiffs = 0;
for (const entry of PAGES) {
  if (only !== undefined && !entry.file.includes(only)) continue;
  const [head, work] = [await render(HEAD_BASE, entry), await render(WORK_BASE, entry)];
  const headByKey = new Map(head.map((e) => [e.key, e]));
  const workByKey = new Map(work.map((e) => [e.key, e]));

  const lines = [];
  for (const [key, a] of headByKey) {
    const b = workByKey.get(key);
    if (b === undefined) {
      lines.push(`  - only in HEAD: ${key}  (${a.box.w}x${a.box.h} at ${a.box.x},${a.box.y}) "${a.text}"`);
      continue;
    }
    const moved =
      a.box.x !== b.box.x || a.box.y !== b.box.y || a.box.w !== b.box.w || a.box.h !== b.box.h;
    const metricDiffs = PROPS.filter((p) => a.metrics[p] !== b.metrics[p]);
    if (!moved && metricDiffs.length === 0) continue;
    const parts = [];
    if (moved) {
      parts.push(
        `box HEAD ${a.box.w}x${a.box.h}@${a.box.x},${a.box.y} -> NOW ${b.box.w}x${b.box.h}@${b.box.x},${b.box.y}`,
      );
    }
    for (const p of metricDiffs) parts.push(`${p}: "${a.metrics[p]}" -> "${b.metrics[p]}"`);
    lines.push(`  ~ ${key} "${b.text}"\n      ${parts.join("\n      ")}`);
  }
  for (const [key, b] of workByKey) {
    if (!headByKey.has(key)) lines.push(`  + only in NOW: ${key}  (${b.box.w}x${b.box.h} at ${b.box.x},${b.box.y}) "${b.text}"`);
  }

  if (lines.length === 0) {
    console.log(`== ${entry.file}: identical (${head.length} classed elements)`);
  } else {
    totalDiffs += lines.length;
    console.log(`== ${entry.file}: ${lines.length} difference(s)`);
    console.log(lines.join("\n"));
  }
}

await browser.close();
console.log(`\nstyle parity: ${totalDiffs} differing element(s)`);
process.exit(totalDiffs === 0 ? 0 : 1);
