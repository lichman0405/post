/**
 * The RSG write/read client the authoring surface uses (WEB-06a).
 *
 * WHY THIS IS ITS OWN MODULE. The Research tab's browse half (WEB-05) reads
 * exactly one route with plain `fetch`. Authoring adds six: the branch list,
 * branch creation, the branch-scoped graph slice, object creation, the next
 * object version, typed relations and the pr-gate check. Keeping them here —
 * import-free, token supplied by the caller — means the components hold no
 * request shapes, and the one place a request is built is the one place the
 * response envelope is decoded.
 *
 * The envelope is the API's (cmd/api/authhttp): `{code, message, request_id,
 * retryable}` on any non-2xx. `ResearchApiError` keeps the code so a caller
 * can map BRANCH_NAME_TAKEN onto its field, and keeps the status so a 404
 * (a project or object the caller may not see) is distinguishable from a
 * 500 without parsing prose.
 */

export interface ResearchFetchInit {
  method?: string;
  headers?: Record<string, string>;
  body?: string;
  credentials?: "include" | "same-origin" | "omit";
  signal?: AbortSignal;
}

export type ResearchFetch = (input: string, init?: ResearchFetchInit) => Promise<{
  ok: boolean;
  status: number;
  json(): Promise<unknown>;
}>;

export class ResearchApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly retryable: boolean;

  constructor(status: number, code: string, message: string, retryable: boolean) {
    super(message);
    this.name = "ResearchApiError";
    this.status = status;
    this.code = code;
    this.retryable = retryable;
  }
}

export interface ResearchClientOptions {
  /** The session-bound CSRF token (lib/auth's sessionTokenStorage). */
  csrfToken?: () => string | null;
  fetch?: ResearchFetch;
}

export interface ResearchBranch {
  id: string;
  name: string;
  purpose?: string | null;
  visibility: string;
  lifecycle_state: string;
  is_main: boolean;
  base_state_id?: string | null;
  created_at: string;
}

export interface ResearchObjectRef {
  id: string;
  object_type: string;
  current_version: number;
  version_id: string;
  version_no: number;
  title: string;
  lifecycle_state: string;
  payload: unknown;
}

export interface ResearchRelationRef {
  id: string;
  relation_type: string;
  source_object_version_id: string;
  target_object_version_id: string;
}

export interface ResearchSlice {
  project_id: string;
  state_id?: string | null;
  objects: ResearchObjectRef[];
  relations: ResearchRelationRef[];
}

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

export interface ResearchClient {
  listBranches(projectId: string): Promise<ResearchBranch[]>;
  createBranch(projectId: string, input: { name: string; purpose: string }): Promise<ResearchBranch>;
  branchSlice(projectId: string, branchId: string): Promise<ResearchSlice>;
  createObject(
    projectId: string,
    branchId: string,
    input: { objectType: string; payload: Record<string, unknown> },
  ): Promise<ResearchObjectRef>;
  createVersion(
    projectId: string,
    branchId: string,
    objectId: string,
    input: { expectedVersion: number; patch: Record<string, unknown> },
  ): Promise<ResearchObjectRef>;
  createRelation(
    projectId: string,
    branchId: string,
    input: { relationType: string; sourceVersionId: string; targetVersionId: string },
  ): Promise<ResearchRelationRef>;
  validateBranch(projectId: string, branchId: string, gate: string): Promise<GateReport>;
}

/**
 * One row per object, at its newest version in the slice.
 *
 * `/query` is a GRAPH read: besides each object at its as-of version it also
 * returns the exact versions a relation pins (an edge keeps the version it
 * meant, docs/09). On a branch where an object was continued after a relation
 * pointed at its earlier version, that object therefore appears twice. The
 * authoring inventory asks a different question — "which objects does this
 * branch hold" — so it keeps the as-of row and drops the pinned-older one;
 * otherwise one hypothesis would be listed, and offered as a link/select
 * target, once per version.
 */
export function newestVersionPerObject(objects: ResearchObjectRef[]): ResearchObjectRef[] {
  const byID = new Map<string, ResearchObjectRef>();
  const order: string[] = [];
  for (const object of objects) {
    const seen = byID.get(object.id);
    if (seen === undefined) {
      order.push(object.id);
      byID.set(object.id, object);
      continue;
    }
    if (object.version_no > seen.version_no) byID.set(object.id, object);
  }
  // First-seen order, one row per object: the earlier slot the object was
  // listed in, now carrying its newest version.
  const out: ResearchObjectRef[] = [];
  for (const id of order) {
    const object = byID.get(id);
    if (object !== undefined) out.push(object);
  }
  return out;
}

function decodeEnvelope(body: unknown): { code: string; message: string; retryable: boolean } {
  if (body !== null && typeof body === "object" && !Array.isArray(body)) {
    const record = body as Record<string, unknown>;
    return {
      code: typeof record.code === "string" ? record.code : "",
      message: typeof record.message === "string" ? record.message : "",
      retryable: record.retryable === true,
    };
  }
  return { code: "", message: "", retryable: false };
}

/** Build the client for one API origin. */
export function createResearchClient(apiBaseUrl: string, options: ResearchClientOptions = {}): ResearchClient {
  const base = apiBaseUrl.replace(/\/+$/, "");
  const fetchFn: ResearchFetch =
    options.fetch ?? (fetch as unknown as ResearchFetch);

  async function call(
    method: string,
    path: string,
    body?: unknown,
    signal?: AbortSignal,
  ): Promise<unknown> {
    // The CSRF header name and the JSON content type are written literally:
    // the SAST rule set refuses a computed member write on a plain object,
    // and this is the one place the header is set.
    const csrf = options.csrfToken?.() ?? null;
    const headers: Record<string, string> =
      body === undefined ? { accept: "application/json" } : { accept: "application/json", "Content-Type": "application/json" };
    if (csrf !== null && method !== "GET") headers["X-CSRF-Token"] = csrf;
    const init: ResearchFetchInit = { method, headers, credentials: "include" };
    if (body !== undefined) init.body = JSON.stringify(body);
    if (signal !== undefined) init.signal = signal;
    const response = await fetchFn(base + path, init);
    let parsed: unknown = null;
    try {
      parsed = await response.json();
    } catch {
      parsed = null;
    }
    if (!response.ok) {
      const envelope = decodeEnvelope(parsed);
      throw new ResearchApiError(
        response.status,
        envelope.code,
        envelope.message === "" ? `request failed (${response.status})` : envelope.message,
        envelope.retryable,
      );
    }
    return parsed;
  }

  const projectPath = (projectId: string): string => `/api/v1/projects/${encodeURIComponent(projectId)}`;

  return {
    async listBranches(projectId) {
      const body = await call("GET", `${projectPath(projectId)}/branches`);
      const record = body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {};
      return Array.isArray(record.branches) ? (record.branches as ResearchBranch[]) : [];
    },

    async createBranch(projectId, input) {
      const body = await call("POST", `${projectPath(projectId)}/branches`, {
        name: input.name,
        purpose: input.purpose === "" ? null : input.purpose,
      });
      return body as ResearchBranch;
    },

    async branchSlice(projectId, branchId) {
      const body = await call(
        "GET",
        `${projectPath(projectId)}/query?branch_id=${encodeURIComponent(branchId)}`,
      );
      const record = body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {};
      return {
        project_id: typeof record.project_id === "string" ? record.project_id : projectId,
        state_id: typeof record.state_id === "string" ? record.state_id : null,
        objects: Array.isArray(record.objects)
          ? newestVersionPerObject(record.objects as ResearchObjectRef[])
          : [],
        relations: Array.isArray(record.relations) ? (record.relations as ResearchRelationRef[]) : [],
      };
    },

    async createObject(projectId, branchId, input) {
      const body = await call(
        "POST",
        `${projectPath(projectId)}/branches/${encodeURIComponent(branchId)}/objects`,
        { object_type: input.objectType, payload: input.payload },
      );
      return body as ResearchObjectRef;
    },

    async createVersion(projectId, branchId, objectId, input) {
      const body = await call(
        "POST",
        `${projectPath(projectId)}/branches/${encodeURIComponent(branchId)}/objects/${encodeURIComponent(objectId)}:version`,
        { expected_version: input.expectedVersion, patch: input.patch },
      );
      return body as ResearchObjectRef;
    },

    async createRelation(projectId, branchId, input) {
      const body = await call(
        "POST",
        `${projectPath(projectId)}/branches/${encodeURIComponent(branchId)}/relations`,
        {
          relation_type: input.relationType,
          source_object_version_id: input.sourceVersionId,
          target_object_version_id: input.targetVersionId,
        },
      );
      return body as ResearchRelationRef;
    },

    async validateBranch(projectId, branchId, gate) {
      // The route's path segment is "{branchId}:validate"; the ":validate"
      // suffix is part of the path, not a query.
      const body = await call(
        "POST",
        `${projectPath(projectId)}/branches/${encodeURIComponent(branchId)}:validate`,
        { gate },
      );
      const record = body !== null && typeof body === "object" ? (body as Record<string, unknown>) : {};
      return {
        gate: typeof record.gate === "string" ? record.gate : gate,
        verdict: typeof record.verdict === "string" ? record.verdict : "",
        results: Array.isArray(record.results) ? (record.results as GateResult[]) : [],
        explanation: typeof record.explanation === "string" ? record.explanation : "",
      };
    },
  };
}
