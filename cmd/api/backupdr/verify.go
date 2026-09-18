package backupdr

import (
	"encoding/json"
	"fmt"

	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/assets"
)

// The two hash definitions the reconciliation's release-manifest axis is
// checked against. Both are DELEGATED, and that is the whole content of
// this file: "the stored release document still digests to its
// manifest_hash" and "the stored asset manifest still digests to its
// integrity_hash" are questions the product already has one answer to
// each, and a second implementation here would be a second definition of
// what those hashes mean.
//
// The delegation is also what keeps the axis honest across a refactor: if
// internal/application/releases changes its canonical rendering, this file
// follows it automatically instead of drifting into a check that agrees
// with yesterday's product.

// parseReleaseManifest reads a stored release manifest document the way the
// product's own export path does (releases.Command.Manifest):
// json.Unmarshal into releases.ReleaseManifest. A document that does not
// parse is reported as unparseable rather than as drift — the caller
// distinguishes the two, because "this is not a manifest" and "this
// manifest says something different from what the row says" are different
// findings.
func parseReleaseManifest(raw []byte) (*releases.ReleaseManifest, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var m releases.ReleaseManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, false
	}
	return &m, true
}

// assetManifestHash derives the canonical hash of a stored asset manifest
// document. assets.ManifestHash is the verification half of the publish
// gate's own check (internal/assets/manifest.go: Manifest.Hash is what
// integrity_hash carries; ManifestHash derives it from stored bytes the
// same way), so a version that fails here is one the publisher could not
// have produced.
func assetManifestHash(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", fmt.Errorf("asset manifest: no document")
	}
	h, err := assets.ManifestHash(raw)
	if err != nil {
		return "", fmt.Errorf("asset manifest: %w", err)
	}
	return h, nil
}
