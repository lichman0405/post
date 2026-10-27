/**
 * Entry-point pre-flight for the WEB-01 journey (docs/70 §6.4).
 *
 * Before a single business step runs, this checks that the page it is about
 * to drive actually offers the entries the journey depends on:
 *
 *   - the sign-in form (email, password, primary submit);
 *   - the account-establishment entry (create-an-account control).
 *
 * A missing service, a missing route or a missing entry point exits non-zero
 * with the reason. There is no skip path: docs/70 §6.4 says a missing
 * service or entry fails the run, and an always-green checker would be the
 * exact bypass G23 exists to catch.
 *
 * Usage: node tests/web-v2/entrypoint-check.mjs <login-url>
 */

import { chromium } from "playwright";

const LOGIN_URL = process.argv[2] ?? process.env.WEB_V2_LOGIN_URL ?? "";
if (LOGIN_URL === "") {
  console.error("web-v2 entrypoint-check: FAILED — no login URL was provided");
  process.exit(1);
}

function fail(message) {
  console.error(`web-v2 entrypoint-check: FAILED — ${message}`);
  process.exit(1);
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, locale: "en-US" });
    const page = await context.newPage();
    let response;
    try {
      response = await page.goto(LOGIN_URL, { waitUntil: "domcontentloaded", timeout: 20000 });
    } catch (err) {
      fail(`service unreachable at ${LOGIN_URL}: ${err instanceof Error ? err.message : String(err)} — a skip is not a pass`);
    }
    if (response === null) {
      fail(`no HTTP response from ${LOGIN_URL} — a skip is not a pass`);
    }
    if (response.status() >= 400) {
      fail(`login route answered HTTP ${response.status()} at ${LOGIN_URL}`);
    }

    async function require(selector, what) {
      const locator = page.locator(selector).first();
      try {
        await locator.waitFor({ state: "visible", timeout: 10000 });
      } catch {
        const count = await page.locator(selector).count().catch(() => 0);
        fail(`${what} is missing: expected a visible ${selector} at ${LOGIN_URL} (found ${count})`);
      }
    }

    await require(".auth-card", "the sign-in card");
    await require('form input[type="email"]', "the sign-in email field");
    await require('form input[type="password"]', "the sign-in password field");
    await require('form button[type="submit"]', "the primary sign-in button");
    // The account-establishment entry is a separate control that switches
    // the same card into sign-up mode. Without it a first-time user (the
    // U01 role) cannot obtain an account from the UI at all.
    await require(".auth-sub-after button", "the account-establishment entry (create an account)");

    console.log(`web-v2 entrypoint-check: OK — sign-in form and account-establishment entry present at ${LOGIN_URL}`);
  } finally {
    await browser.close();
  }
}

main().catch((err) => {
  fail(err instanceof Error ? err.message : String(err));
});
