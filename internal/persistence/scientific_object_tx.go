package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// The transaction-scoped scientific-object writes (T0208): the consuming
// RSG service runs these inside the state commit's open transaction, so an
// object/version row and the state transition that carries it share one
// atomic boundary. The bodies mirror the pool-bound CreateObject and
// CreateVersion (scientific_object_store.go) — same CAS, same canonical
// payload hashing, same domain outcomes — with the transaction already
// open and the object id pre-generated (the commit's operation summary
// names it, commit_linkage).

// CreateObjectInTx implements rsg.ObjectPort: the object row (with the
// caller's pre-generated id) and version 1, on the commit transaction.
func (s *ScientificObjectStore) CreateObjectInTx(ctx context.Context, tx states.Transaction, in rsg.CreateObjectInTxParams) (domain.ScientificObject, domain.ScientificObjectVersion, error) {
	objectUUID, err := textUUID(in.ObjectID)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	projectUUID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	insert, err := versionInsertParams(in.ObjectID, 1, in.Version)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	q := sqlc.New(tx)
	row, err := q.CreateScientificObjectWithID(ctx, sqlc.CreateScientificObjectWithIDParams{
		ID:         objectUUID,
		ProjectID:  projectUUID,
		ObjectType: in.ObjectType,
		CreatedBy:  insert.CreatedBy,
	})
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	insert.ObjectID = row.ID
	if err := canonicalizeAndHash(ctx, q, &insert); err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	vRow, err := q.CreateScientificObjectVersion(ctx, insert)
	if err != nil {
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	// The same 0 → 1 head advance as the pool path: the counter and the log
	// move in one transaction, and no other transaction can see the row yet
	// (it was inserted in this one), so the CAS cannot lose here.
	bumped, err := q.BumpScientificObjectVersionNo(ctx, sqlc.BumpScientificObjectVersionNoParams{
		ObjectID:          row.ID,
		ExpectedVersionNo: 0,
	})
	if err != nil || bumped != 1 {
		if err == nil {
			err = fmt.Errorf("persistence: create scientific object in tx: CAS returned %d, want 1", bumped)
		}
		return domain.ScientificObject{}, domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	obj := objectFromRow(row)
	obj.CurrentVersionNo = 1
	return obj, versionFromRow(vRow), nil
}

// CreateVersionInTx implements rsg.ObjectPort: append version expected+1
// on the commit transaction, with the same compare-and-swap semantics as
// the pool-bound CreateVersion (a zero-row CAS is one read away from
// missing object vs lost expectation).
func (s *ScientificObjectStore) CreateVersionInTx(ctx context.Context, tx states.Transaction, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error) {
	objectUUID, err := textUUID(objectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	insert, err := versionInsertParams(objectID, 0, in)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	q := sqlc.New(tx)
	bumped, err := q.BumpScientificObjectVersionNo(ctx, sqlc.BumpScientificObjectVersionNoParams{
		ObjectID:          objectUUID,
		ExpectedVersionNo: int32(expected),
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
		}
		// Zero rows: the object is missing, or the expectation lost. One
		// read distinguishes the two.
		obj, gerr := q.GetScientificObjectByID(ctx, objectUUID)
		if errors.Is(gerr, pgx.ErrNoRows) {
			return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
		}
		if gerr != nil {
			return domain.ScientificObjectVersion{}, mapObjectTxWriteError(gerr)
		}
		return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{
			ObjectID: objectID, Expected: expected, Actual: int(obj.CurrentVersionNo),
		}
	}
	insert.VersionNo = bumped
	if err := canonicalizeAndHash(ctx, q, &insert); err != nil {
		return domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	vRow, err := q.CreateScientificObjectVersion(ctx, insert)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Defense in depth, same as the pool path: the CAS makes a
			// duplicate version_no unreachable — only a manually drifted
			// counter could raise 23505 here. The transaction is aborted, so
			// the log's own head cannot be read back; report the same stable
			// conflict with the expectation-based fallback the pool path
			// uses when its diagnostic read fails.
			return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{
				ObjectID: objectID, Expected: expected, Actual: expected + 1,
			}
		}
		return domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	return versionFromRow(vRow), nil
}

// GetVersionByID implements rsg.ObjectPort: one version row by its own id
// (the relation endpoint pin).
func (s *ScientificObjectStore) GetVersionByID(ctx context.Context, versionID string) (domain.ScientificObjectVersion, error) {
	id, err := textUUID(versionID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetScientificObjectVersionByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidText(err) {
		return domain.ScientificObjectVersion{}, sciobjects.ErrVersionNotFound
	}
	if err != nil {
		return domain.ScientificObjectVersion{}, fmt.Errorf("persistence: get scientific object version by id: %w", err)
	}
	return versionFromRow(row), nil
}

// mapObjectTxWriteError translates a storage-boundary failure inside the
// commit transaction onto the domain outcomes: FK/format violations are
// validation outcomes (a referenced project/state/user does not exist, the
// payload is not valid JSON); everything else stays a store failure for
// the commit to roll back over.
func mapObjectTxWriteError(err error) error {
	var pgErr *pgconn.PgError
	if isInvalidText(err) || (errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23502")) {
		return sciobjects.ErrValidation
	}
	return err
}

var _ rsg.ObjectPort = (*ScientificObjectStore)(nil)
