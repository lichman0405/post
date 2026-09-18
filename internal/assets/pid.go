package assets

import "github.com/lichman0405/post/internal/domain"

// PID is an asset's persistent identifier: 26 lowercase Crockford base32
// characters (128 bits of randomness, no padding), URL-safe and
// unambiguous in print (the Crockford alphabet drops i, l, o, u).
//
// It is an ALIAS of domain.PID, not a type of its own, and that is a real
// decision rather than a rename: research_assets.pid (migration 00064) and
// knowledge_publications.pid (migration 00083) are the SAME identifier
// format — one CHECK regex, one generator — and T0805 moved the generator
// to internal/domain so that the knowledge publish could mint a pid
// without the knowledge domain importing the asset domain. The alias
// keeps every existing caller spelling the same (`assets.PID`,
// `assets.NewPID`, `assets.ValidPID`, and the `Valid` method through the
// alias) while there is exactly one implementation of the encoding. A
// second copy of the base32 encoder is what 00064 warns against: two
// definitions drift the first time one side changes.
//
// A pid is generated once, at asset creation, and then never changes: it
// is not derived from the slug, the owning organization, the title, or any
// other revisable attribute, so renaming the asset or transferring it
// between organizations leaves its identity — and the persistent URLs
// built from it — untouched (acceptance: asset ID 不随 slug/org 变化).
// The database carries the same invariant: research_assets.pid is NOT
// NULL, UNIQUE, and CHECK-constrained to this exact shape (migration
// 00064), so no writer can smuggle a mutable-derived identifier past the
// application.
type PID = domain.PID

// pidAlphabet and pidLen are this package's names for the format's two
// constants, so that the encoding test beside this file reads them from
// the one definition (domain) rather than from a copy.
const (
	pidAlphabet = domain.PIDAlphabet
	pidLen      = domain.PIDLen
)

// NewPID returns a fresh random persistent identifier. The encoding and
// the entropy source are domain.NewPID's — one definition for both the
// asset and the knowledge publication.
func NewPID() (PID, error) { return domain.NewPID() }

// ValidPID reports whether s has the exact pid shape. Uppercase input is
// rejected: pids are emitted lowercase, and accepting case variants of one
// identity creates lookalike ambiguity — exactly what the Crockford
// alphabet exists to avoid.
func ValidPID(s string) bool { return domain.ValidPID(s) }
