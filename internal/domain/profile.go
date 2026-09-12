package domain

import "strings"

// Profile is the research-profile view of a person identity (docs/04,
// docs/42): the identity facts from users joined with the profile content
// from the profiles table (docs/21 lists both as canonical tables — the
// identity stays credential-adjacent, the profile content grows
// independently, e.g. visibility settings in a later task).
//
// Public/private field distinction (T0102 L1): Handle, DisplayName and Bio
// are the public profile fields — the public profile endpoint renders
// exactly these (plus ID and CreatedAt) to anonymous readers. Email,
// DisabledAt and anything credential-shaped are private and never leave
// the authenticated surface. V1 has no per-field visibility configuration
// (docs/13 requires user control only for confidential contribution
// summaries, a later settings task).
type Profile struct {
	User User
	Bio  string
}

// MaxBioLen is the longest stored bio, in bytes.
const MaxBioLen = 2000

// ValidBio reports whether bio fits the V1 profile shape: any UTF-8 text
// up to 2000 bytes; the empty string clears the bio. The bound exists so a
// hostile client cannot stuff megabytes into the profile table.
func ValidBio(bio string) bool {
	return len(bio) <= MaxBioLen
}

// MaxDisplayNameLen is the longest stored display name, in bytes — the
// same bound authn.Signup enforces (T0101).
const MaxDisplayNameLen = 200

// ValidDisplayName reports whether name is a usable display name:
// non-empty after trimming and at most 200 bytes. T0101 checked length
// only; the visible-text rule is new in T0102 so a whitespace-only rename
// cannot blank a profile. Signup and profile update share this one rule.
func ValidDisplayName(name string) bool {
	return len(strings.TrimSpace(name)) > 0 && len(name) <= MaxDisplayNameLen
}
