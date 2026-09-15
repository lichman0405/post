/**
 * Client-side pull-request client for the Go API (T0403, extended by
 * T0408).
 *
 * Reads the PR surface the Pull requests tab renders:
 * GET /api/v1/projects/{id}/pull-requests (the project's PR list),
 * GET /api/v1/projects/{id}/pull-requests/{number} (one PR),
 * GET /api/v1/projects/{id}/pull-requests/{number}/checks (the machine
 * integrity report the detail page's checks section renders),
 * GET /api/v1/projects/{id}/pull-requests/{number}/diff (the three-way
 * Research State Diff the PR page's tabs render, cmd/api/pullrequestshttp)
 * and GET /api/v1/projects/{id}/pull-requests/{number}/reviews (the
 * recorded review rounds, cmd/api/reviewhttp). All calls carry the
 * session cookie (credentials: include).
 *
 * The one write is the review submission
 * (POST .../pull-requests/{number}/reviews): it carries the session-bound
 * CSRF token from the shared "post.csrf" storage (the SAME key as
 * lib/auth.ts and lib/conflicts.ts), so the guard always accepts it.
 * Authorization truth stays in the API: a refusal renders its own
 * message, the page never guesses the caller's role.
 *
 * The module is import-free by construction (like lib/projects.ts), so
 * the node:test suite (pulls.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/projects.ts.
 */

/** The wire PR shape (cmd/api/pullrequestshttp prPayload, docs/43 states). */
export interface PullRequest {
  id: string;
  number: number;
  title: string;
  body: string;
  state: string;
  source_branch_id: string;
  target_branch_id: string;
  base_state_id: string;
  proposed_state_id: string;
  created_by: string;
  created_at: string;
}

/** One check outcome (internal/rsg/integrity Result). */
export interface CheckResult {
  dimension: string;
  check: string;
  severity: "warning" | "blocking";
  passed: boolean;
  subject?: string;
  detail?: string;
  why: string;
}

/** The machine integrity report (internal/rsg/integrity Report). */
export interface IntegrityReport {
  kind: string;
  project_id: string;
  pr_number: number;
  base_state_id: string;
  proposed_state_id: string;
  verdict: "pass" | "pass_with_warnings" | "blocked";
  results: CheckResult[];
  explanation: string;
  computed_at: string;
}

/* ---------- The three-way Research State Diff (internal/rsg/diff) ---------- */

/** One state ref of the three-way diff (internal/rsg/diff StateRef). */
export interface StateRef {
  id: string;
  /** The state's recorded git commit sha; null when it has none. */
  git_ref: string | null;
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

/** One object's three-way change (internal/rsg/diff ObjectChange). */
export interface ObjectChange {
  object_id: string;
  object_type: string;
  /** created | updated | aborted | reopened (diff.ChangeKind). */
  kind: string;
  base_version: ObjectVersion | null;
  source_version: ObjectVersion;
  target_version: ObjectVersion | null;
  /** The target side moved the same object since the branch point. */
  target_moved: boolean;
  /** Base→source field differences, canonical order. */
  changed_fields: string[];
  /** Base→target field differences, canonical order. */
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

/** One raw file-diff ref of the three-way (internal/rsg/diff FileDiffRef). */
export interface FileDiffRef {
  /** "source" (base→source: the proposal's files) or "target" (base→target). */
  kind: string;
  /** The base state's recorded git sha; "" when it has none. */
  base_git_ref: string;
  /** The head state's recorded git sha; "" when it has none. */
  head_git_ref: string;
}

/** The diff's roll-up (internal/rsg/diff Summary). */
export interface DiffSummary {
  objects_created: number;
  objects_updated: number;
  objects_aborted: number;
  objects_reopened: number;
  relations_created: number;
  relations_updated: number;
}

/** The three-way Research State Diff (internal/rsg/diff Document). */
export interface DiffDocument {
  format_version: string;
  project_id: string;
  /** The pinned merge base — the PR's base_state_id, never the branch head. */
  base: StateRef;
  /** The proposed side — the PR's proposed_state_id. */
  source: StateRef;
  /** The target side — the target branch's CURRENT head. */
  target: StateRef;
  object_changes: ObjectChange[];
  relation_changes: RelationChange[];
  file_diff_refs: FileDiffRef[];
  summary: DiffSummary;
}

/* ---------- Reviews (domain.Review, cmd/api/reviewhttp) ---------- */

/** One recorded review (cmd/api/reviewhttp reviewPayload). */
export interface Review {
  id: string;
  pull_request_id: string;
  reviewer_id: string;
  /** The review dimension: scientific or integrity (docs/09 §5). */
  kind: string;
  /** approved | changes_requested | comment. */
  decision: string;
  /** The state this decision judged (the head at review time). */
  reviewed_state_id: string;
  /** The review-responsibility label the API resolved; "" when none. */
  responsibility: string;
  body: string;
  created_at: string;
}

/** One review submission (the POST body; identity is derived server-side). */
export interface ReviewInput {
  /** The review dimension: scientific or integrity. */
  kind: "scientific" | "integrity";
  /** The verdict about the PR's current proposed head. */
  decision: "approved" | "changes_requested" | "comment";
  /** The free-form reasoning; "" when none. */
  body: string;
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
 * (Same shape as lib/projects.ts ApiError.)
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
 * The shared CSRF storage key — MUST match lib/auth.ts and
 * lib/conflicts.ts: the token is session-bound, one per tab, and every
 * client reads/writes the same one.
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

/** The pulls client: one per API origin. */
export interface PullsClient {
  /** The project's pull requests, oldest first (the API's number order). */
  list(projectId: string): Promise<PullRequest[]>;
  /** One PR by number; 404 carries the existence-hiding envelope. */
  get(projectId: string, number: number): Promise<PullRequest>;
  /** The PR's machine integrity report. */
  checks(projectId: string, number: number): Promise<IntegrityReport>;
  /** The PR's three-way Research State Diff (base / source / target). */
  diff(projectId: string, number: number): Promise<DiffDocument>;
  /** The PR's recorded reviews, oldest first. */
  reviews(projectId: string, number: number): Promise<Review[]>;
  /**
   * Record one review decision on the PR's current head. The POST carries
   * the session-bound CSRF token; the answer is the recorded review.
   */
  submitReview(projectId: string, number: number, input: ReviewInput): Promise<Review>;
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

/** Options for createPullsClient. */
export interface PullsClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /** CSRF token storage; default: the shared session storage ("post.csrf"). */
  storage?: TokenStorage;
}

/** Build the pulls client for one API origin. */
export function createPullsClient(
  apiBaseUrl: string,
  options: PullsClientOptions = {},
): PullsClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);
  const storage = options.storage ?? sessionTokenStorage();

  /** One JSON call against the API; the session cookie rides along. */
  async function call(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = {};
    // A write carries the session's CSRF token (the guard rejects writes
    // without it); the reads never need one, and sending none keeps a
    // read from ever tripping the guard's write path.
    if (body !== undefined) {
      const csrf = storage.get();
      if (csrf !== null) headers["X-CSRF-Token"] = csrf;
      headers["Content-Type"] = "application/json";
    }
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

  // The guard covers every field of the declared shape, including the
  // ones the detail page renders as text (body, created_by) and the
  // branch ids it keys on: a response missing one must fail here rather
  // than render `undefined`.
  function asPR(body: unknown): PullRequest {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("number" in body) || typeof body.number !== "number" ||
      !("title" in body) || typeof body.title !== "string" ||
      !("body" in body) || typeof body.body !== "string" ||
      !("state" in body) || typeof body.state !== "string" ||
      !("source_branch_id" in body) || typeof body.source_branch_id !== "string" ||
      !("target_branch_id" in body) || typeof body.target_branch_id !== "string" ||
      !("base_state_id" in body) || typeof body.base_state_id !== "string" ||
      !("proposed_state_id" in body) || typeof body.proposed_state_id !== "string" ||
      !("created_by" in body) || typeof body.created_by !== "string" ||
      !("created_at" in body) || typeof body.created_at !== "string"
    ) {
      throw new Error("unexpected pull request response shape");
    }
    return body as PullRequest;
  }

  // The guards below pin the fields the PR page renders. The wire
  // carries more than the page reads (payloads, hashes, provenance), and
  // a field nothing renders is deliberately not policed here: the page
  // must fail loudly on a missing rendered field, and stay silent about
  // the rest.
  function asStateRef(body: unknown): StateRef {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string"
    ) {
      throw new Error("unexpected state ref shape");
    }
    return body as StateRef;
  }

  /** The fields of a version the change lists render. */
  function asVersion(body: unknown, label: string): void {
    if (typeof body !== "object" || body === null) {
      throw new Error(`unexpected ${label} version shape`);
    }
  }

  function asObjectChange(body: unknown): ObjectChange {
    if (
      typeof body !== "object" || body === null ||
      !("object_id" in body) || typeof body.object_id !== "string" ||
      !("object_type" in body) || typeof body.object_type !== "string" ||
      !("kind" in body) || typeof body.kind !== "string" ||
      !("target_moved" in body) || typeof body.target_moved !== "boolean" ||
      !("changed_fields" in body) || !Array.isArray(body.changed_fields) ||
      !("source_version" in body) || typeof body.source_version !== "object" ||
      body.source_version === null
    ) {
      throw new Error("unexpected object change shape");
    }
    asVersion(body.source_version, "object");
    const change = body as ObjectChange;
    // The source version is the one side a change always has (a created
    // object has no base, an aborted object has no target), and the tab
    // lists render its labels — a list row without them is unreadable.
    const source = change.source_version as unknown as Record<string, unknown>;
    for (const field of ["id", "object_id", "object_type", "title", "lifecycle_state", "state_id"]) {
      if (typeof source[field] !== "string") {
        throw new Error(`unexpected object change shape: source version has no ${field}`);
      }
    }
    // The rights pin is a risk input (a created object born pinned to its
    // own policy is a visibility change the reviewer must see), so its
    // absence has to fail loudly: read as "inherits the default" it would
    // hide exactly the case the banner exists for. null is the ordinary
    // "no pin" value and stays accepted.
    const policy = source["visibility_policy_id"];
    if (policy !== null && typeof policy !== "string") {
      throw new Error("unexpected object change shape: source version has no visibility_policy_id");
    }
    return change;
  }

  function asRelationChange(body: unknown): RelationChange {
    if (
      typeof body !== "object" || body === null ||
      !("relation_id" in body) || typeof body.relation_id !== "string" ||
      !("kind" in body) || typeof body.kind !== "string" ||
      !("target_moved" in body) || typeof body.target_moved !== "boolean" ||
      !("changed_fields" in body) || !Array.isArray(body.changed_fields) ||
      !("source_version" in body) || typeof body.source_version !== "object" ||
      body.source_version === null
    ) {
      throw new Error("unexpected relation change shape");
    }
    asVersion(body.source_version, "relation");
    const change = body as RelationChange;
    const source = change.source_version as unknown as Record<string, unknown>;
    for (const field of ["id", "relation_id", "relation_type", "state_id"]) {
      if (typeof source[field] !== "string") {
        throw new Error(`unexpected relation change shape: source version has no ${field}`);
      }
    }
    return change;
  }

  function asFileDiffRef(body: unknown): FileDiffRef {
    if (
      typeof body !== "object" || body === null ||
      !("kind" in body) || typeof body.kind !== "string" ||
      !("base_git_ref" in body) || typeof body.base_git_ref !== "string" ||
      !("head_git_ref" in body) || typeof body.head_git_ref !== "string"
    ) {
      throw new Error("unexpected file diff ref shape");
    }
    return body as FileDiffRef;
  }

  function asDiff(body: unknown): DiffDocument {
    if (
      typeof body !== "object" || body === null ||
      !("format_version" in body) || typeof body.format_version !== "string" ||
      !("project_id" in body) || typeof body.project_id !== "string" ||
      !("base" in body) || !("source" in body) || !("target" in body) ||
      !("object_changes" in body) || !Array.isArray(body.object_changes) ||
      !("relation_changes" in body) || !Array.isArray(body.relation_changes) ||
      !("file_diff_refs" in body) || !Array.isArray(body.file_diff_refs) ||
      !("summary" in body) || typeof body.summary !== "object" || body.summary === null
    ) {
      throw new Error("unexpected diff response shape");
    }
    const summary = body.summary as unknown as Record<string, unknown>;
    for (const field of [
      "objects_created",
      "objects_updated",
      "objects_aborted",
      "objects_reopened",
      "relations_created",
      "relations_updated",
    ]) {
      if (typeof summary[field] !== "number") {
        throw new Error(`unexpected diff response shape: summary has no ${field}`);
      }
    }
    return {
      ...(body as DiffDocument),
      base: asStateRef(body.base),
      source: asStateRef(body.source),
      target: asStateRef(body.target),
      object_changes: body.object_changes.map(asObjectChange),
      relation_changes: body.relation_changes.map(asRelationChange),
      file_diff_refs: body.file_diff_refs.map(asFileDiffRef),
    };
  }

  function asReview(body: unknown): Review {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("pull_request_id" in body) || typeof body.pull_request_id !== "string" ||
      !("reviewer_id" in body) || typeof body.reviewer_id !== "string" ||
      !("kind" in body) || typeof body.kind !== "string" ||
      !("decision" in body) || typeof body.decision !== "string" ||
      !("reviewed_state_id" in body) || typeof body.reviewed_state_id !== "string" ||
      !("responsibility" in body) || typeof body.responsibility !== "string" ||
      !("body" in body) || typeof body.body !== "string" ||
      !("created_at" in body) || typeof body.created_at !== "string"
    ) {
      throw new Error("unexpected review response shape");
    }
    return body as Review;
  }

  function asResult(body: unknown): CheckResult {
    if (
      typeof body !== "object" || body === null ||
      !("dimension" in body) || typeof body.dimension !== "string" ||
      !("check" in body) || typeof body.check !== "string" ||
      !("severity" in body) || typeof body.severity !== "string" ||
      !("passed" in body) || typeof body.passed !== "boolean"
    ) {
      throw new Error("unexpected check result shape");
    }
    return body as CheckResult;
  }

  function asReport(body: unknown): IntegrityReport {
    if (
      typeof body !== "object" || body === null ||
      !("kind" in body) || body.kind !== "integrity" ||
      !("pr_number" in body) || typeof body.pr_number !== "number" ||
      !("verdict" in body) || typeof body.verdict !== "string" ||
      !("results" in body) || !Array.isArray(body.results) ||
      !("explanation" in body) || typeof body.explanation !== "string" ||
      !("computed_at" in body) || typeof body.computed_at !== "string"
    ) {
      throw new Error("unexpected integrity report shape");
    }
    return { ...(body as IntegrityReport), results: body.results.map(asResult) };
  }

  return {
    async list(projectId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (!Array.isArray(body)) {
        throw new Error("unexpected pull request list response shape");
      }
      return body.map(asPR);
    },
    async get(projectId, number) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests/${number}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asPR(result.body);
    },
    async checks(projectId, number) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests/${number}/checks`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asReport(result.body);
    },
    async diff(projectId, number) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests/${number}/diff`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asDiff(result.body);
    },
    async reviews(projectId, number) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests/${number}/reviews`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (!Array.isArray(body)) {
        throw new Error("unexpected review list response shape");
      }
      return body.map(asReview);
    },
    async submitReview(projectId, number, input) {
      const result = await call(
        "POST",
        `/api/v1/projects/${encodeURIComponent(projectId)}/pull-requests/${number}/reviews`,
        { kind: input.kind, decision: input.decision, body: input.body },
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asReview(result.body);
    },
    csrfToken: () => storage.get(),
  };
}

/**
 * The six canonical integrity dimensions in report order
 * (internal/rsg/integrity check.go): the checks section renders one
 * group per dimension, in this order.
 */
export const INTEGRITY_DIMENSIONS = [
  "schema",
  "provenance",
  "dependency",
  "rights",
  "visibility",
  "blob",
] as const;

/** Human-facing label for one integrity dimension. */
export function dimensionLabel(dimension: string): string {
  switch (dimension) {
    case "schema":
      return "Schema";
    case "provenance":
      return "Provenance";
    case "dependency":
      return "Dependency";
    case "rights":
      return "Rights";
    case "visibility":
      return "Visibility";
    case "blob":
      return "Blob references";
    default:
      return dimension;
  }
}

/** Human-facing message for a stable pull-request API code. */
export function messageForPullRequestCode(code: string): string {
  switch (code) {
    case "PULL_REQUEST_NOT_FOUND":
      return "This pull request does not exist, or you do not have access to it.";
    case "VALIDATION_FAILED":
      return "That request could not be processed. Check the pull request number and try again.";
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "STATE_NOT_FOUND":
      return "One of the compared research states no longer exists, so the diff cannot be shown.";
    case "REVIEW_ALREADY_SUBMITTED":
      return "You already recorded this review kind for this head — the author has to update the proposal, then review it again.";
    case "PR_TERMINAL":
      return "This pull request is already closed; a closed proposal accepts no further reviews.";
    case "AUTH_FORBIDDEN":
      return "You are not permitted to submit a review in this project.";
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to submit a review.";
    case "CSRF_FAILED":
      return "The request was rejected as cross-site. Reload the page and try again.";
    case "SERVICE_UNAVAILABLE":
      return "Pull request data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while loading pull request data. Please try again.";
  }
}

/* ---------- The PR page's tabs (docs/06 §6) ---------- */

/**
 * The PR page's tabs, in render order. docs/06 §6 fixes this shape: the
 * first screen is the Research State Diff — the scientific objects
 * created/updated/aborted, knowledge changes, evidence changes, protocol/
 * schema changes, visibility changes and dependency impact — and the raw
 * Git diff lives on a secondary tab, never on the first screen.
 */
export const PULL_TABS = [
  "summary",
  "scientific",
  "knowledge",
  "evidence",
  "checks",
  "raw",
] as const;

/** One tab of the PR page. */
export type PullTab = (typeof PULL_TABS)[number];

/** The tab the page opens on — never the raw file diff (docs/06 §6). */
export const DEFAULT_PULL_TAB: PullTab = "summary";

/** Human-facing tab label. */
export function pullTabLabel(tab: PullTab): string {
  switch (tab) {
    case "summary":
      return "Summary";
    case "scientific":
      return "Scientific changes";
    case "knowledge":
      return "Knowledge changes";
    case "evidence":
      return "Evidence";
    case "checks":
      return "Checks";
    case "raw":
      return "Raw Files";
  }
}

/** One line of what a tab holds, rendered under the tab bar. */
export function pullTabHint(tab: PullTab): string {
  switch (tab) {
    case "summary":
      return "What this proposal changes, in one screen: counts, both review dimensions, and the risks a reviewer must not miss.";
    case "scientific":
      return "Every scientific object the proposal creates, updates, aborts or reopens, with its three-way fields.";
    case "knowledge":
      return "Changes to the knowledge objects (claims, hypotheses, research questions, findings) and the knowledge relations between them.";
    case "evidence":
      return "Evidence assertions and the evidence relations (docs/10 §4) this proposal adds or changes.";
    case "checks":
      return "The machine integrity report: one row per check, with its own reason.";
    case "raw":
      return "The recorded Git refs of both sides. This is the raw file view — the semantic diff above is the page's subject.";
  }
}

/* ---------- Change classification (docs/10 §4, relationcatalog) ---------- */

/**
 * The knowledge objects: the objects that state what the project believes
 * (docs/08 §scientific objects). A change to one of them is a knowledge
 * change, whoever noticed it.
 */
export const KNOWLEDGE_OBJECT_TYPES = [
  "research_question",
  "hypothesis",
  "claim",
  "finding",
] as const;

/** The evidence objects (docs/10: evidence is its own object type). */
export const EVIDENCE_OBJECT_TYPES = ["evidence_assertion"] as const;

/**
 * The evidence relation types (docs/10 §4) — the edges that say how an
 * evidence object bears on a knowledge object. The list is docs/10 §4
 * verbatim; the Go side pins the same nine (domain.EvidenceRelations).
 */
export const EVIDENCE_RELATION_TYPES = [
  "supports",
  "contradicts",
  "consistent_with",
  "inconsistent_with",
  "reproduces",
  "fails_to_reproduce",
  "validates",
  "challenges",
  "contextualizes",
] as const;

/**
 * The knowledge-category relation types (internal/rsg/relationcatalog
 * CategoryKnowledge, docs/44) — the edges between claims, questions,
 * hypotheses and results. It is a superset of the evidence relations:
 * addresses_question, tests_hypothesis, competes_with and refines carry
 * knowledge without asserting evidence, so they are rendered as knowledge
 * changes, and the nine evidence-shaped ones as evidence changes.
 */
export const KNOWLEDGE_RELATION_TYPES = [
  "addresses_question",
  "challenges",
  "competes_with",
  "consistent_with",
  "contextualizes",
  "contradicts",
  "fails_to_reproduce",
  "inconsistent_with",
  "refines",
  "reproduces",
  "supports",
  "tests_hypothesis",
  "validates",
] as const;

/** Which bucket one changed object belongs to. */
export function objectBucket(objectType: string): "knowledge" | "evidence" | "structure" {
  if (isEvidenceObjectType(objectType)) return "evidence";
  if (isKnowledgeObjectType(objectType)) return "knowledge";
  return "structure";
}

/** Which bucket one changed relation belongs to. */
export function relationBucket(relationType: string): "evidence" | "knowledge" | "structure" {
  if (isEvidenceRelationType(relationType)) return "evidence";
  if (isKnowledgeRelationType(relationType)) return "knowledge";
  return "structure";
}

export function isEvidenceObjectType(objectType: string): boolean {
  return (EVIDENCE_OBJECT_TYPES as readonly string[]).includes(objectType);
}

export function isKnowledgeObjectType(objectType: string): boolean {
  return (KNOWLEDGE_OBJECT_TYPES as readonly string[]).includes(objectType);
}

export function isEvidenceRelationType(relationType: string): boolean {
  return (EVIDENCE_RELATION_TYPES as readonly string[]).includes(relationType);
}

export function isKnowledgeRelationType(relationType: string): boolean {
  return (KNOWLEDGE_RELATION_TYPES as readonly string[]).includes(relationType);
}

/** The relation type of a change: the edge's type is version-stable. */
export function relationTypeOf(change: RelationChange): string {
  return change.source_version.relation_type;
}

/**
 * The knowledge changes of a diff: knowledge objects plus the knowledge
 * relations that assert no evidence. Evidence changes are listed by
 * evidenceChanges instead — the two tabs partition the knowledge layer
 * rather than repeat each other.
 */
export function knowledgeChanges(diff: DiffDocument): {
  objects: ObjectChange[];
  relations: RelationChange[];
} {
  return {
    objects: diff.object_changes.filter((c) => objectBucket(c.object_type) === "knowledge"),
    relations: diff.relation_changes.filter((c) => relationBucket(relationTypeOf(c)) === "knowledge"),
  };
}

/** The evidence changes of a diff: evidence objects and evidence relations. */
export function evidenceChanges(diff: DiffDocument): {
  objects: ObjectChange[];
  relations: RelationChange[];
} {
  return {
    objects: diff.object_changes.filter((c) => objectBucket(c.object_type) === "evidence"),
    relations: diff.relation_changes.filter((c) => relationBucket(relationTypeOf(c)) === "evidence"),
  };
}

/** Human-facing label for one change kind (diff.ChangeKind). */
export function changeKindLabel(kind: string): string {
  switch (kind) {
    case "created":
      return "created";
    case "updated":
      return "updated";
    case "aborted":
      return "aborted";
    case "reopened":
      return "reopened";
    default:
      return kind;
  }
}

/** The diff's totals, object changes and relation changes separately. */
export function diffTotals(diff: DiffDocument): {
  objects: number;
  relations: number;
  files: number;
} {
  return {
    objects: diff.object_changes.length,
    relations: diff.relation_changes.length,
    files: diff.file_diff_refs.length,
  };
}

/* ---------- Review state (docs/09 §5: two dimensions, never flattened) ---------- */

/** Whether a review judged an older head than the one proposed now. */
export function isStaleReview(review: Review, headStateId: string): boolean {
  return review.reviewed_state_id !== headStateId;
}

/**
 * The latest review of one dimension that judged the CURRENT head, or
 * null when that dimension has none. A decision on an older head stays
 * visible (it is a recorded fact) but never counts as the head's state:
 * the head is what a reviewer now would judge.
 *
 * The list arrives in the API's own order — oldest first, `created_at`
 * then `id` (ListReviewsByPullRequest), a total order — so the LAST match
 * is the latest one. Never compare `created_at` as a string: it is RFC
 * 3339 with a variable number of fractional digits (Go omits a zero
 * fraction and trims trailing zeros), so "…T12:00:00Z" sorts ABOVE
 * "…T12:00:00.5Z" and "…T12:00:00.5Z" above "…T12:00:00.55Z", each time
 * picking the earlier review.
 */
export function latestHeadReview(
  reviews: Review[],
  kind: string,
  headStateId: string,
): Review | null {
  let found: Review | null = null;
  for (const review of reviews) {
    if (review.kind !== kind || isStaleReview(review, headStateId)) continue;
    found = review;
  }
  return found;
}

/* ---------- Risks (docs/06 §6: 重要风险必须明显) ---------- */

/** One risk the PR page puts above the tab bar. */
export interface PullRisk {
  /** "blocking" is the machine's own severity or a refused review; the rest warn. */
  severity: "blocking" | "warning";
  /** The stable machine code (RISK_*), for tests and for the API's log. */
  code: string;
  title: string;
  detail: string;
  /** The tab holding the evidence for this risk. */
  tab: PullTab;
}

const RISK_BLOCKING_CHECK = "RISK_BLOCKING_CHECK";
const RISK_INTEGRITY_BLOCKED = "RISK_INTEGRITY_VERDICT_BLOCKED";
const RISK_WARNING_CHECK = "RISK_WARNING_CHECK";
const RISK_TARGET_MOVED = "RISK_TARGET_MOVED";
const RISK_OBJECTS_ABORTED = "RISK_OBJECTS_ABORTED";
const RISK_SCHEMA_CHANGED = "RISK_SCHEMA_CHANGED";
const RISK_VISIBILITY_CHANGED = "RISK_VISIBILITY_CHANGED";
const RISK_CHANGES_REQUESTED = "RISK_CHANGES_REQUESTED";

/**
 * The visibility policy one object version pins, or null when it pins
 * none (the version inherits the project default). The API always sends
 * the key; `asObjectChange` rejects a version that omits it, because a
 * missing rights pin would otherwise read as "inherits the default".
 */
function pinnedPolicy(version: ObjectVersion): string | null {
  const policy: string | null | undefined = version.visibility_policy_id;
  return policy === undefined || policy === null || policy === "" ? null : policy;
}

/**
 * Whether one object change is a visibility decision a reviewer must see.
 *
 * `changed_fields` cannot answer this on its own. The diff engine records
 * no field differences for a creation — internal/rsg/diff's
 * classifyObject returns (created, nil), there is no base value to differ
 * from — so a proposal that creates an object already pinned to its own
 * rights policy would render as "no visibility risk" while the merge
 * hands that object's rights to a policy nobody saw. For a created object
 * the pin itself is the decision: a nil pin inherits the project default
 * (the ordinary case — flagging every creation would bury the real risks
 * in noise), a non-nil pin is this PR choosing who may read the object
 * once it exists.
 *
 * No other category has this shape, which is why none goes the same way:
 * every object version carries a schema_ref, so a created object's schema
 * is not a deviation from a default — it is the object itself, and the
 * schema tab already shows it.
 */
function visibilityNoteworthy(change: ObjectChange): boolean {
  if (change.changed_fields.includes("visibility_policy_id")) return true;
  return change.kind === "created" && pinnedPolicy(change.source_version) !== null;
}

/** One line naming what happened to one object's visibility policy. */
function visibilityLine(change: ObjectChange): string {
  const title = change.source_version.title || change.object_id;
  const policy = pinnedPolicy(change.source_version);
  if (change.kind === "created") return `${title} (born pinned to ${policy})`;
  const before = change.base_version === null ? null : pinnedPolicy(change.base_version);
  return `${title} (policy ${before === null ? "inherited default" : before} → ${policy === null ? "inherited default" : policy})`;
}

/** What assessPullRisks reads: the PR's three answers plus the head it judges. */
export interface RiskInput {
  /** The machine integrity report. */
  report: IntegrityReport;
  /** The three-way Research State Diff. */
  diff: DiffDocument;
  /** The PR's recorded reviews. */
  reviews: Review[];
  /** The state the reviews are judged against: the PR's proposed head. */
  headStateId: string;
}

/**
 * The risks a reviewer must not miss, computed from the PR's own answers:
 * the machine's failing checks (at their own severity — a failed blocking
 * check is blocking here, never softened), a `blocked` verdict with no
 * blocking row, the target branch having moved the same entries, aborted
 * objects, schema changes, visibility-policy changes and a
 * changes_requested decision on the current head.
 *
 * Nothing here is invented: every entry names a fact the API returned,
 * and the page renders the list verbatim. An empty result means the page
 * shows no risk banner at all — the honest rendering of "nothing to warn
 * about", never a green "all clear" the page cannot vouch for.
 */
export function assessPullRisks(input: RiskInput): PullRisk[] {
  const { report, diff, reviews, headStateId } = input;
  const risks: PullRisk[] = [];

  // The machine's own rows, in report order.
  for (const result of report.results) {
    if (result.passed) continue;
    const subject = result.subject !== undefined && result.subject !== "" ? ` (${result.subject})` : "";
    const blocking = result.severity === "blocking";
    risks.push({
      severity: blocking ? "blocking" : "warning",
      code: blocking ? RISK_BLOCKING_CHECK : RISK_WARNING_CHECK,
      title: `${dimensionLabel(result.dimension)} check failed: ${result.check}${subject}`,
      detail: result.detail !== undefined && result.detail !== "" ? result.detail : result.why,
      tab: "checks",
    });
  }
  // The verdict is the report's own authority: a `blocked` verdict with
  // no blocking row would otherwise render as "no important risk".
  if (report.verdict === "blocked" && !risks.some((r) => r.severity === "blocking")) {
    risks.push({
      severity: "blocking",
      code: RISK_INTEGRITY_BLOCKED,
      title: "The machine integrity verdict is blocked",
      detail: report.explanation,
      tab: "checks",
    });
  }

  const movedObjects = diff.object_changes.filter((c) => c.target_moved);
  const movedRelations = diff.relation_changes.filter((c) => c.target_moved);
  const moved = [...movedObjects.map((c) => c.object_id), ...movedRelations.map((c) => c.relation_id)];
  if (moved.length > 0) {
    risks.push({
      severity: "warning",
      code: RISK_TARGET_MOVED,
      title: `The target branch moved ${moved.length} of the compared entries too`,
      detail:
        `${moved.join(", ")} — both sides changed these since the branch point, ` +
        `so the two moves have to be read together before this proposal merges.`,
      tab: "scientific",
    });
  }

  const aborted = diff.object_changes.filter((c) => c.kind === "aborted");
  if (aborted.length > 0) {
    risks.push({
      severity: "warning",
      code: RISK_OBJECTS_ABORTED,
      title: `${aborted.length} object${aborted.length === 1 ? " is" : "s are"} aborted by this proposal`,
      detail:
        `${aborted.map((c) => c.source_version.title || c.object_id).join(", ")} — ` +
        `nothing disappears, so an abort is a scientific act the reviewer must see.`,
      tab: "scientific",
    });
  }

  const schemaChanged = diff.object_changes.filter((c) => c.changed_fields.includes("schema_ref"));
  if (schemaChanged.length > 0) {
    risks.push({
      severity: "warning",
      code: RISK_SCHEMA_CHANGED,
      title: `${schemaChanged.length} object${schemaChanged.length === 1 ? "" : "s"} change their governing schema`,
      detail: schemaChanged
        .map((c) => `${c.source_version.title || c.object_id} → ${c.source_version.schema_ref.id} ${c.source_version.schema_ref.version}`)
        .join(", "),
      tab: "scientific",
    });
  }

  const visibilityChanged = diff.object_changes.filter(visibilityNoteworthy);
  if (visibilityChanged.length > 0) {
    risks.push({
      severity: "warning",
      code: RISK_VISIBILITY_CHANGED,
      title: `Visibility changes on ${visibilityChanged.length} object${visibilityChanged.length === 1 ? "" : "s"}`,
      detail:
        `${visibilityChanged.map(visibilityLine).join(", ")} — ` +
        `rights are version-pinned, so the reviewer has to see who can read what after this merge.`,
      tab: "scientific",
    });
  }

  const refused = reviews.filter(
    (r) => r.decision === "changes_requested" && r.reviewed_state_id === headStateId,
  );
  if (refused.length > 0) {
    risks.push({
      severity: "blocking",
      code: RISK_CHANGES_REQUESTED,
      title: `${refused.length} review${refused.length === 1 ? "" : "s"} request changes on the current head`,
      detail: refused.map((r) => `${r.kind} review by ${r.reviewer_id}`).join(", "),
      tab: "summary",
    });
  }

  // Blocking first: the banner leads with what stops the merge.
  return [
    ...risks.filter((r) => r.severity === "blocking"),
    ...risks.filter((r) => r.severity === "warning"),
  ];
}

/**
 * The raw patch URL of one commit, through the Files raw-diff channel
 * (GET /api/v1/projects/{id}/files/diff?sha=) the files page already
 * links to. It is one commit's patch — the PR page never claims a range
 * diff the API does not serve.
 */
export function rawPatchUrl(apiBaseUrl: string, projectId: string, sha: string): string {
  return (
    apiBaseUrl +
    `/api/v1/projects/${encodeURIComponent(projectId)}/files/diff` +
    `?sha=${encodeURIComponent(sha)}`
  );
}

