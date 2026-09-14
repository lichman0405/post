package domain

import (
	"regexp"
	"time"
)

// ProjectSchemaProfile is one immutable registered schema profile of a
// project (T0213): a namespaced, versioned JSON Schema document that
// extends an official base schema with project-specific typed fields.
// Canonical columns: infra/migrations/00038_project_schema_profiles.sql.
//
// Profiles follow the same immutability rule as every versioned catalog
// entry (docs/21 §8): an id+version pair is never overwritten — new content
// takes a new version — and every scientific object version pins the exact
// (schema_id, schema_version) it was written under, so a profile v2 never
// invalidates history written under v1.
type ProjectSchemaProfile struct {
	// ID is the uuid v4 text form of the profile row.
	ID string
	// ProjectID is the research boundary that owns the profile.
	ProjectID string
	// SchemaID is the registry id of the profile document — the
	// "project:<project_id>:<name>" form derived server-side, so ids are
	// namespaced by construction and can never collide across projects or
	// squat a canonical id.
	SchemaID string
	// Version is the profile's version label ("1", "2", ...) — the
	// schemareg.Ref.Version half.
	Version string
	// BaseSchemaID is the official base schema this profile extends
	// (the schemareg.Ref.ID half of the pin).
	BaseSchemaID string
	// BaseSchemaVersion is the base schema registry version the profile
	// was generated from (the schemareg.Ref.Version half of the pin).
	BaseSchemaVersion string
	// Content is the exact generated profile document (Go-canonical JSON
	// bytes as text) — the document the registry registers, byte for byte.
	Content string
	// ContentHash is the sha256 hex digest of Content (docs/21 §10): it
	// pins the validating content, matching the registry's own
	// Schema.ContentHash.
	ContentHash string
	CreatedBy   string
	CreatedAt   time.Time
}

// profileNameRe bounds the profile name token: lowercase snake_case, the
// same shape as canonical object type tokens. The schema id is derived as
// "project:<project_id>:<name>", so the name can never smuggle namespace
// separators or URI authority.
var profileNameRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// profileNameMaxLen bounds the name's length (64 characters): the derived
// schema id fits the database CHECK (char_length(schema_id) <= 512) with
// an enormous margin, and — the point — the name is refused HERE, before
// the registry or the database ever see it. A longer token would pass
// service validation and the in-memory registry and then die on the
// database CHECK, leaving a registry entry with no row behind it.
const profileNameMaxLen = 64

// ValidProfileName reports whether name is a valid profile name token:
// 1-64 characters of lowercase snake_case.
func ValidProfileName(name string) bool {
	if len(name) < 1 || len(name) > profileNameMaxLen {
		return false
	}
	return profileNameRe.MatchString(name)
}

// ValidProfileVersion reports whether v is a valid profile version label —
// the database CHECK's shape (1-64 characters of A-Za-z0-9._-), mirroring
// the policy version label rule (00033).
func ValidProfileVersion(v string) bool {
	if len(v) < 1 || len(v) > 64 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}
