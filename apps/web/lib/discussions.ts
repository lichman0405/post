/**
 * Client-side discussions client for the Go API (T0811).
 *
 * A discussion is the conversation a target carries: a project, a
 * published knowledge object or a research pull request. Threads and
 * comments are ordinary rows — they carry no version, they are not part
 * of the scientific state, and nothing here can move it. What CAN move it
 * is a promotion: turning one comment into a proposed research object
 * (an Issue, a Hypothesis, or an external-evidence proposal) through the
 * service that owns that object, which is why promote is a separate call
 * with its own authorization.
 *
 * Routes (cmd/api/discussionhttp/wiring.go — the contract the API
 * registers; specs/api/openapi.yaml does not carry them yet):
 * GET    /api/v1/projects/{id}/discussions?target_type=&target_id=
 * POST   /api/v1/projects/{id}/discussions
 * GET    /api/v1/projects/{id}/discussions/{threadId}
 * POST   /api/v1/projects/{id}/discussions/{threadId}/comments
 * DELETE /api/v1/projects/{id}/discussions/{threadId}/comments/{commentId}
 * POST   /api/v1/projects/{id}/discussions/{threadId}/comments/{commentId}/promotions
 * GET    /api/v1/projects/{id}/discussions/promotions?ref=<kind>:<id>
 * GET    /api/v1/projects/{id}/discussions/promotions/{promotionId}
 *
 * Two shape notes the UI depends on:
 *
 *   - A withdrawn comment answers `body: null` with `deleted: true`. The
 *     row is never removed (nothing disappears; state only evolves), so
 *     the UI renders a tombstone line rather than dropping the row;
 *   - A promotion's `promoted_ref` is "<kind>:<uuid>" — the same
 *     reference the provenance read accepts, which is how a page answers
 *     "where did this object come from".
 *
 * All calls carry the session cookie (credentials: include); every state
 * change also heads the session-bound CSRF token. The module is
 * import-free by construction (like lib/milestones.ts), so the node:test
 * suite (discussions.test.mjs) runs it through Node's type stripping. The
 * ApiError shape mirrors lib/projects.ts.
 */

/** The wire thread shape (cmd/api/discussionhttp threadPayload). */
export interface DiscussionThread {
  id: string;
  project_id: string;
  /** "project" | "knowledge" | "pull_request". */
  target_type: string;
  target_id: string;
  created_by: string;
  created_at: string;
  /** The list read's rendering fields; zero on a thread read by id. */
  comment_count: number;
  last_comment_at: string | null;
}

/** The wire comment shape (commentPayload). Body is null once withdrawn. */
export interface DiscussionComment {
  id: string;
  thread_id: string;
  project_id: string;
  body: string | null;
  created_by: string;
  created_at: string;
  deleted: boolean;
  deleted_at: string | null;
  deleted_by: string | null;
}

/** The wire promotion shape (promotionPayload). */
export interface DiscussionPromotion {
  id: string;
  project_id: string;
  thread_id: string;
  comment_id: string;
  /** "issue" | "hypothesis" | "external_evidence". */
  promoted_kind: string;
  /** "<kind>:<uuid>" — the ref the provenance read accepts. */
  promoted_ref: string;
  promoted_by: string;
  promoted_at: string;
}

/** One provenance answer: the promotion, its thread and its comment. */
export interface PromotionOrigin {
  promotion: DiscussionPromotion;
  thread: DiscussionThread;
  comment: DiscussionComment;
}

/** A thread with its comments, as the read routes answer. */
export interface ThreadDetail {
  thread: DiscussionThread;
  comments: DiscussionComment[];
}

/** The evidence declarations an external-evidence promotion carries. */
export interface EvidenceProposalInput {
  targetVersionRef: string;
  evidenceVersionRef: string;
  relation: string;
  evidenceType: string;
  scope?: unknown;
  directness?: string;
  inferenceNature?: string;
  reasoningNote?: string;
}

/** One promotion request: the kind plus the declarations that kind needs. */
export interface PromoteInput {
  kind: string;
  title?: string;
  issueType?: string;
  branchId?: string;
  questionId?: string;
  evidence?: EvidenceProposalInput;
}

/** The promotion answer: provenance, plus the created object under its key. */
export interface PromotionResult extends PromotionOrigin {
  issue?: {
    id: string;
    project_id: string;
    number: number;
    issue_type: string;
    title: string;
    body: string;
    state: string;
    created_by: string;
    created_at: string;
  };
  scientific_object?: {
    id: string;
    project_id: string;
    object_type: string;
    version_id: string;
    version_no: number;
    state_id: string;
    title: string;
    lifecycle_state: string;
    payload: unknown;
  };
  evidence_assertion?: {
    id: string;
    project_id: string;
    state_id: string;
    relation_type: string;
    evidence_type: string;
    reasoning_note: string;
    /** "unreviewed" on creation: a proposal waiting for a reviewer. */
    review_state: string;
    visibility: string;
    created_by: string;
    created_at: string;
  };
}

/** The API error envelope (docs/22 §5): stable codes, no stack traces. */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/**
 * ApiError carries the API's envelope (the API answers one code per
 * failure shape), so the UI renders a sentence for the codes it knows and
 * a generic line for the rest — never a raw transport error.
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

/** The three target kinds a thread can be about (internal/domain). */
export const TARGET_KINDS = [
  { kind: "project", name: "Project" },
  { kind: "knowledge", name: "Knowledge" },
  { kind: "pull_request", name: "Pull request" },
] as const;

/** The three things a proposal can be promoted into (internal/domain). */
export const PROMOTION_KINDS = [
  { kind: "issue", name: "Issue" },
  { kind: "hypothesis", name: "Hypothesis" },
  { kind: "external_evidence", name: "External evidence proposal" },
] as const;

/** Whether the target kind is one the API accepts. */
export function isKnownTargetKind(kind: string): boolean {
  return TARGET_KINDS.some((entry) => entry.kind === kind);
}

/** Whether the promotion kind is one the API accepts. */
export function isKnownPromotionKind(kind: string): boolean {
  return PROMOTION_KINDS.some((entry) => entry.kind === kind);
}

/** The display name of a target kind; unknown kinds render raw. */
export function targetDisplayName(kind: string): string {
  const known = TARGET_KINDS.find((entry) => entry.kind === kind);
  return known === undefined ? kind : known.name;
}

/** The display name of a promotion kind; unknown kinds render raw. */
export function promotionDisplayName(kind: string): string {
  const known = PROMOTION_KINDS.find((entry) => entry.kind === kind);
  return known === undefined ? kind : known.name;
}

/** The promoted object's reference, "<kind>:<uuid>". */
export function promotionRef(kind: string, id: string): string {
  return `${kind}:${id}`;
}

/**
 * Split a promoted ref. null when it is not "<kind>:<id>" with a kind the
 * API knows — a page that cannot read a ref shows the object, not a link
 * it cannot resolve.
 */
export function parsePromotionRef(
  ref: string,
): { kind: string; id: string } | null {
  const at = ref.indexOf(":");
  if (at <= 0 || at === ref.length - 1) return null;
  const kind = ref.slice(0, at);
  if (!isKnownPromotionKind(kind)) return null;
  return { kind, id: ref.slice(at + 1) };
}

/**
 * The text a thread renders for one comment: the body, or the tombstone
 * line when the comment was withdrawn. The row stays in the conversation
 * (nothing disappears), so the UI needs a line for it rather than a
 * filter that would rewrite history.
 */
export function commentBodyForDisplay(comment: DiscussionComment): string {
  if (comment.deleted) return "This comment was withdrawn.";
  return comment.body ?? "";
}

/** Whether a comment carries a tombstone. */
export function isTombstone(comment: DiscussionComment): boolean {
  return comment.deleted;
}

/** Human-facing message for a stable discussion API code. */
export function messageForDiscussionCode(code: string): string {
  switch (code) {
    case "DISCUSSION_VALIDATION_FAILED":
      return "Check what you wrote: a comment needs text and a promotion needs its own declarations.";
    case "DISCUSSION_FORBIDDEN":
      return "You do not have permission to do this here. Promoting a proposal needs write access to the project.";
    case "DISCUSSION_PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "DISCUSSION_THREAD_NOT_FOUND":
      return "This discussion does not exist.";
    case "DISCUSSION_COMMENT_NOT_FOUND":
      return "This comment does not exist in this discussion.";
    case "DISCUSSION_COMMENT_DELETED":
      return "This comment was withdrawn, so it cannot be promoted.";
    case "DISCUSSION_TARGET_NOT_FOUND":
      return "The thing you are discussing does not exist in this project.";
    case "DISCUSSION_BRANCH_NOT_FOUND":
      return "The research branch you named does not exist in this project.";
    case "DISCUSSION_PROMOTION_NOT_FOUND":
      return "This promotion does not exist.";
    case "DISCUSSION_SERVICE_UNAVAILABLE":
      return "Discussion data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while working with this discussion. Please try again.";
  }
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

/** Options for createDiscussionsClient. */
export interface DiscussionsClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /**
   * The session-bound CSRF token for state changes (the API's guard
   * demands it). The module stays import-free, so the caller supplies the
   * source — the page reads lib/auth's sessionTokenStorage.
   */
  csrfToken?: () => string | null;
}

/** The discussions client surface one project page uses. */
export interface DiscussionsClient {
  listThreads(
    projectId: string,
    targetType: string,
    targetId: string,
  ): Promise<DiscussionThread[]>;
  getThread(projectId: string, threadId: string): Promise<ThreadDetail>;
  openThread(
    projectId: string,
    input: { targetType: string; targetId: string; body: string },
  ): Promise<ThreadDetail>;
  addComment(
    projectId: string,
    threadId: string,
    body: string,
  ): Promise<DiscussionComment>;
  withdrawComment(
    projectId: string,
    threadId: string,
    commentId: string,
  ): Promise<DiscussionComment>;
  promote(
    projectId: string,
    threadId: string,
    commentId: string,
    input: PromoteInput,
  ): Promise<PromotionResult>;
  promotionsForRef(
    projectId: string,
    ref: string,
  ): Promise<PromotionOrigin[]>;
  getPromotion(projectId: string, promotionId: string): Promise<PromotionOrigin>;
}

const CSRF_HEADER = "X-CSRF-Token";

/** Build the discussions client for one API origin. */
export function createDiscussionsClient(
  apiBaseUrl: string,
  options: DiscussionsClientOptions = {},
): DiscussionsClient {
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

  function asComment(body: unknown): DiscussionComment {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("thread_id" in body) || typeof body.thread_id !== "string" ||
      !("created_by" in body) || typeof body.created_by !== "string"
    ) {
      throw new Error("unexpected comment response shape");
    }
    return body as DiscussionComment;
  }

  function asThreadDetail(body: unknown): ThreadDetail {
    if (
      typeof body !== "object" || body === null ||
      !("thread" in body) || typeof body.thread !== "object" || body.thread === null ||
      !("comments" in body) || !Array.isArray(body.comments)
    ) {
      throw new Error("unexpected thread response shape");
    }
    const detail = body as ThreadDetail;
    return { thread: detail.thread, comments: detail.comments.map(asComment) };
  }

  function asOrigin(body: unknown): PromotionOrigin {
    if (
      typeof body !== "object" || body === null ||
      !("promotion" in body) || typeof body.promotion !== "object" || body.promotion === null ||
      !("thread" in body) || typeof body.thread !== "object" || body.thread === null ||
      !("comment" in body) || typeof body.comment !== "object" || body.comment === null
    ) {
      throw new Error("unexpected promotion response shape");
    }
    return body as PromotionOrigin;
  }

  /** The project-scoped discussions collection path. */
  function threadsPath(projectId: string): string {
    return `/api/v1/projects/${encodeURIComponent(projectId)}/discussions`;
  }

  return {
    async listThreads(projectId, targetType, targetId) {
      const query =
        `?target_type=${encodeURIComponent(targetType)}` +
        `&target_id=${encodeURIComponent(targetId)}`;
      const result = await call("GET", threadsPath(projectId) + query);
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("threads" in body) || !Array.isArray(body.threads)
      ) {
        throw new Error("unexpected thread list response shape");
      }
      return body.threads as DiscussionThread[];
    },
    async getThread(projectId, threadId) {
      const result = await call(
        "GET",
        `${threadsPath(projectId)}/${encodeURIComponent(threadId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asThreadDetail(result.body);
    },
    async openThread(projectId, input) {
      const result = await call("POST", threadsPath(projectId), {
        target_type: input.targetType,
        target_id: input.targetId,
        body: input.body,
      });
      if (result.status >= 400) fail(result.status, result.body);
      return asThreadDetail(result.body);
    },
    async addComment(projectId, threadId, body) {
      const result = await call(
        "POST",
        `${threadsPath(projectId)}/${encodeURIComponent(threadId)}/comments`,
        { body },
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asComment(result.body);
    },
    async withdrawComment(projectId, threadId, commentId) {
      const result = await call(
        "DELETE",
        `${threadsPath(projectId)}/${encodeURIComponent(threadId)}/comments/${encodeURIComponent(commentId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asComment(result.body);
    },
    async promote(projectId, threadId, commentId, input) {
      const payload: Record<string, unknown> = { kind: input.kind };
      if (input.title !== undefined) payload.title = input.title;
      if (input.issueType !== undefined) payload.issue_type = input.issueType;
      if (input.branchId !== undefined) payload.branch_id = input.branchId;
      if (input.questionId !== undefined) {
        payload.hypothesis = { question_id: input.questionId };
      }
      if (input.evidence !== undefined) {
        payload.evidence = {
          target_version_ref: input.evidence.targetVersionRef,
          evidence_version_ref: input.evidence.evidenceVersionRef,
          relation: input.evidence.relation,
          evidence_type: input.evidence.evidenceType,
          scope: input.evidence.scope,
          directness: input.evidence.directness,
          inference_nature: input.evidence.inferenceNature,
          reasoning_note: input.evidence.reasoningNote,
        };
      }
      const result = await call(
        "POST",
        `${threadsPath(projectId)}/${encodeURIComponent(threadId)}/comments/${encodeURIComponent(commentId)}/promotions`,
        payload,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asOrigin(result.body) as PromotionResult;
    },
    async promotionsForRef(projectId, ref) {
      const result = await call(
        "GET",
        `${threadsPath(projectId)}/promotions?ref=${encodeURIComponent(ref)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("promotions" in body) || !Array.isArray(body.promotions)
      ) {
        throw new Error("unexpected promotion list response shape");
      }
      return body.promotions.map(asOrigin);
    },
    async getPromotion(projectId, promotionId) {
      const result = await call(
        "GET",
        `${threadsPath(projectId)}/promotions/${encodeURIComponent(promotionId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asOrigin(result.body);
    },
  };
}
