package templates

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
)

// Service orchestrates the template use cases: listing the official
// catalog and instantiating a project from a template. Every default is
// applied through the owning service (projects / schemaprofiles / policy /
// rsg) with the SAME actor — the creator is the new project's owner, so
// the owning gates resolve exactly as if the user performed the steps by
// hand. The service itself owns no authorization beyond resolving the
// template: project creation is the guarded action.
type Service struct {
	catalog        *Catalog
	store          InstantiationStore
	projects       ProjectCreator
	projectsReader ProjectReader
	profiles       SchemaProfileRegistrar
	policy         PolicySetter
	mapSeeds       MapSeeder
	logger         *slog.Logger
}

// Deps wires the service. Catalog, store and every gate are required: an
// unwired service refuses at call time rather than guessing (fail closed,
// docs/12). Policy and mapSeeds may be nil only when the resolved
// template defaults nothing of that kind — the service still refuses a
// template that needs an unwired step (never silently skip a default).
type Deps struct {
	Catalog  *Catalog
	Store    InstantiationStore
	Projects ProjectCreator
	// ProjectReader gates the instantiation-record read behind the
	// project's own visibility (the same service instance as Projects in
	// production).
	ProjectReader ProjectReader
	Profiles      SchemaProfileRegistrar
	Policy        PolicySetter
	MapSeeds      MapSeeder
	// Logger receives the cause of a failed default (see failStep): the
	// report the caller reads carries a stable sentence, the dependency
	// detail stays here. Optional — nil means the process default logger,
	// which cmd/api sets to the server's JSON handler.
	Logger *slog.Logger
}

// NewService builds the service on the ports.
func NewService(deps Deps) *Service {
	return &Service{
		catalog:        deps.Catalog,
		store:          deps.Store,
		projects:       deps.Projects,
		projectsReader: deps.ProjectReader,
		profiles:       deps.Profiles,
		policy:         deps.Policy,
		mapSeeds:       deps.MapSeeds,
		logger:         deps.Logger,
	}
}

// log is the service log: the wired logger, or the process default (the
// server's configured handler) when none was wired.
func (s *Service) log() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.Default()
}

// InstantiateInput carries a create-from-template request. Every project
// field the caller supplies WINS over the template's default — the
// template fills only what the caller left unset. Fields absent in the
// template AND the request fail the same validation the plain create
// runs.
type InstantiateInput struct {
	// TemplateID is the template slug (e.g. "materials-discovery").
	TemplateID string
	// TemplateVersion pins the template version; "" resolves the latest.
	TemplateVersion string
	// OrganizationID / ProgramID / Slug / Name / Purpose / Visibility are
	// the ordinary create-project fields. Slug is REQUIRED: no template
	// carries a slug default (TemplateProjectDefaults holds name, purpose
	// and visibility only), so a request that omits it fails the projects
	// service's slug validation exactly as a plain create would — the
	// template cannot name the project's path for it. OrganizationID and
	// ProgramID are likewise the caller's alone (a template never picks an
	// organization or program); Name, Purpose and Visibility fall back to
	// the template's defaults when the caller leaves them unset.
	OrganizationID *string
	ProgramID      *string
	Slug           string
	Name           string
	Purpose        string
	Visibility     domain.ProjectVisibility
}

// Instantiation is the full outcome of one create-from-template: the
// template version that resolved, the created project + creator
// membership, the provenance record, and the per-default application
// report.
type Instantiation struct {
	Template      domain.ProjectTemplate
	Project       domain.Project
	Membership    domain.ProjectMembership
	Instantiation domain.TemplateInstantiation
	Applied       AppliedReport
}

// AppliedReport is the per-default application report: one entry per
// default the template carried, each either applied or failed. The project
// row is the primary fact of the request — a failed default never fakes a
// failed creation, it shows up here.
type AppliedReport struct {
	// Recorded is the provenance-record step ("记录 template id/version").
	Recorded *StepOutcome
	// Profiles is one entry per template schema profile (registration
	// order).
	Profiles []AppliedProfile
	// Policy is the review-defaults step (nil when the template defaults
	// no policy).
	Policy *StepOutcome
	// Map is the research-map step (nil when the template seeds no
	// questions): the created main branch and the seeded questions.
	Map *AppliedMap
}

// StepOutcome is one applied-or-failed step: the applied value travels
// with the outcome; Error describes a failure.
//
// The report is a WIRE boundary — handlers.go renders it into the 201 body
// — so a failure is published in stable terms only: the step's name and
// the failure CLASS, never the owning service's error text (see failStep).
// A dependency error travels with driver text, host names, user names and
// database names; docs/45 draws the line at stable codes. The cause is not
// dropped — it goes to the service log.
type StepOutcome struct {
	Error string
}

// Failure classes the report publishes (never the cause itself).
const (
	// classUnavailable: the owning dependency could not be reached — the
	// same default may apply on a later attempt.
	classUnavailable = "the service was unavailable"
	// classRejected: the owning service refused this default under its own
	// rules (validation, authorization, an existing row).
	classRejected = "the owning service refused the default"
	// classEncoding: the default could not even be encoded for the owning
	// service — an internal fault, reported without the encoder's text.
	classEncoding = "the default could not be encoded"
)

// AppliedProfile is one schema profile registration outcome.
type AppliedProfile struct {
	StepOutcome
	// SchemaID is the registered profile's namespaced schema id
	// ("project:<project_id>:<name>"); Version its profile version
	// label ("1").
	SchemaID string
	Version  string
}

// AppliedMap is the research-map seeding outcome: the branch the seeds
// were created on, and the seeded questions in creation order (children
// after their parents).
type AppliedMap struct {
	StepOutcome
	BranchID  string
	Questions []AppliedQuestion
}

// AppliedQuestion is one seeded research question.
type AppliedQuestion struct {
	StepOutcome
	// ObjectID is the created scientific object's id.
	ObjectID string
	// Statement is the question text that was seeded.
	Statement string
}

// Instantiate creates a project from an official template: resolves the
// template, merges its project defaults under the caller's fields, runs
// the ordinary project creation, records the provenance row, and applies
// each remaining default through its owning service. The applied steps
// are reported individually (see AppliedReport) — the project exists as
// soon as its create succeeds, and no later failure is hidden or faked.
func (s *Service) Instantiate(ctx context.Context, actor domain.User, in InstantiateInput) (Instantiation, error) {
	if s.catalog == nil {
		return Instantiation{}, fmt.Errorf("%w: no template catalog configured", ErrStore)
	}
	tmpl, err := s.catalog.Resolve(in.TemplateID, in.TemplateVersion)
	if err != nil {
		return Instantiation{}, err
	}
	createIn := mergeCreateInput(in, tmpl)
	if s.projects == nil {
		return Instantiation{}, fmt.Errorf("%w: no project creator configured", ErrStore)
	}
	project, membership, err := s.projects.Create(ctx, actor, createIn)
	if err != nil {
		return Instantiation{}, wrapProjectError(err)
	}
	out := Instantiation{Template: tmpl, Project: project, Membership: membership}

	// The provenance record (requirement: 记录 template id/version). It
	// is a recorded fact, never a control: nothing reads it back to
	// re-apply anything.
	if s.store != nil {
		rec, rerr := s.store.RecordInstantiation(ctx, domain.TemplateInstantiation{
			ProjectID:       project.ID,
			TemplateID:      tmpl.ID,
			TemplateVersion: tmpl.Version,
			TemplateName:    tmpl.Name,
			CreatedBy:       actor.ID,
		})
		if rerr != nil {
			step := s.failOwningStep("the template origin record", project.ID, wrapStoreError(rerr))
			out.Applied.Recorded = &step
		} else {
			out.Applied.Recorded = &StepOutcome{}
			out.Instantiation = rec
		}
	} else {
		out.Applied.Recorded = &StepOutcome{Error: "no instantiation store configured"}
	}

	// Schema profile defaults (T0213): each registered as version "1" of
	// the project's namespaced profile id.
	for _, p := range tmpl.Schemas {
		applied := AppliedProfile{StepOutcome: StepOutcome{}, SchemaID: "project:" + project.ID + ":" + p.Name, Version: "1"}
		out.Applied.Profiles = append(out.Applied.Profiles, applied)
	}
	if len(tmpl.Schemas) > 0 {
		if s.profiles == nil {
			// Nothing was registered, so EVERY profile carries the error:
			// a single failed outcome would render profiles 1..n as
			// applied on the wire — a "schema registered" fact that never
			// happened (docs/12: refuse, never guess).
			for i := range out.Applied.Profiles {
				out.Applied.Profiles[i].Error = "no schema profile registrar configured"
			}
		} else {
			for i, p := range tmpl.Schemas {
				if _, err := s.profiles.Register(ctx, actor, project.ID, schemaprofiles.RegisterInput{
					Name:       p.Name,
					Version:    "1",
					Base:       schemaprofiles.Ref{ID: p.Base.ID, Version: p.Base.Version},
					Properties: p.Properties,
					Required:   p.Required,
				}); err != nil {
					out.Applied.Profiles[i].StepOutcome = s.failOwningStep("the schema profile "+p.Name, project.ID, wrapProfileError(err))
				}
			}
		}
	}

	// Review defaults (docs/12 §5): the project's FIRST policy version —
	// an ordinary version row the owner supersedes through the policy
	// surface. Required schema profiles are deliberately NOT set here: a
	// required_schema_profiles rule would govern future object writes,
	// and a template only initializes — it never controls.
	if len(tmpl.Review.Rules) > 0 {
		out.Applied.Policy = &StepOutcome{}
		if s.policy == nil {
			out.Applied.Policy.Error = "no policy setter configured"
		} else {
			doc, merr := json.Marshal(tmpl.Review.Rules)
			if merr != nil {
				*out.Applied.Policy = s.failStep("the review defaults", classEncoding, project.ID, merr)
			} else if _, perr := s.policy.SetProjectPolicy(ctx, actor, project.ID, "v1", doc); perr != nil {
				*out.Applied.Policy = s.failOwningStep("the review defaults", project.ID, wrapPolicyError(perr))
			}
		}
	}

	// Research-map seeds (T0211): the initial research questions on the
	// project's first (main) branch, each a normal scientific object.
	if len(tmpl.Map) > 0 {
		out.Applied.Map = s.seedMap(ctx, actor, project, tmpl)
	}
	return out, nil
}

// seedMap creates the main branch and the template's question tree on it.
// The branch mirrors the project's own visibility preset (docs/09 §1: a
// branch stays within its project's preset — a private project cannot
// host a public branch, and nothing here widens visibility). Questions
// are created parents-first so each child can pin its parent's object id.
func (s *Service) seedMap(ctx context.Context, actor domain.User, project domain.Project, tmpl domain.ProjectTemplate) *AppliedMap {
	m := &AppliedMap{StepOutcome: StepOutcome{}}
	if s.mapSeeds == nil {
		m.Error = "no map seeder configured"
		return m
	}
	purpose := fmt.Sprintf("Initial research state seeded by template %s %s", tmpl.ID, tmpl.Version)
	branch, err := s.mapSeeds.CreateBranch(ctx, actor, project.ID, rsg.CreateBranchInput{
		Name:       domain.MainBranchName,
		BaseRef:    "",
		Visibility: domain.BranchVisibility(project.Visibility),
		Purpose:    &purpose,
	})
	if err != nil {
		m.StepOutcome = s.failOwningStep("the research map's main branch", project.ID, wrapMapError(err))
		return m
	}
	m.BranchID = branch.ID

	// Parents first, then their children, depth-first in declaration
	// order — a child's payload pins its parent's freshly created object
	// id (the advisory parent_question_id the outline renders the tree
	// from, T0211).
	var seedNode func(parentID string, q domain.TemplateQuestion)
	seedNode = func(parentID string, q domain.TemplateQuestion) {
		payload := map[string]any{
			"statement":      q.Statement,
			"question_state": "open",
			"purpose":        q.Purpose,
		}
		if parentID != "" {
			payload["parent_question_id"] = parentID
		}
		raw, merr := json.Marshal(payload)
		applied := AppliedQuestion{StepOutcome: StepOutcome{}, Statement: q.Statement}
		if merr != nil {
			applied.StepOutcome = s.failStep("a seeded research question", classEncoding, project.ID, merr)
			m.Questions = append(m.Questions, applied)
			return
		}
		res, oerr := s.mapSeeds.CreateObject(ctx, actor, project.ID, branch.ID, rsg.CreateObjectInput{
			ObjectType: "research_question",
			Payload:    raw,
		})
		if oerr != nil {
			applied.StepOutcome = s.failOwningStep("a seeded research question", project.ID, wrapMapError(oerr))
			m.Questions = append(m.Questions, applied)
			return
		}
		applied.ObjectID = res.Object.ID
		m.Questions = append(m.Questions, applied)
		for _, child := range q.Children {
			seedNode(applied.ObjectID, child)
		}
	}
	for _, q := range tmpl.Map {
		seedNode("", q)
	}
	return m
}

// failOwningStep classifies one owning-service failure and publishes only
// its class: ErrStore-wrapped causes (any dependency failure the owning
// service did not recognize — the store, the database, the network)
// become classUnavailable, everything the owning service named itself
// (validation, authorization, an existing row) becomes classRejected.
func (s *Service) failOwningStep(step, projectID string, cause error) StepOutcome {
	class := classRejected
	if errors.Is(cause, ErrStore) {
		class = classUnavailable
	}
	return s.failStep(step, class, projectID, cause)
}

// failStep renders one failed default for the application report and sends
// its cause to the service log.
//
// The report carries the step name and the failure class ONLY — never the
// cause. The report is rendered into the 201 body (handlers.go), and a
// dependency error is not safe to publish: it carries driver text, host
// names, user names and database names (docs/45: stable codes, no
// dependency detail — handlers.go's templateError holds the same line for
// the response status, TestTemplateErrorDoesNotLeakRawErrors). The cause
// is not thrown away: it is logged with the step and the project, so
// operators keep the detail the caller must not see.
func (s *Service) failStep(step, class, projectID string, cause error) StepOutcome {
	s.log().Warn("templates: default not applied",
		"step", step, "class", class, "project_id", projectID, "error", cause)
	return StepOutcome{Error: step + ": " + class}
}

// mergeCreateInput merges the template's project defaults under the
// caller's fields: every field the caller supplied wins; the template
// fills the rest. A field neither supplied nor defaulted falls through to
// the projects service's own validation (purpose and visibility are
// always required from one of the two).
func mergeCreateInput(in InstantiateInput, tmpl domain.ProjectTemplate) projects.CreateProjectInput {
	out := projects.CreateProjectInput{
		OrganizationID: in.OrganizationID,
		ProgramID:      in.ProgramID,
		Slug:           in.Slug,
		Name:           in.Name,
		Purpose:        in.Purpose,
		Visibility:     in.Visibility,
	}
	if out.Name == "" {
		out.Name = tmpl.Project.Name
	}
	if out.Purpose == "" {
		out.Purpose = tmpl.Project.Purpose
	}
	if out.Visibility == "" {
		out.Visibility = tmpl.Project.Visibility
	}
	return out
}

// List returns the catalog's latest version per template id, id-sorted —
// the catalog view the UI renders.
func (s *Service) List() ([]domain.ProjectTemplate, error) {
	if s.catalog == nil {
		return nil, fmt.Errorf("%w: no template catalog configured", ErrStore)
	}
	return s.catalog.LatestIDs(), nil
}

// Get returns one template by id and version ("" = latest).
func (s *Service) Get(id, version string) (domain.ProjectTemplate, error) {
	if s.catalog == nil {
		return domain.ProjectTemplate{}, fmt.Errorf("%w: no template catalog configured", ErrStore)
	}
	return s.catalog.Resolve(id, version)
}

// GetInstantiation returns the project's recorded template origin, or
// ErrInstantiationNotFound when the project was created without one. The
// project read runs FIRST (docs/09: reads decide their own visibility): a
// caller who may not read the project gets the existence-hiding
// ErrProjectNotFound, never the record.
func (s *Service) GetInstantiation(ctx context.Context, r projects.Reader, projectID string) (domain.TemplateInstantiation, error) {
	if s.projectsReader == nil {
		return domain.TemplateInstantiation{}, fmt.Errorf("%w: no project reader configured", ErrStore)
	}
	if _, err := s.projectsReader.Get(ctx, r, projectID); err != nil {
		return domain.TemplateInstantiation{}, wrapProjectError(err)
	}
	if s.store == nil {
		return domain.TemplateInstantiation{}, fmt.Errorf("%w: no instantiation store configured", ErrStore)
	}
	rec, err := s.store.GetInstantiation(ctx, projectID)
	if err != nil {
		return domain.TemplateInstantiation{}, wrapStoreError(err)
	}
	return rec, nil
}

// wrapStoreError keeps the store's sentinels and turns everything else
// into ErrStore, with the cause kept for the log (failStep logs the
// wrapped error; the report publishes only the class).
func wrapStoreError(err error) error {
	if err == nil ||
		errors.Is(err, ErrInstantiationExists) ||
		errors.Is(err, ErrInstantiationNotFound) ||
		errors.Is(err, ErrValidation) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// wrapProjectError maps the projects service's outcomes onto this
// package's: the create validation and existence errors pass through (the
// transport already maps them), everything else is an ErrStore.
func wrapProjectError(err error) error {
	if err == nil || errors.Is(err, projects.ErrValidation) ||
		errors.Is(err, projects.ErrForbidden) ||
		errors.Is(err, projects.ErrProjectNotFound) ||
		errors.Is(err, projects.ErrMemberNotFound) ||
		errors.Is(err, projects.ErrSlugTaken) ||
		errors.Is(err, projects.ErrOrgNotFound) ||
		errors.Is(err, projects.ErrOrgDeactivated) ||
		errors.Is(err, projects.ErrProgramNotFound) ||
		errors.Is(err, projects.ErrProgramOrgMismatch) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

// wrapProfileError/wrapPolicyError/wrapMapError keep the owning services'
// outcomes recognizable so failOwningStep can classify them: a refusal the
// service named itself passes through, anything else becomes ErrStore (the
// unavailable class). Neither shape reaches the report as text.
func wrapProfileError(err error) error {
	if err == nil || errors.Is(err, schemaprofiles.ErrValidation) ||
		errors.Is(err, schemaprofiles.ErrForbidden) ||
		errors.Is(err, schemaprofiles.ErrProjectNotFound) ||
		errors.Is(err, schemaprofiles.ErrProfileVersionExists) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

func wrapPolicyError(err error) error {
	if err == nil || errors.Is(err, policy.ErrValidation) ||
		errors.Is(err, policy.ErrForbidden) ||
		errors.Is(err, policy.ErrProjectNotFound) ||
		errors.Is(err, policy.ErrVersionTaken) ||
		errors.Is(err, policy.ErrProjectRelaxesOrg) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}

func wrapMapError(err error) error {
	if err == nil || errors.Is(err, rsg.ErrValidation) ||
		errors.Is(err, rsg.ErrForbidden) {
		return err
	}
	return fmt.Errorf("%w: %v", ErrStore, err)
}
