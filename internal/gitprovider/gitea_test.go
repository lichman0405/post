package gitprovider_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/config"
	"github.com/lichman0405/post/internal/gitprovider"
)

// fakeGitea is a scripted Gitea API v1 surface: each registered route
// answers with a fixed (status, body), and every request is recorded for
// assertions. A request no route covers answers 404.
type fakeGitea struct {
	routes []giteaRoute
	reqs   []giteaRequest
}

type giteaRoute struct {
	method, prefix string
	status         int
	body           string
}

type giteaRequest struct {
	method, path, escapedPath, auth string
	body                            map[string]any
}

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	rec := giteaRequest{
		method: r.Method, path: r.URL.Path, escapedPath: r.URL.EscapedPath(),
		auth: r.Header.Get("Authorization"),
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &rec.body)
	}
	f.reqs = append(f.reqs, rec)
	for _, rt := range f.routes {
		if rt.method == r.Method && strings.HasPrefix(r.URL.Path, rt.prefix) {
			w.WriteHeader(rt.status)
			_, _ = w.Write([]byte(rt.body))
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"fake gitea: no route"}`))
}

// requests returns the recorded requests filtered by method and decoded
// path prefix.
func (f *fakeGitea) requests(method, prefix string) []giteaRequest {
	var out []giteaRequest
	for _, r := range f.reqs {
		if r.method == method && strings.HasPrefix(r.path, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// newGiteaAdapter wires the adapter onto a live httptest server over f.
func newGiteaAdapter(t *testing.T, f *fakeGitea) *gitprovider.GiteaAdapter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return gitprovider.NewGiteaAdapter(gitprovider.Config{
		BaseURL: srv.URL,
		Token:   config.Secret("test-token"),
	})
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestGiteaOwnerResolvedAndCached: the service account login comes from the
// token (GET /user), and is fetched exactly once per adapter.
func TestGiteaOwnerResolvedAndCached(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
	}}
	a := newGiteaAdapter(t, f)
	ctx := testCtx(t)

	for i := 0; i < 2; i++ {
		owner, err := a.Owner(ctx)
		if err != nil {
			t.Fatalf("Owner call %d: %v", i, err)
		}
		if owner != "post-git-svc" {
			t.Errorf("Owner call %d = %q, want post-git-svc", i, owner)
		}
	}
	if got := len(f.requests(http.MethodGet, "/api/v1/user")); got != 1 {
		t.Errorf("GET /user hit %d times, want 1 (cached)", got)
	}
}

// TestGiteaOwnerTransientFailureRetried: a failed resolution is NOT cached
// — the next call re-resolves (a transient outage or a rotated token must
// not poison every later call for the process lifetime), while a success
// still is.
func TestGiteaOwnerTransientFailureRetried(t *testing.T) {
	var gets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/user" {
			http.NotFound(w, r)
			return
		}
		gets++
		if gets == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"login":"post-git-svc"}`))
	}))
	t.Cleanup(srv.Close)
	a := gitprovider.NewGiteaAdapter(gitprovider.Config{BaseURL: srv.URL, Token: config.Secret("test-token")})

	ctx := testCtx(t)
	if _, err := a.Owner(ctx); !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Fatalf("first Owner error = %v, want ErrUnavailable", err)
	}
	owner, err := a.Owner(ctx)
	if err != nil {
		t.Fatalf("second Owner: %v", err)
	}
	if owner != "post-git-svc" {
		t.Errorf("owner = %q, want post-git-svc", owner)
	}
	if gets != 2 {
		t.Errorf("GET /user hit %d times, want 2 (failure re-resolved, success cached)", gets)
	}
}

// TestGiteaOwnerUnauthorized: rejected credentials map to ErrUnauthorized —
// and the provider's message rides along.
func TestGiteaOwnerUnauthorized(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusUnauthorized, body: `{"message":"token is revoked"}`},
	}}
	a := newGiteaAdapter(t, f)

	_, err := a.Owner(testCtx(t))
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Errorf("Owner error = %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "token is revoked") {
		t.Errorf("error %q lost the provider message", err)
	}
}

// TestGiteaProviderDown: a dead provider maps to ErrUnavailable, and the
// transport detail never reaches the caller (it can embed secrets).
func TestGiteaProviderDown(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	base := srv.URL
	srv.Close() // now nobody listens
	a := gitprovider.NewGiteaAdapter(gitprovider.Config{BaseURL: base, Token: config.Secret("test-token")})

	_, err := a.Owner(testCtx(t))
	if !errors.Is(err, gitprovider.ErrUnavailable) {
		t.Errorf("Owner error = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), base) {
		t.Errorf("error %q leaks the base URL", err)
	}
}

// TestGiteaEnsureRepositoryCreates: the create path posts the full spec to
// the service account's namespace with the token attached.
func TestGiteaEnsureRepositoryCreates(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
		{method: http.MethodPost, prefix: "/api/v1/user/repos", status: http.StatusCreated,
			body: `{"id":42,"full_name":"post-git-svc/p-abc","owner":{"login":"post-git-svc"},"clone_url":"http://gitea/post-git-svc/p-abc.git","private":true}`},
	}}
	a := newGiteaAdapter(t, f)

	repo, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{
		Name: "p-abc", Description: "POST project slug (Name)", Private: true,
	})
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if repo.Owner != "post-git-svc" || repo.Name != "p-abc" || repo.ID != 42 || !repo.Private {
		t.Errorf("repo = %+v, want post-git-svc/p-abc id 42 private", repo)
	}

	creates := f.requests(http.MethodPost, "/api/v1/user/repos")
	if len(creates) != 1 {
		t.Fatalf("POST /user/repos = %d calls, want 1", len(creates))
	}
	c := creates[0]
	if c.auth != "token test-token" {
		t.Errorf("create auth = %q, want the token header", c.auth)
	}
	if c.body["name"] != "p-abc" || c.body["private"] != true || c.body["auto_init"] != false {
		t.Errorf("create body = %v, want name/private/auto_init=false", c.body)
	}
	if c.body["description"] != "POST project slug (Name)" {
		t.Errorf("create description = %v", c.body["description"])
	}
}

// TestGiteaEnsureRepositoryExisting: the lookup hit short-circuits — no
// create call, and an already-private repository is adopted as-is (no
// repair PATCH).
func TestGiteaEnsureRepositoryExisting(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
		{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusOK,
			body: `{"id":42,"full_name":"post-git-svc/p-abc","owner":{"login":"post-git-svc"},"clone_url":"u","private":true}`},
	}}
	a := newGiteaAdapter(t, f)

	repo, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{Name: "p-abc", Private: true})
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if repo.ID != 42 || !repo.Private {
		t.Errorf("repo = %+v, want id 42 private", repo)
	}
	if got := f.requests(http.MethodPost, "/api/v1/user/repos"); len(got) != 0 {
		t.Errorf("existing repo triggered %d creates, want 0", len(got))
	}
	if got := f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-abc"); len(got) != 0 {
		t.Errorf("private repo triggered %d repair PATCHes, want 0", len(got))
	}
}

// TestGiteaEnsureRepositoryAdoptsAndRepairsPublicRepo: a pre-existing
// PUBLIC repository of the spec's name is adopted, and the adopt path
// repairs its Git-layer visibility (PATCH private) instead of handing it
// back public — and the repair failure surfaces instead of the repo.
func TestGiteaEnsureRepositoryAdoptsAndRepairsPublicRepo(t *testing.T) {
	t.Run("repair", func(t *testing.T) {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
			{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusOK,
				body: `{"id":42,"full_name":"post-git-svc/p-abc","owner":{"login":"post-git-svc"},"clone_url":"u","private":false}`},
			{method: http.MethodPatch, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusOK, body: `{}`},
		}}
		a := newGiteaAdapter(t, f)

		repo, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{Name: "p-abc", Private: true})
		if err != nil {
			t.Fatalf("EnsureRepository: %v", err)
		}
		if !repo.Private {
			t.Error("adopted repo = public, want the repaired private visibility")
		}
		patches := f.requests(http.MethodPatch, "/api/v1/repos/post-git-svc/p-abc")
		if len(patches) != 1 {
			t.Fatalf("repair PATCH = %d calls, want 1", len(patches))
		}
		if v, ok := patches[0].body["private"].(bool); !ok || !v {
			t.Errorf("repair PATCH body = %v, want {\"private\":true}", patches[0].body)
		}
		if got := f.requests(http.MethodPost, "/api/v1/user/repos"); len(got) != 0 {
			t.Errorf("adopt path issued %d creates, want 0", len(got))
		}
	})

	t.Run("repair failure surfaces", func(t *testing.T) {
		f := &fakeGitea{routes: []giteaRoute{
			{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
			{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusOK,
				body: `{"id":42,"full_name":"post-git-svc/p-abc","owner":{"login":"post-git-svc"},"clone_url":"u","private":false}`},
			{method: http.MethodPatch, prefix: "/api/v1/repos/post-git-svc/p-abc", status: http.StatusForbidden, body: `{"message":"no permission"}`},
		}}
		a := newGiteaAdapter(t, f)

		_, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{Name: "p-abc", Private: true})
		if !errors.Is(err, gitprovider.ErrUnauthorized) {
			t.Errorf("repair failure = %v, want ErrUnauthorized (a public repo must not be adopted silently)", err)
		}
	})
}

// TestGiteaEnsureRepositoryCreateRace: a 409 on create (a concurrent
// provisioner won) re-reads and returns the existing repository.
func TestGiteaEnsureRepositoryCreateRace(t *testing.T) {
	var lookups int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/user":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"login":"post-git-svc"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/repos/post-git-svc/p-abc":
			lookups++
			if lookups == 1 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":77,"full_name":"post-git-svc/p-abc","owner":{"login":"post-git-svc"},"clone_url":"u","private":true}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/user/repos":
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"message":"repo already exists"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	a := gitprovider.NewGiteaAdapter(gitprovider.Config{BaseURL: srv.URL, Token: config.Secret("test-token")})

	repo, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{Name: "p-abc", Private: true})
	if err != nil {
		t.Fatalf("EnsureRepository: %v", err)
	}
	if repo.ID != 77 {
		t.Errorf("race repo id = %d, want 77 (the winner's id)", repo.ID)
	}
}

// TestGiteaEnsureRepositoryErrorMapping: provider statuses map onto the
// port's sentinels with the provider message attached.
func TestGiteaEnsureRepositoryErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   error
		msg    string // expected provider-message fragment, "" = none
	}{
		{"422 name invalid", http.StatusUnprocessableEntity, `{"message":"name is not valid"}`, gitprovider.ErrConflict, "name is not valid"},
		{"401 token revoked", http.StatusUnauthorized, `{"message":"token is revoked"}`, gitprovider.ErrUnauthorized, "token is revoked"},
		{"500 exploded", http.StatusInternalServerError, ``, gitprovider.ErrUnavailable, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeGitea{routes: []giteaRoute{
				{method: http.MethodGet, prefix: "/api/v1/user", status: http.StatusOK, body: `{"login":"post-git-svc"}`},
				{method: http.MethodGet, prefix: "/api/v1/repos/post-git-svc/p-x", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
				{method: http.MethodPost, prefix: "/api/v1/user/repos", status: tc.status, body: tc.body},
			}}
			a := newGiteaAdapter(t, f)

			_, err := a.EnsureRepository(testCtx(t), gitprovider.RepositorySpec{Name: "p-x", Private: true})
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if tc.msg != "" && !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error %q lost the provider message %q", err, tc.msg)
			}
		})
	}
}

// TestGiteaGetRepositoryNotFound: the lookup 404 maps to ErrNotFound.
func TestGiteaGetRepositoryNotFound(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
	}}
	a := newGiteaAdapter(t, f)

	_, err := a.GetRepository(testCtx(t), "o", "n")
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("GetRepository error = %v, want ErrNotFound", err)
	}
}

// TestGiteaPathEscaping: owner/name go into the path through urlSegment, so
// an exotic name can never inject path structure.
func TestGiteaPathEscaping(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o", status: http.StatusNotFound, body: `{}`},
	}}
	a := newGiteaAdapter(t, f)

	_, _ = a.GetRepository(testCtx(t), "o w", "a/b")
	if len(f.reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(f.reqs))
	}
	if got := f.reqs[0].escapedPath; got != "/api/v1/repos/o%20w/a%2Fb" {
		t.Errorf("escaped path = %q, want /api/v1/repos/o%%20w/a%%2Fb", got)
	}
}

// TestGiteaEnsureWebhookCreates: the first provisioning posts exactly one
// gitea-type hook with the secret in the config (Gitea never returns it,
// so the request body is the only place it exists provider-side).
func TestGiteaEnsureWebhookCreates(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusOK, body: `[]`},
		{method: http.MethodPost, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusCreated, body: `{"id":9,"active":true}`},
	}}
	a := newGiteaAdapter(t, f)

	hook, err := a.EnsureWebhook(testCtx(t), gitprovider.WebhookSpec{
		Repository: gitprovider.Repository{Owner: "o", Name: "n", ID: 42},
		URL:        "http://host/api/v1/git/hooks/gitea",
		Secret:     "deadbeef",
		Events:     []string{"push"},
	})
	if err != nil {
		t.Fatalf("EnsureWebhook: %v", err)
	}
	if hook.ID != 9 || !hook.Active {
		t.Errorf("hook = %+v, want id 9 active", hook)
	}

	creates := f.requests(http.MethodPost, "/api/v1/repos/o/n/hooks")
	if len(creates) != 1 {
		t.Fatalf("POST hooks = %d calls, want 1", len(creates))
	}
	c := creates[0]
	if c.body["type"] != "gitea" || c.body["active"] != true {
		t.Errorf("hook body type/active = %v", c.body)
	}
	if evs, ok := c.body["events"].([]any); !ok || len(evs) != 1 || evs[0] != "push" {
		t.Errorf("hook events = %v, want [push]", c.body["events"])
	}
	cfgBody, ok := c.body["config"].(map[string]any)
	if !ok {
		t.Fatalf("hook config = %v", c.body["config"])
	}
	if cfgBody["url"] != "http://host/api/v1/git/hooks/gitea" || cfgBody["content_type"] != "json" || cfgBody["secret"] != "deadbeef" {
		t.Errorf("hook config = %v", cfgBody)
	}
	if patches := f.requests(http.MethodPatch, "/api/v1/repos/o/n/hooks"); len(patches) != 0 {
		t.Errorf("create path issued %d PATCHes, want 0", len(patches))
	}
}

// TestGiteaEnsureWebhookUpdatesInPlace: a re-provisioning finds the existing
// hook by URL and PATCHes it (rotated secret) instead of stacking a second
// hook; hooks for other URLs are left alone.
func TestGiteaEnsureWebhookUpdatesInPlace(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusOK, body: `[
			{"id":9,"type":"gitea","active":true,"config":{"url":"http://host/api/v1/git/hooks/gitea"}},
			{"id":10,"type":"gitea","active":true,"config":{"url":"http://other/url"}}
		]`},
		{method: http.MethodPatch, prefix: "/api/v1/repos/o/n/hooks/9", status: http.StatusOK, body: `{"id":9,"active":true}`},
	}}
	a := newGiteaAdapter(t, f)

	hook, err := a.EnsureWebhook(testCtx(t), gitprovider.WebhookSpec{
		Repository: gitprovider.Repository{Owner: "o", Name: "n", ID: 42},
		URL:        "http://host/api/v1/git/hooks/gitea",
		Secret:     "rotated-secret",
		Events:     []string{"push"},
	})
	if err != nil {
		t.Fatalf("EnsureWebhook: %v", err)
	}
	if hook.ID != 9 {
		t.Errorf("hook id = %d, want 9 (the existing one, updated)", hook.ID)
	}
	patches := f.requests(http.MethodPatch, "/api/v1/repos/o/n/hooks/9")
	if len(patches) != 1 {
		t.Fatalf("PATCH hooks/9 = %d calls, want 1", len(patches))
	}
	if cfg, ok := patches[0].body["config"].(map[string]any); !ok || cfg["secret"] != "rotated-secret" {
		t.Errorf("PATCH body config = %v, want the rotated secret", patches[0].body["config"])
	}
	if creates := f.requests(http.MethodPost, "/api/v1/repos/o/n/hooks"); len(creates) != 0 {
		t.Errorf("update path issued %d creates, want 0 (no duplicate hooks)", len(creates))
	}
	if patches10 := f.requests(http.MethodPatch, "/api/v1/repos/o/n/hooks/10"); len(patches10) != 0 {
		t.Errorf("the other URL's hook was touched (%d PATCHes)", len(patches10))
	}
}

// TestGiteaEnsureWebhookRepoGone: the hook list 404 maps to ErrNotFound.
func TestGiteaEnsureWebhookRepoGone(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusNotFound, body: `{"message":"Not Found"}`},
	}}
	a := newGiteaAdapter(t, f)

	_, err := a.EnsureWebhook(testCtx(t), gitprovider.WebhookSpec{
		Repository: gitprovider.Repository{Owner: "o", Name: "n"},
		URL:        "http://host/x", Secret: "s", Events: []string{"push"},
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("EnsureWebhook error = %v, want ErrNotFound", err)
	}
}

// TestGiteaWebhookCreateErrorMapping: a failed hook create maps statuses.
func TestGiteaWebhookCreateErrorMapping(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusOK, body: `[]`},
		{method: http.MethodPost, prefix: "/api/v1/repos/o/n/hooks", status: http.StatusForbidden, body: `{"message":"no permission"}`},
	}}
	a := newGiteaAdapter(t, f)

	_, err := a.EnsureWebhook(testCtx(t), gitprovider.WebhookSpec{
		Repository: gitprovider.Repository{Owner: "o", Name: "n"},
		URL:        "http://host/x", Secret: "s", Events: []string{"push"},
	})
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Errorf("EnsureWebhook error = %v, want ErrUnauthorized", err)
	}
}
