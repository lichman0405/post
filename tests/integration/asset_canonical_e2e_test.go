// Task T0710 required test "asset canonical e2e" — the canonical asset chain
// walked from end to end on the REAL path, in one composed production tree
// over one real PostgreSQL.
//
// The task's own words are the chain: "Release→Dataset Asset publish→另一
// Project reference/depend→fork derived", accepted as "PID/version/rights/
// lineage 全正确". Four hops, and the suite is one subtest per thing that has
// to be true for the chain to have happened rather than to have been
// described:
//
//  1. RELEASE. POST /api/v1/projects/{id}/releases (releasehttp) creates the
//     immutable snapshot of main's head at that moment. The assertion is not
//     the 201: it is that main MOVES afterwards and the release's stored
//     state_id and exported manifest bytes do not (docs/11 §1, invariant 5).
//     Red condition: a release whose manifest were re-rendered from the
//     current head would export state_id = head2 and different bytes.
//
//  2. DATASET PUBLISH. POST /api/v1/projects/{id}/assets:publish
//     (assetshttp, assetpublish.Command) turns the version document into an
//     immutable published version of a NEW asset identity, and internal/
//     assets.NewPID mints the identity HERE — assetpublish/command.go:265-299
//     is the only place a pid is decided. The assertions are the pid's shape
//     and uniqueness, that it is a different thing from the version label,
//     and that the stored manifest is the document that was sent: the row's
//     manifest parses back to a document whose canonical hash IS the stored
//     integrity_hash (assets.ManifestHash), and the stored jsonb equals the
//     sent canonical bytes field for field. Red condition: a publish that
//     stored a re-rendered or trimmed manifest, or a pid that repeated.
//
//  3. CROSS-PROJECT DEPENDENCY. A second project's publication declares a
//     pin on the first project's exact version, and the publish transaction
//     records the usage — asset_dependencies, written by
//     persistence/asset_publish_store.go:366 recordUsageDeclarations from
//     the publish's own manifest (internal/assets/usage.go). The asserted
//     row is the (consumer project, provider VERSION row, depends_on,
//     visibility_of_usage = the consumer version's own visibility) tuple.
//     Red condition: a usage row keyed on the asset instead of the version,
//     typed references, or carrying the provider's visibility.
//
//  4. FORK/DERIVE + LINEAGE. POST /api/v1/projects/{id}/assets:derive
//     (assetshttp/derive.go, internal/assets.DeriveCommand — T0708) creates
//     a NEW asset identity from the pinned parent version and records ONE
//     asset_lineage edge whose parent end is the parent VERSION's row id.
//     The assertions are the new identity (a different asset row with a
//     different pid), the edge's direction and type, the parent version's
//     immutability (fingerprint, plus the append-only trigger actually
//     refusing an UPDATE and a DELETE), and the three-valued rights verdict
//     with the RESTRICTED case really walked.
//
// # Why the fourth hop is a real route and not a stub
//
// The whole suite drives HTTP: every hop above is one request to a handler
// that cmd/api/main.go mounts, behind the real auth guard (session + CSRF),
// over the real commands, stores and permission matrix. Nothing here calls a
// command to "get past" a surface, and nothing fakes the derivation — a
// derivation that did not go through assetshttp.New(...).Register would not
// be evidence that the product can do it. T0708 landed that route
// (cmd/api/assetshttp/derive.go); this suite is its end-to-end half in the
// chain, and the two existing suites that pin it in isolation
// (internal/assets/derive_test.go for the decisions,
// tests/integration/asset_lineage_test.go over rows) are left exactly as
// they are.
//
// # `references` is not expressible in V1, and this suite does not invent it
//
// The chain's third hop is written "reference/depend" — the citation half and
// the dependency half of docs/11 §5. Only the dependency half exists. Every
// pin a publication declares is recorded as depends_on, and the reason is
// written into the repository: internal/assets/usage.go:45-52 — "Every pin is
// a dependency, never a citation. […] A manifest has no field that means
// \"background knowledge\": nothing in the document distinguishes a version
// that was consulted from one that was used as an input, so a citation is not
// expressible here and this rule does not invent one."
//
// DependencyTypeReferences is in the vocabulary and the readers render it,
// but NO product path writes a row that carries it, so the only way to put
// one in a database is a bare INSERT — which is what
// tests/integration/asset_page_test.go:658-663 does, as a fixture for the
// reader it is testing. This suite therefore walks the depends_on half AND
// asserts the absence of the other: every row in asset_dependencies in the
// whole database is one of the rows the publishes and derivations in this
// run wrote, and all of them are depends_on. A raw-SQL 'references' row — the
// cheapest way to fake "the citation half is wired" — would make that count
// non-zero and fail the suite. That absence is recorded as a FACT, not
// patched: adding the citation path is a product decision, and a manifest
// field for it is a contract (specs/**) change this task may not make.
//
// # What this suite does not do
//
// It does not touch apps/web: the browser face of the asset hub is T0709's
// suite (tests/e2e-assets/), and the API is where this chain lives. It does
// not add a migration, a route, or a spec: internal/**, cmd/**, specs/**,
// infra/** and apps/web/** are outside this task's scope, and a gap found in
// any of them is reported rather than patched.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/assetshttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/policyhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/releasehttp"
	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/manifests"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// assetCanonicalTaskID namespaces this task's test databases
// (test_T0710_<run_id>).
const assetCanonicalTaskID = "T0710"

// --------------------------------------------------------------------------
// The composed surface

// canonicalWorld is the chain's whole tree, as cmd/api/main.go wires it: the
// auth guard, organizations, projects, policy, releases, and the asset
// surface with its publish, its fork/derive write and its reads. Only the
// session store is in-memory, exactly as every suite in this package
// composes it.
type canonicalWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
	// rsg is the research-state service, used by the FIXTURE only: the
	// release gate requires an accepted state on main, and the accepted
	// state is created through the same production object path the release
	// suite seeds its own with (raw SQL would be a second, weaker way to
	// build a state the ladder validates).
	rsg *rsg.Service
}

func newCanonicalWorld(t *testing.T, ctx context.Context) *canonicalWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), assetCanonicalTaskID)

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
	stateStore := persistence.NewStateStore(pool)
	policyStore := persistence.NewPolicyStore(pool)
	releaseCommand := releases.NewCommand(
		releases.NewService(
			stateStore,
			manifests.NewService(stateStore, persistence.NewManifestStore(pool)),
			projectStore,
			persistence.NewBranchStore(pool),
			policyStore,
			persistence.NewReleaseStore(pool),
			reg,
		),
		projectStore,
		policyStore,
		persistence.NewBranchStore(pool),
		stateStore,
		appvalidation.NewService(
			persistence.NewValidationSnapshotRepository(stateStore),
			rsgvalidation.NewValidator(reg),
		),
		persistence.NewReleaseStore(pool),
		authz.NewMatrixEngine(),
	)
	releaseAPI := releasehttp.New(releasehttp.Deps{
		Command:  releaseCommand,
		Projects: projectAPI.Service(),
	})
	publishCommand := assetpublish.NewCommand(assetpublish.Deps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetPublishStore(pool, rsgvalidation.NewValidator(reg)),
		Authz:    authz.NewMatrixEngine(),
	})
	// The derivation is wired over ONE membership adapter handed to both the
	// command and its store, the way main.go hands it: the authorization
	// outside the transaction and the parent-read gate inside it ask the same
	// question, and there is one implementation of "is this caller a member".
	deriveCommand := assets.NewDeriveCommand(assets.DeriveDeps{
		Members:  projectStore,
		Policies: policyAPI.Service(),
		Rules:    policy.NewRuleEvaluator(),
		Store:    persistence.NewAssetDeriveStore(pool, rsgvalidation.NewValidator(reg), projectStore),
		Authz:    authz.NewMatrixEngine(),
	})
	rsgSvc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/organizations", orgAPI(orgStore).Routes())
	mux.Handle("/api/v1/organizations/", orgAPI(orgStore).Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	policyAPI.Register(mux)
	releaseAPI.Register(mux)
	assetshttp.New(assetshttp.Deps{
		State:        assetshttp.NewPostgresStateStore(pool),
		Projects:     projectAPI.Service(),
		Publish:      publishCommand,
		Derive:       deriveCommand,
		Pages:        persistence.NewAssetPageStore(pool),
		Members:      projectAPI.Service(),
		Dependencies: persistence.NewProjectDependencyStore(pool),
		Metadata: assetmetadata.NewCommand(assetmetadata.Deps{
			Members: projectStore,
			Store:   persistence.NewAssetMetadataStore(pool),
		}),
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &canonicalWorld{ts: ts, pool: pool, rsg: rsgSvc}
}

// orgAPI wires the organization surface (the org create route this suite
// uses, and the org policy route policyhttp registers beside it).
func orgAPI(store *persistence.OrgStore) *orgshttp.API {
	return orgshttp.New(orgshttp.Deps{Store: store})
}

// --------------------------------------------------------------------------
// The repository the chain runs in

// canonicalProject creates a project through the real route and returns its
// id — the caller becomes its owner (the store writes the membership with
// the project), which is the membership every governed write below is
// authorized against.
func canonicalProject(t *testing.T, uc *testUserClient, slug, name, visibility, orgID string) string {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":"T0710 canonical asset chain","visibility":%q`,
		slug, name, visibility)
	if orgID != "" {
		body += fmt.Sprintf(`,"organization_id":%q`, orgID)
	}
	body += "}"
	resp := uc.do(t, http.MethodPost, "/api/v1/projects", body)
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	if created.Membership.Role != "owner" {
		t.Fatalf("the creator's membership role = %q, want owner: every write below is authorized as one", created.Membership.Role)
	}
	return created.Project.ID
}

// canonicalMainBranch gives one project main, and returns the branch id and
// the genesis state id. It is separate from the appending below because the
// chain needs main to MOVE: a release is only shown to be a snapshot by
// putting something newer in front of it, and a helper that always created
// the branch could only ever produce one head (its second call is refused —
// "branch name already taken").
func (w *canonicalWorld) canonicalMainBranch(t *testing.T, ctx context.Context, owner domain.User, projectID string) (branchID, genesis string) {
	t.Helper()
	main, err := w.rsg.CreateBranch(ctx, owner, projectID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create main branch: %v", err)
	}
	if err := w.pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, projectID).Scan(&genesis); err != nil {
		t.Fatalf("read the genesis state: %v", err)
	}
	return main.ID, genesis
}

// canonicalAcceptedState appends a full accepted state to a branch through
// the real rsg service — a research question, and a hypothesis that names it
// (migration 00040 enforces the reference) — and returns the head state the
// append produced.
//
// "Accepted" is the release gate's ladder: an accepted state is what a
// release pins and what an unpublished project's version documents pin as
// their source accepted state (docs/11 §3's "source accepted state/release" —
// internal/assets/gate.go accepts either). The objects go in through the
// production path rather than by SQL because the ladder validates their
// schema, and a hand-written row would be a weaker fixture for a rule the
// gate is about to apply.
func (w *canonicalWorld) canonicalAcceptedState(t *testing.T, ctx context.Context, owner domain.User, projectID, branchID, statement string) (head string) {
	t.Helper()
	qRes, err := w.rsg.CreateObject(ctx, owner, projectID, branchID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		Payload:    json.RawMessage(fmt.Sprintf(`{"statement":%q,"purpose":%q,"question_state":"open"}`, statement+"?", "answer "+statement)),
	})
	if err != nil {
		t.Fatalf("create the fixture research question: %v", err)
	}
	res, err := w.rsg.CreateObject(ctx, owner, projectID, branchID, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":%q,"question_id":%q,"hypothesis_type":"mechanistic","scope":{"detail":"canonical chain"}}`,
			statement, qRes.Object.ID)),
	})
	if err != nil {
		t.Fatalf("create the fixture hypothesis: %v", err)
	}
	return res.Version.StateID
}

// canonicalOpenBlob attaches one openly readable blob to a state of the
// project, and returns its id: a dataset manifest's required metadata names
// blobs (internal/assets: the dataset metadata table), and the publish
// preview reads whether the named blob resolves and how it is attached.
func canonicalOpenBlob(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, ownerID, stateID, tag string) string {
	t.Helper()
	objectID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_objects (project_id, object_type, created_by)
		 VALUES ($1, 'dataset_record', $2) RETURNING id`, projectID, ownerID)
	objectVersionID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title, lifecycle_state, payload, integrity_hash, created_by)
		 VALUES ($1, 1, $2, 'https://open-rd.example/schemas/dataset.schema.json', '1', $3, 'active',
		         '{}'::jsonb, 'x', $4) RETURNING id`,
		objectID, stateID, "canonical record "+tag, ownerID)
	blobID := mustQueryUUID(t, ctx, pool,
		`INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		 VALUES ($1, 2048, $2, $3) RETURNING id`, "sha256:canonical-"+tag, "blobs/canonical/"+tag, ownerID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO blob_attachments (blob_id, scientific_object_version_id, attachment_role, access_level, state_id)
		 VALUES ($1, $2, 'data', 'open', $3)`, blobID, objectVersionID, stateID); err != nil {
		t.Fatalf("attach the %s blob: %v", tag, err)
	}
	return blobID
}

// canonicalReviews seeds the review/approval record the release gate reads:
// one research PR proposing the accepted head against main, with an approved
// scientific review and an approved integrity review (docs/09 §5 keeps the
// two dimensions apart, and the gate requires both). The rows go in by SQL
// for the release suite's own reason — the review surface is not this
// task's, and a fixture that re-tested it would be testing T0605 again.
func canonicalReviews(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID, mainBranchID, researchBranchID, genesis, head, reviewerID string) {
	t.Helper()
	q := sqlc.New(pool)
	pr, err := q.CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(projectID),
		Number:          1,
		SourceBranchID:  parseUUIDOrDie(researchBranchID),
		TargetBranchID:  parseUUIDOrDie(mainBranchID),
		BaseStateID:     parseUUIDOrDie(genesis),
		ProposedStateID: parseUUIDOrDie(head),
		Title:           "canonical chain fixture PR",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(reviewerID),
	})
	if err != nil {
		t.Fatalf("seed the fixture pull request: %v", err)
	}
	for _, kind := range []string{"scientific", "integrity"} {
		if _, err := q.CreateReview(ctx, sqlc.CreateReviewParams{
			PullRequestID:   pr.ID,
			ReviewerID:      parseUUIDOrDie(reviewerID),
			ReviewKind:      kind,
			Decision:        "approved",
			ReviewedStateID: parseUUIDOrDie(head),
			Responsibility:  "",
			Body:            "",
		}); err != nil {
			t.Fatalf("seed the fixture %s review: %v", kind, err)
		}
	}
}

// --------------------------------------------------------------------------
// The version documents this chain publishes and derives

// canonicalCandidate is the publish suite's candidate (buildCandidate) with
// the one field this chain varies and that builder does not expose: the
// rights declaration's usage.derivatives, the value the derivation's verdict
// reads (internal/assets.RequireDerivable).
//
// The document is re-rendered through rights.Document rather than patched as
// JSON, so the bytes the route receives are the bytes the rights package
// produces — the same rule the builder follows for the document it builds.
func canonicalCandidate(t *testing.T, derivatives rights.Permission, o candidateOptions) publishCandidate {
	t.Helper()
	c := buildCandidate(t, o)
	doc, err := rights.Parse(c.cand.RightsJSON)
	if err != nil {
		t.Fatalf("the fixture's rights document does not parse: %v", err)
	}
	doc.Usage.Derivatives = derivatives
	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal the fixture rights document: %v", err)
	}
	c.cand.RightsJSON = raw
	if o.pid != "" {
		// buildCandidate pre-gated this candidate; the rights bytes moved
		// under it, so the gate is re-run over the document that will be
		// sent. (The create case -- an empty pid -- cannot be pre-gated at
		// all; buildCandidate asserts that, and the pid is minted inside the
		// command.)
		if _, err := assets.Gate(c.cand); err != nil {
			t.Fatalf("the fixture candidate is not publishable after the rights declaration was set: %v", err)
		}
	}
	return c
}

// canonicalDeriveBody renders a derivation request from one candidate: the
// version document fields under the publish body's names (the derive request
// carries the publish body plus three fields of its own, cmd/api/assetshttp/
// derive.go), plus parent, relation and — only when one is given — the
// explicit derivatives confirmation. The absent case is a case: the
// unspecified verdict turns on it.
func canonicalDeriveBody(t *testing.T, c publishCandidate, parent, relation, confirmation string) string {
	t.Helper()
	out := map[string]any{
		"parent":         parent,
		"relation":       relation,
		"asset_type":     string(c.cand.AssetType),
		"version":        c.cand.Version,
		"manifest":       json.RawMessage(c.cand.Manifest),
		"rights":         json.RawMessage(c.cand.RightsJSON),
		"origin_refs":    c.cand.OriginRefs,
		"visibility":     string(c.cand.Visibility),
		"integrity_hash": c.cand.IntegrityHash,
		"creator_ids":    c.cand.CreatorIDs,
		"title":          c.title,
		"slug":           c.slug,
	}
	if confirmation != "" {
		out["derivatives_confirmation"] = confirmation
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal the derive body: %v", err)
	}
	return string(raw)
}

// --------------------------------------------------------------------------
// The rows, as SQL reports them

// canonicalScalar runs one single-value query and returns its text. NULL is
// an error rather than an empty string: "the column is unset" and "the column
// is empty" are different findings, and a helper that folded them would hide
// the first.
func canonicalScalar(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("scalar query failed: %s: %v", sql, err)
	}
	return v
}

// canonicalVersionRowText is one version row, every column of it, as text —
// to_jsonb renders the whole row, so "this row did not change" is one string
// comparison over all of the row rather than over the columns a reader
// remembered to list (versionFingerprintOf, which this suite also uses for
// the parent, is the older and narrower form of the same idea).
func canonicalVersionRowText(t *testing.T, ctx context.Context, pool *pgxpool.Pool, versionRowID string) string {
	t.Helper()
	return canonicalScalar(t, ctx, pool,
		`SELECT to_jsonb(v)::text FROM research_asset_versions v WHERE v.id = $1`, versionRowID)
}

// canonicalVersionRow resolves one published version's row id from the pid
// and label a client holds: the identity the dependency rows and the lineage
// edges name, which neither wire carries.
func canonicalVersionRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid, version string) string {
	t.Helper()
	return canonicalScalar(t, ctx, pool,
		`SELECT v.id::text FROM research_asset_versions v
		   JOIN research_assets a ON a.id = v.asset_id
		  WHERE a.pid = $1 AND v.version = $2`, pid, version)
}

// canonicalAssetRow resolves one asset's row id from its pid.
func canonicalAssetRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pid string) string {
	t.Helper()
	return canonicalScalar(t, ctx, pool, `SELECT id::text FROM research_assets WHERE pid = $1`, pid)
}

// canonicalUsage is one asset_dependencies row as this suite reads it.
type canonicalUsage struct {
	ProjectID         string
	AssetVersionID    string
	DependencyType    string
	VisibilityOfUsage string
}

// canonicalUsages reads the whole asset_dependencies table, ordered: the
// chain makes statements about "the row this publication wrote" and about
// "every row in this database", and one reader answers both.
func canonicalUsages(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []canonicalUsage {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT project_id::text, asset_version_id::text, dependency_type, visibility_of_usage
		   FROM asset_dependencies
		  ORDER BY project_id, asset_version_id, dependency_type`)
	if err != nil {
		t.Fatalf("read asset_dependencies: %v", err)
	}
	defer rows.Close()
	var out []canonicalUsage
	for rows.Next() {
		var u canonicalUsage
		if err := rows.Scan(&u.ProjectID, &u.AssetVersionID, &u.DependencyType, &u.VisibilityOfUsage); err != nil {
			t.Fatalf("scan asset_dependencies: %v", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read asset_dependencies: %v", err)
	}
	return out
}

// canonicalIdentityCount counts the asset identities and version rows a
// project holds: the "nothing was written" assertion for the refusals, read
// from the database rather than inferred from a status code.
func canonicalIdentityCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, projectID string) (assets, versions int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT (SELECT count(*) FROM research_assets WHERE origin_project_id = $1),
		        (SELECT count(*) FROM research_asset_versions v
		           JOIN research_assets a ON a.id = v.asset_id
		          WHERE a.origin_project_id = $1)`, projectID).Scan(&assets, &versions); err != nil {
		t.Fatalf("count the project's identities: %v", err)
	}
	return assets, versions
}

// canonicalEdgeCount counts the stored lineage edges.
func canonicalEdgeCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	mustScanCount(t, ctx, pool, `SELECT count(*) FROM asset_lineage`, &n)
	return n
}

// --------------------------------------------------------------------------
// The chain

// TestAssetCanonicalE2E is the required test "asset canonical e2e": the
// release → dataset publish → cross-project dependency → fork/derive chain,
// hop by hop, over real PostgreSQL and the real HTTP surface, with the
// PID/version/rights/lineage acceptance read out of the stored rows.
//
// The subtests share one database and run in order, because they are one
// journey: the release the publish pins, the version the consumer pins, and
// the parent the derivation forks from are each produced by the subtest
// above. A later phase cannot be read as passing if an earlier one did not
// happen — that is the point of running them in one tree rather than as four
// independent suites that each build their own fixture.
func TestAssetCanonicalE2E(t *testing.T) {
	ctx := testCtx(t)
	w := newCanonicalWorld(t, ctx)

	alice, aliceID := signup(t, w.ts.URL, "canonical-alice@example.com", "canonical-alice")
	bob, bobID := signup(t, w.ts.URL, "canonical-bob@example.com", "canonical-bob")

	// ----------------------------------------------------------------------
	// The three projects of the chain, each through the real create route.
	//
	//   provider  — alice's, and the only one that releases: the chain starts
	//               at a release, so this is the project whose snapshot is
	//               frozen (docs/11 §1).
	//   consumer  — bob's: it depends on the provider's version and never
	//               touches the provider's project (docs/11 §5 Dependency).
	//   derivation— bob's too, and deliberately a THIRD project: a derivation
	//               creates a new identity in a project of its own, and
	//               folding it into the consumer would leave "the fork landed
	//               somewhere else" unasserted.
	//
	// The provider is public and so is the consumer: a pin into another
	// private project is the leak the preview refuses (docs/23 §4), and this
	// suite is about the permitted half.
	resp := alice.do(t, http.MethodPost, "/api/v1/organizations",
		`{"slug":"canonical-hub-labs","name":"Canonical Hub Labs"}`)
	mustStatus(t, resp, http.StatusCreated)
	var createdOrg orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&createdOrg); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	orgID := createdOrg.Organization.ID

	providerID := canonicalProject(t, alice, "canonical-provider", "Canonical Provider", "public", orgID)
	consumerID := canonicalProject(t, bob, "canonical-consumer", "Canonical Consumer", "public", "")
	derivationID := canonicalProject(t, bob, "canonical-derivation", "Canonical Derivation", "public", "")

	// The policy in force for the provider: the release gate pins a rights/
	// policy snapshot, and a scope with no policy contributes no pin (a
	// release without one is refused — that refusal is T0606's own case, and
	// it is asserted at tests/integration/release_e2e_test.go:411). The
	// public-asset IP review rule is deliberately NOT set: setting it true
	// blocks every public asset version, and no IP review exists in this
	// build (assetpublish.Command.requirePolicy).
	resp = alice.do(t, http.MethodPut, "/api/v1/organizations/"+orgID+"/policy",
		`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":2}}`)
	mustStatus(t, resp, http.StatusCreated)
	orgPolicy := decodePolicyVersion(t, resp)
	resp = alice.do(t, http.MethodPut, "/api/v1/projects/"+providerID+"/policy",
		`{"version":"v1","policy":{"main_protected":true,"release_min_reviewers":2}}`)
	mustStatus(t, resp, http.StatusCreated)
	providerPolicy := decodePolicyVersion(t, resp)

	aliceUser := domain.User{ID: aliceID}
	bobUser := domain.User{ID: bobID}

	mainBranchID, genesis := w.canonicalMainBranch(t, ctx, aliceUser, providerID)
	head1 := w.canonicalAcceptedState(t, ctx, aliceUser, providerID, mainBranchID, "canonical chain probe")
	researchBranch, err := w.rsg.CreateBranch(ctx, aliceUser, providerID, rsg.CreateBranchInput{
		Name:       "canonical-research-path",
		BaseRef:    genesis,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the fixture research branch: %v", err)
	}
	canonicalReviews(t, ctx, w.pool, providerID, mainBranchID, researchBranch.ID, genesis, head1, bobID)
	providerBlob := canonicalOpenBlob(t, ctx, w.pool, providerID, aliceID, genesis, "provider")

	consumerMainBranch, consumerGenesis := w.canonicalMainBranch(t, ctx, bobUser, consumerID)
	consumerHead := w.canonicalAcceptedState(t, ctx, bobUser, consumerID, consumerMainBranch, "consumer chain probe")
	consumerBlob := canonicalOpenBlob(t, ctx, w.pool, consumerID, bobID, consumerGenesis, "consumer")

	derivationMainBranch, derivationGenesis := w.canonicalMainBranch(t, ctx, bobUser, derivationID)
	derivationHead := w.canonicalAcceptedState(t, ctx, bobUser, derivationID, derivationMainBranch, "derivation chain probe")
	derivationBlob := canonicalOpenBlob(t, ctx, w.pool, derivationID, bobID, derivationGenesis, "derivation")

	// ----------------------------------------------------------------------
	// Hop 1 — Release.

	var release releaseE2EPayload
	var releaseManifest []byte

	t.Run("1. the release freezes the accepted snapshot main holds", func(t *testing.T) {
		resp := createRelease(t, alice, providerID, "v1.0.0", "Canonical provider release", "canonical-release-key-1")
		mustStatus(t, resp, http.StatusCreated)
		release = decodeRelease(t, resp)
		if release.ID == "" || release.Version != "v1.0.0" {
			t.Fatalf("created release = %+v", release)
		}
		// The frozen snapshot is MAIN'S HEAD at this instant, not "some
		// state": the row carries head1, which the fixture created.
		if release.StateID != head1 {
			t.Fatalf("release state_id = %s, want main's head %s", release.StateID, head1)
		}
		// The policy snapshot is pinned by id (both scopes), which is what
		// makes the release's rights/policy version reproducible.
		if release.PolicyVersionID == nil || *release.PolicyVersionID != providerPolicy.ID {
			t.Fatalf("project policy pin = %v, want %s", release.PolicyVersionID, providerPolicy.ID)
		}
		if release.OrgPolicyVersionID == nil || *release.OrgPolicyVersionID != orgPolicy.ID {
			t.Fatalf("org policy pin = %v, want %s", release.OrgPolicyVersionID, orgPolicy.ID)
		}
		var hash string
		var storedState string
		if err := w.pool.QueryRow(ctx,
			`SELECT manifest_hash, state_id::text FROM releases WHERE id = $1`, release.ID).Scan(&hash, &storedState); err != nil {
			t.Fatalf("read the stored release: %v", err)
		}
		if hash != release.ManifestHash || storedState != head1 {
			t.Fatalf("stored release = hash %s state %s, want %s / %s", hash, storedState, release.ManifestHash, head1)
		}
		releaseManifest, _ = getManifestBytes(t, alice, providerID, release.ID)
		if len(releaseManifest) == 0 {
			t.Fatal("the release manifest export is empty")
		}
	})

	t.Run("2. main moves on and the release does not", func(t *testing.T) {
		// Main advances: another accepted state becomes its head. This is the
		// subtest that gives the chain its first hop rather than merely its
		// first row — without the move, "the release pinned main's head" and
		// "the release pinned whatever main happens to be" are the same
		// observation.
		head2 := w.canonicalAcceptedState(t, ctx, aliceUser, providerID, mainBranchID, "canonical chain probe two")
		if head2 == head1 {
			t.Fatalf("main's head did not move: both states are %s", head1)
		}
		// The exported manifest of the OLD release is byte-identical to the
		// pre-move export...
		after, _ := getManifestBytes(t, alice, providerID, release.ID)
		if string(after) != string(releaseManifest) {
			t.Fatalf("the release manifest moved when main advanced:\nbefore: %s\nafter:  %s", releaseManifest, after)
		}
		// ...and the document still names the state it froze, not the new
		// head. Both halves are asserted because either alone is passable by
		// accident: bytes could repeat while the row drifted, and the row
		// could hold while the export re-rendered.
		var doc map[string]any
		if err := json.Unmarshal(after, &doc); err != nil {
			t.Fatalf("release manifest parse: %v", err)
		}
		if doc["state_id"] != head1 {
			t.Fatalf("release manifest state_id = %v, want the frozen %s (main's head is now %s)", doc["state_id"], head1, head2)
		}
		if got := canonicalScalar(t, ctx, w.pool, `SELECT state_id::text FROM releases WHERE id = $1`, release.ID); got != head1 {
			t.Fatalf("the release row's state_id = %s, want %s", got, head1)
		}
		if got := canonicalScalar(t, ctx, w.pool, `SELECT id::text FROM project_states WHERE id = $1`, head2); got != head2 {
			t.Fatalf("the new head did not land: %s", got)
		}
	})

	// ----------------------------------------------------------------------
	// Hop 2 — Dataset asset publish, under the release.

	providerPins := []string{"release:" + release.ID, "project:" + providerID}
	// Three versions of ONE asset, and they are what makes "pid is not
	// version" checkable: the same identity accumulates version rows, each
	// one immutable and each carrying its own rights declaration. The three
	// declarations are also the derivation's three verdicts (hop 4), which is
	// why they are published here rather than seeded: the verdict reads the
	// STORED bytes of a published version, so the way to have the three is to
	// publish the three.
	providerVersions := []struct {
		label       string
		derivatives rights.Permission
		title       string
		slug        string
	}{
		{"1.0.0", rights.PermissionAllowed, "Zeolite stability screening", "canonical-dataset"},
		{"1.1.0", rights.PermissionRestricted, "", ""},
		{"1.2.0", rights.PermissionUnspecified, "", ""},
	}

	var providerPID string
	var parentRowV100 string

	t.Run("3. the dataset publish mints the identity and stores the document it was sent", func(t *testing.T) {
		for i, v := range providerVersions {
			o := candidateOptions{
				// The FIRST publication creates the identity: an empty pid is
				// the create case, and the command mints one
				// (assetpublish/command.go: the pid is decided HERE and this
				// is the only place it is decided). The later two name the
				// pid the first one minted, so they add VERSIONS to it.
				pid:         providerPID,
				version:     v.label,
				visibility:  "public",
				refs:        providerPins,
				creators:    []string{aliceID},
				blobIDs:     []string{providerBlob},
				accessLevel: "open",
				dataAccess:  rights.DataAccessOpen,
				title:       v.title,
				slug:        v.slug,
			}
			cand := canonicalCandidate(t, v.derivatives, o)
			got := mustPublish(t, alice, providerID, cand.body(t), fmt.Sprintf("canonical-publish-%d", i))
			if got.Version != v.label {
				t.Fatalf("published version = %q, want %q", got.Version, v.label)
			}
			if i == 0 {
				providerPID = got.AssetPID
			} else if got.AssetPID != providerPID {
				t.Fatalf("version %s landed on pid %q, want the created identity %q", v.label, got.AssetPID, providerPID)
			}
			if got.Visibility != "public" || got.PublishedBy != aliceID {
				t.Fatalf("published version = %+v", got)
			}
			// The hash the wire reports IS the hash of the bytes the caller
			// sent, recomputed here with crypto/sha256 over the canonical
			// manifest — not trusted from the fixture's own builder.
			sum := sha256.Sum256(cand.cand.Manifest)
			if want := hex.EncodeToString(sum[:]); got.IntegrityHash != want {
				t.Fatalf("integrity_hash = %s, want the sha256 of the sent manifest %s", got.IntegrityHash, want)
			}
			rowID := canonicalVersionRow(t, ctx, w.pool, got.AssetPID, v.label)
			storedManifest := canonicalScalar(t, ctx, w.pool,
				`SELECT manifest::text FROM research_asset_versions WHERE id = $1`, rowID)
			// The stored document is the one that was sent, field for field:
			// jsonb equality is semantic, so a dropped or added field fails.
			if canonicalScalar(t, ctx, w.pool,
				`SELECT (manifest = $2::jsonb)::text FROM research_asset_versions WHERE id = $1`, rowID, string(cand.cand.Manifest)) != "true" {
				t.Fatalf("the stored manifest is not the document that was sent:\nstored: %s\nsent:   %s", storedManifest, cand.cand.Manifest)
			}
			// And the stored hash verifies over the STORED bytes through the
			// production verifier: parse, re-render canonically, digest. A
			// row whose manifest had been re-rendered by a different writer
			// would still verify, but one whose manifest and hash disagreed
			// could not.
			derived, err := assets.ManifestHash([]byte(storedManifest))
			if err != nil {
				t.Fatalf("the stored manifest of %s does not parse: %v", v.label, err)
			}
			if stored := canonicalScalar(t, ctx, w.pool,
				`SELECT integrity_hash FROM research_asset_versions WHERE id = $1`, rowID); stored != derived {
				t.Fatalf("stored integrity_hash = %s, want the stored manifest's own canonical hash %s", stored, derived)
			}
			// The rights declaration round-trips too: the derivation's
			// verdict reads these bytes, so "the declaration stored is the
			// declaration sent" is a precondition of hop 4 rather than a
			// decoration of hop 2.
			storedRights := canonicalScalar(t, ctx, w.pool,
				`SELECT rights_json::text FROM research_asset_versions WHERE id = $1`, rowID)
			doc, err := rights.Parse([]byte(storedRights))
			if err != nil {
				t.Fatalf("the stored rights declaration of %s does not parse: %v", v.label, err)
			}
			if doc.Usage.Derivatives != v.derivatives {
				t.Fatalf("stored usage.derivatives of %s = %q, want %q", v.label, doc.Usage.Derivatives, v.derivatives)
			}
		}

		// PID: a legal shape, unique, and a different thing from the version
		// label. The three assertions are separate because each can fail
		// alone: a pid can be well shaped and collide, and it can be unique
		// and equal to the label the caller sent.
		if len(providerPID) != assetPIDLen {
			t.Fatalf("pid = %q (%d characters), want %d", providerPID, len(providerPID), assetPIDLen)
		}
		for _, r := range providerPID {
			if !strings.ContainsRune(assetPIDAlphabet, r) {
				t.Fatalf("pid = %q carries %q, which is outside the storable alphabet %q", providerPID, r, assetPIDAlphabet)
			}
		}
		var samePID int
		mustScanCount(t, ctx, w.pool, `SELECT count(*) FROM research_assets WHERE pid = $1`, &samePID, providerPID)
		if samePID != 1 {
			t.Fatalf("research_assets rows carrying pid %s = %d, want exactly 1", providerPID, samePID)
		}
		if providerPID == "1.0.0" || strings.Contains(providerPID, "1.0.0") {
			t.Fatalf("pid %q is the version label: an identity that repeats a version is not an identity", providerPID)
		}
		// One identity, three versions — the pid names the ASSET.
		assetRow := canonicalAssetRow(t, ctx, w.pool, providerPID)
		var versions int
		mustScanCount(t, ctx, w.pool, `SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, &versions, assetRow)
		if versions != len(providerVersions) {
			t.Fatalf("version rows under pid %s = %d, want %d", providerPID, versions, len(providerVersions))
		}
		if got := canonicalScalar(t, ctx, w.pool,
			`SELECT origin_project_id::text FROM research_assets WHERE pid = $1`, providerPID); got != providerID {
			t.Fatalf("the identity's origin project = %s, want the publishing project %s", got, providerID)
		}
		parentRowV100 = canonicalVersionRow(t, ctx, w.pool, providerPID, "1.0.0")
	})

	t.Run("4. the published version remembers the release it came from", func(t *testing.T) {
		// The Release → publish link is a stored fact, not a convention: the
		// version row's source_release_id is resolved from the candidate's
		// origin refs (persistence/asset_publish_store.go sourceReleaseID),
		// and it is asserted against the release created in hop 1 by ROW ID.
		got := canonicalScalar(t, ctx, w.pool,
			`SELECT coalesce(source_release_id::text, '') FROM research_asset_versions WHERE id = $1`, parentRowV100)
		if got != release.ID {
			t.Fatalf("version 1.0.0 source_release_id = %q, want the release %s", got, release.ID)
		}
		if refs := canonicalScalar(t, ctx, w.pool,
			`SELECT array_to_string(origin_refs, ',') FROM research_asset_versions WHERE id = $1`, parentRowV100); !strings.Contains(refs, "release:"+release.ID) {
			t.Fatalf("origin_refs = %q, want the release pin among them", refs)
		}
		// The release's own manifest holds no asset, and that is the correct
		// reading of the two: a release freezes the RSG snapshot, an asset
		// version is published FROM it, and the two are separate immutable
		// things joined by the pin above (CLAUDE.md invariant 5).
		if !strings.Contains(string(releaseManifest), head1) {
			t.Fatalf("the release manifest does not name the state it froze: %s", releaseManifest)
		}
	})

	// ----------------------------------------------------------------------
	// Hop 3 — another project depends on the version.

	t.Run("5. another project's publish records the cross-project depends_on", func(t *testing.T) {
		pin := mustPin(t, providerPID, "1.0.0")
		consumer := canonicalCandidate(t, rights.PermissionAllowed, candidateOptions{
			pid:         "",
			version:     "0.1.0",
			visibility:  "public",
			refs:        []string{"state:" + consumerHead, "project:" + consumerID},
			pins:        []assets.DependencyPin{pin},
			creators:    []string{bobID},
			blobIDs:     []string{consumerBlob},
			accessLevel: "open",
			dataAccess:  rights.DataAccessOpen,
			title:       "MOF screening set",
			slug:        "canonical-consumer-set",
		})
		// The consumer publishes into ITS OWN project and pins the provider's
		// version. This is the cross-project half: the publisher is a
		// different user in a different project.
		got := mustPublish(t, bob, consumerID, consumer.body(t), "canonical-consumer-1")
		consumerRow := canonicalVersionRow(t, ctx, w.pool, got.AssetPID, "0.1.0")

		usages := canonicalUsages(t, ctx, w.pool)
		if len(usages) != 1 {
			t.Fatalf("asset_dependencies rows = %d (%+v), want exactly the one declaration this publish made", len(usages), usages)
		}
		u := usages[0]
		if u.ProjectID != consumerID {
			t.Fatalf("the usage row's project = %s, want the USING project %s (the table is keyed by consumer, internal/assets/usage.go)", u.ProjectID, consumerID)
		}
		if u.AssetVersionID != parentRowV100 {
			t.Fatalf("the usage row names version row %s, want the provider's 1.0.0 row %s (a pin names an exact version, never an asset)", u.AssetVersionID, parentRowV100)
		}
		if u.DependencyType != string(assets.DependencyTypeDependsOn) {
			t.Fatalf("dependency_type = %q, want %q", u.DependencyType, assets.DependencyTypeDependsOn)
		}
		// visibility_of_usage is the CONSUMER version's own visibility, not
		// the provider's and not a request field (internal/assets/usage.go:
		// it is a derivation, and there is no input a client could set).
		ownVisibility := canonicalScalar(t, ctx, w.pool,
			`SELECT visibility FROM research_asset_versions WHERE id = $1`, consumerRow)
		if u.VisibilityOfUsage != ownVisibility {
			t.Fatalf("visibility_of_usage = %q, want the publishing version's own %q", u.VisibilityOfUsage, ownVisibility)
		}
		// The pin is on the VERSION, and the same asset has two other
		// versions: a row keyed on the asset would be satisfied by either.
		if u.AssetVersionID == canonicalVersionRow(t, ctx, w.pool, providerPID, "1.1.0") {
			t.Fatal("the usage row resolved to 1.1.0")
		}
	})

	t.Run("6. the citation half is not expressible, and nothing here fabricates it", func(t *testing.T) {
		// internal/assets/usage.go:45-52: "Every pin is a dependency, never a
		// citation. […] A manifest has no field that means \"background
		// knowledge\": nothing in the document distinguishes a version that
		// was consulted from one that was used as an input, so a citation is
		// not expressible here and this rule does not invent one."
		//
		// So the chain's "reference/depend" is walked on its dependency half,
		// and the citation half is ASSERTED ABSENT rather than manufactured.
		// A 'references' row can only enter this database through a bare
		// INSERT (no product path writes one), and this subtest is what makes
		// such a row — the cheapest way to fake "the reference chain works" —
		// fail the suite instead of passing it.
		usages := canonicalUsages(t, ctx, w.pool)
		for _, u := range usages {
			if u.DependencyType != string(assets.DependencyTypeDependsOn) {
				t.Fatalf("asset_dependencies holds a %q row (%+v): 'references' is in the vocabulary "+
					"(internal/assets/dependency_type.go) but no product path writes it, so this row was not "+
					"produced by the chain under test. See internal/assets/usage.go:45-52 — the citation half of "+
					"docs/11 §5 is not expressible in V1 and this suite does not assert it.", u.DependencyType, u)
			}
		}
		// The vocabulary still admits it — the point is that the READERS can
		// render a value the WRITERS cannot produce, not that the value was
		// removed.
		found := false
		for _, known := range assets.AllDependencyTypes() {
			if known == assets.DependencyTypeReferences {
				found = true
			}
		}
		if !found {
			t.Fatalf("the vocabulary no longer admits %q — if that changed, the recorded reason (usage.go:44-53) changed too and this suite's note is stale", assets.DependencyTypeReferences)
		}
		_ = usages
	})

	// ----------------------------------------------------------------------
	// Hop 4 — fork/derive onto a new identity, with the lineage edge.

	var childPID string
	var childRow string

	t.Run("7. a derivation creates a new identity and records the edge to the parent version", func(t *testing.T) {
		beforeEdges := canonicalEdgeCount(t, ctx, w.pool)
		parentBefore := canonicalVersionRowText(t, ctx, w.pool, parentRowV100)

		child := canonicalCandidate(t, rights.PermissionAllowed, candidateOptions{
			pid:         "",
			version:     "1.0.0",
			visibility:  "public",
			refs:        []string{"state:" + derivationHead, "project:" + derivationID},
			creators:    []string{bobID},
			blobIDs:     []string{derivationBlob},
			accessLevel: "open",
			dataAccess:  rights.DataAccessOpen,
			title:       "Forked zeolite screening",
			slug:        "canonical-derived-dataset",
		})
		body := canonicalDeriveBody(t, child, providerPID+"@1.0.0", "forked_from", "")
		got := mustDerive(t, bob, derivationID, body, "canonical-derive-1")

		// The wire's own account, which is read back from the STORED
		// derivation rather than echoed from the request.
		if got.ParentAssetPID != providerPID || got.ParentVersion != "1.0.0" {
			t.Fatalf("the response names parent %s@%s, want %s@1.0.0", got.ParentAssetPID, got.ParentVersion, providerPID)
		}
		if got.Relation != string(assets.RelationForkedFrom) {
			t.Fatalf("relation = %q, want %q", got.Relation, assets.RelationForkedFrom)
		}
		// The NEW identity is new: a different pid, and not the parent's.
		childPID = got.AssetPID
		if childPID == providerPID {
			t.Fatalf("the derivation returned the parent's pid %s: no new identity was created", childPID)
		}
		if len(childPID) != assetPIDLen {
			t.Fatalf("the derived pid = %q (%d characters), want %d", childPID, len(childPID), assetPIDLen)
		}
		var samePID int
		mustScanCount(t, ctx, w.pool, `SELECT count(*) FROM research_assets WHERE pid = $1`, &samePID, childPID)
		if samePID != 1 {
			t.Fatalf("research_assets rows carrying the derived pid %s = %d, want exactly 1", childPID, samePID)
		}
		if got := canonicalScalar(t, ctx, w.pool,
			`SELECT origin_project_id::text FROM research_assets WHERE pid = $1`, childPID); got != derivationID {
			t.Fatalf("the derived identity's origin project = %s, want the target project %s", got, derivationID)
		}
		// The parent's asset gained NO version from this: derivative identity
		// is a new asset, not a new version of the parent.
		var parentVersions int
		mustScanCount(t, ctx, w.pool,
			`SELECT count(*) FROM research_asset_versions WHERE asset_id = $1`, &parentVersions, canonicalAssetRow(t, ctx, w.pool, providerPID))
		if parentVersions != len(providerVersions) {
			t.Fatalf("the parent asset holds %d versions after the derivation, want the %d it had", parentVersions, len(providerVersions))
		}
		childRow = canonicalVersionRow(t, ctx, w.pool, childPID, "1.0.0")

		// The EDGE, read from the table: one new row, both ends as version
		// row ids, the direction parent → child, and the relation the request
		// asked for.
		edges := lineageEdges(t, ctx, w.pool)
		if len(edges) != beforeEdges+1 {
			t.Fatalf("asset_lineage rows = %d, want %d", len(edges), beforeEdges+1)
		}
		var found bool
		for _, e := range edges {
			if e.childVersionID == childRow {
				if e.parentVersionID != parentRowV100 {
					t.Fatalf("the edge into %s has parent %s, want the parent VERSION row %s (an edge keyed by pid could not answer 'derived from WHAT' a version later)",
						childRow, e.parentVersionID, parentRowV100)
				}
				if e.relation != string(assets.RelationForkedFrom) {
					t.Fatalf("edge relation = %q, want %q", e.relation, assets.RelationForkedFrom)
				}
				found = true
			}
		}
		if !found {
			t.Fatalf("no lineage edge names the derived version %s; rows: %+v", childRow, edges)
		}

		// The parent version is unchanged, EVERY column of it. The
		// fingerprint above is the whole row (to_jsonb), so this is not a
		// comparison over the columns a reader remembered.
		if after := canonicalVersionRowText(t, ctx, w.pool, parentRowV100); after != parentBefore {
			t.Fatalf("the parent version changed under the derivation:\nbefore: %s\nafter:  %s", parentBefore, after)
		}
	})

	t.Run("8. the parent version and the edge are immutable at the database", func(t *testing.T) {
		// "Unchanged" and "could not have changed" are different findings,
		// and the second is the invariant (CLAUDE.md §9, invariant 5): the
		// append-only triggers of migrations 00014/00015 are read here by
		// attempting the mutations they forbid. A real refusal is required —
		// a statement that quietly affected zero rows would prove nothing.
		for _, tc := range []struct {
			name string
			sql  string
			args []any
		}{
			{"UPDATE the lineage edge's relation", `UPDATE asset_lineage SET relation_type = 'supersedes' WHERE child_asset_version_id = $1`, []any{childRow}},
			{"DELETE the lineage edge", `DELETE FROM asset_lineage WHERE parent_asset_version_id = $1`, []any{parentRowV100}},
			{"UPDATE the parent version's label", `UPDATE research_asset_versions SET version = '9.9.9' WHERE id = $1`, []any{parentRowV100}},
			{"DELETE the parent version", `DELETE FROM research_asset_versions WHERE id = $1`, []any{parentRowV100}},
		} {
			_, err := w.pool.Exec(ctx, tc.sql, tc.args...)
			if err == nil {
				t.Fatalf("%s was ACCEPTED by the database: history is not append-only there", tc.name)
			}
			// append_only_guard raises with ERRCODE = 'P0001' (migration
			// 00014). Asserting the SQLSTATE and not merely "an error" is
			// what tells the trigger apart from a typo in this statement.
			if code := sqlState(t, err); code != "P0001" {
				t.Fatalf("%s failed with SQLSTATE %s, want P0001 from append_only_guard: %v", tc.name, code, err)
			}
			if !strings.Contains(err.Error(), "append-only") {
				t.Fatalf("%s failed with an unexpected error: %v", tc.name, err)
			}
		}
		// The refusals changed nothing: the rows are where they were.
		if n := canonicalEdgeCount(t, ctx, w.pool); n != 1 {
			t.Fatalf("asset_lineage rows = %d after the refused mutations, want 1", n)
		}
		if got := canonicalScalar(t, ctx, w.pool,
			`SELECT version FROM research_asset_versions WHERE id = $1`, parentRowV100); got != "1.0.0" {
			t.Fatalf("the parent version label = %q after the refused mutation", got)
		}

		// The probe's POSITIVE CONTROL, without which the four refusals above
		// are only "these statements errored" and not "the guard refused
		// them": the same pool, the same Exec, against a table that is
		// deliberately NOT append-only is accepted. research_assets carries
		// no trigger (migration 00014's list is
		// scientific_object_versions, relation_versions, project_states,
		// state_commits, releases, research_asset_versions, asset_lineage,
		// policy_versions, validation_results, contribution_events,
		// audit_log, research_events, external_reference_snapshots) because
		// the identity's title and slug are editable metadata — the VERSIONS
		// are the history. An UPDATE here succeeding is what makes the
		// P0001 above a statement about the guard.
		if _, err := w.pool.Exec(ctx,
			`UPDATE research_assets SET slug = slug WHERE id = $1`,
			canonicalAssetRow(t, ctx, w.pool, providerPID)); err != nil {
			t.Fatalf("the control UPDATE on the deliberately mutable research_assets was refused, so the refusals above measure nothing about which tables are guarded: %v", err)
		}
	})

	t.Run("9. rights: allowed, unspecified and restricted are three different answers", func(t *testing.T) {
		identitiesBefore, versionsBefore := canonicalIdentityCount(t, ctx, w.pool, derivationID)
		edgesBefore := canonicalEdgeCount(t, ctx, w.pool)

		// (a) RESTRICTED — refused, and no confirmation can overrule it. The
		// parent's publisher said no (internal/assets.RequireDerivable).
		restricted := canonicalCandidate(t, rights.PermissionAllowed, candidateOptions{
			pid:         "",
			version:     "1.0.0",
			visibility:  "public",
			refs:        []string{"state:" + derivationHead, "project:" + derivationID},
			creators:    []string{bobID},
			blobIDs:     []string{derivationBlob},
			accessLevel: "open",
			dataAccess:  rights.DataAccessOpen,
			title:       "Forbidden fork",
			slug:        "canonical-forbidden-fork",
		})
		// The confirmation is SENT with it, so the refusal is about the
		// prohibition and not about a missing field.
		mustRefuseDerive(t, bob, derivationID,
			canonicalDeriveBody(t, restricted, providerPID+"@1.1.0", "derived_from", "I accept the terms"),
			"canonical-derive-restricted", http.StatusForbidden, assets.CodeDeriveDerivativesRestricted)

		// (b) UNSPECIFIED — neither permission nor prohibition: refused until
		// the caller says, explicitly, that it is deriving from a declaration
		// that says nothing.
		unspecified := canonicalCandidate(t, rights.PermissionAllowed, candidateOptions{
			pid:         "",
			version:     "1.0.0",
			visibility:  "public",
			refs:        []string{"state:" + derivationHead, "project:" + derivationID},
			creators:    []string{bobID},
			blobIDs:     []string{derivationBlob},
			accessLevel: "open",
			dataAccess:  rights.DataAccessOpen,
			title:       "Unstated fork",
			slug:        "canonical-unstated-fork",
		})
		mustRefuseDerive(t, bob, derivationID,
			canonicalDeriveBody(t, unspecified, providerPID+"@1.2.0", "derived_from", ""),
			"canonical-derive-unspecified", http.StatusForbidden, assets.CodeDeriveDerivativesUnspecified)

		// Every refusal above wrote NOTHING: no identity, no version, no
		// edge. Read from the database, not inferred from the status.
		if a, v := canonicalIdentityCount(t, ctx, w.pool, derivationID); a != identitiesBefore || v != versionsBefore {
			t.Fatalf("the refused derivations wrote: identities %d→%d, versions %d→%d", identitiesBefore, a, versionsBefore, v)
		}
		if n := canonicalEdgeCount(t, ctx, w.pool); n != edgesBefore {
			t.Fatalf("the refused derivations wrote %d lineage edges", n-edgesBefore)
		}

		// (c) The same unspecified parent, WITH the confirmation: permitted,
		// and the confirmation is recorded with the actor in the audit row —
		// so the record says "this person said so" rather than "the platform
		// decided silence meant yes".
		const confirmation = "deriving from a parent whose declaration is silent; accepting it as-is"
		confirmed := canonicalCandidate(t, rights.PermissionAllowed, candidateOptions{
			pid:         "",
			version:     "1.0.0",
			visibility:  "public",
			refs:        []string{"state:" + derivationHead, "project:" + derivationID},
			creators:    []string{bobID},
			blobIDs:     []string{derivationBlob},
			accessLevel: "open",
			dataAccess:  rights.DataAccessOpen,
			title:       "Confirmed fork",
			slug:        "canonical-confirmed-fork",
		})
		got := mustDerive(t, bob, derivationID,
			canonicalDeriveBody(t, confirmed, providerPID+"@1.2.0", "derived_from", confirmation),
			"canonical-derive-confirmed")
		if got.Relation != string(assets.RelationDerivedFrom) {
			t.Fatalf("relation = %q, want %q", got.Relation, assets.RelationDerivedFrom)
		}
		if got.AssetPID == providerPID || got.AssetPID == childPID {
			t.Fatalf("the confirmed derivation reused an identity: %s", got.AssetPID)
		}
		// The edge names ITS parent — 1.2.0, not 1.0.0. An implementation
		// that always recorded "the asset's first version" would pass every
		// assertion above and fail this one.
		confirmedRow := canonicalVersionRow(t, ctx, w.pool, got.AssetPID, "1.0.0")
		parentV120 := canonicalVersionRow(t, ctx, w.pool, providerPID, "1.2.0")
		var edgeFound bool
		for _, e := range lineageEdges(t, ctx, w.pool) {
			if e.childVersionID != confirmedRow {
				continue
			}
			edgeFound = true
			if e.parentVersionID != parentV120 {
				t.Fatalf("the confirmed derivation's parent = %s, want the version it actually derived from, %s", e.parentVersionID, parentV120)
			}
			if e.relation != string(assets.RelationDerivedFrom) {
				t.Fatalf("edge relation = %q, want %q", e.relation, assets.RelationDerivedFrom)
			}
		}
		if !edgeFound {
			t.Fatal("the confirmed derivation recorded no lineage edge")
		}
		// The audit row carries the verdict and the confirmation, in the
		// same transaction as the edge.
		audit := canonicalScalar(t, ctx, w.pool,
			`SELECT after_summary->>$2 FROM audit_log
			  WHERE project_id = $1 AND action = $3 AND after_summary->>'parent' = $4
			  ORDER BY occurred_at DESC, id DESC LIMIT 1`,
			derivationID, assets.DerivativesConfirmationField, assets.ActionAssetDerived, providerPID+"@1.2.0")
		if audit != confirmation {
			t.Fatalf("the audit row's %s = %q, want %q", assets.DerivativesConfirmationField, audit, confirmation)
		}
		if declared := canonicalScalar(t, ctx, w.pool,
			`SELECT after_summary->>'derivatives_declared' FROM audit_log
			  WHERE project_id = $1 AND action = $2 AND after_summary->>'parent' = $3
			  ORDER BY occurred_at DESC, id DESC LIMIT 1`,
			derivationID, assets.ActionAssetDerived, providerPID+"@1.2.0"); declared != string(rights.PermissionUnspecified) {
			t.Fatalf("the audit row's derivatives_declared = %q, want %q", declared, rights.PermissionUnspecified)
		}
	})

	t.Run("10. the chain reads back as one graph", func(t *testing.T) {
		// The last thing: the four hops are visible from the outside as one
		// graph rather than as four rows a test happens to know about. The
		// provider's page shows the fork, the fork's page shows its parent,
		// and the consumer's declared usage is readable from the consumer's
		// project — each through the route that serves it.
		parentPage := canonicalAssetPage(t, alice, providerPID, "1.0.0")
		if !pageCarriesChild(parentPage, childPID) {
			t.Fatalf("the provider's page for %s@1.0.0 does not show the derivation into %s:\n%s", providerPID, childPID, parentPage.raw)
		}
		childPage := canonicalAssetPage(t, bob, childPID, "1.0.0")
		if !strings.Contains(childPage.raw, providerPID) {
			t.Fatalf("the derived asset's page does not name its parent %s:\n%s", providerPID, childPage.raw)
		}
		raw, deps := fetchProjectDependencies(t, bob, consumerID)
		if len(deps.Dependencies) != 1 {
			t.Fatalf("the consumer's dependency read = %d entries, want the one declared usage:\n%s", len(deps.Dependencies), raw)
		}
		if deps.Dependencies[0].DependencyType != string(assets.DependencyTypeDependsOn) ||
			!strings.Contains(deps.Dependencies[0].Pin, providerPID+"") {
			t.Fatalf("the consumer's dependency entry = %+v, want the depends_on pin on %s", deps.Dependencies[0], providerPID)
		}
	})
}

// canonicalAssetPage reads one asset version's page through the real route
// and returns the raw bytes plus the decoded payload.
type canonicalPage struct {
	raw     string
	payload assetPagePayload
}

func canonicalAssetPage(t *testing.T, uc *testUserClient, pid, version string) canonicalPage {
	t.Helper()
	raw, payload := fetchAssetPage(t, uc, pid, version)
	return canonicalPage{raw: raw, payload: payload}
}

// pageCarriesChild reports whether a page's lineage block names the given
// child pid — read from the raw bytes, so the assertion is about the whole
// surface rather than about one field of one struct.
func pageCarriesChild(p canonicalPage, childPID string) bool {
	return strings.Contains(p.raw, childPID)
}
