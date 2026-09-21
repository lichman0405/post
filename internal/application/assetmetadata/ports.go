package assetmetadata

import (
	"context"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters
// live in internal/persistence). The revision command needs exactly two
// things outside itself — the actor's membership role in the project, and
// the store that owns the revision transaction.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore; a project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — which is what the role gate is built on:
// an unknown project is an unknown membership, not a disclosed absence
// (see Command.requireManager). The interface is the one
// assetpublish.MembershipPort declares, deliberately spelled the same
// way, because it is the same question about the same table.
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// StorePort is the metadata store: one transaction that locks the asset,
// reads the metadata it is about to replace, writes the new values and
// appends the audit row.
//
// There is exactly one method, and there is no read method, because a
// revision is not a read-modify-write the command performs: the before
// values are read INSIDE the transaction that overwrites them, under the
// same row lock. A command that read the current metadata first and
// passed it down would be recording a before value another transaction
// could have moved in between — the window the same-transaction rule
// exists to close.
type StorePort interface {
	// ReviseMetadata revises the named asset's metadata and returns the
	// stored result. The before/after pair of the audit row is filled
	// HERE, from the locked row, because only the transaction can read
	// the before half.
	//
	// It answers the sentinels of this package. A refusal writes nothing:
	// no column of research_assets changes and no audit row is appended.
	ReviseMetadata(ctx context.Context, req RevisionRequest) (Revision, error)
}

// RevisionRequest is everything one revision needs to execute: which
// asset, in which project, the fields to write, the actor, and the audit
// row skeleton to append.
//
// It carries the audit row rather than building it, so there is one place
// that decides what a revision records (Command.auditEntry) — the same
// split assetpublish.PublishRequest and assetrights.ChangeRequest make:
// the command fixes everything the request itself knows, the store fills
// the half only the transaction can read.
type RevisionRequest struct {
	// ProjectID is the project the revision is authorized in, in text
	// uuid form. The store refuses a revision whose asset's origin
	// project is not this one, so the pair cannot be used to authorize in
	// one project and write in another (the assetrights rule).
	ProjectID string
	// AssetPID is the asset's persistent identifier.
	AssetPID string
	// Changes are the fields to write. A nil field stays unchanged.
	Changes Changes
	// ActorID is the revising user's id.
	ActorID string
	// Audit is the row the store appends inside the transaction. Its
	// TargetRef and ProjectID are already set by the command; its
	// BeforeSummary and AfterSummary are filled by the store from the
	// locked row.
	Audit domain.AuditEntry
}

// Changes is one revision request's field set: a nil pointer means "leave
// this field as it is", and a non-nil pointer means "write this value",
// including the empty one (which is how a field is cleared). It is the
// shape projects.UpdateSettingsInput has, for the same reason — an
// update that cannot express "unchanged" cannot distinguish a client that
// omitted a field from one that cleared it.
//
// Every field is a pointer to the STORED value's own type: *string for a
// text column, *[]string for an array column. A slice is not a pointer
// itself, which is why the list fields are pointers to slices rather than
// bare slices — a bare nil slice cannot be told from an empty one, and
// those two are different requests.
type Changes struct {
	// Title and Slug are the display fields.
	Title *string
	Slug  *string
	// Description is the prose description; the empty string clears it.
	Description *string
	// Keywords, Contact and Documentation are the three list fields; an
	// empty slice clears one.
	Keywords      *[]string
	Contact       *[]string
	Documentation *[]string
	// Cover must be nil. A non-nil value answers
	// *CoverNotSupportedError — the caller is told plainly that the blob
	// channel does not exist yet, instead of having the field silently
	// dropped (the rule settings.go states for Visibility).
	Cover *string
}

// Empty reports whether the request carries no field at all. A request
// with nothing in it is refused: there is no revision to make, and an
// audit row recording one would be a row about nothing.
func (c Changes) Empty() bool {
	return c.Title == nil && c.Slug == nil && c.Description == nil &&
		c.Keywords == nil && c.Contact == nil && c.Documentation == nil
}

// Revision is one stored metadata revision: the asset it was about and
// the metadata as it now stands.
//
// There is no version field and no version label, and that is the
// contract rather than an omission: docs/11 §4's whole point is that this
// write does not produce a scientific version, so a result that named one
// would be describing a revision that did not happen. The record of WHO
// revised WHEN is the audit_log row the same transaction wrote — that is
// what 保留 audit means, and it is why no updated_by/updated_at column
// exists on the asset row to disagree with it.
type Revision struct {
	// ProjectID is the project the revision was authorized in.
	ProjectID string
	// AssetPID is the asset's persistent identifier — the same value it
	// held before the revision. It is returned so the caller can build
	// the URL it already had (assets.AssetURL).
	AssetPID string
	// Metadata is the asset's metadata as stored after the write.
	Metadata assets.AssetMetadata
}
