/**
 * The Explore index client (T0802).
 *
 * Reads one surface:
 *
 *   GET /api/v1/explore    the aggregated public index — six sections
 *
 * The route is mounted by cmd/api/explorehttp (it is not in
 * specs/api/openapi.yaml: the contract declares no aggregate read, the same
 * position T0505's provenance reads are in). It is anonymous-readable: the
 * shared /api/v1 guard lets an unauthenticated GET through and the handler
 * asks for no principal, because the answer has no per-caller variant — the
 * index is built from reads whose own rules decide what is public.
 *
 * # What this module does NOT do
 *
 * It does not filter, and it does not decide visibility. Every row the API
 * returns is rendered; every row it omits is omitted by the server. A
 * front end that dropped a row would be a second implementation of the
 * disclosure rule in the layer with no test for it — the shape docs/23 §3
 * forbids ("前端隐藏不能替代后端拒绝"). In particular a nil `project` on a
 * knowledge or contribution row means the SERVER withheld it; the page says
 * nothing about it, because "project hidden" is itself a fact about a
 * private object.
 *
 * It does not rank. The order the API returns is the order the page
 * renders: the API's rule is freshness (docs/05 §6 forbids点赞数 as the core
 * ranking and nothing here re-orders by anything), and a client-side sort
 * would be a second, invisible ranking rule.
 *
 * Import-free by construction (like lib/assets.ts and lib/projects.ts), so
 * the node:test suite (explore.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** One dimension of the index. The six values are the API's own tab names. */
export const EXPLORE_TABS = [
  "projects",
  "assets",
  "knowledge",
  "people",
  "organizations",
  "contributions",
] as const;
export type ExploreTab = (typeof EXPLORE_TABS)[number];

/**
 * One tab's display label (docs/05 §6's dimension names).
 *
 * "Open Contributions" is the one name that is not the plural of an entity:
 * the tab lists opportunities a contributor can take on, so the label says
 * what the reader is looking at rather than naming a table.
 */
const TAB_LABELS: Record<ExploreTab, string> = {
  projects: "Projects",
  assets: "Assets",
  knowledge: "Knowledge",
  people: "People",
  organizations: "Organizations",
  contributions: "Open Contributions",
};

/** The label of one tab. An unknown value renders verbatim (see assets.ts). */
export function tabLabel(tab: string): string {
  return (TAB_LABELS as Record<string, string>)[tab] ?? tab;
}

/** The id of a tab's <button role="tab"> and of the panel it controls. */
export function tabId(tab: string): string {
  return `explore-tab-${tab}`;
}

/** The id of the panel a tab controls. */
export function tabPanelId(tab: string): string {
  return `explore-panel-${tab}`;
}

/** A project identity the API chose to render (nil when withheld). */
export interface ExploreProjectRef {
  id: string;
  slug: string;
  name: string;
  url: string;
}

/** One public project (internal/application/explore.ProjectItem). */
export interface ExploreProject {
  id: string;
  slug: string;
  name: string;
  purpose: string;
  activity_status: string;
  url: string;
  created_at: string;
}

/**
 * One asset row. The shape is the asset hub's own browse row
 * (internal/assets.BrowseItem), reused whole: /explore's assets tab and
 * /assets render one index built by one rule (T0709).
 */
export interface ExploreAsset {
  pid: string;
  type: string;
  title: string;
  slug: string;
  url: string;
  /** Null when the server withheld the project (a private project's asset). */
  origin_project: { id: string; name: string; slug: string; visibility: string } | null;
  public_versions: number;
  latest_version: string;
  latest_url: string;
  latest_published_at: string;
}

/** One published knowledge object version. */
export interface ExploreKnowledge {
  id: string;
  object_id: string;
  object_type: string;
  public_version: string;
  title: string;
  /** Null when the publishing project is private (docs/12 §2). */
  project: ExploreProjectRef | null;
  published_at: string;
  lifecycle_state: string;
}

/** One research profile. */
export interface ExplorePerson {
  id: string;
  handle: string;
  display_name: string;
  bio: string;
  url: string;
}

/** One active organization. */
export interface ExploreOrganization {
  id: string;
  slug: string;
  name: string;
  description: string;
}

/** One open contribution opportunity. */
export interface ExploreContribution {
  id: string;
  title: string;
  description: string;
  difficulty: string;
  target_type: string;
  required_capabilities: string[];
  /** Null when the project is private. */
  project: ExploreProjectRef | null;
  publicized_at: string;
}

/** One tab's answer. There is no total: `items.length` is what the reader sees. */
export interface ExploreSection<T> {
  tab: ExploreTab;
  items: T[];
}

/** The whole index (internal/application/explore.Index). */
export interface ExploreIndex {
  projects: ExploreSection<ExploreProject>;
  assets: ExploreSection<ExploreAsset>;
  knowledge: ExploreSection<ExploreKnowledge>;
  people: ExploreSection<ExplorePerson>;
  organizations: ExploreSection<ExploreOrganization>;
  contributions: ExploreSection<ExploreContribution>;
}

/** A failure the API named with a wire code (docs/45). */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
  }
}

/**
 * The sentence a caller sees for a wire code. `EXPLORE_UNAVAILABLE` is the
 * surface's only failure: the index is one aggregate read, so there is no
 * per-section error to report differently — and the page shows an error
 * state, never a short list, because a missing section would read as "there
 * is nothing there" (docs/51: empty and error are different states).
 */
export function messageForExploreCode(code: string): string {
  switch (code) {
    case "EXPLORE_UNAVAILABLE":
      return "The index is temporarily unavailable. Nothing was dropped — the whole index failed to load.";
    default:
      return "The index could not be loaded.";
  }
}

/**
 * The rows of one section, in the order the API returned them.
 *
 * The tab is resolved by name rather than by position so a re-ordered or
 * re-sectioned payload cannot silently render one dimension's rows under
 * another dimension's heading.
 */
export function sectionItems<T>(index: ExploreIndex, tab: ExploreTab): T[] {
  const sections = index as unknown as Record<string, ExploreSection<T> | undefined>;
  const section = sections[tab];
  return section !== undefined && Array.isArray(section.items) ? section.items : [];
}

/**
 * isIndexShape reports whether a decoded body is an index this module can
 * render: every one of the six sections present, each with its tab and an
 * items array.
 *
 * It validates SHAPE and nothing else — it is a guard against rendering a
 * malformed or truncated answer as if it were the index, not a second
 * opinion about what may be listed. A section missing from the body is not
 * an empty section, and the caller renders the error state for it.
 */
export function isIndexShape(value: unknown): value is ExploreIndex {
  if (typeof value !== "object" || value === null) return false;
  const record = value as Record<string, unknown>;
  return EXPLORE_TABS.every((tab) => {
    const section = record[tab];
    return (
      typeof section === "object" &&
      section !== null &&
      Array.isArray((section as Record<string, unknown>).items)
    );
  });
}

/** A date as the index renders one: the calendar day, UTC, no time of day. */
export function publishedOn(iso: string): string {
  const at = new Date(iso);
  if (Number.isNaN(at.getTime())) return "";
  return at.toISOString().slice(0, 10);
}

/** The minimal fetch shape this module needs (mirrors lib/assets.ts). */
export type FetchLike = (input: string, init?: Record<string, unknown>) => Promise<{
  status: number;
  ok?: boolean;
  json: () => Promise<unknown>;
}>;

/** Options for createExploreClient. */
export interface ExploreClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/** The client the index page calls. */
export interface ExploreClient {
  index(): Promise<ExploreIndex>;
}

/**
 * createExploreClient builds the client over a validated API origin.
 *
 * `credentials: "include"` sends the session cookie when the browser has
 * one — the same request either way, and the same answer: the server does
 * not vary the index by caller (there is no private half to add; that half
 * is /projects and /notifications). Sending the cookie is what keeps the
 * call honest behind a session-aware proxy; it is not a request for a
 * different index.
 */
export function createExploreClient(
  apiBaseUrl: string,
  options: ExploreClientOptions = {},
): ExploreClient {
  const doFetch = options.fetch ?? (globalThis.fetch as unknown as FetchLike);
  return {
    async index(): Promise<ExploreIndex> {
      const res = await doFetch(`${apiBaseUrl}/api/v1/explore`, {
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      const body: unknown = await res.json().catch(() => null);
      if (res.status < 200 || res.status >= 300) {
        const code =
          typeof body === "object" && body !== null && typeof (body as Record<string, unknown>).code === "string"
            ? ((body as Record<string, unknown>).code as string)
            : "UNKNOWN";
        throw new ApiError(res.status, code, messageForExploreCode(code));
      }
      if (!isIndexShape(body)) {
        throw new ApiError(res.status, "MALFORMED_INDEX", "The index could not be loaded.");
      }
      return body;
    },
  };
}
