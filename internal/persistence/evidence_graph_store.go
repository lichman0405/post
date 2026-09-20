package persistence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/evidence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// EvidenceGraphStore is the evidence-graph read adapter (T0506): the
// evidence_assertions rows of one pinned target version, as the
// project-scoped evidence read renders them TO ONE READER (ADR-024).
//
// It reads the table the evidence write path (T0806) writes and nothing
// else: no provenance_edges row, no RSG relation row, and no write method of
// any kind. The query it runs
// (internal/persistence/queries/evidence.sql, ListEvidenceAssertionsForTarget)
// carries the audience predicate itself — public rows, plus the private rows
// of the reader's own projects — so what reaches this adapter is already what
// the reader may be shown, and no filter has to be re-spelled above it.
//
// The caller still reaches it only after the project read gate has run, and
// the object it reads is checked to belong to the path project
// (evidencegraph.Service.object): the gate decides whether the reader may see
// the PROJECT, the predicate decides which ROWS it may see. They are two
// questions, and this task moved only the second.
type EvidenceGraphStore struct {
	pool *pgxpool.Pool
}

// NewEvidenceGraphStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewEvidenceGraphStore(pool *pgxpool.Pool) *EvidenceGraphStore {
	return &EvidenceGraphStore{pool: pool}
}

// ListForTargetVersion implements evidencegraph.AssertionStore: the
// assertions pinning one target object version that readerUserID may be
// rendered, oldest first (the query's own total order, (created_at, id), so
// one database state renders one document).
//
// An EMPTY readerUserID is the anonymous reader and is passed to SQL as NULL,
// which is the query's fail-closed branch: the membership clauses cannot be
// true for a reader that names no user, so the answer is exactly the public
// rows. The same answer is given for an id that cannot be a uuid — a caller
// that handed us something that names no user gets no user's rows, which is
// this read's direction everywhere else too (the column's DEFAULT is
// 'private'). It is NOT an error: an unreadable reader is not a broken read.
//
// Stance is deliberately NOT filled: the projection derives the label from
// Relation (internal/evidence.Group), so a row cannot carry one label and sit
// in another label's bucket.
//
// An id that cannot name a version row answers an empty list rather than an
// error — the same answer a version with no assertions gets, so this read is
// not an existence oracle either (the sibling read, EvidenceStore.
// ListPublishedEvidence, states the same rule).
func (s *EvidenceGraphStore) ListForTargetVersion(ctx context.Context, objectVersionID, readerUserID string) ([]evidence.Assertion, error) {
	id, err := textUUID(objectVersionID)
	if err != nil {
		return []evidence.Assertion{}, nil
	}
	reader, err := textUUID(readerUserID)
	if err != nil {
		reader = pgtype.UUID{} // no user id resolved: the public rows only
	}
	rows, err := sqlc.New(s.pool).ListEvidenceAssertionsForTarget(ctx, sqlc.ListEvidenceAssertionsForTargetParams{
		ObjectVersionID: id,
		ReaderUserID:    reader,
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list evidence assertions for target version: %w", err)
	}
	out := make([]evidence.Assertion, 0, len(rows))
	for _, row := range rows {
		out = append(out, evidence.Assertion{
			ID:                      pgUUIDToText(row.ID),
			Relation:                row.RelationType,
			EvidenceType:            row.EvidenceType,
			Directness:              row.Directness,
			InferenceNature:         row.InferenceNature,
			Scope:                   json.RawMessage(row.Scope),
			ReasoningNote:           derefText(row.ReasoningNote),
			ReviewState:             row.ReviewState,
			TargetObjectVersionID:   pgUUIDToText(row.TargetObjectVersionID),
			EvidenceObjectVersionID: pgUUIDToText(row.EvidenceObjectVersionID),
			CreatedAt:               knowledgepublish.FormatInstant(row.CreatedAt.Time),
		})
	}
	return out, nil
}
