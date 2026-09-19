-- +goose Up
-- External evidence on the network (task T0806, docs/10 §7):
--
--   "Published Knowledge Object 页面区分 Origin Evidence、Reviewed
--    External Evidence、Unreviewed External Evidence。原发布者不能删除
--    外部反证；只能对其本项目 assessment 负责。"
--
-- The three classes are a READ-SIDE classification and they are NOT stored
-- here. They are derived from two axes that already exist:
--
--   1. origin vs external — whether the project that owns the assertion is
--      the project that owns the published version it targets. Both ends
--      are readable (evidence_assertions.project_id, and the publication's
--      object_version_id -> scientific_object_versions.object_id ->
--      scientific_objects.project_id chain, 00005/00010), so the axis is
--      computed at read time by internal/domain.ClassifyEvidenceNetwork.
--      A stored three-valued column would be a third thing to keep in sync
--      and would drift from review_state the moment either moved.
--   2. reviewed vs unreviewed — evidence_assertions.review_state, which
--      00007 has carried since the table was created.
--
-- What this migration adds is the rest of the evidence-assertion schema
-- docs/10 §3 names ("... external/internal、visibility、created transition")
-- and the one thing the table is missing at the storage layer: the fact
-- that an assertion is never deleted.
--
-- 1. evidence_origin (docs/10 §3's "external/internal").
--
-- Values: 'internal' means the asserting project IS the project that owns
-- the published version the assertion targets — the endpoint's Origin
-- Evidence side. 'external' means another project asserted it (Reviewed /
-- Unreviewed External Evidence, decided afterwards by review_state).
--
-- The column is the assertion's own PROVENANCE RECORD, written once by the
-- write path from the same comparison the read recomputes; it is never
-- client input, and the read side deliberately does not consult it (a
-- declaration that could be edited must not be able to move a row between
-- buckets). DEFAULT 'internal' is the neutral member: a row written by
-- anything that does not go through the application records the origin
-- side, and the read still decides on the two facts it reads itself.
--
-- 2. visibility (docs/10 §3).
--
-- Values are the platform's visibility vocabulary ('public','private',
-- exactly as projects/branches declare it). It is the assertion's own
-- visibility axis: whether the assertion may be rendered by the PUBLIC
-- network read (GET /knowledge/{knowledgeId}, which is `security: []`).
-- DEFAULT 'private' is the fail-closed direction — an assertion nothing
-- explicitly made public is not rendered anywhere (docs/12 §5: missing
-- information is never a reason to widen).
--
-- The write path derives it from the axes that already exist rather than
-- from the request: public only when the asserting project AND the branch
-- the assertion was committed to are both public. There is no
-- client-supplied visibility in the request shape (specs/mcp/tools.json,
-- evidence.create_assertion), so there is nothing to trust and nothing to
-- override.

ALTER TABLE evidence_assertions
  ADD COLUMN evidence_origin text NOT NULL DEFAULT 'internal'
    CONSTRAINT evidence_assertions_evidence_origin_check
    CHECK (evidence_origin IN ('internal', 'external')),
  ADD COLUMN visibility text NOT NULL DEFAULT 'private'
    CONSTRAINT evidence_assertions_visibility_check
    CHECK (visibility IN ('public', 'private'));

-- 3. DELETE is refused; UPDATE is not.
--
-- 00058 deliberately did NOT take the append-only pair on this table, and
-- its header says why: reviewing an assertion is a legitimate in-place
-- state change (unreviewed → reviewed → rejected), and the 00014 pair
-- guards the version LOGS (scientific_object_versions and friends), not
-- version-less member rows. That reasoning still holds, and this migration
-- does not undo it: the 00014 row guard (BEFORE UPDATE OR DELETE) is
-- deliberately NOT taken, so the review transition stays legal and the
-- UPDATE half of the pair is never claimed.
--
-- What is taken is the DELETE half and the 00015 TRUNCATE half, because
-- the requirement being met is not about reviews: 原发布者不能删除外部反证.
-- A raw `DELETE FROM evidence_assertions` was legal before this
-- migration — there is no guard on the table at all — so any holder of a
-- connection could erase another project's counter-evidence without
-- leaving a trace, which is exactly the act the domain forbids. The guard
-- is the 00014/00015 discipline reused verbatim (append_only_guard(), the
-- same function and the same SQLSTATE P0001 the other guarded tables
-- raise, so an operator reads one message shape everywhere).
--
-- The guard is actor-agnostic because the database knows no actor: it
-- refuses the DELETE of an assertion row, whoever asks. It is therefore
-- strictly stronger than "the origin maintainer may not delete external
-- evidence" — nobody deletes an assertion, which is CLAUDE.md §9.8
-- ("Nothing disappears; state only evolves") read at the storage layer,
-- and which no write path in this build contradicts: the product has no
-- delete route and no Delete query (the application half of the same
-- requirement), and a rejected assertion leaves the picture by its
-- review_state, in place, the way 00058 already describes.
--
-- A guard that instead tried to compute "is this row external?" from the
-- project comparison would be bypassable by exactly the actor it is aimed
-- at: raw SQL could UPDATE evidence_assertions.project_id to the deleter's
-- own project first (the column is an ordinary FK column) and then delete.
-- Refusing the DELETE outright has no such precursor.

CREATE TRIGGER evidence_assertions_delete_guard
  BEFORE DELETE ON evidence_assertions
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER evidence_assertions_no_truncate
  BEFORE TRUNCATE ON evidence_assertions FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
