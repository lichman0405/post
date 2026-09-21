// Task T0708 required test "asset lineage" — the end-to-end half.
//
// The unit suite pins the command's decisions without a database
// (internal/assets/derive_test.go: the relation vocabulary, the three-valued
// rights verdict, the parent read ruler's agreement with the page's own, the
// request shape, the authorization step, the replay rules). This file pins
// what only real PostgreSQL and the whole composed path can settle — the
// eight acceptance criteria of the task, each over real rows:
//
//  1. The LINEAGE EDGE is stored, and it pins the PARENT VERSION: the HTTP
//     request creates one asset_lineage row whose parent end is the parent
//     version row's id, whose child end is the new version row's id, and
//     whose relation is the one the request asked for. The parent end is
//     asserted against the row id, never against the parent's pid or slug: a
//     pid names an ASSET, which accumulates versions, so an edge keyed on it
//     could not answer "derived from WHAT" a version later.
//
//  2. The parent version is UNCHANGED, byte-for-byte per column, and the
//     path issues no UPDATE and no DELETE against it. The fingerprint is one
//     string over every column the row has; the append-only trigger is
//     asserted beside it, so "the row did not change" is not confused with
//     "the row could not have changed".
//
//  3. The parent's stored rights declaration decides, and its THREE values
//     are three different answers: restricted → refused; allowed → performed;
//     unspecified → performed ONLY against an explicit confirmation, which
//     is readable from the audit row the same transaction wrote. An
//     UNREADABLE declaration ({} — the shape the pre-T0705 seed rows carry)
//     is refused as unreadable rather than read as silence. Every refusal
//     writes nothing: not an asset, not a version, not an edge, not an audit
//     row, not a ledger entry.
//
//  4. A caller who may not READ a version may not DERIVE from it, and is
//     answered indistinguishably from a version that does not exist
//     (ADR-024). The two responses are compared byte-for-byte apart from the
//     request id, and the control — the version's own project's member —
//     shows the refusal is about the reader.
//
//  5. The new identity is genuinely new: a different pid, a new asset row in
//     the TARGET project, a version row of its own. The derivation never
//     adds a version to the parent's asset.
//
//  5b. The GOVERNANCE rule that blocks a publish blocks a derivation. A
//     derivation writes a new public asset version, so it is the act
//     domain.RulePublicAssetIPReview governs; a target project whose policy
//     in force sets the rule refuses the derivation with the publish's own
//     wire code and writes nothing, and the same request succeeds before the
//     rule is set. This is criterion 5 applied to the one thing the derive
//     path could otherwise route around.
//
//  6. The asset page renders the edge from BOTH ends: the parent's page
//     shows the child, and the child's page shows the parent, through
//     GET /api/v1/assets/{pid}.
//
//  7. Idempotency is a COUNT, not an existence: one key replayed yields one
//     asset, one version, one edge, one ledger entry and one audit row, and
//     two CONCURRENT requests carrying one key apply once.
//
//  8. The refusal for a PROHIBITED derivation is distinguishable from the one
//     for an UNSPECIFIED parent, which is distinguishable from an UNREADABLE
//     document: three codes, because they ask a client for three different
//     things (stop / confirm / nobody can read this).
//
// The composition is cmd/api/main.go's: the real auth guard, the real derive
// command over the real store, the real permission matrix over the canonical
// CSV, the real asset page reader. Only the session store is in-memory,
// exactly as the publish and page e2e suites compose it.

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// assetLineageTaskID namespaces this task's test databases
// (test_T0708_<run_id>).
const assetLineageTaskID = "T0708"

// deriveURL is the route this task mounts (cmd/api/assetshttp/derive.go):
// one custom method under the project's assets, the publish's own shape.
func deriveURL(projectID string) string {
	return "/api/v1/projects/" + projectID + "/assets:derive"
}

// --------------------------------------------------------------------------
// The composed surface

// assetLineageWorld is the fork/derive path as production wires it.
type assetLineageWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
	// derive is the SAME command the HTTP route drives. The agent case and
	// the matrix walk have no HTTP expression in V1 (no agent token exists,
	// and the transport sets IsAgent false in one place), so they are driven
	// through the command over the real store — the state of the build,
	// asserted where it is decidable.
	derive *assets.DeriveCommand
}

// newAssetLineageWorld composes the production tree over one test database,
// exactly as cmd/api/main.go does.
func newAssetLineageWorld(t *testing.T, ctx context.Context) *assetLineageWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetLineageTaskID)

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
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	// The membership adapter is ONE value handed to both the command and the
	// store, the way main.go hands it: the authorization outside the
	// transaction and the parent read gate inside it ask the same question,
	// and there is one implementation of "is this caller a member".
	deriveCommand := assets.NewDeriveCommand(assets.DeriveDeps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetDeriveStore(pool, rsgvalidation.NewValidator(reg), projectStore),
		Authz:    authz.NewMatrixEngine(),
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(mux)
	assetshttp.New(assetshttp.Deps{
		State:        assetshttp.NewPostgresStateStore(pool),
		Projects:     projectAPI.Service(),
		Publish:      publishCommand,
		Pages:        persistence.NewAssetPageStore(pool),
		Members:      projectAPI.Service(),
		Dependencies: persistence.NewProjectDependencyStore(pool),
		Derive:       deriveCommand,
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &assetLineageWorld{ts: ts, pool: pool, derive: deriveCommand}
}

// --------------------------------------------------------------------------
// The wire shape this suite reads (declared here rather than reused from
// cmd/api/assetshttp, so a silent JSON tag change in the payload fails here)

type derivedAssetWire struct {
	AssetPID       string          `json:"asset_pid"`
	Version        string          `json:"version"`
	Visibility     string          `json:"visibility"`
	IntegrityHash  string          `json:"integrity_hash"`
	OriginRefs     []string        `json:"origin_refs"`
	PublishedBy    string          `json:"published_by"`
	PublishedAt    string          `json:"published_at"`
	Manifest       json.RawMessage `json:"manifest"`
	Rights         json.RawMessage `json:"rights"`
	ParentAssetPID string          `json:"parent_asset_pid"`
	ParentVersion  string          `json:"parent_version"`
	Relation       string          `json:"relation"`
}

// deriveErrorWire is the refusal envelope (authhttp.errorEnvelope).
type deriveErrorWire struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Retryable bool   `json:"retryable"`
}

// --------------------------------------------------------------------------
// Requests

// deriveOnce sends one derivation with an Idempotency-Key and returns the
// status and the raw body WITHOUT touching *testing.T — the concurrent case
// below drives it from goroutines, where only the collecting goroutine may
// fail the test.
func deriveOnce(client *http.Client, server, csrf, projectID, body, key string) (int, string, error) {
	req, err := http.NewRequest(http.MethodPost, server+deriveURL(projectID), strings.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, "", err
	}
	return resp.StatusCode, string(raw), nil
}

// derive sends one derivation; key == "" sends no Idempotency-Key.
func derive(t *testing.T, uc *testUserClient, projectID, body, key string) *http.Response {
	t.Helper()
	return uc.doKeyed(t, http.MethodPost, deriveURL(projectID), body, key)
}

// mustDerive requires the 201 and returns the stored derivation as the wire
// renders it.
func mustDerive(t *testing.T, uc *testUserClient, projectID, body, key string) derivedAssetWire {
	t.Helper()
	resp := derive(t, uc, projectID, body, key)
	raw := readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("derive = %d, want 201: %s", resp.StatusCode, raw)
	}
	var got derivedAssetWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("derived version payload: %v: %s", err, raw)
	}
	return got
}

// mustRefuseDerive requires one status + code and returns the refusal body
// verbatim (the disclosure assertions compare raw bytes).
func mustRefuseDerive(t *testing.T, uc *testUserClient, projectID, body, key string, status int, code string) string {
	t.Helper()
	resp := derive(t, uc, projectID, body, key)
	raw := readAll(t, resp)
	if resp.StatusCode != status {
		t.Fatalf("derive = %d, want %d: %s", resp.StatusCode, status, raw)
	}
	var env deriveErrorWire
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("error envelope: %v: %s", err, raw)
	}
	if env.Code != code {
		t.Fatalf("code = %q, want %q: %s", env.Code, code, raw)
	}
	return raw
}

// --------------------------------------------------------------------------
// The fixture

// lineageVersion is one seeded published version: its asset's pid, its label,
// and the two row ids every assertion about an edge is keyed by.
type lineageVersion struct {
	projectID  string
	pid        string
	version    string
	assetID    string
	versionRow string
	visibility string
}

// lineageAsset seeds one research asset (and, always, one version of it) with
// a REAL rights declaration, and returns the identities. The declaration is
// the point of most cases below: the derivation reads the STORED bytes, so a
// fixture that wrote a summary of them would be testing something else.
func lineageAsset(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, userID,
	slug, title, pid, version, visibility string, rightsJSON json.RawMessage) lineageVersion {
	t.Helper()
	assetID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id, pid)
		 VALUES ('dataset', $1, $2, $3, $4) RETURNING id`, slug, title, projectID, pid)
	manifest := lineageManifest(t)
	versionRow := mustQueryUUID(t, ctx, pool,
		`INSERT INTO research_asset_versions
			(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, $2, $3::jsonb, $4::jsonb, $5, 'seed-hash', $6, ARRAY['project:' || $7::text])
		 RETURNING id`,
		assetID, version, string(manifest), string(rightsJSON), visibility, userID, projectID)
	return lineageVersion{
		projectID: projectID, pid: pid, version: version,
		assetID: assetID, versionRow: versionRow, visibility: visibility,
	}
}

// lineageRightsDocument renders one rights declaration with the named
// derivatives value, through the package that owns the format.
func lineageRightsDocument(t *testing.T, derivatives rights.Permission) json.RawMessage {
	t.Helper()
	doc := rights.New()
	doc.Usage.Derivatives = derivatives
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal the fixture rights document: %v", err)
	}
	return raw
}

// lineageManifest is a valid dataset manifest with no blobs and no pins — the
// smallest document the publish gate accepts, rendered through the publish
// suite's own candidate builder so this package keeps ONE definition of "a
// valid dataset manifest".
func lineageManifest(t *testing.T) json.RawMessage {
	t.Helper()
	c := buildCandidate(t, candidateOptions{
		version:     "1.0",
		visibility:  "public",
		accessLevel: "open",
		dataAccess:  rights.DataAccessRestricted,
	})
	return c.cand.Manifest
}

// lineageFixture is the repository the derivation runs against: a public
// project alice owns (the derivation's TARGET), a public project alice owns
// that holds the parents, and a PRIVATE project bob owns that holds one
// private version (the read gate's other side).
type lineageFixture struct {
	aliceID string
	bobID   string
	// childProject is where the new identity lands, and where alice may
	// derive (owner). Its release is the origin pin the child candidate
	// carries, because the gate requires an accepted state or release, and
	// childBlob is the openly attached blob its manifest names.
	childProject string
	childRelease string
	childBlob    string
	// parentProject holds the public parents, one per rights declaration.
	parentProject string
	// privateProject is bob's, and holds bobPrivate.
	privateProject string
	// The parents, one per case of the rights verdict.
	allowed     lineageVersion
	restricted  lineageVersion
	unspecified lineageVersion
	unreadable  lineageVersion
	// bobPrivate is a PRIVATE version in bob's private project: readable by
	// bob, and by nobody else (criterion 4).
	bobPrivate lineageVersion
}

// seedLineageFixture writes the rows with raw SQL on the writable pool: the
// fixture is the repository's state, and the route under test may only ever
// add to it.
func seedLineageFixture(t *testing.T, ctx context.Context, pool *pgxpool.Pool, aliceID, bobID string) *lineageFixture {
	t.Helper()
	f := &lineageFixture{aliceID: aliceID, bobID: bobID}

	// Two public projects (the target and the parents' home) and one private
	// one. Both public projects make alice an owner: the derivation's
	// authorization is resolved from the TARGET project's membership, and the
	// parent read gate from the PARENT's project.
	newProject := func(slug, name, visibility, owner string) string {
		t.Helper()
		id := mustQueryUUID(t, ctx, pool,
			`INSERT INTO projects (slug, name, purpose, visibility, created_by)
			 VALUES ($1, $2, 'T0708 fixture', $3, $4) RETURNING id`, slug, name, visibility, owner)
		if _, err := pool.Exec(ctx,
			`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`, id, owner); err != nil {
			t.Fatalf("seed membership for %s: %v", slug, err)
		}
		return id
	}
	f.childProject = newProject("lineage-child", "Derivation Target", "public", aliceID)
	f.parentProject = newProject("lineage-parent", "Parent Home", "public", aliceID)
	f.privateProject = newProject("lineage-private", "Bob Private", "private", bobID)

	// The child project's accepted state and release: the origin pin a
	// publishable candidate must carry (docs/11 §3).
	stateA := mustQueryUUID(t, ctx, pool,
		`INSERT INTO project_states (project_id, state_hash, manifest_version)
		 VALUES ($1, 'genesis-child', '1') RETURNING id`, f.childProject)
	f.childRelease = mustQueryUUID(t, ctx, pool,
		`INSERT INTO releases (project_id, version, title, state_id, manifest, manifest_hash, created_by)
		 VALUES ($1, 'v1.0.0', 'Target release', $2, '{}'::jsonb, 'x', $3) RETURNING id`,
		f.childProject, stateA, aliceID)

	// An openly attached blob in the target project. The child document must
	// name at least one blob (a dataset's required metadata), and the
	// preview reads whether the named blob resolves and how it is attached:
	// a fixture that named an unresolvable id would be a document whose own
	// data access statement the platform cannot check, which is a different
	// case from the one under test.
	blobObject := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, f.childProject, aliceID)
	blobObjectVersion := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1', 'Derived record', 'active',
		         '{}'::jsonb, 'x', $3) RETURNING id`, blobObject, stateA, aliceID)
	f.childBlob = mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ('sha256:lineage-open', 512, 'blobs/lineage-open', $1) RETURNING id`, aliceID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, f.childBlob, blobObjectVersion, stateA); err != nil {
		t.Fatalf("attach the fixture blob: %v", err)
	}

	// The four parents, each a public version of a public project, differing
	// only in the declaration the verdict reads.
	f.allowed = lineageAsset(t, ctx, pool, f.parentProject, aliceID,
		"parent-allowed", "Parent (derivatives allowed)", "01j9z6k3m4n5p6q7r8s9t0v1w5", "1.0", "public",
		lineageRightsDocument(t, rights.PermissionAllowed))
	f.restricted = lineageAsset(t, ctx, pool, f.parentProject, aliceID,
		"parent-restricted", "Parent (derivatives restricted)", "01j9z6k3m4n5p6q7r8s9t0v1w6", "1.0", "public",
		lineageRightsDocument(t, rights.PermissionRestricted))
	f.unspecified = lineageAsset(t, ctx, pool, f.parentProject, aliceID,
		"parent-unspecified", "Parent (derivatives unspecified)", "01j9z6k3m4n5p6q7r8s9t0v1w7", "1.0", "public",
		lineageRightsDocument(t, rights.PermissionUnspecified))
	// The unreadable one: the empty object, which is the shape the
	// pre-T0705 seed rows carry and which internal/rights deliberately gives
	// no meaning to (an unreadable policy is not a permissive one).
	f.unreadable = lineageAsset(t, ctx, pool, f.parentProject, aliceID,
		"parent-unreadable", "Parent (no declaration)", "01j9z6k3m4n5p6q7r8s9t0v1w8", "1.0", "public",
		json.RawMessage(`{}`))

	// Bob's private version, in bob's private project.
	f.bobPrivate = lineageAsset(t, ctx, pool, f.privateProject, bobID,
		"bob-private", "Bob's private dataset", "01j9z6k3m4n5p6q7r8s9t0v1w9", "0.1", "private",
		lineageRightsDocument(t, rights.PermissionAllowed))

	return f
}

// deriveBody renders the derivation request: the version document of the
// child (through the publish suite's candidate builder, so the document is
// one the gate accepts) plus the three fields that are the derivation's own.
func deriveBody(t *testing.T, f *lineageFixture, parent, relation, version, confirmation string) string {
	t.Helper()
	in := deriveParamsFor(t, f, parent, relation, version, confirmation)
	out := map[string]any{
		"parent":         in.Parent,
		"relation":       in.Relation,
		"asset_type":     in.AssetType,
		"version":        in.Version,
		"manifest":       json.RawMessage(in.Manifest),
		"rights":         json.RawMessage(in.Rights),
		"origin_refs":    in.OriginRefs,
		"visibility":     in.Visibility,
		"integrity_hash": in.IntegrityHash,
		"creator_ids":    in.CreatorIDs,
		"title":          in.Title,
		"slug":           in.Slug,
	}
	// The confirmation is sent only when a case names one: a body that
	// always carried it would hide whether the confirmation field is
	// required, and the absent case is one of the cases.
	if confirmation != "" {
		out["derivatives_confirmation"] = in.Confirmation
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal the derive body: %v", err)
	}
	return string(raw)
}

// deriveParamsFor is the same request as deriveBody, one layer in: the
// command's own input. The two are rendered from ONE candidate, so the wire
// case and the command-level case cannot drift apart.
func deriveParamsFor(t *testing.T, f *lineageFixture, parent, relation, version, confirmation string) assets.DeriveParams {
	t.Helper()
	c := buildCandidate(t, candidateOptions{
		version:     version,
		visibility:  "public",
		refs:        []string{"release:" + f.childRelease},
		creators:    []string{f.aliceID},
		blobIDs:     []string{f.childBlob},
		accessLevel: "open",
		dataAccess:  rights.DataAccessOpen,
		title:       "Derived dataset",
		slug:        "derived-dataset",
	})
	return assets.DeriveParams{
		Parent:        parent,
		Relation:      relation,
		AssetType:     string(c.cand.AssetType),
		Version:       c.cand.Version,
		Manifest:      c.cand.Manifest,
		Rights:        c.cand.RightsJSON,
		OriginRefs:    c.cand.OriginRefs,
		Visibility:    string(c.cand.Visibility),
		IntegrityHash: c.cand.IntegrityHash,
		CreatorIDs:    c.cand.CreatorIDs,
		Title:         c.title,
		Slug:          c.slug,
		Confirmation:  confirmation,
	}
}

// parentRef is the canonical pid@version reference to one seeded version.
func parentRef(v lineageVersion) string { return v.pid + "@" + v.version }

// --------------------------------------------------------------------------
// The rows, as SQL reports them

// lineageEdge is one asset_lineage row, as the derivation's assertions read
// it: both ends as VERSION ROW ids, and the relation as stored.
type lineageEdge struct {
	parentVersionID string
	childVersionID  string
	relation        string
}

// lineageEdges reads every stored edge, oldest first.
func lineageEdges(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []lineageEdge {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT parent_asset_version_id::text, child_asset_version_id::text, relation_type
		   FROM asset_lineage ORDER BY parent_asset_version_id, child_asset_version_id`)
	if err != nil {
		t.Fatalf("read asset_lineage: %v", err)
	}
	defer rows.Close()
	var out []lineageEdge
	for rows.Next() {
		var e lineageEdge
		if err := rows.Scan(&e.parentVersionID, &e.childVersionID, &e.relation); err != nil {
			t.Fatalf("scan asset_lineage: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read asset_lineage: %v", err)
	}
	return out
}

// deriveLedgerRow is one asset_derive_creations row.
type deriveLedgerRow struct {
	projectID       string
	idempotencyKey  string
	assetVersionID  string
	parentVersionID string
	relation        string
}

// deriveLedger reads every ledger entry, oldest first.
func deriveLedger(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []deriveLedgerRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT project_id::text, idempotency_key, asset_version_id::text,
		        parent_asset_version_id::text, relation_type
		   FROM asset_derive_creations ORDER BY created_at, idempotency_key`)
	if err != nil {
		t.Fatalf("read asset_derive_creations: %v", err)
	}
	defer rows.Close()
	var out []deriveLedgerRow
	for rows.Next() {
		var r deriveLedgerRow
		if err := rows.Scan(&r.projectID, &r.idempotencyKey, &r.assetVersionID, &r.parentVersionID, &r.relation); err != nil {
			t.Fatalf("scan asset_derive_creations: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read asset_derive_creations: %v", err)
	}
	return out
}

// derivedAudit is the audit row a derivation wrote, as the task reads it: the
// actor, the action, what it names, and the rights verdict the transaction
// decided on.
type derivedAudit struct {
	actorID   string
	action    string
	targetRef string
	projectID string
	declared  string
	confirm   string
	required  string
}

// deriveAudits reads the derivation's audit rows, oldest first.
func deriveAudits(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []derivedAudit {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT coalesce(actor_id::text, ''), action, coalesce(target_ref, ''), coalesce(project_id::text, ''),
		        coalesce(after_summary->>'derivatives_declared', ''),
		        coalesce(after_summary->>'derivatives_confirmation', ''),
		        coalesce(after_summary->>'derivatives_confirmation_required', '')
		   FROM audit_log WHERE action = $1 ORDER BY occurred_at, id`, assets.ActionAssetDerived)
	if err != nil {
		t.Fatalf("read the derivation audit rows: %v", err)
	}
	defer rows.Close()
	var out []derivedAudit
	for rows.Next() {
		var a derivedAudit
		if err := rows.Scan(&a.actorID, &a.action, &a.targetRef, &a.projectID, &a.declared, &a.confirm, &a.required); err != nil {
			t.Fatalf("scan the derivation audit row: %v", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the derivation audit rows: %v", err)
	}
	return out
}

// derivedRows counts everything a successful derivation writes, in ONE query
// so a case that expects "nothing" fails on the first table that is not
// empty rather than on whichever it happened to check first.
func derivedRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) map[string]int {
	t.Helper()
	return map[string]int{
		"assets": countRows(t, ctx, pool, `SELECT count(*) FROM research_assets WHERE origin_project_id = $1`, projectID),
		"versions": countRows(t, ctx, pool,
			`SELECT count(*) FROM research_asset_versions v JOIN research_assets a ON a.id = v.asset_id
			  WHERE a.origin_project_id = $1`, projectID),
		"edges":  countRows(t, ctx, pool, `SELECT count(*) FROM asset_lineage`),
		"ledger": countRows(t, ctx, pool, `SELECT count(*) FROM asset_derive_creations`),
		"audit":  countRows(t, ctx, pool, `SELECT count(*) FROM audit_log WHERE action = $1`, assets.ActionAssetDerived),
		"events": countRows(t, ctx, pool, `SELECT count(*) FROM research_events WHERE event_type = $1`, assetVersionEventType),
	}
}

// mustWriteNothing asserts that a refused derivation left no trace: no asset
// in the target project, no version, no edge, no ledger entry, no audit row,
// no research event. Counting all six is what makes "a refusal writes nothing"
// a statement about the database rather than about the response.
func mustWriteNothing(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, what string) {
	t.Helper()
	for name, n := range derivedRows(t, ctx, pool, projectID) {
		if n != 0 {
			t.Errorf("%s wrote %d %s rows, want 0 — a refusal must write nothing, the audit row included", what, n, name)
		}
	}
}

// versionFingerprintOf is versionFingerprint (asset_publish_test.go) for a
// version identified by its ROW id, which is what the parent's assertions
// hold. Every column the row has is in it, so "unchanged" is one comparison.
func versionFingerprintOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionRowID string) string {
	t.Helper()
	var (
		id, assetID, label, visibility, integrity, publishedBy, publishedAt, refs string
		manifest, rightsJSON                                                      string
	)
	if err := pool.QueryRow(ctx,
		`SELECT id::text, asset_id::text, version, visibility, integrity_hash, published_by::text,
		        published_at::text, array_to_string(origin_refs, ','),
		        manifest::text, rights_json::text
		   FROM research_asset_versions WHERE id = $1`, versionRowID).
		Scan(&id, &assetID, &label, &visibility, &integrity, &publishedBy, &publishedAt, &refs, &manifest, &rightsJSON); err != nil {
		t.Fatalf("read the stored version %s: %v", versionRowID, err)
	}
	return strings.Join([]string{id, assetID, label, visibility, integrity, publishedBy, publishedAt,
		refs, manifest, rightsJSON}, "\n")
}

// --------------------------------------------------------------------------
// 1 + 5. The edge, and the new identity

// TestAssetLineageStoresTheEdgeToTheParentVersion is the task's first
// criterion: one HTTP request creates a NEW asset identity and records ONE
// lineage edge, whose parent end is the parent VERSION's row id, whose child
// end is the new version's row id, and whose relation is what the request
// asked for.
func TestAssetLineageStoresTheEdgeToTheParentVersion(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-edge-alice@example.com", "lineage-edge-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-edge-bob@example.com", "lineage-edge-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// The parent's row, as it stands before the derivation: the comparison
	// below is against these bytes, not against a re-typed expectation.
	parentBefore := versionFingerprintOf(t, ctx, w.pool, f.allowed.versionRow)

	got := mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")

	// (5) A genuinely NEW identity: a pid of its own, a version of its own.
	if got.AssetPID == "" || got.AssetPID == f.allowed.pid {
		t.Fatalf("derived asset_pid = %q, want a pid of its own (the parent is %q)", got.AssetPID, f.allowed.pid)
	}
	if !assets.ValidPID(got.AssetPID) {
		t.Errorf("derived asset_pid %q is not a pid", got.AssetPID)
	}
	if got.Version != "2.0" || got.Visibility != "public" {
		t.Errorf("derived version = %s@%s, want 2.0@public", got.Version, got.Visibility)
	}
	// The response names the edge it recorded, read back from the stored
	// derivation rather than echoed from the request.
	if got.ParentAssetPID != f.allowed.pid || got.ParentVersion != f.allowed.version || got.Relation != "forked_from" {
		t.Errorf("response edge = %s@%s %s, want %s@%s forked_from",
			got.ParentAssetPID, got.ParentVersion, got.Relation, f.allowed.pid, f.allowed.version)
	}

	// The new asset row is in the TARGET project, with the display fields
	// the request named — and the parent's asset did NOT gain a version.
	childAssetID := assetRowID(t, ctx, w.pool, got.AssetPID)
	var originProject, slug, title string
	if err := w.pool.QueryRow(ctx,
		`SELECT origin_project_id::text, slug, title FROM research_assets WHERE id = $1`, childAssetID).
		Scan(&originProject, &slug, &title); err != nil {
		t.Fatalf("read the derived asset row: %v", err)
	}
	if originProject != f.childProject {
		t.Errorf("the derived asset's origin project = %s, want the target %s", originProject, f.childProject)
	}
	if slug != "derived-dataset" || title != "Derived dataset" {
		t.Errorf("derived asset display fields = %q/%q, want derived-dataset/Derived dataset", slug, title)
	}
	if n := countRows(t, ctx, w.pool,
		`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, f.allowed.assetID); n != 1 {
		t.Errorf("the PARENT's asset carries %d versions, want the 1 it was seeded with — a derivation creates an identity, never a version of the parent", n)
	}

	// (1) The stored edge, keyed by the two VERSION row ids.
	childVersionRow := versionRowID(t, ctx, w.pool, childAssetID, "2.0")
	edges := lineageEdges(t, ctx, w.pool)
	if len(edges) != 1 {
		t.Fatalf("asset_lineage holds %d rows after one derivation, want exactly 1: %+v", len(edges), edges)
	}
	if edges[0].parentVersionID != f.allowed.versionRow {
		t.Errorf("the edge's parent end = %s, want the parent VERSION row %s (pid %s, version %s)",
			edges[0].parentVersionID, f.allowed.versionRow, f.allowed.pid, f.allowed.version)
	}
	if edges[0].childVersionID != childVersionRow {
		t.Errorf("the edge's child end = %s, want the new version row %s", edges[0].childVersionID, childVersionRow)
	}
	if edges[0].relation != "forked_from" {
		t.Errorf("the edge's relation = %q, want the requested %q", edges[0].relation, "forked_from")
	}

	// The ledger entry names the same three things, so a replay answers with
	// the edge this request wrote and not with another edge of the child.
	ledger := deriveLedger(t, ctx, w.pool)
	if len(ledger) != 0 {
		t.Errorf("a derivation without an Idempotency-Key wrote %d ledger rows, want 0: %+v", len(ledger), ledger)
	}

	// (2) The parent version is unchanged, byte-for-byte, in every column.
	if parentAfter := versionFingerprintOf(t, ctx, w.pool, f.allowed.versionRow); parentAfter != parentBefore {
		t.Errorf("the parent version changed:\nbefore: %s\nafter:  %s", parentBefore, parentAfter)
	}
	// ...and the trigger behind that comparison is real: a direct UPDATE of
	// the parent's row is refused, so the fingerprint above could not have
	// been restored silently by something else.
	var pgErr *pgconn.PgError
	_, err := w.pool.Exec(ctx, `UPDATE research_asset_versions SET visibility = 'private' WHERE id = $1`, f.allowed.versionRow)
	if err == nil {
		t.Fatal("an UPDATE of the parent version row succeeded: the append-only guard is not in force, so the fingerprint above proves nothing")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("UPDATE of the parent version = %v, want the append-only guard's P0001", err)
	}

	// The audit row and the research event: one of each, naming the child.
	audits := deriveAudits(t, ctx, w.pool)
	if len(audits) != 1 {
		t.Fatalf("the derivation wrote %d audit rows, want 1", len(audits))
	}
	if audits[0].targetRef != "asset_version:"+childVersionRow {
		t.Errorf("audit target_ref = %q, want the new version row asset_version:%s", audits[0].targetRef, childVersionRow)
	}
	if audits[0].actorID != aliceID || audits[0].projectID != f.childProject {
		t.Errorf("audit actor/project = %s/%s, want %s/%s", audits[0].actorID, audits[0].projectID, aliceID, f.childProject)
	}
	if audits[0].declared != string(rights.PermissionAllowed) || audits[0].required != "false" {
		t.Errorf("audit declaration = %q (required=%q), want the parent's own %q and no confirmation required",
			audits[0].declared, audits[0].required, rights.PermissionAllowed)
	}
}

// TestAssetLineageRecordsTheRequestedRelation: the two relations are the
// caller's choice and are stored verbatim, and 'supersedes' — the 00010
// CHECK's third value, which a derivation never records — is refused as a
// malformed request rather than accepted and reinterpreted.
func TestAssetLineageRecordsTheRequestedRelation(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-relation-alice@example.com", "lineage-relation-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-relation-bob@example.com", "lineage-relation-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// supersedes: refused, writing nothing — the vocabulary this command
	// records is the two identity-creating relations.
	mustRefuseDerive(t, alice, f.childProject,
		deriveBody(t, f, parentRef(f.allowed), "supersedes", "2.0", ""), "",
		http.StatusBadRequest, assets.CodeDeriveValidationFailed)
	mustWriteNothing(t, ctx, w.pool, f.childProject, "a supersedes request")

	got := mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "derived_from", "2.0", ""), "")
	if got.Relation != "derived_from" {
		t.Errorf("response relation = %q, want the requested derived_from", got.Relation)
	}
	edges := lineageEdges(t, ctx, w.pool)
	if len(edges) != 1 || edges[0].relation != "derived_from" {
		t.Fatalf("asset_lineage = %+v, want one derived_from edge", edges)
	}
	if edges[0].parentVersionID != f.allowed.versionRow {
		t.Errorf("the edge's parent end = %s, want %s", edges[0].parentVersionID, f.allowed.versionRow)
	}
}

// --------------------------------------------------------------------------
// 3 + 8. The parent's declaration: three values, three answers

// TestAssetLineageRightsVerdicts walks the parent's stored declaration
// through all three of its values, plus the unreadable case.
//
// Each value is ONE named subtest, so a regression in the first value does not
// stop the later ones from being exercised and a failure report names which
// value broke. The subtests share one fixture and run in order (none is
// t.Parallel), which is what makes the "nothing was written" assertions
// meaningful: every refusal below runs BEFORE any successful derivation in
// this project, so the counts they assert are the counts of the whole case.
func TestAssetLineageRightsVerdicts(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-rights-alice@example.com", "lineage-rights-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-rights-bob@example.com", "lineage-rights-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// --- the refusals come first, so each asserts a database with no rows --

	t.Run("restricted refuses even beside a confirmation", func(t *testing.T) {
		// The confirmation is sent on purpose: a caller cannot know the
		// parent's declaration before asking, so the confirmation must not be
		// read as a licence to override an explicit prohibition.
		mustRefuseDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.restricted), "forked_from", "2.0", "I accept responsibility"), "",
			http.StatusForbidden, assets.CodeDeriveDerivativesRestricted)
		mustWriteNothing(t, ctx, w.pool, f.childProject, "a derivation from a restricted parent")
	})

	t.Run("unspecified without a confirmation is refused, with a code of its own", func(t *testing.T) {
		mustRefuseDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.unspecified), "forked_from", "2.0", ""), "",
			http.StatusForbidden, assets.CodeDeriveDerivativesUnspecified)
		mustWriteNothing(t, ctx, w.pool, f.childProject, "an unconfirmed derivation from an unspecified parent")

		// The two refusals are different answers: one says stop, the other says
		// confirm. If the third value were collapsed into the prohibition (or
		// into the permission), a client could not tell them apart. The CODES
		// are what separate them — the status is 403 for both, deliberately:
		// both say "not you / not like this", and a client branches on `code`.
		if assets.CodeDeriveDerivativesUnspecified == assets.CodeDeriveDerivativesRestricted {
			t.Error("the unspecified and restricted refusals share a wire code: the three values are not distinguishable on the wire")
		}
	})

	t.Run("a blank confirmation is no confirmation", func(t *testing.T) {
		mustRefuseDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.unspecified), "forked_from", "2.0", "   "), "",
			http.StatusForbidden, assets.CodeDeriveDerivativesUnspecified)
		mustWriteNothing(t, ctx, w.pool, f.childProject, "a blank confirmation")
	})

	t.Run("an unreadable declaration is refused as unreadable", func(t *testing.T) {
		mustRefuseDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.unreadable), "forked_from", "2.0", "I accept responsibility"), "",
			http.StatusConflict, assets.CodeDeriveParentRightsUnreadable)
		mustWriteNothing(t, ctx, w.pool, f.childProject, "a derivation from an unreadable declaration")
	})

	// --- the two permissions, which do write ------------------------------

	t.Run("allowed is performed with no confirmation", func(t *testing.T) {
		mustDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")
		if n := countRows(t, ctx, w.pool, `SELECT count(*) FROM asset_lineage`); n != 1 {
			t.Fatalf("the allowed derivation left %d edges, want 1", n)
		}
	})

	t.Run("unspecified is performed on the confirmation, which the audit row records", func(t *testing.T) {
		const confirmation = "I have read the declaration and I accept responsibility for this derivation"
		got := mustDerive(t, alice, f.childProject,
			deriveBody(t, f, parentRef(f.unspecified), "derived_from", "2.0", confirmation), "")
		if got.Relation != "derived_from" {
			t.Errorf("response relation = %q, want derived_from", got.Relation)
		}
		if got.AssetPID == "" || got.AssetPID == f.unspecified.pid {
			t.Errorf("the confirmed derivation's asset_pid = %q, want a new identity", got.AssetPID)
		}

		audits := deriveAudits(t, ctx, w.pool)
		if len(audits) != 2 {
			t.Fatalf("the two performed derivations wrote %d audit rows, want 2", len(audits))
		}
		// Oldest first: the allowed one, then the confirmed one.
		confirmed := audits[1]
		if confirmed.declared != string(rights.PermissionUnspecified) {
			t.Errorf("the audit row records derivatives_declared = %q, want %q", confirmed.declared, rights.PermissionUnspecified)
		}
		if confirmed.confirm != confirmation {
			t.Errorf("the audit row records derivatives_confirmation = %q, want the caller's sentence %q", confirmed.confirm, confirmation)
		}
		if confirmed.required != "true" {
			t.Errorf("the audit row records derivatives_confirmation_required = %q, want \"true\": this derivation happened ON the confirmation", confirmed.required)
		}
		if confirmed.actorID != aliceID {
			t.Errorf("the confirmation is recorded beside actor %s, want the deriving user %s", confirmed.actorID, aliceID)
		}
		// The allowed derivation recorded its declaration too, and says the
		// confirmation was not what permitted it.
		if audits[0].declared != string(rights.PermissionAllowed) || audits[0].required != "false" {
			t.Errorf("the allowed derivation's audit row = %q/required=%q, want %q/false",
				audits[0].declared, audits[0].required, rights.PermissionAllowed)
		}

		// Both performed derivations created their own identity and their own
		// edge: two edges, into two different assets.
		if n := countRows(t, ctx, w.pool, `SELECT count(*) FROM asset_lineage`); n != 2 {
			t.Errorf("asset_lineage holds %d rows, want 2", n)
		}
	})
}

// --------------------------------------------------------------------------
// 4. The read gate

// TestAssetLineageHidesAnUnreadableParent: a caller who may not READ a
// version may not derive from it, and learns nothing about whether it
// exists. The two responses — a version of another project's private asset,
// and a version that does not exist at all — are compared byte for byte
// apart from the request id.
func TestAssetLineageHidesAnUnreadableParent(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-gate-alice@example.com", "lineage-gate-alice")
	bob, bobID := signup(t, w.ts.URL, "lineage-gate-bob@example.com", "lineage-gate-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// Alice is the owner of the TARGET project (so the authorization above
	// the read gate passes) and a stranger to bob's private project.
	hidden := mustRefuseDerive(t, alice, f.childProject,
		deriveBody(t, f, parentRef(f.bobPrivate), "forked_from", "2.0", ""), "",
		http.StatusNotFound, assets.CodeDeriveParentNotFound)
	mustWriteNothing(t, ctx, w.pool, f.childProject, "a derivation from an unreadable private version")

	// A pid@version that names nothing answers the SAME thing: same status,
	// same code, same sentence. The only difference a caller can see is the
	// request id the whole API adds.
	absent := mustRefuseDerive(t, alice, f.childProject,
		deriveBody(t, f, "01j9z6k3m4n5p6q7r8s9t0v1zz@9.9", "forked_from", "2.0", ""), "",
		http.StatusNotFound, assets.CodeDeriveParentNotFound)
	mustWriteNothing(t, ctx, w.pool, f.childProject, "a derivation from an absent version")

	if stripRequestID(t, hidden) != stripRequestID(t, absent) {
		t.Errorf("the two refusals are distinguishable:\nhidden: %s\nabsent: %s", hidden, absent)
	}

	// The control: the parent's OWN project's member derives from the same
	// private version without complaint — into bob's own project, which is
	// where he may derive. The refusal above was about the reader, not about
	// the version being private.
	got := mustDerive(t, bob, f.privateProject,
		deriveBody(t, f, parentRef(f.bobPrivate), "forked_from", "0.2", ""), "")
	if got.ParentAssetPID != f.bobPrivate.pid || got.ParentVersion != f.bobPrivate.version {
		t.Errorf("bob's derivation recorded parent %s@%s, want %s@%s",
			got.ParentAssetPID, got.ParentVersion, f.bobPrivate.pid, f.bobPrivate.version)
	}
	// The edge it recorded starts at bob's private version, and the child is
	// a version row of a NEW asset: the derivation read the version it was
	// allowed to read, and added nothing to it.
	childAssetID := assetRowID(t, ctx, w.pool, got.AssetPID)
	if childAssetID == f.bobPrivate.assetID {
		t.Fatalf("bob's derivation landed in the parent's asset %s: a derivation creates an identity", childAssetID)
	}
	edges := lineageEdges(t, ctx, w.pool)
	if len(edges) != 1 || edges[0].parentVersionID != f.bobPrivate.versionRow {
		t.Fatalf("bob's derivation recorded %+v, want one edge from the private parent %s", edges, f.bobPrivate.versionRow)
	}
}

// stripRequestID removes the request id both refusals carry, so the rest of
// the two bodies can be compared: everything else must be identical.
func stripRequestID(t *testing.T, raw string) string {
	t.Helper()
	var env deriveErrorWire
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("error envelope: %v: %s", err, raw)
	}
	env.RequestID = ""
	out, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("re-render the envelope: %v", err)
	}
	return string(out)
}

// --------------------------------------------------------------------------
// The authorization, over the wire

// TestAssetLineageAuthorizationIsThePublishDecision: the denial precedes every
// lookup on the parent, and a caller who is not the target project's owner is
// resolved against the SAME matrix cell a publish resolves
// (publish_private_to_public), so the refusals are one answer.
func TestAssetLineageAuthorizationIsThePublishDecision(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-authz-alice@example.com", "lineage-authz-alice")
	mallory, malloryID := signup(t, w.ts.URL, "lineage-authz-mallory@example.com", "lineage-authz-mallory")
	_, bobID := signup(t, w.ts.URL, "lineage-authz-bob@example.com", "lineage-authz-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// Mallory is a maintainer of the target project: the matrix's
	// publish_private_to_public cell is `conditional` for that column, and a
	// condition with no specification resolves to a refusal (internal/authz
	// admits only `allow`).
	if _, err := w.pool.Exec(ctx,
		`INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'maintainer')`,
		f.childProject, malloryID); err != nil {
		t.Fatalf("seed mallory's membership: %v", err)
	}
	mustRefuseDerive(t, mallory, f.childProject,
		deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "",
		http.StatusForbidden, assets.CodeDeriveNotPermitted)
	mustWriteNothing(t, ctx, w.pool, f.childProject, "a maintainer's derivation")

	// The SAME request by the project's owner is performed: the refusal is
	// about the actor's class, not about the body.
	mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")

	// A project id that names nothing answers the SAME refusal, never "not
	// found": the denial is resolved before any target lookup, so this route
	// cannot be used to probe which projects exist.
	unknownProject := "00000000-0000-0000-0000-000000000000"
	mustRefuseDerive(t, mallory, unknownProject,
		deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "",
		http.StatusForbidden, assets.CodeDeriveNotPermitted)
	// ...and so does one that is not a uuid at all.
	mustRefuseDerive(t, mallory, "not-a-project",
		deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "",
		http.StatusForbidden, assets.CodeDeriveNotPermitted)
}

// --------------------------------------------------------------------------
// 5b. The governance rule that blocks a publish blocks a derivation

// TestAssetLineageAppliesThePolicyInForce: a derivation writes a NEW PUBLIC
// ASSET VERSION, which is the act domain.RulePublicAssetIPReview governs. The
// rule is evaluated on this path exactly as it is on the publish's, against
// the policy in force for the TARGET project, and a project that requires an
// IP review refuses the derivation with the publish's own wire code.
//
// The test is written as a BEFORE/AFTER pair over one project: the same
// actor, the same parent, the same document, once with the project's policy
// silent and once with it set. That is what makes the second half evidence
// about the policy rather than about the request — every other input is
// identical.
func TestAssetLineageAppliesThePolicyInForce(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-policy-alice@example.com", "lineage-policy-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-policy-bob@example.com", "lineage-policy-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// The wire code is the PUBLISH's: a client that knows
	// RIGHTS_POLICY_BLOCKS_ACTION from the publish must not have to learn a
	// second name for the same governance step. Asserted as a fact about the
	// two constants rather than by spelling the string twice.
	if assets.CodeDerivePolicyRefused != assetpublish.CodePolicyRefused {
		t.Fatalf("derive policy code = %q, publish policy code = %q: one governance step, one code",
			assets.CodeDerivePolicyRefused, assetpublish.CodePolicyRefused)
	}

	// BEFORE: the target project has no policy in force. The derivation is
	// performed (nothing else in this test would tell us the refusal below is
	// about the policy rather than about the request being unsendable).
	mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")

	// The project's policy now requires an IP review of every public asset
	// version. Written through the product's own surface (T0603's PUT), by
	// the project's owner.
	resp := alice.do(t, http.MethodPut, "/api/v1/projects/"+f.childProject+"/policy",
		`{"version":"v1","policy":{"public_asset_ip_review":true}}`)
	mustStatus(t, resp, http.StatusCreated)
	resp.Body.Close()

	// AFTER: the identical request is refused, with the publish's code, and
	// NOTHING is written — no asset, no version, no edge, no ledger entry, no
	// audit row. A governance refusal is a transaction that never opened, not
	// one that rolled back. The counts are read before and after rather than
	// asserted zero, because the BEFORE half above legitimately wrote one of
	// each and this test is about the DELTA the refusal leaves.
	before := derivedRows(t, ctx, w.pool, f.childProject)
	mustRefuseDerive(t, alice, f.childProject,
		deriveBody(t, f, parentRef(f.allowed), "forked_from", "3.0", ""), "",
		http.StatusForbidden, assetpublish.CodePolicyRefused)
	for name, n := range derivedRows(t, ctx, w.pool, f.childProject) {
		if n != before[name] {
			t.Errorf("a derivation refused by the policy in force wrote %s rows: %d, was %d — a refusal must write nothing, the audit row included",
				name, n, before[name])
		}
	}

	// The rule is about PUBLIC asset versions, so the same project's policy
	// does not reach a PRIVATE derivation — which widens nothing, and which
	// the publish does not read the rule for either.
	privateBody := deriveBody(t, f, parentRef(f.allowed), "forked_from", "4.0", "")
	var body map[string]any
	if err := json.Unmarshal([]byte(privateBody), &body); err != nil {
		t.Fatalf("decode the derive body: %v", err)
	}
	body["visibility"] = "private"
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("re-encode the private derive body: %v", err)
	}
	mustDerive(t, alice, f.childProject, string(raw), "")
}

// --------------------------------------------------------------------------
// 6. The page renders the edge from both ends

// TestAssetLineageIsRenderedFromBothEnds: the criterion the UI half was
// narrowed to — "the write path, and the existing read renders it". The
// parent's page shows the child, and the child's page shows the parent.
func TestAssetLineageIsRenderedFromBothEnds(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-page-alice@example.com", "lineage-page-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-page-bob@example.com", "lineage-page-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	got := mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")

	// The child's page: one edge, read as its PARENT.
	raw, childPage := fetchAssetPage(t, alice, got.AssetPID, "")
	if len(childPage.Lineage) != 1 {
		t.Fatalf("the derived asset's page shows %d lineage entries, want 1: %s", len(childPage.Lineage), raw)
	}
	childEdge := childPage.Lineage[0]
	if childEdge.Direction != assets.LineageParent {
		t.Errorf("the derived asset's edge direction = %q, want %q", childEdge.Direction, assets.LineageParent)
	}
	if childEdge.Relation != "forked_from" {
		t.Errorf("the derived asset's edge relation = %q, want forked_from", childEdge.Relation)
	}
	if childEdge.PID != f.allowed.pid || childEdge.Version != f.allowed.version {
		t.Errorf("the derived asset's edge names %s@%s, want the parent %s@%s",
			childEdge.PID, childEdge.Version, f.allowed.pid, f.allowed.version)
	}

	// The parent's page: the same edge, read as its CHILD.
	rawParent, parentPage := fetchAssetPage(t, alice, f.allowed.pid, "")
	if len(parentPage.Lineage) != 1 {
		t.Fatalf("the parent's page shows %d lineage entries, want 1: %s", len(parentPage.Lineage), rawParent)
	}
	parentEdge := parentPage.Lineage[0]
	if parentEdge.Direction != assets.LineageChild {
		t.Errorf("the parent's edge direction = %q, want %q", parentEdge.Direction, assets.LineageChild)
	}
	if parentEdge.Relation != "forked_from" {
		t.Errorf("the parent's edge relation = %q, want forked_from", parentEdge.Relation)
	}
	if parentEdge.PID != got.AssetPID || parentEdge.Version != got.Version {
		t.Errorf("the parent's edge names %s@%s, want the derived %s@%s",
			parentEdge.PID, parentEdge.Version, got.AssetPID, got.Version)
	}
	// The title is rendered because the edge is rendered only for a
	// counterpart the network can open; the derived asset's own title is
	// what the parent's page names.
	if parentEdge.Title != "Derived dataset" {
		t.Errorf("the parent's edge names title %q, want the derived asset's %q", parentEdge.Title, "Derived dataset")
	}
}

// --------------------------------------------------------------------------
// 7. Idempotency, as counts

// TestAssetLineageIsIdempotentPerKey: one key replayed answers the derivation
// the first call made, and the database holds exactly ONE of everything the
// derivation writes.
func TestAssetLineageIsIdempotentPerKey(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-idem-alice@example.com", "lineage-idem-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-idem-bob@example.com", "lineage-idem-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// The target project starts empty of assets: every count below is about
	// what the derivation wrote, not about the fixture.
	if n := countRows(t, ctx, w.pool, `SELECT count(*) FROM research_assets WHERE origin_project_id = $1`, f.childProject); n != 0 {
		t.Fatalf("the target project starts with %d assets, want 0", n)
	}

	const key = "derive-key-1"
	body := deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", "")

	first := mustDerive(t, alice, f.childProject, body, key)
	second := mustDerive(t, alice, f.childProject, body, key)
	if second.AssetPID != first.AssetPID || second.Version != first.Version ||
		second.IntegrityHash != first.IntegrityHash || second.PublishedAt != first.PublishedAt ||
		second.ParentAssetPID != first.ParentAssetPID || second.ParentVersion != first.ParentVersion {
		t.Errorf("the replay answered a different derivation:\n first: %+v\nsecond: %+v", first, second)
	}
	if second.PublishedBy != first.PublishedBy {
		t.Errorf("the replay answered publisher %q, want %q", second.PublishedBy, first.PublishedBy)
	}

	// ONE of everything. The counts are the point: "a row exists" would pass
	// on a derivation that wrote twice.
	counts := derivedRows(t, ctx, w.pool, f.childProject)
	for name, want := range map[string]int{"assets": 1, "versions": 1, "edges": 1, "ledger": 1, "audit": 1, "events": 1} {
		if counts[name] != want {
			t.Errorf("after a derivation and its replay: %d %s rows, want %d", counts[name], name, want)
		}
	}

	// The ledger entry names the child version and the parent edge, so the
	// replay is answerable without re-reading the parent at all.
	ledger := deriveLedger(t, ctx, w.pool)
	if len(ledger) != 1 {
		t.Fatalf("the ledger holds %d rows, want 1: %+v", len(ledger), ledger)
	}
	childAssetID := assetRowID(t, ctx, w.pool, first.AssetPID)
	childVersionRow := versionRowID(t, ctx, w.pool, childAssetID, "2.0")
	if ledger[0].idempotencyKey != key || ledger[0].assetVersionID != childVersionRow ||
		ledger[0].parentVersionID != f.allowed.versionRow || ledger[0].relation != "forked_from" {
		t.Errorf("the ledger row = %+v, want key %q → child %s from parent %s (forked_from)",
			ledger[0], key, childVersionRow, f.allowed.versionRow)
	}
	if ledger[0].projectID != f.childProject {
		t.Errorf("the ledger row's project = %s, want the target %s", ledger[0].projectID, f.childProject)
	}

	// A DIFFERENT derivation under the same key is a conflict, not a replay:
	// the caller would otherwise receive an identity it did not ask for.
	mustRefuseDerive(t, alice, f.childProject,
		deriveBody(t, f, parentRef(f.unspecified), "forked_from", "2.0", "I accept"), key,
		http.StatusConflict, assets.CodeDeriveIdempotencyConflict)
	// Nothing was written by the conflict, and the FIRST derivation's rows
	// are untouched.
	after := derivedRows(t, ctx, w.pool, f.childProject)
	for name, want := range map[string]int{"assets": 1, "versions": 1, "edges": 1, "ledger": 1, "audit": 1, "events": 1} {
		if after[name] != want {
			t.Errorf("after a conflicting replay: %d %s rows, want %d", after[name], name, want)
		}
	}
}

// TestAssetLineageAppliesOnceUnderConcurrency: two requests carrying one key,
// issued at the same time, produce ONE asset, ONE version, ONE edge, ONE
// ledger entry and ONE audit row. This is the case read-then-write cannot
// pass: both requests read an empty ledger before either commits, so only the
// store's second read UNDER the project row lock can make the second one
// replay instead of writing again.
func TestAssetLineageAppliesOnceUnderConcurrency(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-race-alice@example.com", "lineage-race-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-race-bob@example.com", "lineage-race-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	const key = "derive-race-key-1"
	body := deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", "")

	type outcome struct {
		status int
		body   string
		err    error
	}
	results := make([]outcome, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, raw, err := deriveOnce(alice.client, w.ts.URL, alice.csrf, f.childProject, body, key)
			results[i] = outcome{status: status, body: raw, err: err}
		}(i)
	}
	wg.Wait()

	var pids []string
	for i, res := range results {
		if res.err != nil {
			t.Fatalf("concurrent derivation %d: %v", i, res.err)
		}
		if res.status != http.StatusCreated {
			t.Fatalf("concurrent derivation %d = %d: %s", i, res.status, res.body)
		}
		var got derivedAssetWire
		if err := json.Unmarshal([]byte(res.body), &got); err != nil {
			t.Fatalf("concurrent derivation %d payload: %v: %s", i, err, res.body)
		}
		pids = append(pids, got.AssetPID)
	}
	if pids[0] != pids[1] {
		t.Errorf("the two concurrent derivations answered different identities (%s, %s): one key must name one derivation", pids[0], pids[1])
	}

	counts := derivedRows(t, ctx, w.pool, f.childProject)
	for name, want := range map[string]int{"assets": 1, "versions": 1, "edges": 1, "ledger": 1, "audit": 1, "events": 1} {
		if counts[name] != want {
			t.Errorf("two concurrent derivations under one key left %d %s rows, want %d", counts[name], name, want)
		}
	}

	// Both ends of the one stored edge, and the one ledger entry, name the
	// identity both callers were answered with.
	childAssetID := assetRowID(t, ctx, w.pool, pids[0])
	childVersionRow := versionRowID(t, ctx, w.pool, childAssetID, "2.0")
	edges := lineageEdges(t, ctx, w.pool)
	if len(edges) != 1 || edges[0].parentVersionID != f.allowed.versionRow || edges[0].childVersionID != childVersionRow {
		t.Fatalf("the stored edge = %+v, want one edge %s → %s", edges, f.allowed.versionRow, childVersionRow)
	}
	ledger := deriveLedger(t, ctx, w.pool)
	if len(ledger) != 1 || ledger[0].assetVersionID != childVersionRow || ledger[0].idempotencyKey != key {
		t.Fatalf("the ledger = %+v, want one entry for key %q naming %s", ledger, key, childVersionRow)
	}
}

// --------------------------------------------------------------------------
// The agent backstop, over the real store

// TestAssetLineageRefusesAnAgentToken drives the platform-agent case through
// the real command and the real store. V1 mints no agent tokens — the flag is
// produced here rather than by a credential, which is exactly the state of
// the build: the refusal exists BEFORE the tokens do, and it must survive a
// governance edit of the matrix cell it stands beside.
func TestAssetLineageRefusesAnAgentToken(t *testing.T) {
	ctx := testCtx(t)
	w := newAssetLineageWorld(t, ctx)
	alice, aliceID := signup(t, w.ts.URL, "lineage-agent-alice@example.com", "lineage-agent-alice")
	_, bobID := signup(t, w.ts.URL, "lineage-agent-bob@example.com", "lineage-agent-bob")
	f := seedLineageFixture(t, ctx, w.pool, aliceID, bobID)

	// The command's input, assembled from the same candidate the route's body
	// is rendered from: the agent case has no HTTP expression in V1 (the
	// handler sets IsAgent false in one place), so it is driven where it is
	// decidable — the flag the transport would have to set is set here.
	params := deriveParamsFor(t, f, parentRef(f.allowed), "forked_from", "2.0", "")
	params.ProjectID = f.childProject

	_, err := w.derive.Derive(ctx, assets.DeriveActor{User: domain.User{ID: aliceID}, IsAgent: true}, params)
	if !errors.Is(err, assets.ErrDeriveAgentNotPermitted) {
		t.Fatalf("an agent's derivation = %v, want ErrDeriveAgentNotPermitted", err)
	}
	var refusal *assets.DeriveAgentNotPermittedError
	if !errors.As(err, &refusal) || refusal.Code() != assets.CodeDeriveAgentDenied {
		t.Fatalf("the refusal = %v, want the agent denial's own code %q", err, assets.CodeDeriveAgentDenied)
	}
	mustWriteNothing(t, ctx, w.pool, f.childProject, "a refused agent derivation")

	// The second line, asserted on its own: the matrix denies the agent
	// column of publish_private_to_public even when the agent acts as the
	// project's OWNER — while the same row read at the human owner's column
	// permits it, which is what makes this a comparison rather than a proof
	// that the matrix denies everything.
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
		t.Errorf("the permission matrix permits an agent that owns the project to derive: %+v", agentDecision)
	}
	humanDecision, err := engine.Authorize(ctx, authz.Request{
		Action: authz.ActionPublishPrivateToPublic,
		Class:  authz.ClassOf(true, &owner, false),
	})
	if err != nil {
		t.Fatalf("matrix engine: %v", err)
	}
	if !humanDecision.Permits() {
		t.Errorf("the permission matrix denies the project's owner the derivation: %+v", humanDecision)
	}

	// The control: the same request over the wire by the human owner is
	// performed — so the refusal above is about the actor's kind, not about
	// the request.
	mustDerive(t, alice, f.childProject, deriveBody(t, f, parentRef(f.allowed), "forked_from", "2.0", ""), "")
}
