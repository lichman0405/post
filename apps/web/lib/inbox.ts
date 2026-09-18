/**
 * Client-side research inbox for the Go API (T1003).
 *
 * GET  /api/v1/inbox            one page of aggregated notification entries
 * POST /api/v1/inbox/read       mark the entries the caller read
 * POST /api/v1/inbox/read-all   mark what the inbox is serving read
 *
 * An ENTRY is not a notification: it is the deliveries of one target and
 * one event type inside one clock-hour window, rendered as one row with a
 * count (internal/events/inbox.go). That is what keeps a project's
 * per-commit traffic from becoming a list of forty rows — the count is
 * how the list says "and 39 more" without hiding any of them. The page
 * never re-derives the aggregation: `count`, `unread` and the window come
 * from the API, which is the one place that knows how entries are formed.
 *
 * Read state travels the same way. `latest_delivery_id` is the newest
 * delivery the page was shown, and marking read names it: the API marks
 * that entry up to that delivery, so a notification that arrived after
 * the page was rendered stays unread. The client therefore marks and then
 * re-reads — recomputing the badge locally would be a second
 * implementation of a rule the API already owns.
 *
 * Every call carries the session cookie (credentials: include); the two
 * writes also head the session-bound CSRF token (docs/22), which the
 * caller supplies from lib/auth's sessionTokenStorage.
 *
 * The module is import-free by construction (like lib/milestones.ts), so
 * the node:test suite (inbox.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** The wire entry shape (cmd/api/inboxhttp inboxEntryPayload). */
export interface InboxEntry {
  /** project | asset | user | organization | knowledge. */
  target_type: string;
  target_id: string;
  /**
   * The target's display name, resolved by the API when the inbox is read.
   *
   * "" only when the API has no naming query for that target KIND yet (see
   * entryTargetLabel for what to render instead). It never means "you may
   * not read this target": an entry whose target the caller may no longer
   * read is not in the page at all — no name, no id, no count, not in the
   * badge — so a client that reads "" as "no access" would state something
   * about a row it was never sent.
   */
  target_label: string;
  /** The canonical event type (specs/events/event-types.yaml). */
  event_type: string;
  /** RFC 3339 — the start of the clock-hour window the entry aggregates. */
  window_start: string;
  /** How many deliveries this one entry stands for. */
  count: number;
  /** How many of them are unread. */
  unread: number;
  /** unread === 0, as the API computes it. */
  read: boolean;
  first_at: string;
  last_at: string;
  latest_event_id: string;
  /** The anchor a mark-read call names (the newest delivery shown). */
  latest_delivery_id: string;
  /** The subject's route, or "" for a target with no page yet. */
  url: string;
}

/** One page of the inbox (GET /api/v1/inbox). */
export interface InboxPage {
  entries: InboxEntry[];
  /** The badge: entries holding anything unread, over the whole inbox. */
  unreadCount: number;
  /** The view that was served: "unread" or "all". */
  filter: string;
}

/** The two views the API serves (internal/events/inbox.go). */
export const INBOX_FILTER_UNREAD = "unread";
export const INBOX_FILTER_ALL = "all";

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

/** The inbox client: one per API origin. */
export interface InboxClient {
  /**
   * One page of the caller's inbox, newest entry first. filter defaults
   * to the unread view; limit is the page size the API clamps.
   */
  list(filter?: string, limit?: number): Promise<InboxPage>;
  /**
   * Mark the named entries read. The ids are the entries'
   * latest_delivery_id values — the anchors the API marks up to.
   * Returns how many deliveries were marked; an empty list is a no-op
   * that calls nothing.
   */
  markRead(deliveryIds: string[]): Promise<number>;
  /**
   * Mark what the inbox is serving read — every unread entry the API
   * would show the caller, and nothing else: an entry whose target the
   * caller may no longer read is not marked, so it stays unread and comes
   * back unread if access returns. Returns how many deliveries were
   * marked, so a button's "cleared" line counts what was really cleared.
   */
  markAllRead(): Promise<number>;
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

/** Options for createInboxClient. */
export interface InboxClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /**
   * The session-bound CSRF token for the two writes (the API's guard
   * demands it on every state change). The module stays import-free, so
   * the caller supplies the source — the page reads lib/auth's
   * sessionTokenStorage.
   */
  csrfToken?: () => string | null;
}

const CSRF_HEADER = "X-CSRF-Token";

/** Build the inbox client for one API origin. */
export function createInboxClient(
  apiBaseUrl: string,
  options: InboxClientOptions = {},
): InboxClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /** One JSON call against the API; the CSRF token (when known) heads every call. */
  async function call(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = {};
    const csrf = options.csrfToken?.() ?? null;
    if (csrf !== null) headers[CSRF_HEADER] = csrf;
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

  /** One entry, shape-checked: a page that renders half an entry lies. */
  function asEntry(body: unknown): InboxEntry {
    if (
      typeof body !== "object" || body === null ||
      !("target_type" in body) || typeof body.target_type !== "string" ||
      !("target_id" in body) || typeof body.target_id !== "string" ||
      !("event_type" in body) || typeof body.event_type !== "string" ||
      !("window_start" in body) || typeof body.window_start !== "string" ||
      !("count" in body) || typeof body.count !== "number" ||
      !("unread" in body) || typeof body.unread !== "number" ||
      !("latest_delivery_id" in body) || typeof body.latest_delivery_id !== "string"
    ) {
      throw new Error("unexpected inbox entry shape");
    }
    return body as InboxEntry;
  }

  /** The read count the two write routes answer with. */
  async function markedCount(
    path: string,
    body: unknown | undefined,
  ): Promise<number> {
    const result = await call("POST", path, body);
    if (result.status >= 400) fail(result.status, result.body);
    const payload = result.body;
    if (
      typeof payload !== "object" || payload === null ||
      !("read" in payload) || typeof payload.read !== "number"
    ) {
      throw new Error("unexpected mark-read response shape");
    }
    return payload.read;
  }

  return {
    async list(filter = INBOX_FILTER_UNREAD, limit) {
      const params = new URLSearchParams({ filter });
      if (limit !== undefined) params.set("limit", String(limit));
      const result = await call("GET", `/api/v1/inbox?${params.toString()}`);
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("entries" in body) || !Array.isArray(body.entries) ||
        !("unread_count" in body) || typeof body.unread_count !== "number" ||
        !("filter" in body) || typeof body.filter !== "string"
      ) {
        throw new Error("unexpected inbox response shape");
      }
      return {
        entries: body.entries.map(asEntry),
        unreadCount: body.unread_count,
        filter: body.filter,
      };
    },
    async markRead(deliveryIds) {
      // An empty selection is what the API would answer 0 to; calling it
      // would be a request that can only say what the caller already
      // knows, and the caller's UI should not offer the action at all.
      if (deliveryIds.length === 0) return 0;
      return markedCount("/api/v1/inbox/read", { deliveries: deliveryIds });
    },
    async markAllRead() {
      return markedCount("/api/v1/inbox/read-all", undefined);
    },
  };
}

/** Human-facing message for a stable inbox API code. */
export function messageForInboxCode(code: string): string {
  switch (code) {
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to read your inbox.";
    case "INBOX_ENTRY_NOT_FOUND":
      return "That notification is no longer in your inbox. Reload the page.";
    case "VALIDATION_FAILED":
      return "The inbox request was rejected. Reload the page and try again.";
    case "SERVICE_UNAVAILABLE":
      return "Inbox data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while reading the inbox. Please try again.";
  }
}

/**
 * The display name of one canonical event type (specs/events/event-types.yaml).
 * An unknown type renders RAW — the event's own name is the truth, and a
 * made-up label for it would be the page inventing product semantics.
 */
const EVENT_TYPE_NAMES: Record<string, string> = {
  "project.created": "Project created",
  "project.visibility_changed": "Project visibility changed",
  "project.main_frozen": "Main frozen",
  "branch.created": "Branch created",
  "branch.aborted": "Branch aborted",
  "state.committed": "Commit",
  "pull_request.opened": "Pull request opened",
  "pull_request.reviewed": "Pull request reviewed",
  "pull_request.merged": "Pull request merged",
  "scientific_object.version_created": "Object version created",
  "scientific_object.aborted": "Object aborted",
  "scientific_object.reopened": "Object reopened",
  "evidence_assertion.created": "Evidence assertion recorded",
  "release.published": "Release published",
  "research_asset.version_published": "Asset version published",
  "knowledge.version_published": "Knowledge published",
  "knowledge.external_evidence_added": "External evidence added",
  "dependency.status_changed": "Dependency status changed",
  "dependency.impact_detected": "Dependency impact detected",
  "contribution.accepted": "Contribution accepted",
  "credit.dispute_opened": "Credit dispute opened",
  "credit.dispute_resolved": "Credit dispute resolved",
  "rights.visibility_changed": "Rights visibility changed",
  "policy.version_published": "Policy published",
  "webhook.delivery_failed": "Webhook delivery failed",
};

/** The display name of one event type; unknown types render raw. */
export function eventTypeDisplayName(eventType: string): string {
  return EVENT_TYPE_NAMES[eventType] ?? eventType;
}

/**
 * The aggregation badge of one entry: how many notifications it stands
 * for, or null when it stands for one.
 *
 * The threshold is 2 because that is where the aggregation becomes
 * meaningful: a row that says "1 event" beside every notification reads
 * as noise, while a row that says "40 events" is the whole reason the
 * inbox exists in this shape. The number is rendered as the API counted
 * it — never recomputed here.
 */
export function aggregationLabel(count: number): string | null {
  if (!Number.isFinite(count) || count < 2) return null;
  return `${count} events`;
}

/**
 * What a row calls its subject.
 *
 * target_label is "" only when the API has no naming query for that target
 * KIND (knowledge, until it has a surface) — never because the caller may
 * not read the target: such an entry is not in the page at all, so this
 * fallback is reached for rows the caller IS being served. A row that
 * rendered nothing there would look broken, so the fallback names the
 * target's kind and a short id.
 *
 * Do not render an "access denied" state on "" — the row would be claiming
 * the caller was shown something they cannot see, which the API cannot do.
 */
export function entryTargetLabel(entry: InboxEntry): string {
  if (entry.target_label !== "") return entry.target_label;
  const short = entry.target_id.length > 8 ? entry.target_id.slice(0, 8) : entry.target_id;
  return `${entry.target_type} ${short}`;
}

/**
 * One entry's window as a row renders it: the clock hour, UTC.
 *
 * The window is a UTC hour by construction — the API bins deliveries with
 * date_bin from the epoch — so it is labelled UTC rather than silently
 * shifted into the viewer's timezone: two rows claiming different hours
 * for the same deliveries would be one of them lying. An unparseable
 * timestamp renders "" (the row shows no window rather than "Invalid
 * Date").
 */
export function windowLabel(windowStart: string): string {
  const at = new Date(windowStart);
  if (Number.isNaN(at.getTime())) return "";
  const start = at.toISOString().slice(0, 13).replace("T", " ") + ":00";
  const end = new Date(at.getTime() + 3_600_000).toISOString().slice(11, 13);
  return `${start}–${end}:00 UTC`;
}
