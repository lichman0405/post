package attestations

import (
	"context"
	"time"

	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// ValidPID reports whether pid is a persistent identifier: 26 lowercase
// Crockford base32 characters, the format domain.NewPID mints and the
// attestations.pid CHECK enforces (migration 00120).
//
// It is a re-export of assets.ValidPID — which is itself domain.ValidPID —
// for the reason knowledgepublish.ValidPID records: the read answers
// not-found for a pid that is not a pid, without a database round trip, and
// a surface that spells the predicate itself is a surface that can disagree
// with the columns it addresses. The public read has exactly one caller
// (persistence.AttestationStore), and it uses this.
func ValidPID(pid string) bool { return assets.ValidPID(pid) }

// Ports (docs/52: the application orchestrates against ports; adapters live
// in internal/persistence). The attestation command needs three things
// outside itself — the actor's membership role, the store that owns the
// write transaction, and the public read the pid resolves through.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore; a project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — an unknown project is an unknown membership,
// not a disclosed absence.
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// StorePort is the attestation store: the read that resolves what the
// decision is about, and the write transaction that re-runs that decision
// over its own view.
//
// There is deliberately no separate "review port" or "organization port":
// the internal review and the organization's standing setting are resolved
// by ResolveFacts, over the same transaction (or pool) the rest of the
// decision is resolved over. A separate read outside the transaction that
// decides would be a decision made on a record from a different snapshot
// than the one it writes against — the hazard docs/22 §7's re-run exists to
// close, and the reason knowledgepublish.StorePort bundles the review read
// the same way.
type StorePort interface {
	// ResolveFacts reads what the attestation decision is about: the
	// attesting project and its organization's setting, the target version
	// and its visibility inputs, the basis state and the internal review.
	//
	// It answers ErrTargetNotFound / ErrBasisNotFound / ErrReviewNotFound
	// when a named record does not exist or does not belong to the
	// attesting project — all four are the same "no" to the caller, and a
	// caller that is not a member of the project cannot reach the call at
	// all (the command authorizes before it resolves).
	ResolveFacts(ctx context.Context, in ResolveRequest) (Facts, error)

	// Attest runs the write: re-resolve the facts over THIS transaction's
	// view, re-run Judge over them, and — only if that refuses nothing —
	// insert the attestation row and append the audit row in ONE
	// transaction. It answers *Refused when the re-run refused, the
	// package's sentinels otherwise, and it never writes a partial
	// attestation.
	Attest(ctx context.Context, req AttestRequest) (Attested, error)

	// GetPublicAttestation resolves one attestation by pid and returns it
	// ALREADY PROJECTED by Present: the value is what a public reader may
	// see, not a row that each caller narrows for itself.
	//
	// The projection is part of the read rather than a step after it on
	// purpose. A read that returned the private columns and trusted every
	// caller to drop them is exactly the shape ADR-024 refuses (a rule that
	// lives in each caller rather than in the read), and here the rule is
	// cheap to keep in one place: the query behind this method does not
	// select the private columns at all.
	GetPublicAttestation(ctx context.Context, pid string) (PublicAttestation, bool, error)
}

// ReadPort is the public read model of one attestation
// (GET /api/v1/attestations/{attestationId}). It is a port of the
// transport, not of the command: the publish path never reads back.
type ReadPort interface {
	// GetPublicAttestation returns the attestation the pid names, and false
	// when the pid names no attestation (or is not a pid at all — neither
	// can resolve, and the two are indistinguishable to a caller).
	GetPublicAttestation(ctx context.Context, pid string) (PublicAttestation, bool, error)
}

// TargetKind names what an attestation is about. The requirement names
// three: a public Protocol, a public Claim, or a public Asset.
type TargetKind string

const (
	// TargetKindProtocol: the target is a scientific object version whose
	// object_type is "protocol".
	TargetKindProtocol TargetKind = "protocol"
	// TargetKindClaim: the target is a scientific object version whose
	// object_type is "claim".
	TargetKindClaim TargetKind = "claim"
	// TargetKindAsset: the target is a research asset version.
	TargetKindAsset TargetKind = "asset"
)

// AttestableTargetKinds is the closed vocabulary of target kinds, in the
// order the requirement names them. An object whose type is not in this list
// (an experiment, a dataset object, a finding) is not attestable — the
// requirement names three, and a kind outside them would be this package
// inventing a product rule rather than implementing one.
func AttestableTargetKinds() []TargetKind {
	return []TargetKind{TargetKindProtocol, TargetKindClaim, TargetKindAsset}
}

// Validation types. This is the closed vocabulary of what a third party can
// assert it DID, derived from the three target kinds plus the rights axis
// the platform already models (docs/12 §4):
//
//   - reproduction     — the reported result was reproduced
//   - method_validation — the protocol/method itself was validated
//   - data_audit        — the dataset/asset was audited
//   - rights_review     — the rights/licensing position was reviewed
//
// It is a vocabulary rather than a free string for the same reason
// validation_result is three-valued: a statement that reaches the network
// under a project's name must be one the reader can place, and "other" would
// move the judgment back to the reader.
const (
	ValidationTypeReproduction     = "reproduction"
	ValidationTypeMethodValidation = "method_validation"
	ValidationTypeDataAudit        = "data_audit"
	ValidationTypeRightsReview     = "rights_review"
)

// ValidationTypes is the closed vocabulary, in declaration order.
func ValidationTypes() []string {
	return []string{
		ValidationTypeReproduction,
		ValidationTypeMethodValidation,
		ValidationTypeDataAudit,
		ValidationTypeRightsReview,
	}
}

// Validation results. Three values, no numbers: nothing here is summed,
// averaged or scored (CLAUDE.md §9.13, docs/10 §4 "V1 不自动赋数值权重"),
// and two attestations on one version keep their own results side by side
// exactly as two contradictory evidence assertions keep their stances.
const (
	// ValidationResultConfirmed: the validation was performed and the
	// expected outcome held.
	ValidationResultConfirmed = "confirmed"
	// ValidationResultRefuted: the validation was performed and the
	// expected outcome did NOT hold.
	ValidationResultRefuted = "refuted"
	// ValidationResultInconclusive: the validation was performed and did
	// not settle the question. It is a RESULT, not a missing one — the
	// absence of an attestation is how "nobody validated this" is said.
	ValidationResultInconclusive = "inconclusive"
)

// ValidationResults is the closed vocabulary, in declaration order.
func ValidationResults() []string {
	return []string{ValidationResultConfirmed, ValidationResultRefuted, ValidationResultInconclusive}
}

// Organization attribution vocabulary, RE-EXPORTED from the package that
// owns it: organizations.attestation_attribution is a property of the
// organization (internal/application/orgs), and orgs.AttestationAttribution*
// is its one definition. These constants exist so this package — which
// reads the setting on every decision — can spell it without a second copy
// of the two strings; a second copy is a second vocabulary waiting to
// disagree with the CHECK in migration 00120.
const (
	// OrgVisibilityAnonymous: the attesting organization is not named. It is
	// the DEFAULT on organizations.attestation_attribution, because being
	// named is a widening and docs/12 §3 requires an explicit confirmation
	// for every widening ("任何 private→public … 都要求有权限的人显式确认").
	OrgVisibilityAnonymous = string(orgs.AttestationAttributionAnonymous)
	// OrgVisibilityNamed: the attesting organization is named, when its own
	// standing setting also permits it.
	OrgVisibilityNamed = string(orgs.AttestationAttributionNamed)
)

// OrgVisibilities is the closed vocabulary, in declaration order — the
// organization's list, not a copy of it, so an option added there appears
// here without an edit.
func OrgVisibilities() []string { return orgs.AttestationAttributions() }

// ActionAttestationCreated is the audit action name a successful attest
// appends (the audit_log.action column). It follows the dotted
// `<subject>.<verb-past>` convention of internal/domain's Action* vocabulary
// — release.created, milestone.created, asset.version_published.
//
// It is declared HERE rather than beside those for the reason
// assetpublish.ActionAssetVersionPublished records: internal/domain is not
// writable by this task, so a new audit action had to live in the package
// that owns the surface. The Activity page renders actions and never parses
// them, so the name is read the same way wherever it is declared.
const ActionAttestationCreated = "attestation.created"

// Actor is the attesting principal: the authenticated user, and whether the
// request arrived as a platform agent rather than a human session.
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could clear
// it, and the only thing the flag does is refuse. Same shape and same
// reasoning as knowledgepublish.Actor.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the token's
	// owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// ResolveRequest names what a resolution is about, and FOR WHOM.
type ResolveRequest struct {
	// ProjectID is the attesting project.
	ProjectID string
	// ReaderID is the user the target resolution is made FOR — always the
	// authenticated actor, never a value from the request body.
	//
	// It is here because the target read is READER-RELATIVE: the target is
	// somebody else's version, resolved by an id the caller supplied, and
	// the read may not hand back a row the reader cannot read (ADR-024: the
	// read carries the reader, and the rule lives in the read rather than in
	// each caller). A target the reader may not read resolves to no row,
	// which the store answers as ErrTargetNotFound — the same answer an id
	// that names nothing gets, so this surface cannot be asked "does this
	// version exist" about a version the asker cannot open.
	//
	// It is NOT an authorization input: which projects the actor may ATTEST
	// in is the command's own gate (requirePublish), resolved before any
	// read. This field only says whose eyes the target is resolved through.
	ReaderID string
	// TargetObjectVersionID and TargetAssetVersionID are the target pins;
	// exactly one is set (the shape the request was validated into, and the
	// shape the table's CHECK enforces).
	TargetObjectVersionID string
	TargetAssetVersionID  string
	// BasisStateID is the attesting project's own state the attestation
	// rests on, and InternalReviewID the review that authorised it.
	BasisStateID     string
	InternalReviewID string
}

// AttestRequest is everything one attestation needs to execute.
type AttestRequest struct {
	// PID is the attestation's persistent identifier, minted by the command
	// before this call (domain.NewPID — never the column DEFAULT, for the
	// reason migration 00064 records for assets: the application-generated
	// path is the publish command's).
	PID string
	// Resolve is what the facts are resolved for. The store re-resolves it
	// inside the transaction and decides on THAT: any copy the command
	// carries is never the basis of a write.
	Resolve ResolveRequest
	// ValidationType, ValidationResult and OrgVisibility are the validated
	// public half, stored verbatim.
	ValidationType   string
	ValidationResult string
	OrgVisibility    string
	// Actor is the attesting principal.
	Actor Actor
	// Audit is the row the store appends inside the transaction. Its
	// TargetRef is left empty by the command: the store fills it with
	// "attestation:<pid>" once the insert has assigned the pid.
	Audit domain.AuditEntry
}

// Attested is one stored attestation, as the publish returns it.
//
// It carries the PID — the public identity — and the public half. It carries
// no private id at all: a value that flowed back through the transport with
// attesting_project_id on it would be one rendering bug away from the wire,
// and nothing a caller does with the result needs it.
type Attested struct {
	// PID is the attestation's persistent identifier.
	PID string
	// ValidationType and ValidationResult are the public half, as stored.
	ValidationType   string
	ValidationResult string
	// OrgVisibility is the attribution recorded on the row (the promise),
	// NOT whether the organization ends up named — that is a read-time
	// decision (Present) and the org's setting can narrow it later.
	OrgVisibility string
	// CreatedAt is the row's creation instant.
	CreatedAt time.Time
}

// PublicAttestation is the WHOLE of what a public reader is shown, and the
// return type of the public read. Every field is either the record's own
// public half, the target it is about (which is public by construction — the
// command refuses a target that is not), or an attribution that Present
// decided may be shown.
//
// There is deliberately no field here for: the attesting project (id, slug
// or name), the basis state, the internal review, the underlying evidence,
// or any count of anything (docs/23 §5: "私有对象计数也不能通过 public API
// 泄漏"). Adding one would be adding a disclosure, and the type is the
// place that is decided.
type PublicAttestation struct {
	// PID is the attestation's persistent identifier — the id the read was
	// addressed by.
	PID string `json:"pid"`
	// ValidationType and ValidationResult are the public half.
	ValidationType   string `json:"validation_type"`
	ValidationResult string `json:"validation_result"`
	// CreatedAt is the row's creation instant, rendered by FormatInstant.
	CreatedAt string `json:"created_at"`
	// Target is the public version the attestation is about.
	Target PublicTarget `json:"target"`
	// AttributedBy is who is named as having attested. It is an
	// ORGANIZATION or nobody — never a project, never a user.
	AttributedBy AttributedBy `json:"attributed_by"`
	// Disclosure states, inside the document, what this record is and is
	// not. It is the machine-readable half of the task requirement
	// "明确 attestation != public evidence": a reader (or a crawler) does not
	// have to know the platform's model to learn that this is a statement
	// ABOUT a validation rather than evidence about the target.
	Disclosure Disclosure `json:"disclosure"`
}

// PublicTarget is the version an attestation is about.
//
// Every field here is public by the command's own rule (Judge refuses a
// target that is not), so naming it discloses nothing: the target is the
// object or asset the attestation exists to be about. The attesting side is
// the side that is redacted, and it is redacted by never being read.
type PublicTarget struct {
	// Kind is protocol, claim or asset.
	Kind TargetKind `json:"kind"`
	// ObjectID is the scientific object's id (a protocol/claim target) or
	// the research asset's id (an asset target).
	ObjectID string `json:"object_id"`
	// VersionID is the pinned version the attestation is about. It is a
	// version pin, not a moving reference: the attestation keeps its meaning
	// when the object moves on (the same pinning evidence_assertions uses,
	// 00007).
	VersionID string `json:"version_id"`
	// Title is the version's title as stored.
	Title string `json:"title"`
}

// AttributedBy is who the record names as having attested.
type AttributedBy struct {
	// Mode is anonymous or named. It is stated rather than left to be
	// inferred from Organization being absent, because "the attester is not
	// named" is a fact the record should say out loud rather than one a
	// client has to derive from a null.
	//
	// anonymous covers two situations the document does NOT distinguish: a
	// project with no organization, and an organization that may not be
	// named. Which of the two holds is a fact about the attesting project's
	// private side (see Present).
	Mode string `json:"mode"`
	// Organization is the attesting organization when Mode is "named", and
	// nil otherwise.
	Organization *NamedOrganization `json:"organization,omitempty"`
}

// NamedOrganization is an attesting organization a reader may be shown.
// Only these three fields: the OrganizationProfile's own public shape, minus
// everything that is not an identity.
type NamedOrganization struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// Disclosure is the record's statement about itself.
type Disclosure struct {
	// IsEvidence is always false. It is on the wire rather than implied
	// because the distinction is the feature this task exists to build.
	IsEvidence bool `json:"is_evidence"`
	// NamesEvidence is always false: the record cites no evidence version
	// and carries no reasoning. An evidence ASSERTION names what it cites
	// (evidence_object_version_id) and says why (reasoning_note); an
	// attestation has neither column to carry.
	NamesEvidence bool `json:"names_evidence"`
	// Statement is the sentence the document states about itself.
	Statement string `json:"statement"`
}

// Statement is the fixed sentence every public attestation carries about
// itself. It is a constant rather than a template: the record has no facts
// to interpolate but the ones already on the wire beside it, and a sentence
// assembled per row is a sentence that can say something the row does not.
const Statement = "An attestation states that an organization validated the " +
	"named version, and how that validation went. It is not evidence: this " +
	"record names no evidence version, carries no reasoning, and discloses " +
	"nothing about the underlying work it rests on."

// InstantLayout is the shape of every instant this surface puts on the wire:
// millisecond precision, UTC — the same layout knowledgepublish.InstantLayout
// fixes, because two spellings of one moment in one API is a client bug
// waiting to be written.
const InstantLayout = "2006-01-02T15:04:05.000Z"

// FormatInstant renders an instant for the wire (see InstantLayout).
func FormatInstant(t time.Time) string { return t.UTC().Format(InstantLayout) }
