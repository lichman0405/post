package domain

// The network evidence classes (docs/10 §7, docs/31 Gate D):
//
//	"Published Knowledge Object 页面区分 Origin Evidence、Reviewed External
//	 Evidence、Unreviewed External Evidence。"
//
// These three are a READ-SIDE classification, and they are computed from
// two axes that already exist — deliberately NOT stored as a single
// three-valued column, which would be a second source of truth for the
// review state and would drift from it the first time an assertion was
// reviewed:
//
//	axis 1  origin vs external — is the project that owns the assertion the
//	        project that owns the published version it targets? The caller
//	        resolves the two ends (evidence_assertions.project_id and the
//	        publication's object_version_id -> scientific_object_versions ->
//	        scientific_objects.project_id chain) and calls the comparison
//	        below; ClassifyEvidenceNetwork never asks a stored field.
//	axis 2  reviewed vs unreviewed — EvidenceReviewState, which
//	        evidence_assertions has carried since migration 00007.
//
// Nothing here scores, ranks, merges or adjudicates: two assertions on one
// pair stay two assertions, each in its own class (CLAUDE.md §9.13 — no
// Truth Score — and §9.12 — correlation is never promoted to a verdict).
// docs/10 §8's descriptive labels (limited evidence, mixed evidence,
// actively contested, independently reproduced) are NOT computed here: their
// rules are a scientific-semantics decision that no specification fixes, so
// this build produces none of them rather than inventing thresholds.

// EvidenceOrigin is docs/10 §3's "external/internal" field as stored: the
// assertion's own provenance record of which side of axis 1 it was written
// from. 'internal' means the asserting project is the project that owns the
// published version the assertion targets; 'external' means another project
// asserted it.
//
// The READ deliberately does not consult this column: it recomputes the
// comparison from evidence_assertions.project_id and the publication chain,
// so a record that could be edited cannot move a row between buckets (see
// migration 00091's header). The column is the write-time record; the
// classification is computed.
type EvidenceOrigin string

const (
	EvidenceOriginInternal EvidenceOrigin = "internal"
	EvidenceOriginExternal EvidenceOrigin = "external"
)

// ValidEvidenceOrigin reports whether v is one of the two canonical origin
// values. It mirrors migration 00091's CHECK.
func ValidEvidenceOrigin(v EvidenceOrigin) bool {
	return v == EvidenceOriginInternal || v == EvidenceOriginExternal
}

// EvidenceVisibility is the assertion's own visibility axis (docs/10 §3's
// "visibility" field, migration 00091): whether the assertion may be
// rendered by the public network read of a published knowledge object. The
// vocabulary is the platform's two presets — the same values projects and
// branches declare — and 'private' is the fail-closed member: an assertion
// nothing explicitly made public is rendered nowhere (docs/12 §5: missing
// information is never a reason to widen).
type EvidenceVisibility string

const (
	EvidenceVisibilityPublic  EvidenceVisibility = "public"
	EvidenceVisibilityPrivate EvidenceVisibility = "private"
)

// ValidEvidenceVisibility reports whether v is one of the two canonical
// visibility values. It mirrors migration 00091's CHECK.
func ValidEvidenceVisibility(v EvidenceVisibility) bool {
	return v == EvidenceVisibilityPublic || v == EvidenceVisibilityPrivate
}

// EvidenceNetworkClass is one of the three classes a published knowledge
// object's evidence is presented under (docs/10 §7).
type EvidenceNetworkClass string

const (
	// EvidenceClassOrigin: the assertion was made by the project that owns
	// the published version — the endpoint's Origin Evidence.
	EvidenceClassOrigin EvidenceNetworkClass = "origin"
	// EvidenceClassReviewedExternal: another project asserted it and the
	// assertion carries an explicit 'reviewed' review state.
	EvidenceClassReviewedExternal EvidenceNetworkClass = "reviewed_external"
	// EvidenceClassUnreviewedExternal: another project asserted it and it
	// does not carry 'reviewed' — which includes 'rejected'.
	//
	// 'rejected' deliberately lands here rather than getting a class of its
	// own: the contract's vocabulary is three classes, and "reviewed" is a
	// positive claim that only review_state = 'reviewed' supports. A
	// rejected assertion is not dropped (CLAUDE.md §9.8: nothing
	// disappears; state only evolves) and it is not laundered into
	// "reviewed": it is presented with its own review_state, under the
	// class that claims nothing about it.
	EvidenceClassUnreviewedExternal EvidenceNetworkClass = "unreviewed_external"
)

// ClassifyEvidenceNetwork places one assertion in its class: `external`
// reports whether the asserting project differs from the project that owns
// the published version (the caller's comparison, never a stored flag), and
// reviewState is the assertion's own review state as stored.
//
// The function is total: an unknown review state is treated exactly like
// every non-'reviewed' value — fail closed, a class that claims no review.
func ClassifyEvidenceNetwork(external bool, reviewState EvidenceReviewState) EvidenceNetworkClass {
	if !external {
		return EvidenceClassOrigin
	}
	if reviewState == EvidenceReviewReviewed {
		return EvidenceClassReviewedExternal
	}
	return EvidenceClassUnreviewedExternal
}

// CanonicalEvidenceNetworkClasses returns the three classes in the order the
// read surface presents them (origin, then the two external halves).
// The integration drift test pins this list to the response shape.
func CanonicalEvidenceNetworkClasses() []EvidenceNetworkClass {
	return []EvidenceNetworkClass{
		EvidenceClassOrigin,
		EvidenceClassReviewedExternal,
		EvidenceClassUnreviewedExternal,
	}
}

// AssertionIsExternal is axis 1 as one comparison: true when the project
// that owns the assertion is not the project that owns the version the
// assertion targets. Both ids are storage ids of projects; an empty one
// classifies the assertion as external — the fail-closed direction (an
// unknown owner is never presented as the origin's own evidence, and two
// unresolved ids do not establish that the two are the same project).
func AssertionIsExternal(assertingProjectID, targetVersionProjectID string) bool {
	if assertingProjectID == "" || targetVersionProjectID == "" {
		return true
	}
	return assertingProjectID != targetVersionProjectID
}
