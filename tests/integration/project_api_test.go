// Package integration — T0104-TEST-01 "project api" (blocking): the
// project-creation shell exercised end to end against REAL PostgreSQL —
// real migrations, real pgx stores, the real auth guard (session + CSRF)
// and the real projectshttp handlers, exactly as cmd/api/main.go composes
// them (plus the orgshttp surface, since creating an org project requires
// an active organization membership). Only the session store is
// in-memory (Redis semantics are orthogonal to project creation and
// already covered by the auth e2e suite).
//
// The task requirements each map to a phase of the test:
//
//   - "Public/Private preset"     → visibility round-trips for public and
//     private projects; anything else is refused with VALIDATION_FAILED.
//   - "purpose 与可选 program"     → purpose round-trips (and is
//     required); program is optional, validated against the project's
//     organization.
//   - "创建者成为 owner"          → the create response carries the
//     creator's owner membership.
//   - "provision pending 状态"    → the response reports provision_status
//     "pending" and the same state is verified with SQL against the
//     database.
//
// The acceptance criteria:
//
//   - "Project 创建成功且 visibility 正确" → create succeeds over the wire
//     and the visibility preset is exactly what was requested.
//   - "slug conflict 清晰"        → re-using a slug answers 409 with the
//     stable PROJECT_SLUG_TAKEN code, for org projects and personal
//     projects alike.
package integration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const projectTaskID = "T0104"

type projectResponse struct {
	Project struct {
		ID                      string  `json:"id"`
		OrganizationID          *string `json:"organization_id"`
		ProgramID               *string `json:"program_id"`
		Slug                    string  `json:"slug"`
		Name                    string  `json:"name"`
		Purpose                 string  `json:"purpose"`
		ActivityStatus          string  `json:"activity_status"`
		Visibility              string  `json:"visibility"`
		MainFrozen              bool    `json:"main_frozen"`
		GitRepositoryExternalID *string `json:"git_repository_external_id"`
		ProvisionStatus         string  `json:"provision_status"`
		CreatedBy               string  `json:"created_by"`
		CreatedAt               string  `json:"created_at"`
	} `json:"project"`
	Membership struct {
		ProjectID string `json:"project_id"`
		UserID    string `json:"user_id"`
		Role      string `json:"role"`
	} `json:"membership"`
}

type projectListResponse struct {
	Projects []struct {
		ID             string  `json:"id"`
		Slug           string  `json:"slug"`
		OrganizationID *string `json:"organization_id"`
		Visibility     string  `json:"visibility"`
	} `json:"projects"`
}

// TestProjectAPI is the T0104-TEST-01 "project api" gate. It drives the
// full acceptance journey over the wire against real PostgreSQL.
func TestProjectAPI(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), projectTaskID)

	// --- composition (identical to cmd/api/main.go) ---
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(pool),
		Sessions:   sessions,
		Limiter:    limiter,
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	orgStore := persistence.NewOrgStore(pool)
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(pool),
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	// Same dual registration as cmd/api/main.go: the bare path must not
	// be redirected for a trailing slash.
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	alice, aliceID := signup(t, ts.URL, "proj-alice@example.com", "proj-alice")
	bob, _ := signup(t, ts.URL, "proj-bob@example.com", "proj-bob")

	// --- anonymous write is 401 at the guard, before any routing ---
	anon := newTestUserClient(ts.URL)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/projects", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := anon.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusUnauthorized)
	_ = resp.Body.Close()
	// Anonymous reads answer with an empty (not yet populated) list —
	// visibility-aware reads arrived with T0106 (before it, reads were
	// 401 like the write above).
	resp, err = anon.client.Get(ts.URL + "/api/v1/projects")
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	var anonList projectListResponse
	if err := json.NewDecoder(resp.Body).Decode(&anonList); err != nil {
		t.Fatalf("anonymous list payload: %v", err)
	}
	if len(anonList.Projects) != 0 {
		t.Errorf("anonymous list has %d entries, want 0 (no projects yet): %+v", len(anonList.Projects), anonList.Projects)
	}

	// --- malformed JSON and empty bodies answer a clear validation
	// envelope, never a crash ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects", `{not json`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")
	resp = alice.do(t, http.MethodPost, "/api/v1/projects", `{}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")

	// --- alice creates her organization; the org create path makes her an
	// active owner, which the project gate requires ---
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"acme-labs","name":"Acme Research"}`)
	mustStatus(t, resp, http.StatusCreated)
	var orgResp orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&orgResp); err != nil {
		t.Fatalf("create org payload: %v", err)
	}
	orgID := orgResp.Organization.ID

	// --- public project inside the organization (requirement:
	// Public/Private preset) ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"MOF-Screening","name":"MOF Screening","purpose":"screen MOFs for gas separation","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create project payload: %v", err)
	}
	pub := created.Project
	if pub.Visibility != "public" {
		t.Errorf("visibility = %q, want public", pub.Visibility)
	}
	if pub.Slug != "mof-screening" {
		t.Errorf("slug = %q, want normalized mof-screening", pub.Slug)
	}
	if pub.Purpose != "screen MOFs for gas separation" {
		t.Errorf("purpose = %q", pub.Purpose)
	}
	if pub.OrganizationID == nil || *pub.OrganizationID != orgID {
		t.Errorf("organization_id = %v, want %s", pub.OrganizationID, orgID)
	}
	if pub.ProgramID != nil {
		t.Errorf("program_id = %v, want null (no program)", pub.ProgramID)
	}
	if pub.ProvisionStatus != "pending" {
		t.Errorf("provision_status = %q, want pending", pub.ProvisionStatus)
	}
	if pub.ActivityStatus != "planning" {
		t.Errorf("activity_status = %q, want planning", pub.ActivityStatus)
	}
	if pub.GitRepositoryExternalID != nil {
		t.Errorf("git_repository_external_id = %v, want null (not provisioned yet)", pub.GitRepositoryExternalID)
	}
	// --- creator becomes owner (requirement) ---
	if created.Membership.Role != "owner" || created.Membership.UserID != aliceID {
		t.Fatalf("creator membership = %+v, want owner membership of alice (%s)", created.Membership, aliceID)
	}
	if created.Membership.ProjectID != pub.ID || pub.ID == "" {
		t.Fatalf("create response ids = %+v", created)
	}

	// --- provision pending is real database state, not response
	// decoration (requirement) ---
	var dbProvision string
	if err := pool.QueryRow(ctx,
		`SELECT provision_status FROM projects WHERE id = $1`, pub.ID).Scan(&dbProvision); err != nil {
		t.Fatalf("probe provision_status: %v", err)
	}
	if dbProvision != "pending" {
		t.Errorf("provision_status in database = %q, want pending", dbProvision)
	}
	// The creator's owner membership is a real row too.
	var dbRole string
	if err := pool.QueryRow(ctx,
		`SELECT role FROM project_memberships WHERE project_id = $1 AND user_id = $2`,
		pub.ID, aliceID).Scan(&dbRole); err != nil {
		t.Fatalf("probe project_memberships: %v", err)
	}
	if dbRole != "owner" {
		t.Errorf("membership role in database = %q, want owner", dbRole)
	}

	// --- private project inside the organization ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"secret-project","name":"Secret","purpose":"private research","visibility":"private","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusCreated)
	var priv projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&priv); err != nil {
		t.Fatalf("private project payload: %v", err)
	}
	if priv.Project.Visibility != "private" {
		t.Errorf("visibility = %q, want private", priv.Project.Visibility)
	}
	if priv.Project.ProvisionStatus != "pending" {
		t.Errorf("provision_status = %q, want pending", priv.Project.ProvisionStatus)
	}

	// --- slug conflict inside the organization: same slug, and the
	// case-variant that normalizes to it (acceptance: slug conflict 清晰) ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"mof-screening","name":"Duplicate","purpose":"x","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "PROJECT_SLUG_TAKEN")
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"MOF-SCREENING","name":"Duplicate Case","purpose":"x","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "PROJECT_SLUG_TAKEN")

	// --- personal project (no organization) ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"solo-project","name":"Solo","purpose":"personal research","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var personal projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&personal); err != nil {
		t.Fatalf("personal project payload: %v", err)
	}
	if personal.Project.OrganizationID != nil {
		t.Errorf("organization_id = %v, want null (personal project)", personal.Project.OrganizationID)
	}
	if personal.Project.ProvisionStatus != "pending" {
		t.Errorf("provision_status = %q, want pending", personal.Project.ProvisionStatus)
	}

	// Personal slugs have their own uniqueness scope: re-using one answers
	// the same clear conflict…
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"solo-project","name":"Solo 2","purpose":"x","visibility":"public"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "PROJECT_SLUG_TAKEN")
	// …while a slug taken only inside the organization stays free here
	// (org scope and personal scope do not collide).
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"mof-screening","name":"Personal Clone","purpose":"x","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	_ = readAll(t, resp)

	// --- visibility preset validation: anything but public/private is
	// refused with a clear code ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"bad-vis","name":"Bad","purpose":"x","visibility":"internal","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")
	// --- purpose is required ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"no-purpose","name":"No Purpose","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")
	// --- slug shape ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"Has Spaces","name":"Bad Slug","purpose":"x","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")
	// --- name required ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"no-name","name":"  ","purpose":"x","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")

	// --- non-members cannot create projects inside the organization ---
	resp = bob.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"intruder","name":"Intruder","purpose":"x","visibility":"public","organization_id":"`+orgID+`"}`)
	mustStatus(t, resp, http.StatusForbidden)
	mustEnvelope(t, resp, "PROJECT_FORBIDDEN")

	// --- unknown organization ---
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"ghost-org-proj","name":"Ghost","purpose":"x","visibility":"public","organization_id":"00000000-0000-4000-8000-000000000000"}`)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "ORG_NOT_FOUND")

	// --- deactivated organization refuses new projects ---
	resp = alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"dying-org","name":"Dying Org"}`)
	mustStatus(t, resp, http.StatusCreated)
	var dying orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&dying); err != nil {
		t.Fatalf("dying org payload: %v", err)
	}
	resp = alice.do(t, http.MethodDelete, "/api/v1/organizations/"+dying.Organization.ID, "")
	mustStatus(t, resp, http.StatusNoContent)
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"too-late","name":"Too Late","purpose":"x","visibility":"public","organization_id":"`+dying.Organization.ID+`"}`)
	mustStatus(t, resp, http.StatusConflict)
	mustEnvelope(t, resp, "ORG_DEACTIVATED")

	// --- program: optional grouping, validated against the project's
	// organization (requirement: purpose 与可选 program) ---
	var programID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO programs (organization_id, slug, name) VALUES ($1, 'mof-program', 'MOF Program') RETURNING id`,
		orgID).Scan(&programID); err != nil {
		t.Fatalf("seed program: %v", err)
	}
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"program-project","name":"Program Project","purpose":"x","visibility":"private","organization_id":"`+orgID+`","program_id":"`+programID+`"}`)
	mustStatus(t, resp, http.StatusCreated)
	var withProgram projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&withProgram); err != nil {
		t.Fatalf("program project payload: %v", err)
	}
	if withProgram.Project.ProgramID == nil || *withProgram.Project.ProgramID != programID {
		t.Errorf("program_id = %v, want %s", withProgram.Project.ProgramID, programID)
	}
	// Unknown program.
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"ghost-program-proj","name":"Ghost Program","purpose":"x","visibility":"public","organization_id":"`+orgID+`","program_id":"00000000-0000-4000-8000-000000000000"}`)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROGRAM_NOT_FOUND")
	// A program from another organization does not fit.
	var otherOrgID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (slug, name) VALUES ('other-org', 'Other Org') RETURNING id`).Scan(&otherOrgID); err != nil {
		t.Fatalf("seed other org: %v", err)
	}
	var foreignProgramID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO programs (organization_id, slug, name) VALUES ($1, 'foreign-program', 'Foreign Program') RETURNING id`,
		otherOrgID).Scan(&foreignProgramID); err != nil {
		t.Fatalf("seed foreign program: %v", err)
	}
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"cross-program","name":"Cross Program","purpose":"x","visibility":"public","organization_id":"`+orgID+`","program_id":"`+foreignProgramID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "PROGRAM_ORG_MISMATCH")
	// A program on a personal project is refused up front.
	resp = alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"personal-program","name":"Personal Program","purpose":"x","visibility":"public","program_id":"`+programID+`"}`)
	mustStatus(t, resp, http.StatusBadRequest)
	mustEnvelope(t, resp, "VALIDATION_FAILED")

	// --- read isolation (T0106): public projects are readable by anyone;
	// private projects stay hidden from non-members (existence hiding) ---
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+pub.ID, "")
	mustStatus(t, resp, http.StatusOK)
	// GET returns the bare project payload (only create wraps it in
	// {"project": ...}), matching the orgs surface.
	var got projectResponse
	if err := json.Unmarshal([]byte(readAll(t, resp)), &got.Project); err != nil {
		t.Fatalf("get project payload: %v", err)
	}
	if got.Project.Visibility != "public" || got.Project.ProvisionStatus != "pending" {
		t.Errorf("get project = %+v, want visibility public and provision pending", got.Project)
	}
	// The public project is readable by a non-member and by an anonymous
	// caller alike (T0106).
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+pub.ID, "")
	mustStatus(t, resp, http.StatusOK)
	_ = readAll(t, resp)
	resp, err = anon.client.Get(ts.URL + "/api/v1/projects/" + pub.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusOK)
	_ = readAll(t, resp)
	// The private project stays hidden: bob and the anonymous caller get
	// the same existence-hiding 404 an unknown project produces.
	resp = bob.do(t, http.MethodGet, "/api/v1/projects/"+priv.Project.ID, "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")
	resp, err = anon.client.Get(ts.URL + "/api/v1/projects/" + priv.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")
	// Unknown project: same 404 shape.
	resp = alice.do(t, http.MethodGet, "/api/v1/projects/00000000-0000-4000-8000-000000000000", "")
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, "PROJECT_NOT_FOUND")

	// --- list: the actor's projects only ---
	resp = alice.do(t, http.MethodGet, "/api/v1/projects", "")
	mustStatus(t, resp, http.StatusOK)
	var list projectListResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("list payload: %v", err)
	}
	// Five projects: mof-screening (org) + secret-project (org) +
	// program-project (org) + solo-project (personal) + mof-screening
	// (personal clone — same slug, different scope).
	if len(list.Projects) != 5 {
		t.Fatalf("list returned %d projects, want 5: %+v", len(list.Projects), list.Projects)
	}
	slugCount := map[string]int{}
	personalMof := false
	for _, p := range list.Projects {
		slugCount[p.Slug]++
		if p.Slug == "mof-screening" && p.OrganizationID == nil {
			personalMof = true
		}
	}
	if slugCount["mof-screening"] != 2 || slugCount["secret-project"] != 1 ||
		slugCount["solo-project"] != 1 || slugCount["program-project"] != 1 {
		t.Errorf("list slugs = %v, want mof-screening x2 + secret-project + solo-project + program-project", slugCount)
	}
	if !personalMof {
		t.Error("personal mof-screening clone missing from the list (org-scoped slug must not collide with personal scope)")
	}
	resp = bob.do(t, http.MethodGet, "/api/v1/projects", "")
	mustStatus(t, resp, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("bob list payload: %v", err)
	}
	// Bob belongs to nothing, so his list is the visibility-filtered
	// public list (T0106): exactly the one public project — the private
	// ones never appear.
	if len(list.Projects) != 1 || list.Projects[0].Slug != "mof-screening" {
		t.Errorf("bob's project list = %+v, want exactly the public mof-screening (visibility filtering)", list.Projects)
	}
}
