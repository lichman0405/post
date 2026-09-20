/**
 * Client-side Activity (audit log) client for the Go API (T0110, extended
 * by T0607).
 *
 * Reads the member-only Activity feeds (GET /api/v1/projects/{id}/activity
 * and /api/v1/organizations/{id}/activity, keyset-paginated on
 * (occurred_at, id)). Both are read-only by contract — the API answers 405
 * for any other verb — so no CSRF token is involved; the session cookie
 * rides along with credentials: "include" and the feed is exactly as
 * visible as the resource itself (member-only, existence-hiding 404).
 *
 * T0607 adds the two things the page needs on top of the wire shape: the
 * SOURCE filter (a project feed reads the governance registry, the research
 * event registry, or both — ?source=), and the presentation helpers below.
 * The helpers are pure functions of one entry so the rendering rules can be
 * unit-tested without a browser, and so the page has no second copy of them.
 *
 * Import discipline: the module is import-free by construction (like
 * lib/inbox.ts and lib/milestones.ts), so the node:test suite
 * (activity.test.mjs) runs it through Node's type stripping. The one
 * registry it therefore cannot own — the research EVENT vocabulary, which
 * lib/inbox.ts owns — arrives as an argument to activityTitle rather than
 * as an import, because the two vocabularies are separate lists:
 * internal/domain/audit.go is the audit vocabulary and
 * specs/events/event-types.yaml is the event vocabulary, and the domain
 * file records that the names they spell alike are "a coincidence of two
 * separate registries, not a shared identity ... neither list is derived
 * from the other".
 */

/** Which registry a row was read from (cmd/api/audithttp). */
export type ActivitySource = "governance" | "research";

/** One Activity row as the wire renders it
 *  (cmd/api/audithttp activityEntryPayload). Summaries, metadata and the
 *  research row's payload are raw jsonb — rendered, never parsed. */
export interface AuditEntry {
  id: string;
  source: ActivitySource;
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
  /** The event body of a research row; absent on a governance row. */
  payload?: unknown;
  /** The visibility a research row was recorded with; "" on governance. */
  visibility?: string;
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

/** One page request: the registry filter, an opaque server cursor and/or a
 *  page size. */
export interface ActivityQuery {
  limit?: number;
  cursor?: string;
  /** Which registry to read; absent reads both (the API's contract). */
  source?: ActivitySource;
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
      !("occurred_at" in body) || typeof body.occurred_at !== "string" ||
      // source is required, and validated: every rendering rule below
      // branches on it, so an entry without one (or with a third value)
      // would be rendered by whichever branch the code fell into. Refuse
      // the row instead of guessing which registry it came from.
      !("source" in body) ||
      (body.source !== "governance" && body.source !== "research")
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
    if (query?.source) params.push(`source=${query.source}`);
    return params.length === 0 ? base : `${base}?${params.join("&")}`;
  }

  return {
    projectActivity: (projectId, query) =>
      fetchPage(feedURL(`/api/v1/projects/${encodeURIComponent(projectId)}/activity`, query)),
    // The organization feed has one source — governance (research events
    // are project-scoped, so the API refuses source=research there) — and
    // this client therefore offers no filter for it: a parameter that can
    // only hold one value is not a filter.
    orgActivity: (orgId, query) =>
      fetchPage(
        feedURL(
          `/api/v1/organizations/${encodeURIComponent(orgId)}/activity`,
          query === undefined ? undefined : { limit: query.limit, cursor: query.cursor },
        ),
      ),
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

/* -------------------------------------------------------------------------
 * Presentation (T0607).
 *
 * Everything below is a pure function of one entry, so the rendering rules
 * are unit-tested (activity.test.mjs) rather than only observed in a
 * browser. None of them invents a name the platform does not use:
 *
 *   - an action/event type the maps below do not know renders RAW. The
 *     dotted name is the platform's own vocabulary and a page that made up
 *     a friendlier label for an unknown action would be inventing product
 *     semantics (lib/inbox.ts's rule, which this file follows);
 *   - a fact the row does not carry renders nothing at all. An empty
 *     "Explanation" line would say "this abort has no explanation", which
 *     is a different claim from "this row does not carry one" — and the
 *     research event of an abort genuinely does not (the free text lives on
 *     the version row and the audit row, internal/application/aborts/events.go);
 *   - a link is emitted only where a page exists (lib/feeds' rule). Target
 *     refs name a release, an object, a project, an organization or a user;
 *     only the release and the user have a page in this app, and the object
 *     ref (the abort's target) renders as text with its short id.
 * ---------------------------------------------------------------------- */

/** The registry filters the page offers, in the order the chips render. */
export const ACTIVITY_SOURCES: readonly ActivitySource[] = ["governance", "research"];

/** The chip label of one filter; null is the unfiltered ("All") chip. */
export function activityFilterLabel(source: ActivitySource | null): string {
  switch (source) {
    case "governance":
      return "Governance";
    case "research":
      return "Research events";
    default:
      return "All";
  }
}

/**
 * The label of ONE audit action. This table IS the audit vocabulary as
 * display names — internal/domain/audit.go's Action* constants, plus the
 * package-local ones that record rows through the same store:
 * internal/application/forks (project.forked),
 * internal/application/assetpublish (asset.version_published),
 * cmd/api/gittokenshttp (git.token_*), internal/gitprovider (the
 * reconciler's drift finding) and internal/contribution (opportunity
 * transitions). Each label is a plain reading of the platform's own dotted
 * name, never a new claim about it.
 *
 * Two things are deliberately NOT here:
 *
 *   - Names shared with the event vocabulary are NOT left out and NOT
 *     resolved through it. A governance row's title comes from THIS table:
 *     audit_log.action is an audit action, and lookup through the event
 *     registry would make an audit label change when an event label does.
 *     The labels that coincide (project.created, project.main_frozen,
 *     pull_request.merged, scientific_object.aborted,
 *     knowledge.version_published) agree with lib/inbox.ts's, and
 *     activity.test.mjs pins that agreement — a test, not a runtime
 *     derivation.
 *   - scientific_object.reopened is absent because no audit row writes it:
 *     the reopen governance surface is T0610, and today only the EVENT
 *     exists (specs/events/event-types.yaml:23, which internal/application/
 *     aborts names as "belong[ing] to T0610; nothing in this package emits
 *     it"). A governance row with that action therefore renders its raw
 *     dotted name until the action exists to be labelled. Guessing a
 *     constant T0610 has not written would put a name in the table that
 *     the vocabulary does not have.
 */
const AUDIT_ACTION_NAMES: Record<string, string> = {
  "auth.account.signup": "Account created",
  "auth.login.success": "Signed in",
  "auth.login.failed": "Sign-in failed",
  "auth.logout": "Signed out",
  "org.created": "Organization created",
  "org.updated": "Organization updated",
  "org.deactivated": "Organization deactivated",
  "org.member.invited": "Member invited",
  "org.member.updated": "Member updated",
  "org.member.removed": "Member removed",
  "project.created": "Project created",
  "project.forked": "Project forked",
  "project.member_role_changed": "Member role changed",
  "project.settings_updated": "Settings updated",
  "project.main_frozen": "Main frozen",
  "project.schema_profile_registered": "Schema profile registered",
  "project.research_owner_rule_created": "Research owner rule added",
  "project.research_owner_rule_deleted": "Research owner rule removed",
  "project.responsibility_assigned": "Responsibility assigned",
  "project.responsibility_unassigned": "Responsibility removed",
  "policy.version_set": "Policy version set",
  "release.created": "Release created",
  "conflict.resolution_saved": "Conflict resolution saved",
  "pull_request.merged": "Pull request merged",
  "pull_request.review_completed": "Required reviews met",
  "milestone.created": "Milestone created",
  "asset.rights_holder_changed": "Rights holder changed",
  "asset.version_published": "Asset version published",
  "knowledge.version_published": "Knowledge published",
  "scientific_object.aborted": "Object aborted",
  "git.token_issued": "Git credential issued",
  "git.token_revoked": "Git credential revoked",
  "git.access_revoked": "Git access revoked",
  "git.reconciliation.drift_detected": "Drift detected",
  "contribution.opportunity.marked": "Opportunity marked",
  "contribution.opportunity.suggested": "Opportunity suggested",
  "contribution.opportunity.approved": "Opportunity approved",
  "contribution.opportunity.rejected": "Opportunity rejected",
  "contribution.opportunity.closed": "Opportunity closed",
  "contribution.opportunity.publicized": "Opportunity publicized",
};

/** The title of one row, by the registry the row came from (not by which
 *  table happens to have a label for it):
 *
 *    governance  this module's audit vocabulary, or the raw dotted name
 *                when the vocabulary does not have it;
 *    research    the event vocabulary, through the eventLabel function the
 *                caller supplies (lib/inbox.ts's eventTypeDisplayName) —
 *                a research row's action IS an event type;
 *    either      a name no registry knows renders raw. The dotted name is
 *                the platform's own word for the act, and a page that
 *                coined a friendlier label for an unknown action would be
 *                putting a meaning in the platform's mouth.
 */
export function activityTitle(
  entry: AuditEntry,
  eventLabel: (eventType: string) => string,
): string {
  if (entry.source === "research") return eventLabel(entry.action);
  return AUDIT_ACTION_NAMES[entry.action] ?? entry.action;
}

/**
 * The rendering family of one row. Only the families with rendering of
 * their own are named: an abort and a reopen show the reason they carry, a
 * release shows the version it fixed, everything else is a plain line.
 * Aborted branches (branch.aborted) are deliberately NOT the abort family —
 * that is a discarded proposal branch, not a retracted research object, and
 * giving them one rendering would say the two are the same act.
 */
export type ActivityFamily = "abort" | "reopen" | "release" | "other";

/** The family of one row, by its action / event type. */
export function activityFamily(entry: AuditEntry): ActivityFamily {
  switch (entry.action) {
    case "scientific_object.aborted":
      return "abort";
    case "scientific_object.reopened":
      return "reopen";
    case "release.created":
    case "release.published":
      return "release";
    default:
      return "other";
  }
}

/** The actor's display name, or their handle, or a stated absence. */
export function activityActorName(entry: AuditEntry): string {
  if (entry.actor_display_name) return entry.actor_display_name;
  if (entry.actor_handle) return entry.actor_handle;
  return "Unknown actor";
}

/**
 * The actor's profile link. Null when the row names no actor — an
 * unauthenticated event (a sign-in attempt for an address with no account)
 * is a row ABOUT nobody, and linking it to the reader's own profile or to
 * the project would be a lie about who acted.
 */
export function activityActorHref(entry: AuditEntry): string | null {
  if (!entry.actor_id) return null;
  return `/users/${encodeURIComponent(entry.actor_id)}`;
}

/**
 * The channel the row arrived through, in words. The two vocabularies are
 * the platform's two: an audit row records HOW the action reached the API
 * (domain.RequestInfo: session / password / oidc / internal) while a
 * research event records the WRITE PATH that produced it
 * (domain.StateVia: web / api / mcp / claude_code / git_compat / system,
 * copied from the outbox row). An unknown value renders raw.
 */
const VIA_NAMES: Record<string, string> = {
  // audit vocabulary
  session: "Web session",
  password: "Password",
  oidc: "SSO",
  internal: "System",
  // research event vocabulary (state_commits.via)
  web: "Web UI",
  api: "API",
  mcp: "MCP",
  claude_code: "Claude Code",
  git_compat: "Git",
  system: "System",
};

/** The label of one channel value; unknown values render raw. */
export function viaLabel(via: string): string {
  return VIA_NAMES[via] ?? via;
}

/** One rendered fact of a row ("Reason code: contaminated"). */
export interface ActivityDetail {
  label: string;
  value: string;
}

/** The state facts of one row: the event body on a research row (the
 *  payload IS the event), the state it produced on a governance row. */
function activityFacts(entry: AuditEntry): Record<string, unknown> {
  const raw = entry.source === "research" ? entry.payload : entry.after_summary;
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return {};
  return raw as Record<string, unknown>;
}

/** One fact as a string; anything that is not a string or a finite number
 *  reads as absent, so a malformed payload renders nothing rather than
 *  "[object Object]". */
function factString(facts: Record<string, unknown>, key: string): string {
  const v = facts[key];
  if (typeof v === "string") return v;
  if (typeof v === "number" && Number.isFinite(v)) return String(v);
  return "";
}

/**
 * The facts a family renders under its title. Empty for "other" rows: their
 * summaries are not labelled, and a page that printed a raw jsonb object
 * would be showing the wire, not the history.
 *
 * What each family can carry, and from where (all of it read, none of it
 * inferred):
 *
 *   abort / reopen  reason_code and the identifying version, on BOTH
 *                   sources; the human explanation only on the governance
 *                   row, because the event's payload deliberately does not
 *                   carry the free text (aborts/events.go). A governance
 *                   abort also renders the lifecycle move it recorded
 *                   (before → after), which is the one transition the two
 *                   summaries hold.
 *   release         the version it fixed, on both sources, and the manifest
 *                   hash's short form.
 */
export function activityDetail(entry: AuditEntry): ActivityDetail[] {
  const facts = activityFacts(entry);
  const out: ActivityDetail[] = [];
  const push = (label: string, value: string) => {
    if (value !== "") out.push({ label, value });
  };

  switch (activityFamily(entry)) {
    case "abort":
    case "reopen": {
      push("Reason code", factString(facts, "reason_code"));
      // The version the transition recorded. Between the two sources the
      // key differs on purpose: the audit row writes version_no (the
      // version the abort produced) beside aborted_version_no, while the
      // event names the same pair version_no / aborted_version_no.
      const version = factString(facts, "version_no");
      push("Version", version);
      push("Explanation", factString(facts, "explanation"));
      const before = activityBeforeState(entry);
      push("State", before);
      const replacement = factString(facts, "replacement_ref");
      push("Replacement", replacement);
      return out;
    }
    case "release": {
      push("Version", factString(facts, "version"));
      const hash = factString(facts, "manifest_hash");
      if (hash !== "") out.push({ label: "Manifest", value: shortHash(hash) });
      return out;
    }
    default:
      return out;
  }
}

/** The lifecycle move a governance row recorded ("active → aborted"), or
 *  "" when the row is not a governance row or carries only one side. */
function activityBeforeState(entry: AuditEntry): string {
  if (entry.source !== "governance") return "";
  const before = asRecord(entry.before_summary);
  const after = asRecord(entry.after_summary);
  const from = factString(before, "lifecycle_state");
  const to = factString(after, "lifecycle_state");
  if (from === "" || to === "" || from === to) return "";
  return `${from} → ${to}`;
}

function asRecord(raw: unknown): Record<string, unknown> {
  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) return {};
  return raw as Record<string, unknown>;
}

/** The first 12 characters of a hash — the form the Releases tab renders,
 *  so one hash has one short form across the app. */
export function shortHash(hash: string): string {
  return hash.slice(0, 12);
}

/**
 * The target a row names, and the page that shows it (null when there is
 * no such page). Only two of the platform's target refs have one:
 *
 *   release:<id>  → /projects/{projectId}/releases/{id}
 *   user:<id>     → /users/{id}
 *
 * object:<id> (an abort's target), organization:<id> and project:<id> are
 * rendered as text: this app has no object page, no organization detail
 * page, and the project page is where the reader already is. A research row
 * has no target ref at all — and its PAYLOAD is never mined for one, for
 * the reason internal/application/feeds states: a link derived from payload
 * bytes re-implements, badly, the judgement about what the reader may see.
 */
export function activityTarget(projectId: string, entry: AuditEntry): { label: string; href: string | null } | null {
  const ref = entry.target_ref;
  if (!ref) return null;
  const sep = ref.indexOf(":");
  if (sep <= 0) return { label: ref, href: null };
  const kind = ref.slice(0, sep);
  const id = ref.slice(sep + 1);
  if (id === "") return { label: kind, href: null };
  switch (kind) {
    case "release":
      return { label: `Release ${id.slice(0, 8)}`, href: `/projects/${encodeURIComponent(projectId)}/releases/${encodeURIComponent(id)}` };
    case "user":
      return { label: `User ${id.slice(0, 8)}`, href: `/users/${encodeURIComponent(id)}` };
    case "object":
      return { label: `Object ${id.slice(0, 8)}`, href: null };
    case "project":
      return { label: `Project ${id.slice(0, 8)}`, href: null };
    case "organization":
      return { label: `Organization ${id.slice(0, 8)}`, href: null };
    default:
      return { label: ref, href: null };
  }
}

/** The visibility a research row was recorded with, or null on a governance
 *  row (an audit row has no visibility of its own — who may read it is the
 *  scope's read gate, the same gate for both sources). */
export function activityVisibility(entry: AuditEntry): string | null {
  if (entry.source !== "research") return null;
  const v = entry.visibility ?? "";
  return v === "" ? null : v;
}

/**
 * One row's instant, as the timeline renders it: "2026-09-12 10:00 UTC".
 *
 * UTC and labelled so, for the reason lib/inbox.ts states about its
 * windows: the instant is a UTC instant, and a page that shifted it into
 * the viewer's zone would have two rows claiming different hours for the
 * same change, or the server and the browser rendering different strings
 * for one row. An unparseable value renders as it arrived — the platform's
 * own text is at least true, where "" would tell the reader the row has no
 * instant.
 */
export function activityTimestamp(occurredAt: string): string {
  const at = new Date(occurredAt);
  if (Number.isNaN(at.getTime())) return occurredAt;
  const pad = (n: number) => String(n).padStart(2, "0");
  return (
    `${at.getUTCFullYear()}-${pad(at.getUTCMonth() + 1)}-${pad(at.getUTCDate())}` +
    ` ${pad(at.getUTCHours())}:${pad(at.getUTCMinutes())} UTC`
  );
}

/**
 * The source a ?source= value names, or null when the page should read
 * both. An unknown value is REFUSED (null is "absent", not "unknown"):
 * the API refuses a filter it does not have rather than answering a
 * subset, and a page that treated a mistyped value as "no filter" would
 * answer with everything while the reader asked for something.
 */
export function activitySourceFromParam(raw: string | null): ActivitySource | null {
  if (raw === null || raw === "") return null;
  if (raw === "governance" || raw === "research") return raw;
  throw new Error(`unknown activity source ${JSON.stringify(raw)}`);
}
