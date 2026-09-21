package researchcontext

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// The draft's lifecycle vocabulary — the two values migration 00134's CHECK
// admits. They are constants rather than a type because the column is text and
// the store compares the string it read; a named type would have to be
// converted at every boundary for no rule it does not already have.
const (
	// DraftStatusDraft: the candidate. Nothing about the project is research
	// state yet.
	DraftStatusDraft = "draft"
	// DraftStatusConfirmed: the initial state exists. The row records which
	// branch, state, commit and object version it produced, and it stays
	// readable forever (there is no un-confirm and no delete).
	DraftStatusConfirmed = "confirmed"
)

// minIdempotencyKeyLen is components.parameters.IdempotencyKey's minLength
// (specs/api/openapi.yaml) — the same bound cmd/api/mergehttp enforces for its
// own route and the database enforces in 00134's CHECK. A key shorter than
// this cannot name a request unambiguously, so it is refused before anything
// is created.
const minIdempotencyKeyLen = 8

// minResearchQuestionLen is the `statement` minLength of
// specs/schemas/research_question.schema.json, applied at START for the
// caller's sake: a draft whose question cannot become a research_question
// object would be a draft that can never be confirmed, and the caller would
// only learn that after the project existed. The authoritative check is still
// the write path's (rsg semantics + the schema), which runs at confirmation.
const minResearchQuestionLen = 3

// Draft is one Draft Research Context row (migration 00134). Every field is a
// column; the confirmation fields are nil until the draft is confirmed, and
// the database's all-or-nothing CHECK is what makes that a guarantee rather
// than a convention.
type Draft struct {
	// ID is the draftId both routes speak in.
	ID string
	// ProjectID is the planning project the draft belongs to. It exists from
	// the start; it has no branch and no state until the draft is confirmed.
	ProjectID string
	// SearchID is the answered search the draft was built from.
	SearchID string
	// CreatedBy is the actor who started the draft.
	CreatedBy string
	// ResearchQuestion is the caller's question, verbatim.
	ResearchQuestion string
	// The caller's keep/drop/reclassify decisions over the search's
	// selected_refs — three sets, each a subset of it.
	ReferencedRefs []string
	DependencyRefs []string
	CandidateRefs  []string
	// Uncertainties and Hypotheses are docs/14:25's "known uncertainties" and
	// "agent-suggested hypotheses". They live here and nowhere else (see the
	// package doc).
	Uncertainties []string
	Hypotheses    []string
	// Status is DraftStatusDraft or DraftStatusConfirmed.
	Status string
	// IdempotencyKey is the key the START route created this draft with.
	// Stored, not hashed: it is what a replay is compared against.
	IdempotencyKey string
	// ConfirmIdempotencyKey is the key the CONFIRM route used, once. Nil for
	// an unconfirmed draft.
	ConfirmIdempotencyKey *string
	// ConfirmedAt / ConfirmedBy record who confirmed it and when.
	ConfirmedAt *time.Time
	ConfirmedBy *string
	// The initial state the confirmation created: the branch, the state the
	// transition produced, the state commit that names the transition, and the
	// research_question object version. All nil until confirmation.
	InitialBranchID   *string
	InitialStateID    *string
	InitialCommitID   *string
	QuestionObjectID  *string
	QuestionVersionID *string
	// CreatedAt is the draft's creation time.
	CreatedAt time.Time
}

// RefSets returns the draft's three ref sets in one slice, in the order they
// are declared. The containment rule is about all three together (a ref is a
// ref whichever list it is in), so validation and the store's guard read them
// as one.
func (d Draft) RefSets() []string {
	out := make([]string, 0, len(d.ReferencedRefs)+len(d.DependencyRefs)+len(d.CandidateRefs))
	out = append(out, d.ReferencedRefs...)
	out = append(out, d.DependencyRefs...)
	out = append(out, d.CandidateRefs...)
	return out
}

// SearchRecord is what the flow reads from an answered search
// (search_records, 00121): whose it is, and the set its refs are drawn from.
// The answer document itself is deliberately NOT read — nothing in the draft
// is derived from the answer's prose, and the record stays the place the
// answer is read from (see queries/search.sql).
type SearchRecord struct {
	// ID is the searchId the start route is addressed by.
	ID string
	// ActorID is the user the search ran as. Only that user may start a
	// project from it.
	ActorID string
	// SelectedRefs are the ranked refs the search selected, in rank order.
	// The draft's refs must be a subset of this, element by element.
	SelectedRefs []string
}

// StartInput is one POST /search/{searchId}:start-project, decoded.
type StartInput struct {
	// SearchID names the answered search (path parameter).
	SearchID string
	// IdempotencyKey is the contract-required header.
	IdempotencyKey string
	// Name and Slug are the new project's (contract-required) identity, and
	// Slug is globally unique for a personal project (00019) — a taken slug
	// is answered with the project surface's own PROJECT_SLUG_TAKEN.
	Name string
	Slug string
	// Visibility is the contract's optional enum; an absent value means
	// private here (see Service.Start: nothing becomes public by omission).
	Visibility domain.ProjectVisibility
	// ResearchQuestion is the draft's question, verbatim.
	ResearchQuestion string
	// The caller's keep/drop/reclassify decisions over the search's
	// selected_refs. Each may be empty.
	ReferencedRefs []string
	DependencyRefs []string
	CandidateRefs  []string
	// The free-text halves of the draft (docs/14:25).
	Uncertainties []string
	Hypotheses    []string
}

// StartResult is one started (or replayed) draft.
type StartResult struct {
	// Project is the project the draft belongs to. It is returned so the
	// transport answers the same project document POST /projects does
	// without a second read.
	Project domain.Project
	// Draft is the draft row, as stored.
	Draft Draft
	// Replayed reports that no write happened: the search already had this
	// draft and the key matches, so the answer is the one the first call
	// produced.
	Replayed bool
}

// ConfirmInput is one POST /research-context-drafts/{draftId}:confirm,
// decoded.
type ConfirmInput struct {
	// DraftID names the draft (path parameter).
	DraftID string
	// IdempotencyKey is the contract-required header.
	IdempotencyKey string
}

// ConfirmResult is one confirmation (or its replay): the initial state the
// draft became, with the ids a reader follows to check it.
type ConfirmResult struct {
	// Draft is the confirmed row (status 'confirmed', the confirmation
	// record set).
	Draft Draft
	// BranchID is the initial branch (domain.MainBranchName).
	BranchID string
	// StateCommitID names the transition that formed the initial state
	// (docs/09 §2). From it a reader reaches the branch, the state and the
	// object version the transition created.
	StateCommitID string
	// StateID is the state the transition produced (the branch's new head).
	StateID string
	// QuestionObjectID and QuestionVersionID name the research_question
	// object and its version 1.
	QuestionObjectID  string
	QuestionVersionID string
	// Replayed reports that this call wrote no research state: the draft was
	// already confirmed under this key, or the state it records is the
	// transition an earlier attempt of the same confirmation had already
	// written (ProjectState.Adopted). A call that wrote state itself — even the
	// question written on a branch an earlier attempt left — answers false.
	Replayed bool
}

// ConfirmWrite is the confirmation record the store writes in one
// compare-and-swap.
type ConfirmWrite struct {
	DraftID   string
	ActorID   string
	Key       string
	BranchID  string
	StateID   string
	CommitID  string
	ObjectID  string
	VersionID string
}

// InitialState names the initial state a confirmation records: the branch, the
// state the transition produced, the commit that names the transition
// (docs/09 §2) and the research_question object version it created. The fresh
// path and the adoption path both produce one; only where it comes from differs
// (see ProjectState.Adopted).
type InitialState struct {
	BranchID  string
	StateID   string
	CommitID  string
	ObjectID  string
	VersionID string
}

// ProjectState is what the draft's project already has, as the confirmation
// must read it BEFORE it writes anything.
//
// The confirmation is the one write in this flow that makes research state, and
// it is not atomic across the two stores it touches: the state belongs to the
// RSG write path (rsg.Service, its own transactions) and the record belongs to
// the draft table. So the sequence "write the state, then record it" has a
// window, and this value is what makes the window recoverable rather than
// terminal — a retry reads what is already there instead of writing it twice.
//
// # The three answers, and why they are enough
//
//	no main branch        → the ordinary case: nothing has been written yet.
//	main + this question  → an earlier attempt's own work (Ours): finish it.
//	main + anything else  → not this confirmation's work; refuse.
//
// The middle case is recognized by what the confirmation itself put on the
// branch, in the order it puts it there: the branch's purpose is the draft's
// question (purposeFor, set when the branch is created), and the state the
// transition produced carries a research_question object version whose
// `statement` is that same question. A project that only has a draft has no
// state at all (docs/14:25 — the initial state forms on confirmation), and the
// confirmation is the only writer it has, so a main branch carrying this
// draft's question can only have been put there by this draft's own
// confirmation — by an attempt that died before it recorded what it wrote.
//
// # Why the purpose matters as well as the state
//
// The purpose is written FIRST and the state SECOND, so recognizing only the
// state would leave the narrower half of the window unrecoverable: an attempt
// that created the branch and died before the transition would leave a project
// whose main branch carries nothing, which no retry could ever confirm and no
// retry could ever clean up (the draft row is append-only). The purpose is
// matched only when the branch has nothing on it (MainCommits == 0): a branch
// with this draft's question as its purpose AND no transitions is that stopped
// attempt, whereas a branch with commits of its own is somebody else's work
// whatever its purpose says.
type ProjectState struct {
	// MainBranchID is the project's main branch, or "" when the project has no
	// branch yet.
	MainBranchID string
	// MainPurpose is that branch's purpose column: the draft's question
	// (purposeFor) when the confirmation created it, and whatever a branch this
	// flow did not create carries. Nil for no main branch, or a branch that
	// records no purpose.
	MainPurpose *string
	// MainCommits is how many transitions have been committed on that branch.
	// Zero means nothing has been written onto it since it was created.
	MainCommits int
	// Adopted is the initial state an earlier attempt already wrote, or nil when
	// the project's main branch does not carry this draft's research question.
	Adopted *InitialState
}

// HasMain reports whether the project already has its main branch.
func (p ProjectState) HasMain() bool { return p.MainBranchID != "" }

// Ours reports whether the main branch is this draft's own confirmation's work
// — the question the confirmation stamps on everything it creates, found on
// the branch (see the type's doc). It is the one place that decision is made:
// the store reports columns, the service reads them as this.
func (p ProjectState) Ours(question string) bool {
	switch {
	case !p.HasMain():
		return false
	case p.Adopted != nil:
		// The transition is there and its state carries this question.
		return true
	case p.MainCommits > 0:
		// A branch with a history of its own is not a stopped attempt.
		return false
	default:
		return p.MainPurpose != nil && *p.MainPurpose == purposeFor(question)
	}
}

// Ports (docs/52: the application orchestrates against ports; adapters live in
// internal/persistence). Four things outside itself, and each one is a step of
// the flow rather than a convenience.

// SearchRecords reads one answered search. The production implementation is
// *persistence.SearchRecordStore.
type SearchRecords interface {
	// GetSearchRecord returns the record, or ErrSearchNotFound when the id
	// names none.
	GetSearchRecord(ctx context.Context, searchID string) (SearchRecord, error)
}

// DraftStore owns the draft table: the two replay lookups, the read the
// confirm route authorizes against, the insert, and the confirmation's
// compare-and-swap. The production implementation is
// *persistence.ResearchContextStore.
type DraftStore interface {
	// LookupBySearch returns the draft a search already has, or nil.
	LookupBySearch(ctx context.Context, searchID string) (*Draft, error)
	// LookupByStartKey returns the draft an actor's start key already names,
	// or nil.
	LookupByStartKey(ctx context.Context, actorID, idempotencyKey string) (*Draft, error)
	// LookupByConfirmKey returns the draft an actor's CONFIRM key already
	// confirmed, or nil. UNIQUE (confirmed_by, confirm_idempotency_key) makes
	// this at most one row by construction — the same index the store's CAS
	// lands on, read ahead of the state write so a request that is about to be
	// refused as a reused key costs nothing (see Service.Confirm).
	LookupByConfirmKey(ctx context.Context, actorID, idempotencyKey string) (*Draft, error)
	// ProjectState reads what the draft's project already has before the
	// confirmation writes anything (see ProjectState): no main branch, a main
	// branch carrying this draft's question, or a main branch carrying
	// something else.
	ProjectState(ctx context.Context, projectID, researchQuestion string) (ProjectState, error)
	// Get returns one draft by id, or ErrDraftNotFound.
	Get(ctx context.Context, draftID string) (Draft, error)
	// Insert writes the draft, or reports ErrAlreadyStarted (the search has
	// one) / ErrIdempotencyConflict (the key names another draft) — the
	// database's unique keys are the arbiter, so two racing requests cannot
	// both win.
	Insert(ctx context.Context, d Draft) (Draft, error)
	// Confirm writes the confirmation record only while the row is still
	// 'draft' (compare-and-swap), or reports ErrNotConfirmable when it is
	// not — which is how two racing confirmations produce exactly one initial
	// state.
	Confirm(ctx context.Context, in ConfirmWrite) (Draft, error)
}

// Projects is the project surface this flow reuses: the create path (whose
// authorization, slug/name/purpose validation, organization gate and audit are
// all its own) and the membership resolution the confirm route authorizes on.
// The production implementation is *projects.Service, so the project rules are
// not re-implemented here.
type Projects interface {
	// Create creates the project and the creator's owner membership.
	Create(ctx context.Context, actor domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error)
	// GetMembership resolves the actor's role in the project, answering
	// projects.ErrMemberNotFound for a project they may read but do not belong
	// to and projects.ErrProjectNotFound for one they may not see.
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
	// Get reads one project for the caller (the confirmation mirrors the
	// project's visibility onto the branch it creates, docs/09 §1).
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// StateWriter is the ordinary RSG write path the confirmation goes through.
// The production implementation is *rsg.Service — the same object every other
// state-writing surface uses, so a confirmation is a normal state transition
// and not a second way to write one.
type StateWriter interface {
	CreateBranch(ctx context.Context, actor domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error)
	CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error)
}

// CommitReader names the transition a confirmation produced. The production
// implementation is *persistence.ResearchContextStore.
//
// It is a read of its own rather than a field of CreateObject's result because
// the state commit is the RSG's own artifact (docs/09 §2): the confirmation
// records WHICH transition formed the initial state, and asking the transition
// log for it keeps the answer the log's, not a value a caller threaded
// through.
type CommitReader interface {
	// CommitForState returns the commit that produced stateID on branchID, or
	// an error when the log does not name exactly one (a store
	// inconsistency, never a choice to make).
	CommitForState(ctx context.Context, branchID, stateID string) (domain.StateCommit, error)
}
