package rights

import "strings"

// Permission is the value of one of the four permissive usage
// declarations — commercial use, derivatives, redistribution, model
// training (specs/policies/rights-template.yaml usage, docs/12 §4).
//
// The vocabulary is closed and deliberately three-valued: "unspecified"
// is a real answer, not a missing one. An asset published without a
// declared position on commercialization is not the same statement as one
// declared restricted, and collapsing the two would invent a permission
// nobody granted — so a declaration that says nothing stays unspecified
// and the UI renders it as "not declared" rather than as a green light.
type Permission string

const (
	// PermissionAllowed: the declaration permits the use.
	PermissionAllowed Permission = "allowed"
	// PermissionRestricted: the declaration forbids it (outside whatever
	// the named license or agreement separately grants).
	PermissionRestricted Permission = "restricted"
	// PermissionUnspecified: the declaration does not address it.
	PermissionUnspecified Permission = "unspecified"
)

// AllPermissions returns the vocabulary in declaration order (stable — it
// is the order a form or a summary lists the values in).
func AllPermissions() []Permission {
	return []Permission{PermissionAllowed, PermissionRestricted, PermissionUnspecified}
}

// Valid reports whether p is one of the three permissions.
func (p Permission) Valid() bool {
	switch p {
	case PermissionAllowed, PermissionRestricted, PermissionUnspecified:
		return true
	}
	return false
}

// ParsePermission converts a raw value, accepting nothing outside the
// vocabulary.
func ParsePermission(s string) (Permission, bool) {
	p := Permission(strings.TrimSpace(s))
	if !p.Valid() {
		return "", false
	}
	return p, true
}

// Attribution is the value of usage.attribution (docs/12 §4 lists
// attribution among the machine fields; specs/policies/rights-template.yaml
// gives it its own three values). It is NOT the contribution-credit
// vocabulary: credit_attributions (docs/13) records who contributed to a
// research object, and this field records what a reuser of the published
// asset must do about naming it.
type Attribution string

const (
	// AttributionRequired: a reuser must credit the origin.
	AttributionRequired Attribution = "required"
	// AttributionNotRequired: the declaration does not require credit
	// (which is not a statement that credit would be unwelcome).
	AttributionNotRequired Attribution = "not_required"
	// AttributionUnspecified: the declaration does not address it.
	AttributionUnspecified Attribution = "unspecified"
)

// AllAttributions returns the vocabulary in declaration order.
func AllAttributions() []Attribution {
	return []Attribution{AttributionRequired, AttributionNotRequired, AttributionUnspecified}
}

// Valid reports whether a is one of the three attribution values.
func (a Attribution) Valid() bool {
	switch a {
	case AttributionRequired, AttributionNotRequired, AttributionUnspecified:
		return true
	}
	return false
}

// ParseAttribution converts a raw value, accepting nothing outside the
// vocabulary.
func ParseAttribution(s string) (Attribution, bool) {
	a := Attribution(strings.TrimSpace(s))
	if !a.Valid() {
		return "", false
	}
	return a, true
}

// PatentGrant is the value of usage.patent_grant: what the declaration
// says about patent rights (docs/12 §4: "patent grant note").
//
// It has no "unspecified" value, unlike the other four usage declarations
// — the template's vocabulary is none|see_agreement|explicit — so the
// absence of a grant is stated as none. Whether a grant is enforceable,
// and what a stated one covers, is the agreement's business and never
// this field's (docs/38: 平台不负责替用户判断专利有效性).
type PatentGrant string

const (
	// PatentGrantNone: the declaration grants no patent rights.
	PatentGrantNone PatentGrant = "none"
	// PatentGrantSeeAgreement: the grant is stated in the referenced
	// custom agreement (a bare see_agreement with no ref names nothing;
	// Validate refuses that pair).
	PatentGrantSeeAgreement PatentGrant = "see_agreement"
	// PatentGrantExplicit: the declaration states the grant itself.
	PatentGrantExplicit PatentGrant = "explicit"
)

// AllPatentGrants returns the vocabulary in declaration order.
func AllPatentGrants() []PatentGrant {
	return []PatentGrant{PatentGrantNone, PatentGrantSeeAgreement, PatentGrantExplicit}
}

// Valid reports whether g is one of the three patent-grant values.
func (g PatentGrant) Valid() bool {
	switch g {
	case PatentGrantNone, PatentGrantSeeAgreement, PatentGrantExplicit:
		return true
	}
	return false
}

// ParsePatentGrant converts a raw value, accepting nothing outside the
// vocabulary.
func ParsePatentGrant(s string) (PatentGrant, bool) {
	g := PatentGrant(strings.TrimSpace(s))
	if !g.Valid() {
		return "", false
	}
	return g, true
}

// Usage is the usage declaration block of a Document. Every field is the
// declaration's own statement about one use of the published asset — what
// a reuser may do, what they must do, and what the declaration says about
// patents.
//
// The block is the machine-readable half of docs/38 §1: it records what
// the publisher declared. It does not interpret the license text, and a
// disagreement between the two is not resolved here.
type Usage struct {
	// CommercialUse is the declaration's position on commercial use.
	CommercialUse Permission `json:"commercial_use"`
	// Derivatives is the position on derivative works.
	Derivatives Permission `json:"derivatives"`
	// Redistribution is the position on redistribution.
	Redistribution Permission `json:"redistribution"`
	// ModelTraining is the position on use for model training
	// (docs/12 §4 names it explicitly; it is the axis that a
	// license-text reading of "derivatives" would leave ambiguous).
	ModelTraining Permission `json:"model_training"`
	// Attribution is what a reuser must do about crediting the origin.
	Attribution Attribution `json:"attribution"`
	// PatentGrant is what the declaration says about patent rights.
	PatentGrant PatentGrant `json:"patent_grant"`
}

// Validate returns nil, or one *ValidationError per field whose value is
// outside its vocabulary, in field order.
func (u Usage) Validate() error {
	return joinErrors(
		validatePermission("usage.commercial_use", u.CommercialUse),
		validatePermission("usage.derivatives", u.Derivatives),
		validatePermission("usage.redistribution", u.Redistribution),
		validatePermission("usage.model_training", u.ModelTraining),
		validateAttribution(u.Attribution),
		validatePatentGrant(u.PatentGrant),
	)
}

func validatePermission(field string, p Permission) error {
	if p.Valid() {
		return nil
	}
	return &ValidationError{
		Code:   CodeInvalidUsageValue,
		Field:  field,
		Detail: "expected one of allowed|restricted|unspecified, got " + quote(string(p)),
	}
}

func validateAttribution(a Attribution) error {
	if a.Valid() {
		return nil
	}
	return &ValidationError{
		Code:   CodeInvalidUsageValue,
		Field:  "usage.attribution",
		Detail: "expected one of required|not_required|unspecified, got " + quote(string(a)),
	}
}

func validatePatentGrant(g PatentGrant) error {
	if g.Valid() {
		return nil
	}
	return &ValidationError{
		Code:   CodeInvalidUsageValue,
		Field:  "usage.patent_grant",
		Detail: "expected one of none|see_agreement|explicit, got " + quote(string(g)),
	}
}
