package domain

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// ExternalReference is the LIVE identity of one external source
// (external_references, migration 00011): the canonical
// source_type + external_identifier pair plus the canonical URL. It is
// deliberately NOT a scientific object: the identity is cross-project
// canonical — every project that references the same paper/patent/dataset
// shares one identity row (docs/19 §2: live identity), while each
// project's external_reference object versions its own view. The identity
// row is created and maintained by the 00045 identity-sync trigger from
// external_reference object payloads, so the unique pair is enforced for
// ANY write path.
type ExternalReference struct {
	// ID is the uuid v4 text form (matches the external_references.id
	// uuid column).
	ID string
	// SourceType names the external source kind (external_reference
	// schema enum: publication, patent, database, standard, vendor, web,
	// other).
	SourceType string
	// ExternalIdentifier is the canonical identifier as the registering
	// source spells it; DOI-shaped identifiers are normalized (doi:
	// prefix stripped, case-folded — DOI names are case-insensitive,
	// ISO 26324) so one DOI always lands on one identity row.
	ExternalIdentifier string
	// CanonicalURL is the live URL the identifier resolves at; empty
	// when unknown.
	CanonicalURL string
}

// ExternalReferenceSnapshot is one pinned snapshot of the identity's
// upstream state at one access (external_reference_snapshots, migration
// 00011). Rows are append-only — the database itself rejects UPDATE and
// DELETE (migrations 00014/00015) — so a refresh can only ever append a
// new snapshot, never rewrite one that exists (docs/19 §2, §5).
type ExternalReferenceSnapshot struct {
	// ID is the uuid v4 text form of the snapshot row.
	ID string
	// ExternalReferenceID names the live identity this snapshot pins.
	ExternalReferenceID string
	// AccessedAt is the server time the upstream source was observed.
	AccessedAt time.Time
	// UpstreamVersion is the upstream version marker, when the source
	// reports one (e.g. the CSL-JSON version field); empty otherwise.
	UpstreamVersion string
	// Metadata is the upstream metadata document as a JSON object —
	// untrusted upstream data, never instructions (docs/23 §8).
	Metadata json.RawMessage
	// SnapshotHash is the sha256 hex digest of the stored metadata
	// bytes, derived server-side (the 00045 trigger derives it from the
	// jsonb-canonical text, so a stored row always re-hashes to its
	// stored hash; docs/23 §3: never caller-supplied truth).
	SnapshotHash string
	// BlobID names the cached file blob when the reference carries a
	// file that was fetched and stored (blobs, migration 00008); nil
	// when there is none.
	BlobID *string
}

// ExternalReferenceSourceType is the external_reference.schema.json
// source_type enum: the kind of external source a reference names.
type ExternalReferenceSourceType string

const (
	ExternalSourcePublication ExternalReferenceSourceType = "publication"
	ExternalSourcePatent      ExternalReferenceSourceType = "patent"
	ExternalSourceDatabase    ExternalReferenceSourceType = "database"
	ExternalSourceStandard    ExternalReferenceSourceType = "standard"
	ExternalSourceVendor      ExternalReferenceSourceType = "vendor"
	ExternalSourceWeb         ExternalReferenceSourceType = "web"
	ExternalSourceOther       ExternalReferenceSourceType = "other"
)

// ValidExternalReferenceSourceType reports whether s is one of the seven
// canonical source types of external_reference.schema.json.
func ValidExternalReferenceSourceType(s string) bool {
	switch s {
	case "publication", "patent", "database", "standard", "vendor", "web", "other":
		return true
	}
	return false
}

// CanonicalExternalReferenceSourceTypes returns the seven source types in
// the order the schema enum declares them. The integration drift test pins
// this list to the schema's own enum, so a schema change cannot silently
// outrun the domain model.
func CanonicalExternalReferenceSourceTypes() []string {
	return []string{"publication", "patent", "database", "standard", "vendor", "web", "other"}
}

// ASCIIWhitespace is the explicit, deterministic whitespace set every
// external-reference identity/snapshot check trims and matches with in
// Go: SPACE, TAB, LF, CR, FF, VT. Migration 00045 spells the SAME set in
// SQL — btrim(x, E' \t\n\r\f\x0b') and the same bracket classes, with VT
// written as \x0b because E-string constants have no \v escape (the SQL
// spelling E'\v' is the literal character 'v'). The two copies must
// answer identically for identical inputs; the integration drift test
// feeds the same strings to both. Unicode-aware TrimSpace and POSIX
// [[:space:]] are deliberately NOT used on this surface: [[:space:]] is
// locale-dependent, and the two sides must never disagree about what
// counts as "blank" or as part of a DOI.
const ASCIIWhitespace = " \t\n\r\f\v"

// doiShape matches a DOI name (10.<registrant>/<suffix>) wherever it
// appears at the front of an identifier: bare ("10.1000/xyz"), with the
// DOI scheme ("doi:10.1000/xyz") or as a doi.org URL. The suffix runs to
// the end of the string and is spelled with the explicit whitespace
// class [^ \t\n\r\f\v] — DOI names contain no whitespace, and the
// trailing part of any identifier that starts DOI-shaped is the DOI.
// Case-insensitive overall: DOI names are case-insensitive (ISO 26324),
// and the prefix itself is written in either case ("DOI:", "DOI.ORG").
// The SQL copy (00045) matches case-insensitively with the same explicit
// class; the integration test pins the two copies input by input.
var doiShape = regexp.MustCompile(`^(?i)(?:https?://(?:dx\.)?doi\.org/|doi:[ \t\n\r\f\v]*)?(10\.\d{4,9}/[^ \t\n\r\f\v]+)$`)

// NormalizeExternalIdentifier returns the canonical form of an external
// identifier for identity comparison and storage. The rule is mechanical
// and type-independent: the value is first trimmed with the explicit
// ASCII whitespace set (ASCIIWhitespace — the same set the 00045 SQL
// trims with), then DOI-shaped identifiers are normalized (scheme/URL
// prefix stripped, case-folded — DOI names are case-insensitive per
// ISO 26324) and everything else passes through trimmed. The 00045
// identity-sync trigger applies the same rule in SQL; the integration
// test pins the two copies together.
func NormalizeExternalIdentifier(identifier string) (string, error) {
	id := strings.Trim(identifier, ASCIIWhitespace)
	if id == "" {
		return "", ErrIdentifierEmpty
	}
	if m := doiShape.FindStringSubmatch(id); m != nil {
		return strings.ToLower(m[1]), nil
	}
	return id, nil
}

// ErrIdentifierEmpty reports an external identifier that is absent or
// whitespace-only — an identity cannot be keyed by nothing.
var ErrIdentifierEmpty = errors.New("external identifier must not be empty")
