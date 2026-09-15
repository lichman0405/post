package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ReviewStore is the production reviews.Repository adapter over
// PostgreSQL (sqlc generated queries, pgx).
//
// Submission invariants run in ONE transaction: the PR row is read
// project-scoped and locked (existence, lifecycle and the proposed head
// the review evaluates), the review row is inserted with that head
// pinned — reviewed_state_id is always the PR's current proposed head,
// never a caller-supplied copy (a review is a judgment about one state,
// docs/09 §5) — and the changes_requested projection lands inside the
// same transaction (docs/43: a changes_requested decision moves
// review_required -> changes_requested through the same CAS the PR
// service uses). The row lock serializes the submission against
// concurrent head refreshes and state transitions, so the pinned head
// and the projection can never disagree.
//
// The database guards back every application rule: the kind/decision
// CHECKs (migration 00061), the UNIQUE(pull_request_id, reviewer_id,
// review_kind, reviewed_state_id) — one decision per person per
// dimension per head — and the reviewed_state_id FK. A terminal PR
// (merged/closed/aborted) refuses a review row in the adapter, before
// the insert: a closed proposal's review record is fixed, and the
// release's acceptance record (T0605) must not change after the fact.
type ReviewStore struct {
	pool *pgxpool.Pool
}

// NewReviewStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewReviewStore(pool *pgxpool.Pool) *ReviewStore {
	return &ReviewStore{pool: pool}
}

// SubmitReview implements reviews.Repository.
func (s *ReviewStore) SubmitReview(ctx context.Context, in reviews.SubmitReviewParams) (domain.Review, error) {
	if !domain.ValidReviewKind(in.Kind) || !domain.ValidReviewDecision(in.Decision) ||
		!domain.ValidReviewBody(in.Body) || !domain.ValidReviewResponsibility(in.Responsibility) {
		return domain.Review{}, reviews.ErrValidation
	}
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.Review{}, reviews.ErrValidation
	}
	reviewerID, err := textUUID(in.ReviewerID)
	if err != nil {
		return domain.Review{}, reviews.ErrValidation
	}
	var review domain.Review
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		pr, err := q.GetPullRequestByProjectAndNumberForUpdate(ctx, sqlc.GetPullRequestByProjectAndNumberForUpdateParams{
			ProjectID: projectID,
			Number:    in.Number,
		})
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return pullrequests.ErrPullRequestNotFound
		}
		if err != nil {
			return err
		}
		if domain.IsTerminalPullRequestState(domain.PullRequestState(pr.State)) {
			return &pullrequests.TerminalError{Number: in.Number, State: domain.PullRequestState(pr.State)}
		}
		row, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
			PullRequestID:   pr.ID,
			ReviewerID:      reviewerID,
			ReviewKind:      string(in.Kind),
			Decision:        string(in.Decision),
			ReviewedStateID: pr.ProposedStateID,
			Responsibility:  in.Responsibility,
			Body:            in.Body,
		})
		if err != nil {
			return mapReviewWriteError(err)
		}
		review = reviewFromRow(row)
		// The changes_requested projection (docs/43): a dimension asked
		// for changes, so the PR row moves review_required ->
		// changes_requested — the fact-based half of the review
		// aggregation T0402 left on the SetState surface. The approved
		// half is deliberately absent: deciding WHEN the dimensions add
		// up to an approval is T0604's required-review calculation, not
		// invented here. The row is locked above, so the CAS holds; a
		// missed CAS (the PR is not in review_required — open,
		// changes_requested already, or moved by T0604's future path)
		// is not an error: the review records either way, and the
		// machine state is whatever it is.
		if domain.ReviewDecision(in.Decision) == domain.ReviewDecisionChangesRequested &&
			domain.PullRequestState(pr.State) == domain.PullRequestStateReviewRequired {
			if _, err := q.SetPullRequestState(ctx, sqlc.SetPullRequestStateParams{
				ProjectID: projectID,
				Number:    in.Number,
				Expected:  string(domain.PullRequestStateReviewRequired),
				NextState: string(domain.PullRequestStateChangesRequested),
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, reviews.ErrAlreadyReviewed) ||
			errors.Is(err, reviews.ErrValidation) ||
			errors.Is(err, pullrequests.ErrPullRequestNotFound) ||
			errors.As(err, new(*pullrequests.TerminalError)) {
			return domain.Review{}, err
		}
		return domain.Review{}, fmt.Errorf("persistence: submit review: %w", err)
	}
	return review, nil
}

// ListReviews implements reviews.Repository. An unknown or malformed
// project has no reviews: empty list, not an error (the same read
// discipline as the PR list).
func (s *ReviewStore) ListReviews(ctx context.Context, projectID string, number int64) ([]domain.Review, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListReviewsByPullRequest(ctx, sqlc.ListReviewsByPullRequestParams{
		ProjectID: id,
		Number:    number,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list reviews: %w", err)
	}
	out := make([]domain.Review, 0, len(rows))
	for _, row := range rows {
		out = append(out, reviewFromRow(row))
	}
	return out, nil
}

// mapReviewWriteError turns a failed review insert into the package
// outcomes: the duplicate-decision unique violation is the one expected
// domain conflict (ErrAlreadyReviewed); foreign-key, check and type
// violations on the validated input are validation results; everything
// else is an adapter failure with the cause kept.
func mapReviewWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return reviews.ErrAlreadyReviewed
		case "23503", "23502", "23514", "22P02":
			return reviews.ErrValidation
		}
	}
	if isInvalidText(err) {
		return reviews.ErrValidation
	}
	return err
}

// reviewFromRow converts a sqlc reviews row to the domain value.
func reviewFromRow(row sqlc.Review) domain.Review {
	return domain.Review{
		ID:              pgUUIDToText(row.ID),
		PullRequestID:   pgUUIDToText(row.PullRequestID),
		ReviewerID:      pgUUIDToText(row.ReviewerID),
		Kind:            domain.ReviewKind(row.ReviewKind),
		Decision:        domain.ReviewDecision(row.Decision),
		ReviewedStateID: pgUUIDToText(row.ReviewedStateID),
		Responsibility:  row.Responsibility,
		Body:            row.Body,
		CreatedAt:       row.CreatedAt.Time,
	}
}
