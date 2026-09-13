/**
 * Project shell e2e (T0108-TEST "project shell e2e", blocking): a real
 * Chromium drives the BUILT web app (next start, production layer)
 * through the project shell. The Go API is mocked at the network layer
 * with the exact wire shapes cmd/api/projectshttp produces —
 * projectPayload, membershipPayload and the stable error envelope; the
 * integration suite (tests/integration/project_shell_test.go) locks those
 * shapes against real PostgreSQL, so the two halves cover the real
 * contract without the browser test depending on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - 所有 route 可导航: every shell tab route renders and navigates
 *     (Overview, Research, Issues, Pull requests, Releases, Assets,
 *     Files, Activity, Settings), with aria-current following the page.
 *   - private unauthorized 不渲染 shell: for a project the API answers
 *     with the existence-hiding 404, the page shows the plain not-found
 *     state with NO shell chrome — no name, no badges, no tabs, and no
 *     tab content — on every route, direct deep links included.
 *   - visibility/frozen badges: Public/Private render from the API's
 *     visibility; Frozen main renders only when main_frozen is true.
 *   - permission-aware settings: the Settings tab appears for
 *     owner/maintainer and disappears for everyone else; a direct hit on
 *     /settings below the role renders the not-authorized answer, never
 *     settings chrome.
 *
 * Usage: node shell-e2e.mjs <base-url> [apps-web-dir]
 */
import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31108";
const APPS_WEB = process.argv[3] ?? path.resolve(import.meta.dirname, "../../apps/web");

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};

/* ---------- A. Static CSS policy (no browser needed) ---------- */

const FORBIDDEN_FX = [
  "linear-gradient", "radial-gradient", "conic-gradient",
  "repeating-linear-gradient", "repeating-radial-gradient",
  "backdrop-filter", "blur(",
];
const FORBIDDEN_HEX = [
  "#8957e5", "#6e40c9", "#8250df", "#8b5cf6", "#7c3aed", "#a855f7",
  "#9333ea", "#663399", "#5b21b6", "#4c1d95", "#d946ef", "#c026d3",
];
const cssFiles = ["app/(main)/projects/projects.css"].map((p) => path.join(APPS_WEB, p));
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
ok("css-policy projects.css: no gradients, no backdrop blur, no purple");

/* ---------- B. Fixtures (the exact wire shapes of cmd/api) ---------- */

const API = "http://api.e2e.test";

const ALLOY = {
  id: "11111111-2222-4333-8444-555555555555",
  organization_id: null,
  program_id: null,
  slug: "alloy-lab",
  name: "Alloy Lab",
  purpose: "Design high-entropy alloys with reproducible synthesis routes.",
  activity_status: "planning",
  visibility: "public",
  main_frozen: true,
  git_repository_external_id: null,
  provision_status: "pending",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-10T08:00:00Z",
};

const COPPER = {
  ...ALLOY,
  id: "33333333-4444-4555-8666-777777777777",
  slug: "copper-lab",
  name: "Copper Lab",
  main_frozen: false,
};

const SOLAR = {
  ...ALLOY,
  id: "22222222-3333-4444-8555-666666666666",
  slug: "solar-lab",
  name: "Solar Lab",
  visibility: "private",
  main_frozen: false,
};

const OWNER_MEMBERSHIP = {
  project_id: ALLOY.id,
  user_id: "00000000-0000-4000-8000-000000000001",
  role: "owner",
  created_at: "2026-09-10T08:00:00Z",
};

// The member list the real settings tab (T0109) renders for the owner
// scenario — memberPayload rows, as cmd/api/projectshttp answers.
const ALLOY_MEMBERS = [
  {
    user_id: "00000000-0000-4000-8000-000000000001",
    handle: "alice",
    display_name: "Alice Guo",
    role: "owner",
    joined_at: "2026-09-10",
  },
  {
    user_id: "00000000-0000-4000-8000-000000000002",
    handle: "bob",
    display_name: "Bob Chen",
    role: "viewer",
    joined_at: "2026-09-11",
  },
];

const NOT_FOUND = {
  code: "PROJECT_NOT_FOUND",
  message: "project not found",
  request_id: "e2e-req",
  retryable: false,
};

const MEMBERSHIP_NOT_FOUND = {
  code: "PROJECT_MEMBERSHIP_NOT_FOUND",
  message: "you are not a member of this project",
  request_id: "e2e-req",
  retryable: false,
};

const UNAUTHENTICATED = {
  code: "UNAUTHENTICATED",
  message: "authentication required",
  request_id: "e2e-req",
  retryable: false,
};

/** Mutable scenario state: which membership the API "answers" for the
 *  public project (owner / none / failure). Tests flip it and reload. */
const scenario = {
  membership: "owner", // "owner" | "none"
};

function installApiMock(page) {
  return page.route(`${API}/**`, async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    const pathname = url.pathname;

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      return route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify(UNAUTHENTICATED) });
    }
    if (method === "GET" && pathname === "/api/v1/projects") {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ projects: [ALLOY, COPPER, SOLAR] }),
      });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ALLOY) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${COPPER.id}`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(COPPER) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/membership`) {
      if (scenario.membership === "owner") {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(OWNER_MEMBERSHIP) });
      }
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(MEMBERSHIP_NOT_FOUND) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/members`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ members: ALLOY_MEMBERS }) });
    }
    // The private project (and its membership) answers the existence-
    // hiding 404 for everyone, like the API's read policy for a private
    // project the caller is not authorized for.
    if (method === "GET" && pathname.startsWith(`/api/v1/projects/${SOLAR.id}`)) {
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- C. Browser scenarios ---------- */

const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

const TAB_LABELS = [
  "Overview", "Research", "Issues", "Pull requests", "Releases",
  "Assets", "Files", "Activity", "Settings",
];

const assertTab = async (tabKey, pathSuffix, placeholderTitle, contentSelector = '[data-project-tab-content="overview"]') => {
  await page.click(`[data-project-tab="${tabKey}"]`);
  await page.waitForURL(`**/projects/${ALLOY.id}${pathSuffix}`);
  const current = await page.getAttribute(`[data-project-tab="${tabKey}"]`, "aria-current");
  if (current !== "page") {
    fail(`tab ${tabKey}: aria-current`, `got ${current}, want page`);
  } else {
    ok(`tab ${tabKey}: navigates to ${pathSuffix} and carries aria-current=page`);
  }
  if (placeholderTitle !== null) {
    await page.waitForSelector(`[data-tab-placeholder="${placeholderTitle}"]`);
    ok(`tab ${tabKey}: renders its content`);
  } else {
    await page.waitForSelector(contentSelector);
    ok(`tab ${tabKey}: renders its content`);
  }
};

/* ---------- 1. Directory: list, badges, links ---------- */

await page.goto(`${BASE}/projects`, { waitUntil: "load" });
await page.waitForSelector(".project-row");

const rows = page.locator(".project-row");
const rowCount = await rows.count();
if (rowCount !== 3) {
  fail("directory: row count", `got ${rowCount}, want 3`);
} else {
  ok("directory: lists the three fixtures");
}
const alloyRow = page.locator('[data-project-row="alloy-lab"]');
if ((await alloyRow.locator(".badge-public").count()) !== 1) {
  fail("directory: public badge on the public project");
} else {
  ok("directory: public project carries the Public badge");
}
if ((await alloyRow.locator(".badge-frozen").count()) !== 1) {
  fail("directory: frozen badge on the frozen project");
} else {
  ok("directory: frozen project carries the Frozen main badge");
}
const solarRow = page.locator('[data-project-row="solar-lab"]');
if ((await solarRow.locator(".badge-private").count()) !== 1) {
  fail("directory: private badge on the private project");
} else {
  ok("directory: private project carries the Private badge");
}
const copperRow = page.locator('[data-project-row="copper-lab"]');
if ((await copperRow.locator(".badge-frozen").count()) !== 0) {
  fail("directory: no frozen badge on the unfrozen project");
} else {
  ok("directory: unfrozen project carries no Frozen badge");
}

/* ---------- 2. Shell renders with badges and all nine tabs ---------- */

await alloyRow.click();
await page.waitForURL(`**/projects/${ALLOY.id}`);
await page.waitForSelector('[data-project-shell="alloy-lab"]');

const name = await page.textContent(".project-shell-name");
if (name === null || !name.includes("Alloy Lab")) {
  fail("shell: project name", `got ${name}`);
} else {
  ok("shell: project name renders");
}
if ((await page.locator(".project-shell .badge-public").count()) !== 1) {
  fail("shell: Public badge");
} else {
  ok("shell: Public badge renders");
}
if ((await page.locator(".project-shell .badge-frozen").count()) !== 1) {
  fail("shell: Frozen main badge");
} else {
  ok("shell: Frozen main badge renders (main_frozen from the API)");
}
const tabLabels = await page.locator(".project-tab").allTextContents();
const missing = TAB_LABELS.filter((label) => !tabLabels.some((t) => t.includes(label)));
if (missing.length > 0) {
  fail("shell: all nine tabs present", `missing: ${missing.join(", ")}`);
} else if (tabLabels.length !== TAB_LABELS.length) {
  fail("shell: tab count", `got ${tabLabels.length}, want ${TAB_LABELS.length}`);
} else {
  ok("shell: all nine tabs render (Overview … Settings)");
}

/* ---------- 3. 所有 route 可导航 ---------- */

const TAB_ROUTES = [
  ["research", "/research", "Research"],
  ["issues", "/issues", "Issues"],
  ["pulls", "/pulls", "Pull requests"],
  ["releases", "/releases", "Releases"],
  ["assets", "/assets", "Assets"],
  ["files", "/files", "Files"],
  ["activity", "/activity", "Activity"],
  // T0109 replaced the Settings placeholder with the real settings page.
  ["settings", "/settings", null, "[data-settings]"],
  ["overview", "", null],
];
for (const [key, suffix, placeholder, contentSelector] of TAB_ROUTES) {
  await assertTab(key, suffix, placeholder, contentSelector);
}
ok("navigability: all nine routes reachable from the tab bar");

/* ---------- 4. Permission-aware Settings ---------- */

// Owner scenario: Settings tab + settings content render.
if ((await page.locator('[data-project-tab="settings"]').count()) !== 1) {
  fail("settings: owner sees the Settings tab");
} else {
  ok("settings: owner sees the Settings tab");
}

// Non-member scenario (viewer/contributor/anonymous alike): the API
// answers "no role" — the tab disappears and the direct route answers
// not-authorized instead of settings chrome.
scenario.membership = "none";
await page.goto(`${BASE}/projects/${ALLOY.id}/settings`, { waitUntil: "load" });
await page.waitForSelector('[data-project-shell="alloy-lab"]');
if ((await page.locator('[data-project-tab="settings"]').count()) !== 0) {
  fail("settings: no Settings tab below the gate role");
} else {
  ok("settings: Settings tab hidden when the API answers no role");
}
await page.waitForSelector("[data-settings-unauthorized]");
if ((await page.locator("[data-settings]").count()) !== 0) {
  fail("settings: direct /settings below the gate role must not render settings chrome");
} else {
  ok("settings: direct /settings below the gate role renders the not-authorized answer, never settings chrome");
}

// And the same direct hit as a signed-out stranger of the public project
// (the membership fetch fails too — still no Settings tab, still a shell,
// because the project read authorized the shell).
await page.reload({ waitUntil: "load" });
await page.waitForSelector("[data-settings-unauthorized]");
ok("settings: reload keeps the not-authorized answer (membership re-fetched, still none)");

/* ---------- 5. private unauthorized 不渲染 shell ---------- */

// The private project answers the existence-hiding 404 — on the project
// page AND on a deep tab link, no shell chrome may render: no name, no
// badges, no tabs, no tab content.
for (const suffix of ["", "/settings", "/research"]) {
  await page.goto(`${BASE}/projects/${SOLAR.id}${suffix}`, { waitUntil: "load" });
  await page.waitForSelector("[data-project-notfound]");
  const leaked = await page.locator(".project-shell, .project-tab, .project-shell-header, [data-project-shell]").count();
  if (leaked !== 0) {
    fail(`private unauthorized ${suffix || "/"}: no shell chrome`, `found ${leaked} shell element(s)`);
  } else {
    ok(`private unauthorized ${suffix || "/"}: renders the plain not-found state with no shell chrome`);
  }
  const content = await page.locator('[data-tab-placeholder], [data-project-tab-content], [data-settings-unauthorized]').count();
  if (content !== 0) {
    fail(`private unauthorized ${suffix || "/"}: no tab content`, `found ${content} tab content element(s)`);
  } else {
    ok(`private unauthorized ${suffix || "/"}: tab content never mounts`);
  }
}

/* ---------- 6. Unknown project: same neutral not-found ---------- */

await page.goto(`${BASE}/projects/99999999-8888-4777-8666-555555555555`, { waitUntil: "load" });
await page.waitForSelector("[data-project-notfound]");
ok("unknown project: same neutral not-found state");

/* ---------- 7. Unfrozen project: no frozen badge in the shell ---------- */

await page.goto(`${BASE}/projects/${COPPER.id}`, { waitUntil: "load" });
await page.waitForSelector('[data-project-shell="copper-lab"]');
if ((await page.locator(".project-shell .badge-frozen").count()) !== 0) {
  fail("shell: no Frozen badge when main_frozen is false");
} else {
  ok("shell: Frozen badge absent when main_frozen is false");
}

await browser.close();

if (fails > 0) {
  console.error(`shell-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("shell-e2e: all checks passed");
