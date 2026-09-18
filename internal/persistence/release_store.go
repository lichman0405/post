package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ReleaseStore is the production releases adapter over PostgreSQL: the
// review-record read the manifest pins (queries/releases_assets.sql,
// T0605) and the release governance write/read surface (T0606) — the
// create transaction, the idempotency ledger and the snapshot-only
// reads. Rows come back grouped by pull request, oldest review first;
// the manifest's canonical ordering is the releases package's rule, not
// the store's.
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
	return listReleaseReviewsFromRows(rows), nil
}

// listReleaseReviewsFromRows groups the flat ListReleaseReviews rows by
// pull request, preserving the query's order (PR number, then review
// time, then row id).
//
// It is a package-level function rather than a loop inside
// ListReleaseReviews because TWO readers now need it and they must not
// disagree: the release gate (ListReleaseReviews, over the pool) and the
// knowledge publication decision (KnowledgePublishStore, over its own
// transaction — internal/application/knowledgepublish requires exactly
// the evidence a release requires, and "this version passed review" has
// to be one read, not two reads that agree today).
func listReleaseReviewsFromRows(rows []sqlc.ListReleaseReviewsRow) []releases.ReviewRecord {
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
	return records
}

// CreateRelease implements releases.ReleaseStorePort. The insert runs in
// one transaction that row-locks the project (GetProjectByIDForUpdate —
// the same lock membership writes take, so concurrent release creates
// for the project serialize) and commits the release row, its
// idempotency ledger entry, its audit row and its release.published
// research event together (docs/53: event + outbox commit with the state
// change). The lock also yields the project's visibility for the event
// row.
//
// A known Idempotency-Key replays instead of inserting: under the
// project lock the ledger read is race-free, and the replay returns the
// first create's row without writing anything (a replay is a read). A
// fresh create whose version already exists answers
// releases.ErrVersionTaken — the UNIQUE(project_id, version) guard.
func (s *ReleaseStore) CreateRelease(ctx context.Context, r domain.Release, audit domain.AuditEntry, idempotencyKey *string) (domain.Release, error) {
	projectID, err := textUUID(r.ProjectID)
	if err != nil {
		return domain.Release{}, releases.ErrProjectNotFound
	}
	stateID, err := textUUID(r.StateID)
	if err != nil {
		return domain.Release{}, fmt.Errorf("persistence: release state id: %w", err)
	}
	createdBy, err := textUUID(r.CreatedBy)
	if err != nil {
		return domain.Release{}, fmt.Errorf("persistence: release creator id: %w", err)
	}
	orgPin, err := optionalUUIDPtr(r.OrgPolicyVersionID)
	if err != nil {
		return domain.Release{}, fmt.Errorf("persistence: release org policy pin: %w", err)
	}
	projectPin, err := optionalUUIDPtr(r.PolicyVersionID)
	if err != nil {
		return domain.Release{}, fmt.Errorf("persistence: release project policy pin: %w", err)
	}
	var created domain.Release
	// One correlation id for the whole create: the request's when the
	// observability middleware (or the auth guard) attached one, else a
	// fresh one — the same fallback the policy service uses, so the audit
	// row, the research event and the outbox row always share one trace
	// id, middleware or not.
	correlationID := ""
	if info, ok := domain.RequestInfoFrom(ctx); ok {
		correlationID = info.CorrelationID
	}
	if correlationID == "" {
		id, err := observability.NewCorrelationID()
		if err != nil {
			return domain.Release{}, fmt.Errorf("persistence: release correlation id: %w", err)
		}
		correlationID = id.String()
	}
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		project, err := q.GetProjectByIDForUpdate(ctx, projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return releases.ErrProjectNotFound
			}
			return err
		}
		if idempotencyKey != nil {
			releaseID, err := q.GetReleaseCreation(ctx, sqlc.GetReleaseCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *idempotencyKey,
			})
			switch {
			case err == nil:
				row, err := q.GetRelease(ctx, sqlc.GetReleaseParams{ProjectID: projectID, ID: releaseID})
				if err != nil {
					return err
				}
				created, err = releaseFromRow(row)
				return err
			case errors.Is(err, pgx.ErrNoRows):
				// first create with this key — fall through to the insert
			default:
				return err
			}
		}
		row, err := q.CreateRelease(ctx, sqlc.CreateReleaseParams{
			ProjectID:          projectID,
			Version:            r.Version,
			Title:              r.Title,
			StateID:            stateID,
			PolicyVersionID:    projectPin,
			OrgPolicyVersionID: orgPin,
			Manifest:           string(r.Manifest),
			ManifestHash:       r.ManifestHash,
			CreatedBy:          createdBy,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" &&
				strings.Contains(pgErr.ConstraintName, "releases_project_id_version") {
				return releases.ErrVersionTaken
			}
			return err
		}
		created, err = releaseFromRow(row)
		if err != nil {
			return err
		}
		if idempotencyKey != nil {
			releaseID, err := textUUID(created.ID)
			if err != nil {
				return err
			}
			if _, err := q.CreateReleaseCreation(ctx, sqlc.CreateReleaseCreationParams{
				ProjectID:      projectID,
				IdempotencyKey: *idempotencyKey,
				ReleaseID:      releaseID,
			}); err != nil {
				return err
			}
		}
		// The audit row names the assigned release id — the store writes
		// it after the insert, so the command left the target ref empty.
		audit.TargetRef = "release:" + created.ID
		audit.CorrelationID = correlationID
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return recordReleasePublished(ctx, q, project.Visibility, createdBy, projectID, correlationID, created)
	})
	if err != nil {
		return domain.Release{}, mapReleaseWriteError(err)
	}
	return created, nil
}

// LookupCreation implements the release command's replay fast path: the
// release an Idempotency-Key already created, or nil when the key has no
// entry yet (the command checks it before resolving any snapshot, so a
// replay never re-runs the gate — the same key returns the first
// create's row forever).
func (s *ReleaseStore) LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*domain.Release, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, releases.ErrProjectNotFound
	}
	q := sqlc.New(s.pool)
	releaseID, err := q.GetReleaseCreation(ctx, sqlc.GetReleaseCreationParams{
		ProjectID:      pID,
		IdempotencyKey: idempotencyKey,
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("persistence: read release creation: %w", err)
	}
	row, err := q.GetRelease(ctx, sqlc.GetReleaseParams{ProjectID: pID, ID: releaseID})
	if err != nil {
		return nil, fmt.Errorf("persistence: read release: %w", err)
	}
	r, err := releaseFromRow(row)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListReleases implements releases.ReleaseStorePort — the project's
// releases, newest first.
func (s *ReleaseStore) ListReleases(ctx context.Context, projectID string) ([]domain.Release, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return nil, releases.ErrProjectNotFound
	}
	rows, err := sqlc.New(s.pool).ListReleases(ctx, pID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list releases: %w", err)
	}
	out := make([]domain.Release, 0, len(rows))
	for _, row := range rows {
		r, err := releaseFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// GetRelease implements releases.ReleaseStorePort: one release of the
// project; a release of another project — or an unknown id — answers
// releases.ErrReleaseNotFound (existence hiding, docs/45).
func (s *ReleaseStore) GetRelease(ctx context.Context, projectID, releaseID string) (domain.Release, error) {
	pID, err := textUUID(projectID)
	if err != nil {
		return domain.Release{}, releases.ErrProjectNotFound
	}
	rID, err := textUUID(releaseID)
	if err != nil {
		return domain.Release{}, releases.ErrReleaseNotFound
	}
	row, err := sqlc.New(s.pool).GetRelease(ctx, sqlc.GetReleaseParams{ProjectID: pID, ID: rID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Release{}, releases.ErrReleaseNotFound
		}
		return domain.Release{}, fmt.Errorf("persistence: read release: %w", err)
	}
	return releaseFromRow(row)
}

// releaseFromRow converts a sqlc releases row to the domain value. The
// manifest is a text column (content-addressed canonical bytes — see
// migration 00053); the store hands the domain the raw document, the
// application renders (and verifies) it. json.RawMessage is zero-copy,
// so the conversion is exact in both directions.
func releaseFromRow(row sqlc.Release) (domain.Release, error) {
	if !row.ID.Valid {
		return domain.Release{}, fmt.Errorf("persistence: release row without id")
	}
	return domain.Release{
		ID:                 pgUUIDToText(row.ID),
		ProjectID:          pgUUIDToText(row.ProjectID),
		Version:            row.Version,
		Title:              row.Title,
		StateID:            pgUUIDToText(row.StateID),
		PolicyVersionID:    uuidPtr(row.PolicyVersionID),
		OrgPolicyVersionID: uuidPtr(row.OrgPolicyVersionID),
		Manifest:           json.RawMessage(row.Manifest),
		ManifestHash:       row.ManifestHash,
		CreatedBy:          pgUUIDToText(row.CreatedBy),
		CreatedAt:          row.CreatedAt.Time,
	}, nil
}

// releaseEventPayload is the payload of the release.published research
// event (specs/events/event-types.yaml): the envelope fields the
// research_events row carries are the columns; payload_version, which
// has no column, travels inside the payload.
type releaseEventPayload struct {
	PayloadVersion int    `json:"payload_version"`
	ReleaseID      string `json:"release_id"`
	Version        string `json:"version"`
	StateID        string `json:"state_id"`
	ManifestHash   string `json:"manifest_hash"`
}

// recordReleasePublished writes the release.published research event and
// its outbox row inside the create transaction (docs/53: the event
// commits with the state change). The event's visibility is the
// project's at create time — the event is as visible as the project that
// carries it. correlationID is the create's trace id (the request's or
// the store's fresh fallback) — the audit row, the event and the outbox
// row share it.
func recordReleasePublished(ctx context.Context, q *sqlc.Queries, visibility string, actorID, projectID pgtype.UUID, correlationID string, release domain.Release) error {
	payload, err := json.Marshal(releaseEventPayload{
		PayloadVersion: 1,
		ReleaseID:      release.ID,
		Version:        release.Version,
		StateID:        release.StateID,
		ManifestHash:   release.ManifestHash,
	})
	if err != nil {
		return fmt.Errorf("persistence: render release event payload: %w", err)
	}
	if _, err := q.RecordResearchEvent(ctx, sqlc.RecordResearchEventParams{
		EventType:     "release.published",
		ActorID:       actorID,
		ProjectID:     projectID,
		Visibility:    visibility,
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: record release event: %w", err)
	}
	if _, err := q.EnqueueOutboxEvent(ctx, sqlc.EnqueueOutboxEventParams{
		EventType:     "release.published",
		Payload:       payload,
		CorrelationID: correlationID,
	}); err != nil {
		return fmt.Errorf("persistence: enqueue release outbox event: %w", err)
	}
	return nil
}

// mapReleaseWriteError keeps the store's own sentinels (the command's
// contract) and wraps everything else with the persistence context.
func mapReleaseWriteError(err error) error {
	if err == nil ||
		errors.Is(err, releases.ErrVersionTaken) ||
		errors.Is(err, releases.ErrProjectNotFound) {
		return err
	}
	return fmt.Errorf("persistence: release write: %w", err)
}
