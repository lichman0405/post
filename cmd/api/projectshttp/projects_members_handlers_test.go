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
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The invite endpoint's wire mapping (T1335): POST
// /api/v1/projects/{projectId}/members adds an existing account, named by
// handle, at the chosen role. These tests run the REAL memstore adapter
// (the in-memory stand-in for persistence.ProjectStore — handle
// resolution, the membership row, the duplicate refusal) through the REAL
// guard, so the session + CSRF requirements and the membership write are
// both exercised end to end. The SQL adapter's transaction is covered by
// tests/integration against real PostgreSQL.

// memberEnv is one composed server: the project space (so a test can seed
// the account directory) and the URL.
type memberEnv struct {
	ts    *httptest.Server
	store *memstore.Projects
}

// newMemberEnv composes the auth surface and the project surface exactly
// like main.go, over the in-memory project space.
func newMemberEnv(t *testing.T) *memberEnv {
	t.Helper()
	store := memstore.NewProjects()
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
		Orgs:  store.Gate(),
		Authz: authz.NewMatrixEngine(),
	})
	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &memberEnv{ts: ts, store: store}
}

// account is one signed-in browser: its client, id, handle and CSRF token.
type account struct {
	client *http.Client
	id     string
	handle string
	csrf   string
}

// signup creates an account through the real signup endpoint and seeds
// the project space's account directory with it — the production tree
// has ONE users table, so a member added by handle must resolve to the
// account that signed up (the in-memory spaces are separate maps, hence
// the seed).
func (e *memberEnv) signup(t *testing.T, handle string) *account {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	body := `{"email":"` + handle + `@example.com","password":"long-enough-password-1","handle":"` + handle + `","display_name":"User ` + handle + `"}`
	resp, err := client.Post(e.ts.URL+"/api/v1/auth/signup", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("signup %s = %d", handle, resp.StatusCode)
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
	if payload.User.ID == "" || payload.CSRFToken == "" {
		t.Fatalf("signup %s returned no id/csrf token", handle)
	}
	e.store.SeedUser(domain.User{
		ID: payload.User.ID, Handle: handle, DisplayName: "User " + handle,
	})
	return &account{client: client, id: payload.User.ID, handle: handle, csrf: payload.CSRFToken}
}

// createProject creates a private personal project as the account (the
// creator becomes its owner) and returns its id.
func (e *memberEnv) createProject(t *testing.T, owner *account, slug string) string {
	t.Helper()
	body := `{"slug":"` + slug + `","name":"` + slug + `","purpose":"exercise the invite path","visibility":"private"}`
	resp := write(t, owner.client, http.MethodPost, e.ts.URL+"/api/v1/projects", owner.csrf, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project = %d", resp.StatusCode)
	}
	var payload struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Project.ID
}

// write performs one JSON write with the CSRF token, exactly as the web
// app sends it.
func write(t *testing.T, client *http.Client, method, url, csrf, body string) *http.Response {
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

// errorCode decodes the standard envelope's stable code.
func errorCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var envelope struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	return envelope.Code
}

// membersOf reads the member list as the account and returns it.
func (e *memberEnv) membersOf(t *testing.T, who *account, projectID string) []memberPayload {
	t.Helper()
	resp, err := who.client.Get(e.ts.URL + "/api/v1/projects/" + projectID + "/members")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list members as %s = %d", who.handle, resp.StatusCode)
	}
	var payload struct {
		Members []memberPayload `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	return payload.Members
}

// findMember returns the member row for userID, or nil.
func findMember(members []memberPayload, userID string) *memberPayload {
	for i := range members {
		if members[i].UserID == userID {
			return &members[i]
		}
	}
	return nil
}

// TestOwnerAddsMemberByHandle is the required test "the owner can add an
// existing account by handle and role through the project members API":
// an owner posts a handle + role, the membership row lands with that role,
// the member list shows the account under its real handle, and the
// account can then read the private project it was added to.
func TestOwnerAddsMemberByHandle(t *testing.T) {
	env := newMemberEnv(t)
	owner := env.signup(t, "owner-account")
	alice := env.signup(t, "alice")
	projectID := env.createProject(t, owner, "invite-lab")

	resp := write(t, owner.client, http.MethodPost,
		env.ts.URL+"/api/v1/projects/"+projectID+"/members", owner.csrf,
		`{"handle":"alice","role":"contributor"}`)
	if resp.StatusCode != http.StatusCreated {
		defer resp.Body.Close()
		var body [512]byte
		n, _ := resp.Body.Read(body[:])
		t.Fatalf("add member = %d, want 201: %s", resp.StatusCode, body[:n])
	}
	var added membershipPayload
	if err := json.NewDecoder(resp.Body).Decode(&added); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if added.UserID != alice.id || added.Role != "contributor" || added.ProjectID != projectID || added.CreatedAt.IsZero() {
		t.Errorf("added = %+v, want alice as a contributor of %s", added, projectID)
	}

	// Persistence: the row is readable through the same member list the
	// Settings page renders, under the account's own handle.
	members := env.membersOf(t, owner, projectID)
	row := findMember(members, alice.id)
	if row == nil {
		t.Fatalf("member list = %+v, want the added account", members)
	}
	if row.Role != "contributor" || row.Handle != "alice" || row.DisplayName != "User alice" {
		t.Errorf("member row = %+v, want alice/contributor with the seeded identity", row)
	}

	// The added account can now read the private project (membership is
	// the read policy) — the invite reaches beyond the member list.
	readResp, err := alice.client.Get(env.ts.URL + "/api/v1/projects/" + projectID)
	if err != nil {
		t.Fatal(err)
	}
	defer readResp.Body.Close()
	if readResp.StatusCode != http.StatusOK {
		t.Errorf("added contributor read = %d, want 200", readResp.StatusCode)
	}
}

// TestNonOwnerCannotAddOrPromote is the required test "a non-owner cannot
// add or promote a member through the API": a viewer is refused on both
// the invite and the role change, a non-member cannot even see the
// private project, and neither refusal writes a membership.
func TestNonOwnerCannotAddOrPromote(t *testing.T) {
	env := newMemberEnv(t)
	owner := env.signup(t, "owner-account")
	viewerAcct := env.signup(t, "viewer-acct")
	carol := env.signup(t, "carol")
	projectID := env.createProject(t, owner, "invite-gate")

	// The owner adds the viewer through the very endpoint under test.
	resp := write(t, owner.client, http.MethodPost,
		env.ts.URL+"/api/v1/projects/"+projectID+"/members", owner.csrf,
		`{"handle":"viewer-acct","role":"viewer"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("owner add viewer = %d, want 201", resp.StatusCode)
	}

	// The viewer cannot add anyone.
	resp = write(t, viewerAcct.client, http.MethodPost,
		env.ts.URL+"/api/v1/projects/"+projectID+"/members", viewerAcct.csrf,
		`{"handle":"carol","role":"contributor"}`)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("viewer add = %d, want 403", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != projects.CodeSettingsForbidden {
		t.Errorf("viewer add code = %q, want %q", code, projects.CodeSettingsForbidden)
	}

	// Nor promote anyone, themselves included.
	resp = write(t, viewerAcct.client, http.MethodPut,
		env.ts.URL+"/api/v1/projects/"+projectID+"/members/"+viewerAcct.id, viewerAcct.csrf,
		`{"role":"owner"}`)
	if resp.StatusCode != http.StatusForbidden {
		resp.Body.Close()
		t.Fatalf("viewer self-promotion = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()

	// A non-member learns nothing: the private project answers the
	// existence-hiding not-found, not a settings error.
	resp = write(t, carol.client, http.MethodPost,
		env.ts.URL+"/api/v1/projects/"+projectID+"/members", carol.csrf,
		`{"handle":"viewer-acct","role":"owner"}`)
	if resp.StatusCode != http.StatusNotFound {
		resp.Body.Close()
		t.Fatalf("non-member add = %d, want 404", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != projects.CodeProjectNotFound {
		t.Errorf("non-member add code = %q, want %q", code, projects.CodeProjectNotFound)
	}

	// Nothing landed: the member list is still owner + viewer, and carol
	// holds no membership.
	members := env.membersOf(t, owner, projectID)
	if len(members) != 2 {
		t.Errorf("members = %d, want the owner and the viewer only", len(members))
	}
	if findMember(members, carol.id) != nil {
		t.Errorf("refused invites must not add a member: %+v", members)
	}
	if row := findMember(members, viewerAcct.id); row == nil || row.Role != "viewer" {
		t.Errorf("viewer row = %+v, want the untouched viewer role", row)
	}
}

// TestAddMemberExplicitErrors is the required test "unknown handle,
// duplicate member and invalid role are explicit API errors": each
// refusal carries its own stable code instead of a generic failure, and a
// missing CSRF token never reaches the store.
func TestAddMemberExplicitErrors(t *testing.T) {
	env := newMemberEnv(t)
	owner := env.signup(t, "owner-account")
	alice := env.signup(t, "alice")
	projectID := env.createProject(t, owner, "invite-errors")
	url := env.ts.URL + "/api/v1/projects/" + projectID + "/members"

	t.Run("unknown handle", func(t *testing.T) {
		resp := write(t, owner.client, http.MethodPost, url, owner.csrf,
			`{"handle":"nobody-here","role":"viewer"}`)
		if resp.StatusCode != http.StatusNotFound {
			resp.Body.Close()
			t.Fatalf("unknown handle = %d, want 404", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != projects.CodeUserNotFound {
			t.Errorf("code = %q, want %q", code, projects.CodeUserNotFound)
		}
	})

	t.Run("invalid role", func(t *testing.T) {
		resp := write(t, owner.client, http.MethodPost, url, owner.csrf,
			`{"handle":"alice","role":"admin"}`)
		if resp.StatusCode != http.StatusBadRequest {
			resp.Body.Close()
			t.Fatalf("invalid role = %d, want 400", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != projects.CodeValidationFailed {
			t.Errorf("code = %q, want %q", code, projects.CodeValidationFailed)
		}
	})

	t.Run("duplicate member", func(t *testing.T) {
		resp := write(t, owner.client, http.MethodPost, url, owner.csrf,
			`{"handle":"alice","role":"contributor"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("first add = %d, want 201", resp.StatusCode)
		}
		resp = write(t, owner.client, http.MethodPost, url, owner.csrf,
			`{"handle":"alice","role":"maintainer"}`)
		if resp.StatusCode != http.StatusConflict {
			resp.Body.Close()
			t.Fatalf("duplicate add = %d, want 409", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != projects.CodeMemberAlreadyExists {
			t.Errorf("code = %q, want %q", code, projects.CodeMemberAlreadyExists)
		}
		// The refusal is not a silent role change.
		members := env.membersOf(t, owner, projectID)
		if row := findMember(members, alice.id); row == nil || row.Role != "contributor" {
			t.Errorf("alice row = %+v, want the untouched contributor role", row)
		}
	})

	t.Run("maintainer can invite, but not as owner", func(t *testing.T) {
		dave := env.signup(t, "dave")
		eve := env.signup(t, "eve")
		frank := env.signup(t, "frank")
		resp := write(t, owner.client, http.MethodPost, url, owner.csrf,
			`{"handle":"dave","role":"maintainer"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("add maintainer = %d, want 201", resp.StatusCode)
		}
		// The maintainer may invite (docs/04 §2: maintainers govern
		// member policy) — just not into the owner role.
		resp = write(t, dave.client, http.MethodPost, url, dave.csrf,
			`{"handle":"frank","role":"contributor"}`)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("maintainer invites contributor = %d, want 201", resp.StatusCode)
		}
		added := env.membersOf(t, owner, projectID)
		if row := findMember(added, frank.id); row == nil || row.Role != "contributor" {
			t.Errorf("frank row = %+v, want the contributor the maintainer added", row)
		}
		resp = write(t, dave.client, http.MethodPost, url, dave.csrf,
			`{"handle":"eve","role":"owner"}`)
		if resp.StatusCode != http.StatusForbidden {
			resp.Body.Close()
			t.Fatalf("maintainer invites owner = %d, want 403", resp.StatusCode)
		}
		if code := errorCode(t, resp); code != projects.CodeOwnerRoleChangeForbidden {
			t.Errorf("code = %q, want %q", code, projects.CodeOwnerRoleChangeForbidden)
		}
		if members := env.membersOf(t, owner, projectID); findMember(members, eve.id) != nil {
			t.Errorf("refused owner invite must not add eve: %+v", members)
		}
	})

	t.Run("missing CSRF token never reaches the store", func(t *testing.T) {
		before := len(env.membersOf(t, owner, projectID))
		resp := write(t, owner.client, http.MethodPost, url, "wrong-token",
			`{"handle":"alice","role":"viewer"}`)
		if resp.StatusCode != http.StatusForbidden {
			resp.Body.Close()
			t.Fatalf("bad CSRF = %d, want 403", resp.StatusCode)
		}
		resp.Body.Close()
		if after := len(env.membersOf(t, owner, projectID)); after != before {
			t.Errorf("CSRF-refused write changed the member count: %d -> %d", before, after)
		}
	})
}
