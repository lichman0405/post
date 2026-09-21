// Task T1107 — the required test "privacy suite", real-PostgreSQL half of
// the two gaps the task closes.
//
// The repository's permission negatives are already dense (privacy_test.go,
// project_authz_test.go, org_permission_test.go,
// activity_audience_failclosed_test.go, asset_publish_test.go,
// knowledge_publish_test.go, search_access_test.go, search_scope_test.go,
// retrieval_test.go, tests/e2e). This file does not restate any of them. It
// adds the things none of them could observe, and every one of them needs a
// real database because every one is a statement about what a READ leaves
// behind:
//
//  1. NO PRIVATE COUNT, AND NO ORACLE THAT STANDS IN FOR ONE. docs/23 §5:
//     "私有对象计数也不能通过 public API 泄漏". The asset page's model
//     refuses to render "N private projects use this" (internal/assets
//     PageUsage: "A usage that fails either condition is not rendered and
//     not counted"), and that refusal has unit tests. What had no test at
//     all was the BACKEND: a database in which several private projects
//     really do use a public object, read through every anonymous surface,
//     with the assertion made on the raw bytes. The instrument here is a
//     DIFFERENTIAL — each surface is read with the private rows absent and
//     again with them present, and the two bodies must be byte-identical.
//     That is strictly stronger than "the private name is absent": it
//     refuses an existence flag, a rendered count, a reordered list, a
//     changed faceting and an ETag that moved, in one comparison. Each
//     differential carries a CONTROL that adds a row the surface MUST
//     render, so a differential that measured nothing fails as loudly as
//     one that leaked.
//
//  2. UNREADABLE AND NONEXISTENT GET THE SAME ANSWER, ITEM BY ITEM.
//     docs/54 scenario #1 is "a private project's content showing up in
//     search/explore/an API error"; docs/45 §24 says an error tells the
//     caller what to do next and "不泄漏 private entity existence". The
//     suites already pin the CODE and the BODY for several surfaces; what
//     was not pinned is the whole answer — every response header as well as
//     the status, the code, the sentence and the byte length. A surface can
//     answer "PROJECT_NOT_FOUND / project not found" for both cases and
//     still be an oracle through a header it sets on one path, or through a
//     body that differs by one byte.
//
//  3. THE EXPLORE INDEX HAS NO POST-LIMIT ORACLE. The knowledge section's
//     read took its LIMIT over the whole corpus and applied the audience
//     rule afterwards, in Go — so a private project's publications spent
//     the window and the section came back short of the public corpus. That
//     shortfall is a count with the sign flipped, and it is what this
//     file's last test is written to catch: it buries twenty public
//     publications under twenty strictly-newer private ones and requires
//     the section's bytes not to move.
//
// # Timing is measured and is NOT a gate
//
// TestProjectAndAssetRefusalsAreIndistinguishable records how long each
// class of request took and prints the raw numbers. It asserts nothing about
// them. 规格没有给出任何时序阈值，所以本套件不设阈值 — the repository's only
// constant-time comparison is the CSRF check in
// cmd/api/authhttp/auth_middleware.go, and neither docs/32 nor docs/23
// makes response time a channel this platform claims to close. A threshold
// invented here would be a product rule this task has no authority to make
// (CLAUDE.md §5, L3), and a suite that went red on a slow CI runner would be
// teaching its readers to ignore it.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/feedshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/embedding"
	"github.com/lichman0405/post/internal/search/retrieval"
)

// privacySideChannelTaskID namespaces this file's test databases
// (test_T1107_<run_id>, docs/66 §3).
const privacySideChannelTaskID = "T1107"

// timingNote is the sentence the task requires, verbatim, next to the code
// that measures. It is a constant rather than a comment so the test can
// PRINT it: a reader who is shown the numbers must be shown this with them.
//
// 规格没有给出任何时序阈值，所以本套件不设阈值。
const timingNote = "规格没有给出任何时序阈值，所以本套件不设阈值"

// ==========================================================================
// The composed anonymous read surface
// ==========================================================================

// sideChannelWorld is the anonymous read surface as production wires it: the
// real guard, the real project read gate, the real asset page and browse
// reads, the project-side dependency read and the three feed routes — over
// READ-ONLY connections, so that "the reads do not write" is a property of
// the server rather than a promise about the code (the same split
// newExploreWorld and assetPageWorld make).
//
// Two ports are deliberately nil. assetshttp.Deps carries State and Publish
// for the two POST routes; this file drives nothing but GETs, and wiring a
// publish command would drag the policy service, the schema registry and the
// RSG validator in behind it for no assertion. The nil ports are named here
// rather than discovered by a panic: cmd/api/main.go wires all six.
type sideChannelWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool
}

// sideChannelWebOrigin is the configured public origin a feed's absolute
// links are built from (POST_WEB_ORIGIN in production). It is deliberately
// not the httptest server's own address: a feed must not echo a
// caller-supplied host.
const sideChannelWebOrigin = "http://web.test"

func newSideChannelWorld(t *testing.T, ctx context.Context) *sideChannelWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), privacySideChannelTaskID)
	readOnly := openReadOnlyPool(t, ctx, dbURL)

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          sideChannelWebOrigin,
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	// The gate, the membership bit and the public list — one project service,
	// over the read-only pool the other reads use.
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(readOnly),
		Orgs:  persistence.NewOrgStore(readOnly),
		Authz: authz.NewMatrixEngine(),
	})
	feedSvc, err := feeds.NewService(persistence.NewFeedStore(readOnly),
		feeds.Config{BaseURL: sideChannelWebOrigin})
	if err != nil {
		t.Fatalf("feeds.NewService: %v", err)
	}

	v1 := http.NewServeMux()
	v1.Handle("/api/v1/auth/", authAPI.Routes())
	v1.Handle("/api/v1/projects", projectAPI.Routes())
	v1.Handle("/api/v1/projects/", projectAPI.Routes())
	assetshttp.New(assetshttp.Deps{
		State:        nil, // the two POST routes; this file sends no writes
		Publish:      nil,
		Projects:     projectAPI.Service(),
		Pages:        persistence.NewAssetPageStore(readOnly),
		Members:      projectAPI.Service(),
		Dependencies: persistence.NewProjectDependencyStore(readOnly),
	}).Register(v1)
	feedshttp.New(feedshttp.Deps{Service: feedSvc}).Register(v1)

	ts := httptest.NewServer(authAPI.Guard(v1))
	t.Cleanup(ts.Close)
	return &sideChannelWorld{ts: ts, pool: pool, readOnly: readOnly}
}

// anonymousGET is one request with no cookie, no session and no CSRF token —
// what a crawler, a feed reader or a stranger's browser sends. Every
// assertion in this file is made over it, because "the public surface" is a
// claim about this request and nothing else.
//
// It returns the status, the headers, the raw body and the round trip's
// duration. The duration is RECORDED and never asserted on: see timingNote.
func (w *sideChannelWorld) anonymousGET(t *testing.T, path string) (int, http.Header, string, time.Duration) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, w.ts.URL+path, nil)
	if err != nil {
		t.Fatalf("build GET %s: %v", path, err)
	}
	start := time.Now()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return resp.StatusCode, resp.Header, string(body), time.Since(start)
}

// mustGET200 requires a 200 and returns the RAW body. An assertion made on a
// decoded struct cannot see a field the transport added, which is exactly
// the class of defect this file exists to catch.
func (w *sideChannelWorld) mustGET200(t *testing.T, path string) string {
	t.Helper()
	status, _, raw, _ := w.anonymousGET(t, path)
	if status != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200: %s", path, status, excerptForLog(raw))
	}
	return raw
}

// mustGETRaw is mustGET200 without the requirement — used where a test needs
// the raw bytes of a refusal.
func (w *sideChannelWorld) mustGETRaw(t *testing.T, path string) (int, http.Header, string) {
	t.Helper()
	status, header, raw, _ := w.anonymousGET(t, path)
	return status, header, raw
}

// snapshot reads every surface once and returns the bodies keyed by path.
func (w *sideChannelWorld) snapshot(t *testing.T, paths []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		out[p] = w.mustGET200(t, p)
	}
	return out
}

// excerptForLog bounds a body so a failure message stays readable.
func excerptForLog(s string) string {
	const max = 1500
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// ==========================================================================
// The fixture
// ==========================================================================

// sideChannelPrivateProject is one PRIVATE project that uses the public
// asset. Both halves matter: the usage row declares itself public, and the
// project's own visibility is the only thing withholding it — which is the
// sharper fixture, because a reader that trusted the row instead of the
// project's identity would render all three
// (internal/assets.PageUsage's second condition is the one that refuses
// them).
type sideChannelPrivateProject struct {
	id    string
	slug  string
	name  string
	owner string // the users.id the project's own asset is attributed to
}

// sideChannelFixture is the state the public surfaces are read against.
// Every string a negative assertion names is a REAL row's value.
type sideChannelFixture struct {
	// openProject is PUBLIC and owns the rendered asset, so an anonymous
	// caller reaches the asset page at all.
	openProject     string
	openProjectSlug string
	// subjectPID is the public asset every private project uses;
	// subjectPublicVersionID is its one PUBLIC version, and subjectAssetID
	// is the row id the later inserts key on.
	subjectPID             string
	subjectAssetID         string
	subjectPublicVersionID string
	// publicPeer is a second PUBLIC project, used only as the control: its
	// usage MUST move the rendered list.
	publicPeer     string
	publicPeerSlug string
	// others are the private projects. Three of them: one is enough to make
	// the point, and three make "a count appeared" distinguishable from a
	// coincidental 1.
	others []sideChannelPrivateProject
}

// seedSideChannelFixture writes the fixture with raw SQL.
//
// Raw SQL, not the publish command, for the reason
// TestPrivateVersionsDoNotSuppressPublicOnes gives for the same choice:
// what is under test is the READ, and the publish command would reach these
// exact rows through a much longer path — the schema registry, the policy
// service, the RSG validator — without adding a single fact the assertions
// use. It is also the repository's own precedent for exactly these tables:
// asset_page_test.go inserts asset_dependencies the same way and says why
// ("asset_lineage and asset_dependencies have no product writer in V1
// (T0709 ships the reader), so the rows ... are inserted here, as rows").
// Nothing in the fixture is a state production cannot reach.
func seedSideChannelFixture(t *testing.T, ctx context.Context, w *sideChannelWorld) *sideChannelFixture {
	t.Helper()
	pool := w.pool

	_, aliceID := signup(t, w.ts.URL, "side-alice@example.com", "side-alice")

	f := &sideChannelFixture{openProjectSlug: "side-open-lab", publicPeerSlug: "side-peer-lab"}

	f.openProject = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ($1, 'Side Channel Open Lab', 'T1107 fixture', 'public', $2) RETURNING id`,
		f.openProjectSlug, aliceID)
	f.publicPeer = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ($1, 'Side Channel Peer Lab', 'T1107 fixture', 'public', $2) RETURNING id`,
		f.publicPeerSlug, aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role)
		 VALUES ($1, $2, 'owner'), ($3, $2, 'owner')`,
		f.openProject, aliceID, f.publicPeer); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}

	// Three private projects, each owned by a different account, so that a
	// leak would have to name one of three identities rather than one, and
	// so that "one of them is the caller's own" is not a confound: the
	// caller here is anonymous.
	for i := 0; i < 3; i++ {
		handle := fmt.Sprintf("side-shadow-%d", i+1)
		_, ownerID := signup(t, w.ts.URL, handle+"@example.com", handle)
		proj := mustQueryUUID(t, ctx, pool,
			`INSERT INTO projects (slug, name, purpose, visibility, created_by)
			 VALUES ($1, $2, 'T1107 fixture', 'private', $3) RETURNING id`,
			handle, fmt.Sprintf("Side Channel Shadow %d", i+1), ownerID)
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			proj, ownerID); err != nil {
			t.Fatalf("seed shadow membership: %v", err)
		}
		f.others = append(f.others, sideChannelPrivateProject{
			id: proj, slug: handle, name: fmt.Sprintf("Side Channel Shadow %d", i+1), owner: ownerID,
		})
	}

	// The public asset: one PUBLIC version, so it is in the browse list and
	// has a page and a feed of its own. The pid is 26 Crockford characters,
	// the shape research_assets_pid_format requires (migration 00064).
	f.subjectPID = "3123456789abcdefghjkmnpqrs"
	f.subjectAssetID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', 'side-open-dataset', 'Side Channel Open Dataset', $1, $2) RETURNING id`,
		f.openProject, f.subjectPID)
	f.subjectPublicVersionID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_asset_versions
		   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, '1.0', '{"metadata":{}}'::jsonb, '{}'::jsonb, 'public', 'sha256:side-1.0', $2,
		         ARRAY['project:' || $3::text]) RETURNING id`,
		f.subjectAssetID, aliceID, f.openProject)

	// Each private project owns one asset of its own. No surface read here
	// renders it; it exists so the database really holds private entities
	// related to the public one, which is the state the task names ("若干
	// 私有项目使用/依赖某个公开对象") rather than an abstraction of it.
	for i := range f.others {
		pid := fmt.Sprintf("4123456789abcdefghjkmnpqr%d", i)
		assetID := mustQueryUUID(t, ctx, pool,
			`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
			 VALUES ('dataset', $1, 'Side Channel Shadow Dataset', $2, $3) RETURNING id`,
			"side-shadow-dataset-"+fmt.Sprint(i+1), f.others[i].id, pid)
		mustQueryUUID(t, ctx, pool,
			`INSERT INTO research_asset_versions
			   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, '1.0', '{"metadata":{}}'::jsonb, '{}'::jsonb, 'private', 'sha256:shadow-1.0', $2,
			         ARRAY['project:' || $3::text]) RETURNING id`,
			assetID, f.others[i].owner, f.others[i].id)
	}
	return f
}

// privateIdentities is every string that would disclose one of the private
// projects — its id, its slug, its name — plus the private assets' own
// labels, as the fixture wrote them. A leak check that can match by accident
// is a check nobody can trust in either direction, so each needle is a
// deliberate, distinctive value (the same reasoning explore_test.go's
// identity constants give).
func (f *sideChannelFixture) privateIdentities() []string {
	out := []string{"Side Channel Shadow Dataset", "side-shadow-dataset-"}
	for _, p := range f.others {
		out = append(out, p.id, p.slug, p.name)
	}
	return out
}

// publicSurfaces is every anonymous read this file reads for a count. The
// list is by SURFACE, not by test: the same set is read around each mutation
// of the fixture, and the readings must agree byte for byte.
func (f *sideChannelFixture) publicSurfaces() []string {
	paths := []string{
		// The asset page: docs/42's eleven blocks, used_by among them.
		"/api/v1/assets/" + f.subjectPID,
		// The browse list: the one integer an anonymous read carries
		// (public_versions).
		"/api/v1/assets",
		// The list endpoint.
		"/api/v1/projects",
		// The dependency page of the public project that owns the asset.
		"/api/v1/projects/" + f.openProject + "/dependencies",
		// The two feeds a stranger can subscribe to.
		"/api/v1/feeds/projects/" + f.openProject,
		"/api/v1/feeds/assets/" + f.subjectPID,
	}
	sort.Strings(paths)
	return paths
}

// assertUnchanged fails for every surface whose body moved between two
// snapshots, naming the path. It is the whole instrument: "the private rows
// are not observable" is exactly "adding them changed nothing a caller can
// read".
func assertUnchanged(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	paths := make([]string, 0, len(before))
	for p := range before {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if before[p] == after[p] {
			t.Logf("%s: %s unchanged (%d bytes)", what, p, len(before[p]))
			continue
		}
		t.Errorf("%s changed the answer of %s — a caller who reads it can count or locate %s\n"+
			"before (%d bytes): %s\nafter  (%d bytes): %s",
			what, p, what, len(before[p]), excerptForLog(before[p]),
			len(after[p]), excerptForLog(after[p]))
	}
}

// assertChanged requires at least one surface to have moved, and names which.
// A differential without this half passes when the fixture is broken, when
// the route refuses, when the store reads nothing — every way of measuring
// nothing looks like privacy.
//
// It reports rather than aborts. An abort here would end the test on the
// first broken control and leave the readable checks above and below — the
// ones that name WHAT leaked — unrun, which is the opposite of what a reader
// needs from a red. Nothing is weakened by continuing: the test has already
// failed, and staying alive only adds assertions to the report.
func assertChanged(t *testing.T, what string, before, after map[string]string) {
	t.Helper()
	moved := []string{}
	for p, body := range before {
		if after[p] != body {
			moved = append(moved, p)
		}
	}
	sort.Strings(moved)
	if len(moved) == 0 {
		t.Errorf("the control %q changed NOTHING on any surface — the differential measured nothing", what)
		return
	}
	t.Logf("control %q moved: %s", what, strings.Join(moved, ", "))
}

// assertNoPrivateValue is the READABLE half of the differential: the byte
// comparison says "these bytes moved" and leaves the reader to work out why,
// and this one names the value that appeared. It is run over EVERY reading,
// not only the last, so a leak that a later write happens to cover up is
// still named at the reading where it was visible.
//
// The needles are enumerated, never matched by shape. A "draft-" substring
// reads stronger than it is: the fourth private version is labelled "3.0" and
// only its hash is draft-shaped, so a surface that rendered that label and
// not the hash would slip a shape-matching needle entirely — and the label
// cannot be a bare "3.0" needle either, because every ISO timestamp in these
// bodies ("...T09:12:03.037626Z") contains that substring. Each needle is
// therefore a shape a rendering actually produces. The caller builds the list
// as the fixture writes, so each reading is checked against everything that
// existed when it was taken.
func assertNoPrivateValue(t *testing.T, what string, reading map[string]string, values []string) {
	t.Helper()
	paths := make([]string, 0, len(reading))
	for p := range reading {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		for _, v := range values {
			if strings.Contains(reading[p], v) {
				t.Errorf("%s renders the private value %q (%s): %s", p, v, what, excerptForLog(reading[p]))
			}
		}
	}
}

// ==========================================================================
// Gap one: no private count on any public surface
// ==========================================================================

// TestPrivateUsageAndVersionsAreUnobservableOnEveryPublicSurface is the
// backend half of docs/23 §5's count rule.
//
// The database holds a public asset that three PRIVATE projects use — their
// usage rows declaring visibility_of_usage = 'public', so the using
// project's own visibility is the ONLY thing withholding them — and that
// accumulates private versions of its own. Every anonymous surface that
// could carry a count is read around each insertion, and the two readings
// must be byte-identical.
//
// Both halves of the rule are exercised: the number of things a surface
// renders must not move, and neither must the number it prints. The
// discrimination is the control — a usage by the peer PUBLIC project and a
// version published public DO move the surfaces — so an implementation that
// dropped used_by entirely, or stopped rendering versions, fails here rather
// than passing as "private".
func TestPrivateUsageAndVersionsAreUnobservableOnEveryPublicSurface(t *testing.T) {
	ctx := testCtx(t)
	w := newSideChannelWorld(t, ctx)
	f := seedSideChannelFixture(t, ctx, w)
	paths := f.publicSurfaces()

	publisher := mustQueryUUID(t, ctx, w.pool,
		`SELECT id FROM users WHERE handle = 'side-alice'`)

	// forbidden accumulates every value the fixture writes with
	// visibility='private' — the identity needles up front, then each private
	// version's label and integrity hash as it is written. Every reading below
	// is scanned for all of them.
	forbidden := append([]string{}, f.privateIdentities()...)
	// The shadow assets' own integrity hash, written by the fixture with
	// visibility='private'. No anonymous surface read here should carry it
	// either, and it is the shape a page that rendered "the version it was
	// built from" would leak.
	forbidden = append(forbidden, "sha256:shadow-1.0")

	// --- the baseline: one public version, no usage of any kind ----------
	baseline := w.snapshot(t, paths)
	subjectPage := baseline["/api/v1/assets/"+f.subjectPID]
	for _, needle := range f.privateIdentities() {
		if strings.Contains(subjectPage, needle) {
			t.Fatalf("the baseline already leaks %q: %s", needle, excerptForLog(subjectPage))
		}
	}
	// A control that the baseline is not an error page: the public asset's
	// page names it. Without this, every assertion below would pass on a 404.
	if !strings.Contains(subjectPage, "Side Channel Open Dataset") {
		t.Fatalf("the public asset's page does not render its own title, so nothing below measures "+
			"anything: %s", excerptForLog(subjectPage))
	}
	assertUsedByLen(t, subjectPage, 0)
	assertVersionsLen(t, subjectPage, 1)
	assertBrowsePublicVersions(t, baseline["/api/v1/assets"], f.subjectPID, 1)
	assertNoPrivateValue(t, "the baseline", baseline, forbidden)

	// --- the usages: three PRIVATE projects use the public version -------
	//
	// visibility_of_usage = 'public' on every one of them, which is the
	// sharper fixture: the row itself declares the usage public, and the
	// using project's identity is what refuses it.
	for i := range f.others {
		if _, err := w.pool.Exec(ctx,
			`INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
			 VALUES ($1, $2, 'depends_on', 'public')`,
			f.others[i].id, f.subjectPublicVersionID); err != nil {
			t.Fatalf("seed private usage %d: %v", i, err)
		}
	}
	afterUsage := w.snapshot(t, paths)
	assertUnchanged(t, "three private projects' usages", baseline, afterUsage)
	assertUsedByLen(t, afterUsage["/api/v1/assets/"+f.subjectPID], 0)
	assertNoPrivateValue(t, "three private usages", afterUsage, forbidden)

	// --- the versions: three PRIVATE versions of the public asset --------
	//
	// This is the axis the one integer on an anonymous read could carry: the
	// browse list prints public_versions, and the difference between it and
	// the total version count is exactly the private count docs/23 §5
	// forbids.
	for i := 0; i < 3; i++ {
		// The label and the hash are built here and pushed onto the needle
		// list in the same statement that writes them, so the readable check
		// cannot drift from the fixture it is reading.
		label := fmt.Sprintf("draft-%d", i+1)
		hash := "sha256:" + label
		mustQueryUUID(t, ctx, w.pool,
			`INSERT INTO research_asset_versions
			   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
			 VALUES ($1, $2, '{"metadata":{}}'::jsonb, '{}'::jsonb, 'private', $3, $4,
			         ARRAY['project:' || $5::text]) RETURNING id`,
			f.subjectAssetID, label, hash, publisher, f.openProject)
		forbidden = append(forbidden, label, hash)
	}
	afterVersions := w.snapshot(t, paths)
	assertUnchanged(t, "three private versions", afterUsage, afterVersions)
	assertNoPrivateValue(t, "three private versions", afterVersions, forbidden)
	assertVersionsLen(t, afterVersions["/api/v1/assets/"+f.subjectPID], 1)
	assertBrowsePublicVersions(t, afterVersions["/api/v1/assets"], f.subjectPID, 1)

	// --- the control: a PUBLIC usage and a PUBLIC version MUST move it ---
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
		 VALUES ($1, $2, 'depends_on', 'public')`, f.publicPeer, f.subjectPublicVersionID); err != nil {
		t.Fatalf("seed the public control usage: %v", err)
	}
	withPublicUsage := w.snapshot(t, paths)
	assertChanged(t, "a usage by a PUBLIC project", afterVersions, withPublicUsage)
	assertUsedByLen(t, withPublicUsage["/api/v1/assets/"+f.subjectPID], 1)
	assertNoPrivateValue(t, "a public usage rendered beside the private ones", withPublicUsage, forbidden)

	if _, err := w.pool.Exec(ctx,
		`INSERT INTO research_asset_versions
		   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, '2.0', '{"metadata":{}}'::jsonb, '{}'::jsonb, 'public', 'sha256:pub-2.0', $2,
		         ARRAY['project:' || $3::text])`, f.subjectAssetID, publisher, f.openProject); err != nil {
		t.Fatalf("seed the public control version: %v", err)
	}
	withPublicVersion := w.snapshot(t, paths)
	assertChanged(t, "a version published PUBLIC", withPublicUsage, withPublicVersion)
	assertBrowsePublicVersions(t, withPublicVersion["/api/v1/assets"], f.subjectPID, 2)
	assertVersionsLen(t, withPublicVersion["/api/v1/assets/"+f.subjectPID], 2)
	assertNoPrivateValue(t, "a public version rendered beside the private ones", withPublicVersion, forbidden)

	// --- one more private row must STILL change nothing ------------------
	//
	// The control above proves the instrument is live; this proves it is
	// still live afterwards — a suite that could only detect the first
	// insertion would miss a count that started at two.
	// The fourth private version is the one a shape-matching needle would miss:
	// its label is "3.0", a bare version string, and only its hash carries the
	// "draft-" shape the earlier three share. Its LABEL cannot be a bare "3.0"
	// needle either — every ISO timestamp in these bodies
	// ("...T09:12:03.037626Z") contains that substring, so a bare needle would
	// fire on a page that leaked nothing. The needles are the two shapes a
	// rendering of it actually produces: the quoted JSON string and the asset
	// URL.
	const ctlLabel, ctlHash = "3.0", "sha256:draft-ctl"
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO research_asset_versions
		   (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, '{"metadata":{}}'::jsonb, '{}'::jsonb, 'private', $3, $4,
		         ARRAY['project:' || $5::text])`,
		f.subjectAssetID, ctlLabel, ctlHash, publisher, f.openProject); err != nil {
		t.Fatalf("seed a further private version: %v", err)
	}
	forbidden = append(forbidden,
		ctlHash,
		`"`+ctlLabel+`"`,
		"/assets/"+f.subjectPID+"/"+ctlLabel,
	)
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
		 VALUES ($1, $2, 'references', 'public')`, f.others[0].id, f.subjectPublicVersionID); err != nil {
		t.Fatalf("seed a further private usage: %v", err)
	}
	afterMorePrivate := w.snapshot(t, paths)
	assertUnchanged(t, "a fourth private version and a second private usage",
		withPublicVersion, afterMorePrivate)
	// The last reading is the one with the MOST private state in the database,
	// which is the reading a leak has the best chance of appearing in.
	assertNoPrivateValue(t, "a fourth private version and a second private usage",
		afterMorePrivate, forbidden)
	// The count the database really holds, asserted so that "nothing was
	// leaked" cannot be satisfied by "nothing was there".
	var privateVersions, privateUsages int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1 AND visibility = 'private'`,
		f.subjectAssetID).Scan(&privateVersions); err != nil {
		t.Fatalf("probe the private version count: %v", err)
	}
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM asset_dependencies ad
		 JOIN projects p ON p.id = ad.project_id
		 WHERE ad.asset_version_id = $1 AND p.visibility = 'private'`,
		f.subjectPublicVersionID).Scan(&privateUsages); err != nil {
		t.Fatalf("probe the private usage count: %v", err)
	}
	if privateVersions != 4 || privateUsages != 4 {
		t.Fatalf("the fixture holds %d private versions and %d private usages, want 4 and 4: an "+
			"assertion that nothing leaked is worth nothing if nothing was there",
			privateVersions, privateUsages)
	}
}

// assertBrowsePublicVersions requires the browse row for pid to report n
// public versions.
func assertBrowsePublicVersions(t *testing.T, raw, pid string, want int) {
	t.Helper()
	var payload assetBrowsePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the browse list is not the document a client reads: %v: %s", err, excerptForLog(raw))
	}
	for _, row := range payload.Assets {
		if row.PID == pid {
			if row.PublicVersions != want {
				t.Errorf("browse public_versions for %s = %d, want %d — that number is the difference "+
					"docs/23 §5 forbids an anonymous reader to compute: %s",
					pid, row.PublicVersions, want, excerptForLog(raw))
			}
			return
		}
	}
	t.Errorf("the browse list has no row for %s: %s", pid, excerptForLog(raw))
}

// assertUsedByLen requires the page's used_by block to have exactly n
// entries. The LENGTH is the count: a block that rendered a placeholder for
// a withheld usage, or the usage itself, moves it.
func assertUsedByLen(t *testing.T, raw string, want int) {
	t.Helper()
	var payload assetPagePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the asset page is not the document a client reads: %v: %s", err, excerptForLog(raw))
	}
	if got := len(payload.UsedBy); got != want {
		t.Errorf("used_by has %d entries, want %d — the length is what an anonymous reader counts: %s",
			got, want, excerptForLog(raw))
	}
}

// assertVersionsLen requires the page's versions block to have exactly n
// entries, for the same reason.
func assertVersionsLen(t *testing.T, raw string, want int) {
	t.Helper()
	var payload assetPagePayload
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("the asset page is not the document a client reads: %v: %s", err, excerptForLog(raw))
	}
	if got := len(payload.Versions); got != want {
		t.Errorf("versions has %d entries, want %d — the length is what an anonymous reader counts: %s",
			got, want, excerptForLog(raw))
	}
}

// ==========================================================================
// Gap two: unreadable and nonexistent are the same answer, item by item
// ==========================================================================

// unknownProjectID and unknownAssetPID are well-formed and absent: the uuid
// is the nil-shaped one other suites in this package use, and the pid is 26
// Crockford characters (research_assets_pid_format), so both name something
// that could exist and does not. A MALFORMED id would be refused by a
// different branch — that is a different question, and it is not the one
// docs/54 scenario #1 asks.
const (
	unknownProjectID = "00000000-0000-4000-8000-000000000000"
	unknownAssetPID  = "0123456789abcdefghjkmnpqrs"
)

// indistinguishable compares every part of two answers that must not differ:
// the status, every response header except Date, and the body byte for byte
// — which is also its length. The error code and sentence are checked by the
// caller before it gets here, because they are decoded from these same bytes
// and a caller that reads them separately gets a better failure message.
//
// Date is excluded, and ONLY Date, because it is the response's own clock
// rather than a statement about the target: two requests a millisecond apart
// cannot carry the same one. It is excluded by NAME rather than by a
// wildcard, so a header added later is compared by default — the safe
// direction, since a header set on one path and not the other is exactly the
// oracle this test exists to catch.
func indistinguishable(t *testing.T, what, leftLabel, leftRaw string, leftHeader http.Header,
	leftStatus int, rightLabel, rightRaw string, rightHeader http.Header, rightStatus int) {
	t.Helper()
	if leftStatus != rightStatus {
		t.Errorf("%s: %s answered %d, %s answered %d", what, leftLabel, leftStatus, rightLabel, rightStatus)
	}
	if leftRaw != rightRaw {
		t.Errorf("%s: the bodies differ — %s (%d bytes) vs %s (%d bytes)\n%s\n%s",
			what, leftLabel, len(leftRaw), rightLabel, len(rightRaw),
			excerptForLog(leftRaw), excerptForLog(rightRaw))
	}

	names := map[string]bool{}
	for k := range leftHeader {
		names[k] = true
	}
	for k := range rightHeader {
		names[k] = true
	}
	ordered := make([]string, 0, len(names))
	for k := range names {
		if k == "Date" {
			continue
		}
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, k := range ordered {
		l, r := leftHeader.Get(k), rightHeader.Get(k)
		if l != r {
			t.Errorf("%s: header %s = %q for %s and %q for %s", what, k, l, leftLabel, r, rightLabel)
		}
	}
	t.Logf("%s: %s and %s agree on status %d, %d body bytes, and headers %v",
		what, leftLabel, rightLabel, leftStatus, len(leftRaw), ordered)
}

// TestProjectAndAssetRefusalsAreIndistinguishable answers gap two over the
// two HTTP surfaces a stranger reaches without a session: the project
// surface and the asset surface.
//
// "Unreadable" is a private project the caller may not read — as an
// ANONYMOUS caller, which the guard lets through on reads by design (the
// middleware continues reads and refuses writes, cmd/api/authhttp). "Does
// not exist" is an id of the same shape that names no row. docs/45 §24
// forbids the first to be distinguishable from the second; the existing
// suites pin the code and the body for both, so what this test adds is the
// REST of the answer — every header — and the timing record beside it.
func TestProjectAndAssetRefusalsAreIndistinguishable(t *testing.T) {
	ctx := testCtx(t)
	w := newSideChannelWorld(t, ctx)
	f := seedSideChannelFixture(t, ctx, w)

	// The private project really exists and really is private in the
	// database — probed, not assumed, so that "the two answers match" cannot
	// be satisfied by two 404s for two nonexistent ids.
	privateProject := f.others[0].id
	var dbVisibility string
	if err := w.pool.QueryRow(ctx,
		`SELECT visibility FROM projects WHERE id = $1::uuid`, privateProject).Scan(&dbVisibility); err != nil {
		t.Fatalf("probe the private project's visibility: %v", err)
	}
	if dbVisibility != "private" {
		t.Fatalf("fixture: the project's stored visibility is %q, want private", dbVisibility)
	}

	// The asset surface's unreadable case: an asset that really HAS a
	// version, owned by that same private project — so the page has
	// something to render and is withheld for the caller's sake, not for
	// want of data. Probed for the same reason.
	privateAssetPID := "4123456789abcdefghjkmnpqr0"
	var privateAssetVersions int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_asset_versions rav
		 JOIN research_assets ra ON ra.id = rav.asset_id WHERE ra.pid = $1`,
		privateAssetPID).Scan(&privateAssetVersions); err != nil {
		t.Fatalf("probe the private asset's versions: %v", err)
	}
	if privateAssetVersions == 0 {
		t.Fatalf("fixture: %s has no versions, so its page could not be read either way", privateAssetPID)
	}

	for _, p := range []struct {
		surface    string
		unreadable string
		absent     string
	}{
		{"the project surface", "/api/v1/projects/" + privateProject, "/api/v1/projects/" + unknownProjectID},
		{"the asset surface", "/api/v1/assets/" + privateAssetPID, "/api/v1/assets/" + unknownAssetPID},
	} {
		leftStatus, leftHeader, leftRaw := w.mustGETRaw(t, p.unreadable)
		rightStatus, rightHeader, rightRaw := w.mustGETRaw(t, p.absent)

		// The refused side must BE a refusal: a 200 on one side would make
		// the comparison below a comparison of two successes.
		if leftStatus != http.StatusNotFound {
			t.Errorf("%s: the unreadable target answered %d, want 404: %s",
				p.surface, leftStatus, excerptForLog(leftRaw))
		}
		var leftEnv, rightEnv errorEnvelope
		if err := json.Unmarshal([]byte(leftRaw), &leftEnv); err != nil {
			t.Fatalf("%s: the unreadable answer is not the error envelope: %v: %s",
				p.surface, err, excerptForLog(leftRaw))
		}
		if err := json.Unmarshal([]byte(rightRaw), &rightEnv); err != nil {
			t.Fatalf("%s: the absent answer is not the error envelope: %v: %s",
				p.surface, err, excerptForLog(rightRaw))
		}
		if leftEnv.Code != rightEnv.Code {
			t.Errorf("%s: code = %q for the unreadable target and %q for the absent one",
				p.surface, leftEnv.Code, rightEnv.Code)
		}
		if leftEnv.Message != rightEnv.Message {
			t.Errorf("%s: message = %q for the unreadable target and %q for the absent one",
				p.surface, leftEnv.Message, rightEnv.Message)
		}
		if leftEnv.Code == "" || leftEnv.Message == "" {
			t.Errorf("%s: the refusal carries no code/message at all: %s", p.surface, excerptForLog(leftRaw))
		}

		indistinguishable(t, p.surface,
			"exists but unreadable", leftRaw, leftHeader, leftStatus,
			"does not exist", rightRaw, rightHeader, rightStatus)

		// The identities must not be in the refusal either: "the same
		// answer" is satisfiable by leaking from both sides at once.
		for _, forbidden := range f.privateIdentities() {
			if strings.Contains(leftRaw, forbidden) || strings.Contains(rightRaw, forbidden) {
				t.Errorf("%s: the refusal leaks %q", p.surface, forbidden)
			}
		}
	}

	// --- the timing record, printed and never asserted on -----------------
	//
	// Five samples of each class, because one sample of a millisecond-scale
	// quantity is a number without a distribution. min and max are printed
	// so a reader can see the spread; nothing here is compared to anything.
	for _, p := range []struct{ surface, unreadable, absent string }{
		{"project", "/api/v1/projects/" + privateProject, "/api/v1/projects/" + unknownProjectID},
		{"asset", "/api/v1/assets/" + privateAssetPID, "/api/v1/assets/" + unknownAssetPID},
	} {
		unreadable := make([]time.Duration, 0, 5)
		absent := make([]time.Duration, 0, 5)
		for i := 0; i < 5; i++ {
			_, _, _, d := w.anonymousGET(t, p.unreadable)
			unreadable = append(unreadable, d)
			_, _, _, d = w.anonymousGET(t, p.absent)
			absent = append(absent, d)
		}
		t.Logf("TIMING RECORD (raw, unasserted) %s surface: unreadable=%v absent=%v "+
			"unreadable_min=%v unreadable_max=%v absent_min=%v absent_max=%v | %s",
			p.surface, unreadable, absent,
			minDuration(unreadable), maxDuration(unreadable),
			minDuration(absent), maxDuration(absent), timingNote)
	}
}

// minDuration and maxDuration summarise a sample for the timing RECORD.
// They exist so the printed line is readable; no assertion reads them, and
// no threshold is compared against them (timingNote).
func minDuration(ds []time.Duration) time.Duration {
	out := ds[0]
	for _, d := range ds {
		if d < out {
			out = d
		}
	}
	return out
}

func maxDuration(ds []time.Duration) time.Duration {
	out := ds[0]
	for _, d := range ds {
		if d > out {
			out = d
		}
	}
	return out
}

// ==========================================================================
// Gap two, retrieval surface
// ==========================================================================

// TestRetrievalCannotTellAnUnreadableDocumentFromNonexistent is the third of
// the three surfaces the acceptance criterion names (项目面、资产面、检索面).
//
// The retrieval layer does have an HTTP route — cmd/api/searchhttp's
// POST /api/v1/search runs the whole pipeline, and its third step is
// retrieval.Retriever (cmd/api/searchhttp/doc.go:20-27, service.go:163,
// wiring.go:31) — but that route is not on the surface this item is about.
// It is a state-changing method: cmd/api/searchhttp/wiring.go:111 registers
// three patterns, all POST, and an anonymous POST is challenged by the auth
// guard with AUTH_UNAUTHENTICATED (wiring.go:117-124) before any handler
// runs, which the handler states again as a structured 401 (handlers.go:200,
// SEARCH_UNAUTHENTICATED). An answered search is recorded as well —
// service.go:177 writes the row, and service.go:184 calls that writer "the
// only writer of it" — so reaching the answer is a write, not a read. An
// anonymous reader therefore cannot reach a retrieval answer over HTTP at
// all, so this item is made at the layer itself, over the real projection,
// the real disclosure predicate and the real SQL.
//
// (Until T1112 this paragraph said "the retrieval layer has no HTTP route
// today". That was false, and the correction is the stronger claim: "no
// route" invites somebody to add one and quietly move the surface, while
// "the route is a POST that refuses anonymous callers and records the
// search" says why that route cannot serve this item however it is written.)
//
// # Why the instrument is a differential and not "two queries that agree"
//
// The obvious form of this test — ask a question that matches only another
// project's document, ask a question that matches nothing, compare — does
// not work here, and the way it fails is worth writing down. The retriever's
// vector signal is a NEAREST-NEIGHBOUR signal: it embeds the query and
// returns the k documents closest to it, so a string of gibberish still
// returns k documents, and two different query strings legitimately return
// different ones. Those differences are all among documents the actor MAY
// read, so none of them discloses anything — but they do make "the two
// answers are byte-identical" false for a reason that has nothing to do with
// privacy. (The first draft of this test asserted exactly that and went red
// on the two scores, which is how the mistake was found.)
//
// What is actually claimed is narrower and stronger: THE ANSWER DOES NOT
// DEPEND ON ROWS THE ACTOR MAY NOT READ. So the corpus is changed under a
// fixed question — a document is added to a project the actor is not a
// member of, and the answer must not move by a byte. The control adds a
// document to the actor's OWN project and requires the answer to move, so
// that "nothing changed" cannot be a corpus the reader never touched.
//
// The two readable halves of the same claim are asserted too: the full-text
// signal reports the same thing (zero hits, ran) for a question that matches
// an unreadable document as for one that matches nothing, and the
// unreadable document's own identifiers appear nowhere in the bytes.
func TestRetrievalCannotTellAnUnreadableDocumentFromNonexistent(t *testing.T) {
	ctx := testCtx(t)
	f := newRetrievalFixture(t, ctx)
	corpus := seedRetrievalCorpus(t, f)
	f.embedTheCorpus(t)

	store, err := retrieval.NewSQLStore(f.pool)
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}
	// The same embedder the batch job used: a query vector from another
	// model is not a weaker match, it is not a match at all.
	r, err := retrieval.NewRetriever(store, mustEmbedder(t, embedding.LocalModel),
		retrieval.WithLogger(silentLogger()))
	if err != nil {
		t.Fatalf("NewRetriever: %v", err)
	}
	embedder := mustEmbedder(t, embedding.LocalModel)

	// Alice's scope: her own project only. The public project is
	// deliberately absent from it (search.Scope's contract), so bob's
	// private document and the public publication are both outside it for
	// different reasons — and only the first is what this test is about.
	aliceScope := f.aliceScope(t)

	// The question is bob's document's own subject, so bob's scope recalls
	// it and alice's must not. run() is one retrieval through the real
	// pipeline, with the answer encoded the way a consumer of it would read.
	const question = "Mg-MOF-74 secret claim"
	run := func(scope search.Scope, q string) (string, retrieval.Result) {
		t.Helper()
		res, err := r.Retrieve(ctx, scope, retrieval.Request{Query: q})
		if err != nil {
			t.Fatalf("Retrieve(%q): %v", q, err)
		}
		return encodeRetrievalResult(t, res), res
	}

	// --- the control that the question is not inert ----------------------
	//
	// It runs FIRST: without it, every "alice saw nothing" below is equally
	// explained by a question no document in the corpus matches.
	bobScope, err := search.ResolveScope(ctx, persistence.NewProjectStore(f.pool), f.bobID)
	if err != nil {
		t.Fatalf("ResolveScope(bob): %v", err)
	}
	_, withBob := run(bobScope, question)
	if !hasRef(withBob, search.EntityRef(search.EntityKnowledge, corpus.secretPID)) {
		t.Fatalf("the control recalled nothing: the question %q does not match the document it was "+
			"taken from, so the assertions below measure an inert corpus. candidates = %v",
			question, retrievalRefs(withBob))
	}

	// --- the full-text signal: unreadable reads exactly like nonexistent --
	aliceRaw, aliceBefore := run(aliceScope, question)
	_, aliceAbsent := run(aliceScope, "qqzzxx nothing in this corpus matches qqzzxx")
	left, ok := signalReport(aliceBefore, retrieval.SignalFullText)
	if !ok {
		t.Fatalf("the retrieval ran no full-text signal at all: %+v", aliceBefore.Signals)
	}
	right, ok := signalReport(aliceAbsent, retrieval.SignalFullText)
	if !ok {
		t.Fatalf("the retrieval ran no full-text signal at all: %+v", aliceAbsent.Signals)
	}
	if left != right {
		t.Errorf("the full-text signal tells a question that matches an unreadable document from one "+
			"that matches nothing: %+v vs %+v", left, right)
	}
	if !left.Ran || left.Hits != 0 {
		t.Errorf("the full-text signal for a question whose only match is another project's "+
			"document reports %+v, want ran with 0 hits — the read path is what withholds it, and a "+
			"signal that ran and found rows it may not render is a signal whose bound is in the "+
			"wrong place", left)
	}
	for _, forbidden := range []string{corpus.secretPID, corpus.secretVersion, corpus.bobMaterialVersion} {
		if strings.Contains(aliceRaw, forbidden) {
			t.Errorf("the retrieval answer for a question matching another project's document "+
				"contains %q: %s", forbidden, excerptForLog(aliceRaw))
		}
	}

	// --- THE DIFFERENTIAL: an unreadable document must not move it -------
	//
	// A second document is added to bob's private project — published and
	// projected through the production pipeline, not written into
	// search_documents — and the SAME question is asked again of the SAME
	// actor. The answer must be byte-identical.
	extraBobPID := "6123456789abcdefghjkmnpqrs"
	bobExtra := f.seedObject(t, f.bobProject, f.bobID, "claim",
		"Mg-MOF-74 secret claim, second", "bob-extra-branch")
	f.publish(t, bobExtra, extraBobPID, f.bobID)
	f.project(t, f.bobProject, "private", extraBobPID)
	if _, err := mustEmbeddingWorker(t, f.pool, embedder).RunOnce(ctx); err != nil {
		t.Fatalf("embed the extra unreadable document: %v", err)
	}
	// The row really landed and really is outside alice's scope: a
	// differential over a document that was never projected measures
	// nothing.
	var projectedVisibility, projectedProject string
	if err := f.pool.QueryRow(ctx,
		`SELECT visibility, COALESCE(project_id::text, '') FROM search_documents WHERE entity_ref = $1`,
		search.EntityRef(search.EntityKnowledge, extraBobPID)).Scan(&projectedVisibility, &projectedProject); err != nil {
		t.Fatalf("the extra document did not reach the projection: %v", err)
	}
	if projectedVisibility != "private" || projectedProject != f.bobProject {
		t.Fatalf("the extra document projected as %q in %q, want private in bob's project",
			projectedVisibility, projectedProject)
	}
	aliceAfter, _ := run(aliceScope, question)
	if aliceAfter != aliceRaw {
		t.Errorf("adding a document to a project the actor is not a member of CHANGED the "+
			"retrieval answer — the answer is a function of rows the actor may not read "+
			"(docs/54 scenario #1):\nbefore (%d bytes): %s\nafter  (%d bytes): %s",
			len(aliceRaw), excerptForLog(aliceRaw), len(aliceAfter), excerptForLog(aliceAfter))
	}
	t.Logf("adding %q to bob's private project left alice's answer identical (%d bytes, %d "+
		"candidates)", extraBobPID, len(aliceAfter), len(aliceBefore.Candidates))

	// --- the control: a document in the actor's OWN project MUST move it --
	//
	// Without this, the byte-identity above is also what a retriever that
	// returned a constant would produce.
	extraAlicePID := "7123456789abcdefghjkmnpqrs"
	aliceExtra := f.seedObject(t, f.aliceProject, f.aliceID, "claim",
		"Mg-MOF-74 secret claim, alice's own", "alice-extra-branch")
	f.publish(t, aliceExtra, extraAlicePID, f.aliceID)
	f.project(t, f.aliceProject, "private", extraAlicePID)
	if _, err := mustEmbeddingWorker(t, f.pool, embedder).RunOnce(ctx); err != nil {
		t.Fatalf("embed the extra readable document: %v", err)
	}
	aliceControl, _ := run(aliceScope, question)
	if aliceControl == aliceAfter {
		t.Fatalf("a document added to the actor's OWN project did not move the answer, so the "+
			"byte-identity above measured nothing: %s", excerptForLog(aliceControl))
	}
	t.Logf("the control: adding %q to alice's own project moved the answer to %d bytes",
		extraAlicePID, len(aliceControl))
}

// signalReport finds one signal's report in a result.
func signalReport(res retrieval.Result, name string) (retrieval.SignalReport, bool) {
	for _, s := range res.Signals {
		if s.Signal == name {
			return s, true
		}
	}
	return retrieval.SignalReport{}, false
}

// encodeRetrievalResult renders a result the way a consumer of it would, for
// the byte-for-byte comparison above.
//
// Result carries no wall clock, no request id and no map iteration order —
// Query, Candidates and Signals are its whole content — so the encoding is a
// function of the retrieval alone, and two retrievals that read the same
// rows encode to the same bytes. The assertion of that is the comparison
// itself: if a later change adds a per-call field, this test fails with the
// two documents side by side, which is the right place to decide what it
// means. `query` is zeroed because it is the caller's own string echoed
// back, which is not a statement about the corpus.
func encodeRetrievalResult(t *testing.T, res retrieval.Result) string {
	t.Helper()
	res.Query = ""
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal the retrieval result: %v", err)
	}
	return string(raw)
}

// ==========================================================================
// The Explore index's post-LIMIT oracle
// ==========================================================================

// TestPrivatePublicationsDoNotCrowdTheExploreIndex is gap one on the ONE
// surface where it was real.
//
// The knowledge section reads the newest explore.SectionLimit publications
// and renders the ones whose audience rule admits them. Until T1107 the
// LIMIT was taken over the whole corpus and the rule was applied afterwards,
// in Go — so a reader who can enumerate the network's public publications
// through other public reads saw the section come back SHORT, and the
// shortfall is a count of publications the reader may not see. docs/23 §5
// forbids the count; a count expressed as an absence is the same disclosure.
//
// The instrument is the differential the rest of this file uses: twenty
// public publications are buried under twenty strictly-newer private ones,
// and the section's bytes must not move. The control publishes twenty MORE
// public ones, newest of all, and requires the section to move.
func TestPrivatePublicationsDoNotCrowdTheExploreIndex(t *testing.T) {
	ctx := testCtx(t)
	w := newExploreWorld(t, ctx)
	pool := w.pool

	// --- accounts and projects -------------------------------------------
	w.aliceID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO users (handle, display_name) VALUES ('crowd-alice', 'Crowd Alice') RETURNING id`)
	w.openProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('crowd-open-lab', 'Crowd Open Lab', 'T1107 fixture', 'public', $1) RETURNING id`,
		w.aliceID)
	w.hiddenProjectID = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('crowd-hidden-lab', 'Crowd Hidden Lab', 'T1107 fixture', 'private', $1) RETURNING id`,
		w.aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role)
		 VALUES ($1, $2, 'owner'), ($3, $2, 'owner')`,
		w.openProjectID, w.aliceID, w.hiddenProjectID); err != nil {
		t.Fatalf("seed memberships: %v", err)
	}

	// The instants are explicit and strictly ordered, so "newer" is a fact
	// about the rows rather than about how long the fixture took to write
	// (explore_test.go's exploreDay gives the same reasoning).
	base := exploreDay(1)

	// --- the twenty public publications ----------------------------------
	//
	// Exactly SectionLimit of them: a full section, which is what makes the
	// shortfall below measurable at all.
	day := func(hours int) time.Time { return base.Add(time.Duration(hours) * time.Hour) }
	publicTitles := make([]string, 0, explore.SectionLimit)
	for i := 0; i < explore.SectionLimit; i++ {
		title := fmt.Sprintf("Crowd Public Question %02d", i+1)
		publicTitles = append(publicTitles, title)
		seedExploreKnowledge(t, ctx, pool, w.openProjectID, w.aliceID, 100+i, title, "active", day(i))
	}
	// Newest first: the section's own order, which the assertion below reads
	// back off the bytes.
	newestFirst(publicTitles)

	// The section as it travelled BEFORE the burial: this read is the
	// differential's left-hand side, and taking it here rather than below is
	// load-bearing — a "before" captured after the private rows were written
	// compares two identical reads and measures nothing. (The mutation check
	// is what caught that: with the WHERE clause removed the section came
	// back empty and this comparison still agreed with itself.)
	rawFilled := w.mustIndexRaw(t)
	_, filled := w.mustIndex(t)
	assertKnowledgeSection(t, filled, publicTitles, "the twenty public publications")

	// --- the twenty private publications, ALL of them newer ---------------
	//
	// One hundred hours past the newest public one, so that the newest
	// SectionLimit rows in the table are exactly these — the burial the
	// whole test is about.
	privateTitles := make([]string, 0, explore.SectionLimit)
	for i := 0; i < explore.SectionLimit; i++ {
		title := fmt.Sprintf("Crowd Hidden Question %02d", i+1)
		privateTitles = append(privateTitles, title)
		seedExploreKnowledge(t, ctx, pool, w.hiddenProjectID, w.aliceID, 200+i, title, "active",
			day(100+i))
	}
	assertBuried(t, ctx, pool, w.openProjectID)

	rawAfter, index := w.mustIndex(t)
	if before, after := knowledgeSectionBytes(t, rawFilled), knowledgeSectionBytes(t, rawAfter); before != after {
		t.Errorf("twenty private publications that are newer than every public one CHANGED the "+
			"knowledge section — the section's shortfall is an existence oracle for rows the reader "+
			"may not see (docs/23 §5, docs/54 scenario #1):\nbefore: %s\nafter:  %s",
			excerptForLog(before), excerptForLog(after))
	}
	assertKnowledgeSection(t, index, publicTitles, "the twenty public publications, after the burial")
	for _, forbidden := range append(privateTitles,
		"Crowd Hidden Lab", w.hiddenProjectID, "crowd-hidden-lab") {
		if strings.Contains(rawAfter, forbidden) {
			t.Errorf("the index leaks %q: %s", forbidden, excerptForLog(rawAfter))
		}
	}

	// --- the control: twenty NEWER public ones MUST move the section ------
	//
	// Without this, everything above passes on a section that renders
	// nothing, or on a fixture whose twenty public rows were never there.
	controlTitles := make([]string, 0, explore.SectionLimit)
	for i := 0; i < explore.SectionLimit; i++ {
		title := fmt.Sprintf("Crowd Control Question %02d", i+1)
		controlTitles = append(controlTitles, title)
		seedExploreKnowledge(t, ctx, pool, w.openProjectID, w.aliceID, 300+i, title, "active",
			day(200+i))
	}
	newestFirst(controlTitles)

	rawControl, controlIndex := w.mustIndex(t)
	if knowledgeSectionBytes(t, rawControl) == knowledgeSectionBytes(t, rawAfter) {
		t.Fatalf("the control published twenty PUBLIC publications newer than everything and the "+
			"knowledge section did not move — the burial above measured nothing:\nsection: %s",
			excerptForLog(knowledgeSectionBytes(t, rawControl)))
	}
	assertKnowledgeSection(t, controlIndex, controlTitles, "the control's twenty newest public publications")

	// --- the timing record for the whole index read ----------------------
	//
	// RECORDED, never asserted on: see timingNote.
	start := time.Now()
	_ = w.mustIndexRaw(t)
	t.Logf("TIMING RECORD (raw, unasserted) the Explore index read: %v | %s",
		time.Since(start), timingNote)
}

// newestFirst reverses a list of titles so it reads in the section's own
// order. The fixture's titles are padded and its instants ascend with the
// index, so descending order is the reverse of the write order — which is
// what makes the section's ORDER a thing this test measures rather than
// assumes (the same technique explore_test.go uses when it writes its rows
// in the opposite order of every freshness key).
func newestFirst(titles []string) {
	sort.Sort(sort.Reverse(sort.StringSlice(titles)))
}

// assertBuried proves the fixture really did bury the public publications:
// the database's own newest SectionLimit publications, by the section's own
// ordering, must all belong to the private project. Without this the test
// above passes on a database where the private rows were written with older
// instants and never crowded anything.
func assertBuried(t *testing.T, ctx context.Context, pool *pgxpool.Pool, openProjectID string) {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT so.project_id::text
		 FROM knowledge_publications kp
		 JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
		 JOIN scientific_objects so ON so.id = sov.object_id
		 ORDER BY kp.published_at DESC, kp.id LIMIT $1`, explore.SectionLimit)
	if err != nil {
		t.Fatalf("probe the burial: %v", err)
	}
	defer rows.Close()
	var fromPublic int
	var total int
	for rows.Next() {
		var projectID string
		if err := rows.Scan(&projectID); err != nil {
			t.Fatalf("scan the burial probe: %v", err)
		}
		total++
		if projectID == openProjectID {
			fromPublic++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the burial probe: %v", err)
	}
	if total != explore.SectionLimit || fromPublic != 0 {
		t.Fatalf("the burial is not real: the newest %d publications by the section's own ordering "+
			"hold %d from the public project, want 0 of %d — a corpus that buries nothing measures "+
			"nothing", total, fromPublic, total)
	}
}

// assertKnowledgeSection requires the index's knowledge section to hold
// exactly the given titles, in order — the readable statement of what the
// byte comparison beside it decides.
func assertKnowledgeSection(t *testing.T, index exploreWireIndex, want []string, what string) {
	t.Helper()
	got := make([]string, 0, len(index.Knowledge.Items))
	for _, item := range index.Knowledge.Items {
		got = append(got, item.Title)
	}
	if len(got) != len(want) {
		t.Fatalf("%s: the knowledge section holds %d rows, want %d: %v", what, len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: the knowledge section's row %d is %q, want %q (full section: %v)",
				what, i, got[i], want[i], got)
		}
	}
}

// knowledgeSectionBytes returns the raw bytes of the index document's
// `knowledge` value — the section as it travelled, not a re-encoding of it.
// json.RawMessage keeps the original text of the value it captured, so two
// sections compare equal only when they were byte-identical on the wire.
func knowledgeSectionBytes(t *testing.T, raw string) string {
	t.Helper()
	var only struct {
		Knowledge json.RawMessage `json:"knowledge"`
	}
	if err := json.Unmarshal([]byte(raw), &only); err != nil {
		t.Fatalf("the index is not the JSON document a client reads: %v: %s", err, excerptForLog(raw))
	}
	return string(only.Knowledge)
}

// mustIndexRaw reads the surface the way a client does and returns the RAW
// body, for the byte comparisons. mustIndex (explore_test.go) does the same
// read and additionally decodes; this one is here so a caller that wants
// only the bytes does not decode a document it is about to compare as text.
func (w *exploreWorld) mustIndexRaw(t *testing.T) string {
	t.Helper()
	resp, err := http.Get(w.ts.URL + "/api/v1/explore")
	if err != nil {
		t.Fatalf("GET /api/v1/explore: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/explore = %d: %s", resp.StatusCode, excerptForLog(raw))
	}
	return raw
}
