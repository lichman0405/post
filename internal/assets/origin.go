package assets

import "strings"

// OriginRef is one canonical provenance pin of a published asset
// version (research_asset_versions.origin_refs, migration 00064): where
// this version came from. The text form is kind:value — the kind names
// the entity class, the value is the entity's uuid in text form (the
// ids every internal origin kind names are uuid rows).
//
// A version's origin refs answer "what was published": the project it
// came out of (docs/11 §2: assets are published out of projects), the
// release or state whose accepted content it fixed, and the scientific
// object versions the asset carries. They are mandatory (the column is
// NOT NULL and carries the non-empty and no-NULL-element CHECKs of
// migration 00064 — the version schema's array of strings, minItems 1)
// and — like the version itself — immutable once published. The
// kind:value vocabulary is this package's: the storage layer only knows
// the elements are strings.
type OriginRef string

// OriginKind names the entity class an origin ref points at. The set is
// closed for V1: every origin is an internal entity of one of these
// four classes.
type OriginKind string

const (
	// KindProject pins the originating project (research boundary,
	// docs/09 §1).
	KindProject OriginKind = "project"
	// KindRelease pins the immutable release the version was published
	// from (docs/11 §1).
	KindRelease OriginKind = "release"
	// KindState pins the accepted project state whose snapshot the
	// version fixed.
	KindState OriginKind = "state"
	// KindObjectVersion pins one scientific object version the asset
	// carries (the published content itself).
	KindObjectVersion OriginKind = "object_version"
)

// Valid reports whether k is one of the V1 origin kinds.
func (k OriginKind) Valid() bool {
	switch k {
	case KindProject, KindRelease, KindState, KindObjectVersion:
		return true
	}
	return false
}

// NewOriginRef builds a canonical origin ref. It reports false when the
// kind is outside the V1 set or the value is not a uuid in text form —
// an origin pin must name an existing internal entity, and every one of
// the four kinds is a uuid row.
func NewOriginRef(kind OriginKind, value string) (OriginRef, bool) {
	if !kind.Valid() || !validUUIDText(value) {
		return "", false
	}
	return OriginRef(string(kind) + ":" + value), true
}

// ParseOriginRef splits a canonical origin ref into its kind and value.
// It is the inverse of NewOriginRef: ok is true exactly when the ref
// has the canonical shape and a known kind. The value is returned
// verbatim (a caller that needs an entity row re-validates it as a
// uuid).
func ParseOriginRef(s string) (kind OriginKind, value string, ok bool) {
	k, v, ok := strings.Cut(s, ":")
	if !ok {
		return "", "", false
	}
	kind = OriginKind(k)
	if !kind.Valid() || !validUUIDText(v) {
		return "", "", false
	}
	return kind, v, true
}

// Valid reports whether r has the canonical origin ref shape: a known
// kind and a uuid value.
func (r OriginRef) Valid() bool {
	_, _, ok := ParseOriginRef(string(r))
	return ok
}

// ValidOriginRefs reports whether every ref in the slice is a valid
// origin ref — the application-side mirror of what T0705's publish
// command must guarantee before a row reaches the database's
// non-empty CHECK.
func ValidOriginRefs(refs []string) bool {
	if len(refs) == 0 {
		return false
	}
	for _, r := range refs {
		if !OriginRef(r).Valid() {
			return false
		}
	}
	return true
}

// validUUIDText reports the lowercase canonical uuid text form
// (8-4-4-4-12 hex). Every V1 origin value is an internal uuid row.
func validUUIDText(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			switch {
			case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			default:
				return false
			}
		}
	}
	return true
}
