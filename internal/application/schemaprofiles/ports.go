package schemaprofiles

import (
	"context"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// Ports: the profile store is the application's one gateway to persisted
// profile state (docs/52: application orchestrates against ports; the
// production adapter is persistence.SchemaProfileStore). The project gate
// is a separate port because project state is owned by the projects
// application; the production implementation is projects.Service.

// Ref pins a schema by registry id + version (the schemareg.Ref shape,
// domain-neutral so the transport can decode into it).
type Ref struct {
	ID      string
	Version string
}

// RegisterInput carries the client's registration request after transport
// decoding: the profile name, the version label, the official base schema
// pin, and the project's custom field definitions. The server derives the
// schema id ("project:<project_id>:<name>") and generates the full profile
// document — the client never supplies either.
type RegisterInput struct {
	// Name is the profile name token (lowercase snake_case); the schema
	// id is derived as "project:<project_id>:<name>".
	Name string
	// Version is the profile version label ([A-Za-z0-9._-]{1,64}).
	Version string
	// Base pins the official base schema this profile extends, by
	// registry id + version (e.g. the canonical experiment schema at
	// version "1").
	Base Ref
	// Properties holds the project's custom field definitions: a map of
	// field name to its JSON Schema fragment. Names must be NEW fields —
	// a profile extends the base, it never redefines base fields.
	Properties map[string]any
	// Required lists additional required field names. Each name must
	// exist among the merged (base + custom) properties: tightening the
	// base's own required array is allowed, inventing a field is not.
	Required []string
}

// ProfileStore is the persistence port for project schema profiles.
type ProfileStore interface {
	// RegisterProfile inserts one immutable profile version and its
	// audit entry in one transaction. It fails with
	// ErrProfileVersionExists when (project, schema_id, version) is
	// already registered — content is never overwritten.
	RegisterProfile(ctx context.Context, p domain.ProjectSchemaProfile, audit domain.AuditEntry) (domain.ProjectSchemaProfile, error)
	// GetProfile returns one version by (project, schema_id, version) —
	// any age — or ErrProfileNotFound.
	GetProfile(ctx context.Context, projectID, schemaID, version string) (domain.ProjectSchemaProfile, error)
	// GetLatestProfile returns the profile's newest registered version,
	// or ErrProfileNotFound.
	GetLatestProfile(ctx context.Context, projectID, schemaID string) (domain.ProjectSchemaProfile, error)
	// ListProfiles returns every profile version of the project, newest
	// first.
	ListProfiles(ctx context.Context, projectID string) ([]domain.ProjectSchemaProfile, error)
	// ListAllProfiles returns every registered profile row of every
	// project — the API startup load.
	ListAllProfiles(ctx context.Context) ([]domain.ProjectSchemaProfile, error)
}

// ProjectGate is the project-surface slice the service needs. The
// production implementation is projects.Service — the same gate shape the
// RSG service uses.
type ProjectGate interface {
	// Get is the visibility-aware project read (T0106): a profile read
	// is exactly as visible as its project.
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
	// GetMembership returns the actor's membership, or
	// projects.ErrMemberNotFound when the project is readable but the
	// actor holds no membership; a denied read answers
	// projects.ErrProjectNotFound (existence hiding).
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}
