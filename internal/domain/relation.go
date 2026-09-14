package domain

import (
	"encoding/json"
	"time"
)

// Relation is the container row of a typed RSG edge: identity and the
// project boundary only (docs/07 §3: every edge has its own global id).
// The edge's scientific content — type, pinned source/target object
// versions, scope/metadata — lives exclusively in the append-only
// RelationVersion log, so an edge row carries no content of its own.
// Canonical columns: infra/migrations/00006_relations.sql; 00025 adds the
// current_version_no head pointer.
type Relation struct {
	// ID is the uuid v4 text form (matches the relations.id uuid column).
	ID string
	// ProjectID is the research boundary the relation belongs to.
	ProjectID string
	// CurrentVersionNo is the materialized head of the version log
	// (docs/21 §5). It doubles as the compare-and-swap cell for
	// expected_version creation; readers that need the log's own truth
	// read the version rows.
	CurrentVersionNo int
	CreatedAt        time.Time
}

// RelationVersion is one immutable row of the append-only version log
// (relation_versions). Rows are never updated or deleted — the database
// itself rejects it (migrations 00014/00015) and the repository surface
// offers no update path; every change is a new version row (docs/21 §4).
type RelationVersion struct {
	// ID is the uuid v4 text form of the version row.
	ID string
	// RelationID names the container edge.
	RelationID string
	// VersionNo is the 1-based position in the edge's version log,
	// unique per relation.
	VersionNo int
	// StateID is the project state this version was created in (docs/07:
	// every change belongs to a state transition).
	StateID string
	// RelationType is the edge's type from the relation catalog
	// (docs/44); it is validated against the catalog on every write
	// (internal/rsg/relationcatalog).
	RelationType string
	// SourceObjectVersionID pins the source endpoint to one exact
	// scientific object version — edges are version-pinned, never
	// object-pinned (docs/07 §3).
	SourceObjectVersionID string
	// TargetObjectVersionID pins the target endpoint to one exact
	// scientific object version.
	TargetObjectVersionID string
	// Payload is the edge's scope/metadata, stored as exact bytes (jsonb
	// round-trips through the same textual JSON).
	Payload json.RawMessage
	// IntegrityHash is the sha256 hex digest of the stored payload's
	// canonical jsonb text form, computed server-side at insert
	// (docs/23 §3: never caller-supplied). jsonb normalizes key order and
	// whitespace, so the hash pins the bytes the row actually holds: a
	// read payload always re-hashes to its stored hash.
	IntegrityHash string
	CreatedBy     string
	CreatedAt     time.Time
}
