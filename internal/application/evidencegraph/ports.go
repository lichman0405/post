package evidencegraph

import (
	"context"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
)

// ObjectStore is the scientific-object slice this read needs. The
// production implementation is persistence.ScientificObjectStore — the same
// adapter the RSG object page reads through, reused so the object and its
// version log have one definition.
//
// The reader this package's reads carry is projects.Reader — the identity the
// transports already resolve from the request for every other
// visibility-aware read (T0106). This package defines no second identity
// concept: ADR-024 makes the reader an explicit INPUT of the evidence read,
// not a new model of who a caller is.
type ObjectStore interface {
	// GetObject resolves one object, or sciobjects.ErrObjectNotFound.
	GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error)
	// ListVersions returns the object's version log, oldest first. A store
	// failure is an error, never an empty log: an empty page and a broken
	// read must not look alike.
	ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error)
}

// AssertionStore is the evidence table's slice this read needs. The
// production implementation is persistence.EvidenceGraphStore, over the
// pre-provisioned per-target query (internal/persistence/queries/evidence.sql,
// ListEvidenceAssertionsForTarget).
type AssertionStore interface {
	// ListForTargetVersion returns the assertions pinning one target object
	// version that readerUserID may be RENDERED, oldest first. The reader is
	// part of the query, not a filter applied afterwards: the audience rule
	// is the read's (ADR-024), and the port states it so an adapter cannot
	// answer a wider set than the reader's. An EMPTY user id is the
	// anonymous reader and must answer the publicly visible rows only — the
	// fail-closed branch, never "everything".
	//
	// Stance is deliberately NOT set by the store: the projection derives it
	// from Relation (one rule, one place). An empty list is the answer for a
	// version with no assertions; a failure is an error.
	ListForTargetVersion(ctx context.Context, objectVersionID, readerUserID string) ([]evidence.Assertion, error)
}

// RelationStore is the RSG relation slice the hypothesis page's second
// section needs. The production implementation is persistence.RelationStore
// — the adapter the object detail page's relations tab already reads
// through.
type RelationStore interface {
	// ListVersionsForObject returns every relation version whose source or
	// target endpoint pins a version of the object, NEWEST FIRST, with the
	// endpoint labels resolved. The order is part of this port's contract:
	// the subordinate-claim read keeps the first row it sees per relation
	// (the relation's current head) rather than an arbitrary version.
	ListVersionsForObject(ctx context.Context, projectID, objectID string) ([]rsg.ObjectRelationVersion, error)
}

// Deps carries the adapters the service reads through.
type Deps struct {
	Objects    ObjectStore
	Assertions AssertionStore
	Relations  RelationStore
}
