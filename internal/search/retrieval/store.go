package retrieval

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/search"
)

// SQLStore is the production Store: the checked-in sqlc queries
// (internal/persistence/queries/search.sql) over a pgx handle.
//
// It is a thin adapter and deliberately nothing more. Every predicate that
// grants or refuses a row lives in the SQL — the access predicate, the entity
// narrowing, the facet containment, the provenance match on the vector, the
// three-way project filter on a hop — so this file's whole job is to hand the
// scope and the narrowing across the boundary unchanged and to convert
// between uuid text and the driver's type. A rule added here instead of in
// the query would be a rule applied AFTER the read, which is the shape
// docs/21 §9 forbids.
type SQLStore struct {
	q *sqlc.Queries
}

// NewSQLStore builds the store over a pgx handle: a *pgxpool.Pool, a pgx.Tx,
// or anything else satisfying sqlc.DBTX. Taking the interface rather than the
// pool is what lets the same store run inside a transaction (a caller that
// has to read a corpus consistently) and in a test's transaction.
func NewSQLStore(db sqlc.DBTX) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("retrieval: a store needs a database handle")
	}
	return &SQLStore{q: sqlc.New(db)}, nil
}

// FullText implements Store.
func (s *SQLStore) FullText(ctx context.Context, q DocumentQuery) ([]DocumentHit, error) {
	if err := checkScope(q.Scope); err != nil {
		return nil, err
	}
	rows, err := s.q.SearchDocumentsFullText(ctx, sqlc.SearchDocumentsFullTextParams{
		Query:             q.Query,
		EntityTypes:       noNarrowing(q.EntityTypes),
		StructuredFilter:  noFilter(q.StructuredFilter),
		PublicOnly:        q.PublicOnly,
		AllowedProjectIds: q.Scope.AllowedProjectUUIDs(),
		PageSize:          int32(q.PageSize),
	})
	if err != nil {
		return nil, err
	}
	out := make([]DocumentHit, 0, len(rows))
	for _, row := range rows {
		out = append(out, DocumentHit{
			Ref:        row.EntityRef,
			EntityType: row.EntityType,
			Visibility: row.Visibility,
			ProjectID:  uuidText(row.ProjectID),
			Title:      row.Title,
			Structured: row.Structured,
			Score:      float64(row.Rank),
		})
	}
	return out, nil
}

// Vector implements Store.
func (s *SQLStore) Vector(ctx context.Context, q VectorQuery) ([]DocumentHit, error) {
	if err := checkScope(q.Scope); err != nil {
		return nil, err
	}
	rows, err := s.q.SearchDocumentsByVector(ctx, sqlc.SearchDocumentsByVectorParams{
		Embedding:         q.Embedding,
		EmbeddingProvider: q.Model.Provider,
		EmbeddingModel:    q.Model.Name,
		EmbeddingVersion:  q.Model.Version,
		EntityTypes:       noNarrowing(q.EntityTypes),
		StructuredFilter:  noFilter(q.StructuredFilter),
		PublicOnly:        q.PublicOnly,
		AllowedProjectIds: q.Scope.AllowedProjectUUIDs(),
		PageSize:          int32(q.PageSize),
	})
	if err != nil {
		return nil, err
	}
	out := make([]DocumentHit, 0, len(rows))
	for _, row := range rows {
		out = append(out, DocumentHit{
			Ref:        row.EntityRef,
			EntityType: row.EntityType,
			Visibility: row.Visibility,
			ProjectID:  uuidText(row.ProjectID),
			Title:      row.Title,
			Structured: row.Structured,
			Score:      row.Similarity,
		})
	}
	return out, nil
}

// Facets implements Store.
func (s *SQLStore) Facets(ctx context.Context, q DocumentQuery) ([]DocumentHit, error) {
	if err := checkScope(q.Scope); err != nil {
		return nil, err
	}
	rows, err := s.q.SearchDocumentsByFacets(ctx, sqlc.SearchDocumentsByFacetsParams{
		EntityTypes:       noNarrowing(q.EntityTypes),
		StructuredFilter:  noFilter(q.StructuredFilter),
		PublicOnly:        q.PublicOnly,
		AllowedProjectIds: q.Scope.AllowedProjectUUIDs(),
		PageSize:          int32(q.PageSize),
	})
	if err != nil {
		return nil, err
	}
	out := make([]DocumentHit, 0, len(rows))
	for _, row := range rows {
		out = append(out, DocumentHit{
			Ref:        row.EntityRef,
			EntityType: row.EntityType,
			Visibility: row.Visibility,
			ProjectID:  uuidText(row.ProjectID),
			Title:      row.Title,
			Structured: row.Structured,
			// No score: this signal has no text to rank against, and
			// inventing one would be inventing a relevance judgement
			// (CLAUDE.md §9.13). Its contribution to fusion is its
			// position, which the query fixes by entity_ref.
			Score: 0,
		})
	}
	return out, nil
}

// ObjectVersions implements Store.
func (s *SQLStore) ObjectVersions(ctx context.Context, scope search.Scope, versionIDs []string) ([]GraphObject, error) {
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
	rows, err := s.q.ListScopeObjectVersions(ctx, sqlc.ListScopeObjectVersionsParams{
		VersionIds: ids,
		ProjectIds: scope.AllowedProjectUUIDs(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]GraphObject, 0, len(rows))
	for _, row := range rows {
		out = append(out, GraphObject{
			ObjectVersionID: uuidText(row.ObjectVersionID),
			ObjectID:        uuidText(row.ObjectID),
			VersionNo:       int(row.VersionNo),
			Title:           row.Title,
			ObjectType:      row.ObjectType,
			ProjectID:       row.ProjectID,
		})
	}
	return out, nil
}

// SeedObjectVersions implements Store.
func (s *SQLStore) SeedObjectVersions(ctx context.Context, scope search.Scope, pids []string) ([]SeedVersion, error) {
	if err := checkScope(scope); err != nil {
		return nil, err
	}
	if len(pids) == 0 {
		return nil, nil
	}
	rows, err := s.q.SearchSeedObjectVersions(ctx, pids)
	if err != nil {
		return nil, err
	}
	out := make([]SeedVersion, 0, len(rows))
	for _, row := range rows {
		out = append(out, SeedVersion{
			Pid: row.Pid,
			GraphObject: GraphObject{
				ObjectVersionID: uuidText(row.ObjectVersionID),
				ObjectID:        uuidText(row.ObjectID),
				VersionNo:       int(row.VersionNo),
				Title:           row.Title,
				ObjectType:      row.ObjectType,
				ProjectID:       row.ProjectID,
			},
		})
	}
	return out, nil
}

// AdjacentRelations implements Store.
func (s *SQLStore) AdjacentRelations(ctx context.Context, scope search.Scope, versionIDs []string) ([]GraphEdge, error) {
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
	rows, err := s.q.ListScopeAdjacentRelationVersions(ctx, sqlc.ListScopeAdjacentRelationVersionsParams{
		VersionIds: ids,
		ProjectIds: scope.AllowedProjectUUIDs(),
	})
	if err != nil {
		return nil, err
	}
	out := make([]GraphEdge, 0, len(rows))
	for _, row := range rows {
		out = append(out, GraphEdge{
			RelationType:    row.RelationType,
			SourceVersionID: uuidText(row.SourceObjectVersionID),
			TargetVersionID: uuidText(row.TargetObjectVersionID),
			SourceProjectID: row.SourceProjectID,
			TargetProjectID: row.TargetProjectID,
		})
	}
	return out, nil
}

// checkScope refuses an unresolved scope at the store boundary.
//
// The retriever already refuses one (Retrieve returns ErrNoScope), and this
// is the second copy on purpose: the queries this file issues express the
// scope ONLY through @allowed_project_ids, and the predicate they carry
// returns public rows when that array is empty. That is the right predicate —
// it is the read query's own, unchanged — but it means an unauthenticated
// call that reached this file with a zero Scope would read the public corpus
// and report success. Refusing it here is what makes "the zero Scope is not a
// scope" a property of the store rather than a convention of its caller.
func checkScope(scope search.Scope) error {
	if !scope.Authenticated() {
		return ErrNoScope
	}
	return nil
}

// noNarrowing spells "no entity narrowing" the one way the SQL understands.
//
// The query guards the parameter with `@entity_types::text[] IS NULL`, and Go
// has two ways to say a list is empty. Nil reaches the server as NULL and the
// guard is true; a NON-NIL EMPTY slice reaches it as the empty array, which is
// not NULL, so the guard is false and `entity_type = ANY('{}')` matches no row.
// A caller that passed an empty slice meaning "no restriction" would therefore
// get an empty result that looks exactly like an empty corpus — a silent
// failure of the loudest kind, and one no compiler can catch.
//
// This is not an access rule and belongs here for a different reason: it is
// the encoding of a parameter, and this file is the only place the parameter
// crosses into SQL.
func noNarrowing(entityTypes []string) []string {
	if len(entityTypes) == 0 {
		return nil
	}
	return entityTypes
}

// noFilter is noNarrowing for the facet filter, and it is not merely the same
// fact twice: an empty NON-NIL byte slice is not NULL either, and `”::jsonb`
// is not an empty filter — the server rejects it as malformed JSON, so the
// read fails with a parse error instead of returning rows.
func noFilter(filter []byte) []byte {
	if len(filter) == 0 {
		return nil
	}
	return filter
}

// uuidText renders a uuid column value as text ("" for NULL). It is the same
// six lines internal/search's projector and internal/events' publisher each
// carry, copied for the same reason they did: the alternative is exporting
// the helper from one of those packages, whose scope is not this one's.
func uuidText(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// uuidList parses uuid text forms into the driver's type.
//
// A malformed id is an error, never a dropped element: the ids come from rows
// this process just read, so one that does not parse means the value is not
// what this code believes it is, and silently narrowing the traversal would
// hide that behind a smaller result set.
func uuidList(ids []string) ([]pgtype.UUID, error) {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		var u pgtype.UUID
		if err := u.Scan(id); err != nil {
			return nil, fmt.Errorf("retrieval: object version id %q: %w", id, err)
		}
		out = append(out, u)
	}
	return out, nil
}
