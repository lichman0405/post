/**
 * Client-side files client for the read-only Files API (T0308 UI over the
 * T0307 surface).
 *
 * Wraps the four JSON reads the Files page renders —
 *   GET /api/v1/projects/{id}/files/tree?ref=&path=
 *   GET /api/v1/projects/{id}/files/content?ref=&path=
 *   GET /api/v1/projects/{id}/files/history?ref=&path=&limit=
 * — and builds the two stream URLs the page links to (never fetches):
 *   GET /api/v1/projects/{id}/files/raw?ref=&path=   (the download link)
 *   GET /api/v1/projects/{id}/files/diff?sha=        (the raw diff link)
 *
 * The client is read-only by construction: it has no method that sends
 * anything but GET, and the two URL builders exist precisely so the page
 * renders plain <a href> links instead of mutating calls. All calls carry
 * the session cookie (credentials: include) — anonymous callers read
 * public projects only, exactly like the projects client.
 *
 * The module is import-free by construction (like lib/auth.ts and
 * lib/projects.ts), so the node:test suite (files.test.mjs) runs it
 * through Node's type stripping. The ApiError shape mirrors lib/projects.ts.
 */

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
 * (Same shape as lib/auth.ts / lib/projects.ts ApiError.)
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

/** The wire tree-entry shape (cmd/api/fileshttp treeEntryPayload). */
export interface FilesTreeEntry {
  name: string;
  path: string;
  /** "blob", "tree", "symlink" or "gitlink" — derived from the git mode. */
  type: string;
  /** The raw git mode ("100644", "040000", …). */
  mode: string;
  size: number;
  sha: string;
}

/** The wire tree-listing shape (cmd/api/fileshttp treePayload). */
export interface FilesTreeListing {
  ref: string;
  path: string;
  sha: string;
  entries: FilesTreeEntry[];
}

/** The displayed form of a blob-pointer manifest (docs/17 §2). */
export interface FilesBlobPointer {
  content_hash: string;
  size_bytes: number;
  blob_id?: string;
}

/** The wire file-view shape (cmd/api/fileshttp filePayload). */
export interface FilesFileView {
  name: string;
  path: string;
  type: string;
  mode: string;
  size: number;
  sha: string;
  /** "text", "binary", "too_large", "symlink" or "gitlink". */
  kind: string;
  content: string;
  truncated: boolean;
  target?: string;
  blob_pointer?: FilesBlobPointer;
}

/** The wire commit shape (cmd/api/fileshttp commitPayload). */
export interface FilesCommitEntry {
  sha: string;
  author: string;
  author_email: string;
  date: string;
  message: string;
}

/** Options shared by the JSON reads. */
export interface FilesReadOptions {
  /** Branch name or full commit SHA; "" = the canonical main branch. */
  ref?: string;
  path?: string;
}

/** The files client: one per API origin. */
export interface FilesClient {
  /** One directory listing at ref/path ("" = root of main). */
  tree(projectId: string, opts: FilesReadOptions): Promise<FilesTreeListing>;
  /** One file's safe preview (the content endpoint reads blobs only). */
  content(projectId: string, opts: FilesReadOptions): Promise<FilesFileView>;
  /** The commit history touching path at ref, newest first. */
  history(projectId: string, opts: FilesReadOptions & { limit?: number }): Promise<FilesCommitEntry[]>;
  /** The download link for one file (the raw channel — plain <a href>). */
  rawUrl(projectId: string, opts: { ref?: string; path: string }): string;
  /** The raw diff link for one commit (the patch channel — plain <a href>). */
  diffUrl(projectId: string, sha: string): string;
}

type FetchLike = (
  input: string,
  init?: { method?: string; headers?: Record<string, string>; credentials?: string },
) => Promise<{ status: number; json(): Promise<unknown> }>;

/** Options for createFilesClient. */
export interface FilesClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/** Build the files client for one API origin. */
export function createFilesClient(
  apiBaseUrl: string,
  options: FilesClientOptions = {},
): FilesClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  /** One GET against the API; the session cookie rides along. */
  async function call(path: string): Promise<{ status: number; body: unknown }> {
    const res = await fetchFn(apiBaseUrl + path, {
      method: "GET",
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

  /** One files route path with the ref/path query, never empty-ref dirty. */
  function route(projectId: string, endpoint: string, opts: FilesReadOptions): string {
    const params = new URLSearchParams();
    if (opts.ref !== undefined && opts.ref !== "") params.set("ref", opts.ref);
    if (opts.path !== undefined && opts.path !== "") params.set("path", opts.path);
    const q = params.toString();
    return (
      `/api/v1/projects/${encodeURIComponent(projectId)}/files/${endpoint}` +
      (q === "" ? "" : `?${q}`)
    );
  }

  function asListing(body: unknown): FilesTreeListing {
    if (
      typeof body !== "object" || body === null ||
      !("entries" in body) || !Array.isArray(body.entries)
    ) {
      throw new Error("unexpected tree response shape");
    }
    return body as FilesTreeListing;
  }

  function asFileView(body: unknown): FilesFileView {
    if (
      typeof body !== "object" || body === null ||
      !("path" in body) || typeof body.path !== "string" ||
      !("kind" in body) || typeof body.kind !== "string"
    ) {
      throw new Error("unexpected file content response shape");
    }
    return body as FilesFileView;
  }

  return {
    async tree(projectId, opts) {
      const result = await call(route(projectId, "tree", opts));
      if (result.status >= 400) fail(result.status, result.body);
      return asListing(result.body);
    },
    async content(projectId, opts) {
      const result = await call(route(projectId, "content", opts));
      if (result.status >= 400) fail(result.status, result.body);
      return asFileView(result.body);
    },
    async history(projectId, opts) {
      const params = new URLSearchParams();
      if (opts.ref !== undefined && opts.ref !== "") params.set("ref", opts.ref);
      if (opts.path !== undefined && opts.path !== "") params.set("path", opts.path);
      if (opts.limit !== undefined) params.set("limit", String(opts.limit));
      const q = params.toString();
      const result = await call(
        `/api/v1/projects/${encodeURIComponent(projectId)}/files/history` +
          (q === "" ? "" : `?${q}`),
      );
      if (result.status >= 400) fail(result.status, result.body);
      const body = result.body;
      if (
        typeof body !== "object" || body === null ||
        !("entries" in body) || !Array.isArray(body.entries)
      ) {
        throw new Error("unexpected history response shape");
      }
      return body.entries as FilesCommitEntry[];
    },
    rawUrl(projectId, opts) {
      // Absolute: the API is another origin than the page — the link
      // target must not resolve against the web origin.
      return apiBaseUrl + route(projectId, "raw", opts);
    },
    diffUrl(projectId, sha) {
      return (
        apiBaseUrl +
        `/api/v1/projects/${encodeURIComponent(projectId)}/files/diff` +
        `?sha=${encodeURIComponent(sha)}`
      );
    },
  };
}

/** Human-facing message for a stable files API code. */
export function messageForFileCode(code: string): string {
  switch (code) {
    case "PROJECT_NOT_FOUND":
      return "This project does not exist, or you do not have access to it.";
    case "FILES_DISABLED":
      return "File browsing is not configured for this server.";
    case "PROJECT_NOT_PROVISIONED":
      return "This project's repository is still being provisioned. Try again shortly.";
    case "FILES_INVALID_REF":
      return "That branch or commit does not look valid.";
    case "FILES_INVALID_SHA":
      return "That commit does not look valid.";
    case "FILES_INVALID_PATH":
      return "That path does not look valid.";
    case "FILES_NOT_FOUND":
      return "No such file, directory or commit in this repository.";
    case "FILES_PATH_IS_DIRECTORY":
      return "That path is a directory — pick a file to preview it.";
    case "GIT_PROVIDER_UNAVAILABLE":
      return "The git backend could not answer. Please try again later.";
    default:
      return "Something went wrong while loading repository files. Please try again.";
  }
}
