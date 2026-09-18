package assetpublish

import (
	"context"
	"encoding/json"
	"time"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters
// live in internal/persistence). The publish command needs exactly four
// things outside itself — the actor's membership role in the project, the
// governance policy in force, the rule evaluator, and the store that owns
// the publish transaction.

// MembershipPort resolves the actor's membership in the project. The
// production implementation is persistence.ProjectStore; a project that
// does not exist, or that the caller is not a member of, answers
// projects.ErrMemberNotFound — which is the resolution the publish
// authorization is built on: an unknown project is an unknown membership,
// not a disclosed absence (see Command.requirePublish).
type MembershipPort interface {
	GetMembership(ctx context.Context, projectID, userID string) (domain.ProjectMembership, error)
}

// PolicyPort resolves the policy in force for one project: the
// organization's lower bound overlaid by the project's own (docs/12 §5).
// The production implementation is *policy.Service; it runs the project
// read gate as part of the resolution, which is why the publish calls it
// only AFTER the authorization above has passed.
type PolicyPort interface {
	EffectivePolicy(ctx context.Context, actor domain.User, projectID string) (domain.EffectivePolicy, error)
}

// RuleEvaluator answers one typed governance question about a policy
// document. The production implementation is policy.RuleEvaluator.
type RuleEvaluator interface {
	Evaluate(ctx context.Context, p domain.Policy, q policy.Query) (policy.Decision, error)
}

// StorePort is the publish store: the idempotency ledger read the command
// checks before doing any work, and the publish transaction itself.
//
// The split is the releases.ReleaseStorePort split, for the same reason:
// a replay is a READ, and it must be possible to answer one without
// resolving a snapshot or running a gate. The command's LookupCreation
// call therefore precedes everything the publish would otherwise compute
// — and the store re-checks the ledger inside its own transaction anyway,
// under the project row lock, because the read outside it is an
// optimization and the one inside it is the guarantee.
type StorePort interface {
	// LookupCreation returns the version an Idempotency-Key already
	// published, or nil when the key has no ledger entry yet.
	LookupCreation(ctx context.Context, projectID, idempotencyKey string) (*PublishedVersion, error)
	// Publish runs the publish: re-check the ledger, resolve the current
	// state, re-run the impact preview over it, and — only if that
	// preview refuses nothing — write the asset row (when the publish
	// creates one), the immutable version row, the ledger entry, the
	// audit row and the domain event in ONE transaction.
	//
	// It answers *PublishRefused when the preview refused, and the
	// sentinels of this package otherwise. It never writes a partial
	// publish: a refused request leaves no row of any kind behind.
	Publish(ctx context.Context, req PublishRequest) (PublishedVersion, error)
}

// PublishRequest is everything one publish needs to execute: the
// candidate (already gated and with its final pid), the decision about
// whether the asset row has to be created, the actor, the audit row to
// append, and the caller's Idempotency-Key.
//
// It carries the actor and the audit row rather than an identity string,
// because the store writes the audit row inside its own transaction (the
// pattern every governed write in this tree follows: the audit record
// commits with the state change or not at all).
type PublishRequest struct {
	// ProjectID is the project the publish happens in, in text uuid form.
	ProjectID string
	// Candidate is the version to publish. Its AssetPID is final by the
	// time the store sees it: the command mints one with assets.NewPID()
	// when the publish creates an asset, and never before the
	// idempotency replay has had its say.
	Candidate assets.PublishCandidate
	// NewAsset is true when the publish must create the asset row the
	// version belongs to, with Candidate.AssetPID as its pid. False means
	// the pid names an asset that must already exist — and a pid that
	// does not is a refusal, not a silent create: a client that names an
	// identity either has it or is asking about the wrong one.
	NewAsset bool
	// Slug and Title are the new asset's display fields. They are read
	// only when NewAsset is true (the command refuses them otherwise, so
	// there is no case in which they are quietly dropped).
	Slug  string
	Title string
	// Actor is the publishing principal.
	Actor Actor
	// Audit is the row the store appends inside the transaction. Its
	// TargetRef is left empty by the command: the store fills it with
	// "asset_version:<id>" once the insert has assigned the id.
	Audit domain.AuditEntry
	// Usages are the asset_dependencies rows this publish declares: the
	// project's use of each exact version its manifest pins
	// (assets.PublishedUsages — a pin is a dependency, and the usage's own
	// visibility is the published version's). The command computes them,
	// the way it computes the audit row above; the store records exactly
	// what it is handed, and it drops the ones whose pin resolves to no
	// stored version — there is no row id to key a usage by, and an
	// unresolved pin is a fact about the publisher's document rather than a
	// version anybody uses.
	Usages []assets.UsageDeclaration
	// IdempotencyKey is the caller's key, or nil when the request
	// carried none.
	IdempotencyKey *string
}

// PublishedVersion is one published research asset version: the stored row
// (append-only, immutable) plus the persistent identity of the asset it
// belongs to.
//
// AssetPID is the version's PUBLIC identity as much as Version is: the
// version row's own asset_id is an internal uuid, and the pair
// (asset pid, version) is what every later reference and URL resolves
// through (docs/11 §2). A caller that has just published needs both.
type PublishedVersion struct {
	// ID is the research_asset_versions row id (internal).
	ID string
	// AssetID is the research_assets row id (internal).
	AssetID string
	// AssetPID is the asset's persistent identifier.
	AssetPID string
	// Version is the immutable version label.
	Version string
	// Visibility is the version's stored visibility.
	Visibility assets.Visibility
	// IntegrityHash is the hash the version was published under: the
	// digest of the manifest bytes in Manifest.
	IntegrityHash string
	// OriginRefs are the stored provenance pins.
	OriginRefs []string
	// PublishedBy is the publishing user's id.
	PublishedBy string
	// PublishedAt is the row's creation instant.
	PublishedAt time.Time
	// Manifest is the stored canonical manifest document.
	Manifest json.RawMessage
	// RightsJSON is the stored rights document.
	RightsJSON json.RawMessage
}
