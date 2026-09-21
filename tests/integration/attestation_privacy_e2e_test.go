package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/attestationhttp"
	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	appvalidation "github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// The T0812 acceptance test. T0812-TEST-01 "attestation privacy e2e".
//
//	go test ./tests/integration -run TestAttestationPrivacyE2E
//
// # What it is for
//
// The requirement is a NEGATIVE one — a private project may say "we validated
// this" about a public version without exposing the private evidence behind
// the statement — and a negative requirement is only met if it is measured
// by trying to break it. So every assertion below attacks the surface:
//
//   - it reads the public page ANONYMOUSLY and searches the raw bytes for
//     every identifier of the private side (the attesting project, its
//     branches, the basis state, the internal review, the two users), not
//     for the fields a rendering rule would have dropped;
//   - it walks the pid space to learn which attestations exist;
//   - it names a private version as the target, and a state belonging to
//     another project as the basis, and checks that the refusals say nothing
//     the caller did not already know.
//
// # The composition, and why it is this one
//
// Real PostgreSQL (testdb.Setup migrates a fresh database from
// infra/migrations, so 00120 and its three triggers are the ones under
// test), the real session/CSRF guard, the real org and project APIs (the
// fixture is created the way a person creates it), the real RSG service (the
// versions, states, pull request and review are committed through the same
// service production runs), the real attestation command over the real
// store, and a real HTTP client against a real server for the public read.
// Only the session and rate-limit stores are in-memory, exactly as
// cmd/api/main.go's e2e siblings compose theirs.
//
// # The fixture
//
// Two projects, and the split is the point:
//
//	attester — visibility "private", owned by att-labs: this is the project
//	           that makes the statement, and everything about it must stay
//	           out of the public page.
//	target   — visibility "public": it owns the protocol version the
//	           statement is ABOUT. Everything about it is public already,
//	           so the page naming it discloses nothing.
//
// Both belong to the same organization and the same owner, which is the
// HARDER case for the privacy claim: if the two projects were strangers, the
// page would leak nothing by accident. Here the attesting project's org is
// the target's org too, and the page still may not name it until the org's
// own standing setting permits it.
const attestationTaskID = "T0812"

// attestationWorld is the composed surface plus the fixture identifiers the
// privacy assertions search for.
type attestationWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
	// svc is the real RSG service, used only to SEED a believable project.
	svc *rsg.Service

	alice, bob     *testUserClient
	aliceID, bobID string
	orgID          string
	orgSlug        string
	orgName        string

	// attester is the PRIVATE project the statement comes from.
	attester *knowledgeProject
	// target is the project that owns the public version the statement is
	// about.
	target *knowledgeProject

	// The public protocol version the attestation names.
	targetObjectID  string
	targetVersionID string

	// The private footing: the attester's own object version, the state the
	// statement rests on, and the review that authorised it.
	attesterObjectID  string
	attesterVersionID string
	attesterStateID   string
	reviewID          string

	// A protocol version of the attester's OWN, inside its PRIVATE project:
	// the right KIND of target, on the wrong side of the visibility axis.
	// It is the case that separates "this is not something one may attest"
	// from "this is not public" (Judge's two separate refusals).
	privateProtocolVersionID string
	// privateAssetVersionID is the same case on the asset axis: an asset
	// version in the attester's own private project.
	privateAssetVersionID string
	// attesterPublicFlaggedAssetVersionID is the row this task's second
	// review round is about, on the attester's side of the wall: an asset
	// version whose OWN axis says 'public', inside a project that is
	// private. Two axes, one answer —
	// attestations.Facts.TargetIsPublic names both, because the platform
	// decides an asset's reachability by its origin project everywhere else
	// (the asset page's read gate, the asset feed's existence, the
	// subscription audience). The MEMBER half of the case: its owner may
	// read it, so it resolves, and Judge is what refuses it.
	attesterPublicFlaggedAssetVersionID string

	// The VICTIM: bob's own private project, holding a protocol version and
	// an asset version that alice may not read (she owns the attesting
	// project and is a member of neither project of bob's). This is the
	// fixture the refusal path used to leak — the caller is a legitimate
	// attestant in its own project and has no relation at all to this one.
	// Both titles are unmistakable strings, so the raw-body searches below
	// cannot pass by accident.
	victim                  *knowledgeProject
	victimProtocolObjectID  string
	victimProtocolVersionID string
	victimProtocolTitle     string
	victimAssetID           string
	victimAssetVersionID    string
	victimAssetTitle        string
	// victimPublicFlaggedAssetVersionID is the STRANGER half of the same
	// row: an asset version flagged 'public' inside bob's PRIVATE project.
	// Alice is not a member of that project, so she may not read the row —
	// and the version's own axis is exactly the fact that must not be
	// enough to hand it to her. Its title is unmistakable, so a raw-body
	// search for it cannot pass by accident.
	victimPublicFlaggedAssetID        string
	victimPublicFlaggedAssetVersionID string
	victimPublicFlaggedAssetTitle     string
	// strangerPublicVersionID is a protocol version in bob's OTHER project,
	// which is public: alice is a stranger to it and may read it anyway. It
	// is the second arm of the reader predicate — readable by the network —
	// and the fixture had no such target before, because both of the other
	// projects belong to the attester.
	strangerPublicVersionID string
	// strangerPublicAssetVersionID is the asset-side positive control in
	// that same public project: public on BOTH axes. It is what stops the
	// two-axis rule from collapsing into "an asset is never a public
	// target" — the tightening has to still admit the asset the network
	// really can reach, or the refusals above would pass for the wrong
	// reason.
	strangerPublicAssetVersionID string
}

// newAttestationWorld composes the production tree over one test database,
// the way cmd/api/main.go composes it, and stops at the surfaces this task
// touches.
func newAttestationWorld(t *testing.T, ctx context.Context) *attestationWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), attestationTaskID)

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
	orgAPI := orgshttp.New(orgshttp.Deps{Store: orgStore})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: projectStore,
		Orgs:  orgStore,
		Authz: authz.NewMatrixEngine(),
	})
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	svc := rsg.NewService(rsg.Deps{
		Projects:  projectAPI.Service(),
		Branches:  branches.NewService(persistence.NewBranchStore(pool)),
		States:    states.NewService(stateStore, appvalidation.NewGuard(rsgvalidation.NewValidator(reg), persistence.NewValidationTxProbe())),
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
		Evidence:  persistence.NewEvidenceStore(pool),
	})

	mux := http.NewServeMux()
	authAPI.Register(mux)
	mux.Handle("/api/v1/organizations", orgAPI.Routes())
	mux.Handle("/api/v1/organizations/", orgAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())

	// The attestation surface, composed exactly as cmd/api/main.go composes
	// it: the command over the project membership port (the same project
	// store), the attestation store and the permission matrix; the public
	// read over that same store.
	attestationStore := persistence.NewAttestationStore(pool)
	attestationhttp.New(attestationhttp.Deps{
		Attest: attestations.NewCommand(attestations.Deps{
			Members: projectStore,
			Store:   attestationStore,
			Authz:   authz.NewMatrixEngine(),
		}),
		Read: attestationStore,
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &attestationWorld{ts: ts, pool: pool, svc: svc}
}

// ---------------------------------------------------------------- fixture

// seed builds the whole fixture through the real APIs.
func (w *attestationWorld) seed(t *testing.T, ctx context.Context) {
	t.Helper()
	w.alice, w.aliceID = signup(t, w.ts.URL, "att-alice@example.com", "att-alice")
	w.bob, w.bobID = signup(t, w.ts.URL, "att-bob@example.com", "att-bob")

	w.orgSlug, w.orgName = "att-labs", "Attestation Labs"
	resp := w.alice.do(t, http.MethodPost, "/api/v1/organizations",
		fmt.Sprintf(`{"slug":%q,"name":%q}`, w.orgSlug, w.orgName))
	mustStatus(t, resp, http.StatusCreated)
	var created orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	w.orgID = created.Organization.ID

	// The private attester, and the public project whose version it attests.
	w.attester = w.newProject(t, ctx, w.alice, w.aliceID, "att-private-lab", "private", w.orgID)
	w.target = w.newProject(t, ctx, w.alice, w.aliceID, "att-public-methods", "public", w.orgID)

	// The public protocol version: an object target is public when the
	// version inherits its project's visibility (no pinned policy) and the
	// owning project is public. This fixture relies on exactly that
	// (attestations.Facts.TargetIsPublic).
	w.targetObjectID, w.targetVersionID, _ = w.seedObject(t, ctx, w.target, "protocol",
		"A published digestion protocol the network may read")

	// The private footing. The object is whatever the project had to commit
	// to reach a state; what matters is the STATE, its pull request and the
	// approved review of it.
	w.attesterObjectID, w.attesterVersionID, w.attesterStateID = w.seedObject(t, ctx, w.attester,
		"dataset", "The internal workbench dataset the validation rests on")
	// A protocol of the attester's own, in the same private project: the
	// kind is attestable, the visibility is not.
	_, w.privateProtocolVersionID, _ = w.seedObject(t, ctx, w.attester,
		"protocol", "An internal protocol that has never been published")
	pr := w.seedPR(t, ctx, w.attester, w.attesterStateID)
	w.reviewID = w.approve(t, ctx, pr, w.attesterStateID, "scientific")

	// The asset-side counterpart of the attester's own private protocol: an
	// asset version in the attester's own private project, which its owner
	// may read and the network may not.
	_, w.privateAssetVersionID = w.seedAssetVersion(t, ctx, w.attester,
		"An internal asset that has never been published", "private", w.aliceID)
	// The same private project, holding an asset version whose OWN axis is
	// 'public'. research_asset_versions.visibility is written by the
	// publish command and says only that about the version; the project's
	// preset is the other axis, and it is the one the platform reads an
	// asset's reachability from. Alice may read this row as a member, so it
	// reaches Judge — which is exactly where the second axis has to be
	// applied.
	_, w.attesterPublicFlaggedAssetVersionID = w.seedAssetVersion(t, ctx, w.attester,
		"An internal asset flagged public inside a private project", "public", w.aliceID)

	// The victim. Bob creates his own project — a PERSONAL one, so there is
	// no organization and no membership that could accidentally explain
	// alice's answer — and holds two private versions in it: one of each
	// attested kind. Bob reads his own rows (he is their owner), alice does
	// not, and that asymmetry is what the subtests below measure.
	w.victim = w.newProject(t, ctx, w.bob, w.bobID, "att-victim-lab", "private", "")
	w.victimProtocolTitle = "PROBE-VICTIM-SECRET-TITLE"
	w.victimProtocolObjectID, w.victimProtocolVersionID, _ = w.seedObjectAs(t, ctx, w.bobID, w.victim,
		"protocol", w.victimProtocolTitle)
	w.victimAssetTitle = "PROBE-VICTIM-SECRET-ASSET-TITLE"
	w.victimAssetID, w.victimAssetVersionID = w.seedAssetVersion(t, ctx, w.victim,
		w.victimAssetTitle, "private", w.bobID)
	// And the same private project's asset version flagged 'public' on its
	// own axis. Nothing in the platform makes these two rows equivalent:
	// the origin project is private, so the asset's page is not readable by
	// a non-member and its feed does not exist. The stranger half of the
	// case is that alice may not read THIS row either, version axis
	// notwithstanding.
	w.victimPublicFlaggedAssetTitle = "PROBE-VICTIM-PUBLIC-FLAGGED-ASSET-TITLE"
	w.victimPublicFlaggedAssetID, w.victimPublicFlaggedAssetVersionID = w.seedAssetVersion(t, ctx, w.victim,
		w.victimPublicFlaggedAssetTitle, "public", w.bobID)

	// Bob's other project, PUBLIC, and the version in it. Alice is a stranger
	// to this one too, so a target here resolves for her only if the reader
	// predicate's other arm works: the version is readable by the network.
	stranger := w.newProject(t, ctx, w.bob, w.bobID, "att-stranger-methods", "public", "")
	_, w.strangerPublicVersionID, _ = w.seedObjectAs(t, ctx, w.bobID, stranger,
		"protocol", "A published method anybody may attest")
	// The asset-side positive: an asset version public on BOTH axes, in a
	// project alice is a stranger to. Without it, every asset assertion in
	// this file would be a refusal, and a rule that refused every asset
	// target would pass all of them.
	_, w.strangerPublicAssetVersionID = w.seedAssetVersion(t, ctx, stranger,
		"A published asset anybody may attest", "public", w.bobID)
}

// newProject creates one project through the real API as owner, and seeds
// main plus one research branch. organizationID "" creates a PERSONAL
// project (projects.organization_id is nullable), which is what the victim
// project is: nobody but its owner is in it.
func (w *attestationWorld) newProject(t *testing.T, ctx context.Context, owner *testUserClient, ownerID, slug, visibility, organizationID string) *knowledgeProject {
	t.Helper()
	org := "null"
	if organizationID != "" {
		org = fmt.Sprintf("%q", organizationID)
	}
	resp := owner.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":"attestation fixture","visibility":%q,"organization_id":%s}`,
			slug, slug, visibility, org))
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	id := created.Project.ID
	main := w.createBranch(t, ctx, ownerID, id, "", "main")
	var genesis string
	if err := w.pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, id).Scan(&genesis); err != nil {
		t.Fatalf("read the genesis state of %s: %v", slug, err)
	}
	return &knowledgeProject{id: id, main: main, probe: w.createBranch(t, ctx, ownerID, id, genesis, "probe"), genesis: genesis}
}

// createBranch seeds one branch through the real RSG service, as actorID —
// the project's owner, because the service authorizes the call.
func (w *attestationWorld) createBranch(t *testing.T, ctx context.Context, actorID, projectID, baseRef, name string) string {
	t.Helper()
	b, err := w.svc.CreateBranch(ctx, domain.User{ID: actorID}, projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseRef,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch %q in %s: %v", name, projectID, err)
	}
	return b.ID
}

// seedObject commits one object version on the project's research branch
// through the real RSG service, as its owner.
func (w *attestationWorld) seedObject(t *testing.T, ctx context.Context, p *knowledgeProject, objectType, purpose string) (string, string, string) {
	t.Helper()
	return w.seedObjectAs(t, ctx, w.aliceID, p, objectType, purpose)
}

// seedObjectAs is seedObject with the actor named: the RSG service
// authorizes the write, so a version in bob's project has to be committed by
// bob. The purpose string becomes the version's stored title
// (rsg.titleFromPayload prefers "purpose").
func (w *attestationWorld) seedObjectAs(t *testing.T, ctx context.Context, actorID string, p *knowledgeProject, objectType, purpose string) (string, string, string) {
	t.Helper()
	res, err := w.svc.CreateObject(ctx, domain.User{ID: actorID}, p.id, p.probe, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(fmt.Sprintf(`{"purpose":%q}`, purpose)),
	})
	if err != nil {
		t.Fatalf("CreateObject %s on branch %s: %v", objectType, p.probe, err)
	}
	return res.Object.ID, res.Version.ID, res.Version.StateID
}

// seedAssetVersion inserts one research asset and one version row directly
// and returns (assetID, versionID).
//
// It is raw SQL rather than the asset publish command for the same reason
// seedPR and approve are: the asset publish surface is a different task's
// subject with its own e2e, and this fixture needs exactly the two rows the
// attestation target read joins. Writing them here also keeps the test
// honest about what it measures — the TARGET READ is under test, not the
// publish path that would have written the row.
func (w *attestationWorld) seedAssetVersion(t *testing.T, ctx context.Context, p *knowledgeProject, title, visibility, publishedBy string) (string, string) {
	t.Helper()
	var assetID string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		 VALUES ('dataset', $1, $2, $3) RETURNING id`,
		assetSlugFor(title), title, p.id).Scan(&assetID); err != nil {
		t.Fatalf("seed asset %q: %v", title, err)
	}
	var versionID string
	if err := w.pool.QueryRow(ctx,
		`INSERT INTO research_asset_versions
		     (asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by, origin_refs)
		 VALUES ($1, 'v1', '{"purpose":"attestation fixture"}'::jsonb, '{}'::jsonb, $2,
		         'sha256:attestation-fixture', $3, ARRAY['project:' || $4::text])
		 RETURNING id`,
		assetID, visibility, publishedBy, p.id).Scan(&versionID); err != nil {
		t.Fatalf("seed asset version %q: %v", title, err)
	}
	return assetID, versionID
}

// assetSlugFor derives a slug from a fixture title. research_assets.slug has
// no uniqueness constraint (only research_assets.pid does), so the only
// requirement is that it is stable and readable.
func assetSlugFor(title string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-':
			b.WriteRune('-')
		}
	}
	return strings.Trim(strings.ReplaceAll(b.String(), "---", "-"), "-")
}

// seedPR seeds one pull_request row proposing proposedState to main from the
// project's research branch, and returns its id.
func (w *attestationWorld) seedPR(t *testing.T, ctx context.Context, p *knowledgeProject, proposedState string) string {
	t.Helper()
	row, err := sqlc.New(w.pool).CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(p.id),
		Number:          1,
		SourceBranchID:  parseUUIDOrDie(p.probe),
		TargetBranchID:  parseUUIDOrDie(p.main),
		BaseStateID:     parseUUIDOrDie(p.genesis),
		ProposedStateID: parseUUIDOrDie(proposedState),
		Title:           "internal review fixture",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(w.aliceID),
	})
	if err != nil {
		t.Fatalf("seed research PR for %s: %v", p.id, err)
	}
	return pgUUIDTextTest(row.ID)
}

// approve seeds one approved review row on a PR and returns its id.
func (w *attestationWorld) approve(t *testing.T, ctx context.Context, prID, reviewedState, kind string) string {
	t.Helper()
	row, err := sqlc.New(w.pool).CreateReview(ctx, sqlc.CreateReviewParams{
		PullRequestID:   parseUUIDOrDie(prID),
		ReviewerID:      parseUUIDOrDie(w.bobID),
		ReviewKind:      kind,
		Decision:        "approved",
		ReviewedStateID: parseUUIDOrDie(reviewedState),
		Responsibility:  "",
		Body:            "",
	})
	if err != nil {
		t.Fatalf("seed %s review on %s: %v", kind, prID, err)
	}
	return pgUUIDTextTest(row.ID)
}

// ------------------------------------------------------------- the calls

// attestBody is the request body of the publish pair.
func (w *attestationWorld) attestBody(target, validationType, result, orgVisibility, basisState, review string) string {
	return fmt.Sprintf(
		`{"target":%q,"validation_type":%q,"validation_result":%q,"org_visibility":%q,"basis_state_id":%q,"internal_review_id":%q}`,
		target, validationType, result, orgVisibility, basisState, review)
}

// publish drives one authenticated publish and returns the response; callers
// assert its status themselves, because half of this file is about refusals.
func (w *attestationWorld) publish(t *testing.T, projectID, body string) *http.Response {
	t.Helper()
	return w.alice.do(t, http.MethodPost,
		"/api/v1/projects/"+projectID+"/attestations:publish", body)
}

// mustPublish publishes and returns the pid, failing on anything but 201.
func (w *attestationWorld) mustPublish(t *testing.T, projectID, body string) string {
	t.Helper()
	resp := w.publish(t, projectID, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("attestation publish = %d, want 201: %s", resp.StatusCode, readAll(t, resp))
	}
	var out struct {
		PID              string `json:"pid"`
		ValidationType   string `json:"validation_type"`
		ValidationResult string `json:"validation_result"`
		OrgVisibility    string `json:"org_visibility"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("attestation publish payload: %v", err)
	}
	return out.PID
}

// anonymousGet reads one attestation as a caller with NO session at all —
// a fresh client, no cookies — and returns the status and the RAW body. The
// raw bytes are what the privacy assertion searches: a body that is decoded
// into a struct first could only prove what the struct has fields for.
func (w *attestationWorld) anonymousGet(t *testing.T, pid string) (int, string) {
	t.Helper()
	anon := newTestUserClient(w.ts.URL)
	resp := anon.do(t, http.MethodGet, "/api/v1/attestations/"+pid, "")
	return resp.StatusCode, readAll(t, resp)
}

// mustReadAnonymous reads one attestation anonymously and returns the body.
func (w *attestationWorld) mustReadAnonymous(t *testing.T, pid string) string {
	t.Helper()
	status, body := w.anonymousGet(t, pid)
	if status != http.StatusOK {
		t.Fatalf("anonymous attestation read = %d, want 200: %s", status, body)
	}
	return body
}

// publicAttestationWire is the decoded public projection.
type publicAttestationWire struct {
	PID              string `json:"pid"`
	ValidationType   string `json:"validation_type"`
	ValidationResult string `json:"validation_result"`
	CreatedAt        string `json:"created_at"`
	Target           struct {
		Kind      string `json:"kind"`
		ObjectID  string `json:"object_id"`
		VersionID string `json:"version_id"`
		Title     string `json:"title"`
	} `json:"target"`
	AttributedBy struct {
		Mode         string `json:"mode"`
		Organization *struct {
			ID   string `json:"id"`
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"organization"`
	} `json:"attributed_by"`
	Disclosure struct {
		IsEvidence    bool   `json:"is_evidence"`
		NamesEvidence bool   `json:"names_evidence"`
		Statement     string `json:"statement"`
	} `json:"disclosure"`
}

// privateIdentifiers is every string that identifies the attesting side, in
// the order the failure message should list them. The public page must
// contain NONE of them.
func (w *attestationWorld) privateIdentifiers() []struct{ what, value string } {
	return []struct{ what, value string }{
		{"the attesting project's id", w.attester.id},
		{"the attesting project's main branch", w.attester.main},
		{"the attesting project's research branch", w.attester.probe},
		{"the attesting project's genesis state", w.attester.genesis},
		{"the attesting project's own object", w.attesterObjectID},
		{"the attesting project's own version", w.attesterVersionID},
		{"the attesting project's unpublished protocol", w.privateProtocolVersionID},
		{"the attesting project's asset flagged public inside it", w.attesterPublicFlaggedAssetVersionID},
		{"the basis state the statement rests on", w.attesterStateID},
		{"the internal review that authorised the statement", w.reviewID},
		{"the attesting project's slug", "att-private-lab"},
		{"the authorizing reviewer's user id", w.bobID},
	}
}

// ---------------------------------------------------------------- the test

// TestAttestationPrivacyE2E is T0812-TEST-01.
func TestAttestationPrivacyE2E(t *testing.T) {
	ctx := context.Background()
	w := newAttestationWorld(t, ctx)
	w.seed(t, ctx)

	targetRef := "object_version:" + w.targetVersionID

	t.Run("a private project may attest a public version, and the public page names no part of it", func(t *testing.T) {
		pid := w.mustPublish(t, w.attester.id, w.attestBody(
			targetRef, "reproduction", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))

		if !pidShape.MatchString(pid) {
			t.Fatalf("pid %q is not a pid", pid)
		}

		body := w.mustReadAnonymous(t, pid)
		var got publicAttestationWire
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatalf("public attestation body: %v (%s)", err, body)
		}

		// The positive half: the statement is readable, and it is about the
		// public version.
		if got.PID != pid {
			t.Errorf("pid = %q, want %q", got.PID, pid)
		}
		if got.ValidationType != "reproduction" || got.ValidationResult != "confirmed" {
			t.Errorf("validation = %s/%s, want reproduction/confirmed", got.ValidationType, got.ValidationResult)
		}
		if got.Target.Kind != "protocol" {
			t.Errorf("target kind = %q, want protocol (the kind is the target's own object_type)", got.Target.Kind)
		}
		if got.Target.VersionID != w.targetVersionID || got.Target.ObjectID != w.targetObjectID {
			t.Errorf("target = %s/%s, want %s/%s",
				got.Target.ObjectID, got.Target.VersionID, w.targetObjectID, w.targetVersionID)
		}

		// The negative half, over the RAW bytes: no identifier of the private
		// side appears anywhere in what an anonymous reader is handed. This is
		// the search a decoded-struct assertion cannot do — a field the
		// projection dropped and a field the projection never had look the
		// same once the body is unmarshalled.
		for _, id := range w.privateIdentifiers() {
			if id.value == "" {
				t.Fatalf("fixture is incomplete: %s is empty", id.what)
			}
			if strings.Contains(body, id.value) {
				t.Errorf("the public attestation body discloses %s (%q):\n%s", id.what, id.value, body)
			}
		}
		// The organization is not named on an anonymous attestation.
		if strings.Contains(body, w.orgSlug) || strings.Contains(body, w.orgName) || strings.Contains(body, w.orgID) {
			t.Errorf("the anonymous attestation names its organization:\n%s", body)
		}
		if got.AttributedBy.Mode != attestations.OrgVisibilityAnonymous || got.AttributedBy.Organization != nil {
			t.Errorf("attributed_by = %+v, want anonymous with no organization", got.AttributedBy)
		}

		// "attestation != public evidence" is stated IN the document, so a
		// reader that knows nothing about the platform still learns it.
		if got.Disclosure.IsEvidence || got.Disclosure.NamesEvidence {
			t.Errorf("disclosure claims evidence: %+v", got.Disclosure)
		}
		if got.Disclosure.Statement == "" {
			t.Error("the record carries no statement about what it is")
		}
	})

	t.Run("the two 404s are one 404", func(t *testing.T) {
		// A pid that resolves to nothing and a segment that is not a pid at
		// all must answer the SAME body, or the route can be walked to learn
		// which pids exist.
		unknownStatus, unknownBody := w.anonymousGet(t, "0000000000000000000000zzzz")
		notAPidStatus, notAPidBody := w.anonymousGet(t, "not-a-pid-at-all")
		if unknownStatus != http.StatusNotFound || notAPidStatus != http.StatusNotFound {
			t.Fatalf("statuses = %d / %d, want 404 / 404", unknownStatus, notAPidStatus)
		}
		var a, b errorEnvelope
		if err := json.Unmarshal([]byte(unknownBody), &a); err != nil {
			t.Fatalf("unknown-pid body: %v", err)
		}
		if err := json.Unmarshal([]byte(notAPidBody), &b); err != nil {
			t.Fatalf("not-a-pid body: %v", err)
		}
		if a.Code != b.Code || a.Message != b.Message || a.Code != attestations.CodeAttestationNotFound {
			t.Errorf("the two 404s differ:\n unknown pid : %s\n not a pid   : %s", unknownBody, notAPidBody)
		}
	})

	t.Run("naming the organization needs the organization's own standing setting", func(t *testing.T) {
		// The organization has never opted in, so asking for `named` is
		// refused — and refused by the server-side re-run, with the report
		// the caller can act on.
		before := w.attestationRows(t, w.attester.id)
		resp := w.publish(t, w.attester.id, w.attestBody(
			targetRef, "method_validation", "inconclusive",
			attestations.OrgVisibilityNamed, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("named publish with an anonymous organization = %d, want 409: %s",
				resp.StatusCode, readAll(t, resp))
		}
		raw := readAll(t, resp)
		var refused struct {
			Code    string   `json:"code"`
			Reasons []string `json:"reasons"`
		}
		if err := json.Unmarshal([]byte(raw), &refused); err != nil {
			t.Fatalf("refusal body: %v (%s)", err, raw)
		}
		if refused.Code != attestations.CodeAttestationRefused {
			t.Errorf("refusal code = %q, want %q", refused.Code, attestations.CodeAttestationRefused)
		}
		if !hasReasonCode(refused.Reasons, attestations.ReasonAttributionNotPermitted) {
			t.Errorf("refusal does not name %s: %v", attestations.ReasonAttributionNotPermitted, refused.Reasons)
		}
		if n := w.attestationRows(t, w.attester.id); n != before {
			t.Errorf("a refused attestation moved the row count from %d to %d", before, n)
		}

		// The organization opts in, through its own governance surface.
		w.setOrgAttribution(t, "named")

		namedPID := w.mustPublish(t, w.attester.id, w.attestBody(
			targetRef, "method_validation", "confirmed",
			attestations.OrgVisibilityNamed, w.attesterStateID, w.reviewID))
		body := w.mustReadAnonymous(t, namedPID)
		var named publicAttestationWire
		if err := json.Unmarshal([]byte(body), &named); err != nil {
			t.Fatalf("named attestation body: %v (%s)", err, body)
		}
		if named.AttributedBy.Mode != attestations.OrgVisibilityNamed {
			t.Fatalf("attributed_by.mode = %q, want named (%s)", named.AttributedBy.Mode, body)
		}
		if named.AttributedBy.Organization == nil ||
			named.AttributedBy.Organization.Slug != w.orgSlug {
			t.Fatalf("attributed_by.organization = %+v, want the att-labs organization", named.AttributedBy.Organization)
		}

		// Naming the organization must NOT have widened anything else: the
		// private identifiers are still absent from the page that names it.
		for _, id := range w.privateIdentifiers() {
			if strings.Contains(body, id.value) {
				t.Errorf("naming the organization also disclosed %s (%q):\n%s", id.what, id.value, body)
			}
		}

		// The organization narrows again. Both attestations are unchanged —
		// the table is append-only — and BOTH read as anonymous, including
		// the one issued while the organization was named. A standing
		// setting is a floor that can be lowered, and lowering it retracts
		// the disclosure it exists to control.
		w.setOrgAttribution(t, "anonymous")
		for _, pid := range []string{namedPID} {
			narrowed := w.mustReadAnonymous(t, pid)
			var got publicAttestationWire
			if err := json.Unmarshal([]byte(narrowed), &got); err != nil {
				t.Fatalf("narrowed body: %v", err)
			}
			if got.AttributedBy.Mode != attestations.OrgVisibilityAnonymous || got.AttributedBy.Organization != nil {
				t.Errorf("after the organization went anonymous, %s still reads %+v:\n%s", pid, got.AttributedBy, narrowed)
			}
			if strings.Contains(narrowed, w.orgSlug) || strings.Contains(narrowed, w.orgName) {
				t.Errorf("after the organization went anonymous, %s still names it:\n%s", pid, narrowed)
			}
		}
	})

	t.Run("a target that is not public is refused, and nothing is written", func(t *testing.T) {
		before := w.attestationRows(t, w.attester.id)
		// A Protocol — an attestable kind — inside the attester's OWN
		// PRIVATE project. Publishing an attestation about it would be the
		// first disclosure of what it names, which is the one thing this
		// surface must never do: the statement is public, so its target has
		// to be public already.
		resp := w.publish(t, w.attester.id, w.attestBody(
			"object_version:"+w.privateProtocolVersionID, "method_validation", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("private-target publish = %d, want 409: %s", resp.StatusCode, readAll(t, resp))
		}
		raw := readAll(t, resp)
		var refused struct {
			Code    string   `json:"code"`
			Reasons []string `json:"reasons"`
		}
		if err := json.Unmarshal([]byte(raw), &refused); err != nil {
			t.Fatalf("refusal body: %v (%s)", err, raw)
		}
		if !hasReasonCode(refused.Reasons, attestations.ReasonTargetNotPublic) {
			t.Errorf("refusal does not name %s: %v", attestations.ReasonTargetNotPublic, refused.Reasons)
		}
		if strings.Contains(raw, w.targetVersionID) {
			t.Errorf("the refusal of a private target mentions another target:\n%s", raw)
		}

		// And the KIND is a separate refusal, reported separately: a version
		// of a type the requirement does not name is not attestable at all,
		// whether or not it is public. The two are different answers because
		// a caller can fix one of them and not the other.
		resp = w.publish(t, w.attester.id, w.attestBody(
			"object_version:"+w.attesterVersionID, "data_audit", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("non-attestable-kind publish = %d, want 409: %s", resp.StatusCode, readAll(t, resp))
		}
		raw = readAll(t, resp)
		if err := json.Unmarshal([]byte(raw), &refused); err != nil {
			t.Fatalf("refusal body: %v (%s)", err, raw)
		}
		if !hasReasonCode(refused.Reasons, attestations.ReasonTargetKindNotAttestable) {
			t.Errorf("refusal does not name %s: %v", attestations.ReasonTargetKindNotAttestable, refused.Reasons)
		}
		// Neither refusal wrote a row: the count is taken as a delta rather
		// than against a literal, so the assertion cannot drift when an
		// earlier case publishes one more.
		if n := w.attestationRows(t, w.attester.id); n != before {
			t.Errorf("refused attestations moved the row count from %d to %d", before, n)
		}
	})

	t.Run("a version in another user's private project is not found, and says nothing about it", func(t *testing.T) {
		// WHO IS ASKING. Alice owns the attesting project, so she is a
		// legitimate attestant: the command authorizes her before it reads
		// anything. What she has no relation to is bob's project — she is not
		// a member, and every row in it is private. This is the caller the
		// defect was measured with, and the reason it was measured with it:
		// against her OWN private version the surface behaved correctly, so
		// an assertion that only used her own project proved the rule
		// exactly where she already owns the data.
		//
		// WHAT WAS WRONG. The target read had no reader predicate, so a
		// version she may not read still resolved to facts, Judge refused
		// it, and the refusal handed those facts back in the 409 body: the
		// title, the object id, the version id and the owning project's
		// visibility. An id that named nothing got a 404. So the response
		// was both a disclosure and an existence oracle over every private
		// version in the database, reachable by any project owner.
		//
		// WHAT IS NOW TRUE, and why the comparison below is 404 against 404
		// rather than 409 against 404. A target the reader may not read
		// resolves to NO ROW — the read is reader-relative — so the answer
		// is the not-found answer, produced by the same code path a random
		// uuid takes. There is no 409 for this input to compare anything
		// with: the refusal that carries the preview is unreachable for a
		// target the caller cannot read, and THAT is the property under
		// test. A 409-vs-404 comparison would pass for a body that merely
		// dropped a field; this one cannot pass unless the two responses are
		// the same document.
		unknown := "object_version:00000000-0000-4000-8000-000000000000"

		// The control first: an id that names nothing, published against.
		controlResp := w.publish(t, w.attester.id, w.attestBody(
			unknown, "reproduction", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if controlResp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown target publish = %d, want 404: %s", controlResp.StatusCode, readAll(t, controlResp))
		}
		controlRaw := readAll(t, controlResp)
		control := envelopeFields(t, controlRaw)
		if control["code"] != attestations.CodeTargetNotFound {
			t.Fatalf("unknown target code = %v, want %q: %s", control["code"], attestations.CodeTargetNotFound, controlRaw)
		}

		// The three ways to name a version alice may not read: bob's
		// protocol version, bob's asset version, and — through the preview
		// route, which goes through the same resolution — bob's protocol
		// version again. Each must be that same document.
		foreign := []struct {
			what, route, target string
		}{
			{"a protocol version in another user's private project", "publish", "object_version:" + w.victimProtocolVersionID},
			{"an asset version in another user's private project", "publish", "asset_version:" + w.victimAssetVersionID},
			{"the same protocol version through the preview route", "publish-preview", "object_version:" + w.victimProtocolVersionID},
		}
		for _, tc := range foreign {
			body := w.attestBody(tc.target, "reproduction", "confirmed",
				attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID)
			path := "/api/v1/projects/" + w.attester.id + "/attestations:" + tc.route
			resp := w.alice.do(t, http.MethodPost, path, body)
			raw := readAll(t, resp)

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s (%s) = %d, want 404 — a refusal here is the disclosure, and its preview is the carrier:\n%s",
					tc.what, tc.route, resp.StatusCode, raw)
				continue
			}
			// Field by field, against the answer an id that names nothing
			// gets: not "the same fields are missing", the same document.
			if got := envelopeFields(t, raw); !reflect.DeepEqual(got, control) {
				t.Errorf("%s (%s) is distinguishable from a target that does not exist:\n unreadable : %v\n nonexistent: %v\n raw: %s",
					tc.what, tc.route, got, control, raw)
			}
			// And the fields themselves: no title, no id of either end, no
			// slug. This is the search the reviewer's probe did — the
			// response must not carry the victim's title, the victim
			// object's id, the victim version's id, the victim project's id
			// or slug, or either of the victim's own version labels.
			for _, secret := range []string{
				w.victimProtocolTitle, w.victimAssetTitle,
				w.victimProtocolVersionID, w.victimProtocolObjectID,
				w.victimAssetVersionID, w.victimAssetID,
				w.victim.id, "att-victim-lab", w.bobID,
			} {
				if strings.Contains(raw, secret) {
					t.Errorf("%s (%s) discloses %q:\n%s", tc.what, tc.route, secret, raw)
				}
			}
			// A 404 with a preview attachment would still be a refusal, so
			// say it explicitly rather than trusting the equality above to
			// have caught a nested field.
			if strings.Contains(raw, "\"preview\"") || strings.Contains(raw, "\"reasons\"") {
				t.Errorf("%s (%s) answered with a refusal report:\n%s", tc.what, tc.route, raw)
			}
		}

		// THE OTHER DIRECTION, and it is what makes the assertions above
		// mean "not yours" rather than "not public": the same caller, the
		// same request shape, against a private version in her OWN project.
		// She may read that one, so the target resolves, Judge refuses it
		// for the reason the caller can act on, and the refusal names the
		// version — which it may, because it is hers.
		for _, tc := range []struct{ what, target string }{
			{"her own private protocol version", "object_version:" + w.privateProtocolVersionID},
			{"her own private asset version", "asset_version:" + w.privateAssetVersionID},
		} {
			resp := w.publish(t, w.attester.id, w.attestBody(tc.target, "reproduction", "confirmed",
				attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
			if resp.StatusCode != http.StatusConflict {
				t.Errorf("%s = %d, want 409 — the gate is reader-relative, not a blanket refusal: %s",
					tc.what, resp.StatusCode, readAll(t, resp))
				continue
			}
			raw := readAll(t, resp)
			var refused struct {
				Reasons []string `json:"reasons"`
			}
			if err := json.Unmarshal([]byte(raw), &refused); err != nil {
				t.Fatalf("%s refusal body: %v (%s)", tc.what, err, raw)
			}
			if !hasReasonCode(refused.Reasons, attestations.ReasonTargetNotPublic) {
				t.Errorf("%s: refusal does not name %s: %v", tc.what, attestations.ReasonTargetNotPublic, refused.Reasons)
			}
			id := strings.TrimPrefix(tc.target, "object_version:")
			id = strings.TrimPrefix(id, "asset_version:")
			if !strings.Contains(raw, id) {
				t.Errorf("%s: the refusal of a version the caller may read does not name it:\n%s", tc.what, raw)
			}
		}

		// THE SECOND ARM of the same predicate, and the reason it is asserted
		// here rather than left to the first subtest: EVERY OTHER TARGET in
		// this fixture sits in a project alice owns, so the reader predicate
		// would be satisfied by membership alone throughout the file. A gate
		// that had quietly become "your own projects only" would pass all of
		// that and still be wrong — attesting somebody else's public work is
		// the entire point of the surface. Bob's public project is the
		// stranger case: alice is a member of neither of his projects, and
		// this target resolves for the only other reason there is.
		pid := w.mustPublish(t, w.attester.id, w.attestBody(
			"object_version:"+w.strangerPublicVersionID, "data_audit", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		body := w.mustReadAnonymous(t, pid)
		var shared publicAttestationWire
		if err := json.Unmarshal([]byte(body), &shared); err != nil {
			t.Fatalf("stranger-target body: %v (%s)", err, body)
		}
		if shared.Target.VersionID != w.strangerPublicVersionID {
			t.Errorf("the public read of a stranger's attestation names target %q, want %q:\n%s",
				shared.Target.VersionID, w.strangerPublicVersionID, body)
		}
		for _, secret := range []string{w.victimProtocolTitle, w.victimAssetTitle, w.victim.id} {
			if strings.Contains(body, secret) {
				t.Errorf("attesting one public version disclosed another project's data (%q):\n%s", secret, body)
			}
		}
	})

	t.Run("an asset version flagged public inside a private project is not a public target", func(t *testing.T) {
		// THE ROW. research_asset_versions.visibility is written by the
		// publish command and it can say 'public' while the project that
		// ORIGINATED the asset is private. Those two rows must not be
		// equivalent, because the platform decides an asset's reachability
		// by its origin project in three other places: the asset page's
		// read gate (cmd/api/assetshttp/page.go reads the origin project and
		// answers 404 for a non-member), the asset feed's existence
		// (GetFeedAsset, whose comment names the same rule) and the
		// subscription audience (T1002). A target predicate that stopped at
		// the version's own axis would call a row network-visible that all
		// three of those refuse to show — and the public read renders
		// ra.title AS asset_title (ResolvePublicAttestation), so what would
		// be disclosed is that private project's asset title, on the open
		// network, to a caller who is a stranger to it.
		//
		// Both halves are asserted below, and they fail for DIFFERENT
		// reasons under the old predicate: the stranger's half because the
		// read handed the row over, the member's half because Judge read
		// only one axis and admitted the statement.

		// (1) THE STRANGER. Bob's private project, which alice is not a
		// member of. The control first: an asset-version id that names
		// nothing. Then the same request for the flagged version, through
		// both routes. A 404 that differs in any field, or that carries the
		// victim's title, is the disclosure this rule exists to prevent —
		// and it is also the existence oracle: a 409 here would say "this
		// version exists, and here is what it is".
		unknownAsset := "asset_version:00000000-0000-4000-8000-000000000000"
		controlResp := w.publish(t, w.attester.id, w.attestBody(
			unknownAsset, "reproduction", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if controlResp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown asset target publish = %d, want 404: %s",
				controlResp.StatusCode, readAll(t, controlResp))
		}
		controlRaw := readAll(t, controlResp)
		control := envelopeFields(t, controlRaw)
		if control["code"] != attestations.CodeTargetNotFound {
			t.Fatalf("unknown asset target code = %v, want %q: %s",
				control["code"], attestations.CodeTargetNotFound, controlRaw)
		}

		for _, route := range []string{"publish", "publish-preview"} {
			what := "a public-flagged asset version in another user's private project (" + route + ")"
			body := w.attestBody("asset_version:"+w.victimPublicFlaggedAssetVersionID,
				"reproduction", "confirmed",
				attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID)
			resp := w.alice.do(t, http.MethodPost,
				"/api/v1/projects/"+w.attester.id+"/attestations:"+route, body)
			raw := readAll(t, resp)

			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("%s = %d, want 404 — the version's own axis is not the asset's reachability, and a refusal here discloses the row:\n%s",
					what, resp.StatusCode, raw)
				continue
			}
			if got := envelopeFields(t, raw); !reflect.DeepEqual(got, control) {
				t.Errorf("%s is distinguishable from a target that does not exist:\n flagged    : %v\n nonexistent: %v\n raw: %s",
					what, got, control, raw)
			}
			for _, secret := range []string{
				w.victimPublicFlaggedAssetTitle,
				w.victimPublicFlaggedAssetVersionID,
				w.victimPublicFlaggedAssetID,
				w.victim.id, "att-victim-lab", w.bobID,
			} {
				if strings.Contains(raw, secret) {
					t.Errorf("%s discloses %q:\n%s", what, secret, raw)
				}
			}
			if strings.Contains(raw, "\"preview\"") || strings.Contains(raw, "\"reasons\"") {
				t.Errorf("%s answered with a refusal report instead of the not-found document:\n%s", what, raw)
			}
		}

		// (2) THE MEMBER. The same kind of row — an asset version flagged
		// 'public' — inside alice's OWN private project. She may read it as
		// a member, so the reader predicate is satisfied and the row reaches
		// Judge. That is where the second axis has to be applied, and the
		// answer is a refusal with a code the caller can act on rather than
		// a 404: the caller owns the row, so there is nothing left to hide
		// from her, and "your project is private" is the thing she can fix.
		resp := w.publish(t, w.attester.id, w.attestBody(
			"asset_version:"+w.attesterPublicFlaggedAssetVersionID, "reproduction", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("publish against a public-flagged version in the caller's own private project = %d, want 409: %s",
				resp.StatusCode, readAll(t, resp))
		}
		raw := readAll(t, resp)
		var refused struct {
			Reasons []string `json:"reasons"`
		}
		if err := json.Unmarshal([]byte(raw), &refused); err != nil {
			t.Fatalf("refusal body: %v (%s)", err, raw)
		}
		if !hasReasonCode(refused.Reasons, attestations.ReasonTargetNotPublic) {
			t.Errorf("refusal does not name %s: %v", attestations.ReasonTargetNotPublic, refused.Reasons)
		}
		// And the refusal names the axis that FAILED. This version's own
		// visibility is 'public', so a sentence asserting otherwise would be
		// false about a fact the caller can read on the row itself — which
		// is the difference between a refusal a caller can act on and one
		// that sends her looking at the wrong column.
		if !strings.Contains(raw, "originated it is") {
			t.Errorf("the refusal of a public-flagged version inside a private project does not name the project axis:\n%s", raw)
		}
		if strings.Contains(raw, "research_asset_versions.visibility is not") {
			t.Errorf("the refusal claims the version's own axis is not public, and it is 'public':\n%s", raw)
		}

		// (3) THE POSITIVE CONTROL, and the reason the two refusals above
		// mean "not this row" rather than "not assets". Bob's public
		// project, an asset version public on BOTH axes, a caller who is a
		// member of neither of his projects: the statement publishes, and
		// the anonymous read renders the asset title — which is the
		// projection this task deliberately does NOT change, and which is
		// correct precisely because this asset's origin project is public.
		// Without this case, a rule that refused every asset target would
		// pass every assertion above.
		pid := w.mustPublish(t, w.attester.id, w.attestBody(
			"asset_version:"+w.strangerPublicAssetVersionID, "data_audit", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		body := w.mustReadAnonymous(t, pid)
		var shared publicAttestationWire
		if err := json.Unmarshal([]byte(body), &shared); err != nil {
			t.Fatalf("public-asset-target body: %v (%s)", err, body)
		}
		if shared.Target.Kind != string(attestations.TargetKindAsset) {
			t.Errorf("the public read of an asset attestation reports kind %q, want %q:\n%s",
				shared.Target.Kind, attestations.TargetKindAsset, body)
		}
		if shared.Target.VersionID != w.strangerPublicAssetVersionID {
			t.Errorf("the public read names target %q, want %q:\n%s",
				shared.Target.VersionID, w.strangerPublicAssetVersionID, body)
		}
		if !strings.Contains(body, "A published asset anybody may attest") {
			t.Errorf("the public read of a genuinely public asset target does not render its title:\n%s", body)
		}
		// And attesting one public asset discloses nothing about the
		// private ones: the flagged row and the victim project stay out.
		for _, secret := range []string{
			w.victimPublicFlaggedAssetTitle, w.victimAssetTitle,
			w.victimPublicFlaggedAssetVersionID, w.victim.id,
		} {
			if strings.Contains(body, secret) {
				t.Errorf("the public read of a public target discloses %q:\n%s", secret, body)
			}
		}
	})

	t.Run("a basis state of another project is not found, not forbidden", func(t *testing.T) {
		// The public target project's genesis state is a real state the
		// caller can see — and it is NOT the attester's. The answer must be
		// the same one an id that does not exist gets: a caller must not be
		// able to ask "does project X have a state Y" through this route.
		resp := w.publish(t, w.attester.id, w.attestBody(
			targetRef, "data_audit", "confirmed",
			attestations.OrgVisibilityAnonymous, w.target.genesis, w.reviewID))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("foreign basis state = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
		}
		mustEnvelope(t, resp, attestations.CodeBasisStateNotFound)

		// Same for a review of another project: one review exists on a pull
		// request of the attester, so naming the target project's own review
		// position is answered as unknown.
		resp = w.publish(t, w.attester.id, w.attestBody(
			targetRef, "data_audit", "confirmed",
			attestations.OrgVisibilityAnonymous, w.attesterStateID, "00000000-0000-4000-8000-000000000000"))
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown internal review = %d, want 404: %s", resp.StatusCode, readAll(t, resp))
		}
		mustEnvelope(t, resp, attestations.CodeInternalReviewNeeded)
	})

	t.Run("attesting is a session-bearing governance write", func(t *testing.T) {
		anon := newTestUserClient(w.ts.URL)
		resp := anon.do(t, http.MethodPost, "/api/v1/projects/"+w.attester.id+"/attestations:publish",
			w.attestBody(targetRef, "reproduction", "confirmed",
				attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("anonymous publish = %d, want 401: %s", resp.StatusCode, readAll(t, resp))
		}

		// A member of neither project: bob is an account, not a project
		// member, so the matrix refuses him — and the refusal is the
		// existence-hiding project answer, not a disclosure that the project
		// exists and he may not attest in it.
		resp = w.bob.do(t, http.MethodPost, "/api/v1/projects/"+w.attester.id+"/attestations:publish",
			w.attestBody(targetRef, "reproduction", "confirmed",
				attestations.OrgVisibilityAnonymous, w.attesterStateID, w.reviewID))
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("non-member publish = %d, want 403 or 404: %s", resp.StatusCode, readAll(t, resp))
		}
	})
}

// setOrgAttribution flips the organization's standing setting through the
// real PATCH route, as its owner.
func (w *attestationWorld) setOrgAttribution(t *testing.T, value string) {
	t.Helper()
	resp := w.alice.do(t, http.MethodPatch, "/api/v1/organizations/"+w.orgID,
		fmt.Sprintf(`{"attestation_attribution":%q}`, value))
	mustStatus(t, resp, http.StatusOK)
	var got struct {
		AttestationAttribution string `json:"attestation_attribution"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("org update payload: %v", err)
	}
	if got.AttestationAttribution != value {
		t.Fatalf("attestation_attribution = %q after setting %q", got.AttestationAttribution, value)
	}
}

// attestationRows counts the attestations rows the attesting project holds.
// It reads the table directly because a refused publish must be shown to
// have written NOTHING, and the public read cannot answer that.
func (w *attestationWorld) attestationRows(t *testing.T, projectID string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM attestations WHERE attesting_project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count attestations: %v", err)
	}
	return n
}

// envelopeFields decodes an error body into every field it carries, except
// request_id — the one value that legitimately differs between two
// responses.
//
// It decodes into a MAP rather than the shared errorEnvelope struct, and
// that is the point: a struct can only show the fields it has, so comparing
// two structs proves nothing about a body that grew a `preview` or a
// `reasons` attachment (the 409's shape) alongside the envelope the probe
// decoded. Comparing maps compares the documents.
//
// It also requires request_id to be PRESENT, so the comparison it feeds is
// known to be over two real per-request envelopes rather than over two
// bodies that happened to be empty.
func envelopeFields(t *testing.T, raw string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("error body is not one JSON object: %v (%s)", err, raw)
	}
	if _, ok := body["request_id"]; !ok {
		t.Fatalf("error body carries no request_id, so a comparison over it would prove less than it claims: %s", raw)
	}
	delete(body, "request_id")
	return body
}

// hasReasonCode reports whether one of the refusal lines carries the code.
// The lines are "<code>: <detail>" (attestationRefusalReasonLines).
func hasReasonCode(lines []string, code string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, code+": ") {
			return true
		}
	}
	return false
}
