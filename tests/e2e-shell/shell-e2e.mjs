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
 *     (Overview, Research, Issues, Pull requests, Releases, Milestones,
 *     Assets, Files, Activity, Settings), with aria-current following the
 *     page. The bar is asserted as an ORDERED contract, not as a set: a tab
 *     that changed place is a navigation change, and "these labels all
 *     appear somewhere" cannot see one.
 *   - the tabs that are now REAL pages are asserted to render their content,
 *     not merely to mount: every route whose TabPlaceholder was replaced
 *     (research, issues, pulls, releases, milestones, assets, files, activity,
 *     settings) is required to LIST rows. A mount-only check also passes on
 *     the empty and the error state, so it could not tell a working page from
 *     a broken one.
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

/* The four lists the REAL tab pages read (T0403 pulls, the releases hub,
 * T0609 milestones, the Activity feed). Each shape is the one its client
 * validates and cmd/api answers, and each page is asserted to LIST these
 * rows — so a row that stops travelling is a failure here, not a quietly
 * emptier page. The pulls shape is the one tests/e2e-pulls/pulls-e2e.mjs
 * drives end to end. */

const ALLOY_PULLS = [
  {
    id: "pr-00000000-0000-4000-8000-000000000007",
    number: 7,
    title: "Propose the single-phase claim",
    body: "Adds the claim, the evidence assertion behind it and the supports edge.",
    state: "open",
    source_branch_id: "branch-proposal",
    target_branch_id: "branch-main",
    base_state_id: "bbbbbbbb-0000-4000-8000-000000000001",
    proposed_state_id: "dddddddd-0000-4000-8000-000000000002",
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-14T09:00:00Z",
  },
];

const ALLOY_RELEASES = [
  {
    id: "11111111-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
    project_id: ALLOY.id,
    version: "v0.1.0",
    title: "First reproducible snapshot",
    state_id: "eeeeeeee-0000-4000-8000-000000000005",
    policy_version_id: null,
    org_policy_version_id: null,
    manifest_hash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-15T09:00:00Z",
  },
];

const ALLOY_MILESTONES = [
  {
    id: "22222222-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
    project_id: ALLOY.id,
    kind: "candidate_selected",
    label: null,
    occurred_at: "2026-09-16T09:00:00Z",
    release_id: null,
    created_by: "00000000-0000-4000-8000-000000000001",
    created_at: "2026-09-16T09:00:00Z",
  },
];

const ALLOY_ACTIVITY = [
  {
    id: "33333333-cccc-4ccc-8ccc-cccccccccccc",
    source: "governance",
    actor_id: "00000000-0000-4000-8000-000000000001",
    actor_handle: "alice",
    actor_display_name: "Alice Guo",
    via: "ui",
    action: "project.created",
    target_ref: null,
    project_id: ALLOY.id,
    organization_id: null,
    correlation_id: "44444444-dddd-4ddd-8ddd-dddddddddddd",
    before_summary: null,
    after_summary: null,
    metadata: null,
    visibility: null,
    occurred_at: "2026-09-10T08:00:00Z",
  },
];

// The Research/Overview tab (T0108 demo, PR #338) reads an overview aggregate
// and renders counts/cards. The shape matches what
// apps/web/app/(main)/projects/[id]/research/page.tsx expects.
const ALLOY_OVERVIEW = {
  counts: { questions: 2, findings: 1, hypotheses: 2, claims: 3, other_objects: 0 },
  key_questions: [
    {
      object_id: "qqqqqqqq-0000-4000-8000-000000000001",
      statement: "Can MOF-X separate ethylene under humidity?",
      question_state: "partially_answered",
      hypotheses: [
        { object_id: "hhhhhhhh-0000-4000-8000-000000000001", object_type: "hypothesis", title: "Open-metal sites bind preferentially" },
      ],
      findings: [
        { object_id: "ffffffff-0000-4000-8000-000000000001", object_type: "finding", title: "Humidity above 40% RH collapses selectivity" },
      ],
    },
  ],
  key_findings: [
    {
      object_id: "ffffffff-0000-4000-8000-000000000001",
      statement: "Humidity above 40% RH collapses selectivity",
      finding_type: "observation",
      assessment: "supporting",
      claims: [
        { object_id: "cccccccc-0000-4000-8000-000000000001", version_id: "vvvvvvvv-0000-4000-8000-000000000001", title: "Selectivity loss is reversible below 30% RH", resolved: true },
      ],
    },
  ],
  branches: {
    active: [
      { id: "bbbbbbbb-0000-4000-8000-000000000001", name: "humidity-sweep", purpose: "Map selectivity vs RH", head_state_id: "ssssssss-0000-4000-8000-000000000001" },
    ],
    merged: 1,
    aborted: 0,
  },
  current_main: {
    head_state_id: "ssssssss-0000-4000-8000-000000000002",
    latest_commit: { message: "Accept humidity threshold", actor: "alice" },
  },
  empty: false,
};

// The Assets tab (T0108 demo, PR #338) queries project objects and maps over
// them. Without a mocked query endpoint the page dereferences undefined and
// throws an uncaught error, so the mock must answer this route.
const ALLOY_ASSETS = {
  objects: [
    {
      id: "oooooooo-0000-4000-8000-000000000001",
      object_type: "dataset",
      version_no: 1,
      title: "Breakthrough curves at 40% RH",
      payload: { purpose: "Primary humidity-validation dataset" },
    },
    {
      id: "oooooooo-0000-4000-8000-000000000002",
      object_type: "protocol",
      version_no: 2,
      title: "Activation and sample prep",
      payload: { purpose: "Standardized activation protocol" },
    },
  ],
};

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
    // The four real tab pages, each with the envelope its own client
    // validates: pulls answers a BARE ARRAY, the other three an object with
    // the list under its name plus a cursor where the feed paginates.
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/pull-requests`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ALLOY_PULLS) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/releases`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ releases: ALLOY_RELEASES }) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/milestones`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ milestones: ALLOY_MILESTONES }) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/activity`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ entries: ALLOY_ACTIVITY, next_cursor: null }) });
    }
    // The Files tab (T0308) is the real read-only page now: it mounts one
    // tree read at the root of main — served here so the tab renders its
    // actual content, not an error state.
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/files/tree`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ref: "main",
          path: "",
          sha: "tree-e2e-1",
          entries: [
            { name: "README.md", path: "README.md", type: "blob", mode: "100644", size: 12, sha: "s1" },
            { name: "docs", path: "docs", type: "tree", mode: "040000", size: 0, sha: "s2" },
          ],
        }),
      });
    }
    // The Research/Overview tab (T0108 demo, PR #338) is a real page now.
    // It reads the same overview aggregate the landing /projects/[id] page does.
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/overview`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ALLOY_OVERVIEW) });
    }
    // The Assets tab (T0108 demo, PR #338) queries project objects and lists
    // them; an unmocked 404 makes it throw, so it must be answered here.
    if (method === "GET" && pathname === `/api/v1/projects/${ALLOY.id}/query`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ALLOY_ASSETS) });
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

/**
 * The tab bar as the CONTRACT states it — specs/ui/routes.yaml
 * `project_tabs` plus the label each key renders (docs/05 §3 Project 导航).
 * Asserted as an ordered list, key AND label: the previous shape compared a
 * count and a set of labels, which accepts any permutation of the bar and
 * any key/label mismatch that still produces the same set of strings.
 */
const TAB_CONTRACT = [
  { key: "overview", label: "Overview" },
  { key: "research", label: "Research" },
  { key: "issues", label: "Issues" },
  { key: "pulls", label: "Pull requests" },
  { key: "releases", label: "Releases" },
  { key: "milestones", label: "Milestones" },
  { key: "assets", label: "Assets" },
  { key: "files", label: "Files" },
  { key: "activity", label: "Activity" },
  { key: "settings", label: "Settings" },
];

/** waitFor returns the selector's fate rather than throwing: a route that
 *  fails must be REPORTED as one failure and let the remaining routes run.
 *  The previous shape threw on the first missing selector and killed the
 *  process, which is how pulls, releases and activity all drifted beneath a
 *  single crash — everything after the first stale route was never probed. */
const waitFor = async (selector) => {
  try {
    await page.waitForSelector(selector);
    return true;
  } catch {
    return false;
  }
};

const assertTab = async ({ key, path: pathSuffix, placeholder, ready, rows }) => {
  try {
    await page.click(`[data-project-tab="${key}"]`);
    await page.waitForURL(`**/projects/${ALLOY.id}${pathSuffix}`);
  } catch (error) {
    fail(`tab ${key}: navigates to ${pathSuffix}`, String(error).slice(0, 160));
    return;
  }
  const current = await page.getAttribute(`[data-project-tab="${key}"]`, "aria-current");
  if (current !== "page") {
    fail(`tab ${key}: aria-current`, `got ${current}, want page`);
  } else {
    ok(`tab ${key}: navigates to ${pathSuffix} and carries aria-current=page`);
  }
  if (placeholder !== null) {
    if (await waitFor(`[data-tab-placeholder="${placeholder}"]`)) {
      ok(`tab ${key}: renders the ${placeholder} placeholder`);
    } else {
      fail(`tab ${key}: renders the ${placeholder} placeholder`, "the placeholder is gone — if a real page replaced it, point this route at that page instead of dropping the check");
    }
    return;
  }
  if (!(await waitFor(ready))) {
    fail(`tab ${key}: the real page renders`, `nothing matched ${ready}`);
    return;
  }
  if (rows === undefined) {
    ok(`tab ${key}: the real page renders`);
    return;
  }
  if (!(await waitFor(rows))) {
    fail(`tab ${key}: the real page LISTS its rows`, `nothing matched ${rows} — a page that mounts but lists nothing is also what an error or empty state looks like`);
    return;
  }
  const listed = await page.locator(rows).count();
  ok(`tab ${key}: the real page renders and lists ${listed} row(s) of ${rows}`);
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

/* ---------- 2. Shell renders with badges and the whole tab contract ---------- */

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
// The bar, read as an ordered (key, label) list and compared to the
// contract position by position. A permutation, a renamed label, a missing
// tab and an extra one are four different failures here; the count-and-set
// check this replaces saw only the last two.
const renderedTabs = await page.locator(".project-tab").evaluateAll((tabs) =>
  tabs.map((tab) => ({
    key: tab.getAttribute("data-project-tab"),
    label: (tab.textContent ?? "").trim(),
  })),
);
const barShape = (tabs) => tabs.map((tab) => `${tab.key}=${tab.label}`).join(" | ");
const divergence = TAB_CONTRACT.findIndex(
  (tab, i) => renderedTabs[i]?.key !== tab.key || renderedTabs[i]?.label !== tab.label,
);
if (divergence !== -1 || renderedTabs.length !== TAB_CONTRACT.length) {
  const where = divergence === -1
    ? `the bar has ${renderedTabs.length} tabs, the contract has ${TAB_CONTRACT.length}`
    : `the first divergence is at position ${divergence}`;
  fail("shell: the tab bar is the contract, in order",
    `${where}\n  contract: ${barShape(TAB_CONTRACT)}\n  rendered: ${barShape(renderedTabs)}`);
} else {
  ok(`shell: the tab bar is the contract in order — ${TAB_CONTRACT.length} tabs, ` +
     `${TAB_CONTRACT[0].label} … ${TAB_CONTRACT[TAB_CONTRACT.length - 1].label}`);
}

/* ---------- 3. 所有 route 可导航 ---------- */

// Each entry is the route's state as the page source has it today:
// `placeholder` is the TabPlaceholder title the route still renders (null
// once the real page replaced it), `ready` proves the real page mounted and
// `rows` proves it lists something. The earlier manifest named a
// placeholder for THREE routes the product had already replaced — the run
// crashed on the first of them, so the other two were never reached.
const TAB_ROUTES = [
  // T0108 demo (PR #338) replaced the Research placeholder with a real
  // overview page that lists counts, questions, findings and branches.
  { key: "research", path: "/research", placeholder: null, ready: '[data-project-tab-content="research"]', rows: ".research-card" },
  // T0108 demo (PR #338) replaced the Issues placeholder with a real page
  // that lists demo issues.
  { key: "issues", path: "/issues", placeholder: null, ready: '[data-project-tab-content="issues"]', rows: ".demo-issue" },
  // T0403 replaced the Pull requests placeholder with the real list page.
  { key: "pulls", path: "/pulls", placeholder: null, ready: "[data-pulls-list]", rows: "[data-pull-row]" },
  // The releases hub is a real page of its own (list + manifest links).
  { key: "releases", path: "/releases", placeholder: null, ready: '[data-project-tab-content="releases"]', rows: "[data-release-row]" },
  // T0609's milestones page; specs/ui/routes.yaml carries the tab.
  { key: "milestones", path: "/milestones", placeholder: null, ready: '[data-project-tab-content="milestones"]', rows: "[data-milestone-row]" },
  // T0108 demo (PR #338) replaced the Assets placeholder with a real page
  // that queries project objects; the mock above serves the query endpoint.
  { key: "assets", path: "/assets", placeholder: null, ready: '[data-project-tab-content="assets"]', rows: ".demo-asset" },
  // T0308 replaced the Files placeholder with the real read-only page.
  { key: "files", path: "/files", placeholder: null, ready: "[data-files-page]", rows: "[data-files-entry]" },
  // The Activity feed is the real member-only timeline.
  { key: "activity", path: "/activity", placeholder: null, ready: '[data-project-tab-content="activity"]', rows: "[data-activity-row]" },
  // T0109 replaced the Settings placeholder with the real settings page.
  { key: "settings", path: "/settings", placeholder: null, ready: "[data-settings]", rows: "[data-member-row]" },
  // Overview is the landing route, a summary rather than a list.
  { key: "overview", path: "", placeholder: null, ready: '[data-project-tab-content="overview"]' },
];
for (const route of TAB_ROUTES) {
  await assertTab(route);
}
const realRoutes = TAB_ROUTES.filter((route) => route.placeholder === null);
const listingRoutes = realRoutes.filter((route) => route.rows !== undefined);
ok(`navigability: all ${TAB_ROUTES.length} routes reachable — ` +
   `${TAB_ROUTES.length - realRoutes.length} still placeholders, ` +
   `${realRoutes.length} real pages, ${listingRoutes.length} of them asserted to list rows`);

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
