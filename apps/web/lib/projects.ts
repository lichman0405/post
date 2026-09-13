/**
 * Client-side projects client for the Go API (T0108, T0109).
 *
 * Reads the project surface the shell and directory render from:
 * GET /api/v1/projects (the actor's projects) and
 * GET /api/v1/projects/{id} + /api/v1/projects/{id}/membership (the
 * shell's project state + the caller's own role). The settings surface
 * (T0109) adds the member list, the role-change write and the
 * purpose/activity-status edit. All calls carry the session cookie
 * (credentials: include); writes also head the session-bound CSRF token
 * when the caller supplied a source for it.
 *
 * The module is import-free by construction (like lib/auth.ts and
 * lib/profile.ts), so the node:test suite (projects.test.mjs) runs it
 * through Node's type stripping. The ApiError shape mirrors lib/auth.ts.
 */

/** The wire project shape (cmd/api/projectshttp projectPayload). */
export interface Project {
  id: string;
  organization_id: string | null;
  program_id: string | null;
  slug: string;
  name: string;
  purpose: string;
  activity_status: string;
  visibility: "public" | "private";
  main_frozen: boolean;
  git_repository_external_id: string | null;
  provision_status: string;
  created_by: string;
  created_at: string;
}

/** The project role a membership grants (internal/domain ProjectRole). */
export type ProjectRole = "owner" | "maintainer" | "contributor" | "viewer";

/** The wire membership shape (cmd/api/projectshttp membershipPayload). */
export interface ProjectMembership {
  project_id: string;
  user_id: string;
  role: ProjectRole;
  created_at: string;
}

/** The wire member-list row (cmd/api/projectshttp memberPayload). */
export interface ProjectMember {
  user_id: string;
  handle: string;
  display_name: string;
  role: ProjectRole;
  joined_at: string;
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

/** The projects client: one per API origin. */
export interface ProjectsClient {
  /** The projects the signed-in actor belongs to (any role). */
  list(): Promise<Project[]>;
  /** One project by id; 404 carries the existence-hiding envelope. */
  get(projectId: string): Promise<Project>;
  /**
   * The caller's own membership, or null when they hold none (the API
   * answers 404 PROJECT_MEMBERSHIP_NOT_FOUND for a project the caller
   * may read but is not a member of — "no role", not an error).
   */
  myMembership(projectId: string): Promise<ProjectMembership | null>;
  /**
   * The member list (identity + role + join date), owner/maintainer only
   * — the API refuses everyone else with SETTINGS_FORBIDDEN.
   */
  listMembers(projectId: string): Promise<ProjectMember[]>;
  /**
   * Change one member's role (owner/maintainer; the API enforces the
   * owner-management and last-owner rules). The session-bound CSRF token
   * rides along when the page provided one.
   */
  setMemberRole(projectId: string, userId: string, role: ProjectRole): Promise<ProjectMembership>;
  /**
   * Edit the project purpose and/or activity status. Visibility is
   * preview-only: sending it answers VISIBILITY_CHANGE_NOT_SUPPORTED, so
   * the client never offers it.
   */
  updateSettings(
    projectId: string,
    input: { purpose?: string; activity_status?: string },
  ): Promise<Project>;
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

/** Options for createProjectsClient. */
export interface ProjectsClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /**
   * The session-bound CSRF token for the write calls (the API's guard
   * demands it on every state change). The module stays import-free, so
   * the caller supplies the source — the settings page reads lib/auth's
   * sessionTokenStorage.
   */
  csrfToken?: () => string | null;
}

const CSRF_HEADER = "X-CSRF-Token";

/** Build the projects client for one API origin. */
export function createProjectsClient(
  apiBaseUrl: string,
  options: ProjectsClientOptions = {},
): ProjectsClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /**
   * One JSON call against the API; the session cookie rides along and the
   * CSRF token (when known) heads every write.
   */
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

  function asProject(body: unknown): Project {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("slug" in body) || typeof body.slug !== "string" ||
      !("name" in body) || typeof body.name !== "string" ||
      !("visibility" in body) || typeof body.visibility !== "string" ||
      !("main_frozen" in body) || typeof body.main_frozen !== "boolean"
    ) {
      throw new Error("unexpected project response shape");
    }
    return body as Project;
  }

  function asMembership(body: unknown): ProjectMembership {
    if (
      typeof body !== "object" || body === null ||
      !("project_id" in body) || typeof body.project_id !== "string" ||
      !("user_id" in body) || typeof body.user_id !== "string" ||
      !("role" in body) || typeof body.role !== "string"
    ) {
      throw new Error("unexpected membership response shape");
    }
    return body as ProjectMembership;
  }

  function asMember(body: unknown): ProjectMember {
    if (
      typeof body !== "object" || body === null ||
      !("user_id" in body) || typeof body.user_id !== "string" ||
      !("handle" in body) || typeof body.handle !== "string" ||
      !("display_name" in body) || typeof body.display_name !== "string" ||
      !("role" in body) || typeof body.role !== "string" ||
      !("joined_at" in body) || typeof body.joined_at !== "string"
    ) {
      throw new Error("unexpected member response shape");
    }
    return body as ProjectMember;
  }

  return {
    async list() {
      const result = await call("GET", "/api/v1/projects");
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("projects" in body) || !Array.isArray(body.projects)
      ) {
        throw new Error("unexpected project list response shape");
      }
      return body.projects.map(asProject);
    },
    async get(projectId) {
      const result = await call("GET", `/api/v1/projects/${encodeURIComponent(projectId)}`);
      if (result.status >= 400) fail(result.status, result.body);
      return asProject(result.body);
    },
    async myMembership(projectId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/membership`,
      );
      if (result.status === 404) {
        // PROJECT_MEMBERSHIP_NOT_FOUND — "no role", not an error state.
        return null;
      }
      if (result.status >= 400) fail(result.status, result.body);
      return asMembership(result.body);
    },
    async listMembers(projectId) {
      const result = await call(
        "GET",
        `/api/v1/projects/${encodeURIComponent(projectId)}/members`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("members" in body) || !Array.isArray(body.members)
      ) {
        throw new Error("unexpected member list response shape");
      }
      return body.members.map(asMember);
    },
    async setMemberRole(projectId, userId, role) {
      const result = await call(
        "PUT",
        `/api/v1/projects/${encodeURIComponent(projectId)}/members/${encodeURIComponent(userId)}`,
        { role },
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asMembership(result.body);
    },
    async updateSettings(projectId, input) {
      const result = await call(
        "PATCH",
        `/api/v1/projects/${encodeURIComponent(projectId)}`,
        input,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asProject(result.body);
    },
  };
}

/** Roles that may manage project settings (the Settings tab gate). */
export function mayManageSettings(role: ProjectRole | null): boolean {
  return role === "owner" || role === "maintainer";
}

/** Human-facing message for a stable project API code. */
export function messageForProjectCode(code: string): string {
  switch (code) {
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "PROJECT_MEMBERSHIP_NOT_FOUND":
      return "You are not a member of this project.";
    case "SETTINGS_FORBIDDEN":
      return "Only owners and maintainers can change project settings.";
    case "MEMBER_NOT_FOUND":
      return "That member is no longer part of this project.";
    case "LAST_OWNER":
      return "A project must keep at least one owner.";
    case "SELF_ROLE_CHANGE_FORBIDDEN":
      return "You cannot change your own role.";
    case "OWNER_ROLE_CHANGE_FORBIDDEN":
      return "Only an owner can grant or revoke the owner role.";
    case "VISIBILITY_CHANGE_NOT_SUPPORTED":
      return "Changing visibility is not available yet.";
    case "CSRF_FAILED":
      return "Your session security token was stale. Reload the page and try again.";
    case "SERVICE_UNAVAILABLE":
      return "Project data is temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong while loading project data. Please try again.";
  }
}
