package assetpublish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// ActionAssetVersionPublished is the audit action name a successful
// publish appends (the audit_log.action column). It follows the dotted
// `<subject>.<verb-past>` convention of internal/domain's Action*
// vocabulary — release.created, milestone.created, pull_request.merged.
//
// It is declared HERE rather than beside those, and that is a scope fact
// rather than a design choice: internal/domain is not writable by this
// task, so a new audit action had to live in the package that owns the
// surface. The name is spelled the same way and read the same way by the
// Activity page, which renders actions and never parses them.
//
// It deliberately does NOT reuse the research event's name
// (research_asset.version_published). The two vocabularies are separate
// registries — the audit vocabulary is internal/domain's, the event
// vocabulary is specs/events/event-types.yaml — and a shared spelling
// would suggest a shared identity that does not exist (the note
// domain.ActionPullRequestMerged carries).
const ActionAssetVersionPublished = "asset.version_published"

// Command is the research asset publish use case (T0705): the governance
// write that turns a proposed version into an immutable, published one.
//
// What it is, in the words of the specs it implements: docs/23 §4 calls
// private→public the platform's highest-risk operation and enumerates what
// it must carry — a human explicit action, an impact preview, rights
// validation, no hidden private dependency leak, and an audit event. This
// command is all five: the human is the authenticated actor whose class
// the permission matrix admits, the preview is re-run server-side inside
// this transaction, the rights document is validated by the publish gate
// (assets.Gate) before anything is resolved, the leak check is the
// preview's blocking private dependencies, and the audit row commits with
// the version.
//
// # The order of the steps, and why it is the order
//
//  1. shape      — validate the request, before anything is read
//  2. agent      — the domain backstop, before anything is read
//  3. authorize  — membership class vs the matrix, before anything is read
//  4. replay     — an Idempotency-Key that already published, as a READ
//  5. policy     — the governance policy in force for the project
//  6. publish    — the store's transaction: re-check the ledger, resolve
//     the state, re-run the preview, gate, write, audit, emit
//
// Steps 2 and 3 come before every lookup on purpose: a refusal there must
// not disclose whether the project or the asset exists (the
// releases.ErrForbidden rule — "resolved before any target lookup, so the
// denial never discloses whether the project exists"). Step 5 needs the
// project row, which is why it is after step 3 rather than before it.
//
// # Two lines of defence against an agent
//
// docs/23 §4: "Agent token 默认无 visibility:publish scope". An agent
// publishing is refused twice over, and both refusals are deliberate:
//
//   - the DOMAIN backstop (step 2, AgentNotPermittedError). It is
//     resolved from the actor value alone, before any read, and it holds
//     even if the matrix were ever configured to permit an agent.
//   - the MATRIX (step 3). The agent column of
//     publish_private_to_public is `deny`, so the same request is refused
//     again by the authorization the publish runs for every actor.
//
// Neither is redundant with the other and neither is a substitute for the
// other: the matrix is a policy document that a governance change may
// edit, and the backstop is the product rule that the edit cannot waive.
//
// # The matrix row is evaluated for EVERY publish, not only a widening one
//
// The action's name is `publish_private_to_public` and this command
// evaluates it for a private-target publish too. That is the literal
// reading of the task's own instruction ("发布必须判定
// publish_private_to_public") and of internal/application/contribution/
// doc.go, which requires the consuming API to map Publicize onto exactly
// this action. Its visible consequence is intended and fail-closed: with
// the matrix as it stands (maintainer = conditional, owner = allow) V1
// publishes are an OWNER action, because a conditional cell whose
// condition has no specification (issue #237) resolves to a refusal —
// internal/authz's Permits() admits only `allow`.
type Command struct {
	members  MembershipPort
	policies PolicyPort
	rules    RuleEvaluator
	store    StorePort
	authz    authz.Engine
	newPID   func() (assets.PID, error)
}

// Deps carries the adapters a Command is wired over.
type Deps struct {
	// Members resolves the actor's project membership (the production
	// value is *persistence.ProjectStore).
	Members MembershipPort
	// Policies resolves the policy in force (the production value is
	// *policy.Service).
	Policies PolicyPort
	// Rules evaluates one rule of a policy document (the production value
	// is *policy.RuleEvaluator).
	Rules RuleEvaluator
	// Store is the publish store (the production value is
	// *persistence.AssetPublishStore).
	Store StorePort
	// Authz is the permission-matrix engine (the production value is
	// authz.NewMatrixEngine()).
	Authz authz.Engine
	// NewPID mints a persistent identifier for a new asset. Tests inject
	// a deterministic sequence; production leaves it nil and gets
	// assets.NewPID.
	NewPID func() (assets.PID, error)
}

// NewCommand wires the publish command.
func NewCommand(deps Deps) *Command {
	c := &Command{
		members:  deps.Members,
		policies: deps.Policies,
		rules:    deps.Rules,
		store:    deps.Store,
		authz:    deps.Authz,
		newPID:   deps.NewPID,
	}
	if c.newPID == nil {
		c.newPID = func() (assets.PID, error) { return assets.NewPID() }
	}
	return c
}

// Actor is the publishing principal: the authenticated user, and whether
// the request arrived as a platform agent rather than a human session
// (docs/23 §4: an agent token carries no `visibility:publish` scope).
//
// The flag is a field of a value the TRANSPORT builds from the principal,
// never a field of the request body: a caller that could set it could
// clear it, and the only thing the flag does is refuse.
//
// It is a type of this package rather than a field on domain.User because
// a publish is the only place in this build that needs it — agent tokens
// do not exist yet (V1 authenticates sessions), so the flag's producers
// are non-HTTP callers and tests today, and its consumer is the backstop
// that will already be in place when an agent token does arrive. The shape
// follows internal/contribution.Actor, which carries the same flag for the
// same reason.
type Actor struct {
	// User is the authenticated user. An agent acts AS a user — the
	// token's owner — so this is never empty.
	User domain.User
	// IsAgent reports whether the request arrived as a platform agent
	// (MCP/API), not as a human session.
	IsAgent bool
}

// PublishParams is one publish request: the proposed version, plus the
// optional identity of the asset it continues.
type PublishParams struct {
	// ProjectID is the project the publish happens in (the asset's own
	// project too: an asset's versions are published in the asset's
	// project, docs/11 §2).
	ProjectID string
	// AssetPID is the persistent identifier of an EXISTING asset this
	// version is added to. Empty means "publish a new asset", and the
	// command mints the pid with assets.NewPID() — never the column
	// DEFAULT (migration 00064's note: the application-generated path
	// arrives with the publish command).
	AssetPID string
	// AssetType is one of the four V1 types.
	AssetType string
	// Version is the immutable version label.
	Version string
	// Manifest is the manifest document, as it would be stored.
	Manifest json.RawMessage
	// Rights is the rights document, as it would be stored.
	Rights json.RawMessage
	// OriginRefs are the provenance pins, in canonical kind:value form.
	OriginRefs []string
	// Visibility is the published version's visibility.
	Visibility string
	// IntegrityHash is the digest of the canonical manifest.
	IntegrityHash string
	// CreatorIDs are the credited users.
	CreatorIDs []string
	// Title and Slug are the DISPLAY fields of an asset this publish
	// creates. Both are required when AssetPID is empty and refused when
	// it is not: publishing a version is not an asset-metadata revision
	// (docs/11 §4 gives metadata its own, separately audited surface),
	// and silently ignoring a field the caller sent would be worse than
	// refusing it.
	Title string
	Slug  string
	// IdempotencyKey replays the publication it names (docs/22): the same
	// key returns the version the first call published, forever.
	IdempotencyKey *string
}

// Publish runs the publish and returns the stored version.
func (c *Command) Publish(ctx context.Context, actor Actor, in PublishParams) (PublishedVersion, error) {
	if err := c.requireAgent(actor); err != nil {
		return PublishedVersion{}, err
	}
	candidate, newAsset, slug, title, err := c.prepare(in)
	if err != nil {
		return PublishedVersion{}, err
	}
	if err := c.requirePublish(ctx, actor.User.ID, in.ProjectID, actor.IsAgent); err != nil {
		return PublishedVersion{}, err
	}
	if in.IdempotencyKey != nil {
		replayed, err := c.store.LookupCreation(ctx, in.ProjectID, *in.IdempotencyKey)
		if err != nil {
			return PublishedVersion{}, mapPublishError(err)
		}
		if replayed != nil {
			if err := replayedMatches(*replayed, candidate, newAsset); err != nil {
				return PublishedVersion{}, err
			}
			return *replayed, nil
		}
	}
	if err := c.requirePolicy(ctx, actor.User, in.ProjectID, candidate.Visibility); err != nil {
		return PublishedVersion{}, err
	}
	stored, err := c.store.Publish(ctx, PublishRequest{
		ProjectID:      in.ProjectID,
		Candidate:      candidate,
		NewAsset:       newAsset,
		Slug:           slug,
		Title:          title,
		Actor:          actor,
		Audit:          auditEntry(actor.User, in.ProjectID, candidate),
		IdempotencyKey: in.IdempotencyKey,
	})
	if err != nil {
		return PublishedVersion{}, mapPublishError(err)
	}
	return stored, nil
}

// requireAgent is the domain backstop: an agent never publishes, whatever
// the matrix says. It reads the actor value alone, so it is resolved
// before any lookup.
func (c *Command) requireAgent(actor Actor) error {
	if actor.IsAgent {
		return &AgentNotPermittedError{Action: "publish"}
	}
	return nil
}

// prepare validates the request shape and renders the candidate the gate
// and the preview both take.
//
// The pid is decided HERE, and this is the only place it is decided: a
// request that names no asset gets a fresh pid from assets.NewPID(),
// which is the pid the create will store. Nothing else in this package
// mints one — in particular the idempotency replay runs after this
// point but does not compare minted pids (a replay never compares an
// identity the caller did not send).
func (c *Command) prepare(in PublishParams) (candidate assets.PublishCandidate, newAsset bool, slug, title string, err error) {
	if strings.TrimSpace(in.ProjectID) == "" {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	assetType, ok := assets.ParseType(in.AssetType)
	if !ok {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: asset_type must be one of the four V1 types, got %q", ErrValidation, in.AssetType)
	}
	version := strings.TrimSpace(in.Version)
	if !assets.ValidVersionLabel(version) {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: version %q must be 1..%d characters of [A-Za-z0-9._-]", ErrValidation, in.Version, assets.MaxVersionLen)
	}
	visibility := assets.Visibility(strings.TrimSpace(in.Visibility))
	if !visibility.Valid() {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: visibility must be public or private, got %q", ErrValidation, in.Visibility)
	}

	namedPID := strings.TrimSpace(in.AssetPID)
	var pid assets.PID
	switch {
	case namedPID == "":
		minted, err := c.newPID()
		if err != nil {
			return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: mint a pid: %v", ErrStore, err)
		}
		if !ValidPID(minted) {
			return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: the pid generator produced %q, which is not a pid", ErrStore, minted)
		}
		pid = minted
		newAsset = true
	case !ValidPID(assets.PID(namedPID)):
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: asset_id %q is not a persistent identifier (26 Crockford base32 characters)", ErrValidation, in.AssetPID)
	default:
		pid = assets.PID(namedPID)
	}

	slug = strings.TrimSpace(in.Slug)
	title = strings.TrimSpace(in.Title)
	switch {
	case newAsset && title == "":
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: title is required when the publish creates an asset", ErrValidation)
	case newAsset && slug == "":
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: slug is required when the publish creates an asset", ErrValidation)
	case !newAsset && title != "":
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: title is not accepted when publishing a version of an existing asset; asset metadata is revised on its own surface (docs/11 §4)", ErrValidation)
	case !newAsset && slug != "":
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: slug is not accepted when publishing a version of an existing asset; asset metadata is revised on its own surface (docs/11 §4)", ErrValidation)
	}
	if len(slug) > MaxSlugLen {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: slug must be 1..%d characters", ErrValidation, MaxSlugLen)
	}
	if len(title) > MaxTitleLen {
		return assets.PublishCandidate{}, false, "", "", fmt.Errorf("%w: title must be 1..%d characters", ErrValidation, MaxTitleLen)
	}

	// The refs are canonicalised HERE, through assets.NewOriginRef, so
	// what the gate validates and what the row stores are the same
	// strings. A ref that is not canonical is left as the caller sent it:
	// the gate is the component that refuses it, and it refuses it by
	// name (ASSET_INVALID_PROVENANCE_REF) — a pre-normalisation here would
	// hide which of two spellings was refused.
	refs := make([]string, 0, len(in.OriginRefs))
	for _, raw := range in.OriginRefs {
		kind, value, ok := assets.ParseOriginRef(raw)
		if !ok {
			refs = append(refs, raw)
			continue
		}
		ref, _ := assets.NewOriginRef(kind, value)
		refs = append(refs, string(ref))
	}

	return assets.PublishCandidate{
		AssetPID:      pid,
		AssetType:     assetType,
		Version:       version,
		Manifest:      in.Manifest,
		RightsJSON:    in.Rights,
		OriginRefs:    refs,
		Visibility:    visibility,
		IntegrityHash: strings.TrimSpace(in.IntegrityHash),
		CreatorIDs:    in.CreatorIDs,
	}, newAsset, slug, title, nil
}

// ValidPID reports whether pid has the persistent-identifier shape. It is
// assets.ValidPID, named here so the command reads the same predicate the
// gate does.
func ValidPID(pid assets.PID) bool { return assets.ValidPID(string(pid)) }

// MaxSlugLen and MaxTitleLen bound the display fields of a newly created
// asset. The columns are unbounded text; a publish is not the place to
// store an essay, and a bounded refusal is one a client can act on.
const (
	MaxSlugLen  = 128
	MaxTitleLen = 256
)

// requirePublish authorizes the publish: resolve the actor's membership
// role and evaluate authz.ActionPublishPrivateToPublic for the class.
//
// The denial precedes every lookup, and that is the point rather than an
// artifact of the ordering: the membership resolution answers
// projects.ErrMemberNotFound for a project that does not exist exactly as
// it does for one the caller does not belong to, so a caller probing for
// projects with this route is answered "not permitted" either way. Only
// projects.ErrProjectNotFound — the deliberate disclosure the project
// surface makes for an invisible project — is relayed as a not-found, and
// it is not the answer this resolution gives for an unknown project (see
// MembershipPort).
//
// A nil or erroring engine is a wiring failure, never a permission: the
// refusal is ErrStore.
func (c *Command) requirePublish(ctx context.Context, actorID, projectID string, isAgent bool) error {
	if c.members == nil || c.authz == nil {
		return fmt.Errorf("%w: publish command not fully wired", ErrStore)
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
		return ErrProjectNotFound
	default:
		return mapPublishError(err)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, role, isAgent),
	})
	if err != nil {
		return fmt.Errorf("%w: authorize publish: %v", ErrStore, err)
	}
	if !decision.Permits() {
		return ErrForbidden
	}
	return nil
}

// requirePolicy evaluates the governance policy in force for the
// publication (docs/12 §5: the organization's bound overlaid by the
// project's own, never silently looser).
//
// The rule is domain.RulePublicAssetIPReview — "requires an IP review
// before a public asset is published" — and it is read only when the
// version being published is PUBLIC, because that is the only case the
// rule is about.
//
// Three of the four answers are refusals and one is not:
//
//   - a policy that cannot be read: refusal. An unreadable policy is not
//     a permissive one.
//   - a policy that cannot be evaluated: refusal, for the same reason.
//   - the rule SET TO TRUE: refusal. This build records no IP review, so
//     a policy that requires one is a requirement the platform cannot
//     satisfy — the honest answer is RIGHTS_POLICY_BLOCKS_ACTION, not a
//     publication that quietly skipped the review.
//   - the rule set to FALSE, or absent: permitted. docs/23 §4 lists the
//     org policy approvals as OPTIONAL, and the rule's own documentation
//     says true is the stricter value — so a policy that does not require
//     an IP review does not require one. This is the one place this
//     command and merge.Service.requirePolicy answer differently, and
//     they should: frozen main is protected by the platform itself
//     (docs/09 §3), so silence cannot waive it, while an IP review is a
//     governance requirement that only a policy can impose.
func (c *Command) requirePolicy(ctx context.Context, actor domain.User, projectID string, visibility assets.Visibility) error {
	if visibility != assets.VisibilityPublic {
		// A private publication widens nothing; the rule is about public
		// assets and is not read.
		return nil
	}
	if c.policies == nil || c.rules == nil {
		return fmt.Errorf("%w: publish command not fully wired", ErrStore)
	}
	rule := domain.RulePublicAssetIPReview
	refuse := func(found, value bool, reason string, cause error) error {
		return &PolicyRefusedError{Rule: rule, Found: found, Bool: value, Reason: reason, Err: cause}
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

// replayedMatches decides whether an Idempotency-Key's stored publication
// is the SAME publication the caller is repeating (replay) or a different
// one (conflict, docs/45).
//
// Two things are compared, and they are the two the caller controls:
//
//   - the version label. A key that published version A may not answer
//     for a request that asks for version B: the caller would receive a
//     row it did not ask for and could not tell.
//   - the asset pid, WHEN THE REQUEST NAMES ONE. The comparison is
//     deliberately skipped for a create request (an empty pid), because a
//     create request's pid is minted per request — comparing it would
//     turn every repeat of a create into a conflict, which is exactly the
//     case docs/22's Idempotency-Key exists for.
//
// The residual case that rule leaves open — a key whose first publish
// named a pid, replayed by a request that names none and carries the same
// version label — is a replay rather than a conflict. It cannot produce a
// second row (a replay is a read), and the row it returns is one this
// caller's own key produced.
func replayedMatches(stored PublishedVersion, candidate assets.PublishCandidate, newAsset bool) error {
	if stored.Version != candidate.Version {
		return fmt.Errorf("%w: this Idempotency-Key published version %q, and this request is for version %q",
			ErrIdempotencyConflict, stored.Version, candidate.Version)
	}
	if !newAsset && stored.AssetPID != string(candidate.AssetPID) {
		return fmt.Errorf("%w: this Idempotency-Key published a version of asset %q, and this request is for asset %q",
			ErrIdempotencyConflict, stored.AssetPID, candidate.AssetPID)
	}
	return nil
}

// mapPublishError keeps the sentinels of this package and of its ports
// (the port contracts) and turns everything else into ErrStore.
func mapPublishError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, ErrValidation),
		errors.Is(err, ErrAgentNotPermitted),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrAssetNotFound),
		errors.Is(err, ErrAssetExists),
		errors.Is(err, ErrVersionImmutable),
		errors.Is(err, ErrIdempotencyConflict),
		errors.Is(err, ErrPolicyRefused),
		errors.Is(err, ErrRefused):
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// auditEntry renders the audit row the store appends inside the publish
// transaction. TargetRef is left empty: the store fills it with
// "asset_version:<id>" once the insert has assigned the id (the release
// store's rule).
func auditEntry(actor domain.User, projectID string, candidate assets.PublishCandidate) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Via:       domain.ViaSession,
		Action:    ActionAssetVersionPublished,
		ProjectID: projectID,
		AfterSummary: map[string]any{
			"asset_id":       string(candidate.AssetPID),
			"version":        candidate.Version,
			"asset_type":     string(candidate.AssetType),
			"visibility":     string(candidate.Visibility),
			"integrity_hash": candidate.IntegrityHash,
			"origin_refs":    candidate.OriginRefs,
		},
	}
}
