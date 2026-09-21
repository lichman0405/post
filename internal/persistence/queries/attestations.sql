-- Attestations (T0812, migration 00120): the public statement a project
-- makes about a public object or asset version it did not author, plus the
-- private side it cites and never discloses.
--
-- # Two families of query, and the wall between them
--
-- The RESOLUTION queries (ResolveAttestationTargetObject,
-- ResolveAttestationTargetAsset, ResolveAttestationAttester,
-- ResolveAttestationInternalReview) are the publish command's reads: they
-- carry the private side because the decision is made on it.
--
-- The PUBLIC READ (ResolvePublicAttestation) is a different query, and it
-- deliberately has no column for the private side at all: attesting_project_id,
-- basis_state_id and internal_review_id are NOT selected. The projection in
-- internal/application/attestations cannot leak what it was never handed —
-- which is a stronger guarantee than a projection that drops fields it
-- holds, and it is the one this surface takes.
--
-- # The audience rule is not in SQL (the public read), and IS in SQL (the target read)
--
-- There is no visibility predicate on the public read, and that is the same
-- decision internal/application/knowledgepublish records for
-- ResolvePublishedKnowledge: the rule that decides who may see what is Go
-- (attestations.Present), and a second, SQL-shaped copy of it is how two
-- answers to "may this be shown" start to disagree. Unlike a publication,
-- however, an attestation has NO audience gate to re-check — what this read
-- returns is already the public half by construction (see above), so the
-- predicate would have nothing to filter.
--
-- The TARGET reads are the opposite case, and the two are not in tension.
-- They are not projections of the attesting side at all: they resolve
-- SOMEBODY ELSE'S row by an id the caller supplied, and everything they
-- return (the title, the object id, the version id, the owning project's
-- visibility) is that other party's private data when the row is private.
-- So the question "may this reader read this row" has to be answered
-- BEFORE any of it is handed back, and it is answered here, in the read —
-- reader-relative, the same way events_audit.sql and evidence.sql answer it
-- for their own rows (ADR-024: the read carries the reader). A caller that
-- may not read the row gets no row at all, which is why this read cannot be
-- an existence oracle: "not yours" and "does not exist" are one answer
-- because they are one code path.

-- name: ResolveAttestationTargetObject :one
-- The scientific object version a Protocol/Claim attestation would name:
-- what it is, what type it is, and the two facts that decide whether it is
-- PUBLIC (the owning project's preset, and the version's OWN visibility
-- axis — scientific_object_versions.visibility_policy_id, migration 00005).
--
-- No project_id filter: this read resolves the TARGET, which by definition
-- belongs to another project (the attester attests somebody else's public
-- work). The caller passes the id it already holds.
--
-- The reader predicate (@reader_user_id) is the pair of axes the product
-- reads such a version by, and it is the SAME pair attestations.Facts.
-- TargetIsPublic decides on — the target's own public rule, or a
-- project_memberships row for the project that owns it (the criterion
-- projects.ProjectStore.GetMembership answers, expressed here so the filter
-- is the read's rather than its caller's). A version the reader may not read
-- is not returned at all, so the resolution answers the same "no such
-- version" it answers for an id that names nothing.
--
-- It is deliberately not "a target that could be attested": whether a
-- READABLE target may be attested is Judge's decision in Go, and this
-- predicate must not become a second, SQL-shaped copy of it. This answers
-- "may this reader read this row", and nothing else.
--
-- Fail closed: a reader that resolves to no user id arrives as SQL NULL, the
-- membership clause is then NULL rather than true, and the row comes back
-- only when it is genuinely public — the direction events_audit.sql:145 and
-- evidence.sql:101 record for their own reader predicates.
SELECT sov.id AS version_id,
       sov.object_id,
       sov.version_no,
       sov.title,
       sov.lifecycle_state,
       sov.visibility_policy_id,
       so.object_type,
       so.project_id AS owning_project_id,
       p.visibility AS owning_project_visibility
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE sov.id = @object_version_id
  AND (
      (sov.visibility_policy_id IS NULL AND p.visibility = 'public')
      OR EXISTS (SELECT 1 FROM project_memberships pm
                 WHERE pm.project_id = so.project_id
                   AND pm.user_id = @reader_user_id)
  );

-- name: ResolveAttestationTargetAsset :one
-- The research asset version an Asset attestation would name, and the TWO
-- facts that decide whether it is public: research_asset_versions.visibility
-- (the asset's own axis, written by the publish command) AND
-- projects.visibility of the project that ORIGINATED it
-- (research_assets.origin_project_id).
--
-- The second axis is not decoration. An asset version can be flagged
-- 'public' inside a project that is not, and the platform decides an
-- asset's reachability by its origin project in three other places already:
-- the asset page's read gate (cmd/api/assetshttp/page.go — the project read
-- through projects.ProjectStore.Get, 404 for a non-member), the asset
-- feed's existence (GetFeedAsset below, and the comment there names the
-- same rule), and the subscription audience resolution (T1002). A predicate
-- that consulted only the version's own axis would read a version as
-- network-visible that the page, the feed and the subscription all refuse
-- to show.
--
-- The reader predicate is the same two arms as the object read above, with
-- this kind's own public rule in the first one: the asset version is public
-- on BOTH axes, or the reader holds a project_memberships row for the
-- project the asset belongs to (research_assets.origin_project_id — the
-- project whose read gate decides who may open the asset's page,
-- asset_page.sql:42). The membership arm is a conjunction with the version
-- being readable at all; it is not widened by the project axis, because a
-- member of the origin project may read the version whatever either axis
-- says.
--
-- The origin project's visibility comes back with the row for the same
-- reason the object read returns the owning project's: attestations.Facts.
-- TargetIsPublic decides on it in Go, and a decision that is made in Go
-- must be made over a fact the read handed it rather than over a second
-- query (the arrangement ResolveAttestationTargetObject records).
SELECT rav.id AS version_id,
       rav.asset_id,
       rav.version,
       rav.visibility,
       rav.integrity_hash,
       ra.title,
       ra.asset_type,
       p.visibility AS owning_project_visibility
FROM research_asset_versions rav
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p ON p.id = ra.origin_project_id
WHERE rav.id = @asset_version_id
  AND (
      (rav.visibility = 'public' AND p.visibility = 'public')
      OR EXISTS (SELECT 1 FROM project_memberships pm
                 WHERE pm.project_id = ra.origin_project_id
                   AND pm.user_id = @reader_user_id)
  );

-- name: ResolveAttestationAttester :one
-- The attesting project, and the standing answer of its organization to
-- "may we be named" (organizations.attestation_attribution, migration 00120).
--
-- organization_attestation_attribution is NULL for a personal project (no
-- organization) and non-NULL otherwise: the LEFT JOIN is the difference, and
-- the command refuses the NULL branch for a project that has an organization
-- (the trigger in 00120 makes the same rule unconditional).
SELECT p.id AS project_id,
       p.visibility AS project_visibility,
       p.organization_id,
       o.attestation_attribution AS organization_attestation_attribution
FROM projects p
LEFT JOIN organizations o ON o.id = p.organization_id
WHERE p.id = @project_id;

-- name: ResolveAttestationInternalReview :one
-- The internal review an attestation cites: the review, the pull request it
-- was recorded on, and the state it judged.
--
-- The row carries the PR's project and the reviewed state's project because
-- the command checks them (the review must belong to the attesting project,
-- and must have judged the attestation's own basis state) — the same two
-- facts the trigger in 00120 makes unconditional for any write path. The
-- check here exists so the refusal is a named reason rather than a
-- constraint violation; the constraint is what makes it true.
SELECT r.id AS review_id,
       r.review_kind,
       r.decision,
       r.reviewed_state_id,
       pr.id AS pull_request_id,
       pr.project_id AS pull_request_project_id,
       pr.state AS pull_request_state,
       ps.project_id AS reviewed_state_project_id
FROM reviews r
JOIN pull_requests pr ON pr.id = r.pull_request_id
JOIN project_states ps ON ps.id = r.reviewed_state_id
WHERE r.id = @review_id;

-- name: ResolveAttestationBasisState :one
-- The private state the attestation would rest on. Read so the command can
-- name the project it belongs to rather than let the trigger answer with a
-- constraint violation.
SELECT ps.id AS state_id,
       ps.project_id,
       ps.state_hash,
       ps.created_at
FROM project_states ps
WHERE ps.id = @state_id;

-- name: CreateAttestation :one
-- The publish command's insert. One row, no ledger: an attestation has no
-- idempotency key because a repeat is a LEGITIMATE second statement (a
-- re-validation after a first inconclusive result), not a replay — the
-- migration records that decision. Nothing sums these rows, so a duplicate
-- buys the caller nothing either.
--
-- The pid is passed in and never left to the column DEFAULT: a persistent
-- identifier that two writers derive differently is not a persistent
-- identity (00064 records the same decision for assets, 00083 for
-- knowledge publications).
INSERT INTO attestations (
    pid,
    target_object_version_id,
    target_asset_version_id,
    attesting_project_id,
    attesting_organization_id,
    basis_state_id,
    internal_review_id,
    validation_type,
    validation_result,
    org_visibility,
    created_by
) VALUES (
    @pid,
    @target_object_version_id,
    @target_asset_version_id,
    @attesting_project_id,
    @attesting_organization_id,
    @basis_state_id,
    @internal_review_id,
    @validation_type,
    @validation_result,
    @org_visibility,
    @created_by
)
RETURNING *;

-- name: ResolvePublicAttestation :one
-- The public read, by pid (GET /api/v1/attestations/{attestationId}).
--
-- It selects the PUBLIC HALF of the row (pid, validation_type,
-- validation_result, org_visibility, created_at, the target pin) and the
-- facts the attribution rule needs to apply: the organization the attestation
-- recorded, and that organization's CURRENT standing setting. Nothing else.
--
-- The private side is not merely unrendered here, it is unread:
-- attesting_project_id, basis_state_id and internal_review_id have no column
-- in this result. A projection cannot disclose what the query never handed
-- it, and the privacy e2e asserts exactly that about the wire body.
SELECT a.pid,
       a.validation_type,
       a.validation_result,
       a.org_visibility,
       a.created_at,
       a.target_object_version_id,
       a.target_asset_version_id,
       sov.object_id,
       sov.version_no AS object_version_no,
       sov.title AS object_version_title,
       sov.lifecycle_state AS object_version_lifecycle_state,
       so.object_type,
       rav.asset_id,
       rav.version AS asset_version_label,
       rav.visibility AS asset_version_visibility,
       ra.title AS asset_title,
       ra.asset_type,
       a.attesting_organization_id,
       o.slug AS organization_slug,
       o.name AS organization_name,
       o.attestation_attribution AS organization_attestation_attribution
FROM attestations a
LEFT JOIN scientific_object_versions sov ON sov.id = a.target_object_version_id
LEFT JOIN scientific_objects so ON so.id = sov.object_id
LEFT JOIN research_asset_versions rav ON rav.id = a.target_asset_version_id
LEFT JOIN research_assets ra ON ra.id = rav.asset_id
LEFT JOIN organizations o ON o.id = a.attesting_organization_id
WHERE a.pid = @pid;
