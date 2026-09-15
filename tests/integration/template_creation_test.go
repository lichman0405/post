package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/templateshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/templates"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0214: 官方材料研发 Project Templates — over a REAL PostgreSQL, the
// full create-from-template loop: a user instantiates an official template
// (Materials Discovery), the project is created with the template's
// defaults (purpose/visibility presets, schema profiles, the first policy
// version, the initial research-map questions) applied ONCE through the
// owning services, and the template id + version are recorded in the
// append-only provenance table. The project then evolves independently
// (new profile versions, new policy versions, more objects) — and a
// template UPGRADE (a new catalog version) only affects FUTURE
// instantiations, never existing projects.
//
// Required test: "template creation e2e" (this file).

const templateTaskID = "T0214"

// templateFixture seeds alice (template consumer) and bob (a stranger) and
// wires the templates service over the SAME production graph cmd/api uses:
// the projects/schemaprofiles/policy/rsg services over one shared
// PostgreSQL, with the official catalog.
type templateFixture struct {
	tmpl     *templates.Service
	projects *projects.Service
	profiles *schemaprofiles.Service
	policy   *policy.Service
	rsg      *rsg.Service
	pool     *pgxpool.Pool
	alice    domain.User
	bob      domain.User
}

func newTemplateFixture(t *testing.T, ctx context.Context, catalog *templates.Catalog) *templateFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), templateTaskID)
	cred := persistence.NewCredentialStore(pool)
	seed := func(email, handle, name string) domain.User {
		u, err := cred.CreateWithPassword(ctx, email, "hash", handle, name)
		if err != nil {
			t.Fatalf("seed user %s: %v", handle, err)
		}
		return u
	}
	alice := seed("template-alice@example.com", "template-alice", "Alice")
	bob := seed("template-bob@example.com", "template-bob", "Bob")

	tmpl, projectSvc, profileSvc, policySvc, rsgSvc := buildTemplateService(t, ctx, pool, catalog)
	return &templateFixture{
		tmpl:     tmpl,
		projects: projectSvc,
		profiles: profileSvc,
		policy:   policySvc,
		rsg:      rsgSvc,
		pool:     pool,
		alice:    alice,
		bob:      bob,
	}
}

// buildTemplateService wires the templates service over the SAME
// production graph cmd/api uses, on one pool. Reusable: a template
// UPGRADE is a new service instance (new catalog) over the SAME database.
func buildTemplateService(t *testing.T, ctx context.Context, pool *pgxpool.Pool, catalog *templates.Catalog) (*templates.Service, *projects.Service, *schemaprofiles.Service, *policy.Service, *rsg.Service) {
	t.Helper()
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	profileSvc := schemaprofiles.NewService(schemaprofiles.Deps{
		Store:    persistence.NewSchemaProfileStore(pool),
		Projects: projectSvc,
		Schemas:  reg,
	})
	guard := validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())
	stateStore := persistence.NewStateStore(pool)
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:       projectSvc,
		Branches:       branches.NewService(persistence.NewBranchStore(pool)),
		States:         states.NewService(stateStore, guard),
		Latest:         stateStore,
		Objects:        persistence.NewScientificObjectStore(pool),
		Relations:      persistence.NewRelationStore(pool),
		SchemaProfiles: profileSvc,
		Authz:          authz.NewMatrixEngine(),
		Schemas:        reg,
		// The transactional outbox (T1001): the RSG service fails closed
		// without its recorder, and production wires the same value — a
		// fixture missing it would test a graph cmd/api never builds.
		Events: events.Recorder{},
	})
	policySvc := policy.NewService(persistence.NewPolicyStore(pool), orgStore, projectStore, nil)
	if catalog == nil {
		catalog = templates.Official()
	}
	tmplSvc := templates.NewService(templates.Deps{
		Catalog:       catalog,
		Store:         persistence.NewTemplateStore(pool),
		Projects:      projectSvc,
		ProjectReader: projectSvc,
		Profiles:      profileSvc,
		Policy:        policySvc,
		MapSeeds:      rsgSvc,
	})
	return tmplSvc, projectSvc, profileSvc, policySvc, rsgSvc
}

// upgradedCatalog builds a v2 of materials-discovery (new defaults) on top
// of the official catalog: the v2 entry is declared newest, v1 stays
// pinnable — the exact upgrade shape the production catalog uses.
func upgradedCatalog() *templates.Catalog {
	base := templates.Official()
	v1, err := base.Resolve("materials-discovery", "v1")
	if err != nil {
		panic("official catalog lost materials-discovery v1: " + err.Error())
	}
	v2 := v1
	v2.Version = "v2"
	v2.Description = "The upgraded Materials Discovery template."
	v2.Project.Purpose = "Upgraded purpose: screen, synthesize and validate with the new workflow."
	v2.Review.Rules[domain.RuleReleaseMinReviewers] = json.RawMessage(`2`)
	v2.Map = []domain.TemplateQuestion{
		{Statement: "Which candidates survive the upgraded screen?", Purpose: "upgrade"},
	}
	entries := []domain.ProjectTemplate{v2, v1}
	for _, e := range base.LatestIDs() {
		if e.ID != "materials-discovery" {
			entries = append(entries, e)
		}
	}
	return templates.NewCatalogMust(entries...)
}

// instantiationFacts is the project's template-origin database state, read
// back with SQL (never a service claim).
//
// The fields are EXPORTED on purpose. The upgrade guard below compares two
// readings of one project by rendering them (fmtFacts), and encoding/json
// silently drops unexported fields — a facts struct with no exported field
// renders as "{}" for EVERY reading, which would make that comparison true
// no matter what an upgrade did. Exported fields keep the rendering equal to
// the facts (staticcheck's SA9005 flags the unexported shape for the same
// reason).
type instantiationFacts struct {
	RecordTemplateID      string
	RecordTemplateVersion string
	RecordTemplateName    string
	RecordCreatedBy       string
	Name                  string // projects.name — a template default too
	Purpose               string
	Visibility            string
	Profiles              []string // "schema_id@version@base_schema_id"
	PolicyVersions        []string // "version@policy_json"
	Branches              []string // "name@visibility"
	Questions             []string // "statement@parent"
}

func readInstantiationFacts(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) instantiationFacts {
	t.Helper()
	var f instantiationFacts
	if err := pool.QueryRow(ctx, `
		SELECT template_id, template_version, template_name, created_by::text
		FROM project_template_instantiations WHERE project_id = $1`, projectID).
		Scan(&f.RecordTemplateID, &f.RecordTemplateVersion, &f.RecordTemplateName, &f.RecordCreatedBy); err != nil {
		t.Fatalf("read template instantiation: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT name, purpose, visibility FROM projects WHERE id = $1`, projectID).
		Scan(&f.Name, &f.Purpose, &f.Visibility); err != nil {
		t.Fatalf("read project: %v", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT schema_id, version, base_schema_id FROM project_schema_profiles
		WHERE project_id = $1 ORDER BY created_at, schema_id`, projectID)
	if err != nil {
		t.Fatalf("read profiles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var schemaID, version, base string
		if err := rows.Scan(&schemaID, &version, &base); err != nil {
			t.Fatalf("scan profile: %v", err)
		}
		f.Profiles = append(f.Profiles, schemaID+"@"+version+"@"+base)
	}
	rows.Close()
	rows, err = pool.Query(ctx, `
		SELECT version, policy_json::text FROM policy_versions
		WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		t.Fatalf("read policy versions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var version, doc string
		if err := rows.Scan(&version, &doc); err != nil {
			t.Fatalf("scan policy version: %v", err)
		}
		f.PolicyVersions = append(f.PolicyVersions, version+"@"+doc)
	}
	rows.Close()
	rows, err = pool.Query(ctx, `
		SELECT name, visibility FROM branches WHERE project_id = $1 ORDER BY created_at`, projectID)
	if err != nil {
		t.Fatalf("read branches: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, visibility string
		if err := rows.Scan(&name, &visibility); err != nil {
			t.Fatalf("scan branch: %v", err)
		}
		f.Branches = append(f.Branches, name+"@"+visibility)
	}
	rows.Close()
	rows, err = pool.Query(ctx, `
		SELECT ov.payload->>'statement', ov.payload->>'parent_question_id'
		FROM scientific_object_versions ov
		JOIN scientific_objects o ON o.id = ov.object_id
		WHERE o.project_id = $1 AND o.object_type = 'research_question'
		ORDER BY ov.created_at`, projectID)
	if err != nil {
		t.Fatalf("read seeded questions: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var statement string
		var parent *string
		if err := rows.Scan(&statement, &parent); err != nil {
			t.Fatalf("scan question: %v", err)
		}
		parentText := ""
		if parent != nil {
			parentText = *parent
		}
		f.Questions = append(f.Questions, statement+"@"+parentText)
	}
	rows.Close()
	return f
}

// TestTemplateCreationE2E is the required "template creation e2e" test.
func TestTemplateCreationE2E(t *testing.T) {
	ctx := testCtx(t)
	f := newTemplateFixture(t, ctx, nil)

	// Phase 1: alice creates a project from the official Materials
	// Discovery template, overriding only the slug — every other default
	// comes from the template, applied through the owning services.
	out, err := f.tmpl.Instantiate(ctx, f.alice, templates.InstantiateInput{
		TemplateID: "materials-discovery",
		Slug:       "my-anode-lab",
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	p1 := out.Project
	if p1.Slug != "my-anode-lab" || p1.Name != "Materials Discovery" ||
		p1.Visibility != domain.VisibilityPrivate || p1.Purpose == "" {
		t.Fatalf("project = %+v, want the caller's slug with the template's defaults", p1)
	}
	if out.Membership.Role != domain.ProjectRoleOwner || out.Membership.UserID != f.alice.ID {
		t.Fatalf("membership = %+v, want alice as owner", out.Membership)
	}
	// The provenance record carries template id + version + name.
	if out.Instantiation.TemplateID != "materials-discovery" || out.Instantiation.TemplateVersion != "v1" ||
		out.Instantiation.TemplateName != "Materials Discovery" || out.Instantiation.ProjectID != p1.ID {
		t.Fatalf("instantiation record = %+v, want materials-discovery@v1 on %s", out.Instantiation, p1.ID)
	}
	// The application report shows every step applied.
	if out.Applied.Recorded == nil || out.Applied.Recorded.Error != "" {
		t.Fatalf("recorded step = %+v, want applied", out.Applied.Recorded)
	}
	if len(out.Applied.Profiles) != 2 {
		t.Fatalf("applied profiles = %d, want 2 (material_ext, experiment_ext)", len(out.Applied.Profiles))
	}
	for _, p := range out.Applied.Profiles {
		if p.Error != "" || p.Version != "1" {
			t.Errorf("profile %s = v%s err=%q, want applied at v1", p.SchemaID, p.Version, p.Error)
		}
	}
	if out.Applied.Policy == nil || out.Applied.Policy.Error != "" {
		t.Fatalf("policy step = %+v, want applied", out.Applied.Policy)
	}
	if out.Applied.Map == nil || out.Applied.Map.Error != "" {
		t.Fatalf("map step = %+v, want applied", out.Applied.Map)
	}
	if len(out.Applied.Map.Questions) != 4 {
		t.Fatalf("seeded questions = %d, want 4 (two roots + two children)", len(out.Applied.Map.Questions))
	}

	// Phase 2: the DATABASE facts, read back with SQL — the template's
	// defaults landed as ordinary project-owned rows, and the origin is
	// recorded.
	facts := readInstantiationFacts(t, ctx, f.pool, p1.ID)
	if facts.RecordTemplateID != "materials-discovery" || facts.RecordTemplateVersion != "v1" ||
		facts.RecordTemplateName != "Materials Discovery" || facts.RecordCreatedBy != f.alice.ID {
		t.Fatalf("record row = %+v, want materials-discovery@v1 by alice", facts)
	}
	if facts.Name != "Materials Discovery" || facts.Purpose == "" || facts.Visibility != "private" {
		t.Fatalf("project row = name %q purpose %q visibility %s, want the template defaults", facts.Name, facts.Purpose, facts.Visibility)
	}
	wantProfiles := []string{
		"project:" + p1.ID + ":material_ext@1@https://open-rd.example/schemas/material.schema.json",
		"project:" + p1.ID + ":experiment_ext@1@https://open-rd.example/schemas/experiment.schema.json",
	}
	if len(facts.Profiles) != 2 || facts.Profiles[0] != wantProfiles[0] || facts.Profiles[1] != wantProfiles[1] {
		t.Fatalf("profile rows = %v, want %v", facts.Profiles, wantProfiles)
	}
	if len(facts.PolicyVersions) != 1 || !strings.HasPrefix(facts.PolicyVersions[0], "v1@") {
		t.Fatalf("policy rows = %v, want one v1 row", facts.PolicyVersions)
	}
	var policyRules map[string]any
	if err := json.Unmarshal([]byte(facts.PolicyVersions[0][len("v1@"):]), &policyRules); err != nil {
		t.Fatalf("policy doc is not a JSON rule map: %v", err)
	}
	if policyRules["main_protected"] != true || policyRules["release_min_reviewers"] != float64(1) ||
		policyRules["public_asset_ip_review"] != true {
		t.Errorf("policy rules = %v, want the template's review defaults", policyRules)
	}
	if _, ok := policyRules["required_schema_profiles"]; ok {
		t.Errorf("the template set required_schema_profiles (%v) — a control rule a template must never set", policyRules)
	}
	if len(facts.Branches) != 1 || facts.Branches[0] != "main@private" {
		t.Fatalf("branches = %v, want one main branch at the project's private preset", facts.Branches)
	}
	if len(facts.Questions) != 4 {
		t.Fatalf("seeded question rows = %d, want 4", len(facts.Questions))
	}
	roots, children := 0, 0
	for _, q := range facts.Questions {
		if strings.HasSuffix(q, "@") {
			roots++
		} else {
			children++
		}
	}
	if roots != 2 || children != 2 {
		t.Fatalf("question tree = %v, want 2 roots and 2 children with parent links", facts.Questions)
	}

	// Phase 3: independent evolution (创建后可独立演化) — the owner goes
	// past the template through the NORMAL surfaces: a profile v2, a new
	// policy version, and a new object on main. The template's rows stay
	// version-1 rows; nothing re-applies.
	if _, err := f.profiles.Register(ctx, f.alice, p1.ID, schemaprofiles.RegisterInput{
		Name: "material_ext", Version: "2",
		Base: schemaprofiles.Ref{ID: schemareg.CanonicalNamespace + "material.schema.json", Version: schemareg.CanonicalV1},
		Properties: map[string]any{
			"synthesis_route":      map[string]any{"type": "string"},
			"target_application":   map[string]any{"type": "string"},
			"key_property_targets": map[string]any{"type": "object", "additionalProperties": true},
			"doping_strategy":      map[string]any{"type": "string"},
		},
		Required: []string{"target_application"},
	}); err != nil {
		t.Fatalf("register material_ext v2: %v", err)
	}
	if _, err := f.policy.SetProjectPolicy(ctx, f.alice, p1.ID, "v2",
		json.RawMessage(`{"release_min_reviewers":2}`)); err != nil {
		t.Fatalf("set policy v2: %v", err)
	}
	if _, err := f.rsg.CreateObject(ctx, f.alice, p1.ID, out.Applied.Map.BranchID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(`{"statement":"A question of the owner's own making?","question_state":"open"}`),
	}); err != nil {
		t.Fatalf("owner object after instantiation: %v", err)
	}
	facts = readInstantiationFacts(t, ctx, f.pool, p1.ID)
	if len(facts.Profiles) != 3 {
		t.Errorf("profiles after evolution = %d, want 3 (template v1s + the owner's v2)", len(facts.Profiles))
	}
	if len(facts.PolicyVersions) != 2 || !strings.HasPrefix(facts.PolicyVersions[1], "v2@") {
		t.Errorf("policy versions after evolution = %v, want the owner's v2 newest", facts.PolicyVersions)
	}
	if len(facts.Questions) != 5 {
		t.Errorf("questions after evolution = %d, want 5 (4 seeds + the owner's own)", len(facts.Questions))
	}
	// The origin record is untouched by all of it.
	if facts.RecordTemplateID != "materials-discovery" || facts.RecordTemplateVersion != "v1" {
		t.Errorf("record after evolution = %s@%s, want the untouched v1 origin", facts.RecordTemplateID, facts.RecordTemplateVersion)
	}

	// Phase 4: a template UPGRADE (v2 catalog) only affects FUTURE
	// instantiations. The existing project keeps its v1 defaults and its
	// v1 origin record; a pinned v1 instantiation keeps v1; an unpinned
	// instantiation gets v2. The upgrade is a new service instance over
	// the SAME database — exactly what a redeploy does.
	upgradedSvc, _, _, _, _ := buildTemplateService(t, ctx, f.pool, upgradedCatalog())
	beforeUpgrade := readInstantiationFacts(t, ctx, f.pool, p1.ID)

	pinned, err := upgradedSvc.Instantiate(ctx, f.alice, templates.InstantiateInput{
		TemplateID: "materials-discovery", TemplateVersion: "v1", Slug: "pinned-v1-lab",
	})
	if err != nil {
		t.Fatalf("pinned v1 Instantiate: %v", err)
	}
	if pinned.Template.Version != "v1" || pinned.Instantiation.TemplateVersion != "v1" {
		t.Fatalf("pinned instantiation resolved %s and recorded %s, want v1",
			pinned.Template.Version, pinned.Instantiation.TemplateVersion)
	}
	if pinned.Project.Purpose == v2Purpose() {
		t.Fatalf("pinned v1 project got the upgraded purpose — a pinned version must keep its own defaults")
	}
	if len(pinned.Applied.Map.Questions) != 4 {
		t.Fatalf("pinned v1 seeded %d questions, want the v1 tree (4)", len(pinned.Applied.Map.Questions))
	}

	latest, err := upgradedSvc.Instantiate(ctx, f.alice, templates.InstantiateInput{
		TemplateID: "materials-discovery", Slug: "latest-lab",
	})
	if err != nil {
		t.Fatalf("latest Instantiate: %v", err)
	}
	if latest.Template.Version != "v2" || latest.Instantiation.TemplateVersion != "v2" {
		t.Fatalf("latest instantiation resolved %s and recorded %s, want v2",
			latest.Template.Version, latest.Instantiation.TemplateVersion)
	}
	if latest.Project.Purpose != v2Purpose() {
		t.Fatalf("latest project purpose = %q, want the upgraded v2 default", latest.Project.Purpose)
	}
	if len(latest.Applied.Map.Questions) != 1 {
		t.Fatalf("latest seeded %d questions, want the v2 tree (1)", len(latest.Applied.Map.Questions))
	}

	afterUpgrade := readInstantiationFacts(t, ctx, f.pool, p1.ID)
	beforeText, afterText := fmtFacts(beforeUpgrade), fmtFacts(afterUpgrade)
	// The comparison below IS the guard for acceptance criterion 3 (模板升级
	// 不会自动改已有项目), and it only guards while the rendering it compares
	// is the facts: a rendering that lost them ("{}" on both sides) would
	// stay green through any upgrade. Assert the rendering really carries
	// p1's rows before trusting the equality — including the project NAME,
	// which the template also defaults and which a re-applying upgrade would
	// rewrite.
	if !strings.Contains(beforeText, `"RecordTemplateID"`) || !strings.Contains(beforeText, `"Questions"`) ||
		beforeUpgrade.Name == "" || beforeUpgrade.Purpose == "" || len(beforeUpgrade.Questions) == 0 {
		t.Fatalf("the upgrade comparison is vacuous — the rendering carries no facts (%d bytes): %s",
			len(beforeText), beforeText)
	}
	if beforeText != afterText {
		t.Fatalf("the template upgrade CHANGED the existing project:\nbefore: %s\nafter:  %s", beforeText, afterText)
	}

	// Phase 5: failure semantics — an unknown template creates nothing; a
	// project without a template origin reads as not-found; the record
	// read is exactly as visible as the project (bob, a stranger to the
	// private project, gets the existence-hiding 404; an unknown project
	// answers the same).
	_, err = f.tmpl.Instantiate(ctx, f.alice, templates.InstantiateInput{TemplateID: "nope", Slug: "ghost"})
	if !errors.Is(err, templates.ErrTemplateNotFound) {
		t.Fatalf("unknown template = %v, want ErrTemplateNotFound", err)
	}
	// The template defaults the name, the purpose and the visibility — the
	// SLUG is the caller's: no template carries one (TemplateProjectDefaults
	// holds those three only), so a create-from-template without it fails
	// the projects service's own slug validation, exactly as a plain create
	// would. This is the InstantiateInput contract the doc states.
	if _, err := f.tmpl.Instantiate(ctx, f.alice, templates.InstantiateInput{TemplateID: "materials-discovery"}); !errors.Is(err, projects.ErrValidation) {
		t.Fatalf("slug-less instantiate = %v, want the projects service's ErrValidation (the template cannot pick the project's path)", err)
	}
	plain, _, err := f.projects.Create(ctx, f.alice, projects.CreateProjectInput{
		Slug: "no-template-lab", Name: "No Template Lab", Purpose: "Plain project.",
		Visibility: domain.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("plain project create: %v", err)
	}
	if _, err := f.tmpl.GetInstantiation(ctx, projects.Reader{UserID: f.alice.ID, Authenticated: true}, plain.ID); !errors.Is(err, templates.ErrInstantiationNotFound) {
		t.Fatalf("record for a template-less project = %v, want ErrInstantiationNotFound", err)
	}
	if _, err := f.tmpl.GetInstantiation(ctx, projects.Reader{UserID: f.alice.ID, Authenticated: true}, "99999999-9999-4999-8999-999999999999"); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("record for an unknown project = %v, want the existence-hiding ErrProjectNotFound", err)
	}
	if _, err := f.tmpl.GetInstantiation(ctx, projects.Reader{UserID: f.bob.ID, Authenticated: true}, p1.ID); !errors.Is(err, projects.ErrProjectNotFound) {
		t.Fatalf("bob reading the record = %v, want the existence-hiding ErrProjectNotFound", err)
	}
}

// v2Purpose is the upgraded template's purpose (shared by the phase-4
// assertions).
func v2Purpose() string {
	return "Upgraded purpose: screen, synthesize and validate with the new workflow."
}

// fmtFacts renders the fact set for the upgrade no-op comparison. The
// rendering is that comparison's only input, so it has to carry the facts:
// see the exported-fields note on instantiationFacts, and the non-vacuous
// assertion at the call site.
func fmtFacts(f instantiationFacts) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// TestTemplateCreationHTTPSurface drives the template routes over the
// production HTTP stack (real guard, real stores, real services): the
// catalog reads flow unauthenticated, the instantiate write is session +
// CSRF guarded, the 201 body answers the project + record + applied
// report, and the record read is exactly as visible as the project.
func TestTemplateCreationHTTPSurface(t *testing.T) {
	ctx := testCtx(t)
	f := newTemplateFixture(t, ctx, nil)

	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(f.pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg:      cfg,
		Secure:   false,
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	templateshttp.New(templateshttp.Deps{Service: f.tmpl}).Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	// The catalog read flows unauthenticated and lists the six official
	// templates.
	resp, err := http.Get(ts.URL + "/api/v1/templates")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	var list struct {
		Templates []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"templates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(list.Templates) != 6 {
		t.Fatalf("catalog = %d templates, want 6", len(list.Templates))
	}
	wantIDs := map[string]bool{
		"materials-discovery": true, "computational-screening": true,
		"experimental-validation": true, "paper-reproduction": true,
		"dataset-construction": true, "benchmarking": true,
	}
	for _, e := range list.Templates {
		if !wantIDs[e.ID] {
			t.Errorf("catalog carries unexpected id %q", e.ID)
		}
		if e.Version != "v1" {
			t.Errorf("%s version = %s, want v1", e.ID, e.Version)
		}
	}

	// One template, pinned by version; an unknown version answers 404.
	resp, err = http.Get(ts.URL + "/api/v1/templates/materials-discovery?version=v1")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	var one struct {
		ID      string `json:"id"`
		Version string `json:"version"`
		Map     []struct {
			Statement string `json:"statement"`
		} `json:"map"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&one); err != nil {
		t.Fatalf("decode template: %v", err)
	}
	if one.ID != "materials-discovery" || one.Version != "v1" || len(one.Map) != 2 {
		t.Fatalf("template = %s@%s with %d map seeds, want materials-discovery@v1 with 2", one.ID, one.Version, len(one.Map))
	}
	resp, err = http.Get(ts.URL + "/api/v1/templates/materials-discovery?version=nope")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, templates.CodeTemplateNotFound)

	// The guarded write: alice instantiates over the wire with her CSRF
	// token; the 201 body answers the project, the record and the report.
	alice, aliceID := signup(t, ts.URL, "template-http-alice@example.com", "template-http-alice")
	resp = alice.do(t, http.MethodPost, "/api/v1/templates/materials-discovery/instantiate",
		`{"slug":"wire-anode-lab","name":"Wire Anode Lab"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created struct {
		Template struct {
			ID string `json:"id"`
		} `json:"template"`
		Project struct {
			ID         string `json:"id"`
			Name       string `json:"name"`
			Purpose    string `json:"purpose"`
			Visibility string `json:"visibility"`
		} `json:"project"`
		Record struct {
			TemplateID      string `json:"template_id"`
			TemplateVersion string `json:"template_version"`
			ProjectID       string `json:"project_id"`
		} `json:"record"`
		Applied struct {
			Recorded map[string]any `json:"recorded"`
			Profiles []struct {
				Error string `json:"error"`
			} `json:"profiles"`
			Policy map[string]any `json:"policy"`
			Map    struct {
				Error     string `json:"error"`
				Questions []any  `json:"questions"`
			} `json:"map"`
		} `json:"applied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode instantiate response: %v", err)
	}
	if created.Template.ID != "materials-discovery" {
		t.Fatalf("template = %s, want materials-discovery", created.Template.ID)
	}
	if created.Project.Name != "Wire Anode Lab" || created.Project.Visibility != "private" || created.Project.Purpose == "" {
		t.Fatalf("project = %+v, want the caller's name with the template's defaults", created.Project)
	}
	if created.Record.TemplateID != "materials-discovery" || created.Record.TemplateVersion != "v1" ||
		created.Record.ProjectID != created.Project.ID {
		t.Fatalf("record = %+v, want materials-discovery@v1 recorded on the new project", created.Record)
	}
	if errVal, ok := created.Applied.Recorded["error"]; ok && errVal != "" {
		t.Errorf("recorded step = %v, want applied", created.Applied.Recorded)
	}
	if len(created.Applied.Profiles) != 2 {
		t.Errorf("profiles = %d, want 2", len(created.Applied.Profiles))
	}
	if errVal, ok := created.Applied.Policy["error"]; ok && errVal != "" {
		t.Errorf("policy step = %v, want applied", created.Applied.Policy)
	}
	if created.Applied.Map.Error != "" || len(created.Applied.Map.Questions) != 4 {
		t.Errorf("map step = %v, want 4 seeded questions applied", created.Applied.Map)
	}

	// The wire-created project's record row is a database fact.
	facts := readInstantiationFacts(t, ctx, f.pool, created.Project.ID)
	if facts.RecordTemplateID != "materials-discovery" || facts.RecordTemplateVersion != "v1" ||
		facts.RecordCreatedBy != aliceID {
		t.Fatalf("record row = %+v, want materials-discovery@v1 created by the wire actor %s", facts, aliceID)
	}

	// The record read is exactly as visible as the project: the owner
	// reads it; a stranger to the private project gets the
	// existence-hiding 404.
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+created.Project.ID+"/template-instantiation", "")
	mustStatus(t, resp, http.StatusOK)
	var record struct {
		TemplateID      string `json:"template_id"`
		TemplateVersion string `json:"template_version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if record.TemplateID != "materials-discovery" || record.TemplateVersion != "v1" {
		t.Fatalf("record read = %s@%s, want materials-discovery@v1", record.TemplateID, record.TemplateVersion)
	}
	bob, _ := signup(t, ts.URL, "template-http-bob@example.com", "template-http-bob")
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+created.Project.ID+"/template-instantiation", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, projects.CodeProjectNotFound)

	// The write guard is in force on the template path: an anonymous write
	// answers 401 before routing, a wrong CSRF token answers 403.
	resp, err = http.Post(ts.URL+"/api/v1/templates/materials-discovery/instantiate",
		"application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusUnauthorized)
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/api/v1/templates/materials-discovery/instantiate", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", "wrong-token")
	resp, err = alice.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusForbidden)

	// An unknown template answers the stable 404 envelope.
	resp = alice.do(t, http.MethodPost, "/api/v1/templates/ghost/instantiate", `{"slug":"ghost-lab"}`)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, templates.CodeTemplateNotFound)
}
