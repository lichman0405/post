/**
 * Client-side profile client for the Go API (T0102).
 *
 * Reads the public profile surface (GET /api/v1/users/{id}/profile and
 * /users/by-handle/{handle}/profile — both anonymous-readable, no CSRF
 * needed) and applies the owner's edits (PATCH) with the session-bound
 * CSRF token, exactly as the API's write guard requires.
 *
 * The module is import-free by construction (like lib/auth.ts and
 * lib/config.ts), so the node:test suite (profile.test.mjs) runs it
 * through Node's type stripping. The ApiError/sessionTokenStorage shapes
 * deliberately mirror lib/auth.ts — the storage key is the SAME
 * "post.csrf" key, so both clients share one CSRF token per browser tab.
 */

/** The public profile wire shape (cmd/api/profilehttp publicProfilePayload).
 *  Email is deliberately absent: the API never sends it here. */
export interface ProfileUser {
  id: string;
  handle: string;
  display_name: string;
  bio: string;
  created_at: string;
}

/** The editable field set (cmd/api/profilehttp profileUpdateRequest). */
export interface ProfileUpdate {
  handle?: string;
  display_name?: string;
  bio?: string;
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

/** Where the CSRF token survives a reload (sessionStorage, per tab). */
export interface TokenStorage {
  get(): string | null;
  set(token: string): void;
  clear(): void;
}

/**
 * The shared CSRF storage key — MUST match lib/auth.ts: the token is
 * session-bound, one per tab, and both clients read/write the same one.
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

/** The profile client: one per API origin. */
export interface ProfileClient {
  /** Public profile by user id — the stable URL (survives handle renames). */
  get(userId: string): Promise<ProfileUser>;
  /** Public profile by current handle (a convenience lookup). */
  getByHandle(handle: string): Promise<ProfileUser>;
  /** Apply the owner's edits; the CSRF token is attached from storage. */
  update(userId: string, patch: ProfileUpdate): Promise<ProfileUser>;
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

/** Options for createProfileClient. */
export interface ProfileClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /** CSRF token storage; default: the shared session storage ("post.csrf"). */
  storage?: TokenStorage;
}

/** Build the profile client for one API origin. */
export function createProfileClient(
  apiBaseUrl: string,
  options: ProfileClientOptions = {},
): ProfileClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);
  const storage = options.storage ?? sessionTokenStorage();

  /** One JSON call to the API; carries the CSRF token when known. */
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
      parsed = null;
    }
    return { status: res.status, body: parsed };
  }

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

  function asProfile(body: unknown): ProfileUser {
    if (
      typeof body !== "object" || body === null ||
      !("id" in body) || typeof body.id !== "string" ||
      !("handle" in body) || typeof body.handle !== "string" ||
      !("display_name" in body) || typeof body.display_name !== "string" ||
      !("bio" in body) || typeof body.bio !== "string" ||
      !("created_at" in body) || typeof body.created_at !== "string"
    ) {
      throw new Error("unexpected profile response shape");
    }
    return body as ProfileUser;
  }

  async function fetchProfile(path: string): Promise<ProfileUser> {
    const result = await call("GET", path);
    if (result.status >= 400) fail(result.status, result.body);
    return asProfile(result.body);
  }

  return {
    get: (userId) =>
      fetchProfile(`/api/v1/users/${encodeURIComponent(userId)}/profile`),
    getByHandle: (handle) =>
      fetchProfile(`/api/v1/users/by-handle/${encodeURIComponent(handle)}/profile`),
    async update(userId, patch) {
      const result = await call(
        "PATCH",
        `/api/v1/users/${encodeURIComponent(userId)}/profile`,
        patch,
      );
      if (result.status >= 400) fail(result.status, result.body);
      return asProfile(result.body);
    },
    csrfToken: () => storage.get(),
  };
}

/** Human-facing message for a stable profile API code. */
export function messageForProfileCode(code: string): string {
  switch (code) {
    case "USER_NOT_FOUND":
      return "This user does not exist.";
    case "AUTH_FORBIDDEN":
      return "Only the profile owner can edit these fields.";
    case "AUTH_UNAUTHENTICATED":
      return "Sign in to edit this profile.";
    case "VALIDATION_FAILED":
      return "Please check the form: the input is not valid.";
    case "HANDLE_ALREADY_TAKEN":
      return "This handle is already taken.";
    case "CSRF_FAILED":
      return "The request was rejected as cross-site. Please reload and try again.";
    case "SERVICE_UNAVAILABLE":
      return "Profiles are temporarily unavailable. Please try again later.";
    default:
      return "Something went wrong. Please try again.";
  }
}
