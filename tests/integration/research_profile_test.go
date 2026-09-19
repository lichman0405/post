// Task T0808 — Research Profile / Organization Profile over REAL PostgreSQL:
// the two read surfaces composed as cmd/api/main.go composes them, driven over
// real HTTP against the real schema, through the real store
// (internal/persistence.ResearchProfileStore) and the real queries
// (internal/persistence/queries/research_profile.sql) on connections the
// server refuses to write on.
//
// Why a second suite when tests/e2e/research_profile_e2e_test.go already
// drives both routes end to end: that suite substitutes the row source, and
// the two things this task can get wrong in a way no unit test sees are
// exactly the two a real database settles —
//
//  1. the SQL. Ten new queries, five new indexes, generated sqlc code and a
//     scanner per column: a join that attaches the wrong project to a credit,
//     a NULL that scans into a string, an ORDER BY that does not order, a
//     LIMIT whose tiebreaker is not unique. tests/e2e cannot see any of
//     them, because it answers with Go structs.
//
//  2. the ROWS the product cannot produce. The fixture below is RAW: the
//     database holds a private project that published a public version, a
//     second project's public usage of it, a disabled account, a
//     deactivated organization, a private version of an asset the subject is
//     credited on, and a ledger row whose actor is disabled. No writer in
//     this tree can create most of that, which is the point — the negative
//     assertions are made against state the application layer never had a
//     chance to filter on the way in.
//
// The two acceptances of this task are asserted on the RAW RESPONSE BYTES of
// the composed route, so a field added in the transport fails here:
//
//   - 离职后个人历史保留: the affiliation that ENDED in 2025 renders in full
//     (role, both calendar dates, the verified flag), the ledger rows the
//     person wrote while affiliated keep naming the organization on the
//     organization's own profile, and the ended membership is what makes the
//     NEXT events stop resolving there — asserted by the count of ledger rows
//     the organization still carries, with the raw table as the control.
//
//   - private info 不泄漏: the private project, the private version label and
//     the deactivated employer's identity reach no payload on either surface,
//     while the raw tables are shown to CONTAIN them (the control that keeps
//     the absence from being an empty fixture), and users.email appears
//     nowhere on either surface.
//
// Every negative block carries its positive half; a leak check that passed
// on an empty answer measured nothing.
//
// What is deliberately NOT here: the browser rendering (apps/web, exercised
// by its own unit tests and the web build) and the in-memory composition
// (tests/e2e, the required "research profile e2e").

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/researchprofilehttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/researchprofile"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// rpIntTaskID namespaces this task's test databases (test_T0808_<run_id>).
const rpIntTaskID = "T0808"

// The fixture's own vocabulary. Every string here is one the assertions below
// search the response BYTES for, in one direction or the other, so a leak
// check and its control are the same literal.
const (
	rpIntOrgSlug    = "rp-int-institute"
	rpIntOrgName    = "RP Int Institute"
	rpIntClosedSlug = "rp-int-wound-down"
	rpIntClosedName = "RP Int Wound Down Institute"

	rpIntOpenSlug   = "rp-int-open-lab"
	rpIntOpenName   = "RP Int Open Lab"
	rpIntSecretSlug = "rp-int-secret-lab"
	rpIntSecretName = "RP Int Secret Lab"

	// rpIntSecretAssetTitle is the title of an asset whose ORIGIN PROJECT is
	// private and whose version 1.0 is public. It must appear on the PERSON's
	// profile (a credit is a fact about a version, docs/11 §6) and NOT on the
	// organization's (which draws only from its own public projects). One
	// string, two surfaces, opposite outcomes: if both withheld it, a single
	// global filter would explain it and neither rule would be tested.
	rpIntSecretAssetTitle = "RP Int Secret Set"
	// rpIntPrivateVersion labels a version of the OPEN project's asset whose
	// stored visibility is private: it reaches no payload at all.
	rpIntPrivateVersion = "0.9-do-not-ship"
	// rpIntEmail is the subject's address. It is identity, not a profile
	// field, and no surface may print it.
	rpIntEmail = "rp-int-alice@example.com"
)

// rpIntFixture is every id the assertions need, plus the organizations and
// projects they talk about by name.
type rpIntFixture struct {
	alice, gone  string
	org, closed  string
	open, secret string
}

// rpIntWorld is the composed surface: a writable pool that seeds the fixture,
// and a read-only pool behind the profile store.
type rpIntWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool
	f        *rpIntFixture
}

// newRPIntWorld composes the production tree over one test database.
//
// Two pools over ONE database, and the split is the point: the writable one
// seeds the fixture; the read-only one backs the port the READ ROUTES use.
// "A profile cannot write" is then a property of the connections the reads
// run on rather than a promise about the code. The auth surface stays on the
// writable pool — signup is a write, and it is not part of these routes.
func newRPIntWorld(t *testing.T, ctx context.Context) *rpIntWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), rpIntTaskID)
	readOnly := openReadOnlyPool(t, ctx, dbURL)

	orgStore := persistence.NewOrgStore(pool)
	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          "http://web.test",
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})

	mux := http.NewServeMux()
	authAPI.Register(mux)
	// The REAL organization management subtree, registered exactly as
	// cmd/api/main.go registers it (the bare path beside the subtree). The
	// research-profile route IT MUST COEXIST WITH is the reason it is here:
	// a ServeMux conflict panics at wiring time, and a pattern that shadowed
	// the subtree would silently take the organization management routes
	// with it. Both are asserted below.
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	mux.Handle("/api/v1/organizations", orgAPI.Routes())
	mux.Handle("/api/v1/organizations/", orgAPI.Routes())
	researchprofilehttp.New(researchprofilehttp.Deps{
		Reader: persistence.NewResearchProfileStore(readOnly),
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &rpIntWorld{ts: ts, pool: pool, readOnly: readOnly, f: seedRPInt(t, ctx, pool)}
}

// --------------------------------------------------------------------------
// The fixture: raw rows

// seedRPInt writes the rows the assertions read. Every insert is SQL rather
// than a command, because the state this suite needs is state the product
// deliberately cannot produce: a private project that published, a public
// usage by a private project, a disabled account, a deactivated organization,
// an ENDED membership whose ledger rows remain.
func seedRPInt(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *rpIntFixture {
	t.Helper()
	f := &rpIntFixture{}

	uid := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %s: %v", sql, err)
		}
		return id
	}

	f.alice = uid(`INSERT INTO users (handle, display_name, email)
		VALUES ('rp-int-alice', 'RP Int Alice', $1) RETURNING id`, rpIntEmail)
	f.gone = uid(`INSERT INTO users (handle, display_name, disabled_at)
		VALUES ('rp-int-gone', 'RP Int Gone', now()) RETURNING id`)

	f.org = uid(`INSERT INTO organizations (slug, name, description)
		VALUES ($1, $2, 'heterogeneous catalysis') RETURNING id`, rpIntOrgSlug, rpIntOrgName)
	f.closed = uid(`INSERT INTO organizations (slug, name, deactivated_at)
		VALUES ($1, $2, now()) RETURNING id`, rpIntClosedSlug, rpIntClosedName)

	// The affiliation that ENDED, and one under an employer that was
	// deactivated afterwards. Both are the person's history (docs/04 §6).
	if _, err := pool.Exec(ctx, `INSERT INTO organization_memberships
		(organization_id, user_id, role, affiliation_start, affiliation_end, verified)
		VALUES ($1, $2, 'contributor', DATE '2024-01-15', DATE '2025-06-30', true)`,
		f.org, f.alice); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO organization_memberships
		(organization_id, user_id, role, affiliation_start, verified)
		VALUES ($1, $2, 'viewer', DATE '2023-01-01', false)`, f.closed, f.alice); err != nil {
		t.Fatalf("seed closed membership: %v", err)
	}

	f.open = uid(`INSERT INTO projects
		(organization_id, slug, name, purpose, activity_status, visibility, created_by)
		VALUES ($1, $2, $3, 'screen zeolites', 'active', 'public', $4) RETURNING id`,
		f.org, rpIntOpenSlug, rpIntOpenName, f.alice)
	f.secret = uid(`INSERT INTO projects
		(organization_id, slug, name, purpose, activity_status, visibility, created_by)
		VALUES ($1, $2, $3, 'internal screening', 'active', 'private', $4) RETURNING id`,
		f.org, rpIntSecretSlug, rpIntSecretName, f.alice)

	// Four ledger rows. The NEWEST one is on the private project — a reader
	// that leaked it would leak it at the TOP of the list, where the ordering
	// assertions below would see it. The third is the disabled account's, the
	// fourth carries no organization at all.
	// org is any so one call can pass NULL: a ledger row with no affiliation
	// at time is legal (00011) and the profile drops it.
	contrib := func(actor string, org any, project, eventType, role, at string, accepted, released bool, via string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO contribution_events
			(actor_id, organization_id_at_time, project_id, event_type, role_codes,
			 accepted_context, released_context, occurred_at, via)
			VALUES ($1, $2, $3, $4, ARRAY[$5], $6, $7, $8, $9)`,
			actor, org, project, eventType, role, accepted, released, at, via); err != nil {
			t.Fatalf("seed contribution: %v", err)
		}
	}
	contrib(f.alice, f.org, f.secret, "research_state.merged", "reviewer", "2026-02-28T10:00:00Z", true, false, "api")
	contrib(f.alice, f.org, f.open, "research_state.merged", "author", "2026-01-10T10:00:00Z", true, true, "web")
	contrib(f.gone, f.org, f.open, "research_state.merged", "author", "2026-03-05T10:00:00Z", true, false, "web")
	contrib(f.alice, nil, f.open, "research_state.merged", "author", "2026-03-10T10:00:00Z", true, false, "web")

	// Two assets: one in the public project, one in the private project. The
	// private project's version 1.0 is PUBLIC — docs/12 §2 lets a private
	// project publish, and the version then travels without the project.
	openAsset := uid(`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'rp-int-open-set', 'RP Int Open Set', $1) RETURNING id`, f.open)
	secretAsset := uid(`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'rp-int-secret-set', $1, $2) RETURNING id`,
		rpIntSecretAssetTitle, f.secret)

	// version publishes one version of an asset. origin_refs is the canonical
	// provenance pin every published version must carry (00056: NOT NULL,
	// non-empty, no null elements), and it names the ORIGIN PROJECT here
	// because that is what the asset's provenance is.
	version := func(assetID, projectID, version, visibility, publishedAt string) string {
		t.Helper()
		return uid(`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash,
			 published_by, published_at, origin_refs)
			VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, $3, 'ih-rp-int', $4, $5,
			        ARRAY['project:' || $6::text]) RETURNING id`,
			assetID, version, visibility, f.alice, publishedAt, projectID)
	}
	// The private version is the NEWER one: on the person's assets dimension
	// it sorts first if the visibility rule fails.
	openPrivate := version(openAsset, f.open, rpIntPrivateVersion, "private", "2026-02-20T10:00:00Z")
	openPublic := version(openAsset, f.open, "1.0", "public", "2026-01-12T10:00:00Z")
	secretPublic := version(secretAsset, f.secret, "1.0", "public", "2026-01-13T10:00:00Z")

	for i, versionID := range []string{openPrivate, openPublic, secretPublic} {
		if _, err := pool.Exec(ctx, `INSERT INTO asset_version_parties
			(asset_version_id, role, party_kind, party_id, position, recorded_by)
			VALUES ($1, 'creator', 'user', $2, $3, $2)`, versionID, f.alice, i); err != nil {
			t.Fatalf("seed credit: %v", err)
		}
	}

	// Four usages of versions the subject is credited on. Only the first may
	// render, and each of the other three fails a DIFFERENT condition, so a
	// rule that lost one of them still fails here.
	depend := func(projectID, versionID, dependencyType, usage string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO asset_dependencies
			(project_id, asset_version_id, dependency_type, visibility_of_usage, created_at)
			VALUES ($1, $2, $3, $4, '2026-01-20T10:00:00Z')`, projectID, versionID, dependencyType, usage); err != nil {
			t.Fatalf("seed dependency: %v", err)
		}
	}
	depend(f.open, openPublic, "depends_on", "public")    // renders
	depend(f.open, openPublic, "derived_from", "private") // usage not public
	depend(f.open, secretPublic, "depends_on", "public")  // used version's project private
	depend(f.secret, openPublic, "depends_on", "public")  // using project private

	// Three reproduction assertions by the subject, each with its visibility
	// given EXPLICITLY: 00091 added the assertion's own axis with
	// DEFAULT 'private', and a fixture that leaned on that default would be
	// asserting about a value it never chose. The chain is branches ->
	// project_states -> scientific_object_versions -> evidence_assertions,
	// seeded here because it is what the FK columns require.
	branch := uid(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'public', 'refs/heads/main', $2) RETURNING id`, f.open, f.alice)
	state := uid(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-rp-int', 'v1') RETURNING id`, f.open, branch)
	obj := uid(`INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'sample', $2) RETURNING id`, f.open, f.alice)
	objVer := func(no int) string {
		t.Helper()
		return uid(`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version,
			 title, lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, $2, $3, $4, 'core/sample', '1', 'RP Int Sample', 'active',
			 '{}'::jsonb, 'ih-rp-int', $5) RETURNING id`, obj, no, state, branch, f.alice)
	}
	target, evidence := objVer(1), objVer(2)
	assert := func(projectID, relation, reviewState, visibility, at string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO evidence_assertions
			(project_id, state_id, target_object_version_id, evidence_object_version_id,
			 relation_type, evidence_type, review_state, created_by, created_at, visibility)
			VALUES ($1, $2, $3, $4, $5, 'experimental', $6, $7, $8, $9)`,
			projectID, state, target, evidence, relation, reviewState, f.alice, at, visibility); err != nil {
			t.Fatalf("seed assertion: %v", err)
		}
	}
	// The two PUBLIC assertions render (one naming its project, one with its
	// private project withheld). The third is private ON ITS OWN AXIS and must
	// reach no payload: 00091's header is "an assertion nothing explicitly
	// made public is not rendered anywhere", and it carries the review state
	// no rendered row has plus the NEWEST created_at, so a leak would sort to
	// the top of the dimension.
	assert(f.open, "reproduces", "reviewed", "public", "2026-01-15T10:00:00Z")
	assert(f.secret, "fails_to_reproduce", "unreviewed", "public", "2026-02-25T10:00:00Z")
	assert(f.open, "reproduces", "rejected", "private", "2026-03-01T10:00:00Z")

	return f
}

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// internal/application/researchprofile, so a silent JSON tag change fails
// here)

type rpIntProjectRef struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	URL  string `json:"url"`
}

type rpIntPersonRef struct {
	ID          string `json:"id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	URL         string `json:"url"`
}

type rpIntAffiliation struct {
	Organization *struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"organization"`
	Role     string  `json:"role"`
	Start    *string `json:"affiliation_start"`
	End      *string `json:"affiliation_end"`
	Verified bool    `json:"verified"`
}

type rpIntContribution struct {
	EventType       string           `json:"event_type"`
	RoleCodes       []string         `json:"role_codes"`
	OccurredAt      time.Time        `json:"occurred_at"`
	AcceptedContext bool             `json:"accepted_context"`
	ReleasedContext bool             `json:"released_context"`
	Via             string           `json:"via"`
	Project         *rpIntProjectRef `json:"project"`
	Actor           *rpIntPersonRef  `json:"actor"`
}

type rpIntAsset struct {
	PID         string           `json:"pid"`
	Title       string           `json:"title"`
	AssetType   string           `json:"asset_type"`
	Version     string           `json:"version"`
	URL         string           `json:"url"`
	Role        string           `json:"role"`
	Project     *rpIntProjectRef `json:"project"`
	PublishedAt time.Time        `json:"published_at"`
}

type rpIntReuse struct {
	PID            string           `json:"pid"`
	Title          string           `json:"title"`
	Version        string           `json:"version"`
	URL            string           `json:"url"`
	AssetType      string           `json:"asset_type"`
	DependencyType string           `json:"dependency_type"`
	DeclaredAt     time.Time        `json:"declared_at"`
	Project        *rpIntProjectRef `json:"project"`
}

type rpIntReproduction struct {
	Relation    string           `json:"relation"`
	ReviewState string           `json:"review_state"`
	CreatedAt   time.Time        `json:"created_at"`
	Project     *rpIntProjectRef `json:"project"`
}

type rpIntPersonPayload struct {
	Person struct {
		ID          string `json:"id"`
		Handle      string `json:"handle"`
		DisplayName string `json:"display_name"`
		Bio         string `json:"bio"`
		URL         string `json:"url"`
	} `json:"person"`
	Affiliations  []rpIntAffiliation  `json:"affiliations"`
	Contributions []rpIntContribution `json:"contributions"`
	Assets        []rpIntAsset        `json:"assets"`
	Reuse         []rpIntReuse        `json:"reuse"`
	Reproductions []rpIntReproduction `json:"reproductions"`
	Projects      []rpIntProjectRef   `json:"projects"`
}

type rpIntOrgPayload struct {
	Organization struct {
		ID          string `json:"id"`
		Slug        string `json:"slug"`
		Name        string `json:"name"`
		Description string `json:"description"`
		URL         string `json:"url"`
	} `json:"organization"`
	Projects []struct {
		ID             string `json:"id"`
		Slug           string `json:"slug"`
		Name           string `json:"name"`
		Purpose        string `json:"purpose"`
		ActivityStatus string `json:"activity_status"`
		URL            string `json:"url"`
	} `json:"projects"`
	Activity []rpIntContribution `json:"activity"`
	Assets   []rpIntAsset        `json:"assets"`
}

// rpIntForbiddenKey is the vocabulary a profile payload may not use as a field
// name: docs/13 §4 ("禁止单一分数"), docs/13 §6 and CLAUDE.md §9 invariant 13.
var rpIntForbiddenKey = regexp.MustCompile(`(?i)(score|rank|rating|weight|reputation|total|count|impact|percentile|points|karma|metric)`)

// --------------------------------------------------------------------------
// Helpers

func (w *rpIntWorld) get(t *testing.T, path string) (int, string) {
	t.Helper()
	resp, err := w.ts.Client().Get(w.ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readAll(t, resp)
}

func (w *rpIntWorld) personPayload(t *testing.T, userID string) (rpIntPersonPayload, string) {
	t.Helper()
	status, raw := w.get(t, "/api/v1/users/"+userID+"/research-profile")
	if status != http.StatusOK {
		t.Fatalf("GET research-profile(%s) = %d: %s", userID, status, raw)
	}
	var payload rpIntPersonPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("research-profile is not the JSON a client reads: %v\n%s", err, raw)
	}
	return payload, raw
}

func (w *rpIntWorld) orgPayload(t *testing.T, slug string) (rpIntOrgPayload, string) {
	t.Helper()
	status, raw := w.get(t, "/api/v1/organizations/"+slug+"/profile")
	if status != http.StatusOK {
		t.Fatalf("GET organizations/%s/profile = %d: %s", slug, status, raw)
	}
	var payload rpIntOrgPayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("organization profile is not the JSON a client reads: %v\n%s", err, raw)
	}
	return payload, raw
}

// count asks the DATABASE, not the API: it is how a control proves the
// fixture really holds the row an absence is being claimed about.
func (w *rpIntWorld) count(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %s: %v", sql, err)
	}
	return n
}

// rpIntJSONKeys returns every object key of a JSON document, at any depth.
func rpIntJSONKeys(t *testing.T, raw string) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("payload is not JSON: %v\n%s", err, raw)
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

// ==========================================================================
// The tests

// TestResearchProfileIntegration is the person surface over real rows.
func TestResearchProfileIntegration(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)
	f := w.f

	// --- The fixture is RAW, and this is the control for every absence below.
	// If the private rows were not in the database, "the payload does not
	// carry them" would be a statement about an empty table.
	if got := w.count(t, ctx, `SELECT count(*) FROM contribution_events WHERE actor_id = $1`, f.alice); got != 3 {
		t.Fatalf("the fixture holds %d ledger rows for the subject, want 3 "+
			"(one private-project, one public-project, one unaffiliated): the leak checks below measure nothing", got)
	}
	if got := w.count(t, ctx, `SELECT count(*) FROM projects WHERE organization_id = $1 AND visibility = 'private'`, f.org); got != 1 {
		t.Fatalf("the fixture holds %d private projects, want 1", got)
	}
	if got := w.count(t, ctx, `SELECT count(*) FROM research_asset_versions WHERE visibility = 'private'`); got != 1 {
		t.Fatalf("the fixture holds %d private versions, want 1", got)
	}
	if got := w.count(t, ctx, `SELECT count(*) FROM asset_dependencies`); got != 4 {
		t.Fatalf("the fixture holds %d dependencies, want 4: the reuse rules below would be untested", got)
	}

	payload, raw := w.personPayload(t, f.alice)

	// --- The positive half: what the rules DO let through ---------------
	if payload.Person.ID != f.alice || payload.Person.Handle != "rp-int-alice" {
		t.Errorf("person = %+v, want the subject", payload.Person)
	}
	if payload.Person.URL != "/users/"+f.alice {
		t.Errorf("person url = %q, want the profile route", payload.Person.URL)
	}
	for _, want := range []string{rpIntEmail} {
		if strings.Contains(raw, want) {
			t.Errorf("the payload carries %q: users.email is identity, not a profile field", want)
		}
	}

	// Contributions: the two rows whose project may be named, newest first.
	// The private-project row is GONE — dropped, not rendered with a nil
	// project (a nil project would still say "this person did something on
	// 2026-02-28") and, because it is the NEWEST row in the table, a leak
	// would put it at the TOP of this list. The row with no affiliation at
	// time renders: an organization_id_at_time of NULL says nothing about the
	// work, which is a contribution to a public project like any other.
	if len(payload.Contributions) != 2 {
		t.Fatalf("contributions = %d, want 2 (the private-project row is dropped):\n%s",
			len(payload.Contributions), raw)
	}
	for i, c := range payload.Contributions {
		if c.Project == nil || c.Project.Slug != rpIntOpenSlug {
			t.Errorf("contribution[%d] project = %+v, want the public project", i, c.Project)
		}
		if c.Actor != nil {
			t.Errorf("the person's own profile names an actor on their own contribution: %+v", c.Actor)
		}
		if c.EventType != "research_state.merged" || !c.AcceptedContext {
			t.Errorf("contribution[%d] facts = %+v, want the stored envelope", i, c)
		}
	}
	c := payload.Contributions[0]
	// Ordering: the newest row that may be rendered comes first.
	if want := time.Date(2026, time.March, 10, 10, 0, 0, 0, time.UTC); !c.OccurredAt.Equal(want) {
		t.Errorf("contributions are not newest-first: [0] = %s, want %s", c.OccurredAt, want)
	}
	// ... and it is the row with NO organization at time, which the
	// organization's own profile therefore must not carry.
	if got := w.count(t, ctx, `SELECT count(*) FROM contribution_events
		WHERE actor_id = $1 AND occurred_at = $2 AND organization_id_at_time IS NULL`,
		f.alice, c.OccurredAt); got != 1 {
		t.Errorf("contributions[0] is not the unaffiliated row (%d matches)", got)
	}
	if c.Via != "web" || c.ReleasedContext || len(c.RoleCodes) != 1 || c.RoleCodes[0] != "author" {
		t.Errorf("contribution facts = %+v, want via=web, released=false, role_codes=[author]", c)
	}
	released := payload.Contributions[1]
	if want := time.Date(2026, time.January, 10, 10, 0, 0, 0, time.UTC); !released.OccurredAt.Equal(want) {
		t.Errorf("contributions[1] occurred_at = %s, want %s", released.OccurredAt, want)
	}
	if !released.ReleasedContext {
		t.Errorf("released_context = false, want the ledger row's released_context=true: %+v", released)
	}

	// Assets: two credits, the public version and the PRIVATE PROJECT's public
	// version — the latter without its project. The private VERSION is gone.
	if len(payload.Assets) != 2 {
		t.Fatalf("assets = %d, want 2 (public version + the private project's public version):\n%s",
			len(payload.Assets), raw)
	}
	byVersion := map[string]rpIntAsset{}
	for _, a := range payload.Assets {
		byVersion[a.Version] = a
	}
	open, ok := byVersion["1.0"]
	if !ok {
		t.Fatalf("the public version is missing:\n%s", raw)
	}
	if open.Title != "RP Int Open Set" || open.AssetType != "dataset" || open.Role != "creator" {
		t.Errorf("asset facts = %+v, want the stored title/type and the credited role", open)
	}
	if open.Project == nil || open.Project.Slug != rpIntOpenSlug {
		t.Errorf("the public project was withheld from its own asset: %+v", open.Project)
	}
	if open.URL == "" || !strings.HasPrefix(open.URL, "/assets/") {
		t.Errorf("asset url = %q, want the version's persistent address", open.URL)
	}
	if want := time.Date(2026, time.January, 12, 10, 0, 0, 0, time.UTC); !open.PublishedAt.Equal(want) {
		t.Errorf("published_at = %s, want %s", open.PublishedAt, want)
	}
	// The private project's version renders WITH THE TITLE and WITHOUT the
	// project — the same pair internal/assets.BuildBrowse renders.
	secret := rpIntAsset{}
	for _, a := range payload.Assets {
		if a.Title == rpIntSecretAssetTitle {
			secret = a
		}
	}
	if secret.PID == "" {
		t.Fatalf("the private project's PUBLIC version is missing from the person's assets: "+
			"a published version is a fact about the version, not about its project:\n%s", raw)
	}
	if secret.Project != nil {
		t.Errorf("the private origin project was named: %+v", secret.Project)
	}
	if secret.Role != "creator" {
		t.Errorf("the credit's role was lost with the project: %+v", secret)
	}

	// Reuse: only the both-public usage. One item — the other three fail a
	// DIFFERENT condition each (found by the counts above and below).
	if len(payload.Reuse) != 1 {
		t.Fatalf("reuse = %d, want 1:\n%s", len(payload.Reuse), raw)
	}
	r := payload.Reuse[0]
	if r.Project == nil || r.Project.Slug != rpIntOpenSlug {
		t.Errorf("reuse project = %+v, want the public using project", r.Project)
	}
	if r.DependencyType != "depends_on" || r.Version != "1.0" {
		t.Errorf("reuse facts = %+v, want the stored dependency row", r)
	}
	// asset_type is a real column and the reuse read selects it: a field the
	// payload carries but never fills would read as "no type" rather than as
	// an omission.
	if r.AssetType != "dataset" || r.Title != "RP Int Open Set" {
		t.Errorf("reuse asset facts = %+v, want the used version's own asset", r)
	}
	if want := time.Date(2026, time.January, 20, 10, 0, 0, 0, time.UTC); !r.DeclaredAt.Equal(want) {
		t.Errorf("declared_at = %s, want the dependency row's created_at %s", r.DeclaredAt, want)
	}

	// Reproductions: TWO rows render — a public assertion in the public project
	// (names its project) and a public assertion whose project is private
	// (rendered, project withheld; a failed reproduction is not a demerit).
	//
	// The third assertion in the database is private on its OWN axis and is NOT
	// here. 00091 gave evidence_assertions that axis for this route's sake —
	// "whether the assertion may be rendered by the PUBLIC network read" — and
	// this route is anonymous (cmd/api/main.go: the guard lets anonymous reads
	// through). The row exists in the database (the count below is the control)
	// and carries a review state and a created_at no rendered row has, so a leak
	// is visible as a count, as a byte and as the newest row.
	if got := w.count(t, ctx, `SELECT count(*) FROM evidence_assertions WHERE created_by = $1`, f.alice); got != 3 {
		t.Fatalf("the fixture holds %d assertions by the subject, want 3 "+
			"(two public, one private on its own axis): the leak check below would measure nothing", got)
	}
	if got := w.count(t, ctx, `SELECT count(*) FROM evidence_assertions
		WHERE created_by = $1 AND visibility = 'private'`, f.alice); got != 1 {
		t.Fatalf("the fixture holds %d private assertions, want 1", got)
	}
	if len(payload.Reproductions) != 2 {
		t.Fatalf("reproductions = %d, want 2 (the private assertion is dropped, not rendered):\n%s",
			len(payload.Reproductions), raw)
	}
	if strings.Contains(raw, "rejected") {
		t.Errorf("the payload carries the private assertion's review state: the row's own "+
			"visibility axis (00091) is not being read:\n%s", raw)
	}
	byRelation := map[string]rpIntReproduction{}
	for _, rep := range payload.Reproductions {
		byRelation[rep.Relation] = rep
	}
	pubRep, ok := byRelation["reproduces"]
	if !ok {
		t.Fatalf("the `reproduces` assertion is missing:\n%s", raw)
	}
	if pubRep.Project == nil || pubRep.Project.Slug != rpIntOpenSlug || pubRep.ReviewState != "reviewed" {
		t.Errorf("reproduces = %+v, want the public project and the stored review state", pubRep)
	}
	privRep, ok := byRelation["fails_to_reproduce"]
	if !ok {
		t.Fatalf("the `fails_to_reproduce` assertion was dropped with its project:\n%s", raw)
	}
	if privRep.Project != nil {
		t.Errorf("the private asserting project was named: %+v", privRep.Project)
	}
	if privRep.ReviewState != "unreviewed" {
		t.Errorf("review_state = %q, want the row's own stored state", privRep.ReviewState)
	}
	// The newest row that may be rendered is the 2026-02-25 one: the dropped
	// assertion is one day newer, so a leak would be at the TOP of the list.
	if want := time.Date(2026, time.February, 25, 10, 0, 0, 0, time.UTC); !payload.Reproductions[0].CreatedAt.Equal(want) {
		t.Errorf("reproductions are not newest-first over the RENDERABLE rows: [0] = %s, want %s",
			payload.Reproductions[0].CreatedAt, want)
	}

	// Projects: derived from the rows above, deduplicated, public ones only.
	if len(payload.Projects) != 1 || payload.Projects[0].Slug != rpIntOpenSlug {
		t.Errorf("projects = %+v, want exactly the one public project the rendered rows name", payload.Projects)
	}

	// --- 离职后个人历史保留 ------------------------------------------------
	// The ENDED affiliation renders in full. The membership ended on
	// 2025-06-30 and the person is still readable in 2026.
	if len(payload.Affiliations) != 2 {
		t.Fatalf("affiliations = %d, want 2 (one ended, one under a deactivated employer):\n%s",
			len(payload.Affiliations), raw)
	}
	var ended, withheld *rpIntAffiliation
	for i := range payload.Affiliations {
		a := &payload.Affiliations[i]
		if a.Organization == nil {
			withheld = a
			continue
		}
		if a.Organization.Slug == rpIntOrgSlug {
			ended = a
		}
	}
	if ended == nil {
		t.Fatalf("the ENDED affiliation is missing:\n%s", raw)
	}
	if ended.End == nil || *ended.End != "2025-06-30" {
		t.Errorf("affiliation_end = %v, want 2025-06-30", ended.End)
	}
	if ended.Start == nil || *ended.Start != "2024-01-15" {
		t.Errorf("affiliation_start = %v, want 2024-01-15", ended.Start)
	}
	if ended.Role != "contributor" || !ended.Verified {
		t.Errorf("the person's own history was not rendered in full: %+v", ended)
	}
	if ended.Organization.URL != "/organizations/"+rpIntOrgSlug {
		t.Errorf("affiliation organization url = %q", ended.Organization.URL)
	}
	// The deactivated employer's identity is withheld while the person's own
	// row survives: deactivation ends the organization's public profile, not
	// the affiliation of the people who worked there.
	if withheld == nil {
		t.Fatalf("the affiliation under a deactivated organization was dropped with it:\n%s", raw)
	}
	if withheld.Role != "viewer" || withheld.Start == nil || *withheld.Start != "2023-01-01" {
		t.Errorf("the person's row was lost with the employer: %+v", withheld)
	}

	// --- private info 不泄漏 ----------------------------------------------
	// On the SAME bytes: no withheld identity, no private version, no
	// organization that was deactivated.
	for _, forbidden := range []string{
		rpIntSecretSlug, rpIntSecretName, // the private project
		rpIntPrivateVersion,        // the private version's label
		rpIntClosedSlug,            // the deactivated employer
		rpIntClosedName,            // ... by name too
		"rp-int-gone",              // the disabled account
		rpIntEmail,                 // the address
		f.secret, f.closed, f.gone, // ... and their raw ids
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the payload carries %q, which no rule lets it render:\n%s", forbidden, raw)
		}
	}

	// No score-shaped key, at any depth (docs/13 §4, CLAUDE.md §9.13).
	for _, key := range rpIntJSONKeys(t, raw) {
		if rpIntForbiddenKey.MatchString(key) {
			t.Errorf("the payload carries the key %q: a profile shows evidence, not a score", key)
		}
	}
}

// TestResearchProfileIntegrationOrganization is the organization surface over
// the same rows.
func TestResearchProfileIntegrationOrganization(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)
	f := w.f

	// The control: the ledger really holds two rows attributable to the
	// organization (the subject's two, one of them on the private project) and
	// one whose actor is disabled.
	if got := w.count(t, ctx, `SELECT count(*) FROM contribution_events WHERE organization_id_at_time = $1`, f.org); got != 3 {
		t.Fatalf("the fixture holds %d rows for the organization, want 3", got)
	}

	payload, raw := w.orgPayload(t, rpIntOrgSlug)

	if payload.Organization.ID != f.org || payload.Organization.Slug != rpIntOrgSlug ||
		payload.Organization.Name != rpIntOrgName || payload.Organization.Description != "heterogeneous catalysis" {
		t.Errorf("organization = %+v, want the stored row", payload.Organization)
	}
	if payload.Organization.URL != "/organizations/"+rpIntOrgSlug {
		t.Errorf("organization url = %q", payload.Organization.URL)
	}

	// Projects: public only. The private project is in the database (asserted
	// above) and is not on the institution's public page.
	if len(payload.Projects) != 1 {
		t.Fatalf("projects = %d, want 1 public project:\n%s", len(payload.Projects), raw)
	}
	if p := payload.Projects[0]; p.Slug != rpIntOpenSlug || p.Name != rpIntOpenName ||
		p.Purpose != "screen zeolites" || p.ActivityStatus != "active" {
		t.Errorf("project = %+v, want the stored facts", p)
	}

	// Activity: the two attributable rows whose project may be named, each
	// naming its actor. The private-project row and the disabled account's row
	// are gone.
	if len(payload.Activity) != 1 {
		t.Fatalf("activity = %d, want 1:\n%s", len(payload.Activity), raw)
	}
	a := payload.Activity[0]
	if a.Actor == nil || a.Actor.Handle != "rp-int-alice" {
		t.Errorf("activity actor = %+v, want the affiliated person", a.Actor)
	}
	if a.Actor != nil && a.Actor.URL != "/users/"+f.alice {
		t.Errorf("activity actor url = %q, want the profile route", a.Actor.URL)
	}
	if a.Project == nil || a.Project.Slug != rpIntOpenSlug {
		t.Errorf("activity project = %+v, want the public project", a.Project)
	}

	// Assets: the organization's own PUBLIC projects' versions — so the public
	// project's version, and NOT the private project's public version, even
	// though that version is public and belongs to a project this
	// organization owns. The two surfaces differ here on purpose: the person's
	// profile lists the CREDIT, the organization's page lists what the
	// institution's public work produced.
	if len(payload.Assets) != 1 {
		t.Fatalf("assets = %d, want 1 (the public project's version):\n%s", len(payload.Assets), raw)
	}
	if got := payload.Assets[0]; got.Title != "RP Int Open Set" || got.Project == nil || got.Project.Slug != rpIntOpenSlug {
		t.Errorf("asset = %+v, want the public project's own version", got)
	}

	// 离职后个人历史保留, from the organization's side: the ledger rows naming
	// this organization SURVIVE the end of the affiliation that produced them.
	// The membership ended 2025-06-30; the rows are from 2026 and still name
	// the organization.
	// All three of them, including the one on the private project and the one
	// whose actor is disabled: the DATA is there, and the page renders one.
	if got := w.count(t, ctx, `SELECT count(*) FROM contribution_events ce
		JOIN organization_memberships om ON om.organization_id = ce.organization_id_at_time
		WHERE ce.organization_id_at_time = $1 AND om.affiliation_end IS NOT NULL
		  AND ce.occurred_at > om.affiliation_end`, f.org); got != 3 {
		t.Fatalf("the fixture's attributable rows do not all postdate the ended membership: %d, want 3 "+
			"— without that, this suite does not test 离职后个人历史保留 at all", got)
	}

	// private info 不泄漏, on the same bytes.
	for _, forbidden := range []string{
		rpIntSecretSlug, rpIntSecretName, rpIntSecretAssetTitle,
		rpIntPrivateVersion, rpIntClosedSlug, rpIntClosedName,
		"rp-int-gone", rpIntEmail,
		f.secret, f.closed, f.gone,
	} {
		if strings.Contains(raw, forbidden) {
			t.Errorf("the organization payload carries %q:\n%s", forbidden, raw)
		}
	}
	for _, key := range rpIntJSONKeys(t, raw) {
		if rpIntForbiddenKey.MatchString(key) {
			t.Errorf("the organization payload carries the key %q", key)
		}
	}
}

// TestResearchProfileIntegrationRoutesCoexistWithOrganizations pins the
// registration against the REAL organization subtree. The profile route is
// one segment longer than the subtree's own two-segment pattern, and a
// ServeMux that resolved it to the subtree would answer the profile path with
// the organization's 401 (anonymous) — or, worse, a request for an
// organization would resolve to the profile route.
func TestResearchProfileIntegrationRoutesCoexistWithOrganizations(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)

	// The three-segment profile path reaches the profile surface.
	if status, raw := w.get(t, "/api/v1/organizations/"+rpIntOrgSlug+"/profile"); status != http.StatusOK {
		t.Errorf("the profile path did not reach the profile surface: %d %s", status, raw)
	}
	// The two-segment path still reaches the organization management subtree,
	// which answers an anonymous caller its own 401 — NOT the profile
	// surface's 404 envelope, and not a redirect.
	status, raw := w.get(t, "/api/v1/organizations/"+w.f.org)
	if status != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/organizations/{orgId} = %d, want the orgs subtree's 401: %s", status, raw)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("the subtree's answer is not an envelope: %v %s", err, raw)
	}
	if env.Code == "ORG_NOT_FOUND" || env.Code == "USER_NOT_FOUND" {
		t.Errorf("the two-segment path was answered by the research-profile surface (code %q)", env.Code)
	}
	if strings.Contains(raw, rpIntOrgName) {
		t.Errorf("the anonymous organization read rendered the organization: %s", raw)
	}
}

// TestResearchProfileIntegrationExistenceHiding: every not-public outcome
// leaves as one indistinguishable 404 — an unknown id and a disabled account
// on the person route, an unknown slug and a deactivated organization on the
// organization route (docs/45).
func TestResearchProfileIntegrationExistenceHiding(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)
	f := w.f

	// The control: the disabled account and the deactivated organization
	// really are in the database, so the 404s below are decisions.
	if got := w.count(t, ctx, `SELECT count(*) FROM users WHERE id = $1 AND disabled_at IS NOT NULL`, f.gone); got != 1 {
		t.Fatal("the disabled-account control: the row is not disabled in the database")
	}
	if got := w.count(t, ctx, `SELECT count(*) FROM organizations WHERE id = $1 AND deactivated_at IS NOT NULL`, f.closed); got != 1 {
		t.Fatal("the deactivated-organization control: the row is not deactivated in the database")
	}

	type answer struct {
		status        int
		code, message string
	}
	askPerson := func(id string) answer {
		status, raw := w.get(t, "/api/v1/users/"+id+"/research-profile")
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil && status != http.StatusNotFound {
			t.Fatalf("GET research-profile(%s) = %d, not an envelope: %s", id, status, raw)
		}
		return answer{status, env.Code, env.Message}
	}
	askOrg := func(slug string) answer {
		status, raw := w.get(t, "/api/v1/organizations/"+slug+"/profile")
		var env struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(raw), &env); err != nil && status != http.StatusNotFound {
			t.Fatalf("GET organizations/%s/profile = %d, not an envelope: %s", slug, status, raw)
		}
		return answer{status, env.Code, env.Message}
	}

	unknownUser := askPerson("99999999-9999-4999-8999-999999999999")
	// The positive control for this block: an id that DOES exist answers 200.
	if status, _ := w.get(t, "/api/v1/users/"+f.alice+"/research-profile"); status != http.StatusOK {
		t.Fatalf("the control subject does not read: %d", status)
	}
	for _, tc := range []struct{ name, id string }{
		{"a malformed id", "not-a-uuid"},
		{"a disabled account", f.gone},
	} {
		got := askPerson(tc.id)
		if got.status != http.StatusNotFound || got.code == "" {
			t.Errorf("%s answered %d/%q, want the not-found envelope", tc.name, got.status, got.code)
		}
		if got.status != unknownUser.status || got.code != unknownUser.code || got.message != unknownUser.message {
			t.Errorf("%s answered %d/%q/%q while an unknown id answered %d/%q/%q — "+
				"a caller must not be able to tell them apart",
				tc.name, got.status, got.code, got.message,
				unknownUser.status, unknownUser.code, unknownUser.message)
		}
	}

	unknownOrg := askOrg("no-such-institute")
	if status, raw := w.get(t, "/api/v1/organizations/"+rpIntOrgSlug+"/profile"); status != http.StatusOK {
		t.Fatalf("the control organization does not read: %d %s", status, raw)
	}
	// A deactivated organization and a slug outside the identity charset
	// answer exactly as an unknown slug does.
	// rpIntDeactivatedSlug names the deactivated one: f.closed's slug.
	for _, tc := range []struct{ name, slug string }{
		{"a deactivated organization", rpIntClosedSlug},
		{"a malformed slug", "no_such_institute"},
	} {
		got := askOrg(tc.slug)
		if got.status != unknownOrg.status || got.code != unknownOrg.code || got.message != unknownOrg.message {
			t.Errorf("%s answered %d/%q/%q while an unknown slug answered %d/%q/%q",
				tc.name, got.status, got.code, got.message, unknownOrg.status, unknownOrg.code, unknownOrg.message)
		}
	}

	// And the normalized spelling still finds the organization: the slug is
	// its identity charset, case-insensitive after domain.NormalizeOrgSlug.
	if status, _ := w.get(t, "/api/v1/organizations/RP-Int-Institute/profile"); status != http.StatusOK {
		t.Errorf("the upper-case spelling of a real slug = %d, want 200", status)
	}
}

// TestResearchProfileIntegrationReadsWriteNothing: the profile routes run on
// connections the SERVER refuses to write on, and the whole database is
// fingerprinted before and after every read route.
func TestResearchProfileIntegrationReadsWriteNothing(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)

	var setting string
	if err := w.readOnly.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&setting); err != nil {
		t.Fatalf("SHOW default_transaction_read_only: %v", err)
	}
	if setting != "on" {
		t.Fatalf("the profile store's pool carries default_transaction_read_only=%q, want on", setting)
	}

	// The control: the probe is a REAL write, and the read-only session is
	// what stops it. Without this half, "nothing was written" could be a
	// statement about a broken probe.
	err := probeWrite(ctx, w.readOnly, "rp-int-write-probe")
	if err == nil {
		t.Fatal("a write through the profile store's pool succeeded: the read-only session is not in force")
	}
	if got := sqlState(t, err); got != readOnlySQLState {
		t.Fatalf("the write was refused with SQLSTATE %s, want %s: %v", got, readOnlySQLState, err)
	}

	before := fingerprintDatabase(t, ctx, w.pool)
	for _, path := range []string{
		"/api/v1/users/" + w.f.alice + "/research-profile",
		"/api/v1/users/" + w.f.gone + "/research-profile",
		"/api/v1/organizations/" + rpIntOrgSlug + "/profile",
		"/api/v1/organizations/" + rpIntClosedSlug + "/profile",
	} {
		status, raw := w.get(t, path)
		if status == http.StatusServiceUnavailable {
			t.Errorf("GET %s = 503 over the real schema: %s", path, raw)
		}
	}
	if after := fingerprintDatabase(t, ctx, w.pool); after != before {
		t.Errorf("the read routes changed the database.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// TestResearchProfileIntegrationMigrationIndexesExist is the check that the
// migration's five indexes are really on the tables they were written for:
// the migration catalog test asserts they are DECLARED
// (tests/integration/migration_test.go), and this asserts the server built
// them and a reader can use them.
func TestResearchProfileIntegrationMigrationIndexesExist(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)

	for _, tc := range []struct {
		index, table string
	}{
		{"contribution_events_actor_occurred_idx", "contribution_events"},
		{"contribution_events_org_occurred_idx", "contribution_events"},
		{"asset_version_parties_party_idx", "asset_version_parties"},
		{"evidence_assertions_author_relation_idx", "evidence_assertions"},
		{"asset_dependencies_version_idx", "asset_dependencies"},
	} {
		var got string
		if err := w.pool.QueryRow(ctx, `SELECT tablename FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = $1`, tc.index).Scan(&got); err != nil {
			t.Errorf("index %s is not in the database: %v", tc.index, err)
			continue
		}
		if got != tc.table {
			t.Errorf("index %s is on %s, want %s", tc.index, got, tc.table)
		}
	}

	// The readers' own access path, asked of the planner: the actor read is a
	// range scan over (actor_id, occurred_at DESC). A scan is a function of
	// the planner's statistics, so this asserts the index is CHOOSABLE for the
	// shape the query has, not that the plan is pinned.
	rows, err := w.pool.Query(ctx, `EXPLAIN (COSTS OFF)
		SELECT id FROM contribution_events
		WHERE actor_id = $1 ORDER BY occurred_at DESC, id DESC LIMIT 200`, w.f.alice)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var planLines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("EXPLAIN scan: %v", err)
		}
		planLines = append(planLines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EXPLAIN rows: %v", err)
	}
	// Every line: EXPLAIN answers one row PER PLAN NODE, so reading only the
	// first sees "Limit" and nothing else — a probe that reports success while
	// measuring nothing.
	plan := strings.Join(planLines, "\n")
	if !strings.Contains(plan, "contribution_events_actor_occurred_idx") &&
		!strings.Contains(plan, "contribution_events_actor_id_") {
		t.Errorf("the actor read has no index to use:\n%s", plan)
	}
}

// TestResearchProfileIntegrationUnknownIdIsNotAnError is the fail-closed half
// of the store's contract: a value that cannot be a uuid is a "no such row",
// never a 500 with a driver's words in it. The route answers 404, and the
// body carries no SQLSTATE, no table name and no column name.
func TestResearchProfileIntegrationUnknownIdIsNotAnError(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)

	for _, path := range []string{
		"/api/v1/users/not-a-uuid/research-profile",
		"/api/v1/users/00000000-0000-4000-8000-000000000000/research-profile",
		"/api/v1/organizations/_/profile",
		"/api/v1/organizations/-/profile",
		"/api/v1/organizations/" + strings.Repeat("x", 200) + "/profile",
	} {
		status, raw := w.get(t, path)
		if status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404:\n%s", path, status, raw)
			continue
		}
		lower := strings.ToLower(raw)
		for _, forbidden := range []string{"sqlstate", "pgx", "select ", "from ", "invalid input syntax"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("GET %s leaked the store's cause (%q):\n%s", path, forbidden, raw)
			}
		}
	}
}

// ==========================================================================
// The bounded window

// rpIntBuriedRows is how many rows each burial writes: strictly more than the
// window a read can hold, so the renderable row behind them is outside it.
const rpIntBuriedRows = researchprofile.FetchLimit + 10

// rpIntBuried is the starvation fixture's vocabulary.
type rpIntBuried struct {
	person, org            string
	open, secret           string
	openAsset, secretAsset string
	publicVersion          string
}

// seedRPIntBuried writes one person and one organization whose NEWEST rows on
// every dimension are rows no rule renders, with one renderable row behind
// each burial.
//
// Written as raw SQL because what is under test is the read, not the writer:
// the product cannot produce most of this state at once, and the point is the
// shape of the window rather than the history that would have built it.
func seedRPIntBuried(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *rpIntBuried {
	t.Helper()
	b := &rpIntBuried{}
	uid := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed buried: %s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed buried: %s: %v", sql, err)
		}
	}

	b.person = uid(`INSERT INTO users (handle, display_name)
		VALUES ('rp-int-buried', 'RP Int Buried') RETURNING id`)
	b.org = uid(`INSERT INTO organizations (slug, name)
		VALUES ('rp-int-buried-institute', 'RP Int Buried Institute') RETURNING id`)
	b.open = uid(`INSERT INTO projects
		(organization_id, slug, name, purpose, activity_status, visibility, created_by)
		VALUES ($1, 'rp-int-buried-open-lab', 'RP Int Buried Open Lab', 'screen zeolites',
		        'active', 'public', $2) RETURNING id`, b.org, b.person)
	b.secret = uid(`INSERT INTO projects
		(organization_id, slug, name, purpose, activity_status, visibility, created_by)
		VALUES ($1, 'rp-int-buried-secret-lab', 'RP Int Buried Secret Lab', 'internal screening',
		        'active', 'private', $2) RETURNING id`, b.org, b.person)

	// 1. The ledger. Two facts are buried at once here: the person's
	// contributions dimension (the window is over actor_id) and the
	// organization's activity (the same rows carry organization_id_at_time).
	buried := `TIMESTAMPTZ '2026-06-01T10:00:00Z' + make_interval(secs => g)`
	exec(`INSERT INTO contribution_events
		(actor_id, organization_id_at_time, project_id, event_type, role_codes,
		 accepted_context, released_context, occurred_at, via)
		SELECT $1, $2, $3, 'research_state.merged', ARRAY['author'], true, false,
		       `+buried+`, 'web'
		FROM generate_series(1, $4) g`, b.person, b.org, b.secret, rpIntBuriedRows)
	exec(`INSERT INTO contribution_events
		(actor_id, organization_id_at_time, project_id, event_type, role_codes,
		 accepted_context, released_context, occurred_at, via)
		VALUES ($1, $2, $3, 'research_state.merged', ARRAY['author'], true, true,
		        '2026-01-01T10:00:00Z', 'web')`, b.person, b.org, b.open)

	// 2. The person's assets. One asset of the PUBLIC project with
	// FetchLimit+10 newer PRIVATE versions, every one of them credited to the
	// buried person (the window is over CREDITS), and the public version
	// behind them.
	b.openAsset = uid(`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'rp-int-buried-open-set', 'RP Int Buried Open Set', $1) RETURNING id`, b.open)
	drafts := `INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash,
		 published_by, published_at, origin_refs)
		SELECT $1, 'draft-' || lpad(g::text, 3, '0'), '{}'::jsonb, '{}'::jsonb, 'private',
		       'ih-buried', $2, ` + buried + `, ARRAY['project:' || $3::text]
		FROM generate_series(1, $4) g`
	exec(drafts, b.openAsset, b.person, b.open, rpIntBuriedRows)
	exec(`INSERT INTO asset_version_parties
		(asset_version_id, role, party_kind, party_id, position, recorded_by)
		SELECT rav.id, 'creator', 'user', $1, 0, $1 FROM research_asset_versions rav
		WHERE rav.asset_id = $2 AND rav.visibility = 'private'`, b.person, b.openAsset)
	b.publicVersion = uid(`INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash,
		 published_by, published_at, origin_refs)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'public', 'ih-buried', $2,
		        '2026-01-02T10:00:00Z', ARRAY['project:' || $3::text]) RETURNING id`,
		b.openAsset, b.person, b.open)
	exec(`INSERT INTO asset_version_parties
		(asset_version_id, role, party_kind, party_id, position, recorded_by)
		VALUES ($1, 'creator', 'user', $2, 0, $2)`, b.publicVersion, b.person)

	// 3. The organization's assets. The window is over the versions of ITS
	// projects, so the burial is a newer asset of the PRIVATE project: the
	// organization's page may not draw a version of a project it cannot name,
	// and before the predicate was in the read those rows filled the window.
	b.secretAsset = uid(`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'rp-int-buried-secret-set', 'RP Int Buried Secret Set', $1) RETURNING id`, b.secret)
	exec(drafts, b.secretAsset, b.person, b.secret, rpIntBuriedRows)

	// 4. Reproductions: the chain the FK columns require, FetchLimit+10 PRIVATE
	// assertions newer than the one public assertion. The buried rows are in
	// the PUBLIC project, which is what makes them a window problem rather than
	// only a leak: the model renders an assertion of a public project.
	branch := uid(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'public', 'refs/heads/main', $2) RETURNING id`, b.open, b.person)
	state := uid(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-rp-int-buried', 'v1') RETURNING id`, b.open, branch)
	obj := uid(`INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'sample', $2) RETURNING id`, b.open, b.person)
	objVer := func(no int) string {
		t.Helper()
		return uid(`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version,
			 title, lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, $2, $3, $4, 'core/sample', '1', 'RP Int Buried Sample', 'active',
			 '{}'::jsonb, 'ih-buried', $5) RETURNING id`, obj, no, state, branch, b.person)
	}
	target, evidence := objVer(1), objVer(2)
	exec(`INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, review_state, created_by, created_at, visibility)
		SELECT $1, $2, $3, $4, 'reproduces', 'experimental', 'unreviewed', $5,
		       `+buried+`, 'private'
		FROM generate_series(1, $6) g`, b.open, state, target, evidence, b.person, rpIntBuriedRows)
	exec(`INSERT INTO evidence_assertions
		(project_id, state_id, target_object_version_id, evidence_object_version_id,
		 relation_type, evidence_type, review_state, created_by, created_at, visibility)
		VALUES ($1, $2, $3, $4, 'reproduces', 'experimental', 'reviewed', $5,
		        '2026-01-03T10:00:00Z', 'public')`, b.open, state, target, evidence, b.person)

	return b
}

// TestResearchProfileWindowIsCountedInRenderableRows: every profile read is
// BOUNDED — it reads the newest researchprofile.FetchLimit rows — and the model
// then drops the rows no rule renders. Read RAW, that window is a resource a
// writer can exhaust: a person whose newest rows are all on a private project
// pushes their public ones out of the window, and their profile answers as if
// they had published nothing at all.
//
// That is a statement about the platform's own output rather than about
// disclosure — nothing private leaks either way — and it is wrong on its own
// terms. It is T1004's finding for this surface (tests/integration/feed_test.go,
// TestPrivateVersionsDoNotSuppressPublicOnes): each read states its render
// predicate in SQL and the LIMIT comes AFTER it, so the window is counted in
// rows the surface may render. Five dimensions are buried rather than one: the
// ten reads share a shape but not a predicate, and a window is per-query.
func TestResearchProfileWindowIsCountedInRenderableRows(t *testing.T) {
	ctx := testCtx(t)
	w := newRPIntWorld(t, ctx)
	b := seedRPIntBuried(t, ctx, w.pool)

	// The controls, asked of the DATABASE: each burial really is deeper than
	// the window and strictly newer than the row that must survive it. Without
	// them a green run could mean the fixture was simply small.
	for _, tc := range []struct {
		what string
		sql  string
		args []any
	}{
		{"ledger rows on the private project", `SELECT count(*) FROM contribution_events
			WHERE actor_id = $1 AND project_id = $2`, []any{b.person, b.secret}},
		{"credits on private versions of the public project's asset", `SELECT count(*) FROM asset_version_parties avp
			JOIN research_asset_versions rav ON rav.id = avp.asset_version_id
			WHERE avp.party_id = $1 AND rav.asset_id = $2 AND rav.visibility = 'private'`,
			[]any{b.person, b.openAsset}},
		{"private assertions by the subject", `SELECT count(*) FROM evidence_assertions
			WHERE created_by = $1 AND project_id = $2 AND visibility = 'private'`, []any{b.person, b.open}},
		{"versions of the organization's private project", `SELECT count(*) FROM research_asset_versions rav
			JOIN research_assets ra ON ra.id = rav.asset_id
			WHERE ra.origin_project_id = $1`, []any{b.secret}},
	} {
		if got := w.count(t, ctx, tc.sql, tc.args...); got <= researchprofile.FetchLimit {
			t.Fatalf("fixture: %d %s, which the window of %d already accommodates — "+
				"the probe below would pass for the wrong reason", got, tc.what, researchprofile.FetchLimit)
		}
	}

	// --- The person's profile: the public rows are all still there ----------
	payload, raw := w.personPayload(t, b.person)
	if len(payload.Contributions) != 1 {
		t.Fatalf("contributions = %d, want exactly the one public row: the window was spent on "+
			"private-project rows:\n%s", len(payload.Contributions), raw)
	}
	if want := time.Date(2026, time.January, 1, 10, 0, 0, 0, time.UTC); !payload.Contributions[0].OccurredAt.Equal(want) {
		t.Errorf("the surviving contribution is not the public one: %s", payload.Contributions[0].OccurredAt)
	}
	if len(payload.Assets) != 1 || payload.Assets[0].Version != "1.0" {
		t.Fatalf("assets = %+v, want exactly the public version: the window was spent on private versions",
			payload.Assets)
	}
	if len(payload.Reproductions) != 1 || payload.Reproductions[0].ReviewState != "reviewed" {
		t.Fatalf("reproductions = %+v, want exactly the public assertion: the window was spent on "+
			"private assertions", payload.Reproductions)
	}
	if strings.Contains(raw, "draft-") {
		t.Errorf("the person's payload renders a private version's label:\n%s", raw)
	}

	// --- The organization's profile: the same property, its own reads -------
	orgPayload, orgRaw := w.orgPayload(t, "rp-int-buried-institute")
	if len(orgPayload.Activity) != 1 {
		t.Fatalf("activity = %d, want exactly the one attributable public row: the window was "+
			"spent on private-project rows:\n%s", len(orgPayload.Activity), orgRaw)
	}
	if actor := orgPayload.Activity[0].Actor; actor == nil || actor.ID != b.person {
		t.Errorf("the surviving activity does not credit the buried person: %+v", actor)
	}
	if len(orgPayload.Assets) != 1 || orgPayload.Assets[0].Version != "1.0" {
		t.Fatalf("organization assets = %+v, want exactly the public project's public version",
			orgPayload.Assets)
	}
	if strings.Contains(orgRaw, "draft-") || strings.Contains(orgRaw, "rp-int-buried-secret-lab") {
		t.Errorf("the organization payload renders a row no rule lets it render:\n%s", orgRaw)
	}
}
