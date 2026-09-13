/**
 * Client-side Activity (audit log) client for the Go API (T0110).
 *
 * Reads the member-only Activity feeds (GET /api/v1/projects/{id}/activity
 * and /api/v1/organizations/{id}/activity, keyset-paginated on
 * (occurred_at, id)). Both are read-only by contract — the API answers 405
 * for any other verb — so no CSRF token is involved; the session cookie
 * rides along with credentials: "include" and the feed is exactly as
 * visible as the resource itself (member-only, existence-hiding 404).
 *
 * The module is import-free by construction (like lib/auth.ts and
 * lib/profile.ts), so the node:test suite (activity.test.mjs) runs it
 * through Node's type stripping. The ApiError shape deliberately mirrors
 * lib/auth.ts.
 */

/** One audit row as the wire renders it
 *  (cmd/api/audithttp activityEntryPayload). Summaries and metadata are
 *  raw jsonb — rendered, never parsed. */
export interface AuditEntry {
  id: string;
  actor_id: string | null;
  actor_handle: string | null;
  actor_display_name: string | null;
  via: string;
  action: string;
  target_ref: string | null;
  project_id: string | null;
  organization_id: string | null;
  correlation_id: string;
  before_summary?: unknown;
  after_summary?: unknown;
  metadata?: unknown;
  occurred_at: string;
}

/** One page of a feed: newest first, plus the opaque next-page cursor
 *  (null on the last page). */
export interface ActivityPage {
  entries: AuditEntry[];
  next_cursor: string | null;
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
 * codes it knows and a generic line otherwise — never raw transport errors.
 * (Same shape as lib/auth.ts ApiError.)
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

/** One page request: an opaque server cursor and/or a page size. */
export interface ActivityQuery {
  limit?: number;
  cursor?: string;
}

/** The Activity client: one per API origin. */
export interface ActivityClient {
  /** The project's audit feed, newest first, member-only. */
  projectActivity(projectId: string, query?: ActivityQuery): Promise<ActivityPage>;
  /** The organization's audit feed, newest first, member-only. */
  orgActivity(orgId: string, query?: ActivityQuery): Promise<ActivityPage>;
}

type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    credentials?: string;
  },
) => Promise<{ status: number; json(): Promise<unknown> }>;

/** Options for createActivityClient. */
export interface ActivityClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/** Build the Activity client for one API origin. */
export function createActivityClient(
  apiBaseUrl: string,
  options: ActivityClientOptions = {},
): ActivityClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /** Throw ApiError for any non-2xx, decoding the stable envelope. */
  function fail(status: number, body: unknown): never {
    if (typeof body === "object" && body !== null && "code" in body) {
      throw new ApiError(status, body as { code: string; message: string });
    }
    throw new ApiError(status, {
      code: "UNKNOWN",
      message: "the server answered unexpectedly; try again",
    });
  }

  function asEntry(body: unknown): AuditEntry {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("via" in body) || typeof body.via !== "string" ||
      !("action" in body) || typeof body.action !== "string" ||
      !("correlation_id" in body) || typeof body.correlation_id !== "string" ||
      !("occurred_at" in body) || typeof body.occurred_at !== "string"
    ) {
      throw new Error("unexpected activity entry shape");
    }
    return body as AuditEntry;
  }

  async function fetchPage(path: string): Promise<ActivityPage> {
    const res = await fetchFn(apiBaseUrl + path, { credentials: "include" });
    if (res.status < 200 || res.status >= 300) {
      // Any non-2xx (a 3xx without a followable location too) surfaces as
      // a stable ApiError, never as a shape error on the body parse below.
      let parsed: unknown = null;
      try {
        parsed = await res.json();
      } catch {
        parsed = null;
      }
      fail(res.status, parsed);
    }
    const body = await res.json();
    if (
      typeof body !== "object" || body === null ||
      !("entries" in body) || !Array.isArray(body.entries) ||
      !("next_cursor" in body) ||
      (body.next_cursor !== null && typeof body.next_cursor !== "string")
    ) {
      throw new Error("unexpected activity response shape");
    }
    return {
      entries: body.entries.map(asEntry),
      next_cursor: body.next_cursor as string | null,
    };
  }

  /** One feed URL: the scope path plus the optional page query. */
  function feedURL(base: string, query?: ActivityQuery): string {
    const params: string[] = [];
    if (query?.limit !== undefined) params.push(`limit=${query.limit}`);
    if (query?.cursor) params.push(`cursor=${encodeURIComponent(query.cursor)}`);
    return params.length === 0 ? base : `${base}?${params.join("&")}`;
  }

  return {
    projectActivity: (projectId, query) =>
      fetchPage(feedURL(`/api/v1/projects/${encodeURIComponent(projectId)}/activity`, query)),
    orgActivity: (orgId, query) =>
      fetchPage(feedURL(`/api/v1/organizations/${encodeURIComponent(orgId)}/activity`, query)),
  };
}

/** Human-facing message for a stable Activity API code. */
export function messageForAuditCode(code: string): string {
  switch (code) {
    case "PROJECT_NOT_FOUND":
      return "This project is not visible to you.";
    case "ORG_NOT_FOUND":
      return "This organization is not visible to you.";
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to view this activity.";
    case "VALIDATION_FAILED":
      return "The page request is not valid; start from the beginning of the feed.";
    case "METHOD_NOT_ALLOWED":
      return "The audit log is read-only.";
    case "SERVICE_UNAVAILABLE":
      return "Activity is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong. Please try again.";
  }
}
