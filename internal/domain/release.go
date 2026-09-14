package domain

import (
	"encoding/json"
	"strings"
	"time"
)

// Release is one stored immutable release row (canonical table releases,
// migration 00010, extended by 00053). The row is a complete snapshot:
// Manifest carries the full canonical release manifest document
// (internal/application/releases) and ManifestHash its content hash — the
// release renders from the row alone, never from live state, so an old
// release cannot drift when the current state evolves (docs/11 §1,
// CLAUDE.md §9.5).
type Release struct {
	// ID is the uuid v4 text form of the release row.
	ID string
	// ProjectID is the research boundary the release belongs to.
	ProjectID string
	// Version is the release version string (unique per project, the
	// database UNIQUE(project_id, version) guard); the bounded label
	// shape of ValidPolicyVersion.
	Version string
	// Title is the human-facing label; the release version when the
	// creator left it blank.
	Title string
	// StateID is the accepted main snapshot the release fixed.
	StateID string
	// PolicyVersionID is the project policy version pinned in force
	// (docs/12 §5); nil when the project had no policy at release time.
	PolicyVersionID *string
	// OrgPolicyVersionID is the organization policy version pinned in
	// force (the lower bound); nil when the project has no organization
	// or the organization had no policy at release time.
	OrgPolicyVersionID *string
	// Manifest is the full canonical release manifest document as stored
	// (jsonb). Never re-derived on read.
	Manifest json.RawMessage
	// ManifestHash is the manifest's content hash (the digest over its
	// canonical content, generated_at excluded).
	ManifestHash string
	// CreatedBy is the user id of the releasing actor.
	CreatedBy string
	CreatedAt time.Time
}

// MaxReleaseTitleLen bounds the release title. It is display metadata,
// not part of the manifest hash, so the bound is a display decision: long
// enough for a real title, short enough that a page of releases stays
// readable.
const MaxReleaseTitleLen = 200

// ValidReleaseTitle reports whether the raw title is storable after
// trimming: 1..200 characters.
func ValidReleaseTitle(title string) bool {
	t := strings.TrimSpace(title)
	return len(t) >= 1 && len(t) <= MaxReleaseTitleLen
}
