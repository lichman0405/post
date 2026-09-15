package rights

// ValidationError is one broken rights rule: a stable code, the JSON field
// path it applies to ("" for a whole-document rule such as the version),
// and a sentence naming what was found.
//
// The code is the machine-readable half and the only part a caller may
// match on — the API layer maps these onto its own error envelope, the way
// internal/application/milestones maps its sentinels onto
// MILESTONE_* codes. The detail is for humans and may be reworded.
type ValidationError struct {
	// Code is one of the RIGHTS_* constants below.
	Code string
	// Field is the JSON path of the offending field, e.g.
	// "usage.commercial_use"; empty for a document-level rule.
	Field string
	// Detail names what was found and what is accepted.
	Detail string
}

// Error renders the code, the field path when there is one, and the detail.
func (e *ValidationError) Error() string {
	if e.Field == "" {
		return e.Code + ": " + e.Detail
	}
	return e.Code + " (" + e.Field + "): " + e.Detail
}

// The stable validation codes. They are uppercase snake case to match the
// API's error vocabulary (docs/45: RIGHTS_POLICY_BLOCKS_ACTION; the
// milestone service's MILESTONE_VALIDATION_FAILED), so a future rights
// endpoint relays them without translation. None of them is a product
// error code today: no rights endpoint exists yet (T0705/T0709 own the
// publish and asset surfaces).
const (
	// CodeMalformedDocument: the bytes are not one JSON object this
	// package can decode — bad syntax, a JSON value that is not an
	// object, an unknown field, or trailing content.
	CodeMalformedDocument = "RIGHTS_MALFORMED_DOCUMENT"
	// CodeUnsupportedVersion: version is not a DocumentVersion this
	// package understands (including a document with no version at all).
	CodeUnsupportedVersion = "RIGHTS_UNSUPPORTED_VERSION"
	// CodeInvalidLicenseID: standard_license_id is present but not a
	// well-formed identifier (see ValidLicenseID — a syntax rule, not a
	// check against the SPDX license list).
	CodeInvalidLicenseID = "RIGHTS_INVALID_LICENSE_ID"
	// CodeInvalidAgreementRef: custom_agreement_ref is present but empty
	// or out of shape.
	CodeInvalidAgreementRef = "RIGHTS_INVALID_AGREEMENT_REF"
	// CodeInvalidUsageValue: a usage declaration carries a value outside
	// its own vocabulary.
	CodeInvalidUsageValue = "RIGHTS_INVALID_USAGE_VALUE"
	// CodeInvalidDataAccess: visibility.data_access is not open or
	// restricted.
	CodeInvalidDataAccess = "RIGHTS_INVALID_DATA_ACCESS"
	// CodeInvalidMetadataVisibility: visibility.metadata is empty or out
	// of shape.
	CodeInvalidMetadataVisibility = "RIGHTS_INVALID_METADATA_VISIBILITY"
	// CodePatentGrantWithoutAgreement: usage.patent_grant is
	// "see_agreement" while custom_agreement_ref names no agreement.
	CodePatentGrantWithoutAgreement = "RIGHTS_PATENT_GRANT_WITHOUT_AGREEMENT"
	// CodeNotesTooLong: notes exceeds MaxNotesLen.
	CodeNotesTooLong = "RIGHTS_NOTES_TOO_LONG"
)
