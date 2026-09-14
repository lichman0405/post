/**
 * Files read-only e2e (T0308 required test "files read-only e2e"): a real
 * Chromium drives the BUILT web app (next start, production layer)
 * through the read-only Files page. The Go API is mocked at the network
 * layer with the exact wire shapes cmd/api/fileshttp produces —
 * treePayload, filePayload, historyPayload, the raw attachment stream and
 * the raw diff text stream; the Go handler suite (cmd/api/fileshttp) and
 * the FilesReader suite lock those shapes against the real guard, so the
 * two halves cover the real contract without the browser test depending
 * on infra.
 *
 * What is asserted here (the task's acceptance criteria):
 *   - tree/preview/history/download/context sidebar: the tree lists and
 *     descends, a file previews with its content, the history renders per
 *     file with author/date/sha, the download link streams the raw
 *     attachment, the context sidebar carries repository ref/SHA, the
 *     file's git facts and the actions;
 *   - raw diff link: every history entry carries a raw diff link into the
 *     API's text/plain patch stream, opened in a new tab;
 *   - 无 mutation controls (DOM): inside the files page there is no form,
 *     input, textarea, select, contenteditable or draggable element and
 *     no interactive element whose accessible name is an edit/upload/
 *     delete-style action;
 *   - 无 mutation calls (network): every request the page makes to the
 *     files API is a GET — the read-only page never sends anything else;
 *   - 可跳到 Scientific Context: the "View scientific context" action
 *     navigates to the project's Research tab.
 *
 * Usage: node files-e2e.mjs <base-url> [apps-web-dir]
 */
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

/* ---------- A. Fixtures (the exact wire shapes of cmd/api) ---------- */

const API = "http://api.e2e.test";

const PROJECT = {
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
  provision_status: "ready",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-10T08:00:00Z",
};

const OWNER_MEMBERSHIP = {
  project_id: PROJECT.id,
  user_id: "00000000-0000-4000-8000-000000000001",
  role: "owner",
  created_at: "2026-09-10T08:00:00Z",
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

const NOT_FOUND = {
  code: "FILES_NOT_FOUND",
  message: "no such file, directory or ref in the project's repository",
  request_id: "e2e-req",
  retryable: false,
};

const SHA_C1 = "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0";
const SHA_C0 = "f0e1d2c3b4a5968778695a4b3c2d1e0f9a8b7c6d5";

const TREE_ROOT = {
  ref: "main",
  path: "",
  sha: "tree-root-sha-001",
  entries: [
    { name: "README.md", path: "README.md", type: "blob", mode: "100644", size: 34, sha: "blob-readme-1" },
    { name: "docs", path: "docs", type: "tree", mode: "040000", size: 0, sha: "tree-docs-1" },
    { name: "data", path: "data", type: "tree", mode: "040000", size: 0, sha: "tree-data-1" },
    { name: "plot.png", path: "plot.png", type: "blob", mode: "100644", size: 4200, sha: "blob-plot-1" },
  ],
};

const TREE_DOCS = {
  ref: "main",
  path: "docs",
  sha: "tree-docs-1",
  entries: [
    { name: "01.md", path: "docs/01.md", type: "blob", mode: "100644", size: 21, sha: "blob-01-1" },
  ],
};

const FILE_README = {
  name: "README.md",
  path: "README.md",
  type: "blob",
  mode: "100644",
  size: 34,
  sha: "blob-readme-1",
  kind: "text",
  content: "Alloy Lab — reproducible synthesis.\n",
  truncated: false,
};

const FILE_01 = {
  name: "01.md",
  path: "docs/01.md",
  type: "blob",
  mode: "100644",
  size: 21,
  sha: "blob-01-1",
  kind: "text",
  content: "# Synthesis protocol\n",
  truncated: false,
};

const FILE_PLOT = {
  name: "plot.png",
  path: "plot.png",
  type: "blob",
  mode: "100644",
  size: 4200,
  sha: "blob-plot-1",
  kind: "binary",
  content: "",
  truncated: false,
};

const HISTORY_README = {
  ref: "main",
  path: "README.md",
  entries: [
    {
      sha: SHA_C1,
      author: "Alice Guo",
      author_email: "alice@example.com",
      date: "2026-09-14T10:00:00Z",
      message: "Add alloy dataset pointer",
    },
    {
      sha: SHA_C0,
      author: "Alice Guo",
      author_email: "alice@example.com",
      date: "2026-09-10T08:00:00Z",
      message: "Seed repository",
    },
  ],
};

const HISTORY_01 = {
  ref: "main",
  path: "docs/01.md",
  entries: [
    {
      sha: SHA_C0,
      author: "Alice Guo",
      author_email: "alice@example.com",
      date: "2026-09-10T08:00:00Z",
      message: "Seed repository",
    },
  ],
};

const PATCH_C1 = `From ${SHA_C1} Mon Sep 14 10:00:00 2026
From: Alice Guo <alice@example.com>
Subject: [PATCH] Add alloy dataset pointer

---
 README.md | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

diff --git a/README.md b/README.md
index 1111111..2222222 100644
--- a/README.md
+++ b/README.md
@@ -1 +1 @@
-Alloy Lab.
+Alloy Lab — reproducible synthesis.
`;

const RAW_BODY = "Alloy Lab — reproducible synthesis.\n";

/** Mutable scenario state: which membership the API "answers" (owner /
 *  none). Tests flip it and reload. */
const scenario = { membership: "owner" };

/**
 * The recorded API traffic: every request the page sent, so the read-only
 * assertion can inspect the actual methods — not just what the DOM
 * offers.
 */
const apiTraffic = [];
/** Record non-GET API requests to fail the whole run loudly. */
const nonGetRequests = [];

function qs(url) {
  return Object.fromEntries(new URL(url).searchParams.entries());
}

function installApiMock(page) {
  // Context-level routing, not page-level: the raw-diff link opens a
  // popup, and only a context route intercepts that popup's own request
  // (a page-level route would let it escape to real DNS and fail).
  return page.context().route(`${API}/**`, async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    const method = request.method();
    const pathname = url.pathname;
    apiTraffic.push({ method, pathname, query: qs(request.url()) });
    if (method !== "GET") {
      nonGetRequests.push(`${method} ${request.url()}`);
    }

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      return route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify(UNAUTHENTICATED) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}`) {
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(PROJECT) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/membership`) {
      if (scenario.membership === "owner") {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(OWNER_MEMBERSHIP) });
      }
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(MEMBERSHIP_NOT_FOUND) });
    }

    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/files/tree`) {
      const { path: p } = qs(request.url());
      if (p === "docs") {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TREE_DOCS) });
      }
      if (p === "" || p === undefined) {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TREE_ROOT) });
      }
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/files/content`) {
      const { path: p } = qs(request.url());
      const file = { "README.md": FILE_README, "docs/01.md": FILE_01, "plot.png": FILE_PLOT }[p];
      if (!file) {
        return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
      }
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(file) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/files/history`) {
      const { path: p } = qs(request.url());
      const history = { "README.md": HISTORY_README, "docs/01.md": HISTORY_01 }[p];
      if (!history) {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ ref: "main", path: p, entries: [] }) });
      }
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(history) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/files/raw`) {
      const { path: p } = qs(request.url());
      if (p === "README.md") {
        return route.fulfill({
          status: 200,
          contentType: "application/octet-stream",
          headers: {
            "Content-Disposition": 'attachment; filename="README.md"',
            "X-Content-Type-Options": "nosniff",
          },
          body: RAW_BODY,
        });
      }
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    if (method === "GET" && pathname === `/api/v1/projects/${PROJECT.id}/files/diff`) {
      const { sha } = qs(request.url());
      if (sha === SHA_C1) {
        return route.fulfill({
          status: 200,
          contentType: "text/plain; charset=utf-8",
          headers: { "X-Content-Type-Options": "nosniff" },
          body: PATCH_C1,
        });
      }
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- B. Mutation-control assertions ---------- */

// Interactive elements whose accessible name is an edit/upload/delete-
// style action have no place on the read-only Files page. ("download" and
// "view" survive on purpose — they are reads.)
const MUTATION_TEXT =
  /\b(edit|upload|delete|remove|replace|rename|create|new file|add file|commit|push|publish|write|drop)\b/i;

async function assertNoMutationControls(page, scope) {
  // Structural: no mutation-capable element may exist at all.
  const structural = await page.locator(
    `${scope} form, ${scope} input, ${scope} textarea, ${scope} select, ${scope} [contenteditable], ${scope} [draggable]`,
  ).count();
  if (structural !== 0) {
    fail("read-only DOM: no mutation-capable elements",
      `found ${structural} (form/input/textarea/select/contenteditable/draggable)`);
  } else {
    ok("read-only DOM: no form, input, textarea, select, contenteditable or draggable element");
  }

  // Semantic: no interactive element whose name is a mutation action.
  const interactive = page.locator(`${scope} button, ${scope} a, ${scope} [role="button"]`);
  const names = await interactive.evaluateAll((els) =>
    els.map((el) => el.getAttribute("aria-label") ?? el.textContent ?? "").join("\n"),
  );
  const offenders = names
    .split("\n")
    .map((n) => n.trim())
    .filter((n) => n !== "" && MUTATION_TEXT.test(n));
  if (offenders.length > 0) {
    fail("read-only DOM: no mutation-action names", `found: ${offenders.join(" | ")}`);
  } else {
    ok("read-only DOM: no interactive element named as a mutation action");
  }

  // Network: the page may only ever send GETs to the files API.
  if (nonGetRequests.length > 0) {
    fail("read-only network: every files API call is a GET", `found: ${nonGetRequests.join(" | ")}`);
  } else {
    ok("read-only network: every files API call is a GET");
  }
}

/* ---------- C. Browser scenarios ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 900 }, acceptDownloads: true });
const page = await context.newPage();
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

const FILES = `${BASE}/projects/${PROJECT.id}/files`;

/* ---------- 1. Tree renders at the root of main ---------- */

await page.goto(FILES, { waitUntil: "load" });
await page.waitForSelector('[data-project-shell="alloy-lab"]');
await page.waitForSelector("[data-files-page]");
ok("files page renders inside the project shell");

await page.waitForSelector('[data-files-entry="README.md"]');
const entryNames = await page.locator("[data-files-entry] .files-entry-name").allTextContents();
for (const want of ["README.md", "docs", "data", "plot.png"]) {
  if (!entryNames.includes(want)) {
    fail("tree: root lists the repository entries", `missing ${want} (got ${entryNames.join(", ")})`);
  }
}
ok("tree: root lists the repository entries");

const docType = await page.getAttribute('[data-files-entry="docs"]', "data-entry-type");
const fileType = await page.getAttribute('[data-files-entry="README.md"]', "data-entry-type");
if (docType !== "tree" || fileType !== "blob") {
  fail("tree: entries carry their git type", `docs=${docType}, README.md=${fileType}`);
} else {
  ok("tree: entries carry their git type (tree vs blob)");
}

if ((await page.locator('[data-files-crumb="root"][aria-current="page"]').count()) !== 1) {
  fail("tree: root crumb is the current page");
} else {
  ok("tree: root crumb is the current page");
}

/* ---------- 2. Tree descends into a directory ---------- */

await page.click('[data-files-entry="docs"]');
await page.waitForSelector('[data-files-entry="01.md"]');
if ((await page.locator('[data-files-crumb="docs"][aria-current="page"]').count()) !== 1) {
  fail("tree: descending marks the docs crumb current");
} else {
  ok("tree: descending into docs lists its entries and marks the crumb current");
}

/* ---------- 3. Preview + history + context sidebar ---------- */

await page.click('[data-files-entry="01.md"]');
await page.waitForSelector('[data-files-preview-name="01.md"]');
const preview = await page.locator("[data-files-content]").textContent();
if (preview !== "# Synthesis protocol\n") {
  fail("preview: file content renders verbatim", `got ${JSON.stringify(preview)}`);
} else {
  ok("preview: file content renders verbatim");
}

await page.waitForSelector("[data-files-history-entry]");
const historyCount = await page.locator("[data-files-history-entry]").count();
if (historyCount !== 1) {
  fail("history: one commit for docs/01.md", `got ${historyCount}`);
} else {
  ok("history: the per-file commit list renders");
}
const historyMeta = await page.locator("[data-files-history-entry]").first().textContent();
if (!historyMeta.includes("Seed repository") || !historyMeta.includes("Alice Guo") || !historyMeta.includes(SHA_C0.slice(0, 7))) {
  fail("history: message, author and short sha render", `got ${historyMeta}`);
} else {
  ok("history: message, author and short sha render");
}

const facts = await page.locator("[data-files-facts]").textContent();
for (const want of ["docs/01.md", "blob", "100644", "21 B"]) {
  if (!facts.includes(want)) {
    fail("context sidebar: file facts render", `missing ${want} (got ${facts})`);
  }
}
ok("context sidebar: path, type, mode and size render");

const contextText = await page.locator("[data-files-context]").textContent();
if (!contextText.includes("main") || !contextText.includes("tree-docs-1")) {
  fail("context sidebar: repository section shows the branch and tree SHA", `got ${contextText}`);
} else {
  ok("context sidebar: repository section shows the branch and tree SHA");
}

/* ---------- 4. Download streams the raw attachment ---------- */

const downloadHref = await page.getAttribute("[data-files-download]", "href");
if (downloadHref !== `${API}/api/v1/projects/${PROJECT.id}/files/raw?ref=main&path=docs%2F01.md`) {
  fail("download: link points at the raw endpoint", `got ${downloadHref}`);
} else {
  ok("download: link points at the raw endpoint");
}

await page.click('[data-files-crumb="root"]');
await page.waitForSelector('[data-files-entry="README.md"]');
await page.click('[data-files-entry="README.md"]');
await page.waitForSelector('[data-files-preview-name="README.md"]');
const [download] = await Promise.all([
  page.waitForEvent("download"),
  page.click("[data-files-download]"),
]);
if (download.suggestedFilename() !== "README.md") {
  fail("download: attachment filename", `got ${download.suggestedFilename()}`);
} else {
  ok("download: the raw stream arrives as an attachment with the filename");
}
const downloadStream = await download.createReadStream();
const chunks = [];
for await (const chunk of downloadStream) chunks.push(chunk);
if (Buffer.concat(chunks).toString("utf8") !== RAW_BODY) {
  fail("download: attachment body is the file bytes");
} else {
  ok("download: attachment body is the file bytes");
}

/* ---------- 5. Raw diff link opens the patch stream ---------- */

await page.waitForSelector("[data-files-history-entry]");
const diffHref = await page.locator("[data-files-diff]").first().getAttribute("href");
if (diffHref !== `${API}/api/v1/projects/${PROJECT.id}/files/diff?sha=${SHA_C1}`) {
  fail("raw diff: link points at the diff endpoint", `got ${diffHref}`);
} else {
  ok("raw diff: history entry links the commit patch endpoint");
}
const [popup] = await Promise.all([
  page.waitForEvent("popup"),
  page.locator("[data-files-diff]").first().click(),
]);
await popup.waitForLoadState("load");
const patchText = await popup.textContent("pre");
if (!patchText.includes("diff --git a/README.md b/README.md")) {
  fail("raw diff: the patch stream renders in the new tab", `got ${patchText}`);
} else {
  ok("raw diff: the patch stream renders in the new tab");
}
await popup.close();

/* ---------- 6. Binary files preview as a pointer, never decoded ---------- */

await page.click('[data-files-crumb="root"]');
await page.waitForSelector('[data-files-entry="plot.png"]');
await page.click('[data-files-entry="plot.png"]');
await page.waitForSelector('[data-files-preview-name="plot.png"]');
if ((await page.locator('[data-files-kind="binary"]').count()) !== 1) {
  fail("preview: binary files carry the binary badge");
} else {
  ok("preview: binary files carry the binary badge");
}
if ((await page.locator("[data-files-content]").count()) !== 0) {
  fail("preview: binary content is never decoded into the page");
} else {
  ok("preview: binary content is never decoded into the page");
}

/* ---------- 7. Jump to Scientific Context ---------- */

await page.click('[data-files-entry="README.md"]');
await page.waitForSelector('[data-files-scientific-context]');
const contextHref = await page.getAttribute("[data-files-scientific-context]", "href");
if (contextHref !== `/projects/${PROJECT.id}/research`) {
  fail("scientific context: action links the Research tab", `got ${contextHref}`);
} else {
  ok("scientific context: action links the Research tab");
}
await page.click("[data-files-scientific-context]");
await page.waitForURL(`**/projects/${PROJECT.id}/research`);
const researchCurrent = await page.getAttribute('[data-project-tab="research"]', "aria-current");
if (researchCurrent !== "page") {
  fail("scientific context: the Research tab becomes current", `got ${researchCurrent}`);
} else {
  ok("scientific context: the jump lands on the Research tab with aria-current=page");
}

/* ---------- 8. Read-only assertions across everything seen so far ---------- */

await page.goto(FILES, { waitUntil: "load" });
await page.waitForSelector("[data-files-page]");
await page.click('[data-files-entry="README.md"]');
await page.waitForSelector("[data-files-preview-name]");
await assertNoMutationControls(page, "[data-files-page]");

const filesCalls = apiTraffic.filter((c) => c.pathname.includes("/files/"));
if (filesCalls.length === 0) {
  fail("read-only network: the page really exercised the files API");
} else {
  ok(`read-only network: the page exercised the files API with ${filesCalls.length} GET call(s)`);
}

/* ---------- 9. Non-member of a public project still browses ---------- */

scenario.membership = "none";
await page.reload({ waitUntil: "load" });
await page.waitForSelector("[data-files-page]");
await page.waitForSelector('[data-files-entry="README.md"]');
if ((await page.locator('[data-project-tab="settings"]').count()) !== 0) {
  fail("non-member: shell hides the Settings tab (API answered no role)");
} else {
  ok("non-member: files still browse while the shell hides the Settings tab");
}

await browser.close();

if (fails > 0) {
  console.error(`files-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("files-e2e: all checks passed");
