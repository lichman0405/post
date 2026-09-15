package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
)

// --- fakes: each gate records what it saw and answers what the test pins ---

type fakeProjects struct {
	created  []projects.CreateProjectInput
	actor    []domain.User
	failWith error
	nextID   int
	gets     []projects.Reader
	getErr   error
}

func (f *fakeProjects) Create(_ context.Context, actor domain.User, in projects.CreateProjectInput) (domain.Project, domain.ProjectMembership, error) {
	f.actor = append(f.actor, actor)
	f.created = append(f.created, in)
	if f.failWith != nil {
		return domain.Project{}, domain.ProjectMembership{}, f.failWith
	}
	f.nextID++
	id := "project-" + string(rune('0'+f.nextID))
	slug := in.Slug
	if slug == "" {
		slug = "proj-" + id
	}
	p := domain.Project{
		ID: id, Slug: slug, Name: in.Name, Purpose: in.Purpose,
		Visibility: in.Visibility, ActivityStatus: "planning",
		ProvisionStatus: domain.ProvisionPending, CreatedBy: actor.ID,
	}
	m := domain.ProjectMembership{ProjectID: id, UserID: actor.ID, Role: domain.ProjectRoleOwner}
	return p, m, nil
}

func (f *fakeProjects) Get(_ context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	f.gets = append(f.gets, r)
	if f.getErr != nil {
		return domain.Project{}, f.getErr
	}
	return domain.Project{ID: projectID}, nil
}

type fakeProfiles struct {
	registered []schemaprofiles.RegisterInput
	projects   []string
	failOn     map[string]error
}

func (f *fakeProfiles) Register(_ context.Context, _ domain.User, projectID string, in schemaprofiles.RegisterInput) (domain.ProjectSchemaProfile, error) {
	f.projects = append(f.projects, projectID)
	f.registered = append(f.registered, in)
	if f.failOn != nil {
		if err, ok := f.failOn[in.Name]; ok {
			return domain.ProjectSchemaProfile{}, err
		}
	}
	return domain.ProjectSchemaProfile{ProjectID: projectID, SchemaID: "project:" + projectID + ":" + in.Name, Version: in.Version}, nil
}

type fakePolicy struct {
	set []struct {
		projectID, version string
		doc                json.RawMessage
	}
	err error
}

func (f *fakePolicy) SetProjectPolicy(_ context.Context, _ domain.User, projectID, version string, doc json.RawMessage) (domain.PolicyVersion, error) {
	f.set = append(f.set, struct {
		projectID string
		version   string
		doc       json.RawMessage
	}{projectID, version, doc})
	if f.err != nil {
		return domain.PolicyVersion{}, f.err
	}
	return domain.PolicyVersion{Version: version}, nil
}

type fakeMap struct {
	branches []struct {
		projectID string
		in        rsg.CreateBranchInput
	}
	objects []struct {
		projectID, branchID string
		in                  rsg.CreateObjectInput
	}
	failBranchWith error
	failObjectOn   map[string]error
	nextObject     int
}

func (f *fakeMap) CreateBranch(_ context.Context, _ domain.User, projectID string, in rsg.CreateBranchInput) (domain.Branch, error) {
	f.branches = append(f.branches, struct {
		projectID string
		in        rsg.CreateBranchInput
	}{projectID, in})
	if f.failBranchWith != nil {
		return domain.Branch{}, f.failBranchWith
	}
	return domain.Branch{ID: "branch-1", ProjectID: projectID, Name: in.Name, Visibility: in.Visibility}, nil
}

func (f *fakeMap) CreateObject(_ context.Context, _ domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error) {
	f.objects = append(f.objects, struct {
		projectID string
		branchID  string
		in        rsg.CreateObjectInput
	}{projectID, branchID, in})
	if f.failObjectOn != nil {
		var payload map[string]any
		_ = json.Unmarshal(in.Payload, &payload)
		stmt, _ := payload["statement"].(string)
		if err, ok := f.failObjectOn[stmt]; ok {
			return rsg.ObjectResult{}, err
		}
	}
	f.nextObject++
	return rsg.ObjectResult{Object: domain.ScientificObject{ID: "obj-" + string(rune('0'+f.nextObject)), ObjectType: "research_question"}}, nil
}

type fakeStore struct {
	recorded []domain.TemplateInstantiation
	got      []string
	err      error
}

func (f *fakeStore) RecordInstantiation(_ context.Context, r domain.TemplateInstantiation) (domain.TemplateInstantiation, error) {
	f.recorded = append(f.recorded, r)
	if f.err != nil {
		return domain.TemplateInstantiation{}, f.err
	}
	r.ID = "rec-1"
	return r, nil
}

func (f *fakeStore) GetInstantiation(_ context.Context, projectID string) (domain.TemplateInstantiation, error) {
	f.got = append(f.got, projectID)
	if f.err != nil {
		return domain.TemplateInstantiation{}, f.err
	}
	return domain.TemplateInstantiation{ID: "rec-1", ProjectID: projectID, TemplateID: "materials-discovery", TemplateVersion: "v1"}, nil
}

// testTemplate is one full-featured template for the happy path.
func testTemplate() domain.ProjectTemplate {
	return domain.ProjectTemplate{
		ID:          "materials-discovery",
		Version:     "v1",
		Name:        "Materials Discovery",
		Description: "test",
		Project: domain.TemplateProjectDefaults{
			Name: "Materials Discovery", Purpose: "Find a better anode material.",
			Visibility: domain.VisibilityPrivate,
		},
		// Two profiles on purpose: the official templates carry more than
		// one (materials-discovery: material_ext + experiment_ext), and a
		// multi-profile template is what makes an unwired registrar visible
		// on EVERY profile instead of only the first.
		Schemas: []domain.TemplateSchemaProfile{
			{Name: "material_ext", Base: domain.SchemaRef{ID: "https://open-rd.example/schemas/material", Version: "1"},
				Properties: map[string]any{"target_band_gap": map[string]any{"type": "number"}}, Required: []string{"target_band_gap"}},
			{Name: "experiment_ext", Base: domain.SchemaRef{ID: "https://open-rd.example/schemas/experiment", Version: "1"},
				Properties: map[string]any{"screening_stage": map[string]any{"type": "string"}}, Required: []string{"screening_stage"}},
		},
		Review: TemplateReviewDefaultsForTest(),
		Map: []domain.TemplateQuestion{
			{Statement: "Which family to screen?", Purpose: "scope", Children: []domain.TemplateQuestion{
				{Statement: "Which dopants?", Purpose: "scope"},
			}},
		},
	}
}

func TemplateReviewDefaultsForTest() domain.TemplateReviewDefaults {
	return domain.TemplateReviewDefaults{Rules: map[string]json.RawMessage{
		"main_protected":          json.RawMessage(`true`),
		"release_min_reviewers":   json.RawMessage(`1`),
		"raw_data_retention_days": json.RawMessage(`3650`),
		"public_asset_ip_review":  json.RawMessage(`true`),
	}}
}

func newTestService(cat *Catalog, store InstantiationStore, proj *fakeProjects, prof SchemaProfileRegistrar, pol PolicySetter, seeds MapSeeder) *Service {
	return NewService(Deps{Catalog: cat, Store: store, Projects: proj, ProjectReader: proj, Profiles: prof, Policy: pol, MapSeeds: seeds})
}

func actor() domain.User { return domain.User{ID: "user-1"} }

// --- the acceptance behaviors ---

// TestInstantiateAppliesDefaultsThroughOwningServices pins the full happy
// path: the template fills the unset project fields, the provenance record
// carries the template id + version, and every default reaches its owning
// service with the caller's actor.
func TestInstantiateAppliesDefaultsThroughOwningServices(t *testing.T) {
	tmpl := testTemplate()
	cat := NewCatalogMust(tmpl)
	proj := &fakeProjects{}
	prof := &fakeProfiles{}
	pol := &fakePolicy{}
	seeds := &fakeMap{}
	store := &fakeStore{}
	svc := newTestService(cat, store, proj, prof, pol, seeds)

	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("Instantiate failed: %v", err)
	}
	if out.Project.ID != "project-1" {
		t.Fatalf("project id = %s, want project-1", out.Project.ID)
	}
	// Template defaults filled the unset create fields.
	ci := proj.created[0]
	if ci.Name != "Materials Discovery" || ci.Purpose != "Find a better anode material." || ci.Visibility != domain.VisibilityPrivate {
		t.Errorf("create input = %+v, want the template's project defaults", ci)
	}
	// The provenance record carries template id + version (记录 id/version).
	if len(store.recorded) != 1 {
		t.Fatalf("recorded = %d rows, want 1", len(store.recorded))
	}
	rec := store.recorded[0]
	if rec.ProjectID != "project-1" || rec.TemplateID != "materials-discovery" || rec.TemplateVersion != "v1" || rec.CreatedBy != "user-1" {
		t.Errorf("record = %+v, want project-1 @ materials-discovery v1 by user-1", rec)
	}
	// EVERY profile reached the registrar as version "1" of the namespaced
	// id — not just the first one.
	if len(prof.registered) != 2 || len(prof.projects) != 2 || prof.projects[0] != "project-1" || prof.projects[1] != "project-1" {
		t.Fatalf("profile registrations = %+v on %v, want both profiles on project-1", prof.registered, prof.projects)
	}
	if prof.registered[0].Name != "material_ext" || prof.registered[0].Version != "1" ||
		prof.registered[0].Base.ID != "https://open-rd.example/schemas/material" {
		t.Errorf("first registration = %+v, want material_ext v1 on the canonical material base", prof.registered[0])
	}
	if prof.registered[1].Name != "experiment_ext" || prof.registered[1].Version != "1" ||
		prof.registered[1].Base.ID != "https://open-rd.example/schemas/experiment" {
		t.Errorf("second registration = %+v, want experiment_ext v1 on the canonical experiment base", prof.registered[1])
	}
	// The policy reached the policy service as the FIRST version ("v1") with
	// the template's rules — and without required_schema_profiles.
	if len(pol.set) != 1 || pol.set[0].projectID != "project-1" || pol.set[0].version != "v1" {
		t.Fatalf("policy set = %+v, want one v1 row on project-1", pol.set)
	}
	var rules map[string]json.RawMessage
	if err := json.Unmarshal(pol.set[0].doc, &rules); err != nil {
		t.Fatalf("policy doc is not a rule map: %v", err)
	}
	if _, ok := rules["required_schema_profiles"]; ok {
		t.Error("the template set required_schema_profiles — a control rule a template must never set")
	}
	if string(rules["main_protected"]) != "true" || string(rules["release_min_reviewers"]) != "1" {
		t.Errorf("policy rules = %s, want the template's review defaults", rules)
	}
	// The map seeded one branch and the question tree parents-first.
	if len(seeds.branches) != 1 || seeds.branches[0].in.Name != domain.MainBranchName {
		t.Fatalf("branches = %+v, want one main branch", seeds.branches)
	}
	if seeds.branches[0].in.Visibility != domain.BranchVisibility(domain.VisibilityPrivate) {
		t.Errorf("branch visibility = %s, want the project's private preset", seeds.branches[0].in.Visibility)
	}
	if len(seeds.objects) != 2 {
		t.Fatalf("objects = %d, want parent + child", len(seeds.objects))
	}
	var first, second map[string]any
	_ = json.Unmarshal(seeds.objects[0].in.Payload, &first)
	_ = json.Unmarshal(seeds.objects[1].in.Payload, &second)
	if first["statement"] != "Which family to screen?" || second["statement"] != "Which dopants?" {
		t.Errorf("seeded statements = [%v, %v], want the question tree in order", first["statement"], second["statement"])
	}
	if _, hasParent := first["parent_question_id"]; hasParent {
		t.Error("the root question carries parent_question_id — only children should")
	}
	if second["parent_question_id"] != "obj-1" {
		t.Errorf("child parent_question_id = %v, want the parent's created object id obj-1", second["parent_question_id"])
	}
	// The report shows every step applied ("" errors) — one profile outcome
	// per template profile, each carrying its namespaced schema id.
	if out.Applied.Recorded == nil || out.Applied.Recorded.Error != "" {
		t.Errorf("record step = %+v, want applied", out.Applied.Recorded)
	}
	if len(out.Applied.Profiles) != 2 {
		t.Fatalf("profile outcomes = %+v, want one per template profile", out.Applied.Profiles)
	}
	for i, p := range out.Applied.Profiles {
		if p.Error != "" {
			t.Errorf("profile outcome %d = %+v, want applied", i, p)
		}
		if p.SchemaID != "project:project-1:"+prof.registered[i].Name || p.Version != "1" {
			t.Errorf("profile outcome %d = %+v, want project:project-1:%s v1", i, p, prof.registered[i].Name)
		}
	}
	if out.Applied.Policy == nil || out.Applied.Policy.Error != "" || out.Applied.Map == nil || out.Applied.Map.Error != "" {
		t.Errorf("applied report = %+v, want policy and map applied", out.Applied)
	}
	if out.Applied.Map.BranchID != "branch-1" || len(out.Applied.Map.Questions) != 2 {
		t.Errorf("applied map = %+v, want branch-1 with 2 questions", out.Applied.Map)
	}
	if out.Instantiation.ID != "rec-1" {
		t.Errorf("instantiation id = %s, want the store's rec-1", out.Instantiation.ID)
	}
}

// TestCallerFieldsWinOverTemplateDefaults pins "创建后可独立演化": every
// caller-supplied field beats the template's default — the template fills
// only what was left unset.
func TestCallerFieldsWinOverTemplateDefaults(t *testing.T) {
	cat := NewCatalogMust(testTemplate())
	proj := &fakeProjects{}
	svc := newTestService(cat, &fakeStore{}, proj, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})

	org := "org-1"
	slug := "my-slug"
	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{
		TemplateID: "materials-discovery", OrganizationID: &org, ProgramID: &slug,
		Slug: slug, Name: "My Project", Purpose: "My own goal.", Visibility: domain.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("Instantiate failed: %v", err)
	}
	ci := proj.created[0]
	if ci.Slug != "my-slug" || ci.Name != "My Project" || ci.Purpose != "My own goal." ||
		ci.Visibility != domain.VisibilityPublic || ci.OrganizationID == nil || *ci.OrganizationID != "org-1" {
		t.Errorf("create input = %+v, want the caller's fields everywhere", ci)
	}
	if out.Project.Visibility != domain.VisibilityPublic {
		t.Errorf("project visibility = %s, want the caller's public", out.Project.Visibility)
	}
}

// TestTemplateUpgradeDoesNotTouchExistingProjects pins the third
// acceptance criterion at the service level: after a v2 upgrade, a caller
// pinning v1 gets exactly the v1 defaults, and the record carries v1 — the
// upgrade only changes what NEW instantiations (unpinned or pinned v2)
// receive.
func TestTemplateUpgradeDoesNotTouchExistingProjects(t *testing.T) {
	v1 := testTemplate()
	v2 := v1
	v2.Version = "v2"
	v2.Project.Purpose = "The upgraded purpose."
	v2.Map = []domain.TemplateQuestion{{Statement: "The upgraded question?"}}
	cat := NewCatalogMust(v2, v1) // newest first, both pinnable
	store := &fakeStore{}
	proj := &fakeProjects{}
	seeds := &fakeMap{}
	svc := newTestService(cat, store, proj, &fakeProfiles{}, &fakePolicy{}, seeds)

	// A caller pinning v1 keeps the v1 defaults after the v2 upgrade.
	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery", TemplateVersion: "v1"})
	if err != nil {
		t.Fatalf("pinned v1 instantiate failed: %v", err)
	}
	if proj.created[0].Purpose != "Find a better anode material." {
		t.Errorf("pinned-v1 purpose = %q, want the v1 default", proj.created[0].Purpose)
	}
	if store.recorded[0].TemplateVersion != "v1" {
		t.Errorf("pinned-v1 record version = %s, want v1", store.recorded[0].TemplateVersion)
	}
	if len(seeds.objects) != 2 {
		t.Errorf("pinned-v1 seeded %d questions, want the v1 tree (2)", len(seeds.objects))
	}
	if out.Template.Version != "v1" {
		t.Errorf("resolved template version = %s, want v1", out.Template.Version)
	}

	// A NEW instantiation without a pin gets the upgraded defaults.
	out2, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("latest instantiate failed: %v", err)
	}
	if proj.created[1].Purpose != "The upgraded purpose." {
		t.Errorf("latest purpose = %q, want the upgraded v2 default", proj.created[1].Purpose)
	}
	if store.recorded[1].TemplateVersion != "v2" {
		t.Errorf("latest record version = %s, want v2", store.recorded[1].TemplateVersion)
	}
	if len(seeds.objects) != 3 {
		t.Errorf("latest seeded %d questions total, want 2 (v1) + 1 (v2)", len(seeds.objects))
	}
	_ = out2
}

// TestPartialApplicationIsReportedNotFaked pins the partial-application
// semantics: a default that fails shows up in the report naming the step
// and the failure class — the created project is the primary fact and the
// instantiation still succeeds. The owning service's error TEXT stays out
// of the report (TestAppliedReportCarriesNoDependencyDetail covers that
// boundary and the log that keeps the cause).
func TestPartialApplicationIsReportedNotFaked(t *testing.T) {
	cat := NewCatalogMust(testTemplate())
	prof := &fakeProfiles{failOn: map[string]error{"material_ext": schemaprofiles.ErrValidation}}
	pol := &fakePolicy{err: policy.ErrProjectNotFound}
	seeds := &fakeMap{failBranchWith: rsg.ErrValidation}
	svc := newTestService(cat, &fakeStore{}, &fakeProjects{}, prof, pol, seeds)

	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("Instantiate failed (%v) — a failed default must not fake a failed creation", err)
	}
	if out.Project.ID != "project-1" {
		t.Fatalf("project id = %s, want project-1 (the primary fact)", out.Project.ID)
	}
	if len(out.Applied.Profiles) != 2 {
		t.Fatalf("profile outcomes = %+v, want one per template profile", out.Applied.Profiles)
	}
	if got := out.Applied.Profiles[0].Error; got != "the schema profile material_ext: the owning service refused the default" {
		t.Errorf("profile outcome 0 = %+v, want the refused profile named in the report", out.Applied.Profiles[0])
	}
	if out.Applied.Profiles[1].Error != "" {
		t.Errorf("profile outcome 1 = %+v, want applied — a failing default must not fail its siblings", out.Applied.Profiles[1])
	}
	if out.Applied.Policy == nil || out.Applied.Policy.Error != "the review defaults: the owning service refused the default" {
		t.Errorf("policy outcome = %+v, want the refusal reported", out.Applied.Policy)
	}
	if out.Applied.Map == nil || out.Applied.Map.Error != "the research map's main branch: the owning service refused the default" {
		t.Errorf("map outcome = %+v, want the branch failure reported", out.Applied.Map)
	}
	if out.Applied.Recorded == nil || out.Applied.Recorded.Error != "" {
		t.Errorf("record outcome = %+v, want applied", out.Applied.Recorded)
	}
}

// TestCreateFailureIsFatal: the project create is the guarded, primary
// action — its failure fails the instantiation with the projects
// service's own error (the transport maps it to the plain-create codes).
func TestCreateFailureIsFatal(t *testing.T) {
	cat := NewCatalogMust(testTemplate())
	proj := &fakeProjects{failWith: projects.ErrSlugTaken}
	store := &fakeStore{}
	seeds := &fakeMap{}
	svc := newTestService(cat, store, proj, &fakeProfiles{}, &fakePolicy{}, seeds)

	_, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if !errors.Is(err, projects.ErrSlugTaken) {
		t.Fatalf("Instantiate = %v, want the projects service's ErrSlugTaken", err)
	}
	if len(store.recorded) != 0 {
		t.Error("the provenance record was written although the project create failed")
	}
	if len(seeds.branches) != 0 {
		t.Error("the map was seeded although the project create failed")
	}
}

// TestUnknownTemplate: an unknown id fails before anything is created.
func TestUnknownTemplate(t *testing.T) {
	svc := newTestService(NewCatalogMust(testTemplate()), &fakeStore{}, &fakeProjects{}, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	_, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "nope"})
	if !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("Instantiate = %v, want ErrTemplateNotFound", err)
	}
}

// TestUnwiredGatesFailClosed: a template that defaults profiles/policy/map
// questions must not silently skip them when the gate is unwired — the
// steps report "no registrar configured" instead of vanishing. EVERY
// profile carries the error: with a nil registrar nothing was registered,
// so rendering profile 0 as failed and profiles 1..n as applied would put
// a "schema applied" fact on the wire that never happened.
func TestUnwiredGatesFailClosed(t *testing.T) {
	cat := NewCatalogMust(testTemplate())
	svc := newTestService(cat, &fakeStore{}, &fakeProjects{}, nil, nil, nil)

	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("Instantiate failed: %v", err)
	}
	if len(out.Applied.Profiles) != 2 {
		t.Fatalf("profile outcomes = %+v, want one per template profile", out.Applied.Profiles)
	}
	for i, p := range out.Applied.Profiles {
		if p.Error == "" || !strings.Contains(p.Error, "configured") {
			t.Errorf("profile outcome %d = %+v, want an unwired-gate error on EVERY profile", i, p)
		}
	}
	if out.Applied.Policy.Error == "" || !strings.Contains(out.Applied.Policy.Error, "configured") {
		t.Errorf("policy outcome = %+v, want an unwired-gate error", out.Applied.Policy)
	}
	if out.Applied.Map.Error == "" || !strings.Contains(out.Applied.Map.Error, "configured") {
		t.Errorf("map outcome = %+v, want an unwired-gate error", out.Applied.Map)
	}
}

// TestInstantiateWithNoCatalogFailsClosed: the service refuses instead of
// guessing (fail closed).
func TestInstantiateWithNoCatalogFailsClosed(t *testing.T) {
	svc := newTestService(nil, &fakeStore{}, &fakeProjects{}, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	if _, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "x"}); !errors.Is(err, ErrStore) {
		t.Fatalf("Instantiate = %v, want ErrStore", err)
	}
	if _, err := svc.List(); !errors.Is(err, ErrStore) {
		t.Fatalf("List = %v, want ErrStore", err)
	}
	if _, err := svc.Get("x", ""); !errors.Is(err, ErrStore) {
		t.Fatalf("Get = %v, want ErrStore", err)
	}
}

// TestRecordStoreFailureIsReported: a failing provenance store shows up in
// the report without failing the creation.
func TestRecordStoreFailureIsReported(t *testing.T) {
	svc := newTestService(NewCatalogMust(testTemplate()), &fakeStore{err: ErrInstantiationExists}, &fakeProjects{}, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("Instantiate failed: %v", err)
	}
	if out.Applied.Recorded == nil || out.Applied.Recorded.Error != "the template origin record: the owning service refused the default" {
		t.Errorf("record outcome = %+v, want the store's refusal reported", out.Applied.Recorded)
	}
}

// TestAppliedReportCarriesNoDependencyDetail is the report's half of the
// no-leak gate (handlers_test.go holds the transport half,
// TestTemplateErrorDoesNotLeakRawErrors): a default failing with a
// dependency error — driver text, host, user and database names — is
// reported to the caller in stable terms only, and the cause is kept in
// the service log rather than dropped.
func TestAppliedReportCarriesNoDependencyDetail(t *testing.T) {
	const cause = "failed to connect to host=secret-db.internal user=postgres database=post: dial tcp 10.1.2.3:5432: connect: connection refused"
	leaky := errors.New(cause)

	var logged bytes.Buffer
	svc := newTestService(
		NewCatalogMust(testTemplate()),
		&fakeStore{err: leaky},
		&fakeProjects{},
		&fakeProfiles{failOn: map[string]error{"material_ext": leaky, "experiment_ext": leaky}},
		&fakePolicy{err: leaky},
		&fakeMap{failBranchWith: leaky},
	)
	svc.logger = slog.New(slog.NewTextHandler(&logged, nil))

	out, err := svc.Instantiate(context.Background(), actor(), InstantiateInput{TemplateID: "materials-discovery"})
	if err != nil {
		t.Fatalf("Instantiate failed (%v) — a failed default must not fake a failed creation", err)
	}

	reported := map[string]string{
		"recorded":    out.Applied.Recorded.Error,
		"profiles[0]": out.Applied.Profiles[0].Error,
		"profiles[1]": out.Applied.Profiles[1].Error,
		"policy":      out.Applied.Policy.Error,
		"map":         out.Applied.Map.Error,
	}
	for step, text := range reported {
		if text == "" {
			t.Errorf("%s: the failed default was not reported at all", step)
			continue
		}
		for _, leak := range []string{"secret-db", "postgres", "10.1.2.3", "5432", "connection refused", "host=", "database="} {
			if strings.Contains(text, leak) {
				t.Errorf("%s report entry %q carries the dependency's own text (%q) — the report is a wire boundary (docs/45)", step, text, leak)
			}
		}
	}
	if got := out.Applied.Profiles[0].Error; got != "the schema profile material_ext: the service was unavailable" {
		t.Errorf("profiles[0] = %q, want the step named with the unavailable class", got)
	}
	if got := out.Applied.Profiles[1].Error; got != "the schema profile experiment_ext: the service was unavailable" {
		t.Errorf("profiles[1] = %q, want the step named with the unavailable class", got)
	}
	if got := out.Applied.Policy.Error; got != "the review defaults: the service was unavailable" {
		t.Errorf("policy = %q, want the step named with the unavailable class", got)
	}
	if got := out.Applied.Recorded.Error; got != "the template origin record: the service was unavailable" {
		t.Errorf("recorded = %q, want the step named with the unavailable class", got)
	}
	if got := out.Applied.Map.Error; got != "the research map's main branch: the service was unavailable" {
		t.Errorf("map = %q, want the step named with the unavailable class", got)
	}

	// The cause is not thrown away: one log line per failed step, each
	// naming the step and the project (the detail the caller must not see).
	if n := strings.Count(logged.String(), cause); n != 5 {
		t.Errorf("the log carries the cause %d time(s), want one per failed step (5):\n%s", n, logged.String())
	}
	for _, step := range []string{
		"the template origin record", "the schema profile material_ext",
		"the schema profile experiment_ext", "the review defaults", "the research map's main branch",
	} {
		if !strings.Contains(logged.String(), step) {
			t.Errorf("the log does not name the step %q:\n%s", step, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "project_id=project-1") {
		t.Errorf("the log does not carry the project id:\n%s", logged.String())
	}
}

// TestGetInstantiationRunsProjectReadFirst: the record read is gated by
// the project's own visibility — a denied project read surfaces as
// ErrProjectNotFound (existence hiding), and the store is never reached.
func TestGetInstantiationRunsProjectReadFirst(t *testing.T) {
	store := &fakeStore{}
	svc := newTestService(NewCatalogMust(testTemplate()), store, &fakeProjects{}, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	rec, err := svc.GetInstantiation(context.Background(), projects.Reader{UserID: "user-1", Authenticated: true}, "project-1")
	if err != nil {
		t.Fatalf("GetInstantiation failed: %v", err)
	}
	if rec.TemplateID != "materials-discovery" || rec.TemplateVersion != "v1" {
		t.Errorf("record = %s@%s, want materials-discovery@v1", rec.TemplateID, rec.TemplateVersion)
	}
	if len(store.got) != 1 || store.got[0] != "project-1" {
		t.Errorf("store reads = %v, want one read of project-1", store.got)
	}

	denied := &fakeProjects{getErr: projects.ErrProjectNotFound}
	svc = newTestService(NewCatalogMust(testTemplate()), store, denied, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	if _, err := svc.GetInstantiation(context.Background(), projects.Reader{}, "project-1"); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("GetInstantiation = %v, want the existence-hiding ErrProjectNotFound", err)
	}
	if len(store.got) != 1 {
		t.Errorf("store reads grew to %d — the denied read must not reach the store", len(store.got))
	}

	svc = newTestService(NewCatalogMust(testTemplate()), &fakeStore{err: ErrInstantiationNotFound}, &fakeProjects{}, &fakeProfiles{}, &fakePolicy{}, &fakeMap{})
	if _, err := svc.GetInstantiation(context.Background(), projects.Reader{Authenticated: true}, "project-1"); !errors.Is(err, ErrInstantiationNotFound) {
		t.Fatalf("GetInstantiation = %v, want ErrInstantiationNotFound", err)
	}
}
