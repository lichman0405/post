package gitprovider_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// The adapter-level tests for the user-access surface (T0304): the exact
// provider requests the adapter makes, which identity each one
// authenticates as, and the sentinel mapping. The fakeGitea surface from
// gitea_test.go records auth headers, so the identity split — admin basic
// auth for accounts/tokens, service token for collaborator grants — is
// asserted, not assumed.

// newUserAccess wires the user-access adapter onto a live httptest server.
func newUserAccess(t *testing.T, f *fakeGitea) *gitprovider.GiteaUserAccess {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return gitprovider.NewGiteaUserAccess(gitprovider.Config{
		BaseURL:       srv.URL,
		Token:         config.Secret("svc-token"),
		AdminUser:     "siteadmin",
		AdminPassword: config.Secret("admin-pw"),
	})
}

func TestUserAccessEnsureGitUserCreates(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/users/u-", status: http.StatusNotFound},
		{method: http.MethodPost, prefix: "/api/v1/admin/users", status: http.StatusCreated, body: `{"login":"u-abc"}`},
	}}
	a := newUserAccess(t, f)
	user, err := a.EnsureGitUser(testCtx(t), "00000000-0000-0000-0000-0000000000ab")
	if err != nil {
		t.Fatalf("EnsureGitUser: %v", err)
	}
	if user.Name != "u-00000000-0000-0000-0000-0000000000ab" {
		t.Errorf("user = %q, want the derived shadow login", user.Name)
	}
	// The create call authenticates as the ADMIN (basic auth), not as the
	// service account.
	creates := f.requests(http.MethodPost, "/api/v1/admin/users")
	if len(creates) != 1 {
		t.Fatalf("POST /admin/users called %d times, want 1", len(creates))
	}
	if !strings.HasPrefix(creates[0].auth, "Basic ") {
		t.Errorf("create call auth = %q, want the admin's basic auth", creates[0].auth)
	}
	if strings.Contains(creates[0].auth, "token ") {
		t.Errorf("create call auth %q leaked the service token", creates[0].auth)
	}
	if creates[0].body["username"] != "u-00000000-0000-0000-0000-0000000000ab" {
		t.Errorf("create username = %v", creates[0].body["username"])
	}
	if creates[0].body["must_change_password"] != false || creates[0].body["send_notify"] != false {
		t.Errorf("create body must not force a password change or a mail: %v", creates[0].body)
	}
	if email, _ := creates[0].body["email"].(string); email != "00000000-0000-0000-0000-0000000000ab@users.invalid" {
		t.Errorf("create email = %q, want the derived .invalid address", email)
	}
	if pw, _ := creates[0].body["password"].(string); len(pw) != 48 {
		t.Errorf("create password length = %d, want 48 hex chars (24 bytes)", len(pw))
	}
}

func TestUserAccessEnsureGitUserAdoptsExisting(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/users/u-", status: http.StatusOK, body: `{"login":"u-abc"}`},
	}}
	a := newUserAccess(t, f)
	user, err := a.EnsureGitUser(testCtx(t), "abc")
	if err != nil {
		t.Fatalf("EnsureGitUser: %v", err)
	}
	if user.Name != "u-abc" {
		t.Errorf("user = %q", user.Name)
	}
	if got := f.requests(http.MethodPost, "/api/v1/admin/users"); len(got) != 0 {
		t.Errorf("existing account must not be re-created (%d creates)", len(got))
	}
}

func TestUserAccessEnsureGitUserAdoptsCreateRace(t *testing.T) {
	// The provider answers 422 "already exists" on the create (a
	// concurrent creator won): the adapter must adopt, not fail. The fake
	// is stateful: the lookup answers 404 only before the race, then the
	// account exists.
	var lookups int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/users/u-"):
			lookups++
			if lookups == 1 {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"login":"u-abc"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/admin/users":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"user already exists [name: u-abc]"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := gitprovider.NewGiteaUserAccess(gitprovider.Config{
		BaseURL:       srv.URL,
		Token:         config.Secret("svc-token"),
		AdminUser:     "siteadmin",
		AdminPassword: config.Secret("admin-pw"),
	})
	if _, err := a.EnsureGitUser(testCtx(t), "abc"); err != nil {
		t.Fatalf("EnsureGitUser over a create race: %v", err)
	}
	if lookups != 2 {
		t.Errorf("GET /users called %d times, want 2 (probe + adopt)", lookups)
	}
}

func TestUserAccessEnsureGitUserMapsFailures(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/users/u-", status: http.StatusNotFound},
		{method: http.MethodPost, prefix: "/api/v1/admin/users", status: http.StatusForbidden, body: `{"message":"only admins"}`},
	}}
	a := newUserAccess(t, f)
	_, err := a.EnsureGitUser(testCtx(t), "abc")
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Fatalf("EnsureGitUser = %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "only admins") {
		t.Errorf("error %q must carry the provider message", err)
	}
}

func TestUserAccessGrantUsesServiceToken(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodPut, prefix: "/api/v1/repos/svc/r/collaborators/", status: http.StatusNoContent},
	}}
	a := newUserAccess(t, f)
	err := a.GrantAccess(testCtx(t),
		gitprovider.Repository{Owner: "svc", Name: "r"},
		gitprovider.GitUser{Name: "u-abc"}, gitprovider.AccessWrite)
	if err != nil {
		t.Fatalf("GrantAccess: %v", err)
	}
	puts := f.requests(http.MethodPut, "/api/v1/repos/svc/r/collaborators/")
	if len(puts) != 1 {
		t.Fatalf("PUT collaborators called %d times", len(puts))
	}
	if puts[0].auth != "token svc-token" {
		t.Errorf("collaborator grant must ride the SERVICE ACCOUNT token, got %q", puts[0].auth)
	}
	if puts[0].body["permission"] != "write" {
		t.Errorf("permission = %v, want write", puts[0].body["permission"])
	}
}

func TestUserAccessGrantRepoMissing(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodPut, prefix: "/api/v1/repos/", status: http.StatusNotFound},
	}}
	a := newUserAccess(t, f)
	err := a.GrantAccess(testCtx(t),
		gitprovider.Repository{Owner: "svc", Name: "gone"},
		gitprovider.GitUser{Name: "u-abc"}, gitprovider.AccessRead)
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Fatalf("GrantAccess = %v, want ErrNotFound", err)
	}
}

func TestUserAccessRevokeIsIdempotent(t *testing.T) {
	// 404 (repo or collaborator gone) is success: "no access" already holds.
	for _, code := range []int{http.StatusNoContent, http.StatusNotFound} {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodDelete, prefix: "/api/v1/repos/", status: code},
		}}
		a := newUserAccess(t, f)
		if err := a.RevokeAccess(testCtx(t),
			gitprovider.Repository{Owner: "svc", Name: "r"},
			gitprovider.GitUser{Name: "u-abc"}); err != nil {
			t.Fatalf("RevokeAccess (status %d): %v", code, err)
		}
	}
}

func TestUserAccessCreateTokenScopes(t *testing.T) {
	for level, wantScopes := range map[gitprovider.AccessLevel][]string{
		gitprovider.AccessRead:  {"read:repository"},
		gitprovider.AccessWrite: {"read:repository", "write:repository"},
	} {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodPost, prefix: "/api/v1/users/u-abc/tokens", status: http.StatusCreated,
				body: `{"id":42,"name":"post-x","sha1":"deadbeef"}`},
		}}
		a := newUserAccess(t, f)
		tok, err := a.CreateUserToken(testCtx(t), gitprovider.GitUser{Name: "u-abc"},
			gitprovider.UserTokenSpec{Name: "post-x", Level: level})
		if err != nil {
			t.Fatalf("CreateUserToken(%s): %v", level, err)
		}
		if tok.ID != 42 || tok.Value != "deadbeef" {
			t.Errorf("token = %+v, want id 42 value deadbeef", tok)
		}
		reqs := f.requests(http.MethodPost, "/api/v1/users/u-abc/tokens")
		if len(reqs) != 1 {
			t.Fatalf("POST tokens called %d times", len(reqs))
		}
		if !strings.HasPrefix(reqs[0].auth, "Basic ") {
			t.Errorf("token mint must ride the ADMIN's basic auth, got %q", reqs[0].auth)
		}
		scopes, _ := reqs[0].body["scopes"].([]any)
		if len(scopes) != len(wantScopes) {
			t.Fatalf("scopes(%s) = %v, want %v", level, scopes, wantScopes)
		}
		for i, want := range wantScopes {
			if scopes[i] != want {
				t.Errorf("scopes(%s)[%d] = %v, want %s", level, i, scopes[i], want)
			}
		}
	}
}

func TestUserAccessCreateTokenUnauthorized(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodPost, prefix: "/api/v1/users/u-abc/tokens", status: http.StatusUnauthorized, body: `{"message":"auth required"}`},
	}}
	a := newUserAccess(t, f)
	_, err := a.CreateUserToken(testCtx(t), gitprovider.GitUser{Name: "u-abc"},
		gitprovider.UserTokenSpec{Name: "x", Level: gitprovider.AccessRead})
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Fatalf("CreateUserToken = %v, want ErrUnauthorized", err)
	}
}

func TestUserAccessDeleteTokenIsIdempotent(t *testing.T) {
	for _, code := range []int{http.StatusNoContent, http.StatusNotFound} {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodDelete, prefix: "/api/v1/users/u-abc/tokens/", status: code},
		}}
		a := newUserAccess(t, f)
		if err := a.DeleteUserToken(testCtx(t), gitprovider.GitUser{Name: "u-abc"}, 42); err != nil {
			t.Fatalf("DeleteUserToken (status %d): %v", code, err)
		}
	}
}

func TestUserAccessDeleteTokenFailure(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodDelete, prefix: "/api/v1/users/u-abc/tokens/", status: http.StatusInternalServerError},
	}}
	a := newUserAccess(t, f)
	err := a.DeleteUserToken(testCtx(t), gitprovider.GitUser{Name: "u-abc"}, 42)
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("DeleteUserToken = %v, want ErrUnavailable", err)
	}
}

func TestGitUserNameShape(t *testing.T) {
	if got := gitprovider.GitUserName("00000000-0000-0000-0000-0000000000ab"); got != "u-00000000-0000-0000-0000-0000000000ab" {
		t.Errorf("GitUserName = %q", got)
	}
}
