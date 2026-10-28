/**
 * The Research tab's data model (WEB-05 / G06).
 *
 * WHY THIS IS A PURE MODULE. The map and the list must show the SAME object
 * set — an outline whose picture and whose rows disagree is the failure a
 * reader cannot see, because each surface looks complete on its own. Both
 * are therefore rendered from ONE function (`researchNodes`) rather than
 * from two traversals that happen to agree today. This module holds that
 * function and nothing else: no React, no fetch, no copy (every returned
 * string is either API data or a stable domain code the component resolves
 * through the message catalog).
 *
 * The outline is the server's aggregation (internal/application/rsg
 * OutlineQuestion / OutlineFinding): questions are the tree, findings are
 * the other axis, and an object joins a question only through an
 * `addresses_question` relation. That shape is why the map's edges are
 * derived from the outline's own lists instead of a second read.
 */

export type ResearchView = "questions" | "findings";

/** One object reference in the outline (a row of an addressed-by list, a
 *  claim of a finding, or the unassigned remainder). */
export interface OutlineRef {
  object_id: string;
  object_type: string;
  /** Empty/absent when the as-of version carries no branch: the coordinate
   *  does not exist, so no link is built (never a fabricated one). */
  branch_id?: string;
  title: string;
}

export interface OutlineClaim {
  /** Absent when the pinned claim version does not resolve to a claim of
   *  this project (an external reference) — such a row is shown but has no
   *  drill-down. */
  object_id?: string;
  version_id: string;
  branch_id?: string;
  title: string;
  resolved: boolean;
}

export interface OutlineQuestion {
  object_id: string;
  version_id: string;
  branch_id?: string;
  title: string;
  statement: string;
  question_state: string;
  parent_question_id: string;
  children: OutlineQuestion[];
  hypotheses: OutlineRef[];
  findings: OutlineRef[];
  other_objects: OutlineRef[];
}

export interface OutlineFinding {
  object_id: string;
  version_id: string;
  branch_id?: string;
  title: string;
  statement: string;
  finding_type: string;
  assessment: string;
  claims: OutlineClaim[];
  questions: OutlineRef[];
}

export interface OutlineCounts {
  questions: number;
  findings: number;
  hypotheses: number;
  claims: number;
  other_objects: number;
}

export interface ResearchOutline {
  project_id: string;
  project_slug: string;
  project_name: string;
  counts: OutlineCounts;
  questions: OutlineQuestion[];
  findings: OutlineFinding[];
  unassigned: OutlineRef[];
}

/** Where a node sits in the outline — the map's lanes and the list's
 *  grouping both read this, so the two cannot drift. */
export type NodeRole = "question" | "linked" | "finding" | "claim" | "unassigned";

/** One adjacency of a node, as the outline states it. `kind` is a stable
 *  relation/containment code (e.g. `addresses_question`), never a label. */
export interface NodeRelation {
  objectId: string;
  title: string;
  kind: string;
}

/** One object of the scoped outline, rendered identically by the map and
 *  the list. */
export interface ResearchNode {
  objectId: string;
  objectType: string;
  title: string;
  /** The version's own statement/summary (API data; may be empty). */
  statement: string;
  /** A stable state code (question_state, finding assessment, …) shown as a
   *  badge; "" when the object type has none. */
  state: string;
  role: NodeRole;
  /** The drill-down branch; "" means the node has no link. */
  branchId: string;
  relations: NodeRelation[];
}

/** An object's outline coordinate: the facts a detail read needs. */
export interface ObjectCoordinate {
  objectId: string;
  objectType: string;
  title: string;
  branchId: string;
}

/* ---- The detail read (GET …/objects/{objectId}?include=versions,relations) ----
 *
 * The route's default payload is the single object + its selected version,
 * which is what every existing consumer parses. The browse surface opts
 * into the richer shape (`include=versions,relations`), which adds the
 * version log and the relations with both endpoints resolved. These types
 * describe that opt-in shape only. */

export interface ObjectSchemaRef {
  id: string;
  version: string;
}

export interface ObjectVersionPayload {
  version_id: string;
  version_no: number;
  state_id: string;
  title: string;
  lifecycle_state: string;
  schema_ref: ObjectSchemaRef;
  /** The version's own payload (immutable — an old version keeps its
   *  content, which is why selecting it must render THIS object). */
  payload: unknown;
  created_by: string;
  created_at: string;
  /** The object's newest version (the log's head). */
  current: boolean;
}

export interface ObjectRelationPayload {
  relation_type: string;
  /** "outgoing" when the selected object is the relation's source,
   *  "incoming" when it is the target. */
  direction: "outgoing" | "incoming";
  other_object_id: string;
  other_object_type: string;
  other_title: string;
  version_no: number;
  created_by: string;
  created_at: string;
}

export interface ObjectDetail {
  id: string;
  project_id: string;
  object_type: string;
  current_version: number;
  version_id: string;
  version_no: number;
  state_id: string;
  title: string;
  lifecycle_state: string;
  schema_ref: ObjectSchemaRef;
  payload: unknown;
  project_slug: string;
  project_name: string;
  branch_id: string;
  branch_name: string;
  selected_version_no: number;
  versions: ObjectVersionPayload[];
  relations: ObjectRelationPayload[];
}

/** The web's own detail URL. No branch in the path: the detail view
 *  resolves the coordinate from the outline, so an old link cannot pin a
 *  branch the object has left. */
export function objectHref(projectId: string, objectId: string, params?: { version?: number; from?: string; fromVersion?: number }): string {
  const query = new URLSearchParams();
  if (params?.version !== undefined) query.set("version", String(params.version));
  if (params?.from !== undefined && params.from !== "") {
    query.set("from", params.from);
    if (params.fromVersion !== undefined) query.set("fromVersion", String(params.fromVersion));
  }
  const suffix = query.toString();
  return `/projects/${projectId}/research/objects/${objectId}${suffix === "" ? "" : `?${suffix}`}`;
}

/** The research tab's own URL for a view, optionally focused on one node. */
export function researchHref(projectId: string, view: ResearchView, focus?: string): string {
  const query = new URLSearchParams();
  query.set("view", view);
  if (focus !== undefined && focus !== "") query.set("focus", focus);
  return `/projects/${projectId}/research?${query.toString()}`;
}

/** Normalizes the `?view=` value. Unknown/missing falls back to questions —
 *  a navigation is never an error. */
export function researchViewOf(raw: string | string[] | undefined): ResearchView {
  return firstParam(raw) === "findings" ? "findings" : "questions";
}

export function firstParam(raw: string | string[] | undefined): string {
  if (Array.isArray(raw)) return raw[0] ?? "";
  return raw ?? "";
}

/** Parses the `?version=` selector. Returns null when absent (latest) and
 *  `"invalid"` when present but not a positive integer — the caller renders
 *  a refusal rather than silently showing the latest version, which would
 *  answer a reader with a version they did not ask for. */
export function versionParamOf(raw: string | string[] | undefined): number | null | "invalid" {
  const value = firstParam(raw);
  if (value === "") return null;
  if (!/^[1-9][0-9]*$/.test(value)) return "invalid";
  return Number(value);
}

function branchOf(version: { branch_id?: string }): string {
  return version.branch_id ?? "";
}

function questionNode(q: OutlineQuestion, relations: NodeRelation[]): ResearchNode {
  return {
    objectId: q.object_id,
    objectType: "research_question",
    title: q.title,
    statement: q.statement,
    state: q.question_state,
    role: "question",
    branchId: branchOf(q),
    relations,
  };
}

function findingNode(f: OutlineFinding, relations: NodeRelation[]): ResearchNode {
  return {
    objectId: f.object_id,
    objectType: "finding",
    title: f.title,
    statement: f.statement,
    state: f.assessment === "" ? f.finding_type : f.assessment,
    role: "finding",
    branchId: branchOf(f),
    relations,
  };
}

function refNode(ref: OutlineRef, role: NodeRole, relations: NodeRelation[]): ResearchNode {
  return {
    objectId: ref.object_id,
    objectType: ref.object_type,
    title: ref.title,
    statement: "",
    state: "",
    role,
    branchId: branchOf(ref),
    relations,
  };
}

function claimNode(claim: OutlineClaim, relations: NodeRelation[]): ResearchNode {
  return {
    objectId: claim.object_id ?? "",
    objectType: "claim",
    title: claim.title,
    statement: "",
    state: "",
    role: "claim",
    branchId: branchOf(claim),
    relations,
  };
}

/**
 * The scoped object set for one view. Both the map and the list render THIS
 * array, in this order: equality between them is structural, not a
 * convention the two renderers have to keep.
 *
 * Questions view: every question of the tree (parents before children),
 * everything that addresses a question (hypothesis / finding / any other
 * object, experiments included), and the unassigned remainder.
 * Findings view: every finding, the claims it cites (resolved ones), the
 * questions it addresses, and the same unassigned remainder.
 *
 * Deduped by object id, first occurrence wins: an object that is both a
 * finding and linked from a question renders once, so a reader never sees
 * the same object twice in one picture.
 */
export function researchNodes(outline: ResearchOutline, view: ResearchView): ResearchNode[] {
  const nodes: ResearchNode[] = [];
  const seen = new Set<string>();
  const push = (node: ResearchNode): void => {
    if (node.objectId === "" || seen.has(node.objectId)) return;
    seen.add(node.objectId);
    nodes.push(node);
  };

  if (view === "questions") {
    const walk = (q: OutlineQuestion): void => {
      const children = q.children.map((child) => ({
        objectId: child.object_id,
        title: child.title,
        kind: "parent_question_of",
      }));
      push(questionNode(q, children));
      for (const ref of q.hypotheses) {
        push(refNode(ref, "linked", [{ objectId: q.object_id, title: q.title, kind: "answers_question" }]));
      }
      for (const ref of q.findings) {
        push(refNode(ref, "linked", [{ objectId: q.object_id, title: q.title, kind: "answers_question" }]));
      }
      for (const ref of q.other_objects) {
        push(refNode(ref, "linked", [{ objectId: q.object_id, title: q.title, kind: "answers_question" }]));
      }
      q.children.forEach(walk);
    };
    outline.questions.forEach(walk);
  } else {
    for (const finding of outline.findings) {
      const relations: NodeRelation[] = [];
      for (const claim of finding.claims) {
        if (claim.object_id !== undefined) {
          const id = claim.object_id;
          relations.push({ objectId: id, title: claim.title, kind: "cites_claim" });
        }
      }
      for (const question of finding.questions) {
        relations.push({ objectId: question.object_id, title: question.title, kind: "addresses_question" });
      }
      push(findingNode(finding, relations));
      for (const claim of finding.claims) {
        if (claim.object_id === undefined) continue;
        push(claimNode(claim, [{ objectId: finding.object_id, title: finding.title, kind: "cited_by_finding" }]));
      }
      for (const question of finding.questions) {
        push(refNode(question, "linked", [{ objectId: finding.object_id, title: finding.title, kind: "addressed_by_finding" }]));
      }
    }
  }

  for (const ref of outline.unassigned) {
    push(refNode(ref, "unassigned", []));
  }

  return nodes;
}

/**
 * Every object the outline names, with its drill-down coordinate. The
 * detail view resolves both the branch it must read and the object's place
 * in the outline from this index; an object the outline does not name has
 * no coordinate and therefore no detail page.
 */
export function researchObjects(outline: ResearchOutline): Map<string, ObjectCoordinate> {
  const index = new Map<string, ObjectCoordinate>();
  const add = (objectId: string | undefined, objectType: string, title: string, branchId: string): void => {
    if (objectId === undefined || objectId === "" || index.has(objectId)) return;
    index.set(objectId, { objectId, objectType, title, branchId });
  };
  const addRef = (ref: OutlineRef): void => add(ref.object_id, ref.object_type, ref.title, branchOf(ref));
  const walk = (q: OutlineQuestion): void => {
    add(q.object_id, "research_question", q.title, branchOf(q));
    q.hypotheses.forEach(addRef);
    q.findings.forEach(addRef);
    q.other_objects.forEach(addRef);
    q.children.forEach(walk);
  };
  outline.questions.forEach(walk);
  for (const finding of outline.findings) {
    add(finding.object_id, "finding", finding.title, branchOf(finding));
    for (const claim of finding.claims) add(claim.object_id, "claim", claim.title, branchOf(claim));
    finding.questions.forEach(addRef);
  }
  outline.unassigned.forEach(addRef);
  return index;
}

/** The object types the catalog has a label for. Anything else renders its
 *  own stable code (humanized) rather than a missing-key marker: a type this
 *  build predates is data, and data is shown as it arrived.
 *
 *  The catalog key is written out rather than derived from the object type:
 *  catalog keys may not contain underscores (lib/i18n's key-shape test), and
 *  a derived key would also have made a new type silently unreferenceable.
 *
 *  A Map, not a Record: the type code comes from the outline (data), and a
 *  Map lookup keeps that data out of dynamic property access (SAST
 *  security/detect-object-injection) — the same shape the WEB-03 org
 *  identity index uses. */
const OBJECT_TYPE_KEYS: ReadonlyMap<string, string> = new Map([
  ["research_question", "research.type.research-question"],
  ["hypothesis", "research.type.hypothesis"],
  ["experiment", "research.type.experiment"],
  ["finding", "research.type.finding"],
  ["claim", "research.type.claim"],
  ["material", "research.type.material"],
  ["dataset", "research.type.dataset"],
  ["protocol", "research.type.protocol"],
  ["sample", "research.type.sample"],
  ["calculation", "research.type.calculation"],
  ["external_reference", "research.type.external-reference"],
]);

/** The catalog key for an object type, or null when the type is unknown. */
export function objectTypeKey(objectType: string): string | null {
  return OBJECT_TYPE_KEYS.get(objectType) ?? null;
}

/** The catalog key for an outline adjacency, or null (unknown kinds render
 *  their own code). Same reason for the written-out keys as above. */
const RELATION_KIND_KEYS: ReadonlyMap<string, string> = new Map([
  ["addresses_question", "research.relation.addresses-question"],
  ["parent_question_of", "research.relation.parent-question-of"],
  ["cites_claim", "research.relation.cites-claim"],
  ["cited_by_finding", "research.relation.cited-by-finding"],
  ["addressed_by_finding", "research.relation.addressed-by-finding"],
]);

export function relationKindKey(kind: string): string | null {
  return RELATION_KIND_KEYS.get(kind) ?? null;
}

/** Turns a stable code into readable text WITHOUT translating it: this is
 *  the fallback for values the catalog was not taught, and a reader must see
 *  the code unchanged rather than a miss marker (docs/28 §3). */
export function humanizeCode(code: string): string {
  return code.replaceAll("_", " ");
}

/** The counts summary in display order (stable codes + numbers only). */
export function countEntries(outline: ResearchOutline): Array<{ key: string; count: number }> {
  return [
    { key: "research.count.questions", count: outline.counts.questions },
    { key: "research.count.hypotheses", count: outline.counts.hypotheses },
    { key: "research.count.claims", count: outline.counts.claims },
    { key: "research.count.findings", count: outline.counts.findings },
    { key: "research.count.other", count: outline.counts.other_objects },
  ];
}

/** The lanes the map draws, in first-seen order: one lane per object type
 *  keeps the picture grouped by what the objects ARE while the list stays
 *  in outline order (grouping by role would make the list the type dump
 *  the outline must not be). */
export function mapLanes(nodes: ResearchNode[]): Array<{ objectType: string; nodes: ResearchNode[] }> {
  const lanes = new Map<string, ResearchNode[]>();
  for (const node of nodes) {
    const lane = lanes.get(node.objectType);
    if (lane === undefined) lanes.set(node.objectType, [node]);
    else lane.push(node);
  }
  return [...lanes.entries()].map(([objectType, laneNodes]) => ({ objectType, nodes: laneNodes }));
}

/** The payload fields a detail page renders, in a fixed order: the schema's
 *  own field names (stable codes) with their values as text. Nested values
 *  are rendered as compact JSON — payloads are data, not copy. */
export function payloadEntries(payload: unknown): Array<{ field: string; value: string }> {
  if (payload === null || typeof payload !== "object" || Array.isArray(payload)) return [];
  const out: Array<{ field: string; value: string }> = [];
  for (const [field, value] of Object.entries(payload as Record<string, unknown>)) {
    out.push({ field, value: payloadValueText(value) });
  }
  return out;
}

function payloadValueText(value: unknown): string {
  if (value === null || value === undefined) return "";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(value);
}
