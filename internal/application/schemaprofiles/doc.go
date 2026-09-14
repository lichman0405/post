// Package schemaprofiles owns the project schema profile surface (T0213):
// a project registers namespaced, versioned JSON Schema profiles that
// extend an official base schema with project-specific typed fields —
// custom experiment fields without ever changing platform core.
//
// A profile is generated server-side as a merge of the pinned base
// schema's property definitions and the project's custom field
// definitions, so "extends" holds by construction: base properties are
// copied at registration time, custom properties may only ADD new fields
// (never redefine base fields), and additional required fields may only
// tighten. The generated document is registered into the schema registry
// (internal/rsg/schemareg) under its "project:<project_id>:<name>" id and
// persisted append-only in project_schema_profiles (00038). Immutability
// is doubled: the registry refuses an id+version re-registration with
// different content, and the database refuses UPDATE/DELETE — a profile
// v2 is a new version row and never invalidates history written under v1
// (docs/21 §8: every object version pins its schema ref).
//
// Authorization. Registering a profile is a governance action: the caller
// must be a project maintainer or above, resolved through the project
// surface's visibility-aware reads (a hidden project answers the
// existence-hiding not-found, a role too low answers forbidden — nothing
// is disclosed either way). Reads are exactly as visible as the project
// itself. The permission matrix has no schema-profile row (the CSV is the
// policy source of truth and specs/policies is outside this task's
// scope); the role gate is the default-deny server-side rule until a
// policy-owning task adds the row — the same L1 shape the settings surface
// recorded for T0109.
//
// Every registration writes an audit_log row in the same transaction as
// the profile version (domain.ActionSchemaProfileRegistered).
package schemaprofiles
