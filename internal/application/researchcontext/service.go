package researchcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Service runs the two commands of the flow (doc.go). It holds no state of
// its own: every dependency is handed to it at wiring time.
type Service struct {
	searches SearchRecords
	drafts   DraftStore
	projects Projects
	states   StateWriter
	commits  CommitReader
	authz    authz.Engine
}

// Deps carries the production graph. All six are required; New refuses
// without them because a missing one does not degrade either command, it
// changes what the command means (a start with no search reader would invent
// its refs; a confirm with no state writer could not form the state it claims
// to form).
type Deps struct {
	SearchRecords SearchRecords
	DraftStore    DraftStore
	Projects      Projects
	StateWriter   StateWriter
	CommitReader  CommitReader
	Authz         authz.Engine
}

// New wires the service. A missing dependency panics: it can only happen at
// wiring time, in one place (cmd/api/main.go), where a nil field is a
// programming mistake rather than a runtime condition.
func New(deps Deps) *Service {
	switch {
	case deps.SearchRecords == nil:
		panic("researchcontext: Deps.SearchRecords is required")
	case deps.DraftStore == nil:
		panic("researchcontext: Deps.DraftStore is required")
	case deps.Projects == nil:
		panic("researchcontext: Deps.Projects is required")
	case deps.StateWriter == nil:
		panic("researchcontext: Deps.StateWriter is required")
	case deps.CommitReader == nil:
		panic("researchcontext: Deps.CommitReader is required")
	case deps.Authz == nil:
		panic("researchcontext: Deps.Authz is required")
	}
	return &Service{
		searches: deps.SearchRecords,
		drafts:   deps.DraftStore,
		projects: deps.Projects,
		states:   deps.StateWriter,
		commits:  deps.CommitReader,
		authz:    deps.Authz,
	}
}

// Start runs POST /search/{searchId}:start-project: one answered search
// becomes a planning project plus a Draft Research Context, and NO research
// state is written.
//
// # The order, and why each step is where it is
//
//	validate → read the search → replay → refs → project → draft
//
// Validation first, because a key that cannot name a request unambiguously is
// not worth a single row. Then the search, because nothing about the request
// means anything without the record it is built from — and the record's actor
// is the first authorization fact: a search the caller did not run is refused
// (the contract's own 409) before the refs are even looked at.
//
// The replay lookups come before the refs check because a replay is a READ:
// answering it must not depend on the request being valid now, only on the key
// (or the search) naming the draft the first call made. That is what makes a
// retry after a timeout succeed.
//
// The refs are checked BEFORE the project is created, and this is the one
// ordering decision worth naming: a caller who sends a ref the search never
// returned has made a mistake that must cost nothing, and creating a planning
// project first would leave them with a project they did not ask for. The
// check is a subset test on the record's own selected_refs, and it does not
// loosen the database's guard (00134) — that refusal remains for any writer
// that never asked this function.
//
// # What is deliberately absent
//
// No branch, state, commit or object is written here, and the function has no
// dependency that could write one: it does not receive the StateWriter at all
// in this path's call graph. That is the structural half of "an agent answer
// never writes main by itself"; the storage half is asserted against real
// PostgreSQL in the e2e test.
func (s *Service) Start(ctx context.Context, actor domain.User, in StartInput) (StartResult, error) {
	if err := validateStart(actor, in); err != nil {
		return StartResult{}, err
	}
	question := strings.TrimSpace(in.ResearchQuestion)

	rec, err := s.searches.GetSearchRecord(ctx, in.SearchID)
	if err != nil {
		if errors.Is(err, ErrSearchNotFound) {
			return StartResult{}, ErrSearchNotFound
		}
		return StartResult{}, fmt.Errorf("%w: read search record: %v", ErrStore, err)
	}
	if rec.ActorID != actor.ID {
		// The contract's 409 for this route. Nothing about the record is
		// echoed: not its query, not its refs, not its actor.
		return StartResult{}, ErrSearchNotOwned
	}

	existing, err := s.drafts.LookupBySearch(ctx, in.SearchID)
	if err != nil {
		return StartResult{}, fmt.Errorf("%w: look up draft by search: %v", ErrStore, err)
	}
	if existing != nil {
		if existing.IdempotencyKey == in.IdempotencyKey {
			project, err := s.projects.Get(ctx, readerFor(actor), existing.ProjectID)
			if err != nil {
				return StartResult{}, fmt.Errorf("%w: read the draft's project: %v", ErrStore, err)
			}
			return StartResult{Project: project, Draft: *existing, Replayed: true}, nil
		}
		return StartResult{}, ErrAlreadyStarted
	}

	// The caller's own key, naming a draft of another search: one key names one
	// creation (00134's UNIQUE (created_by, idempotency_key)). This lookup is
	// reached only when the search has no draft, so it is exactly the case
	// where answering with the key's draft would open a second project.
	byKey, err := s.drafts.LookupByStartKey(ctx, actor.ID, in.IdempotencyKey)
	if err != nil {
		return StartResult{}, fmt.Errorf("%w: look up draft by key: %v", ErrStore, err)
	}
	if byKey != nil {
		return StartResult{}, ErrIdempotencyConflict
	}

	if err := checkRefs(rec.SelectedRefs, referenced(in)); err != nil {
		return StartResult{}, err
	}

	project, _, err := s.projects.Create(ctx, actor, projects.CreateProjectInput{
		Slug:       strings.TrimSpace(in.Slug),
		Name:       strings.TrimSpace(in.Name),
		Purpose:    purposeFor(question),
		Visibility: visibilityOrDefault(in.Visibility),
	})
	if err != nil {
		// The project surface's own outcomes, passed through wrapped so the
		// transport answers the codes the project surface answers
		// (PROJECT_SLUG_TAKEN, PROJECT_FORBIDDEN, VALIDATION_FAILED, …).
		switch {
		case errors.Is(err, projects.ErrValidation),
			errors.Is(err, projects.ErrSlugTaken),
			errors.Is(err, projects.ErrForbidden),
			errors.Is(err, projects.ErrOrgNotFound),
			errors.Is(err, projects.ErrOrgDeactivated),
			errors.Is(err, projects.ErrProgramNotFound),
			errors.Is(err, projects.ErrProgramOrgMismatch):
			return StartResult{}, err
		default:
			return StartResult{}, fmt.Errorf("%w: create the draft's project: %v", ErrStore, err)
		}
	}

	draft, err := s.drafts.Insert(ctx, Draft{
		ProjectID:        project.ID,
		SearchID:         in.SearchID,
		CreatedBy:        actor.ID,
		ResearchQuestion: question,
		ReferencedRefs:   clean(in.ReferencedRefs),
		DependencyRefs:   clean(in.DependencyRefs),
		CandidateRefs:    clean(in.CandidateRefs),
		Uncertainties:    clean(in.Uncertainties),
		Hypotheses:       clean(in.Hypotheses),
		IdempotencyKey:   in.IdempotencyKey,
	})
	if err != nil {
		// A request that raced this one past the lookups above loses here,
		// on the database's unique keys. When the winner's draft is the one
		// THIS key would have produced, the loser answers the replay the
		// caller asked for; otherwise the refusal stands.
		if out, ok := s.racingReplay(ctx, actor, in, err); ok {
			return out, nil
		}
		return StartResult{}, err
	}
	return StartResult{Project: project, Draft: draft}, nil
}

// racingReplay answers a start that lost the insert race with a replay when,
// and only when, the draft now on the row is the one this request was asking
// for: same search, same key. It never turns "somebody else already started
// this search" into a success — that is ErrAlreadyStarted, and the caller
// would otherwise receive a project it did not ask for.
func (s *Service) racingReplay(ctx context.Context, actor domain.User, in StartInput, cause error) (StartResult, bool) {
	if !errors.Is(cause, ErrAlreadyStarted) && !errors.Is(cause, ErrIdempotencyConflict) {
		return StartResult{}, false
	}
	winner, err := s.drafts.LookupBySearch(ctx, in.SearchID)
	if err != nil || winner == nil || winner.IdempotencyKey != in.IdempotencyKey {
		return StartResult{}, false
	}
	project, err := s.projects.Get(ctx, readerFor(actor), winner.ProjectID)
	if err != nil {
		return StartResult{}, false
	}
	return StartResult{Project: project, Draft: *winner, Replayed: true}, true
}

// Confirm runs POST /research-context-drafts/{draftId}:confirm: the draft
// stops being a candidate and becomes the project's initial state.
//
// # The order
//
//	validate → read the draft → authorize → replay → branch → object →
//	commit → record
//
// The authorization happens after the draft is read (its project is what the
// membership is resolved in) and before ANYTHING about it is disclosed or
// written: a caller who is not a maintainer and a caller looking at a draft
// that does not exist are answered identically (ErrDraftNotFound), so the
// route cannot be used to discover drafts.
//
// The replay is a read of the already-confirmed row: the same key answers the
// recorded confirmation, a different key is refused (the contract's 409).
//
// The state writes are the ordinary RSG path — CreateBranch (which creates the
// project's genesis state when it has none) and CreateObject on that branch —
// so the initial state is reached exactly the way every other transition is.
// The confirmation row is written LAST, as a compare-and-swap, and that order
// is deliberate: a crash between the state write and the record leaves a
// confirmed state with an unconfirmed draft row, which the next call repairs
// (the CAS is still 'draft' → the state it writes is already there and its
// commit is found by result state, so the retry lands on the same state and
// records it). The reverse order would publish a confirmation whose state does
// not exist.
func (s *Service) Confirm(ctx context.Context, actor domain.User, in ConfirmInput) (ConfirmResult, error) {
	if err := validateConfirm(actor, in); err != nil {
		return ConfirmResult{}, err
	}

	draft, err := s.drafts.Get(ctx, in.DraftID)
	if err != nil {
		if errors.Is(err, ErrDraftNotFound) {
			return ConfirmResult{}, ErrDraftNotFound
		}
		return ConfirmResult{}, fmt.Errorf("%w: read draft: %v", ErrStore, err)
	}

	// "No permission" and "cannot see" answer the same: the denial is
	// resolved before anything about the draft (its status, its question, its
	// refs) can be observed by the caller.
	if err := s.requireConfirm(ctx, actor, draft.ProjectID); err != nil {
		return ConfirmResult{}, err
	}

	if draft.Status == DraftStatusConfirmed {
		if draft.ConfirmIdempotencyKey != nil && *draft.ConfirmIdempotencyKey == in.IdempotencyKey {
			return confirmResult(draft, true), nil
		}
		return ConfirmResult{}, ErrNotConfirmable
	}

	project, err := s.projects.Get(ctx, readerFor(actor), draft.ProjectID)
	if err != nil {
		return ConfirmResult{}, fmt.Errorf("%w: read the draft's project: %v", ErrStore, err)
	}

	// The initial branch. BaseRef is empty and that is the point: the
	// project has no state yet (a draft is not an RSG), so the branch is
	// created on the genesis root resolveBaseState makes for it — the same
	// call internal/application/templates makes to seed a project's map.
	purpose := purposeFor(draft.ResearchQuestion)
	branch, err := s.states.CreateBranch(ctx, actor, draft.ProjectID, rsg.CreateBranchInput{
		Name:       domain.MainBranchName,
		BaseRef:    "",
		Visibility: domain.BranchVisibility(project.Visibility),
		Purpose:    &purpose,
	})
	if err != nil {
		return ConfirmResult{}, s.stateWriteFailed(ctx, in.DraftID, err)
	}

	// The research question, as the object every other surface creates: the
	// same payload shape the template seeder writes (statement +
	// question_state), validated by the same semantics and schema the RSG
	// write path always runs.
	res, err := s.states.CreateObject(ctx, actor, draft.ProjectID, branch.ID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    questionPayload(draft.ResearchQuestion),
	})
	if err != nil {
		return ConfirmResult{}, s.stateWriteFailed(ctx, in.DraftID, err)
	}

	// The transition that carried it. A branch's chain is linear and states
	// are content-addressed, so exactly one commit produced this state; the
	// reader refuses anything else rather than picking one.
	commit, err := s.commits.CommitForState(ctx, branch.ID, res.Version.StateID)
	if err != nil {
		return ConfirmResult{}, fmt.Errorf("%w: read the confirmation's state commit: %v", ErrStore, err)
	}

	stored, err := s.drafts.Confirm(ctx, ConfirmWrite{
		DraftID:   in.DraftID,
		ActorID:   actor.ID,
		Key:       in.IdempotencyKey,
		BranchID:  branch.ID,
		StateID:   res.Version.StateID,
		CommitID:  commit.ID,
		ObjectID:  res.Object.ID,
		VersionID: res.Version.ID,
	})
	if err != nil {
		if errors.Is(err, ErrNotConfirmable) {
			// Another confirmation landed between the read above and this
			// write. When it was the same caller with the same key, this IS
			// the replay and is answered as one; otherwise the draft is
			// confirmed by someone else's request and the refusal stands.
			if fresh, rerr := s.drafts.Get(ctx, in.DraftID); rerr == nil &&
				fresh.Status == DraftStatusConfirmed &&
				fresh.ConfirmIdempotencyKey != nil && *fresh.ConfirmIdempotencyKey == in.IdempotencyKey {
				return confirmResult(fresh, true), nil
			}
			return ConfirmResult{}, ErrNotConfirmable
		}
		return ConfirmResult{}, fmt.Errorf("%w: record the confirmation: %v", ErrStore, err)
	}
	return confirmResult(stored, false), nil
}

// stateWriteFailed classifies a failed RSG write during a confirmation.
//
// The RSG write can fail for two reasons that must not be confused. One is that
// the project already has the branch this confirmation creates — which is what
// happens when a SECOND confirmation started before the first one recorded
// itself (the branch name is unique per project, 00004). That is not a store
// failure: the draft is confirmed by now, and the answer the caller deserves is
// the same 409 an already-confirmed draft gets. The other is a real failure and
// is reported as one.
func (s *Service) stateWriteFailed(ctx context.Context, draftID string, cause error) error {
	if fresh, err := s.drafts.Get(ctx, draftID); err == nil && fresh.Status == DraftStatusConfirmed {
		return ErrNotConfirmable
	}
	return fmt.Errorf("%w: write the initial state: %v", ErrStore, cause)
}

// requireConfirm authorizes one confirmation: resolve the caller's membership
// in the draft's project, then ask the policy engine for
// authz.ActionMergeMain — the matrix row whose class is "maintainer and above",
// which is exactly the right docs/14:25 gives the confirmation ("用户确认后才形成
// initial state" — the human who owns the project's state decides).
//
// # Why an existing action and not a new one
//
// The matrix (specs/policies/permissions-matrix.csv) is the closed vocabulary
// of product actions (authz/doc.go), and confirm is not a row in it. Reusing
// merge_main is the closest existing row and — the part that matters — its
// cell is the class this operation requires: maintainer allow, owner allow,
// everyone below deny, agent deny. An action whose cell was WIDER would
// loosen the permission model, which is the thing T0610's ruling refuses (see
// authz/action.go's ActionReopenMainObject); a narrower one would refuse the
// project's own maintainers. It also reads the same way in domain terms:
// docs/13's "Merge controls acceptance" is the rule that an agent's proposal
// becomes the project's state only when a human accepts it, and a
// confirmation is exactly that act for a draft.
//
// The two errors a caller can produce — no membership (projects.ErrMemberNotFound)
// and a project they cannot see (projects.ErrProjectNotFound) — both end in the
// same refusal, ErrDraftNotFound, with the role left nil so the matrix decides
// on the authenticated-non-member column.
func (s *Service) requireConfirm(ctx context.Context, actor domain.User, projectID string) error {
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound), errors.Is(err, projects.ErrProjectNotFound):
		role = nil
	default:
		return fmt.Errorf("%w: resolve membership: %v", ErrStore, err)
	}

	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionMergeMain,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrDraftNotFound
	}
	return nil
}

// checkRefs refuses a ref the search never returned. The comparison is
// element-wise on the record's own selected_refs, and the first stranger is
// named (the caller sent it, so naming it discloses nothing).
//
// It is the write path's half of a guarding pair: migration 00134's
// research_context_draft_refs_guard refuses the same thing for any writer that
// does not come through here. The duplication is deliberate and is the shape
// this repository uses for cross-table invariants (internal/search/answer's
// guard + 00121's CHECK): the Go side refuses early with a sentence the caller
// can act on, the database side refuses late and cannot be bypassed.
func checkRefs(selected, requested []string) error {
	allowed := make(map[string]struct{}, len(selected))
	for _, ref := range selected {
		allowed[ref] = struct{}{}
	}
	for _, ref := range requested {
		if _, ok := allowed[ref]; !ok {
			return &UngroundedRefError{Ref: ref}
		}
	}
	return nil
}

// referenced flattens the three ref sets of a request, in declaration order.
func referenced(in StartInput) []string {
	out := make([]string, 0, len(in.ReferencedRefs)+len(in.DependencyRefs)+len(in.CandidateRefs))
	out = append(out, in.ReferencedRefs...)
	out = append(out, in.DependencyRefs...)
	out = append(out, in.CandidateRefs...)
	return out
}

// clean renders a nil slice as the empty one: the draft's list columns are NOT
// NULL, and "the caller kept nothing" is a real state that must be stored as an
// empty list rather than as an absent value.
func clean(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// questionPayload renders the research_question document the confirmation
// writes. It is the payload shape internal/application/templates.seedMap
// already writes for a seeded question (statement + question_state), which is
// what makes a confirmed draft and a seeded template question the same kind of
// object with the same schema. question_state is "open" because the question
// was just formed and nothing has answered it.
func questionPayload(question string) []byte {
	doc, err := json.Marshal(map[string]any{
		"statement":      question,
		"question_state": "open",
	})
	if err != nil {
		// Unreachable: the map holds two strings. Returning nil would hand
		// the write path an empty payload that fails schema validation,
		// which is a worse answer than an empty document that fails it
		// immediately and visibly.
		return nil
	}
	return doc
}

// purposeFor derives the project's purpose from the draft's research question.
// The start route's contract has no purpose field while project creation
// requires one, and the honest derivation is the question itself: it is what
// the project is for. It is truncated to a length the project surface accepts
// so a long question cannot fail a create for a reason the caller cannot see.
func purposeFor(question string) string {
	const maxPurpose = 500
	if utf8.RuneCountInString(question) <= maxPurpose {
		return question
	}
	runes := []rune(question)
	return string(runes[:maxPurpose])
}

// visibilityOrDefault applies this flow's fail-closed reading of the
// contract's optional `visibility`: an absent value means private. Nothing in
// the platform becomes public by omission (docs/12 §3: widening is never
// automatic), and a caller who wants a public project says so.
func visibilityOrDefault(v domain.ProjectVisibility) domain.ProjectVisibility {
	if v == "" {
		return domain.VisibilityPrivate
	}
	return v
}

// readerFor builds the caller identity the project reads are resolved with.
func readerFor(actor domain.User) projects.Reader {
	return projects.Reader{UserID: actor.ID, Authenticated: true}
}

// validateStart checks the request's shape, not its meaning: the search id and
// key that make the call nameable, and the fields the contract marks required.
// Slug/name/research-question CONTENT rules stay where they are owned — the
// project surface validates the first two, and the research_question schema
// validates the last at confirmation — with the two exceptions the caller
// cannot recover from afterwards: a question too short to ever be confirmable
// (minResearchQuestionLen), and a visibility outside the contract's enum.
func validateStart(actor domain.User, in StartInput) error {
	switch {
	case actor.ID == "":
		return fmt.Errorf("%w: no actor", ErrValidation)
	case strings.TrimSpace(in.SearchID) == "":
		return fmt.Errorf("%w: search_id is required", ErrValidation)
	case len(in.IdempotencyKey) < minIdempotencyKeyLen:
		return fmt.Errorf("%w: Idempotency-Key must be at least %d characters", ErrValidation, minIdempotencyKeyLen)
	case strings.TrimSpace(in.Name) == "":
		return fmt.Errorf("%w: name is required", ErrValidation)
	case strings.TrimSpace(in.Slug) == "":
		return fmt.Errorf("%w: slug is required", ErrValidation)
	case utf8.RuneCountInString(strings.TrimSpace(in.ResearchQuestion)) < minResearchQuestionLen:
		return fmt.Errorf("%w: research_question must be at least %d characters (specs/schemas/research_question.schema.json)", ErrValidation, minResearchQuestionLen)
	case in.Visibility != "" && in.Visibility != domain.VisibilityPublic && in.Visibility != domain.VisibilityPrivate:
		return fmt.Errorf("%w: visibility must be public or private", ErrValidation)
	}
	return nil
}

// validateConfirm checks the confirm request's shape.
func validateConfirm(actor domain.User, in ConfirmInput) error {
	switch {
	case actor.ID == "":
		return fmt.Errorf("%w: no actor", ErrValidation)
	case strings.TrimSpace(in.DraftID) == "":
		return fmt.Errorf("%w: draft_id is required", ErrValidation)
	case len(in.IdempotencyKey) < minIdempotencyKeyLen:
		return fmt.Errorf("%w: Idempotency-Key must be at least %d characters", ErrValidation, minIdempotencyKeyLen)
	}
	return nil
}

// confirmResult renders a confirmed draft as the wire's result, for a fresh
// confirmation (replayed false) or an answer to a repeat (replayed true).
//
// Every id it publishes is the stored one. Nothing is re-derived from the
// project's current state: a replay must answer what the FIRST call answered,
// and a value read now would be a claim about the present dressed as a record
// of the past.
func confirmResult(draft Draft, replayed bool) ConfirmResult {
	out := ConfirmResult{Draft: draft, Replayed: replayed}
	if draft.InitialBranchID != nil {
		out.BranchID = *draft.InitialBranchID
	}
	if draft.InitialCommitID != nil {
		out.StateCommitID = *draft.InitialCommitID
	}
	if draft.InitialStateID != nil {
		out.StateID = *draft.InitialStateID
	}
	if draft.QuestionObjectID != nil {
		out.QuestionObjectID = *draft.QuestionObjectID
	}
	if draft.QuestionVersionID != nil {
		out.QuestionVersionID = *draft.QuestionVersionID
	}
	return out
}
