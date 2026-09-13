package e2e

import (
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
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The privacy e2e suite runs the T0106 read-isolation surface the way a
// browser/API client does: real HTTP servers and clients, the production
// guard + handlers (cmd/api/authhttp + cmd/api/projectshttp), real Redis
// protocol (miniredis) and one in-memory project space (memstore implements
// the projects.ProjectStore + projects.OrgGate ports — the production graph
// over PostgreSQL is covered by tests/integration, where the SQL visibility
// filter itself is exercised).
//
// T0106-TEST-01 "privacy negative e2e" (blocking):
//
//	go test ./tests/e2e -run TestE2EPrivacyNegative
//
// The negative path is the point: every assertion below tries to make a
// private project visible to someone who must not see it — anonymous
// listing, anonymous id guessing, authenticated non-member reads — and
// checks the refusal, not the happy path.

// newPrivacyE2EEnv mirrors cmd/api main.go's product wiring: one guarded
// /api/v1 mux carrying the auth surface and the project surface — the
// exact composition the production binary mounts, with the in-memory
// project store in place of PostgreSQL.
func newPrivacyE2EEnv(t *testing.T, cfg authn.Config) *e2eEnv {
	t.Helper()
	users := memstore.NewUsers()
	mr := miniredis.RunT(t)
	redisClient := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	authAPI := authhttp.New(authhttp.Deps{
		Users:      users,
		Sessions:   persistence.NewRedisSessionStore(redisClient),
		Limiter:    persistence.NewRedisRateLimiter(redisClient),
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	projects := memstore.NewProjects()
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projects,
		Orgs:  projects.Gate(),
		Authz: authz.NewMatrixEngine(),
	})
	v1 := http.NewServeMux()
	authAPI.Register(v1)
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
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

// e2eCreateProject drives one authenticated project creation over the wire
// and returns the created project's id.
func e2eCreateProject(t *testing.T, env *e2eEnv, csrf, body string) string {
	t.Helper()
	resp := env.do(t, http.MethodPost, "/api/v1/projects", body, map[string]string{"X-CSRF-Token": csrf})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create project = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var payload struct {
		Project struct {
			ID string `json:"id"`
		} `json:"project"`
	}
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil {
		t.Fatalf("create project body: %v", err)
	}
	if payload.Project.ID == "" {
		t.Fatal("create project did not return an id")
	}
	return payload.Project.ID
}

type e2eProjectListPayload struct {
	Projects []struct {
		ID         string `json:"id"`
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		Visibility string `json:"visibility"`
	} `json:"projects"`
}

// e2eListProjects GETs the project list and decodes it; anything but 200 is
// a test failure.
func e2eListProjects(t *testing.T, env *e2eEnv) e2eProjectListPayload {
	t.Helper()
	resp := env.do(t, http.MethodGet, "/api/v1/projects", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list projects = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	var payload e2eProjectListPayload
	if err := json.Unmarshal(bodyBytes(t, resp), &payload); err != nil {
		t.Fatalf("list projects body: %v", err)
	}
	return payload
}

// TestE2EPrivacyNegative drives the T0106 acceptance criteria over the
// wire:
//
//   - 匿名搜索/list 不出现 private: the anonymous list contains the public
//     project only — the private projects' ids, slugs and names never
//     appear anywhere in the response body;
//   - 直接猜 ID 不泄漏 metadata: an anonymous (or non-member) direct GET of
//     a private project id answers the same existence-hiding 404 an unknown
//     id produces, with no project metadata anywhere in the body;
//
// plus the positive half of the requirement (匿名可读 public project
// shell) and the structural backstop (anonymous writes are still 401 at
// the guard).
func TestE2EPrivacyNegative(t *testing.T) {
	env := newPrivacyE2EEnv(t, defaultCfg())

	// Alice signs up and creates one public and one private project.
	aliceJar := env.client.Jar
	alice := e2eSignup(t, env, `{"email":"privacy-alice@example.com","password":"long-enough-password-1","handle":"privacy-alice","display_name":"Alice"}`)
	openID := e2eCreateProject(t, env, alice.CSRFToken,
		`{"slug":"open-lab","name":"Open MOF Lab","purpose":"public MOF screening","visibility":"public"}`)
	secretID := e2eCreateProject(t, env, alice.CSRFToken,
		`{"slug":"secret-lab","name":"Secret Zeolite Lab","purpose":"unpublished zeolite work","visibility":"private"}`)

	// Bob signs up on a fresh browser and creates his own private project.
	env.client.Jar = freshJar()
	bob := e2eSignup(t, env, `{"email":"privacy-bob@example.com","password":"long-enough-password-1","handle":"privacy-bob","display_name":"Bob"}`)
	bobJar := env.client.Jar
	bobSecretID := e2eCreateProject(t, env, bob.CSRFToken,
		`{"slug":"bob-secret","name":"Bob Hidden Lab","purpose":"bob private research","visibility":"private"}`)

	// --- anonymous list: public only ------------------------------------
	// Acceptance: 匿名 list 不出现 private — not just structurally absent
	// from the decoded payload, but absent from the raw response body (no
	// id, slug, name or purpose string of any private project anywhere).
	env.client.Jar = freshJar()
	list := e2eListProjects(t, env)
	found := map[string]bool{}
	for _, p := range list.Projects {
		found[p.ID] = true
		if p.ID == secretID || p.ID == bobSecretID {
			t.Errorf("anonymous list leaked private project %s (%s)", p.ID, p.Slug)
		}
		if p.Visibility == "private" {
			t.Errorf("anonymous list contains a private-visibility entry: %+v", p)
		}
	}
	if !found[openID] {
		t.Errorf("anonymous list is missing the public project: %+v", list.Projects)
	}
	if len(list.Projects) != 1 {
		t.Errorf("anonymous list has %d entries, want exactly the 1 public project: %+v", len(list.Projects), list.Projects)
	}
	resp := env.do(t, http.MethodGet, "/api/v1/projects", "", nil)
	if raw := string(bodyBytes(t, resp)); strings.Contains(raw, "secret-lab") ||
		strings.Contains(raw, "Secret Zeolite Lab") || strings.Contains(raw, "unpublished zeolite work") ||
		strings.Contains(raw, "bob-secret") || strings.Contains(raw, "Bob Hidden Lab") ||
		strings.Contains(raw, secretID) || strings.Contains(raw, bobSecretID) {
		t.Errorf("anonymous list body leaks private project metadata: %s", raw)
	}

	// --- anonymous direct reads: public open, private hidden ------------
	// Anonymous can read the public shell (requirement: 匿名可读 public
	// project shell).
	resp = env.do(t, http.MethodGet, "/api/v1/projects/"+openID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET public = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	// Acceptance: 直接猜 ID 不泄漏 metadata — a guessed private id (and a
	// guessed unknown id) answer the identical existence-hiding 404
	// envelope, and the bodies carry no project metadata.
	for _, id := range []string{secretID, bobSecretID, "00000000-0000-4000-8000-000000000000"} {
		resp = env.do(t, http.MethodGet, "/api/v1/projects/"+id, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("anonymous GET %s = %d, want 404 (existence hiding): %s", id, resp.StatusCode, bodyBytes(t, resp))
		}
		raw := string(bodyBytes(t, resp))
		var envelope struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal([]byte(raw), &envelope); err != nil || envelope.Code != "PROJECT_NOT_FOUND" {
			t.Errorf("anonymous GET %s envelope = %s (err %v), want PROJECT_NOT_FOUND", id, raw, err)
		}
		if strings.Contains(raw, "secret-lab") || strings.Contains(raw, "bob-secret") ||
			strings.Contains(raw, "Zeolite") || strings.Contains(raw, "Hidden") {
			t.Errorf("anonymous 404 for %s leaks project metadata: %s", id, raw)
		}
	}

	// --- anonymous writes stay refused at the guard ---------------------
	resp = env.do(t, http.MethodPost, "/api/v1/projects", `{"slug":"sneaky","name":"Sneaky","purpose":"x","visibility":"public"}`, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST = %d, want 401", resp.StatusCode)
	}

	// --- authenticated non-member: private stays hidden -----------------
	// Bob is signed in: his list gains his own project, the public project
	// stays visible, and Alice's private project stays out of both the
	// list and the direct read.
	env.client.Jar = bobJar
	bobList := e2eListProjects(t, env)
	bobFound := map[string]bool{}
	for _, p := range bobList.Projects {
		bobFound[p.ID] = true
		if p.ID == secretID {
			t.Errorf("bob's list leaked alice's private project: %+v", p)
		}
	}
	if !bobFound[openID] || !bobFound[bobSecretID] {
		t.Errorf("bob's list = %+v, want public open-lab + his own bob-secret", bobList.Projects)
	}
	resp = env.do(t, http.MethodGet, "/api/v1/projects/"+secretID, "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("bob GET alice's private = %d, want 404 (existence hiding): %s", resp.StatusCode, bodyBytes(t, resp))
	}

	// --- the owner still reads her private project ----------------------
	env.client.Jar = aliceJar
	resp = env.do(t, http.MethodGet, "/api/v1/projects/"+secretID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("owner GET private = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
}
