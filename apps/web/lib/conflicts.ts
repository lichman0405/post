/**
 * Client-side conflicts client for the Scientific Conflict Resolution UI
 * (T0407 over the T0405 detector + T0401 diff surfaces).
 *
 * Wraps the two calls the Conflicts page makes —
 *   GET /api/v1/projects/{id}/conflicts?base_state_id=&source_state_id=&target_state_id=
 *   PUT /api/v1/projects/{id}/resolutions   (the human's decisions)
 * — with the exact wire shapes cmd/api/conflicthttp produces/consumes
 * (snake_case; the report carries the three-way diff, so the base/source
 * (A)/target (B) values ride along in report.diff.object_changes).
 *
 * The write carries the session-bound CSRF token from the shared
 * "post.csrf" storage (the SAME key as lib/profile.ts / lib/auth.ts —
 * one token per tab, so the save is always accepted by the guard).
 * Authorization truth stays in the API: a 403 renders its message, the
 * page never guesses whether the caller may decide.
 *
 * The module is import-free by construction (like lib/auth.ts and
 * lib/files.ts), so the node:test suite (conflicts.test.mjs) runs it
 * through Node's type stripping. The ApiError shape mirrors lib/files.ts.
 */

/** The three-way triple every conflicts call is scoped by. */
export interface ConflictTriple {
  base_state_id: string;
  source_state_id: string;
  target_state_id: string;
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
 * (Same shape as lib/auth.ts / lib/files.ts ApiError.)
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

/** Where the CSRF token survives a reload (sessionStorage, per tab). */
export interface TokenStorage {
  get(): string | null;
  set(token: string): void;
  clear(): void;
}

/**
 * The shared CSRF storage key — MUST match lib/auth.ts: the token is
 * session-bound, one per tab, and every client reads/writes the same one.
 */
const STORAGE_KEY = "post.csrf";

/** TokenStorage over the browser's sessionStorage; falls back to memory. */
export function sessionTokenStorage(): TokenStorage {
  if (typeof window !== "undefined" && typeof window.sessionStorage !== "undefined") {
    return {
      get: () => window.sessionStorage.getItem(STORAGE_KEY),
      set: (token) => window.sessionStorage.setItem(STORAGE_KEY, token),
      clear: () => window.sessionStorage.removeItem(STORAGE_KEY),
    };
  }
  let memory: string | null = null;
  return {
    get: () => memory,
    set: (token) => {
      memory = token;
    },
    clear: () => {
      memory = null;
    },
  };
}

/* ---------- The read wire shapes (cmd/api/conflicthttp + the detector) ---------- */

/** One classified conflict (internal/rsg/conflict Conflict). */
export interface ConflictDetail {
  category: string;
  /** The stable machine-readable code (e.g. SCIENTIFIC_FIELD_DIVERGES). */
  code: string;
  /** The object/relation fields the conflict covers, canonical order. */
  fields: string[];
  /** The top-level payload keys that diverged; empty unless "payload" is in fields. */
  payload_keys: string[];
  /** The target-side paired object of an identity conflict; "" otherwise. */
  other_object_id: string;
  /** The detector's human-readable explanation — advisory only. */
  detail: string;
}

/** One object's conflict verdict (internal/rsg/conflict ObjectVerdict). */
export interface ObjectVerdict {
  object_id: string;
  object_type: string;
  kind: string;
  auto_mergeable: boolean;
  conflicts: ConflictDetail[];
}

/** One relation's conflict verdict (internal/rsg/conflict RelationVerdict). */
export interface RelationVerdict {
  relation_id: string;
  kind: string;
  auto_mergeable: boolean;
  conflicts: ConflictDetail[];
}

/** The detector's summary roll-up (internal/rsg/conflict Summary). */
export interface ConflictSummary {
  objects_auto_mergeable: number;
  objects_conflicted: number;
  relations_auto_mergeable: number;
  relations_conflicted: number;
  conflicts_by_category: { category: string; count: number }[];
}

/** One scientific object version (internal/rsg/manifest ObjectVersion). */
export interface ObjectVersion {
  id: string;
  object_id: string;
  object_type: string;
  version_no: number;
  state_id: string;
  branch_id: string | null;
  schema_ref: { id: string; version: string };
  title: string;
  lifecycle_state: string;
  /** The versioned scientific content in canonical form. */
  payload: unknown;
  visibility_policy_id: string | null;
  integrity_hash: string;
  created_by: string;
  created_at: string;
}

/** One object's three-way change (internal/rsg/diff ObjectChange). */
export interface ObjectChange {
  object_id: string;
  object_type: string;
  kind: string;
  base_version: ObjectVersion | null;
  source_version: ObjectVersion;
  target_version: ObjectVersion | null;
  target_moved: boolean;
  changed_fields: string[];
  target_changed_fields: string[];
  new_versions: ObjectVersion[];
}

/** One relation's three-way change (internal/rsg/diff RelationChange). */
export interface RelationChange {
  relation_id: string;
  kind: string;
  base_version: RelationVersion | null;
  source_version: RelationVersion;
  target_version: RelationVersion | null;
  target_moved: boolean;
  changed_fields: string[];
  target_changed_fields: string[];
  new_versions: RelationVersion[];
}

/** One relation version (internal/rsg/manifest RelationVersion). */
export interface RelationVersion {
  id: string;
  relation_id: string;
  version_no: number;
  state_id: string;
  relation_type: string;
  source_object_version_id: string;
  target_object_version_id: string;
  payload: unknown;
  integrity_hash: string;
  created_by: string;
  created_at: string;
}

/** One state ref of the three-way diff (internal/rsg/diff StateRef). */
export interface StateRef {
  id: string;
  git_ref: string | null;
}

/** The three-way diff (internal/rsg/diff Diff) the report carries. */
export interface ConflictDiff {
  format_version: string;
  project_id: string;
  base: StateRef;
  source: StateRef;
  target: StateRef;
  object_changes: ObjectChange[];
  relation_changes: RelationChange[];
  file_diff_refs: {
    kind: string;
    base_git_ref: string;
    head_git_ref: string;
  }[];
  summary: {
    objects_created: number;
    objects_updated: number;
    objects_aborted: number;
    objects_reopened: number;
    relations_created: number;
    relations_updated: number;
  };
}

/** The detector report (internal/rsg/conflict Report). */
export interface ConflictReport {
  format_version: string;
  project_id: string;
  diff: ConflictDiff;
  object_verdicts: ObjectVerdict[];
  relation_verdicts: RelationVerdict[];
  auto_mergeable: boolean;
  summary: ConflictSummary;
}

/** One evidence assertion (internal/application/resolutions EvidenceItem). */
export interface EvidenceItem {
  relation_type: string;
  evidence_type: string;
  directness: string;
  reasoning_note: string | null;
  review_state: string;
  evidence_object_id: string;
  evidence_object_type: string;
  evidence_title: string;
  evidence_object_version_no: number;
}

/** The per-side evidence context of one conflicted object. */
export interface ObjectEvidence {
  object_id: string;
  source_evidence: EvidenceItem[];
  target_evidence: EvidenceItem[];
}

/** The GET conflicts answer (cmd/api/conflicthttp conflictViewPayload). */
export interface ConflictView {
  report: ConflictReport;
  evidence: ObjectEvidence[];
  resolutions: ResolutionRecord[];
}

/* ---------- The write wire shapes (cmd/api/conflicthttp) ---------- */

/**
 * The five human decision kinds (docs/09 §8: Accept A / Accept B / Keep
 * both versions / Create validation branch / keep unresolved). The list
 * is exhaustive — the API refuses any other spelling, and nothing on the
 * wire or in the UI computes a merged value. No kind averages or derives
 * parameters (docs/09 §7: Protocol 冲突数值折中禁止自动).
 */
export const RESOLUTION_KINDS = [
  "accept_source",
  "accept_target",
  "keep_both",
  "validation_branch",
  "unresolved",
] as const;

/** One of the five decision kinds. */
export type ResolutionKind = (typeof RESOLUTION_KINDS)[number];

/** One submitted decision (cmd/api/conflicthttp decisionPayload). */
export interface DecisionInput {
  /** "object" or "relation" — which change the conflict is on. */
  target_kind: "object" | "relation";
  target_id: string;
  code: string;
  fields: string[];
  payload_keys: string[];
  other_object_id: string | null;
  kind: ResolutionKind;
  note: string;
}

/** One recorded decision (cmd/api/conflicthttp resolutionPayload). */
export interface ResolutionRecord {
  id: string;
  project_id: string;
  base_state_id: string;
  source_state_id: string;
  target_state_id: string;
  target_kind: string;
  target_id: string;
  code: string;
  fields: string[];
  payload_keys: string[];
  other_object_id: string | null;
  kind: string;
  note: string;
  decided_by: string;
  decided_at: string;
  updated_at: string;
}

/** The conflicts client: one per API origin. */
export interface ConflictsClient {
  /** The conflict report + evidence + recorded decisions of one triple. */
  get(projectId: string, triple: ConflictTriple): Promise<ConflictView>;
  /**
   * Record the human's decisions for the triple. The PUT carries the
   * session-bound CSRF token; the answer is the full updated plan.
   */
  save(
    projectId: string,
    triple: ConflictTriple,
    decisions: DecisionInput[],
  ): Promise<ResolutionRecord[]>;
  /** The currently stored CSRF token, if any. */
  csrfToken(): string | null;
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

/** Options for createConflictsClient. */
export interface ConflictsClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /** CSRF token storage; default: the shared session storage ("post.csrf"). */
  storage?: TokenStorage;
}

/** Build the conflicts client for one API origin. */
export function createConflictsClient(
  apiBaseUrl: string,
  options: ConflictsClientOptions = {},
): ConflictsClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);
  const storage = options.storage ?? sessionTokenStorage();

  /** One JSON call; carries the CSRF token when known (like lib/profile.ts). */
  async function call(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = {};
    const csrf = storage.get();
    if (csrf !== null) headers["X-CSRF-Token"] = csrf;
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

  function route(projectId: string, endpoint: string, triple: ConflictTriple): string {
    const params = new URLSearchParams();
    params.set("base_state_id", triple.base_state_id);
    params.set("source_state_id", triple.source_state_id);
    params.set("target_state_id", triple.target_state_id);
    return (
      `/api/v1/projects/${encodeURIComponent(projectId)}/${endpoint}` +
      `?${params.toString()}`
    );
  }

  /** The query of the read; the write carries the triple in its body. */
  function conflictsPath(projectId: string, triple: ConflictTriple): string {
    return route(projectId, "conflicts", triple);
  }

  function asView(body: unknown): ConflictView {
    if (
      typeof body !== "object" || body === null ||
      !("report" in body) || typeof (body as { report: unknown }).report !== "object" ||
      !("evidence" in body) || !Array.isArray((body as { evidence: unknown }).evidence) ||
      !("resolutions" in body) || !Array.isArray((body as { resolutions: unknown }).resolutions)
    ) {
      throw new Error("unexpected conflicts response shape");
    }
    return body as ConflictView;
  }

  return {
    async get(projectId, triple) {
      const result = await call("GET", conflictsPath(projectId, triple));
      if (result.status >= 400) fail(result.status, result.body);
      return asView(result.body);
    },
    async save(projectId, triple, decisions) {
      const result = await call(
        "PUT",
        `/api/v1/projects/${encodeURIComponent(projectId)}/resolutions`,
        {
          base_state_id: triple.base_state_id,
          source_state_id: triple.source_state_id,
          target_state_id: triple.target_state_id,
          resolutions: decisions,
        },
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("resolutions" in body) || !Array.isArray((body as { resolutions: unknown }).resolutions)
      ) {
        throw new Error("unexpected resolutions response shape");
      }
      return (body as { resolutions: ResolutionRecord[] }).resolutions;
    },
    csrfToken: () => storage.get(),
  };
}

/** Human-facing message for a stable conflicts API code. */
export function messageForConflictCode(code: string): string {
  switch (code) {
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "VALIDATION_FAILED":
      return "The request is not valid — re-read the conflict report and try again.";
    case "CONFLICT_NOT_FOUND":
      return "The conflict changed since this page loaded. Reload and decide again.";
    case "AUTH_FORBIDDEN":
      return "You are not allowed to resolve conflicts in this project.";
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to record conflict resolutions.";
    case "CSRF_FAILED":
      return "The request was rejected as cross-site. Please reload and try again.";
    case "STATE_NOT_FOUND":
      return "One of the three states does not exist (anymore).";
    case "SERVICE_UNAVAILABLE":
      return "Conflict resolution is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while loading conflicts. Please try again.";
  }
}
