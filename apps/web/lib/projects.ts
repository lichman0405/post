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
 * The create form's input (WEB-04, D16).
 *
 * `visibility` is required and carries no default here on purpose: D16's
 * "default Public" is a property of the FORM (the control is preselected
 * Public, and the reader may switch it to Private before submitting), not of
 * this client. A client that silently substituted "public" for a missing
 * value would make an accidental publish the quiet outcome of a bug.
 */
export interface CreateProjectInput {
  slug: string;
  name: string;
  /** Required by the API (docs/05: a project states what it is for). */
  purpose: string;
  visibility: "public" | "private";
  /** Personal project when null (no organization). */
  organization_id?: string | null;
  program_id?: string | null;
}

/** The 201 body: the new project and the caller's owner membership. */
export interface CreateProjectResult {
  project: Project;
  membership: ProjectMembership;
}

/**
 * The form field a create refusal belongs to, or null for a form-level
 * message (D16/U01: "a duplicate slug shows a field error").
 *
 * The API answers stable codes, not field names; this module is the one
 * place that knows which input produced which code, so the form can put the
 * sentence next to the offending control instead of stacking it in a banner
 * the reader has to map back onto a field.
 */
export type CreateField = "slug" | "name" | "purpose" | "visibility";

/**
 * createFieldForError maps a create refusal onto the field that caused it.
 *
 * PROJECT_SLUG_TAKEN is unambiguously the slug. VALIDATION_FAILED reaches the
 * wire as `projects: invalid input: <rule>` (ErrValidation's text plus the
 * service's `%w: …` rule), so the rule is read AFTER the `invalid input: `
 * marker rather than from the start of the message — the old leading-word
 * check could never match a sentence that begins with the service name.
 * The rule phrases are the service's own (internal/application/projects
 * service.go); a message that names none of them (a malformed body, a
 * program/organization mismatch) is form-level, and null says so rather than
 * guessing a field the reader would not recognise.
 */
const VALIDATION_RULE_MARKER = "invalid input: ";

export function createFieldForError(err: unknown): CreateField | null {
  if (!(err instanceof ApiError)) return null;
  if (err.code === "PROJECT_SLUG_TAKEN") return "slug";
  if (err.code !== "VALIDATION_FAILED") return null;
  const text = err.message.toLowerCase();
  const markerAt = text.indexOf(VALIDATION_RULE_MARKER);
  const rule = markerAt >= 0 ? text.slice(markerAt + VALIDATION_RULE_MARKER.length) : text;
  if (rule.startsWith("slug may only contain")) return "slug";
  if (rule.startsWith("name is required")) return "name";
  if (rule.startsWith("purpose is required")) return "purpose";
  if (rule.startsWith("visibility must be")) return "visibility";
  return null;
}

/** One organization the create form can put a project in (the caller's). */
export interface ProjectOrganization {
  id: string;
  slug: string;
  name: string;
  deactivated_at: string | null;
}

/**
 * A create-form field's own problem, as a stable code the page resolves to
 * catalog copy: no value at all, or a value the shape rule refuses. null
 * means "nothing wrong that can be decided here".
 *
 * The rules below mirror internal/domain (ValidProjectSlug/Name/Purpose)
 * because a form that only learns "wrong" after a round trip is a form the
 * reader has to fight. They are NOT the authority: the API validates again
 * and its refusal wins, which is why the codes — not the sentences — travel
 * out of here.
 */
export type CreateFieldProblem = "required" | "invalid";

/** The slug shape after normalization: [a-z0-9-], 1..64, no edge dash. */
export function projectSlugProblem(slug: string): CreateFieldProblem | null {
  const n = slug.trim().toLowerCase();
  if (n === "") return "required";
  if (n.length > 64) return "invalid";
  if (n.startsWith("-") || n.endsWith("-")) return "invalid";
  if (!/^[a-z0-9-]+$/.test(n)) return "invalid";
  return null;
}

/** The API's own length unit: internal/domain measures `len(n)` in BYTES, not
 *  UTF-16 code units. Mirroring bytes here keeps the form from accepting a
 *  name the server will refuse (200 CJK characters are 600 bytes). */
function utf8Bytes(value: string): number {
  return new TextEncoder().encode(value).length;
}

/** A project name is non-blank and at most 200 bytes (the API's limit). */
export function projectNameProblem(name: string): CreateFieldProblem | null {
  const n = name.trim();
  if (n === "") return "required";
  if (utf8Bytes(n) > 200) return "invalid";
  return null;
}

/** A purpose is non-blank and at most 4000 bytes (D16: required). */
export function projectPurposeProblem(purpose: string): CreateFieldProblem | null {
  const n = purpose.trim();
  if (n === "") return "required";
  if (utf8Bytes(n) > 4000) return "invalid";
  return null;
}

/**
 * suggestProjectSlug derives an address from a typed name, so the reader
 * does not have to invent one: lowercase, every run of non-address
 * characters collapsed to a single dash, no edge dash, at most 64.
 *
 * It is a SUGGESTION, never a decision: the form stops applying it the
 * moment the reader edits the address itself (see project-directory.tsx),
 * and the server still normalizes and validates whatever is submitted.
 */
export function suggestProjectSlug(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 64)
    .replace(/-+$/g, "");
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
  /**
   * Create a project (POST /api/v1/projects). The caller becomes its owner
   * and the project comes back provision-pending. A slug already in use
   * answers PROJECT_SLUG_TAKEN (409) — see createFieldForError.
   */
  create(input: CreateProjectInput): Promise<CreateProjectResult>;
  /**
   * The organizations the caller may create a project in — the create
   * form's picker source (GET /api/v1/organizations, a member-only read
   * owned by the organization surface; WEB-03 builds that surface's own
   * client, this is only the read the create form needs).
   */
  myOrganizations(): Promise<ProjectOrganization[]>;
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
    // The CSRF header's name is written out here rather than held in a
    // constant and assigned through `headers[NAME] = …`: a computed member
    // write is what the SAST rule set refuses by rule (a plain object reached
    // with a variable key), and the literal is the clearer spelling at the one
    // place it is used.
    const csrf = options.csrfToken?.() ?? null;
    const headers: Record<string, string> =
      csrf === null ? {} : { "X-CSRF-Token": csrf };
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

  function asOrganization(body: unknown): ProjectOrganization {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("slug" in body) || typeof body.slug !== "string" ||
      !("name" in body) || typeof body.name !== "string"
    ) {
      throw new Error("unexpected organization response shape");
    }
    const deactivated = "deactivated_at" in body ? body.deactivated_at : null;
    return {
      id: body.id,
      slug: body.slug,
      name: body.name,
      deactivated_at: typeof deactivated === "string" ? deactivated : null,
    };
  }

  /** The 201 envelope: the project plus the caller's owner membership. */
  function asCreateResult(body: unknown): CreateProjectResult {
    if (
      typeof body !== "object" || body === null ||
      !("project" in body) || !("membership" in body)
    ) {
      throw new Error("unexpected project create response shape");
    }
    return {
      project: asProject(body.project),
      membership: asMembership(body.membership),
    };
  }

  return {
    async create(input) {
      // The body carries the fields the caller set. `null` organization and
      // program mean "personal project" and are sent as-is, because the API
      // distinguishes an absent id from an empty string on purpose (an
      // empty string is refused, never read as "none").
      const result = await call("POST", "/api/v1/projects", {
        slug: input.slug,
        name: input.name,
        purpose: input.purpose,
        visibility: input.visibility,
        organization_id: input.organization_id ?? null,
        program_id: input.program_id ?? null,
      });
      if (result.status >= 400) fail(result.status, result.body);
      return asCreateResult(result.body);
    },
    async myOrganizations() {
      const result = await call("GET", "/api/v1/organizations");
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("organizations" in body) || !Array.isArray(body.organizations)
      ) {
        throw new Error("unexpected organization list response shape");
      }
      return body.organizations.map(asOrganization);
    },
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

/**
 * Human-facing message for a stable project API code.
 *
 * `fallback` is the sentence for a code this module does not know, because
 * the default one names a READ ("loading project data") and the create form
 * needs a sentence about a WRITE that did not happen. Callers that only ever
 * read keep the default.
 */
export function messageForProjectCode(
  code: string,
  fallback = "Something went wrong while loading project data. Please try again.",
): string {
  switch (code) {
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "PROJECT_MEMBERSHIP_NOT_FOUND":
      return "You are not a member of this project.";
    case "PROJECT_SLUG_TAKEN":
      return "A project with this address already exists. Choose a different one.";
    case "PROJECT_FORBIDDEN":
      return "You are not an active member of this organization, so you cannot create a project in it.";
    case "ORG_NOT_FOUND":
      return "That organization does not exist.";
    case "ORG_DEACTIVATED":
      return "That organization is deactivated; it cannot hold new projects.";
    case "PROGRAM_NOT_FOUND":
      return "That program does not exist.";
    case "PROGRAM_ORG_MISMATCH":
      return "That program belongs to a different organization.";
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
      return fallback;
  }
}
