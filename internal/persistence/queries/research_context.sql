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
-- WHERE status = 'draft' is the whole concurrency story for TWO
-- confirmations OF THE SAME DRAFT: the branch, the state and the
-- research_question object are created by the application through the
-- ordinary RSG write path before this statement runs, and the second one of
-- a pair either fails earlier (branch names are unique per project,
-- 00004 — the branch the first one created is named main) or loses here and
-- reads no row back (pgx.ErrNoRows), which the write path answers as the
-- draft's own 409. Exactly one confirmation is recorded, whatever the
-- concurrency.
--
-- It is NOT the whole story for two DIFFERENT drafts: this statement's
-- unique index (research_context_drafts_confirm_key_uniq) refuses the second
-- one with a constraint violation, and by then that request has already
-- written its own project's state through the RSG path, in transactions of
-- its own that nothing here can roll back. That is why the application reads
-- the key BEFORE it writes anything (GetResearchContextDraftByConfirmKey
-- below) and why it can recover a state an earlier attempt left
-- (GetProjectInitialState below); see researchcontext.Service.Confirm.
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

-- ---------------------------------------------------------------------------
-- The confirmation's own reads, ahead of its writes (T0909)
--
-- A confirmation writes research state through the RSG path and then records
-- it here, and the two are not one transaction (the state belongs to
-- rsg.Service, whose transactions are its own; the record belongs to this
-- table). The window that leaves is recoverable only if the NEXT call can see
-- what the previous one did before it writes anything, which is what these two
-- reads are for. Both are reads this flow alone makes, of rows this flow alone
-- creates, so they live beside the flow's own queries rather than in the RSG's.

-- name: GetResearchContextDraftByConfirmKey :one
-- The draft an actor's confirm key already confirmed, if any. This is the
-- replay read's other half and the guard the confirmation runs BEFORE it
-- writes state: a key that already names a confirmation can be recognized by
-- a read, and a request that is going to be refused for it must not leave a
-- branch, a state, a commit and an object version behind — the draft row it
-- would have confirmed is append-only (00134's guard), so nothing could ever
-- reconcile the two afterwards.
--
-- The predicate is exactly the partial unique index's
-- (research_context_drafts_confirm_key_uniq: confirmed_by +
-- confirm_idempotency_key, WHERE the key IS NOT NULL), so this read and the
-- constraint's refusal can never disagree about which key names which
-- confirmation; the index remains the arbiter for a writer that races this
-- read, and the write path maps that refusal onto the same outcome.
SELECT
    id, project_id, search_id, created_by, research_question,
    referenced_refs, dependency_refs, candidate_refs,
    uncertainties, hypotheses, status,
    idempotency_key, confirm_idempotency_key,
    confirmed_at, confirmed_by,
    initial_branch_id, initial_state_id, initial_commit_id,
    question_object_id, question_version_id, created_at
FROM research_context_drafts
WHERE confirmed_by = @confirmed_by AND confirm_idempotency_key = @confirm_idempotency_key;

-- name: GetProjectInitialState :one
-- What the draft's project already has, as the confirmation must read it
-- before it writes anything: the project's main branch (the one branch the
-- confirmation creates), its purpose and how many transitions have been
-- committed on it, and — when one of those transitions produced a state
-- carrying a research_question object version whose `statement` is exactly
-- this draft's question — that transition's own ids.
--
-- One row per project that HAS a main branch; zero rows (pgx.ErrNoRows) for a
-- project that has none, which is every project this flow creates until its
-- draft is confirmed (T0908 asserts it: after start-project the four tables a
-- transition writes hold zero rows). The four adoption columns are NULL when
-- main exists and does not carry this question — a state this confirmation did
-- not make, and must not record.
--
-- Why the question identifies the state: the confirmation is the only writer
-- this project has between its creation and its initial state, and the only
-- thing it writes is the draft's own research question (docs/14:25: the
-- initial state forms on confirmation). A state on main carrying exactly that
-- question is therefore this draft's own confirmation's work — the one an
-- earlier attempt wrote and died before recording.
--
-- The purpose column is that same question, and it is written EARLIER than the
-- state (BranchStore.CreateBranch stores the purpose the caller passed, and
-- the confirmation passes purposeFor(question)): it is what makes an attempt
-- that stopped between the branch and the transition recognizable. Which
-- columns are trusted when is the application's rule, not this query's — see
-- researchcontext.ProjectState.Ours.
--
-- The branch is read by name and not by "the project's first branch": main is
-- what the confirmation asks the RSG path for (domain.MainBranchName), and a
-- branch row can only be renamed by a path that does not exist here.
SELECT
    b.id AS main_branch_id,
    b.purpose,
    (SELECT count(*) FROM state_commits c WHERE c.branch_id = b.id) AS main_commits,
    m.commit_id,
    m.state_id,
    m.object_id,
    m.version_id
FROM branches b
LEFT JOIN LATERAL (
    SELECT c.id AS commit_id, c.result_state_id AS state_id, o.id AS object_id, v.id AS version_id
      FROM state_commits c
      JOIN scientific_object_versions v ON v.state_id = c.result_state_id
      JOIN scientific_objects o ON o.id = v.object_id
     WHERE c.branch_id = b.id
       AND o.project_id = b.project_id
       AND o.object_type = 'research_question'
       AND v.payload ->> 'statement' = @statement::text
     ORDER BY c.created_at, c.id
     LIMIT 1
) m ON true
WHERE b.project_id = @project_id AND b.name = 'main';
