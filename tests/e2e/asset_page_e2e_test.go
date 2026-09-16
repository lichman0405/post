package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The asset hub e2e (T0709): the asset page and the browse list driven over
// real HTTP through the production composition — one guarded /api/v1 mux
// carrying the auth surface, the project surface and the asset surface,
// exactly the way cmd/api/main.go mounts them, so the guard, the routing
// and the project read gate are the shipped ones rather than test doubles.
//
// What is real here and what is not, stated plainly, because the difference
// is the difference between this suite and the integration one:
//
//   - real: the guard (an anonymous read flows through, a write does not),
//     the routing of every asset path, the handler's step order (parse →
//     state read → project read gate → membership read → build), the one
//     404 shared by five different situations, the closed four-type set of
//     the browse filter, and every disclosure rule — those live in
//     internal/assets.BuildPage/BuildBrowse, and this suite exercises them
//     through the wire.
//   - not real: the ROW source. The reader below is an in-memory fixture,
//     the same substitution tests/e2e/privacy_e2e_test.go makes for
//     projects; tests/integration/asset_page_test.go runs the real
//     PostgreSQL reader against the real schema. The two halves meet at the
//     JSON shape this suite asserts.
//
// The projects the assets belong to ARE real: they are created over the
// wire by a signed-up user, so the project read gate and the membership
// read answer from the real project service over a real store.

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

// The fixture pids. Twenty-six characters, the pid length
// (internal/assets/pid.go pidLen), in Crockford base32 — a pid of any other
// shape is refused by the route before a page is built, and the test would
// be measuring the 404 path instead of the page.
const (
	assetE2EPID           = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w1")
	assetE2EHiddenPID     = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w2")
	assetE2EOpenDepPID    = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w3")
	assetE2EAllPrivatePID = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w4")
	assetE2EUnresolvedPID = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w5")
	assetE2EUnknownPID    = assets.PID("01j9z6k3m4n5p6q7r8s9t0v1w9")
)

// assetE2EAlice is the fixture's publisher: the user who signs up, creates
// the projects and (in the store's fiction) published every version.
var assetE2EAlice = struct {
	ID          string
	Handle      string
	DisplayName string
}{
	ID:          "00000000-0000-4000-8000-00000000000a",
	Handle:      "assets-alice",
	DisplayName: "Alice Guo",
}

// The two project slugs and names the suite creates over the wire. They are
// also the identity the leak assertions search for, so they are declared
// once here.
const (
	assetE2EOpenSlug    = "open-materials-lab"
	assetE2EOpenName    = "Open Materials Lab"
	assetE2EPrivateSlug = "hidden-usage-lab"
	assetE2EPrivateName = "Hidden Usage Lab"
	assetE2EUserSlug    = "catalysis-group"
	assetE2EUserName    = "Catalysis Group"
)

// excerpt is the head of a response body for a failure that is about the
// document as a whole — a whole page printed into the test log buries the
// assertion that failed.
func excerpt(body string) string {
	const limit = 600
	if len(body) <= limit {
		return body
	}
	return body[:limit] + "…(" + itoa(len(body)) + " bytes)"
}

func itoa(n int) string { return strconv.Itoa(n) }

// leakContext is the window of a body around the first occurrence of a
// string that must not be in it: enough to see where the leak is, without
// printing a whole document into the test log.
func leakContext(body, secret string) string {
	i := strings.Index(body, secret)
	if i < 0 {
		return body
	}
	start := i - 140
	if start < 0 {
		start = 0
	}
	end := i + len(secret) + 140
	if end > len(body) {
		end = len(body)
	}
	return "…" + body[start:end] + "…"
}

// assetE2EUsers maps the fixture's users by id, as the reader answers them.
func assetE2EUsers() map[string]assets.PageUser {
	return map[string]assets.PageUser{
		assetE2EAlice.ID: {
			UserID:      assetE2EAlice.ID,
			Handle:      assetE2EAlice.Handle,
			DisplayName: assetE2EAlice.DisplayName,
		},
	}
}

// assetE2EManifest is a manifest document in the canonical stored shape
// (internal/assets.Manifest: version, asset_type, metadata,
// dependency_pins — the parser the page uses refuses unknown top-level
// fields, so a fixture with a field that parser does not know would render
// no metadata at all and the metadata assertions would be measuring
// nothing).
//
// The metadata block carries the four keys a material_collection requires
// plus one publisher-declared extra, and the pins are the fixture's three
// cases: a public version of a private project (dropped), a public version
// of a public project (rendered and linked), and a pid the repository has
// no row for (rendered as its own bytes).
func assetE2EManifest() json.RawMessage {
	return json.RawMessage(`{
		"version": 1,
		"asset_type": "material_collection",
		"metadata": {
			"purpose": "Crystallographic screening of MOF-5 synthesis routes",
			"member_refs": ["MOF-5/Zn4O(BDC)3"],
			"selection_criteria": "Every route with a reported BET surface area",
			"custodian": "Open Materials Lab",
			"cell_chemistry": "Zn4O(BDC)3"
		},
		"dependency_pins": [
			"01j9z6k3m4n5p6q7r8s9t0v1w2@7.1",
			"01j9z6k3m4n5p6q7r8s9t0v1w3@1.4",
			"01j9z6k3m4n5p6q7r8s9t0v1w5@0.1"
		]
	}`)
}

// assetE2ERights is a rights document in the stored shape (the internal/
// rights template, version 1). The reader returns it as raw bytes and it
// reaches the wire verbatim, which is what the assertion checks.
func assetE2ERights() json.RawMessage {
	return json.RawMessage(`{
		"version": 1,
		"standard_license_id": "CC-BY-4.0",
		"usage": {"commercial_use": "allowed", "attribution": "required"},
		"visibility": {"metadata": "project_policy", "data_access": "restricted"}
	}`)
}

// assetE2EFixture builds the reader's answer for the public asset. It is
// RAW — the private project, the private version, the private usage and the
// private event are all in here, and internal/assets.BuildPage is what
// decides each one.
//
// openProject is the asset's own project (created over the wire by the
// caller, so the read gate can answer for it), privateProject is the
// private project whose public version must NOT be linked from the
// dependencies or the lineage block, and publicUserProject is a second
// public project that uses the asset.
func assetE2EFixture(openProject, privateProject, publicUserProject domain.Project) assets.PageState {
	const (
		ver10 = "ver-1"
		ver20 = "ver-2"
		ver30 = "ver-3"
	)
	public := assets.VisibilityPublic
	private := assets.VisibilityPrivate

	v10 := assets.PageVersionState{
		ID:            ver10,
		Version:       "1.0",
		Visibility:    public,
		IntegrityHash: "sha256:1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f1f",
		PublishedBy:   assetE2EAlice.ID,
		PublishedAt:   time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
		Manifest:      assetE2EManifest(),
		RightsJSON:    assetE2ERights(),
		OriginRefs:    []string{"project:" + openProject.ID, "project:" + privateProject.ID},
	}
	v20 := assets.PageVersionState{
		ID:            ver20,
		Version:       "2.0",
		Visibility:    public,
		IntegrityHash: "sha256:2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f2f",
		PublishedBy:   assetE2EAlice.ID,
		PublishedAt:   time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
		Manifest:      assetE2EManifest(),
		RightsJSON:    assetE2ERights(),
		OriginRefs:    []string{"project:" + openProject.ID},
	}
	// The private version: a non-member must not see it anywhere, and must
	// not see a count standing in for it either.
	v30 := assets.PageVersionState{
		ID:            ver30,
		Version:       "3.0",
		Visibility:    private,
		IntegrityHash: "sha256:3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f3f",
		PublishedBy:   assetE2EAlice.ID,
		PublishedAt:   time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
		Manifest:      assetE2EManifest(),
		RightsJSON:    assetE2ERights(),
		OriginRefs:    []string{"project:" + openProject.ID},
	}

	return assets.PageState{
		Asset: assets.PageAssetState{
			ID:              "asset-row-1",
			PID:             assetE2EPID,
			Type:            assets.TypeMaterialCollection,
			Title:           "MOF-5 Synthesis Collection",
			Slug:            "mof5-synthesis",
			OriginProjectID: openProject.ID,
			CreatedAt:       time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC),
		},
		Project: assets.PageProjectState{
			ID: openProject.ID, Name: openProject.Name, Slug: openProject.Slug,
			Visibility: assets.VisibilityPublic,
		},
		Versions: []assets.PageVersionState{v10, v20, v30},
		Users:    assetE2EUsers(),
		Pins: map[assets.DependencyPin]assets.PagePinState{
			// A PUBLIC version in a PRIVATE project: the version axis says
			// linkable and the project axis says no. This pin separates the
			// two axes of mayLinkVersion, which is the whole reason the
			// check takes both.
			"01j9z6k3m4n5p6q7r8s9t0v1w2@7.1": {
				Resolved: true, Visibility: public, ProjectVisibility: private,
				Title: "Hidden Alloy Screen", Type: assets.TypeDataset,
				PID: assetE2EHiddenPID, Version: "7.1",
			},
			// Both axes public: rendered, with its title, type and url.
			"01j9z6k3m4n5p6q7r8s9t0v1w3@1.4": {
				Resolved: true, Visibility: public, ProjectVisibility: public,
				Title: "BDC Linker Protocol", Type: assets.TypeProtocol,
				PID: assetE2EOpenDepPID, Version: "1.4",
			},
			// The repository has no row for this pid: rendered for its own
			// bytes, with nothing resolved behind it.
			"01j9z6k3m4n5p6q7r8s9t0v1w5@0.1": {Resolved: false},
		},
		Refs: map[assets.OriginRef]assets.StoredRef{
			assets.OriginRef("project:" + openProject.ID): {
				Ref: assets.OriginRef("project:" + openProject.ID), Resolved: true,
				ProjectID: openProject.ID,
			},
			assets.OriginRef("project:" + privateProject.ID): {
				Ref: assets.OriginRef("project:" + privateProject.ID), Resolved: true,
				ProjectID: privateProject.ID, ProjectVisibility: private,
			},
		},
		Lineage: []assets.PageLineageState{
			{
				Relation: "derived_from",
				// The rendered version derives from a PUBLIC version of a
				// PRIVATE project: the edge is dropped whole, not blanked.
				Parent: assets.PageLineageEnd{
					VersionID: "hidden-ver", PID: assetE2EHiddenPID, Version: "7.1",
					Visibility: public, ProjectVisibility: private, Title: "Hidden Alloy Screen",
				},
				Child: assets.PageLineageEnd{VersionID: ver20, PID: assetE2EPID, Version: "2.0"},
			},
			{
				Relation: "supersedes",
				Parent: assets.PageLineageEnd{
					VersionID: ver10, PID: assetE2EPID, Version: "1.0",
					Visibility: public, ProjectVisibility: public,
					Title: "MOF-5 Synthesis Collection",
				},
				Child: assets.PageLineageEnd{VersionID: ver20, PID: assetE2EPID, Version: "2.0"},
			},
		},
		Usages: []assets.PageUsageState{
			{
				// A private project using a public asset: neither its name,
				// its slug nor its id may reach the page, and no count may
				// stand in for it.
				AssetVersionID: ver20, ProjectID: privateProject.ID,
				ProjectName: privateProject.Name, ProjectSlug: privateProject.Slug,
				ProjectVisibility: private, VisibilityOfUsage: public,
				DependencyType: "derived_from",
				CreatedAt:      time.Date(2026, 9, 11, 8, 0, 0, 0, time.UTC),
			},
			{
				AssetVersionID: ver20, ProjectID: publicUserProject.ID,
				ProjectName: publicUserProject.Name, ProjectSlug: publicUserProject.Slug,
				ProjectVisibility: public, VisibilityOfUsage: public,
				DependencyType: "derived_from",
				CreatedAt:      time.Date(2026, 9, 11, 8, 30, 0, 0, time.UTC),
			},
			{
				// A PRIVATE usage by a public project: the usage's own
				// declaration is the first condition, and it fails.
				AssetVersionID: ver20, ProjectID: publicUserProject.ID,
				ProjectName: publicUserProject.Name, ProjectSlug: publicUserProject.Slug,
				ProjectVisibility: public, VisibilityOfUsage: private,
				DependencyType: "cites",
				CreatedAt:      time.Date(2026, 9, 11, 9, 0, 0, 0, time.UTC),
			},
		},
		Events: []assets.PageEventState{
			{
				Type: "research_asset.version_published", Visibility: public,
				ActorID: assetE2EAlice.ID, AssetVersion: "2.0",
				OccurredAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
			},
			{
				// An event the publish command recorded as private (the
				// project was private when it was published): a non-member
				// must not see it.
				Type: "research_asset.version_published", Visibility: private,
				ActorID: assetE2EAlice.ID, AssetVersion: "3.0",
				OccurredAt: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
			},
		},
	}
}

// assetE2EAllPrivateFixture is an asset whose every version is private: a
// non-member's read answers the page's one 404, so the pid cannot be used
// to prove the asset exists.
func assetE2EAllPrivateFixture(openProject, privateProject, publicUserProject domain.Project) assets.PageState {
	state := assetE2EFixture(openProject, privateProject, publicUserProject)
	state.Asset.PID = assetE2EAllPrivatePID
	state.Asset.Title = "Unpublished Zeolite Screen"
	state.Asset.Slug = "unpublished-zeolite"
	state.Pins = map[assets.DependencyPin]assets.PagePinState{}
	state.Lineage = nil
	state.Usages = nil
	state.Events = nil
	for i := range state.Versions {
		state.Versions[i].Visibility = assets.VisibilityPrivate
	}
	return state
}

// assetE2EReader is the in-memory PageReader: one state per pid plus the
// browse rows. reads counts the state reads, so a test can assert that a
// request refused at the parsing step did not touch the store at all, and
// broken makes every read fail (the fail-closed case).
type assetE2EReader struct {
	states map[assets.PID]assets.PageState
	rows   []assets.BrowseRowState
	reads  int
	broken bool
}

func (r *assetE2EReader) LoadAssetPage(_ context.Context, pid assets.PID) (assets.PageState, bool, error) {
	r.reads++
	if r.broken {
		return assets.PageState{}, false, fmt.Errorf("fixture: the asset reader is unavailable")
	}
	state, ok := r.states[pid]
	return state, ok, nil
}

func (r *assetE2EReader) ListBrowseAssets(_ context.Context, filter *assets.Type) ([]assets.BrowseRowState, error) {
	r.reads++
	if r.broken {
		return nil, fmt.Errorf("fixture: the asset reader is unavailable")
	}
	if filter == nil {
		return r.rows, nil
	}
	out := make([]assets.BrowseRowState, 0, len(r.rows))
	for _, row := range r.rows {
		if row.Type == *filter {
			out = append(out, row)
		}
	}
	return out, nil
}

// assetE2ENoPublish satisfies the surface's third port. Publishing is not
// this suite's subject (T0705 has its own suite), and a nil port would make
// a route panic instead of saying why it is not wired.
type assetE2ENoPublish struct{}

func (assetE2ENoPublish) Publish(context.Context, assetpublish.Actor, assetpublish.PublishParams) (assetpublish.PublishedVersion, error) {
	return assetpublish.PublishedVersion{}, fmt.Errorf("publish is not wired in the asset page e2e")
}

// ---------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------

// assetE2EEnv is one composed deployment, plus the fixture reader the suite
// mutates between requests.
type assetE2EEnv struct {
	*e2eEnv
	reader *assetE2EReader
}

// newAssetE2EEnv composes the deployment the way cmd/api/main.go does for
// the surfaces this suite needs: authhttp + projectshttp + assetshttp on
// one v1 mux, guarded once, mounted under /api/v1/. The project surface is
// not a stand-in — it is the same *projects.Service the asset surface's
// Projects gate and Members read are wired to.
func newAssetE2EEnv(t *testing.T, reader *assetE2EReader) *assetE2EEnv {
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
	assetsAPI := assetshttp.New(assetshttp.Deps{
		// State is the publish-preview's reader. The two publish routes are
		// T0705's subject and are not exercised here.
		State:    nil,
		Projects: projectAPI.Service(),
		Members:  projectAPI.Service(),
		Publish:  assetE2ENoPublish{},
		Pages:    reader,
	})

	v1 := http.NewServeMux()
	authAPI.Register(v1)
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	assetsAPI.Register(v1)

	mux := http.NewServeMux()
	mux.Handle("/api/v1/", authAPI.Guard(v1))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = redisClient.Close() })

	jar, _ := cookiejar.New(nil)
	env := &e2eEnv{
		api:   ts,
		redis: mr,
		users: users,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return &assetE2EEnv{e2eEnv: env, reader: reader}
}

// assetE2EAliceCredentials is the signup body of the fixture's user —
// declared once, because the same account is signed up and later logged in.
const assetE2EAliceCredentials = `{"email":"assets-alice@example.com","password":"long-enough-password-1","handle":"assets-alice","display_name":"Alice Guo"}`

// threeProjects signs alice up, creates the fixture's three projects over
// the wire and returns them. The env's client is left on a FRESH jar
// afterwards: anonymous is the reader every test asserts against first, and
// the member case comes back through signInAsProjectOwner.
func (e *assetE2EEnv) threeProjects(t *testing.T) (open, private, using domain.Project) {
	t.Helper()
	alice := e2eSignup(t, e.e2eEnv, assetE2EAliceCredentials)
	openID := e2eCreateProject(t, e.e2eEnv, alice.CSRFToken,
		`{"slug":"`+assetE2EOpenSlug+`","name":"`+assetE2EOpenName+
			`","purpose":"MOF synthesis screening","visibility":"public"}`)
	privateID := e2eCreateProject(t, e.e2eEnv, alice.CSRFToken,
		`{"slug":"`+assetE2EPrivateSlug+`","name":"`+assetE2EPrivateName+
			`","purpose":"unpublished alloy screening","visibility":"private"}`)
	usingID := e2eCreateProject(t, e.e2eEnv, alice.CSRFToken,
		`{"slug":"`+assetE2EUserSlug+`","name":"`+assetE2EUserName+
			`","purpose":"MOF catalysis studies","visibility":"public"}`)
	e.client.Jar = freshJar()
	return domain.Project{
		ID: openID, Name: assetE2EOpenName, Slug: assetE2EOpenSlug,
		Visibility: domain.VisibilityPublic,
	}, domain.Project{
		ID: privateID, Name: assetE2EPrivateName, Slug: assetE2EPrivateSlug,
		Visibility: domain.VisibilityPrivate,
	}, domain.Project{
		ID: usingID, Name: assetE2EUserName, Slug: assetE2EUserSlug,
		Visibility: domain.VisibilityPublic,
	}
}

// signInAsProjectOwner returns a jar carrying a session for the user who
// created the fixture's projects. The projects already exist — created by
// threeProjects with the same credentials — so this is a LOGIN, which is
// also the honest way for a test to become the member: it proves the
// membership is read from the store rather than from a value the test
// handed the handler.
func (e *assetE2EEnv) signInAsProjectOwner(t *testing.T) {
	t.Helper()
	jar := freshJar()
	e.client.Jar = jar
	resp := e.do(t, http.MethodPost, "/api/v1/auth/login",
		`{"email":"assets-alice@example.com","password":"long-enough-password-1"}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
}

// ---------------------------------------------------------------------
// The tests
// ---------------------------------------------------------------------

// TestE2EAssetPageAnonymous drives the anonymous page and checks the eleven
// items docs/42 §Asset Page fixes, one by one, against the wire — plus what
// may NOT be on it.
func TestE2EAssetPageAnonymous(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, using := env.threeProjects(t)
	reader.states[assetE2EPID] = assetE2EFixture(open, private, using)

	resp := env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID), "", nil)
	raw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the anonymous asset page = %d: %s", resp.StatusCode, raw)
	}

	// The block keys themselves: a block whose list is empty still has its
	// field, and a field that vanished would be a silent hole in one of the
	// eleven items, so the presence check is on the raw bytes.
	for _, want := range []string{
		`"asset"`, `"pid"`, `"type"`, `"origin"`, `"rights"`, `"creators"`,
		`"metadata"`, `"dependencies"`, `"lineage"`, `"used_by"`, `"versions"`, `"events"`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the page is missing the %s block: %s", want, excerpt(raw))
		}
	}

	var page struct {
		Asset struct {
			PID           string `json:"pid"`
			Type          string `json:"type"`
			Title         string `json:"title"`
			Slug          string `json:"slug"`
			OriginProject *struct {
				ID         string `json:"id"`
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
			} `json:"origin_project"`
		} `json:"asset"`
		Version struct {
			Version       string `json:"version"`
			URL           string `json:"url"`
			Visibility    string `json:"visibility"`
			IntegrityHash string `json:"integrity_hash"`
			PublishedBy   *struct {
				Handle string `json:"handle"`
			} `json:"published_by"`
		} `json:"version"`
		Origin []struct {
			Ref       string `json:"ref"`
			Kind      string `json:"kind"`
			Resolved  bool   `json:"resolved"`
			ProjectID string `json:"project_id"`
			Link      string `json:"link"`
		} `json:"origin"`
		Rights   map[string]any `json:"rights"`
		Creators []struct {
			UserID      string `json:"user_id"`
			Handle      string `json:"handle"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
		} `json:"creators"`
		Metadata []struct {
			Key   string `json:"key"`
			Value any    `json:"value"`
		} `json:"metadata"`
		Dependencies []struct {
			Pin      string `json:"pin"`
			Resolved bool   `json:"resolved"`
			Public   bool   `json:"public"`
			Title    string `json:"title"`
			Type     string `json:"type"`
			URL      string `json:"url"`
		} `json:"dependencies"`
		Lineage []struct {
			Relation  string `json:"relation"`
			Direction string `json:"direction"`
			PID       string `json:"pid"`
			Version   string `json:"version"`
			URL       string `json:"url"`
			Title     string `json:"title"`
		} `json:"lineage"`
		UsedBy []struct {
			ProjectID      string `json:"project_id"`
			ProjectName    string `json:"project_name"`
			ProjectSlug    string `json:"project_slug"`
			DependencyType string `json:"dependency_type"`
			CreatedAt      string `json:"created_at"`
		} `json:"used_by"`
		Versions []struct {
			Version       string `json:"version"`
			URL           string `json:"url"`
			Visibility    string `json:"visibility"`
			IntegrityHash string `json:"integrity_hash"`
			Current       bool   `json:"current"`
		} `json:"versions"`
		Events []struct {
			Type       string `json:"type"`
			OccurredAt string `json:"occurred_at"`
			Version    string `json:"version"`
			Actor      *struct {
				Handle string `json:"handle"`
			} `json:"actor"`
		} `json:"events"`
	}
	if err := json.Unmarshal([]byte(raw), &page); err != nil {
		t.Fatalf("asset page body: %v (%s)", err, raw)
	}

	// 1. PID/version — two identifiers, and the version's URL is built from
	//    both, because a label alone names a version of SOME asset.
	if page.Asset.PID != string(assetE2EPID) || page.Version.Version != "2.0" {
		t.Errorf("pid/version = %q/%q, want %q/2.0", page.Asset.PID, page.Version.Version, assetE2EPID)
	}
	if page.Version.URL != "/assets/"+string(assetE2EPID)+"/2.0" {
		t.Errorf("version url = %q, want the pid+label address", page.Version.URL)
	}
	if page.Version.Visibility != "public" || !strings.HasPrefix(page.Version.IntegrityHash, "sha256:") {
		t.Errorf("the rendered version's facts are incomplete: %+v", page.Version)
	}
	if page.Version.PublishedBy == nil || page.Version.PublishedBy.Handle != assetE2EAlice.Handle {
		t.Errorf("published_by = %+v, want the publishing actor", page.Version.PublishedBy)
	}
	// 2. Type — one of the closed set of four.
	if page.Asset.Type != string(assets.TypeMaterialCollection) {
		t.Errorf("type = %q, want material_collection", page.Asset.Type)
	}
	// 3. Origin — the public project, resolved, with its link.
	if len(page.Origin) != 1 || page.Origin[0].Ref != "project:"+open.ID ||
		page.Origin[0].Kind != "project" || !page.Origin[0].Resolved ||
		page.Origin[0].ProjectID != open.ID || page.Origin[0].Link != "/projects/"+open.ID {
		t.Errorf("origin = %+v, want exactly the public project ref with its link", page.Origin)
	}
	// 4. Rights — the stored document, verbatim.
	if page.Rights["standard_license_id"] != "CC-BY-4.0" {
		t.Errorf("rights = %+v, want the stored document", page.Rights)
	}
	// 5. Creators — the publishing actor, with the role named.
	if len(page.Creators) != 1 || page.Creators[0].Role != assets.CreatorRolePublisher ||
		page.Creators[0].Handle != assetE2EAlice.Handle ||
		page.Creators[0].UserID != assetE2EAlice.ID ||
		page.Creators[0].DisplayName != assetE2EAlice.DisplayName {
		t.Errorf("creators = %+v, want the publisher with its role named", page.Creators)
	}
	// 6. Metadata — the manifest's keys, with their declared values.
	if len(page.Metadata) != 5 {
		t.Errorf("metadata = %+v, want the manifest's five keys", page.Metadata)
	}
	for _, entry := range page.Metadata {
		if entry.Key == "cell_chemistry" && entry.Value != "Zn4O(BDC)3" {
			t.Errorf("metadata cell_chemistry = %v, want the declared string", entry.Value)
		}
		if entry.Key == "" {
			t.Errorf("a metadata entry has no key: %+v", entry)
		}
	}
	// 7. Dependencies — the linkable pin and the unresolvable one; the pin
	//    into a private project is gone entirely.
	if len(page.Dependencies) != 2 {
		t.Fatalf("dependencies = %+v, want the public pin and the unresolved one", page.Dependencies)
	}
	var sawPublicPin, sawUnresolved bool
	for _, dep := range page.Dependencies {
		switch dep.Pin {
		case string(assetE2EOpenDepPID) + "@1.4":
			sawPublicPin = true
			if !dep.Resolved || !dep.Public || dep.Title != "BDC Linker Protocol" ||
				dep.Type != string(assets.TypeProtocol) ||
				dep.URL != "/assets/"+string(assetE2EOpenDepPID)+"/1.4" {
				t.Errorf("the linkable pin = %+v, want a resolved public entry with its url", dep)
			}
		case string(assetE2EUnresolvedPID) + "@0.1":
			sawUnresolved = true
			if dep.Resolved || dep.Public || dep.URL != "" {
				t.Errorf("the unresolved pin = %+v, want its own bytes and nothing resolved", dep)
			}
		default:
			t.Errorf("a pin that must not be rendered is on the page: %+v", dep)
		}
	}
	if !sawPublicPin || !sawUnresolved {
		t.Errorf("dependencies = %+v, want one public pin and one unresolved pin", page.Dependencies)
	}
	// 8. Lineage — only the edge whose other end is public. The
	//    derived_from edge points at a private project's version, so it is
	//    dropped whole rather than blanked.
	if len(page.Lineage) != 1 || page.Lineage[0].Relation != "supersedes" ||
		page.Lineage[0].Direction != assets.LineageParent ||
		page.Lineage[0].URL != "/assets/"+string(assetE2EPID)+"/1.0" {
		t.Errorf("lineage = %+v, want only the supersedes edge to 1.0", page.Lineage)
	}
	// 9. Used/derived public links — the public usage only.
	if len(page.UsedBy) != 1 || page.UsedBy[0].ProjectID != using.ID ||
		page.UsedBy[0].ProjectSlug != assetE2EUserSlug ||
		page.UsedBy[0].ProjectName != assetE2EUserName ||
		page.UsedBy[0].DependencyType != "derived_from" {
		t.Errorf("used_by = %+v, want exactly the public usage", page.UsedBy)
	}
	// 10. Versions — the public ones, each with its own hash, exactly one
	//     marked current.
	if len(page.Versions) != 2 {
		t.Fatalf("versions = %+v, want the two public versions", page.Versions)
	}
	hashes := map[string]string{}
	current := 0
	for _, v := range page.Versions {
		hashes[v.Version] = v.IntegrityHash
		if v.Current {
			current++
			if v.Version != "2.0" {
				t.Errorf("the current version = %q, want 2.0", v.Version)
			}
		}
	}
	if current != 1 {
		t.Errorf("versions marked current = %d, want exactly 1", current)
	}
	if hashes["1.0"] == hashes["2.0"] || hashes["1.0"] == "" || hashes["2.0"] == "" {
		t.Errorf("two versions share a hash or carry none: %v", hashes)
	}
	// 11. Network events — the public one, with its actor.
	if len(page.Events) != 1 || page.Events[0].Type != "research_asset.version_published" ||
		page.Events[0].Version != "2.0" || page.Events[0].Actor == nil ||
		page.Events[0].Actor.Handle != assetE2EAlice.Handle {
		t.Errorf("events = %+v, want the public publish event with its actor", page.Events)
	}

	if page.Asset.OriginProject == nil || page.Asset.OriginProject.ID != open.ID ||
		page.Asset.OriginProject.Slug != assetE2EOpenSlug ||
		page.Asset.OriginProject.Visibility != "public" {
		t.Errorf("origin_project = %+v, want the public project", page.Asset.OriginProject)
	}

	// --- what may NOT be on the wire ------------------------------------
	// The private project's identity, the private version's label, the
	// private asset's pid, the private usage, the private event. Checked on
	// the RAW body rather than on the decoded struct: a field added to the
	// payload later cannot smuggle one out past this list unnoticed.
	for _, secret := range []string{
		private.ID, assetE2EPrivateSlug, assetE2EPrivateName,
		string(assetE2EHiddenPID), "Hidden Alloy Screen",
		`"3.0"`, "7.1", "hidden-ver",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("the anonymous page leaks %q: %s", secret, leakContext(raw, secret))
		}
	}
	// And no sentence about a count of what was withheld: docs/23 §5
	// forbids the number, so it must not be phrased either.
	for _, phrase := range []string{"hidden", "private versions", "1 private"} {
		if strings.Contains(raw, phrase) {
			t.Errorf("the anonymous page carries the phrase %q, which reads as a withheld count: %s",
				phrase, leakContext(raw, phrase))
		}
	}
	// No blob URL either, for any blob: this platform has no route that
	// serves one (proved in TestE2EAssetBlobRouteDoesNotExist below), so a
	// page that printed one would be a link to a 404.
	for _, forbidden := range []string{"/blob", "/download", "files/raw"} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the asset page carries a %q URL: %s", forbidden, leakContext(raw, forbidden))
		}
	}
}

// TestE2EAssetPageMembershipComesFromTheProjectService drives the one bit
// that separates the network's view of an asset from its own project's: the
// same URL, asked twice, with the second caller a real member — signed up
// and made the owner of the project over the wire, then logged back in, so
// the membership is read from the real project store through the real
// service rather than handed to the handler.
func TestE2EAssetPageMembershipComesFromTheProjectService(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, using := env.threeProjects(t)
	reader.states[assetE2EPID] = assetE2EFixture(open, private, using)

	// The visitor (the fresh jar threeProjects left behind): the public
	// versions only.
	resp := env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID), "", nil)
	visitorRaw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the visitor's page = %d: %s", resp.StatusCode, visitorRaw)
	}
	if strings.Contains(visitorRaw, `"3.0"`) {
		t.Errorf("the visitor's page renders the private version: %s", excerpt(visitorRaw))
	}
	if !strings.Contains(visitorRaw, open.ID) {
		t.Errorf("the visitor's page withholds the PUBLIC project's identity: %s", excerpt(visitorRaw))
	}

	// The member: the same URL, alice's session cookie rides along, the
	// handler asks the project service for her membership, and the private
	// version renders — labelled private rather than hidden, because a
	// member of the project is exactly who that label is for.
	env.signInAsProjectOwner(t)
	resp = env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID), "", nil)
	memberRaw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the member's page = %d: %s", resp.StatusCode, memberRaw)
	}
	if !strings.Contains(memberRaw, `"3.0"`) {
		t.Errorf("the member's page does not render the private version: %s", excerpt(memberRaw))
	}
	if !strings.Contains(memberRaw, `"visibility":"private"`) {
		t.Errorf("the member's page does not label the private version as private: %s", excerpt(memberRaw))
	}
	if strings.Contains(memberRaw, assetE2EPrivateName) {
		t.Errorf("the member's page names the private USAGE project, which no view may render: %s", excerpt(memberRaw))
	}

	// A signed-in NON-member: authenticated is not a member.
	env.client.Jar = freshJar()
	bob := e2eSignup(t, env.e2eEnv,
		`{"email":"assets-bob@example.com","password":"long-enough-password-1","handle":"assets-bob","display_name":"Bob Ning"}`)
	if bob.CSRFToken == "" {
		t.Fatal("the second signup returned no CSRF token")
	}
	resp = env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID), "", nil)
	strangerRaw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the non-member's page = %d: %s", resp.StatusCode, strangerRaw)
	}
	if strings.Contains(strangerRaw, `"3.0"`) {
		t.Errorf("a signed-in non-member sees the private version: %s", excerpt(strangerRaw))
	}
	if strings.Contains(strangerRaw, private.ID) || strings.Contains(strangerRaw, assetE2EPrivateName) {
		t.Errorf("the non-member's page names the private project or its usage: %s", excerpt(strangerRaw))
	}
}

// TestE2EAssetPageAnswersOneIndistinguishable404 drives every situation
// that must not be distinguishable: an unknown pid, a slug-shaped segment,
// an asset whose project the caller may not read, an asset with nothing
// visible, and a version the caller may not see. One status, one code, one
// message, and no identity in any of the bodies.
func TestE2EAssetPageAnswersOneIndistinguishable404(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, using := env.threeProjects(t)

	// One asset in a PRIVATE project (the read gate refuses it) and one
	// whose every version is private (the gate passes, nothing is visible).
	reader.states[assetE2EPID] = assetE2EFixture(private, private, using)
	reader.states[assetE2EAllPrivatePID] = assetE2EAllPrivateFixture(open, private, using)

	cases := []struct {
		name string
		path string
	}{
		{"an unknown pid", "/api/v1/assets/" + string(assetE2EUnknownPID)},
		{"a slug-shaped segment", "/api/v1/assets/mof5-synthesis"},
		{"a private project's asset", "/api/v1/assets/" + string(assetE2EPID)},
		{"an asset with nothing visible", "/api/v1/assets/" + string(assetE2EAllPrivatePID)},
		{"a version a non-member may not see", "/api/v1/assets/" + string(assetE2EPID) + "?version=3.0"},
	}
	first := ""
	for i, tc := range cases {
		resp := env.do(t, http.MethodGet, tc.path, "", nil)
		body := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d, want 404: %s", tc.name, resp.StatusCode, body)
			continue
		}
		if i == 0 {
			first = body
		} else if body != first {
			t.Errorf("%s answered %s; %s answered %s — the five must be indistinguishable",
				tc.name, body, cases[0].name, first)
		}
		var decoded envelope
		if err := json.Unmarshal([]byte(body), &decoded); err != nil {
			t.Errorf("%s: the body is not the error envelope: %v (%s)", tc.name, err, body)
			continue
		}
		if decoded.Code != assetshttp.CodeAssetNotFound {
			t.Errorf("%s: code = %q, want %q", tc.name, decoded.Code, assetshttp.CodeAssetNotFound)
		}
		for _, secret := range []string{
			private.ID, assetE2EPrivateSlug, assetE2EPrivateName,
			string(assetE2EPID), string(assetE2EAllPrivatePID), "mof5-synthesis", "3.0",
		} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %q: %s", tc.name, secret, leakContext(body, secret))
			}
		}
	}
}

// TestE2EAssetPageVersionParameter drives the version query: a label that
// names a visible version renders that version, a label that could never be
// stored is refused 400 before anything is read, and a label that names a
// version the caller may not see is the same 404 an unknown pid gets.
func TestE2EAssetPageVersionParameter(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, using := env.threeProjects(t)
	reader.states[assetE2EPID] = assetE2EFixture(open, private, using)

	resp := env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID)+"?version=1.0", "", nil)
	raw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("version=1.0 = %d: %s", resp.StatusCode, raw)
	}
	if !strings.Contains(raw, `"version":"1.0"`) ||
		!strings.Contains(raw, "/assets/"+string(assetE2EPID)+"/1.0") {
		t.Errorf("version=1.0 did not render that version: %s", raw)
	}

	// A label that cannot be stored: 400, and nothing was read — a request
	// refused at the parsing step must not touch the store.
	before := reader.reads
	resp = env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID)+"?version=not%20a%20label", "", nil)
	body := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a malformed version = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, assetshttp.CodeAssetPageValidationFailed) {
		t.Errorf("a malformed version's code = %s, want %s", body, assetshttp.CodeAssetPageValidationFailed)
	}
	// The page route's own code, not the browse list's: the two surfaces are
	// answered and logged separately (cmd/api/assetshttp), and a page request
	// that reported the list's code would tell the caller their filter was
	// wrong.
	if strings.Contains(body, assetshttp.CodeAssetListValidationFailed) {
		t.Errorf("a malformed version answers the browse list's code: %s", body)
	}
	if reader.reads != before {
		t.Errorf("a malformed version read the state (%d reads before, %d after)", before, reader.reads)
	}

	// Well-formed labels that name no visible version — the private one and
	// one that does not exist — are the page's 404.
	for _, version := range []string{"3.0", "9.9"} {
		resp = env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID)+"?version="+version, "", nil)
		body := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("version=%s = %d, want 404: %s", version, resp.StatusCode, body)
		}
		if !strings.Contains(body, assetshttp.CodeAssetNotFound) {
			t.Errorf("version=%s: code = %s, want %s", version, body, assetshttp.CodeAssetNotFound)
		}
	}
}

// TestE2EAssetBrowseFiltersByTheClosedTypeSet drives the hub's list: no
// filter lists every asset with something public, a type filters, and a
// value outside the four-type set is refused 400 naming the set — rather
// than answered with an empty list, which would claim "there are no such
// assets".
func TestE2EAssetBrowseFiltersByTheClosedTypeSet(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, _ := env.threeProjects(t)
	now := time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC)
	reader.rows = []assets.BrowseRowState{
		{PID: assetE2EPID, Type: assets.TypeMaterialCollection,
			Title: "MOF-5 Synthesis Collection", Slug: "mof5-synthesis",
			OriginProjectID: open.ID, OriginProjectName: open.Name, OriginProjectSlug: open.Slug,
			OriginProjectVisibility: assets.VisibilityPublic,
			PublicVersions:          2, LatestVersion: "2.0", LatestPublishedAt: now},
		{PID: assetE2EHiddenPID, Type: assets.TypeDataset,
			Title: "Screening Run 7", Slug: "screening-run-7",
			OriginProjectID: private.ID, OriginProjectName: private.Name, OriginProjectSlug: private.Slug,
			OriginProjectVisibility: assets.VisibilityPrivate,
			PublicVersions:          1, LatestVersion: "0.4", LatestPublishedAt: now.Add(time.Minute)},
		// A row with nothing public: never listed, and not counted either.
		{PID: assetE2EAllPrivatePID, Type: assets.TypeDataset,
			Title: "Unpublished Zeolite Screen", Slug: "unpublished-zeolite",
			OriginProjectID: open.ID, OriginProjectName: open.Name, OriginProjectSlug: open.Slug,
			OriginProjectVisibility: assets.VisibilityPublic,
			PublicVersions:          0, LatestVersion: "0.0", LatestPublishedAt: now},
	}

	resp := env.do(t, http.MethodGet, "/api/v1/assets", "", nil)
	raw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("browse = %d: %s", resp.StatusCode, raw)
	}
	var list struct {
		Type   *string  `json:"type"`
		Types  []string `json:"types"`
		Assets []struct {
			PID            string `json:"pid"`
			Type           string `json:"type"`
			Title          string `json:"title"`
			URL            string `json:"url"`
			PublicVersions int    `json:"public_versions"`
			LatestVersion  string `json:"latest_version"`
			LatestURL      string `json:"latest_url"`
			OriginProject  *struct {
				ID         string `json:"id"`
				Slug       string `json:"slug"`
				Visibility string `json:"visibility"`
			} `json:"origin_project"`
		} `json:"assets"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("browse body: %v (%s)", err, raw)
	}
	if list.Type != nil {
		t.Errorf("the unfiltered browse reports a filter: %q", *list.Type)
	}
	wantTypes := []string{"dataset", "protocol", "material_collection", "benchmark"}
	if strings.Join(list.Types, ",") != strings.Join(wantTypes, ",") {
		t.Errorf("browse offers types %v, want the closed set %v", list.Types, wantTypes)
	}
	if len(list.Assets) != 2 {
		t.Fatalf("browse listed %d assets, want the 2 with a public version: %s", len(list.Assets), raw)
	}
	for _, asset := range list.Assets {
		switch asset.PID {
		case string(assetE2EPID):
			if asset.URL != "/assets/"+string(assetE2EPID) ||
				asset.LatestURL != "/assets/"+string(assetE2EPID)+"/2.0" ||
				asset.PublicVersions != 2 || asset.LatestVersion != "2.0" {
				t.Errorf("the public row = %+v, want its url, latest url, count and label", asset)
			}
			if asset.OriginProject == nil || asset.OriginProject.ID != open.ID ||
				asset.OriginProject.Slug != assetE2EOpenSlug {
				t.Errorf("the public row withholds its public project: %+v", asset.OriginProject)
			}
		case string(assetE2EHiddenPID):
			if asset.OriginProject != nil {
				t.Errorf("browse named a private project: %+v", asset.OriginProject)
			}
			if asset.PublicVersions != 1 || asset.LatestVersion != "0.4" {
				t.Errorf("the private project's public row = %+v, want its public version facts only", asset)
			}
		default:
			t.Errorf("browse listed an asset it must not: %+v", asset)
		}
	}
	for _, secret := range []string{
		private.ID, assetE2EPrivateSlug, assetE2EPrivateName, "unpublished-zeolite",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("browse leaks %q: %s", secret, leakContext(raw, secret))
		}
	}

	// One type at a time: the filter selects, and the answer says which
	// filter it applied.
	for _, want := range wantTypes {
		resp := env.do(t, http.MethodGet, "/api/v1/assets?type="+want, "", nil)
		body := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("browse type=%s = %d: %s", want, resp.StatusCode, body)
		}
		var filtered struct {
			Type   *string `json:"type"`
			Assets []struct {
				Type string `json:"type"`
			} `json:"assets"`
		}
		if err := json.Unmarshal([]byte(body), &filtered); err != nil {
			t.Fatalf("browse type=%s body: %v", want, err)
		}
		if filtered.Type == nil || *filtered.Type != want {
			t.Errorf("browse type=%s reports filter %v", want, filtered.Type)
		}
		for _, asset := range filtered.Assets {
			if asset.Type != want {
				t.Errorf("browse type=%s returned a %s asset", want, asset.Type)
			}
		}
	}

	// A value outside the set is refused, and the refusal names the set.
	resp = env.do(t, http.MethodGet, "/api/v1/assets?type=melting_point", "", nil)
	body := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("browse type=melting_point = %d, want 400: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, assetshttp.CodeAssetListValidationFailed) {
		t.Errorf("the browse refusal's code = %s, want %s", body, assetshttp.CodeAssetListValidationFailed)
	}
	for _, want := range wantTypes {
		if !strings.Contains(body, want) {
			t.Errorf("the browse refusal does not name %q: %s", want, body)
		}
	}
}

// TestE2EAssetBlobRouteDoesNotExist is the acceptance criterion "a
// restricted blob is not downloadable, proven by the route not existing" —
// proven twice over, and neither proof is a hidden button.
//
// The first proof is structural: every path that would serve bytes is
// probed on the SAME composed server the rest of this suite uses, and each
// answers the Go standard library's own 404 ("404 page not found",
// text/plain) rather than this product's JSON envelope. That is the
// difference between "a route exists and refused" and "no route matched at
// all" — a product handler would have had to write the envelope — and the
// control request below shows this server does write the envelope when a
// product route is what answered.
//
// The second proof is the page's own answer: the raw body of the asset page
// carries no URL that could be one, asserted in
// TestE2EAssetPageAnonymous.
func TestE2EAssetBlobRouteDoesNotExist(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}}
	env := newAssetE2EEnv(t, reader)
	open, private, using := env.threeProjects(t)
	reader.states[assetE2EPID] = assetE2EFixture(open, private, using)

	// The control: the asset route DOES answer with this product's JSON
	// envelope, so the probes below measure "no route", not "this server
	// answers text/plain to everything".
	control := env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EUnknownPID), "", nil)
	controlBody := string(bodyBytes(t, control))
	if control.StatusCode != http.StatusNotFound || !strings.Contains(controlBody, `"code"`) {
		t.Fatalf("the control: an unknown pid = %d %s, want this product's JSON 404", control.StatusCode, controlBody)
	}

	for _, path := range []string{
		"/api/v1/assets/" + string(assetE2EPID) + "/blob",
		"/api/v1/assets/" + string(assetE2EPID) + "/download",
		"/api/v1/assets/" + string(assetE2EPID) + "/raw",
		"/api/v1/assets/" + string(assetE2EPID) + "/files/raw",
		"/api/v1/blobs",
		"/api/v1/blobs/sha256:9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a9a",
	} {
		resp := env.do(t, http.MethodGet, path, "", nil)
		body := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 (no such route): %s", path, resp.StatusCode, body)
			continue
		}
		if strings.Contains(body, `"code"`) || strings.Contains(body, assetshttp.CodeAssetNotFound) {
			t.Errorf("GET %s was answered by a product handler: %s", path, body)
		}
		if !strings.Contains(body, "404 page not found") {
			t.Errorf("GET %s was not the standard library's unmatched-route 404: %q", path, body)
		}
	}
}

// TestE2EAssetPageRefusesToInventAnAnswerOnAReadFailure is the fail-closed
// half: when the state read fails, the page answers 503 rather than a page
// built from nothing. A page is a statement about the repository as it was
// read, and "no public usage, no lineage, no events" is a claim nobody
// finished checking.
func TestE2EAssetPageRefusesToInventAnAnswerOnAReadFailure(t *testing.T) {
	reader := &assetE2EReader{states: map[assets.PID]assets.PageState{}, broken: true}
	env := newAssetE2EEnv(t, reader)
	env.threeProjects(t)

	resp := env.do(t, http.MethodGet, "/api/v1/assets/"+string(assetE2EPID), "", nil)
	body := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a failed read = %d, want 503: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, assetshttp.CodeAssetPageUnavailable) {
		t.Errorf("a failed read's code = %s, want %s", body, assetshttp.CodeAssetPageUnavailable)
	}
}
