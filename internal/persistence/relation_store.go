package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// RelationStore is the production relations.Repository adapter over
// PostgreSQL (sqlc generated queries, pgx). The version log is append-only
// on two layers at once: the port exposes no update path, and the database
// rejects any UPDATE/DELETE of a version row itself (migrations
// 00014/00015). Version creation is an atomic compare-and-swap on
// relations.current_version_no (migration 00025, T0203): the counter
// advances from expected to expected+1 only while it still equals
// expected, so concurrent writers serialize into exactly one winner and
// stable EXPECTED_VERSION_MISMATCH losers — no unique-violation races.
// Endpoints are pinned to scientific object versions by foreign key: a
// write naming a non-existent source or target version is rejected and
// reported as *relations.ReferencedVersionNotFoundError.
type RelationStore struct {
	pool *pgxpool.Pool
}

// NewRelationStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewRelationStore(pool *pgxpool.Pool) *RelationStore {
	return &RelationStore{pool: pool}
}

// CreateRelation implements relations.Repository. Relation row and version
// 1 are one transaction: a relation is never observable without its first
// version, and the counter starts at 1, matching the log.
func (s *RelationStore) CreateRelation(ctx context.Context, in relations.CreateRelationParams) (domain.Relation, domain.RelationVersion, error) {
	insert, err := relationInsertParams("", 1, in.Version)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, relations.ErrValidation
	}
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, relations.ErrValidation
	}
	var rel domain.Relation
	var v1 domain.RelationVersion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateRelation(ctx, projectID)
		if err != nil {
			return err
		}
		insert.RelationID = row.ID
		if err := canonicalizeRelationAndHash(ctx, q, &insert); err != nil {
			return err
		}
		vRow, err := q.CreateRelationVersion(ctx, insert)
		if err != nil {
			return err
		}
		// Advance the head pointer 0 → 1 through the same CAS that guards
		// every later version, so the counter and the log always move in
		// one transaction. No other transaction can see the row yet (it
		// was inserted in this one), so the CAS cannot lose here.
		bumped, err := q.BumpRelationVersionNo(ctx, sqlc.BumpRelationVersionNoParams{
			RelationID:        row.ID,
			ExpectedVersionNo: 0,
		})
		if err != nil || bumped != 1 {
			if err == nil {
				err = fmt.Errorf("persistence: create relation: CAS returned %d, want 1", bumped)
			}
			return err
		}
		rel = relationFromRow(row)
		rel.CurrentVersionNo = 1
		v1 = relationVersionFromRow(vRow)
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case isInvalidText(err) || (errors.As(err, &pgErr) && pgErr.Code == "23503"):
			// 23503 foreign_key_violation: a referenced project, state,
			// object version or user does not exist — the request names
			// something the domain does not have. 22P02: the payload is
			// not valid JSON.
			return domain.Relation{}, domain.RelationVersion{}, mapRelationWriteError(err, "", in.Version.SourceObjectVersionID, in.Version.TargetObjectVersionID)
		default:
			return domain.Relation{}, domain.RelationVersion{}, fmt.Errorf("persistence: create relation: %w", err)
		}
	}
	return rel, v1, nil
}

// CreateVersion implements relations.Repository. The CAS below is the
// whole concurrency story: it claims version expected+1 only while the
// relation's counter still equals expected. Every losing interleaving — a
// concurrent winner, a stale expectation, an expectation below the head —
// surfaces the same *relations.VersionConflictError, never a raw storage
// error.
func (s *RelationStore) CreateVersion(ctx context.Context, relationID string, expected int, in relations.VersionParams) (domain.RelationVersion, error) {
	relationUUID, err := textUUID(relationID)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrRelationNotFound
	}
	insert, err := relationInsertParams(relationID, 0, in)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrValidation
	}
	var v domain.RelationVersion
	var dupErr *pgconn.PgError // version_no unique backstop (see below)
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		bumped, err := q.BumpRelationVersionNo(ctx, sqlc.BumpRelationVersionNoParams{
			RelationID:        relationUUID,
			ExpectedVersionNo: int32(expected),
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			// Zero rows: the relation is missing, or the expectation lost.
			// One read distinguishes the two.
			row, gerr := q.GetRelationByID(ctx, relationUUID)
			if errors.Is(gerr, pgx.ErrNoRows) {
				return relations.ErrRelationNotFound
			}
			if gerr != nil {
				return gerr
			}
			return &relations.VersionConflictError{
				RelationID: relationID, Expected: expected, Actual: int(row.CurrentVersionNo),
			}
		}
		insert.VersionNo = bumped
		if err := canonicalizeRelationAndHash(ctx, q, &insert); err != nil {
			return err
		}
		vRow, err := q.CreateRelationVersion(ctx, insert)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				dupErr = pgErr // keep for the diagnostic read after rollback
			}
			return err
		}
		v = relationVersionFromRow(vRow)
		return nil
	})
	if dupErr != nil {
		// Defense in depth: the CAS makes a duplicate version_no
		// unreachable through the port — only a manually drifted counter
		// could raise 23505 here. Report the same stable conflict, with
		// the log's own head (the version rows are the truth; the
		// counter is a projection).
		actual, aerr := s.currentLogHead(ctx, relationUUID)
		if aerr != nil {
			actual = expected + 1
		}
		return domain.RelationVersion{}, &relations.VersionConflictError{
			RelationID: relationID, Expected: expected, Actual: actual,
		}
	}
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case isInvalidText(err) || (errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502")):
			// 23503/23502: a referenced state, object version or user does
			// not exist, or a NOT NULL column was violated — validation
			// outcomes, not store failures (the uuids were parsed in Go
			// before the transaction). 22P02: the payload is not valid
			// JSON.
			return domain.RelationVersion{}, mapRelationWriteError(err, relationID, in.SourceObjectVersionID, in.TargetObjectVersionID)
		default:
			return domain.RelationVersion{}, fmt.Errorf("persistence: create relation version: %w", err)
		}
	}
	return v, nil
}

// GetRelation implements relations.Repository.
func (s *RelationStore) GetRelation(ctx context.Context, relationID string) (domain.Relation, error) {
	id, err := textUUID(relationID)
	if err != nil {
		return domain.Relation{}, relations.ErrRelationNotFound
	}
	row, err := sqlc.New(s.pool).GetRelationByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.Relation{}, relations.ErrRelationNotFound
	}
	if err != nil {
		return domain.Relation{}, fmt.Errorf("persistence: get relation: %w", err)
	}
	return relationFromRow(row), nil
}

// GetVersion implements relations.Repository.
func (s *RelationStore) GetVersion(ctx context.Context, relationID string, versionNo int) (domain.RelationVersion, error) {
	id, err := textUUID(relationID)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrRelationVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetRelationVersionByNo(ctx, sqlc.GetRelationVersionByNoParams{
		RelationID: id, VersionNo: int32(versionNo),
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.RelationVersion{}, relations.ErrRelationVersionNotFound
	}
	if err != nil {
		return domain.RelationVersion{}, fmt.Errorf("persistence: get relation version: %w", err)
	}
	return relationVersionFromRow(row), nil
}

// GetLatestVersion implements relations.Repository.
func (s *RelationStore) GetLatestVersion(ctx context.Context, relationID string) (domain.RelationVersion, error) {
	id, err := textUUID(relationID)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrRelationVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetLatestRelationVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.RelationVersion{}, relations.ErrRelationVersionNotFound
	}
	if err != nil {
		return domain.RelationVersion{}, fmt.Errorf("persistence: get latest relation version: %w", err)
	}
	return relationVersionFromRow(row), nil
}

// ListVersions implements relations.Repository.
func (s *RelationStore) ListVersions(ctx context.Context, relationID string) ([]domain.RelationVersion, error) {
	id, err := textUUID(relationID)
	if err != nil {
		return nil, nil // cannot name a version log; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListRelationVersions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions: %w", err)
	}
	vs := make([]domain.RelationVersion, 0, len(rows))
	for _, row := range rows {
		vs = append(vs, relationVersionFromRow(row))
	}
	return vs, nil
}

// ListVersionsByType implements relations.Repository. The project filter
// rides on the relations container row; the type filter rides on the
// version row.
func (s *RelationStore) ListVersionsByType(ctx context.Context, projectID string, relationType string) ([]domain.RelationVersion, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListRelationVersionsByType(ctx, sqlc.ListRelationVersionsByTypeParams{
		ProjectID: projectUUID, RelationType: relationType,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions by type: %w", err)
	}
	vs := make([]domain.RelationVersion, 0, len(rows))
	for _, row := range rows {
		vs = append(vs, relationVersionFromRow(row))
	}
	return vs, nil
}

// ListVersionsByTypes implements relations.Repository (the category
// query).
func (s *RelationStore) ListVersionsByTypes(ctx context.Context, projectID string, relationTypes []string) ([]domain.RelationVersion, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	if len(relationTypes) == 0 {
		return []domain.RelationVersion{}, nil
	}
	rows, err := sqlc.New(s.pool).ListRelationVersionsByTypes(ctx, sqlc.ListRelationVersionsByTypesParams{
		ProjectID: projectUUID, RelationTypes: relationTypes,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions by types: %w", err)
	}
	vs := make([]domain.RelationVersion, 0, len(rows))
	for _, row := range rows {
		vs = append(vs, relationVersionFromRow(row))
	}
	return vs, nil
}

// ListVersionsForObject implements the rsg relation port's detail-page
// read (T0210): every relation version whose source or target endpoint
// pins a version of the object, newest first, with both endpoint display
// labels resolved in the same round trip. The project boundary is
// enforced on the relations container row.
func (s *RelationStore) ListVersionsForObject(ctx context.Context, projectID, objectID string) ([]rsg.ObjectRelationVersion, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, nil // cannot name a project; empty, not an error
	}
	objectUUID, err := textUUID(objectID)
	if err != nil {
		return nil, nil // cannot name an object; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListRelationVersionsForObject(ctx, sqlc.ListRelationVersionsForObjectParams{
		ProjectID: projectUUID, ObjectID: objectUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions for object: %w", err)
	}
	out := make([]rsg.ObjectRelationVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, rsg.ObjectRelationVersion{
			Relation: domain.RelationVersion{
				ID:                    pgUUIDToText(row.ID),
				RelationID:            pgUUIDToText(row.RelationID),
				VersionNo:             int(row.VersionNo),
				StateID:               pgUUIDToText(row.StateID),
				RelationType:          row.RelationType,
				SourceObjectVersionID: pgUUIDToText(row.SourceObjectVersionID),
				TargetObjectVersionID: pgUUIDToText(row.TargetObjectVersionID),
				Payload:               row.Payload,
				IntegrityHash:         row.IntegrityHash,
				CreatedBy:             pgUUIDToText(row.CreatedBy),
				CreatedAt:             row.CreatedAt.Time,
			},
			Source: rsg.ObjectRelationEndpoint{
				ObjectID:   pgUUIDToText(row.SourceObjectID),
				ObjectType: row.SourceObjectType,
				Title:      row.SourceTitle,
			},
			Target: rsg.ObjectRelationEndpoint{
				ObjectID:   pgUUIDToText(row.TargetObjectID),
				ObjectType: row.TargetObjectType,
				Title:      row.TargetTitle,
			},
		})
	}
	return out, nil
}

// currentLogHead is the version log's own head — the truth the conflict
// diagnostics report. It runs on the pool (after the failed transaction is
// gone) on the rare unique-violation backstop path only.
func (s *RelationStore) currentLogHead(ctx context.Context, relationID pgtype.UUID) (int, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_no), 0) FROM relation_versions WHERE relation_id = $1`, relationID)
	var head int
	if err := row.Scan(&head); err != nil {
		return 0, err
	}
	return head, nil
}

// mapRelationWriteError translates a storage-boundary failure of a write
// onto the package's domain errors. A foreign_key_violation on the source
// or target endpoint column is the "referenced version does not exist"
// outcome (edges are version-pinned, docs/07 §3) — reported with the side
// the constraint names. Every other FK/format failure is a validation
// outcome (a referenced project/state/user does not exist or a value was
// malformed), mirroring T0202's object store.
func mapRelationWriteError(err error, relationID, sourceVersionID, targetVersionID string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		switch pgErr.ConstraintName {
		case "relation_versions_source_object_version_id_fkey":
			return &relations.ReferencedVersionNotFoundError{Side: "source", VersionID: sourceVersionID}
		case "relation_versions_target_object_version_id_fkey":
			return &relations.ReferencedVersionNotFoundError{Side: "target", VersionID: targetVersionID}
		case "relation_versions_relation_id_fkey":
			return relations.ErrRelationNotFound
		default:
			return relations.ErrValidation
		}
	}
	return relations.ErrValidation
}

// canonicalizeRelationAndHash replaces the insert's payload with
// PostgreSQL's canonical jsonb form and sets the integrity hash to the
// sha256 of that canonical text. jsonb normalizes JSON on input (key
// order, whitespace), so the raw caller bytes and the stored bytes can
// differ; hashing the canonical form makes the stored hash always
// verifiable against the read payload (docs/21 §10). The canonicalization
// doubles as the JSON validity check at the storage boundary (22P02 on
// invalid JSON).
func canonicalizeRelationAndHash(ctx context.Context, q *sqlc.Queries, insert *sqlc.CreateRelationVersionParams) error {
	canonical, err := q.CanonicalizeRelationPayload(ctx, insert.Payload)
	if err != nil {
		return err
	}
	insert.Payload = canonical
	insert.IntegrityHash = payloadHash(canonical) // shared helper from T0202's object store
	return nil
}

// relationInsertParams builds the sqlc insert parameters for one new
// version row. The relation id may be zero (CreateRelation fills it from
// the created relation row) and the version number may be zero
// (CreateVersion fills it from the CAS result); every referenced id is
// parsed here so a malformed uuid becomes a clean error, never a panic and
// never a NULL write. Payload and IntegrityHash are completed by
// canonicalizeRelationAndHash inside the transaction.
func relationInsertParams(relationID string, versionNo int, in relations.VersionParams) (sqlc.CreateRelationVersionParams, error) {
	p := sqlc.CreateRelationVersionParams{
		VersionNo:    int32(versionNo),
		RelationType: in.RelationType,
		Payload:      in.Payload,
	}
	var err error
	if relationID != "" {
		if p.RelationID, err = textUUID(relationID); err != nil {
			return p, err
		}
	}
	if p.StateID, err = textUUID(in.StateID); err != nil {
		return p, err
	}
	if p.SourceObjectVersionID, err = textUUID(in.SourceObjectVersionID); err != nil {
		return p, err
	}
	if p.TargetObjectVersionID, err = textUUID(in.TargetObjectVersionID); err != nil {
		return p, err
	}
	if p.CreatedBy, err = textUUID(in.CreatedBy); err != nil {
		return p, err
	}
	return p, nil
}

// relationFromRow converts a sqlc relations row to the domain value.
func relationFromRow(row sqlc.Relation) domain.Relation {
	return domain.Relation{
		ID:               pgUUIDToText(row.ID),
		ProjectID:        pgUUIDToText(row.ProjectID),
		CurrentVersionNo: int(row.CurrentVersionNo),
		CreatedAt:        row.CreatedAt.Time,
	}
}

// relationVersionFromRow converts a sqlc relation_versions row to the
// domain value. Payload stays the exact stored bytes.
func relationVersionFromRow(row sqlc.RelationVersion) domain.RelationVersion {
	return domain.RelationVersion{
		ID:                    pgUUIDToText(row.ID),
		RelationID:            pgUUIDToText(row.RelationID),
		VersionNo:             int(row.VersionNo),
		StateID:               pgUUIDToText(row.StateID),
		RelationType:          row.RelationType,
		SourceObjectVersionID: pgUUIDToText(row.SourceObjectVersionID),
		TargetObjectVersionID: pgUUIDToText(row.TargetObjectVersionID),
		Payload:               row.Payload,
		IntegrityHash:         row.IntegrityHash,
		CreatedBy:             pgUUIDToText(row.CreatedBy),
		CreatedAt:             row.CreatedAt.Time,
	}
}

var _ relations.Repository = (*RelationStore)(nil)
