/**
 * The Explore e2e harness's API origin (mock).
 *
 * WHY THIS IS A SERVER AND NOT `page.route`
 *
 * tests/e2e-assets mocks the API at the network layer, because the asset hub
 * is a client component: the browser makes the call. /explore is the
 * opposite — page.tsx is a server component that reads the index in the Next
 * server process (the index IS the page, and a directory that only exists
 * after a browser fetch is useless to a crawler or to a reader without
 * JavaScript). Playwright's route interception only sees the BROWSER's
 * requests, so the only way to answer a server-side read is to be a real
 * origin: this process listens on a port and the web app is built with
 * `API_BASE_URL` pointing at it.
 *
 * That is a weaker mock than a route handler in one way and a stronger one
 * in another: the call really goes over TCP and a real HTTP stack, and the
 * page's `cache: "no-store"` is really exercised; but the WIRE SHAPE is this
 * file's, not the Go handler's. The shape is not this file's invention — it
 * is the JSON internal/application/explore.Index, internal/assets.BrowseItem
 * and lib/explore.ts decode, and it is locked against the real handler by
 * tests/e2e/explore_e2e_test.go (real ServeMux + real application layer) and
 * against real PostgreSQL by tests/integration/explore_test.go. Those two
 * are where "the index is real" is decided; this file is where "the page
 * renders and drives it" is decided.
 *
 * Usage: node mock-api.mjs <port>
 *
 * Modes (flipped by the spec via GET /__mode?set=...):
 *   ok           the six-section index (the fixture below)
 *   empty        the six sections, each with zero items
 *   unavailable  503 EXPLORE_UNAVAILABLE — the surface's one failure
 *   malformed    200, but the body is missing a section
 */

import { createServer } from "node:http";

const PORT = Number(process.argv[2] ?? 0);
if (!Number.isInteger(PORT) || PORT <= 0) {
  console.error("mock-api: usage: node mock-api.mjs <port>");
  process.exit(2);
}

/* ---------- The fixture: one public world, one private project ---------- */

/**
 * The public project the index may NAME. Its identity is asserted PRESENT by
 * the spec, in all three places a row can carry a project (an asset's
 * origin, a knowledge row, an opportunity).
 */
const OPEN_PROJECT = {
  id: "01j9z6k3m4n5p6q7r8s9t0v1w01",
  slug: "open-materials-lab",
  name: "Open Materials Lab",
  url: "/projects/01j9z6k3m4n5p6q7r8s9t0v1w01",
};

/**
 * The private project. It owns a published asset version, a published
 * knowledge object version and a publicized opportunity — all three of which
 * ARE listed, each with `project: null` (docs/12 §2: a private project may
 * publish, and then the publication travels without it). No identity of this
 * project exists anywhere in the payload: no ref, no slug, no id.
 */
const HIDDEN_PROJECT = {
  id: "01j9z6k3m4n5p6q7r8s9t0v1w02",
  slug: "withheld-internal-lab",
  name: "Withheld Internal Lab",
};

/**
 * Every order below is DELIBERATELY the freshness order the API produces
 * (newest first), written out rather than sorted here: the spec asserts the
 * DOM carries exactly this order, so a client that re-sorted (or that
 * rendered a map's key order) fails instead of silently agreeing.
 */
function indexFixture() {
  return {
    projects: {
      tab: "projects",
      items: [
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w03",
          slug: "catalyst-screening-group",
          name: "Catalyst Screening Group",
          purpose: "Screen earth-abundant catalysts for low-temperature ammonia synthesis.",
          activity_status: "active",
          url: "/projects/01j9z6k3m4n5p6q7r8s9t0v1w03",
          created_at: "2026-09-14T10:00:00Z",
        },
        {
          id: OPEN_PROJECT.id,
          slug: OPEN_PROJECT.slug,
          name: OPEN_PROJECT.name,
          purpose: "What governs the phase stability of mixed-halide perovskites under load?",
          activity_status: "active",
          url: OPEN_PROJECT.url,
          created_at: "2026-09-02T10:00:00Z",
        },
      ],
    },
    assets: {
      tab: "assets",
      items: [
        {
          pid: "01j9z6k3m4n5p6q7r8s9t0v1w11",
          type: "dataset",
          title: "Perovskite Stability Dataset",
          slug: "perovskite-stability",
          url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w11",
          // Named: the project is public (T0709's BuildBrowse rule).
          origin_project: {
            id: OPEN_PROJECT.id,
            name: OPEN_PROJECT.name,
            slug: OPEN_PROJECT.slug,
            visibility: "public",
          },
          public_versions: 2,
          latest_version: "1.1.0",
          latest_url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w11?version=1.1.0",
          latest_published_at: "2026-09-13T10:00:00Z",
        },
        {
          pid: "01j9z6k3m4n5p6q7r8s9t0v1w12",
          type: "protocol",
          title: "Coating Reproducibility Protocol",
          slug: "coating-reproducibility",
          url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w12",
          // Withheld: the asset IS public (it has a published version), the
          // project that originated it is not.
          origin_project: null,
          public_versions: 1,
          latest_version: "0.9.0",
          latest_url: "/assets/01j9z6k3m4n5p6q7r8s9t0v1w12?version=0.9.0",
          latest_published_at: "2026-09-06T10:00:00Z",
        },
      ],
    },
    knowledge: {
      tab: "knowledge",
      items: [
        {
          id: "kp-01j9z6k3m4n5p6q7r8s9t0v1w31",
          object_id: "01j9z6k3m4n5p6q7r8s9t0v1w21",
          object_type: "research_question",
          public_version: "v2",
          title: "Nickel-Rich Cathode Degradation",
          project: {
            id: OPEN_PROJECT.id,
            slug: OPEN_PROJECT.slug,
            name: OPEN_PROJECT.name,
            url: OPEN_PROJECT.url,
          },
          published_at: "2026-09-12T10:00:00Z",
          lifecycle_state: "active",
        },
        {
          id: "kp-01j9z6k3m4n5p6q7r8s9t0v1w32",
          object_id: "01j9z6k3m4n5p6q7r8s9t0v1w22",
          object_type: "research_question",
          public_version: "v1",
          title: "Unlisted Origin Question",
          // Withheld: published by the private project (docs/12 §2).
          project: null,
          published_at: "2026-09-08T10:00:00Z",
          lifecycle_state: "superseded",
        },
      ],
    },
    people: {
      tab: "people",
      items: [
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w41",
          handle: "alice-chen",
          display_name: "Alice Chen",
          bio: "Computational materials scientist, perovskite stability.",
          url: "/users/01j9z6k3m4n5p6q7r8s9t0v1w41",
        },
        {
          // No bio: the row renders without a note (the empty-value branch).
          id: "01j9z6k3m4n5p6q7r8s9t0v1w42",
          handle: "bob-ioannidis",
          display_name: "Bob Ioannidis",
          bio: "",
          url: "/users/01j9z6k3m4n5p6q7r8s9t0v1w42",
        },
      ],
    },
    organizations: {
      tab: "organizations",
      items: [
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w51",
          slug: "helix-institute",
          name: "Helix Institute",
          description: "An independent lab for energy materials.",
        },
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w52",
          slug: "open-foundry",
          name: "Open Foundry",
          // No description (the other empty-value branch).
          description: "",
        },
      ],
    },
    contributions: {
      tab: "contributions",
      items: [
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w61",
          title: "Improve the stability harness",
          description: "Extend the cycling harness to 500 cycles and publish the raw traces.",
          difficulty: "intermediate",
          target_type: "research_question",
          required_capabilities: ["go", "postgres"],
          project: {
            id: OPEN_PROJECT.id,
            slug: OPEN_PROJECT.slug,
            name: OPEN_PROJECT.name,
            url: OPEN_PROJECT.url,
          },
          publicized_at: "2026-09-11T10:00:00Z",
        },
        {
          id: "01j9z6k3m4n5p6q7r8s9t0v1w62",
          title: "Trace a solubility anomaly",
          description: "",
          difficulty: "advanced",
          target_type: "research_question",
          required_capabilities: ["python"],
          // Withheld: the opportunity was publicized by the private project.
          project: null,
          publicized_at: "2026-09-07T10:00:00Z",
        },
      ],
    },
  };
}

/**
 * The identities the spec asserts are NOWHERE in the answer. Kept next to
 * the fixture so the two cannot drift: adding a leak to the fixture without
 * listing it here is the one mistake that would make the spec pass wrongly.
 */
const FORBIDDEN = [HIDDEN_PROJECT.name, HIDDEN_PROJECT.slug, HIDDEN_PROJECT.id];

/* ---------- The origin ---------- */

const TABS = ["projects", "assets", "knowledge", "people", "organizations", "contributions"];

let mode = "ok";
let exploreReads = 0;

function payload() {
  if (mode === "empty") {
    const out = {};
    for (const tab of TABS) out[tab] = { tab, items: [] };
    return out;
  }
  const body = indexFixture();
  if (mode === "malformed") {
    // A section missing from the body: not an empty section, a broken
    // answer. The client's shape guard is what turns this into the error
    // panel instead of a Contributions tab that claims nothing is open.
    delete body.contributions;
  }
  return body;
}

function send(res, status, body, origin) {
  const json = JSON.stringify(body);
  const headers = {
    "content-type": "application/json",
    "cache-control": "no-store",
  };
  // The browser calls this origin too (the global nav's session read), so
  // the mock must be CORS-correct for a credentialed request from the web
  // app's own origin: echo it, never "*".
  if (origin) {
    headers["access-control-allow-origin"] = origin;
    headers["access-control-allow-credentials"] = "true";
    headers["vary"] = "Origin";
  }
  res.writeHead(status, headers);
  res.end(json);
}

const server = createServer((req, res) => {
  const url = new URL(req.url, `http://127.0.0.1:${PORT}`);
  const origin = req.headers.origin;

  if (req.method === "OPTIONS") {
    res.writeHead(204, {
      "access-control-allow-origin": origin ?? "*",
      "access-control-allow-credentials": "true",
      "access-control-allow-methods": "GET, OPTIONS",
      "access-control-allow-headers": "accept, content-type",
      vary: "Origin",
    });
    res.end();
    return;
  }

  // Control plane for the spec, in-process with the run script's env. Not
  // part of any API: the real origin has no such route, and the web app
  // never calls it.
  if (url.pathname === "/__mode") {
    const set = url.searchParams.get("set");
    if (set !== null) mode = set;
    return send(res, 200, { mode, exploreReads }, origin);
  }
  if (url.pathname === "/__stats") {
    return send(res, 200, { mode, exploreReads, forbidden: FORBIDDEN }, origin);
  }

  if (url.pathname === "/api/v1/explore") {
    exploreReads += 1;
    if (mode === "unavailable") {
      return send(
        res,
        503,
        {
          code: "EXPLORE_UNAVAILABLE",
          message: "the network index is temporarily unavailable",
          request_id: "e2e-explore",
          retryable: true,
        },
        origin,
      );
    }
    return send(res, 200, payload(), origin);
  }

  if (url.pathname === "/api/v1/auth/session") {
    // Anonymous: the harness tests the public index, and the page does not
    // vary by caller. The nav renders its signed-out state.
    return send(
      res,
      401,
      { code: "UNAUTHENTICATED", message: "authentication required", request_id: "e2e-explore", retryable: false },
      origin,
    );
  }

  send(res, 404, { code: "NOT_FOUND", message: "no such route", request_id: "e2e-explore", retryable: false }, origin);
});

server.listen(PORT, "127.0.0.1", () => {
  console.log(`mock-api: listening on http://127.0.0.1:${PORT}`);
});
