package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/profilehttp"
	"github.com/lichman0405/post/cmd/api/researchprofilehttp"
	"github.com/lichman0405/post/internal/application/researchprofile"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// The Research Profile / Organization Profile e2e (T0808 required test
// "research profile e2e"): GET /api/v1/users/{id}/research-profile and
// GET /api/v1/organizations/{slug}/profile driven over real HTTP through the
// production composition — one guarded /api/v1 mux carrying the auth surface,
// the profile surface, the new research-profile surface AND an
// /api/v1/organizations/ subtree handler, exactly the way cmd/api/main.go
// mounts them.
//
// What is real here and what is not, stated plainly, because the difference
// is the difference between this suite and tests/integration
// /research_profile_test.go:
//
//   - real: the guard (an anonymous GET flows through, a write does not), the
//     ROUTING of both new paths against the /api/v1/organizations/ subtree
//     that is already mounted there, the two handlers, the model and every
//     rule in it (which rows render, which identity may be named, the drop
//     without a count, the render cap), the indistinguishability of the 404s,
//     the 503-with-a-code failure path, and the wire JSON a browser receives.
//   - not real: the row source. The ten reads come from the in-memory reader
//     below, the same substitution tests/e2e/explore_e2e_test.go makes for
//     three of its six sections; tests/integration/research_profile_test.go
//     runs the real PostgreSQL adapter over the real schema, over rows that
//     are RAW (a private project, a disabled account, a deactivated
//     organization, a private version).
//
// The identities are REAL: the subject is created over the wire by a
// signed-up user, so the assertions below search the response bytes for a
// string the API is genuinely carrying.

// ---------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------

const (
	rpE2EOpenProjectSlug    = "rp-e2e-open-lab"
	rpE2EOpenProjectName    = "RP E2E Open Lab"
	rpE2EPrivateProjectSlug = "rp-e2e-secret-lab"
	rpE2EPrivateProjectName = "RP E2E Secret Lab"

	rpE2EOrgSlug    = "rp-e2e-institute"
	rpE2EOrgName    = "RP E2E Institute"
	rpE2EClosedSlug = "rp-e2e-wound-down"
	rpE2EClosedName = "RP E2E Wound Down Institute"
	// rpE2EDeactivatedSlug names a DEACTIVATED organization: it exists, and
	// the surface must answer it exactly as it answers an unknown slug.
	rpE2EDeactivatedSlug = "rp-e2e-deactivated"

	// rpE2EVersionPrivate labels a version whose stored visibility is
	// private: it is in the reader's answer and must reach no payload.
	rpE2EVersionPrivate = "0.9-do-not-ship"
	// rpE2EPrivateAssetTitle is the title of the asset the subject is
	// credited on through that private version.
	rpE2EPrivateAssetTitle = "RP E2E Private Version Asset"

	// rpE2EUnknownID is an id that names no account.
	rpE2EUnknownID = "99999999-9999-4999-8999-999999999999"
	// rpE2EGoneID names a DISABLED account.
	rpE2EGoneID = "11111111-1111-4111-8111-1111111111bb"
	// rpE2EMalformedID cannot be a uuid at all.
	rpE2EMalformedID = "not-a-uuid"
)

const (
	rpE2EOpenID   = "33333333-3333-4333-8333-3333333333aa"
	rpE2EPrivID   = "33333333-3333-4333-8333-3333333333bb"
	rpE2EOrgID    = "22222222-2222-4222-8222-2222222222aa"
	rpE2EClosedID = "22222222-2222-4222-8222-2222222222bb"
)

const rpE2EAssetPID = "01j9z6k3m4n5p6q7r8s9t0v1wb"

func rpE2EDay(d int) time.Time {
	return time.Date(2026, time.September, d, 10, 0, 0, 0, time.UTC)
}

func rpE2EPtr(t time.Time) *time.Time { return &t }

// rpE2EReader is the in-memory row source (see the file comment).
//
// It answers for the SUBJECT by whatever id it is asked about — the signed-up
// account's id is only known after the signup request — and reserves two ids
// for the two not-public outcomes: a disabled account and nothing at all.
// fail makes every read fail, which is how the 503 path is driven.
//
// subjectID is the id the SUBJECT signed up with, which the test learns from
// the signup response. It is a field rather than a constant because the ledger
// row an organization renders credits a PERSON (actor_id), so the fixture has
// to name the account that actually exists in this world for the rendered
// actor link to be dereferenceable. The test sets it once, between the signup
// request and the first profile request, and no request is in flight then.
type rpE2EReader struct {
	fail      bool
	subjectID string
}

func (r rpE2EReader) failWith(err error) error {
	if r.fail {
		return err
	}
	return nil
}

func (r rpE2EReader) GetPerson(ctx context.Context, userID string) (researchprofile.PersonRow, bool, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return researchprofile.PersonRow{}, false, err
	}
	switch userID {
	case rpE2EUnknownID, rpE2EMalformedID:
		return researchprofile.PersonRow{}, false, nil
	case rpE2EGoneID:
		return researchprofile.PersonRow{
			ID: rpE2EGoneID, Handle: "rp-e2e-gone", DisplayName: "RP E2E Gone",
			DisabledAt: rpE2EPtr(rpE2EDay(1)),
		}, true, nil
	}
	return researchprofile.PersonRow{
		ID: userID, Handle: "rp-e2e-alice", DisplayName: "RP E2E Alice",
		Bio: "zeolite screening",
	}, true, nil
}

func (r rpE2EReader) ListAffiliations(ctx context.Context, userID string) ([]researchprofile.AffiliationRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	start := rpE2EDay(1)
	end := rpE2EDay(20)
	return []researchprofile.AffiliationRow{
		{
			// The ENDED affiliation: acceptance 离职后个人历史保留.
			OrganizationID: rpE2EOrgID, OrganizationSlug: rpE2EOrgSlug, OrganizationName: rpE2EOrgName,
			Role: "contributor", AffiliationStart: &start, AffiliationEnd: &end, Verified: true,
		},
		{
			// A deactivated employer: the row is the person's history and
			// stays; the organization's identity does not surface.
			OrganizationID: rpE2EClosedID, OrganizationSlug: rpE2EClosedSlug,
			OrganizationName:          rpE2EClosedName,
			OrganizationDeactivatedAt: rpE2EPtr(rpE2EDay(2)),
			Role:                      "viewer", AffiliationStart: &start,
		},
	}, nil
}

func (r rpE2EReader) ListContributions(ctx context.Context, userID string) ([]researchprofile.ContributionRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.ContributionRow{
		{
			// The NEWEST row is the one that must not render: a leak of it
			// would sort to the top of the list.
			EventType: "research_state.merged", RoleCodes: []string{"reviewer", "author"},
			OccurredAt: rpE2EDay(28), Accepted: true, Via: "web",
			ProjectID: rpE2EPrivID, ProjectSlug: rpE2EPrivateProjectSlug,
			ProjectName: rpE2EPrivateProjectName, ProjectVisibility: "private",
		},
		{
			EventType: "research_state.merged", RoleCodes: []string{"author"},
			OccurredAt: rpE2EDay(10), Accepted: true, Released: true, Via: "api",
			ProjectID: rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
		},
	}, nil
}

func (r rpE2EReader) ListPersonAssets(ctx context.Context, userID string) ([]researchprofile.AssetCreditRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.AssetCreditRow{
		{
			PID: "01j9z6k3m4n5p6q7r8s9t0v1wa", Title: rpE2EPrivateAssetTitle, AssetType: "dataset",
			Version: rpE2EVersionPrivate, VersionVisibility: "private", Role: "creator",
			ProjectID: rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
			PublishedAt: rpE2EDay(25),
		},
		{
			PID: rpE2EAssetPID, Title: "RP E2E Public Dataset", AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public", Role: "creator",
			ProjectID: rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
			PublishedAt: rpE2EDay(12),
		},
	}, nil
}

func (r rpE2EReader) ListReuses(ctx context.Context, userID string) ([]researchprofile.ReuseRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.ReuseRow{
		{
			// A public declaration by a PRIVATE project: PageUsage's second
			// condition keeps it off the page.
			PID: rpE2EAssetPID, Title: "RP E2E Public Dataset", Version: "1.0",
			VersionVisibility: "public", VersionProjectVisibility: "public",
			UsageVisibility: "public", DependencyType: "depends_on", DeclaredAt: rpE2EDay(26),
			ProjectID: rpE2EPrivID, ProjectSlug: rpE2EPrivateProjectSlug,
			ProjectName: rpE2EPrivateProjectName, ProjectVisibility: "private",
		},
	}, nil
}

func (r rpE2EReader) ListReproductions(ctx context.Context, userID string) ([]researchprofile.ReproductionRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.ReproductionRow{
		{
			// An assertion nothing explicitly made public: 00091's visibility
			// column is DEFAULT 'private', and its header is explicit that
			// such a row is "not rendered anywhere". It is NEWER than the
			// public one, so a surface that dropped the rule would render it
			// first and the count assertion below would see two rows.
			Relation: "reproduces", ReviewState: "rejected", CreatedAt: rpE2EDay(16),
			AssertionVisibility: "private",
			ProjectID:           rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
		},
		{
			Relation: "fails_to_reproduce", ReviewState: "unreviewed", CreatedAt: rpE2EDay(15),
			AssertionVisibility: "public",
			ProjectID:           rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
		},
	}, nil
}

func (r rpE2EReader) GetOrganizationBySlug(ctx context.Context, slug string) (researchprofile.OrganizationRow, bool, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return researchprofile.OrganizationRow{}, false, err
	}
	switch slug {
	case rpE2EOrgSlug:
		return researchprofile.OrganizationRow{
			ID: rpE2EOrgID, Slug: rpE2EOrgSlug, Name: rpE2EOrgName, Description: "catalysis",
		}, true, nil
	case rpE2EDeactivatedSlug:
		return researchprofile.OrganizationRow{
			ID: rpE2EClosedID, Slug: rpE2EDeactivatedSlug, Name: rpE2EClosedName,
			DeactivatedAt: rpE2EPtr(rpE2EDay(2)),
		}, true, nil
	}
	return researchprofile.OrganizationRow{}, false, nil
}

func (r rpE2EReader) ListOrganizationProjects(ctx context.Context, organizationID string) ([]researchprofile.ProjectRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.ProjectRow{
		{
			ID: rpE2EOpenID, Slug: rpE2EOpenProjectSlug, Name: rpE2EOpenProjectName,
			Purpose: "screen zeolites", ActivityStatus: "active", Visibility: "public",
		},
		{
			ID: rpE2EPrivID, Slug: rpE2EPrivateProjectSlug, Name: rpE2EPrivateProjectName,
			Purpose: "internal", ActivityStatus: "active", Visibility: "private",
		},
	}, nil
}

func (r rpE2EReader) ListOrganizationAssets(ctx context.Context, organizationID string) ([]researchprofile.AssetCreditRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.AssetCreditRow{
		{
			PID: rpE2EAssetPID, Title: "RP E2E Public Dataset", AssetType: "dataset",
			Version: "1.0", VersionVisibility: "public",
			ProjectID: rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
			PublishedAt: rpE2EDay(12),
		},
	}, nil
}

func (r rpE2EReader) ListOrganizationActivity(ctx context.Context, organizationID string) ([]researchprofile.ContributionRow, error) {
	if err := r.failWith(context.DeadlineExceeded); err != nil {
		return nil, err
	}
	return []researchprofile.ContributionRow{
		{
			EventType: "research_state.merged", OccurredAt: rpE2EDay(10), Via: "web",
			ProjectID: rpE2EOpenID, ProjectSlug: rpE2EOpenProjectSlug,
			ProjectName: rpE2EOpenProjectName, ProjectVisibility: "public",
			// actor_id is a PERSON id: this row credits the subject, the
			// account that signed up in this world. The rendered actor link is
			// "/users/" + actor_id, so it has to resolve to a real account for
			// the assertion below to mean anything.
			ActorID: r.subjectID, ActorHandle: "rp-e2e-alice", ActorDisplayName: "RP E2E Alice",
		},
	}, nil
}

// ---------------------------------------------------------------------
// The composed surface
// ---------------------------------------------------------------------

type rpE2EEnv struct{ env *e2eEnv }

// newRPProfE2EEnv mirrors cmd/api main.go's product wiring: one guarded
// /api/v1 mux carrying the auth surface, the profile surface, the
// research-profile surface, and an /api/v1/organizations/ SUBTREE handler
// standing in for cmd/api/orgshttp (which needs PostgreSQL and has no
// in-memory adapter). The subtree is mounted here on purpose: it is the
// registration the new /api/v1/organizations/{slug}/profile pattern has to
// coexist with, and a ServeMux conflict would PANIC at wiring time rather
// than fail a request — which is why the composition is built inside the
// test rather than assumed.
func newRPProfE2EEnv(t *testing.T, reader *rpE2EReader) *rpE2EEnv {
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
	rpAPI := researchprofilehttp.New(researchprofilehttp.Deps{Reader: reader})

	v1 := http.NewServeMux()
	authAPI.Register(v1)
	profileAPI.Register(v1)
	// The stand-in for the organization management subtree: it answers the
	// two-segment path and NOT the profile path, so a request that reaches it
	// is a request that was routed as before.
	orgSubtree := http.NewServeMux()
	orgSubtree.HandleFunc("GET /api/v1/organizations/{orgId}", func(w http.ResponseWriter, r *http.Request) {
		authhttp.WriteJSON(w, http.StatusOK, map[string]string{"subtree": r.PathValue("orgId")})
	})
	v1.Handle("/api/v1/organizations/", orgSubtree)
	rpAPI.Register(v1)

	ts := httptest.NewServer(authAPI.Guard(v1))
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = redisClient.Close() })
	jar, _ := cookiejar.New(nil)
	return &rpE2EEnv{env: &e2eEnv{
		api:   ts,
		redis: mr,
		users: users,
		client: &http.Client{
			Jar:           jar,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}}
}

// rpForbiddenKey is the vocabulary a profile payload may not use as a field
// name: docs/13 §4 ("禁止单一分数。Profile 展示多维 evidence"), docs/13 §6
// (raw counts are not quality proxies) and CLAUDE.md §9 invariant 13 (no
// Truth Score / Research Score).
var rpForbiddenKey = regexp.MustCompile(`(?i)(score|rank|rating|weight|reputation|total|count|impact|percentile|points|karma|metric)`)

// ---------------------------------------------------------------------
// Required test "research profile e2e"
// ---------------------------------------------------------------------

// TestE2EResearchProfile is the required test. It drives both surfaces the
// way a browser does and asserts the two acceptances (离职后个人历史保留, private
// info 不泄漏) plus the no-score rule on the RAW BYTES of every answer — a
// field some layer added on its own fails here, which is what makes this a
// check on the whole surface rather than on one function. Every negative
// block carries its positive half, because a leak check that passed on an
// empty answer measured nothing.
func TestE2EResearchProfile(t *testing.T) {
	reader := &rpE2EReader{}
	world := newRPProfE2EEnv(t, reader)
	env := world.env

	// The subject signs up: the identity the profile names is one the real
	// profile store created over the wire.
	alice := e2eSignup(t, env, `{"email":"rp-e2e-alice@example.com","password":"long-enough-password-1","handle":"rp-e2e-alice","display_name":"RP E2E Alice"}`)
	aliceID := alice.User.ID
	// The ledger row the organization surface renders credits a person, so the
	// fixture names the account that just signed up: "/users/" + actor_id then
	// resolves to the profile that exists. No request is in flight here.
	reader.subjectID = aliceID

	// --- The person surface, anonymously --------------------------------
	env.client.Jar = freshJar()
	resp := env.do(t, http.MethodGet, "/api/v1/users/"+aliceID+"/research-profile", "", nil)
	raw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET research-profile = %d: %s", resp.StatusCode, raw)
	}

	// The positive half: every dimension docs/42 names is present, with the
	// public facts in it.
	for _, want := range []string{
		`"person"`, `"affiliations"`, `"contributions"`, `"assets"`, `"reuse"`, `"reproductions"`, `"projects"`,
		rpE2EOpenProjectSlug, rpE2EOpenProjectName,
		rpE2EOrgSlug, `"fails_to_reproduce"`, rpE2EAssetPID, alice.User.Handle,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("the profile is missing %s:\n%s", want, raw)
		}
	}

	// The negative half, on the same bytes: the private project's identity,
	// the withheld employer's identity, the private version label and the
	// private asset's title are all absent.
	for _, forbidden := range []string{
		rpE2EPrivateProjectSlug, rpE2EPrivateProjectName,
		rpE2EClosedSlug, rpE2EClosedName,
		rpE2EVersionPrivate, rpE2EPrivateAssetTitle,
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the payload carries %q, which no rule lets it render:\n%s", forbidden, raw)
		}
	}

	// No score-shaped key anywhere, at any depth.
	for _, key := range rpE2EJSONKeys(t, raw) {
		if rpForbiddenKey.MatchString(key) {
			t.Errorf("the payload carries the key %q: docs/13 §4 forbids a single score", key)
		}
	}

	// 离职后个人历史保留: the ENDED affiliation is rendered in full — role,
	// both dates, verification — even though the employment is over.
	var personProfile rpPersonProfileWire
	if err := json.Unmarshal([]byte(raw), &personProfile); err != nil {
		t.Fatalf("the profile is not the JSON a client reads: %v", err)
	}
	// A DROPPED contribution is absent, not blank: the private-project row is
	// one of the two the reader answered, and a rule that rendered it with an
	// empty project would satisfy every byte scan below (neither the slug nor
	// the name would appear) while still saying that the person did something
	// on 2026-09-28. The count is the one assertion a substring search cannot
	// make.
	if len(personProfile.Contributions) != 1 {
		t.Fatalf("contributions = %d, want 1 (the newest row is on a private project and is dropped):\n%s",
			len(personProfile.Contributions), raw)
	}
	if got := personProfile.Contributions[0].Project; got == nil || got.Slug != rpE2EOpenProjectSlug {
		t.Errorf("the rendered contribution does not name the public project: %+v", got)
	}

	// The assertion's OWN visibility (evidence_assertions.visibility, 00091):
	// the reader answers two assertions in the SAME public project, one
	// private and newer, one public. The private one must not render, and the
	// public one must — with its project still named, which is what makes the
	// first half a leak check rather than an empty-list check.
	if len(personProfile.Reproductions) != 1 {
		t.Fatalf("reproductions = %d, want exactly the public assertion (the private one is newer "+
			"and must not render):\n%s", len(personProfile.Reproductions), raw)
	}
	assertion := personProfile.Reproductions[0]
	if assertion.Relation != "fails_to_reproduce" || assertion.CreatedAt != rpE2EDay(15).Format(time.RFC3339) {
		t.Errorf("the row rendered is the private assertion (relation %q, created_at %q); the public "+
			"one is the control and must be the survivor", assertion.Relation, assertion.CreatedAt)
	}
	if assertion.Project == nil || assertion.Project.Name != rpE2EOpenProjectName {
		t.Errorf("the public assertion lost its project, which no rule withholds: %+v", assertion.Project)
	}
	// The private row's relation, on the raw bytes: a payload that carried it
	// would say the subject recorded a reproduction its author never published.
	if strings.Contains(raw, `"reproduces"`) {
		t.Errorf("the payload carries the private assertion's relation:\n%s", raw)
	}

	if len(personProfile.Affiliations) != 2 {
		t.Fatalf("affiliations = %d, want 2 (one ended, one whose employer was deactivated):\n%s",
			len(personProfile.Affiliations), raw)
	}
	var ended *rpAffiliationWire
	for i := range personProfile.Affiliations {
		a := personProfile.Affiliations[i]
		if a.Organization != nil && a.Organization.Slug == rpE2EOrgSlug {
			ended = &personProfile.Affiliations[i]
		}
	}
	if ended == nil {
		t.Fatalf("the ended affiliation is missing from the answer:\n%s", raw)
	}
	if ended.End == nil || *ended.End != "2026-09-20" {
		t.Errorf("affiliation_end = %v, want the recorded calendar day 2026-09-20", ended.End)
	}
	if ended.Start == nil || *ended.Start != "2026-09-01" || ended.Role != "contributor" || !ended.Verified {
		t.Errorf("the person's own history was not rendered in full: %+v", ended)
	}
	// And the deactivated employer's row survives with its identity withheld.
	var withheld *rpAffiliationWire
	for i := range personProfile.Affiliations {
		if personProfile.Affiliations[i].Organization == nil {
			withheld = &personProfile.Affiliations[i]
		}
	}
	if withheld == nil {
		t.Fatalf("the affiliation under a deactivated organization was dropped with it:\n%s", raw)
	}
	if withheld.Role != "viewer" || withheld.Start == nil {
		t.Errorf("the person's own row was lost with the employer: %+v", withheld)
	}

	// --- The organization surface, anonymously ---------------------------
	resp = env.do(t, http.MethodGet, "/api/v1/organizations/"+rpE2EOrgSlug+"/profile", "", nil)
	orgRaw := string(bodyBytes(t, resp))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("anonymous GET organization profile = %d: %s", resp.StatusCode, orgRaw)
	}
	for _, want := range []string{
		`"organization"`, `"projects"`, `"activity"`, `"assets"`,
		rpE2EOrgSlug, rpE2EOrgName, rpE2EOpenProjectSlug, alice.User.Handle,
	} {
		if !strings.Contains(orgRaw, want) {
			t.Errorf("the organization profile is missing %s:\n%s", want, orgRaw)
		}
	}
	for _, forbidden := range []string{rpE2EPrivateProjectSlug, rpE2EPrivateProjectName} {
		if strings.Contains(orgRaw, forbidden) {
			t.Errorf("the organization payload carries the private project %q:\n%s", forbidden, orgRaw)
		}
	}
	// The activity row's actor link, decoded rather than grepped: the handle
	// substring above would be satisfied by a row whose actor URL pointed at
	// anything at all (the fixture used to carry an ORGANIZATION id here, and
	// nothing noticed). A ledger row credits a person, so the link the
	// organization's page prints has to be that person's profile.
	var orgProfile rpOrganizationProfileWire
	if err := json.Unmarshal([]byte(orgRaw), &orgProfile); err != nil {
		t.Fatalf("the organization profile is not the JSON a client reads: %v", err)
	}
	if len(orgProfile.Activity) != 1 {
		t.Fatalf("activity = %d, want the 1 public ledger row:\n%s", len(orgProfile.Activity), orgRaw)
	}
	actor := orgProfile.Activity[0].Actor
	if actor == nil {
		t.Fatalf("the organization's activity names no actor, so nobody is credited:\n%s", orgRaw)
	}
	if actor.ID != aliceID {
		t.Errorf("the actor credited is %q, want the account that signed up (%q)", actor.ID, aliceID)
	}
	if actor.URL != "/users/"+aliceID {
		t.Errorf("the actor link is %q, want %q — the row credits a person and must link to that "+
			"person's profile", actor.URL, "/users/"+aliceID)
	}
	if actor.Handle != alice.User.Handle {
		t.Errorf("the actor handle is %q, want %q", actor.Handle, alice.User.Handle)
	}
	for _, key := range rpE2EJSONKeys(t, orgRaw) {
		if rpForbiddenKey.MatchString(key) {
			t.Errorf("the organization payload carries the key %q", key)
		}
	}

	// The slug is the organization's identity charset and is case-insensitive
	// after normalization (domain.NormalizeOrgSlug), so the same organization
	// answers under either spelling — and the answer is the same document.
	upper := string(bodyBytes(t, env.do(t, http.MethodGet, "/api/v1/organizations/RP-E2E-Institute/profile", "", nil)))
	if upper != orgRaw {
		t.Errorf("the upper-case spelling answered a different document:\n%s", upper)
	}

	// --- Not public is indistinguishable from not there ------------------
	// An unknown id, an id that is not a uuid, and a DISABLED account must
	// answer one indistinguishable 404 (docs/45 existence hiding).
	unknownUser := decodeEnvelope(t, env.do(t, http.MethodGet, "/api/v1/users/"+rpE2EUnknownID+"/research-profile", "", nil))
	for _, tc := range []struct {
		name string
		id   string
	}{{"malformed id", rpE2EMalformedID}, {"disabled account", rpE2EGoneID}} {
		got := decodeEnvelope(t, env.do(t, http.MethodGet, "/api/v1/users/"+tc.id+"/research-profile", "", nil))
		if got.Code != researchprofile.CodeUserNotFound {
			t.Errorf("%s: code = %q, want %q", tc.name, got.Code, researchprofile.CodeUserNotFound)
		}
		if got.Code != unknownUser.Code || got.Message != unknownUser.Message {
			t.Errorf("%s answered %q/%q while an unknown id answered %q/%q — the two must not be tellable apart",
				tc.name, got.Code, got.Message, unknownUser.Code, unknownUser.Message)
		}
	}

	// The same for the organization surface: an unknown slug, a deactivated
	// organization, and a slug outside the identity charset.
	unknownOrg := decodeEnvelope(t, env.do(t, http.MethodGet, "/api/v1/organizations/no-such-institute/profile", "", nil))
	if unknownOrg.Code != researchprofile.CodeOrgNotFound {
		t.Errorf("unknown slug code = %q, want %q", unknownOrg.Code, researchprofile.CodeOrgNotFound)
	}
	for _, tc := range []struct {
		name string
		slug string
	}{{"deactivated organization", rpE2EDeactivatedSlug}, {"malformed slug", "no_such_institute"}} {
		got := decodeEnvelope(t, env.do(t, http.MethodGet, "/api/v1/organizations/"+tc.slug+"/profile", "", nil))
		if got.Code != unknownOrg.Code || got.Message != unknownOrg.Message {
			t.Errorf("%s answered %q/%q while an unknown slug answered %q/%q — the two must not be tellable apart",
				tc.name, got.Code, got.Message, unknownOrg.Code, unknownOrg.Message)
		}
	}

	// --- Routing against the organization subtree ------------------------
	// The two-segment path still reaches the subtree handler; only the
	// profile path reaches the new surface. This is the registration
	// cmd/api/main.go composes, and getting it wrong would silently shadow
	// the organization management routes.
	subtreeResp := env.do(t, http.MethodGet, "/api/v1/organizations/whatever", "", nil)
	if subtreeResp.StatusCode != http.StatusOK {
		t.Fatalf("the organization subtree was shadowed: GET = %d", subtreeResp.StatusCode)
	}
	if body := string(bodyBytes(t, subtreeResp)); !strings.Contains(body, "subtree") {
		t.Errorf("the two-segment path did not reach the subtree handler: %s", body)
	}

	// --- The guard -------------------------------------------------------
	// An anonymous write is refused before it reaches either surface.
	if resp := env.do(t, http.MethodPost, "/api/v1/users/"+aliceID+"/research-profile", `{}`, nil); resp.StatusCode == http.StatusOK {
		t.Error("an anonymous POST to the research profile was accepted")
	}
	if resp := env.do(t, http.MethodDelete, "/api/v1/organizations/"+rpE2EOrgSlug+"/profile", "", nil); resp.StatusCode == http.StatusOK {
		t.Error("an anonymous DELETE of the organization profile was accepted")
	}
}

// rpPersonProfileWire decodes the two fields this suite asserts on. The rest
// of the document is checked on the raw bytes above, so a struct that named
// every field would only be a second place for the payload to be described.
type rpPersonProfileWire struct {
	Affiliations  []rpAffiliationWire  `json:"affiliations"`
	Contributions []rpContributionWire `json:"contributions"`
	Reproductions []rpReproductionWire `json:"reproductions"`
}

type rpContributionWire struct {
	Project *struct {
		Slug string `json:"slug"`
	} `json:"project"`
}

// rpReproductionWire reads a reproduction row including its project, which the
// public assertion is required to keep naming (a private PROJECT withholds the
// project; it never drops the assertion).
type rpReproductionWire struct {
	Relation    string `json:"relation"`
	ReviewState string `json:"review_state"`
	CreatedAt   string `json:"created_at"`
	Project     *struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"project"`
}

// rpOrganizationProfileWire is what the organization surface sends, including
// the actor a ledger row credits — the link that must resolve to a person.
type rpOrganizationProfileWire struct {
	Activity []struct {
		Actor *struct {
			ID     string `json:"id"`
			Handle string `json:"handle"`
			URL    string `json:"url"`
		} `json:"actor"`
	} `json:"activity"`
}

type rpAffiliationWire struct {
	Organization *struct {
		Slug string `json:"slug"`
	} `json:"organization"`
	Role     string  `json:"role"`
	Start    *string `json:"affiliation_start"`
	End      *string `json:"affiliation_end"`
	Verified bool    `json:"verified"`
}

// TestE2EResearchProfileServiceUnavailable: a reader that cannot answer makes
// the two surfaces answer 503 with the shared code — never an empty profile,
// which would say "there is nothing here" about a dimension that might be
// full.
func TestE2EResearchProfileServiceUnavailable(t *testing.T) {
	env := newRPProfE2EEnv(t, &rpE2EReader{fail: true}).env
	env.client.Jar = freshJar()

	for _, path := range []string{
		"/api/v1/users/rp-e2e-subject/research-profile",
		"/api/v1/organizations/" + rpE2EOrgSlug + "/profile",
	} {
		resp := env.do(t, http.MethodGet, path, "", nil)
		raw := string(bodyBytes(t, resp))
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("GET %s = %d, want 503:\n%s", path, resp.StatusCode, raw)
			continue
		}
		var env503 envelope
		if err := json.Unmarshal([]byte(raw), &env503); err != nil {
			t.Errorf("GET %s: body is not an error envelope: %v", path, err)
			continue
		}
		if env503.Code != researchprofile.CodeUnavailable {
			t.Errorf("GET %s: code = %q, want %q", path, env503.Code, researchprofile.CodeUnavailable)
		}
		// The envelope must not carry dependency detail (docs/22 §5). The
		// reader's cause is a deadline error, and none of its words may
		// surface.
		lower := strings.ToLower(raw)
		if strings.Contains(lower, "deadline") || strings.Contains(lower, "context") || strings.Contains(lower, "sql") {
			t.Errorf("GET %s leaked the store's cause:\n%s", path, raw)
		}
	}
}

// TestE2EResearchProfileHealthyCompositionAnswers200 is the control for the
// test above: with the SAME composition and a working reader, the surface
// answers 200. Without it, "503 on failure" could be satisfied by an API that
// answers 503 always — the check would pass while measuring nothing.
func TestE2EResearchProfileHealthyCompositionAnswers200(t *testing.T) {
	env := newRPProfE2EEnv(t, &rpE2EReader{}).env
	env.client.Jar = freshJar()
	resp := env.do(t, http.MethodGet, "/api/v1/users/rp-e2e-subject/research-profile", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the healthy composition answered %d: %s", resp.StatusCode, bodyBytes(t, resp))
	}
	raw := string(bodyBytes(t, resp))
	if !strings.Contains(raw, rpE2EOpenProjectSlug) {
		t.Errorf("the healthy answer carries no dimension content, so the 503 checks measured nothing:\n%s", raw)
	}
}

// rpE2EJSONKeys returns every object key of a JSON document, at any depth.
func rpE2EJSONKeys(t *testing.T, raw string) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the payload is not JSON: %v\n%s", err, raw)
	}
	var keys []string
	var walk func(node any)
	walk = func(node any) {
		switch typed := node.(type) {
		case map[string]any:
			for k, v := range typed {
				keys = append(keys, k)
				walk(v)
			}
		case []any:
			for _, v := range typed {
				walk(v)
			}
		}
	}
	walk(doc)
	return keys
}
