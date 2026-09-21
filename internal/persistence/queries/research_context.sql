-- The Draft Research Context (T0908): the writes and the two replay reads of
-- the flow docs/14:25 describes — a search answer becomes a draft, and the
-- draft becomes a project's initial state when its owner confirms it.
--
-- The table's own rules (one draft per search, one draft per key per actor,
-- refs drawn only from the search's selected_refs, draft -> confirmed once)
-- live in migration 00134, not here. What belongs here is the one thing SQL
-- can say that a constraint cannot: the confirmation is a compare-and-swap on
-- status = 'draft', so two confirmations racing each other produce ONE
-- transition and one loser that reads no row back, rather than two initial
-- states for one project.
--
-- There is no DELETE anywhere in this file. A draft is the record of how a
-- project began (migration 00134's guard refuses the delete even if one were
-- written here).

-- name: InsertResearchContextDraft :one
-- The draft row, written once. Every column is a value the caller already
-- produced: the project was created for it, the search it was built from is
-- already recorded, and the refs are the caller's keep/drop/reclassify
-- decisions over that search's selected_refs (refused by the table's trigger
-- if any of them is not — this query does not repeat the rule, exactly as
-- InsertSearchRecord does not repeat citations <@ selected_refs).
--
-- The caller passes an empty array rather than NULL for the five list
-- columns: "the caller kept nothing" and "the caller sent nothing" are the
-- same state here, and the columns are NOT NULL so that the draft's shape
-- does not depend on a reader's generosity with NULL.
INSERT INTO research_context_drafts (
    project_id, search_id, created_by, research_question,
    referenced_refs, dependency_refs, candidate_refs,
    uncertainties, hypotheses, idempotency_key
) VALUES (
    @project_id, @search_id, @created_by, @research_question,
    @referenced_refs, @dependency_refs, @candidate_refs,
    @uncertainties, @hypotheses, @idempotency_key
)
RETURNING *;

-- name: GetResearchContextDraft :one
-- One draft in full, by its id — the draftId both routes speak in, and the
-- only read the confirm route needs before it decides whether the caller may
-- confirm (the project it names is the project whose membership is checked).
SELECT
    id, project_id, search_id, created_by, research_question,
    referenced_refs, dependency_refs, candidate_refs,
    uncertainties, hypotheses, status,
    idempotency_key, confirm_idempotency_key,
    confirmed_at, confirmed_by,
    initial_branch_id, initial_state_id, initial_commit_id,
    question_object_id, question_version_id, created_at
FROM research_context_drafts
WHERE id = @id;

-- name: GetResearchContextDraftBySearch :one
-- The draft a search already has, if it has one. This is the replay read of
-- the START route: a request that finds a row here resumes the draft the key
-- (or another key) already created instead of opening a second project.
-- UNIQUE (search_id) makes this at most one row by construction.
SELECT
    id, project_id, search_id, created_by, research_question,
    referenced_refs, dependency_refs, candidate_refs,
    uncertainties, hypotheses, status,
    idempotency_key, confirm_idempotency_key,
    confirmed_at, confirmed_by,
    initial_branch_id, initial_state_id, initial_commit_id,
    question_object_id, question_version_id, created_at
FROM research_context_drafts
WHERE search_id = @search_id;

-- name: GetResearchContextDraftByStartKey :one
-- The other half of the start route's replay: the draft a caller's own
-- Idempotency-Key already created. Reached only when the search has no
-- draft, and it is how "the same key, the same project" is answered when the
-- caller cannot name the search again. UNIQUE (created_by, idempotency_key)
-- makes this at most one row by construction; a key that names a draft for a
-- DIFFERENT search is the IDEMPOTENCY_CONFLICT the write path reports, not a
-- replay.
SELECT
    id, project_id, search_id, created_by, research_question,
    referenced_refs, dependency_refs, candidate_refs,
    uncertainties, hypotheses, status,
    idempotency_key, confirm_idempotency_key,
    confirmed_at, confirmed_by,
    initial_branch_id, initial_state_id, initial_commit_id,
    question_object_id, question_version_id, created_at
FROM research_context_drafts
WHERE created_by = @created_by AND idempotency_key = @idempotency_key;

-- name: ConfirmResearchContextDraft :one
-- The confirmation, as a compare-and-swap.
--
-- WHERE status = 'draft' is the whole concurrency story: the branch, the
-- state and the research_question object are created by the application
-- through the ordinary RSG write path BEFORE this statement runs, so two
-- confirmations racing each other each create their own state and exactly one
-- of them lands here — the loser reads no row (pgx.ErrNoRows) and rolls its
-- own transaction back, leaving one initial state and one refusal. A second
-- confirmation of an already-confirmed draft therefore cannot rewrite what
-- the first one created.
--
-- The five ids are the transition's own result, handed back by the path that
-- produced them; this query records them and derives nothing. The table's
-- all-or-nothing CHECK means a confirmation missing any of them is not a
-- state the database can store.
UPDATE research_context_drafts
SET status                   = 'confirmed',
    confirm_idempotency_key  = @confirm_idempotency_key,
    confirmed_at             = now(),
    confirmed_by             = @confirmed_by,
    initial_branch_id        = @initial_branch_id,
    initial_state_id         = @initial_state_id,
    initial_commit_id        = @initial_commit_id,
    question_object_id       = @question_object_id,
    question_version_id      = @question_version_id
WHERE id = @id AND status = 'draft'
RETURNING *;
