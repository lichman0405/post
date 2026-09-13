/**
 * Project settings e2e (T0109 required test "settings e2e", blocking): a
 * real Chromium drives the BUILT web app (next start, production layer)
 * through the settings tab. The Go API is mocked at the network layer
 * with the exact wire shapes cmd/api/projectshttp produces — the flat
 * projectPayload, membershipPayload, memberPayload and the stable error
 * envelope; the integration suite (tests/integration/project_settings_test.go)
 * locks those shapes AND the server-side rules (role gate, CSRF, audit
 * placeholder rows) against real PostgreSQL, so the two halves cover the
 * real contract without the browser test depending on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - 成员列表/角色变更: the member table renders identity + role + join
 *     date; a role change PUTs {role} with the session-bound X-CSRF-Token
 *     header and the row updates; a refused change (LAST_OWNER) renders
 *     the stable error message and changes nothing.
 *   - viewer 无 settings: with the API answering a viewer membership, the
 *     Settings tab disappears and a direct /settings hit renders the
 *     not-authorized answer — never settings chrome, and no settings
 *     calls leave the page.
 *   - visibility 预览: the visibility control is disabled and carries the
 *     preview-only note (the publishing guard is a later milestone).
 *   - 项目目的/活动状态编辑: the purpose/activity edit PATCHes the
 *     changed fields with the CSRF header, shows success, and the shell
 *     header reflects the saved purpose without a reload.
 *   - owner/maintainer 动作有 audit placeholder: the page states that
 *     owner/maintainer changes are recorded in the audit log (the audit
 *     row itself is the integration suite's proof); a maintainer sees the
 *     owner rows locked and cannot pick the owner role.
 *
 * Usage: node settings-e2e.mjs <base-url> [apps-web-dir]
 */
import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31109";
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

const LAB = {
  id: "11111111-2222-4333-8444-555555555555",
  organization_id: null,
  program_id: null,
  slug: "settings-lab",
  name: "Settings Lab",
  purpose: "Exercise the settings surface.",
  activity_status: "planning",
  visibility: "public",
  main_frozen: false,
  git_repository_external_id: null,
  provision_status: "pending",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-10T08:00:00Z",
};

const ALICE = "00000000-0000-4000-8000-000000000001";
const BOB = "00000000-0000-4000-8000-000000000002";
const CAROL = "00000000-0000-4000-8000-000000000003";

const NOT_FOUND = {
  code: "PROJECT_NOT_FOUND",
  message: "project not found",
  request_id: "e2e-req",
  retryable: false,
};

const LAST_OWNER = {
  code: "LAST_OWNER",
  message: "the project must keep at least one owner",
  request_id: "e2e-req",
  retryable: false,
};

/** The actor the API "sees" per role scenario (real user ids decide the
 *  shell's userId, so the page can mark the "(you)" row). */
const ACTOR = { owner: ALICE, maintainer: BOB, viewer: CAROL };

/** Mutable scenario state. setScenario rebuilds it per role. */
const scenario = {
  role: "owner",
  userId: ALICE,
  purpose: LAB.purpose,
  activityStatus: "planning",
  members: [
    { user_id: ALICE, handle: "alice", display_name: "Alice Guo", role: "owner", joined_at: "2026-09-10" },
    { user_id: BOB, handle: "bob", display_name: "Bob Chen", role: "viewer", joined_at: "2026-09-11" },
    { user_id: CAROL, handle: "carol", display_name: "Carol Díaz", role: "contributor", joined_at: "2026-09-12" },
  ],
  /** A member whose role change the mock refuses with LAST_OWNER. */
  refuseRoleChangeFor: null,
};

function setScenario(role) {
  scenario.role = role;
  scenario.userId = ACTOR[role];
  scenario.members =
    role === "maintainer"
      ? [
          { user_id: ALICE, handle: "alice", display_name: "Alice Guo", role: "owner", joined_at: "2026-09-10" },
          { user_id: BOB, handle: "bob", display_name: "Bob Chen", role: "maintainer", joined_at: "2026-09-11" },
          { user_id: CAROL, handle: "carol", display_name: "Carol Díaz", role: "contributor", joined_at: "2026-09-12" },
        ]
      : [
          { user_id: ALICE, handle: "alice", display_name: "Alice Guo", role: "owner", joined_at: "2026-09-10" },
          { user_id: BOB, handle: "bob", display_name: "Bob Chen", role: "viewer", joined_at: "2026-09-11" },
          { user_id: CAROL, handle: "carol", display_name: "Carol Díaz", role: "contributor", joined_at: "2026-09-12" },
        ];
  scenario.refuseRoleChangeFor = null;
}

/** Every settings write the page issued, in order (the wire assertions). */
const writes = [];

function installApiMock(page) {
  return page.route(`${API}/**`, async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    const pathname = url.pathname;
    const body = route.request().postData();

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      // The page refreshes its CSRF token from the session (lib/auth
      // sessionTokenStorage); every write must echo this exact token.
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          user: { id: scenario.userId, handle: "alice", email: "alice@example.com", display_name: "Alice Guo" },
          csrf_token: "e2e-csrf-token",
        }),
      });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${LAB.id}`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ...LAB,
          purpose: scenario.purpose,
          activity_status: scenario.activityStatus,
        }),
      });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${LAB.id}/membership`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          project_id: LAB.id,
          user_id: scenario.userId,
          role: scenario.role,
          created_at: "2026-09-10T08:00:00Z",
        }),
      });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${LAB.id}/members`) {
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ members: scenario.members }),
      });
    }
    if (method === "PUT" && pathname.startsWith(`/api/v1/projects/${LAB.id}/members/`)) {
      const target = pathname.split("/").pop();
      const csrf = route.request().headers()["x-csrf-token"] ?? null;
      writes.push({ method, path: pathname, body: body === null ? null : JSON.parse(body), csrf });
      if (scenario.refuseRoleChangeFor === target) {
        return route.fulfill({ status: 409, contentType: "application/json", body: JSON.stringify(LAST_OWNER) });
      }
      const role = JSON.parse(body).role;
      const member = scenario.members.find((m) => m.user_id === target);
      member.role = role;
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          project_id: LAB.id,
          user_id: target,
          role,
          created_at: "2026-09-13T08:00:00Z",
        }),
      });
    }
    if (method === "PATCH" && pathname === `/api/v1/projects/${LAB.id}`) {
      const csrf = route.request().headers()["x-csrf-token"] ?? null;
      const patch = JSON.parse(body);
      writes.push({ method, path: pathname, body: patch, csrf });
      if (patch.purpose !== undefined) scenario.purpose = patch.purpose;
      if (patch.activity_status !== undefined) scenario.activityStatus = patch.activity_status;
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          ...LAB,
          purpose: scenario.purpose,
          activity_status: scenario.activityStatus,
        }),
      });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- C. Browser scenarios ---------- */

setScenario("owner");
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

const SETTINGS_URL = `${BASE}/projects/${LAB.id}/settings`;
const memberRow = (id) => page.locator(`[data-member-row="${id}"]`);

/* ---------- 1. Owner opens Settings: members, visibility, details ---------- */

await page.goto(SETTINGS_URL, { waitUntil: "load" });
await page.waitForSelector('[data-project-shell="settings-lab"]');
await page.waitForSelector("[data-settings]");

if ((await page.locator('[data-project-tab="settings"]').count()) !== 1) {
  fail("settings: owner sees the Settings tab");
} else {
  ok("settings: owner sees the Settings tab");
}
const current = await page.getAttribute('[data-project-tab="settings"]', "aria-current");
if (current !== "page") {
  fail("settings: aria-current on the active tab", `got ${current}`);
} else {
  ok("settings: Settings tab carries aria-current=page");
}

await page.waitForSelector('[data-member-row]');
const memberCount = await page.locator("[data-member-row]").count();
if (memberCount !== 3) {
  fail("members: row count", `got ${memberCount}, want 3`);
} else {
  ok("members: the table lists all three members");
}

const aliceIdentity = await memberRow(ALICE).locator(".settings-member-identity").textContent();
if (aliceIdentity === null || !aliceIdentity.includes("Alice Guo") || !aliceIdentity.includes("@alice")) {
  fail("members: identity (name + handle) renders", `got ${aliceIdentity}`);
} else {
  ok("members: identity renders as display name + @handle");
}
const aliceJoined = await memberRow(ALICE).locator(".settings-member-joined").textContent();
if (aliceJoined === null || !aliceJoined.includes("2026-09-10")) {
  fail("members: join date renders", `got ${aliceJoined}`);
} else {
  ok("members: join date renders from joined_at");
}
const aliceStatic = memberRow(ALICE).locator('[data-role-static="owner"]');
if ((await aliceStatic.count()) !== 1 || !((await aliceStatic.textContent()) ?? "").includes("(you)")) {
  fail("members: the actor's own row is locked and marked (you)");
} else {
  ok("members: the actor's own row shows a static role with (you)");
}
const bobSelect = memberRow(BOB).locator("[data-role-select]");
if ((await bobSelect.count()) !== 1 || (await bobSelect.inputValue()) !== "viewer") {
  fail("members: other rows carry a role select with the current role");
} else {
  ok("members: other rows carry a role select pre-set to the member's role");
}

const visibilitySelect = page.locator("[data-visibility-preview]");
if ((await visibilitySelect.count()) !== 1 || (await visibilitySelect.isDisabled()) !== true ||
    (await visibilitySelect.inputValue()) !== "public") {
  fail("visibility: the control renders disabled with the current value");
} else {
  ok("visibility: the control renders the current value, disabled (preview-only)");
}
const visibilityNote = await page.locator("[data-visibility-note]").textContent();
if (visibilityNote === null || !visibilityNote.includes("preview-only")) {
  fail("visibility: the preview-only note explains the disabled control", `got ${visibilityNote}`);
} else {
  ok("visibility: the note explains the preview-only state");
}

// The form seeds from the shell's project via an effect; wait for the
// values rather than racing the first paint.
await page.waitForFunction(() => {
  const el = document.querySelector("[data-purpose-input]");
  return el !== null && el.value === "Exercise the settings surface.";
});
await page.waitForFunction(() => {
  const el = document.querySelector("[data-activity-select]");
  return el !== null && el.value === "planning";
});
const purposeValue = await page.locator("[data-purpose-input]").inputValue();
if (purposeValue !== "Exercise the settings surface.") {
  fail("details: purpose field pre-filled from the project", `got ${purposeValue}`);
} else {
  ok("details: purpose field pre-filled from the project");
}
if ((await page.locator("[data-activity-select]").inputValue()) !== "planning") {
  fail("details: activity status pre-filled from the project");
} else {
  ok("details: activity status pre-filled from the project");
}

const auditNote = await page.locator("[data-audit-note]").textContent();
if (auditNote === null || !/audit log/i.test(auditNote)) {
  fail("audit: the page states owner/maintainer changes are recorded in the audit log", `got ${auditNote}`);
} else {
  ok("audit: the page states owner/maintainer changes land in the audit log (placeholder note)");
}

/* ---------- 2. Role change: PUT wire with CSRF, row updates ---------- */

writes.length = 0;
await bobSelect.selectOption("maintainer");
await page.waitForSelector('[data-settings-notice="success"]');
const put = writes[0];
if (put === undefined || put.method !== "PUT" || put.path !== `/api/v1/projects/${LAB.id}/members/${BOB}` ||
    put.csrf !== "e2e-csrf-token" || JSON.stringify(put.body) !== JSON.stringify({ role: "maintainer" })) {
  fail("role change: PUT wire", `got ${JSON.stringify(put)}`);
} else {
  ok("role change: PUTs {role} to the member route with the session-bound X-CSRF-Token");
}
if ((await bobSelect.inputValue()) !== "maintainer") {
  fail("role change: the row reflects the new role");
} else {
  ok("role change: the row reflects the new role after the write");
}

/* ---------- 3. A refused role change renders the stable error ---------- */

scenario.refuseRoleChangeFor = CAROL;
await memberRow(CAROL).locator("[data-role-select]").selectOption("viewer");
await page.waitForSelector('[data-settings-notice="error"]');
const errorText = await page.locator('[data-settings-notice="error"]').textContent();
if (errorText === null || !/at least one owner/i.test(errorText)) {
  fail("role change: LAST_OWNER maps to the stable message", `got ${errorText}`);
} else {
  ok("role change: LAST_OWNER renders the stable error message");
}
if ((await memberRow(CAROL).locator("[data-role-select]").inputValue()) !== "contributor") {
  fail("role change: the refused row keeps its role");
} else {
  ok("role change: the refused row keeps its role");
}

/* ---------- 4. Purpose/activity edit: PATCH wire + header update ---------- */

writes.length = 0;
await page.fill("[data-purpose-input]", "A sharper research goal.");
await page.selectOption("[data-activity-select]", "active");
await page.click("[data-save-settings]");
await page.waitForSelector('[data-settings-notice="success"]');
const patch = writes.find((w) => w.method === "PATCH");
if (patch === undefined || patch.path !== `/api/v1/projects/${LAB.id}` ||
    patch.csrf !== "e2e-csrf-token" ||
    JSON.stringify(patch.body) !== JSON.stringify({ purpose: "A sharper research goal.", activity_status: "active" })) {
  fail("settings edit: PATCH wire", `got ${JSON.stringify(patch)}`);
} else {
  ok("settings edit: PATCHes exactly the changed fields with the session-bound X-CSRF-Token");
}
const headerPurpose = await page.locator(".project-shell-purpose").textContent();
if (headerPurpose === null || !headerPurpose.includes("A sharper research goal.")) {
  fail("settings edit: the shell header reflects the saved purpose without a reload", `got ${headerPurpose}`);
} else {
  ok("settings edit: the shell header reflects the saved purpose without a reload");
}

/* ---------- 5. viewer 无 settings ---------- */

setScenario("viewer");
const writesBeforeViewer = writes.length;
await page.goto(SETTINGS_URL, { waitUntil: "load" });
await page.waitForSelector("[data-settings-unauthorized]");
if ((await page.locator('[data-project-tab="settings"]').count()) !== 0) {
  fail("viewer: no Settings tab");
} else {
  ok("viewer: the Settings tab is hidden");
}
if ((await page.locator("[data-settings]").count()) !== 0) {
  fail("viewer: no settings chrome on a direct /settings hit");
} else {
  ok("viewer: a direct /settings hit renders the not-authorized answer, never settings chrome");
}
if (writes.length !== writesBeforeViewer) {
  fail("viewer: the not-authorized page issues no settings calls", `+${writes.length - writesBeforeViewer} write(s)`);
} else {
  ok("viewer: the not-authorized page issues no settings calls (UI gate, API stays the boundary)");
}

/* ---------- 6. Maintainer: owner rows locked, owner role unpickable ---------- */

setScenario("maintainer");
await page.goto(SETTINGS_URL, { waitUntil: "load" });
await page.waitForSelector("[data-settings]");
if ((await page.locator('[data-project-tab="settings"]').count()) !== 1) {
  fail("maintainer: sees the Settings tab");
} else {
  ok("maintainer: sees the Settings tab");
}
await page.waitForSelector('[data-member-row]');
if ((await memberRow(ALICE).locator('[data-role-static="owner"]').count()) !== 1 ||
    (await memberRow(ALICE).locator("[data-role-select]").count()) !== 0) {
  fail("maintainer: owner rows are locked (no select)");
} else {
  ok("maintainer: owner rows show a static role, never an editable select");
}
const selfStatic = memberRow(BOB).locator("[data-role-static]");
if ((await selfStatic.count()) !== 1 || !((await selfStatic.textContent()) ?? "").includes("(you)")) {
  fail("maintainer: own row is locked and marked (you)");
} else {
  ok("maintainer: own row is locked and marked (you)");
}
const carolSelect = memberRow(CAROL).locator("[data-role-select]");
const carolOptions = await carolSelect.locator("option").evaluateAll((opts) => opts.map((o) => o.value));
if (carolOptions.includes("owner")) {
  fail("maintainer: the owner role is not pickable", `options ${carolOptions.join(",")}`);
} else {
  ok("maintainer: the role select offers no owner option");
}
writes.length = 0;
await carolSelect.selectOption("viewer");
await page.waitForSelector('[data-settings-notice="success"]');
const maintainerPut = writes.find((w) => w.method === "PUT");
if (maintainerPut === undefined || JSON.stringify(maintainerPut.body) !== JSON.stringify({ role: "viewer" }) ||
    maintainerPut.csrf !== "e2e-csrf-token") {
  fail("maintainer: role change wire", `got ${JSON.stringify(maintainerPut)}`);
} else {
  ok("maintainer: a non-owner role change PUTs with the CSRF token");
}

await browser.close();

if (fails > 0) {
  console.error(`settings-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("settings-e2e: all checks passed");
