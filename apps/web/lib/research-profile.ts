/**
 * Client-side Research Profile / Organization Profile reader (T0808).
 *
 * Reads the two public profile surfaces — GET /api/v1/users/{id}/research-profile
 * and GET /api/v1/organizations/{slug}/profile (both anonymous-readable, no
 * CSRF) — and renders them. docs/42 lists what a Research Profile shows:
 * affiliations, public contribution dimensions, assets, reuse, reproductions.
 *
 * Import-free by construction (like lib/profile.ts and lib/config.ts), so the
 * node:test suite (research-profile.test.mjs) runs it through Node's type
 * stripping. Its ApiError-shaped error deliberately mirrors lib/auth.ts and
 * lib/profile.ts: one shape per import-free module, because an import between
 * them would break that suite.
 *
 * WHAT THIS MODULE REFUSES, and why it is not just a shape check:
 * parsePersonProfile/parseOrganizationProfile throw when a payload carries a
 * key in the score vocabulary (score/rank/rating/weight/reputation/...). The
 * API is forbidden from sending one — docs/13 §4 ("禁止单一分数"), docs/13 §6,
 * CLAUDE.md §9 invariant 13 — and the rule is asserted there on the raw
 * response bytes. This module asserts it a second time, at the point of
 * rendering, so that a regression in the transport renders an error line
 * instead of a number a researcher would read as a judgement about a person.
 * A client that renders whatever it is handed is where a score would become
 * real.
 */

/** The API error envelope (docs/22 §5): stable codes, no dependency detail. */
export interface ResearchProfileErrorEnvelope {
  code: string;
  message: string;
  request_id?: string;
  retryable?: boolean;
}

/**
 * ResearchProfileError is the module's single failure type: the API's
 * envelope for a non-2xx, and a synthetic UNEXPECTED for a 2xx whose body
 * this module will not render (see SCORE_LIKE).
 */
export class ResearchProfileError extends Error {
  readonly code: string;
  readonly status: number;
  readonly retryable: boolean;

  constructor(status: number, envelope: ResearchProfileErrorEnvelope) {
    super(envelope.message);
    this.name = "ResearchProfileError";
    this.code = envelope.code;
    this.status = status;
    this.retryable = envelope.retryable ?? status === 503;
  }
}

/** A project identity a row may name. `null` when it may not be named —
 *  never a placeholder: a withheld project is indistinguishable from no
 *  project at all, which is the point (a private project must not leak its
 *  existence through a profile). Only the dimensions whose rule withholds a
 *  project while keeping the row use the nullable form; a reuse row is
 *  dropped by the API instead, so it names its project or does not exist. */
export interface ProfileProjectRef {
  id: string;
  slug: string;
  name: string;
  url: string;
}

export interface ProfileOrganizationRef {
  id: string;
  slug: string;
  name: string;
  url: string;
}

export interface ProfilePersonRef {
  id: string;
  handle: string;
  display_name: string;
  url: string;
}

export interface ProfileAffiliation {
  organization: ProfileOrganizationRef | null;
  role: string;
  affiliation_start: string | null;
  affiliation_end: string | null;
  verified: boolean;
}

export interface ProfileContribution {
  event_type: string;
  role_codes: string[];
  occurred_at: string;
  accepted_context: boolean;
  released_context: boolean;
  via: string;
  project: ProfileProjectRef | null;
  actor?: ProfilePersonRef;
}

export interface ProfileAsset {
  pid: string;
  title: string;
  asset_type: string;
  version: string;
  url: string;
  role?: string;
  project: ProfileProjectRef | null;
  published_at: string;
}

export interface ProfileReuse {
  pid: string;
  title: string;
  version: string;
  url: string;
  asset_type: string;
  dependency_type: string;
  declared_at: string;
  /** Not nullable: a usage whose project may not be named is dropped by the
   *  API rather than sent with a withheld identity (the model drops it),
   *  because a usage IS the using project's declaration — a reuse row without
   *  its project has nothing left to say. The dimensions whose project can be
   *  withheld (assets, reproductions) keep the nullable ProfileProjectRef. */
  project: ProfileProjectRef;
}

export interface ProfileReproduction {
  relation: string;
  review_state: string;
  created_at: string;
  project: ProfileProjectRef | null;
}

export interface ProfileOrgProject {
  id: string;
  slug: string;
  name: string;
  purpose: string;
  activity_status: string;
  url: string;
}

export interface PersonResearchProfile {
  person: { id: string; handle: string; display_name: string; bio: string; url: string };
  affiliations: ProfileAffiliation[];
  contributions: ProfileContribution[];
  assets: ProfileAsset[];
  reuse: ProfileReuse[];
  reproductions: ProfileReproduction[];
  projects: ProfileProjectRef[];
}

export interface OrganizationResearchProfile {
  organization: { id: string; slug: string; name: string; description: string; url: string };
  projects: ProfileOrgProject[];
  activity: ProfileContribution[];
  assets: ProfileAsset[];
}

/** The reader: one per API origin. */
export interface ResearchProfileClient {
  person(userId: string): Promise<PersonResearchProfile>;
  organization(slug: string): Promise<OrganizationResearchProfile>;
}

type FetchLike = (
  input: string,
  init?: {
    method?: string;
    headers?: Record<string, string>;
    credentials?: string;
  },
) => Promise<{ status: number; json(): Promise<unknown> }>;

export interface ResearchProfileClientOptions {
  /** Fetch implementation (tests inject a fake); default: global fetch. */
  fetch?: FetchLike;
}

/**
 * SCORE_LIKE is the field-name vocabulary a profile payload may not use:
 * docs/13 §4 forbids a single score and CLAUDE.md §9 invariant 13 forbids a
 * Truth Score / Research Score. The API's own unit and e2e suites assert it
 * on the response bytes; this is the same rule at the rendering boundary.
 */
const SCORE_LIKE =
  /(score|rank|rating|weight|reputation|total|count|impact|percentile|points|karma|metric)/i;

/**
 * refuseScoreFields walks the whole parsed payload, at any depth, and throws
 * for the first key whose NAME is in the score vocabulary. It is on the name
 * rather than on the value because a number is legitimate everywhere in this
 * payload (a version number, a year) — what may not exist is a field that
 * means "how good".
 */
function refuseScoreFields(value: unknown, path = "$"): void {
  if (Array.isArray(value)) {
    value.forEach((item, i) => refuseScoreFields(item, `${path}[${i}]`));
    return;
  }
  if (typeof value !== "object" || value === null) return;
  for (const [key, nested] of Object.entries(value as Record<string, unknown>)) {
    if (SCORE_LIKE.test(key)) {
      throw new Error(
        `the profile payload carries the field ${path}.${key}, which no profile may render`,
      );
    }
    refuseScoreFields(nested, `${path}.${key}`);
  }
}

/** asObject narrows an unknown to a record, or throws. */
function asObject(value: unknown, what: string): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error(`unexpected ${what} response shape`);
  }
  return value as Record<string, unknown>;
}

/** str reads a required string field. */
function str(source: Record<string, unknown>, key: string): string {
  const value = source[key];
  if (typeof value !== "string") {
    throw new Error(`unexpected profile response shape: ${key} is not a string`);
  }
  return value;
}

/** list reads a required array field and returns it as Records. */
function list(source: Record<string, unknown>, key: string, what: string): Record<string, unknown>[] {
  const value = source[key];
  if (!Array.isArray(value)) {
    throw new Error(`unexpected profile response shape: ${key} is not a list`);
  }
  return value.map((item, i) => asObject(item, `${what}.${key}[${i}]`));
}

/** projectRef reads a nullable project reference. null stays null: the
 *  payload distinguishes "withheld" from "no project" by nothing at all, and
 *  this module keeps it that way. */
function projectRef(value: unknown): ProfileProjectRef | null {
  if (value === null || value === undefined) return null;
  const obj = asObject(value, "project");
  return { id: str(obj, "id"), slug: str(obj, "slug"), name: str(obj, "name"), url: str(obj, "url") };
}

/** namedProjectRef reads a project reference the row is required to carry.
 *  A null here is a payload this module will not render: the API drops a
 *  usage whose project may not be named, so "no project on a reuse row" is a
 *  shape the model cannot produce and the client must not invent a sentence
 *  for (an error line is honest; a placeholder would be a fiction a reader
 *  could take for a real project). */
function namedProjectRef(source: Record<string, unknown>, what: string): ProfileProjectRef {
  const ref = projectRef(source.project);
  if (ref === null) {
    throw new Error(`unexpected profile response shape: ${what} names no project`);
  }
  return ref;
}

/** parsePersonProfile validates the Research Profile payload and refuses a
 *  score-shaped field anywhere in it. */
export function parsePersonProfile(body: unknown): PersonResearchProfile {
  refuseScoreFields(body);
  const root = asObject(body, "research-profile");
  const person = asObject(root.person, "person");
  return {
    person: {
      id: str(person, "id"),
      handle: str(person, "handle"),
      display_name: str(person, "display_name"),
      bio: str(person, "bio"),
      url: str(person, "url"),
    },
    affiliations: list(root, "affiliations", "research-profile").map((a) => {
      const org = a.organization;
      return {
        organization:
          org === null || org === undefined
            ? null
            : {
                id: str(asObject(org, "organization"), "id"),
                slug: str(asObject(org, "organization"), "slug"),
                name: str(asObject(org, "organization"), "name"),
                url: str(asObject(org, "organization"), "url"),
              },
        role: str(a, "role"),
        affiliation_start: nullableString(a.affiliation_start),
        affiliation_end: nullableString(a.affiliation_end),
        verified: a.verified === true,
      };
    }),
    contributions: list(root, "contributions", "research-profile").map(parseContribution),
    assets: list(root, "assets", "research-profile").map(parseAsset),
    reuse: list(root, "reuse", "research-profile").map((r) => ({
      pid: str(r, "pid"),
      title: str(r, "title"),
      version: str(r, "version"),
      url: str(r, "url"),
      asset_type: str(r, "asset_type"),
      dependency_type: str(r, "dependency_type"),
      declared_at: str(r, "declared_at"),
      project: namedProjectRef(r, "reuse"),
    })),
    reproductions: list(root, "reproductions", "research-profile").map((r) => ({
      relation: str(r, "relation"),
      review_state: str(r, "review_state"),
      created_at: str(r, "created_at"),
      project: projectRef(r.project),
    })),
    projects: list(root, "projects", "research-profile").map((p) => ({
      id: str(p, "id"),
      slug: str(p, "slug"),
      name: str(p, "name"),
      url: str(p, "url"),
    })),
  };
}

/** parseOrganizationProfile validates the Organization Profile payload. */
export function parseOrganizationProfile(body: unknown): OrganizationResearchProfile {
  refuseScoreFields(body);
  const root = asObject(body, "organization-profile");
  const org = asObject(root.organization, "organization");
  return {
    organization: {
      id: str(org, "id"),
      slug: str(org, "slug"),
      name: str(org, "name"),
      description: str(org, "description"),
      url: str(org, "url"),
    },
    projects: list(root, "projects", "organization-profile").map((p) => ({
      id: str(p, "id"),
      slug: str(p, "slug"),
      name: str(p, "name"),
      purpose: str(p, "purpose"),
      activity_status: str(p, "activity_status"),
      url: str(p, "url"),
    })),
    activity: list(root, "activity", "organization-profile").map(parseContribution),
    assets: list(root, "assets", "organization-profile").map(parseAsset),
  };
}

function parseContribution(c: Record<string, unknown>): ProfileContribution {
  const roles = c.role_codes;
  if (!Array.isArray(roles)) {
    throw new Error("unexpected profile response shape: role_codes is not a list");
  }
  const out: ProfileContribution = {
    event_type: str(c, "event_type"),
    role_codes: roles.map((r) => String(r)),
    occurred_at: str(c, "occurred_at"),
    accepted_context: c.accepted_context === true,
    released_context: c.released_context === true,
    via: str(c, "via"),
    project: projectRef(c.project),
  };
  if (c.actor !== null && c.actor !== undefined) {
    const actor = asObject(c.actor, "actor");
    out.actor = {
      id: str(actor, "id"),
      handle: str(actor, "handle"),
      display_name: str(actor, "display_name"),
      url: str(actor, "url"),
    };
  }
  return out;
}

function parseAsset(a: Record<string, unknown>): ProfileAsset {
  const out: ProfileAsset = {
    pid: str(a, "pid"),
    title: str(a, "title"),
    asset_type: str(a, "asset_type"),
    version: str(a, "version"),
    url: str(a, "url"),
    project: projectRef(a.project),
    published_at: str(a, "published_at"),
  };
  if (typeof a.role === "string" && a.role !== "") out.role = a.role;
  return out;
}

function nullableString(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

/** Build the reader for one API origin. */
export function createResearchProfileClient(
  apiBaseUrl: string,
  options: ResearchProfileClientOptions = {},
): ResearchProfileClient {
  const fetchFn: FetchLike = options.fetch ?? (fetch as unknown as FetchLike);

  async function call(path: string): Promise<unknown> {
    const res = await fetchFn(apiBaseUrl + path, {
      method: "GET",
      headers: { Accept: "application/json" },
      credentials: "include",
    });
    let parsed: unknown = null;
    try {
      parsed = await res.json();
    } catch {
      parsed = null;
    }
    if (res.status >= 400) {
      if (typeof parsed === "object" && parsed !== null && "code" in parsed) {
        throw new ResearchProfileError(
          res.status,
          parsed as ResearchProfileErrorEnvelope,
        );
      }
      throw new ResearchProfileError(res.status, {
        code: "UNKNOWN",
        message: "the server answered unexpectedly; try again",
      });
    }
    return parsed;
  }

  return {
    async person(userId) {
      return parsePersonProfile(
        await call(`/api/v1/users/${encodeURIComponent(userId)}/research-profile`),
      );
    },
    async organization(slug) {
      return parseOrganizationProfile(
        await call(`/api/v1/organizations/${encodeURIComponent(slug)}/profile`),
      );
    },
  };
}

/** A named identity a row may print: the link and the label for it. */
export interface PrintableEntity {
  /** null when the identity carries no address, so the caller renders a
   *  plain span instead of a link (the shape EntityLink already takes). */
  url: string | null;
  label: string;
}

/**
 * printableEntity decides what a row may print for an identity the payload
 * either names or withholds, and it is the ONE place that decision is made:
 * every profile row renders what this returns and nothing else.
 *
 * null means PRINT NO ENTITY LABEL AT ALL — never a placeholder. A rendered
 * "Organization not named" would tell a reader of an anonymous page that a
 * name exists and is being withheld, and on this payload that inference is
 * exact rather than vague: the model nulls an affiliation's organization only
 * when the employer has been deactivated (the other condition drops the whole
 * row, internal/application/researchprofile/model.go), so the sentence would
 * confirm the deactivation of a named institution. "Say nothing about why" is
 * the rule this function exists to make testable, and rendering nothing is the
 * whole of the answer — the reason is not the reader's to know.
 *
 * The row's OTHER facts are untouched: an affiliation whose organization is
 * withheld still shows its role, both dates and its verification, because the
 * person's own history is kept when an employer goes away (docs/04 §6).
 *
 * An identity with no name is treated as no identity rather than as a blank
 * label, so this can never print an empty string in the place a name belongs.
 */
export function printableEntity(
  entity: { name: string; url: string } | null | undefined,
): PrintableEntity | null {
  if (entity === null || entity === undefined || entity.name === "") return null;
  return { url: entity.url === "" ? null : entity.url, label: entity.name };
}

/**
 * formatAffiliationWindow renders an affiliation's two calendar dates.
 *
 * The API sends plain ISO calendar days (the canonical columns are `date`,
 * never `timestamptz`) and says explicitly that "nothing here is derived from
 * 'now'" — a client renders "since 2024" or "2024 – 2025" itself. This is
 * that rendering, and it is the reason the API does not send a sentence: the
 * server would be deciding what "now" means for a reader in another time
 * zone.
 */
export function formatAffiliationWindow(
  start: string | null,
  end: string | null,
): string {
  if (start === null && end === null) return "dates not recorded";
  if (start === null) return `until ${end}`;
  if (end === null) return `${start} – present`;
  return `${start} – ${end}`;
}

/** formatDay renders an ISO instant as its calendar date. */
export function formatDay(instant: string): string {
  const at = new Date(instant);
  if (Number.isNaN(at.getTime())) return instant;
  return at.toISOString().slice(0, 10);
}

/** The channel vocabulary is the API's (state_commits.via); an unknown value
 *  is shown as it arrived rather than hidden. */
export function describeVia(via: string): string {
  switch (via) {
    case "web":
      return "web";
    case "api":
      return "API";
    case "mcp":
      return "MCP";
    case "claude_code":
      return "agent";
    case "git_compat":
      return "Git";
    case "system":
      return "system";
    default:
      return via;
  }
}

/** describeRelation names the two reproduction relations of docs/10 §4. */
export function describeRelation(relation: string): string {
  switch (relation) {
    case "reproduces":
      return "Reproduced";
    case "fails_to_reproduce":
      return "Failed to reproduce";
    default:
      return relation;
  }
}

/** Human-facing message for a stable code from either profile route. */
export function messageForResearchProfileCode(code: string): string {
  switch (code) {
    case "USER_NOT_FOUND":
      return "This person has no public research profile.";
    case "ORG_NOT_FOUND":
      return "This organization has no public profile.";
    case "SERVICE_UNAVAILABLE":
      return "Research profiles are temporarily unavailable. Please try again later.";
    case "UNEXPECTED":
      return "The profile could not be rendered: it carried a field a profile is not allowed to show.";
    default:
      return "Something went wrong. Please try again.";
  }
}
