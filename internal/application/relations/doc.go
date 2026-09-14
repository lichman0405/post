// Package relations orchestrates the typed relation use cases against the
// Repository port (docs/52: application orchestrates against ports;
// adapters live in internal/persistence).
//
// A relation is a typed, directed, version-pinned RSG edge (docs/07 §3):
// every version pins its source and target to exact scientific object
// versions, its relation_type is validated against the relation catalog
// (internal/rsg/relationcatalog, docs/44), and version rows are immutable
// on two layers at once — the port offers no update path and the database
// rejects in-place UPDATE/DELETE (migrations 00014/00015).
//
// Authorization is not this package's job: actors and project membership
// checks belong to the consuming API task, which passes only resolved
// identities in (T0203).
package relations
