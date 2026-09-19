// Package search owns the search/discovery surface per docs/14_SEARCH_DISCOVERY.md
// and ADR-010: answers cite pinned entity versions and show conflicts and
// limits. V1 runs on PostgreSQL FTS + pgvector (ADR-005).
//
// T0901 fills the projection half of it: the consumer that turns domain
// events into search_documents rows (projector.go), the table of which
// events address which entity (projection.go), the source reads that build
// a document (sources.go) and the rebuild that re-derives the whole index
// from those sources (rebuild.go). The read half is not here: querying and
// ranking are the canonical SearchDocuments query
// (internal/persistence/queries/search.sql, whose visibility filter is what
// a projected row's visibility means) and T0905's surface; vector retrieval
// and its embedding provider are T0902's, which is why every document this
// package writes leaves search_documents.embedding NULL.
package search
