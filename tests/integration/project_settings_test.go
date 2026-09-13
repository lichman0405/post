// Package integration — T0109 settings surface exercised end to end
// against REAL PostgreSQL: real migrations, real pgx stores, the real
// auth guard (session + CSRF) and the real projectshttp handlers, exactly
// as cmd/api/main.go composes them. The suite proves:
//
//   - the member list carries identity + role + join date for
//     owner/maintainer, and is refused (403) below the gate;
//   - role changes follow the owner/maintainer rules (owner management is
//     owner-only, no self-change, last owner protected) and every
//     accepted change lands its audit_log placeholder in the same
//     transaction — with actor id, via, action, target, project and the
//     request's correlation id;
//   - the purpose/activity-status edit round-trips and is audited;
//     visibility changes are refused (preview-only);
//   - a stranger of the private project gets the existence-hiding 404 on
//     every settings route.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const projectSettingsTaskID = "T0109"

// memberRow is the wire shape of one member-list row.
type memberRow struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joined_at"`
}

// settingsProjectPayload is the FLAT project shape handleGet and
// handleUpdateSettings answer (projectResponse in project_api_test.go is
// the wrapped create shape).
type settingsProjectPayload struct {
	ID              string `json:"id"`
	Slug            string `json:"slug"`
	Name            string `json:"name"`
	Purpose         string `json:"purpose"`
	ActivityStatus  string `json:"activity_status"`
	Visibility      string `json:"visibility"`
	MainFrozen      bool   `json:"main_frozen"`
	ProvisionStatus string `json:"provision_status"`
}

// auditRow mirrors the audit_log columns the settings writes must fill.
type auditRow struct {
	ActorID       string `json:"actor_id"`
	Via           string `json:"via"`
	Action        string `json:"action"`
	TargetRef     string `json:"target_ref"`
	ProjectID     string `json:"project_id"`
	CorrelationID string `json:"correlation_id"`
}

// newSettingsServer composes the production tree (like cmd/api/main.go)
// and returns it plus the store handle and the pool for SQL-level
// verification (all three share ONE test database — testdb.Setup creates
// a fresh namespaced database per call, so the helpers take the pool
// instead of re-running Setup).
func newSettingsServer(t *testing.T, ctx context.Context) (*httptest.Server, *persistence.ProjectStore, *pgxpool.Pool) {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), projectSettingsTaskID)
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
	store := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: store,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/organizations", orgAPI.Routes())
	apiMux.Handle("/api/v1/organizations/", orgAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	t.Cleanup(ts.Close)
	return ts, store, pool
}

// seedMember inserts one project membership directly (the management API
// is what the test exercises — the seeds are the fixtures).
func seedMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, userID, role string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, $3)`,
		projectID, userID, role); err != nil {
		t.Fatalf("seed %s membership: %v", role, err)
	}
}

// listAuditRows reads the audit_log rows of one project (newest first).
func listAuditRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) []auditRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT actor_id::text, via, action, target_ref, project_id::text, correlation_id
		FROM audit_log
		WHERE project_id = $1
		ORDER BY occurred_at DESC, id DESC`, projectID)
	if err != nil {
		t.Fatalf("list audit rows: %v", err)
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var a auditRow
		var targetRef *string
		if err := rows.Scan(&a.ActorID, &a.Via, &a.Action, &targetRef, &a.ProjectID, &a.CorrelationID); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		if targetRef != nil {
			a.TargetRef = *targetRef
		}
		out = append(out, a)
	}
	return out
}

func TestProjectSettings(t *testing.T) {
	ctx := testCtx(t)
	ts, store, pool := newSettingsServer(t, ctx)

	alice, aliceID := signup(t, ts.URL, "settings-alice@example.com", "settings-alice")
	_, bobID := signup(t, ts.URL, "settings-bob@example.com", "settings-bob")
	carol, carolID := signup(t, ts.URL, "settings-carol@example.com", "settings-carol")
	eve, eveID := signup(t, ts.URL, "settings-eve@example.com", "settings-eve")
	stranger, _ := signup(t, ts.URL, "settings-stranger@example.com", "settings-stranger")

	// Alice creates a PRIVATE project; the creator becomes owner.
	resp := alice.do(t, http.MethodPost, "/api/v1/projects",
		`{"slug":"settings-lab","name":"Settings Lab","purpose":"exercise the settings surface","visibility":"private"}`)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	projectID := created.Project.ID

	// Fixtures: bob maintainer, carol viewer.
	seedMember(t, ctx, pool, projectID, bobID, "maintainer")
	seedMember(t, ctx, pool, projectID, carolID, "viewer")

	t.Run("member list carries identity and roles", func(t *testing.T) {
		resp := alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/members", "")
		mustStatus(t, resp, http.StatusOK)
		var payload struct {
			Members []memberRow `json:"members"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("members payload: %v", err)
		}
		if len(payload.Members) != 3 {
			t.Fatalf("members = %d, want 3", len(payload.Members))
		}
		// The owner joined first (creation), the seeds after — the list is
		// ordered by join time.
		if payload.Members[0].UserID != aliceID || payload.Members[0].Role != "owner" {
			t.Errorf("members[0] = %+v, want alice as owner", payload.Members[0])
		}
		for _, m := range payload.Members {
			if m.Handle == "" || m.DisplayName == "" || m.JoinedAt == "" {
				t.Errorf("member %s missing identity or join date: %+v", m.UserID, m)
			}
		}
		if payload.Members[1].Role != "maintainer" || payload.Members[2].Role != "viewer" {
			t.Errorf("members[1:] = %+v, want maintainer then viewer", payload.Members[1:])
		}
	})

	t.Run("viewer is refused on every settings route", func(t *testing.T) {
		for _, tc := range []struct {
			method, path, body string
		}{
			{http.MethodGet, "/api/v1/projects/" + projectID + "/members", ""},
			{http.MethodPut, "/api/v1/projects/" + projectID + "/members/" + bobID, `{"role":"viewer"}`},
			{http.MethodPatch, "/api/v1/projects/" + projectID, `{"purpose":"trying"}`},
		} {
			resp := carol.do(t, tc.method, tc.path, tc.body)
			mustStatus(t, resp, http.StatusForbidden)
			mustEnvelope(t, resp, projects.CodeSettingsForbidden)
		}
		// The viewer's attempts must not have landed anything in the audit
		// log.
		if audits := listAuditRows(t, ctx, pool, projectID); len(audits) != 0 {
			t.Errorf("viewer attempts wrote %d audit rows, want 0", len(audits))
		}
	})

	t.Run("owner changes roles with audit placeholder", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+carolID, `{"role":"maintainer"}`)
		mustStatus(t, resp, http.StatusOK)
		audits := listAuditRows(t, ctx, pool, projectID)
		if len(audits) != 1 {
			t.Fatalf("audit rows = %d, want 1", len(audits))
		}
		a := audits[0]
		if a.ActorID != aliceID || a.Via != "api" || a.Action != "project.member_role_changed" ||
			a.TargetRef != carolID || a.ProjectID != projectID || a.CorrelationID == "" {
			t.Errorf("audit row = %+v, want actor/via/action/target/project/correlation", a)
		}
		// And the role really changed.
		resp = carol.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/membership", "")
		mustStatus(t, resp, http.StatusOK)
		var membership projectMembershipResponse
		if err := json.NewDecoder(resp.Body).Decode(&membership); err != nil {
			t.Fatalf("membership payload: %v", err)
		}
		if membership.Role != "maintainer" {
			t.Errorf("carol role = %q, want maintainer", membership.Role)
		}
	})

	t.Run("maintainer governs non-owner roles", func(t *testing.T) {
		// Carol is now a maintainer; she demotes bob maintainer->viewer
		// (member policy is maintainer-level, docs/04 §2).
		resp := carol.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+bobID, `{"role":"viewer"}`)
		mustStatus(t, resp, http.StatusOK)
		// But she may not grant or revoke the owner role.
		resp = carol.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+aliceID, `{"role":"viewer"}`)
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, projects.CodeOwnerRoleChangeForbidden)
		resp = carol.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+bobID, `{"role":"owner"}`)
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, projects.CodeOwnerRoleChangeForbidden)
	})

	t.Run("nobody changes their own role", func(t *testing.T) {
		resp := alice.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+aliceID, `{"role":"maintainer"}`)
		mustStatus(t, resp, http.StatusForbidden)
		mustEnvelope(t, resp, projects.CodeSelfRoleChangeForbidden)
	})

	t.Run("last owner is protected", func(t *testing.T) {
		// A second owner (eve) lets alice exercise the demote-an-owner
		// path: allowed while two owners exist.
		seedMember(t, ctx, pool, projectID, eveID, "owner")
		resp := alice.do(t, http.MethodPut,
			"/api/v1/projects/"+projectID+"/members/"+eveID, `{"role":"maintainer"}`)
		mustStatus(t, resp, http.StatusOK)
		// Direct store-level proof of the race guard (the service rules
		// make a sole-owner demotion reachable over the wire only through
		// a concurrent race): demoting the sole owner answers ErrLastOwner
		// under the project lock.
		_, err := store.UpdateMembershipRole(ctx, projectID, aliceID,
			domain.ProjectRoleMaintainer, domain.AuditEntry{
				ActorID:       bobID,
				Via:           "api",
				Action:        "project.member_role_changed",
				TargetRef:     &aliceID,
				ProjectID:     projectID,
				CorrelationID: "integration-last-owner",
				Before:        map[string]any{"role": "owner"},
				After:         map[string]any{"role": "maintainer"},
			})
		if !errors.Is(err, projects.ErrLastOwner) {
			t.Fatalf("last-owner demotion = %v, want ErrLastOwner", err)
		}
		// The refused write must not have produced an audit row (the
		// refusal happens before the audit insert, inside the same tx).
		audits := listAuditRows(t, ctx, pool, projectID)
		if len(audits) != 3 { // 1 (alice->carol) + 1 (carol->bob) + 1 (alice->eve)
			t.Errorf("audit rows = %d, want 3 (refused writes are never audited)", len(audits))
		}
	})

	t.Run("purpose and activity status edit round-trips, audited", func(t *testing.T) {
		resp := alice.do(t, http.MethodPatch, "/api/v1/projects/"+projectID,
			`{"purpose":"A sharper research goal.","activity_status":"active"}`)
		mustStatus(t, resp, http.StatusOK)
		var payload settingsProjectPayload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("settings payload: %v", err)
		}
		if payload.Purpose != "A sharper research goal." || payload.ActivityStatus != "active" {
			t.Errorf("payload = %+v, want the new purpose/status", payload)
		}
		audits := listAuditRows(t, ctx, pool, projectID)
		if len(audits) == 0 || audits[0].Action != "project.settings_updated" ||
			audits[0].ActorID != aliceID || audits[0].CorrelationID == "" {
			t.Errorf("newest audit row = %+v, want the settings_updated placeholder", audits[0])
		}
		// The before/after summaries hold the state around the change.
		var before, after map[string]string
		if err := pool.QueryRow(ctx, `SELECT before_summary, after_summary FROM audit_log
			WHERE project_id = $1 AND action = 'project.settings_updated'
			ORDER BY occurred_at DESC LIMIT 1`, projectID).Scan(&before, &after); err != nil {
			t.Fatalf("audit summaries: %v", err)
		}
		if before["purpose"] != "exercise the settings surface" || after["activity_status"] != "active" ||
			after["purpose"] != "A sharper research goal." {
			t.Errorf("summaries = before %v after %v, want the state around the change", before, after)
		}
	})

	t.Run("visibility stays preview-only", func(t *testing.T) {
		resp := alice.do(t, http.MethodPatch, "/api/v1/projects/"+projectID,
			`{"visibility":"public"}`)
		mustStatus(t, resp, http.StatusBadRequest)
		mustEnvelope(t, resp, projects.CodeVisibilityChangeNotSupported)
		// Nothing changed and nothing was audited.
		resp = alice.do(t, http.MethodGet, "/api/v1/projects/"+projectID, "")
		mustStatus(t, resp, http.StatusOK)
		var payload settingsProjectPayload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatalf("project payload: %v", err)
		}
		if payload.Visibility != "private" {
			t.Errorf("visibility = %q, want unchanged private", payload.Visibility)
		}
	})

	t.Run("malformed settings input is refused", func(t *testing.T) {
		for _, body := range []string{
			`{}`,
			`{"purpose":"   "}`,
			`{"activity_status":"sleeping"}`,
			`{"purpose":"ok","activity_status":"planning","visibility":"public"}`,
		} {
			resp := alice.do(t, http.MethodPatch, "/api/v1/projects/"+projectID, body)
			mustStatus(t, resp, http.StatusBadRequest)
		}
	})

	t.Run("stranger of the private project gets existence hiding", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/projects/" + projectID + "/members",
		} {
			resp := stranger.do(t, http.MethodGet, path, "")
			mustStatus(t, resp, http.StatusNotFound)
			mustEnvelope(t, resp, projects.CodeProjectNotFound)
		}
		resp := stranger.do(t, http.MethodPatch, "/api/v1/projects/"+projectID,
			`{"purpose":"sneaking"}`)
		mustStatus(t, resp, http.StatusNotFound)
		mustEnvelope(t, resp, projects.CodeProjectNotFound)
	})

	t.Run("anonymous callers are 401", func(t *testing.T) {
		anon := newTestUserClient(ts.URL)
		resp := anon.do(t, http.MethodGet, "/api/v1/projects/"+projectID+"/members", "")
		mustStatus(t, resp, http.StatusUnauthorized)
	})

	t.Run("the project still has its owner", func(t *testing.T) {
		var owners int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM project_memberships WHERE project_id = $1 AND role = 'owner'`,
			projectID).Scan(&owners); err != nil {
			t.Fatalf("count owners: %v", err)
		}
		if owners < 1 {
			t.Errorf("owners = %d, want >= 1 (never orphaned)", owners)
		}
	})

	_ = eve
}
