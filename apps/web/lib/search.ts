/**
 * The search answer client (T0907).
 *
 * Reads one surface:
 *
 *   POST /api/v1/search    the evidence-backed answer (T0906)
 *
 * The answer document is the ANSWER LAYER's canonical rendering
 * (internal/search/answer.Answer.CanonicalJSON, pinned by tests/answer's
 * golden fixtures), so this module declares the fields that document has and
 * nothing else. Every name below is a field name from answer.go — there is
 * no translation table between "the wire" and "the page", because a second
 * vocabulary is a second definition of what an answer is.
 *
 * # What this module does NOT do
 *
 * It does not derive anything the answer does not carry. docs/42 §Search
 * Answer and docs/05 §5 ask the page for `comparison`, `conditions` and an
 * `Evidence Map`; the answer document has no key for any of the three, and
 * computing them from `sources` would make this file a second, untested
 * definition of what an answer means (and would put numbers or claims on the
 * page that no producer ever made). The one projection that IS honest is
 * here as evidenceRows(): `limitations` and `conflicts` carry `refs`, and the
 * page can align those refs with `sources` to draw a claim↔evidence view
 * without inventing a single field.
 *
 * It does not re-rank and it does not filter by visibility. The order of
 * `sources` is the ranking's (rank is a POSITION, not a quality), and every
 * row the API returned is rendered.
 *
 * Import-free by construction (like lib/explore.ts and lib/assets.ts), so the
 * node:test suite (search.test.mjs) runs it through Node's type stripping.
 */

/** The two statuses an answer can have (internal/search/answer/answer.go). */
export const ANSWER_STATUSES = ["answered", "fallback"] as const;
export type AnswerStatus = (typeof ANSWER_STATUSES)[number];

/**
 * The closed set of fallback reasons (answer.go's Reason). A fallback is not
 * an error: it is the platform declining to write an answer it could not
 * ground. Each reason gets its own sentence — six causes are six different
 * things to tell a reader, and "something went wrong" would say none of them.
 */
export const FALLBACK_REASONS = [
  "no_provider",
  "no_sources",
  "provider_error",
  "provider_timeout",
  "invalid_answer",
  "ungrounded_citation",
] as const;
export type FallbackReason = (typeof FALLBACK_REASONS)[number];

/** One sentence the platform derived about its own evidence. */
export interface SearchStatement {
  text: string;
  /** The sources it is about; absent when the statement is about the search. */
  refs?: string[];
  /** "platform" today; "answer" is reserved for a model-authored statement. */
  origin: string;
}

/** One factor behind a source's position (ranking.Assessment). */
export interface SearchFactor {
  factor: string;
  level: string;
  level_rank: number;
  reason: string;
  facts?: Record<string, number>;
}

/**
 * One source: the entity the answer may cite AND, at the same time, one
 * underlying result (answer.go states the two are one list, with `cited`
 * marking the subset the summary leans on).
 */
export interface SearchSource {
  rank: number;
  ref: string;
  kind: string;
  entity_type?: string;
  object_type?: string;
  title?: string;
  version?: string;
  /** Where a click lands, or "" when the platform has no address for it. */
  href?: string;
  project_id?: string;
  labels: string[];
  factors: SearchFactor[];
  cited: boolean;
}

/** One evidence-backed answer. */
export interface SearchAnswer {
  answer_version: string;
  status: AnswerStatus;
  reason?: string;
  query: string;
  answer_view: boolean;
  summary: string;
  citations: string[];
  limitations: SearchStatement[];
  conflicts: SearchStatement[];
  sources: SearchSource[];
}

/** POST /api/v1/search's response: the record's id and the answer. */
export interface SearchResponse {
  search_id: string;
  answer: SearchAnswer;
}

/** The API's error envelope (cmd/api/authhttp/envelope.go). */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/** An API refusal, carrying the wire code so a page can render per-code copy. */
export class ApiError extends Error {
  readonly code: string;
  readonly status: number;
  readonly retryable: boolean;
  /** The envelope's request id, "" when the failure produced no envelope. */
  readonly requestId: string;

  constructor(status: number, envelope: ErrorEnvelope) {
    super(envelope.message);
    this.name = "ApiError";
    this.code = envelope.code;
    this.status = status;
    this.retryable = envelope.retryable ?? status === 503;
    this.requestId = envelope.request_id ?? "";
  }
}

/**
 * The codes this page can be handed, and the line it renders for each.
 *
 * The four SEARCH_* codes are cmd/api/searchhttp's. The last two are this
 * client's own: a body that is not JSON never reaches a code, and a fetch
 * that throws has no envelope at all — both are failures, and rendering
 * either as an empty result list would be the "failure is not emptiness"
 * mistake docs/51 names.
 */
export function messageForSearchCode(code: string): string {
  switch (code) {
    case "SEARCH_INVALID_REQUEST":
      return "That search could not be run as written. Check the question and try again.";
    case "SEARCH_UNAUTHENTICATED":
      return "Sign in to run a search.";
    case "SEARCH_UNAVAILABLE":
      return "Search is temporarily unavailable. Try again.";
    case "SEARCH_RECORD_FAILED":
      return "The search could not be recorded, so it was not answered. Try again.";
    case "MALFORMED_ANSWER":
      return "The answer could not be read, so nothing is shown for this search.";
    default:
      return "The search service could not be reached. Try again.";
  }
}

/**
 * The headline a fallback renders, one per reason.
 *
 * These sentences are the page's own and they say only what the reason code
 * says: each one is the same fact internal/search/answer's derive.go writes
 * into the answer's first limitation (fallbackText), shortened for a
 * heading. The platform's full sentence is rendered verbatim underneath —
 * this is a heading over that fact, never a second account of it, and it
 * makes no claim the answer does not carry.
 */
export function fallbackHeadline(reason: string): string {
  switch (reason) {
    case "no_provider":
      return "No written answer: this deployment has no answer model configured.";
    case "no_sources":
      return "No written answer: the search returned no source to cite.";
    case "provider_error":
      return "No written answer: the answer model could not be reached.";
    case "provider_timeout":
      return "No written answer: the answer model did not answer in time.";
    case "invalid_answer":
      return "No written answer: the model's document did not satisfy the answer schema.";
    case "ungrounded_citation":
      return "No written answer: the model cited an entity the search did not return.";
    default:
      // A reason this page has not been taught is still a fallback, and the
      // page still says the one thing that is true of every one of them. The
      // platform's own sentence is rendered beside it either way.
      return "No written answer for this search.";
  }
}

/** A source's display label: its title, or the ref when it has none. */
export function sourceLabel(source: SearchSource): string {
  const title = (source.title ?? "").trim();
  return title === "" ? source.ref : title;
}

/** The kind/type line under a source's title, from the answer's own fields. */
export function sourceKindLabel(source: SearchSource): string {
  const parts = [source.kind, source.entity_type ?? source.object_type ?? ""];
  return parts.filter((part) => part !== "").join(" · ");
}

/**
 * The web address a source's href resolves to.
 *
 * `href` is produced by internal/search/answer/locator.go and is an API path
 * in the answer's own namespace ("/api/v1/assets/{pid}?version=2"), never a
 * web-app route: the answer layer refuses to guess a page and refuses to
 * compose a URL this app might not serve. Resolving it against the API
 * origin is therefore the whole of this function — it adds no path segment,
 * and an empty href stays empty so the page can render the source as text.
 *
 * A future producer could hand back an absolute address instead; it is
 * already resolved and re-prefixing it would corrupt it. Only http and https
 * are handed back that way. Any other scheme — javascript:, data:, vbscript:,
 * file: — is "no address" (the empty string above), not a link: the caller
 * puts this value into an anchor's href, so passing one through would make
 * this function the place where an executable address becomes clickable. The
 * page already renders "no address" as text, so nothing downstream changes.
 */
export function sourceHref(apiBaseUrl: string, href: string | undefined): string {
  const path = (href ?? "").trim();
  if (path === "") return "";
  // Any scheme means an absolute address, which is never re-prefixed with
  // the API origin — but only http(s) is an address this app will link to.
  // The match is case-insensitive, so the allowlist has to be as well.
  if (/^[a-z][a-z0-9+.-]*:/i.test(path)) return /^https?:/i.test(path) ? path : "";
  return apiBaseUrl.replace(/\/+$/, "") + path;
}

/** The sources the answer's citations name, in the ranking's order. */
export function citedSources(answer: SearchAnswer): SearchSource[] {
  return answer.sources.filter((source) => source.cited);
}

/** One row of the claim↔evidence view. */
export interface EvidenceRow {
  /** "limitation" or "conflict" — which of the answer's two lists it came from. */
  section: "limitation" | "conflict";
  text: string;
  /** The refs the statement named, in the order the statement named them. */
  refs: string[];
  /** Those refs resolved against the answer's sources; empty when it named none. */
  sources: SearchSource[];
}

/**
 * evidenceRows projects the answer's own statements onto the answer's own
 * sources: for every limitation and conflict, the refs it is about and the
 * source rows those refs name.
 *
 * This is the ONE block docs/42's "Evidence Map" can honestly be drawn from
 * without inventing a field: the answer carries no evidence map, but it does
 * carry statements that each name the sources they are about, and sources
 * that each carry an identity. A ref that matches no source is kept in refs
 * and simply has no row — the page shows the ref, and never a source the
 * answer did not return.
 */
export function evidenceRows(answer: SearchAnswer): EvidenceRow[] {
  const byRef = new Map(answer.sources.map((source) => [source.ref, source]));
  const rows: EvidenceRow[] = [];
  const add = (section: EvidenceRow["section"], statements: SearchStatement[]) => {
    for (const statement of statements) {
      const refs = statement.refs ?? [];
      rows.push({
        section,
        text: statement.text,
        refs,
        sources: refs.map((ref) => byRef.get(ref)).filter((s): s is SearchSource => s !== undefined),
      });
    }
  };
  add("limitation", answer.limitations ?? []);
  add("conflict", answer.conflicts ?? []);
  return rows;
}

/**
 * isSearchResponse reports whether a decoded body is a response this page can
 * render.
 *
 * It validates SHAPE and nothing else: the answer present, a status the
 * vocabulary has, and the three lists the page iterates present as lists. A
 * body that fails it is an ERROR, never an answer with nothing in it — a
 * truncated document rendered as "no sources" would read as a search that
 * found nothing (docs/51: failure is not emptiness).
 */
export function isSearchResponse(value: unknown): value is SearchResponse {
  if (typeof value !== "object" || value === null) return false;
  const response = value as Record<string, unknown>;
  if (typeof response.search_id !== "string" || response.search_id === "") return false;
  const answer = response.answer;
  if (typeof answer !== "object" || answer === null) return false;
  const record = answer as Record<string, unknown>;
  if (!ANSWER_STATUSES.includes(record.status as AnswerStatus)) return false;
  if (typeof record.summary !== "string") return false;
  if (typeof record.answer_view !== "boolean") return false;
  return (
    Array.isArray(record.sources) &&
    Array.isArray(record.citations) &&
    Array.isArray(record.limitations) &&
    Array.isArray(record.conflicts)
  );
}

/** The minimal fetch shape this module needs (mirrors lib/assets.ts). */
export type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    credentials?: string;
    body?: string;
  },
) => Promise<{ status: number; json: () => Promise<unknown> }>;

/** Options for createSearchClient. */
export interface SearchClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/** The search client: one per API origin. */
export interface SearchClient {
  /** One question, answered. Throws ApiError on any failure. */
  search(query: string): Promise<SearchResponse>;
}

/** The API address of the answer. */
export function searchUrl(apiBaseUrl: string): string {
  return `${apiBaseUrl.replace(/\/+$/, "")}/api/v1/search`;
}

/** Decode an error body into the envelope, falling back to a generic code. */
function envelopeFor(status: number, body: unknown): ErrorEnvelope {
  if (typeof body === "object" && body !== null) {
    const env = body as Partial<ErrorEnvelope>;
    if (typeof env.code === "string" && env.code !== "") {
      return {
        code: env.code,
        message: typeof env.message === "string" ? env.message : "request failed",
        request_id: env.request_id,
        retryable: env.retryable,
      };
    }
  }
  return { code: "UNKNOWN", message: `request failed with status ${status}`, retryable: false };
}

/** Create the search client over one API origin. */
export function createSearchClient(
  apiBaseUrl: string,
  options: SearchClientOptions = {},
): SearchClient {
  const doFetch = options.fetch ?? (globalThis.fetch as unknown as FetchLike);

  return {
    async search(query: string): Promise<SearchResponse> {
      let res: { status: number; json: () => Promise<unknown> };
      try {
        // credentials: "include" — POST /search runs behind the auth guard
        // (specs/api's global security), so the session cookie has to ride
        // along; a caller without one gets the 401 the page renders.
        res = await doFetch(searchUrl(apiBaseUrl), {
          method: "POST",
          headers: { "content-type": "application/json", accept: "application/json" },
          credentials: "include",
          body: JSON.stringify({ query }),
        });
      } catch {
        // No envelope exists when the connection itself fails.
        throw new ApiError(0, { code: "UNREACHABLE", message: "search unreachable", retryable: true });
      }
      let body: unknown;
      try {
        body = await res.json();
      } catch {
        throw new ApiError(res.status, {
          code: "MALFORMED_ANSWER",
          message: "the answer was not JSON",
          retryable: res.status >= 500,
        });
      }
      if (res.status < 200 || res.status >= 300) {
        throw new ApiError(res.status, envelopeFor(res.status, body));
      }
      if (!isSearchResponse(body)) {
        throw new ApiError(res.status, {
          code: "MALFORMED_ANSWER",
          message: "the answer did not have the shape of an answer",
          retryable: false,
        });
      }
      return body;
    },
  };
}
