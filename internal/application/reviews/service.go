package reviews

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the per-dimension review use cases (docs/09 §5:
// scientific and integrity reviews are recorded per dimension, never
// flattened into one approve; docs/43: "Scientific/Integrity review 各自
// 状态"). It owns input validation and the authorization — the matrix
// row submit_scientific_review with the conditional verdict resolved
// through the reviewer-responsibility resolver (acceptance "权限正确") —
// and it carries the required-review calculation (T0604) into the
// adapter, which lands BOTH halves of the fact-based projection: a
// changes_requested decision moves review_required -> changes_requested,
// and a satisfied calculation moves review_required -> approved ->
// merge_ready (docs/43; never bypassing merge_ready, which is the only
// state T0406's merge service accepts).
//
// The calculation itself is not made here: it is the project's routing
// data and policy (domain.BuildRequiredReviews), resolved by the Routing
// port. This service only decides which responsibility label the review
// is recorded under, so the recorded label matches the rule that routed
// the change.
type Service struct {
	repo           Repository
	projects       ProjectGate
	authz          authz.Engine
	responsibility ResponsibilityGate
	routing        Routing
}

// NewService wires the service.
func NewService(deps Deps) *Service {
	return &Service{
		repo:           deps.Repo,
		projects:       deps.Projects,
		authz:          deps.Authz,
		responsibility: deps.Responsibility,
		routing:        deps.Routing,
	}
}

// SubmitReview records one review decision for the PR. The authorization
// runs BEFORE any PR lookup: a denied caller learns nothing about the PR
// (docs/45, the same discipline as the RSG write path).
//
// Permission shape (specs/policies/permissions-matrix.csv):
//
//   - maintainer/owner: allow — the review records with the hook's label
//     when one resolves (best-effort metadata, not a permission);
//   - viewer/contributor/authenticated non-member: conditional — allowed
//     only while the reviewer-responsibility hook resolves a label;
//     without a hook, or with no label, or on a hook error, refused
//     (default deny);
//   - everything else (anonymous actors, agents, unknown classes):
//     refused.
func (s *Service) SubmitReview(ctx context.Context, actor domain.User, projectID string, number int64, in SubmitReviewInput) (domain.Review, error) {
	if err := validateInput(in); err != nil {
		return domain.Review{}, err
	}
	if actor.ID == "" {
		return domain.Review{}, fmt.Errorf("%w: reviewer identity is required", ErrValidation)
	}
	labels, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return domain.Review{}, err
	}
	requirement := s.requirementFor(ctx, projectID, number)
	review, err := s.repo.SubmitReview(ctx, SubmitReviewParams{
		ProjectID:      projectID,
		Number:         number,
		ReviewerID:     actor.ID,
		Kind:           in.Kind,
		Decision:       in.Decision,
		Responsibility: pickResponsibility(labels, requirement),
		Body:           in.Body,
		Requirement:    requirement,
	})
	if err != nil {
		return domain.Review{}, wrapStoreError(err)
	}
	return review, nil
}

// List returns every review of the PR, oldest first. Like
// pullrequests.List this is a service-level read: the consuming read
// path runs the project visibility gate (docs/45) exactly as the RSG
// reads do.
func (s *Service) List(ctx context.Context, projectID string, number int64) ([]domain.Review, error) {
	if projectID == "" {
		return nil, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return nil, fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	reviews, err := s.repo.ListReviews(ctx, projectID, number)
	if err != nil {
		return nil, wrapStoreError(err)
	}
	return reviews, nil
}

// authorize resolves the matrix verdict for the actor and returns the
// responsibility labels the actor holds (the label to record is chosen
// from them against the proposal's routing). It fails closed on every
// unresolved shape (docs/12).
func (s *Service) authorize(ctx context.Context, actor domain.User, projectID string) ([]string, error) {
	if s.authz == nil {
		return nil, fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	role, err := s.projectRole(ctx, actor, projectID)
	if err != nil {
		return nil, err
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionSubmitScientificReview,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		// Unconditional allow (maintainer/owner): the resolver is
		// consulted only to fill the label; an error here is metadata
		// loss, not a permission question — the verdict already stands.
		if s.responsibility != nil {
			if labels, err := s.responsibility.Responsibilities(ctx, actor, projectID); err == nil {
				return normalizeLabels(labels), nil
			}
		}
		return nil, nil
	case decision.Verdict == authz.VerdictConditional:
		// The reviewer-responsibility resolver resolves the condition; an
		// absent resolver, a resolver error, or no held label is refusal.
		if s.responsibility == nil {
			return nil, ErrForbidden
		}
		labels, err := s.responsibility.Responsibilities(ctx, actor, projectID)
		if err != nil {
			return nil, fmt.Errorf("%w: reviewer responsibility unknowable: %v", ErrStore, err)
		}
		labels = normalizeLabels(labels)
		if len(labels) == 0 {
			return nil, ErrForbidden
		}
		return labels, nil
	default:
		// Deny, and every verdict this site does not resolve (agents:
		// proposal_or_scoped — no scoped review resolution in V1).
		return nil, ErrForbidden
	}
}

// requirementFor resolves the proposal's required-review calculation. It is
// deliberately forgiving about the CALCULATION and unforgiving about what
// the calculation decides: a missing or failing routing yields the zero
// calculation (no head, no requirements), which EvaluateRequiredReviews
// never reports as satisfied — the review records either way (a human
// judgment is a fact about the head, whatever the project's configuration
// says), and the state advance, which the calculation decides, does not
// happen. An unknown requirement set is never an empty one.
func (s *Service) requirementFor(ctx context.Context, projectID string, number int64) domain.RequiredReviews {
	if s.routing == nil {
		return domain.RequiredReviews{}
	}
	required, err := s.routing.RequiredReviews(ctx, projectID, number)
	if err != nil {
		return domain.RequiredReviews{}
	}
	return required
}

// pickResponsibility chooses the label the review is recorded under: the
// first label the reviewer holds that the proposal's routing actually
// asked for, so reviews.responsibility names the responsibility the
// resolved rule routed the change to; otherwise the reviewer's first held
// label (attribution is a fact about the reviewer, and it stays truthful
// even when the routing asked for something else). A reviewer who holds
// nothing records no label — the empty value is what a review with no
// responsibility looks like, and it satisfies no routed requirement.
func pickResponsibility(labels []string, required domain.RequiredReviews) string {
	if len(labels) == 0 {
		return ""
	}
	routed := make(map[string]bool, len(required.RoutedLabels))
	for _, label := range required.RoutedLabels {
		routed[label] = true
	}
	for _, label := range labels {
		if routed[label] {
			return label
		}
	}
	return labels[0]
}

// normalizeLabels trims, drops blanks and deduplicates the resolver's
// answer, in a deterministic (sorted) order — the order pickResponsibility
// walks, so the recorded label never depends on the store's row order.
func normalizeLabels(labels []string) []string {
	seen := make(map[string]bool, len(labels))
	out := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		out = append(out, label)
	}
	sort.Strings(out)
	return out
}

// projectRole resolves the actor's membership role in the project — the
// authz class input. A caller who may not read the project gets
// projects.ErrProjectNotFound (the existence-hiding 404) before any role
// is decided; a readable project without a membership answers "no role",
// which ClassOf resolves to the authenticated_nonmember matrix column.
func (s *Service) projectRole(ctx context.Context, actor domain.User, projectID string) (*domain.ProjectRole, error) {
	if s.projects == nil {
		return nil, fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	membership, err := s.projects.GetMembership(ctx, actor, projectID)
	switch {
	case err == nil:
		role := membership.Role
		return &role, nil
	case errors.Is(err, projects.ErrMemberNotFound):
		return nil, nil
	default:
		return nil, wrapStoreError(err)
	}
}

// SubmitReviewInput carries one review submission. Kind and Decision use
// the documented dimension and decision vocabularies; the reviewer's
// identity and the responsibility label are derived server-side (the
// actor and the hook), never taken from the client.
type SubmitReviewInput struct {
	// Kind is the review dimension: scientific or integrity.
	Kind domain.ReviewKind
	// Decision is the reviewer's verdict about the PR's current
	// proposed head.
	Decision domain.ReviewDecision
	// Body carries the free-form review reasoning; empty when none.
	Body string
}

// validateInput checks the submission's shape: the dimension and the
// decision are the documented vocabularies, and the body fits the domain
// bound.
func validateInput(in SubmitReviewInput) error {
	if !domain.ValidReviewKind(in.Kind) {
		return fmt.Errorf("%w: review kind must be scientific or integrity (docs/09 §5)", ErrValidation)
	}
	if !domain.ValidReviewDecision(in.Decision) {
		return fmt.Errorf("%w: review decision must be approved, changes_requested or comment", ErrValidation)
	}
	if !domain.ValidReviewBody(in.Body) {
		return fmt.Errorf("%w: body is oversized", ErrValidation)
	}
	return nil
}

// wrapStoreError keeps the expected domain outcomes (missing PR,
// terminal PR, duplicate decision, validation, forbidden, project
// missing) and turns everything else — including an adapter or gate that
// cannot run — into ErrStore for the handler, with the cause kept for
// the log.
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrValidation) ||
		errors.Is(err, ErrForbidden) ||
		errors.Is(err, ErrAlreadyReviewed) ||
		errors.Is(err, ErrStore) ||
		errors.Is(err, pullrequests.ErrPullRequestNotFound) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.As(err, new(*pullrequests.TerminalError)) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
