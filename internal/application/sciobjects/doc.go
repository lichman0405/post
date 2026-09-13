// Package sciobjects owns the scientific object use cases: creating an
// object together with its version 1, appending the next immutable version
// guarded by an expected_version compare-and-swap, and reading the version
// log (T0202, docs/08, docs/21 §4/§5, CLAUDE.md §9.8).
//
// The version log is append-only by design: the Repository port exposes no
// update path (an update of an existing version is unrepresentable here),
// the database rejects UPDATE/DELETE of version rows itself
// (infra/migrations/00014, 00015), and the stable conflict outcome of two
// concurrent version creations is ErrVersionConflict / EXPECTED_VERSION_MISMATCH
// (docs/45). The production adapter is persistence.ScientificObjectStore;
// wire schema validation (internal/rsg/schemareg) in the consuming API task.
package sciobjects
