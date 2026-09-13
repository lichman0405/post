// Package relationcatalog owns the V1 relation type catalog (docs/44):
// the canonical core relation types, their categories and semantics, and
// the validation rule every relation type must satisfy before a relation
// version is written (T0203).
//
// The catalog is the enforcement point for "relations never degrade into
// related_to": related_to is the one weak type and is marked as such — it
// participates in no provenance or dependency inference (docs/07 §3).
// Domain/project namespaced types (e.g. "materials:synthesized_from") are
// an extension path declared by docs/44; V1 has no registration mechanism
// yet, so any type outside this catalog fails validation explicitly.
package relationcatalog
