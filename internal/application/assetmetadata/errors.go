package assetmetadata

import "errors"

// Sentinel errors: one per distinct OUTCOME a transport has to be able to
// answer differently (docs/45 — stable outcomes, never dependency
// detail). Everything else is ErrStore.
//
// The order they are listed in is the order the command resolves them in,
// and that order is the contract: the shape check precedes every read, and
// the role gate precedes every lookup of the asset (see
// Command.Revise).
var (
	// ErrValidation: the request is not a metadata revision at all — no
	// project, a pid that is not a pid, a title or slug that is empty or
	// over its bound, a description over its bound, or a list field with
	// too many entries or a blank/oversized one. The refusal names the
	// field; it never restates the request's content.
	ErrValidation = errors.New("assetmetadata: validation failed")
	// ErrForbidden: the actor may not revise this asset's metadata. This
	// is the default-deny role gate, and it answers the SAME value for
	// "not a member" and for "role too low" — the check discloses
	// nothing, not even which of the two refused (the shape
	// projects.ErrSettingsForbidden has, settings.go:174-188). Resolved
	// before any asset lookup, so a denial never discloses whether the pid
	// exists either (the releases.ErrForbidden rule).
	ErrForbidden = errors.New("assetmetadata: the actor may not revise this asset's metadata")
	// ErrProjectNotFound: no project row exists for the given id, or the
	// project is not visible to the caller (existence hiding, docs/45).
	ErrProjectNotFound = errors.New("assetmetadata: project not found")
	// ErrAssetNotFound: the request named a pid that names no stored
	// asset, or one whose origin project is not the project the request
	// named. Both are answered the same way and neither is silently
	// repaired: metadata is revised for an asset the caller already
	// holds, never for one adopted by guessing.
	ErrAssetNotFound = errors.New("assetmetadata: asset not found")
	// ErrCoverNotSupported: the request asked to set the cover. The cover
	// is RESERVED, not served — docs/11 §4 lists it among the revisable
	// metadata, so the column and the field exist
	// (research_assets.cover_blob_id, assets.AssetMetadata.CoverBlobID),
	// but the blob surface has no way to hand out those bytes: no upload
	// route, no download route, no signed URL and no TTL anywhere in the
	// tree. A cover that could be set would be an image no reader could
	// ever fetch, so the exchange is refused BY NAME rather than dropped
	// — the caller is told that the channel is missing and whose it is,
	// instead of having a field it sent silently discarded (the rule
	// projects.UpdateSettings applies to Visibility, settings.go:47-54).
	ErrCoverNotSupported = errors.New("assetmetadata: the cover is reserved, and this build has no blob channel to serve it")
	// ErrStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrStore = errors.New("assetmetadata: store failure")
)

// Wire codes (docs/45): one per failure shape, so a client branches
// without parsing messages. VALIDATION_FAILED, ASSET_NOT_FOUND and
// SERVICE_UNAVAILABLE are the error model's own names; the other three
// name outcomes this surface alone has (the same division
// assetpublish's code block states).
const (
	// CodeValidationFailed: the request is not a metadata revision.
	CodeValidationFailed = "VALIDATION_FAILED"
	// CodeForbidden: the actor may not revise this asset's metadata —
	// "not a member" and "role too low" answer this one code, which is
	// the property rather than a collision (see ErrForbidden).
	CodeForbidden = "ASSET_METADATA_FORBIDDEN"
	// CodeProjectNotFound: the project is unknown or invisible.
	CodeProjectNotFound = "ASSET_METADATA_PROJECT_NOT_FOUND"
	// CodeAssetNotFound: the pid names no asset of that project.
	CodeAssetNotFound = "ASSET_NOT_FOUND"
	// CodeCoverNotSupported: the request asked for the reserved cover.
	CodeCoverNotSupported = "ASSET_COVER_NOT_SUPPORTED"
	// CodeServiceUnavailable: the metadata data is temporarily
	// unavailable.
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// CoverNotSupportedError is the typed refusal of a cover change. The
// typed form matters because the refusal carries WHICH field was refused
// and WHY the build cannot serve it: a transport that had to parse a
// message to answer "the blob surface has no channel yet" would be one
// string edit away from answering nothing.
//
// It is the shape AgentNotPermittedError has in the two asset packages —
// a refusal whose payload is a small vocabulary rather than prose.
type CoverNotSupportedError struct{}

func (e *CoverNotSupportedError) Error() string {
	return "assetmetadata: asset cover cannot be revised — docs/11 §4 lists cover as revisable metadata and the slot is reserved for it, but the bytes it names have no serving path in this build: the blob surface declares no upload route, no download route, no signed URL and no TTL, so a cover set today would be an image no reader could fetch. The channel that makes a cover real belongs to the blob-transfer task; until it exists, this field is refused rather than silently dropped"
}

// Unwrap makes errors.Is(err, ErrCoverNotSupported) true, so the two
// spellings of one outcome agree.
func (e *CoverNotSupportedError) Unwrap() error { return ErrCoverNotSupported }

// Code is the stable wire code of this outcome (docs/45).
func (e *CoverNotSupportedError) Code() string { return CodeCoverNotSupported }
