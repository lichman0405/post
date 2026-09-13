package projectshttp

import (
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The settings endpoints' wire mapping through the REAL guard: real
// signup issues the session, and the write routes (PUT role, PATCH
// settings) additionally require the session-bound CSRF token the signup
// payload returned — so a missing or wrong token is part of what these
// tests prove. The stub store's settings maps exercise the service rules
// end to end (role gate, owner management, preview-only visibility).

const settingsProjectID = "11111111-2222-4333-8444-555555555555"

func newSettingsStubStore() *stubProjectStore {
	return &stubProjectStore{
		project: domain.Project{
			ID: settingsProjectID, Slug: "settings-lab", Name: "Settings Lab",
			Purpose:        "exercise the settings surface",
			ActivityStatus: "planning",
			Visibility:     domain.VisibilityPublic,
		},
		settingsMembers: map[string]domain.ProjectMembership{},
	}
}

// newSettingsTestServer composes the auth surface + project routes like
// newShellTestServer, and returns the server, the authed client, the
// signed-up user id and the CSRF token the writes must echo.
func newSettingsTestServer(t *testing.T, store projects.ProjectStore) (*httptest.Server, *http.Client, string, string) {
	t.Helper()
	authAPI := authhttp.New(authhttp.Deps{
		Users:      memstore.NewUsers(),
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
		Secure: false,
	})
	projectAPI := New(Deps{
		Store: store,
		Orgs:  &stubOrgGate{},
		Authz: stubEngine{},
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	authed := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := authed.Post(ts.URL+"/api/v1/auth/signup", "application/json",
		strings.NewReader(`{"email":"settings-handler@example.com","password":"long-enough-password-1","handle":"settings-handler","display_name":"Settings Handler"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup = %d", resp.StatusCode)
	}
	var payload struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return ts, authed, payload.User.ID, payload.CSRFToken
}

// settingsWrite performs one JSON write with the CSRF token attached,
// exactly as the web app sends it.
func settingsWrite(t *testing.T, client *http.Client, method, url, csrf, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// seedOwner gives the signed-up actor the owner membership (the id is
// only known after signup, so the seed happens post-hoc).
func seedOwner(store *stubProjectStore, actorID string) {
	store.settingsMembers[actorID] = domain.ProjectMembership{
		ProjectID: settingsProjectID, UserID: actorID,
		Role:      domain.ProjectRoleOwner,
		CreatedAt: time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC),
	}
}

func TestSettingsWireMapping(t *testing.T) {
	t.Run("member list round-trips", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, _ := newSettingsTestServer(t, store)
		seedOwner(store, actorID)
		store.settingsMembers["22222222-3333-4333-8444-555555555555"] = domain.ProjectMembership{
			ProjectID: settingsProjectID, UserID: "22222222-3333-4333-8444-555555555555",
			Role:      domain.ProjectRoleViewer,
			CreatedAt: time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC),
		}

		resp, err := authed.Get(ts.URL + "/api/v1/projects/" + settingsProjectID + "/members")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var payload struct {
			Members []memberPayload `json:"members"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if len(payload.Members) != 2 {
			t.Fatalf("members = %d, want 2", len(payload.Members))
		}
		first := payload.Members[0]
		if first.UserID != actorID || first.Role != "owner" ||
			first.Handle == "" || first.DisplayName == "" || first.JoinedAt == "" {
			t.Errorf("members[0] = %+v, want the owner with identity and join date", first)
		}
	})

	t.Run("role change round-trips with CSRF, audit recorded", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, csrf := newSettingsTestServer(t, store)
		seedOwner(store, actorID)
		target := "22222222-3333-4333-8444-555555555555"
		store.settingsMembers[target] = domain.ProjectMembership{
			ProjectID: settingsProjectID, UserID: target, Role: domain.ProjectRoleViewer,
		}

		resp := settingsWrite(t, authed, http.MethodPut,
			ts.URL+"/api/v1/projects/"+settingsProjectID+"/members/"+target, csrf,
			`{"role":"maintainer"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body := make([]byte, 512)
			n, _ := resp.Body.Read(body)
			t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body[:n])
		}
		var payload membershipPayload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Role != "maintainer" || payload.UserID != target || payload.ProjectID != settingsProjectID {
			t.Errorf("payload = %+v, want the maintainer membership", payload)
		}
		if len(store.audits) != 1 || store.audits[0].Action != "project.member_role_changed" {
			t.Errorf("audits = %+v, want one member_role_changed placeholder", store.audits)
		}
	})

	t.Run("write without CSRF token is refused by the guard", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, _ := newSettingsTestServer(t, store)
		seedOwner(store, actorID)
		target := "22222222-3333-4333-8444-555555555555"
		store.settingsMembers[target] = domain.ProjectMembership{
			ProjectID: settingsProjectID, UserID: target, Role: domain.ProjectRoleViewer,
		}

		resp := settingsWrite(t, authed, http.MethodPut,
			ts.URL+"/api/v1/projects/"+settingsProjectID+"/members/"+target, "wrong-token",
			`{"role":"maintainer"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 CSRF_FAILED", resp.StatusCode)
		}
		if len(store.audits) != 0 {
			t.Errorf("CSRF-refused write must not reach the store, got %d audits", len(store.audits))
		}
	})

	t.Run("settings update round-trips, visibility refused", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, csrf := newSettingsTestServer(t, store)
		seedOwner(store, actorID)

		resp := settingsWrite(t, authed, http.MethodPatch,
			ts.URL+"/api/v1/projects/"+settingsProjectID, csrf,
			`{"purpose":"A sharper goal.","activity_status":"active"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var payload projectPayload
		if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Purpose != "A sharper goal." || payload.ActivityStatus != "active" {
			t.Errorf("payload = %+v, want the updated purpose/status", payload)
		}
		if len(store.audits) != 1 || store.audits[0].Action != "project.settings_updated" {
			t.Errorf("audits = %+v, want one settings_updated placeholder", store.audits)
		}

		resp = settingsWrite(t, authed, http.MethodPatch,
			ts.URL+"/api/v1/projects/"+settingsProjectID, csrf,
			`{"visibility":"public"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("visibility change status = %d, want 400", resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != projects.CodeVisibilityChangeNotSupported {
			t.Errorf("code = %q, want %q", envelope.Code, projects.CodeVisibilityChangeNotSupported)
		}
	})

	t.Run("below-maintainer member gets SETTINGS_FORBIDDEN", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, csrf := newSettingsTestServer(t, store)
		store.settingsMembers[actorID] = domain.ProjectMembership{
			ProjectID: settingsProjectID, UserID: actorID, Role: domain.ProjectRoleViewer,
		}

		resp, err := authed.Get(ts.URL + "/api/v1/projects/" + settingsProjectID + "/members")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("members as viewer = %d, want 403", resp.StatusCode)
		}
		resp = settingsWrite(t, authed, http.MethodPatch,
			ts.URL+"/api/v1/projects/"+settingsProjectID, csrf,
			`{"purpose":"trying"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("update as viewer = %d, want 403", resp.StatusCode)
		}
		if len(store.audits) != 0 {
			t.Errorf("refused viewer writes must not produce audits, got %d", len(store.audits))
		}
	})

	t.Run("unknown target member answers MEMBER_NOT_FOUND", func(t *testing.T) {
		store := newSettingsStubStore()
		ts, authed, actorID, csrf := newSettingsTestServer(t, store)
		seedOwner(store, actorID)

		resp := settingsWrite(t, authed, http.MethodPut,
			ts.URL+"/api/v1/projects/"+settingsProjectID+"/members/99999999-8888-4777-8666-555555555555", csrf,
			`{"role":"viewer"}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", resp.StatusCode)
		}
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Code != projects.CodeMemberNotFound {
			t.Errorf("code = %q, want %q", envelope.Code, projects.CodeMemberNotFound)
		}
	})
}
