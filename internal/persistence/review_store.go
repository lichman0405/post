package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/reviews"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// eventPullRequestReviewed is the canonical event every review submission
// produces (specs/events/event-types.yaml: `pull_request.reviewed`). It is
// written into the transactional outbox inside the submission's own
// transaction (docs/53): the record of the judgment and the judgment
// itself commit together or not at all. The payload carries
// identity/reference only — the docs/52「Workers」口径 the merge and RSG
// events follow — never review prose.
const eventPullRequestReviewed = "pull_request.reviewed"

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
//
// T0604 lands the OTHER half of the projection in the same transaction:
// the required-review calculation the service resolved for this head is
// evaluated against the reviews recorded at that head, and when it is
// satisfied the PR advances review_required -> approved -> merge_ready
// through the same compare-and-swap the PR service uses (docs/43 — and
// never bypassing merge_ready, the only state T0406's merge service
// accepts) with one audit row and one event recording the advance. The
// row lock serializes concurrent submissions, so two approvals arriving
// together advance the PR exactly once: the loser of the lock reads the
// state the winner committed and finds nothing left to advance.
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
		// stateBefore is the state this submission found the PR in;
		// stateAfter is the state it has reached so far. Both travel to the
		// event below: state_before/state_after report what the submission
		// spanned, and state_after is what a subscriber reads to tell a
		// submission that merely recorded from one that advanced.
		stateBefore := domain.PullRequestState(pr.State)
		stateAfter := stateBefore
		// The changes_requested projection (docs/43): a dimension asked
		// for changes, so the PR row moves review_required ->
		// changes_requested — the fact-based half of the review
		// aggregation T0402 left on the SetState surface. The row is
		// locked above, so the CAS holds; a missed CAS (the PR is not in
		// review_required — open, changes_requested already, or already
		// advanced by the approved half below) is not an error: the
		// review records either way, and the machine state is whatever it
		// is. A move that lands is part of what this submission did, so it
		// becomes the state the submission reports and the state the
		// approved half starts from.
		if domain.ReviewDecision(in.Decision) == domain.ReviewDecisionChangesRequested &&
			stateAfter == domain.PullRequestStateReviewRequired {
			if _, err := q.SetPullRequestState(ctx, sqlc.SetPullRequestStateParams{
				ProjectID: projectID,
				Number:    in.Number,
				Expected:  string(domain.PullRequestStateReviewRequired),
				NextState: string(domain.PullRequestStateChangesRequested),
			}); err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return err
				}
			} else {
				stateAfter = domain.PullRequestStateChangesRequested
			}
		}
		// The approved half (T0604): the required-review calculation,
		// evaluated against the reviews recorded at the head it is about.
		if err := s.projectApproval(ctx, tx, q, pr, review, in.Requirement, stateBefore, stateAfter); err != nil {
			return err
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

// projectApproval evaluates the required-review calculation against the
// reviews recorded for the proposal's current head and advances the PR
// when the calculation is satisfied. It runs inside the submission
// transaction, with the PR row already locked.
//
// stateBefore is the state the submission found the PR in, stateAfter the
// state it has reached so far — the changes_requested projection may
// already have moved it, and that move is this submission's. Both are
// reported in the event payload.
//
// Three conditions gate the advance, and all three are fail-closed:
//
//   - the calculation must be ABOUT the head the submission landed on
//     (RequiredReviews.HeadStateID == pr.proposed_state_id). A
//     calculation computed before the head moved describes a proposal
//     that is no longer on the table: the review records (it is a judgment
//     about the current head, honestly pinned to it) and nothing advances,
//     because an advance decided by a stale calculation would approve
//     changes nobody reviewed.
//   - the calculation must be satisfied (domain.EvaluateRequiredReviews):
//     every routed responsibility's approval present, the release minimum
//     reached, every change routed. An unrouted change or an empty
//     requirement set never satisfies — missing configuration refuses.
//   - the PR must be in review_required as the submission LEAVES it. The
//     compare-and-swap decides it: docs/43's machine is review_required ->
//     approved -> merge_ready, and a PR in any other state (open,
//     changes_requested — including one this very submission just moved
//     there — or already merge_ready) is not moved by a review.
//
// When the advance happens, the audit row and the domain event are written
// in the SAME transaction as the state move (docs/26, docs/53): a
// merge_ready PR always has the record of when and under whose review the
// calculation was satisfied.
//
// The event is written for EVERY submission (the canonical
// pull_request.reviewed), not only for the advance: the submission is
// itself the research event. It is written last, after every move the
// submission makes, so its payload carries the state the submission
// actually left behind — and no branch below returns before it.
func (s *ReviewStore) projectApproval(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, pr sqlc.PullRequest, review domain.Review, required domain.RequiredReviews, stateBefore, stateAfter domain.PullRequestState) error {
	advanced := false
	if required.HeadStateID != "" && required.HeadStateID == pgUUIDToText(pr.ProposedStateID) {
		rows, err := q.ListReviewsByPullRequest(ctx, sqlc.ListReviewsByPullRequestParams{
			ProjectID: pr.ProjectID,
			Number:    pr.Number,
		})
		if err != nil {
			return err
		}
		recorded := make([]domain.Review, 0, len(rows))
		for _, row := range rows {
			recorded = append(recorded, reviewFromRow(row))
		}
		progress := domain.EvaluateRequiredReviews(required, recorded)
		if progress.Satisfied && stateAfter == domain.PullRequestStateReviewRequired {
			for _, next := range []domain.PullRequestState{domain.PullRequestStateApproved, domain.PullRequestStateMergeReady} {
				_, casErr := q.SetPullRequestState(ctx, sqlc.SetPullRequestStateParams{
					ProjectID: pr.ProjectID,
					Number:    pr.Number,
					Expected:  string(stateAfter),
					NextState: string(next),
				})
				if errors.Is(casErr, pgx.ErrNoRows) {
					// The CAS lost. The row is locked by this transaction,
					// so nothing else moved it: the state is not the
					// review_required the calculation was gated on, and no
					// advance happens. The submission still falls through to
					// its own event rather than swallowing it — a recorded
					// judgment whose event is missing is a hole in the
					// research record.
					break
				}
				if casErr != nil {
					return casErr
				}
				stateAfter = next
			}
			if stateAfter == domain.PullRequestStateMergeReady {
				advanced = true
				if err := s.recordReviewCompleted(ctx, q, pr, review, required, progress, stateAfter); err != nil {
					return err
				}
			}
		}
	}
	return s.recordReviewedEvent(ctx, tx, q, pr, review, stateBefore, stateAfter, advanced)
}

// recordReviewCompleted writes the governance record of an advance: the
// audit row naming the PR, the reviews that satisfied the calculation and
// the state it moved to. It commits with the state move.
func (s *ReviewStore) recordReviewCompleted(ctx context.Context, q *sqlc.Queries, pr sqlc.PullRequest, review domain.Review, required domain.RequiredReviews, progress domain.ReviewProgress, stateAfter domain.PullRequestState) error {
	missing := make([]string, 0, len(progress.Missing))
	for _, req := range progress.Missing {
		missing = append(missing, req.String())
	}
	requirements := make([]string, 0, len(required.Requirements))
	for _, req := range required.Requirements {
		requirements = append(requirements, req.String())
	}
	return appendAudit(ctx, q, domain.AuditEntry{
		Action:        domain.ActionPullRequestReviewCompleted,
		ProjectID:     pgUUIDToText(pr.ProjectID),
		BeforeSummary: map[string]any{"state": string(domain.PullRequestStateReviewRequired)},
		AfterSummary: map[string]any{
			"pull_request_id":        pgUUIDToText(pr.ID),
			"pull_request_number":    pr.Number,
			"state":                  string(stateAfter),
			"approvals":              progress.Approvals,
			"required_approvals":     progress.RequiredApprovals,
			"requirements":           requirements,
			"routed_labels":          required.RoutedLabels,
			"release_merge":          required.ReleaseMerge,
			"public_asset_ip_review": required.PublicAssetIPReview,
		},
		Metadata: map[string]any{
			"review_id":        review.ID,
			"review_kind":      string(review.Kind),
			"decision":         string(review.Decision),
			"responsibility":   review.Responsibility,
			"missing_required": missing,
		},
	})
}

// recordReviewedEvent writes pull_request.reviewed into the outbox inside
// the submission transaction. Visibility is the TARGET BRANCH's (the
// subject the review is about, docs/12 §3: an event is never more visible
// than its subject), read here because the branch row is the only place
// the value lives; anything but the canonical public value falls back to
// private (fail closed — the event never guesses).
func (s *ReviewStore) recordReviewedEvent(ctx context.Context, tx pgx.Tx, q *sqlc.Queries, pr sqlc.PullRequest, review domain.Review, stateBefore, stateAfter domain.PullRequestState, advanced bool) error {
	branch, err := q.GetBranchByID(ctx, pr.TargetBranchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("persistence: record review event: the PR's target branch does not exist")
	}
	if err != nil {
		return err
	}
	payload, err := json.Marshal(reviewEventPayload{
		PayloadVersion:    1,
		ReviewID:          review.ID,
		PullRequestID:     pgUUIDToText(pr.ID),
		PullRequestNumber: pr.Number,
		ReviewKind:        string(review.Kind),
		Decision:          string(review.Decision),
		ReviewedStateID:   review.ReviewedStateID,
		Responsibility:    review.Responsibility,
		StateBefore:       string(stateBefore),
		StateAfter:        string(stateAfter),
		Advanced:          advanced,
	})
	if err != nil {
		return fmt.Errorf("persistence: render review event payload: %w", err)
	}
	if err := events.Record(ctx, tx, events.Event{
		EventType:  eventPullRequestReviewed,
		ActorID:    review.ReviewerID,
		ProjectID:  pgUUIDToText(pr.ProjectID),
		Visibility: reviewEventVisibility(branch.Visibility),
		Payload:    payload,
	}); err != nil {
		return fmt.Errorf("persistence: record review event: %w", err)
	}
	return nil
}

// reviewEventPayload is the payload of pull_request.reviewed: identity and
// reference only (never the review prose), plus the state the submission
// left behind, so a subscriber can tell an approval that made the proposal
// mergeable from one that merely recorded.
type reviewEventPayload struct {
	PayloadVersion    int    `json:"payload_version"`
	ReviewID          string `json:"review_id"`
	PullRequestID     string `json:"pull_request_id"`
	PullRequestNumber int64  `json:"pull_request_number"`
	ReviewKind        string `json:"review_kind"`
	Decision          string `json:"decision"`
	ReviewedStateID   string `json:"reviewed_state_id"`
	Responsibility    string `json:"responsibility"`
	StateBefore       string `json:"state_before"`
	StateAfter        string `json:"state_after"`
	Advanced          bool   `json:"advanced"`
}

// reviewEventVisibility maps a branch's visibility to the event vocabulary
// (docs/12 §3: an event is never more visible than its subject). Anything
// but the canonical public value falls back to private — fail closed, the
// same rule the merge and freeze stores apply.
func reviewEventVisibility(v string) string {
	if v == string(domain.BranchVisibilityPublic) {
		return events.VisibilityPublic
	}
	return events.VisibilityPrivate
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
