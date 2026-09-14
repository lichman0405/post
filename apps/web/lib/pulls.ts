/**
 * Client-side pull-request client for the Go API (T0403).
 *
 * Reads the PR surface the Pull requests tab renders:
 * GET /api/v1/projects/{id}/pull-requests (the project's PR list),
 * GET /api/v1/projects/{id}/pull-requests/{number} (one PR) and
 * GET /api/v1/projects/{id}/pull-requests/{number}/checks (the machine
 * integrity report the detail page's checks section renders). All calls
 * carry the session cookie (credentials: include); everything here is a
 * read, so no CSRF token rides along (the same discipline as
 * lib/projects.ts reads).
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

/** The pulls client: one per API origin. */
export interface PullsClient {
  /** The project's pull requests, oldest first (the API's number order). */
  list(projectId: string): Promise<PullRequest[]>;
  /** One PR by number; 404 carries the existence-hiding envelope. */
  get(projectId: string, number: number): Promise<PullRequest>;
  /** The PR's machine integrity report. */
  checks(projectId: string, number: number): Promise<IntegrityReport>;
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
}

/** Build the pulls client for one API origin. */
export function createPullsClient(
  apiBaseUrl: string,
  options: PullsClientOptions = {},
): PullsClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /** One JSON read against the API; the session cookie rides along. */
  async function call(method: string, path: string): Promise<{ status: number; body: unknown }> {
    const res = await fetchFn(apiBaseUrl + path, {
      method,
      headers: {},
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
    case "SERVICE_UNAVAILABLE":
      return "Pull request data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while loading pull request data. Please try again.";
  }
}
