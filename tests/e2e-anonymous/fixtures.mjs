/**
 * The two worlds the anonymous-pages e2e (T0801 required test "anonymous
 * e2e") distinguishes:
 *
 *   - ONE public project, its release, its published asset and one public
 *     profile — the entities an anonymous reader (a crawler, first of all)
 *     must be able to read, and which must therefore be indexable;
 *   - ONE private project (with an asset of its own) whose name, slug,
 *     purpose and id must not be reachable from outside, and which must
 *     therefore never be indexable.
 *
 * Both mock-api.mjs (which serves them) and anonymous-e2e.mjs (which
 * searches the served pages for them) import this file, so a canary is
 * spelled once: renaming the private project cannot leave the checklist
 * searching for a string the mock no longer serves.
 *
 * The unknown-id fixtures are the other half of the existence-hiding rule
 * (docs/45): an id that names nothing must produce exactly the same answer
 * as the private entity. Every "unknown" id below is a valid UUID that no
 * fixture defines.
 */

/* ---------- the public world ---------- */

export const PUBLIC_PROJECT = {
  id: "11111111-1111-4111-8111-111111111111",
  organization_id: null,
  program_id: null,
  slug: "open-materials",
  name: "Open Materials Lab",
  purpose: "Screen metal-organic frameworks for post-combustion CO2 capture.",
  activity_status: "active",
  visibility: "public",
  main_frozen: true,
  git_repository_external_id: "post/open-materials",
  provision_status: "provisioned",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-08-01T09:00:00Z",
};

export const RELEASE = {
  id: "33333333-3333-4333-8333-333333333333",
  project_id: PUBLIC_PROJECT.id,
  version: "1.0",
  title: "Screening results",
  state_id: "44444444-4444-4444-8444-444444444444",
  policy_version_id: null,
  org_policy_version_id: null,
  manifest_hash: "sha256:5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f5f",
  created_by: "00000000-0000-4000-8000-000000000001",
  created_at: "2026-09-01T10:00:00Z",
};

export const ASSET_PID = "01j9z6k3m4n5p6q7r8s9t0v1w01";
export const ASSET_TITLE = "MOF-5 Synthesis Collection";
export const ASSET_SLUG = "mof5-synthesis";

export const ALICE = {
  user_id: "00000000-0000-4000-8000-000000000001",
  handle: "alice",
  display_name: "Alice Guo",
};

export const PROFILE = {
  id: ALICE.user_id,
  handle: ALICE.handle,
  display_name: ALICE.display_name,
  bio: "MOF chemist. Works on CO2 capture materials.",
  created_at: "2026-07-01T09:00:00Z",
};

/* ---------- the private world (the leak canaries) ---------- */

export const PRIVATE_PROJECT = {
  id: "99999999-9999-4999-8999-999999999999",
  slug: "confidential-catalysis-lab",
  name: "Confidential Catalysis Lab",
  purpose: "Unpublished ligand screening for an industrial partner.",
};

export const PRIVATE_ASSET_PID = "01j9z6k3m4n5p6q7r8s9t0v1w02";
export const PRIVATE_RELEASE_ID = "88888888-8888-4888-8888-888888888888";

/** Every word of the private world: the strings no outside reader may be served. */
export const PRIVATE_WORDS = [
  PRIVATE_PROJECT.name,
  PRIVATE_PROJECT.slug,
  PRIVATE_PROJECT.purpose,
];

/* ---------- the ids that name nothing ---------- */

export const UNKNOWN_PROJECT_ID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa";
export const UNKNOWN_ASSET_PID = "01j9z6k3m4n5p6q7r8s9t0v1w99";
export const UNKNOWN_PROFILE_ID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb";
export const UNKNOWN_RELEASE_ID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc";

/* ---------- what a page must say about each public entity ---------- */

/** The unindexable <head> every non-public page carries (lib/entity-meta.ts). */
export const HIDDEN_TITLE = "Not found — POST";
export const HIDDEN_DESCRIPTION = "This page is not available on POST.";

/** What a page that COULD be described says about a public entity. */
export const EXPECTED = {
  project: {
    title: `${PUBLIC_PROJECT.name} — POST`,
    description: PUBLIC_PROJECT.purpose,
    path: `/projects/${PUBLIC_PROJECT.id}`,
  },
  release: {
    title: `${RELEASE.title} — POST`,
    description: `Release ${RELEASE.version} of ${PUBLIC_PROJECT.name} — an immutable research snapshot.`,
    path: `/projects/${PUBLIC_PROJECT.id}/releases/${RELEASE.id}`,
  },
  asset: {
    title: `${ASSET_TITLE} — POST`,
    description: `Research asset published by ${PUBLIC_PROJECT.name} on POST.`,
    path: `/assets/${ASSET_PID}`,
  },
  assetVersion: {
    title: `${ASSET_TITLE} — POST`,
    description: `Version 2.0 of a research asset published by ${PUBLIC_PROJECT.name} on POST.`,
    path: `/assets/${ASSET_PID}/2.0`,
  },
  profile: {
    title: `${PROFILE.display_name} (@${PROFILE.handle}) — POST`,
    description: PROFILE.bio,
    path: `/users/${PROFILE.id}`,
  },
};
