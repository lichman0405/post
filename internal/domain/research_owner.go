package domain

import (
	"strings"
	"time"
)

// Scientific responsibility and Research Owners routing (T0604, docs/04
// §3). The section is two sentences and both are load-bearing:
//
//	「不与 Access Role 混合。项目可配置：Experimental Reviewer、
//	  Computational Reviewer、Data Reviewer、Project Lead、IP Reviewer
//	  等责任标签。责任用于 Review routing，不自动赋予更高访问权限。」
//	「类似 CODEOWNERS 的 Research Owners 规则可按对象类型/Schema/领域
//	  匹配 reviewer。」
//
// Responsibilities are therefore PROJECT DATA, not a closed vocabulary in
// code: the label set is open ("等" — and the examples are examples), and
// the mapping from a change to the responsible label is per project. The
// canonical rows live in research_owner_rules and
// responsibility_assignments (migration 00084); this file carries their
// shape and their validation, and nothing here touches authorization:
// docs/04 §3's「不自动赋予更高访问权限」is enforced by internal/authz
// never reading these values at all.

// MaxResponsibilityLabelLen bounds a responsibility label. It mirrors
// ValidReviewResponsibility (the stored reviews.responsibility bound), so
// a label a project configures is always storable on the review that
// acts under it.
const MaxResponsibilityLabelLen = 200

// ValidResponsibilityLabel reports whether label is a usable
// responsibility label: non-blank after trimming, and bounded. The
// vocabulary itself is the project's; this only bounds the stored text
// (the same discipline as domain.ValidReviewResponsibility).
func ValidResponsibilityLabel(label string) bool {
	t := strings.TrimSpace(label)
	return t != "" && len(t) <= MaxResponsibilityLabelLen
}

// ResearchOwnerMatchKind is how one Research Owners rule matches a change
// (docs/04 §3: 对象类型/Schema/领域).
type ResearchOwnerMatchKind string

const (
	// ResearchOwnerMatchObjectType matches the scientific object's type —
	// the canonical V1 type name ("protocol", "experiment", "dataset", …)
	// or any type a later schema adds (the registry, not this list,
	// decides what is canonical).
	ResearchOwnerMatchObjectType ResearchOwnerMatchKind = "object_type"
	// ResearchOwnerMatchSchema matches the schema reference the object
	// version pins: a canonical type schema id
	// (https://open-rd.example/schemas/<type>.schema.json, or the bare
	// "$id" a client may pass) or a project schema profile id
	// ("project:<project_id>:<name>", T0213).
	ResearchOwnerMatchSchema ResearchOwnerMatchKind = "schema"
	// ResearchOwnerMatchDomain matches the object payload's `domain`
	// field — the subject area docs/04 §3 names as 「领域」 (the protocol
	// schema's domain property). A payload without one matches no domain
	// rule: an absent field is not a wildcard.
	ResearchOwnerMatchDomain ResearchOwnerMatchKind = "domain"
)

// ValidResearchOwnerMatchKind reports whether kind is one of the three
// documented match kinds (the table CHECK, mirrored so the service
// refuses before the store).
func ValidResearchOwnerMatchKind(kind ResearchOwnerMatchKind) bool {
	switch kind {
	case ResearchOwnerMatchObjectType, ResearchOwnerMatchSchema, ResearchOwnerMatchDomain:
		return true
	}
	return false
}

// ResearchOwnerRule is one CODEOWNERS-like routing rule: a change matched
// by (kind, value) in the project requires the responsibility label.
// Canonical table research_owner_rules (migration 00084).
type ResearchOwnerRule struct {
	// ID is the uuid v4 text form of the rule row.
	ID string
	// ProjectID is the research boundary the rule belongs to.
	ProjectID string
	// MatchKind is how the rule matches a change.
	MatchKind ResearchOwnerMatchKind
	// MatchValue is the matched value (an object type, a schema id or a
	// domain), 1..200 characters, already trimmed.
	MatchValue string
	// Responsibility is the label the rule routes the change to.
	Responsibility string
	// CreatedBy names the user who wrote the rule.
	CreatedBy string
	CreatedAt time.Time
}

// ResponsibilityAssignment records that a user holds a responsibility
// label in a project. Canonical table responsibility_assignments
// (migration 00084). It grants nothing beyond the routing itself
// (docs/04 §3).
type ResponsibilityAssignment struct {
	ProjectID      string
	UserID         string
	Responsibility string
	CreatedBy      string
	CreatedAt      time.Time
}

// ValidResearchOwnerMatchValue reports whether value is a usable match
// value: 1..200 characters and already trimmed (the table CHECK, mirrored
// — a rule whose value carries surrounding whitespace matches nothing and
// would silently stop routing).
func ValidResearchOwnerMatchValue(value string) bool {
	return value != "" && len(value) <= MaxResponsibilityLabelLen && value == strings.TrimSpace(value)
}
