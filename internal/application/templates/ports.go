package templates

import (
	"context"
	"encoding/json"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
)

// Ports: the templates service orchestrates against the OWNING
// applications instead of touching their stores — every default is
// applied through the same service gates a user performing the steps by
// hand would pass (docs/52: application orchestrates against ports). The
// production adapters are the projects/schemaprofiles/policy/rsg services
// themselves, which satisfy these interfaces structurally. Each gate is
// the smallest slice the templates service needs, so unit tests compose
// the orchestration with fakes instead of the whole production graph.

// ProjectCreator is the slice of the projects service the instantiation
// needs: the ordinary project creation (which runs the create_project
// authorization itself).
type ProjectCreator interface {
	Create(ctx context.Context, actor domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error)
}

// ProjectReader is the slice of the projects service the instantiation
// record read needs: the project read (visibility + membership enforced,
// denied reads existence-hidden as ErrProjectNotFound, docs/45). A
// project's template origin is exactly as visible as the project itself —
// a private project's origin never leaks to a caller who may not read the
// project.
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// SchemaProfileRegistrar is the slice of the schema profile service the
// instantiation needs: registering one template profile as version "1" of
// the new project's namespaced schema id.
type SchemaProfileRegistrar interface {
	Register(ctx context.Context, actor domain.User, projectID string, in schemaprofiles.RegisterInput) (domain.ProjectSchemaProfile, error)
}

// PolicySetter is the slice of the policy service the instantiation needs:
// writing the template's review defaults as the project's FIRST policy
// version.
type PolicySetter interface {
	SetProjectPolicy(ctx context.Context, actor domain.User, projectID, version string, doc json.RawMessage) (domain.PolicyVersion, error)
}

// MapSeeder is the slice of the RSG service the instantiation needs:
// creating the first (main) branch and the initial research-question
// objects on it — the research-map seeds (T0211's question axis).
type MapSeeder interface {
	CreateBranch(ctx context.Context, actor domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error)
	CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error)
}

// InstantiationStore is the persistence port for the provenance records
// (project_template_instantiations, 00056). The table is append-only at
// the database; the store answers the project's one template origin.
type InstantiationStore interface {
	// RecordInstantiation inserts the provenance row. It fails with
	// ErrInstantiationExists when the project already records an origin
	// (UNIQUE (project_id)); any other failure — a foreign-key violation
	// from an unknown project or creator included — travels out as an
	// ordinary error and the service reports it as a failed step.
	RecordInstantiation(ctx context.Context, r domain.TemplateInstantiation) (domain.TemplateInstantiation, error)
	// GetInstantiation returns the project's template origin or
	// ErrInstantiationNotFound when it has none.
	GetInstantiation(ctx context.Context, projectID string) (domain.TemplateInstantiation, error)
}
