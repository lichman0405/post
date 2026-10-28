/**
 * Client-side organization client for the Go API (T1304 / WEB-03, G03).
 *
 * The web app has no backend of its own (docs/52): organization creation,
 * the member list, invites, role changes and end-of-affiliation all travel
 * to /api/v1/organizations with the session cookie (credentials: include)
 * and the session-bound CSRF token on every state change — exactly as the
 * API's write guard requires (cmd/api/authhttp). The token source is the
 * SAME "post.csrf" sessionStorage entry lib/auth.ts uses, so one sign-in
 * serves every client in the tab.
 *
 * The server is the authorization boundary; this module adds no policy of
 * its own. It exists so the page never re-implements wire shapes, and so a
 * refusal arrives as the API's stable code rather than as a raw status.
 */

import { sessionTokenStorage, type TokenStorage } from "../../../lib/auth";

/** The four organization roles (internal/domain.Organization roles). */
export type OrgRole = "owner" | "maintainer" | "contributor" | "viewer";

/** Every bounded role, in the governance order the UI offers them. */
export const ORG_ROLES: OrgRole[] = ["owner", "maintainer", "contributor", "viewer"];

/** The wire organization shape (cmd/api/orgshttp orgPayload). */
export interface Organization {
  id: string;
  slug: string;
  name: string;
  description: string;
  created_at: string;
  deactivated_at: string | null;
  /** Reported on the single-organization read only (docs/45); absent in
   *  the directory list. */
  attestation_attribution?: string;
}

/** The wire membership shape (cmd/api/orgshttp membershipPayload). */
export interface OrganizationMembership {
  organization_id: string;
  user_id: string;
  role: OrgRole;
  /** Calendar date "YYYY-MM-DD" (the canonical column is date). */
  affiliation_start: string;
  /** Calendar date while the affiliation has ended; null while current. */
  affiliation_end: string | null;
  verified: boolean;
}

/** The API error envelope (docs/22 §5): stable codes, no stack traces. */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/** ApiError carries the API's envelope (same shape as lib/auth.ts). */
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

/** The organization client: one per API origin. */
export interface OrgClient {
  /** The organizations the signed-in actor currently belongs to. */
  list(): Promise<Organization[]>;
  /** Create an organization; the actor becomes its owner. */
  create(input: {
    slug: string;
    name: string;
    description: string;
  }): Promise<{ organization: Organization; membership: OrganizationMembership }>;
  /** Every membership — current AND historical (history is never deleted). */
  listMembers(organizationId: string): Promise<OrganizationMembership[]>;
  /** Invite an existing account (by handle) with a role. Owner only. */
  invite(
    organizationId: string,
    input: {
      handle: string;
      role: OrgRole;
      affiliationStart?: string;
      verified?: boolean;
    },
  ): Promise<OrganizationMembership>;
  /** Change one member's role. Owner only (a member cannot promote itself). */
  updateRole(
    organizationId: string,
    userId: string,
    role: OrgRole,
  ): Promise<OrganizationMembership>;
  /**
   * End an affiliation (leave, or remove someone). The row is kept: 离职不
   * 删除历史. An owner may end anyone's; any member may end their own.
   */
  endAffiliation(organizationId: string, userId: string): Promise<void>;
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

/** Options for createOrgClient. */
export interface OrgClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /** CSRF token storage; default: the shared session storage ("post.csrf"). */
  storage?: TokenStorage;
  /** Explicit token source; wins over `storage` when supplied. */
  csrfToken?: () => string | null;
}

export function createOrgClient(
  apiBaseUrl: string,
  options: OrgClientOptions = {},
): OrgClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);
  const storage = options.storage ?? sessionTokenStorage();
  const token = options.csrfToken ?? (() => storage.get());

  async function call(
    method: string,
    path: string,
    body?: unknown,
  ): Promise<{ status: number; body: unknown }> {
    const headers: Record<string, string> = {};
    const csrf = token();
    // Literal keys only: the header name is a fixed protocol constant, so it
    // belongs in the object literal beside Content-Type rather than in a
    // computed property a scanner has to treat as caller-influenced
    // (SAST security/detect-object-injection; the same shape lib/profile.ts
    // uses). It MUST stay "X-CSRF-Token" — the name lib/auth.ts and the API
    // guard both use.
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
      parsed = null; // 204 has no JSON body
    }
    return { status: res.status, body: parsed };
  }

  function fail(status: number, body: unknown): never {
    if (typeof body === "object" && body !== null && "code" in body) {
      throw new ApiError(status, body as ErrorEnvelope);
    }
    throw new ApiError(status, {
      code: "UNKNOWN",
      message: "the server answered unexpectedly; try again",
    });
  }

  function asOrganization(body: unknown): Organization {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("slug" in body) || typeof body.slug !== "string" ||
      !("name" in body) || typeof body.name !== "string"
    ) {
      throw new Error("unexpected organization response shape");
    }
    return body as Organization;
  }

  function asMembership(body: unknown): OrganizationMembership {
    if (
      typeof body !== "object" || body === null ||
      !("organization_id" in body) || typeof body.organization_id !== "string" ||
      !("user_id" in body) || typeof body.user_id !== "string" ||
      !("role" in body) || typeof body.role !== "string" ||
      !("affiliation_start" in body) || typeof body.affiliation_start !== "string" ||
      !("verified" in body) || typeof body.verified !== "boolean"
    ) {
      throw new Error("unexpected membership response shape");
    }
    return body as OrganizationMembership;
  }

  return {
    async list() {
      const result = await call("GET", "/api/v1/organizations");
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (typeof body !== "object" || body === null || !("organizations" in body)) {
        throw new Error("unexpected organization list response shape");
      }
      const raw = (body as { organizations: unknown }).organizations;
      if (!Array.isArray(raw)) {
        throw new Error("unexpected organization list response shape");
      }
      return raw.map(asOrganization);
    },
    async create(input) {
      const result = await call("POST", "/api/v1/organizations", input);
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("organization" in body) || !("membership" in body)
      ) {
        throw new Error("unexpected organization create response shape");
      }
      return {
        organization: asOrganization((body as { organization: unknown }).organization),
        membership: asMembership((body as { membership: unknown }).membership),
      };
    },
    async listMembers(organizationId) {
      const result = await call(
        "GET",
        `/api/v1/organizations/${encodeURIComponent(organizationId)}/members`,
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (typeof body !== "object" || body === null || !("members" in body)) {
        throw new Error("unexpected member list response shape");
      }
      const raw = (body as { members: unknown }).members;
      if (!Array.isArray(raw)) {
        throw new Error("unexpected member list response shape");
      }
      return raw.map(asMembership);
    },
    async invite(organizationId, input) {
      const result = await call(
        "POST",
        `/api/v1/organizations/${encodeURIComponent(organizationId)}/members`,
        {
          handle: input.handle,
          role: input.role,
          affiliation_start: input.affiliationStart ?? "",
          verified: input.verified ?? false,
        },
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asMembership(result.body);
    },
    async updateRole(organizationId, userId, role) {
      const result = await call(
        "PATCH",
        `/api/v1/organizations/${encodeURIComponent(organizationId)}/members/${encodeURIComponent(userId)}`,
        { role },
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asMembership(result.body);
    },
    async endAffiliation(organizationId, userId) {
      const result = await call(
        "DELETE",
        `/api/v1/organizations/${encodeURIComponent(organizationId)}/members/${encodeURIComponent(userId)}`,
      );
      if (result.status >= 400) fail(result.status, result.body);
    },
    csrfToken: token,
  };
}

/** True while the affiliation is current (the API keeps ended rows). */
export function membershipActive(m: OrganizationMembership): boolean {
  return m.affiliation_end === null;
}

/** A member's human-facing role word. The wire role stays a stable code. */
export function roleLabel(role: OrgRole): string {
  switch (role) {
    case "owner":
      return "Owner";
    case "maintainer":
      return "Maintainer";
    case "contributor":
      return "Contributor";
    case "viewer":
      return "Viewer";
  }
}

/**
 * Human-facing message for a stable organization API code. These are the
 * refusal-path sentences only; the ordinary page copy is the catalog's (or,
 * for this feature, the component's own — the organizations management
 * surface is outside the i18n core group, see the task RESULT's coverage
 * table).
 */
export function messageForOrgCode(code: string): string {
  switch (code) {
    case "ORG_NOT_FOUND":
      return "This organization does not exist, or you are not a member of it.";
    case "ORG_SLUG_TAKEN":
      return "An organization with this slug already exists.";
    case "ORG_FORBIDDEN":
      return "Only organization owners may perform this action.";
    case "ORG_DEACTIVATED":
      return "This organization is deactivated; its governance is frozen.";
    case "MEMBER_NOT_FOUND":
      return "That membership does not exist.";
    case "MEMBER_ALREADY_EXISTS":
      return "That person is already a member of this organization.";
    case "LAST_OWNER":
      return "The organization must keep at least one active owner.";
    case "USER_NOT_FOUND":
      return "No account with that handle exists.";
    case "VALIDATION_FAILED":
      return "Please check the form: the input is not valid.";
    case "CSRF_FAILED":
      return "The request was rejected as cross-site. Please reload and try again.";
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to manage this organization.";
    case "SERVICE_UNAVAILABLE":
      return "Organizations are temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong. Please try again.";
  }
}
