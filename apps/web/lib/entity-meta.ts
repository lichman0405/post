/**
 * Public-entity page metadata and the public crawl index (T0801).
 *
 * docs/51 §"Public pages 服务端输出可索引内容" is the rule this module
 * implements: a public page server-renders indexable content, and a page
 * whose entity is not public is not indexed. Both halves are decided HERE,
 * on the server, in a <head> a crawler reads without running any
 * JavaScript — the client components below these routes keep rendering the
 * page itself.
 *
 * # The audience is a crawler, so the fetch is anonymous
 *
 * Every fetch in this module is a request with NO session cookie, no
 * Authorization header and `credentials: "omit"` — the same request a
 * search engine's crawler makes. That is the whole visibility mechanism
 * here: `GET /api/v1/projects/{id}` answers a public project to an
 * anonymous caller and the existence-hiding 404 to everyone else
 * (docs/45; the API decides, T0106), so a 200 IS the public answer and
 * this module owns no second copy of the rule. A private project cannot
 * be indexed through a reader's session either, because the session is
 * never sent: indexability is a property of the ENTITY, not of whoever
 * happens to be looking at it.
 *
 * # Not public means: no name, no distinction, no index
 *
 * Every non-public outcome — unknown id, private entity, API unreachable,
 * malformed payload — collapses into ONE head (hiddenPageMetadata): the
 * same generic title, no canonical, `noindex, nofollow`, and not one word
 * of the entity. "This project is private" and "no such project" must be
 * the same page from outside (docs/45 existence hiding; the same reason
 * the API answers both with one 404 code), and a crawler must not be able
 * to build a list of private ids by diffing the two.
 *
 * # Why the pages stay 200
 *
 * A signed-in member of a private project legitimately renders that page —
 * the client component fetches it with their session. The server therefore
 * cannot answer 404 for an entity the anonymous metadata fetch did not
 * find; it answers 200 with the unindexable head above, and the page body
 * stays exactly as visible as the API lets it be.
 *
 * Import-free by construction (like lib/projects.ts and lib/assets.ts), so
 * the node:test suite (entity-meta.test.mjs) runs it through Node's type
 * stripping; the copy of a URL path below is the small price of that, and
 * each one names the module that owns it.
 */

import type { Metadata } from "next";

/** The public entity families the web app serves (docs/05 §4; routes: docs/05 §3). */
export type PublicEntityKind = "project" | "release" | "asset" | "profile";

/** What a page needs to put an entity in its <head>. */
export interface PublicEntityMeta {
  /** The entity's own name: a project name, an asset title, a display name. */
  name: string;
  /** One line in the entity's own words (a purpose, a bio); "" when it has none. */
  description: string;
}

/** One crawl-index row: a public page, and when its entity first existed. */
export interface PublicIndex {
  projects: { id: string; created_at: string }[];
  assets: { pid: string; created_at: string }[];
}

/** The generic title of every page this module cannot describe (see the header). */
export const HIDDEN_PAGE_TITLE = "Not found — POST";

/** The generic description of the same pages: a sentence about the page, never about an entity. */
export const HIDDEN_PAGE_DESCRIPTION = "This page is not available on POST.";

/**
 * What is said about an entity that has no words of its own. It describes
 * the PAGE, not the research: a front end that invented a scientific
 * description would be writing claims the entity never made.
 */
const NO_DESCRIPTION = "Public research on POST.";

/** Meta descriptions are truncated by search engines around here. */
const DESCRIPTION_LIMIT = 160;

/** The longest name this module will put in a <title> before cutting it. */
const TITLE_LIMIT = 80;

/** How long a metadata fetch may take before its entity is treated as not public. */
const FETCH_TIMEOUT_MS = 2000;

// ---------------------------------------------------------------------
// Pure helpers
// ---------------------------------------------------------------------

/** Collapses whitespace and cuts at limit, so a multi-line purpose is one meta line. */
export function oneLine(text: string, limit = DESCRIPTION_LIMIT): string {
  const collapsed = text.replace(/\s+/g, " ").trim();
  if (collapsed.length <= limit) return collapsed;
  return `${collapsed.slice(0, limit - 1).trimEnd()}…`;
}

/**
 * The web path of one public entity. These are the app's own routes
 * (docs/05 §3 project pages, §4 network entities); the asset half is
 * internal/assets/url.go's persistent address, spelled the same way here
 * as in lib/assets.ts (assetHref/assetVersionHref).
 */
export function publicEntityPath(
  kind: PublicEntityKind,
  id: string,
  releaseId?: string,
  version?: string,
): string {
  switch (kind) {
    case "project":
      return `/projects/${encodeURIComponent(id)}`;
    case "release":
      return `/projects/${encodeURIComponent(id)}/releases/${encodeURIComponent(releaseId ?? "")}`;
    case "asset":
      return version === undefined || version === ""
        ? `/assets/${encodeURIComponent(id)}`
        : `/assets/${encodeURIComponent(id)}/${encodeURIComponent(version)}`;
    case "profile":
      return `/users/${encodeURIComponent(id)}`;
  }
}

/**
 * The <head> of a page whose entity a crawler (i.e. an anonymous caller)
 * may see: the entity's own name, its own one-line description, its
 * canonical address, and an explicit invitation to index it.
 */
export function publicPageMetadata(entity: PublicEntityMeta, canonical: string): Metadata {
  const title = `${oneLine(entity.name, TITLE_LIMIT)} — POST`;
  const description =
    entity.description.trim() === "" ? NO_DESCRIPTION : oneLine(entity.description);
  return {
    title,
    description,
    alternates: { canonical },
    robots: { index: true, follow: true },
    openGraph: {
      type: "website",
      title,
      description,
      url: canonical,
      siteName: "POST",
    },
  };
}

/**
 * The <head> of a page this module cannot describe: not indexed, and
 * identical for every such page — see the module header. No canonical:
 * there is nothing here for a search engine to consolidate, and naming the
 * address would assert the page is a real resource.
 *
 * This function defines no canonical; it cannot unsay a parent segment's.
 * Next merges the metadata of every segment of a route, so a page that
 * hides itself UNDER a segment that describes a public entity (a release
 * URL under a public project, say) keeps that segment's canonical — a
 * public, already-indexable address, which is why that is acceptable. What
 * this function owns everywhere is the part that decides the question:
 * the generic title, the generic description, and `noindex, nofollow`.
 */
export function hiddenPageMetadata(): Metadata {
  return {
    title: HIDDEN_PAGE_TITLE,
    description: HIDDEN_PAGE_DESCRIPTION,
    robots: { index: false, follow: false },
    openGraph: {
      type: "website",
      title: HIDDEN_PAGE_TITLE,
      description: HIDDEN_PAGE_DESCRIPTION,
      siteName: "POST",
    },
  };
}

/**
 * The deployment's public web origin (scheme://host[:port]) for absolute
 * URLs — canonical links, OpenGraph urls, the sitemap.
 *
 * A configured origin (POST_WEB_ORIGIN, the same variable the API binds
 * its CORS and OIDC redirects to) wins. Without one, the origin is derived
 * from the request itself: a reverse proxy in front of the app sets
 * x-forwarded-*, and a direct request carries Host. Deriving it is what
 * keeps a plain `next start` deployment (make smoke, the e2e harnesses)
 * emitting correct absolute URLs with no extra configuration.
 *
 * NEVER emit a redirect or a link from this value without validating it
 * first — the Host header is caller-controlled, which is exactly why a
 * configured origin takes precedence in production.
 */
export function resolvePublicOrigin(
  configured: string,
  request: {
    host?: string | null;
    forwardedHost?: string | null;
    forwardedProto?: string | null;
  },
): string {
  const fromConfig = normalizeOrigin(configured);
  if (fromConfig !== "") return fromConfig;

  const host = (request.forwardedHost ?? request.host ?? "").trim();
  if (!isHostLike(host)) return "";
  const proto = (request.forwardedProto ?? "").trim().toLowerCase();
  const scheme = proto === "https" || proto === "http" ? proto : "http";
  return `${scheme}://${host}`;
}

/** The crawl index of every public entity page, from the anonymous public reads. */
export function publicIndexEntries(
  origin: string,
  index: PublicIndex,
): { url: string; lastModified?: Date }[] {
  const entries: { url: string; lastModified?: Date }[] = [
    { url: `${origin}/` },
    { url: `${origin}/projects` },
    { url: `${origin}/assets` },
  ];
  for (const project of index.projects) {
    entries.push(withLastModified(`${origin}${publicEntityPath("project", project.id)}`, project.created_at));
  }
  for (const asset of index.assets) {
    entries.push(withLastModified(`${origin}${publicEntityPath("asset", asset.pid)}`, asset.created_at));
  }
  return entries;
}

function withLastModified(url: string, createdAt: string): { url: string; lastModified?: Date } {
  const when = new Date(createdAt);
  if (Number.isNaN(when.getTime())) return { url };
  return { url, lastModified: when };
}

/** scheme://host[:port] with no path and no trailing slash, or "" when unusable. */
function normalizeOrigin(value: string): string {
  const trimmed = value.trim();
  if (trimmed === "") return "";
  let parsed: URL;
  try {
    parsed = new URL(trimmed);
  } catch {
    return "";
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") return "";
  if (parsed.host === "") return "";
  return `${parsed.protocol}//${parsed.host}`;
}

/** Host[:port] and nothing else — no scheme, no path, no whitespace, no credentials. */
function isHostLike(host: string): boolean {
  if (host === "" || host.length > 255) return false;
  if (/[\s/@\\?#]/.test(host)) return false;
  return /^[A-Za-z0-9.\-]+(:\d{1,5})?$/.test(host) || /^\[[0-9A-Fa-f:.]+\](:\d{1,5})?$/.test(host);
}

// ---------------------------------------------------------------------
// The anonymous reads
// ---------------------------------------------------------------------

type JSONObject = Record<string, unknown>;

function isObject(value: unknown): value is JSONObject {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/** One string field of a payload, "" when absent or of another type. */
function field(body: JSONObject, key: string): string {
  const value = body[key];
  return typeof value === "string" ? value : "";
}

function apiURL(apiBaseUrl: string, path: string): string {
  return `${apiBaseUrl.replace(/\/+$/, "")}/api/v1${path}`;
}

/**
 * One anonymous JSON read. Returns null for every outcome that is not a
 * 2xx JSON object: a refusal (404/401/403/5xx), a timeout, an unreachable
 * API, a body that is not JSON. null always means "not public" downstream,
 * which is the fail-closed direction — an API outage makes pages
 * temporarily unindexable, never wrongly indexed.
 */
async function getJSON(url: string, timeoutMs = FETCH_TIMEOUT_MS): Promise<JSONObject | null> {
  try {
    const res = await fetch(url, {
      cache: "no-store",
      credentials: "omit",
      headers: { Accept: "application/json" },
      signal: AbortSignal.timeout(timeoutMs),
    });
    if (!res.ok) return null;
    const body: unknown = await res.json();
    return isObject(body) ? body : null;
  } catch {
    return null;
  }
}

/**
 * One public project (GET /api/v1/projects/{id}, anonymous): its name and
 * its purpose.
 */
export async function fetchPublicProjectMeta(
  apiBaseUrl: string,
  projectId: string,
): Promise<PublicEntityMeta | null> {
  const body = await getJSON(apiURL(apiBaseUrl, `/projects/${encodeURIComponent(projectId)}`));
  if (body === null) return null;
  const name = field(body, "name");
  if (name === "") return null;
  return { name, description: field(body, "purpose") };
}

/**
 * One public release (GET /api/v1/projects/{id}/releases/{releaseId}):
 * the release itself plus the project it belongs to, so the title names
 * what the snapshot is OF and the description names which release it is.
 */
export async function fetchPublicReleaseMeta(
  apiBaseUrl: string,
  projectId: string,
  releaseId: string,
): Promise<PublicEntityMeta | null> {
  const project = await getJSON(apiURL(apiBaseUrl, `/projects/${encodeURIComponent(projectId)}`));
  if (project === null) return null;
  const projectName = field(project, "name");
  if (projectName === "") return null;

  const release = await getJSON(
    apiURL(
      apiBaseUrl,
      `/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(releaseId)}`,
    ),
  );
  if (release === null) return null;
  const version = field(release, "version");
  const title = field(release, "title");
  const name = title !== "" ? title : `Release ${version}`;
  if (name.trim() === "") return null;
  return {
    name,
    description:
      version === ""
        ? `Immutable research release of ${projectName}.`
        : `Release ${version} of ${projectName} — an immutable research snapshot.`,
  };
}

/**
 * One public asset version (GET /api/v1/assets/{pid}[?version=], anonymous):
 * its title, plus the origin project's name when the API chose to render it
 * (it withholds the identity of a project a reader may not see — this
 * module renders what it is given and never re-derives it).
 */
export async function fetchPublicAssetMeta(
  apiBaseUrl: string,
  pid: string,
  version?: string,
): Promise<PublicEntityMeta | null> {
  const base = apiURL(apiBaseUrl, `/assets/${encodeURIComponent(pid)}`);
  const url = version === undefined || version === "" ? base : `${base}?version=${encodeURIComponent(version)}`;
  const body = await getJSON(url);
  if (body === null) return null;

  const asset = body["asset"];
  if (!isObject(asset)) return null;
  const title = field(asset, "title");
  if (title === "") return null;

  const origin = asset["origin_project"];
  const originName = isObject(origin) ? field(origin, "name") : "";
  const published = originName === "" ? "" : ` published by ${originName}`;
  // The version is named only on the version route: the pid route resolves
  // to whichever version the reader may see, so naming one there would
  // describe something the URL does not say (T0709's page does the same).
  const subject =
    version === undefined || version === ""
      ? "Research asset"
      : `Version ${version} of a research asset`;
  return {
    name: title,
    description: `${subject}${published} on POST.`,
  };
}

/**
 * One public profile (GET /api/v1/users/{id}/profile, anonymous): the
 * display name, the handle, and the owner's own bio.
 */
export async function fetchPublicProfileMeta(
  apiBaseUrl: string,
  userId: string,
): Promise<PublicEntityMeta | null> {
  const body = await getJSON(apiURL(apiBaseUrl, `/users/${encodeURIComponent(userId)}/profile`));
  if (body === null) return null;
  const displayName = field(body, "display_name");
  const handle = field(body, "handle");
  if (displayName === "" && handle === "") return null;
  return {
    name: handle === "" ? displayName : `${displayName} (@${handle})`,
    description: field(body, "bio"),
  };
}

/**
 * The public index a sitemap is built from: every public project and every
 * public asset, read anonymously (GET /api/v1/projects answers an
 * anonymous caller with the public list only, T0106; the asset hub answers
 * with public versions only, T0709).
 *
 * A read that fails yields an empty list rather than an error: a sitemap
 * that omits URLs for a moment is recoverable, an API outage must not turn
 * /sitemap.xml into a 500 that a crawler remembers.
 */
export async function fetchPublicIndex(apiBaseUrl: string, timeoutMs = FETCH_TIMEOUT_MS): Promise<PublicIndex> {
  const [projectsBody, assetsBody] = await Promise.all([
    getJSON(apiURL(apiBaseUrl, "/projects"), timeoutMs),
    getJSON(apiURL(apiBaseUrl, "/assets"), timeoutMs),
  ]);
  const index: PublicIndex = { projects: [], assets: [] };

  const projects = projectsBody === null ? undefined : projectsBody["projects"];
  if (Array.isArray(projects)) {
    for (const row of projects) {
      if (!isObject(row)) continue;
      const id = field(row, "id");
      if (id === "") continue;
      index.projects.push({ id, created_at: field(row, "created_at") });
    }
  }

  const assets = assetsBody === null ? undefined : assetsBody["assets"];
  if (Array.isArray(assets)) {
    for (const row of assets) {
      if (!isObject(row)) continue;
      const pid = field(row, "pid");
      if (pid === "") continue;
      index.assets.push({ pid, created_at: field(row, "latest_published_at") });
    }
  }

  return index;
}
