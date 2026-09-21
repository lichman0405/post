package security

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/feedshttp"
	"github.com/lichman0405/post/cmd/api/fileshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/cmd/api/searchhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/answer"
	"github.com/lichman0405/post/internal/search/ranking"
	"github.com/lichman0405/post/internal/search/retrieval"
	edgesec "github.com/lichman0405/post/internal/security"
)

// Each registered exit is driven through the handler that produces it, and
// the headers that come back are judged — twice, on purpose:
//
//	bare  the handler alone. This is where the exit OWNS its headers: an
//	      exit that only looks safe because a layer above adds nosniff is
//	      an exit that is unsafe everywhere else it is mounted.
//	edged the same handler under internal/security.Headers(), which is how
//	      every response leaves the process. This is where the whole
//	      response is judged (JudgeExit), including the policy the edge
//	      picks from the media type.
//
// The two are not redundant. The bare probe is what makes the mutations the
// task requires ("attachment becomes inline", "nosniff deleted") fail; the
// edged probe is what makes "the composed response is a conforming byte
// exit" a statement about the process rather than about a handler.

const (
	probeProjectID = "11111111-2222-4333-8444-555555555555"
	probeObjectID  = "obj-1"
	probeBranchID  = "br-1"
)

// probe is one request against one handler, with what the registry says the
// response must look like.
type probe struct {
	name string
	key  string // registry key
	// build returns the handler as the production code composes it, plus
	// the request to send.
	build func(t *testing.T) http.Handler
	req   func(t *testing.T) *http.Request
	// contentType is the exact Content-Type the response must carry.
	contentType string
	// disposition is the exact Content-Disposition value, or "" when the
	// response must carry none at all.
	disposition string
	// cacheControl is the exact Cache-Control the response must carry, or ""
	// when the probe makes no claim about caching. Most exits are responses
	// a cache may keep and they say nothing about it; the search answer is
	// the one row whose Wire promises no-store, and a promise no probe
	// measures is documentation rather than a check — the same reason this
	// table exists beside the registry at all.
	cacheControl string
	// kind is the exit classification JudgeExit must reach on the edged
	// response.
	kind edgesec.ExitKind
	// status is the status the probe expects; the headers are asserted on
	// this response, whatever it is.
	status int
}

func TestRegisteredExitsSendTheHeadersTheyDeclare(t *testing.T) {
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			if _, ok := registry[p.key]; !ok {
				t.Fatalf("probe %q names %q, which is not in the byte-exit registry", p.name, p.key)
			}

			// ---- the handler on its own -------------------------------
			bare := do(t, p.build(t), securityHeadersOff(p.req(t)))
			if bare.Code != p.status {
				t.Fatalf("bare handler answered %d, want %d (body %s)",
					bare.Code, p.status, truncate(bare.Body.String()))
			}
			if got := bare.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("bare handler sent X-Content-Type-Options: %q, want %q — the exit must "+
					"state it itself, not inherit it from the edge above", got, "nosniff")
			}
			if got := bare.Header().Get("Content-Type"); got != p.contentType {
				t.Errorf("bare handler sent Content-Type: %q, want %q", got, p.contentType)
			}
			if got := bare.Header().Get("Content-Disposition"); got != p.disposition {
				t.Errorf("bare handler sent Content-Disposition: %q, want %q", got, p.disposition)
			}
			if p.cacheControl != "" {
				if got := bare.Header().Get("Cache-Control"); got != p.cacheControl {
					t.Errorf("bare handler sent Cache-Control: %q, want %q — nothing above this exit sets "+
						"it, so the row's promise is only kept if the exit does", got, p.cacheControl)
				}
			}

			// ---- the composed chain -----------------------------------
			edged := do(t, edgesec.Headers()(p.build(t)), p.req(t))
			if edged.Code != p.status {
				t.Fatalf("edged handler answered %d, want %d", edged.Code, p.status)
			}
			kind, err := edgesec.JudgeExit(edged.Header())
			if err != nil {
				t.Fatalf("the composed response is not a conforming byte exit: %v\nheaders: %s",
					err, dumpHeaders(edged.Header()))
			}
			if string(kind) != string(p.kind) {
				t.Errorf("composed exit kind = %q, want %q", kind, p.kind)
			}
		})
	}

	// The registry's Kind column must agree with the probes: a row whose
	// kind nobody probes is a classification nothing checks.
	probed := map[string]bool{}
	for _, p := range probes {
		probed[p.key] = true
		if want := registry[p.key]; string(want.Kind) != string(p.kind) {
			t.Errorf("registry says %s is %q, the probe says %q", p.key, want.Kind, p.kind)
		}
		// A probe whose row declares a different wire shape than the probe
		// asserts is two answers to one question.
		if p.disposition != "" && p.kind != edgesec.ExitOpaque {
			t.Errorf("%s asserts a disposition but is classified %q: only an opaque exit "+
				"is required to say it is a download", p.key, p.kind)
		}
	}

	// Every exit whose headers a mistake could turn into an execution
	// channel must be driven at runtime — the attachment exits (a wrong
	// disposition renders the bytes) and the document exits (a missing
	// policy lets script run). The remaining rows are the JSON envelope
	// writers, whose media type is not negotiable and whose shape is pinned
	// by TestEveryRegisteredExitStatesItsOwnHeaders above and by the
	// envelope contract tests in their own packages.
	var unprobed []string
	for key, site := range registry {
		if probed[key] || site.Kind == KindInlineText {
			continue
		}
		unprobed = append(unprobed, key)
	}
	if len(unprobed) > 0 {
		sort.Strings(unprobed)
		t.Errorf("these exits are classified without ever being measured:\n  %s\n\n"+
			"Add a probe that drives the real handler and asserts what it sends.",
			strings.Join(unprobed, "\n  "))
	}
}

var probes = []probe{
	{
		name:        "the canonical JSON envelope",
		key:         "authhttp/envelope.go:WriteJSON",
		contentType: "application/json",
		kind:        edgesec.ExitInlineText,
		status:      http.StatusOK,
		build: func(t *testing.T) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authhttp.WriteJSON(w, http.StatusOK, map[string]string{"ok": "true"})
			})
		},
		req: func(t *testing.T) *http.Request { return httptest.NewRequest(http.MethodGet, "/x", nil) },
	},
	{
		name:        "the error envelope",
		key:         "authhttp/envelope.go:WriteError",
		contentType: "application/json",
		kind:        edgesec.ExitInlineText,
		status:      http.StatusTeapot,
		build: func(t *testing.T) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				authhttp.WriteError(w, r, http.StatusTeapot, "TEAPOT", "no")
			})
		},
		req: func(t *testing.T) *http.Request { return httptest.NewRequest(http.MethodGet, "/x", nil) },
	},
	{
		name:        "the search answer",
		key:         "searchhttp/handlers.go:writeSearchJSON",
		contentType: "application/json; charset=utf-8",
		// Cache-Control is asserted here and not in the registry: an answer
		// belongs to the actor who asked, so it must not be kept by a shared
		// cache, and no layer above the exit sets the header (the edge does
		// not touch caching).
		cacheControl: "no-store",
		kind:         edgesec.ExitInlineText,
		status:       http.StatusOK,
		build:        func(t *testing.T) http.Handler { return searchHandler(t) },
		req: func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/api/v1/search",
				strings.NewReader(`{"query":"co2 uptake"}`))
			r.Header.Set("Content-Type", "application/json")
			// A write route under the guard carries the session's CSRF token
			// beside the cookie; the probe session's token is the one the
			// store below answers with.
			r.Header.Set("X-CSRF-Token", probeCSRF)
			r.AddCookie(&http.Cookie{Name: "post_session", Value: probeSessionToken})
			return r
		},
	},
	{
		name:        "the raw download channel",
		key:         "fileshttp/files.go:handleRaw",
		contentType: "application/octet-stream",
		disposition: `attachment; filename="notes.md"`,
		kind:        edgesec.ExitOpaque,
		status:      http.StatusOK,
		build: func(t *testing.T) http.Handler {
			return filesHandler(t)
		},
		req: func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/"+probeProjectID+"/files/raw?ref=main&path=notes.md", nil)
		},
	},
	{
		name:        "the raw patch channel",
		key:         "fileshttp/files.go:handleDiff",
		contentType: "text/plain; charset=utf-8",
		kind:        edgesec.ExitInlineText,
		status:      http.StatusOK,
		build: func(t *testing.T) http.Handler {
			return filesHandler(t)
		},
		req: func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/"+probeProjectID+"/files/diff?sha="+probeSHA, nil)
		},
	},
	{
		name:        "the release manifest",
		key:         "releasehttp/release_handlers.go:handleGetReleaseManifest",
		contentType: "application/json",
		disposition: `attachment; filename="release-1.2.3.manifest.json"`,
		kind:        edgesec.ExitOpaque,
		status:      http.StatusOK,
		build: func(t *testing.T) http.Handler {
			return releaseHandler(t)
		},
		req: func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/"+probeProjectID+"/releases/rel-1/manifest", nil)
		},
	},
	{
		name:        "an Atom feed",
		key:         "feedshttp/handlers.go:writeDocument",
		contentType: "application/atom+xml; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusOK,
		build:       func(t *testing.T) http.Handler { return feedHandler(t) },
		req: func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet,
				"/api/v1/feeds/projects/"+probeProjectID, nil)
		},
	},
	{
		name:        "an RSS feed",
		key:         "feedshttp/handlers.go:writeDocument",
		contentType: "application/rss+xml; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusOK,
		build:       func(t *testing.T) http.Handler { return feedHandler(t) },
		req: func(t *testing.T) *http.Request {
			return httptest.NewRequest(http.MethodGet,
				"/api/v1/feeds/projects/"+probeProjectID+"?format=rss", nil)
		},
	},
	{
		name:        "the project overview page",
		key:         "rsghttp/overview.go:handleOverviewPage",
		contentType: "text/html; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusOK,
		build:       func(t *testing.T) http.Handler { return rsgHandler(t) },
		req: func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+probeProjectID+"/overview", nil)
			r.Header.Set("Accept", "text/html,application/xhtml+xml")
			return r
		},
	},
	{
		name:        "the research page",
		key:         "rsghttp/outline.go:handleResearchPage",
		contentType: "text/html; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusOK,
		build:       func(t *testing.T) http.Handler { return rsgHandler(t) },
		req: func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/"+probeProjectID+"/research", nil)
			r.Header.Set("Accept", "text/html")
			return r
		},
	},
	{
		name:        "the object detail page",
		key:         "rsghttp/page.go:handleObjectDetailPage",
		contentType: "text/html; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusOK,
		build:       func(t *testing.T) http.Handler { return rsgHandler(t) },
		req: func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/"+probeProjectID+"/branches/"+probeBranchID+"/objects/"+probeObjectID, nil)
			r.Header.Set("Accept", "text/html")
			return r
		},
	},
	{
		name:        "the HTML error page",
		key:         "rsghttp/page.go:renderObjectPageError",
		contentType: "text/html; charset=utf-8",
		kind:        edgesec.ExitDocument,
		status:      http.StatusNotFound,
		build:       func(t *testing.T) http.Handler { return rsgHandler(t) },
		req: func(t *testing.T) *http.Request {
			r := httptest.NewRequest(http.MethodGet,
				"/api/v1/projects/"+probeProjectID+"/branches/"+probeBranchID+"/objects/missing-object", nil)
			r.Header.Set("Accept", "text/html")
			return r
		},
	},
}

// ---- harness ----------------------------------------------------------

func do(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// securityHeadersOff exists so that the bare probe is unambiguous: it is the
// handler's own response, with nothing added. It is a named function rather
// than an inline identity so a reader of the probe cannot mistake the bare
// path for "the same thing, minus an accident".
func securityHeadersOff(r *http.Request) *http.Request { return r }

func dumpHeaders(h http.Header) string {
	var b strings.Builder
	for k, v := range h {
		b.WriteString(k + ": " + strings.Join(v, ", ") + "; ")
	}
	return b.String()
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

// probeSHA is a syntactically valid commit sha (ValidateCommitSHA accepts
// 7..40 lowercase hex).
const probeSHA = "0123456789abcdef0123456789abcdef01234567"

// ---- fileshttp --------------------------------------------------------

func filesHandler(t *testing.T) http.Handler {
	t.Helper()
	store := &probeProjectStore{project: domain.Project{
		ID: probeProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPublic,
	}}
	reader := gitprovider.NewFilesReader(probeFilesPort{}, probeResolver{})
	api := fileshttp.New(fileshttp.Deps{
		Projects: projects.NewService(store, probeOrgGate{}, authz.NewMatrixEngine()),
		Files:    reader,
	})
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

// probeProjectStore answers the read gate for one public project, so the
// probes exercise the anonymous reader — the caller class with no session
// to lean on.
type probeProjectStore struct {
	projects.ProjectStore
	project domain.Project
}

func (s *probeProjectStore) GetProject(_ context.Context, projectID string) (domain.Project, error) {
	if s.project.ID == projectID {
		return s.project, nil
	}
	return domain.Project{}, projects.ErrProjectNotFound
}

func (s *probeProjectStore) GetMembership(context.Context, string, string) (domain.ProjectMembership, error) {
	return domain.ProjectMembership{}, projects.ErrMemberNotFound
}

type probeOrgGate struct{ projects.OrgGate }

type probeResolver struct{}

func (probeResolver) RepoRef(context.Context, string) (gitprovider.RepoRef, error) {
	return gitprovider.RepoRef{ProjectID: probeProjectID, Owner: "post-git-svc", Name: "p-" + probeProjectID}, nil
}

// probeFilesPort serves scripted provider bytes: the payload is real enough
// to be misread (an HTML document with a script tag and a CSV), which is
// what makes the disposition assertions mean something.
type probeFilesPort struct {
	gitprovider.FilesPort
}

const probeRawBody = "<html><body><script>alert(1)</script></body></html>\n"

func (probeFilesPort) GetRaw(context.Context, gitprovider.Repository, string, string) (int64, io.ReadCloser, error) {
	return int64(len(probeRawBody)), io.NopCloser(strings.NewReader(probeRawBody)), nil
}

func (probeFilesPort) GetCommitPatch(context.Context, gitprovider.Repository, string) (int64, io.ReadCloser, error) {
	const patch = "diff --git a/notes.md b/notes.md\n+<script>alert(1)</script>\n"
	return int64(len(patch)), io.NopCloser(strings.NewReader(patch)), nil
}

// ---- releasehttp ------------------------------------------------------

func releaseHandler(t *testing.T) http.Handler {
	t.Helper()
	store := &probeProjectStore{project: domain.Project{
		ID: probeProjectID, Slug: "lab", Name: "Lab", Visibility: domain.VisibilityPublic,
	}}
	api := releasehttp.New(releasehttp.Deps{
		Command:  probeReleaseCommand{},
		Projects: projects.NewService(store, probeOrgGate{}, authz.NewMatrixEngine()),
	})
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

type probeReleaseCommand struct{ releasehttp.CommandPort }

func (probeReleaseCommand) Get(context.Context, string, string) (domain.Release, error) {
	return domain.Release{ID: "rel-1", ProjectID: probeProjectID, Version: "1.2.3",
		CreatedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}, nil
}

func (probeReleaseCommand) Manifest(context.Context, string, string) ([]byte, error) {
	return []byte(`{"version":"1.2.3","schema_version":"9"}`), nil
}

// ---- feedshttp --------------------------------------------------------

func feedHandler(t *testing.T) http.Handler {
	t.Helper()
	api := feedshttp.New(feedshttp.Deps{Service: probeFeedService{}})
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

type probeFeedService struct{}

func (probeFeedService) Document(_ context.Context, target feeds.Target, format feeds.Format) ([]byte, error) {
	if format == feeds.FormatRSS {
		return []byte(`<?xml version="1.0" encoding="utf-8"?><rss version="2.0"></rss>`), nil
	}
	return []byte(`<?xml version="1.0" encoding="utf-8"?><feed xmlns="http://www.w3.org/2005/Atom"></feed>`), nil
}

// ---- rsghttp ----------------------------------------------------------

func rsgHandler(t *testing.T) http.Handler {
	t.Helper()
	api := rsghttp.New(rsghttp.Deps{Service: probeRSGService{}})
	mux := http.NewServeMux()
	api.Register(mux)
	return mux
}

// probeRSGService embeds the production interface and overrides the three
// reads the page probes exercise; every other method is a nil call, which
// would panic loudly rather than answer something plausible.
type probeRSGService struct{ rsghttp.Service }

func (probeRSGService) ProjectOverview(context.Context, projects.Reader, string) (rsg.ProjectOverview, error) {
	return rsg.ProjectOverview{
		ProjectID: probeProjectID, ProjectSlug: "lab", ProjectName: "Lab",
		Purpose: "a public project", Visibility: "public", ActivityStatus: "active",
	}, nil
}

func (probeRSGService) ResearchOutline(context.Context, projects.Reader, string) (rsg.ResearchOutline, error) {
	return rsg.ResearchOutline{ProjectID: probeProjectID, ProjectSlug: "lab", ProjectName: "Lab"}, nil
}

func (probeRSGService) GetObjectDetail(_ context.Context, _ projects.Reader, projectID, branchID, objectID string, _ *int) (rsg.ObjectDetail, error) {
	if objectID != probeObjectID {
		// The handler's own read decided the object is missing; the page
		// answers the HTML error page instead, which is the
		// renderObjectPageError exit.
		return rsg.ObjectDetail{}, sciobjects.ErrObjectNotFound
	}
	version := domain.ScientificObjectVersion{
		ID: "ver-1", ObjectID: objectID, VersionNo: 1, StateID: "state-1",
		Title: "A sample", LifecycleState: domain.LifecycleActive,
		Payload: []byte(`{"material":"MOF-5"}`),
	}
	return rsg.ObjectDetail{
		Project:  domain.Project{ID: projectID, Slug: "lab", Name: "Lab"},
		Branch:   domain.Branch{ID: branchID, ProjectID: projectID, Name: "main"},
		Object:   domain.ScientificObject{ID: objectID, ProjectID: projectID, ObjectType: "sample", CurrentVersionNo: 1},
		Versions: []domain.ScientificObjectVersion{version},
		Selected: version,
	}, nil
}

// ---- searchhttp -------------------------------------------------------

// The search probe drives the route the way cmd/api mounts it: the search
// surface behind the REAL auth guard.
//
// The guard is not scaffolding here. It is the only thing that can put an
// actor into the request context from outside the package — the handler asks
// authhttp.PrincipalID, and nothing but the guard writes that value — so a
// probe that injected one another way would be measuring a composition
// production never builds. Its three ports are answered below in place of
// Redis and PostgreSQL; the pipeline's ports are answered beside them.
const (
	probeSessionToken = "probe-session-token"
	probeCSRF         = "probe-csrf-token"
	probeActorID      = "6a1f0d3e-1c2b-4a5d-8e9f-0a1b2c3d4e5f"
	probeSearchID     = "9f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	probeAssetRef     = "asset:AST-0001@2"
)

func searchHandler(t *testing.T) http.Handler {
	t.Helper()
	// The REAL answer layer, with no provider configured: that is the
	// supported state of a deployment that has not answered "may platform
	// content go to a model", and it always produces a document (the
	// structured fallback, generator.go). What a model would have written is
	// pinned by tests/answer's fixtures; this probe is about the exit.
	answerer, err := answer.New(answer.Deps{})
	if err != nil {
		t.Fatalf("answer.New: %v", err)
	}

	v1 := http.NewServeMux()
	searchhttp.New(searchhttp.Deps{
		Scope:     probeSearchScope{},
		Retriever: probeRetriever{},
		Ranker:    probeRanker{},
		Answerer:  answerer,
		Records:   probeSearchRecords{},
	}).Register(v1)

	auth := authhttp.New(authhttp.Deps{
		Users: probeUserStore{}, Sessions: probeSessionStore{}, Limiter: probeLimiter{},
		Cfg: authn.Config{WebOrigin: "http://127.0.0.1:3000", SessionTTL: time.Hour},
	})
	return auth.Guard(v1)
}

// probeSessionStore answers the one lookup the guard makes for the probe's
// cookie.
type probeSessionStore struct{ authn.SessionStore }

func (probeSessionStore) Get(_ context.Context, token string) (authn.Session, error) {
	if token != probeSessionToken {
		return authn.Session{}, authn.ErrSessionNotFound
	}
	return authn.Session{
		Token: probeSessionToken, UserID: probeActorID, CSRFToken: probeCSRF,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// probeUserStore answers the account lookup behind that session.
type probeUserStore struct{ authn.UserStore }

func (probeUserStore) GetByID(_ context.Context, id string) (authn.UserRecord, error) {
	if id != probeActorID {
		return authn.UserRecord{}, authn.ErrUserNotFound
	}
	return authn.UserRecord{User: domain.User{
		ID: probeActorID, Handle: "probe", Email: "probe@post.test",
	}}, nil
}

// probeLimiter allows every check: the search route takes no rate-limit
// decision of its own, and the limit that could refuse a request is not what
// this probe measures.
type probeLimiter struct{ authn.RateLimiter }

func (probeLimiter) Check(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return true, 0, nil
}

// probeSearchScope puts the probe's actor in one project.
// search.ResolveScope scans the id as a uuid column, so the fixture is a
// uuid.
type probeSearchScope struct{ search.ProjectScopeReader }

func (probeSearchScope) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	return []domain.Project{{ID: probeProjectID, Name: "Lab", Visibility: domain.VisibilityPrivate}}, nil
}

// probeRetriever recalls one candidate and reports the signals a deployment
// with no embedder reports.
type probeRetriever struct{ searchhttp.Retriever }

func (probeRetriever) Retrieve(_ context.Context, _ search.Scope, req retrieval.Request) (retrieval.Result, error) {
	return retrieval.Result{Query: req.Query, Signals: []retrieval.SignalReport{
		{Signal: retrieval.SignalFullText, Ran: true, Hits: 1},
		{Signal: retrieval.SignalVector, Skipped: retrieval.SkippedNoEmbedder},
		{Signal: retrieval.SignalFacets, Skipped: retrieval.SkippedNoFacets},
		{Signal: retrieval.SignalGraph, Ran: true},
	}}, nil
}

// probeRanker orders that one candidate first.
type probeRanker struct{ searchhttp.Ranker }

func (probeRanker) Rank(_ context.Context, _ search.Scope, _ retrieval.Request, res retrieval.Result) (ranking.Result, error) {
	return ranking.Result{
		Query: res.Query, Factors: ranking.Factors(),
		Ranked: []ranking.Ranked{{
			Rank: 1, Ref: probeAssetRef, Kind: retrieval.KindDocument, EntityType: "asset",
			Title: "Mg-MOF-74 CO2 uptake", Version: "2",
			Candidate: retrieval.Candidate{
				Ref: probeAssetRef, Kind: retrieval.KindDocument, EntityType: "asset",
				Identity: "AST-0001", Version: "2", Title: "Mg-MOF-74 CO2 uptake",
				Visibility: "public", ProjectID: probeProjectID, ObjectType: "material",
			},
		}},
	}, nil
}

// probeSearchRecords answers the record write with an id: the search is
// recorded before it is answered, and a failure there is a 503 rather than
// the exit this probe is about.
type probeSearchRecords struct{ searchhttp.RecordWriter }

func (probeSearchRecords) Save(context.Context, persistence.SearchRecord) (string, error) {
	return probeSearchID, nil
}
