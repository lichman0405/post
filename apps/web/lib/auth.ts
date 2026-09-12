/**
 * Client-side authentication client for the Go API (T0101).
 *
 * The web app has no backend of its own: these helpers call the API's
 * /api/v1/auth surface and carry the session cookie (credentials: include)
 * plus the session-bound CSRF token in X-CSRF-Token on every state change,
 * exactly as the API's write guard requires (cmd/api/authhttp).
 *
 * The module is import-free by construction (like lib/config.ts), so the
 * node:test suite (auth.test.mjs) runs it through Node's type stripping.
 */

/** The wire user shape (cmd/api/authhttp userPayload). */
export interface AuthUser {
  id: string;
  handle: string;
  email: string;
  display_name: string;
}

/** The wire session payload: the user plus the CSRF token to echo back. */
export interface AuthSession {
  user: AuthUser;
  csrf_token: string;
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

const STORAGE_KEY = "post.csrf";

const CSRF_HEADER = "X-CSRF-Token";

/** The auth client: one per API origin. */
export interface AuthClient {
  /** Sign up with email + password; the session cookie arrives by Set-Cookie. */
  signup(input: {
    email: string;
    password: string;
    handle?: string;
    displayName?: string;
  }): Promise<AuthSession>;
  /** Log in with email + password. */
  login(email: string, password: string): Promise<AuthSession>;
  /** Revoke the session (CSRF-bound). */
  logout(): Promise<void>;
  /** Whoami; resolves null when there is no active session (401). */
  session(): Promise<AuthSession | null>;
  /** The OIDC provider's authorize URL (starts the browser redirect). */
  oidcAuthorizeUrl(): Promise<string>;
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

/** Options for createAuthClient. */
export interface AuthClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
  /** CSRF token storage; default: sessionStorage-backed. */
  storage?: TokenStorage;
}

/** Build the auth client for one API origin. */
export function createAuthClient(
  apiBaseUrl: string,
  options: AuthClientOptions = {},
): AuthClient {
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
      parsed = null; // 204/redirects have no JSON body
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

  function asSession(body: unknown): AuthSession {
    if (
      typeof body !== "object" || body === null ||
      !("csrf_token" in body) || typeof body.csrf_token !== "string" ||
      !("user" in body) || typeof body.user !== "object" || body.user === null
    ) {
      throw new Error("unexpected auth response shape");
    }
    return body as AuthSession;
  }

  async function startSession(result: { status: number; body: unknown }): Promise<AuthSession> {
    if (result.status >= 400) fail(result.status, result.body);
    const session = asSession(result.body);
    storage.set(session.csrf_token);
    return session;
  }

  return {
    async signup(input) {
      const result = await call("POST", "/api/v1/auth/signup", {
        email: input.email,
        password: input.password,
        handle: input.handle ?? "",
        display_name: input.displayName ?? "",
      });
      return startSession(result);
    },
    async login(email, password) {
      const result = await call("POST", "/api/v1/auth/login", {
        email,
        password,
      });
      return startSession(result);
    },
    async logout() {
      const result = await call("POST", "/api/v1/auth/logout");
      storage.clear();
      if (result.status >= 400 && result.status !== 401) fail(result.status, result.body);
    },
    async session() {
      const result = await call("GET", "/api/v1/auth/session");
      if (result.status === 401) {
        storage.clear();
        return null;
      }
      if (result.status >= 400) fail(result.status, result.body);
      const session = asSession(result.body);
      storage.set(session.csrf_token);
      return session;
    },
    async oidcAuthorizeUrl() {
      const result = await call("GET", "/api/v1/auth/oidc/authorize-url");
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (typeof body !== "object" || body === null ||
          !("authorize_url" in body) || typeof body.authorize_url !== "string") {
        throw new Error("unexpected authorize-url response shape");
      }
      return body.authorize_url;
    },
    csrfToken: () => storage.get(),
  };
}

/** Human-facing message for a stable API code (login/signup errors). */
export function messageForCode(code: string): string {
  switch (code) {
    case "AUTH_INVALID_CREDENTIALS":
      return "Invalid email or password.";
    case "EMAIL_ALREADY_REGISTERED":
      return "An account with this email already exists.";
    case "VALIDATION_FAILED":
      return "Please check the form: the input is not valid.";
    case "RATE_LIMITED":
      return "Too many attempts. Please wait a moment and try again.";
    case "CSRF_FAILED":
      return "The request was rejected as cross-site. Please reload and try again.";
    case "SERVICE_UNAVAILABLE":
      return "Sign-in is temporarily unavailable. Please try again later.";
    case "OIDC_NOT_CONFIGURED":
      return "Sign-in with an external provider is not configured here.";
    case "OIDC_PROVIDER_FAILED":
      return "The identity provider failed. Please try again.";
    case "OIDC_EMAIL_NOT_VERIFIED":
      return "The provider did not verify your email address.";
    default:
      return "Something went wrong. Please try again.";
  }
}

/**
 * Human-facing message for an OIDC callback failure code. The API redirects
 * failed flows to /login?error=<code> (cmd/api/authhttp handleOIDCCallback);
 * the login page renders this so the user knows why external sign-in
 * failed instead of landing on a silent form.
 */
export function messageForOIDCError(code: string): string {
  switch (code) {
    case "state_mismatch":
      return "The external sign-in request could not be verified. Please try again.";
    case "no_code":
      return "The provider returned no sign-in code. Please try again.";
    case "email_not_verified":
      return "The provider did not verify your email address.";
    case "provider_failed":
      return "The identity provider failed. Please try again.";
    default:
      return "External sign-in failed. Please try again.";
  }
}
