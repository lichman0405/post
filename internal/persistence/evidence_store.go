package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/evidencenetwork"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rights"
)

// EvidenceStore is the production evidence-assertion adapter (T0806): the
// transaction-scoped write the RSG service drives inside a state commit,
// the two reads the write decision needs (a version's owning project, and
// the publication a version carries), and the read that builds the network
// evidence section of a published knowledge document.
//
// The table is append-only in the DELETE sense (migration 00091): there is
// no update path and no delete path in this adapter, and the database
// refuses a bare DELETE of any row — including one that bypasses this code
// entirely. Reviewing an assertion (unreviewed → reviewed → rejected) is a
// legitimate in-place state change and is deliberately NOT blocked; it has
// no route in this build (see the task result).
type EvidenceStore struct {
	pool *pgxpool.Pool
}

// NewEvidenceStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewEvidenceStore(pool *pgxpool.Pool) *EvidenceStore {
	return &EvidenceStore{pool: pool}
}

// CreateEvidenceAssertionInTx implements rsg.EvidencePort: one
// evidence_assertions row on the commit transaction, with the caller's
// pre-generated id (the commit's operation summary names the same id).
//
// The row's review_state is NOT written: it is the column's own default
// ('unreviewed'), which is what the returned row reports. An assertion is
// born unreviewed and the reviewer moves it — the author's write cannot
// pre-empt that.
func (s *EvidenceStore) CreateEvidenceAssertionInTx(ctx context.Context, tx states.Transaction, in rsg.CreateEvidenceAssertionInTxParams) (rsg.EvidenceAssertionRow, error) {
	id, err := textUUID(in.ID)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion id: %v", rsg.ErrValidation, err)
	}
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion project: %v", rsg.ErrValidation, err)
	}
	stateID, err := textUUID(in.StateID)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion state: %v", rsg.ErrValidation, err)
	}
	targetID, err := textUUID(in.TargetObjectVersionID)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion target version: %v", rsg.ErrValidation, err)
	}
	evidenceID, err := textUUID(in.EvidenceObjectVersionID)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion evidence version: %v", rsg.ErrValidation, err)
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: evidence assertion author: %v", rsg.ErrValidation, err)
	}
	row, err := sqlc.New(tx).CreateEvidenceAssertion(ctx, sqlc.CreateEvidenceAssertionParams{
		ID:                      id,
		ProjectID:               projectID,
		StateID:                 stateID,
		TargetObjectVersionID:   targetID,
		EvidenceObjectVersionID: evidenceID,
		RelationType:            in.RelationType,
		EvidenceType:            in.EvidenceType,
		Scope:                   scopeBytes(in.Scope),
		Directness:              in.Directness,
		InferenceNature:         in.InferenceNature,
		ReasoningNote:           nullableText(in.ReasoningNote),
		CreatedBy:               createdBy,
		EvidenceOrigin:          in.EvidenceOrigin,
		Visibility:              in.Visibility,
	})
	if err != nil {
		// The endpoints were all resolved before the write, so a foreign-key
		// or CHECK violation here means the row is not the one the decision
		// was made about: a store failure, never a silent success, and the
		// commit's transaction rolls back with it.
		return rsg.EvidenceAssertionRow{}, fmt.Errorf("%w: create evidence assertion %s: %v", rsg.ErrStore, in.ID, err)
	}
	return evidenceRowFromSQLC(row), nil
}

// GetVersionProjectFacts implements rsg.EvidencePort. A version that does
// not exist — and an id that could never name one — answer
// sciobjects.ErrVersionNotFound, so a caller cannot use this read to tell
// "there is no such version" from "that id is not a version id".
func (s *EvidenceStore) GetVersionProjectFacts(ctx context.Context, objectVersionID string) (rsg.VersionProjectFacts, error) {
	id, err := textUUID(objectVersionID)
	if err != nil {
		return rsg.VersionProjectFacts{}, sciobjects.ErrVersionNotFound
	}
	row, err := sqlc.New(s.pool).GetVersionProjectFacts(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return rsg.VersionProjectFacts{}, sciobjects.ErrVersionNotFound
	}
	if err != nil {
		return rsg.VersionProjectFacts{}, fmt.Errorf("%w: resolve version project facts: %v", rsg.ErrStore, err)
	}
	return rsg.VersionProjectFacts{
		ObjectVersionID:    row.ObjectVersionID,
		ObjectID:           row.ObjectID,
		ObjectType:         row.ObjectType,
		ProjectID:          row.ProjectID,
		ProjectVisibility:  row.ProjectVisibility,
		VisibilityPolicyID: uuidTextPtr(row.VisibilityPolicyID),
	}, nil
}

// GetKnowledgePublicationForVersion implements rsg.EvidencePort: the
// publication a version carries, with the rights document read raw — the
// token inside it is a Go parse (knowledgepublish.AudienceFor), never a SQL
// predicate. A version with no publication answers (zero, false, nil): the
// caller decides what that means, and this adapter does not filter by
// audience.
func (s *EvidenceStore) GetKnowledgePublicationForVersion(ctx context.Context, objectVersionID string) (rsg.KnowledgePublicationFacts, bool, error) {
	id, err := textUUID(objectVersionID)
	if err != nil {
		return rsg.KnowledgePublicationFacts{}, false, nil
	}
	row, err := sqlc.New(s.pool).GetKnowledgePublicationForVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return rsg.KnowledgePublicationFacts{}, false, nil
	}
	if err != nil {
		return rsg.KnowledgePublicationFacts{}, false, fmt.Errorf("%w: resolve publication for version: %v", rsg.ErrStore, err)
	}
	doc, parseErr := rights.Parse(row.RightsJson)
	return rsg.KnowledgePublicationFacts{
		ID:                 row.ID,
		PID:                row.Pid,
		PublicVersion:      row.PublicVersion,
		ObjectVersionID:    row.ObjectVersionID,
		ProjectID:          row.ProjectID,
		ProjectVisibility:  row.ProjectVisibility,
		VisibilityPolicyID: uuidTextPtr(row.VisibilityPolicyID),
		Rights:             doc,
		RightsValid:        parseErr == nil,
	}, true, nil
}

// ListPublishedEvidence implements the network evidence read the published
// knowledge document renders: the assertions against one published object
// version that the network may see, newest first, with the two facts the
// classification is computed from (the asserting project and the review
// state).
//
// It reads one row beyond the limit, so it can report whether the section
// was cut instead of handing back a short list that looks complete. The
// visibility predicate is applied HERE as well as in Go
// (evidencenetwork.Build): the query is the strategy that keeps the read
// cheap, and the rule is applied to what comes back.
//
// A target version id that is not a uuid names nothing and answers an empty
// section — the same answer a version with no public evidence gets, so this
// read is not an existence oracle either.
func (s *EvidenceStore) ListPublishedEvidence(ctx context.Context, targetObjectVersionID, targetProjectID string, limit int) ([]evidencenetwork.Assertion, bool, error) {
	if limit <= 0 {
		limit = evidencenetwork.MaxAssertions
	}
	versionID, err := textUUID(targetObjectVersionID)
	if err != nil {
		return nil, false, nil
	}
	projectID, err := textUUID(targetProjectID)
	if err != nil {
		return nil, false, nil
	}
	rows, err := sqlc.New(s.pool).ListPublishedEvidenceForTarget(ctx, sqlc.ListPublishedEvidenceForTargetParams{
		ObjectVersionID: versionID,
		TargetProjectID: projectID,
		RowLimit:        int32(limit) + 1, //nolint:gosec // limit is bounded by the caller's constant
	})
	if err != nil {
		return nil, false, fmt.Errorf("%w: list published evidence: %v", knowledgepublish.ErrStore, err)
	}
	truncated := len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	out := make([]evidencenetwork.Assertion, 0, len(rows))
	for _, row := range rows {
		out = append(out, evidencenetwork.Assertion{
			ID:                      row.ID,
			Relation:                row.RelationType,
			Stance:                  evidencenetwork.StanceOf(row.RelationType),
			EvidenceType:            row.EvidenceType,
			Directness:              row.Directness,
			InferenceNature:         row.InferenceNature,
			Scope:                   row.Scope,
			ReasoningNote:           row.ReasoningNote,
			ReviewState:             row.ReviewState,
			AssertingProjectID:      row.ProjectID,
			TargetObjectVersionID:   row.TargetObjectVersionID,
			EvidenceObjectVersionID: row.EvidenceObjectVersionID,
			CreatedAt:               knowledgepublish.FormatInstant(row.CreatedAt.Time),
			SourceProjectVisibility: row.SourceProjectVisibility,
		})
	}
	return out, truncated, nil
}

// evidenceRowFromSQLC converts the stored row to the port's shape. The
// review state is READ BACK rather than echoed from the request: the column
// default is what a new assertion carries, and reporting an invented value
// would make the response a claim about a row nobody read.
func evidenceRowFromSQLC(row sqlc.EvidenceAssertion) rsg.EvidenceAssertionRow {
	return rsg.EvidenceAssertionRow{
		ID:                      pgUUIDToText(row.ID),
		ProjectID:               pgUUIDToText(row.ProjectID),
		StateID:                 pgUUIDToText(row.StateID),
		TargetObjectVersionID:   pgUUIDToText(row.TargetObjectVersionID),
		EvidenceObjectVersionID: pgUUIDToText(row.EvidenceObjectVersionID),
		RelationType:            row.RelationType,
		EvidenceType:            row.EvidenceType,
		Scope:                   row.Scope,
		Directness:              row.Directness,
		InferenceNature:         row.InferenceNature,
		ReasoningNote:           derefText(row.ReasoningNote),
		ReviewState:             row.ReviewState,
		EvidenceOrigin:          row.EvidenceOrigin,
		Visibility:              row.Visibility,
		CreatedBy:               pgUUIDToText(row.CreatedBy),
		CreatedAt:               row.CreatedAt.Time,
	}
}

// scopeBytes renders the assertion's scope for storage: a missing value is
// the empty object the column defaults to. The service has already refused a
// scope that is not a JSON object (domain.EvidenceAssertion.Validate) and the
// database re-checks it (00058's scope CHECK), so this is a rendering step,
// not a validation one.
func scopeBytes(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("{}")
	}
	return raw
}

// derefText renders a NULL text column as "" (the read shapes carry plain
// strings).
func derefText(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
