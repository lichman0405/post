package pullrequests

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the pull-request use cases against the Repository
// port. It owns input validation (the port trusts, the service verifies)
// and the docs/43 transition map; it does NOT authorize — creation and
// lifecycle moves are project-governance actions whose membership/role
// checks belong to the consuming API task, which passes resolved
// identities in (package doc).
//
// The base state is fixed at creation and never re-derived: there is no
// service surface for it either (acceptance "PR base 不随 main 漂移").
// The proposed head moves only through RefreshProposed (acceptance
// "head update 可显式 refresh").
type Service struct {
	repo Repository
}

// NewService wires the service.
func NewService(repo Repository) *Service {
	return &Service{repo: repo}
}

// Create opens a new PR: validate the shape, hand the project-scoped
// atomic insert to the adapter. The returned PR carries its per-project
// number, its fixed base (the target branch's head at creation) and its
// proposed head (the source branch's head at creation), state open.
func (s *Service) Create(ctx context.Context, in CreatePullRequestParams) (domain.PullRequest, error) {
	if err := validateCreate(in); err != nil {
		return domain.PullRequest{}, err
	}
	pr, err := s.repo.CreatePullRequest(ctx, in)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// Get returns one PR of the project by its number, or
// ErrPullRequestNotFound (also for a PR of another project — never leak
// a foreign entity's existence).
func (s *Service) Get(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	if err := validateRef(projectID, number); err != nil {
		return domain.PullRequest{}, err
	}
	pr, err := s.repo.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// GetByCreationKey returns the project's PR that an earlier creation
// request carrying creationKey opened, or ErrPullRequestNotFound when the
// key names nothing (no such key, or the empty key, which names nothing
// by construction — migration 00089). It is the replay read a creation
// command needs: a repeated request finds the proposal the first attempt
// opened, including one that has since merged, instead of opening a
// second. The caller owns the decision to replay — this method only
// answers what the key names.
func (s *Service) GetByCreationKey(ctx context.Context, projectID, creationKey string) (domain.PullRequest, error) {
	if projectID == "" {
		return domain.PullRequest{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if creationKey == "" {
		return domain.PullRequest{}, fmt.Errorf("%w: creation key is required", ErrValidation)
	}
	pr, err := s.repo.GetPullRequestByCreationKey(ctx, projectID, creationKey)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// List returns every PR of the project, in number order.
func (s *Service) List(ctx context.Context, projectID string) ([]domain.PullRequest, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	prs, err := s.repo.ListPullRequests(ctx, projectID)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return prs, nil
}

// RequestReview moves the PR into review (docs/43: open →
// review_required, and the review loop's re-entry changes_requested →
// review_required after the author updated the head).
//
// This is the command's keyless form — the shape an internal caller (a
// fixture, a projection, an operator fixing a stuck proposal) uses, and
// the shape it has had since T0402. The product route always carries the
// contract's Idempotency-Key and calls RequestReviewKeyed.
func (s *Service) RequestReview(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	return s.RequestReviewKeyed(ctx, projectID, number, "")
}

// RequestReviewKeyed is RequestReview with the Idempotency-Key the
// contract route carried (specs/api/openapi.yaml,
// components.parameters.IdempotencyKey: required, minLength 8 on POST
// .../{prId}:request-review).
//
// The key is OPTIONAL on the command and required on the route, which is
// the same split the creation command makes for its CreationKey: an
// internal caller names none, and a key that IS sent has to be a usable
// one — below the contract's minLength it names nothing a retry could be
// matched against. Where the creation key is a row lookup (migration
// 00089), this key is recorded on the audit row and otherwise unused: the
// state is the idempotency record, so a repeated request is answered by
// the PR already being in review_required
// (internal/persistence/pullrequest_store.go, and the freeze's identical
// note in internal/application/mainfreeze/doc.go).
func (s *Service) RequestReviewKeyed(ctx context.Context, projectID string, number int64, idempotencyKey string) (domain.PullRequest, error) {
	if err := validateRef(projectID, number); err != nil {
		return domain.PullRequest{}, err
	}
	if idempotencyKey != "" && len(idempotencyKey) < MinCreationKeyLen {
		return domain.PullRequest{}, fmt.Errorf(
			"%w: Idempotency-Key must be at least %d characters (specs/api/openapi.yaml)",
			ErrValidation, MinCreationKeyLen)
	}
	pr, err := s.repo.RequestReview(ctx, projectID, number, idempotencyKey)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// Close withdraws the PR without merging (docs/43: closed is terminal,
// reachable from any non-terminal state).
func (s *Service) Close(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	return s.setState(ctx, projectID, number, domain.PullRequestStateClosed)
}

// Abort invalidates the PR without merging (docs/43: aborted is
// terminal, reachable from any non-terminal state).
func (s *Service) Abort(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	return s.setState(ctx, projectID, number, domain.PullRequestStateAborted)
}

// MarkMerged closes the PR as merged (docs/43: merge_ready → merged,
// terminal): its proposal became part of the accepted state. The
// semantic merge itself is T0406's engine; this is the lifecycle
// transition its saga lands.
func (s *Service) MarkMerged(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	return s.setState(ctx, projectID, number, domain.PullRequestStateMerged)
}

// SetState drives a review-projection transition the flows above do not
// name (changes_requested, approved, merge_ready — docs/43): the
// transition map is checked against the PR's current state and the move
// runs as a compare-and-swap, so a concurrent transition fails with
// *StateConflictError instead of overwriting. The review aggregation
// that decides WHEN to move is T0404's (and T0403's) business — this is
// the surface it lands on.
func (s *Service) SetState(ctx context.Context, projectID string, number int64, to domain.PullRequestState) (domain.PullRequest, error) {
	if err := validateRef(projectID, number); err != nil {
		return domain.PullRequest{}, err
	}
	if !domain.ValidPullRequestState(to) || to == domain.PullRequestStateOpen {
		return domain.PullRequest{}, fmt.Errorf("%w: %q is not a pull request transition target (docs/43)", ErrValidation, to)
	}
	current, err := s.repo.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	if !domain.CanTransitionPullRequestState(current.State, to) {
		return domain.PullRequest{}, &TransitionError{Number: number, From: current.State, To: to}
	}
	pr, err := s.repo.SetPullRequestState(ctx, projectID, number, current.State, to)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// RefreshProposed re-pins the PR's proposed state to the source branch's
// CURRENT head — the explicit head refresh. It is the only path that
// moves the proposed state (migration 00051's fixity guard); a terminal
// PR (merged/closed/aborted) refuses it (*TerminalError, docs/43:
// terminal PRs never move).
func (s *Service) RefreshProposed(ctx context.Context, projectID string, number int64) (domain.PullRequest, error) {
	if err := validateRef(projectID, number); err != nil {
		return domain.PullRequest{}, err
	}
	pr, err := s.repo.RefreshProposedState(ctx, projectID, number)
	if err != nil {
		return domain.PullRequest{}, wrapStoreError(err)
	}
	return pr, nil
}

// setState runs one named transition through SetState (the map check and
// the CAS are shared).
func (s *Service) setState(ctx context.Context, projectID string, number int64, to domain.PullRequestState) (domain.PullRequest, error) {
	return s.SetState(ctx, projectID, number, to)
}

// validateRef checks the (project, number) address shape.
func validateRef(projectID string, number int64) error {
	if projectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	return nil
}

// validateCreate checks the creation request's shape: identities present,
// the branch pair distinct, title and body in the domain shape. The
// branches' existence in the project and their lifecycles are checked by
// the adapter, which reads the branch rows inside the insert transaction
// — the service cannot verify them without duplicating the read.
func validateCreate(in CreatePullRequestParams) error {
	if in.ProjectID == "" {
		return fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if in.SourceBranchID == "" || in.TargetBranchID == "" {
		return fmt.Errorf("%w: source_branch_id and target_branch_id are required", ErrValidation)
	}
	if in.SourceBranchID == in.TargetBranchID {
		return fmt.Errorf("%w: a pull request proposes a diff between two branches — source and target must differ", ErrValidation)
	}
	if !domain.ValidPullRequestTitle(in.Title) {
		return fmt.Errorf("%w: title is blank or oversized", ErrValidation)
	}
	if !domain.ValidPullRequestBody(in.Body) {
		return fmt.Errorf("%w: body is oversized", ErrValidation)
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("%w: created_by is required", ErrValidation)
	}
	// The Idempotency-Key is optional on the command (an internal caller
	// opening a proposal by hand sends none), but a key that IS sent has
	// to be a usable one: below the contract's minLength it names nothing
	// a retry could be matched against, and storing it would let a client
	// believe a retry is safe when the row it would collide with is not
	// the one it meant.
	if in.CreationKey != "" && len(in.CreationKey) < MinCreationKeyLen {
		return fmt.Errorf("%w: creation_key must be at least %d characters (specs/api/openapi.yaml IdempotencyKey)",
			ErrValidation, MinCreationKeyLen)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (missing PR/branch,
// closed branch, missing head, unstructured source content, invalid
// transition, CAS conflict, terminal, project missing, validation) and
// turns everything else — including an adapter that cannot run — into
// ErrStore for the handler, with the cause kept for the log.
//
// ErrBranchUnstructuredChanges is one of the kept outcomes because it is a
// RULE's refusal, not an outage: the source branch's recorded semantic
// state is unstructured_changes (00042) and the answer belongs to the
// caller, who can act on it — 409 on the wire, not "service unavailable".
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrPullRequestNotFound) ||
		errors.Is(err, ErrBranchNotFound) ||
		errors.Is(err, ErrBranchNotActive) ||
		errors.Is(err, ErrBranchHeadMissing) ||
		errors.Is(err, ErrBranchUnstructuredChanges) ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.As(err, new(*TransitionError)) ||
		errors.As(err, new(*StateConflictError)) ||
		errors.As(err, new(*TerminalError)) ||
		errors.As(err, new(*BranchNotActiveError)) {
		return err
	}
	// Both verbs are %w: the sentence is unchanged, but the database's own
	// error stays in the chain. The insert this wraps is the one 00042's
	// pull_request_semantic_gate and 00086's pull_request_fork_gate refuse
	// with P0001, and a caller given only "store failure" cannot tell a
	// rule refusal from an outage.
	return fmt.Errorf("%w: %w", ErrStore, err)
}
