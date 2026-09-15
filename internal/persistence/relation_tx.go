package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// The transaction-scoped relation write (T0208): the consuming RSG service
// runs it inside the state commit's open transaction, so the relation row,
// its version 1 and the state transition carrying them share one atomic
// boundary. The body mirrors the pool-bound CreateRelation
// (relation_store.go) — same endpoint FK mapping, same canonical payload
// hashing — with the transaction already open and the relation id
// pre-generated (the commit's operation summary names it, commit_linkage).

// CreateRelationInTx implements rsg.RelationPort: the relation row (with
// the caller's pre-generated id) and version 1, on the commit transaction.
func (s *RelationStore) CreateRelationInTx(ctx context.Context, tx states.Transaction, in rsg.CreateRelationInTxParams) (domain.Relation, domain.RelationVersion, error) {
	relationUUID, err := textUUID(in.RelationID)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, relations.ErrValidation
	}
	projectUUID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, relations.ErrValidation
	}
	insert, err := relationInsertParams(in.RelationID, 1, in.Version)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, relations.ErrValidation
	}
	q := sqlc.New(tx)
	row, err := q.CreateRelationWithID(ctx, sqlc.CreateRelationWithIDParams{
		ID:        relationUUID,
		ProjectID: projectUUID,
	})
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, mapRelationTxWriteError(err, in.Version.SourceObjectVersionID, in.Version.TargetObjectVersionID)
	}
	insert.RelationID = row.ID
	if err := canonicalizeRelationAndHash(ctx, q, &insert); err != nil {
		return domain.Relation{}, domain.RelationVersion{}, mapRelationTxWriteError(err, in.Version.SourceObjectVersionID, in.Version.TargetObjectVersionID)
	}
	vRow, err := q.CreateRelationVersion(ctx, insert)
	if err != nil {
		return domain.Relation{}, domain.RelationVersion{}, mapRelationTxWriteError(err, in.Version.SourceObjectVersionID, in.Version.TargetObjectVersionID)
	}
	// The same 0 → 1 head advance as the pool path: the counter and the log
	// move in one transaction, and no other transaction can see the row yet
	// (it was inserted in this one), so the CAS cannot lose here.
	bumped, err := q.BumpRelationVersionNo(ctx, sqlc.BumpRelationVersionNoParams{
		RelationID:        row.ID,
		ExpectedVersionNo: 0,
	})
	if err != nil || bumped != 1 {
		if err == nil {
			err = fmt.Errorf("persistence: create relation in tx: CAS returned %d, want 1", bumped)
		}
		return domain.Relation{}, domain.RelationVersion{}, mapRelationTxWriteError(err, in.Version.SourceObjectVersionID, in.Version.TargetObjectVersionID)
	}
	rel := relationFromRow(row)
	rel.CurrentVersionNo = 1
	return rel, relationVersionFromRow(vRow), nil
}

// AppendRelationVersionInTx appends one relation version on the commit
// transaction (T0406): a relation already exists for every relation a
// three-way diff can report — the diff reads its versions — so the merge
// never creates the container, it appends the version the accepted state
// carries. The body mirrors CreateVersionInTx (and the pool-bound
// CreateVersion): the counter CAS first, then the version row, so a lost
// expectation reports the same *relations.VersionConflictError the rest of
// the platform raises, and the merge's optimistic retry can recognize it.
func (s *RelationStore) AppendRelationVersionInTx(ctx context.Context, tx states.Transaction, relationID string, expected int, in relations.VersionParams) (domain.RelationVersion, error) {
	relationUUID, err := textUUID(relationID)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrRelationNotFound
	}
	insert, err := relationInsertParams(relationID, 0, in)
	if err != nil {
		return domain.RelationVersion{}, relations.ErrValidation
	}
	q := sqlc.New(tx)
	bumped, err := q.BumpRelationVersionNo(ctx, sqlc.BumpRelationVersionNoParams{
		RelationID:        relationUUID,
		ExpectedVersionNo: int32(expected),
	})
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return domain.RelationVersion{}, mapRelationTxWriteError(err, in.SourceObjectVersionID, in.TargetObjectVersionID)
		}
		// Zero rows: the relation is missing, or the expectation lost. One
		// read distinguishes the two.
		row, gerr := q.GetRelationByID(ctx, relationUUID)
		if errors.Is(gerr, pgx.ErrNoRows) {
			return domain.RelationVersion{}, relations.ErrRelationNotFound
		}
		if gerr != nil {
			return domain.RelationVersion{}, mapRelationTxWriteError(gerr, in.SourceObjectVersionID, in.TargetObjectVersionID)
		}
		return domain.RelationVersion{}, &relations.VersionConflictError{
			RelationID: relationID, Expected: expected, Actual: int(row.CurrentVersionNo),
		}
	}
	insert.VersionNo = bumped
	if err := canonicalizeRelationAndHash(ctx, q, &insert); err != nil {
		return domain.RelationVersion{}, mapRelationTxWriteError(err, in.SourceObjectVersionID, in.TargetObjectVersionID)
	}
	vRow, err := q.CreateRelationVersion(ctx, insert)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// Defense in depth, same as CreateVersionInTx: the CAS makes a
			// duplicate version_no unreachable — only a manually drifted
			// counter could raise 23505 here, and the transaction is aborted
			// so the log head cannot be read back. Report the same stable
			// conflict with the expectation-based fallback.
			return domain.RelationVersion{}, &relations.VersionConflictError{
				RelationID: relationID, Expected: expected, Actual: expected + 1,
			}
		}
		return domain.RelationVersion{}, mapRelationTxWriteError(err, in.SourceObjectVersionID, in.TargetObjectVersionID)
	}
	return relationVersionFromRow(vRow), nil
}

// mapRelationTxWriteError translates a storage-boundary failure inside the
// commit transaction onto the domain outcomes: an endpoint FK names the
// missing version with its side, every other FK/format violation is a
// validation outcome, everything else stays a store failure for the commit
// to roll back over.
func mapRelationTxWriteError(err error, sourceVersionID, targetVersionID string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		switch pgErr.ConstraintName {
		case "relation_versions_source_object_version_id_fkey":
			return &relations.ReferencedVersionNotFoundError{Side: "source", VersionID: sourceVersionID}
		case "relation_versions_target_object_version_id_fkey":
			return &relations.ReferencedVersionNotFoundError{Side: "target", VersionID: targetVersionID}
		case "relation_versions_relation_id_fkey":
			return relations.ErrRelationNotFound
		}
	}
	return mapObjectTxWriteError(err) // 22P02/other FK/NOT NULL → ErrValidation, else raw
}

var _ rsg.RelationPort = (*RelationStore)(nil)
