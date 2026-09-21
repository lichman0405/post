/**
 * Client-side attestation module for the Go API (T0812).
 *
 * Reads the one public surface a private project's statement creates:
 *
 *   GET /api/v1/attestations/{attestationId}
 *
 * The path is this task's own L1 decision (cmd/api/attestationhttp/doc.go):
 * the specifications name the capability — docs/12 §2 "Private Project …
 * 可显式 Publish Asset/Knowledge/Attestation", docs/13 §5 — and fix no path
 * for it. `{attestationId}` is the attestation's PID, and the route is
 * deliberately NOT nested under a project: the identity a citation holds is
 * the pid, and nesting it under the ATTESTING project's segment would put
 * that project's id in the URL of a public page about somebody else's work.
 *
 * The read is public with no second axis, so this module sends no
 * credentials the caller does not already have and applies NO audience rule
 * — there is none to apply. An attestation is written already public: the
 * command refuses to write one whose target is not already readable by the
 * network, so there is no restricted attestation to hide.
 *
 * # What this module does NOT do
 *
 * It does not redact, and it could not: the projection
 * (internal/application/attestations.Present) happens in the read, and the
 * query behind it selects no private column. The attesting project, the
 * basis state the statement rests on and the internal review that
 * authorised it have no field here to be dropped from, which is the
 * difference between a rule this module could get wrong and one it cannot
 * express. What this module DOES do is refuse to render anything the API did
 * not send: `attestationFacts` builds the page's fact list from named
 * fields, so a future API that added a private key would not be rendered by
 * a page that was never told what it means (docs/23 §3 — 前端隐藏不能替代
 * 后端拒绝, and its converse: the front end does not decide what is shown).
 *
 * Import-free by construction (like lib/assets.ts and lib/projects.ts), so
 * the node:test suite (attestations.test.mjs) runs it through Node's type
 * stripping. The ApiError shape mirrors lib/assets.ts.
 */

/** The error envelope every product route answers with (docs/45). */
export interface ErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/** ApiError mirrors lib/assets.ts: one code, one status, one message. */
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

/** One disclosure entry (internal/application/attestations.DisclosureItem). */
export interface AttestationDisclosureItem {
  field: string;
  revealed: boolean;
  detail: string;
}

/** The version the statement is about. Every field is public by the
 *  command's own rule — it refuses a target that is not — so rendering it
 *  names nothing new. */
export interface AttestationTarget {
  kind: string;
  object_id: string;
  version_id: string;
  title: string;
}

/** The attesting organization, when the record names one. */
export interface AttestationOrganization {
  id: string;
  slug: string;
  name: string;
}

/** Who the record names as having attested. It is an ORGANIZATION or
 *  nobody — never a project, never a user. */
export interface AttestationAttribution {
  mode: string;
  organization?: AttestationOrganization | null;
}

/** The record's statement about itself, on the wire rather than implied. */
export interface AttestationDisclosure {
  is_evidence: boolean;
  names_evidence: boolean;
  statement: string;
}

/** One public attestation (internal/application/attestations.PublicAttestation). */
export interface Attestation {
  pid: string;
  validation_type: string;
  validation_result: string;
  created_at: string;
  target: AttestationTarget;
  attributed_by: AttestationAttribution;
  disclosure: AttestationDisclosure;
}

/** The closed validation-type vocabulary (attestations.ValidationTypes). */
export const VALIDATION_TYPES = [
  "reproduction",
  "method_validation",
  "data_audit",
  "rights_review",
] as const;

/** The closed validation-result vocabulary (attestations.ValidationResults). */
export const VALIDATION_RESULTS = ["confirmed", "refuted", "inconclusive"] as const;

const TYPE_LABELS: Record<string, string> = {
  reproduction: "Reproduction",
  method_validation: "Method validation",
  data_audit: "Data audit",
  rights_review: "Rights review",
};

const RESULT_LABELS: Record<string, string> = {
  confirmed: "Confirmed",
  refuted: "Refuted",
  inconclusive: "Inconclusive",
};

/**
 * The display label of one validation type.
 *
 * An unknown value renders VERBATIM rather than as "other" — the same rule
 * as assetTypeLabel: the set is closed on the server, so a value outside it
 * is a value this client does not know, not a value that does not exist.
 */
export function validationTypeLabel(type: string): string {
  return TYPE_LABELS[type] ?? type;
}

/** The display label of one validation result, verbatim when unknown. */
export function validationResultLabel(result: string): string {
  return RESULT_LABELS[result] ?? result;
}

/**
 * The Primer tone of one validation result.
 *
 * Three results, three tones, and NONE of them is neutral: a result the
 * reader has to decode is a result this page failed to state. "confirmed"
 * is green, "refuted" is red, "inconclusive" is yellow — and a value
 * outside the closed set gets no tone, because a colour would assert
 * something this client cannot know.
 *
 * Nothing here is a score. The platform has no Truth Score and no Research
 * Score (CLAUDE.md §9.13), and a page that rendered a result as a bar, a
 * percentage or a rank would be inventing one.
 */
export function validationResultTone(result: string): "success" | "danger" | "attention" | "" {
  switch (result) {
    case "confirmed":
      return "success";
    case "refuted":
      return "danger";
    case "inconclusive":
      return "attention";
    default:
      return "";
  }
}

/**
 * How the record's attribution reads on the page.
 *
 * An anonymous attestation says so OUT LOUD rather than leaving the reader
 * to infer it from an absent organization: the API states the mode, and
 * "the attester is not named" is a fact the page should report, not one a
 * reader has to derive from a null. It does NOT say which of the two
 * anonymous situations holds — a project with no organization, or an
 * organization that may not be named — because the document does not
 * distinguish them and telling them apart would disclose a fact about the
 * attesting project's private side.
 */
export function attributionLabel(attribution: AttestationAttribution): string {
  if (attribution.mode === "named" && attribution.organization) {
    return `Named: ${attribution.organization.name}`;
  }
  return "Anonymous";
}

/**
 * The facts this page renders, as (label, value) rows.
 *
 * It is a FUNCTION over named fields rather than the whole object, and that
 * is the module's one privacy-relevant decision: a page that spread the
 * attestation into its markup would render whatever a future API added,
 * including a key it was never taught to keep private. Here the set is
 * closed by construction, and `attestations.test.mjs` feeds it a payload
 * carrying the attesting project's id and the basis state to show that
 * neither reaches the rendered list.
 */
export function attestationFacts(a: Attestation): { label: string; value: string }[] {
  return [
    { label: "Validation", value: validationTypeLabel(a.validation_type) },
    { label: "Result", value: validationResultLabel(a.validation_result) },
    { label: "Attributed by", value: attributionLabel(a.attributed_by) },
    { label: "Made public", value: a.created_at },
  ];
}

/**
 * The facts this page does not show, and cannot: it names them so a reader
 * learns that the absence is the design rather than a gap.
 *
 * It is a FIXED list, not one derived from the response. A list computed
 * from the payload would grow a line the moment an API disclosed something,
 * which is the wrong direction: what a reader needs to know is what an
 * attestation structurally does not carry, and that is a fact about the
 * record's shape.
 */
export const WITHHELD_FACTS: readonly string[] = [
  "Which project issued the statement",
  "The state the statement rests on — the work behind it",
  "The internal review that authorised it",
  "The evidence itself, and the reasoning",
  "How many rows like it exist — no count, anywhere",
];

/** The absolute URL of one attestation's page on this site. */
export function attestationHref(pid: string): string {
  return `/attestations/${encodeURIComponent(pid)}`;
}

/** The API URL the page data is read from. */
export function attestationUrl(apiBaseUrl: string, pid: string): string {
  return `${apiBaseUrl}/api/v1/attestations/${encodeURIComponent(pid)}`;
}

/**
 * The one sentence a wire code gets on the page.
 *
 * ATTESTATION_NOT_FOUND is ONE line for every reason the API can answer it:
 * an unknown pid, a segment that is not a pid at all, and — in the general
 * case — a record this caller may not learn about. The route answers the
 * same body for all of them deliberately, and a client that rendered four
 * different messages would rebuild the oracle the single code exists not to
 * be (cmd/api/attestationhttp/read.go).
 */
export function messageForAttestationCode(code: string): string {
  switch (code) {
    case "ATTESTATION_NOT_FOUND":
      return "No such attestation.";
    case "SERVICE_UNAVAILABLE":
      return "Attestation data is temporarily unavailable. Try again.";
    default:
      return "Something went wrong loading this attestation.";
  }
}

/** The fetch implementation the client uses; injectable for tests. */
export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/**
 * The outcome of reading one attestation.
 *
 * Three outcomes rather than the two an `Attestation | null` can hold, and
 * the third one is the point: `missing` is the API's answer ABOUT THE
 * RECORD, and `unavailable` is the absence of an answer at all.
 */
export type AttestationReadOutcome =
  | { kind: "found"; attestation: Attestation }
  | { kind: "missing" }
  | { kind: "unavailable"; code: string };

/**
 * readAttestation reads one attestation and says which of the three
 * outcomes it got.
 *
 * # Why `missing` is ONE outcome, and why it is not the only failure
 *
 * Every 404 the API can answer — an unknown pid, a segment that is not a
 * pid, and a record this caller may not learn about — is the same body with
 * the same code, deliberately (cmd/api/attestationhttp/read.go, docs/45).
 * Splitting those apart here would rebuild the existence oracle the wire
 * code exists not to be, so they collapse into one `missing`, and the page
 * answers notFound() for it.
 *
 * A 5xx (or a fetch that never reached the API) is a different statement
 * entirely: it is not an answer about the record, and rendering it as "No
 * such attestation" would tell a reader and a crawler that a record which
 * may well exist does not — a temporary outage turned into a permanent
 * claim, in the one place (a public, indexable page) where the difference is
 * durable. It therefore travels as its own outcome, carrying the API's own
 * code so the page can render what that code says (messageForAttestationCode,
 * whose SERVICE_UNAVAILABLE line names the retry) plus the one step the
 * reader can take (docs/45: an error names the next step).
 */
export async function readAttestation(
  apiBaseUrl: string,
  pid: string,
  fetchFn: FetchLike = globalThis.fetch as unknown as FetchLike,
): Promise<AttestationReadOutcome> {
  try {
    return { kind: "found", attestation: await fetchAttestation(apiBaseUrl, pid, fetchFn) };
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return { kind: "missing" };
    // A code the API sent is the code the copy is written for; anything else
    // (a fetch that failed before a response existed) is the
    // service-unavailable case, which is what it is from the reader's side.
    return { kind: "unavailable", code: err instanceof ApiError ? err.code : "SERVICE_UNAVAILABLE" };
  }
}

/**
 * fetchAttestation reads one attestation and returns it.
 *
 * credentials: "include" so the session cookie rides along when the browser
 * already has one — harmless on this route, which decides nothing by
 * session, and the same call the other public reads make (lib/assets.ts).
 */
export async function fetchAttestation(
  apiBaseUrl: string,
  pid: string,
  fetchFn: FetchLike = globalThis.fetch as unknown as FetchLike,
): Promise<Attestation> {
  const res = await fetchFn(attestationUrl(apiBaseUrl, pid), { credentials: "include" });
  const body = (await res.json()) as Attestation & Partial<ErrorEnvelope>;
  if (res.status < 200 || res.status >= 300) {
    throw new ApiError(res.status, {
      code: body.code ?? "INTERNAL_ERROR",
      message: body.message ?? "",
      retryable: body.retryable,
    });
  }
  return body as Attestation;
}
