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
	// serve, when set, decides the response itself (returning true when it
	// handled the request) — for paths whose answer must change between
	// calls (e.g. the adopt-first miss and the later read-back hit).
	serve func(w http.ResponseWriter, r *http.Request) bool
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
			if rt.serve != nil {
				if rt.serve(w, r) {
					return
				}
				continue
			}
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
func newGiteaAdapter(t *testing.T, f *fakeGitea, opts ...gitprovider.AdapterOption) *gitprovider.GiteaAdapter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return gitprovider.NewGiteaAdapter(gitprovider.Config{
		BaseURL: srv.URL,
		Token:   config.Secret("test-token"),
	}, opts...)
}

// gitCall is one recorded git CLI invocation (the scripted runner's input).
type gitCall struct {
	args []string
	env  []string
}

// envValue returns the value of one environment entry, or "" when absent.
func (c gitCall) envValue(key string) string {
	for _, kv := range c.env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			return v
		}
	}
	return ""
}

// scriptedGit builds a GitRunner whose every invocation is answered by the
// next step (extra invocations fail the test), and recorded for
// assertions.
func scriptedGit(t *testing.T, steps ...func(call gitCall) (string, error)) gitprovider.GitRunner {
	t.Helper()
	i := 0
	return func(_ context.Context, env []string, args ...string) (string, error) {
		if i >= len(steps) {
			t.Fatalf("unexpected git call #%d: %v", i+1, args)
		}
		call := gitCall{args: args, env: env}
		out, err := steps[i](call)
		i++
		return out, err
	}
}

// okGit answers one git invocation with success.
func okGit(gitCall) (string, error) { return "", nil }

// failGit answers one git invocation with a failure carrying the given
// output text (the surface mapGitErr classifies).
func failGit(out string) func(gitCall) (string, error) {
	return func(gitCall) (string, error) { return out, errors.New("git: exit status 128") }
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

// --- branch refs (T0303) -----------------------------------------------------

const testBranchHeadSHA = "0123456789abcdef0123456789abcdef01234567"
const testForkHeadSHA = "fedcba9876543210fedcba9876543210fedcba98"

func testRepo() gitprovider.Repository {
	return gitprovider.Repository{Owner: "o", Name: "n"}
}

// refsBody is the refs-API answer shape (an ARRAY — checked against the
// running instance).
func refsBody(name, sha string) string {
	return `[{"ref":"refs/heads/` + name + `","object":{"type":"commit","sha":"` + sha + `"}}]`
}

// servingRef answers successive GETs of one ref endpoint with the given
// bodies in order ("" = 404), then repeats the last — e.g. the adopt-first
// miss and the later read-back hit on the same path.
func servingRef(name string, bodies ...string) giteaRoute {
	i := 0
	return giteaRoute{
		method: http.MethodGet,
		prefix: "/api/v1/repos/o/n/git/refs/heads/" + name,
		serve: func(w http.ResponseWriter, _ *http.Request) bool {
			body := bodies[i]
			if i < len(bodies)-1 {
				i++
			}
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return true
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(body))
			return true
		},
	}
}

// recordGit is a scriptedGit step that records the call and succeeds.
func recordGit(calls *[]gitCall) func(gitCall) (string, error) {
	return func(c gitCall) (string, error) { *calls = append(*calls, c); return "", nil }
}

// TestGiteaEnsureBranchCreated: a create with an explicit fork sha goes
// over the git protocol — the fork commit is shallow-fetched (the git
// client needs the object locally) and pushed to the new ref — and the
// provider-side head is read back through the refs API. The token travels
// only in the environment, never in argv.
func TestGiteaEnsureBranchCreated(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", "", refsBody("feature-x", testBranchHeadSHA)),
	}}
	var calls []gitCall
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		recordGit(&calls), recordGit(&calls), recordGit(&calls))))

	ref, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{
		Repository: testRepo(), Name: "feature-x", ForkRef: testBranchHeadSHA,
	})
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if ref.Name != "feature-x" || ref.HeadSHA != testBranchHeadSHA {
		t.Errorf("ref = %+v, want feature-x @ %s", ref, testBranchHeadSHA)
	}
	if got := len(calls); got != 3 {
		t.Fatalf("git calls = %d, want 3 (init, fetch, push): %+v", got, calls)
	}
	if got := calls[0].args[2:]; !strings.HasPrefix(got[0], "init") {
		t.Errorf("call 0 = %v, want the scratch bare init", got)
	}
	if got := calls[1].args[2:]; len(got) != 4 || got[0] != "fetch" || got[1] != "--depth=1" ||
		!strings.HasSuffix(got[2], "/o/n.git") || got[3] != testBranchHeadSHA {
		t.Errorf("fetch call = %v, want [fetch --depth=1 <url>/o/n.git %s]", got, testBranchHeadSHA)
	}
	if got := calls[2].args[2:]; len(got) != 3 || got[0] != "push" ||
		!strings.HasSuffix(got[1], "/o/n.git") || got[2] != testBranchHeadSHA+":refs/heads/feature-x" {
		t.Errorf("push call = %v, want [push <url>/o/n.git %s:refs/heads/feature-x]", got, testBranchHeadSHA)
	}
	for _, c := range calls {
		if got := c.envValue("GIT_CONFIG_VALUE_0"); got != "Authorization: token test-token" {
			t.Errorf("git env token = %q, want the Authorization header value", got)
		}
		if strings.Contains(strings.Join(c.args, " "), "test-token") {
			t.Error("the token reached git argv — it must ride in the environment only")
		}
	}
	if got := len(f.requests(http.MethodGet, "/api/v1/repos/o/n/git/refs/heads/feature-x")); got != 2 {
		t.Errorf("refs GET hits = %d, want 2 (adopt-first miss, read-back)", got)
	}
}

// TestGiteaDeleteBranchAbsentRefIsSuccess: the git client refuses the
// delete of a nonexistent remote ref ("remote ref does not exist"), so
// exactly that refusal maps to success — the port contract makes a
// missing ref "the goal already achieved" (a redelivered close job must
// not fail on work that is already done). Real refusals still map.
func TestGiteaDeleteBranchAbsentRefIsSuccess(t *testing.T) {
	f := &fakeGitea{}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		failGit("error: unable to delete 'x': remote ref does not exist\nerror: failed to push some refs"))))
	if err := a.DeleteBranch(testCtx(t), testRepo(), "x"); err != nil {
		t.Errorf("DeleteBranch on an already-absent ref = %v, want success", err)
	}

	a2 := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		failGit("remote: Access denied"))))
	if err := a2.DeleteBranch(testCtx(t), testRepo(), "x"); !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Errorf("DeleteBranch on an unauthorized provider = %v, want ErrUnauthorized", err)
	}
}

// TestGiteaGitEnvStripsAmbientGitNamespace: an ambient GIT_* variable in
// the API process (GIT_DIR, GIT_OBJECT_DIRECTORY,
// GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_WORK_TREE, GIT_ASKPASS, ...) would
// redirect the adapter's git invocation away from its per-operation
// scratch repository — the hermeticity promise is that the GIT_*
// namespace of the subprocess is the adapter's alone.
func TestGiteaGitEnvStripsAmbientGitNamespace(t *testing.T) {
	ambient := []string{"GIT_DIR", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_WORK_TREE", "GIT_ASKPASS", "GIT_SSH"}
	for _, k := range ambient {
		t.Setenv(k, "/ambient/"+strings.ToLower(k))
	}
	var calls []gitCall
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		recordGit(&calls), recordGit(&calls))))
	if err := a.DeleteBranch(testCtx(t), testRepo(), "x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	for i, c := range calls {
		for _, k := range ambient {
			if got := c.envValue(k); got != "" {
				t.Errorf("call %d: ambient %s=%q reached the git subprocess", i, k, got)
			}
		}
		if got := c.envValue("GIT_CONFIG_VALUE_0"); got != "Authorization: token test-token" {
			t.Errorf("call %d: token env = %q, want the adapter's own slot", i, got)
		}
	}
}

// TestGiteaEnsureBranchResolvesDefaultBranch: an empty fork ref resolves
// the repository's default branch through the refs API first, then forks
// at its tip.
func TestGiteaEnsureBranchResolvesDefaultBranch(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", "", refsBody("feature-x", testForkHeadSHA)),
		servingRef("main", refsBody("main", testForkHeadSHA)),
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n", status: http.StatusOK,
			body: `{"id":1,"full_name":"o/n","default_branch":"main","private":true}`},
	}}
	var calls []gitCall
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		recordGit(&calls), recordGit(&calls), recordGit(&calls))))

	ref, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{Repository: testRepo(), Name: "feature-x"})
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if ref.HeadSHA != testForkHeadSHA {
		t.Errorf("ref head = %q, want the default branch's tip %s", ref.HeadSHA, testForkHeadSHA)
	}
	if got := len(calls); got != 3 {
		t.Fatalf("git calls = %d, want 3", got)
	}
	if got := calls[1].args[2:]; len(got) != 4 || got[3] != testForkHeadSHA {
		t.Errorf("fetch call = %v, want the fork at main's tip %s", got, testForkHeadSHA)
	}
	if got := calls[2].args[2:]; len(got) != 3 || got[2] != testForkHeadSHA+":refs/heads/feature-x" {
		t.Errorf("push call = %v, want the fork at main's tip", got)
	}
}

// TestGiteaEnsureBranchEmptyRepository: no refs at all (T0301's
// auto_init=false) — ErrNotFound, and no git call is attempted.
func TestGiteaEnsureBranchEmptyRepository(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", ""),
		{method: http.MethodGet, prefix: "/api/v1/repos/o/n", status: http.StatusOK,
			body: `{"id":1,"full_name":"o/n","default_branch":"","private":true}`},
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{Repository: testRepo(), Name: "feature-x"})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("EnsureBranch error = %v, want ErrNotFound", err)
	}
}

// TestGiteaEnsureBranchMissingForkRef: the fork ref does not exist
// provider-side — ErrNotFound (the syncer keeps the row retryable).
func TestGiteaEnsureBranchMissingForkRef(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", ""),
		servingRef("ghost", ""),
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	_, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{
		Repository: testRepo(), Name: "feature-x", ForkRef: "ghost",
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("EnsureBranch error = %v, want ErrNotFound", err)
	}
}

// TestGiteaEnsureBranchAdoptsExisting: the ref already exists — adopted
// through the refs API, no git call.
func TestGiteaEnsureBranchAdoptsExisting(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", refsBody("feature-x", testBranchHeadSHA)),
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t)))

	ref, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{
		Repository: testRepo(), Name: "feature-x", ForkRef: testBranchHeadSHA,
	})
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if ref.HeadSHA != testBranchHeadSHA {
		t.Errorf("adopted head = %q, want %s", ref.HeadSHA, testBranchHeadSHA)
	}
	if got := len(f.requests(http.MethodGet, "/api/v1/repos/o/n/git/refs/heads/feature-x")); got != 1 {
		t.Errorf("adopt GET hits = %d, want 1", got)
	}
}

// TestGiteaEnsureBranchAdoptsConcurrentWinner: a racing creator lands the
// ref between our read and our push — the rejected push adopts instead of
// failing (the sync is idempotent, a redelivered job never errors).
func TestGiteaEnsureBranchAdoptsConcurrentWinner(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", "", refsBody("feature-x", testForkHeadSHA)),
	}}
	var calls []gitCall
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		recordGit(&calls), recordGit(&calls), func(c gitCall) (string, error) {
			calls = append(calls, c)
			return "! [rejected]        " + testBranchHeadSHA + " -> feature-x (non-fast-forward)\n" +
				"error: failed to push some refs", errors.New("git: exit status 1")
		})))

	ref, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{
		Repository: testRepo(), Name: "feature-x", ForkRef: testBranchHeadSHA,
	})
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if ref.HeadSHA != testForkHeadSHA {
		t.Errorf("adopted head = %q, want the raced ref's %s", ref.HeadSHA, testForkHeadSHA)
	}
}

// TestGiteaEnsureBranchUnknownForkSHA: the fork sha the canonical store
// recorded does not exist provider-side — the fetch fails with the
// provider's "not our ref", mapped onto ErrNotFound.
func TestGiteaEnsureBranchUnknownForkSHA(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", ""),
	}}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		failGit("fatal: remote error: upload-pack: not our ref 1111111111111111111111111111111111111111"),
	)))

	_, err := a.EnsureBranch(testCtx(t), gitprovider.BranchSpec{
		Repository: testRepo(), Name: "feature-x", ForkRef: "1111111111111111111111111111111111111111",
	})
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("EnsureBranch error = %v, want ErrNotFound", err)
	}
}

// TestGiteaGetBranchNotFound: a missing ref is ErrNotFound.
func TestGiteaGetBranchNotFound(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", ""),
	}}
	a := newGiteaAdapter(t, f)

	_, err := a.GetBranch(testCtx(t), testRepo(), "feature-x")
	if !errors.Is(err, gitprovider.ErrNotFound) {
		t.Errorf("GetBranch error = %v, want ErrNotFound", err)
	}
}

// TestGiteaGetBranchReadsRefsAPI: the ref is read through the refs API
// (the /branches API cannot answer for pushed refs on the deployed
// instance), and its tip sha is the object sha.
func TestGiteaGetBranchReadsRefsAPI(t *testing.T) {
	f := &fakeGitea{routes: []giteaRoute{
		servingRef("feature-x", refsBody("feature-x", testBranchHeadSHA)),
	}}
	a := newGiteaAdapter(t, f)

	ref, err := a.GetBranch(testCtx(t), testRepo(), "feature-x")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if ref.Name != "feature-x" || ref.HeadSHA != testBranchHeadSHA {
		t.Errorf("ref = %+v, want feature-x @ %s", ref, testBranchHeadSHA)
	}
}

// TestGiteaDeleteBranch: the ref is deleted by a push with an empty source
// — and the protocol itself is idempotent (deleting a missing ref
// succeeds), so the close strategy "the ref must not exist" holds without
// any existence check.
func TestGiteaDeleteBranch(t *testing.T) {
	var calls []gitCall
	a := newGiteaAdapter(t, &fakeGitea{}, gitprovider.WithGitRunner(scriptedGit(t,
		recordGit(&calls), recordGit(&calls))))

	if err := a.DeleteBranch(testCtx(t), testRepo(), "feature-x"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if got := len(calls); got != 2 {
		t.Fatalf("git calls = %d, want 2 (init, push)", got)
	}
	if got := calls[1].args[2:]; len(got) != 3 || got[0] != "push" ||
		!strings.HasSuffix(got[1], "/o/n.git") || got[2] != ":refs/heads/feature-x" {
		t.Errorf("push call = %v, want [push <url>/o/n.git :refs/heads/feature-x]", got)
	}
}

// TestGiteaDeleteBranchErrorMapping: the git protocol's failure surface
// maps onto the port's sentinels — a protected-ref refusal is ErrConflict
// (the request cannot be satisfied as specified), a credential refusal is
// ErrUnauthorized.
func TestGiteaDeleteBranchErrorMapping(t *testing.T) {
	f := &fakeGitea{}
	a := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		failGit("remote: protected branch hook declined to update refs/heads/feature-x"),
	)))
	err := a.DeleteBranch(testCtx(t), testRepo(), "feature-x")
	if !errors.Is(err, gitprovider.ErrConflict) {
		t.Errorf("DeleteBranch (protected) error = %v, want ErrConflict", err)
	}

	a2 := newGiteaAdapter(t, f, gitprovider.WithGitRunner(scriptedGit(t,
		okGit,
		failGit("fatal: could not read Username for 'http://127.0.0.1:3000': terminal prompts disabled"),
	)))
	err = a2.DeleteBranch(testCtx(t), testRepo(), "feature-x")
	if !errors.Is(err, gitprovider.ErrUnauthorized) {
		t.Errorf("DeleteBranch (credentials) error = %v, want ErrUnauthorized", err)
	}
}
