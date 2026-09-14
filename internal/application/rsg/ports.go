package rsg

import (
	"context"
	"encoding/json"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/relations"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: application orchestrates against ports; adapters live in
// internal/persistence). The RSG service composes the slices of the
// existing surfaces it needs: project membership (the authz class input
// and the read gate), branches, states, and the object/relation writes.
// The object and relation write methods are transaction-scoped: they run
// on the state commit's open transaction (states.Transaction), so a
// version and its state transition share one atomic boundary.

// ProjectGate is the project-surface slice the RSG service needs. The
// production implementation is projects.Service.
type ProjectGate interface {
	// GetMembership returns the actor's membership, or
	// projects.ErrMemberNotFound when the project is readable but the
	// actor holds no membership; a denied read answers
	// projects.ErrProjectNotFound (existence hiding, docs/45).
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
	// Get is the visibility-aware project read (T0106); the GET object
	// command runs it so a read is exactly as visible as its project.
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// BranchPort is the branch-surface slice the RSG service needs. The
// production implementation is branches.Service.
type BranchPort interface {
	Create(ctx context.Context, in branches.CreateBranchParams) (domain.Branch, error)
	// Get returns the project's branch, or branches.ErrBranchNotFound
	// (a branch of another project reports the same outcome).
	Get(ctx context.Context, projectID, branchID string) (domain.Branch, error)
}

// StatePort is the state-surface slice the RSG service needs: every
// scientific-state write lands as one state commit. The production
// implementation is states.Service (wired with the validation guard).
type StatePort interface {
	Commit(ctx context.Context, in states.CommitParams, write states.WriteFunc) (domain.ProjectState, domain.StateCommit, error)
	CreateInitialState(ctx context.Context, in states.CreateInitialStateParams) (domain.ProjectState, error)
	// GetBranchHead returns the branch's current head state — the base
	// every commit is built on.
	GetBranchHead(ctx context.Context, branchID string) (domain.ProjectState, error)
}

// LatestStatePort resolves the project's most recent state — the default
// fork point of a branch created without an explicit base_ref (a project
// with no state yet gets its genesis root created instead). The production
// implementation is persistence.StateStore.
type LatestStatePort interface {
	GetLatestState(ctx context.Context, projectID string) (domain.ProjectState, error)
}

// ObjectPort is the scientific-object slice the RSG service needs. The
// production implementation is persistence.ScientificObjectStore; the
// Create*InTx methods run on the commit transaction.
type ObjectPort interface {
	GetObject(ctx context.Context, objectID string) (domain.ScientificObject, error)
	GetLatestVersion(ctx context.Context, objectID string) (domain.ScientificObjectVersion, error)
	// GetVersion returns one version by its 1-based number (the object
	// detail page's version switch), or sciobjects.ErrVersionNotFound.
	GetVersion(ctx context.Context, objectID string, versionNo int) (domain.ScientificObjectVersion, error)
	// ListVersions returns the whole version log in ascending order (the
	// version switcher options); empty for an unknown object.
	ListVersions(ctx context.Context, objectID string) ([]domain.ScientificObjectVersion, error)
	// GetVersionByID returns one version row by its own id (the relation
	// endpoint pin), or sciobjects.ErrVersionNotFound.
	GetVersionByID(ctx context.Context, versionID string) (domain.ScientificObjectVersion, error)
	CreateObjectInTx(ctx context.Context, tx states.Transaction, in CreateObjectInTxParams) (domain.ScientificObject, domain.ScientificObjectVersion, error)
	CreateVersionInTx(ctx context.Context, tx states.Transaction, objectID string, expected int, in sciobjects.VersionParams) (domain.ScientificObjectVersion, error)
}

// RelationPort is the relation-surface slice the RSG service needs. The
// production implementation is persistence.RelationStore; CreateRelationInTx
// runs on the commit transaction.
type RelationPort interface {
	CreateRelationInTx(ctx context.Context, tx states.Transaction, in CreateRelationInTxParams) (domain.Relation, domain.RelationVersion, error)
	// ListVersionsForObject returns the relation versions whose source or
	// target endpoint pins a version of the object, newest first, with the
	// endpoint display labels resolved (the object detail page's relations
	// tab); empty when the object has no relations.
	ListVersionsForObject(ctx context.Context, projectID, objectID string) ([]ObjectRelationVersion, error)
}

// ProfilePort resolves display handles for creator ids — the detail page
// attributes versions to people, not raw uuids. The production
// implementation is persistence.ProfileStore.
type ProfilePort interface {
	GetByUserID(ctx context.Context, userID string) (domain.Profile, error)
}

// ObjectRelationEndpoint is one relation endpoint's display label,
// resolved by the store in the same round trip as the row itself.
type ObjectRelationEndpoint struct {
	// ObjectID is the endpoint version's container object.
	ObjectID string
	// ObjectType is the container object's scientific type.
	ObjectType string
	// Title is the endpoint version's title.
	Title string
}

// ObjectRelationVersion is one relation version incident to an object,
// with both endpoint labels resolved (the object detail page's relations
// tab).
type ObjectRelationVersion struct {
	Relation domain.RelationVersion
	Source   ObjectRelationEndpoint
	Target   ObjectRelationEndpoint
}

// CreateObjectInTxParams carries an object creation inside a state commit.
// ObjectID is the pre-generated id: the commit's operation summary names
// it (commit_linkage), so the caller generates it before committing.
type CreateObjectInTxParams struct {
	ObjectID   string
	ProjectID  string
	ObjectType string
	CreatedBy  string
	Version    sciobjects.VersionParams
}

// CreateRelationInTxParams carries a relation creation inside a state
// commit. RelationID is pre-generated, same reason as above.
type CreateRelationInTxParams struct {
	RelationID string
	ProjectID  string
	Version    relations.VersionParams
}

// Command inputs (the transport decodes JSON into these and the service
// verifies — the port trusts, the service checks).

// CreateBranchInput carries a research-branch creation request. BaseRef is
// the base state id the branch forks ("" = the project's latest state,
// with the genesis root created when the project has no state yet);
// Visibility empty means "default to the project's preset".
type CreateBranchInput struct {
	Name       string
	BaseRef    string
	Visibility domain.BranchVisibility
	Purpose    *string
}

// CreateObjectInput carries a first-version object creation. Payload must
// be a JSON object (OpenAPI: payload: {type: object}); SchemaRef may name
// the schema $id explicitly — V1 pins the canonical schema per object
// type, so any other value is refused (extension schemas are T0201's
// surface).
type CreateObjectInput struct {
	ObjectType string
	Payload    json.RawMessage
	SchemaRef  string
}

// CreateObjectVersionInput carries a next-version creation: a shallow
// merge patch (top-level keys only) on top of the current payload,
// expected_version being the compare-and-swap expectation.
type CreateObjectVersionInput struct {
	ExpectedVersion int
	Patch           json.RawMessage
}

// CreateRelationInput carries a typed relation creation. Payload is the
// edge's metadata object; empty input is stored as {}.
type CreateRelationInput struct {
	RelationType          string
	SourceObjectVersionID string
	TargetObjectVersionID string
	Payload               json.RawMessage
}
