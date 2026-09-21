package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/merge"
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
	if err := abortInsertParams(&p, in); err != nil {
		return p, err
	}
	if err := reopenInsertParams(&p, in); err != nil {
		return p, err
	}
	return p, nil
}

// abortInsertParams fills the abort record's six columns (migration 00100).
// The record is all-or-nothing — the database's abort_record_shape CHECK
// enforces the same rule — so a caller that supplies only part of it is
// refused here, before the row reaches the server, with the same
// ErrValidation shape every other malformed version input gets. A version
// with no record leaves every column NULL, never ”.
func abortInsertParams(p *sqlc.CreateScientificObjectVersionParams, in sciobjects.VersionParams) error {
	if in.AbortRequestKey != "" {
		if len(in.AbortRequestKey) < minAbortRequestKeyLen {
			return fmt.Errorf("%w: the abort request key must be at least %d characters",
				sciobjects.ErrValidation, minAbortRequestKeyLen)
		}
		p.AbortRequestKey = ptrText(in.AbortRequestKey)
	}
	if in.Abort == nil {
		if in.LifecycleState == domain.LifecycleAborted && p.AbortRequestKey != nil {
			// An aborted version may legitimately carry no record (a
			// pre-00100 row, or a fixture that only moves the lifecycle),
			// but a request key without a record would name a decision
			// nothing recorded.
			return fmt.Errorf("%w: an abort request key requires the abort record it belongs to", sciobjects.ErrValidation)
		}
		return nil
	}
	if in.LifecycleState != domain.LifecycleAborted {
		return fmt.Errorf("%w: an abort record belongs to a version in lifecycle %q, not %q",
			sciobjects.ErrValidation, domain.LifecycleAborted, in.LifecycleState)
	}
	if strings.TrimSpace(in.Abort.ReasonCode) == "" {
		return fmt.Errorf("%w: the abort reason code is required", sciobjects.ErrValidation)
	}
	if strings.TrimSpace(in.Abort.Explanation) == "" {
		return fmt.Errorf("%w: the abort explanation is required", sciobjects.ErrValidation)
	}
	if in.Abort.DecidedBy == "" {
		return fmt.Errorf("%w: the abort actor is required", sciobjects.ErrValidation)
	}
	if in.Abort.DecidedAt.IsZero() {
		return fmt.Errorf("%w: the abort time is required", sciobjects.ErrValidation)
	}
	decidedBy, err := textUUID(in.Abort.DecidedBy)
	if err != nil {
		return fmt.Errorf("%w: abort actor: %v", sciobjects.ErrValidation, err)
	}
	p.AbortReasonCode = ptrText(in.Abort.ReasonCode)
	p.AbortExplanation = ptrText(in.Abort.Explanation)
	if in.Abort.ReplacementRef != "" {
		p.AbortReplacementRef = ptrText(in.Abort.ReplacementRef)
	}
	p.AbortedBy = decidedBy
	p.AbortedAt = pgtype.Timestamptz{Time: in.Abort.DecidedAt, Valid: true}
	return nil
}

// minAbortRequestKeyLen is the contract's own Idempotency-Key bound
// (specs/api/openapi.yaml, components.parameters.IdempotencyKey: minLength
// 8), enforced at the adapter the same way pullrequests.MinCreationKeyLen
// is: a key too short to be a deliberate token is refused rather than
// stored.
const minAbortRequestKeyLen = 8

// reopenInsertParams fills the reopen record's five columns (migration
// 00123). It is abortInsertParams' twin, rule for rule: all-or-nothing (the
// database's reopen_record_shape CHECK enforces the same rule), the record
// belongs to a version whose lifecycle is 'reopened', a request key without
// the record it belongs to is refused, and an absent record leaves every
// column NULL.
//
// 00100's abort_replacement_ref has no counterpart here on purpose: that
// field is docs/46:7's "replacement/superseding ref", a field of an ABORT
// record — a reopen has no replacement. See migration 00123.
func reopenInsertParams(p *sqlc.CreateScientificObjectVersionParams, in sciobjects.VersionParams) error {
	if in.ReopenRequestKey != "" {
		if len(in.ReopenRequestKey) < minAbortRequestKeyLen {
			return fmt.Errorf("%w: the reopen request key must be at least %d characters",
				sciobjects.ErrValidation, minAbortRequestKeyLen)
		}
		p.ReopenRequestKey = ptrText(in.ReopenRequestKey)
	}
	if in.Reopen == nil {
		if in.LifecycleState == domain.LifecycleReopened && p.ReopenRequestKey != nil {
			// A reopened version may legitimately carry no record (a fixture
			// that only moves the lifecycle), but a request key without a
			// record would name a decision nothing recorded.
			return fmt.Errorf("%w: a reopen request key requires the reopen record it belongs to", sciobjects.ErrValidation)
		}
		return nil
	}
	if in.LifecycleState != domain.LifecycleReopened {
		return fmt.Errorf("%w: a reopen record belongs to a version in lifecycle %q, not %q",
			sciobjects.ErrValidation, domain.LifecycleReopened, in.LifecycleState)
	}
	if strings.TrimSpace(in.Reopen.ReasonCode) == "" {
		return fmt.Errorf("%w: the reopen reason code is required", sciobjects.ErrValidation)
	}
	if strings.TrimSpace(in.Reopen.Explanation) == "" {
		return fmt.Errorf("%w: the reopen explanation is required", sciobjects.ErrValidation)
	}
	if in.Reopen.DecidedBy == "" {
		return fmt.Errorf("%w: the reopen actor is required", sciobjects.ErrValidation)
	}
	if in.Reopen.DecidedAt.IsZero() {
		return fmt.Errorf("%w: the reopen time is required", sciobjects.ErrValidation)
	}
	decidedBy, err := textUUID(in.Reopen.DecidedBy)
	if err != nil {
		return fmt.Errorf("%w: reopen actor: %v", sciobjects.ErrValidation, err)
	}
	p.ReopenReasonCode = ptrText(in.Reopen.ReasonCode)
	p.ReopenExplanation = ptrText(in.Reopen.Explanation)
	p.ReopenedBy = decidedBy
	p.ReopenedAt = pgtype.Timestamptz{Time: in.Reopen.DecidedAt, Valid: true}
	return nil
}

// ptrText returns a pointer to s — the nullable-text shape sqlc generates
// for a `text` column without NOT NULL.
func ptrText(s string) *string { return &s }

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
		Abort:              abortRecordFromRow(row),
		Reopen:             reopenRecordFromRow(row),
	}
}

// abortRecordFromRow reads the abort record the row carries, or nil when it
// carries none. The two required halves — the reason code and the
// explanation — decide presence: migration 00100's abort_record_shape CHECK
// makes the record all-or-nothing, so a row that has one has them all, and a
// row that has none has none of them. Nil (not a zero-valued record) is what
// "not an abort" reads as, so no caller has to test a sentinel string.
func abortRecordFromRow(row sqlc.ScientificObjectVersion) *domain.AbortRecord {
	if row.AbortReasonCode == nil || row.AbortExplanation == nil {
		return nil
	}
	rec := &domain.AbortRecord{
		ReasonCode:  *row.AbortReasonCode,
		Explanation: *row.AbortExplanation,
		DecidedBy:   pgUUIDToText(row.AbortedBy),
	}
	if row.AbortReplacementRef != nil {
		rec.ReplacementRef = *row.AbortReplacementRef
	}
	if row.AbortedAt.Valid {
		rec.DecidedAt = row.AbortedAt.Time
	}
	return rec
}

// reopenRecordFromRow is abortRecordFromRow's twin for migration 00123's
// columns: the record the row carries, or nil when it carries none. The two
// required halves — the reason code and the explanation — decide presence,
// because reopen_record_shape makes the record all-or-nothing.
func reopenRecordFromRow(row sqlc.ScientificObjectVersion) *domain.ReopenRecord {
	if row.ReopenReasonCode == nil || row.ReopenExplanation == nil {
		return nil
	}
	rec := &domain.ReopenRecord{
		ReasonCode:  *row.ReopenReasonCode,
		Explanation: *row.ReopenExplanation,
		DecidedBy:   pgUUIDToText(row.ReopenedBy),
	}
	if row.ReopenedAt.Valid {
		rec.DecidedAt = row.ReopenedAt.Time
	}
	return rec
}

// GetVersionByAbortRequestKey implements sciobjects.Repository: the version
// an earlier abort request with this Idempotency-Key appended, or
// ErrVersionNotFound when the key has not been used on this object. It is the
// read half of the abort command's replay (migration 00100 keeps the key on
// the version row, so the state itself is the idempotency record). The
// pool-bound GetVersionByID above is the sibling read this one mirrors.
func (s *ScientificObjectStore) GetVersionByAbortRequestKey(ctx context.Context, objectID, requestKey string) (domain.ScientificObjectVersion, error) {
	if requestKey == "" {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	objectUUID, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByAbortRequestKey(ctx, sqlc.GetScientificObjectVersionByAbortRequestKeyParams{
		ObjectID:        objectUUID,
		AbortRequestKey: ptrText(requestKey),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
		}
		return domain.ScientificObjectVersion{}, fmt.Errorf("%w: %v", sciobjects.ErrStore, err)
	}
	return versionFromRow(row), nil
}

// GetVersionByReopenRequestKey implements sciobjects.Repository: the version
// an earlier reopen request with this Idempotency-Key appended, or
// ErrVersionNotFound when the key has not been used on this object. It is
// the read half of the reopen command's replay (migration 00123 keeps the
// key on the version row) and the exact sibling of the abort read above —
// separate column, separate command, so neither read can answer for the
// other's request.
func (s *ScientificObjectStore) GetVersionByReopenRequestKey(ctx context.Context, objectID, requestKey string) (domain.ScientificObjectVersion, error) {
	if requestKey == "" {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	objectUUID, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByReopenRequestKey(ctx, sqlc.GetScientificObjectVersionByReopenRequestKeyParams{
		ObjectID:         objectUUID,
		ReopenRequestKey: ptrText(requestKey),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
		}
		return domain.ScientificObjectVersion{}, fmt.Errorf("%w: %v", sciobjects.ErrStore, err)
	}
	return versionFromRow(row), nil
}

// AbortRecordOf implements merge.AbortReader: the abort record versionID
// carries, or nil when that version is not an abort. The read exists for the
// merge, which copies the record onto the accepted version (a main-line abort
// reaches main only through a Research PR, so the merge is the one place the
// record has to travel). It reads the row by id, so a version the merge is
// about to materialize from a proposal branch is found whether or not the
// branch has since been closed.
func (s *ScientificObjectStore) AbortRecordOf(ctx context.Context, versionID string) (*domain.AbortRecord, error) {
	id, err := textUUID(versionID)
	if err != nil {
		return nil, nil
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", sciobjects.ErrStore, err)
	}
	return abortRecordFromRow(row), nil
}

// ReopenRecordOf implements merge.ReopenReader: the reopen record versionID
// carries, or nil when that version is not a reopen. AbortRecordOf's twin,
// with the same reason for existing — a main-line reopen reaches main only
// through a Research PR, so the merge is the one place the record has to
// travel — and the same read-by-id shape, so a version the merge is about to
// materialize is found whether or not the branch has since been closed.
func (s *ScientificObjectStore) ReopenRecordOf(ctx context.Context, versionID string) (*domain.ReopenRecord, error) {
	id, err := textUUID(versionID)
	if err != nil {
		return nil, nil
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %v", sciobjects.ErrStore, err)
	}
	return reopenRecordFromRow(row), nil
}

var (
	_ sciobjects.Repository = (*ScientificObjectStore)(nil)
	_ merge.AbortReader     = (*ScientificObjectStore)(nil)
	_ merge.ReopenReader    = (*ScientificObjectStore)(nil)
)
