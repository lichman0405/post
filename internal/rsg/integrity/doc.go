// Package integrity is the machine engine of the Pull Request integrity
// review (T0403): it checks one PR proposal across the six canonical
// dimensions — schema, provenance, dependency, rights, visibility, blob —
// and returns a machine result, never a human judgment.
//
// Every check is a pure function of the injected snapshot: the engine
// never reads or writes storage, so the same snapshot always derives the
// same report (inputs are sorted canonically; the report order is the
// declaration order of specs). The application layer assembles the
// snapshot from persistence (internal/application/prchecks); this package
// is the deterministic core that the report on the PR page renders.
//
// Severity is per check, not per dimension: a failed blocking check
// blocks the proposal (the integrity review reports "blocked"), a failed
// warning check is reported and does not block (docs/43: the machine
// result informs the human review decision — the Scientific/Integrity
// review states are T0404's domain). The blocking/warning split is the
// task's acceptance criterion and is declared, with its rationale, in
// the spec table (check.go) — every rendered result carries its why.
package integrity
