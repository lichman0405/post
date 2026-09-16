package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/explorehttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/contribution"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The Explore e2e (T0802 required test "explore e2e"): GET /api/v1/explore
// driven over real HTTP through the production composition — one guarded
// /api/v1 mux carrying the auth surface, the project surface, the profile
// surface and the Explore surface, exactly the way cmd/api/main.go mounts
// them, so the guard, the routing and the index's own rules are the shipped
// ones rather than a test's rewrite of them.
//
// What is real here and what is not, stated plainly, because the difference
// is the difference between this suite and tests/integration/explore_test.go:
//
//   - real: the guard (an anonymous GET flows through, a write does not),
//     the routing of /api/v1/explore, the six-section model and every rule
//     in it (internal/application/explore: which rows render, which project
//     may be named, the freshness ranking, the section cap), the adapters
//     that make the Projects tab the project service's own public list and
//     the Assets tab the asset hub's own BuildBrowse output, the
//     503-with-a-code failure path, and the wire JSON a browser receives.
//   - not real: the identity store (memstore) and the ROW source for three
//     of the six sections. Knowledge, people and organizations come from the
//     in-memory reader below, the same substitution
//     tests/e2e/privacy_e2e_test.go makes for projects;
//     tests/integration/explore_test.go runs the real PostgreSQL adapter for
//     those three against the real schema.
//
// The projects the fixture rows belong to are REAL: they are created over
// the wire by a signed-up user, so the Projects tab and the "may this
// project be named?" rule answer from the real project service over a real
// store, including the private project that must never be named.

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

const (
	exploreE2EOpenSlug    = "explore-open-lab"
	exploreE2EOpenName    = "Explore Open Lab"
	exploreE2EPrivateSlug = "explore-private-lab"
	exploreE2EPrivateName = "Explore Private Lab"
)

// exploreE2EAliceCredentials is the signup body of the fixture's user.
const exploreE2EAliceCredentials = `{"email":"explore-alice@example.com","password":"long-enough-password-1","handle":"explore-alice","display_name":"Alice Guo"}`

// The fixture knowledge rows. The private project's row is the one the
// acceptance criterion is about: a private project may publish (docs/12 §2),
// so the row IS in the index — and the project behind it must not be named.
const (
	exploreE2EKnowledgeOpenTitle    = "Open Catalysis Question"
	exploreE2EKnowledgeHiddenTitle  = "Withheld Alloy Question"
	exploreE2EKnowledgeOpenObjectID = "11111111-1111-4111-8111-111111111101"
	exploreE2EKnowledgeHidObjectID  = "11111111-1111-4111-8111-111111111102"
	// exploreE2EKnowledgeOriginlessTitle is a published row whose project the
	// index enumerated in NO section — not a private project it saw and
	// withheld, a project it never saw at all. The row is public (it was
	// published) and renders without a project, which is the fail-closed
	// reading: an id that is not in the public set is not a project the
	// index may name.
	exploreE2EKnowledgeOriginlessTitle = "Unlisted Origin Question"
	// exploreE2EPrivateFixtureProjectID names a project the index enumerates
	// in NO section: it is deliberately a distinctive string, because the
	// leak assertions search the response bytes for it and a short id would
	// also match other fixture text — a false positive that would make a
	// broken check look like a working one.
	exploreE2EPrivateFixtureProjectID = "9f3c7a1e-hidden-project-uuid"
)

// exploreE2EReader answers the three sections that have no in-memory
// production adapter (knowledge, people, organizations), and can fail on
// demand so the 503 path is driven rather than assumed.
type exploreE2EReader struct {
	knowledge     []explore.KnowledgeRow
	people        []explore.PersonRow
	organizations []explore.OrganizationRow

	broken bool
	calls  int
}

func (r *exploreE2EReader) ListPublishedKnowledge(context.Context) ([]explore.KnowledgeRow, error) {
	r.calls++
	if r.broken {
		return nil, fmt.Errorf("fixture: the knowledge reader is unavailable")
	}
	return r.knowledge, nil
}

func (r *exploreE2EReader) ListPublicPeople(context.Context) ([]explore.PersonRow, error) {
	r.calls++
	if r.broken {
		return nil, fmt.Errorf("fixture: the people reader is unavailable")
	}
	return r.people, nil
}

func (r *exploreE2EReader) ListPublicOrganizations(context.Context) ([]explore.OrganizationRow, error) {
	r.calls++
	if r.broken {
		return nil, fmt.Errorf("fixture: the organizations reader is unavailable")
	}
	return r.organizations, nil
}

// exploreE2EAssetPageReader is the asset hub's browse port, in-memory: the
// production value is *persistence.AssetPageStore.
type exploreE2EAssetPageReader struct {
	rows []assets.BrowseRowState
}

func (r *exploreE2EAssetPageReader) ListBrowseAssets(_ context.Context, filter *assets.Type) ([]assets.BrowseRowState, error) {
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

// exploreE2EFixtureAssets is the browse fixture, in the RAW shape T0709's
// store returns: one asset whose project is public, one whose project is
// private (BuildBrowse withholds its origin_project), and one with nothing
// public at all (BuildBrowse drops it — a private version is not a listing).
func exploreE2EFixtureAssets(open, private domain.Project) []assets.BrowseRowState {
	return []assets.BrowseRowState{
		{
			PID: assets.PID("01j9z6k3m4n5p6q7r8s9t0v1x1"), Type: assets.TypeDataset,
			Title: "Open Catalyst Screen", Slug: "open-catalyst-screen",
			OriginProjectID: open.ID, OriginProjectName: open.Name, OriginProjectSlug: open.Slug,
			OriginProjectVisibility: assets.VisibilityPublic,
			PublicVersions:          1, LatestVersion: "1.0.0",
			LatestPublishedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
		},
		{
			PID: assets.PID("01j9z6k3m4n5p6q7r8s9t0v1x2"), Type: assets.TypeProtocol,
			Title: "Hidden Alloy Protocol", Slug: "hidden-alloy-protocol",
			OriginProjectID: private.ID, OriginProjectName: private.Name, OriginProjectSlug: private.Slug,
			OriginProjectVisibility: assets.VisibilityPrivate,
			PublicVersions:          1, LatestVersion: "0.9.0",
			// NEWER than the open one: if the withheld name leaked, freshness
			// would put it first, where a leak is hardest to miss.
			LatestPublishedAt: time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC),
		},
		{
			PID: assets.PID("01j9z6k3m4n5p6q7r8s9t0v1x3"), Type: assets.TypeBenchmark,
			Title: "Nothing Public Benchmark", Slug: "nothing-public-benchmark",
			OriginProjectID: open.ID, OriginProjectName: open.Name, OriginProjectSlug: open.Slug,
			OriginProjectVisibility: assets.VisibilityPublic,
			PublicVersions:          0, LatestVersion: "0.0.0",
			LatestPublishedAt: time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
		},
	}
}

// exploreE2EComposite is the six-read port for this suite. It composes the
// production adapters where the suite can (the real project service, the
// real BuildBrowse over fixture rows) with the in-memory reader, so what is
// exercised is the surface's own rules over rows a reader resolved —
// private project identities and unpublished rows included.
type exploreE2EComposite struct {
	*explorehttp.ProjectSource
	*explorehttp.AssetSource
	*exploreE2EReader

	contributions []contribution.ContributionOpportunity
}

func (c *exploreE2EComposite) ListPublicContributions(context.Context) ([]contribution.ContributionOpportunity, error) {
	return c.contributions, nil
}

// ---------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------

type exploreE2EEnv struct {
	*e2eEnv
	reader *exploreE2EReader
}

// exploreE2EStack builds the guarded deployment and returns the environment
// plus the composite, so a test can mutate the fixture rows between
// requests.
func exploreE2EStack(t *testing.T, reader *exploreE2EReader) (*exploreE2EEnv, *exploreE2EComposite) {
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
	profileAPI := profilehttp.New(profilehttp.Deps{Profiles: users})
	projectsStore := memstore.NewProjects()
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectsStore,
		Orgs:  projectsStore.Gate(),
		Authz: authz.NewMatrixEngine(),
	})

	assets := &exploreE2EAssetPageReader{}
	composite := &exploreE2EComposite{
		ProjectSource:    &explorehttp.ProjectSource{Service: projectAPI.Service()},
		AssetSource:      &explorehttp.AssetSource{Pages: assets},
		exploreE2EReader: reader,
	}
	exploreAPI := explorehttp.New(explorehttp.Deps{Reader: composite})

	v1 := http.NewServeMux()
	authAPI.Register(v1)
	profileAPI.Register(v1)
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	exploreAPI.Register(v1)

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
	return &exploreE2EEnv{e2eEnv: env, reader: reader}, composite
}

// exploreE2EPopulate signs a user up, creates the fixture's two projects over
// the wire, feeds the composite the rows that depend on their ids, and leaves
// the client on a FRESH jar: anonymous is the caller every assertion starts
// from.
func exploreE2EPopulate(t *testing.T, env *exploreE2EEnv, composite *exploreE2EComposite) (open, private domain.Project) {
	t.Helper()
	alice := e2eSignup(t, env.e2eEnv, exploreE2EAliceCredentials)
	openID := e2eCreateProject(t, env.e2eEnv, alice.CSRFToken,
		`{"slug":"`+exploreE2EOpenSlug+`","name":"`+exploreE2EOpenName+
			`","purpose":"catalysis screening","visibility":"public"}`)
	privateID := e2eCreateProject(t, env.e2eEnv, alice.CSRFToken,
		`{"slug":"`+exploreE2EPrivateSlug+`","name":"`+exploreE2EPrivateName+
			`","purpose":"unpublished alloy screening","visibility":"private"}`)

	open = domain.Project{
		ID: openID, Slug: exploreE2EOpenSlug, Name: exploreE2EOpenName,
		Visibility: domain.VisibilityPublic,
	}
	private = domain.Project{
		ID: privateID, Slug: exploreE2EPrivateSlug, Name: exploreE2EPrivateName,
		Visibility: domain.VisibilityPrivate,
	}

	// The composite's rows can only be built once the REAL ids exist.
	composite.AssetSource = &explorehttp.AssetSource{
		Pages: &exploreE2EAssetPageReader{rows: exploreE2EFixtureAssets(open, private)},
	}
	env.client.Jar = freshJar()
	return open, private
}

// ---------------------------------------------------------------------
// Wire shapes
// ---------------------------------------------------------------------

// exploreE2EIndex is the decoded answer, in the shape the browser reads it.
type exploreE2EIndex struct {
	Projects struct {
		Tab   string `json:"tab"`
		Items []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"items"`
	} `json:"projects"`
	Assets struct {
		Tab   string `json:"tab"`
		Items []struct {
			PID           string `json:"pid"`
			Title         string `json:"title"`
			OriginProject *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"origin_project"`
		} `json:"items"`
	} `json:"assets"`
	Knowledge struct {
		Tab   string `json:"tab"`
		Items []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Project *struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"project"`
		} `json:"items"`
	} `json:"knowledge"`
	People struct {
		Tab   string `json:"tab"`
		Items []struct {
			ID       string `json:"id"`
			Handle   string `json:"handle"`
			FullName string `json:"display_name"`
		} `json:"items"`
	} `json:"people"`
	Organizations struct {
		Tab   string `json:"tab"`
		Items []struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
		} `json:"items"`
	} `json:"organizations"`
	Contributions struct {
		Tab   string `json:"tab"`
		Items []struct {
			ID      string `json:"id"`
			Title   string `json:"title"`
			Project *struct {
				ID string `json:"id"`
			} `json:"project"`
			Diff string   `json:"difficulty"`
			Caps []string `json:"required_capabilities"`
		} `json:"items"`
	} `json:"contributions"`
}

// exploreE2EGetIndex performs the read and returns the RAW body — the bytes a
// browser (or a hostile crawler) receives. A struct assertion can only prove
// a field is empty; the leak assertions read the body.
func exploreE2EGetIndex(t *testing.T, env *exploreE2EEnv) (string, *http.Response) {
	t.Helper()
	resp := env.do(t, http.MethodGet, "/api/v1/explore", "", nil)
	return string(bodyBytes(t, resp)), resp
}

func decodeExploreE2EIndex(t *testing.T, body string) exploreE2EIndex {
	t.Helper()
	var index exploreE2EIndex
	if err := json.Unmarshal([]byte(body), &index); err != nil {
		t.Fatalf("the index is not the JSON object the client reads: %v", err)
	}
	return index
}

// exploreE2EForbid asserts that none of the needles appears in the body,
// after first proving the check is not vacuous: each needle the caller
// expects to be present elsewhere (a public project's name, say) is passed
// as a required string and must be found. A leak assertion that cannot fail
// is worse than none, because it is believed.
func exploreE2EForbid(t *testing.T, body string, required []string, needles ...string) {
	t.Helper()
	for _, want := range required {
		if !strings.Contains(body, want) {
			t.Fatalf("the check is vacuous: %q is absent from the body too", want)
		}
	}
	for _, needle := range needles {
		if needle == "" {
			t.Fatalf("an empty needle would match everything; the fixture is wrong")
		}
		if strings.Contains(body, needle) {
			t.Errorf("the index contains %q; body = %s", needle, exploreE2EExcerpt(body))
		}
	}
}

// exploreE2EExcerpt returns the window around the first occurrence of a
// forbidden string, or the head of the body: a whole index printed into the
// failure log buries the assertion that failed.
func exploreE2EExcerpt(body string) string {
	const limit = 900
	if len(body) <= limit {
		return body
	}
	return body[:limit] + fmt.Sprintf("…(%d bytes)", len(body))
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

// TestE2EExploreIndexIsTheOpenNetworksSixDimensions is the requirement
// "Projects/Assets/Knowledge/People/Organizations/Open Contributions tabs"
// over the wire: one anonymous GET answers all six sections, each carrying
// its own tab name and the rows the readers resolved.
func TestE2EExploreIndexIsTheOpenNetworksSixDimensions(t *testing.T) {
	reader := &exploreE2EReader{}
	env, composite := exploreE2EStack(t, reader)
	open, private := exploreE2EPopulate(t, env, composite)

	reader.knowledge = []explore.KnowledgeRow{
		{
			PublicationID: "kp-open", ObjectID: exploreE2EKnowledgeOpenObjectID,
			ObjectType: "research_question", PublicVersion: "v1",
			Title: exploreE2EKnowledgeOpenTitle, ProjectID: open.ID,
			PublishedAt:    time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active",
		},
		{
			// A PRIVATE project's publication: it is public knowledge
			// (docs/12 §2 lets a private project publish) and the project
			// behind it must not be named.
			PublicationID: "kp-hidden", ObjectID: exploreE2EKnowledgeHidObjectID,
			ObjectType: "research_question", PublicVersion: "v2",
			Title: exploreE2EKnowledgeHiddenTitle, ProjectID: private.ID,
			PublishedAt:    time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
			LifecycleState: "superseded",
		},
	}
	reader.people = []explore.PersonRow{
		{ID: "00000000-0000-4000-8000-00000000000b", Handle: "explore-bob",
			DisplayName: "Bob Lin", Bio: "zeolite chemist",
			CreatedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)},
	}
	reader.organizations = []explore.OrganizationRow{
		{ID: "00000000-0000-4000-8000-00000000000c", Slug: "explore-institute",
			Name: "Explore Institute", CreatedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)},
	}
	publishedAt := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	composite.contributions = []contribution.ContributionOpportunity{
		{
			ID: "00000000-0000-4000-8000-00000000000d", ProjectID: open.ID,
			TargetType: contribution.TargetResearchQuestion,
			Title:      "Reproduce the screening", Description: "redo run 7",
			Difficulty:           contribution.DifficultyBeginner,
			RequiredCapabilities: []string{"python", "dft"},
			State:                contribution.OpportunityStateOpen,
			Visibility:           contribution.OpportunityVisibilityPublic,
			PublicizedAt:         &publishedAt,
		},
	}

	body, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/explore = %d: %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want JSON", ct)
	}
	index := decodeExploreE2EIndex(t, body)

	// Six sections, each knowing which dimension it is: a client cannot
	// mislabel a panel.
	for _, section := range []struct {
		name string
		tab  string
		n    int
	}{
		{"projects", index.Projects.Tab, len(index.Projects.Items)},
		{"assets", index.Assets.Tab, len(index.Assets.Items)},
		{"knowledge", index.Knowledge.Tab, len(index.Knowledge.Items)},
		{"people", index.People.Tab, len(index.People.Items)},
		{"organizations", index.Organizations.Tab, len(index.Organizations.Items)},
		{"contributions", index.Contributions.Tab, len(index.Contributions.Items)},
	} {
		if section.tab != section.name {
			t.Errorf("section %s carries tab %q", section.name, section.tab)
		}
		if section.n == 0 {
			t.Errorf("section %s is empty although its reader returned a row", section.name)
		}
	}

	// The Projects section is the REAL project service's public half: the
	// public project is there, the private one is not.
	if len(index.Projects.Items) != 1 || index.Projects.Items[0].Slug != exploreE2EOpenSlug {
		t.Fatalf("projects = %+v, want only the public project", index.Projects.Items)
	}

	// The knowledge row of the private project IS listed (it was published),
	// with no project named.
	var hidden *struct {
		ID      string `json:"id"`
		Title   string `json:"title"`
		Project *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"project"`
	}
	for i := range index.Knowledge.Items {
		if index.Knowledge.Items[i].Title == exploreE2EKnowledgeHiddenTitle {
			hidden = &index.Knowledge.Items[i]
		}
	}
	if hidden == nil {
		t.Fatalf("the published row of a private project is missing: %+v", index.Knowledge.Items)
	}
	if hidden.Project != nil {
		t.Errorf("the private project was named: %+v", hidden.Project)
	}

	// The contribution is listed, and it DOES name its public project.
	if len(index.Contributions.Items) != 1 {
		t.Fatalf("contributions = %+v", index.Contributions.Items)
	}
	if got := index.Contributions.Items[0].Caps; len(got) != 2 || got[0] != "dft" || got[1] != "python" {
		t.Errorf("capabilities = %v, want canonical order [dft python]", got)
	}
	if index.Contributions.Items[0].Project == nil {
		t.Errorf("the public project's opportunity lost its project name")
	}
}

// TestE2EExploreNeverNamesAPrivateProject is acceptance "private 不出现" read
// off the response BYTES: no private project's name, slug or id appears
// anywhere in the index — while the public project's does, so the check
// cannot pass by searching a body for something that was never there.
func TestE2EExploreNeverNamesAPrivateProject(t *testing.T) {
	reader := &exploreE2EReader{}
	env, composite := exploreE2EStack(t, reader)
	_, private := exploreE2EPopulate(t, env, composite)

	reader.knowledge = []explore.KnowledgeRow{
		{PublicationID: "kp-hidden", ObjectID: exploreE2EKnowledgeHidObjectID,
			ObjectType: "research_question", PublicVersion: "v2",
			Title: exploreE2EKnowledgeHiddenTitle, ProjectID: private.ID,
			PublishedAt:    time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active"},
		{PublicationID: "kp-originless", ObjectID: exploreE2EKnowledgeOpenObjectID,
			ObjectType: "research_question", PublicVersion: "v1",
			Title: exploreE2EKnowledgeOriginlessTitle, ProjectID: exploreE2EPrivateFixtureProjectID,
			PublishedAt:    time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active"},
	}
	reader.organizations = []explore.OrganizationRow{
		// An organization whose description mentions nothing private: the
		// section is here so the body is a full index, not a two-section
		// fragment.
		{ID: "00000000-0000-4000-8000-00000000000c", Slug: "explore-institute",
			Name: "Explore Institute", CreatedAt: time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)},
	}
	publishedAt := time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC)
	composite.contributions = []contribution.ContributionOpportunity{
		{
			ID: "00000000-0000-4000-8000-00000000000d", ProjectID: private.ID,
			TargetType:   contribution.TargetResearchQuestion,
			Title:        "Withheld project's opportunity",
			Description:  "work on unpublished alloy research",
			Difficulty:   contribution.DifficultyAdvanced,
			State:        contribution.OpportunityStateOpen,
			Visibility:   contribution.OpportunityVisibilityPublic,
			PublicizedAt: &publishedAt,
		},
	}

	body, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/explore = %d: %s", resp.StatusCode, body)
	}

	exploreE2EForbid(t, body,
		// Required: the public project and the public asset ARE named, so a
		// body that lost everything cannot pass this test.
		[]string{exploreE2EOpenName, exploreE2EOpenSlug, "Open Catalyst Screen"},
		// Required to be absent: the private project's identity in every form
		// it exists in, the private-publishing knowledge row's project, and
		// the id of a project this index never saw at all.
		exploreE2EPrivateName, exploreE2EPrivateSlug, private.ID,
		exploreE2EPrivateFixtureProjectID,
	)

	// The withheld rows are still LISTED — the publication and the
	// opportunity are public (docs/12 §2/§3), and hiding them would be a
	// second, stricter rule invented by this surface.
	for _, want := range []string{
		exploreE2EKnowledgeHiddenTitle, exploreE2EKnowledgeOriginlessTitle,
		"Withheld project's opportunity",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the public row %q is missing: the surface withheld something that is public", want)
		}
	}
}

// TestE2EExploreRanksByFreshnessAndNeverByPopularity is acceptance
// "无 like-based core ranking" over the wire: each section is ordered by its
// freshness key, newest first, whatever order the reader returned — and the
// payload carries no popularity field (nothing to rank on) and no total.
func TestE2EExploreRanksByFreshnessAndNeverByPopularity(t *testing.T) {
	reader := &exploreE2EReader{}
	env, composite := exploreE2EStack(t, reader)
	open, _ := exploreE2EPopulate(t, env, composite)

	// Deliberately OLDEST FIRST, and with the ids in the reverse order of the
	// dates: a surface that rendered the reader's order, or that sorted by
	// id, would produce a different list.
	reader.knowledge = []explore.KnowledgeRow{
		{PublicationID: "kp-old", ObjectID: "11111111-1111-4111-8111-1111111111c1",
			ObjectType: "research_question", PublicVersion: "v1", Title: "Oldest",
			ProjectID: open.ID, PublishedAt: time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active"},
		{PublicationID: "kp-new", ObjectID: "11111111-1111-4111-8111-1111111111c3",
			ObjectType: "research_question", PublicVersion: "v1", Title: "Newest",
			ProjectID: open.ID, PublishedAt: time.Date(2026, 9, 14, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active"},
		{PublicationID: "kp-mid", ObjectID: "11111111-1111-4111-8111-1111111111c2",
			ObjectType: "research_question", PublicVersion: "v1", Title: "Middle",
			ProjectID: open.ID, PublishedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
			LifecycleState: "active"},
	}
	reader.people = []explore.PersonRow{
		{ID: "u-old", Handle: "old", DisplayName: "Old", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "u-new", Handle: "new", DisplayName: "New", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}
	reader.organizations = []explore.OrganizationRow{
		{ID: "o-old", Slug: "org-old", Name: "Old Org", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
		{ID: "o-new", Slug: "org-new", Name: "New Org", CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
	}

	body, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/explore = %d: %s", resp.StatusCode, body)
	}
	index := decodeExploreE2EIndex(t, body)

	titles := make([]string, 0, len(index.Knowledge.Items))
	for _, k := range index.Knowledge.Items {
		titles = append(titles, k.Title)
	}
	if want := []string{"Newest", "Middle", "Oldest"}; !exploreE2EEqual(titles, want) {
		t.Errorf("knowledge order = %v, want newest-first %v", titles, want)
	}
	if index.People.Items[0].Handle != "new" {
		t.Errorf("people order = %+v, want the newest account first", index.People.Items)
	}
	if index.Organizations.Items[0].Slug != "org-new" {
		t.Errorf("organizations order = %+v, want the newest organization first", index.Organizations.Items)
	}
	// The assets section is the hub's own list, whose order is
	// latest_published_at: the fixture's newest is the PRIVATE project's
	// asset, which is listed (its version is public) with no project named.
	if want := "01j9z6k3m4n5p6q7r8s9t0v1x2"; index.Assets.Items[0].PID != want {
		t.Errorf("assets order = %+v, want the newest publication first (%s)", index.Assets.Items, want)
	}

	// Nothing to rank on: no popularity field, and no total to subtract the
	// visible rows from (docs/23 §5).
	exploreE2EForbid(t, body, []string{"Newest", "New Org"}, "like", "likes", "star", "stars",
		"popularity", "score", "votes", "trending", "total", "count", "view_count")

	// The people and organizations sections rank on a creation time that is
	// NOT a published fact: it must not be rendered (see PersonItem).
	var decoded map[string]struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"people", "organizations"} {
		for _, item := range decoded[section].Items {
			if _, ok := item["created_at"]; ok {
				t.Errorf("%s rendered a hidden ranking key: %v", section, item)
			}
		}
	}
}

// TestE2EExploreIsOneAnswerForEveryCaller proves the surface has no
// per-caller variant: an anonymous request and a signed-in one receive
// byte-identical answers, which is what makes serving it without a session
// safe rather than merely convenient.
func TestE2EExploreIsOneAnswerForEveryCaller(t *testing.T) {
	reader := &exploreE2EReader{}
	env, composite := exploreE2EStack(t, reader)
	open, private := exploreE2EPopulate(t, env, composite)
	reader.knowledge = []explore.KnowledgeRow{
		{PublicationID: "kp-open", ObjectID: exploreE2EKnowledgeOpenObjectID,
			ObjectType: "research_question", PublicVersion: "v1",
			Title: exploreE2EKnowledgeOpenTitle, ProjectID: open.ID,
			PublishedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC), LifecycleState: "active"},
		{PublicationID: "kp-hidden", ObjectID: exploreE2EKnowledgeHidObjectID,
			ObjectType: "research_question", PublicVersion: "v2",
			Title: exploreE2EKnowledgeHiddenTitle, ProjectID: private.ID,
			PublishedAt: time.Date(2026, 9, 11, 10, 0, 0, 0, time.UTC), LifecycleState: "active"},
	}

	anonymous, anonResp := exploreE2EGetIndex(t, env)
	if anonResp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous read = %d: %s", anonResp.StatusCode, anonymous)
	}

	// The project's own creator signs in — the caller with the strongest
	// claim to see more — and reads the same route.
	e2eSignup(t, env.e2eEnv, `{"email":"explore-carol@example.com","password":"long-enough-password-1","handle":"explore-carol","display_name":"Carol"}`)
	signedIn, signedResp := exploreE2EGetIndex(t, env)
	if signedResp.StatusCode != http.StatusOK {
		t.Fatalf("signed-in read = %d: %s", signedResp.StatusCode, signedIn)
	}

	if anonymous != signedIn {
		t.Errorf("the index differs by caller:\nanonymous = %s\nsigned-in = %s",
			exploreE2EExcerpt(anonymous), exploreE2EExcerpt(signedIn))
	}
	// And the member case is the one that matters: the private project is
	// still not named to a signed-in caller either — the private half of a
	// caller's world belongs to the signed-in surfaces, not to this index.
	exploreE2EForbid(t, signedIn, []string{exploreE2EOpenName},
		exploreE2EPrivateName, exploreE2EPrivateSlug, private.ID,
	)
}

// TestE2EExploreFailsWholeRatherThanShort drives the failure path: a reader
// that cannot answer makes the whole index a 503 with a wire code, never a
// short list that a reader would take for "there is nothing there".
func TestE2EExploreFailsWholeRatherThanShort(t *testing.T) {
	reader := &exploreE2EReader{}
	env, composite := exploreE2EStack(t, reader)
	open, _ := exploreE2EPopulate(t, env, composite)
	reader.knowledge = []explore.KnowledgeRow{
		{PublicationID: "kp-open", ObjectID: exploreE2EKnowledgeOpenObjectID,
			ObjectType: "research_question", PublicVersion: "v1",
			Title: exploreE2EKnowledgeOpenTitle, ProjectID: open.ID,
			PublishedAt: time.Date(2026, 9, 10, 10, 0, 0, 0, time.UTC), LifecycleState: "active"},
	}

	// The healthy read first, so the failure below is a change of one
	// variable and not "it never worked".
	healthy, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusOK || !strings.Contains(healthy, exploreE2EKnowledgeOpenTitle) {
		t.Fatalf("healthy read = %d: %s", resp.StatusCode, healthy)
	}

	reader.broken = true
	body, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("broken read = %d, want 503: %s", resp.StatusCode, body)
	}
	if !strings.Contains(body, explorehttp.CodeExploreUnavailable) {
		t.Errorf("the refusal does not carry the wire code: %s", body)
	}
	// No partial index: not one section, not one row. A 503 with rows in it
	// would be the partial answer this surface refuses to give.
	for _, forbidden := range []string{exploreE2EOpenName, exploreE2EKnowledgeOpenTitle, `"projects"`, `"items"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the refusal carried index content (%q): %s", forbidden, body)
		}
	}
}

// TestE2EExploreIsReadOnlyAndAnonymous pins the surface's edges: a write is
// refused by the shared guard before routing (this surface has no write to
// reach), and the read needs no session, no CSRF token and no query
// parameter — a caller cannot ask for a different index.
func TestE2EExploreIsReadOnlyAndAnonymous(t *testing.T) {
	reader := &exploreE2EReader{}
	env, _ := exploreE2EStack(t, reader)
	anonymous := freshJar()

	// Anonymous read, no cookie at all.
	env.client.Jar = anonymous
	body, resp := exploreE2EGetIndex(t, env)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous read = %d: %s", resp.StatusCode, body)
	}

	// A write never reaches the surface: the guard refuses it first, and it
	// refuses it the same way whether or not the route exists.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		resp := env.do(t, method, "/api/v1/explore", `{}`, nil)
		payload := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /api/v1/explore = %d, want 401 or 405: %s", method, resp.StatusCode, payload)
		}
		if strings.Contains(payload, exploreE2EOpenName) {
			t.Errorf("%s answered with index content: %s", method, payload)
		}
	}

	// A query string changes nothing: the surface takes no parameters, so
	// there is no projection to ask for and no filter to narrow by.
	withQuery := env.do(t, http.MethodGet, "/api/v1/explore?tab=people&limit=1", "", nil)
	if withQuery.StatusCode != http.StatusOK {
		t.Fatalf("read with a query string = %d", withQuery.StatusCode)
	}
	if string(bodyBytes(t, withQuery)) != body {
		t.Errorf("a query string changed the answer; the surface has no parameters")
	}
}

func exploreE2EEqual(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
