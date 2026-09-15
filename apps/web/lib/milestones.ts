/**
 * Client-side milestones client for the Go API (T0609).
 *
 * Reads and records the project's milestone timeline:
 * GET  /api/v1/projects/{id}/milestones
 * POST /api/v1/projects/{id}/milestones
 * GET  /api/v1/projects/{id}/milestones/{milestoneId}
 * A milestone is a dated fact on the research timeline (candidate
 * selected, paper submitted, patent filed, external validation, or a
 * custom label) — it is NOT a project lifecycle state, so there is
 * deliberately no completed milestone kind and no update/delete surface.
 * The list arrives in research order (occurred_at ascending with
 * creation-order tie-breaks); the API owns that order and the page
 * renders it as-is.
 * All calls carry the session cookie (credentials: include); the create
 * also heads the session-bound CSRF token and an Idempotency-Key (the
 * caller supplies it and keeps it until the create succeeds, so a retry
 * replays instead of duplicating a timeline entry — docs/22).
 *
 * The module is import-free by construction (like lib/projects.ts), so
 * the node:test suite (milestones.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** The wire milestone shape (cmd/api/milestonehttp milestonePayload). */
export interface Milestone {
  id: string;
  project_id: string;
  kind: string;
  /** The custom label; null for canonical kinds without one. */
  label: string | null;
  /** RFC 3339 — the milestone's position on the research timeline. */
  occurred_at: string;
  /** The optional release link; null when the milestone names no release. */
  release_id: string | null;
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

/** The milestone-create input the POST body renders from. */
export interface CreateMilestoneInput {
  /** One of the canonical kinds or "custom" (custom requires a label). */
  kind: string;
  /** The custom label; optional for the canonical kinds. */
  label?: string;
  /** RFC 3339 — the milestone's date on the research timeline. */
  occurredAt: string;
  /** The optional release link (must resolve in the same project). */
  releaseId?: string;
  /**
   * The Idempotency-Key for this create attempt (docs/22): the same key
   * replays the create it names. The page generates one per fresh form
   * and keeps it across retries, so a double submit is a replay, not a
   * duplicate timeline entry.
   */
  idempotencyKey?: string;
}

/** The milestones client: one per API origin. */
export interface MilestoneClient {
  /**
   * The project's milestone timeline in research order (occurred_at
   * ascending) — the API's ordering, never re-sorted client-side.
   */
  list(projectId: string): Promise<Milestone[]>;
  /** One milestone by id; 404 carries the existence-hiding envelope. */
  get(projectId: string, milestoneId: string): Promise<Milestone>;
  /**
   * Record a milestone (owner/maintainer — the API enforces it via the
   * same governance action as release creation).
   */
  create(projectId: string, input: CreateMilestoneInput): Promise<Milestone>;
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

/** Options for createMilestonesClient. */
export interface MilestonesClientOptions {
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

/** The canonical milestone kinds and their display names (internal/domain). */
export const MILESTONE_KINDS = [
  { kind: "candidate_selected", name: "Candidate selected" },
  { kind: "paper_submitted", name: "Paper submitted" },
  { kind: "patent_filed", name: "Patent filed" },
  { kind: "external_validation", name: "External validation" },
  { kind: "custom", name: "Custom" },
] as const;

/** The display name of one milestone kind; unknown kinds render raw. */
export function kindDisplayName(kind: string): string {
  const known = MILESTONE_KINDS.find((entry) => entry.kind === kind);
  return known === undefined ? kind : known.name;
}

/**
 * The row label of one milestone: the custom label when present (the
 * label IS the marker's name), otherwise the kind's display name.
 */
export function milestoneDisplayName(kind: string, label: string | null): string {
  if (label !== null && label !== "") return label;
  return kindDisplayName(kind);
}

/** Whether the kind is one the API accepts (drives the form's select). */
export function isKnownMilestoneKind(kind: string): boolean {
  return MILESTONE_KINDS.some((entry) => entry.kind === kind);
}

/** Roles the matrix lets record milestones (ActionCreateRelease, reused). */
export function mayCreateMilestone(role: string | null): boolean {
  return role === "owner" || role === "maintainer";
}

/** Human-facing message for a stable milestone API code. */
export function messageForMilestoneCode(code: string): string {
  switch (code) {
    case "MILESTONE_PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "MILESTONE_NOT_FOUND":
      return "This milestone does not exist.";
    case "MILESTONE_RELEASE_NOT_FOUND":
      return "The release you linked does not exist in this project.";
    case "MILESTONE_FORBIDDEN":
      return "Only owners and maintainers can record milestones.";
    case "MILESTONE_VALIDATION_FAILED":
      return "Check the kind, the label and the occurred-at date.";
    case "SERVICE_UNAVAILABLE":
      return "Milestone data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while working with milestones. Please try again.";
  }
}

/** Build the milestones client for one API origin. */
export function createMilestonesClient(
  apiBaseUrl: string,
  options: MilestonesClientOptions = {},
): MilestoneClient {
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

  function asMilestone(body: unknown): Milestone {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("project_id" in body) || typeof body.project_id !== "string" ||
      !("kind" in body) || typeof body.kind !== "string" ||
      !("occurred_at" in body) || typeof body.occurred_at !== "string" ||
      !("created_by" in body) || typeof body.created_by !== "string" ||
      !("created_at" in body) || typeof body.created_at !== "string"
    ) {
      throw new Error("unexpected milestone response shape");
    }
    return body as Milestone;
  }

  return {
    async list(projectId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/milestones`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("milestones" in body) || !Array.isArray(body.milestones)
      ) {
        throw new Error("unexpected milestone list response shape");
      }
      return body.milestones.map(asMilestone);
    },
    async get(projectId, milestoneId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/milestones/${encodeURIComponent(milestoneId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asMilestone(result.body);
    },
    async create(projectId, input) {
      const result = await call(
        "POST",
        `/api/v1/projects/${encodeURIComponent(projectId)}/milestones`,
        {
          kind: input.kind,
          label: input.label,
          occurred_at: input.occurredAt,
          release_id: input.releaseId,
        },
        input.idempotencyKey,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asMilestone(result.body);
    },
  };
}
