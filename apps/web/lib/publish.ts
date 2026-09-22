/**
 * Client-side asset publish confirmation client (T1103).
 *
 * Wraps the two publish routes cmd/api/assetshttp registers:
 *   POST /api/v1/projects/{projectId}/assets:publish-preview
 *   POST /api/v1/projects/{projectId}/assets:publish
 *
 * The preview is the decision surface: it returns the six impact categories
 * docs/06 §8 requires and the blockers that would stop the publication. The
 * publish executes the exact candidate the preview was computed for, plus the
 * title/slug needed when the candidate names no existing asset.
 *
 * This module is import-free by construction (like lib/releases.ts), so the
 * node:test suite can run it through Node's type stripping.
 */

/** One publish candidate, in the shape the preview/publish bodies accept. */
export interface AssetCandidate {
  asset_pid?: string;
  asset_type: string;
  version: string;
  manifest: unknown;
  /**
   * The rights declaration to publish under. ABSENT when the stored version
   * has none: the API refuses a candidate with no rights document
   * (ASSET_MISSING_RIGHTS), and this client does not fill the gap with a
   * declaration the version never carried.
   */
  rights?: unknown;
  origin_refs: string[];
  visibility: string;
  integrity_hash: string;
  creator_ids: string[];
  title?: string;
  slug?: string;
}

/** One object version the publication would carry. */
export interface PreviewObject {
  object_version_id: string;
  object_id?: string;
  project_id?: string;
  title?: string;
  current_visibility: string;
}

/** One metadata key the manifest declares. */
export interface PreviewMetadata {
  key: string;
  value: unknown;
}

/** One blob the manifest names, with current and declared access. */
export interface PreviewBlob {
  blob_id: string;
  resolved: boolean;
  current_access: string;
  declared_access: string;
}

/** One resolved origin ref. */
export interface PreviewRef {
  ref: string;
  kind?: string;
  resolved: boolean;
  project_id?: string;
  current_visibility?: string;
}

/** One dependency pin the manifest declares. */
export interface PreviewDependency {
  pin: string;
  resolved: boolean;
  current_visibility?: string;
}

/** One referenced thing that is not public today. */
export interface PrivateDependency {
  kind: string;
  ref: string;
  blocking: boolean;
  detail: string;
}

/** One refusal from the publish gate or the rights model. */
export interface Blocker {
  code: string;
  field: string;
  detail: string;
}

/** The complete preview response (internal/assets.ImpactPreview on the wire). */
export interface ImpactPreview {
  project_id: string;
  version: string;
  target_visibility: string;
  asset: {
    pid?: string;
    asset_type: string;
    resolved: boolean;
    title?: string;
    origin_project_id?: string;
  };
  facts: {
    source_pinned: boolean;
    version_pinned: boolean;
    contributors: boolean;
    rights: boolean;
    visibility: boolean;
    dependency_pins: boolean;
    integrity_hash: boolean;
    metadata: boolean;
  };
  publishable: boolean;
  objects: PreviewObject[];
  metadata: PreviewMetadata[];
  blobs: PreviewBlob[];
  refs: PreviewRef[];
  dependencies: PreviewDependency[];
  private_dependencies: PrivateDependency[];
  publish_blockers: Blocker[];
  rights_blockers: Blocker[];
}

/** The 201 response from a successful publish. */
export interface PublishedVersion {
  asset_pid: string;
  version: string;
  visibility: string;
  integrity_hash: string;
  origin_refs: string[];
  published_by: string;
  published_at: string;
  manifest: unknown;
  rights: unknown;
}

/** The 409 refusal body from a blocked publish. */
export interface PublishBlockedBody {
  code: string;
  message: string;
  preview: ImpactPreview;
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
  readonly body?: unknown;

  constructor(status: number, envelope: ErrorEnvelope, body?: unknown) {
    super(envelope.message);
    this.name = "ApiError";
    this.code = envelope.code;
    this.status = status;
    this.retryable = envelope.retryable ?? status === 503;
    this.body = body;
  }
}

/** The publish client: one per API origin. */
export interface PublishClient {
  /** Preview the impact of publishing this candidate. */
  preview(projectId: string, candidate: AssetCandidate): Promise<ImpactPreview>;
  /** Execute the publish. */
  publish(
    projectId: string,
    candidate: AssetCandidate,
    idempotencyKey: string,
  ): Promise<PublishedVersion>;
}

type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    body?: string;
    credentials?: string;
  },
) => Promise<{ status: number; json(): Promise<unknown> }>;

/** Options for createPublishClient. */
export interface PublishClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /**
   * The session-bound CSRF token for the publish (the API's guard demands it
   * on every state change). The module stays import-free, so the caller
   * supplies the source — the page reads lib/auth's sessionTokenStorage.
   */
  csrfToken?: () => string | null;
}

const CSRF_HEADER = "X-CSRF-Token";
const IDEMPOTENCY_HEADER = "Idempotency-Key";

/** Build the publish client for one API origin. */
export function createPublishClient(
  apiBaseUrl: string,
  options: PublishClientOptions = {},
): PublishClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  async function call(
    method: string,
    path: string,
    body?: unknown,
    idempotencyKey?: string,
  ): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = {};
    const csrf = options.csrfToken?.() ?? null;
    if (csrf !== null) headers[CSRF_HEADER] = csrf;
    if (idempotencyKey !== undefined) headers[IDEMPOTENCY_HEADER] = idempotencyKey;
    if (body !== undefined) headers["Content-Type"] = "application/json";
    const res = await fetchFn(apiBaseUrl + path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: "include",
    });
    let parsed: unknown = null;
    try {
      parsed = await res.json();
    } catch {
      parsed = null;
    }
    return { status: res.status, body: parsed };
  }

  function fail(status: number, body: unknown): never {
    if (typeof body === "object" && body !== null && "code" in body) {
      throw new ApiError(status, body as ErrorEnvelope, body);
    }
    throw new ApiError(status, {
      code: "UNKNOWN",
      message: "the server answered unexpectedly; try again",
    });
  }

  function isImpactPreview(value: unknown): value is ImpactPreview {
    return (
      typeof value === "object" &&
      value !== null &&
      "publishable" in value &&
      typeof (value as ImpactPreview).publishable === "boolean"
    );
  }

  function isPublishedVersion(value: unknown): value is PublishedVersion {
    return (
      typeof value === "object" &&
      value !== null &&
      "asset_pid" in value &&
      typeof (value as PublishedVersion).asset_pid === "string"
    );
  }

  return {
    async preview(projectId, candidate) {
      const path = `/api/v1/projects/${encodeURIComponent(projectId)}/assets:publish-preview`;
      const result = await call("POST", path, candidate);
      if (result.status >= 400) fail(result.status, result.body);
      if (!isImpactPreview(result.body)) {
        throw new Error("unexpected preview response shape");
      }
      return result.body;
    },
    async publish(projectId, candidate, idempotencyKey) {
      const path = `/api/v1/projects/${encodeURIComponent(projectId)}/assets:publish`;
      const result = await call("POST", path, candidate, idempotencyKey);
      if (result.status >= 400) fail(result.status, result.body);
      if (!isPublishedVersion(result.body)) {
        throw new Error("unexpected publish response shape");
      }
      return result.body;
    },
  };
}

/** Encode a candidate so it can survive in a URL query parameter. */
export function encodeCandidateQuery(candidate: AssetCandidate): string {
  const json = JSON.stringify(candidate);
  return base64UrlEncode(json);
}

/** Decode a candidate from a URL query parameter. */
export function decodeCandidateQuery(value: string): AssetCandidate {
  return JSON.parse(base64UrlDecode(value)) as AssetCandidate;
}

function base64UrlEncode(input: string): string {
  const bytes = new TextEncoder().encode(input);
  const chars =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  let output = "";
  for (let i = 0; i < bytes.length; i += 3) {
    const a = bytes[i];
    const b = i + 1 < bytes.length ? bytes[i + 1] : null;
    const c = i + 2 < bytes.length ? bytes[i + 2] : null;
    output += chars[a >> 2];
    output += chars[((a & 3) << 4) | (b === null ? 0 : b >> 4)];
    output += b === null ? "=" : chars[((b & 15) << 2) | (c === null ? 0 : c >> 6)];
    output += c === null ? "=" : chars[c & 63];
  }
  return output.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/g, "");
}

function base64UrlDecode(value: string): string {
  const padded = value.padEnd(value.length + ((4 - (value.length % 4)) % 4), "=");
  const base64 = padded.replace(/-/g, "+").replace(/_/g, "/");
  const chars =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  let binary = "";
  for (let i = 0; i < base64.length; i += 4) {
    const enc1 = chars.indexOf(base64[i]);
    const enc2 = chars.indexOf(base64[i + 1]);
    const enc3 = base64[i + 2] === "=" ? 0 : chars.indexOf(base64[i + 2]);
    const enc4 = base64[i + 3] === "=" ? 0 : chars.indexOf(base64[i + 3]);
    const c1 = (enc1 << 2) | (enc2 >> 4);
    const c2 = ((enc2 & 15) << 4) | (enc3 >> 2);
    const c3 = ((enc3 & 3) << 6) | enc4;
    binary += String.fromCharCode(c1);
    if (base64[i + 2] !== "=") binary += String.fromCharCode(c2);
    if (base64[i + 3] !== "=") binary += String.fromCharCode(c3);
  }
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.charCodeAt(i);
  }
  return new TextDecoder().decode(bytes);
}

/**
 * The longest candidate query parameter a publish link may carry.
 *
 * The confirmation page is addressed by its candidate (`?candidate=…`), and
 * that parameter is a PATH the browser must ask the web server for. Node's
 * HTTP server caps a request's header block at 16 KiB by default — request
 * line included, cookies included — and answers 431 above it, so a candidate
 * that does not fit turns the entry point into a dead link: the reader who
 * clicks it gets an error page, not a confirmation. The budget below is
 * deliberately well under that cap: the request line carries the path AND the
 * query, and the browser adds its own headers and cookies to the same block.
 *
 * A candidate over the budget is not offered rather than offered and broken
 * (see lib/publish's publishConfirmationHref callers). Today's manifests fit
 * it by a wide margin; a metadata-heavy one does not.
 */
export const MAX_CANDIDATE_QUERY_LENGTH = 6000;

/** Metadata value shapes the manifest canonical form admits. */
export type ManifestMetadataValue = string | string[] | Record<string, string>;

/** A minimal dataset manifest, sufficient for the publish gate's shape check. */
export interface DatasetManifest {
  version: number;
  asset_type: string;
  metadata: Record<string, unknown>;
  dependency_pins: { pin: string }[];
}

/**
 * Render one JSON value the way Go's encoding/json renders it: object keys
 * ascending, compact, and the three HTML-significant characters escaped
 * (`<`, `>`, `&`) as `json.Marshal` escapes them by default.
 *
 * The escaping is the part a hand-written canonicaliser gets wrong and only
 * notices when a publisher's metadata contains an angle bracket: the
 * integrity hash is compared to the digest of Go's bytes, so a difference
 * of one escape is the difference between a publishable manifest and
 * ASSET_INTEGRITY_HASH_MISMATCH.
 */
function canonicalValue(value: unknown): string {
  let out: string;
  if (value === null) return "null";
  if (typeof value === "string" || typeof value === "number" || typeof value === "boolean") {
    out = JSON.stringify(value);
  } else if (Array.isArray(value)) {
    out = `[${value.map((v) => (v === undefined ? "null" : canonicalValue(v))).join(",")}]`;
  } else if (typeof value === "object") {
    const record = value as Record<string, unknown>;
    const keys = Object.keys(record).sort();
    const entries = keys
      .filter((k) => record[k] !== undefined)
      .map((k) => `${JSON.stringify(k)}:${canonicalValue(record[k])}`);
    out = `{${entries.join(",")}}`;
  } else {
    return "null";
  }
  return out
    .replace(/</g, "\\u003c")
    .replace(/>/g, "\\u003e")
    .replace(/&/g, "\\u0026");
}

/**
 * Build a stable, compact JSON string matching Go's canonical manifest JSON
 * (internal/assets.Manifest.CanonicalJSON): the struct's field order, the
 * metadata map sorted, and no whitespace.
 */
function canonicalManifestJSON(manifest: DatasetManifest): string {
  const metadataKeys = Object.keys(manifest.metadata).sort();
  const metadataEntries = metadataKeys.map(
    (key) => `${JSON.stringify(key)}:${canonicalValue(manifest.metadata[key])}`,
  );
  // A DependencyPin is a bare string on the wire (internal/assets
  // `type DependencyPin string`), not an object wrapping one.
  const pins = manifest.dependency_pins.map((p) => JSON.stringify(p.pin)).join(",");
  return (
    `{"version":${manifest.version},` +
    `"asset_type":${canonicalValue(manifest.asset_type)},` +
    `"metadata":{${metadataEntries.join(",")}},` +
    `"dependency_pins":[${pins}]}`
  );
}

async function sha256Hex(input: string): Promise<string> {
  const encoder = new TextEncoder();
  const data = encoder.encode(input);
  if (typeof crypto !== "undefined" && typeof crypto.subtle === "object") {
    const buf = await crypto.subtle.digest("SHA-256", data);
    return [...new Uint8Array(buf)].map((b) => b.toString(16).padStart(2, "0")).join("");
  }
  // Node fallback for tests.
  const { createHash } = await import("node:crypto");
  return createHash("sha256").update(input).digest("hex");
}

/**
 * The part of one rendered asset-version page this builder reads
 * (a structural subset of lib/assets' AssetPage).
 *
 * Everything in it is the API's own answer about one STORED version, which
 * is the point: the confirmation page's candidate is the stored version
 * re-declared with a wider target visibility, not a document this client
 * invented. The manifest is the one exception and it is a faithful
 * reconstruction — the page exposes the manifest's metadata block and its
 * dependency pins, and nothing else of it is part of the hash.
 */
export interface StoredAssetVersion {
  asset: { pid: string; type: string; origin_project: { id: string } | null };
  /**
   * The rendered version. `integrityHash` is the digest the version was
   * PUBLISHED under (research_asset_versions.integrity_hash, rendered
   * verbatim at apps/web/app/(main)/assets/asset-page.tsx): a sha256 hex
   * digest of the version's canonical manifest bytes, with no prefix.
   */
  version: { version: string; visibility: string; integrityHash: string };
  origin: { ref: string }[];
  rights: unknown;
  metadata: { key: string; value: unknown }[];
  dependencies: { pin: string }[];
  /** The credits the stored version carries, as the API renders them. */
  creators: { kind: string; partyId: string }[];
}

/** Inputs for building a publish candidate from one stored version. */
export interface AssetVersionCandidateInput {
  stored: StoredAssetVersion;
  /** The version label to publish under; default: `<stored>-public`. */
  publishedVersion?: string;
}

/**
 * The outcome of rebuilding a candidate from a rendered version.
 *
 * It is a union rather than "a candidate or an exception" so that the
 * fail-closed case cannot be forgotten: a caller that wants the href has to
 * narrow to `kind === "ready"` first, and the case where this page cannot
 * rebuild the stored document has no candidate to hand out.
 */
export type CandidateBuild =
  | { kind: "ready"; candidate: AssetCandidate }
  | ({
      /** The stored version carries no digest to compare against. */
      kind: "integrity-absent";
    } & IntegrityDifference)
  | ({ kind: "integrity-mismatch" } & IntegrityDifference);

/** What was compared, and what the page can say about the difference. */
export interface IntegrityDifference {
  /** The digest the stored version was published under ("" when absent). */
  declaredHash: string;
  /** The digest of the manifest rebuilt from this page's blocks. */
  rebuiltHash: string;
  /** How many dependency pins the rebuilt manifest carries. */
  pins: number;
  /** How many metadata keys the rebuilt manifest carries. */
  metadataKeys: number;
  /** A sentence naming the difference, for the reader. */
  detail: string;
}

/**
 * Build the publish candidate that would make a stored version's content
 * publicly visible as a NEW version of the same asset.
 *
 * A version row is immutable (docs/11 §4), so widening visibility cannot be
 * an edit: it is a second version, published under the same asset pid, with
 * the same manifest, the same provenance and the same rights declaration,
 * and a version label the publication owns. `<stored>-public` is that label;
 * if it is already taken the API refuses the publication with
 * ASSET_VERSION_IMMUTABLE, which is the correct answer — one public edition
 * of one version.
 *
 * The pid is the one the caller is already reading, which is the only way
 * this client can name an asset at all: the platform has no route that lists
 * a project's assets (GET /api/v1/assets is the public catalog) and no
 * release→asset link, so a page cannot discover "the asset this came from".
 * It is holding it, or it does not have it.
 *
 * The credits are the stored version's own: the new version credits whoever
 * the version it re-declares credited (docs/11 §3). internal/assets' gate
 * wants at least one entry that is a user id, so only the user credits are
 * carried; an asset published under an organization's name alone has no user
 * to credit and the gate refuses the candidate, which is the honest answer —
 * the credit column cannot store what the caller asked for.
 *
 * # The rebuilt document must hash to the stored one
 *
 * A version row is immutable, and its integrity_hash covers the manifest it
 * was published with. The manifest this function rebuilds is NOT that
 * document read back: it is reconstructed from the two blocks the page
 * renders — the metadata block and the dependency pins — and those blocks are
 * renders, not the document. internal/assets' `dependenciesOf` (page.go)
 * DROPS a pin whose pinned version or project is not public, so for a
 * version that pins a private dependency the page shows fewer pins than the
 * stored manifest declares. Publishing the rebuild would store a version
 * whose document is narrower than the one the reader was looking at, under a
 * hash that says the two agree.
 *
 * So the rebuild is checked against the stored digest before anything is
 * offered, and a disagreement is a REFUSAL (fail closed), not a warning: the
 * two digests are over the same canonical bytes (internal/assets
 * Manifest.CanonicalJSON — `version`, `asset_type`, the sorted metadata
 * block, then the pins — and the publish gate stores the candidate's hash
 * only after checking it equals exactly that digest), so they are directly
 * comparable.
 */
export async function buildCandidateFromStoredVersion(
  input: AssetVersionCandidateInput,
): Promise<CandidateBuild> {
  const stored = input.stored;
  const manifest: DatasetManifest = {
    version: 1,
    asset_type: stored.asset.type,
    metadata: Object.fromEntries(stored.metadata.map((m) => [m.key, m.value])),
    dependency_pins: stored.dependencies.map((d) => ({ pin: d.pin })),
  };
  const integrityHash = await sha256Hex(canonicalManifestJSON(manifest));

  const declaredHash = normalizeIntegrityHash(stored.version.integrityHash);
  if (declaredHash === null) {
    return {
      kind: "integrity-absent",
      declaredHash: "",
      rebuiltHash: integrityHash,
      pins: manifest.dependency_pins.length,
      metadataKeys: Object.keys(manifest.metadata).length,
      detail:
        "This version carries no integrity hash, so the document this page " +
        "rebuilds from its blocks cannot be checked against the one that was " +
        "published. The publication is not offered.",
    };
  }
  if (declaredHash !== integrityHash) {
    return {
      kind: "integrity-mismatch",
      declaredHash,
      rebuiltHash: integrityHash,
      pins: manifest.dependency_pins.length,
      metadataKeys: Object.keys(manifest.metadata).length,
      detail:
        `The published hash of this version (${declaredHash}) does not cover ` +
        `the document this page can rebuild from the metadata and the ` +
        `${manifest.dependency_pins.length} dependency pin${
          manifest.dependency_pins.length === 1 ? "" : "s"
        } it renders (${integrityHash}). One way that happens: a pin naming a ` +
        "version or a project this reader cannot open is withheld from the " +
        "dependency block, so the document rebuilt here is narrower than the " +
        "one that was published. The publication is not offered: publishing " +
        "this page's document would store a narrower version than the one you " +
        "are reading.",
    };
  }

  const candidate: AssetCandidate = {
    asset_pid: stored.asset.pid,
    asset_type: manifest.asset_type,
    version: input.publishedVersion ?? `${stored.version.version}-public`,
    manifest,
    origin_refs: stored.origin.map((o) => o.ref),
    // The change under review. A version's visibility is what a publication
    // sets; nothing else here moves.
    visibility: "public",
    integrity_hash: integrityHash,
    creator_ids: stored.creators
      .filter((c) => c.kind === "user")
      .map((c) => c.partyId),
  };
  // The stored declaration, or NO declaration at all. The API answers null
  // for a version whose rights document was not resolved (internal/assets'
  // rightsOf), and a fabricated declaration here would not only render a
  // rights statement the version never carried but would replace the gate's
  // own ASSET_MISSING_RIGHTS refusal with a publication that claims rights
  // nobody declared.
  if (stored.rights !== null && stored.rights !== undefined) {
    candidate.rights = stored.rights;
  }
  return { kind: "ready", candidate };
}

/**
 * The digest a stored version was published under, or null when there is
 * none to compare.
 *
 * Normalising the prefix and the case is not a loosening: the comparison
 * that matters is "same 64 hex digits", and a hash some other writer stored
 * upper-cased or prefixed (the gate writes bare lowercase hex) would
 * otherwise withhold a publication for a difference that is not one.
 */
function normalizeIntegrityHash(raw: unknown): string | null {
  if (typeof raw !== "string") return null;
  const trimmed = raw.trim();
  if (trimmed === "") return null;
  const body = trimmed.toLowerCase().startsWith("sha256:")
    ? trimmed.slice("sha256:".length)
    : trimmed;
  return body.toLowerCase();
}

/**
 * Human-facing message for a stable publish API code.
 *
 * Every code this page can be answered with is named here, because the
 * default is a dead end for the reader: "something went wrong" tells a
 * signed-out visitor the same thing it tells a publisher whose document the
 * gate refused, and only one of them can act on it. The codes are the ones
 * cmd/api/assetshttp writes (publish.go, handlers.go) and
 * internal/application/assetpublish declares; the message says what happened
 * and, where the reader can do something, what that is.
 */
export function messageForPublishCode(code: string): string {
  switch (code) {
    case "ASSET_PUBLISH_BLOCKED":
      return "The publication was refused. Review the blockers above and try again.";
    case "AUTH_UNAUTHENTICATED":
      return "You are not signed in, so you cannot publish. Sign in and open this page again.";
    case "PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL":
      return "You do not have permission to publish research assets in this project. Publishing is the project owner's decision.";
    case "PROJECT_FORBIDDEN":
      return "You do not have access to this project, so you cannot publish here.";
    case "ASSET_PUBLISH_AGENT_DENIED":
      return "A platform agent may not publish a research asset; a person has to make this decision.";
    case "VALIDATION_FAILED":
      return "The publication was refused as invalid. A version label is 1 to 64 characters of letters, digits, or '.', '_' and '-', and a manifest must declare its type's required metadata.";
    case "ASSET_PREVIEW_VALIDATION_FAILED":
      return "This page's publish candidate is not a well-formed candidate, so no preview can be computed for it.";
    case "ASSET_PREVIEW_UNAVAILABLE":
      return "The impact preview is temporarily unavailable. Please try again later.";
    case "ASSET_PUBLISH_PROJECT_NOT_FOUND":
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "ASSET_NOT_FOUND":
      return "The asset this version belongs to was not found in this project.";
    case "ASSET_ALREADY_EXISTS":
      return "An asset with this identifier already exists.";
    case "ASSET_VERSION_IMMUTABLE":
      return "This version label is already taken for this asset, and a published version cannot be replaced. Publish under another label.";
    case "IDEMPOTENCY_CONFLICT":
      return "This attempt was already submitted with a different document. Reload the page and try again.";
    case "RIGHTS_POLICY_BLOCKS_ACTION":
      return "The policy in force blocks this publication.";
    case "SERVICE_UNAVAILABLE":
      return "Publish data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while publishing. Please try again.";
  }
}
