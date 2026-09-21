package attestations

import "time"

// Row is one attestations row as the PUBLIC read resolves it: the public
// half, the target it names, and the two attribution inputs.
//
// It has no field for the attesting project, the basis state or the
// internal review, and that is the privacy argument made structural rather
// than procedural: ResolvePublicAttestation
// (internal/persistence/queries/attestations.sql) does not SELECT any of
// the three, so a projection cannot disclose what the query never handed
// it. A field added here would have to be added to that query first, which
// is exactly the review a disclosure should have to pass.
//
// The row is exported because the store fills it in, and Present is its
// only renderer. Nothing else in the package reads it.
type Row struct {
	// PID is the attestation's persistent identifier.
	PID string
	// ValidationType and ValidationResult are the recorded public half.
	ValidationType   string
	ValidationResult string
	// OrgVisibility is the attribution RECORDED when the statement was
	// issued — the promise. It is not the answer to "is the organization
	// named now": see Present.
	OrgVisibility string
	// CreatedAt is the row's creation instant.
	CreatedAt time.Time
	// Target is the public version the attestation is about.
	Target RowTarget
	// Organization is the organization the attestation recorded, with its
	// standing setting as it stands NOW, or nil when it recorded none.
	Organization *RowOrganization
}

// RowTarget is the version an attestation is about, as read.
type RowTarget struct {
	// Kind is derived by the store, never stored: an object target's kind
	// is its object_type ('protocol' or 'claim'), an asset target's is
	// TargetKindAsset.
	Kind TargetKind
	// ObjectID is the scientific object id, or the research asset id.
	ObjectID string
	// VersionID is the pinned version id.
	VersionID string
	// Title is the version's title as stored.
	Title string
}

// RowOrganization is the attesting organization as the public read
// resolves it: its identity, and its standing setting read at the moment
// of this read rather than at the moment of the write.
type RowOrganization struct {
	// ID is organizations.id.
	ID string
	// Slug and Name are the organization's public identity.
	Slug string
	Name string
	// Setting is organizations.attestation_attribution AS IT STANDS NOW.
	Setting string
}

// Present is the ONE place that turns a row into what a public reader may
// see, and it holds exactly one judgement: whether the attesting
// organization is named.
//
// The rule is a CONJUNCTION of two independent gates:
//
//   - the attestation's recorded OrgVisibility, which is the promise made
//     when the statement was issued, and
//   - the organization's CURRENT standing setting
//     (organizations.attestation_attribution, migration 00120),
//
// and the narrower of the two wins. An organization that flips to
// anonymous stops being named on everything it ever issued; flipping back
// does not re-name the attestations issued while it was anonymous. Both
// directions are the one rule, and both are what a reader of a privacy
// surface should expect: a standing setting cannot retroactively break a
// promise made under it, and it cannot fail to retract a disclosure it
// exists to control.
//
// # What Present does NOT decide
//
// It does not re-check the target's visibility, and it does not re-run
// Judge. What was admissible when it was written stays readable: an object
// that later pins a restrictive policy, or a project that later goes
// private, does not un-say a statement that was made in public. A
// retraction is a future design (migration 00120 records that V1 has
// none), and re-judging here would be this function inventing one.
//
// # The two anonymous cases are one answer
//
// A row with no organization at all and a row whose organization may not
// be named both render as anonymous, and the document does not tell them
// apart. That is deliberate: which of the two holds is a fact about the
// attesting project's private side, and the projection's job is to not
// disclose it. What a reader learns is the honest minimum — the attester
// is not named.
func Present(row Row) PublicAttestation {
	mode := OrgVisibilityAnonymous
	var named *NamedOrganization
	if row.OrgVisibility == OrgVisibilityNamed &&
		row.Organization != nil &&
		row.Organization.Setting == OrgVisibilityNamed {
		mode = OrgVisibilityNamed
		named = &NamedOrganization{
			ID:   row.Organization.ID,
			Slug: row.Organization.Slug,
			Name: row.Organization.Name,
		}
	}
	return PublicAttestation{
		PID:              row.PID,
		ValidationType:   row.ValidationType,
		ValidationResult: row.ValidationResult,
		CreatedAt:        FormatInstant(row.CreatedAt),
		Target: PublicTarget{
			Kind:      row.Target.Kind,
			ObjectID:  row.Target.ObjectID,
			VersionID: row.Target.VersionID,
			Title:     row.Target.Title,
		},
		AttributedBy: AttributedBy{Mode: mode, Organization: named},
		Disclosure: Disclosure{
			IsEvidence:    false,
			NamesEvidence: false,
			Statement:     Statement,
		},
	}
}
