package ranking

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/search"
)

// SQLStore is the production FactorStore: two checked-in queries
// (internal/persistence/queries/search.sql) over a pgx handle.
//
// Like retrieval.SQLStore it is a thin adapter and deliberately nothing more:
// every predicate that admits or refuses a row lives in the SQL, and this
// file's job is to hand the scope across unchanged, convert between uuid text
// and the driver's type, and turn the scope's refusals into an ABSENCE rather
// than a zero row.
type SQLStore struct {
	q *sqlc.Queries
}

// NewSQLStore builds the store over a pgx handle (a *pgxpool.Pool, a pgx.Tx,
// or anything satisfying sqlc.DBTX). A nil handle is refused: the alternative
// is a nil-pointer panic at the first ranking.
func NewSQLStore(db sqlc.DBTX) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("ranking: a store needs a database handle")
	}
	return &SQLStore{q: sqlc.New(db)}, nil
}

// VersionsForPids implements FactorStore.
//
// The publication→version mapping is read with the SAME query the traversal's
// seed step uses (SearchSeedObjectVersions), and it is deliberately not a
// second implementation of that join: a publication pins exactly one version
// (00083), and two queries disagreeing about which would rank a document by
// another version's evidence.
//
// That query carries no scope predicate, and it must not: its input is a list
// of pids the caller has already been authorized to see, not a scope. The
// scope is therefore applied HERE, to its output — and the output is SPLIT,
// not filtered. A version whose project the scope covers is admitted, exactly
// as retrieval.seeds admits it. A version whose project it does not cover is
// REFUSED: reported in the second map, with its id, so the ranking can say
// "the version exists and is outside the caller's scope" rather than "the
// candidate resolves to no version". The two rank differently (unknown vs
// unversioned) and the second statement would be false.
//
// The split widens nothing: the refused map carries no fact about the version
// beyond the id the publication already pins for any reader of the document,
// and the version's facts are still refused by Factors (fail-closed, in SQL) —
// so a candidate the traversal refused to seed still cannot become rankable
// by facts the ranking resolved by another route. What changes is only that
// the refusal is NAMED.
//
// # The case this deliberately does NOT decide (reported to the Supervisor)
//
// The rule above has one consequence worth naming, because it is a
// product/permission question and not an implementation detail: a PUBLIC
// network document whose publication belongs to a project the caller is not a
// member of is recalled by the retrieval (a document is read by its own
// visibility, docs/14) and its facts are then refused here, so every
// database-backed factor reports `unknown` and it ranks at the bottom. The
// alternative reading — rank a public document by the public external
// evidence attached to its version, since docs/10 §7 shows the network
// Reviewed and Unreviewed External Evidence on a published Knowledge Object —
// is defensible, and it is NOT implemented, because choosing between the two
// is a visibility/permission decision (docs/23 §5, CLAUDE.md §5 L3) and not
// this task's to make. This task implements the conservative reading, and is
// consistent with the retrieval that precedes it: the same candidate is
// seeded by the traversal only when its project is in scope. If the Supervisor
// decides the wider reading is the product's, the change is the scope
// predicate on that one query plus its fixture — not a redesign.
func (s *SQLStore) VersionsForPids(ctx context.Context, scope search.Scope, pids []string) (map[string]string, map[string]string, error) {
	if err := checkScope(scope); err != nil {
		return nil, nil, err
	}
	if len(pids) == 0 {
		return map[string]string{}, map[string]string{}, nil
	}
	allowed := make(map[string]bool)
	for _, id := range scope.AllowedProjectIDs() {
		allowed[id] = true
	}
	rows, err := s.q.SearchSeedObjectVersions(ctx, pids)
	if err != nil {
		return nil, nil, err
	}
	admitted := make(map[string]string, len(rows))
	refused := make(map[string]string)
	for _, row := range rows {
		id := uuidText(row.ObjectVersionID)
		if id == "" {
			continue
		}
		if !allowed[row.ProjectID] {
			refused[row.Pid] = id
			continue
		}
		admitted[row.Pid] = id
	}
	return admitted, refused, nil
}

// Factors implements FactorStore.
//
// The read is scope-filtered in SQL (SearchRankingFacts' own WHERE), and a
// version the scope does not admit is ABSENT from the result rather than
// present with zero counts. That distinction is the whole contract: the
// ranking reports an absent row as `unknown` and a present row of zeros as
// `none`, and a store that returned zeros for a refused version would make
// "we may not read this" indistinguishable from "there is nothing there".
func (s *SQLStore) Factors(ctx context.Context, scope search.Scope, versionIDs []string) ([]VersionFacts, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	ids, err := uuidList(versionIDs)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.q.SearchRankingFacts(ctx, sqlc.SearchRankingFactsParams{
		VersionIds: ids,
		ProjectIds: scope.AllowedProjectUUIDs(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]VersionFacts, 0, len(rows))
	for _, row := range rows {
		out = append(out, VersionFacts{
			ObjectVersionID:         uuidText(row.ObjectVersionID),
			ObjectID:                row.ObjectID,
			VersionNo:               int(row.VersionNo),
			NewestVersionNo:         int(row.NewestVersionNo),
			LifecycleState:          row.LifecycleState,
			IsNewest:                row.IsNewest,
			EvidenceAssertions:      int(row.EvidenceAssertions),
			EvidenceReviewed:        int(row.EvidenceReviewed),
			EvidenceRejected:        int(row.EvidenceRejected),
			EvidenceDirect:          int(row.EvidenceDirect),
			EvidenceSupporting:      int(row.EvidenceSupporting),
			EvidenceContradicting:   int(row.EvidenceContradicting),
			Reproduces:              int(row.Reproduces),
			ReproducesIndependent:   int(row.ReproducesIndependent),
			FailsToReproduce:        int(row.FailsToReproduce),
			ReviewsScientific:       int(row.ReviewsScientific),
			ReviewsApproved:         int(row.ReviewsApproved),
			ReviewsChangesRequested: int(row.ReviewsChangesRequested),
			ContradictingRelations:  int(row.ContradictingRelations),
		})
	}
	return out, nil
}

// checkScope refuses an unresolved scope at the store boundary.
//
// It is the second copy of the retriever's rule on purpose, and the reason is
// the same one retrieval.checkScope gives: the queries this file issues
// express the scope ONLY through @project_ids, so a zero Scope would reach
// the server as an empty array and the read would silently answer "no
// projects" — a ranking in which every candidate is `unknown`, reported as a
// success. "The zero Scope is not a scope" has to be a property of the store
// rather than a convention of its caller.
func checkScope(scope search.Scope) error {
	if !scope.Authenticated() {
		return ErrNoScope
	}
	return nil
}

// uuidText renders a uuid column value as text ("" for NULL). It is the same
// six lines retrieval.store.go carries, copied for the same reason that one
// copied it from the projector: the alternative is exporting the helper from
// a package whose scope is not this one's.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// uuidList parses uuid text forms into the driver's type.
//
// A malformed id is an error, never a dropped element: the ids come from
// candidates this process just read, so one that does not parse means the
// value is not what this code believes it is, and silently narrowing the read
// would hide that behind a shorter fact sheet — which the ranking would then
// report as `unknown` facts rather than as the defect it is.
func uuidList(ids []string) ([]pgtype.UUID, error) {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		var u pgtype.UUID
		if err := u.Scan(id); err != nil {
			return nil, fmt.Errorf("ranking: object version id %q: %w", id, err)
		}
		out = append(out, u)
	}
	return out, nil
}
