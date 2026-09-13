// Package rsg owns the Research State Graph semantics: branches as research
// state evolution paths, commits as state transitions, frozen main updated
// only through Research PRs (docs/07_RSG_SPEC.md, ADR-002, ADR-006).
// T0002 scaffold.
//
// Subpackage schemareg owns the scientific object schema catalog: the
// canonical JSON Schemas (specs/schemas/), their id+version registry, and
// document validation against it (T0201).
package rsg
