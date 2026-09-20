-- Evidence assertions (canonical table: evidence_assertions). Evidence is a
-- directed, typed relation between object versions — separate from the RSG
-- relation graph (invariant 10: Provenance Graph != Evidence Graph).
--
-- T0806 adds the network-evidence read and the two columns 00091 adds:
-- evidence_origin (docs/10 §3's external/internal) and visibility.
--
-- # Who decides what, here
--
-- The three read classes (Origin / Reviewed External / Unreviewed External,
-- docs/10 §7) are NOT spelled in SQL. ListPublishedEvidenceForTarget returns
-- the two FACTS the classification is computed from — the asserting
-- project's id (compared by the caller against the published version's
-- owning project) and the assertion's review_state — and
-- internal/domain.ClassifyEvidenceNetwork decides.
--
-- The predicates below ARE spelled here, and they are the read STRATEGY that
-- keeps the fail-closed rule cheap; the caller re-checks each row before
-- rendering it (the same split feeds.sql's header describes, and the same
-- reason: a predicate is never a substitute for the rule). Two things are
-- filtered:
--
--   ea.visibility = 'public'          the assertion's own visibility axis:
--                                     an assertion nothing made public is
--                                     not rendered on the public read, and
--                                     'private' is the column DEFAULT.
--   (origin OR asserting project public)
--                                     the asserting project's own visibility.
--                                     A public assertion can only be written
--                                     by a public project through the write
--                                     path; this half is the structural
--                                     refusal for a row written any OTHER
--                                     way (a private project's work is not
--                                     published by asserting it somewhere
--                                     else, docs/12 §2 发布不等于公开), and
--                                     the origin half is why an assertion in
--                                     the published object's OWN project is
--                                     judged by the publication read gate
--                                     the caller already passed rather than
--                                     by the project's preset again.

-- name: CreateEvidenceAssertion :one
-- The assertion row, written inside a state commit (states.WriteFunc): the
-- state_id the commit assigns is the transition the assertion was created
-- in (docs/10 §3's "created transition"), so the assertion is traceable to
-- the commit that carries it, like every other member row.
--
-- The id is supplied by the caller (the column's default is for writers
-- that have no commit to name the row in): the commit's operation summary
-- carries the same id, which is what makes the assertion findable from the
-- transition and the transition findable from the assertion.
--
-- evidence_origin and visibility are SERVER-DERIVED inputs of this query,
-- never client input: the caller resolves the project comparison and the
-- visibility axes before it gets here (see 00091's header).
INSERT INTO evidence_assertions
    (id, project_id, state_id, target_object_version_id, evidence_object_version_id,
     relation_type, evidence_type, scope, directness, inference_nature,
     reasoning_note, created_by, evidence_origin, visibility)
VALUES
    (@id, @project_id, @state_id, @target_object_version_id, @evidence_object_version_id,
     @relation_type, @evidence_type, @scope, @directness, @inference_nature,
     @reasoning_note, @created_by, @evidence_origin, @visibility)
RETURNING *;

-- name: ListEvidenceAssertionsForTarget :many
-- Every assertion against one target version that the given reader may be
-- RENDERED, oldest first. The reader is an explicit input of the read
-- (ADR-024), and the row it may see is decided here rather than after the
-- rows are fetched (a predicate — see the file header for why).
--
-- A row is rendered to a reader when any ONE of the three holds:
--
--   ea.visibility = 'public'   the assertion's own axis. An assertion nothing
--                              explicitly made public is not rendered anywhere
--                              (00091's header, docs/12 §5).
--   the reader is a member of the ASSERTING project (ea.project_id)
--   the reader is a member of the TARGET's own project (so.project_id)
--
-- Both membership clauses are the same criterion the project store's
-- GetMembership answers (a project_memberships row for (project, user)),
-- expressed here so the filter is the read's rather than its caller's. They
-- are the union the ADR names, and they are deliberately NOT folded into a
-- single `visibility = 'public'` predicate: members must still see what they
-- see today (an anonymous-only predicate would take the private row away from
-- both parties, and the target project's maintainers must keep seeing external
-- evidence about their own object — docs/24 §2, "external evidence 不可被
-- origin maintainer 静默删除").
--
-- Fail closed: a reader that resolves to no user id arrives as SQL NULL, and
-- every membership clause is then NULL rather than true, so an unresolvable
-- reader gets exactly the public rows. Same direction as the column's own
-- DEFAULT 'private'.
SELECT ea.*
FROM evidence_assertions ea
JOIN scientific_object_versions sov ON sov.id = ea.target_object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
WHERE ea.target_object_version_id = @object_version_id
  AND (
      ea.visibility = 'public'
      OR EXISTS (SELECT 1 FROM project_memberships am
                 WHERE am.project_id = ea.project_id AND am.user_id = @reader_user_id)
      OR EXISTS (SELECT 1 FROM project_memberships tm
                 WHERE tm.project_id = so.project_id AND tm.user_id = @reader_user_id)
  )
ORDER BY ea.created_at, ea.id;

-- name: ListPublishedEvidenceForTarget :many
-- The evidence section of GET /knowledge/{knowledgeId}: the assertions
-- against one PUBLISHED object version that the network read may render.
--
-- The asserting project travels with each row because the caller needs BOTH
-- ends of the origin/external axis to classify the row (see the file
-- header); source_project_visibility is the strategy half of the predicate
-- above and is re-checked by the caller.
--
-- Newest first, bounded by @row_limit: the evidence on a popular published
-- object is unbounded, and an unbounded read is a resource a writer can
-- exhaust. The bound cuts the OLDEST rows — a reader of network evidence is
-- looking for what the network has to say, and silently dropping the newest
-- counter-evidence would be the one truncation this feature must not make
-- (docs/24: external evidence 不可被 origin maintainer 静默删除). The
-- caller fetches one row beyond the limit and reports whether the section
-- was truncated rather than presenting a short list as complete.
--
-- The order is total ((created_at, id) DESC), so one database state renders
-- one document.
SELECT ea.id::text AS id,
       ea.project_id::text AS project_id,
       ea.target_object_version_id::text AS target_object_version_id,
       ea.evidence_object_version_id::text AS evidence_object_version_id,
       ea.relation_type,
       ea.evidence_type,
       ea.scope,
       ea.directness,
       ea.inference_nature,
       COALESCE(ea.reasoning_note, '')::text AS reasoning_note,
       ea.review_state,
       ea.created_at,
       sp.visibility::text AS source_project_visibility
FROM evidence_assertions ea
JOIN projects sp ON sp.id = ea.project_id
WHERE ea.target_object_version_id = @object_version_id::uuid
  AND ea.visibility = 'public'
  AND (ea.project_id = @target_project_id::uuid OR sp.visibility = 'public')
ORDER BY ea.created_at DESC, ea.id DESC
LIMIT @row_limit;

-- name: GetVersionProjectFacts :one
-- One version's owning project, resolved through the object it belongs to
-- (scientific_object_versions -> scientific_objects -> projects), with the
-- version's own visibility axis.
--
-- Both ends of an assertion are resolved with this query, because both
-- answers come from the same chain: the TARGET end decides origin vs
-- external (is this project the one the published version belongs to?), and
-- the EVIDENCE end answers the same question about the cited version.
SELECT sov.id::text AS object_version_id,
       sov.object_id::text AS object_id,
       sov.visibility_policy_id,
       so.object_type,
       so.project_id::text AS project_id,
       p.visibility::text AS project_visibility
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE sov.id = @object_version_id::uuid;

-- name: GetKnowledgePublicationForVersion :one
-- The publication a version carries, with the inputs the audience rule
-- decides with (knowledgepublish.AudienceFor): the project's preset, the
-- version's own visibility axis, and the rights document read RAW — the
-- rights token is a Go parse, never a JSON predicate (feeds.sql's header
-- makes the same argument). The row is fetched whatever the audience turns
-- out to be; the caller refuses rather than the query filtering, so there
-- is one definition of who may read a publication.
SELECT kp.id::text AS id,
       kp.pid,
       kp.public_version,
       kp.rights_json,
       kp.object_version_id::text AS object_version_id,
       sov.visibility_policy_id,
       so.project_id::text AS project_id,
       p.visibility::text AS project_visibility
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE kp.object_version_id = @object_version_id::uuid
ORDER BY kp.published_at, kp.id
LIMIT 1;
