// Task T1004 — the public RSS/Atom feeds over real PostgreSQL.
//
// The unit suites pin the RULES (internal/application/feeds: the one
// disclosure rule, the stable ids, the two document shapes) and the
// transport's answers (cmd/api/feedshttp: negotiation, the one 404, the
// validator). This file pins what only a real database and the composed
// route can settle:
//
//  1. The two acceptance criteria, over the whole surface: an ANONYMOUS
//     caller (no cookie, no session) reads a public project's, asset's and
//     knowledge object's feed — and a private one has no feed AT ALL, not
//     even for its own owner.
//
//  2. The disclosure rules hold END TO END. The fixture is raw on purpose:
//     the public project holds a PRIVATE version of an asset that is
//     otherwise public, and the private project holds a published asset and
//     a published knowledge object. The anonymous answer must name none of
//     the private things, and the negative assertions are made on the RAW
//     RESPONSE BODY, so a field the transport added on its own fails here.
//     (The shipped queries also exclude private rows, so this is the rule
//     verified where a client can see it; the rule itself, applied to rows
//     the reader did return, is the unit suite's.)
//
//  3. The entry identities are read off the REAL rows: the version's own
//     uuid (the urn) and the pid plus the immutable version label (the
//     link). A feed whose ids came from anywhere else would be a feed whose
//     ids move when something else does.
//
//  4. The reads WRITE NOTHING: the feed store sits on a pool whose
//     connections carry default_transaction_read_only=on, so the server
//     refuses a write with SQLSTATE 25006 — with a control proving the
//     probe is a real write — while every feed route still answers over it.
//
// What is deliberately NOT here: what a feed document may CONTAIN as a
// document (the unit suite's, byte for byte), the web app's rendering of a
// feed, and the knowledge object's own page (it does not exist yet — T0805
// owns the published knowledge object and its id scheme; see the note on
// the knowledge links below).

package integration

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/feedshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/feeds"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// feedTaskID namespaces this task's test databases (test_T1004_<run_id>).
const feedTaskID = "T1004"

// feedWebOrigin is the configured public origin every URL in a document is
// built from (POST_WEB_ORIGIN in production). The assertion below checks the
// links against THIS value rather than against the request's host, which is
// the property that keeps a feed from echoing whatever host a client sent.
const feedWebOrigin = "http://web.test"

// --------------------------------------------------------------------------
// The composed surface

// feedWorld is the feed surface as production wires it: the real guard, the
// real publish command (so the version rows a feed reads are the ones the
// product's own writer created), and the feed store over READ-ONLY
// connections.
type feedWorld struct {
	ts       *httptest.Server
	pool     *pgxpool.Pool
	readOnly *pgxpool.Pool
}

func newFeedWorld(t *testing.T, ctx context.Context) *feedWorld {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), feedTaskID)
	readOnly := openReadOnlyPool(t, ctx, dbURL)

	authAPI := authhttp.New(authhttp.Deps{
		Users:    persistence.NewCredentialStore(pool),
		Sessions: memstore.NewSessions(),
		Limiter:  memstore.NewLimiter(),
		Cfg: authn.Config{
			WebOrigin:          feedWebOrigin,
			SessionTTL:         time.Hour,
			LoginLimitPerEmail: 1000,
			LoginLimitPerIP:    10000,
			LoginWindow:        time.Minute,
			SignupLimitPerIP:   10000,
		},
	})
	orgStore := persistence.NewOrgStore(pool)
	projectStore := persistence.NewProjectStore(pool)
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	policyAPI := policyhttp.New(policyhttp.Deps{
		Store:    persistence.NewPolicyStore(pool),
		Orgs:     orgStore,
		Projects: projectStore,
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})

	// The feed service over the READ-ONLY pool: the same construction
	// cmd/api/main.go does, with feedService(...) inlined so the test states
	// the wiring it is testing rather than a helper that could drift from it.
	feedSvc, err := feeds.NewService(persistence.NewFeedStore(readOnly), feeds.Config{BaseURL: feedWebOrigin})
	if err != nil {
		t.Fatalf("feeds.NewService: %v", err)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	assetshttp.New(assetshttp.Deps{
		State:    assetshttp.NewPostgresStateStore(pool),
		Projects: projectAPI.Service(),
		Publish:  publishCommand,
		Pages:    persistence.NewAssetPageStore(pool),
		Members:  projectAPI.Service(),
	}).Register(mux)
	feedshttp.New(feedshttp.Deps{Service: feedSvc}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &feedWorld{ts: ts, pool: pool, readOnly: readOnly}
}

// --------------------------------------------------------------------------
// Reading a feed

// anonymousFeed fetches a feed URL with NO cookie, no session and no CSRF
// token: exactly the request a feed reader or a crawler makes. Every
// positive assertion in this file goes through it, because "未登录可订阅" is
// a claim about this request and nothing else.
func anonymousFeed(t *testing.T, server, path string, headers map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, server+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	// A plain client with no jar: nothing this test does can carry a session
	// even by accident.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header, readAll(t, resp)
}

// feedEntryWire is the shape this suite reads out of an Atom document.
// Declared here (rather than reused from internal/application/feeds) so a
// silent change to the model's own serialization fails HERE, where the
// assertions are about what a reader receives.
type feedLinkWire struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

// Link is a POINTER on both wire types so that "no element at all" and "an
// element with an empty href" are different values. The difference is the
// point: an entity with no page must render no link, and a document that
// instead carried href="" would be a link to nothing — the suite has to be
// able to tell the two apart to assert the first.
type feedEntryWire struct {
	ID      string        `xml:"id"`
	Title   string        `xml:"title"`
	Updated string        `xml:"updated"`
	Link    *feedLinkWire `xml:"link"`
	Summary string        `xml:"summary"`
}

type feedDocumentWire struct {
	XMLName xml.Name        `xml:"feed"`
	ID      string          `xml:"id"`
	Title   string          `xml:"title"`
	Updated string          `xml:"updated"`
	Link    *feedLinkWire   `xml:"link"`
	Entries []feedEntryWire `xml:"entry"`
}

func parseFeed(t *testing.T, raw string) feedDocumentWire {
	t.Helper()
	var doc feedDocumentWire
	if err := xml.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("the served document is not parseable Atom: %v\n%s", err, raw)
	}
	return doc
}

// feedErrorCode reads the envelope's code out of a refusal body.
func feedErrorCode(t *testing.T, raw string) string {
	t.Helper()
	var envelope struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("the refusal body is not the JSON envelope: %v (%q)", err, raw)
	}
	return envelope.Code
}

// --------------------------------------------------------------------------
// The fixture

// feedFixture is the arrangement: a public project with published output, a
// private project with the same, and the rows that make the difference
// between them legible.
type feedFixture struct {
	publicProject  string
	privateProject string

	// A public asset in the public project, with one PUBLIC version (1.0)
	// and one PRIVATE one (hidden-label). The private version is what the
	// project feed must not render even though the asset is public.
	publicAssetPID        string
	publicVersionID       string
	privateVersionID      string
	privateVersionLabel   string
	publicVersionLabel    string
	assetDescriptionTitle string

	// A published asset inside the PRIVATE project: a public version of an
	// asset the network may not reach, because its project is not public.
	privateAssetPID string

	// A published knowledge object in the public project, an unpublished one
	// in the same project, and a published one in the private project.
	publicObjectID         string
	publicPublicationID    string
	publicObjectVersion    string
	publicObjectTitle      string
	unpublishedObjectID    string
	unpublishedObjectTitle string
	privateObjectID        string
	privatePublicationID   string
	privateObjectTitle     string
}

// seedFeedFixture writes the fixture. Project, state, object and publication
// rows are SQL — knowledge_publications has no product writer yet (T0805 is
// the explicit Publish to Network that will create them) and the rest are
// rows rather than requests — while the ASSET VERSIONS are published through
// the real command, so the feed reads versions production wrote.
func seedFeedFixture(t *testing.T, ctx context.Context, w *feedWorld) *feedFixture {
	t.Helper()
	alice, aliceID := signup(t, w.ts.URL, "feed-alice@example.com", "feed-alice")
	bob, bobID := signup(t, w.ts.URL, "feed-bob@example.com", "feed-bob")
	pool := w.pool
	f := &feedFixture{
		publicVersionLabel:     "1.0",
		privateVersionLabel:    "hidden-label",
		assetDescriptionTitle:  "Open Alloy Screen",
		publicObjectVersion:    "v1.0",
		publicObjectTitle:      "Solvent-Free Synthesis",
		unpublishedObjectTitle: "Draft Claim Never Published",
		privateObjectTitle:     "Withheld Claim",
	}

	f.publicProject = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('open-lab', 'Open Lab', 'A public research project.', 'public', $1) RETURNING id`, aliceID)
	f.privateProject = mustQueryUUID(t, ctx, pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('hidden-lab', 'Hidden Lab', 'not for the network', 'private', $1) RETURNING id`, bobID)
	for _, m := range []struct{ project, user string }{
		{f.publicProject, aliceID}, {f.privateProject, bobID},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
			m.project, m.user); err != nil {
			t.Fatalf("seed membership: %v", err)
		}
	}
	publicState := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-feed-open', '1') RETURNING id`, f.publicProject)
	privateState := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-feed-private', '1') RETURNING id`, f.privateProject)
	publicRelease := mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Feed Baseline', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.publicProject, publicState, aliceID)
	privateRelease := mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Hidden Baseline', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.privateProject, privateState, bobID)

	blob := mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:feed-open', 4096, 'blobs/feed-open', $1) RETURNING id`, aliceID)

	// The knowledge objects, defined before the publishes because the blob
	// the asset versions carry has to be openly attached to something: the
	// publish gate refuses a version whose rights declare open data access
	// over a blob no attachment calls open (T0704/T0705), and an attachment
	// is (blob, object version).
	//
	// The titles are deliberately free of digits: the negative assertions
	// below are plain substring checks on the raw body, and a digit-bearing
	// title could collide with a timestamp.
	seedObject := func(projectID, ownerID, title string, state string) string {
		t.Helper()
		objectID := mustQueryUUID(t, ctx, pool,
			`INSERT INTO scientific_objects (project_id, object_type, created_by)
			 VALUES ($1, 'claim', $2) RETURNING id`, projectID, ownerID)
		mustQueryUUID(t, ctx, pool,
			`INSERT INTO scientific_object_versions
				(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
			 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/claim.schema.json', '1',
			         $3, 'active', '{}'::jsonb, 'x', $4) RETURNING id`,
			objectID, state, title, ownerID)
		return objectID
	}
	// The version row of an object's first version, read back by its own key
	// rather than threaded through the helper (what the feed reads is the
	// publication, and a fixture that handed over the wrong row would fail
	// here rather than in the assertion).
	firstVersionID := func(objectID string) string {
		t.Helper()
		return mustQueryUUID(t, ctx, pool,
			`SELECT id::text FROM scientific_object_versions WHERE object_id = $1 AND version_no = 1`, objectID)
	}
	publishKnowledge := func(versionID, publisherID string) string {
		t.Helper()
		return mustQueryUUID(t, ctx, pool,
			`INSERT INTO knowledge_publications (object_version_id, public_version, rights_json, published_by)
			 VALUES ($1, $2, '{"license":"CC-BY-4.0"}'::jsonb, $3) RETURNING id`,
			versionID, f.publicObjectVersion, publisherID)
	}

	// The blob carrier: an object in the public project whose version holds
	// the open attachment. It is never published and so never appears in a
	// feed — it exists because the asset publish gate resolves the blob's
	// access through its attachments (assets.Preview).
	carrier := seedObject(f.publicProject, aliceID, "Feed Blob Carrier", publicState)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`,
		blob, firstVersionID(carrier), publicState); err != nil {
		t.Fatalf("attach the blob openly: %v", err)
	}

	// The public asset's public version.
	published := mustPublish(t, alice, f.publicProject, buildCandidate(t, candidateOptions{
		version: f.publicVersionLabel, visibility: "public",
		refs: []string{"release:" + publicRelease}, creators: []string{aliceID},
		blobIDs: []string{blob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: f.assetDescriptionTitle, slug: "open-alloy-screen",
	}).body(t), "feed-asset-1")
	f.publicAssetPID = published.AssetPID
	assetID := assetRowID(t, ctx, pool, f.publicAssetPID)
	f.publicVersionID = versionRowID(t, ctx, pool, assetID, f.publicVersionLabel)

	// The SAME asset's private version: a public project may hold one (a
	// private publication widens nothing), and it is the row the project
	// feed must drop while keeping the public one.
	mustPublish(t, alice, f.publicProject, buildCandidate(t, candidateOptions{
		pid: f.publicAssetPID, version: f.privateVersionLabel, visibility: "private",
		refs: []string{"release:" + publicRelease}, creators: []string{aliceID},
		blobIDs: []string{blob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
	}).body(t), "feed-asset-2")
	f.privateVersionID = versionRowID(t, ctx, pool, assetID, f.privateVersionLabel)

	// A published asset inside the private project: its version is public,
	// its project is not.
	published = mustPublish(t, bob, f.privateProject, buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs: []string{"release:" + privateRelease}, creators: []string{bobID},
		blobIDs: []string{blob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "Withheld Screen", slug: "withheld-screen",
	}).body(t), "feed-private-asset-1")
	f.privateAssetPID = published.AssetPID

	// The published knowledge objects.
	f.publicObjectID = seedObject(f.publicProject, aliceID, f.publicObjectTitle, publicState)
	f.publicPublicationID = publishKnowledge(firstVersionID(f.publicObjectID), aliceID)

	// An object in the public project that was never published: no feed.
	f.unpublishedObjectID = seedObject(f.publicProject, aliceID, f.unpublishedObjectTitle, publicState)

	// A PUBLISHED object in the private project: the same shape as the
	// public one, one visibility flag away from being reachable.
	f.privateObjectID = seedObject(f.privateProject, bobID, f.privateObjectTitle, privateState)
	f.privatePublicationID = publishKnowledge(firstVersionID(f.privateObjectID), bobID)

	return f
}

// --------------------------------------------------------------------------
// 1. 未登录可订阅 public feed

// TestFeedPublicProjectIsSubscribableAnonymously is the first acceptance
// criterion over real rows: no session, no cookie, and the document a feed
// reader would subscribe to.
func TestFeedPublicProjectIsSubscribableAnonymously(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)

	status, header, raw := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+f.publicProject, nil)
	if status != http.StatusOK {
		t.Fatalf("anonymous project feed = %d, want 200: %s", status, raw)
	}
	if ct := header.Get("Content-Type"); ct != "application/atom+xml; charset=utf-8" {
		t.Errorf("Content-Type = %q, want Atom", ct)
	}
	doc := parseFeed(t, raw)
	if want := "urn:post:feed:project:" + f.publicProject; doc.ID != want {
		t.Errorf("feed id = %q, want %q", doc.ID, want)
	}
	if doc.Title != "Open Lab" {
		t.Errorf("feed title = %q, want the project's name", doc.Title)
	}
	wantProjectLink := feedWebOrigin + "/projects/" + f.publicProject
	if doc.Link == nil {
		t.Errorf("the project feed has no alternate link, want %q", wantProjectLink)
	} else if doc.Link.Href != wantProjectLink {
		t.Errorf("feed alternate = %q, want %q (built from the CONFIGURED origin, not the request's host)",
			doc.Link.Href, wantProjectLink)
	}
	if len(doc.Entries) != 2 {
		t.Fatalf("entries = %d, want the asset version and the publication: %s", len(doc.Entries), raw)
	}

	// The entries are the rows: the version's own uuid is the urn, and the
	// link is the pid plus the immutable version label.
	byID := map[string]feedEntryWire{}
	for _, e := range doc.Entries {
		byID[e.ID] = e
	}
	versionEntry, ok := byID["urn:post:asset-version:"+f.publicVersionID]
	if !ok {
		t.Fatalf("no entry for the published version %s:\n%s", f.publicVersionID, raw)
	}
	// The asset version HAS a page, so this entry's link must be present and
	// point at it. This is the control for the publication assertion below:
	// without it, a model that cleared every entry's link would pass.
	wantVersionLink := feedWebOrigin + "/assets/" + f.publicAssetPID + "/" + f.publicVersionLabel
	if versionEntry.Link == nil {
		t.Errorf("version entry has no link, want %q", wantVersionLink)
	} else if versionEntry.Link.Href != wantVersionLink {
		t.Errorf("version entry link = %q, want %q", versionEntry.Link.Href, wantVersionLink)
	} else if versionEntry.Link.Rel != "alternate" {
		t.Errorf("version entry link rel = %q, want alternate", versionEntry.Link.Rel)
	}
	if !strings.Contains(versionEntry.Summary, "feed-alice") {
		t.Errorf("version entry summary does not name the publisher: %q", versionEntry.Summary)
	}
	if _, err := time.Parse(time.RFC3339, versionEntry.Updated); err != nil {
		t.Errorf("version entry updated = %q is not RFC 3339: %v", versionEntry.Updated, err)
	}

	publicationEntry, ok := byID["urn:post:knowledge-publication:"+f.publicPublicationID]
	if !ok {
		t.Fatalf("no entry for the published knowledge version %s:\n%s", f.publicPublicationID, raw)
	}
	if !strings.Contains(publicationEntry.Title, f.publicObjectTitle) {
		t.Errorf("publication entry title = %q, want the version's own title", publicationEntry.Title)
	}
	// A knowledge publication has NO page: no route renders one (T0805 owns
	// that surface, and apps/web/app/sitemap.ts says so). So there is no link
	// element at all — not an empty href, which is a link to nothing. The
	// entry keeps its identity in the urn, asserted by the map lookup above.
	if publicationEntry.Link != nil {
		t.Errorf("publication entry carries a link (%+v), want no link element: no route renders a knowledge publication",
			publicationEntry.Link)
	}

	// The feed is deterministic: the same read answers the same document, so
	// the validator is a fact about the content.
	againStatus, againHeader, againRaw := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+f.publicProject, nil)
	if againStatus != http.StatusOK || againRaw != raw {
		t.Errorf("a second anonymous read answered %d and a different document", againStatus)
	}
	etag := header.Get("ETag")
	if etag == "" || etag != againHeader.Get("ETag") {
		t.Errorf("ETag = %q then %q, want one stable validator", etag, againHeader.Get("ETag"))
	}
	notModified, _, body := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+f.publicProject,
		map[string]string{"If-None-Match": etag})
	if notModified != http.StatusNotModified || body != "" {
		t.Errorf("If-None-Match answered %d with %d bytes, want 304 and no body", notModified, len(body))
	}

	// ?format=rss answers the RSS shape of the same content (a feed reader
	// that speaks only RSS), over the same composition.
	rssStatus, rssHeader, rssRaw := anonymousFeed(t, w.ts.URL,
		"/api/v1/feeds/projects/"+f.publicProject+"?format=rss", nil)
	if rssStatus != http.StatusOK {
		t.Fatalf("RSS feed = %d, want 200: %s", rssStatus, rssRaw)
	}
	if ct := rssHeader.Get("Content-Type"); ct != "application/rss+xml; charset=utf-8" {
		t.Errorf("RSS Content-Type = %q", ct)
	}
	var rss struct {
		Channel struct {
			Title string `xml:"title"`
			Items []struct {
				GUID struct {
					IsPermaLink string `xml:"isPermaLink,attr"`
					Value       string `xml:",chardata"`
				} `xml:"guid"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal([]byte(rssRaw), &rss); err != nil {
		t.Fatalf("the RSS document does not parse: %v\n%s", err, rssRaw)
	}
	if rss.Channel.Title != "Open Lab" || len(rss.Channel.Items) != 2 {
		t.Errorf("RSS channel = %q with %d items, want the project and 2 items", rss.Channel.Title, len(rss.Channel.Items))
	}
	for i, item := range rss.Channel.Items {
		if item.GUID.IsPermaLink != "false" || !strings.HasPrefix(item.GUID.Value, "urn:post:") {
			t.Errorf("RSS item %d guid = %+v, want the stable urn with isPermaLink=false", i, item.GUID)
		}
	}
}

// TestFeedAssetAndKnowledgeAreSubscribableAnonymously covers the other two
// families, and the one public target that has NO feed: an object nobody has
// published.
func TestFeedAssetAndKnowledgeAreSubscribableAnonymously(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)

	status, _, raw := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/assets/"+f.publicAssetPID, nil)
	if status != http.StatusOK {
		t.Fatalf("anonymous asset feed = %d, want 200: %s", status, raw)
	}
	doc := parseFeed(t, raw)
	if want := "urn:post:feed:asset:" + f.publicAssetPID; doc.ID != want {
		t.Errorf("asset feed id = %q, want %q", doc.ID, want)
	}
	if len(doc.Entries) != 1 {
		t.Fatalf("asset feed entries = %d, want only the public version:\n%s", len(doc.Entries), raw)
	}
	wantEntryLink := feedWebOrigin + "/assets/" + f.publicAssetPID + "/" + f.publicVersionLabel
	if doc.Entries[0].Link == nil {
		t.Errorf("asset entry has no link, want %q", wantEntryLink)
	} else if doc.Entries[0].Link.Href != wantEntryLink {
		t.Errorf("asset entry link = %q, want %q", doc.Entries[0].Link.Href, wantEntryLink)
	}

	status, _, raw = anonymousFeed(t, w.ts.URL, "/api/v1/feeds/knowledge/"+f.publicObjectID, nil)
	if status != http.StatusOK {
		t.Fatalf("anonymous knowledge feed = %d, want 200: %s", status, raw)
	}
	doc = parseFeed(t, raw)
	if want := "urn:post:feed:knowledge:" + f.publicObjectID; doc.ID != want {
		t.Errorf("knowledge feed id = %q, want %q", doc.ID, want)
	}
	if len(doc.Entries) != 1 || doc.Entries[0].ID != "urn:post:knowledge-publication:"+f.publicPublicationID {
		t.Fatalf("knowledge feed entries = %+v, want the one publication", doc.Entries)
	}
	if !strings.Contains(doc.Entries[0].Title, f.publicObjectTitle) {
		t.Errorf("knowledge entry title = %q, want the version's title", doc.Entries[0].Title)
	}
	// Neither the feed nor its entry carries a URL, because the platform
	// serves no /knowledge page — and the feed's identity (the urn above) is
	// not derived from one, so nothing that identifies it was lost. The asset
	// feed read just above is the control: its links ARE there.
	if doc.Link != nil {
		t.Errorf("knowledge feed carries an alternate link (%+v), want none: there is no /knowledge route", doc.Link)
	}
	if doc.Entries[0].Link != nil {
		t.Errorf("knowledge feed entry carries a link (%+v), want none", doc.Entries[0].Link)
	}

	// An object in the public project that was never published has no feed:
	// an empty one would say "this exists and is public but has no output",
	// which is a statement about a private thing.
	status, _, raw = anonymousFeed(t, w.ts.URL, "/api/v1/feeds/knowledge/"+f.unpublishedObjectID, nil)
	if status != http.StatusNotFound {
		t.Errorf("unpublished knowledge object feed = %d, want 404: %s", status, raw)
	}

	// A public project that has published NOTHING is the one empty feed that
	// exists: the project itself is public.
	emptyProject := mustQueryUUID(t, ctx, w.pool,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('quiet-lab', 'Quiet Lab', 'Nothing published yet.', 'public',
		         (SELECT id FROM users WHERE handle = 'feed-alice')) RETURNING id`)
	status, _, raw = anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+emptyProject, nil)
	if status != http.StatusOK {
		t.Fatalf("empty public project feed = %d, want 200: %s", status, raw)
	}
	if doc := parseFeed(t, raw); len(doc.Entries) != 0 || doc.Title != "Quiet Lab" {
		t.Errorf("empty feed = %+v, want the project's name and no entries", doc)
	}
}

// --------------------------------------------------------------------------
// 2. private 无 feed

// TestPrivateTargetsHaveNoFeedForAnyone is the second acceptance criterion.
// Every private target answers the ONE 404, and the answers are compared as
// BYTES: a private project's feed must be indistinguishable from an id that
// names nothing, or the route is an existence oracle for private work.
func TestPrivateTargetsHaveNoFeedForAnyone(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)
	bob := loginFeedUser(t, w.ts.URL, "feed-bob@example.com")

	unknownProject := "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
	unknownObject := "1e1e1e1e-1e1e-4e1e-8e1e-1e1e1e1e1e1e"

	cases := []struct {
		name string
		path string
	}{
		{"private project's feed", "/api/v1/feeds/projects/" + f.privateProject},
		{"unknown project's feed", "/api/v1/feeds/projects/" + unknownProject},
		{"published asset in a private project", "/api/v1/feeds/assets/" + f.privateAssetPID},
		{"published knowledge object in a private project", "/api/v1/feeds/knowledge/" + f.privateObjectID},
		{"unknown knowledge object", "/api/v1/feeds/knowledge/" + unknownObject},
		{"a slug where a pid belongs", "/api/v1/feeds/assets/withheld-screen"},
	}
	var privateBody, unknownBody string
	for _, tc := range cases {
		status, header, raw := anonymousFeed(t, w.ts.URL, tc.path, nil)
		if status != http.StatusNotFound {
			t.Errorf("anonymous %s = %d, want 404: %s", tc.name, status, raw)
			continue
		}
		if code := feedErrorCode(t, raw); code != feedshttp.CodeFeedNotFound {
			t.Errorf("%s code = %q, want %q", tc.name, code, feedshttp.CodeFeedNotFound)
		}
		if cc := header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s Cache-Control = %q, want no-store on a refusal", tc.name, cc)
		}
		// Nothing about the private entity may ride along in the refusal.
		for _, leak := range []string{"Hidden Lab", "hidden-lab", f.privateProject, f.privateAssetPID, f.privateObjectID,
			f.privateObjectTitle, "Withheld Screen"} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s refusal names %q: %s", tc.name, leak, raw)
			}
		}
		switch tc.name {
		case "private project's feed":
			privateBody = raw
		case "unknown project's feed":
			unknownBody = raw
		}
	}
	if privateBody != unknownBody {
		t.Errorf("a private project and an unknown id answer different bodies:\nprivate: %s\nunknown: %s",
			privateBody, unknownBody)
	}

	// A MEMBER's session must not unlock it either: a feed is a URL a reader
	// subscribes to, and an answer that depended on who fetched it would not
	// be a feed.
	for _, path := range []string{
		"/api/v1/feeds/projects/" + f.privateProject,
		"/api/v1/feeds/assets/" + f.privateAssetPID,
		"/api/v1/feeds/knowledge/" + f.privateObjectID,
	} {
		resp := bob.do(t, http.MethodGet, path, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("the private project's owner got %d for %s, want 404", resp.StatusCode, path)
		}
		if body := readAll(t, resp); feedErrorCode(t, body) != feedshttp.CodeFeedNotFound {
			t.Errorf("owner's refusal for %s is not %s: %s", path, feedshttp.CodeFeedNotFound, body)
		}
	}
}

// TestPrivateRowsAreNeverRenderedFromThePublicFeed: the public project's feed
// is served to the network, and the fixture hands it a private version of an
// otherwise public asset. The negative assertions are on the RAW BODY, so a
// field the transport added on its own — a count, an id, a label — fails
// here rather than being argued about.
func TestPrivateRowsAreNeverRenderedFromThePublicFeed(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)

	// The row really exists, and really is private: without this the negative
	// assertions below would pass on a fixture that never wrote it.
	var count int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_asset_versions WHERE id = $1 AND visibility = 'private'`,
		f.privateVersionID).Scan(&count); err != nil {
		t.Fatalf("read the private version: %v", err)
	}
	if count != 1 {
		t.Fatalf("the fixture's private version %s is not a private row", f.privateVersionID)
	}

	for _, path := range []string{
		"/api/v1/feeds/projects/" + f.publicProject,
		"/api/v1/feeds/assets/" + f.publicAssetPID,
	} {
		status, _, raw := anonymousFeed(t, w.ts.URL, path, nil)
		if status != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200: %s", path, status, raw)
		}
		for _, leak := range []string{f.privateVersionID, f.privateVersionLabel,
			// The version's address must not appear either: a link to a
			// version the network may not read is the leak in its most
			// clickable form.
			f.publicAssetPID + "/" + f.privateVersionLabel} {
			if strings.Contains(raw, leak) {
				t.Errorf("%s renders %q, which is a private row:\n%s", path, leak, raw)
			}
		}
		if !strings.Contains(raw, f.publicVersionID) {
			t.Errorf("%s does not render the public version %s — the filter dropped the wrong row",
				path, f.publicVersionID)
		}
	}

	// The project feed carries the knowledge publication and drops nothing
	// that belongs to the OTHER project: a feed is about one project.
	status, _, raw := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+f.publicProject, nil)
	if status != http.StatusOK {
		t.Fatalf("public project feed = %d", status)
	}
	for _, leak := range []string{f.privatePublicationID, f.privateAssetPID, f.privateProject} {
		if strings.Contains(raw, leak) {
			t.Errorf("the public project's feed names %q, which belongs to the private project:\n%s", leak, raw)
		}
	}
}

// TestPrivateVersionsDoNotSuppressPublicOnes: the feed reads are BOUNDED —
// they read the newest DefaultMaxEntries rows and the model then drops the
// private ones. Read raw, that window is a resource a writer can exhaust:
// more than DefaultMaxEntries newer PRIVATE versions of a public asset push
// the public ones out of the window, and the feed of a public, published
// asset answers as if there were nothing to subscribe to.
//
// That is a statement about the platform's own output, not about disclosure
// — nothing private leaks either way — and it is wrong on its own terms: a
// subscriber that follows a public asset must not lose its feed because
// someone drafted, privately, above it. This pins the property that the
// window is counted in rows a public feed may RENDER.
//
// The fixture's private rows are written directly (the publish command would
// do the same work through a much longer path), because what is under test
// is the read, not the writer.
func TestPrivateVersionsDoNotSuppressPublicOnes(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)

	// Bury the fixture's public version: strictly newer private versions of
	// the same asset, one more than the window can hold.
	assetID := assetRowID(t, ctx, w.pool, f.publicAssetPID)
	for i := 0; i < feeds.DefaultMaxEntries+10; i++ {
		if _, err := w.pool.Exec(ctx,
			`INSERT INTO research_asset_versions
			   (asset_id, version, manifest, rights_json, visibility, integrity_hash,
			    published_by, published_at, origin_refs)
			 VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 'private', 'x',
			         (SELECT id FROM users WHERE handle = 'feed-alice'),
			         now() + make_interval(secs => $3), ARRAY['project:' || $4::text])`,
			assetID, fmt.Sprintf("draft-%02d", i), i+1, f.publicProject); err != nil {
			t.Fatalf("seed private version %d: %v", i, err)
		}
	}

	// The control: the burial is real — the private rows really are newer
	// than the public one, so a query without a visibility filter really
	// would have handed only private rows back.
	var newerPrivate int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM research_asset_versions pv
		 JOIN research_asset_versions pub ON pub.id = $1::uuid
		 WHERE pv.asset_id = pub.asset_id AND pv.visibility = 'private'
		   AND pv.published_at > pub.published_at`,
		f.publicVersionID).Scan(&newerPrivate); err != nil {
		t.Fatalf("count the newer private versions: %v", err)
	}
	if newerPrivate <= feeds.DefaultMaxEntries {
		t.Fatalf("fixture: only %d private versions are newer than the public one, "+
			"which the window of %d already accommodates — the probe would pass for the wrong reason",
			newerPrivate, feeds.DefaultMaxEntries)
	}

	// The asset's own feed still exists, and still renders the public
	// version: a public asset with a public version has a feed, whatever is
	// drafted above it.
	status, _, raw := anonymousFeed(t, w.ts.URL, "/api/v1/feeds/assets/"+f.publicAssetPID, nil)
	if status != http.StatusOK {
		t.Fatalf("the public asset's feed = %d, want 200: a public asset with a public version has a feed; %s",
			status, raw)
	}
	doc := parseFeed(t, raw)
	if len(doc.Entries) != 1 || doc.Entries[0].ID != "urn:post:asset-version:"+f.publicVersionID {
		t.Errorf("asset feed entries = %+v, want exactly the public version %s",
			doc.Entries, f.publicVersionID)
	}

	// The project's feed renders it too.
	status, _, raw = anonymousFeed(t, w.ts.URL, "/api/v1/feeds/projects/"+f.publicProject, nil)
	if status != http.StatusOK {
		t.Fatalf("the public project's feed = %d, want 200: %s", status, raw)
	}
	rendered := false
	for _, e := range parseFeed(t, raw).Entries {
		if e.ID == "urn:post:asset-version:"+f.publicVersionID {
			rendered = true
		}
	}
	if !rendered {
		t.Errorf("the project feed dropped the public version %s: the window was spent on private rows", f.publicVersionID)
	}

	// And the private rows are still not rendered — the fix for the window
	// must not have widened anything.
	if strings.Contains(raw, "draft-") {
		t.Errorf("the project feed renders a private version label:\n%s", raw)
	}
}

// --------------------------------------------------------------------------
// 3. The reads write nothing

// TestFeedReadsWriteNothing runs every feed route over a pool whose
// connections carry default_transaction_read_only=on. The control (a real
// INSERT that the server refuses with SQLSTATE 25006) is what makes the rest
// evidence: without it, "the reads did not write" would be a claim about
// queries nobody proved could fail.
func TestFeedReadsWriteNothing(t *testing.T) {
	ctx := context.Background()
	w := newFeedWorld(t, ctx)
	f := seedFeedFixture(t, ctx, w)

	// The control: this pool really does refuse a write.
	var probeErr error
	_, probeErr = w.readOnly.Exec(ctx,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('readonly-probe', 'probe', 'probe', 'public',
		         (SELECT id FROM users WHERE handle = 'feed-alice'))`)
	if probeErr == nil {
		t.Fatal("the read-only pool accepted a write — the probe is not measuring read-only-ness")
	}
	if !strings.Contains(probeErr.Error(), "25006") {
		t.Fatalf("the refused probe failed for the wrong reason: %v", probeErr)
	}
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO projects (slug, name, purpose, visibility, created_by)
		 VALUES ('readonly-probe', 'probe', 'probe', 'public',
		         (SELECT id FROM users WHERE handle = 'feed-alice'))`); err != nil {
		t.Fatalf("the same SQL on the writable pool failed, so the refusal was not about the session: %v", err)
	}

	// The feed routes, over the same read-only connections.
	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/feeds/projects/" + f.publicProject, http.StatusOK},
		{"/api/v1/feeds/projects/" + f.privateProject, http.StatusNotFound},
		{"/api/v1/feeds/assets/" + f.publicAssetPID, http.StatusOK},
		{"/api/v1/feeds/knowledge/" + f.publicObjectID, http.StatusOK},
		{"/api/v1/feeds/knowledge/" + f.unpublishedObjectID, http.StatusNotFound},
	}
	for _, tc := range cases {
		status, _, raw := anonymousFeed(t, w.ts.URL, tc.path, nil)
		if status != tc.want {
			t.Errorf("GET %s over read-only connections = %d, want %d: %s", tc.path, status, tc.want, raw)
		}
	}
}

// loginFeedUser signs in an EXISTING account (the fixture signs users up
// through the real endpoint) and answers an authenticated browser for it:
// the session that must NOT unlock a private project's feed.
func loginFeedUser(t *testing.T, server, email string) *testUserClient {
	t.Helper()
	uc := newTestUserClient(server)
	resp := uc.do(t, http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"email":%q,"password":"long-enough-password-1"}`, email))
	mustStatus(t, resp, http.StatusOK)
	uc.csrf = csrfFromAuth(t, resp)
	return uc
}
