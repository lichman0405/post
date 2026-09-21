package attestations

import "fmt"

// Want is the public half a caller is asking for: what the attestation
// would say if it were written. It is the request's value set, validated
// before anything is read, and the preview renders what it would produce.
type Want struct {
	// ValidationType is one of ValidationTypes().
	ValidationType string
	// ValidationResult is one of ValidationResults().
	ValidationResult string
	// OrgVisibility is the attribution the caller asks to RECORD: one of
	// OrgVisibilities(). "named" is only admissible when the organization
	// permits it (Judge).
	OrgVisibility string
}

// Named reports whether the caller asked for the organization to be named.
func (w Want) Named() bool { return w.OrgVisibility == OrgVisibilityNamed }

// DisclosureItem is one entry of the disclosure impact list: a single fact
// the attestation either publishes or withholds, named in one line.
//
// The list exists because a widening the caller cannot enumerate is a
// widening the caller cannot consent to (docs/06 §48: a high-impact
// confirmation shows WHAT is about to become public). Here the list is
// short on purpose, and the withheld half is the longer one — which is the
// feature, not an accident.
type DisclosureItem struct {
	// Field names the fact in dotted form ("attestation.validation_type",
	// "attesting_project.id"), so a client can group the list without
	// parsing the prose.
	Field string `json:"field"`
	// Revealed is true for an entry the attestation publishes and false for
	// one it withholds. The two lists carry the flag so a caller can
	// concatenate them and render one table.
	Revealed bool `json:"revealed"`
	// Detail is one sentence about the entry.
	Detail string `json:"detail"`
}

// Preview is the attestation preview: what the publish would make public,
// what it would keep private, and every entry that blocks it.
//
// It is computed from the SAME facts and the SAME Judge the publish runs,
// so what a caller previewed and what a refusal reports cannot be two
// different documents (knowledgepublish.PreviewOf makes the same choice for
// the same reason).
//
// It is shown to a caller who has already been authorized as a member of
// the attesting project, so echoing ProjectID here discloses nothing that
// caller did not already know.
type Preview struct {
	// ProjectID is the attesting project — the project the caller is a
	// member of.
	ProjectID string `json:"project_id"`
	// Target is the version the attestation would name.
	Target PublicTarget `json:"target"`
	// ValidationType and ValidationResult are what it would record.
	ValidationType   string `json:"validation_type"`
	ValidationResult string `json:"validation_result"`
	// OrgVisibility is the attribution that would be RECORDED on the row.
	OrgVisibility string `json:"org_visibility"`
	// OrganizationSetting is the attesting organization's standing setting
	// as it stands now, or empty when the project has no organization. It
	// is echoed so a caller can see which of the two gates decided
	// WouldNameOrganization rather than being told only the answer.
	OrganizationSetting string `json:"organization_setting,omitempty"`
	// WouldNameOrganization is whether the PUBLISHED record would name the
	// organization — the conjunction of the requested OrgVisibility and
	// the organization's standing setting (Present's rule, applied to the
	// value this request would store).
	WouldNameOrganization bool `json:"would_name_organization"`
	// Published lists every fact the attestation would make public.
	Published []DisclosureItem `json:"published"`
	// Withheld lists every private fact it would NOT publish. This is the
	// half that matters: a caller has to be able to see that the
	// underlying evidence stays where it is.
	Withheld []DisclosureItem `json:"withheld"`
	// Blocking lists every entry that would refuse the publish, in Judge's
	// order.
	Blocking []Reason `json:"blocking"`
	// Admissible is Blocking being empty — the preview's own answer to
	// "would this be admitted", not a promise that it will be (the publish
	// re-runs Judge over its own transaction's view).
	Admissible bool `json:"admissible"`
}

// BuildPreview renders the preview of a decision input and a requested
// public half. The store uses it for the refusal it reports, so a refusal
// and the preview that preceded it are one document.
//
// # The target it renders is one the caller could read
//
// This function echoes the target's title and both of its ids, and it is
// handed to a caller that has been refused — including, through the 409
// body, a caller whose refusal was ABOUT the target. That is safe for one
// reason and it is not a property of this function: the facts only ever
// exist for a target the resolving reader could read. The store's target
// read is reader-relative (ResolveRequest.ReaderID), so a version the
// caller may not read resolves to no row and answers ErrTargetNotFound
// before any Facts is built — there is no unreadable target here to redact,
// which is why this function does not try to decide what may be shown. A
// caller editing this file must keep that split: the readability rule
// belongs to the read (ADR-024), not to a rendering step.
func BuildPreview(f Facts, w Want) Preview {
	blocking := Judge(f, w.Named())
	return Preview{
		ProjectID:             f.ProjectID,
		Target:                publicTargetOf(f),
		ValidationType:        w.ValidationType,
		ValidationResult:      w.ValidationResult,
		OrgVisibility:         w.OrgVisibility,
		OrganizationSetting:   f.OrganizationSetting,
		WouldNameOrganization: w.Named() && f.AttributionPermitted(),
		Published:             revealedItems(f, w),
		Withheld:              withheldItems(f, w),
		Blocking:              blocking,
		Admissible:            len(blocking) == 0,
	}
}

// publicTargetOf renders the target the attestation would name, in the
// shape the public read returns it.
func publicTargetOf(f Facts) PublicTarget {
	return PublicTarget{
		Kind:      f.Target.Kind,
		ObjectID:  f.Target.ObjectID,
		VersionID: f.Target.VersionID,
		Title:     f.Target.Title,
	}
}

// revealedItems is the half of the impact list the attestation publishes.
// Every entry here is a field of PublicAttestation or PublicTarget — the
// list is DERIVED from the wire shape rather than written beside it, so the
// two cannot drift apart into a preview that under-reports a disclosure.
func revealedItems(f Facts, w Want) []DisclosureItem {
	attribution := "the record says the attester is not named, and does not say whether an organization was involved"
	if w.Named() && f.AttributionPermitted() {
		attribution = "the record names the attesting organization — its id, slug and name — which its own standing setting permits"
	}
	return []DisclosureItem{
		{Field: "attestation.pid", Revealed: true,
			Detail: "the attestation's own persistent identifier, minted at write time"},
		{Field: "attestation.validation_type", Revealed: true,
			Detail: fmt.Sprintf("the kind of validation performed (%q), from the published vocabulary", w.ValidationType)},
		{Field: "attestation.validation_result", Revealed: true,
			Detail: fmt.Sprintf("how it went (%q) — confirmed, refuted or inconclusive, and no number: nothing here is scored or weighted", w.ValidationResult)},
		{Field: "attestation.created_at", Revealed: true,
			Detail: "when the statement was made"},
		{Field: "attestation.attributed_by", Revealed: true,
			Detail: attribution},
		{Field: "target.kind", Revealed: true,
			Detail: fmt.Sprintf("that the target is a %s — it is public, so this names nothing new", string(f.Target.Kind))},
		{Field: "target.object_id", Revealed: true,
			Detail: "the public object or asset the attestation is about"},
		{Field: "target.version_id", Revealed: true,
			Detail: "the pinned version it is about — a pin, not a moving reference, so the attestation keeps its meaning when the object moves on"},
		{Field: "target.title", Revealed: true,
			Detail: "the public version's title as stored"},
	}
}

// withheldItems is the half of the impact list that stays private — the
// entries a caller has to be able to see are NOT in the record. It is
// derived from Row's shape the same way: Row has no field for any of these,
// which is why they can be listed with confidence rather than with hope.
func withheldItems(f Facts, w Want) []DisclosureItem {
	items := []DisclosureItem{
		{Field: "attesting_project", Revealed: false,
			Detail: "which project issued the attestation — its id, slug, name, visibility and membership are not read by the public query and have no field in the public projection"},
		{Field: "attesting_project.basis_state", Revealed: false,
			Detail: "the project state the attestation rests on, and through it the manifest of private work underneath: cited by id on the row, never selected by the public read"},
		{Field: "attesting_project.internal_review", Revealed: false,
			Detail: "the internal review that authorised the statement, its reviewer, its decision and the pull request it was recorded on: cited by id on the row, never selected by the public read"},
		{Field: "attesting_project.evidence", Revealed: false,
			Detail: "the underlying private evidence itself — this is structural rather than procedural: attestations has no column a reasoning note, a citation or an excerpt could be written into (migration 00120), which is what makes an attestation not-evidence rather than evidence-that-is-hidden"},
		{Field: "attesting_project.counts", Revealed: false,
			Detail: "no count of anything is published anywhere on this surface, on any route — docs/23 §5: private object counts must not leak through the public API"},
	}
	if !(w.Named() && f.AttributionPermitted()) {
		items = append(items, DisclosureItem{Field: "attesting_organization", Revealed: false,
			Detail: fmt.Sprintf("the attesting organization's identity is not named (%s); the record says only that the attester is not named, and does not say whether an organization was involved at all", attributionRule(f))})
	}
	return items
}
