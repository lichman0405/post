/**
 * Visual smoke (T0107, G1): a real Chromium renders the app and we assert
 * what a designer would check by eye — the GitHub-style header is present,
 * compact and neutral (no gradients, no glass, no purple), every global
 * destination renders, and the responsive baseline collapses the header at
 * narrow widths instead of overflowing.
 *
 * Usage: node visual-smoke.mjs <base-url> <apps-web-dir>
 */
import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31107";
const APPS_WEB = process.argv[3] ?? path.resolve(import.meta.dirname, "../../apps/web");

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};

/* ---------- A. Static CSS policy (no browser needed) ---------- */

const FORBIDDEN_HEX = [
  "#8957e5", "#6e40c9", "#8250df", "#8b5cf6", "#7c3aed", "#a855f7",
  "#9333ea", "#663399", "#5b21b6", "#4c1d95", "#d946ef", "#c026d3",
];
const FORBIDDEN_FX = [
  "linear-gradient", "radial-gradient", "conic-gradient",
  "repeating-linear-gradient", "repeating-radial-gradient",
  "backdrop-filter", "blur(",
];

const cssFiles = ["app/nav/nav.css", "app/globals.css"].map((p) =>
  path.join(APPS_WEB, p),
);
for (const file of cssFiles) {
  const css = fs.readFileSync(file, "utf8").toLowerCase();
  for (const token of FORBIDDEN_FX) {
    if (css.includes(token)) {
      fail(`css-policy ${path.basename(file)}: no ${token}`, "forbidden visual effect found");
    }
  }
  for (const hex of FORBIDDEN_HEX) {
    if (css.includes(hex)) {
      fail(`css-policy ${path.basename(file)}: no purple ${hex}`, "purple is not a brand/CTA color (docs/06 §2)");
    }
  }
}
ok(`css-policy ${path.basename(cssFiles[0])} + ${path.basename(cssFiles[1])}: no gradients, no backdrop blur, no purple`);

const navCss = fs.readFileSync(cssFiles[0], "utf8");
if (!/@media/.test(navCss)) {
  fail("css-policy nav.css: responsive baseline", "no media query in nav.css");
} else {
  ok("css-policy nav.css: responsive baseline media query present");
}

/* ---------- B. Desktop rendering (1280x800) ---------- */

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));

const DESKTOP_LABELS = ["Home", "Explore", "Projects", "Assets", "People", "Organizations"];
const DRAWER_LABELS = ["Search", "Home", "Explore", "Projects", "Assets", "People", "Organizations", "Notifications"];

const computed = (locator, props) =>
  locator.evaluate((el, props) => {
    const s = getComputedStyle(el);
    const out = {};
    for (const p of props) out[p] = s[p];
    return out;
  }, props);

const isPurple = (color) => {
  const m = /rgba?\((\d+),\s*(\d+),\s*(\d+)/.exec(color);
  if (!m) return false;
  const [r, g, b] = [Number(m[1]), Number(m[2]), Number(m[3])];
  return r > g && b > g && r > 120 && b > 120;
};

await page.goto(BASE, { waitUntil: "load" });
await page.waitForSelector(".global-nav-link", { state: "attached" });

const header = page.locator(".global-nav");
ok("header renders on /", (await header.count()) === 1);
const headerBox = await header.boundingBox();
if (!headerBox || headerBox.height > 64) {
  fail("desktop information density: header height <= 64px", `height=${headerBox?.height}`);
} else {
  ok(`desktop information density: header height ${headerBox.height}px <= 64px`);
}

for (const label of DESKTOP_LABELS) {
  const link = page.locator(`.global-nav-links a:visible`, { hasText: label }).first();
  if ((await link.count()) === 0) {
    fail(`desktop nav shows ${label}`, "link not found");
    continue;
  }
  const size = await computed(link, ["fontSize"]);
  if (parseFloat(size.fontSize) > 14.5) {
    fail(`desktop density: ${label} font-size <= 14px`, `font-size=${size.fontSize}`);
  }
}
ok(`desktop nav shows ${DESKTOP_LABELS.join(", ")} at <= 14px`);

const searchInput = page.locator('form[role="search"] input[type="search"][name="q"]');
ok("search form input[name=q] visible", await searchInput.isVisible());
const placeholder = await searchInput.getAttribute("placeholder");
if (placeholder !== "Search POST") {
  fail("search placeholder is 'Search POST'", `got ${placeholder}`);
} else {
  ok("search placeholder is 'Search POST'");
}

const bell = page.locator('a[aria-label="Notifications"][href="/notifications"]');
ok("notifications bell links to /notifications", (await bell.count()) === 1);
const logo = page.locator('a[aria-label="POST home"][href="/"]');
ok("logo links home", (await logo.count()) === 1);
const signIn = page.locator('a[href="/login"]:visible', { hasText: "Sign in" });
ok("signed-out state offers Sign in in the header", (await signIn.count()) >= 1);

// Hydration can transiently expose two copies of the nav (Suspense swap);
// wait for the settled DOM, then assert the single current marker.
await page.waitForFunction(
  () => document.querySelectorAll(".global-nav-link[aria-current='page']").length === 1,
  undefined,
  { timeout: 5000 },
).catch(() => fail("aria-current=page marks the active destination", "marker never settled"));
const currentText = await page.evaluate(
  () => document.querySelector(".global-nav-link[aria-current='page']")?.textContent?.trim(),
);
ok("aria-current=page marks the active destination", currentText === "Home");

// Exactly one non-hidden main landmark per document (HTML conformance,
// docs/06 §10): the layout provides <main id="main">; page fragments must
// not add a second one — this assertion is what caught the home page's
// nested <main class="status-main"> before the fix. (Mapped to plain data
// inside the page: Playwright cannot serialize DOM elements back.)
const mains = await page.evaluate(() =>
  [...document.querySelectorAll("main")]
    .filter((m) => !m.hidden && getComputedStyle(m).display !== "none")
    .map((m) => ({ id: m.id, cls: m.className })),
);
if (mains.length !== 1 || mains[0]?.id !== "main") {
  fail(
    "home page has exactly one non-hidden main landmark (#main)",
    `found ${mains.map((m) => `main#${m.id}.${m.cls}`).join(", ") || "none"}`,
  );
} else {
  ok("home page has exactly one non-hidden main landmark (#main)");
}

// The desktop row shows each destination exactly once: Notifications lives
// on the bell shortcut, not as a second row entry (docs/05 §1).
const rowTexts = await page.locator(".global-nav-links a:visible").allTextContents();
const expectedRow = ["Home", "Explore", "Projects", "Assets", "People", "Organizations"];
if (rowTexts.map((t) => t.trim()).join("|") !== expectedRow.join("|")) {
  fail("desktop row shows each destination exactly once", `row=${JSON.stringify(rowTexts.map((t) => t.trim()))}`);
} else {
  ok("desktop row shows each destination exactly once (bell carries Notifications)");
}

const headerStyles = await computed(header, ["backgroundImage", "backdropFilter", "backgroundColor"]);
if (headerStyles.backgroundImage !== "none" || headerStyles.backdropFilter !== "none") {
  fail("header has no gradient/glass", JSON.stringify(headerStyles));
} else {
  ok("header background: no gradient, no backdrop filter");
}
if (isPurple(headerStyles.backgroundColor)) {
  fail("header background is neutral (not purple)", `bg=${headerStyles.backgroundColor}`);
} else {
  ok(`header background is neutral: ${headerStyles.backgroundColor}`);
}

/* Every destination renders under the nav (stub pages, honest 200s). */
const destinations = [
  ["/explore", "Explore"],
  ["/projects", "Projects"],
  ["/assets", "Assets"],
  ["/people", "People"],
  ["/organizations", "Organizations"],
  ["/notifications", "Notifications"],
  ["/search?q=solid-state", "Search"],
];
for (const [href, h1] of destinations) {
  const res = await page.goto(BASE + href, { waitUntil: "load" });
  if (res.status() !== 200) {
    fail(`GET ${href} -> 200`, `got ${res.status()}`);
    continue;
  }
  const title = await page.locator("h1").first().textContent();
  if (title?.trim() !== h1) {
    fail(`GET ${href} renders h1 ${h1}`, `got h1=${title?.trim()}`);
    continue;
  }
  if ((await page.locator(".global-nav").count()) !== 1) {
    fail(`GET ${href} keeps the global header`);
  } else if (h1 === "Notifications") {
    // Notifications is carried by the bell on desktop; the active marker
    // lives on the bell link, not in the text row.
    await page.waitForFunction(
      () => document.querySelector('a[aria-label="Notifications"][aria-current="page"]') !== null,
      undefined,
      { timeout: 5000 },
    ).catch(() => fail(`aria-current follows route on ${href}`, "bell not marked after hydration"));
  } else if (h1 !== "Search") {
    await page.waitForFunction(
      (want) => document.querySelector(".global-nav-link[aria-current='page']")?.textContent?.trim() === want,
      h1,
      { timeout: 5000 },
    ).catch(() => fail(`aria-current follows route on ${href}`, "not set after hydration"));
  }
  ok(`GET ${href} -> 200, h1 ${h1}, header present`);
}
ok("search result page echoes the query", await page.locator("q").textContent() === "solid-state");

/* The sign-in surface keeps the slim wordmark header (GitHub-style). */
await page.goto(BASE + "/login", { waitUntil: "load" });
const loginHeader = page.locator(".global-nav");
ok("/login header shows the wordmark only", (await loginHeader.count()) === 1 && (await loginHeader.locator(".global-nav-links").count()) === 0);

/* ---------- C. Responsive baseline ---------- */

await page.setViewportSize({ width: 768, height: 800 });
await page.goto(BASE, { waitUntil: "load" });
ok("768px: desktop nav row visible", await page.locator(".global-nav-links").first().isVisible());

await page.setViewportSize({ width: 375, height: 667 });
await page.goto(BASE, { waitUntil: "load" });
if (await page.locator(".global-nav-links").first().isVisible()) {
  fail("375px: desktop nav row collapses");
} else {
  ok("375px: desktop nav row collapses");
}
if (await page.locator('form[role="search"]').first().isVisible()) {
  fail("375px: header search form collapses (drawer carries Search)");
} else {
  ok("375px: header search form collapses (drawer carries Search)");
}
const toggle = page.locator('button[aria-label="Global navigation menu"]');
ok("375px: mobile menu toggle visible", await toggle.isVisible());

// The SSR'd button is inert until hydration attaches its click handler, so
// a single click can land before it is interactive. Click in-page until the
// drawer actually opens — a bounded readiness wait, not a retry that masks
// a real failure (the drawer must open for the checks below to pass).
let drawerOpened = false;
try {
  await page.waitForFunction(() => {
    const t = document.querySelector('button[aria-label="Global navigation menu"]');
    if (!t) return false;
    if (t.getAttribute("aria-expanded") !== "true") t.click();
    return t.getAttribute("aria-expanded") === "true";
  }, undefined, { polling: 200, timeout: 5000 });
  drawerOpened = true;
} catch {
  drawerOpened = false;
}

const missing = [];
for (const label of DRAWER_LABELS) {
  const item = page.locator(`#global-nav-drawer a:visible`, { hasText: label }).first();
  if ((await item.count()) === 0) missing.push(label);
}
if (missing.length > 0) fail(`drawer contains ${missing.join(", ")}`);
else ok(`drawer lists ${DRAWER_LABELS.join(", ")}`);
if (!drawerOpened) {
  fail("drawer toggle aria-expanded=true while open", "drawer never opened after 5s");
} else {
  ok("drawer toggle aria-expanded=true while open");
}
if (missing.length === 0 && drawerOpened) {
  ok("drawer keeps every destination reachable on mobile");
}

/* ---------- D. Screenshots as artifacts ---------- */

const shots = process.env.T0107_SHOT_DIR ?? path.join(import.meta.dirname, "shots");
fs.mkdirSync(shots, { recursive: true });
await page.screenshot({ path: path.join(shots, "mobile-drawer.png") });
await page.keyboard.press("Escape");
await page.setViewportSize({ width: 1280, height: 800 });
await page.goto(BASE, { waitUntil: "load" });
await page.screenshot({ path: path.join(shots, "desktop-home.png"), fullPage: true });
console.log(`# screenshots: ${shots}`);

await browser.close();

if (fails > 0) {
  console.log(`\nvisual smoke: ${fails} check(s) FAILED`);
  process.exit(1);
}
console.log("\nvisual smoke: all checks passed");
