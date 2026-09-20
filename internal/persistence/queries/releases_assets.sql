-- Releases, research assets, publications (canonical tables:
-- validation_results, releases, research_assets, research_asset_versions,
-- knowledge_publications). Release/asset versions are immutable (invariant 5).

-- name: RecordValidationResult :one
INSERT INTO validation_results (project_id, state_id, gate, status, result_json)
VALUES (@project_id, @state_id, @gate, @status, @result_json)
RETURNING *;

-- name: CreateRelease :one
INSERT INTO releases (project_id, version, title, state_id, policy_version_id, org_policy_version_id, manifest, manifest_hash, created_by)
VALUES (@project_id, @version, @title, @state_id, @policy_version_id, @org_policy_version_id, @manifest, @manifest_hash, @created_by)
RETURNING *;

-- name: GetReleaseByProjectAndVersion :one
SELECT * FROM releases
WHERE project_id = @project_id AND version = @version;

-- name: ListReleases :many
SELECT * FROM releases
WHERE project_id = @project_id
ORDER BY created_at DESC, id DESC;

-- name: GetRelease :one
SELECT * FROM releases
WHERE project_id = @project_id AND id = @id;

-- name: GetReleaseCreation :one
SELECT release_id FROM release_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateReleaseCreation :one
INSERT INTO release_creations (project_id, idempotency_key, release_id)
VALUES (@project_id, @idempotency_key, @release_id)
RETURNING *;

-- name: CreateResearchAsset :one
INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
VALUES (@asset_type, @slug, @title, @origin_project_id)
RETURNING *;

-- name: PublishResearchAssetVersion :one
-- The publish command's version insert (T0705). origin_refs is written
-- here because it is NOT NULL with two CHECKs since 00064 (at least one
-- element, no NULL element) and the version's provenance is exactly what
-- that column is: a publish that left it to a default could not store a
-- row at all. This query had no producer before T0705 — the preview
-- (T0704) only reads — so extending it is not a change to a shipped
-- writer; issue #225 recorded the gap when the column was added.
INSERT INTO research_asset_versions
    (asset_id, version, source_release_id, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
VALUES
    (@asset_id, @version, @source_release_id, @manifest, @rights_json, @visibility, @integrity_hash, @published_by, @origin_refs)
RETURNING *;

-- name: GetResearchAssetVersion :one
SELECT * FROM research_asset_versions
WHERE asset_id = @asset_id AND version = @version;

-- name: PublishKnowledgePublication :one
-- The publish command's insert (T0805). This query had no producer before
-- it — knowledge_publications has had no writer since migration 00010 —
-- so extending it with the pid column (migration 00083) is not a change
-- to a shipped writer, the way PublishResearchAssetVersion's origin_refs
-- was not one for T0705.
--
-- The pid is passed in and never left to the column DEFAULT: migration
-- 00064 records the same decision for assets ("the application-generated
-- path ... arrives with T0705 (publish)"), and a persistent identifier
-- that two writers derive differently is not a persistent identity. The
-- DEFAULT stays for rows written before the publish command could
-- generate one.
INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by, pid)
VALUES (@object_version_id, @public_version, @rights_json, @published_by, @pid)
RETURNING *;

-- name: ListReleaseReviews :many
-- The review/approval record of one release (T0605, extended by T0611):
-- every review row of the research PRs targeting main that either
--
--   1. proposed a state which is the released state or one of its
--      ancestors (the T0605 edge), or
--   2. were merged into the lineage — the merge row that accepted the
--      proposal recorded a result state inside that lineage
--      (semantic_merges.result_state_id, the T0611 edge).
--
-- Edge 2 is what makes the states that entered main the ONE way docs/09
-- §3 allows readable at all. A merge commits a NEW state whose parent is
-- the target head it ran on — not the proposal (migration 00069 pins the
-- triple; internal/application/merge commits BaseStateID = target head) —
-- so the proposal state is never an ancestor of the accepted state, and
-- edge 1 alone answers EMPTY for exactly the lineage that got there by
-- being merged. Both edges stay: a proposal inside the lineage is part of
-- the record whether or not it was ever merged, and dropping edge 1 would
-- lose those states' reviews.
--
-- The two edges are ONE predicate over ONE select, so a PR that matches
-- both (its proposed state is an ancestor AND its merge result is in the
-- lineage) is still one group of rows, each review row counted once: the
-- union is of PRs, and a union spelled as two selects would emit that
-- PR's reviews twice.
--
-- No target filter is repeated on edge 2. It does not need one: the merge
-- row's target branch is the branch its result state was committed on
-- (00069 says so in as many words), so a result state inside the lineage
-- — which is main's own chain by construction — is a merge ONTO main. The
-- target filter outside names main explicitly for edge 1, where it is
-- needed: a duplicate proposal of the same state against another branch is
-- not part of main's acceptance record.
--
-- Ordered by PR number then review time then row id (a total order — the
-- release manifest's canonical sorting is the releases package's rule,
-- not the store's).
WITH RECURSIVE lineage(id) AS (
  SELECT project_states.id FROM project_states WHERE project_states.id = @state_id
  UNION
  SELECT ps.parent_state_id FROM project_states ps
  JOIN lineage l ON ps.id = l.id
  WHERE ps.parent_state_id IS NOT NULL
)
SELECT pr.number AS pull_request_number,
       pr.proposed_state_id,
       r.id,
       r.reviewer_id,
       r.review_kind,
       r.decision,
       r.body,
       r.created_at
FROM reviews r
JOIN pull_requests pr ON pr.id = r.pull_request_id
WHERE pr.target_branch_id = @main_branch_id
  AND (pr.proposed_state_id IN (SELECT id FROM lineage)
       OR EXISTS (
         SELECT 1 FROM semantic_merges sm
         WHERE sm.pull_request_id = pr.id
           AND sm.result_state_id IN (SELECT id FROM lineage)
       ))
ORDER BY pr.number, r.created_at, r.id;
