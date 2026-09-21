package orgs

import "fmt"

// AttestationAttribution is an organization's STANDING answer to "may this
// organization be named on an attestation it issues"
// (organizations.attestation_attribution, migration 00120).
//
// It lives here, in the package that owns the organization, because it is a
// property OF the organization and not of any one attestation: the
// attestation records its own org_visibility — the choice made for that one
// statement — and the public projection names the organization only when
// BOTH say named (internal/application/attestations.Present). One
// vocabulary, two surfaces that read it; the attestation package re-exports
// these values rather than spelling a second copy of them.
type AttestationAttribution string

const (
	// AttestationAttributionAnonymous: the organization is not named on
	// attestations it issues. It is the DEFAULT on the column, because
	// being named is a widening and docs/12 §3 requires an explicitly
	// confirmed actor for every widening ("任何 private→public … 都要求有
	// 权限的人显式确认").
	AttestationAttributionAnonymous AttestationAttribution = "anonymous"
	// AttestationAttributionNamed: the organization may be named, and
	// attestations that ask for it are named.
	AttestationAttributionNamed AttestationAttribution = "named"
)

// AttestationAttributions is the closed vocabulary, in declaration order.
// Anonymous first: it is the default, and a list a UI renders as options
// should put the safe one first.
func AttestationAttributions() []string {
	return []string{string(AttestationAttributionAnonymous), string(AttestationAttributionNamed)}
}

// ValidAttestationAttribution reports whether v is one of the two values.
//
// It is the same closed vocabulary the column's CHECK enforces: the
// application refuses an unknown value with a sentence, and the database
// refuses the write unconditionally. A caller reaching the store with
// something else would be a defect, not a request.
func ValidAttestationAttribution(v string) bool {
	switch AttestationAttribution(v) {
	case AttestationAttributionAnonymous, AttestationAttributionNamed:
		return true
	}
	return false
}

// attributionValidationError is the one sentence an unknown value gets.
func attributionValidationError(v string) error {
	return fmt.Errorf("%w: attestation_attribution %q is not one of %s, %s",
		ErrValidation, v, AttestationAttributionAnonymous, AttestationAttributionNamed)
}
