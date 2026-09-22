/**
 * Client-side assets client for the Go API (T0709).
 *
 * Reads the asset hub's two surfaces:
 *
 *   GET /api/v1/assets            the browse list (type filter, optional)
 *   GET /api/v1/assets/{pid}      one asset's page data, `?version=` optional
 *
 * The first is the contract's path for the asset page data
 * (specs/api/openapi.yaml: /assets/{assetId}, `security: []` — a public
 * read); the second is the hub's list, mounted by cmd/api/assetshttp
 * (page.go) because the contract has no path for a listing. Both are
 * anonymous-readable, and both calls therefore carry the session cookie
 * only when the browser already has one (credentials: "include") — a
 * signed-in caller sees the versions and identities their own projects'
 * membership allows, and the SERVER decides that (internal/assets.BuildPage;
 * a member's page renders the private versions, a visitor's does not).
 *
 * # What this module does NOT do
 *
 * It does not filter. Every block the API returns is rendered by the page:
 * a front end that dropped a row the API sent would be a second
 * implementation of the disclosure rule, in the layer with no test for it
 * and every reason to be believed — and the one docs/23 §3 forbids outright
 * ("前端隐藏不能替代后端拒绝"). Absence in this module is always the
 * server's decision, never the client's.
 *
 * It holds no copy of the visibility rules and no opinion about who may see
 * what. It does carry the closed V1 type set, because the browse filter has
 * to offer exactly the types the platform has — and the API echoes the set
 * back (BrowseList.types) so the page renders the server's list once it has
 * an answer, with the local copy used only for the first paint.
 *
 * Import-free by construction (like lib/projects.ts and lib/releases.ts),
 * so the node:test suite (assets.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** The closed V1 asset type set (internal/assets.Type; research_assets.asset_type's CHECK). */
export const ASSET_TYPES = ["dataset", "protocol", "material_collection", "benchmark"] as const;
export type AssetType = (typeof ASSET_TYPES)[number];

/** The catalog key of one asset type's display label (T1105).
 *
 *  A KEY and not a sentence, because this module cannot reach the catalog:
 *  the page resolves it with t(assetTypeLabelKey(type)). The type CODE — the
 *  value `data-asset-type` carries and the value the wire speaks — is the
 *  type itself and never passes through here (docs/28 §3). */
const TYPE_LABEL_KEYS: Record<AssetType, string> = {
  dataset: "asset.type.dataset",
  protocol: "asset.type.protocol",
  material_collection: "asset.type.materialCollection",
  benchmark: "asset.type.benchmark",
};

/**
 * The catalog key of one asset type's display label, or null for a value this
 * client was not taught.
 *
 * An unknown value renders VERBATIM rather than as its nearest known type or
 * as "other": the set is closed on the server, so a value outside it is a
 * value this client does not know — not a value that does not exist — and a
 * label invented here would misreport the row it labels. The CALLER does that
 * (`t(assetTypeLabelKey(x) ?? x)`), which is why this returns null rather than
 * the raw value: a function that cannot translate must not pretend to.
 */
export function assetTypeLabelKey(type: string): string | null {
  return (TYPE_LABEL_KEYS as Record<string, string>)[type] ?? null;
}

/** The catalog key of the browse list's empty line for one type filter.
 *
 *  The line is per type rather than one sentence with the label interpolated
 *  into it, because English lower-cases the label inside the sentence ("No
 *  published dataset assets yet.") and no other language does. One key per
 *  type keeps the English byte-identical without an English-only transform
 *  the catalog could not express. */
const TYPE_EMPTY_KEYS: Record<AssetType, string> = {
  dataset: "assets.browse.empty.dataset",
  protocol: "assets.browse.empty.protocol",
  material_collection: "assets.browse.empty.materialCollection",
  benchmark: "assets.browse.empty.benchmark",
};

/** The catalog key of that empty line, or null for a type this client was not
 *  taught (the caller then renders the unfiltered line). */
export function assetTypeEmptyKey(type: string): string | null {
  return (TYPE_EMPTY_KEYS as Record<string, string>)[type] ?? null;
}

/** A project identity the API chose to render (cmd/api/assetshttp; nil when withheld). */
export interface AssetProject {
  id: string;
  name: string;
  slug: string;
  visibility: string;
}

/** A user identity as the API renders one. */
export interface AssetUser {
  user_id: string;
  handle: string;
  display_name: string;
}

/** One browse-list row (internal/assets.BrowseItem). */
export interface AssetSummary {
  pid: string;
  type: AssetType;
  title: string;
  slug: string;
  url: string;
  /** Null when the server withheld the project (a private project's asset). */
  origin_project: AssetProject | null;
  /** How many versions of this asset are public — never a total. */
  public_versions: number;
  latest_version: string;
  latest_url: string;
  latest_published_at: string;
}

/** The browse answer (internal/assets.BrowseList). */
export interface AssetBrowseList {
  /** The filter the server applied, null for "every type". */
  type: AssetType | null;
  /** The closed V1 type set, from the server. */
  types: AssetType[];
  assets: AssetSummary[];
}

/** One version row of the page's versions block. */
export interface AssetVersionSummary {
  version: string;
  url: string;
  visibility: string;
  integrity_hash: string;
  published_at: string;
  current: boolean;
}

/** One metadata key of the rendered version's manifest. */
export interface AssetMetadataEntry {
  key: string;
  value: unknown;
}

/** One resolved origin ref. */
export interface AssetOrigin {
  ref: string;
  kind: string;
  resolved: boolean;
  title?: string;
  project_id?: string;
  link?: string;
}

/** One dependency pin of the rendered version (internal/assets.PageDependency). */
export interface AssetDependency {
  pin: string;
  resolved: boolean;
  public: boolean;
  title?: string;
  type?: AssetType;
  url?: string;
}

/** One fork/derive edge (internal/assets.PageLineage). */
export interface AssetLineage {
  relation: string;
  direction: "parent" | "child";
  pid: string;
  version: string;
  url: string;
  title: string;
}

/** One PUBLIC usage (internal/assets.PageUsage). */
export interface AssetUsage {
  project_id: string;
  project_name: string;
  project_slug: string;
  dependency_type: string;
  created_at: string;
}

/** One research event (internal/assets.PageEvent). */
export interface AssetEvent {
  type: string;
  occurred_at: string;
  version?: string;
  actor: AssetUser | null;
}

/** One credited party (internal/assets.PageCreator — the role is named). */
/**
 * One credited party of the rendered version (internal/assets.PageCreator).
 *
 * The party is an identity — a kind and an id TOGETHER, never an id alone:
 * `kind` says which of the two identity tables `party_id` is a row of (00002
 * keeps users and organizations separate), and `handle`/`display_name` are
 * read from that table — a user's handle and display name, or an
 * organization's slug and name. It deliberately does NOT extend AssetUser: an
 * organization has no user id, and rendering one in a user field is the
 * merge those two tables exist to prevent.
 */
export interface AssetCreator {
  /** Which identity table `party_id` names: "user" or "organization". */
  kind: string;
  party_id: string;
  /** A user's handle, or an organization's slug. */
  handle: string;
  /** A user's display name, or an organization's name. */
  display_name: string;
  /** The credited relationship: "creator" for the credits published today. */
  role: string;
}

/**
 * The handle a credited party is shown under.
 *
 * A user's handle is a mention and is rendered as one (`@carol`). An
 * organization has no handle: `handle` carries its SLUG, and `@open-mof-lab`
 * would render a lab in the user-mention shape the platform uses to link to
 * people — the merge internal/assets.PageCreator's split of the two identity
 * tables exists to prevent. The organization therefore shows its slug bare.
 */
export function creatorHandleLabel(creator: Pick<AssetCreator, "kind" | "handle">): string {
  return creator.kind === "user" ? `@${creator.handle}` : creator.handle;
}

/** One asset page's data, in the eleven-block order of docs/42 §Asset Page. */
export interface AssetPage {
  asset: {
    pid: string;
    type: AssetType;
    title: string;
    slug: string;
    origin_project: AssetProject | null;
    created_at: string;
  };
  version: {
    version: string;
    url: string;
    visibility: string;
    integrity_hash: string;
    published_at: string;
    published_by: AssetUser | null;
  };
  origin: AssetOrigin[];
  rights: unknown;
  creators: AssetCreator[];
  metadata: AssetMetadataEntry[];
  dependencies: AssetDependency[];
  lineage: AssetLineage[];
  used_by: AssetUsage[];
  versions: AssetVersionSummary[];
  events: AssetEvent[];
}

/** The API error envelope (docs/22 §5). */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/** ApiError carries the API's envelope (same shape as lib/projects.ts). */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly retryable: boolean;

  constructor(status: number, envelope: ErrorEnvelope) {
    super(envelope.message);
    this.name = "ApiError";
    this.code = envelope.code;
    this.status = status;
    this.retryable = envelope.retryable ?? status === 503;
  }
}

/** The asset hub client: one per API origin. */
export interface AssetsClient {
  /**
   * The hub's list. `type` is one of the four V1 types; omitting it asks
   * for every type (an absent filter is not a filter).
   */
  browse(type?: AssetType | null): Promise<AssetBrowseList>;
  /**
   * One asset's page data. `version` names a version to render; omitting it
   * renders the newest version the caller may see.
   *
   * A 404 (ASSET_NOT_FOUND) covers an unknown pid, an asset whose project
   * the caller may not read, an asset with nothing the caller may see, and
   * a version the caller may not see — deliberately indistinguishable, so
   * the page must render ONE not-found state for all of them.
   */
  page(pid: string, version?: string | null): Promise<AssetPage>;
}

type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    credentials?: string;
  },
) => Promise<{ status: number; json(): Promise<unknown> }>;

/** Options for createAssetsClient. */
export interface AssetsClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/** The web URL of an asset's page. */
export function assetHref(pid: string): string {
  return `/assets/${encodeURIComponent(pid)}`;
}

/** The web URL of one asset version (a persistent address, internal/assets/url.go). */
export function assetVersionHref(pid: string, version: string): string {
  return `${assetHref(pid)}/${encodeURIComponent(version)}`;
}

/**
 * The API URL of the browse list, with the type filter when one was asked
 * for.
 *
 * An absent filter is not a filter: null and undefined both mean "every
 * type", and the URL carries no `type=` at all. (The parameter is typed to
 * the closed set, so there is no third "empty" value to guard against — a
 * caller cannot reach this function holding a value the API would refuse.)
 */
export function browseUrl(apiBaseUrl: string, type?: AssetType | null): string {
  const base = `${apiBaseUrl.replace(/\/+$/, "")}/api/v1/assets`;
  if (type === undefined || type === null) return base;
  return `${base}?type=${encodeURIComponent(type)}`;
}

/** The API URL of one asset's page data, with the version when one was asked for. */
export function assetPageUrl(apiBaseUrl: string, pid: string, version?: string | null): string {
  const base = `${apiBaseUrl.replace(/\/+$/, "")}/api/v1/assets/${encodeURIComponent(pid)}`;
  if (version === undefined || version === null || version === "") return base;
  return `${base}?version=${encodeURIComponent(version)}`;
}

/**
 * The catalog key of the line a page renders for an API refusal.
 *
 * The codes are cmd/api/assetshttp's. ASSET_NOT_FOUND is ONE line on
 * purpose: the API answers it for four different situations, and the page
 * must not try to tell them apart — anything more specific would be the
 * oracle the single code exists not to be. The code is what this table
 * maps; the line itself is in the catalog, so it renders in the reader's
 * language.
 */
export function assetCodeKey(code: string): string {
  switch (code) {
    case "ASSET_NOT_FOUND":
      return "asset.code.notFound";
    case "ASSET_PAGE_UNAVAILABLE":
      return "asset.code.unavailable";
    case "ASSET_LIST_VALIDATION_FAILED":
      return "asset.code.listValidationFailed";
    case "ASSET_PAGE_VALIDATION_FAILED":
      return "asset.code.pageValidationFailed";
    default:
      return "asset.code.generic";
  }
}

/**
 * The envelope this client synthesizes itself: when the body did not decode
 * into one, or when the connection failed before there was a body at all.
 *
 * `message` carries the CODE and not a sentence. An ApiError's message is a
 * developer diagnostic — the pages render the code through the catalog
 * (assetCodeKey), never this string — so prose here would be English that no
 * reader is ever shown, and the code is exactly what an engineer reading a
 * log needs. It is also the one shape this file can offer the scanner: the
 * scanner's rules cannot tell a developer diagnostic from rendered copy in a
 * .ts file, and a file that has to be exempted from the rules is a file whose
 * NEXT sentence goes unnoticed.
 */
function syntheticEnvelope(code: string, retryable: boolean): ErrorEnvelope {
  return { code, message: code, request_id: undefined, retryable };
}

/** Decode an error body into the envelope, falling back to a generic code. */
function envelopeFor(status: number, body: unknown): ErrorEnvelope {
  if (typeof body === "object" && body !== null) {
    const env = body as Partial<ErrorEnvelope>;
    if (typeof env.code === "string" && env.code !== "") {
      return {
        code: env.code,
        /* An envelope that carries a code but no message of its own is
         * answered with its own code, for the reason above. */
        message: typeof env.message === "string" ? env.message : env.code,
        request_id: env.request_id,
        retryable: env.retryable,
      };
    }
  }
  return syntheticEnvelope("UNKNOWN", status === 503 || status >= 500);
}

/** Create the assets client over one API origin. */
export function createAssetsClient(
  apiBaseUrl: string,
  options: AssetsClientOptions = {},
): AssetsClient {
  const doFetch = options.fetch ?? (globalThis.fetch as unknown as FetchLike);

  async function get<T>(url: string): Promise<T> {
    // credentials: "include" — the session cookie rides along when the
    // browser has one, which is what makes a member's view of their own
    // project's asset possible. It is not required: the routes are public
    // reads, so an anonymous caller gets the network's view.
    const res = await doFetch(url, { credentials: "include" });
    const body = await res.json();
    if (res.status < 200 || res.status >= 300) {
      throw new ApiError(res.status, envelopeFor(res.status, body));
    }
    return body as T;
  }

  return {
    browse(type) {
      return get<AssetBrowseList>(browseUrl(apiBaseUrl, type ?? null));
    },
    page(pid, version) {
      return get<AssetPage>(assetPageUrl(apiBaseUrl, pid, version ?? null));
    },
  };
}
