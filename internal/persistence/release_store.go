package persistence

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ReleaseStore is the production releases.ReleaseReviewPort adapter over
// PostgreSQL: the review/approval record one release manifest pins — the
// reviews of the research PRs targeting main whose proposed states are in
// the released state's lineage (queries/releases_assets.sql). Rows come
// back grouped by pull request, oldest review first; the manifest's
// canonical ordering is the releases package's rule, not the store's.
type ReleaseStore struct {
	pool *pgxpool.Pool
}

// NewReleaseStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewReleaseStore(pool *pgxpool.Pool) *ReleaseStore {
	return &ReleaseStore{pool: pool}
}

// ListReleaseReviews implements releases.ReleaseReviewPort. An unknown
// state or main branch (a UUID that names no row) yields an empty record,
// not an error — the caller's state read already reported not-found, and
// a state without merged PRs legitimately has no review record (the
// release gate then refuses, as it should). A malformed id is an error,
// never a silent empty record.
func (s *ReleaseStore) ListReleaseReviews(ctx context.Context, stateID, mainBranchID string) ([]releases.ReviewRecord, error) {
	stateUUID, err := textUUID(stateID)
	if err != nil {
		return nil, fmt.Errorf("persistence: invalid state id %q: %w", stateID, err)
	}
	mainUUID, err := textUUID(mainBranchID)
	if err != nil {
		return nil, fmt.Errorf("persistence: invalid main branch id %q: %w", mainBranchID, err)
	}
	rows, err := sqlc.New(s.pool).ListReleaseReviews(ctx, sqlc.ListReleaseReviewsParams{
		MainBranchID: mainUUID,
		StateID:      stateUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list release reviews: %w", err)
	}
	// Group the flat rows by pull request, preserving the query's order
	// (PR number, then review time, then row id).
	records := make([]releases.ReviewRecord, 0, len(rows))
	for _, row := range rows {
		proposed := pgUUIDToText(row.ProposedStateID)
		n := len(records)
		if n == 0 || records[n-1].PullRequestNumber != row.PullRequestNumber || records[n-1].ProposedStateID != proposed {
			records = append(records, releases.ReviewRecord{
				PullRequestNumber: row.PullRequestNumber,
				ProposedStateID:   proposed,
				Reviews:           []releases.Review{},
			})
			n = len(records)
		}
		records[n-1].Reviews = append(records[n-1].Reviews, releases.Review{
			ID:         pgUUIDToText(row.ID),
			ReviewerID: pgUUIDToText(row.ReviewerID),
			ReviewKind: row.ReviewKind,
			Decision:   row.Decision,
			Body:       row.Body,
			CreatedAt:  row.CreatedAt.Time,
		})
	}
	return records, nil
}
