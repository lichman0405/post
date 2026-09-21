package attestations

import (
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/domain"
)

// The wire's target reference vocabulary. An attestation names ONE version
// to be about, and which table that version lives in is part of what the
// caller is saying — a scientific object version (a Protocol or a Claim) or
// a research asset version (an Asset).
//
// It is a ref string (`<kind>:<uuid>`) rather than two body fields for the
// same reason the shape it lands in is: two nullable fields make "both" and
// "neither" spellable, and then the API has to explain a request it allowed
// a caller to write. One string cannot express two pins, and the exactly-one
// rule becomes structural rather than validated.
//
// The prefix is REQUIRED here, unlike knowledge_version_ref, where a bare
// uuid is accepted. There the prefix is a convenience for a reader, because
// only one table is ever meant; here it is the only thing that says which of
// two tables the uuid indexes, so a bare uuid is ambiguous and refused.
const (
	// refObjectVersion names a scientific_object_versions row. Whether the
	// attestation is about a Protocol or a Claim is decided by that
	// version's object's object_type, and is not known until the row is
	// read — which is why this ref does not say.
	refObjectVersion = "object_version:"
	// refAssetVersion names a research_asset_versions row.
	refAssetVersion = "asset_version:"
)

// TargetRef is a parsed target reference: exactly one field is set.
type TargetRef struct {
	// ObjectVersionID is the scientific_object_versions id, or empty.
	ObjectVersionID string
	// AssetVersionID is the research_asset_versions id, or empty.
	AssetVersionID string
}

// ParseTargetRef parses the wire's target reference
// (`object_version:<uuid>` or `asset_version:<uuid>`).
//
// It validates the ref's SHAPE only — that the prefix is one of the two and
// the rest is a canonical uuid. Whether the version exists, and whether it
// is admissible as a target, are the store's and Judge's calls; doing either
// here would be a second answer to questions that already have one home.
//
// The error wraps ErrValidation, which the transport renders as
// VALIDATION_FAILED with this sentence.
func ParseTargetRef(ref string) (TargetRef, error) {
	trimmed := strings.TrimSpace(ref)
	if trimmed == "" {
		return TargetRef{}, fmt.Errorf("%w: target is required, as %s<uuid> or %s<uuid>", ErrValidation, refObjectVersion, refAssetVersion)
	}
	switch {
	case strings.HasPrefix(trimmed, refObjectVersion):
		id := strings.TrimPrefix(trimmed, refObjectVersion)
		if !domain.ValidUUID(id) {
			return TargetRef{}, fmt.Errorf("%w: target %q does not name a version — an object_version ref must be object_version:<uuid>", ErrValidation, trimmed)
		}
		return TargetRef{ObjectVersionID: id}, nil
	case strings.HasPrefix(trimmed, refAssetVersion):
		id := strings.TrimPrefix(trimmed, refAssetVersion)
		if !domain.ValidUUID(id) {
			return TargetRef{}, fmt.Errorf("%w: target %q does not name a version — an asset_version ref must be asset_version:<uuid>", ErrValidation, trimmed)
		}
		return TargetRef{AssetVersionID: id}, nil
	}
	return TargetRef{}, fmt.Errorf("%w: target %q names no kind — it must be %s<uuid> (a Protocol or Claim version) or %s<uuid> (an Asset version); a bare uuid is ambiguous, because the two are different kinds of row", ErrValidation, trimmed, refObjectVersion, refAssetVersion)
}
