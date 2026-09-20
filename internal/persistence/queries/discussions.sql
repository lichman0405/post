-- Discussion threads, comments and promotions (T0811; canonical tables
-- discussion_threads, discussion_comments, discussion_promotions,
-- migration 00104).
--
-- Every query here touches those three tables and nothing else: no
-- statement in this file names scientific_object_versions, relation_versions
-- or contribution_events, which is the SQL half of "a comment does not
-- change scientific state" (the Go half is that the discussions command
-- has no state-commit port at all).

-- name: CreateDiscussionThread :one
INSERT INTO discussion_threads (project_id, target_type, target_id, created_by)
VALUES (@project_id, @target_type, @target_id, @created_by)
RETURNING *;

-- name: GetDiscussionThread :one
-- One thread of one project. project_id is in the predicate, not a filter
-- applied afterwards: a thread id of another project is not-found, never a
-- foreign thread (existence hiding, docs/45).
SELECT * FROM discussion_threads
WHERE project_id = @project_id AND id = @id;

-- name: ListDiscussionThreads :many
-- The threads of one target, oldest first (created_at, id — a total
-- order). comment_count counts the comments that still stand (a deleted
-- comment is retained but no longer part of the conversation, which is
-- what the list renders), and last_comment_at is the newest one's time —
-- NULL for a thread whose opening comment was deleted.
SELECT t.*,
       (SELECT count(*) FROM discussion_comments c
         WHERE c.thread_id = t.id AND c.deleted_at IS NULL)::bigint AS comment_count,
       (SELECT max(c.created_at) FROM discussion_comments c
         WHERE c.thread_id = t.id AND c.deleted_at IS NULL)::timestamptz AS last_comment_at
FROM discussion_threads t
WHERE t.project_id = @project_id
  AND t.target_type = @target_type
  AND t.target_id = @target_id
ORDER BY t.created_at, t.id;

-- name: CreateDiscussionComment :one
INSERT INTO discussion_comments (thread_id, project_id, body, created_by)
VALUES (@thread_id, @project_id, @body, @created_by)
RETURNING *;

-- name: GetDiscussionComment :one
-- One comment of one project (same boundary rule as GetDiscussionThread).
SELECT * FROM discussion_comments
WHERE project_id = @project_id AND id = @id;

-- name: ListDiscussionComments :many
-- One thread's comments in creation order. Deleted comments are RETURNED:
-- the row is never removed (CLAUDE.md §9.8) and the transport renders the
-- tombstone; only the body stops being served.
SELECT * FROM discussion_comments
WHERE thread_id = @thread_id
ORDER BY created_at, id;

-- name: SoftDeleteDiscussionComment :one
-- The delete surface's only write: a tombstone (when, by whom), never a
-- DELETE. The `deleted_at IS NULL` predicate is the compare-and-swap — a
-- second delete of the same comment matches nothing and is reported as
-- not-found rather than overwriting the first tombstone's author.
UPDATE discussion_comments
SET deleted_at = now(), deleted_by = @deleted_by
WHERE project_id = @project_id AND id = @id AND deleted_at IS NULL
RETURNING *;

-- name: CreateDiscussionPromotion :one
-- One promotion record: the comment that was proposed, what it became,
-- who promoted it. promoted_ref is "<kind>:<uuid>" — the CHECK in 00104
-- derives the prefix from promoted_kind, so the pair cannot disagree.
INSERT INTO discussion_promotions
    (project_id, thread_id, comment_id, promoted_kind, promoted_ref, promoted_by)
VALUES
    (@project_id, @thread_id, @comment_id, @promoted_kind, @promoted_ref, @promoted_by)
RETURNING *;

-- name: GetDiscussionPromotion :one
SELECT * FROM discussion_promotions
WHERE project_id = @project_id AND id = @id;

-- name: ListDiscussionPromotionsByRef :many
-- The reverse read: every promotion that produced the object a ref names,
-- oldest first. The promoted object does not carry its origin — a
-- scientific object, an issue row and an evidence assertion all have their
-- own tables — so this is the query that answers "where did this come
-- from", joined by the caller to discussion_comments (the author) and
-- discussion_threads (the target).
SELECT * FROM discussion_promotions
WHERE project_id = @project_id
  AND promoted_kind = @promoted_kind
  AND promoted_ref = @promoted_ref
ORDER BY promoted_at, id;

-- name: GetDiscussionKnowledgeTargetProject :one
-- The project that owns the object a publication pid names (T0811's
-- knowledge target check): a thread's knowledge target must be a
-- publication OF THE THREAD'S PROJECT, and this resolves the one fact that
-- decides it. It is a read of the publication, its version and the object
-- that carries it — no visibility predicate: the command's project gate
-- has already run the caller's read rule, and a second SQL-shaped copy of
-- it is how two answers to "who may see this" start to disagree (the same
-- rule knowledgepublish.AudienceFor owns).
SELECT so.project_id
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
WHERE kp.pid = @pid;
