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

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/schemaprofileshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0213: Project Schema Extension 与 Custom Metadata — over a REAL
// PostgreSQL, the full extension loop: a project registers namespaced,
// versioned schema profiles extending the official base schemas, objects
// are written against them (the version row pins the exact profile ref),
// the progressive gates validate against the registered profile, and a
// profile v2 never invalidates history written under v1 (docs/21 §8). The
// base's metadata/conditions properties are the escape hatch: arbitrary
// custom metadata passes the strict profile at the pr gate without ever
// touching platform core.
//
// Required test: "schema extension integration" (this file).

const schemaProfileTaskID = "T0213"

// schemaProfileFixture seeds alice (owner), bob (no membership), carol
// (viewer) and one private project with a research branch, and wires the
// profile service, the rsg service and the validation service over the
// SAME schema registry and the SAME database — the exact composition
// cmd/api/main.go uses (profileSvc is the rsg service's Profiles resolver
// and the validator's registry is the registry profiles register into).
type schemaProfileFixture struct {
	svc       *rsg.Service
	profiles  *schemaprofiles.Service
	validate  *validation.Service
	reg       *schemareg.Registry
	pool      *pgxpool.Pool
	alice     domain.User
	bob       domain.User
	carol     domain.User
	project   domain.Project
	branch    string
	secondRef string
}

func newSchemaProfileFixture(t *testing.T, ctx context.Context) *schemaProfileFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), schemaProfileTaskID)
	cred := persistence.NewCredentialStore(pool)
	seed := func(email, handle, name string) domain.User {
		u, err := cred.CreateWithPassword(ctx, email, "hash", handle, name)
		if err != nil {
			t.Fatalf("seed user %s: %v", handle, err)
		}
		return u
	}
	alice := seed("profile-alice@example.com", "profile-alice", "Alice")
	bob := seed("profile-bob@example.com", "profile-bob", "Bob")
	carol := seed("profile-carol@example.com", "profile-carol", "Carol")

	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "profile-fixture", Name: "Profile Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "profile-project",
		Name:            "Profile Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}
	// carol is a viewer of the private project; bob holds no membership.
	if _, err := pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role)
		VALUES ($1, $2, 'viewer')`, project.ID, carol.ID); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	guard := validation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())
	stateStore := persistence.NewStateStore(pool)
	projectSvc := projects.NewService(projectStore, orgStore, authz.NewMatrixEngine())
	profileSvc := schemaprofiles.NewService(schemaprofiles.Deps{
		Store:    persistence.NewSchemaProfileStore(pool),
		Projects: projectSvc,
		Schemas:  reg,
	})
	svc := rsg.NewService(rsg.Deps{
		Projects:       projectSvc,
		Branches:       branches.NewService(persistence.NewBranchStore(pool)),
		States:         states.NewService(stateStore, guard),
		Latest:         stateStore,
		Objects:        persistence.NewScientificObjectStore(pool),
		Relations:      persistence.NewRelationStore(pool),
		SchemaProfiles: profileSvc,
		Authz:          authz.NewMatrixEngine(),
		Schemas:        reg,
	})
	validate := validation.NewService(
		persistence.NewValidationSnapshotRepository(stateStore),
		rsgvalidation.NewValidator(reg),
	)

	branch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}
	second, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name: "incomplete", BaseRef: "", Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create second branch: %v", err)
	}

	return &schemaProfileFixture{
		svc:       svc,
		profiles:  profileSvc,
		validate:  validate,
		reg:       reg,
		pool:      pool,
		alice:     alice,
		bob:       bob,
		carol:     carol,
		project:   project,
		branch:    branch.ID,
		secondRef: second.ID,
	}
}

// experimentBaseRef is the canonical experiment schema the profiles extend.
var experimentBaseRef = schemaprofiles.Ref{
	ID:      schemareg.CanonicalNamespace + "experiment.schema.json",
	Version: schemareg.CanonicalV1,
}

// registerExperimentProfile registers one version of the experiment
// profile through the service, returning the profile row.
func (f *schemaProfileFixture) registerExperimentProfile(t *testing.T, ctx context.Context, version string, custom map[string]any, required []string) domain.ProjectSchemaProfile {
	t.Helper()
	p, err := f.profiles.Register(ctx, f.alice, f.project.ID, schemaprofiles.RegisterInput{
		Name:       "experiment_ext",
		Version:    version,
		Base:       experimentBaseRef,
		Properties: custom,
		Required:   required,
	})
	if err != nil {
		t.Fatalf("register experiment_ext v%s: %v", version, err)
	}
	return p
}

// objectPayload builds an experiment payload satisfying the base schema's
// own required array (objective) plus the given content fields (empty for
// none).
func objectPayload(fields string) json.RawMessage {
	if fields == "" {
		return json.RawMessage(`{"objective":"N2 isotherm screening"}`)
	}
	return json.RawMessage(`{"objective":"N2 isotherm screening",` + fields + `}`)
}

// TestSchemaProfileExtensionIntegration is the required "schema extension
// integration" test: the full loop from profile registration to gate
// validation, over real PostgreSQL and the real registry.
func TestSchemaProfileExtensionIntegration(t *testing.T) {
	ctx := testCtx(t)
	f := newSchemaProfileFixture(t, ctx)
	schemaID := "project:" + f.project.ID + ":experiment_ext"

	// Phase 1: alice registers v1 of the experiment profile — one custom
	// typed field, required (acceptance: project-specific fields without
	// touching platform core). The row is persisted with its exact content
	// and hash, and the registry compiled it.
	v1 := f.registerExperimentProfile(t, ctx, "1",
		map[string]any{"catalyst_batch": map[string]any{"type": "string"}},
		[]string{"catalyst_batch"})
	if v1.SchemaID != schemaID || v1.BaseSchemaID != experimentBaseRef.ID {
		t.Fatalf("v1 row = %s on %s, want %s on the experiment base", v1.SchemaID, v1.BaseSchemaID, schemaID)
	}

	// Phase 2: an experiment object is created under the profile ref. The
	// version row pins the exact profile id+version — the database fact,
	// read back with SQL, not a service claim. The payload carries the
	// custom field plus ARBITRARY content under the base's metadata and
	// conditions properties — the escape hatch (custom metadata always has
	// a way in, even under the strict profile).
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "experiment",
		SchemaRef:  schemaID,
		Payload:    objectPayload(`"catalyst_batch":"B-7","metadata":{"lab":"L3","device":1},"conditions":{"temp_k":298,"custom_note":"any string"}`),
	})
	if err != nil {
		t.Fatalf("CreateObject under the profile: %v", err)
	}
	var pinnedSchemaID, pinnedSchemaVersion string
	if err := f.pool.QueryRow(ctx, `
		SELECT schema_id, schema_version FROM scientific_object_versions WHERE id = $1`,
		res.Version.ID).Scan(&pinnedSchemaID, &pinnedSchemaVersion); err != nil {
		t.Fatalf("read pinned schema ref: %v", err)
	}
	if pinnedSchemaID != schemaID || pinnedSchemaVersion != "1" {
		t.Fatalf("pinned ref = %s v%s, want %s v1", pinnedSchemaID, pinnedSchemaVersion, schemaID)
	}

	// Phase 3: the pr gate validates the branch — schema_typed is BLOCKING
	// there, so this proves the profile (custom required field + arbitrary
	// metadata through the hatch) survives the strictest schema gate.
	report, err := f.validate.ValidateBranch(ctx, f.project.ID, f.branch, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("ValidateBranch(pr): %v", err)
	}
	if report.Blocked() {
		t.Fatalf("pr gate blocked the profile object:\n%s", report.Explanation)
	}
	if hasBlocking(report, rsgvalidation.CheckSchemaTyped) {
		t.Fatalf("pr gate blocks on schema_typed although every object satisfies its profile")
	}

	// Phase 4: on a second branch, an object MISSING the required custom
	// field commits at draft (schema_typed only warns there) but the pr
	// gate blocks on schema_typed — the profile's custom requirement is
	// enforced by the gate ladder, not just by registration.
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.secondRef, rsg.CreateObjectInput{
		ObjectType: "experiment",
		SchemaRef:  schemaID,
		Payload:    objectPayload(``),
	}); err != nil {
		t.Fatalf("draft object without the custom field: %v", err)
	}
	report2, err := f.validate.ValidateBranch(ctx, f.project.ID, f.secondRef, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("ValidateBranch(pr) on the incomplete branch: %v", err)
	}
	if !report2.Blocked() || !hasBlocking(report2, rsgvalidation.CheckSchemaTyped) {
		t.Fatalf("incomplete branch verdict = %s (schema_typed blocking?), want blocked on schema_typed:\n%s",
			report2.Verdict, report2.Explanation)
	}
	// The explanation names the missing custom field — the refusal is
	// actionable, not opaque.
	if !strings.Contains(report2.Explanation, "catalyst_batch") {
		t.Errorf("the pr refusal does not name the missing custom field:\n%s", report2.Explanation)
	}

	// Phase 5: a spurious top-level field (never declared by the profile)
	// is refused at pr — the strict additionalProperties:false survived the
	// merge; the escape hatch is metadata/conditions, not arbitrary
	// top-level keys.
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.secondRef, rsg.CreateObjectInput{
		ObjectType: "experiment",
		SchemaRef:  schemaID,
		Payload:    objectPayload(`"catalyst_batch":"B-7","spurious_field":true`),
	}); err != nil {
		t.Fatalf("draft object with spurious field: %v", err)
	}
	report3, err := f.validate.ValidateBranch(ctx, f.project.ID, f.secondRef, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("ValidateBranch(pr) on the spurious branch: %v", err)
	}
	if !report3.Blocked() || !hasBlocking(report3, rsgvalidation.CheckSchemaTyped) {
		t.Fatalf("spurious-field branch verdict = %s, want blocked on schema_typed:\n%s",
			report3.Verdict, report3.Explanation)
	}

	// Phase 6: profile v2 lands (a new field, same name) — and the v1
	// history stays valid: the first branch still passes the pr gate
	// against the pinned v1, and the v1 row's stored content is untouched.
	f.registerExperimentProfile(t, ctx, "2",
		map[string]any{
			"catalyst_batch": map[string]any{"type": "string"},
			"solvent":        map[string]any{"type": "string"},
		},
		[]string{"catalyst_batch"})
	reportAfterV2, err := f.validate.ValidateBranch(ctx, f.project.ID, f.branch, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("ValidateBranch(pr) after v2: %v", err)
	}
	if reportAfterV2.Blocked() {
		t.Fatalf("v2 invalidated v1 history: the v1 branch no longer passes the pr gate:\n%s",
			reportAfterV2.Explanation)
	}
	// The registry holds both versions; the row still pins v1.
	if _, err := f.reg.Lookup(schemareg.Ref{ID: schemaID, Version: "1"}); err != nil {
		t.Errorf("v1 no longer registered: %v", err)
	}
	if _, err := f.reg.Lookup(schemareg.Ref{ID: schemaID, Version: "2"}); err != nil {
		t.Errorf("v2 not registered: %v", err)
	}
	var v1Rows int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM project_schema_profiles WHERE project_id = $1 AND schema_id = $2 AND version = '1'`,
		f.project.ID, schemaID).Scan(&v1Rows); err != nil {
		t.Fatalf("count v1 rows: %v", err)
	}
	if v1Rows != 1 {
		t.Errorf("v1 rows = %d, want 1 (append-only: v2 is a new row)", v1Rows)
	}

	// Phase 7: the schema-ref resolution refuses what a project may not
	// use — an unregistered id, another project's id, and a profile of a
	// different type — all with the same ErrValidation shape (docs/45: no
	// existence disclosure).
	foreignID := "project:99999999-9999-4999-8999-999999999999:experiment_ext"
	for _, ref := range []string{"project:" + f.project.ID + ":ghost", foreignID} {
		if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
			ObjectType: "experiment", SchemaRef: ref, Payload: objectPayload(`"catalyst_batch":"B-7"`),
		}); !errors.Is(err, rsg.ErrValidation) {
			t.Errorf("schema_ref %q: err = %v, want ErrValidation", ref, err)
		}
	}
	// A profile of hypotheses can never govern an experiment.
	hypProfile, err := f.profiles.Register(ctx, f.alice, f.project.ID, schemaprofiles.RegisterInput{
		Name: "hypothesis_ext", Version: "1",
		Base: schemaprofiles.Ref{ID: schemareg.CanonicalNamespace + "hypothesis.schema.json", Version: schemareg.CanonicalV1},
	})
	if err != nil {
		t.Fatalf("register hypothesis profile: %v", err)
	}
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "experiment", SchemaRef: hypProfile.SchemaID, Payload: objectPayload(`"catalyst_batch":"B-7"`),
	}); !errors.Is(err, rsg.ErrValidation) {
		t.Errorf("type-mismatched profile: err = %v, want ErrValidation", err)
	}
	// The canonical id itself stays usable (no regression).
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.branch, rsg.CreateObjectInput{
		ObjectType: "experiment", SchemaRef: experimentBaseRef.ID, Payload: objectPayload(``),
	}); err != nil {
		t.Errorf("canonical schema ref regressed: %v", err)
	}

	// Phase 8: the profile rows are append-only at the DATABASE — UPDATE
	// and DELETE are rejected by the guard trigger (P0001), and a duplicate
	// (project, schema_id, version) insert is rejected (23505).
	_, err = f.pool.Exec(ctx, `UPDATE project_schema_profiles SET version = '1' WHERE id = $1`, v1.ID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("UPDATE a profile row = %v, want SQLSTATE P0001 (append-only)", err)
	}
	_, err = f.pool.Exec(ctx, `DELETE FROM project_schema_profiles WHERE id = $1`, v1.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Errorf("DELETE a profile row = %v, want SQLSTATE P0001 (append-only)", err)
	}
	_, err = f.pool.Exec(ctx, `
		INSERT INTO project_schema_profiles
			(project_id, schema_id, version, base_schema_id, base_schema_version, content, content_hash, created_by)
		VALUES ($1, $2, '1', $3, $4, '{}', repeat('a', 64), $5)`,
		f.project.ID, schemaID, experimentBaseRef.ID, experimentBaseRef.Version, f.alice.ID)
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Errorf("duplicate profile insert = %v, want SQLSTATE 23505", err)
	}

	// Phase 9: restart convergence — a FRESH registry (a new process) plus
	// the persisted rows re-registers every profile via LoadAll, and the
	// whole gate ladder still resolves and validates the persisted
	// history against it (idempotent: loading twice is a no-op).
	freshReg, err := schemareg.New()
	if err != nil {
		t.Fatalf("fresh schemareg.New: %v", err)
	}
	freshProfiles := schemaprofiles.NewService(schemaprofiles.Deps{
		Store:    persistence.NewSchemaProfileStore(f.pool),
		Projects: projects.NewService(persistence.NewProjectStore(f.pool), persistence.NewOrgStore(f.pool), authz.NewMatrixEngine()),
		Schemas:  freshReg,
	})
	if err := freshProfiles.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll into the fresh registry: %v", err)
	}
	if err := freshProfiles.LoadAll(ctx); err != nil {
		t.Fatalf("second LoadAll: %v", err)
	}
	freshValidate := validation.NewService(
		persistence.NewValidationSnapshotRepository(persistence.NewStateStore(f.pool)),
		rsgvalidation.NewValidator(freshReg),
	)
	freshReport, err := freshValidate.ValidateBranch(ctx, f.project.ID, f.branch, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("fresh-registry ValidateBranch: %v", err)
	}
	if freshReport.Blocked() {
		t.Fatalf("after LoadAll the fresh registry blocks the persisted v1 history:\n%s",
			freshReport.Explanation)
	}
}

// TestSchemaProfileHTTPSurface drives the registration routes over the
// production HTTP stack (real guard, real stores, real registry): the
// maintainer-or-above gate answers the settings surface's stable outcomes
// — a viewer gets 403, a non-member of the private project gets the
// existence-hiding 404 — and the wire codes stay stable (201 create, 409
// immutable version, 400 malformed, reads exactly as visible as the
// project).
func TestSchemaProfileHTTPSurface(t *testing.T) {
	ctx := testCtx(t)
	f := newSchemaProfileFixture(t, ctx)

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
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(f.pool),
		Orgs:  persistence.NewOrgStore(f.pool),
		Authz: authz.NewMatrixEngine(),
	})
	profilesAPI := schemaprofileshttp.New(schemaprofileshttp.Deps{Service: f.profiles})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	profilesAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	// Real accounts bound to the fixture's project via memberships: alice
	// the owner, carol a viewer, bob a stranger to the private project.
	// (The emails differ from the store-seeded fixture users — those rows
	// already exist, so these accounts must sign up over the wire.)
	alice, aliceID := signup(t, ts.URL, "schema-http-alice@example.com", "schema-http-alice")
	carol, carolID := signup(t, ts.URL, "schema-http-carol@example.com", "schema-http-carol")
	bob, _ := signup(t, ts.URL, "schema-http-bob@example.com", "schema-http-bob")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		parseUUIDOrDie(f.project.ID), parseUUIDOrDie(aliceID)); err != nil {
		t.Fatalf("seed owner membership: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'viewer')`,
		parseUUIDOrDie(f.project.ID), parseUUIDOrDie(carolID)); err != nil {
		t.Fatalf("seed viewer membership: %v", err)
	}

	path := "/api/v1/projects/" + f.project.ID + "/schema-profiles"
	registerBody := `{"name":"experiment_ext","version":"1",` +
		`"base":{"id":"https://open-rd.example/schemas/experiment.schema.json","version":"1"},` +
		`"properties":{"catalyst_batch":{"type":"string"}},"required":["catalyst_batch"]}`

	// The owner registers: 201, and the payload carries the derived schema
	// id and the exact generated content.
	resp := alice.do(t, http.MethodPost, path, registerBody)
	mustStatus(t, resp, http.StatusCreated)
	var created struct {
		SchemaRef struct {
			ID      string `json:"id"`
			Version string `json:"version"`
		} `json:"schema_ref"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if created.SchemaRef.ID != "project:"+f.project.ID+":experiment_ext" || created.SchemaRef.Version != "1" {
		t.Errorf("registered ref = %+v, want the derived project id at v1", created.SchemaRef)
	}
	var doc map[string]any
	if err := json.Unmarshal(created.Content, &doc); err != nil {
		t.Fatalf("profile content is not JSON: %v", err)
	}
	if doc["additionalProperties"] != false {
		t.Errorf("profile content lost the strict additionalProperties: %v", doc["additionalProperties"])
	}

	// The same name+version again: 409 — versions are immutable.
	resp = alice.do(t, http.MethodPost, path, registerBody)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, schemaprofiles.CodeProfileVersionExists)

	// The viewer and the stranger are both refused: the viewer with the
	// flat 403, the non-member with the existence-hiding 404 (the private
	// project's stable outcomes, docs/45).
	resp = carol.do(t, http.MethodPost, path, registerBody)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, schemaprofiles.CodeForbidden)
	resp = bob.do(t, http.MethodPost, path, registerBody)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, projects.CodeProjectNotFound)

	// A malformed body answers 400 with the validation code.
	resp = alice.do(t, http.MethodPost, path, `{"name":"bad_name"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, schemaprofiles.CodeValidation)

	// Reads are exactly as visible as the project: the owner lists and
	// reads (newest + pinned version), the stranger gets the 404.
	resp = alice.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusOK)
	var list struct {
		Profiles []struct {
			SchemaRef struct {
				Version string `json:"version"`
			} `json:"schema_ref"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if len(list.Profiles) != 1 || list.Profiles[0].SchemaRef.Version != "1" {
		t.Errorf("list = %+v, want the one v1 profile", list.Profiles)
	}
	resp = alice.do(t, http.MethodGet, path+"/experiment_ext", "")
	mustStatus(t, resp, http.StatusOK)
	resp = alice.do(t, http.MethodGet, path+"/experiment_ext/versions/1", "")
	mustStatus(t, resp, http.StatusOK)
	resp = alice.do(t, http.MethodGet, path+"/experiment_ext/versions/9", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, schemaprofiles.CodeProfileNotFound)
	resp = bob.do(t, http.MethodGet, path, "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, projects.CodeProjectNotFound)
}
