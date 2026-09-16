// Task T0705 required test "asset publish tests" — the end-to-end half.
//
// The unit suites pin the command's decisions
// (internal/application/assetpublish: the authorization order, the agent
// backstop, the policy rule, the replay rules, the audit row) and the
// transport's shape (cmd/api/assetshttp: the route, the session guard, the
// envelope, the complete refusal report). This file pins what only a real
// PostgreSQL and the whole composed path can settle — the six acceptance
// criteria of the task, each over real rows, plus the durability the
// command claims:
//
//  1. An AGENT may not publish. V1 mints no agent tokens, so the flag the
//     transport sets is false for every request that can arrive today; the
//     fact is asserted where it is decidable — the command, over the real
//     store — and the second line of defence is asserted independently:
//     the permission matrix denies the agent column even when the agent is
//     the project's OWNER. Nothing is written by either.
//
//  2. A repeated request is IDEMPOTENT: the same Idempotency-Key returns
//     the version the first call published, and the database holds one row
//     of every kind the publish writes — one version, one ledger entry,
//     ONE AUDIT ROW. The key is scoped to its publication, not to the
//     asset: one key used for a different target is a conflict, and a
//     different key publishes the next version.
//
//  3. A private→public publication is REFUSED when a private dependency
//     exists, and the caller receives the COMPLETE report — the same
//     document the preview route answers with, computed over the state the
//     publish itself saw — not a generic forbidden. The three leak shapes
//     docs/23 §4 names are seeded as real rows (a pin to a still-private
//     version, an origin ref into another project's private state, a blob
//     whose bytes are not openly attached under a rights declaration that
//     promises open data), each refused and each named in the report; and
//     the same bodies published PRIVATE are accepted, so the refusal is
//     about the widening and not about the dependencies existing. One case
//     moves the state between the preview and the publish: the publish
//     refuses, which is what makes "re-run over the current state" a fact
//     rather than an intention.
//
//  4. A published asset version is IMMUTABLE: a second publication of the
//     same (asset, version) is refused with ASSET_VERSION_IMMUTABLE, the
//     stored row is byte-for-byte what the first publish wrote, and the
//     database itself refuses an UPDATE or DELETE of it (the 00014
//     append-only trigger, with a control proving the same statement
//     succeeds on a table that is mutable by design). A different version
//     of the same asset is still publishable — immutability is about the
//     version, not about the asset.
//
//  5. The DENIAL PRECEDES the target lookup: a request against a project
//     id that names nothing answers the permission-class refusal, never
//     "project not found" — including for an id that is not even a uuid —
//     and an authenticated non-member gets exactly the same answer for a
//     project that DOES exist. The two are indistinguishable, which is the
//     property; the same body against the caller's own project succeeds,
//     which is what makes the refusal a fact about the actor.
//
//  6. The IDEMPOTENCY KEY really blocks the second publish: same key, same
//     asset version, one row, no second audit row, no second research
//     event.
//
// The composition is cmd/api/main.go's: the real auth guard, the real
// publish command with the real store, the real state reader over the
// canonical queries, the real permission matrix over the canonical CSV.
// Only the session store is in-memory, exactly as the release and preview
// e2e suites compose it.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// assetPublishTaskID namespaces this task's test databases
// (test_T0705_<run_id>).
const assetPublishTaskID = "T0705"

// assetVersionEventType is the domain event a publish records
// (specs/events/event-types.yaml).
const assetVersionEventType = "research_asset.version_published"

// --------------------------------------------------------------------------
// The composed surface

// assetPublishWorld is the publish path as production wires it.
type assetPublishWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
	// publish is the SAME command the HTTP route drives — the agent case
	// has no HTTP expression in V1, and driving the command directly is
	// what lets it be tested against the real store rather than a fake.
	publish *assetpublish.Command
	// store is the same store the command holds, exposed so the replay can
	// be driven with the command out of the picture (see
	// TestAssetPublishStoreReplaysUnderItsOwnLock).
	store *persistence.AssetPublishStore
}

// newAssetPublishWorld composes the production tree over one test
// database, exactly as cmd/api/main.go does.
func newAssetPublishWorld(t *testing.T, ctx context.Context) *assetPublishWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetPublishTaskID)

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
	publishStore := persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg))
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    publishStore,
		Authz:    authz.NewMatrixEngine(),
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(mux)
	assetshttp.New(assetshttp.Deps{
		State:    assetshttp.NewPostgresStateStore(pool),
		Projects: projectAPI.Service(),
		Publish:  publishCommand,
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &assetPublishWorld{ts: ts, pool: pool, publish: publishCommand, store: publishStore}
}

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// cmd/api/assetshttp, so a silent JSON tag change in the payload fails
// here)

type publishedAssetWire struct {
	AssetPID      string          `json:"asset_pid"`
	Version       string          `json:"version"`
	Visibility    string          `json:"visibility"`
	IntegrityHash string          `json:"integrity_hash"`
	OriginRefs    []string        `json:"origin_refs"`
	PublishedBy   string          `json:"published_by"`
	PublishedAt   string          `json:"published_at"`
	Manifest      json.RawMessage `json:"manifest"`
	Rights        json.RawMessage `json:"rights"`
}

// publishRefusalWire is the refusal body: the shared error envelope plus
// the complete preview (cmd/api/assetshttp blockedEnvelope).
type publishRefusalWire struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
	Retryable bool           `json:"retryable"`
	Preview   previewPayload `json:"preview"`
}

// publishURL is the contract's path (specs/api/openapi.yaml).
func publishURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/assets:publish"
}

// publish sends one publish request; key == "" sends no Idempotency-Key.
func publish(t *testing.T, uc *testUserClient, projectID, body, key string) *http.Response {
	t.Helper()
	return uc.doKeyed(t, http.MethodPost, publishURL(projectID), body, key)
}

// mustPublish sends one publish and requires the 201, returning the stored
// version as the wire renders it.
func mustPublish(t *testing.T, uc *testUserClient, projectID, body, key string) publishedAssetWire {
	t.Helper()
	resp := publish(t, uc, projectID, body, key)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got publishedAssetWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("published version payload: %v: %s", err, raw)
	}
	return got
}

// mustRefuse requires the 409 + ASSET_PUBLISH_BLOCKED refusal and returns
// the whole report.
func mustRefuse(t *testing.T, uc *testUserClient, projectID, body, key string) publishRefusalWire {
	t.Helper()
	resp := publish(t, uc, projectID, body, key)
	mustStatus(t, resp, http.StatusConflict)
	raw := readAll(t, resp)
	var got publishRefusalWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("refusal payload: %v: %s", err, raw)
	}
	if got.Code != assetpublish.CodePublishBlocked {
		t.Fatalf("refusal code = %q, want %q: %s", got.Code, assetpublish.CodePublishBlocked, raw)
	}
	return got
}

// mustRefuseWithCode requires one status + code and returns the raw body
// (the disclosure assertions read the raw bytes). It is named apart from
// mustRefuse above because it is the general form — the refusal that is NOT
// the publish's own report, i.e. the ones whose body is the plain envelope.
func mustRefuseWithCode(t *testing.T, uc *testUserClient, projectID, body, key string, status int, code string) string {
	t.Helper()
	resp := publish(t, uc, projectID, body, key)
	mustStatus(t, resp, status)
	raw := readAll(t, resp)
	var env struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("error envelope: %v: %s", err, raw)
	}
	if env.Code != code {
		t.Fatalf("code = %q, want %q: %s", env.Code, code, raw)
	}
	return raw
}

// --------------------------------------------------------------------------
// The candidate

// candidateOptions is the proposed publication: the knobs are the fields a
// case varies.
type candidateOptions struct {
	pid         string
	version     string
	visibility  string
	refs        []string
	pins        []assets.DependencyPin
	creators    []string
	blobIDs     []string
	accessLevel string
	dataAccess  rights.DataAccess
	title       string
	slug        string
}

// publishCandidate is one candidate plus the display fields a create
// carries. It keeps the gate's own value, so the request body the route
// receives is rendered from the very document the gate validates.
type publishCandidate struct {
	cand  assets.PublishCandidate
	title string
	slug  string
}

// buildCandidate assembles a dataset version — the type whose required
// metadata names blobs, which is what the blob leak shape needs — through
// the production documents (assets.Manifest, rights.Document).
//
// When the options name an asset, the helper also refuses to hand back a
// candidate the gate itself would reject: a fixture the gate refuses would
// test that refusal instead of the case under test. When they do not, the
// candidate CANNOT pass the gate — assets.Gate refuses an absent pid
// (gate.go: "expected a persisted asset's pid") — and that refusal is
// asserted rather than skipped, because it is what makes the create flow a
// fact: the pid the command's prepare mints with assets.NewPID is supplied
// before the gate ever sees the candidate, so a create cannot be expressed
// as a pre-gated candidate at all.
func buildCandidate(t *testing.T, o candidateOptions) publishCandidate {
	t.Helper()
	blobs := make([]any, 0, len(o.blobIDs))
	for _, id := range o.blobIDs {
		blobs = append(blobs, id)
	}
	m := assets.Manifest{
		Version:   assets.ManifestFormatVersion,
		AssetType: assets.TypeDataset,
		Metadata: assets.Metadata{
			"purpose":       "asset publish test",
			"data_type":     "table",
			"blob_ids":      blobs,
			"access_level":  o.accessLevel,
			"quality_notes": "reviewed",
		},
		DependencyPins: o.pins,
	}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	doc := rights.New()
	doc.Visibility.DataAccess = o.dataAccess
	rightsJSON, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	cand := assets.PublishCandidate{
		AssetPID:      assets.PID(o.pid),
		AssetType:     assets.TypeDataset,
		Version:       o.version,
		Manifest:      raw,
		RightsJSON:    rightsJSON,
		OriginRefs:    o.refs,
		Visibility:    assets.Visibility(o.visibility),
		IntegrityHash: hash,
		CreatorIDs:    o.creators,
	}
	if o.pid == "" {
		if _, err := assets.Gate(cand); err == nil {
			t.Fatal("the gate accepted a candidate with no pid: the create case assumes it cannot, " +
				"and a fixture that could have been pre-gated would make the minting untested")
		}
		return publishCandidate{cand: cand, title: o.title, slug: o.slug}
	}
	if _, err := assets.Gate(cand); err != nil {
		t.Fatalf("the fixture candidate is not publishable: %v", err)
	}
	return publishCandidate{cand: cand, title: o.title, slug: o.slug}
}

// body renders the candidate as the route's request body.
func (c publishCandidate) body(t *testing.T) string {
	t.Helper()
	out := map[string]any{
		"asset_pid":      string(c.cand.AssetPID),
		"asset_type":     string(c.cand.AssetType),
		"version":        c.cand.Version,
		"manifest":       json.RawMessage(c.cand.Manifest),
		"rights":         json.RawMessage(c.cand.RightsJSON),
		"origin_refs":    c.cand.OriginRefs,
		"visibility":     string(c.cand.Visibility),
		"integrity_hash": c.cand.IntegrityHash,
		"creator_ids":    c.cand.CreatorIDs,
	}
	// The display fields ride along only when the publication creates the
	// asset: the command refuses them otherwise, and a body that always
	// carried them would be testing that refusal in every other case.
	if c.title != "" {
		out["title"] = c.title
	}
	if c.slug != "" {
		out["slug"] = c.slug
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal publish body: %v", err)
	}
	return string(raw)
}

// params renders the candidate as the command's input — the shape the
// non-HTTP agent case needs.
func (c publishCandidate) params(projectID string, key *string) assetpublish.PublishParams {
	return assetpublish.PublishParams{
		ProjectID:      projectID,
		AssetPID:       string(c.cand.AssetPID),
		AssetType:      string(c.cand.AssetType),
		Version:        c.cand.Version,
		Manifest:       c.cand.Manifest,
		Rights:         c.cand.RightsJSON,
		OriginRefs:     c.cand.OriginRefs,
		Visibility:     string(c.cand.Visibility),
		IntegrityHash:  c.cand.IntegrityHash,
		CreatorIDs:     c.cand.CreatorIDs,
		Title:          c.title,
		Slug:           c.slug,
		IdempotencyKey: key,
	}
}

// cleanCandidate is a publication of the fixture's version-less asset that
// carries nothing private: the publishing project's own private release
// (carried by its own publication, docs/12 §2), an openly attached blob,
// and no pins. It is the control every refusal below is read against.
func cleanCandidate(t *testing.T, f *assetPreviewFixture, aliceID, version, visibility string) publishCandidate {
	t.Helper()
	return buildCandidate(t, candidateOptions{
		pid:         f.pubAssetPID,
		version:     version,
		visibility:  visibility,
		refs:        []string{"release:" + f.releaseA},
		creators:    []string{aliceID},
		blobIDs:     []string{f.openBlob},
		accessLevel: "open",
		dataAccess:  rights.DataAccessOpen,
	})
}

// mustPin builds one canonical dependency pin, or fails: a pin the platform
// cannot express would make the case test the pin's shape instead of the
// dependency rule.
func mustPin(t *testing.T, pid, version string) assets.DependencyPin {
	t.Helper()
	pin, ok := assets.NewDependencyPin(assets.PID(pid), version)
	if !ok {
		t.Fatalf("the fixture cannot pin %s@%s in the canonical form", pid, version)
	}
	return pin
}

// --------------------------------------------------------------------------
// The rows, as SQL reports them

// countRows answers one counting query, so "nothing was written" is a
// statement about the database rather than about the response.
func countRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", sql, err)
	}
	return n
}

// assetRowID resolves one research_assets row by its pid.
func assetRowID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM research_assets WHERE pid = $1`, pid).Scan(&id); err != nil {
		t.Fatalf("research_assets by pid %s: %v", pid, err)
	}
	return id
}

// versionRowID resolves one published version's row id — the identity the
// audit row and the ledger name, which the wire deliberately does not carry.
func versionRowID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, assetID, version string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM research_asset_versions WHERE asset_id = $1 AND version = $2`,
		assetID, version).Scan(&id); err != nil {
		t.Fatalf("research_asset_versions %s@%s: %v", assetID, version, err)
	}
	return id
}

// versionFingerprint renders every column a publish writes as one string,
// so "the row did not change" is one comparison instead of nine.
func versionFingerprint(t *testing.T, ctx context.Context, pool *pgxpool.Pool, assetID, version string) string {
	t.Helper()
	var (
		id, label, visibility, integrity, publishedBy, publishedAt, refs string
		manifest, rightsJSON                                             string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id::text, version, visibility, integrity_hash, published_by::text,
		        published_at::text, array_to_string(origin_refs, ','),
		        manifest::text, rights_json::text
		   FROM research_asset_versions WHERE asset_id = $1 AND version = $2`,
		assetID, version).Scan(&id, &label, &visibility, &integrity, &publishedBy, &publishedAt,
		&refs, &manifest, &rightsJSON); err != nil {
		t.Fatalf("read the stored version %s@%s: %v", assetID, version, err)
	}
	return strings.Join([]string{id, label, visibility, integrity, publishedBy, publishedAt,
		refs, manifest, rightsJSON}, "\n")
}

// --------------------------------------------------------------------------
// 1. An agent may not publish

// TestAssetPublishRefusesAnAgentToken drives the platform-agent case
// through the real command and the real store. V1 has no agent tokens —
// every request that can arrive today is a session — so the flag is
// produced here rather than by a credential, which is exactly the state of
// the build: the refusal exists BEFORE the tokens do, and this is what
// makes it a fact about the code rather than about a route nobody can
// call.
func TestAssetPublishRefusesAnAgentToken(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-agent-alice@example.com", "publish-agent-alice")
	_, bobID := signup(t, w.ts.URL, "publish-agent-bob@example.com", "publish-agent-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	c := cleanCandidate(t, f, aliceID, "9.0", "public")
	_, err := w.publish.Publish(ctx, assetpublish.Actor{User: domain.User{ID: aliceID}, IsAgent: true}, c.params(f.projectA, nil))
	if !errors.Is(err, assetpublish.ErrAgentNotPermitted) {
		t.Fatalf("an agent's publish = %v, want ErrAgentNotPermitted (the backstop must run before anything else)", err)
	}

	// Nothing was written by the refusal — not the version it proposed, not
	// the ledger, not the audit row, not the event. The version count is
	// scoped to the asset the agent tried to publish, because the fixture
	// itself seeds two published versions (its two dependencies); the other
	// four tables are empty until a publish writes them.
	probeAssetID := assetRowID(t, ctx, w.pool, f.pubAssetPID)
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1::uuid`, probeAssetID); n != 0 {
		t.Errorf("the refused agent publish wrote %d versions of the asset it named, want 0", n)
	}
	for _, table := range []string{"asset_publish_creations", "audit_log", "research_events", "outbox_events"} {
		if n := countRows(t, ctx, w.pool, "SELECT count(*) FROM "+table); n != 0 {
			t.Errorf("%s holds %d rows after a refused agent publish, want 0", table, n)
		}
	}

	// The SECOND line of defence, asserted on its own: the permission
	// matrix denies the agent column for the action, and that denial does
	// not depend on which role the agent acts as — an agent that IS the
	// project's owner is still denied. The rule that follows a human owner
	// is the same row read at the human column, which is what makes this a
	// comparison rather than a proof that the matrix denies everything.
	engine := authz.NewMatrixEngine()
	owner := domain.ProjectRoleOwner
	agentDecision, err := engine.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &owner, true),
	})
	if err != nil {
		t.Fatalf("matrix engine: %v", err)
	}
	if agentDecision.Permits() {
		t.Errorf("the permission matrix permits an agent that owns the project to publish: %+v", agentDecision)
	}
	humanDecision, err := engine.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &owner, false),
	})
	if err != nil {
		t.Fatalf("matrix engine: %v", err)
	}
	if !humanDecision.Permits() {
		t.Errorf("the permission matrix denies the project's owner the publish: %+v", humanDecision)
	}

	// The control: the same publication, the same session, over the wire —
	// accepted. The refusal above is about the agent, not about the row.
	got := mustPublish(t, alice, f.projectA, c.body(t), "")
	if got.Version != "9.0" || got.Visibility != "public" {
		t.Errorf("owner publish = %s@%s, want 9.0@public", got.AssetPID, got.Version)
	}
}

// --------------------------------------------------------------------------
// 2 + 6. The Idempotency-Key

// TestAssetPublishIsIdempotentPerKey is the repeat contract (docs/22): the
// same key answers the version the first call published, forever, and the
// second call writes NOTHING — which is asserted where a second write
// would be visible: the version count, the ledger, and the audit log. The
// task's sixth criterion is that last one: a replay that appended a second
// audit row would look idempotent on the wire and be a double publication
// in the record.
func TestAssetPublishIsIdempotentPerKey(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-idem-alice@example.com", "publish-idem-alice")
	_, bobID := signup(t, w.ts.URL, "publish-idem-bob@example.com", "publish-idem-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	assetID := assetRowID(t, ctx, w.pool, f.pubAssetPID)
	const key = "publish-key-1"
	body := cleanCandidate(t, f, aliceID, "1.0", "public").body(t)

	first := mustPublish(t, alice, f.projectA, body, key)
	second := mustPublish(t, alice, f.projectA, body, key)
	if second.AssetPID != first.AssetPID || second.Version != first.Version ||
		second.IntegrityHash != first.IntegrityHash || second.PublishedAt != first.PublishedAt {
		t.Errorf("the replay answered a different publication:\n first: %+v\nsecond: %+v", first, second)
	}

	versionID := versionRowID(t, ctx, w.pool, assetID, "1.0")
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetID); n != 1 {
		t.Errorf("the asset carries %d versions after a publish and its replay, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM asset_publish_creations WHERE project_id = $1::uuid AND idempotency_key = $2`,
		f.projectA, key); n != 1 {
		t.Errorf("the ledger holds %d entries for the key, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM audit_log WHERE action = $1 AND target_ref = $2`,
		assetpublish.ActionAssetVersionPublished, "asset_version:"+versionID); n != 1 {
		t.Errorf("the audit log holds %d publication rows for the version, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_events WHERE event_type = $1`, assetVersionEventType); n != 1 {
		t.Errorf("the research event log holds %d publication events, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1`, assetVersionEventType); n != 1 {
		t.Errorf("the outbox holds %d publication events, want 1", n)
	}

	// The key is scoped to ITS publication: the same key for a different
	// version is a conflict, not a replay, and not a second row.
	other := cleanCandidate(t, f, aliceID, "1.1", "public")
	mustRefuseWithCode(t, alice, f.projectA, other.body(t), key, http.StatusConflict, assetpublish.CodeIdempotencyConflict)
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetID); n != 1 {
		t.Errorf("the conflicting key wrote a version: %d rows for the asset, want 1", n)
	}

	// The control in the other direction: a DIFFERENT key publishes the
	// next version. The key scopes a replay; it does not lock the asset.
	got := mustPublish(t, alice, f.projectA, other.body(t), "publish-key-2")
	if got.Version != "1.1" {
		t.Errorf("the second version published as %q, want 1.1", got.Version)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetID); n != 2 {
		t.Errorf("the asset carries %d versions, want 2 (one per key)", n)
	}
}

// --------------------------------------------------------------------------
// 3. The private dependencies are refused, with the whole report

// TestAssetPublishRefusesPrivateDependencies is the acceptance criterion
// the task turns on: a private→public publication whose impact preview
// names a BLOCKING private dependency is refused, the caller is handed the
// complete report, and nothing is written.
//
// Each leak shape docs/23 §4 names is seeded as a real row and refused on
// its own, so a passing case cannot be explained by a neighbouring one:
//
//	(a) a dependency pin that resolves to a still-private version,
//	(b) an origin ref into ANOTHER project's private state,
//	(c) a blob that is not openly attached while the rights declaration
//	    promises open data.
//
// The verdicts are read against two controls: the same candidate published
// PRIVATE (legal — a private publication widens nothing), and the same
// candidate published public with every dependency public.
func TestAssetPublishRefusesPrivateDependencies(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-leak-alice@example.com", "publish-leak-alice")
	_, bobID := signup(t, w.ts.URL, "publish-leak-bob@example.com", "publish-leak-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	assetID := assetRowID(t, ctx, w.pool, f.pubAssetPID)
	privatePin := mustPin(t, f.privateAssetPID, "0.1")
	publicPin := mustPin(t, f.publicAssetPID, "1.0")

	cases := []struct {
		name     string
		opts     candidateOptions
		wantKind string
		wantRef  string
	}{
		{
			name:     "a pin to a still-private version",
			opts:     candidateOptions{pins: []assets.DependencyPin{privatePin}},
			wantKind: assets.DependencyAssetVersion,
			wantRef:  f.privatePin,
		},
		{
			name:     "an origin ref into another project's private state",
			opts:     candidateOptions{refs: []string{"release:" + f.releaseB}},
			wantKind: "release",
			wantRef:  "release:" + f.releaseB,
		},
		{
			name:     "a restricted blob under a promise of open data",
			opts:     candidateOptions{blobIDs: []string{f.restrictedBlob}, dataAccess: rights.DataAccessOpen},
			wantKind: assets.DependencyBlob,
			wantRef:  f.restrictedBlob,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.pid = f.pubAssetPID
			opts.version = fmt.Sprintf("1.%d", i)
			opts.visibility = "public"
			opts.creators = []string{aliceID}
			opts.blobIDs = append(opts.blobIDs, f.openBlob)
			if opts.refs == nil {
				opts.refs = []string{"release:" + f.releaseA}
			}
			if opts.accessLevel == "" {
				opts.accessLevel = "open"
			}
			if opts.dataAccess == "" {
				opts.dataAccess = rights.DataAccessOpen
			}
			cand := buildCandidate(t, opts)

			refusal := mustRefuse(t, alice, f.projectA, cand.body(t), "")

			// The report is COMPLETE: it is the preview document, computed
			// over the state the publish saw, not a sentence. A caller
			// receives the same answer the preview route gives, which is
			// what lets it act on the entry that blocked it.
			p := refusal.Preview
			if p.ProjectID != f.projectA || p.Version != opts.version || p.TargetVisibility != "public" {
				t.Errorf("report = project %q version %q target %q, want the refused publication",
					p.ProjectID, p.Version, p.TargetVisibility)
			}
			if p.Asset.PID != f.pubAssetPID || !p.Asset.Resolved {
				t.Errorf("report asset = %+v, want the fixture's asset resolved", p.Asset)
			}
			if !p.Facts.SourcePinned || !p.Facts.VersionPinned || !p.Facts.Contributors ||
				!p.Facts.Rights || !p.Facts.Visibility || !p.Facts.DependencyPins ||
				!p.Facts.IntegrityHash || !p.Facts.Metadata {
				t.Errorf("report facts = %+v, want every publish checklist fact true (the refusal is about the dependency)", p.Facts)
			}
			if p.Publishable {
				t.Errorf("report publishable = true over a private dependency")
			}
			if len(p.PublishBlockers) != 0 || len(p.RightsBlockers) != 0 {
				t.Errorf("report blockers = %v / %v, want none: the candidate itself is publishable", p.PublishBlockers, p.RightsBlockers)
			}
			dep := privateDep(t, p.PrivateDeps, tc.wantRef)
			if dep.Kind != tc.wantKind || !dep.Blocking {
				t.Errorf("the private dependency %s = %+v, want kind %s and blocking", tc.wantRef, dep, tc.wantKind)
			}
			if dep.Detail == "" {
				t.Errorf("the private dependency %s carries no detail: %+v", tc.wantRef, dep)
			}

			// Nothing was written. A refusal that left a version behind
			// would be a publication the caller was told did not happen.
			if n := countRows(t, ctx, w.pool,
				`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetID); n != 0 {
				t.Errorf("%d versions were written by a refused publish, want 0", n)
			}
			if n := countRows(t, ctx, w.pool, `SELECT count(*) FROM asset_publish_creations`); n != 0 {
				t.Errorf("%d ledger rows were written by a refused publish, want 0", n)
			}
			if n := countRows(t, ctx, w.pool, `SELECT count(*) FROM audit_log`); n != 0 {
				t.Errorf("%d audit rows were written by a refused publish, want 0", n)
			}
		})
	}

	// Control 1: the SAME dependencies published PRIVATE are accepted —
	// docs/23 §4 is about private→public, and a private publication widens
	// nothing. The dependencies are still NAMED; they just do not block.
	privateBody := buildCandidate(t, candidateOptions{
		pid: f.pubAssetPID, version: "2.0", visibility: "private",
		refs: []string{"release:" + f.releaseA}, pins: []assets.DependencyPin{privatePin},
		creators: []string{aliceID}, blobIDs: []string{f.openBlob, f.restrictedBlob},
		accessLevel: "restricted", dataAccess: rights.DataAccessOpen,
	}).body(t)
	got := mustPublish(t, alice, f.projectA, privateBody, "")
	if got.Visibility != "private" || got.Version != "2.0" {
		t.Errorf("the private publication stored %s@%s, want 2.0@private", got.Version, got.Visibility)
	}

	// Control 2: the same shape with every dependency public is accepted
	// public — the refusal above is about what is private, not about
	// dependencies existing at all.
	cleanBody := buildCandidate(t, candidateOptions{
		pid: f.pubAssetPID, version: "2.1", visibility: "public",
		refs: []string{"release:" + f.releaseA}, pins: []assets.DependencyPin{publicPin},
		creators: []string{aliceID}, blobIDs: []string{f.openBlob},
		accessLevel: "open", dataAccess: rights.DataAccessOpen,
	}).body(t)
	got = mustPublish(t, alice, f.projectA, cleanBody, "")
	if got.Visibility != "public" || got.Version != "2.1" {
		t.Errorf("the public publication with public dependencies stored %s@%s, want 2.1@public", got.Version, got.Visibility)
	}
}

// TestAssetPublishReRunsThePreviewOverCurrentState is the differential
// case: the publish runs the impact preview itself, inside its own
// transaction, over the rows as they are NOW — not over what the caller
// previewed earlier, and not over what the caller claims.
//
// The state is moved between the two requests, and the state that moves is
// the FOREIGN project's visibility, which is the one column this rule reads
// that is mutable at all (an asset version is append-only). The same body
// is accepted while the foreign project is public and refused once it is
// private, which no cached or caller-supplied preview could do.
func TestAssetPublishReRunsThePreviewOverCurrentState(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-rerun-alice@example.com", "publish-rerun-alice")
	_, bobID := signup(t, w.ts.URL, "publish-rerun-bob@example.com", "publish-rerun-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	body := func(version string) string {
		return buildCandidate(t, candidateOptions{
			pid: f.pubAssetPID, version: version, visibility: "public",
			refs: []string{"release:" + f.releaseB}, creators: []string{aliceID},
			blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		}).body(t)
	}

	// While the foreign project is public, its release rides along.
	f.makeForeignProjectPublic(t, ctx, w.pool)
	got := mustPublish(t, alice, f.projectA, body("3.0"), "")
	if got.Version != "3.0" {
		t.Fatalf("publish over a public foreign release stored version %q, want 3.0", got.Version)
	}

	// The state moves: the same project becomes private. The same body is
	// now a private dependency, and the publish refuses it.
	if _, err := w.pool.Exec(ctx, `UPDATE projects SET visibility = 'private' WHERE id = $1`, f.projectB); err != nil {
		t.Fatalf("make the foreign project private again: %v", err)
	}
	refusal := mustRefuse(t, alice, f.projectA, body("3.1"), "")
	dep := privateDep(t, refusal.Preview.PrivateDeps, "release:"+f.releaseB)
	if !dep.Blocking {
		t.Errorf("the foreign release = %+v, want blocking once its project is private", dep)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetRowID(t, ctx, w.pool, f.pubAssetPID)); n != 1 {
		t.Errorf("%d versions for the asset, want 1 (only the accepted publish wrote)", n)
	}
}

// --------------------------------------------------------------------------
// 4. A published version is immutable

// TestPublishedAssetVersionIsImmutable proves immutability three ways:
// the publish path refuses a repeat of the same (asset, version) with a
// code of its own, the stored row is unchanged by that refusal, and the
// DATABASE refuses an edit of the row — the 00014 append-only trigger,
// with a control showing the same statement succeeds on a table that is
// mutable by design, so the refusal is the guard rather than a broken
// connection.
func TestPublishedAssetVersionIsImmutable(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-imm-alice@example.com", "publish-imm-alice")
	_, bobID := signup(t, w.ts.URL, "publish-imm-bob@example.com", "publish-imm-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	assetID := assetRowID(t, ctx, w.pool, f.pubAssetPID)
	body := cleanCandidate(t, f, aliceID, "1.0", "public").body(t)
	got := mustPublish(t, alice, f.projectA, body, "")
	before := versionFingerprint(t, ctx, w.pool, assetID, "1.0")
	versionID := versionRowID(t, ctx, w.pool, assetID, "1.0")

	// A repeat with no key, and a repeat under a DIFFERENT key: both are
	// the same refusal, because a published version is not re-publishable
	// by anyone — the key only ever answered for ITS OWN publication, which
	// the ledger check above resolves first.
	mustRefuseWithCode(t, alice, f.projectA, body, "", http.StatusConflict, assetpublish.CodeAssetVersionImmutable)
	mustRefuseWithCode(t, alice, f.projectA, body, "publish-imm-key", http.StatusConflict, assetpublish.CodeAssetVersionImmutable)

	if after := versionFingerprint(t, ctx, w.pool, assetID, "1.0"); after != before {
		t.Errorf("the stored version changed across a refused republication:\nbefore: %s\nafter:  %s", before, after)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, assetID); n != 1 {
		t.Errorf("the asset carries %d versions, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM audit_log WHERE target_ref = $1`, "asset_version:"+versionID); n != 1 {
		t.Errorf("the audit log holds %d rows for the version, want 1 (a refused republication appends nothing)", n)
	}

	// The row cannot be edited, by this test or by anything else: the
	// trigger migration 00014 installs on research_asset_versions.
	_, err := w.pool.Exec(ctx,
		`UPDATE research_asset_versions SET visibility = 'private' WHERE id = $1`, versionID)
	wantAppendOnlyErr(t, "UPDATE research_asset_versions", "research_asset_versions", "UPDATE", err)
	_, err = w.pool.Exec(ctx,
		`DELETE FROM research_asset_versions WHERE id = $1`, versionID)
	wantAppendOnlyErr(t, "DELETE FROM research_asset_versions", "research_asset_versions", "DELETE", err)

	// The control: the very same statement against the MUTABLE table the
	// asset row lives in succeeds. Without it, "the UPDATE was refused"
	// would also be explained by a statement that was wrong for another
	// reason.
	if _, err := w.pool.Exec(ctx,
		`UPDATE research_assets SET title = title WHERE id = $1`, assetID); err != nil {
		t.Errorf("the control UPDATE on the mutable research_assets row was refused: %v", err)
	}

	// Immutability is about the VERSION, not about the asset: the next
	// version of the same asset is publishable.
	got = mustPublish(t, alice, f.projectA, cleanCandidate(t, f, aliceID, "1.1", "public").body(t), "")
	if got.Version != "1.1" {
		t.Errorf("the next version of the same asset published as %q, want 1.1", got.Version)
	}
}

// --------------------------------------------------------------------------
// 5. The denial precedes the target lookup

// TestAssetPublishDenialPrecedesTheTargetLookup pins the order: the actor
// is resolved against the matrix BEFORE anything is read, so a project id
// that names nothing is answered as a permission refusal — never as
// "project not found". The two controls are what make it a statement about
// the ORDER: the same body against the caller's own project succeeds, and
// an authenticated non-member receives exactly the same answer for a
// project that DOES exist, which is the property the order buys (no
// existence oracle).
func TestAssetPublishDenialPrecedesTheTargetLookup(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-order-alice@example.com", "publish-order-alice")
	bob, bobID := signup(t, w.ts.URL, "publish-order-bob@example.com", "publish-order-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	body := cleanCandidate(t, f, aliceID, "1.0", "public").body(t)

	// A well-formed uuid that names nothing, and a string that is not a
	// uuid at all — neither can be resolved, so neither may be disclosed.
	for _, unknown := range []string{"00000000-0000-0000-0000-000000000000", "no-such-project"} {
		raw := mustRefuseWithCode(t, alice, unknown, body, "", http.StatusForbidden,
			assetpublish.CodePublishPrivateToPublicRequiresApproval)
		lower := strings.ToLower(raw)
		for _, leak := range []string{"not found", "no such", "does not exist", "unknown project"} {
			if strings.Contains(lower, leak) {
				t.Errorf("the denial for project %q discloses the target (%q in the body): %s", unknown, leak, raw)
			}
		}
	}

	// The second control: the same body against a project that DOES exist
	// and that the caller is not a member of — bob publishing in alice's
	// project, where the fixture seeds no membership for him. Same status,
	// same code as the two unknown ids above: an authenticated non-member
	// cannot tell an existing project from a nonexistent one through this
	// route, which is the property resolving the actor first buys.
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM project_memberships WHERE project_id = $1::uuid AND user_id = $2::uuid`,
		f.projectA, bobID); n != 0 {
		t.Fatalf("the fixture made bob a member of the publishing project: %d memberships", n)
	}
	existing := mustRefuseWithCode(t, bob, f.projectA, body, "", http.StatusForbidden,
		assetpublish.CodePublishPrivateToPublicRequiresApproval)
	if existing == "" {
		t.Error("the non-member's denial carries no body at all")
	}

	// The first control: the same body, the same session, the project the
	// caller owns — accepted. The refusals above are about the actor's
	// standing in THAT project, not about the body.
	got := mustPublish(t, alice, f.projectA, body, "")
	if got.AssetPID != f.pubAssetPID || got.Version != "1.0" {
		t.Errorf("the owner's publish stored %s@%s, want %s@1.0", got.AssetPID, got.Version, f.pubAssetPID)
	}
	_ = existing
}

// --------------------------------------------------------------------------
// The publication itself

// TestAssetPublishStoreReplaysUnderItsOwnLock reaches the store DIRECTLY,
// with the command out of the picture, and publishes the same key twice.
//
// The task's sixth criterion is asserted over the route above, and the route
// answers a replay from TWO places: the command's LookupCreation, which runs
// outside any transaction, and the store's re-read of the ledger inside the
// publish transaction under the project lock. The first is an optimization
// and the second is the guarantee (two concurrent publishes with one key
// serialize on that lock and the loser must replay rather than insert), so a
// sequential test through the command observes the first and would stay
// green if the second regressed. This case is the one that observes the
// second: it calls the store twice itself, so the store's own ledger read is
// the only thing that can answer the second call.
func TestAssetPublishStoreReplaysUnderItsOwnLock(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	_, aliceID := signup(t, w.ts.URL, "publish-store-replay@example.com", "publish-store-replay")
	_, bobID := signup(t, w.ts.URL, "publish-store-replay-bob@example.com", "publish-store-replay-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	cand := cleanCandidate(t, f, aliceID, "1.0", "public")
	key := "store-replay-key"
	req := assetpublish.PublishRequest{
		ProjectID:      f.projectA,
		Candidate:      cand.cand,
		Actor:          assetpublish.Actor{User: domain.User{ID: aliceID}},
		Audit:          domain.AuditEntry{ActorID: aliceID, Via: domain.ViaSession, Action: assetpublish.ActionAssetVersionPublished, ProjectID: f.projectA},
		IdempotencyKey: &key,
	}

	first, err := w.store.Publish(ctx, req)
	if err != nil {
		t.Fatalf("the first publish: %v", err)
	}
	second, err := w.store.Publish(ctx, req)
	if err != nil {
		t.Fatalf("the second publish under the same key = %v, want the first's version replayed", err)
	}
	if second.ID != first.ID || second.AssetPID != first.AssetPID || second.Version != first.Version ||
		second.PublishedAt != first.PublishedAt {
		t.Errorf("the store's second publish answered a different publication:\n first: %+v\nsecond: %+v", first, second)
	}

	assetID := assetRowID(t, ctx, w.pool, f.pubAssetPID)
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1::uuid`, assetID); n != 1 {
		t.Errorf("the asset carries %d versions after two publishes under one key, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM asset_publish_creations WHERE project_id = $1::uuid AND idempotency_key = $2`,
		f.projectA, key); n != 1 {
		t.Errorf("the ledger holds %d entries for the key, want 1", n)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM audit_log WHERE target_ref = $1`, "asset_version:"+first.ID); n != 1 {
		t.Errorf("the audit log holds %d rows for the version, want 1: the replay must not append a second one", n)
	}

	// The store's lookup answers for the key as well, which is what the
	// command's replay path reads — asserted here so the two ports are
	// proven to agree about the same ledger row rather than each being
	// self-consistent.
	replayed, err := w.store.LookupCreation(ctx, f.projectA, key)
	if err != nil {
		t.Fatalf("LookupCreation: %v", err)
	}
	if replayed == nil || replayed.ID != first.ID {
		t.Errorf("LookupCreation = %+v, want the version the first publish wrote", replayed)
	}
	if other, err := w.store.LookupCreation(ctx, f.projectA, "no-such-key"); err != nil || other != nil {
		t.Errorf("LookupCreation for an unused key = (%+v, %v), want (nil, nil)", other, err)
	}
}

// TestAssetPublishWritesTheWholePublication reads back what the 201
// claims, over real rows:
//
//   - a publication that CREATES the asset mints the pid in the
//     application (assets.NewPID, never the column DEFAULT) and stores the
//     display fields the request gave;
//   - the origin_refs are PERSISTED (issue #225): the version's provenance
//     is a stored fact, not only a validated one;
//   - the STORED manifest bytes still hash to the row's integrity_hash
//     (T0702's residual risk 2): the reader verifies what it read, not
//     what the writer said;
//   - the version row, the ledger entry, the audit row, the research event
//     and its outbox row were written in ONE transaction — one correlation
//     id names all of them.
func TestAssetPublishWritesTheWholePublication(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetPublishWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "publish-full-alice@example.com", "publish-full-alice")
	_, bobID := signup(t, w.ts.URL, "publish-full-bob@example.com", "publish-full-bob")
	f := seedAssetPreviewFixture(t, ctx, w.pool, aliceID, bobID)

	// The publication creates the asset: no asset_pid, and the display
	// fields a create requires.
	cand := buildCandidate(t, candidateOptions{
		version: "1.0", visibility: "public",
		refs: []string{"release:" + f.releaseA}, creators: []string{aliceID},
		blobIDs: []string{f.openBlob}, accessLevel: "open", dataAccess: rights.DataAccessOpen,
		title: "A newly published dataset", slug: "newly-published-dataset",
	})
	const key = "publish-full-key"
	got := mustPublish(t, alice, f.projectA, cand.body(t), key)

	if !assets.ValidPID(got.AssetPID) {
		t.Errorf("the created asset's pid = %q, want a minted persistent identifier", got.AssetPID)
	}
	if got.AssetPID == f.pubAssetPID {
		t.Errorf("the created asset reused the fixture's pid %q", got.AssetPID)
	}
	if got.PublishedBy != aliceID {
		t.Errorf("published_by = %q, want the publishing user %q", got.PublishedBy, aliceID)
	}
	if got.PublishedAt == "" {
		t.Error("published_at is empty")
	}
	if got.Visibility != "public" || got.Version != "1.0" {
		t.Errorf("the response = %s@%s, want 1.0@public", got.Version, got.Visibility)
	}

	// The asset row: the pid the application minted, the display fields the
	// request gave, in the publishing project.
	var (
		slug, title, assetType, originProject string
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT slug, title, asset_type, origin_project_id::text FROM research_assets WHERE pid = $1`,
		got.AssetPID).Scan(&slug, &title, &assetType, &originProject); err != nil {
		t.Fatalf("read the created asset: %v", err)
	}
	if slug != "newly-published-dataset" || title != "A newly published dataset" {
		t.Errorf("the created asset = %q / %q, want the request's display fields", slug, title)
	}
	if assetType != string(assets.TypeDataset) || originProject != f.projectA {
		t.Errorf("the created asset = type %q project %q, want a dataset in the publishing project", assetType, originProject)
	}

	assetID := assetRowID(t, ctx, w.pool, got.AssetPID)
	versionID := versionRowID(t, ctx, w.pool, assetID, "1.0")

	// origin_refs are stored (issue #225).
	var storedRefs []string
	if err := w.pool.QueryRow(ctx,
		`SELECT origin_refs FROM research_asset_versions WHERE id = $1`, versionID).Scan(&storedRefs); err != nil {
		t.Fatalf("read the stored origin refs: %v", err)
	}
	if len(storedRefs) != 1 || storedRefs[0] != "release:"+f.releaseA {
		t.Errorf("the stored origin_refs = %v, want the ref the publish carried", storedRefs)
	}

	// The stored manifest still verifies against the row's hash.
	var (
		storedManifest, storedHash, storedVisibility string
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT manifest::text, integrity_hash, visibility FROM research_asset_versions
		  WHERE id = $1`, versionID).Scan(&storedManifest, &storedHash, &storedVisibility); err != nil {
		t.Fatalf("read the stored version: %v", err)
	}
	readBack, err := assets.ManifestHash([]byte(storedManifest))
	if err != nil {
		t.Fatalf("ManifestHash(the stored bytes): %v", err)
	}
	if readBack != storedHash {
		t.Errorf("the stored manifest hashes to %s, the row claims %s", readBack, storedHash)
	}
	if storedHash != got.IntegrityHash {
		t.Errorf("the row stored hash %s while the response claimed %s", storedHash, got.IntegrityHash)
	}
	if storedVisibility != "public" {
		t.Errorf("the stored visibility = %q, want public", storedVisibility)
	}

	// The ledger, the audit row and the events: one transaction, one
	// correlation id.
	var ledgerVersion string
	if err := w.pool.QueryRow(ctx,
		`SELECT asset_version_id::text FROM asset_publish_creations
		  WHERE project_id = $1::uuid AND idempotency_key = $2`, f.projectA, key).Scan(&ledgerVersion); err != nil {
		t.Fatalf("read the ledger entry: %v", err)
	}
	if ledgerVersion != versionID {
		t.Errorf("the ledger entry names version %s, want %s", ledgerVersion, versionID)
	}

	var (
		auditAction, auditVia, auditTarget, auditActor, auditProject, auditCorrelation string
		auditAfter                                                                     []byte
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT action, via, target_ref, actor_id::text, project_id::text, correlation_id, after_summary
		   FROM audit_log WHERE target_ref = $1`, "asset_version:"+versionID).
		Scan(&auditAction, &auditVia, &auditTarget, &auditActor, &auditProject, &auditCorrelation, &auditAfter); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if auditAction != assetpublish.ActionAssetVersionPublished {
		t.Errorf("audit action = %q, want %q", auditAction, assetpublish.ActionAssetVersionPublished)
	}
	if auditVia != string(domain.ViaSession) {
		t.Errorf("audit via = %q, want %q", auditVia, domain.ViaSession)
	}
	if auditActor != aliceID || auditProject != f.projectA {
		t.Errorf("audit actor/project = %s/%s, want %s/%s", auditActor, auditProject, aliceID, f.projectA)
	}
	// The summary carries the publication's own facts — the pid, the
	// label, the visibility, the hash and the provenance — so an auditor
	// reading only the audit log can say what was published.
	var summary map[string]any
	if err := json.Unmarshal(auditAfter, &summary); err != nil {
		t.Fatalf("audit after_summary: %v: %s", err, auditAfter)
	}
	if summary["asset_id"] != got.AssetPID || summary["version"] != "1.0" ||
		summary["visibility"] != "public" || summary["integrity_hash"] != got.IntegrityHash {
		t.Errorf("audit after_summary = %v, want the publication's facts", summary)
	}

	var (
		eventType, eventVisibility, eventCorrelation string
		eventPayload                                 []byte
	)
	if err := w.pool.QueryRow(ctx,
		`SELECT event_type, visibility, correlation_id, payload FROM research_events
		  WHERE event_type = $1`, assetVersionEventType).
		Scan(&eventType, &eventVisibility, &eventCorrelation, &eventPayload); err != nil {
		t.Fatalf("read the research event: %v", err)
	}
	if eventCorrelation != auditCorrelation {
		t.Errorf("the research event carries correlation %q, the audit row %q: they must commit together",
			eventCorrelation, auditCorrelation)
	}
	if eventVisibility != "private" {
		t.Errorf("the event's visibility = %q, want the project's visibility at publish time", eventVisibility)
	}
	var payload map[string]any
	if err := json.Unmarshal(eventPayload, &payload); err != nil {
		t.Fatalf("event payload: %v: %s", err, eventPayload)
	}
	if payload["asset_id"] != got.AssetPID || payload["version"] != "1.0" ||
		payload["integrity_hash"] != got.IntegrityHash || payload["asset_version_id"] != versionID {
		t.Errorf("the event payload = %v, want the published version", payload)
	}

	var outboxType, outboxCorrelation string
	if err := w.pool.QueryRow(ctx,
		`SELECT event_type, correlation_id FROM outbox_events WHERE event_type = $1`,
		assetVersionEventType).Scan(&outboxType, &outboxCorrelation); err != nil {
		t.Fatalf("read the outbox row: %v", err)
	}
	if outboxCorrelation != auditCorrelation {
		t.Errorf("the outbox row carries correlation %q, the audit row %q: one transaction, one trace",
			outboxCorrelation, auditCorrelation)
	}
}
