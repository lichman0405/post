// Package schemareg owns the POST scientific object schema catalog: it loads
// the canonical JSON Schemas, registers them by schema id + version, and
// validates JSON documents against them.
//
// Registration is immutable. An id+version pair, once registered, is never
// overwritten: re-registering the same pair with different content fails, and
// new content must take a new version (docs/21 §8 — every object version pins
// a schema id/version and old schema data always stays valid).
//
// The canonical V1 set ships with the code (schemas/*.json, a synced copy of
// specs/schemas/ per docs/65), registers under version "1", and is keyed by
// each schema's $id under the reserved namespace
// https://open-rd.example/schemas/. Everything outside that namespace is the
// namespaced extension path: Register admits domain- or project-owned schemas
// (e.g. T0213 project schema profiles) under their own "<namespace>:<name>"
// ids, and unknown schemas fail explicitly until they are registered
// (docs/45: SCHEMA_VALIDATION_FAILED).
package schemareg
