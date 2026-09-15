package reviews

import (
	"context"
	"errors"
	"fmt"
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
// through the reviewer-responsibility hook (acceptance "权限正确") — and
// it lands the fact-based PR projection (a changes_requested decision
// moves review_required -> changes_requested through T0402's SetState
// surface).
//
// It does NOT decide when the PR is approved: that needs the
// required-review calculation (T0604: which responsibilities/kinds are
// required for which changes), which is deliberately not invented here.
type Service struct {
	repo           Repository
	projects       ProjectGate
	authz          authz.Engine
	responsibility ResponsibilityGate
}

// NewService wires the service.
func NewService(deps Deps) *Service {
	return &Service{
		repo:           deps.Repo,
		projects:       deps.Projects,
		authz:          deps.Authz,
		responsibility: deps.Responsibility,
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
	responsibility, err := s.authorize(ctx, actor, projectID)
	if err != nil {
		return domain.Review{}, err
	}
	review, err := s.repo.SubmitReview(ctx, SubmitReviewParams{
		ProjectID:      projectID,
		Number:         number,
		ReviewerID:     actor.ID,
		Kind:           in.Kind,
		Decision:       in.Decision,
		Responsibility: responsibility,
		Body:           in.Body,
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
// responsibility label to record. It fails closed on every unresolved
// shape (docs/12).
func (s *Service) authorize(ctx context.Context, actor domain.User, projectID string) (string, error) {
	if s.authz == nil {
		return "", fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	role, err := s.projectRole(ctx, actor, projectID)
	if err != nil {
		return "", err
	}
	decision, err := s.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionSubmitScientificReview,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		// Unconditional allow (maintainer/owner): the hook is consulted
		// only to fill the label; an error here is metadata loss, not a
		// permission question.
		if s.responsibility != nil {
			if label, err := s.responsibility.ReviewResponsibility(ctx, projectID, actor.ID); err == nil {
				return strings.TrimSpace(label), nil
			}
		}
		return "", nil
	case decision.Verdict == authz.VerdictConditional:
		// The reviewer-responsibility hook resolves the condition; an
		// absent hook, a hook error, or no resolved label is refusal.
		if s.responsibility == nil {
			return "", ErrForbidden
		}
		label, err := s.responsibility.ReviewResponsibility(ctx, projectID, actor.ID)
		if err != nil {
			return "", fmt.Errorf("%w: reviewer responsibility unknowable: %v", ErrStore, err)
		}
		label = strings.TrimSpace(label)
		if label == "" {
			return "", ErrForbidden
		}
		return label, nil
	default:
		// Deny, and every verdict this site does not resolve (agents:
		// proposal_or_scoped — no scoped review resolution in V1).
		return "", ErrForbidden
	}
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
