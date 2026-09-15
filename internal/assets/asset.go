package assets

import (
	"encoding/json"
	"strings"
	"time"
)

// Type is the research asset type: the closed V1 set of docs/11 §2 —
// Dataset, Protocol, Material Collection, Benchmark. The same four values
// are carried by two other copies: the database CHECK on
// research_assets.asset_type (migration 00010) and the asset_type enum of
// specs/schemas/research-asset-version.schema.json.
//
// Each pair is pinned by a test, not by review: TestAssetCoreTypeEnum
// stores every value of AllTypes() and rejects values outside it, so Go
// and the database cannot drift apart silently;
// TestAssetCoreTypeSetMatchesSchema compares AllTypes() with the schema's
// enum, which covers the one pair nothing else in the tree compared
// (check-schema-drift only checks that the three copies of specs/schemas
// agree with each other). A change to any single copy therefore fails a
// test that names the other one.
type Type string

// The V1 asset type set. Never extended silently: a new type is a schema
// change in every one of the three copies above, not an addition here.
const (
	TypeDataset            Type = "dataset"
	TypeProtocol           Type = "protocol"
	TypeMaterialCollection Type = "material_collection"
	TypeBenchmark          Type = "benchmark"
)

// AllTypes returns the V1 type set in declaration order (stable — it is
// the order every list endpoint and fixture uses).
func AllTypes() []Type {
	return []Type{TypeDataset, TypeProtocol, TypeMaterialCollection, TypeBenchmark}
}

// Valid reports whether t is one of the four V1 types.
func (t Type) Valid() bool {
	switch t {
	case TypeDataset, TypeProtocol, TypeMaterialCollection, TypeBenchmark:
		return true
	}
	return false
}

// ParseType converts a raw type string, accepting nothing outside the
// V1 set.
func ParseType(s string) (Type, bool) {
	t := Type(strings.TrimSpace(s))
	if !t.Valid() {
		return "", false
	}
	return t, true
}

// Visibility is the published version's visibility: exactly the two
// values research_asset_versions.visibility admits (migration 00010).
// It is the coarse public/private axis of the rights layer, not the
// rights model itself (T0703): metadata vs blob access distinctions live
// in rights_json, never in this enum.
type Visibility string

const (
	VisibilityPublic  Visibility = "public"
	VisibilityPrivate Visibility = "private"
)

// Valid reports whether v is public or private.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityPublic, VisibilityPrivate:
		return true
	}
	return false
}

// MaxVersionLen bounds an asset version label. The bound and the character
// set of ValidVersionLabel follow the shape the domain's version
// validators give their own labels (1..64 of [A-Za-z0-9._-], e.g.
// domain.ValidPolicyVersion, which validates policy versions — there is no
// release-version validator in the tree): long enough for any real version
// scheme, short enough to stay a readable URL segment. The agreement is a
// convention, not a link: each validator owns its bound, and nothing keeps
// them in step.
const MaxVersionLen = 64

// versionLabelCharset is the character set an asset version label may
// use — letters, digits, '.', '_' and '-' — the subset that needs no
// percent-encoding as a bare URL path segment. The rules that read it
// (ValidVersionLabel and its tests) share this one constant.
const versionLabelCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-"

// ValidVersionLabel reports whether the raw label is storable after
// trimming: 1..64 characters of letters, digits, '.', '_' or '-'
// ("1", "v2", "2026-09-15"), and not one of the two dot-segment forms
// "." and "..".
//
// The character set keeps the label a bare path segment in the persistent
// version URL (/assets/{pid}/{version}) with no percent-encoding — but the
// set alone is not enough. "." and ".." are not names in RFC 3986 §5.2.4
// terms, they are path segments that a resolver removes: /assets/{pid}/..
// normalises to /assets/. A published label is immutable forever
// (UNIQUE(asset_id, version) plus the append-only row guard), so a version
// published under such a label would have a permanently broken link.
// Labels that merely contain dots — "1.0", "a..b" — are ordinary segments
// and stay valid. The label is validated here, by the publish command
// (T0705), and by the database's per-asset uniqueness; the database does
// not constrain the label's shape, so this function is the only gate.
func ValidVersionLabel(version string) bool {
	v := strings.TrimSpace(version)
	if v == "" || len(v) > MaxVersionLen {
		return false
	}
	if v == "." || v == ".." {
		return false
	}
	for _, r := range v {
		if !strings.ContainsRune(versionLabelCharset, r) {
			return false
		}
	}
	return true
}

// Asset is one research asset row (research_assets, migration 00010
// extended by 00064): the network-reusable object identity of docs/11
// §2. PID is the persistent public identifier — the value the network
// sees and the URLs are built from; it never changes, whatever happens
// to Slug or the owning organization. ID is the internal uuid the
// foreign keys use.
type Asset struct {
	// ID is the uuid v4 text form of the research_assets row (internal
	// foreign-key target, not a public identity).
	ID string
	// PID is the persistent identifier: 26 random Crockford base32
	// characters, unique across all assets, never derived from mutable
	// metadata.
	PID PID
	// Type is one of the four V1 asset types.
	Type Type
	// Slug is display-level, revisable metadata (docs/11 §4) — it never
	// appears in an identity or a persistent URL.
	Slug string
	// Title is the human-facing label.
	Title string
	// OriginProjectID is the project the asset was published from
	// (docs/11 §2: assets are published out of projects).
	OriginProjectID string
	CreatedAt       time.Time
}

// AssetVersion is one stored published version row
// (research_asset_versions, migration 00010 extended by 00064). Rows are
// append-only (migration 00014) and immutable (CLAUDE.md invariant 5):
// the identity of a version is the pair (asset pid, Version), both
// permanent. OriginRefs pin where this version came from; they are
// mandatory — the storage layer holds at least one non-NULL string (the
// column is NOT NULL and carries two CHECKs, migration 00064), so "no
// origin" cannot be stored.
type AssetVersion struct {
	// ID is the uuid v4 text form of the row (internal foreign-key
	// target).
	ID string
	// AssetID is the owning asset's internal row id.
	AssetID string
	// Version is the immutable version label (ValidVersionLabel),
	// unique per asset.
	Version string
	// SourceReleaseID is the release this version was published from,
	// when it was published from a release rather than a state.
	SourceReleaseID *string
	// Manifest is the stored version manifest document (jsonb).
	Manifest json.RawMessage
	// RightsJSON is the stored rights document (jsonb); its model is
	// T0703's, this package only carries it.
	RightsJSON json.RawMessage
	// Visibility is the coarse public/private axis.
	Visibility Visibility
	// IntegrityHash pins the published content.
	IntegrityHash string
	// PublishedBy is the user id of the publishing actor.
	PublishedBy string
	PublishedAt time.Time
	// OriginRefs are the canonical provenance pins (OriginRef): never
	// empty, never containing NULL.
	OriginRefs []string
}
