/**
 * The Research tab's authoring model (WEB-06a / G06, U03 slice a).
 *
 * WHY THIS IS A PURE MODULE. A form is only honest if the payload it sends
 * is the payload the reader sees: the field list, the value → payload
 * conversion, the payload → form continuation, and the "what does the gate
 * still want" projection all live here, outside React and outside fetch, so
 * they can be unit-tested without a browser and so the FOUR forms provably
 * share one conversion instead of four that happen to agree today.
 *
 * WHAT THIS SLICE AUTHORIZES. Question, Hypothesis, Experiment and
 * Calculation only. The other object families (Dataset, Protocol, Material,
 * Sample; Claim, Finding, ExternalReference) are separate work packages
 * (T1336/T1337) — this module deliberately has no field spec for them, and
 * an unknown type is refused rather than rendered as a generic JSON box.
 *
 * DRAFT DISCIPLINE (D15). A draft may be incomplete: the create call sends
 * whatever identity field the reader typed and nothing else. Completeness is
 * checked BEFORE a reader claims the work is ready for review, by the
 * server's own gate report (`POST …:validate`, gate pr); the client never
 * decides that a branch passed, and never reports a submission that did not
 * happen (this slice submits nothing — it only checks).
 *
 * FAILED RESULTS. An experiment or calculation may fail; that is still a
 * valid Experiment/Calculation (docs/08). The stored payload keeps the
 * failure in `result_summary` and records the outcome code in the
 * schema-permitted `metadata.result_outcome`, so a failure is rendered as a
 * failure and is never rewritten into a success.
 */

import type { ProjectRole } from "../../../../../lib/projects";

/** The four object types this slice authors. */
export type AuthoringKind = "research_question" | "hypothesis" | "experiment" | "calculation";

export const AUTHORING_KINDS: readonly AuthoringKind[] = [
  "research_question",
  "hypothesis",
  "experiment",
  "calculation",
];

export function isAuthoringKind(value: string): value is AuthoringKind {
  return (AUTHORING_KINDS as readonly string[]).includes(value);
}

/**
 * The project roles that may write research state.
 *
 * A POSITIVE list, deliberately not `role !== "viewer"`. The shell gives
 * `role: null` for an anonymous reader and for a signed-in non-member of a
 * project that is readable anyway (a Public project), and a gate written as
 * "not a viewer" is then TRUE for exactly the callers who have no write
 * permission — the affordance would fail OPEN. The API still enforces
 * authorization; this only decides whether the UI offers a form at all.
 */
const AUTHORING_ROLES: ReadonlySet<string> = new Set(["owner", "maintainer", "contributor"]);

export function canAuthor(role: ProjectRole | null): boolean {
  return role !== null && AUTHORING_ROLES.has(role);
}

/** How a field is rendered. `select` needs options; `target` is a select of
 *  an object family of THIS BRANCH (value = the object id); `json`/`list`
 *  are the two composite shapes the schemas allow. */
export type FieldControl = "text" | "textarea" | "select" | "target" | "list" | "json";

export interface FieldSpec {
  /** The schema's own field name (a stable code, never a label). */
  name: string;
  /** Catalog key for the field's label. */
  labelKey: string;
  control: FieldControl;
  /** Select options (stable codes). */
  options?: readonly string[];
  /** The object type a `target` select lists. */
  targetType?: string;
  /** The gate's required-field list marks this field (guidance only — the
   *  server's pr-gate report is authoritative; see reviewRequiredFields). */
  requiredForReview?: boolean;
  /** The object schema types this field `["string", "null"]`, so a cleared
   *  value can be sent as an explicit `null` (see assemblePayload). Fields
   *  without this marker cannot be emptied through the API: the schema wants
   *  a string, a code or an object, and none of those has an "empty" value. */
  nullable?: boolean;
}

/**
 * The outcome codes an experiment or a calculation may carry. Kept as
 * stable codes; the catalog gives each a label. `failed` is a first-class
 * outcome, not an error state (docs/08: 实验失败仍是有效 Experiment).
 */
export const RESULT_OUTCOMES = ["succeeded", "failed", "inconclusive", "not_run"] as const;
export type ResultOutcome = (typeof RESULT_OUTCOMES)[number];

/** The payload field the outcome is stored in. The schemas have no
 *  top-level outcome field, and `additionalProperties: false` forbids
 *  inventing one; `metadata` is the schema's own open object.
 *
 *  CONVENTION (not yet a written spec — the Supervisor ratifies or moves
 *  it): the outcome of an experiment or calculation lives at
 *  `metadata.result_outcome`, one of RESULT_OUTCOMES. Nothing else is
 *  written into `metadata` by this build; keys another producer put there
 *  are preserved verbatim on update (see assemblePayload's `base`). */
export const OUTCOME_METADATA_KEY = "result_outcome";

const QUESTION_STATES = ["open", "partially_answered", "unresolved", "superseded", "aborted"] as const;
const HYPOTHESIS_ASSESSMENTS = ["proposed", "under_test", "supported", "weakened", "contested", "aborted"] as const;

/**
 * The fields of each authored type, in display order. The first field is
 * the type's identity: a draft is saveable with that one field alone, which
 * is exactly what D15's "save a minimal draft first" asks for.
 */
const FIELDS: Readonly<Record<AuthoringKind, readonly FieldSpec[]>> = {
  research_question: [
    { name: "statement", labelKey: "research.authoring.field.statement", control: "textarea" },
    {
      name: "purpose",
      labelKey: "research.authoring.field.purpose",
      control: "text",
      requiredForReview: true,
      // research_question.schema.json: "purpose": ["string", "null"].
      nullable: true,
    },
    {
      name: "question_state",
      labelKey: "research.authoring.field.questionState",
      control: "select",
      options: QUESTION_STATES,
      requiredForReview: true,
    },
    { name: "scope", labelKey: "research.authoring.field.scope", control: "json" },
  ],
  hypothesis: [
    { name: "statement", labelKey: "research.authoring.field.statement", control: "textarea" },
    {
      name: "question_id",
      labelKey: "research.authoring.field.question",
      control: "target",
      targetType: "research_question",
    },
    {
      name: "hypothesis_type",
      labelKey: "research.authoring.field.hypothesisType",
      control: "text",
      requiredForReview: true,
      // hypothesis.schema.json: "hypothesis_type": ["string", "null"].
      nullable: true,
    },
    {
      name: "assessment",
      labelKey: "research.authoring.field.assessment",
      control: "select",
      options: HYPOTHESIS_ASSESSMENTS,
    },
    { name: "scope", labelKey: "research.authoring.field.scope", control: "json", requiredForReview: true },
  ],
  experiment: [
    { name: "objective", labelKey: "research.authoring.field.objective", control: "textarea" },
    { name: "operator_ids", labelKey: "research.authoring.field.operators", control: "list", requiredForReview: true },
    { name: "sample_ids", labelKey: "research.authoring.field.samples", control: "list", requiredForReview: true },
    {
      name: "protocol_version_id",
      labelKey: "research.authoring.field.protocolVersion",
      control: "text",
      requiredForReview: true,
      // experiment.schema.json: "protocol_version_id": ["string", "null"].
      nullable: true,
    },
    { name: "conditions", labelKey: "research.authoring.field.conditions", control: "json", requiredForReview: true },
    {
      name: "result_summary",
      labelKey: "research.authoring.field.resultSummary",
      control: "textarea",
      requiredForReview: true,
      // experiment.schema.json: "result_summary": ["string", "null"].
      nullable: true,
    },
    {
      name: "result_outcome",
      labelKey: "research.authoring.field.resultOutcome",
      control: "select",
      options: RESULT_OUTCOMES,
    },
  ],
  calculation: [
    { name: "objective", labelKey: "research.authoring.field.objective", control: "textarea" },
    { name: "method", labelKey: "research.authoring.field.method", control: "textarea" },
    {
      name: "software",
      labelKey: "research.authoring.field.software",
      control: "text",
      requiredForReview: true,
      // calculation.schema.json: "software": ["string", "null"].
      nullable: true,
    },
    { name: "input_object_ids", labelKey: "research.authoring.field.inputs", control: "list", requiredForReview: true },
    {
      name: "protocol_version_id",
      labelKey: "research.authoring.field.protocolVersion",
      control: "text",
      // calculation.schema.json: "protocol_version_id": ["string", "null"].
      nullable: true,
    },
    { name: "parameters", labelKey: "research.authoring.field.parameters", control: "json", requiredForReview: true },
    {
      name: "result_summary",
      labelKey: "research.authoring.field.resultSummary",
      control: "textarea",
      requiredForReview: true,
      // calculation.schema.json: "result_summary": ["string", "null"].
      nullable: true,
    },
    {
      name: "result_outcome",
      labelKey: "research.authoring.field.resultOutcome",
      control: "select",
      options: RESULT_OUTCOMES,
    },
  ],
};

// Looked up through a Map rather than `FIELDS[kind]`: the kind is a runtime
// string, and a computed read on a shared object is exactly the pattern the
// SAST row flags. A kind with no table gets no fields (and `isAuthoringKind`
// is what the call sites check first).
const FIELDS_BY_KIND: ReadonlyMap<string, readonly FieldSpec[]> = new Map(Object.entries(FIELDS));

export function fieldsFor(kind: AuthoringKind): readonly FieldSpec[] {
  return FIELDS_BY_KIND.get(kind) ?? [];
}

/** The identity field: the one field a minimal draft needs. */
export function identityField(kind: AuthoringKind): string {
  return kind === "research_question" || kind === "hypothesis" ? "statement" : "objective";
}

/** The fields this form has an input for that the pr gate's required-field
 *  table also names (`typeRequiredFields` in internal/rsg/validation/spec.go),
 *  so an input can carry the "needed before review" note. It is deliberately
 *  not a full mirror: `calculation.output_blob_ids` is in the server's table
 *  and this slice has no input for it (blobs are another work package), so
 *  the note is guidance and the server's own report is the authority on what
 *  a branch still owes. */
export function reviewRequiredFields(kind: AuthoringKind): readonly string[] {
  return fieldsFor(kind).filter((field) => field.requiredForReview === true).map((field) => field.name);
}

/** A stable enum code rendered as readable text WITHOUT translating it: the
 *  schemas' value vocabularies (question_state, assessment, outcome) are
 *  data, and a code this build predates must show as itself (docs/28 §3). */
export function humanizeOption(code: string): string {
  return code.replaceAll("_", " ");
}

/** The catalog key for an outcome code, or null for a code this build does
 *  not know (the caller renders the code itself, never a miss marker). */
const OUTCOME_KEYS: ReadonlyMap<string, string> = new Map([
  ["succeeded", "research.authoring.outcome.succeeded"],
  ["failed", "research.authoring.outcome.failed"],
  ["inconclusive", "research.authoring.outcome.inconclusive"],
  ["not_run", "research.authoring.outcome.not-run"],
]);

export function outcomeKey(code: string): string | null {
  return OUTCOME_KEYS.get(code) ?? null;
}

/** A form's values: one string per field (a `target`/`select` value is the
 *  stable code; lists are comma-separated; json is the object as text). */
export type AuthoringValues = Record<string, string>;

export function emptyValues(kind: AuthoringKind): AuthoringValues {
  const values: AuthoringValues = {};
  for (const field of fieldsFor(kind)) values[field.name] = "";
  return values;
}

/**
 * One value → its payload shape. An EMPTY value is omitted, never sent as
 * an empty string: a draft that was not filled in must stay absent so the
 * gate reports it as missing instead of accepting "" as an answer.
 */
export function valueToPayload(field: FieldSpec, raw: string): { value?: unknown; error?: string } {
  const value = raw.trim();
  if (value === "") return {};
  switch (field.control) {
    case "list": {
      const items = value
        .split(",")
        .map((item) => item.trim())
        .filter((item) => item !== "");
      return items.length === 0 ? {} : { value: items };
    }
    case "json": {
      let parsed: unknown;
      try {
        parsed = JSON.parse(value);
      } catch {
        return { error: field.name };
      }
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) {
        return { error: field.name };
      }
      return { value: parsed };
    }
    default:
      return { value };
  }
}

export interface AssembleResult {
  payload: Record<string, unknown>;
  /** Field names whose value could not be converted (a json field that is
   *  not an object). The caller renders a field error and sends nothing. */
  invalidFields: string[];
}

/**
 * The create/version payload a form submits. `metadata.result_outcome`
 * carries the outcome code so a failed run is stored as a failed run.
 *
 * `base` is the payload the version being continued already holds. It is
 * passed when EDITING an existing object and left out when creating one:
 *
 *   - `metadata` is merged with the base's metadata, so a key another
 *     producer stored there is not silently dropped by an outcome edit (the
 *     version patch is a shallow merge: naming `metadata` replaces the whole
 *     object). Clearing the outcome removes only the outcome code.
 *   - every other field is the form's own; a field the form does not render
 *     is never named here, and the server's shallow merge keeps it.
 *   - a field the form DOES render but the reader emptied is an explicit
 *     clear when the continued version holds a value there (`null` for a
 *     nullable field, `[]` for a list). The patch is a shallow merge, so
 *     leaving it out would keep the old value alive while the form says the
 *     save succeeded — the reader would be told a clear happened that did
 *     not. On CREATE (`base` absent) an empty field stays out: a draft that
 *     was never filled in must look missing to the gate, not blank.
 */
export function assemblePayload(
  kind: AuthoringKind,
  values: AuthoringValues,
  base?: unknown,
): AssembleResult {
  const payload: Record<string, unknown> = {};
  const invalidFields: string[] = [];
  const baseMetadata = metadataOf(base);
  for (const field of fieldsFor(kind)) {
    const raw = values[field.name] ?? "";
    if (field.name === "result_outcome") {
      const outcome = RESULT_OUTCOMES.find((code) => code === raw.trim());
      // A Map, so the one key this form owns is read and written without a
      // computed property access on a value that came from storage.
      const merged = new Map(Object.entries(baseMetadata));
      // Clearing the select removes the outcome code and nothing else: the
      // patch names metadata, so every key that must survive is named here.
      merged.delete(OUTCOME_METADATA_KEY);
      if (outcome !== undefined) merged.set(OUTCOME_METADATA_KEY, outcome);
      if (merged.size > 0) payload.metadata = Object.fromEntries(merged);
      continue;
    }
    const converted = valueToPayload(field, raw);
    if (converted.error !== undefined) {
      invalidFields.push(converted.error);
      continue;
    }
    if (converted.value !== undefined) {
      payload[field.name] = converted.value;
      continue;
    }
    // The form is empty for this field. On a DRAFT that means "not stated
    // yet", so the field stays out and the gate reports it as missing. On an
    // EDIT of a version that holds a value, leaving it out would silently
    // keep the old value while the form reports a save — so the clear is
    // sent explicitly: `null` for the fields the object schema types
    // ["string", "null"], `[]` for the list fields (also schema-valid). A
    // field whose schema allows neither (a required string, a code, an
    // object) cannot be emptied through this API; it is left unnamed and the
    // reader sees the stored value return when the form re-seeds from the
    // version that was actually saved.
    if (baseHasValue(base, field.name)) {
      if (field.control === "list") payload[field.name] = [];
      else if (field.nullable === true) payload[field.name] = null;
    }
  }
  return { payload, invalidFields };
}

/** The payload's `metadata` object, or {} when it has none. */
function metadataOf(payload: unknown): Record<string, unknown> {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) return {};
  const metadata = (payload as Record<string, unknown>).metadata;
  if (metadata === null || typeof metadata !== "object" || Array.isArray(metadata)) return {};
  return { ...(metadata as Record<string, unknown>) };
}

/** Whether the payload a version continues already holds a value for `name`.
 *  Only a stored value can be CLEARED; an absent one has nothing to undo. */
function baseHasValue(base: unknown, name: string): boolean {
  if (base === null || typeof base !== "object" || Array.isArray(base)) return false;
  const value = new Map(Object.entries(base)).get(name);
  if (value === undefined || value === null) return false;
  if (Array.isArray(value)) return value.length > 0;
  if (typeof value === "object") return Object.keys(value).length > 0;
  return String(value).trim() !== "";
}

/**
 * Whether the editor may continue the version on screen.
 *
 * The version endpoint takes an `expected_version` (the object's CURRENT
 * version) and a patch; the form is seeded from the version the reader is
 * looking at. Those two must be the same version, or continuing version 1
 * of an object that is now at version 2 would write a new version built
 * from version 1's values and silently revert what version 2 changed.
 * So the editor is offered only on the head version; an older version is
 * for reading (and for the relations it pins), not for continuing.
 */
export function mayContinueVersion(selectedVersionNo: number, currentVersion: number): boolean {
  return selectedVersionNo === currentVersion;
}

/** Reads a stored payload back into form values, so "continue this draft"
 *  shows what the version actually holds (never a blank form that would
 *  silently overwrite the draft on save). */
export function valuesFromPayload(kind: AuthoringKind, payload: unknown): AuthoringValues {
  const values = emptyValues(kind);
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) return values;
  const record = payload as Record<string, unknown>;
  for (const field of fieldsFor(kind)) {
    if (field.name === "result_outcome") {
      values.result_outcome = outcomeOf(payload);
      continue;
    }
    const value = record[field.name];
    if (value === undefined || value === null) continue;
    if (Array.isArray(value)) values[field.name] = value.map((item) => String(item)).join(", ");
    else if (typeof value === "object") values[field.name] = JSON.stringify(value);
    else values[field.name] = String(value);
  }
  return values;
}

/** The stored outcome code, or "" when the payload carries none. */
export function outcomeOf(payload: unknown): string {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) return "";
  const metadata = (payload as Record<string, unknown>).metadata;
  if (metadata === null || typeof metadata !== "object" || Array.isArray(metadata)) return "";
  // Read through a Map: the key is this build's constant, but `metadata` is
  // whatever another producer stored, and a computed read on it is what the
  // SAST row flags.
  const code = new Map(Object.entries(metadata)).get(OUTCOME_METADATA_KEY);
  return typeof code === "string" ? code : "";
}

/** Whether the form holds enough to save a DRAFT: its identity field is
 *  non-empty. Everything else may stay blank (D15). */
export function canSaveDraft(kind: AuthoringKind, values: AuthoringValues): boolean {
  return (values[identityField(kind)] ?? "").trim() !== "";
}

/** The fields the outcome select renders as "not stated" plus the codes. */
export function outcomeOptions(): readonly string[] {
  return RESULT_OUTCOMES;
}

/* ---- the gate report (POST …/branches/{branchId}:validate, gate pr) ---- */

export interface GateResult {
  check: string;
  severity: string;
  passed: boolean;
  subject?: string;
  detail?: string;
  why: string;
}

export interface GateReport {
  gate: string;
  verdict: string;
  results: GateResult[];
  explanation: string;
}

/** One row of the review checklist, as the server's report states it. */
export interface GateRow {
  check: string;
  /** "blocking" or "warning" — what the failure means at the pr gate. */
  severity: string;
  /** The object/version the row is about ("" for a snapshot-level check). */
  subject: string;
  /** The concrete gap the gate names (the missing field, the failing hash). */
  detail: string;
  why: string;
}

/** Failed rows only: a passed check needs no reader's attention. */
export function gateRows(report: GateReport): GateRow[] {
  const rows: GateRow[] = [];
  for (const result of report.results) {
    if (result.passed) continue;
    rows.push({
      check: result.check,
      severity: result.severity,
      subject: result.subject ?? "",
      detail: result.detail ?? "",
      why: result.why,
    });
  }
  return rows;
}

export interface GateSummary {
  blocked: boolean;
  blocking: number;
  warnings: number;
  verdict: string;
}

export function gateSummary(report: GateReport): GateSummary {
  let blocking = 0;
  let warnings = 0;
  for (const result of report.results) {
    if (result.passed) continue;
    if (result.severity === "blocking") blocking += 1;
    else warnings += 1;
  }
  return { blocked: report.verdict === "blocked", blocking, warnings, verdict: report.verdict };
}

/* ---- URLs ---------------------------------------------------------------- */

/** The Research tab URL for a view/focus, pinned to a branch when one is
 *  selected. The branch lives in the URL so a reload and a shared link
 *  return to the same research path. */
export function researchBranchHref(projectId: string, view: string, branchId: string, focus = ""): string {
  const query = new URLSearchParams();
  query.set("view", view);
  if (focus !== "") query.set("focus", focus);
  if (branchId !== "") query.set("branch", branchId);
  return `/projects/${projectId}/research?${query.toString()}`;
}

/** One object's page, pinned to the branch it is being read/edited on, with
 *  the same version/return coordinates the browse surface already uses. */
export function branchObjectHref(
  projectId: string,
  objectId: string,
  branchId: string,
  params?: { version?: number; from?: string; fromVersion?: number },
): string {
  const query = new URLSearchParams();
  if (branchId !== "") query.set("branch", branchId);
  if (params?.version !== undefined) query.set("version", String(params.version));
  if (params?.from !== undefined && params.from !== "") {
    query.set("from", params.from);
    if (params.fromVersion !== undefined) query.set("fromVersion", String(params.fromVersion));
  }
  const suffix = query.toString();
  return `/projects/${projectId}/research/objects/${objectId}${suffix === "" ? "" : `?${suffix}`}`;
}
