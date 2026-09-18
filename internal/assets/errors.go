package assets

import (
	"errors"
	"strconv"
)

// ValidationError is one broken asset-document rule: a stable code, the
// JSON path of the field it applies to ("" for a whole-document or
// whole-candidate rule), and a sentence naming what was found. It is the
// same shape as internal/rights.ValidationError, so the two document
// models of a published asset version report their refusals the same way.
//
// The code is the machine-readable half and the only part a caller may
// match on — the API layer maps these onto its own error envelope. The
// detail is for humans and may be reworded.
type ValidationError struct {
	// Code is one of the ASSET_* constants below.
	Code string
	// Field is the JSON path of the offending field, e.g.
	// "metadata.data_type"; empty for a rule about the document as a
	// whole.
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
// API's error vocabulary (docs/45: the rights model's RIGHTS_* codes, the
// milestone service's MILESTONE_VALIDATION_FAILED), so a publish endpoint
// relays them without translation. None of them is a product error code
// today: the publish command that would surface them is T0705.
const (
	// CodeInvalidPID: the candidate's asset pid is not the 26-character
	// Crockford shape every persisted asset carries.
	CodeInvalidPID = "ASSET_INVALID_PID"
	// CodeInvalidVersionLabel: the version label is empty, too long, or
	// outside the character set — it could not be stored as the
	// immutable per-asset version, so nothing pins what is published.
	CodeInvalidVersionLabel = "ASSET_INVALID_VERSION_LABEL"
	// CodeMissingProvenance: the version carries no origin ref at all.
	CodeMissingProvenance = "ASSET_MISSING_PROVENANCE"
	// CodeInvalidProvenanceRef: an origin ref is not a canonical
	// kind:value pin.
	CodeInvalidProvenanceRef = "ASSET_INVALID_PROVENANCE_REF"
	// CodeUnpinnedSource: the origin refs name no release and no state,
	// so the published content is not pinned to accepted research state.
	CodeUnpinnedSource = "ASSET_UNPINNED_SOURCE"
	// CodeMissingRights: the version carries no rights document (the
	// column is NULL/empty).
	CodeMissingRights = "ASSET_MISSING_RIGHTS"
	// CodeInvalidRights: the stored rights document does not parse or
	// does not validate (the cause is the rights package's own
	// RIGHTS_* error, wrapped in the detail).
	CodeInvalidRights = "ASSET_INVALID_RIGHTS"
	// CodeInvalidVisibility: the version's visibility is neither public
	// nor private.
	CodeInvalidVisibility = "ASSET_INVALID_VISIBILITY"
	// CodeInvalidIntegrityHash: the integrity hash is absent or is not a
	// sha256 hex digest.
	CodeInvalidIntegrityHash = "ASSET_INVALID_INTEGRITY_HASH"
	// CodeIntegrityHashMismatch: the integrity hash is well-formed but
	// does not cover the manifest document being published.
	CodeIntegrityHashMismatch = "ASSET_INTEGRITY_HASH_MISMATCH"
	// CodeNoContributors: the version records no creator or contributor.
	CodeNoContributors = "ASSET_NO_CONTRIBUTORS"
	// CodeDuplicateCreatorID: the same user is credited twice in one
	// creator list. Refused by name rather than de-duplicated: the rows
	// 00082 stores are the declaration as it was made, and collapsing two
	// entries into one would store something other than what was declared
	// (the same shape CodeDuplicateDependencyPin has).
	CodeDuplicateCreatorID = "ASSET_CREATOR_ID_DUPLICATE"
	// CodeMalformedManifest: the bytes are not one JSON object of the
	// manifest format — bad syntax, a value that is not an object, an
	// unknown top-level field, or trailing content.
	CodeMalformedManifest = "ASSET_MALFORMED_MANIFEST"
	// CodeUnsupportedManifestVersion: the manifest's format version is
	// not one this platform reads.
	CodeUnsupportedManifestVersion = "ASSET_UNSUPPORTED_MANIFEST_VERSION"
	// CodeUnknownAssetType: the manifest's asset_type is outside the
	// closed V1 set.
	CodeUnknownAssetType = "ASSET_UNKNOWN_ASSET_TYPE"
	// CodeManifestTypeMismatch: the manifest declares one asset type
	// while the version being published is of another — its metadata
	// would be held to the wrong required-field table.
	CodeManifestTypeMismatch = "ASSET_MANIFEST_TYPE_MISMATCH"
	// CodeMissingMetadata: a required metadata field of the asset type is
	// absent (or present as null).
	CodeMissingMetadata = "ASSET_METADATA_MISSING"
	// CodeInvalidMetadataValue: a metadata field is present but its JSON
	// value is not the shape its requirement declares.
	CodeInvalidMetadataValue = "ASSET_METADATA_INVALID_VALUE"
	// CodeInvalidDependencyPin: a dependency pin is not a canonical
	// asset-version pin (pid@version).
	CodeInvalidDependencyPin = "ASSET_DEPENDENCY_PIN_INVALID"
	// CodeDuplicateDependencyPin: the manifest pins the same asset
	// version twice.
	CodeDuplicateDependencyPin = "ASSET_DEPENDENCY_PIN_DUPLICATE"
	// CodeSelfDependencyPin: the version pins itself — a dependency
	// cycle of one, and not what a version depends on.
	CodeSelfDependencyPin = "ASSET_DEPENDENCY_PIN_SELF"
)

// joinErrors drops the nil rules and returns nil, the single error, or an
// errors.Join of all of them — every broken rule is reported in one pass,
// so a caller fixing a candidate sees the whole list rather than one rule
// per attempt. It is the same shape as internal/rights.joinErrors, kept
// separate because the two packages' error types are separate.
func joinErrors(errs ...error) error {
	var kept []error
	for _, err := range errs {
		if err != nil {
			kept = append(kept, err)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	default:
		return errors.Join(kept...)
	}
}

// quote renders a value the way Go source would, so a detail line shows
// the exact bytes a rule refused — including the invisible ones (a
// trailing space, an empty string).
func quote(s string) string { return strconv.Quote(s) }

// itoa renders a bound, an index or a version in a detail line.
func itoa(n int) string { return strconv.Itoa(n) }
