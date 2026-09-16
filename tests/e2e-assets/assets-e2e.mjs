/**
 * Asset hub e2e (T0709 required test "asset ui e2e"): a real Chromium
 * drives the BUILT web app (next start, production layer) through /assets
 * and one asset's page. The Go API is mocked at the network layer with the
 * exact wire shapes cmd/api/assetshttp + internal/assets.BuildPage produce
 * — the two JSON documents are copied from the Go structs' tags
 * (internal/assets/page.go AssetPage/PageAsset/…, browse.go BrowseList) —
 * and those shapes are locked against the real guard and real PostgreSQL
 * by tests/e2e/asset_page_e2e_test.go and
 * tests/integration/asset_page_test.go.
 *
 * What is asserted here, and why each one is here:
 *
 *   - the ELEVEN items of docs/42 §Asset Page, one by one, by name:
 *     PID/version, type, origin, rights, creators, metadata, dependencies,
 *     lineage, used/derived public links, versions, network events. The
 *     checklist below fails and NAMES the item that is missing, so a page
 *     that quietly lost one item cannot pass by rendering the other ten;
 *   - the browse list filters by the closed four-type set, and the filter
 *     the page sends is the API's own value;
 *   - the private project that USES the public asset (and whose name,
 *     slug and id the platform must not disclose) appears NOWHERE in the
 *     DOM — the leak issue #238 was about, checked at the layer a reader
 *     actually looks at;
 *   - the page renders the MEMBER's answer too, unchanged: the same URL
 *     with the server's member payload shows the private version. That is
 *     the proof the page itself filters nothing — visible/not-visible is
 *     the API's decision, not a front-end omission (docs/23 §3);
 *   - one not-found state for the API's one ASSET_NOT_FOUND code: an
 *     unknown pid, a version a non-member may not see and a private
 *     version all render the SAME sentence, and that sentence does not
 *     tell them apart;
 *   - no blob download control, anywhere: no link, no anchor with a
 *     download attribute, no URL into a blob/file/raw/download path. The
 *     blob route's non-existence is proven in the Go e2e (route
 *     enumeration + a 404 probe); this check proves the page offers no
 *     reader-facing way in either — which is what the acceptance criterion
 *     asks a UI test for.
 *
 * Usage: node assets-e2e.mjs <base-url> [apps-web-dir]
 */
import { chromium } from "playwright";

const BASE = process.argv[2] ?? "http://127.0.0.1:31121";

let fails = 0;
const ok = (label) => console.log(`ok   ${label}`);
const fail = (label, detail) => {
  fails += 1;
  console.log(`FAIL ${label}${detail ? `: ${detail}` : ""}`);
};
const check = (label, condition, detail) => {
  if (condition) ok(label);
  else fail(label, detail);
};

/* ---------- A. Fixtures (the exact wire shapes of cmd/api) ---------- */

const API = "http://api.e2e.test";

/** The PUBLIC project: the asset's origin, and the only project the page may name. */
const OPEN_LAB = {
  id: "11111111-1111-4111-8111-111111111111",
  name: "Open Materials Lab",
  slug: "open-materials",
  visibility: "public",
};

/** The project that USES the asset privately. Its identity must not be reachable. */
const HIDDEN_LAB = {
  id: "99999999-9999-4999-8999-999999999999",
  name: "Hidden Usage Lab",
  slug: "hidden-usage-lab",
};

/** The public project that uses the asset publicly: rendered, and it may be. */
const CATALYSIS = {
  id: "33333333-3333-4333-8333-333333333333",
  name: "Catalysis Group",
  slug: "catalysis-group",
  visibility: "public",
};

const ASSET = {
  pid: "01j9z6k3m4n5p6q7r8s9t0v1w01",
  type: "material_collection",
  title: "MOF-5 Synthesis Collection",
  slug: "mof5-synthesis",
  created_at: "2026-08-20T09:00:00Z",
};

const ALICE = {
  user_id: "00000000-0000-4000-8000-000000000001",
  handle: "alice",
  display_name: "Alice Guo",
};

/** The version a non-member must not learn exists. */
const HIDDEN_VERSION = "3.0";

const V10 = {
  version: "1.0",
  url: `/assets/${ASSET.pid}/1.0`,
  visibility: "public",
  integrity_hash: "sha256:1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f",
  published_at: "2026-09-01T10:00:00Z",
  current: false,
};
const V20 = {
  version: "2.0",
  url: `/assets/${ASSET.pid}/2.0`,
  visibility: "public",
  integrity_hash: "sha256:2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f",
  published_at: "2026-09-10T10:00:00Z",
  current: true,
};
const V30 = {
  version: HIDDEN_VERSION,
  url: `/assets/${ASSET.pid}/${HIDDEN_VERSION}`,
  visibility: "private",
  integrity_hash: "sha256:3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f",
  published_at: "2026-09-12T10:00:00Z",
  current: true,
};

const RIGHTS = {
  version: 1,
  standard_license_id: "CC-BY-4.0",
  custom_agreement_ref: null,
  usage: {
    commercial_use: "allowed",
    derivatives: "allowed_with_attribution",
    redistribution: "allowed",
    model_training: "unspecified",
    attribution: "required",
    patent_grant: "not_granted",
  },
  // The two axes of CLAUDE.md §9.7, declared separately: the RECORD is
  // public, and the BYTES are not open.
  visibility: { metadata: "project_policy", data_access: "restricted" },
  notes: "Synthesis parameters are published; precursor batches are not.",
};

const MANIFEST_METADATA = [
  { key: "blob_ids", value: ["sha256:9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a"] },
  { key: "cell_chemistry", value: "Zn4O(BDC)3" },
  { key: "sample_count", value: 128 },
];

const ORIGIN = [
  {
    ref: `project:${OPEN_LAB.id}`,
    kind: "project",
    resolved: true,
    title: "Open Materials Lab",
    project_id: OPEN_LAB.id,
    link: `/projects/${OPEN_LAB.id}`,
  },
];

const DEPENDENCIES = [
  {
    pin: "01j9z6k3m4n5p6q7r8s9t0v1w02@1.4",
    resolved: true,
    public: true,
    title: "BDC Linker Protocol",
    type: "protocol",
    url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w02/1.4",
  },
  // Resolved=false is a fact about the PUBLISHER's document (the pin names
  // a version this repository has no row for), not a withheld row: its own
  // bytes name an asset the platform cannot answer for.
  { pin: "01j9z6k3m4n5p6q7r8s9t0v1w03@0.1", resolved: false, public: false },
];

const LINEAGE = [
  {
    relation: "derived_from",
    direction: "parent",
    pid: "01j9z6k3m4n5p6q7r8s9t0v1w04",
    version: "1.0",
    url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w04/1.0",
    title: "MOF-5 Reference Dataset",
  },
];

const USED_BY = [
  {
    project_id: CATALYSIS.id,
    project_name: CATALYSIS.name,
    project_slug: CATALYSIS.slug,
    dependency_type: "derived_from",
    created_at: "2026-09-11T08:30:00Z",
  },
];

const EVENT = {
  type: "research_asset.version_published",
  occurred_at: "2026-09-10T10:00:00Z",
  version: "2.0",
  actor: ALICE,
};

/**
 * The page payload. `viewer` picks the SERVER's answer, exactly as the
 * real one would: a member of the asset's own project sees the private
 * version, everyone else does not. `want` pins a version to render.
 */
function pagePayload(viewer, want) {
  const member = viewer === "member";
  const all = member ? [V30, V20, V10] : [V20, V10];
  const rendered = want ? all.find((v) => v.version === want) ?? all[0] : all[0];
  return {
    asset: {
      pid: ASSET.pid,
      type: ASSET.type,
      title: ASSET.title,
      slug: ASSET.slug,
      origin_project: OPEN_LAB,
      created_at: ASSET.created_at,
    },
    version: {
      version: rendered.version,
      url: rendered.url,
      visibility: rendered.visibility,
      integrity_hash: rendered.integrity_hash,
      published_at: rendered.published_at,
      published_by: ALICE,
    },
    origin: ORIGIN,
    rights: RIGHTS,
    creators: [{ ...ALICE, role: "publisher" }],
    metadata: MANIFEST_METADATA,
    dependencies: DEPENDENCIES,
    lineage: LINEAGE,
    used_by: USED_BY,
    versions: all.map((v) => ({ ...v, current: v.version === rendered.version })),
    events: member ? [EVENT] : [EVENT],
  };
}

/** One browse row, shaped by internal/assets.BuildBrowse. */
function browseRow({ pid, type, title, slug, project, publicVersions, latestVersion, latestPublishedAt }) {
  return {
    pid,
    type,
    title,
    slug,
    url: `/assets/${pid}`,
    origin_project: project,
    public_versions: publicVersions,
    latest_version: latestVersion,
    latest_url: `/assets/${pid}/${latestVersion}`,
    latest_published_at: latestPublishedAt,
  };
}

const BROWSE_ALL = {
  type: null,
  types: ["dataset", "protocol", "material_collection", "benchmark"],
  assets: [
    browseRow({
      pid: ASSET.pid,
      type: "material_collection",
      title: ASSET.title,
      slug: ASSET.slug,
      project: OPEN_LAB,
      publicVersions: 2,
      latestVersion: "2.0",
      latestPublishedAt: "2026-09-10T10:00:00Z",
    }),
    // The asset of a PRIVATE project: it has a public version, so it is in
    // the network's list — and its project is null, never a blank object.
    browseRow({
      pid: "01j9z6k3m4n5p6q7r8s9t0v1w07",
      type: "dataset",
      title: "Screening Run 7",
      slug: "screening-run-7",
      project: null,
      publicVersions: 1,
      latestVersion: "0.4",
      latestPublishedAt: "2026-09-08T10:00:00Z",
    }),
  ],
};

const BROWSE_DATASETS = {
  type: "dataset",
  types: BROWSE_ALL.types,
  assets: BROWSE_ALL.assets.filter((a) => a.type === "dataset"),
};

const NOT_FOUND = {
  code: "ASSET_NOT_FOUND",
  message: "asset not found",
  request_id: "e2e-req",
  retryable: false,
};

const UNAUTHENTICATED = {
  code: "UNAUTHENTICATED",
  message: "authentication required",
  request_id: "e2e-req",
  retryable: false,
};

const MEMBER = { id: ALICE.user_id, handle: ALICE.handle, email: "alice@example.com", display_name: ALICE.display_name };

/** Mutable scenario state; the tests flip it and reload. */
const scenario = {
  viewer: "anonymous", // "anonymous" | "member"
};

/* ---------- B. The API mock ---------- */

const apiTraffic = [];

function installApiMock(page) {
  return page.route(`${API}/**`, async (route) => {
    const url = new URL(route.request().url());
    const method = route.request().method();
    const pathname = url.pathname;
    apiTraffic.push(`${method} ${url.pathname}${url.search}`);

    if (method === "GET" && pathname === "/api/v1/auth/session") {
      if (scenario.viewer === "member") {
        return route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({ user: MEMBER, csrf_token: "e2e-csrf" }),
        });
      }
      return route.fulfill({ status: 401, contentType: "application/json", body: JSON.stringify(UNAUTHENTICATED) });
    }
    if (method === "GET" && pathname === "/api/v1/assets") {
      const type = url.searchParams.get("type");
      return route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(type === "dataset" ? BROWSE_DATASETS : BROWSE_ALL),
      });
    }
    if (method === "GET" && pathname === `/api/v1/assets/${ASSET.pid}`) {
      const want = url.searchParams.get("version");
      if (want === null) {
        return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(pagePayload(scenario.viewer, null)) });
      }
      if (scenario.viewer !== "member" && want === HIDDEN_VERSION) {
        // The private version, asked for by name: the API's one 404.
        return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
      }
      const known = scenario.viewer === "member" ? [V10, V20, V30] : [V10, V20];
      if (!known.some((v) => v.version === want)) {
        return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
      }
      return route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(pagePayload(scenario.viewer, want)) });
    }
    // Every other asset path — an unknown pid, a slug-shaped segment — is
    // the same 404 the real surface answers.
    if (method === "GET" && pathname.startsWith("/api/v1/assets/")) {
      return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
    }
    return route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify(NOT_FOUND) });
  });
}

/* ---------- C. The eleven items of docs/42 §Asset Page ---------- */

/**
 * docs/42 §Page Specs line 19, verbatim:
 *   PID/version、type、origin、rights、creators、metadata、dependencies、
 *   lineage、used/derived public links、versions、network events
 * Eleven items. The page marks each with data-asset-block; this list is
 * the checklist, in the spec's own order.
 */
const ELEVEN_ITEMS = [
  ["PID/version", ["asset", "version"]],
  ["type", ["asset"]],
  ["origin", ["origin"]],
  ["rights", ["rights"]],
  ["creators", ["creators"]],
  ["metadata", ["metadata"]],
  ["dependencies", ["dependencies"]],
  ["lineage", ["lineage"]],
  ["used/derived public links", ["used_by"]],
  ["versions", ["versions"]],
  ["network events", ["events"]],
];

async function assertElevenItems(page, where) {
  const blocks = await page.evaluate(() =>
    [...document.querySelectorAll("[data-asset-block]")]
      .flatMap((el) => el.getAttribute("data-asset-block").split(/\s+/))
      .filter((name) => name !== ""),
  );
  const missing = [];
  for (const [item, tokens] of ELEVEN_ITEMS) {
    if (!tokens.every((token) => blocks.includes(token))) missing.push(`${item} (${tokens.join("+")})`);
  }
  if (missing.length > 0) {
    fail(`eleven items: all of docs/42 §Asset Page renders${where ? ` (${where})` : ""}`, `missing: ${missing.join(", ")}`);
    return;
  }
  ok(`eleven items: all 11 of docs/42 §Asset Page render${where ? ` (${where})` : ""} — ${ELEVEN_ITEMS.map(([item]) => item).join(", ")}`);
}

/* ---------- D. Browser scenarios ---------- */

const browser = await chromium.launch();
const context = await browser.newContext({ viewport: { width: 1280, height: 1000 }, acceptDownloads: true });
const page = await context.newPage();
// Hydration crashes must fail the run loudly, not silently leave the page
// server-rendered and inert.
page.on("pageerror", (error) => fail("uncaught page error", String(error).slice(0, 200)));
await installApiMock(page);

/* ---------- 1. The hub lists, and the filter is the four-type set ---------- */

await page.goto(`${BASE}/assets`, { waitUntil: "load" });
await page.waitForSelector("[data-asset-count]");
check("hub: the browse list renders", (await page.locator("[data-asset-pid]").count()) === 2, `rows=${await page.locator("[data-asset-pid]").count()}`);

const filterValues = await page.locator("[data-asset-filter]").evaluateAll((els) =>
  els.map((el) => el.getAttribute("data-asset-filter")),
);
check(
  "hub: the filter offers exactly the closed four-type set (plus all types)",
  JSON.stringify([...filterValues].sort()) ===
    JSON.stringify(["all", "benchmark", "dataset", "material_collection", "protocol"]),
  `got ${filterValues.join(", ")}`,
);

const hubText = await page.locator(".assets-browse").textContent();
check("hub: a row names a public project", hubText.includes(OPEN_LAB.name), "the public project is not rendered");
check("hub: the public version count renders", hubText.includes("2 public versions"), hubText);

// The private project that uses the public asset must be nowhere on the hub.
check(
  "hub: the private usage project is not disclosed",
  !hubText.includes(HIDDEN_LAB.name) && !hubText.includes(HIDDEN_LAB.slug) && !hubText.includes(HIDDEN_LAB.id),
  "the private project's identity reached the hub",
);
// Its asset IS listed (it has a public version) and carries NO project.
const bareRow = page.locator('[data-asset-pid="01j9z6k3m4n5p6q7r8s9t0v1w07"]');
check("hub: an asset of a private project is listed without a project", (await bareRow.locator("[data-asset-project]").count()) === 0);

/* ---------- 2. The filter asks the API for the type ---------- */

apiTraffic.length = 0;
await page.click('[data-asset-filter="dataset"]');
await page.waitForSelector('[data-asset-count="1"]');
check(
  "hub: the filter asks the API for the type it offers",
  apiTraffic.includes("GET /api/v1/assets?type=dataset"),
  `requests: ${apiTraffic.join(" | ")}`,
);
check("hub: the filtered list renders only the type asked for", (await page.locator("[data-asset-pid]").count()) === 1);

await page.click('[data-asset-filter="all"]');
await page.waitForSelector('[data-asset-count="2"]');

/* ---------- 3. The asset page, anonymously, item by item ---------- */

await page.click(`[data-asset-pid="${ASSET.pid}"] [data-asset-title]`);
await page.waitForSelector('[data-asset-page="ready"]');
await assertElevenItems(page, "anonymous");

check(
  "asset page: the pid and the version are rendered separately",
  (await page.locator("[data-asset-pid-code]").textContent()) === ASSET.pid &&
    (await page.locator("[data-asset-version]").getAttribute("data-asset-version")) === "2.0",
  `pid=${await page.locator("[data-asset-pid-code]").textContent()} version=${await page.locator("[data-asset-version]").getAttribute("data-asset-version")}`,
);
check(
  "asset page: type renders as one of the closed set",
  (await page.getAttribute("[data-asset-type]", "data-asset-type")) === "material_collection",
);
check(
  "asset page: the content hash of the rendered version renders",
  (await page.locator("[data-asset-hash]").textContent()) === V20.integrity_hash,
);

const pageText = await page.locator(".asset-page").textContent();
check("asset page: origin renders the resolved project ref", pageText.includes(`project:${OPEN_LAB.id}`));
check("asset page: rights renders the publisher's declaration", pageText.includes("CC-BY-4.0"));
check(
  "asset page: rights keeps the two axes apart (record visibility vs data access)",
  pageText.includes("Metadata visibility") &&
    pageText.includes("Project policy") &&
    pageText.includes("Data access") &&
    pageText.includes("Restricted"),
  pageText.slice(pageText.indexOf("Rights"), pageText.indexOf("Rights") + 400),
);
check("asset page: creators names the credited party and its role", pageText.includes(ALICE.display_name) && pageText.includes("publisher"));
check("asset page: metadata renders each declared key", pageText.includes("cell_chemistry") && pageText.includes("Zn4O(BDC)3"));
check(
  "asset page: dependencies render the resolved pin with its title",
  pageText.includes("BDC Linker Protocol") && pageText.includes("01j9z6k3m4n5p6q7r8s9t0v1w02@1.4"),
);
check(
  "asset page: an unresolvable pin is rendered as its own bytes, not as hidden",
  pageText.includes("01j9z6k3m4n5p6q7r8s9t0v1w03@0.1") && pageText.includes("not in this repository"),
);
check(
  "asset page: lineage renders the derived_from edge and its counterpart",
  pageText.includes("derived_from") && pageText.includes("MOF-5 Reference Dataset"),
);
check(
  "asset page: used/derived public links render the public usage only",
  pageText.includes(CATALYSIS.name) && pageText.includes("derived_from"),
);
check("asset page: versions render every version a non-member may see", pageText.includes("1.0") && pageText.includes("2.0"));
check("asset page: network events render the published event", pageText.includes("research_asset.version_published"));

/* ---------- 4. Two versions are distinguishable, and neither is the pid ---------- */

const versionRows = await page.locator("[data-asset-version-row]").evaluateAll((els) =>
  els.map((el) => ({
    version: el.getAttribute("data-asset-version-row"),
    visibility: el.getAttribute("data-asset-version-visibility"),
    current: el.getAttribute("aria-current"),
    text: el.textContent,
  })),
);
check("versions: one row per visible version, newest first", versionRows.length === 2, JSON.stringify(versionRows.map((r) => r.version)));
check(
  "versions: the rendered version is marked, the other is not",
  versionRows[0].current === "true" && versionRows[1].current === null,
  JSON.stringify(versionRows.map((r) => [r.version, r.current])),
);
// The hashes are shown shortened (first 12 hex chars of the body); the two
// versions' bodies are deliberately distinct, so a page that rendered the
// same hash twice — or the pid where the hash belongs — is caught here.
const hashHeads = [V20.integrity_hash, V10.integrity_hash].map((h) => h.slice("sha256:".length, "sha256:".length + 12));
check(
  "versions: each version shows its own content hash, not one shared hash",
  versionRows[0].text.includes(hashHeads[0]) && versionRows[1].text.includes(hashHeads[1]) && hashHeads[0] !== hashHeads[1],
  `want ${hashHeads.join(" / ")} in ${JSON.stringify(versionRows.map((r) => r.text))}`,
);
check(
  "versions: the version label is never presented as the pid",
  versionRows.every((r) => !r.text.includes(ASSET.pid)),
  JSON.stringify(versionRows.map((r) => r.text)),
);

/* ---------- 5. A named version is a persistent address ---------- */

await page.click('[data-asset-version-row="1.0"] a');
await page.waitForSelector("[data-asset-version=\"1.0\"]");
check(
  "version address: /assets/{pid}/{version} renders that version",
  (await page.locator("[data-asset-pid-code]").textContent()) === ASSET.pid &&
    (await page.getAttribute("[data-asset-version]", "data-asset-version")) === "1.0",
);
check(
  "version address: the version asked for is the one marked current",
  (await page.getAttribute('[data-asset-version-row="1.0"]', "aria-current")) === "true" &&
    (await page.getAttribute('[data-asset-version-row="2.0"]', "aria-current")) === null,
);

/* ---------- 6. One 404 for everything a non-member may not see ---------- */

const notFoundTexts = [];
for (const [label, target] of [
  ["an unknown pid", `${BASE}/assets/01j9z6k3m4n5p6q7r8s9t0v1w99`],
  ["the private version, asked for by name", `${BASE}/assets/${ASSET.pid}/${HIDDEN_VERSION}`],
]) {
  await page.goto(target, { waitUntil: "load" });
  await page.waitForSelector('[data-asset-page="error"]');
  const status = await page.getAttribute('[data-asset-page="error"]', "data-asset-status");
  const text = (await page.locator(".asset-notice").textContent()).trim();
  notFoundTexts.push(text);
  check(`not found: ${label} renders the page's one not-found state`, status === "404", `status=${status}`);
}
check(
  "not found: the two states are indistinguishable (same sentence, no reason)",
  notFoundTexts[0] === notFoundTexts[1] && !/private|forbidden|deleted|exists/i.test(notFoundTexts[0]),
  JSON.stringify(notFoundTexts),
);

/* ---------- 7. No blob download control, anywhere ---------- */

async function assertNoBlobDownload(where) {
  const offenders = await page.evaluate(() => {
    const out = [];
    for (const el of document.querySelectorAll("[data-asset-page] a, [data-asset-page] button, [data-asset-page] form, [data-asset-page] input")) {
      const href = el.getAttribute("href") ?? "";
      const text = (el.getAttribute("aria-label") ?? el.textContent ?? "").trim();
      const blobish = /(\/blob|\/blobs|\/files\/raw|\/download|^blob:|\.zip$)/i.test(href);
      const downloadish = el.hasAttribute("download") || /\b(download|export|fetch the bytes|save)\b/i.test(text);
      if (blobish || downloadish) out.push(`${el.tagName} href="${href}" name="${text.slice(0, 60)}"`);
    }
    return out;
  });
  check(`blobs: no download control on ${where}`, offenders.length === 0, offenders.join(" | "));
}
await assertNoBlobDownload("the asset page");

// The blob ids the publisher declared are metadata, and stay there: they
// render as the strings they are, never as a URL.
const blobIdLink = await page.locator(`[data-asset-page] a[href*="sha256"]`).count();
check("blobs: a declared blob id is never turned into a link", blobIdLink === 0, `${blobIdLink} link(s) built from a blob id`);

/* ---------- 8. The private project is not disclosed on the asset page ---------- */

// Back to the anonymous page: sections 5 and 6 left the reader on a
// version address and on the not-found state.
await page.goto(`${BASE}/assets/${ASSET.pid}`, { waitUntil: "load" });
await page.waitForSelector('[data-asset-page="ready"]');
const pageHtml = await page.content();
for (const [label, secret] of [
  ["name", HIDDEN_LAB.name],
  ["slug", HIDDEN_LAB.slug],
  ["id", HIDDEN_LAB.id],
]) {
  check(`private usage: the private project's ${label} is absent from the asset page`, !pageHtml.includes(secret));
}
check(
  "private usage: no count of withheld usages is rendered",
  !/\b\d+\s+(more|private|hidden|other)\b/i.test(pageText) && !pageText.includes("used 2 times"),

);

/* ---------- 9. The member's answer renders too — the page filters nothing ---------- */

scenario.viewer = "member";
await page.goto(`${BASE}/assets/${ASSET.pid}`, { waitUntil: "load" });
await page.waitForSelector('[data-asset-page="ready"]');
await assertElevenItems(page, "member");
const memberRows = await page.locator("[data-asset-version-row]").evaluateAll((els) =>
  els.map((el) => [el.getAttribute("data-asset-version-row"), el.getAttribute("data-asset-version-visibility")]),
);
check(
  "member view: the private version the SERVER sent is rendered, labelled private",
  memberRows.length === 3 && memberRows.some(([v, vis]) => v === HIDDEN_VERSION && vis === "private"),
  JSON.stringify(memberRows),
);
check(
  "member view: the rendered version follows the server's choice",
  (await page.getAttribute("[data-asset-version]", "data-asset-version")) === HIDDEN_VERSION,
  `version=${await page.getAttribute("[data-asset-version]", "data-asset-version")}`,
);
const memberHtml = await page.content();
check(
  "member view: even a member's page discloses no project that is not public",
  !memberHtml.includes(HIDDEN_LAB.name) && !memberHtml.includes(HIDDEN_LAB.id),
);
scenario.viewer = "anonymous";

/* ---------- 10. The reads are reads ---------- */

const nonGet = apiTraffic.filter((call) => !call.startsWith("GET "));
check("reads only: every request the hub makes is a GET", nonGet.length === 0, nonGet.join(" | "));
const assetCalls = apiTraffic.filter((call) => call.includes("/api/v1/assets"));
check("reads only: the pages really exercised the asset API", assetCalls.length > 0, `${assetCalls.length} call(s)`);

await browser.close();

if (fails > 0) {
  console.error(`assets-e2e: FAILED with ${fails} failure(s)`);
  process.exit(1);
}
console.log("assets-e2e: all checks passed");
