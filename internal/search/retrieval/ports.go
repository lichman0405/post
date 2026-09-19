package retrieval

import (
	"context"

	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
)

// The recall signals. These are the three signals docs/14 §2 names for the
// text/facet half of retrieval, plus the traversal. They are a closed set so
// a candidate's explanation can be read without knowing which revision of
// this package produced it.
const (
	// SignalFullText is the PostgreSQL FTS signal.
	SignalFullText = "full_text"
	// SignalVector is the pgvector similarity signal.
	SignalVector = "vector"
	// SignalFacets is the structured-filter signal (no text predicate).
	SignalFacets = "facets"
	// SignalGraph is the relation-traversal signal: the candidate is not a
	// recalled document but a node reached from one.
	SignalGraph = "graph"
)

// Store is the retrieval's read port: the three document reads and the two
// graph reads, each taking the scope it must be executed under.
//
// The scope travels WITH the query rather than being applied to the result,
// which is the whole point of the port's shape: an implementation that
// forgot it could not satisfy the interface, because the parameter it needs
// is not something it can supply itself. The production implementation is
// store.go (pgx over the checked-in sqlc queries); the unit suite drives a
// fake.
type Store interface {
	// FullText returns the documents matching q.Query under q's scope and
	// narrowing, best first.
	FullText(ctx context.Context, q DocumentQuery) ([]DocumentHit, error)
	// Vector returns the documents nearest the query embedding under q's
	// scope and narrowing, nearest first. Implementations must restrict the
	// scan to rows whose stored vector provenance is the query's (see the
	// query's own comment): a distance against another model's vector is not
	// a weak match, it is not a match.
	Vector(ctx context.Context, q VectorQuery) ([]DocumentHit, error)
	// Facets returns the documents matching q's structured filter under
	// q's scope, in the query's own stable order. The query it runs carries
	// no text predicate at all.
	Facets(ctx context.Context, q DocumentQuery) ([]DocumentHit, error)
	// ObjectVersions returns the node rows for the given object version
	// ids, restricted to the scope's projects.
	ObjectVersions(ctx context.Context, scope search.Scope, versionIDs []string) ([]GraphObject, error)
	// SeedObjectVersions maps recalled publication pids to the object
	// version each publication pins, which is what the traversal starts
	// from. A pid that names no publication is simply absent from the
	// result.
	SeedObjectVersions(ctx context.Context, scope search.Scope, pids []string) ([]SeedVersion, error)
	// AdjacentRelations returns one hop: the edges touching any of the
	// given object version ids, at each relation's newest version, whose
	// relation project and both endpoint projects are in the scope.
	AdjacentRelations(ctx context.Context, scope search.Scope, versionIDs []string) ([]GraphEdge, error)
}

// DocumentQuery is one document read: the caller's scope, the narrowing the
// plan and the caller's filters impose, and the page size.
type DocumentQuery struct {
	// Scope is the authorization context. It must be resolved
	// (search.ResolveScope) — the retriever refuses to issue a read
	// otherwise.
	Scope search.Scope
	// Query is the full-text query. It is empty for a facets-only read.
	Query string
	// EntityTypes narrows the read to the projection's entity types. Nil
	// means no narrowing.
	EntityTypes []string
	// StructuredFilter is a JSON object matched against the row's facets as
	// jsonb containment. Nil means no narrowing.
	StructuredFilter []byte
	// PublicOnly narrows the read to rows the read query returns to
	// anybody. It can only ever remove rows.
	PublicOnly bool
	// PageSize is the maximum number of rows to return.
	PageSize int
}

// VectorQuery is one vector read: a DocumentQuery plus the query vector and
// the identity of the model that produced it.
type VectorQuery struct {
	DocumentQuery
	// Embedding is the query vector in pgvector's text input syntax
	// (embedding.FormatVector).
	Embedding string
	// Model is the identity of the embedder that produced it — the canonical
	// embedding.Model, not a copy of it: this port asks for the one identity
	// type the platform has, so "the model that embedded the query" and "the
	// model whose provenance the column must carry" cannot drift into two
	// spellings. The read is restricted to rows whose stored provenance
	// equals this identity.
	Model embedding.Model
}

// DocumentHit is one recalled projection row.
type DocumentHit struct {
	// Ref is the row's entity_ref key: "kind:identity" (search.EntityRef).
	Ref string
	// EntityType is the projection's entity type.
	EntityType string
	// Visibility is the row's own visibility, as the projection computed it.
	Visibility string
	// ProjectID is the owning project's uuid text ("" for a row with no
	// project).
	ProjectID string
	// Title is the row's title.
	Title string
	// Structured is the row's facet object, verbatim.
	Structured []byte
	// Score is the signal's own score — ts_rank, or cosine similarity. It
	// is reported for explanation only: two signals' scores are not
	// comparable quantities, which is why fusion below is rank-based.
	Score float64
}

// GraphObject is one node reached by traversal: a scientific object version.
type GraphObject struct {
	// ObjectVersionID is scientific_object_versions.id, the graph's
	// addressing unit and the pin a candidate cites.
	ObjectVersionID string
	// ObjectID is scientific_objects.id.
	ObjectID string
	// VersionNo is the version's ordinal within its object.
	VersionNo int
	// Title is the version's title.
	Title string
	// ObjectType is scientific_objects.object_type: "claim", "material",
	// "finding", "experiment", ...
	ObjectType string
	// ProjectID is the owning project's uuid text.
	ProjectID string
}

// GraphEdge is one relation version: an edge between two object versions.
type GraphEdge struct {
	// RelationType is relation_versions.relation_type (the canonical
	// vocabulary of internal/rsg/relationcatalog).
	RelationType string
	// SourceVersionID and TargetVersionID are the endpoint versions.
	SourceVersionID string
	TargetVersionID string
	// SourceProjectID and TargetProjectID are the endpoints' projects.
	SourceProjectID string
	TargetProjectID string
}

// SeedVersion is the object version a recalled publication pins: the graph's
// addressing unit, read from a document candidate's own identity.
type SeedVersion struct {
	// Pid is the publication pid the version was resolved from, which is
	// how the caller matches the seed to the document candidate that
	// supplied it (a publication and its version are 1:1 — 00083 — so a
	// pid resolves to exactly one row).
	Pid string
	// GraphObject is the pinned version.
	GraphObject GraphObject
}
