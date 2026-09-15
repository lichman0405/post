/**
 * The rights declaration as a reader sees it (T0703).
 *
 * A published research asset version and a knowledge publication each carry
 * a rights document — the standard license id, the custom agreement
 * reference, six usage declarations (commercial use, derivatives,
 * redistribution, model training, attribution, patent grant) and two
 * separate access axes. The document's shape comes from
 * specs/policies/rights-template.yaml; the Go model that validates and
 * stores it is internal/rights. This module is the presentation half: it
 * reads the stored JSON and turns it into the lines a page renders.
 *
 * Three rules hold everywhere in this file.
 *
 * 1. It renders the declaration and says whose it is. Every value below is
 *    what the publisher declared, recorded and shown. POST records
 *    declarations; it does not interpret them (docs/38 §1-2, ADR-009), so
 *    no label and no sentence here may read as an assurance about what the
 *    law allows, what a license grants, or what anyone is entitled to do.
 *    RIGHTS_LEGAL_NOTICE states the boundary on the page, and
 *    rights-copy.test.mjs scans this module, the panel and the stylesheet
 *    for phrasing that would cross it.
 *
 * 2. It never invents a value, and never reads a declaration as silence.
 *    The stored column is deliberately not constrained to a vocabulary
 *    (migration 00066), so a reader can meet a word this module does not
 *    know, and a document can omit a field the template lists. An omitted
 *    field is NOT_STATED (null) and an unknown one renders verbatim — never
 *    as its nearest known value, and never the other way round either,
 *    because "the publisher said something else" and "the publisher said
 *    nothing" are the two mistakes that would turn a record into advice.
 *    The marker is null rather than a word so that the second of those can
 *    never swallow the first (see NOT_STATED).
 *
 * 3. It keeps the two axes apart (CLAUDE.md §9.7: knowledge visibility !=
 *    blob accessibility). Metadata visibility says who may see the record;
 *    data access says who may fetch the bytes. RIGHTS_ACCESS_NOTICE says
 *    so on the page, and the panel renders them as separate rows rather
 *    than one "visibility" field.
 *
 * Import-free by construction (like lib/milestones.ts), so the node:test
 * suite (rights.test.mjs) runs it through Node's type stripping, and
 * `pnpm --filter @post/web typecheck` typechecks it.
 */

/** The template version this reader understands. */
export const RIGHTS_TEMPLATE_VERSION = 1;

/**
 * The value standing for "this declaration does not state the field".
 *
 * It is `null`, and it is deliberately NOT a word: "was this field
 * declared?" must be answerable independently of "what was it declared to
 * be?", so the marker has to live outside the space a declaration can
 * occupy. A declaration's values are strings — the stored column accepts
 * any JSON object (migration 00066 does not constrain the vocabulary), and
 * visibility.metadata is an open token whose alphabet
 * (internal/rights.ValidMetadataVisibility: [A-Za-z0-9_.:-]) makes
 * "not_stated" a value a publisher may legally declare. A string sentinel
 * would therefore read that declaration as "the publisher said nothing",
 * which is a value the reader invented (the rule in this module's header).
 *
 * The template's own `unspecified` is a third thing again: something the
 * publisher DID declare, rendered as such, and never conflated with this.
 */
export const NOT_STATED = null;

/** The six usage declarations, in the order the template lists them. */
export const USAGE_AXES = [
  "commercial_use",
  "derivatives",
  "redistribution",
  "model_training",
  "attribution",
  "patent_grant",
] as const;

export type UsageAxis = (typeof USAGE_AXES)[number];

/**
 * The template's six usage declarations. A value is the declared string, or
 * NOT_STATED (null) when the document carries none — the two are never
 * confused, which is why the type is nullable rather than a sentinel word.
 */
export interface RightsUsage {
  commercial_use: string | null;
  derivatives: string | null;
  redistribution: string | null;
  model_training: string | null;
  attribution: string | null;
  patent_grant: string | null;
}

/** The two access axes, kept apart on purpose (values as in RightsUsage). */
export interface RightsVisibility {
  metadata: string | null;
  data_access: string | null;
}

/**
 * A rights document as stored. Every vocabulary field is `string`, not a
 * union of the known tokens: the column accepts what the model does not
 * know, so the reader has to carry what it does not know.
 */
export interface RightsDocument {
  version: number;
  standard_license_id: string | null;
  custom_agreement_ref: string | null;
  usage: RightsUsage;
  visibility: RightsVisibility;
  notes: string | null;
}

/** What a page needs from a stored value: a declaration, or the reason there is none. */
export type RightsRead =
  | {
      kind: "declaration";
      document: RightsDocument;
      /**
       * The usage axes and access axes the document did not carry, by
       * field name. Empty for a document that states all of them.
       */
      unstated: string[];
    }
  | { kind: "none"; reason: string };

/* ------------------------------------------------------------------ */
/* Reading                                                             */
/* ------------------------------------------------------------------ */

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Reads a stored rights value. Accepts the object the API hands back, or
 * the JSON text of one; anything else — null, an array, a scalar, text that
 * is not JSON, an object with no version, a version this reader does not
 * know — comes back as `none` with a sentence a page can show, rather than
 * as a half-populated declaration.
 *
 * The version refusal is the load-bearing one: rendering a v2 document with
 * v1's assumptions would attribute to the publisher a reading the document
 * may not support, and the honest move is to say the view cannot read it.
 *
 * A field the document carries is kept as the string it holds, whatever it
 * spells; only a field that is missing, empty or not a string becomes
 * NOT_STATED, and it is listed in `unstated`. The reader never rewrites a
 * declared value into the absent marker.
 */
export function parseRightsDocument(raw: unknown): RightsRead {
  let value: unknown = raw;

  if (typeof value === "string") {
    try {
      value = JSON.parse(value);
    } catch {
      return { kind: "none", reason: "The stored rights value is not JSON." };
    }
  }

  if (value === null || value === undefined) {
    return { kind: "none", reason: "No rights declaration is recorded for this version." };
  }
  if (!isRecord(value)) {
    return {
      kind: "none",
      reason: `The stored rights value is not a declaration (it is ${describe(value)}).`,
    };
  }

  const version = value.version;
  if (typeof version !== "number") {
    return {
      kind: "none",
      reason: "The stored rights value names no template version, so this view cannot read it as a declaration.",
    };
  }
  if (version !== RIGHTS_TEMPLATE_VERSION) {
    return {
      kind: "none",
      reason: `This declaration follows rights template version ${version}; this view reads version ${RIGHTS_TEMPLATE_VERSION}. Read the declaration itself rather than this summary.`,
    };
  }

  const unstated: string[] = [];
  const usageSource = isRecord(value.usage) ? value.usage : {};
  const visibilitySource = isRecord(value.visibility) ? value.visibility : {};

  const usage = {} as RightsUsage;
  for (const axis of USAGE_AXES) {
    const declared = usageSource[axis];
    if (typeof declared === "string" && declared !== "") {
      usage[axis] = declared;
    } else {
      usage[axis] = NOT_STATED;
      unstated.push(axis);
    }
  }

  const visibility = {} as RightsVisibility;
  for (const axis of ["metadata", "data_access"] as const) {
    const declared = visibilitySource[axis];
    if (typeof declared === "string" && declared !== "") {
      visibility[axis] = declared;
    } else {
      visibility[axis] = NOT_STATED;
      unstated.push(axis);
    }
  }

  return {
    kind: "declaration",
    document: {
      version,
      standard_license_id: text(value.standard_license_id),
      custom_agreement_ref: text(value.custom_agreement_ref),
      usage,
      visibility,
      notes: text(value.notes),
    },
    unstated,
  };
}

function text(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function describe(value: unknown): string {
  if (Array.isArray(value)) return "a list";
  if (typeof value === "string") return "a piece of text";
  if (typeof value === "number") return "a number";
  if (typeof value === "boolean") return "a true/false value";
  return "a value of another kind";
}

/* ------------------------------------------------------------------ */
/* Labels                                                              */
/* ------------------------------------------------------------------ */

/**
 * A value outside the vocabulary is rendered verbatim with a marker, never
 * mapped to the nearest known token: the marker says the view does not know
 * the word, which is true, while a mapping would report a declaration the
 * publisher did not make.
 */
function verbatim(value: string): string {
  return `${value} (a value this view does not know)`;
}

function labelFrom(labels: Record<string, string>, value: string | null): string {
  // The one test that means "the document states nothing here". Every other
  // string — including one spelled like an old sentinel — is a declaration
  // and goes down the verbatim path.
  if (value === NOT_STATED) return "Not stated";
  return labels[value] ?? verbatim(value);
}

/** The usage-permission tokens the template defines: allowed | restricted | unspecified. */
const PERMISSION_LABELS: Record<string, string> = {
  allowed: "Allowed",
  restricted: "Restricted",
  unspecified: "Not specified",
};

/** The attribution tokens: required | not_required | unspecified. */
const ATTRIBUTION_LABELS: Record<string, string> = {
  required: "Required",
  not_required: "Not required",
  unspecified: "Not specified",
};

/** The patent-grant tokens: none | see_agreement | explicit. */
const PATENT_GRANT_LABELS: Record<string, string> = {
  none: "None declared",
  see_agreement: "See the agreement",
  explicit: "Explicitly granted",
};

/** The data-access tokens: open | restricted. */
const DATA_ACCESS_LABELS: Record<string, string> = {
  open: "Open",
  restricted: "Restricted",
};

/** Metadata visibility is an open token; the template's default is the only one with a name. */
const METADATA_LABELS: Record<string, string> = {
  project_policy: "Project policy",
};

/** The declared value of one usage axis, as a page renders it. */
export function usageLabel(axis: UsageAxis, value: string | null): string {
  switch (axis) {
    case "commercial_use":
    case "derivatives":
    case "redistribution":
    case "model_training":
      return labelFrom(PERMISSION_LABELS, value);
    case "attribution":
      return labelFrom(ATTRIBUTION_LABELS, value);
    case "patent_grant":
      return labelFrom(PATENT_GRANT_LABELS, value);
  }
}

/** The declared data access, as a page renders it. */
export function dataAccessLabel(value: string | null): string {
  return labelFrom(DATA_ACCESS_LABELS, value);
}

/** The declared metadata visibility, as a page renders it. */
export function metadataLabel(value: string | null): string {
  return labelFrom(METADATA_LABELS, value);
}

/**
 * One row of the usage table: the axis, its heading, its declared value and
 * the rendered line. `declared` is the string the document carries, or
 * NOT_STATED (null) when it carries none — the panel writes it into
 * `data-declared`, and a null there is what makes an absent axis render as
 * an absent attribute rather than as a value the reader made up.
 */
export interface RightsUsageRow {
  axis: UsageAxis;
  heading: string;
  declared: string | null;
  rendered: string;
}

const AXIS_HEADINGS: Record<UsageAxis, string> = {
  commercial_use: "Commercial use",
  derivatives: "Derivatives",
  redistribution: "Redistribution",
  model_training: "Model training",
  attribution: "Attribution",
  patent_grant: "Patent grant",
};

/**
 * The usage declarations as an ordered table. The order is the template's,
 * fixed here rather than taken from the stored object's key order, so two
 * declarations listing the same values render identically.
 */
export function usageRows(document: RightsDocument): RightsUsageRow[] {
  return USAGE_AXES.map((axis) => ({
    axis,
    heading: AXIS_HEADINGS[axis],
    declared: document.usage[axis],
    rendered: usageLabel(axis, document.usage[axis]),
  }));
}

/* ------------------------------------------------------------------ */
/* Copy the page shows                                                 */
/* ------------------------------------------------------------------ */

/**
 * The declaration's own two references.
 *
 * The license id is shown exactly as declared. This view does not check it
 * against a license registry, does not expand it into terms, and does not
 * link it out: a curated list of license texts is a registry this project
 * does not maintain (ADR-009 leaves licensing to the standard instruments),
 * and a link would read as this project standing behind the id. The same
 * holds for the agreement reference — shown as recorded, not fetched.
 */
export function licenseLine(document: RightsDocument): string {
  return document.standard_license_id ?? "No standard license named";
}

export function agreementLine(document: RightsDocument): string {
  return document.custom_agreement_ref ?? "No separate agreement referenced";
}

/**
 * The boundary sentence. Every rights view shows it, and it is the sentence
 * the copy scan exists to protect: a page that dropped it would be
 * presenting a publisher's declaration as this platform's statement about
 * the law.
 */
export const RIGHTS_LEGAL_NOTICE =
  "This is the publisher's own declaration, recorded and shown as it was made. " +
  "POST does not interpret it, does not check it against any license registry, " +
  "and does not give legal advice. Whether a particular use is permitted is a " +
  "question for the declaration, the agreement it references, and the law that " +
  "applies to you.";

/**
 * The two-axes sentence (CLAUDE.md §9.7). Metadata visibility and data
 * access answer different questions, and a reader who takes one for the
 * other will draw the wrong conclusion about the files.
 */
export const RIGHTS_ACCESS_NOTICE =
  "Metadata visibility and data access are separate. A visible record does not " +
  "make the files downloadable, and restricted data access does not hide the " +
  "record.";

/**
 * The line explaining a partial declaration. A document stored before every
 * field was stated, or written without one, is shown as what it is.
 */
export function unstatedNotice(unstated: string[]): string | null {
  if (unstated.length === 0) return null;
  const fields = unstated.map(fieldName).join(", ");
  return `This declaration does not state: ${fields}. Treat those as unstated rather than as permitted or refused.`;
}

/** The human name of a document field, for the unstated line. */
export function fieldName(field: string): string {
  if (field in AXIS_HEADINGS) return AXIS_HEADINGS[field as UsageAxis];
  if (field === "metadata") return "Metadata visibility";
  if (field === "data_access") return "Data access";
  return field;
}
