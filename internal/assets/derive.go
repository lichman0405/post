package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// Research asset fork/derive (T0708): the governed write that creates a NEW
// asset identity from one existing published version and records the
// lineage edge between the two.
//
// docs/11 §5 is the whole specification of what this is — "Fork/Derive：
// 创建新的 Asset/Object identity，保留 lineage" — and it names both halves:
// a NEW identity (a new research_assets row with its own pid, and its first
// immutable version row) and the LINEAGE the derivation keeps (one
// asset_lineage edge, migration 00010). docs/31's Gate C lists the same
// pair among the asset work that has to exist before the gate can close:
// "Asset PID/version/lineage/rights/reference/dependency/fork".
//
// # Where this file lives, and why
//
// A use case over ports belongs in internal/application/* (docs/52), next to
// assetpublish — and that is where it would be if this task's allowed_scope
// had it. It does not: T0708 may write internal/assets/**, internal/
// persistence/**, cmd/api/** and tests/**, and internal/application/** is
// outside all four. The command therefore lives beside the domain rules it
// decides with (Gate, Preview, the lineage model of page.go), and its ports
// are declared HERE rather than in an application package. The production
// adapters are unchanged by that: the same *persistence.ProjectStore
// satisfies MembershipPort (one implementation of "is this caller a
// member", assetpublish's own note), and *persistence.AssetDeriveStore
// satisfies DeriveStorePort.
//
// The one visible consequence is that this package now imports
// internal/application/projects and internal/authz, a direction the layering
// usually runs the other way. It is not a cycle (projects imports orgs,
// authz, domain and observability; authz imports domain) and it has
// precedent in this tree for the same reason: internal/contribution,
// internal/events and internal/search all resolve a membership through an
// application service. The alternative was a second definition of the
// membership sentinels, which is the sort of copy that drifts.

// ActionAssetDerived is the audit action a successful derivation appends
// (audit_log.action). It follows the dotted `<subject>.<verb-past>`
// convention of internal/domain's Action* vocabulary, exactly as
// assetpublish.ActionAssetVersionPublished does, and it is declared here
// for the same scope reason (internal/domain is not writable by this task).
//
// It is a DIFFERENT action from the publish's although a derivation writes a
// published version too, and that is deliberate: the audit vocabulary
// records what was done, and "an asset version was published" and "an asset
// identity was forked from another version" are two different acts with two
// different authorizations to audit. An Activity reader that folded them
// would lose the lineage half of the record.
const ActionAssetDerived = "asset.derived"

// DeriveRelation is the relation type one lineage edge records: how the
// child version stands to the parent it was created from.
//
// The request chooses between the two, and the choice is a statement about
// what the caller made (docs/11 §5 keeps the two apart for exactly that
// reason):
//
//   - forked_from — a fork: a new research line continuing from the parent's
//     content, tracked by the network as a lineage relation.
//   - derived_from — a derivation: the child's content is built out of the
//     parent's (a subset, a transformed form, a re-published view).
//
// SUPERSEDES IS NOT ONE OF THEM. asset_lineage's CHECK (migration 00010)
// admits three values and 'supersedes' is the third: a version replacing
// another in the SAME line. That is not a derivation — it does not create a
// new identity, it retires an old one — and docs/11 §7 gives supersession
// its own lifecycle action. This command neither accepts nor writes it, and
// the ledger table's CHECK (00128) spells the same two values so a row no
// derivation could have written cannot be created by one.
type DeriveRelation string

// The two relations a derivation may record.
const (
	// RelationForkedFrom: the child is a fork of the parent.
	RelationForkedFrom DeriveRelation = "forked_from"
	// RelationDerivedFrom: the child was derived from the parent.
	RelationDerivedFrom DeriveRelation = "derived_from"
)

// AllDeriveRelations is the closed set, in the order the page's own
// vocabulary lists it, for callers that enumerate what the platform reads
// (the way AllTypes and AllDependencyTypes do).
func AllDeriveRelations() []DeriveRelation {
	return []DeriveRelation{RelationForkedFrom, RelationDerivedFrom}
}

// Valid reports whether r is one of the two relations a derivation records.
func (r DeriveRelation) Valid() bool {
	switch r {
	case RelationForkedFrom, RelationDerivedFrom:
		return true
	}
	return false
}

// ParseDeriveRelation reads a relation from its text form. ok is true
// exactly when the string is one of the two relations above — an explicitly
// refused alternative is 'supersedes', which is a legal row in
// asset_lineage and not a derivation.
func ParseDeriveRelation(s string) (DeriveRelation, bool) {
	r := DeriveRelation(strings.TrimSpace(s))
	return r, r.Valid()
}

// ParentVersionRef names the one published version a derivation is FROM, in
// the canonical pid@version form a dependency pin uses (DependencyPin).
//
// The parent is named by its ASSET's pid and its own version label, never by
// an asset id, a slug or a bare label: "forked from WHAT" has to survive the
// parent accumulating versions and its display metadata changing, and only
// the (pid, version) pair resolves to exactly one immutable row forever
// (docs/11 §2, CLAUDE.md invariant 5). It is spelled as a pin rather than as
// two fields because it is the same kind of statement a dependency pin is —
// one exact version of one asset — and the platform already has one text
// form for that ("@" is in neither alphabet, so the split is unambiguous).
type ParentVersionRef string

// NewParentVersionRef builds the canonical reference to one version of one
// asset. It reports false when either half is outside its storable shape: a
// reference that cannot name a stored version is refused at construction
// rather than looked up and reported as absent.
func NewParentVersionRef(pid PID, version string) (ParentVersionRef, bool) {
	pin, ok := NewDependencyPin(pid, version)
	return ParentVersionRef(pin), ok
}

// ParseParentVersionRef splits a canonical reference into the parent asset's
// pid and its version label. It is the inverse of NewParentVersionRef.
func ParseParentVersionRef(s string) (PID, string, bool) {
	return ParseDependencyPin(s)
}

// Valid reports whether r has the canonical pid@version shape.
func (r ParentVersionRef) Valid() bool {
	_, _, ok := ParseParentVersionRef(string(r))
	return ok
}

// DerivativesConfirmationField is the name of the request field through
// which a caller states, explicitly, that it is deriving from a parent whose
// rights declaration does not say whether derivation is permitted. It is
// also the key the confirmation is recorded under in the audit row's
// after_summary, so what was requested and what was recorded are one name.
const DerivativesConfirmationField = "derivatives_confirmation"

// MaxConfirmationLen bounds the confirmation sentence, in characters (the
// unit a person counts — the same rule internal/rights.MaxNotesLen states).
// The column is an unbounded jsonb; an audit row is not the place to store
// an essay, and a bounded refusal is one a client can act on.
const MaxConfirmationLen = 512

// DerivativesVerdict is what RequireDerivable read out of a parent's stored
// rights document: the declaration's own answer, and whether the caller's
// confirmation is what allowed the derivation.
type DerivativesVerdict struct {
	// Permission is the parent's declared usage.derivatives, one of the
	// three values of internal/rights (never empty: an empty or unknown
	// value is an unreadable declaration, which is a refusal).
	Permission rights.Permission
	// Confirmation is the confirmation the caller supplied, trimmed ("" when
	// none was supplied or it was blank).
	Confirmation string
	// ConfirmationRequired reports whether the parent's declaration left
	// derivation UNSPECIFIED — i.e. whether the confirmation above is what
	// made this derivation permitted. It is false for an `allowed`
	// declaration, where a supplied confirmation is recorded but decided
	// nothing.
	ConfirmationRequired bool
}

// RequireDerivable decides whether a derivation from a parent is permitted
// by the parent's stored rights declaration, and reports what it decided on.
//
// The decision is over the declaration's usage.derivatives, which has three
// values, and NONE of them is collapsed into another:
//
//   - restricted — REFUSED. The parent's publisher said derivatives are not
//     permitted, and no confirmation can un-say an explicit prohibition: a
//     confirmation is a caller's statement that it accepts a declaration
//     which says NOTHING, not a licence to override one that says no.
//   - allowed — PERMITTED. The declaration states the permission; nothing
//     further is required of the caller, and a confirmation supplied anyway
//     is recorded without deciding anything (a client cannot know the
//     parent's declaration before asking, so refusing the field would make
//     the write path a game of guessing the policy).
//   - unspecified — NEITHER. This is the real answer for the documents the
//     platform starts from (rights.New() declares every usage unspecified),
//     and it is not permission and not prohibition. It is permitted only
//     when the caller supplies an explicit confirmation, which is written
//     into the audit row BESIDE the actor's identity by the same
//     transaction that writes the edge — so the record says "this person
//     derived from a parent whose declaration did not say, and said so",
//     rather than "the platform decided silence meant yes".
//
// An UNREADABLE declaration is a refusal, and it is refused before any of
// the three is considered: missing bytes, bytes that are not one rights
// document, an unknown field (rights.Parse refuses those), or a
// usage.derivatives value outside the three. An unreadable policy is not a
// permissive one — the same rule the publish command applies to a policy it
// cannot read (assetpublish.Command.requirePolicy).
//
// The returned verdict is meaningful only when err is nil. The error is a
// *RightsRefusal carrying the wire code of the case that fired.
func RequireDerivable(rightsJSON []byte, confirmation string) (DerivativesVerdict, error) {
	doc, err := rights.Parse(rightsJSON)
	if err != nil {
		return DerivativesVerdict{}, &RightsRefusal{
			WireCode: CodeDeriveParentRightsUnreadable,
			Reason:   "the parent version's rights declaration cannot be read, and an unreadable policy is not a permissive one: " + err.Error(),
			Err:      err,
		}
	}
	verdict := DerivativesVerdict{
		Permission:   doc.Usage.Derivatives,
		Confirmation: strings.TrimSpace(confirmation),
	}
	switch doc.Usage.Derivatives {
	case rights.PermissionAllowed:
		return verdict, nil
	case rights.PermissionRestricted:
		return DerivativesVerdict{}, &RightsRefusal{
			WireCode: CodeDeriveDerivativesRestricted,
			Reason: "the parent version's rights declaration states usage.derivatives = \"restricted\": " +
				"no derivation from this version is permitted, and a confirmation cannot overrule an explicit prohibition",
		}
	case rights.PermissionUnspecified:
		if verdict.Confirmation == "" {
			return DerivativesVerdict{}, &RightsRefusal{
				WireCode: CodeDeriveDerivativesUnspecified,
				Reason: "the parent version's rights declaration states usage.derivatives = \"unspecified\", " +
					"which is neither permission nor prohibition: this derivation requires an explicit " +
					DerivativesConfirmationField + " in the request, recorded with the actor in the audit row",
			}
		}
		if len([]rune(verdict.Confirmation)) > MaxConfirmationLen {
			return DerivativesVerdict{}, &RightsRefusal{
				WireCode: CodeDeriveValidationFailed,
				Reason: fmt.Sprintf("%s must be 1..%d characters, got %d",
					DerivativesConfirmationField, MaxConfirmationLen, len([]rune(verdict.Confirmation))),
			}
		}
		verdict.ConfirmationRequired = true
		return verdict, nil
	default:
		// Unreachable through rights.Parse, which validates the value
		// against the three above. Kept, and kept fail-closed, because the
		// reachability of this branch is a claim about another package's
		// rules staying as they are — and if a fourth value ever appears,
		// the safe reading of an unknown declaration is "not readable",
		// not "permitted".
		return DerivativesVerdict{}, &RightsRefusal{
			WireCode: CodeDeriveParentRightsUnreadable,
			Reason:   "the parent version's rights declaration carries usage.derivatives = " + quote(string(doc.Usage.Derivatives)) + ", which is not one of the three values this platform reads",
		}
	}
}

// ParentReadable reports whether a caller may derive from a version, by the
// same ruler the asset page reads a version with.
//
// The page admits a reader to an asset when its originating project is
// readable — public, or the reader is one of its members (projects.Service.Get
// — and admits them to one VERSION when that version is public or they are a
// member (memberMaySee). This function is those two rules, read for a
// version that is not the page's own subject, and it calls them rather than
// restating them: a copy of the ruler is a second answer to "may this caller
// read this version", and the whole point of the rule is that it has one.
//
// The project id in the constructed state is not decoration: mayRenderOwnProject
// refuses to render a project it cannot name, and an unnamed project is
// exactly the "the caller may not see this project" case the derivation has
// to refuse on. The caller passes the parent's own project id, which is
// never empty.
//
// "Readable ⇒ derivable" is the whole of it, and its contrapositive is the
// security property: an unreadable parent is refused even when its pid and
// version label were guessed, because the derivation path is not a way to
// learn which versions exist (ADR-024).
func ParentReadable(projectID string, projectVisibility, versionVisibility Visibility, member bool) bool {
	viewer := PageViewer{Member: member}
	admittedToProject := mayRenderOwnProject(PageProjectState{ID: projectID, Visibility: projectVisibility}, viewer)
	admittedToVersion := memberMaySee(PageVersionState{Visibility: versionVisibility}, viewer)
	return admittedToProject && admittedToVersion
}

// DeriveActor is the deriving principal: the authenticated user, and whether
// the request arrived as a platform agent rather than a human session.
//
// The shape is assetpublish.Actor's, for the same reason: the flag is a fact
// of the transport's authentication, never a field of a request body, and
// the only thing it does is refuse.
type DeriveActor struct {
	// User is the authenticated user.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent.
	IsAgent bool
}

// DeriveParams is one derive request: where the new identity goes, which
// version it comes from, and the version document to store.
type DeriveParams struct {
	// ProjectID is the project the NEW asset belongs to (the child's origin
	// project, research_assets.origin_project_id). It is not the parent's.
	ProjectID string
	// Parent is the version the child is created from, as a canonical
	// pid@version reference.
	Parent string
	// Relation is forked_from or derived_from.
	Relation string
	// AssetPID must be EMPTY: a derivation always creates a new identity, so
	// a request that names an existing asset is refused rather than
	// answered with an identity the caller did not ask for. The field exists
	// so that naming one is a refusal instead of a silent ignore — the same
	// rule the publish applies to a title on an existing asset.
	AssetPID string
	// AssetType is one of the four V1 types, for the new asset.
	AssetType string
	// Version is the new asset's first version label.
	Version string
	// Manifest is the version's manifest document, as it would be stored.
	Manifest json.RawMessage
	// Rights is the version's own rights document, as it would be stored.
	Rights json.RawMessage
	// OriginRefs are the child version's provenance pins.
	OriginRefs []string
	// Visibility is the child version's visibility.
	Visibility string
	// IntegrityHash is the digest of the canonical manifest.
	IntegrityHash string
	// CreatorIDs are the users the child version credits.
	CreatorIDs []string
	// Title and Slug are the display fields of the new asset. Both are
	// required: the derivation always creates one.
	Title string
	Slug  string
	// Confirmation is the caller's explicit statement that it derives from a
	// parent whose declaration leaves derivation unspecified. It is read
	// only when the parent's declaration says "unspecified" (see
	// RequireDerivable) and is recorded in the audit row either way.
	Confirmation string
	// IdempotencyKey replays the derivation it names.
	IdempotencyKey *string
}

// DerivedAsset is one stored derivation: the child version row with its new
// asset's identity, and the edge that records where it came from.
type DerivedAsset struct {
	// ID and AssetID are the child version's and its asset's internal row
	// ids. They are not on the wire; they are what the ledger and the audit
	// row are keyed by.
	ID      string
	AssetID string
	// AssetPID is the NEW asset's persistent identifier.
	AssetPID string
	// Version is the child version's label.
	Version string
	// Visibility, IntegrityHash, OriginRefs, PublishedBy, PublishedAt,
	// Manifest and RightsJSON are the stored version row, as published.
	Visibility    Visibility
	IntegrityHash string
	OriginRefs    []string
	PublishedBy   string
	PublishedAt   time.Time
	Manifest      json.RawMessage
	RightsJSON    json.RawMessage
	// ParentAssetPID and ParentVersion are the edge's parent end, and
	// Relation is what the edge records. All three are what a replay
	// answers with, and what the parent side of the page reads.
	ParentAssetPID string
	ParentVersion  string
	Relation       DeriveRelation
}

// MembershipPort resolves the actor's membership in a project. It is
// assetpublish.MembershipPort's contract, in the same words: the production
// implementation is *persistence.ProjectStore, and a project that does not
// exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — an unknown project is an unknown membership,
// not a disclosed absence.
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// PolicyPort resolves the policy in force for one project: the
// organization's lower bound overlaid by the project's own (docs/12 §5).
// It is assetpublish.PolicyPort's contract in the same words, and the
// production implementation is the same *policy.Service — a derivation
// stores a version document through the same gate a publication does, so
// the governance it answers to is the same governance.
//
// It runs the project read gate as part of the resolution, which is why the
// derivation calls it only AFTER its own authorization has passed.
type PolicyPort interface {
	EffectivePolicy(ctx context.Context, actor domain.User, projectID string) (domain.EffectivePolicy, error)
}

// RuleEvaluator answers one typed governance question about a policy
// document. The production implementation is policy.RuleEvaluator.
type RuleEvaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q policy.Query) (policy.Decision, error)
}

// DeriveStorePort is the derive store: the ledger read the command checks
// before doing any work, and the derivation transaction itself.
//
// The split is the publish's StorePort split for the same reason — a replay
// is a READ, and it must be answerable without resolving a state or running
// a gate.
type DeriveStorePort interface {
	// LookupDerivation returns the derivation an Idempotency-Key already
	// created, or nil when the key has no ledger entry yet.
	LookupDerivation(ctx context.Context, projectID, idempotencyKey string) (*DerivedAsset, error)
	// Derive runs the derivation in ONE transaction: re-check the ledger
	// under the project row lock, resolve the parent version, apply the read
	// gate and the rights verdict, re-run the impact preview and the gate
	// over the current state, and — only if none of them refuses — write the
	// new asset row, the immutable version row, its credits, its declared
	// usages, the lineage edge, the ledger entry, the audit row and the
	// research event.
	//
	// A refusal writes NOTHING, the audit row included: the transaction has
	// not written anything at the point each refusal fires.
	Derive(ctx context.Context, req DeriveRequest) (DerivedAsset, error)
}

// DeriveRequest is everything one derivation needs to execute.
type DeriveRequest struct {
	// ProjectID is the project the new asset belongs to, in text uuid form.
	ProjectID string
	// Parent is the version the child is created from.
	Parent ParentVersionRef
	// Relation is the edge's relation type.
	Relation DeriveRelation
	// Candidate is the child version to store. Its AssetPID is final: the
	// command minted it, and it is the pid the new asset row is created
	// with.
	Candidate PublishCandidate
	// Slug and Title are the new asset's display fields.
	Slug  string
	Title string
	// Actor is the deriving principal.
	Actor DeriveActor
	// Confirmation is the caller's explicit statement (see DeriveParams). It
	// is read by the rights verdict inside the transaction.
	Confirmation string
	// Audit is the row the store appends inside the transaction. TargetRef
	// is left empty by the command and filled by the store with
	// "asset_version:<child id>", and the store adds the three
	// derivatives_* keys the verdict produced — it is the component that
	// read the parent's declaration, so it is the component that can record
	// what it read.
	Audit domain.AuditEntry
	// Usages are the asset_dependencies rows this derivation declares, in the
	// same shape the publish declares them.
	Usages []UsageDeclaration
	// IdempotencyKey is the caller's key, or nil when none was sent.
	IdempotencyKey *string
}

// DeriveCommand is the research asset fork/derive use case.
//
// # The order of the steps, and why it is the order
//
//  1. shape      — validate the request, before anything is read
//  2. agent      — the domain backstop, before anything is read
//  3. authorize  — membership class of the TARGET project vs the matrix
//  4. replay     — an Idempotency-Key that already derived, as a READ
//  5. policy     — the governance policy in force for the target project
//  6. derive     — the store's transaction: re-check the ledger, resolve the
//     parent, apply the read gate and the rights verdict, re-run the preview
//     and the gate, write, audit, emit
//
// Steps 2 and 3 come before every lookup on purpose, exactly as the publish
// puts them: a refusal there must not disclose whether the project exists
// (the releases.ErrForbidden rule). Step 5 needs the project row, which is
// why it is after step 3 rather than before it.
//
// Step 5 is the publish's step 5 for the publish's reason: the store
// commits a new PUBLIC asset version, so a public derivation is a
// publication for every governance purpose the platform has, and the rule
// that governs a public asset version governs it too. Without it the same
// document would be refused through `:publish` and permitted through
// `:derive` — a route around the review rather than a decision about it.
//
// # The parent's two decisions are made INSIDE the transaction
//
// "May this caller read the parent version" (the page's ruler) and "does the
// parent's declaration permit a derivation" (the three-valued verdict) are
// decided where they commit, not in the command. They are facts about rows
// the transaction is about to write after: the parent's declaration is
// immutable (research_asset_versions is append-only) but the parent's
// project's visibility is NOT (projects.visibility is a column a project
// changes), so a decision made outside the transaction and trusted inside it
// would be a decision about a state that can change between the two. There
// is one definition of each, both are exported pure functions
// (RequireDerivable, ParentReadable), and the store runs them.
type DeriveCommand struct {
	members  MembershipPort
	policies PolicyPort
	rules    RuleEvaluator
	store    DeriveStorePort
	authz    authz.Engine
	newPID   func() (PID, error)
}

// DeriveDeps carries the adapters a DeriveCommand is wired over.
type DeriveDeps struct {
	// Members resolves the actor's project membership (the production value
	// is *persistence.ProjectStore).
	Members MembershipPort
	// Policies resolves the governance policy in force for the target
	// project (the production value is *policy.Service, the same adapter the
	// publish command is wired over). See DeriveCommand.requirePolicy for
	// why a derivation reads it at all.
	Policies PolicyPort
	// Rules evaluates one rule of a policy document (the production value is
	// *policy.RuleEvaluator, again the publish's own).
	Rules RuleEvaluator
	// Store is the derive store (the production value is
	// *persistence.AssetDeriveStore).
	Store DeriveStorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
	// NewPID mints the new asset's persistent identifier. Tests inject a
	// deterministic sequence; production leaves it nil and gets NewPID.
	NewPID func() (PID, error)
}

// NewDeriveCommand wires the derive command.
func NewDeriveCommand(deps DeriveDeps) *DeriveCommand {
	c := &DeriveCommand{
		members:  deps.Members,
		policies: deps.Policies,
		rules:    deps.Rules,
		store:    deps.Store,
		authz:    deps.Authz,
		newPID:   deps.NewPID,
	}
	if c.newPID == nil {
		c.newPID = func() (PID, error) { return NewPID() }
	}
	return c
}

// Derive runs the derivation and returns the stored child version with the
// edge it recorded.
func (c *DeriveCommand) Derive(ctx context.Context, actor DeriveActor, in DeriveParams) (DerivedAsset, error) {
	if err := c.requireAgent(actor); err != nil {
		return DerivedAsset{}, err
	}
	relation, parent, candidate, slug, title, err := c.prepare(in)
	if err != nil {
		return DerivedAsset{}, err
	}
	if err := c.requireDerive(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return DerivedAsset{}, err
	}
	if in.IdempotencyKey != nil {
		replayed, err := c.store.LookupDerivation(ctx, in.ProjectID, *in.IdempotencyKey)
		if err != nil {
			return DerivedAsset{}, mapDeriveError(err)
		}
		if replayed != nil {
			if err := replayedMatches(*replayed, parent, relation, candidate); err != nil {
				return DerivedAsset{}, err
			}
			return *replayed, nil
		}
	}
	if err := c.requirePolicy(ctx, actor.User, in.ProjectID, candidate.Visibility); err != nil {
		return DerivedAsset{}, err
	}
	stored, err := c.store.Derive(ctx, DeriveRequest{
		ProjectID:      in.ProjectID,
		Parent:         parent,
		Relation:       relation,
		Candidate:      candidate,
		Slug:           slug,
		Title:          title,
		Actor:          actor,
		Confirmation:   strings.TrimSpace(in.Confirmation),
		Audit:          deriveAuditEntry(actor.User, in.ProjectID, parent, relation, candidate),
		Usages:         deriveDeclaredUsages(candidate),
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return DerivedAsset{}, mapDeriveError(err)
	}
	return stored, nil
}

// requireAgent is the domain backstop: an agent never derives, whatever the
// matrix says.
//
// A derivation creates a published asset version — the same class of act the
// publish is — so docs/23 §4's rule ("Agent token 默认无 visibility:publish
// scope") applies to it as it does there, and the refusal is resolved from
// the actor value alone, before any read. It is unreachable in V1 (no agent
// token exists), and it is the second line beside the matrix, which denies
// the agent column of publish_private_to_public independently.
func (c *DeriveCommand) requireAgent(actor DeriveActor) error {
	if actor.IsAgent {
		return &DeriveAgentNotPermittedError{Action: "derive"}
	}
	return nil
}

// requirePolicy evaluates the governance policy in force for the derivation.
//
// A derivation WRITES A NEW PUBLIC ASSET VERSION — that is what the store
// commits — so a public derivation is a publication for every governance
// purpose the platform has, and the rule that governs a public asset version
// governs it too. The rule is domain.RulePublicAssetIPReview, and this
// method is assetpublish.Command.requirePolicy applied to the same question
// in the same words, because a second answer to it would be a second policy:
// with this step missing, the same version document and the same target
// project would be refused through `:publish` and permitted through
// `:derive`, which is a route around the review rather than a decision about
// it.
//
// Four answers, three of them refusals, exactly as there:
//
//   - a policy that cannot be READ: refusal. An unreadable policy is not a
//     permissive one.
//   - a policy that cannot be EVALUATED: refusal, for the same reason.
//   - the rule SET TO TRUE: refusal. This build records no IP review, so a
//     policy that requires one is a requirement the platform cannot satisfy.
//   - the rule set to FALSE, or ABSENT: permitted (docs/23 §4 lists the org
//     policy approvals as optional, and the rule's own text says true is the
//     stricter value).
//
// It reads the rule only for a PUBLIC child version, because that is the
// only case the rule is about — a private derivation widens nothing, and the
// publish reads it the same way.
//
// It runs AFTER the authorization above and not before it, for the
// publish's reason: EffectivePolicy runs the project read gate, so asking
// it about a project the caller may not derive into would make this route
// an existence oracle.
func (c *DeriveCommand) requirePolicy(ctx context.Context, actor domain.User, projectID string, visibility Visibility) error {
	if visibility != VisibilityPublic {
		// A private derivation widens nothing; the rule is about public
		// assets and is not read.
		return nil
	}
	if c.policies == nil || c.rules == nil {
		return fmt.Errorf("%w: derive command not fully wired", ErrDeriveStore)
	}
	rule := domain.RulePublicAssetIPReview
	refuse := func(found, value bool, reason string, cause error) error {
		return &DerivePolicyRefusedError{Rule: rule, Found: found, Bool: value, Reason: reason, Err: cause}
	}
	effective, err := c.policies.EffectivePolicy(ctx, actor, projectID)
	if err != nil {
		return refuse(false, false, "the policy in force could not be read, and an unreadable policy is not a permissive one", err)
	}
	decision, err := c.rules.Evaluate(ctx, effective.Effective, policy.Query{Rule: rule})
	if err != nil {
		return refuse(false, false, "the policy in force cannot be evaluated, and an unevaluable policy is not a permissive one", err)
	}
	if !decision.Found || !decision.Bool {
		return nil
	}
	return refuse(true, true,
		"the policy in force sets "+rule+" to true — a public asset version requires an IP review before it is published, and this build records no IP review to satisfy it", nil)
}

// prepare validates the request shape and renders the candidate the gate and
// the preview both take.
//
// The pid is minted HERE, and only here: a derivation always creates a new
// asset, so there is no branch in which the caller names one. The minted pid
// is the pid the create will store, and it is not compared on a replay (a
// replay never compares an identity the caller did not send —
// assetpublish.replayedMatches' rule).
func (c *DeriveCommand) prepare(in DeriveParams) (DeriveRelation, ParentVersionRef, PublishCandidate, string, string, error) {
	fail := func(err error) (DeriveRelation, ParentVersionRef, PublishCandidate, string, string, error) {
		return "", "", PublishCandidate{}, "", "", err
	}
	if strings.TrimSpace(in.ProjectID) == "" {
		return fail(fmt.Errorf("%w: project_id is required", ErrDeriveValidation))
	}
	relation, ok := ParseDeriveRelation(in.Relation)
	if !ok {
		return fail(fmt.Errorf("%w: relation must be one of %s; %q is not a derivation (a supersession does not create an identity and is not this command's)",
			ErrDeriveValidation, relationList(), in.Relation))
	}
	parent := ParentVersionRef(strings.TrimSpace(in.Parent))
	if !parent.Valid() {
		return fail(fmt.Errorf("%w: parent must be the canonical pid@version of one published version (pid = 26 Crockford base32 characters, version = 1..%d of [A-Za-z0-9._-]), got %s",
			ErrDeriveValidation, MaxVersionLen, quote(in.Parent)))
	}
	if named := strings.TrimSpace(in.AssetPID); named != "" {
		return fail(fmt.Errorf("%w: asset_id %s is not accepted: a derivation creates a NEW asset identity (docs/11 §5) and never adds a version to an existing asset",
			ErrDeriveValidation, quote(named)))
	}
	assetType, ok := ParseType(in.AssetType)
	if !ok {
		return fail(fmt.Errorf("%w: asset_type must be one of the four V1 types, got %q", ErrDeriveValidation, in.AssetType))
	}
	version := strings.TrimSpace(in.Version)
	if !ValidVersionLabel(version) {
		return fail(fmt.Errorf("%w: version %q must be 1..%d characters of [A-Za-z0-9._-]", ErrDeriveValidation, in.Version, MaxVersionLen))
	}
	visibility := Visibility(strings.TrimSpace(in.Visibility))
	if !visibility.Valid() {
		return fail(fmt.Errorf("%w: visibility must be public or private, got %q", ErrDeriveValidation, in.Visibility))
	}
	slug := strings.TrimSpace(in.Slug)
	title := strings.TrimSpace(in.Title)
	switch {
	case title == "":
		return fail(fmt.Errorf("%w: title is required: a derivation creates an asset", ErrDeriveValidation))
	case slug == "":
		return fail(fmt.Errorf("%w: slug is required: a derivation creates an asset", ErrDeriveValidation))
	case len(slug) > MaxSlugLen:
		return fail(fmt.Errorf("%w: slug must be 1..%d characters", ErrDeriveValidation, MaxSlugLen))
	case len(title) > MaxTitleLen:
		return fail(fmt.Errorf("%w: title must be 1..%d characters", ErrDeriveValidation, MaxTitleLen))
	}
	// A confirmation longer than the bound is refused before the rights
	// verdict rather than inside it, so the bound holds whatever the parent's
	// declaration turns out to say (RequireDerivable checks it again for the
	// one case where it is the deciding rule).
	if len([]rune(strings.TrimSpace(in.Confirmation))) > MaxConfirmationLen {
		return fail(fmt.Errorf("%w: %s must be 1..%d characters", ErrDeriveValidation, DerivativesConfirmationField, MaxConfirmationLen))
	}
	pid, err := c.newPID()
	if err != nil {
		return fail(fmt.Errorf("%w: mint a pid: %v", ErrDeriveStore, err))
	}
	if !ValidPID(string(pid)) {
		return fail(fmt.Errorf("%w: the pid generator produced %q, which is not a pid", ErrDeriveStore, pid))
	}

	// The refs are canonicalised exactly as the publish canonicalises them
	// (through NewOriginRef), so what the gate validates and what the row
	// stores are the same strings; a ref that is not canonical is left as the
	// caller sent it, because the gate is the component that refuses it and
	// it refuses it by name.
	refs := make([]string, 0, len(in.OriginRefs))
	for _, raw := range in.OriginRefs {
		kind, value, ok := ParseOriginRef(raw)
		if !ok {
			refs = append(refs, raw)
			continue
		}
		ref, _ := NewOriginRef(kind, value)
		refs = append(refs, string(ref))
	}

	candidate := PublishCandidate{
		AssetPID:      pid,
		AssetType:     assetType,
		Version:       version,
		Manifest:      in.Manifest,
		RightsJSON:    in.Rights,
		OriginRefs:    refs,
		Visibility:    visibility,
		IntegrityHash: strings.TrimSpace(in.IntegrityHash),
		CreatorIDs:    in.CreatorIDs,
	}
	return relation, parent, candidate, slug, title, nil
}

// The display-field bounds of a newly created asset. They are the same two
// numbers the publish refuses over (assetpublish.MaxSlugLen / MaxTitleLen)
// because they bound the same two columns of the same table, and a
// derivation that creates an asset is held to the bound a publish that
// creates one is held to. This package cannot import that one (it imports
// this one), so the numbers are restated; two bounds for one column would
// be two answers to "how long may a title be", so if one moves the other
// must move with it.
const (
	MaxSlugLen  = 128
	MaxTitleLen = 256
)

// relationList renders the admissible relations for a refusal line.
func relationList() string {
	names := make([]string, 0, len(AllDeriveRelations()))
	for _, r := range AllDeriveRelations() {
		names = append(names, string(r))
	}
	return strings.Join(names, ", ")
}

// requireDerive authorizes the derivation, and it is the SAME decision the
// publish makes: resolve the actor's membership role IN THE TARGET PROJECT
// (the project the new asset will belong to) and evaluate
// authz.ActionPublishPrivateToPublic for its class. No new action is
// introduced and the permission matrix is not edited: a derivation creates a
// published asset version, which is what that action governs.
//
// The action is evaluated for every derivation, not only for one that
// widens visibility, which is the publish's own literal reading of the same
// instruction ("发布必须判定 publish_private_to_public"). Its visible
// consequence is intended and fail-closed: with the matrix as it stands
// (maintainer = conditional, owner = allow) V1 derivations are an OWNER
// action, because a conditional cell whose condition has no specification
// resolves to a refusal (internal/authz admits only `allow`).
//
// The denial precedes every lookup: the membership resolution answers
// projects.ErrMemberNotFound for a project that does not exist exactly as it
// does for one the caller does not belong to, so a caller probing for
// projects with this route is answered "not permitted" either way. Only
// projects.ErrProjectNotFound — the project surface's deliberate disclosure
// for an invisible project — is relayed as a not-found.
func (c *DeriveCommand) requireDerive(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: derive command not fully wired", ErrDeriveStore)
	}
	membership, err := c.members.GetMembership(ctx, projectID, actorID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrDeriveProjectNotFound
	default:
		return mapDeriveError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize derive: %v", ErrDeriveStore, err)
	}
	if !decision.Permits() {
		return ErrDeriveForbidden
	}
	return nil
}

// replayedMatches decides whether an Idempotency-Key's stored derivation is
// the SAME derivation the caller is repeating (replay) or a different one
// (conflict, docs/45).
//
// Three things are compared, and they are the three the caller controls that
// the ledger stores: the parent version, the relation, and the child's
// version label. A key that forked from version A may not answer for a
// request that forks from version B, nor for one that records a different
// relation, nor for one that names a different child version — in each case
// the caller would receive a row it did not ask for and could not tell.
//
// The child's pid is deliberately NOT compared: it is minted per request
// (prepare), so comparing it would turn every repeat of a derivation into a
// conflict — exactly the case an Idempotency-Key exists for.
func replayedMatches(stored DerivedAsset, parent ParentVersionRef, relation DeriveRelation, candidate PublishCandidate) error {
	storedParent, ok := NewParentVersionRef(PID(stored.ParentAssetPID), stored.ParentVersion)
	if !ok || storedParent != parent {
		return fmt.Errorf("%w: this Idempotency-Key derived from %s, and this request derives from %s",
			ErrDeriveIdempotencyConflict, quote(string(storedParent)), quote(string(parent)))
	}
	if stored.Relation != relation {
		return fmt.Errorf("%w: this Idempotency-Key recorded relation %q, and this request asks for %q",
			ErrDeriveIdempotencyConflict, stored.Relation, relation)
	}
	if stored.Version != candidate.Version {
		return fmt.Errorf("%w: this Idempotency-Key created version %q, and this request is for version %q",
			ErrDeriveIdempotencyConflict, stored.Version, candidate.Version)
	}
	return nil
}

// deriveDeclaredUsages renders the asset_dependencies rows the derivation
// declares: one depends_on usage of the project for every exact version the
// child's manifest pins, at the child version's own visibility. It is
// PublishedUsages (usage.go, where the rule and its citations live) applied
// to the child's manifest, and it is the same declaration the publish makes
// for the same document.
//
// The manifest is read here, before the store's transaction, because the
// command has no gate result yet; the store re-parses the same bytes inside
// the transaction (it runs the gate), and the two readings are of one
// immutable byte string, so they cannot disagree. A manifest the parser
// refuses yields no declarations — the store's gate refuses the derivation
// outright, and a derivation that is not going to happen declares nothing.
func deriveDeclaredUsages(candidate PublishCandidate) []UsageDeclaration {
	manifest, err := ParseManifest(candidate.Manifest)
	if err != nil {
		return PublishedUsages(nil, candidate.Visibility)
	}
	return PublishedUsages(manifest.DependencyPins, candidate.Visibility)
}

// deriveAuditEntry renders the audit row the store appends inside the
// derivation transaction. TargetRef and the three derivatives_* keys are
// filled by the store: it is the component that has the child's row id and
// the one that read the parent's declaration.
func deriveAuditEntry(actor domain.User, projectID string, parent ParentVersionRef, relation DeriveRelation, candidate PublishCandidate) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    ActionAssetDerived,
		ProjectID: projectID,
		AfterSummary: map[string]any{
			"asset_id":       string(candidate.AssetPID),
			"version":        candidate.Version,
			"asset_type":     string(candidate.AssetType),
			"visibility":     string(candidate.Visibility),
			"integrity_hash": candidate.IntegrityHash,
			"origin_refs":    candidate.OriginRefs,
			"parent":         string(parent),
			"relation":       string(relation),
		},
	}
}

// mapDeriveError keeps the sentinels of this package and of its ports and
// turns everything else into ErrDeriveStore.
func mapDeriveError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrDeriveValidation),
		errors.Is(err, ErrDeriveAgentNotPermitted),
		errors.Is(err, ErrDeriveForbidden),
		errors.Is(err, ErrDeriveProjectNotFound),
		errors.Is(err, ErrDeriveParentNotFound),
		errors.Is(err, ErrDeriveRightsRefused),
		errors.Is(err, ErrDeriveAssetExists),
		errors.Is(err, ErrDeriveVersionImmutable),
		errors.Is(err, ErrDeriveIdempotencyConflict),
		errors.Is(err, ErrDeriveRefused),
		errors.Is(err, ErrDerivePolicyRefused),
		errors.Is(err, ErrDeriveStore):
		return err
	}
	return fmt.Errorf("%w: %v", ErrDeriveStore, err)
}

// Sentinel errors of the derive surface (docs/45: stable outcomes, never
// dependency detail). Each is a distinct OUTCOME the transport answers
// differently; everything else is ErrDeriveStore.
var (
	// ErrDeriveValidation: the request is not a derivation at all — an empty
	// project, a relation outside the two, a parent that is not a canonical
	// pid@version, a named asset, an unknown type or visibility, a missing
	// title or slug, an over-long confirmation.
	ErrDeriveValidation = errors.New("assets: derive validation failed")
	// ErrDeriveAgentNotPermitted: an agent actor attempted the derivation
	// (see DeriveCommand.requireAgent).
	ErrDeriveAgentNotPermitted = errors.New("assets: agents may not derive research assets")
	// ErrDeriveForbidden: the actor's class does not permit
	// authz.ActionPublishPrivateToPublic over the target project. Resolved
	// before any target lookup, so the denial never discloses whether the
	// project exists.
	ErrDeriveForbidden = errors.New("assets: the actor may not derive research assets here")
	// ErrDeriveProjectNotFound: no project row exists for the given id, or
	// the project is not visible to the caller (existence hiding, docs/45).
	ErrDeriveProjectNotFound = errors.New("assets: project not found")
	// ErrDeriveParentNotFound: the parent names no version the caller may
	// read. It answers the SAME outcome for "there is no such version" and
	// "there is one you may not read", deliberately: the derivation path is
	// not a way to learn which versions exist (ADR-024).
	ErrDeriveParentNotFound = errors.New("assets: the parent version was not found")
	// ErrDeriveRightsRefused: the parent's stored rights declaration refuses
	// the derivation — restricted, unspecified without a confirmation, or
	// unreadable. See RequireDerivable and RightsRefusal.
	ErrDeriveRightsRefused = errors.New("assets: the parent version's rights declaration does not permit this derivation")
	// ErrDeriveAssetExists: the freshly minted pid already names an asset.
	ErrDeriveAssetExists = errors.New("assets: that pid already names an asset")
	// ErrDeriveVersionImmutable: the new asset already carries this version
	// label, which UNIQUE(asset_id, version) makes impossible for a fresh
	// identity — mapped rather than wrapped so a collision cannot be
	// mistaken for a successful create.
	ErrDeriveVersionImmutable = errors.New("assets: this asset version is already published")
	// ErrDeriveIdempotencyConflict: the Idempotency-Key was already used for
	// a DIFFERENT derivation.
	ErrDeriveIdempotencyConflict = errors.New("assets: this Idempotency-Key was used for a different derivation")
	// ErrDeriveRefused: the derivation's own server-side re-run of the impact
	// preview refused it — see DeriveRefused, which carries the report.
	ErrDeriveRefused = errors.New("assets: the derivation was refused")
	// ErrDerivePolicyRefused: the governance policy in force does not permit
	// this derivation — see DerivePolicyRefusedError. It is the publish's
	// own outcome for the same rule (assetpublish.ErrPolicyRefused), because
	// a public derivation IS a publication.
	ErrDerivePolicyRefused = errors.New("assets: the policy in force does not permit this derivation")
	// ErrDeriveStore: a persistence adapter failed, or the data it returned
	// cannot be rendered (cause kept for the log).
	ErrDeriveStore = errors.New("assets: derive store failure")
)

// Wire codes (docs/45): one code per failure shape. The vocabulary the error
// model fixes is used where it exists (VALIDATION_FAILED,
// PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL, IDEMPOTENCY_CONFLICT,
// ASSET_ALREADY_EXISTS, ASSET_VERSION_IMMUTABLE, SERVICE_UNAVAILABLE); the
// codes it does not list name outcomes this surface alone has.
const (
	// CodeDeriveValidationFailed: the request is not a derivation.
	CodeDeriveValidationFailed = "VALIDATION_FAILED"
	// CodeDeriveNotPermitted: the actor's class does not permit the
	// derivation. It is the SAME string the publish surface answers for the
	// same matrix cell (assetpublish.CodePublishPrivateToPublicRequiresApproval),
	// and deliberately so: a client branches on this code to find the
	// approval it needs, and the approval it needs is one.
	CodeDeriveNotPermitted = "PRIVATE_TO_PUBLIC_REQUIRES_APPROVAL"
	// CodeDeriveAgentDenied: an agent actor attempted the derivation. A code
	// of its own although the matrix denies agents too, for the publish's
	// reason: the two refusals come from different places, and a test that
	// cannot tell them apart cannot show that the backstop is an independent
	// second line.
	CodeDeriveAgentDenied = "ASSET_DERIVE_AGENT_DENIED"
	// CodeDeriveProjectNotFound: the target project does not exist or is not
	// visible to the caller.
	CodeDeriveProjectNotFound = "ASSET_DERIVE_PROJECT_NOT_FOUND"
	// CodeDeriveParentNotFound: the parent version does not exist, or exists
	// and is not one this caller may read. ONE code for both, because they
	// are one answer.
	CodeDeriveParentNotFound = "ASSET_DERIVE_PARENT_NOT_FOUND"
	// CodeDeriveParentRightsUnreadable: the parent's stored rights document
	// could not be read. An unreadable policy is not a permissive one.
	CodeDeriveParentRightsUnreadable = "ASSET_DERIVE_PARENT_RIGHTS_UNREADABLE"
	// CodeDeriveDerivativesRestricted: the parent's declaration forbids
	// derivatives.
	CodeDeriveDerivativesRestricted = "ASSET_DERIVE_DERIVATIVES_RESTRICTED"
	// CodeDeriveDerivativesUnspecified: the parent's declaration leaves
	// derivation unspecified and the request carried no confirmation.
	CodeDeriveDerivativesUnspecified = "ASSET_DERIVE_DERIVATIVES_UNSPECIFIED"
	// CodeDeriveAssetExists: the minted pid is taken.
	CodeDeriveAssetExists = "ASSET_ALREADY_EXISTS"
	// CodeDeriveVersionImmutable: the version label is taken (docs/45's own
	// code, shared with the publish for the same column constraint).
	CodeDeriveVersionImmutable = "ASSET_VERSION_IMMUTABLE"
	// CodeDeriveIdempotencyConflict: the Idempotency-Key was used elsewhere
	// (docs/45's own code).
	CodeDeriveIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	// CodeDerivePolicyRefused: the policy in force blocks the derivation
	// (docs/45's own code, shared with the publish for the same rule and
	// the same act).
	CodeDerivePolicyRefused = "RIGHTS_POLICY_BLOCKS_ACTION"
	// CodeDeriveBlocked: the derivation's server-side impact re-check
	// refused it; the response carries the COMPLETE report, as the publish's
	// does and for the same reason.
	CodeDeriveBlocked = "ASSET_DERIVE_BLOCKED"
	// CodeDeriveServiceUnavailable: derive data is temporarily unavailable.
	CodeDeriveServiceUnavailable = "SERVICE_UNAVAILABLE"
)

// DeriveAgentNotPermittedError reports a derivation refused because the
// actor is an agent (DeriveCommand.requireAgent).
type DeriveAgentNotPermittedError struct {
	// Action is the refused action's wire name ("derive").
	Action string
}

// Error implements error.
func (e *DeriveAgentNotPermittedError) Error() string {
	return fmt.Sprintf("assets: agents cannot %s a research asset — a derivation publishes a new asset version and is a human governance action (docs/23 §4, docs/12 §3)", e.Action)
}

// Unwrap keeps errors.Is(err, ErrDeriveAgentNotPermitted) working.
func (e *DeriveAgentNotPermittedError) Unwrap() error { return ErrDeriveAgentNotPermitted }

// DerivePolicyRefusedError reports that the governance policy in force
// blocks the derivation (DeriveCommand.requirePolicy). Its shape is
// assetpublish.PolicyRefusedError's, and the two are separate types only
// because they live in separate packages: they name the same rule, the same
// four answers, and the same wire code.
type DerivePolicyRefusedError struct {
	// Rule is the rule key the refusal is about
	// (domain.RulePublicAssetIPReview).
	Rule string
	// Found reports whether the policy set the rule at all.
	Found bool
	// Bool is the rule's value when Found.
	Bool bool
	// Reason is the human-readable line naming which case fired.
	Reason string
	// Err is the underlying failure when the policy could not be read.
	Err error
}

// Error implements error.
func (e *DerivePolicyRefusedError) Error() string {
	return "assets: the policy in force does not permit this derivation — " + e.Reason
}

// Unwrap keeps errors.Is(err, ErrDerivePolicyRefused) working, and carries
// the read failure when the refusal was a failed read.
func (e *DerivePolicyRefusedError) Unwrap() error {
	if e.Err != nil {
		return fmt.Errorf("%w: %w", ErrDerivePolicyRefused, e.Err)
	}
	return ErrDerivePolicyRefused
}

// Code is the stable wire code of this outcome (docs/45).
func (e *DerivePolicyRefusedError) Code() string { return CodeDerivePolicyRefused }

// Code is the stable wire code of this outcome (docs/45).
func (e *DeriveAgentNotPermittedError) Code() string { return CodeDeriveAgentDenied }

// RightsRefusal reports a derivation the parent's rights declaration refuses,
// and names which of the cases fired (see RequireDerivable).
//
// It is one type rather than three because the three cases differ only in
// their cause and their code: the outcome a client branches on is the code,
// and the sentence is for humans.
type RightsRefusal struct {
	// WireCode is one of CodeDeriveParentRightsUnreadable,
	// CodeDeriveDerivativesRestricted, CodeDeriveDerivativesUnspecified, or
	// CodeDeriveValidationFailed for an over-long confirmation.
	WireCode string
	// Reason is the human-readable line naming which case fired and why.
	Reason string
	// Err is the underlying failure when the document could not be read.
	Err error
}

// Error implements error.
func (e *RightsRefusal) Error() string { return "assets: " + e.Reason }

// Unwrap keeps errors.Is(err, ErrDeriveRightsRefused) working, and carries
// the parse failure when the refusal was an unreadable document.
func (e *RightsRefusal) Unwrap() error {
	if e.Err != nil {
		return fmt.Errorf("%w: %w", ErrDeriveRightsRefused, e.Err)
	}
	return ErrDeriveRightsRefused
}

// Code is the stable wire code of this outcome (docs/45). It is the field
// under the name the other refusal types in this package and in
// assetpublish spell it, so a transport maps every one of them the same way.
func (e *RightsRefusal) Code() string { return e.WireCode }

// DeriveRefused is the derivation refusal: the store re-ran the impact
// preview over the repository's CURRENT state, inside its own transaction,
// and something in that state must not ride along into the version it would
// write.
//
// It carries the whole preview for the reason assetpublish.PublishRefused
// does (docs/23 §4): the refusal is about the entries the derivation would
// have exposed, and a client told only "forbidden" cannot tell which one to
// fix.
type DeriveRefused struct {
	// Preview is the complete impact preview the refusal was decided on.
	Preview ImpactPreview
	// Reasons names, one line each, the entries that blocked.
	Reasons []string
}

// Error implements error.
func (e *DeriveRefused) Error() string {
	msg := ErrDeriveRefused.Error()
	if len(e.Reasons) > 0 {
		msg += ": " + strings.Join(e.Reasons, "; ")
	}
	return msg
}

// Unwrap keeps errors.Is(err, ErrDeriveRefused) working.
func (e *DeriveRefused) Unwrap() error { return ErrDeriveRefused }

// Code is the stable wire code of this outcome (docs/45).
func (e *DeriveRefused) Code() string { return CodeDeriveBlocked }
