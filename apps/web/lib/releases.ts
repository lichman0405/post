/**
 * Client-side releases client for the Go API (T0606).
 *
 * Reads and creates the immutable release surface the Releases tab
 * renders from:
 * GET  /api/v1/projects/{id}/releases
 * POST /api/v1/projects/{id}/releases
 * GET  /api/v1/projects/{id}/releases/{releaseId}
 * The manifest export itself is a plain anchor download (the browser
 * carries the session cookie) — the client only wraps the JSON calls.
 * All calls carry the session cookie (credentials: include); the create
 * also heads the session-bound CSRF token and an Idempotency-Key (the
 * caller supplies it and keeps it until the create succeeds, so a retry
 * replays instead of conflicting — docs/22).
 *
 * The module is import-free by construction (like lib/projects.ts), so
 * the node:test suite (releases.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** The wire release shape (cmd/api/releasehttp releasePayload). */
export interface Release {
  id: string;
  project_id: string;
  version: string;
  title: string;
  state_id: string;
  policy_version_id: string | null;
  org_policy_version_id: string | null;
  manifest_hash: string;
  created_by: string;
  created_at: string;
}

/** The API error envelope (docs/22 §5): stable codes, no stack traces. */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/**
 * ApiError carries the API's envelope: the UI renders `message` for the
 * codes it knows and a generic line otherwise — never raw transport
 * errors. (Same shape as lib/projects.ts ApiError.)
 */
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

/** The release-create input the POST body renders from. */
export interface CreateReleaseInput {
  version: string;
  title?: string;
  /**
   * The Idempotency-Key for this create attempt (docs/22): the same key
   * replays the create it names. The page generates one per fresh form
   * and keeps it across retries, so a double submit is a replay, not a
   * version conflict.
   */
  idempotencyKey?: string;
}

/** The releases client: one per API origin. */
export interface ReleaseClient {
  /** The project's releases, newest first. */
  list(projectId: string): Promise<Release[]>;
  /** One release by id; 404 carries the existence-hiding envelope. */
  get(projectId: string, releaseId: string): Promise<Release>;
  /**
   * Create the project's next immutable release snapshot of main's
   * current accepted state (owner/maintainer — the API enforces it).
   */
  create(projectId: string, input: CreateReleaseInput): Promise<Release>;
  /** The release's canonical manifest document (parsed JSON). */
  manifest(projectId: string, releaseId: string): Promise<unknown>;
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

/** Options for createReleasesClient. */
export interface ReleasesClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /**
   * The session-bound CSRF token for the create (the API's guard demands
   * it on every state change). The module stays import-free, so the
   * caller supplies the source — the page reads lib/auth's
   * sessionTokenStorage.
   */
  csrfToken?: () => string | null;
}

const CSRF_HEADER = "X-CSRF-Token";
const IDEMPOTENCY_HEADER = "Idempotency-Key";

/** Build the releases client for one API origin. */
export function createReleasesClient(
  apiBaseUrl: string,
  options: ReleasesClientOptions = {},
): ReleaseClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /** One JSON call against the API; the CSRF token (when known) heads every call. */
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
      parsed = null; // non-JSON bodies (e.g. a proxy error page)
    }
    return { status: res.status, body: parsed };
  }

  /** Throw ApiError for any non-2xx, decoding the stable envelope. */
  function fail(status: number, body: unknown): never {
    if (typeof body === "object" && body !== null && "code" in body) {
      throw new ApiError(status, body as ErrorEnvelope);
    }
    throw new ApiError(status, {
      code: "UNKNOWN",
      message: "the server answered unexpectedly; try again",
    });
  }

  function asRelease(body: unknown): Release {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("project_id" in body) || typeof body.project_id !== "string" ||
      !("version" in body) || typeof body.version !== "string" ||
      !("title" in body) || typeof body.title !== "string" ||
      !("state_id" in body) || typeof body.state_id !== "string" ||
      !("manifest_hash" in body) || typeof body.manifest_hash !== "string" ||
      !("created_by" in body) || typeof body.created_by !== "string" ||
      !("created_at" in body) || typeof body.created_at !== "string"
    ) {
      throw new Error("unexpected release response shape");
    }
    return body as Release;
  }

  return {
    async list(projectId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/releases`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("releases" in body) || !Array.isArray(body.releases)
      ) {
        throw new Error("unexpected release list response shape");
      }
      return body.releases.map(asRelease);
    },
    async get(projectId, releaseId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(releaseId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asRelease(result.body);
    },
    async create(projectId, input) {
      const result = await call(
        "POST",
        `/api/v1/projects/${encodeURIComponent(projectId)}/releases`,
        { version: input.version, title: input.title },
        input.idempotencyKey,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asRelease(result.body);
    },
    async manifest(projectId, releaseId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(releaseId)}/manifest`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return result.body;
    },
  };
}

/** Roles the matrix lets create releases (ActionCreateRelease). */
export function mayCreateRelease(role: string | null): boolean {
  return role === "owner" || role === "maintainer";
}

/** Human-facing message for a stable release API code. */
export function messageForReleaseCode(code: string): string {
  switch (code) {
    case "RELEASE_PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "RELEASE_NOT_FOUND":
      return "This release does not exist.";
    case "RELEASE_FORBIDDEN":
      return "Only owners and maintainers can create releases.";
    case "RELEASE_VERSION_TAKEN":
      return "This project already has a release with that version. Choose another version.";
    case "RELEASE_NO_MAIN_STATE":
      return "The project's main branch has no accepted state to release yet.";
    case "RELEASE_GATE_BLOCKED":
      return "The release gate refused this snapshot. Fix the blocking checks and try again.";
    case "VALIDATION_FAILED":
      return "Check the version (letters, digits, dots, dashes, underscores) and title.";
    case "SERVICE_UNAVAILABLE":
      return "Release data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while working with releases. Please try again.";
  }
}

/** The manifest export URL of one release (a plain anchor download). */
export function releaseManifestHref(apiBaseUrl: string, projectId: string, releaseId: string): string {
  return (
    `${apiBaseUrl}/api/v1/projects/${encodeURIComponent(projectId)}` +
    `/releases/${encodeURIComponent(releaseId)}/manifest`
  );
}
