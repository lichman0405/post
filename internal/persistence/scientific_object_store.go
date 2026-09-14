package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ScientificObjectStore is the production sciobjects.Repository adapter over
// PostgreSQL (sqlc generated queries, pgx). The version log is append-only
// on two layers at once: the port exposes no update path, and the database
// rejects any UPDATE/DELETE of a version row itself (migrations
// 00014/00015). Version creation is an atomic compare-and-swap on
// scientific_objects.current_version_no (migration 00024, T0202): the
// counter advances from expected to expected+1 only while it still equals
// expected, so concurrent writers serialize into exactly one winner and
// stable EXPECTED_VERSION_MISMATCH losers — no unique-violation races.
type ScientificObjectStore struct {
	pool *pgxpool.Pool
}

// NewScientificObjectStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewScientificObjectStore(pool *pgxpool.Pool) *ScientificObjectStore {
	return &ScientificObjectStore{pool: pool}
}

// CreateObject implements sciobjects.Repository. Object row and version 1
// are one transaction: an object is never observable without its first
// version, and the counter starts at 1, matching the log.
func (s *ScientificObjectStore) CreateObject(ctx context.Context, in sciobjects.CreateObjectParams) (domain.ScientificObject, domain.ScientificObjectVersion, error) {
	insert, err := versionInsertParams("", 1, in.Version)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	var obj domain.ScientificObject
	var v1 domain.ScientificObjectVersion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateScientificObject(ctx, sqlc.CreateScientificObjectParams{
			ProjectID:  projectID,
			ObjectType: in.ObjectType,
			CreatedBy:  insert.CreatedBy,
		})
		if err != nil {
			return err
		}
		insert.ObjectID = row.ID
		if err := canonicalizeAndHash(ctx, q, &insert); err != nil {
			return err
		}
		vRow, err := q.CreateScientificObjectVersion(ctx, insert)
		if err != nil {
			return err
		}
		// Advance the head pointer 0 → 1 through the same CAS that guards
		// every later version, so the counter and the log always move in
		// one transaction. No other transaction can see the row yet (it
		// was inserted in this one), so the CAS cannot lose here.
		bumped, err := q.BumpScientificObjectVersionNo(ctx, sqlc.BumpScientificObjectVersionNoParams{
			ObjectID:          row.ID,
			ExpectedVersionNo: 0,
		})
		if err != nil || bumped != 1 {
			if err == nil {
				err = fmt.Errorf("persistence: create scientific object: CAS returned %d, want 1", bumped)
			}
			return err
		}
		obj = objectFromRow(row)
		obj.CurrentVersionNo = 1
		v1 = versionFromRow(vRow)
		return nil
	})
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case isInvalidText(err) || (errors.As(err, &pgErr) && pgErr.Code == "23503"):
			// 23503 foreign_key_violation: a referenced project, state or
			// user does not exist — the request names something the
			// domain does not have. 22P02: the payload is not valid JSON.
			return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
		default:
			return domain.ScientificObject{}, domain.ScientificObjectVersion{}, fmt.Errorf("persistence: create scientific object: %w", err)
		}
	}
	return obj, v1, nil
}

// CreateVersion implements sciobjects.Repository. The CAS below is the
// whole concurrency story: it claims version expected+1 only while the
// object's counter still equals expected. Every losing interleaving — a
// concurrent winner, a stale expectation, an expectation below the head —
// surfaces the same *sciobjects.VersionConflictError, never a raw storage
// error.
func (s *ScientificObjectStore) CreateVersion(ctx context.Context, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error) {
	objectUUID, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	insert, err := versionInsertParams(objectID, 0, in)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	var v domain.ScientificObjectVersion
	var dupErr *pgconn.PgError // version_no unique backstop (see below)
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		bumped, err := q.BumpScientificObjectVersionNo(ctx, sqlc.BumpScientificObjectVersionNoParams{
			ObjectID:          objectUUID,
			ExpectedVersionNo: int32(expected),
		})
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			// Zero rows: the object is missing, or the expectation lost.
			// One read distinguishes the two.
			obj, gerr := q.GetScientificObjectByID(ctx, objectUUID)
			if errors.Is(gerr, pgx.ErrNoRows) {
				return sciobjects.ErrObjectNotFound
			}
			if gerr != nil {
				return gerr
			}
			return &sciobjects.VersionConflictError{
				ObjectID: objectID, Expected: expected, Actual: int(obj.CurrentVersionNo),
			}
		}
		insert.VersionNo = bumped
		if err := canonicalizeAndHash(ctx, q, &insert); err != nil {
			return err
		}
		vRow, err := q.CreateScientificObjectVersion(ctx, insert)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				dupErr = pgErr // keep for the diagnostic read after rollback
			}
			return err
		}
		v = versionFromRow(vRow)
		return nil
	})
	if dupErr != nil {
		// Defense in depth: the CAS makes a duplicate version_no
		// unreachable through the port — only a manually drifted counter
		// could raise 23505 here. Report the same stable conflict, with
		// the log's own head (the version rows are the truth; the
		// counter is a projection).
		actual, aerr := s.currentLogHead(ctx, objectUUID)
		if aerr != nil {
			actual = expected + 1
		}
		return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{
			ObjectID: objectID, Expected: expected, Actual: actual,
		}
	}
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case isInvalidText(err) || (errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502")):
			// 22P02: the payload is not valid JSON (the uuids were parsed
			// in Go before the transaction). 23503/23502: a referenced
			// state or user does not exist, or a NOT NULL column was
			// violated — validation outcomes, not store failures.
			return domain.ScientificObjectVersion{}, sciobjects.ErrValidation
		default:
			return domain.ScientificObjectVersion{}, fmt.Errorf("persistence: create scientific object version: %w", err)
		}
	}
	return v, nil
}

// GetObject implements sciobjects.Repository.
func (s *ScientificObjectStore) GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error) {
	id, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
	}
	row, err := sqlc.New(s.pool).GetScientificObjectByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ScientificObject{}, sciobjects.ErrObjectNotFound
	}
	if err != nil {
		return domain.ScientificObject{}, fmt.Errorf("persistence: get scientific object: %w", err)
	}
	return objectFromRow(row), nil
}

// GetVersion implements sciobjects.Repository. Version numbers are int4 in
// PostgreSQL: a value outside the int32 range cannot name a row, so it
// answers ErrVersionNotFound — an int32 truncation here would silently
// answer the wrong version (2^32+5 would render as version 5).
func (s *ScientificObjectStore) GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error) {
	if versionNo < 1 || versionNo > math.MaxInt32 {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	id, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByNo(ctx, sqlc.GetScientificObjectVersionByNoParams{
		ObjectID: id, VersionNo: int32(versionNo),
	})
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	if err != nil {
		return domain.ScientificObjectVersion{}, fmt.Errorf("persistence: get scientific object version: %w", err)
	}
	return versionFromRow(row), nil
}

// GetLatestVersion implements sciobjects.Repository.
func (s *ScientificObjectStore) GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error) {
	id, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetLatestScientificObjectVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	if err != nil {
		return domain.ScientificObjectVersion{}, fmt.Errorf("persistence: get latest scientific object version: %w", err)
	}
	return versionFromRow(row), nil
}

// ListVersions implements sciobjects.Repository.
func (s *ScientificObjectStore) ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error) {
	id, err := textUUID(objectID)
	if err != nil {
		return nil, nil // cannot name a version log; empty, not an error
	}
	rows, err := sqlc.New(s.pool).ListScientificObjectVersions(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list scientific object versions: %w", err)
	}
	vs := make([]domain.ScientificObjectVersion, 0, len(rows))
	for _, row := range rows {
		vs = append(vs, versionFromRow(row))
	}
	return vs, nil
}

// currentLogHead is the version log's own head — the truth the conflict
// diagnostics report. It runs on the pool (after the failed transaction is
// gone) on the rare unique-violation backstop path only.
func (s *ScientificObjectStore) currentLogHead(ctx context.Context, objectID pgtype.UUID) (int, error) {
	row := s.pool.QueryRow(ctx,
		`SELECT COALESCE(max(version_no), 0) FROM scientific_object_versions WHERE object_id = $1`, objectID)
	var head int
	if err := row.Scan(&head); err != nil {
		return 0, err
	}
	return head, nil
}

// canonicalizeAndHash replaces the insert's payload with PostgreSQL's
// canonical jsonb form and sets the integrity hash to the sha256 of that
// canonical text. jsonb normalizes JSON on input (key order, whitespace),
// so the raw caller bytes and the stored bytes can differ; hashing the
// canonical form makes the stored hash always verifiable against the read
// payload (docs/21 §10). The canonicalization doubles as the JSON validity
// check at the storage boundary (22P02 on invalid JSON).
func canonicalizeAndHash(ctx context.Context, q *sqlc.Queries, insert *sqlc.CreateScientificObjectVersionParams) error {
	canonical, err := q.CanonicalizeScientificObjectPayload(ctx, insert.Payload)
	if err != nil {
		return err
	}
	insert.Payload = canonical
	insert.IntegrityHash = payloadHash(canonical)
	return nil
}

// versionInsertParams builds the sqlc insert parameters for one new version
// row. The object id may be zero (CreateObject fills it from the created
// object row) and the version number may be zero (CreateVersion fills it
// from the CAS result); every referenced id is parsed here so a malformed
// uuid becomes a clean error, never a panic and never a NULL write.
// Payload and IntegrityHash are completed by canonicalizeAndHash inside the
// transaction.
func versionInsertParams(objectID string, versionNo int, in sciobjects.VersionParams) (sqlc.CreateScientificObjectVersionParams, error) {
	p := sqlc.CreateScientificObjectVersionParams{
		VersionNo:      int32(versionNo),
		SchemaID:       in.SchemaID,
		SchemaVersion:  in.SchemaVersion,
		Title:          in.Title,
		LifecycleState: string(in.LifecycleState),
		Payload:        in.Payload,
	}
	var err error
	if objectID != "" {
		if p.ObjectID, err = textUUID(objectID); err != nil {
			return p, err
		}
	}
	if p.StateID, err = textUUID(in.StateID); err != nil {
		return p, err
	}
	if in.BranchID != nil {
		if p.BranchID, err = textUUID(*in.BranchID); err != nil {
			return p, err
		}
	}
	if in.VisibilityPolicyID != nil {
		if p.VisibilityPolicyID, err = textUUID(*in.VisibilityPolicyID); err != nil {
			return p, err
		}
	}
	if p.CreatedBy, err = textUUID(in.CreatedBy); err != nil {
		return p, err
	}
	return p, nil
}

// payloadHash is the sha256 hex digest of the exact payload bytes.
func payloadHash(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// objectFromRow converts a sqlc scientific_objects row to the domain value.
func objectFromRow(row sqlc.ScientificObject) domain.ScientificObject {
	return domain.ScientificObject{
		ID:               pgUUIDToText(row.ID),
		ProjectID:        pgUUIDToText(row.ProjectID),
		ObjectType:       row.ObjectType,
		CurrentVersionNo: int(row.CurrentVersionNo),
		CreatedBy:        pgUUIDToText(row.CreatedBy),
		CreatedAt:        row.CreatedAt.Time,
	}
}

// versionFromRow converts a sqlc scientific_object_versions row to the
// domain value. Payload stays the exact stored bytes.
func versionFromRow(row sqlc.ScientificObjectVersion) domain.ScientificObjectVersion {
	return domain.ScientificObjectVersion{
		ID:                 pgUUIDToText(row.ID),
		ObjectID:           pgUUIDToText(row.ObjectID),
		VersionNo:          int(row.VersionNo),
		StateID:            pgUUIDToText(row.StateID),
		BranchID:           pgUUIDTextPtr(row.BranchID),
		SchemaID:           row.SchemaID,
		SchemaVersion:      row.SchemaVersion,
		Title:              row.Title,
		LifecycleState:     domain.LifecycleState(row.LifecycleState),
		Payload:            row.Payload,
		VisibilityPolicyID: pgUUIDTextPtr(row.VisibilityPolicyID),
		IntegrityHash:      row.IntegrityHash,
		CreatedBy:          pgUUIDToText(row.CreatedBy),
		CreatedAt:          row.CreatedAt.Time,
	}
}

var _ sciobjects.Repository = (*ScientificObjectStore)(nil)
