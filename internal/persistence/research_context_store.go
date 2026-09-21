package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/researchcontext"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ResearchContextStore is the production adapter for the Draft Research
// Context flow (T0908, migration 00134): the two replay lookups, the read the
// confirm route authorizes against, the insert, the confirmation's
// compare-and-swap and the commit read that traces a confirmation.
//
// # What this store does NOT do
//
// It does not create projects (that is projects.Service, whose authorization,
// slug rules, organization gate and audit are its own), and it does not write
// research state (that is the RSG write path; by the time Confirm is called
// here, the branch, the state, the object and the commit already exist and
// this store only records which ones). Both are deliberate: a store that
// duplicated either would be a second implementation of rules that already
// have one, and the two implementations would disagree one day.
//
// # The two unique keys are the arbiters
//
// The write path looks before it writes (a replay is cheaper than a
// constraint violation to explain), but the lookups are an optimization and
// these are the guarantee:
//
//	research_context_drafts_search_key   one draft per search, under any key
//	research_context_drafts_start_key    one draft per (actor, key)
//	research_context_drafts_confirm_key_uniq  one confirmation per (actor, key)
//
// A request that loses a race lands on one of them and is mapped back onto the
// application's vocabulary (ErrAlreadyStarted / ErrIdempotencyConflict), so
// the loser can be answered honestly instead of being told the store broke.
type ResearchContextStore struct {
	pool *pgxpool.Pool
}

// NewResearchContextStore wires the adapter.
func NewResearchContextStore(pool *pgxpool.Pool) *ResearchContextStore {
	return &ResearchContextStore{pool: pool}
}

// LookupBySearch implements researchcontext.DraftStore: the draft a search
// already has, or nil. UNIQUE (search_id) makes this at most one row.
//
// An id that cannot name a row answers nil rather than an error: the caller
// reaches this only after the record was read for that same id, so a
// malformed one has already been refused, and "no draft" is the truth about a
// search that cannot exist.
func (s *ResearchContextStore) LookupBySearch(ctx context.Context, searchID string) (*researchcontext.Draft, error) {
	id, err := textUUID(searchID)
	if err != nil {
		return nil, nil
	}
	row, err := sqlc.New(s.pool).GetResearchContextDraftBySearch(ctx, id)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read research context draft by search: %w", err)
	}
	draft := researchContextDraftFromRow(row)
	return &draft, nil
}

// LookupByStartKey implements researchcontext.DraftStore: the draft an actor's
// start key already names, or nil. UNIQUE (created_by, idempotency_key) makes
// this at most one row.
func (s *ResearchContextStore) LookupByStartKey(ctx context.Context, actorID, idempotencyKey string) (*researchcontext.Draft, error) {
	actor, err := textUUID(actorID)
	if err != nil {
		return nil, nil
	}
	row, err := sqlc.New(s.pool).GetResearchContextDraftByStartKey(ctx, sqlc.GetResearchContextDraftByStartKeyParams{
		CreatedBy:      actor,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read research context draft by key: %w", err)
	}
	draft := researchContextDraftFromRow(row)
	return &draft, nil
}

// Get implements researchcontext.DraftStore. An id that names no row — or one
// that is not a uuid at all — answers ErrDraftNotFound, the same answer the
// application gives a draft the caller may not see (existence hiding).
func (s *ResearchContextStore) Get(ctx context.Context, draftID string) (researchcontext.Draft, error) {
	id, err := textUUID(draftID)
	if err != nil {
		return researchcontext.Draft{}, researchcontext.ErrDraftNotFound
	}
	row, err := sqlc.New(s.pool).GetResearchContextDraft(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return researchcontext.Draft{}, researchcontext.ErrDraftNotFound
	}
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("persistence: read research context draft: %w", err)
	}
	return researchContextDraftFromRow(row), nil
}

// LookupByConfirmKey implements researchcontext.DraftStore: the draft an
// actor's CONFIRM key already confirmed, or nil.
//
// It reads the very rows the partial unique index
// (research_context_drafts_confirm_key_uniq) covers, with the index's own
// predicate, so this read cannot disagree with the constraint about which key
// names which confirmation. It exists because the confirmation runs it BEFORE
// it writes research state: the write path is a different store with its own
// transactions, so the index's refusal at the end of the flow arrives too late
// to prevent a state being written for a request that is about to be refused.
func (s *ResearchContextStore) LookupByConfirmKey(ctx context.Context, actorID, idempotencyKey string) (*researchcontext.Draft, error) {
	actor, err := textUUID(actorID)
	if err != nil {
		return nil, nil
	}
	key := idempotencyKey
	row, err := sqlc.New(s.pool).GetResearchContextDraftByConfirmKey(ctx, sqlc.GetResearchContextDraftByConfirmKeyParams{
		ConfirmedBy:           actor,
		ConfirmIdempotencyKey: &key,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read research context draft by confirm key: %w", err)
	}
	draft := researchContextDraftFromRow(row)
	return &draft, nil
}

// ProjectState implements researchcontext.DraftStore: what the draft's project
// already has, as the confirmation must read it before it writes anything.
//
// The row it reads is the project's MAIN branch, with the branch's own purpose
// and commit count and — when a transition on it produced a state carrying a
// research_question whose statement is exactly the caller's question — that
// transition's ids. A project with no main branch answers pgx.ErrNoRows, which
// is the ordinary case: nothing has been written for it yet.
//
// Every field is a column (or a count of rows in one); nothing here decides
// what the facts MEAN — whether the branch is this confirmation's own work is
// the application's rule (researchcontext.ProjectState.Ours), and it is stated
// once, there.
func (s *ResearchContextStore) ProjectState(ctx context.Context, projectID, researchQuestion string) (researchcontext.ProjectState, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return researchcontext.ProjectState{}, fmt.Errorf("%w: project id: %v", researchcontext.ErrStore, err)
	}
	row, err := sqlc.New(s.pool).GetProjectInitialState(ctx, sqlc.GetProjectInitialStateParams{
		ProjectID: project,
		Statement: researchQuestion,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return researchcontext.ProjectState{}, nil
	}
	if err != nil {
		return researchcontext.ProjectState{}, fmt.Errorf("persistence: read the project's initial state: %w", err)
	}
	state := researchcontext.ProjectState{
		MainBranchID: pgUUIDToText(row.MainBranchID),
		MainPurpose:  row.Purpose,
		MainCommits:  int(row.MainCommits),
	}
	// The adoption columns are NULL together: the lateral join found no
	// transition on this branch whose result state carries the question.
	if row.CommitID.Valid {
		state.Adopted = &researchcontext.InitialState{
			BranchID:  state.MainBranchID,
			StateID:   pgUUIDToText(row.StateID),
			CommitID:  pgUUIDToText(row.CommitID),
			ObjectID:  pgUUIDToText(row.ObjectID),
			VersionID: pgUUIDToText(row.VersionID),
		}
	}
	return state, nil
}

// Insert implements researchcontext.DraftStore: one INSERT, and the database's
// unique keys decide who wins a race.
//
// The refs guard (00134's research_context_draft_refs_guard) is mapped to
// ErrUngroundedRef as well, even though the application checks refs before it
// creates the project: a writer that never went through that check reaches
// SQL with a stranger ref and must be told what it did, not handed a generic
// store failure. The trigger's message names the ref; this mapping does not
// parse it (the message is for a human), so the error carries the sentinel and
// the caller names the ref it sent.
func (s *ResearchContextStore) Insert(ctx context.Context, d researchcontext.Draft) (researchcontext.Draft, error) {
	projectID, err := textUUID(d.ProjectID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: project id: %v", researchcontext.ErrStore, err)
	}
	searchID, err := textUUID(d.SearchID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: search id: %v", researchcontext.ErrStore, err)
	}
	createdBy, err := textUUID(d.CreatedBy)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: actor id: %v", researchcontext.ErrStore, err)
	}
	row, err := sqlc.New(s.pool).InsertResearchContextDraft(ctx, sqlc.InsertResearchContextDraftParams{
		ProjectID:        projectID,
		SearchID:         searchID,
		CreatedBy:        createdBy,
		ResearchQuestion: d.ResearchQuestion,
		ReferencedRefs:   emptyNotNil(d.ReferencedRefs),
		DependencyRefs:   emptyNotNil(d.DependencyRefs),
		CandidateRefs:    emptyNotNil(d.CandidateRefs),
		Uncertainties:    emptyNotNil(d.Uncertainties),
		Hypotheses:       emptyNotNil(d.Hypotheses),
		IdempotencyKey:   d.IdempotencyKey,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr) && constraintNames(pgErr, "research_context_drafts_search_key"):
			return researchcontext.Draft{}, researchcontext.ErrAlreadyStarted
		case errors.As(err, &pgErr) && constraintNames(pgErr, "research_context_drafts_start_key"):
			return researchcontext.Draft{}, researchcontext.ErrIdempotencyConflict
		case errors.As(err, &pgErr) && pgErr.Code == p0001RaiseException:
			// The refs guard. It is the only trigger on INSERT for this
			// table (the confirmation guard fires on UPDATE and DELETE).
			return researchcontext.Draft{}, researchcontext.ErrUngroundedRef
		default:
			return researchcontext.Draft{}, fmt.Errorf("persistence: insert research context draft: %w", err)
		}
	}
	return researchContextDraftFromRow(row), nil
}

// Confirm implements researchcontext.DraftStore as a compare-and-swap:
//
//	UPDATE … SET the confirmation record WHERE id = $1 AND status = 'draft'
//
// Zero rows updated means the draft was confirmed between the caller's read
// and this write (or is not confirmed and the key is already used elsewhere),
// which the write path answers as the contract's 409 — never as a second
// initial state. The statement itself is atomic, so exactly one confirmation
// can record itself per draft, whatever the concurrency.
func (s *ResearchContextStore) Confirm(ctx context.Context, in researchcontext.ConfirmWrite) (researchcontext.Draft, error) {
	draftID, err := textUUID(in.DraftID)
	if err != nil {
		return researchcontext.Draft{}, researchcontext.ErrDraftNotFound
	}
	actorID, err := textUUID(in.ActorID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: confirming actor id: %v", researchcontext.ErrStore, err)
	}
	branchID, err := textUUID(in.BranchID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: branch id: %v", researchcontext.ErrStore, err)
	}
	stateID, err := textUUID(in.StateID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: state id: %v", researchcontext.ErrStore, err)
	}
	commitID, err := textUUID(in.CommitID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: commit id: %v", researchcontext.ErrStore, err)
	}
	objectID, err := textUUID(in.ObjectID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: object id: %v", researchcontext.ErrStore, err)
	}
	versionID, err := textUUID(in.VersionID)
	if err != nil {
		return researchcontext.Draft{}, fmt.Errorf("%w: object version id: %v", researchcontext.ErrStore, err)
	}
	row, err := sqlc.New(s.pool).ConfirmResearchContextDraft(ctx, sqlc.ConfirmResearchContextDraftParams{
		ID:                    draftID,
		ConfirmIdempotencyKey: &in.Key,
		ConfirmedBy:           actorID,
		InitialBranchID:       branchID,
		InitialStateID:        stateID,
		InitialCommitID:       commitID,
		QuestionObjectID:      objectID,
		QuestionVersionID:     versionID,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// The compare-and-swap found no row in 'draft': someone confirmed it
		// first. The application distinguishes a lost race from a repeated
		// request by re-reading.
		return researchcontext.Draft{}, researchcontext.ErrNotConfirmable
	case err != nil:
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && constraintNames(pgErr, "research_context_drafts_confirm_key_uniq") {
			// The key already names ANOTHER draft's confirmation: a client
			// that reused a key is retrying something else, and the answer is
			// never a second confirmation.
			return researchcontext.Draft{}, researchcontext.ErrIdempotencyConflict
		}
		return researchcontext.Draft{}, fmt.Errorf("persistence: confirm research context draft: %w", err)
	}
	return researchContextDraftFromRow(row), nil
}

// CommitForState implements researchcontext.CommitReader: the transition that
// produced stateID on branchID.
//
// It reads the branch's commit log (the indexed path,
// ListStateCommitsByBranch) and picks the commit whose result state is
// stateID. Exactly one commit can have produced a state — a branch's chain is
// linear and project_states are content-addressed — so "not exactly one" is
// reported as an inconsistency rather than resolved by picking.
func (s *ResearchContextStore) CommitForState(ctx context.Context, branchID, stateID string) (domain.StateCommit, error) {
	branch, err := textUUID(branchID)
	if err != nil {
		return domain.StateCommit{}, fmt.Errorf("persistence: state commit: branch id: %w", err)
	}
	want, err := textUUID(stateID)
	if err != nil {
		return domain.StateCommit{}, fmt.Errorf("persistence: state commit: state id: %w", err)
	}
	rows, err := sqlc.New(s.pool).ListStateCommitsByBranch(ctx, branch)
	if err != nil {
		return domain.StateCommit{}, fmt.Errorf("persistence: list state commits: %w", err)
	}
	var found []domain.StateCommit
	for _, row := range rows {
		if row.ResultStateID == want {
			found = append(found, stateCommitFromRow(row))
		}
	}
	if len(found) != 1 {
		return domain.StateCommit{}, fmt.Errorf("persistence: state commit: %d commits produced state %s on branch %s (want exactly one)", len(found), stateID, branchID)
	}
	return found[0], nil
}

// p0001RaiseException is PostgreSQL's SQLSTATE for a plpgsql RAISE EXCEPTION,
// which is how migration 00134's guards refuse (and how the repository's other
// cross-table guards do).
const p0001RaiseException = "P0001"

// researchContextDraftFromRow converts one research_context_drafts row to the
// application's value. Every field is a column; nothing is derived.
func researchContextDraftFromRow(row sqlc.ResearchContextDraft) researchcontext.Draft {
	return researchcontext.Draft{
		ID:                    pgUUIDToText(row.ID),
		ProjectID:             pgUUIDToText(row.ProjectID),
		SearchID:              pgUUIDToText(row.SearchID),
		CreatedBy:             pgUUIDToText(row.CreatedBy),
		ResearchQuestion:      row.ResearchQuestion,
		ReferencedRefs:        row.ReferencedRefs,
		DependencyRefs:        row.DependencyRefs,
		CandidateRefs:         row.CandidateRefs,
		Uncertainties:         row.Uncertainties,
		Hypotheses:            row.Hypotheses,
		Status:                row.Status,
		IdempotencyKey:        row.IdempotencyKey,
		ConfirmIdempotencyKey: row.ConfirmIdempotencyKey,
		ConfirmedAt:           timestamptzPtr(row.ConfirmedAt),
		ConfirmedBy:           uuidPtr(row.ConfirmedBy),
		InitialBranchID:       uuidPtr(row.InitialBranchID),
		InitialStateID:        uuidPtr(row.InitialStateID),
		InitialCommitID:       uuidPtr(row.InitialCommitID),
		QuestionObjectID:      uuidPtr(row.QuestionObjectID),
		QuestionVersionID:     uuidPtr(row.QuestionVersionID),
		CreatedAt:             row.CreatedAt.Time,
	}
}

// GetSearchRecord implements researchcontext.SearchRecords over the
// SearchRecordStore declared in search_record_store.go: one answered search's
// actor and its selected refs.
//
// It is a method on *SearchRecordStore and not on *ResearchContextStore because
// it reads search_records — the store that owns the record owns its reader.
// What puts the method's declaration in THIS file is its only caller: the draft
// flow's Start, which needs the record's actor (whose search it is) and its
// selected refs (the boundary the draft's refs are checked against). The query
// it runs sits with the record's other queries, in queries/search.sql.
func (s *SearchRecordStore) GetSearchRecord(ctx context.Context, searchID string) (researchcontext.SearchRecord, error) {
	id, err := textUUID(searchID)
	if err != nil {
		return researchcontext.SearchRecord{}, researchcontext.ErrSearchNotFound
	}
	row, err := s.queries.GetSearchRecord(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return researchcontext.SearchRecord{}, researchcontext.ErrSearchNotFound
	}
	if err != nil {
		return researchcontext.SearchRecord{}, fmt.Errorf("persistence: read search record: %w", err)
	}
	return researchcontext.SearchRecord{
		ID:           pgUUIDToText(row.ID),
		ActorID:      pgUUIDToText(row.ActorID),
		SelectedRefs: row.SelectedRefs,
	}, nil
}
