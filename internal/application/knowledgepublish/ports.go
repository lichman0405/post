package knowledgepublish

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// Ports (docs/52: the application orchestrates against ports; adapters live
// in internal/persistence). The publish command needs four things outside
// itself — the actor's membership role, the review record of the version's
// lineage, the store that owns the publish transaction, and the pid
// generator.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore; a project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — which is the resolution the publish
// authorization is built on: an unknown project is an unknown membership,
// not a disclosed absence (see Command.requirePublish).
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// ReviewPort is NOT a port of this command, and there is deliberately no
// such interface: the review record of the version's lineage into main is
// resolved by StorePort.ResolveFacts, over the same transaction (or pool)
// the rest of the decision is resolved over. A separate review port would
// be a SECOND read of the same record, outside the transaction that
// decides — the publish would then be deciding on a record it read from a
// different snapshot than the one it writes against, which is the exact
// hazard docs/22 §7's re-run exists to close.
//
// The record's shape is still internal/application/releases': a
// publication requires exactly the evidence a release requires, and the
// two must be the SAME read, not two reads that agree today. The store
// reads it with the release store's own query and its own grouping helper
// (persistence.listReleaseReviewsFromRows), so "this version passed
// review" has one meaning on both surfaces.

// StorePort is the publish store: the read that resolves what the
// decision is about, the idempotency ledger read the command checks before
// doing any work, and the publish transaction itself.
//
// The split follows assetpublish.StorePort, for the same reason: a replay
// is a READ, and it must be possible to answer one without resolving
// anything else. The command's LookupCreation call therefore precedes
// everything the publish would otherwise compute — and the store re-checks
// the ledger inside its own transaction anyway, under the project row
// lock, because the read outside it is an optimization and the one inside
// it is the guarantee.
type StorePort interface {
	// ResolveFacts reads what the publication decision is about: the
	// version, the project that owns it, the version's own visibility
	// axis, the review record of its lineage into main, and its existing
	// publication (nil when it has none). It answers ErrVersionNotFound
	// when the version does not exist or belongs to another project.
	//
	// It is the read-only half the preview route runs, and the same
	// resolution the publish re-runs inside its transaction.
	ResolveFacts(ctx context.Context, projectID, objectVersionID string) (Facts, error)

	// LookupCreation returns the publication an Idempotency-Key already
	// wrote, or nil when the key has no ledger entry yet.
	LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*Published, error)

	// Publish runs the publish: re-check the ledger, resolve the facts over
	// THIS transaction's view, re-run Judge over them, and — only if that
	// refuses nothing — write the immutable publication row, the ledger
	// entry, the audit row and the research event in ONE transaction.
	//
	// It answers *PublicationRefused when the re-run refused, the
	// package's sentinels otherwise. It never writes a partial publish: a
	// refused request leaves no row of any kind behind.
	Publish(ctx context.Context, req PublishRequest) (Published, error)
}

// ReadPort is the public read model of a published knowledge object
// (GET /knowledge/{knowledgeId}).
//
// It resolves by pid and NEVER filters by audience: the audience rule is
// AudienceFor, in Go, and having a second, SQL-shaped copy of it here is
// how two answers to "who may read this" start to disagree. The caller
// applies AudienceFor to what this returns.
type ReadPort interface {
	// GetPublishedKnowledge returns the publication the pid names, and
	// false when the pid names no publication (or is not a pid at all —
	// neither can resolve, and the two must be indistinguishable to a
	// caller).
	GetPublishedKnowledge(ctx context.Context, pid string) (PublishedKnowledge, bool, error)
}

// PublishParams is one publish request.
type PublishParams struct {
	// ProjectID is the project the publish happens in. The version must
	// belong to it: a publish never resolves a version across the project
	// boundary.
	ProjectID string
	// ObjectVersionID is the scientific object version being published.
	// The transport resolves it from the request's knowledge_version_ref
	// (object_version:<uuid>); it is a uuid by the time it arrives here.
	ObjectVersionID string
	// PublicVersion is the name the publication is shown under. It is the
	// PUBLISHER's string, stored byte for byte (owner ruling
	// L3-20260916-1 #2): this command refuses a missing, blank or
	// oversized one and normalises nothing.
	PublicVersion string
	// Rights is the rights declaration the publication stores, in the
	// shape internal/rights models (specs/policies/rights-template.yaml).
	// It is parsed and validated before anything is read, and the
	// CANONICAL marshalling of the parsed document is what the row stores.
	Rights json.RawMessage
	// IdempotencyKey replays the publication it names (docs/22): the same
	// key returns the publication the first call wrote, forever.
	IdempotencyKey *string
}

// PreviewParams is one preview request: what the publish would be
// published under, without the publication itself. It carries exactly the
// arguments specs/mcp/tools.json gives knowledge.publish_preview
// (knowledge_version_ref, rights) — the tool is a PROPOSAL, so it names no
// public_version: the name is part of executing the proposal, not part of
// asking whether it would be admitted.
type PreviewParams struct {
	ProjectID       string
	ObjectVersionID string
	Rights          json.RawMessage
}

// PublishRequest is everything one publish needs to execute.
type PublishRequest struct {
	// ProjectID is the project the publish happens in, in text uuid form.
	ProjectID string
	// ObjectVersionID is the version being published.
	ObjectVersionID string
	// PID is the publication's persistent identifier, minted by the
	// command before this call (domain.NewPID — never the column DEFAULT,
	// for the reason migration 00064 records for assets: the
	// application-generated path is the publish command's).
	PID string
	// PublicVersion is the publisher's name for the publication, stored
	// verbatim.
	PublicVersion string
	// RightsJSON is the canonical bytes of the parsed rights document, the
	// exact document the row stores and the read model renders.
	RightsJSON json.RawMessage
	// Facts is the decision input the command resolved, carried so a
	// refusal can report the document the caller previewed against. The
	// store re-resolves it inside the transaction and decides on THAT one:
	// this copy is never the basis of a write.
	Facts Facts
	// Actor is the publishing principal.
	Actor Actor
	// Audit is the row the store appends inside the transaction. Its
	// TargetRef is left empty by the command: the store fills it with
	// "knowledge_publication:<id>" once the insert has assigned the id.
	Audit domain.AuditEntry
	// IdempotencyKey is the caller's key, or nil when the request carried
	// none.
	IdempotencyKey *string
}

// Actor is the publishing principal: the authenticated user, and whether
// the request arrived as a platform agent rather than a human session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could
// clear it, and the only thing the flag does is refuse. Same shape and
// same reasoning as assetpublish.Actor.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the
	// token's owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// Published is one published knowledge object version: the stored row plus
// the identity it is addressed by.
//
// PID is the publication's public identity and PublicVersion is the name a
// human reads; the row's own uuid is internal and does not leave the
// process (the asset publish makes the same decision for research_assets).
// That is enforced here rather than left to each rendering: Published has
// one wire shape (MarshalJSON), and the row id is not in it. A caller that
// marshals a Preview — the refusal report carries the existing publication
// — would otherwise publish the internal key.
type Published struct {
	// ID is the knowledge_publications row id (internal; never on the
	// wire).
	ID string
	// PID is the publication's persistent identifier.
	PID string
	// ObjectVersionID is the version that was published.
	ObjectVersionID string
	// PublicVersion is the publisher's name, exactly as stored.
	PublicVersion string
	// RightsJSON is the stored rights document.
	RightsJSON json.RawMessage
	// PublishedBy is the publishing user's id.
	PublishedBy string
	// PublishedAt is the row's creation instant.
	PublishedAt time.Time
}

// publishedWire is Published's wire shape: the identities a client
// addresses, with the row's internal uuid deliberately absent.
type publishedWire struct {
	PID             string          `json:"pid"`
	ObjectVersionID string          `json:"object_version_id"`
	PublicVersion   string          `json:"public_version"`
	Rights          json.RawMessage `json:"rights"`
	PublishedBy     string          `json:"published_by"`
	PublishedAt     string          `json:"published_at"`
}

// InstantLayout is the shape of every instant this surface puts on the
// wire: millisecond precision, UTC. Exported so that the transport's own
// bodies (the 201 of a publish, the 200 of the read) render the same
// instant the same way as the refusal report's existing_publication — two
// spellings of one moment in one API is a client bug waiting to be
// written.
const InstantLayout = "2006-01-02T15:04:05.000Z"

// FormatInstant renders an instant for the wire (see InstantLayout).
func FormatInstant(t time.Time) string { return t.UTC().Format(InstantLayout) }

// MarshalJSON implements json.Marshaler (see Published).
func (p Published) MarshalJSON() ([]byte, error) {
	return json.Marshal(publishedWire{
		PID:             p.PID,
		ObjectVersionID: p.ObjectVersionID,
		PublicVersion:   p.PublicVersion,
		Rights:          p.RightsJSON,
		PublishedBy:     p.PublishedBy,
		PublishedAt:     FormatInstant(p.PublishedAt),
	})
}

// Preview is the publication preview: what would be published, what the
// network would see afterwards, and every entry that blocks it. It is a
// PROPOSAL — it writes nothing and decides nothing; the publish re-runs
// the same decision over its own transaction's view.
type Preview struct {
	// ProjectID is the project the publication would happen in.
	ProjectID string `json:"project_id"`
	// ObjectVersionID, ObjectID and ObjectType name what would be
	// published. The version's TITLE is deliberately absent: a preview
	// answers whether the publication is admissible and who would see it,
	// and returning the content it would publish would hand a caller that
	// may read only the project the version's text as well (the route is
	// gated by the project read, and the project read gate admits
	// non-members of a public project).
	ObjectVersionID string `json:"object_version_id"`
	ObjectID        string `json:"object_id"`
	ObjectType      string `json:"object_type"`
	// PublicVersion is always empty: the preview does not take a name (see
	// PreviewParams). It is carried so a client can render one shape for
	// the preview and the publication.
	PublicVersion string `json:"public_version,omitempty"`
	// Audience is who would be able to read the publication once written —
	// AudienceNetwork or AudienceMembers. It is computed from the version's
	// OWN visibility axis by AudienceFor, and it is the answer to "does
	// publishing this make it public?".
	Audience Audience `json:"audience"`
	// ProjectVisibility is the owning project's preset.
	ProjectVisibility string `json:"project_visibility"`
	// VisibilityPolicyID is the version's own visibility axis: null when
	// the version inherits the project's visibility, a policy id when it
	// does not. It is echoed because it is the input that decides Audience
	// and a caller has to be able to see which one applied.
	VisibilityPolicyID *string `json:"visibility_policy_id"`
	// ReviewApproved reports whether the version passed publication_review.
	ReviewApproved bool `json:"review_approved"`
	// ApprovedReviewKinds names the review dimensions that are approved.
	ApprovedReviewKinds []string `json:"approved_review_kinds"`
	// RequiredReviewKinds names the dimensions a publication requires.
	RequiredReviewKinds []string `json:"required_review_kinds"`
	// Existing is the version's existing publication when it has one, and
	// nil otherwise (owner ruling L3-20260916-1 #3).
	Existing *Published `json:"existing_publication,omitempty"`
	// Blocking lists every entry that would refuse the publication; empty
	// means it would be admitted.
	Blocking []Reason `json:"blocking"`
	// Publishable is Blocking being empty. It is stated rather than left
	// to the client to derive, because it is the one bit the caller acts
	// on.
	Publishable bool `json:"publishable"`
}

// PublishedKnowledge is one published knowledge object as the public read
// resolves it: the publication, the version it published, the object that
// version belongs to, and the inputs AudienceFor decides with.
type PublishedKnowledge struct {
	// PID is the publication's persistent identifier — the id the read was
	// addressed by.
	PID string
	// PublicVersion is the publisher's name, exactly as stored.
	PublicVersion string
	// PublishedBy and PublishedAt are the publication's provenance.
	PublishedBy string
	PublishedAt time.Time
	// Rights is the stored rights document, parsed. RightsValid is false
	// when the stored bytes are not a rights document this build can read
	// (a row written before the model existed, say) — the read answers with
	// the bytes it has, and AudienceFor refuses an unparseable declaration
	// rather than treating it as a licence to publish.
	Rights      rights.Document
	RightsValid bool
	// ObjectVersionID and ObjectID name the version and the object.
	ObjectVersionID string
	ObjectID        string
	// ObjectType is the object's type.
	ObjectType string
	// Title is the version's title.
	Title string
	// LifecycleState is the version's lifecycle_state.
	LifecycleState string
	// SchemaID and SchemaVersion are the version's pinned schema.
	SchemaID      string
	SchemaVersion string
	// IntegrityHash is the version's canonical integrity hash.
	IntegrityHash string
	// VisibilityPolicyID is the version's OWN visibility axis (see Facts).
	VisibilityPolicyID *string
	// ProjectID is the project that owns the object, and
	// ProjectVisibility is its preset. They are returned because the read
	// gate has to run, and because the answer "members only" is one a
	// client should be able to explain.
	ProjectID         string
	ProjectVisibility string
}

// Audience is the audience of the resolved publication, by the one rule.
func (k PublishedKnowledge) Audience() Audience {
	return AudienceFor(k.ProjectVisibility, k.VisibilityPolicyID, k.Rights)
}

// ValidPID reports whether pid has the persistent-identifier shape. It is
// assets.ValidPID — one vocabulary for both the asset and the knowledge
// publication (internal/domain.PID), named here so the command reads the
// same predicate the asset command and the database CHECK do.
func ValidPID(pid string) bool { return assets.ValidPID(pid) }

// MaxPublicVersionLen bounds the publisher's version name.
//
// The column is unbounded text, and a bound is not a normalisation: the
// name is stored exactly as sent (ruling #2), and this refuses a value no
// surface could render rather than truncating one. 256 characters holds a
// version label, a human title and a date, with room to spare; the unit is
// Unicode code points, like every other human-facing bound in this tree
// (internal/rights counts its free-text fields the same way, because the
// documents this platform stores are written in Chinese as often as in
// English).
const MaxPublicVersionLen = 256
