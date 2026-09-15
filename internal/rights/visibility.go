package rights

import (
	"strings"
	"unicode/utf8"
)

// Visibility keeps the two access axes of the rights layer apart, per
// docs/12 §1 (Access Role / Object Visibility / Data Access / Usage Rights
// are separate) and CLAUDE.md invariant 7: Knowledge visibility != Blob
// accessibility.
//
// Metadata is what the network may read about the asset — its page, its
// provenance, its links. DataAccess is whether the bytes themselves may be
// fetched. The canonical case they exist for is metadata public with
// data_access restricted (docs/55's RESTRICTED_BLOB: "metadata 可公开但数据
// 本体受限"; docs/17: 每次下载检查 Project/Object/Blob access policy —
// Public metadata 不代表 blob open download). One axis cannot be derived
// from the other in either direction, which is why they are two fields and
// not one enum.
type Visibility struct {
	// Metadata is the metadata visibility policy this declaration
	// follows.
	Metadata MetadataVisibility `json:"metadata"`
	// DataAccess is the blob access of the published bytes.
	DataAccess DataAccess `json:"data_access"`
}

// DataAccess is the blob access axis: whether the published bytes may be
// fetched. The vocabulary is exactly the one the storage layer already
// uses for blob attachments (blob_attachments.access_level,
// CHECK (access_level IN ('open','restricted')), and
// specs/policies/rights-template.yaml: data_access  # open|restricted).
type DataAccess string

const (
	// DataAccessOpen: the bytes may be fetched by whoever may see the
	// metadata.
	DataAccessOpen DataAccess = "open"
	// DataAccessRestricted: the bytes are not open; access is decided by
	// the project/object/blob policy at download time (docs/17 §3 —
	// short-lived signed URLs, evaluated per fetch).
	DataAccessRestricted DataAccess = "restricted"
)

// AllDataAccess returns the vocabulary in declaration order.
func AllDataAccess() []DataAccess {
	return []DataAccess{DataAccessOpen, DataAccessRestricted}
}

// Valid reports whether a is open or restricted.
func (a DataAccess) Valid() bool {
	switch a {
	case DataAccessOpen, DataAccessRestricted:
		return true
	}
	return false
}

// ParseDataAccess converts a raw value, accepting nothing outside the
// vocabulary.
func ParseDataAccess(s string) (DataAccess, bool) {
	a := DataAccess(strings.TrimSpace(s))
	if !a.Valid() {
		return "", false
	}
	return a, true
}

// MetadataVisibility is the metadata axis: which policy decides who may
// read the asset's metadata.
//
// Unlike DataAccess this is an OPEN vocabulary, and deliberately so:
// specs/policies/rights-template.yaml enumerates the data_access values
// but gives metadata no set — its one example is project_policy — and
// docs/12 §2 says the underlying model is a policy id/scope, never a
// single boolean ("底层使用 policy id/scope，不用单一 is_private 作为永久
// 模型"). Closing the set here would either forbid a policy reference the
// platform is supposed to accept, or freeze a V1 preset list into the
// storage format. So the stored value is a token: the platform's own
// metadata default (MetadataProjectPolicy), a policy id a resolver
// understands, or a preset a later task specs. Unknown tokens round-trip
// unchanged and render verbatim, the way an unknown milestone kind does.
type MetadataVisibility string

// MetadataProjectPolicy is the template's value and the fail-closed
// default: metadata visibility is whatever the owning project's policy
// says, evaluated at read time — not a decision this document makes.
const MetadataProjectPolicy MetadataVisibility = "project_policy"

// MaxMetadataVisibilityLen bounds a metadata visibility token. Long enough
// for a uuid-shaped policy id with a prefix, short enough to stay a label
// rather than a document; notes is where a sentence belongs. The unit is
// characters (Unicode code points), counted with utf8.RuneCountInString,
// as it is for the license id and the agreement ref.
const MaxMetadataVisibilityLen = 64

// ValidMetadataVisibility reports whether s is a well-formed metadata
// visibility token: 1..64 characters, no surrounding space, and no
// whitespace or control character — the shape a policy id, a preset name
// or "project_policy" shares. The set of meaningful values is not closed
// here (see MetadataVisibility).
func ValidMetadataVisibility(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > MaxMetadataVisibilityLen || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ':':
		default:
			return false
		}
	}
	return true
}

// ParseMetadataVisibility converts a raw value, accepting any well-formed
// token — including one this package does not know — and rejecting the
// empty and malformed forms.
func ParseMetadataVisibility(s string) (MetadataVisibility, bool) {
	if !ValidMetadataVisibility(s) {
		return "", false
	}
	return MetadataVisibility(s), true
}

// Validate returns nil, or one *ValidationError per axis value out of
// shape, in field order.
func (v Visibility) Validate() error {
	return joinErrors(
		validateMetadataVisibility(v.Metadata),
		validateDataAccess(v.DataAccess),
	)
}

func validateMetadataVisibility(m MetadataVisibility) error {
	if ValidMetadataVisibility(string(m)) {
		return nil
	}
	return &ValidationError{
		Code:  CodeInvalidMetadataVisibility,
		Field: "visibility.metadata",
		Detail: "expected a metadata visibility token (1.." + itoa(MaxMetadataVisibilityLen) +
			" characters of [A-Za-z0-9_.:-], e.g. " + string(MetadataProjectPolicy) + "), got " + quote(string(m)),
	}
}

func validateDataAccess(a DataAccess) error {
	if a.Valid() {
		return nil
	}
	return &ValidationError{
		Code:   CodeInvalidDataAccess,
		Field:  "visibility.data_access",
		Detail: "expected one of open|restricted, got " + quote(string(a)),
	}
}
