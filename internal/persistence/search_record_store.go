package persistence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// SearchRecordStore writes the record of one answered search (T0906,
// migration 00121, docs/22 §8: the server saves the query plan, the selected
// entity ids and the answer citations).
//
// # One method, and no reader
//
// The store writes a record and returns the search id the contract addresses
// searches by (POST /search/{searchId}:start-project). It has no read method:
// the only reader in the contract resumes a search's sources, and that flow
// is not this task's — a read added here for nobody would be a query shape
// frozen before its caller exists. The integration test reads the row with
// its own SQL, which is also the stronger check: it shows the columns as a
// row rather than through a projection this file chose.
//
// # Why the arguments are the pieces and not an answer type
//
// The row is a composite of FOUR producers: the query and filters came from
// the caller, the plan from the planner, the signal report and selected refs
// from the retrieval and the ranking, the answer document from the answer
// layer. A record type that named them all would have to live in one of those
// packages and re-declare the others' types, so the store takes the pieces
// and this struct is what the API handler fills in. Every one of them is a
// value the caller already produced; see queries/search.sql for why nothing
// here derives one from another.
type SearchRecord struct {
	// ActorID is the user the search ran as. Required: the scope that decided
	// which rows were readable was resolved from it (internal/search/scope.go),
	// so a record with no actor is a search whose reader cannot be named.
	ActorID string
	// Query is the question the caller asked, verbatim. Required and
	// non-blank (the column's CHECK).
	Query string
	// Filters is the caller's structured filters document, verbatim, or nil
	// when the request carried none. Stored as sent: what the filters MEANT
	// is the planner's reading of them, recorded in Plan.
	Filters []byte
	// Plan is the planner's plan document, or nil when no planner ran (a
	// deployment with no provider plans nothing and still searches).
	Plan []byte
	// Signals is the retrieval's SignalReport[] — required, because it is
	// what the answer's coverage limitations are derived from.
	Signals []byte
	// SelectedRefs are the ranked entity version refs the result selected, in
	// rank order. The answer may cite these and nothing else.
	SelectedRefs []string
	// Citations are the refs the answer cites. A ref outside SelectedRefs is
	// refused by the table's CHECK (citations <@ selected_refs) — the second,
	// database-side refusal of the invariant the answer package's guard
	// enforces in Go, so a writer that never read the guard still cannot
	// record an ungrounded citation.
	Citations []string
	// Answer is the answer layer's canonical document
	// (internal/search/answer.Answer.CanonicalJSON).
	Answer []byte
}

// ErrSearchRecord is the store's error class for a record it refused to
// write as given — a missing actor or query, or a document that is not JSON.
// It is separate from a database failure because the two are different
// mistakes: this one is the caller's, and it is knowable before the write.
var ErrSearchRecord = errors.New("persistence: search record")

// SearchRecordStore writes search_records rows over any sqlc executor — the
// production value is the pgx pool cmd/api already builds.
type SearchRecordStore struct {
	queries *sqlc.Queries
}

// NewSearchRecordStore wires the writer.
func NewSearchRecordStore(db sqlc.DBTX) *SearchRecordStore {
	return &SearchRecordStore{queries: sqlc.New(db)}
}

// Save writes one search record and returns its id, the searchId the contract
// addresses (specs/api/openapi.yaml, /search/{searchId}:start-project).
//
// The row is inserted once and never updated: the answer was true of the
// corpus at the moment it was produced, and a record that could be rewritten
// would be a record that could be made to agree with a later question.
//
// The refusal of an ungrounded citation happens in the database (the CHECK),
// not here. This method deliberately does NOT pre-check citations against
// selected refs: a second, Go-side copy of the rule would be the third
// implementation of it (the generator's guard, the CHECK, and the copy) and
// the only thing three copies of a rule guarantee is that they will one day
// disagree. A record that violates the invariant comes back as a constraint
// error naming the constraint, which is a programming error in the caller and
// not a state a validated caller can reach.
func (s *SearchRecordStore) Save(ctx context.Context, rec SearchRecord) (string, error) {
	if rec.ActorID == "" {
		return "", fmt.Errorf("%w: no actor", ErrSearchRecord)
	}
	if rec.Query == "" {
		return "", fmt.Errorf("%w: no query", ErrSearchRecord)
	}
	actorID, err := textUUID(rec.ActorID)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrSearchRecord, err)
	}
	// A jsonb column refuses invalid JSON with a database error whose text is
	// a parser position; checking here names the field instead. The check is
	// a nil-safe pass-through, not a second schema: any valid JSON document
	// is accepted, as the column's own definition says.
	signals, err := jsonDocument("signals", rec.Signals, false)
	if err != nil {
		return "", err
	}
	answer, err := jsonDocument("answer", rec.Answer, false)
	if err != nil {
		return "", err
	}
	filters, err := jsonDocument("filters", rec.Filters, true)
	if err != nil {
		return "", err
	}
	plan, err := jsonDocument("plan", rec.Plan, true)
	if err != nil {
		return "", err
	}
	row, err := s.queries.InsertSearchRecord(ctx, sqlc.InsertSearchRecordParams{
		ActorID:      actorID,
		Query:        rec.Query,
		Filters:      filters,
		Plan:         plan,
		Signals:      signals,
		SelectedRefs: emptyNotNil(rec.SelectedRefs),
		Citations:    emptyNotNil(rec.Citations),
		Answer:       answer,
	})
	if err != nil {
		return "", fmt.Errorf("persistence: search record: %w", err)
	}
	return uuidString(row.ID), nil
}

// jsonDocument validates one jsonb argument. A nil document is allowed only
// for the columns that are nullable (nullable true): an empty NOT NULL column
// would be a NULL refused by the database, and reporting it here as "which
// field is missing" is the difference between an operator-readable error and
// a constraint violation.
func jsonDocument(field string, doc []byte, nullable bool) ([]byte, error) {
	if doc == nil {
		if nullable {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: %s: no document", ErrSearchRecord, field)
	}
	if !json.Valid(doc) {
		return nil, fmt.Errorf("%w: %s: not a JSON document", ErrSearchRecord, field)
	}
	return doc, nil
}

// emptyNotNil renders a nil slice as the empty array. The two columns are NOT
// NULL, so nil would be refused; "this search selected nothing" is a real
// state (a fallback over an empty result) and must be recorded as an empty
// list rather than as an absent one.
func emptyNotNil(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// uuidString renders a uuid for the wire. An unset value renders empty, which
// cannot happen for a RETURNING id from an INSERT that succeeded.
func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return u.String()
}
