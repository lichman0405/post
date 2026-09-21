package persistence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/reopens"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AppendReopenVersionInTx implements reopens.Objects: the reopen proposal's
// only write, and it runs on the commit's transaction.
//
// It is AppendAbortVersionInTx's twin, row for row and rule for rule: the
// version row that moves the lifecycle to 'reopened' (carrying the reopen
// record and the request's Idempotency-Key) and the audit row of the
// decision go in together, under the transaction the caller's state commit
// owns — a failed audit write rolls the version back, and the reopen event
// the caller records in the same callback rolls back with them (docs/53: the
// audit record of a high-risk action is part of the action).
//
// The separate method, rather than a flag on CreateVersionInTx, exists for
// the reason the abort's does: a reopen without its audit record is a state
// this build must not be able to reach, and a caller could forget to write
// one.
//
// The compare-and-swap semantics are the abort's exactly — the same
// BumpScientificObjectVersionNo against in.ExpectedVersionNo, the same
// one-read disambiguation between "no such object" and "the log moved", the
// same VersionConflictError — so of two concurrent reopen requests with one
// Idempotency-Key exactly one bumps the counter. The loser's insert never
// runs, so it writes no audit row and no event.
func (s *ScientificObjectStore) AppendReopenVersionInTx(ctx context.Context, tx states.Transaction, in reopens.ReopenWriteParams) (domain.ScientificObjectVersion, error) {
	objectUUID, err := textUUID(in.ObjectID)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
	}
	insert, err := versionInsertParams(in.ObjectID, 0, in.Version)
	if err != nil {
		return domain.ScientificObjectVersion{}, sciobjects.ErrValidation
	}
	q := sqlc.New(tx)
	bumped, err := q.BumpScientificObjectVersionNo(ctx, sqlc.BumpScientificObjectVersionNoParams{
		ObjectID:          objectUUID,
		ExpectedVersionNo: int32(in.ExpectedVersionNo),
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
		}
		// Zero rows: the object is missing, or the expectation lost. One
		// read distinguishes the two, the same way CreateVersionInTx does.
		obj, gerr := q.GetScientificObjectByID(ctx, objectUUID)
		if errors.Is(gerr, pgx.ErrNoRows) {
			return domain.ScientificObjectVersion{}, sciobjects.ErrObjectNotFound
		}
		if gerr != nil {
			return domain.ScientificObjectVersion{}, mapObjectTxWriteError(gerr)
		}
		return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{
			ObjectID: in.ObjectID, Expected: in.ExpectedVersionNo, Actual: int(obj.CurrentVersionNo),
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
			// Two unique constraints can raise this: version_no per object,
			// and (migration 00123) the reopen request key per object. Both
			// mean the same thing to the caller — another request got there
			// first — and the transaction is already aborted, so neither row
			// can be read back here to say which. The caller's replay read
			// answers it.
			return domain.ScientificObjectVersion{}, &sciobjects.VersionConflictError{
				ObjectID: in.ObjectID, Expected: in.ExpectedVersionNo, Actual: in.ExpectedVersionNo + 1,
			}
		}
		return domain.ScientificObjectVersion{}, mapObjectTxWriteError(err)
	}
	// The audit row is written HERE, on the same transaction, not by the
	// caller: a reopen with no record of who decided it is exactly the state
	// the T0610 record exists to prevent.
	if err := appendAudit(ctx, q, in.Audit); err != nil {
		return domain.ScientificObjectVersion{}, err
	}
	return versionFromRow(vRow), nil
}

// The application layer must not import persistence, so the port assertion
// lives on this side (the same placement as aborts.Objects above).
var _ reopens.Objects = (*ScientificObjectStore)(nil)
