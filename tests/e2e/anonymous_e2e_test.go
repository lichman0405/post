package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// T0801-TEST "anonymous e2e" (blocking), the API half:
//
//	go test ./tests/e2e -run TestE2EAnonymousReleaseReads
//
// T0801 says a public entity's page is readable, and indexable, by a reader
// who is not signed in. On the web side that is
// tests/e2e-anonymous/anonymous-e2e.mjs, which reads the served pages as a
// crawler and as a signed-out visitor. On this side it is the API answer
// those pages are built from — and the release surface was the one entity
// kind with no anonymous coverage:
//
//   - projects: TestE2EPrivacyNegative (anonymous list = public only,
//     anonymous read of a private id = the existence-hiding 404,
//     anonymous writes 401);
//   - assets:   TestE2EAssetPageAnonymous (the anonymous asset page);
//   - profiles: TestE2EProfileJourney (the anonymous public profile);
//   - releases: tests/integration/release_e2e_test.go asserts the
//     ANONYMOUS 404 of a PRIVATE project's release. The other half — that
//     an anonymous caller reads a PUBLIC project's release, and its
//     manifest, at 200 — was asserted by nothing, which is what this test
//     adds.
//
// The claim it pins is the one releasehttp/release_handlers.go reader()
// states: "anonymous stays anonymous — a public project's releases are
// public". Everything on the read path is the production composition: the
// real guard (cmd/api/authhttp), the real project read matrix
// (projectshttp's *projects.Service, the same value the release surface is
// wired to as its visibility gate), the real release handlers. Only the
// release STORE is a stand-in (anonReleaseStore below): no in-memory
// release store exists, and the real one needs PostgreSQL — where
// tests/integration covers it.

// anonReleaseStore is the release command slice the handlers read from: a
// fixed set of releases, plus a count of writes, so the test can assert a
// read is a read.
type anonReleaseStore struct {
	byProject map[string][]domain.Release
	manifest  []byte
	creates   int
}

func (s *anonReleaseStore) Create(context.Context, domain.User, releases.CreateReleaseParams) (domain.Release, error) {
	s.creates++
	return domain.Release{}, releases.ErrForbidden
}

func (s *anonReleaseStore) List(_ context.Context, projectID string) ([]domain.Release, error) {
	return s.byProject[projectID], nil
}

func (s *anonReleaseStore) Get(_ context.Context, projectID, releaseID string) (domain.Release, error) {
	for _, rel := range s.byProject[projectID] {
		if rel.ID == releaseID {
			return rel, nil
		}
	}
	return domain.Release{}, releases.ErrReleaseNotFound
}

func (s *anonReleaseStore) Manifest(_ context.Context, projectID, releaseID string) ([]byte, error) {
	if _, err := s.Get(context.Background(), projectID, releaseID); err != nil {
		return nil, err
	}
	return s.manifest, nil
}

// newAnonE2EEnv composes the deployment the way cmd/api/main.go does for
// the surfaces this test needs: authhttp + projectshttp + releasehttp on
// one v1 mux, guarded once, mounted under /api/v1/.
func newAnonE2EEnv(t *testing.T, store *anonReleaseStore) *e2eEnv {
	t.Helper()
	users := memstore.NewUsers()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: nil,
		Cfg:        defaultCfg(),
		Secure:     false,
	})
	projectsStore := memstore.NewProjects()
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectsStore,
		Orgs:  projectsStore.Gate(),
		Authz: authz.NewMatrixEngine(),
	})
	releaseAPI := releasehttp.New(releasehttp.Deps{
		Command: store,
		// The production adapter: the same service the project surface
		// serves, so the visibility rule under test is the real one.
		Projects: projectAPI.Service(),
	})

	v1 := http.NewServeMux()
	authAPI.Register(v1)
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	releaseAPI.Register(v1)

	ts := httptest.NewServer(authAPI.Guard(v1))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = redisClient.Close() })
	jar, _ := cookiejar.New(nil)
	return &e2eEnv{
		api:   ts,
		redis: mr,
		users: users,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// The public release, and the one a stranger must never learn exists. The
// version and title of the second are canaries: neither may appear in a
// byte the guard lets through.
const (
	anonPublicVersion = "v1.0.0"
	anonPublicTitle   = "Screening results"
	anonSecretVersion = "v9.9.9-secret"
	anonSecretTitle   = "Secret Zeolite Snapshot"
)

// anonUnknownReleaseID names no release under any project.
const anonUnknownReleaseID = "00000000-0000-4000-8000-000000000000"

// TestE2EAnonymousReleaseReads drives the anonymous release surface:
//
//   - 登录墙不挡 public research: a signed-out caller reads a public
//     project's release list (200), the release itself (200, the full
//     payload), and the manifest bytes (200, byte-identical);
//   - private 不索引: the release of a private project answers the
//     existence-hiding 404 — for the list, the release and the manifest
//     alike — and no byte of the response names the release, so an id that
//     is guessed correctly is worth no more than one that is invented;
//   - the surface stays read-only for that caller: a write is 401 at the
//     guard and Create is never called.
func TestE2EAnonymousReleaseReads(t *testing.T) {
	manifest := []byte("{\"canonical\":\"release-manifest\"}\n")
	store := &anonReleaseStore{byProject: map[string][]domain.Release{}, manifest: manifest}
	env := newAnonE2EEnv(t, store)

	// Alice signs up over the wire and creates one public and one private
	// project — the two worlds the anonymous reader can and cannot see.
	alice := e2eSignup(t, env, `{"email":"anon-alice@example.com","password":"long-enough-password-1","handle":"anon-alice","display_name":"Alice"}`)
	openID := e2eCreateProject(t, env, alice.CSRFToken,
		`{"slug":"anon-open-lab","name":"Open MOF Lab","purpose":"public MOF screening","visibility":"public"}`)
	secretID := e2eCreateProject(t, env, alice.CSRFToken,
		`{"slug":"anon-secret-lab","name":"Secret Zeolite Lab","purpose":"unpublished zeolite work","visibility":"private"}`)

	openRelease := domain.Release{
		ID: "11111111-2222-4333-8444-555555555555", ProjectID: openID,
		Version: anonPublicVersion, Title: anonPublicTitle,
		StateID: "11111111-2222-4333-8444-666666666666", ManifestHash: "sha256:5f5f",
	}
	secretRelease := domain.Release{
		ID: "77777777-2222-4333-8444-555555555555", ProjectID: secretID,
		Version: anonSecretVersion, Title: anonSecretTitle,
		StateID: "77777777-2222-4333-8444-666666666666", ManifestHash: "sha256:9a9a",
	}
	store.byProject[openID] = []domain.Release{openRelease}
	store.byProject[secretID] = []domain.Release{secretRelease}

	// From here on, nobody is signed in: a fresh jar is the anonymous
	// caller — the same state a crawler, or a reader who has never logged
	// in, arrives in.
	env.client.Jar = freshJar()

	// --- the public release is public ------------------------------------
	relPath := "/api/v1/projects/" + openID + "/releases"
	resp := env.do(t, http.MethodGet, relPath, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET %s = %d, want 200: %s", relPath, resp.StatusCode, bodyBytes(t, resp))
	}
	var listed struct {
		Releases []struct {
			ID      string `json:"id"`
			Version string `json:"version"`
			Title   string `json:"title"`
		} `json:"releases"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &listed); err != nil {
		t.Fatalf("anonymous release list body: %v", err)
	}
	if len(listed.Releases) != 1 || listed.Releases[0].ID != openRelease.ID ||
		listed.Releases[0].Version != anonPublicVersion || listed.Releases[0].Title != anonPublicTitle {
		t.Fatalf("anonymous release list = %+v, want the one public release %s", listed.Releases, openRelease.ID)
	}

	resp = env.do(t, http.MethodGet, relPath+"/"+openRelease.ID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET one release = %d, want 200: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	raw := string(bodyBytes(t, resp))
	var got struct {
		ID           string `json:"id"`
		ProjectID    string `json:"project_id"`
		Version      string `json:"version"`
		Title        string `json:"title"`
		StateID      string `json:"state_id"`
		ManifestHash string `json:"manifest_hash"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("anonymous release body: %v (%s)", err, raw)
	}
	if got.ID != openRelease.ID || got.ProjectID != openID || got.Version != anonPublicVersion ||
		got.Title != anonPublicTitle || got.StateID != openRelease.StateID ||
		got.ManifestHash != openRelease.ManifestHash {
		t.Errorf("anonymous release = %+v, want %+v", got, openRelease)
	}

	resp = env.do(t, http.MethodGet, relPath+"/"+openRelease.ID+"/manifest", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET manifest = %d, want 200: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	if served := bodyBytes(t, resp); string(served) != string(manifest) {
		t.Errorf("anonymous manifest = %q, want %q", served, manifest)
	}

	// --- the private project's release is not ----------------------------
	//
	// Every read under it answers the PROJECT gate's 404 — the same
	// existence-hiding answer an unknown project id gets (docs/45), not a
	// release-shaped 404 that would confirm the project exists. The code is
	// the release surface's own (RELEASE_PROJECT_NOT_FOUND, not the project
	// surface's PROJECT_NOT_FOUND), which is also what proves the release
	// route — not the project subtree it coexists with — answered.
	secretPath := "/api/v1/projects/" + secretID + "/releases"
	for _, path := range []string{secretPath, secretPath + "/" + secretRelease.ID, secretPath + "/" + secretRelease.ID + "/manifest"} {
		resp = env.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("anonymous GET %s = %d, want 404 (existence hiding): %s", path, resp.StatusCode, bodyBytes(t, resp))
			continue
		}
		body := string(bodyBytes(t, resp))
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil {
			t.Errorf("anonymous GET %s envelope: %v (%s)", path, err, body)
			continue
		}
		if envelope.Code != releases.CodeReleaseProjectNotFound {
			t.Errorf("anonymous GET %s code = %q, want %q", path, envelope.Code, releases.CodeReleaseProjectNotFound)
		}
		for _, canary := range []string{
			anonSecretVersion, anonSecretTitle, secretRelease.ID, secretRelease.ManifestHash,
			"Zeolite", "anon-secret-lab",
		} {
			if strings.Contains(body, canary) {
				t.Errorf("anonymous GET %s leaks %q: %s", path, canary, body)
			}
		}
	}

	// A release id that names nothing, under a PUBLIC project: the release
	// surface's own 404 — the "no such release" answer, which by itself
	// confirms nothing about any other project.
	for _, path := range []string{relPath + "/" + anonUnknownReleaseID, relPath + "/" + anonUnknownReleaseID + "/manifest"} {
		resp = env.do(t, http.MethodGet, path, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("anonymous GET %s = %d, want 404: %s", path, resp.StatusCode, bodyBytes(t, resp))
			continue
		}
		body := string(bodyBytes(t, resp))
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(body), &envelope); err != nil || envelope.Code != releases.CodeReleaseNotFound {
			t.Errorf("anonymous GET %s envelope = %s (err %v), want %s", path, body, err, releases.CodeReleaseNotFound)
		}
	}

	// --- and the reads were reads ----------------------------------------
	if store.creates != 0 {
		t.Errorf("the anonymous reads reached Create %d time(s)", store.creates)
	}
	resp = env.do(t, http.MethodPost, relPath, `{"version":"v2.0.0"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST release = %d, want 401: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	if store.creates != 0 {
		t.Errorf("an anonymous POST reached Create %d time(s), want 0", store.creates)
	}
}

// TestE2EAnonymousReleaseHidesThePrivateProject pins the property that
// makes guessing harmless: on the release surface, the release of a
// private project and a project that exists nowhere must be the SAME
// answer — same status, same code, same message — for every caller who
// may not see the project. A difference between the two is a private
// project's existence, disclosed one id at a time, which is exactly what
// docs/45 forbids.
//
// The second caller class matters: the answer must be about the entity,
// not about the caller. A signed-in non-member is still a stranger to this
// project, and releasehttp's reader() hands the gate THIS caller's reader
// — so the refusal cannot be a privilege of not having logged in, and it
// cannot change the moment a stranger signs up. (It is not a claim that
// signing in grants access: the assertion below is that nothing about the
// answer moves, in either direction.)
func TestE2EAnonymousReleaseHidesThePrivateProject(t *testing.T) {
	store := &anonReleaseStore{byProject: map[string][]domain.Release{}}
	env := newAnonE2EEnv(t, store)
	alice := e2eSignup(t, env, `{"email":"anon-carol@example.com","password":"long-enough-password-1","handle":"anon-carol","display_name":"Carol"}`)
	privateID := e2eCreateProject(t, env, alice.CSRFToken,
		`{"slug":"carol-secret","name":"Carol Hidden Lab","purpose":"private work","visibility":"private"}`)
	store.byProject[privateID] = []domain.Release{{
		ID: "88888888-2222-4333-8444-555555555555", ProjectID: privateID,
		Version: anonSecretVersion, Title: anonSecretTitle,
	}}

	paths := []string{
		"/api/v1/projects/" + privateID + "/releases",
		"/api/v1/projects/" + anonUnknownReleaseID + "/releases",
	}

	// "The same answer" means the whole envelope but the request id: two
	// envelopes that differ in any field a caller can read are two
	// distinguishable situations.
	type answer struct {
		status  int
		code    string
		message string
	}
	read := func(label string) map[string]answer {
		t.Helper()
		out := map[string]answer{}
		for _, path := range paths {
			resp := env.do(t, http.MethodGet, path, "", nil)
			body := string(bodyBytes(t, resp))
			var envelope struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal([]byte(body), &envelope); err != nil {
				t.Fatalf("%s GET %s: envelope: %v (%s)", label, path, err, body)
			}
			out[path] = answer{status: resp.StatusCode, code: envelope.Code, message: envelope.Message}
		}
		return out
	}

	check := func(label string, got map[string]answer) {
		t.Helper()
		private, unknown := got[paths[0]], got[paths[1]]
		if private != unknown {
			t.Errorf("%s: a private project answered %+v while an unknown one answered %+v — the difference discloses the private project", label, private, unknown)
		}
		if private.status != http.StatusNotFound || private.code != releases.CodeReleaseProjectNotFound {
			t.Errorf("%s: private project's release = %+v, want 404 %s", label, private, releases.CodeReleaseProjectNotFound)
		}
		for _, canary := range []string{anonSecretVersion, anonSecretTitle, "Carol Hidden Lab", "carol-secret"} {
			if strings.Contains(private.message, canary) {
				t.Errorf("%s: the refusal names the private project (%q): %s", label, canary, private.message)
			}
		}
	}

	// As the anonymous caller, and then as a signed-in stranger to the
	// project (Bob has an account and no role in Carol's project).
	env.client.Jar = freshJar()
	check("anonymous", read("anonymous"))
	e2eSignup(t, env, `{"email":"anon-bob@example.com","password":"long-enough-password-1","handle":"anon-bob","display_name":"Bob"}`)
	check("signed-in non-member", read("signed-in non-member"))
}
