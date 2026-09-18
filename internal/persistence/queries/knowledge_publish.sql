-- Knowledge publication governance (T0805): the writes and the ledger
-- reads of the knowledge publish command, plus the reads a replay and the
-- public read route need.
--
-- knowledge_publications (migration 00010) has existed since the schema
-- was created with NO writer: PublishKnowledgePublication sat in
-- releases_assets.sql with zero callers. Everything below is that missing
-- path.
--
-- Every write here happens inside ONE transaction
-- (persistence.KnowledgePublishStore.Publish) together with the
-- publication re-check that authorized it, the audit row and the
-- knowledge.version_published domain event, so a refused publish writes
-- nothing and an accepted one is one unit.
--
-- The review record the decision reads is NOT here either: it is
-- ListReleaseReviews (releases_assets.sql), reused verbatim inside the
-- publish transaction so that "this version passed review" is one SQL
-- definition shared with the release gate.

-- name: GetKnowledgePublicationByObjectVersion :one
-- The version's existing publication, or no row (pgx.ErrNoRows) when it
-- has never been published.
--
-- It is deliberately NOT a UNIQUE lookup in the schema sense: 00010
-- carries UNIQUE(object_version_id, public_version), which permits a
-- second row under a different name. Owner ruling L3-20260916-1 #3
-- forbids a second publication of one version, and that rule is enforced
-- by the application (knowledgepublish.Judge), not by an index — the
-- ruling is what forbids it, and a migration is not where a product rule
-- is changed. This read therefore takes the OLDEST row (a version that
-- somehow carries two rows is answered with the first one) and answers
-- "is it published" without depending on the schema having been
-- tightened.
SELECT * FROM knowledge_publications
WHERE object_version_id = @object_version_id
ORDER BY published_at, id
LIMIT 1;

-- name: GetKnowledgePublicationCreation :one
-- The publish ledger lookup: the publication an Idempotency-Key already
-- wrote, or no row (pgx.ErrNoRows) when the key is new. UNIQUE(project_id,
-- idempotency_key) (migration 00083) makes this at most one row by
-- construction.
SELECT publication_id FROM knowledge_publication_creations
WHERE project_id = @project_id AND idempotency_key = @idempotency_key;

-- name: CreateKnowledgePublicationCreation :one
-- The ledger row, written in the same transaction as the publication it
-- names: a publish that rolled back leaves no key behind, and a key that
-- committed names a publication that exists.
INSERT INTO knowledge_publication_creations (project_id, idempotency_key, publication_id)
VALUES (@project_id, @idempotency_key, @publication_id)
RETURNING *;

-- name: GetKnowledgePublication :one
-- One stored publication by its row id — the read a replay answers with.
SELECT * FROM knowledge_publications
WHERE id = @id;

-- name: ResolvePublicationFactRows :one
-- Everything the publication decision reads about the version being
-- published, in ONE row: the version, the object it belongs to, the
-- project that owns the object, and the main branch of that project.
--
-- It is ONE query because the decision is about one version and must see
-- one consistent state; resolved as four reads it could see a version
-- from before a project moved and a project from after.
--
-- project_id is an INPUT rather than an output: a publish never resolves
-- a version across the project boundary, so a version id that belongs to
-- another project (or to none) matches nothing and is reported exactly as
-- an unknown one (docs/45: no foreign entity existence leaks). The join
-- to scientific_objects is what makes that true for the version's object
-- too.
--
-- The main branch is an OUTER join because a project may have no branch
-- named main (none is created with the project); the decision then has no
-- review record to read, and knowledgepublish.Judge refuses the
-- publication rather than assuming it was reviewed. LEFT JOIN LATERAL
-- with LIMIT 1 keeps this to one row whatever the branches table holds.
SELECT sov.id                AS object_version_id,
       sov.object_id,
       sov.version_no,
       sov.state_id,
       sov.branch_id,
       sov.lifecycle_state,
       sov.title,
       sov.schema_id,
       sov.schema_version,
       sov.integrity_hash,
       sov.visibility_policy_id,
       so.object_type,
       so.project_id,
       p.visibility         AS project_visibility,
       mb.id                AS main_branch_id
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
LEFT JOIN LATERAL (
  SELECT b.id FROM branches b
  WHERE b.project_id = so.project_id AND b.name = @main_branch_name
  ORDER BY b.created_at, b.id
  LIMIT 1
) mb ON true
WHERE sov.id = @object_version_id
  AND so.project_id = @project_id;

-- name: ResolvePublishedKnowledge :one
-- The public read model of GET /knowledge/{knowledgeId} (T0805): the
-- publication the pid names, the version it published, the object that
-- version belongs to, and the project that owns the object — everything
-- the audience rule (knowledgepublish.AudienceFor) decides with.
--
-- The read is BY PID, which is why migration 00083 puts the pid on the
-- publication: the identity a reader addresses is the publication's, and
-- a version that is published once has exactly one.
--
-- The query applies NO visibility predicate. Who may read the result is
-- knowledgepublish.AudienceFor, in Go, over the columns below — a second,
-- SQL-shaped copy of that rule is how two answers to "who may read this"
-- start to disagree, and the one in Go is the one the publish decision
-- and the preview already use. The publication row is fetched whatever
-- the audience turns out to be; the transport answers not-found when the
-- audience refuses.
SELECT kp.id,
       kp.pid,
       kp.object_version_id,
       kp.public_version,
       kp.rights_json,
       kp.published_by,
       kp.published_at,
       sov.object_id,
       sov.title,
       sov.lifecycle_state,
       sov.schema_id,
       sov.schema_version,
       sov.integrity_hash,
       sov.visibility_policy_id,
       so.object_type,
       so.project_id,
       p.visibility AS project_visibility
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE kp.pid = @pid;
