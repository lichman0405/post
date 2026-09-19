// Task T0805 required test "knowledge publish tests" — the end-to-end half.
//
// The unit suites pin the command's decisions
// (internal/application/knowledgepublish: the fail-closed audience rule, the
// refusal report, the validation order, the replay rules, the pid) and the
// transport's wiring (cmd/api/knowledgehttp: the three routes, the session
// guard, the project read gate on the preview, the envelope). This file pins
// what only a real PostgreSQL and the whole composed path can settle — the
// acceptance criteria of the task, each over real rows:
//
//  1. knowledge_publications HAS A WRITER: a publish lands a row AND records
//     a knowledge.version_published research event — asserted by reading the
//     event table, never by the HTTP status alone.
//
//  2. 发布 ≠ 公开 (owner ruling L3-20260916-1 #1): publishing records a
//     state and does not widen anybody's visibility. Three combinations are
//     pinned over real rows, and the first is the one a project-visibility-
//     only rule would answer wrongly:
//     (a) a version that pins ITS OWN visibility policy, in a PUBLIC project
//     — publishable, and afterwards the network is answered the
//     existence-hiding 404 while the project's members read it;
//     (b) an inherit-visibility version in a PRIVATE project — publishable
//     (docs/12 §2), and network-invisible afterwards;
//     (c) a version in a PUBLIC project whose rights declaration pins a
//     metadata visibility of its own — publishable, and members-only,
//     because the rights axis is an axis too.
//
//  3. A SECOND PUBLICATION OF THE SAME VERSION IS REFUSED (ruling #3), even
//     under a different public_version — which the table's own
//     UNIQUE(object_version_id, public_version) would have allowed, so the
//     refusal is the application's and this test is what pins it.
//
//  4. public_version IS STORED VERBATIM (ruling #2): a deliberately odd but
//     legal name is compared BYTE FOR BYTE in the database.
//
//  5. PERMISSION IS FAIL-CLOSED: the owner publishes, a maintainer is
//     refused (the matrix row publish_private_to_public has `conditional`
//     for maintainer, and a condition no specification defines is a denial —
//     authz.Permits admits only `allow`, issue #237), a signed-in stranger is
//     refused with the SAME answer, and an anonymous caller is refused before
//     the command runs.
//
//  6. THE PID IS STABLE: 26 lowercase Crockford base32 characters, and it
//     does not change when the project is renamed and moved to another
//     organization — the acceptance 00064 records for assets, for
//     publications.
//
//  7. NO PUBLISHED WITHOUT REVIEW (docs/43 §Publication): a version whose
//     state carries no approved scientific+integrity review record in main's
//     lineage is refused, and the same version publishes once the record
//     exists — which is what makes "the review is what gated it" a fact
//     rather than an intention.
//
//  8. A RETRACTED VERSION IS NOT PUBLISHED KNOWLEDGE: a version the
//     repository currently holds as retracted (lifecycle_state = 'aborted')
//     is refused, and the SAME version reopened (docs/43: aborted →
//     reopened) publishes — the rule is about the state the version is in,
//     not about its history.
//
//  9. THE PUBLIC READ IS NOT AN EXISTENCE ORACLE: a members-only publication
//     answers a signed-in non-member the 404 an unknown pid gets, BYTE FOR
//     BYTE — for a version that pins its own visibility in a public project
//     (ErrMemberNotFound) and for one in a private project
//     (ErrProjectNotFound) alike.
//
// # The fixtures, and the one column no V1 surface writes
//
// The version and the review record are seeded through the production RSG
// service and the production store (a research branch, an object version, a
// research PR targeting main, its two approved reviews) — the same recipe
// the release e2e suite uses, because the publication reads the SAME review
// record the release gate reads.
//
// The visibility axis (case (a) above) is the exception: NOTHING in V1
// writes scientific_object_versions.visibility_policy_id (it is in no
// CreateObject input and no service path), so the fixture pins it with an
// INSERT that copies the version the RSG service just wrote — the table is
// append-only (migration 00014), so an UPDATE is impossible by design and a
// copied row IS how a pinned version is expressed. That the column has
// readers but no writer is recorded in the RESULT as a follow-up: it is a
// gap in the product surface, not in this task.
//
// The composition is cmd/api/main.go's: the real auth guard, the real
// knowledge publish command over the real store, the real public read, the
// real project read gate, the real permission matrix over the canonical CSV.
// Only the session store is in-memory, exactly as the release and asset
// publish e2e suites compose it.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/evidencehttp"
	"github.com/lichman0405/post/cmd/api/knowledgehttp"
	"github.com/lichman0405/post/cmd/api/orgshttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/rsghttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/evidencegraph"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
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
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// knowledgePublishTaskID namespaces this task's test databases
// (test_T0805_<run_id>).
const knowledgePublishTaskID = "T0805"

// knowledgeVersionPublishedEvent is the machine-readable event name
// (specs/events/event-types.yaml) — NOT docs/18's older
// `knowledge.published`, which is the spelling this task replaced.
const knowledgeVersionPublishedEvent = "knowledge.version_published"

// pidShape is the persistent-identifier format as the database CHECK spells
// it and as internal/domain.PID encodes it: 26 characters, lowercase
// Crockford base32 (no i, l, o, u).
var pidShape = regexp.MustCompile(`^[0-9a-hjkmnp-tv-z]{26}$`)

// --------------------------------------------------------------------------
// The composed surface

// knowledgeWorld is the publication path as production wires it.
type knowledgeWorld struct {
	ts   *httptest.Server
	pool *pgxpool.Pool
	// svc is the real RSG service, used only to SEED a believable project
	// (branches, objects, versions, states): the path under test never asks
	// it anything.
	svc *rsg.Service

	alice, bob, carol *testUserClient
	aliceID, bobID    string
	carolID           string
	orgID             string
}

// newKnowledgeWorld composes the production tree over one test database,
// exactly as cmd/api/main.go does.
func newKnowledgeWorld(t *testing.T, ctx context.Context) *knowledgeWorld {
	t.Helper()
	return newKnowledgeWorldFor(t, ctx, knowledgePublishTaskID)
}

// newKnowledgeWorldFor is the same composition over a named test database, so
// a later task's suite (T0806's evidence network) composes the SAME world —
// one wiring, so a test cannot pass against a tree production does not
// build — while keeping its own database namespace (docs/66 §3).
func newKnowledgeWorldFor(t *testing.T, ctx context.Context, taskID string) *knowledgeWorld {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)

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
			// Every case signs up from one address; raise the budget so a case
			// tests the route rather than the signup limiter.
			SignupLimitPerIP: 10000,
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
		// The evidence-assertion write (T0806) is part of the same RSG
		// service, so the seed helper and the evidence route below run the
		// production command over the production store.
		Evidence: persistence.NewEvidenceStore(pool),
	})

	mux := http.NewServeMux()
	mux.Handle("/api/v1/auth/", authAPI.Routes())
	mux.Handle("/api/v1/organizations", orgAPI.Routes())
	mux.Handle("/api/v1/organizations/", orgAPI.Routes())
	mux.Handle("/api/v1/projects", projectAPI.Routes())
	mux.Handle("/api/v1/projects/", projectAPI.Routes())
	publishStore := persistence.NewKnowledgePublishStore(pool)
	knowledgehttp.New(knowledgehttp.Deps{
		Publish: knowledgepublish.NewCommand(knowledgepublish.Deps{
			Members: projectStore,
			Store:   publishStore,
			Authz:   authz.NewMatrixEngine(),
		}),
		Read: publishStore,
		// The read gate and the membership read are the SAME project service
		// (cmd/api/main.go wires them the same way).
		Projects: projectAPI.Service(),
		Members:  projectAPI.Service(),
		// The published version's evidence network (T0806), over the same
		// store the evidence write uses.
		Evidence: persistence.NewEvidenceStore(pool),
	}).Register(mux)
	// The RSG write surface, which carries the contract's evidence-assertion
	// route: the ONE write path an assertion has.
	rsghttp.New(rsghttp.Deps{Service: svc}).Register(mux)
	// The evidence-graph read (T0506): the same project gate, the same object
	// and relation stores, over the evidence table — composed here the way
	// cmd/api/main.go composes it.
	evidencehttp.New(evidencehttp.Deps{
		Service: evidencegraph.New(evidencegraph.Deps{
			Objects:    persistence.NewScientificObjectStore(pool),
			Assertions: persistence.NewEvidenceGraphStore(pool),
			Relations:  persistence.NewRelationStore(pool),
		}),
		Gate: projectAPI.Service(),
	}).Register(mux)

	ts := httptest.NewServer(authAPI.Guard(mux))
	t.Cleanup(ts.Close)
	return &knowledgeWorld{ts: ts, pool: pool, svc: svc}
}

// signups creates the accounts and the organization every case builds on.
func (w *knowledgeWorld) signups(t *testing.T) {
	t.Helper()
	w.alice, w.aliceID = signup(t, w.ts.URL, "kp-alice@example.com", "kp-alice")
	w.bob, w.bobID = signup(t, w.ts.URL, "kp-bob@example.com", "kp-bob")
	w.carol, w.carolID = signup(t, w.ts.URL, "kp-carol@example.com", "kp-carol")
	w.orgID = w.createOrg(t, "kp-labs", "Knowledge Publish Labs")
}

// createOrg creates one organization through the real API and returns its id.
func (w *knowledgeWorld) createOrg(t *testing.T, slug, name string) string {
	t.Helper()
	resp := w.alice.do(t, http.MethodPost, "/api/v1/organizations",
		fmt.Sprintf(`{"slug":%q,"name":%q}`, slug, name))
	mustStatus(t, resp, http.StatusCreated)
	var created orgResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("org create payload: %v", err)
	}
	return created.Organization.ID
}

// knowledgeProject is one seeded project: its id, its main branch (the
// review read resolves the lineage against it), its research branch and the
// genesis state.
type knowledgeProject struct {
	id      string
	main    string
	probe   string
	genesis string
}

// newProject creates a project through the real API (alice = owner, so she
// may publish in it) and seeds main plus one research branch.
func (w *knowledgeWorld) newProject(t *testing.T, ctx context.Context, slug, visibility string) *knowledgeProject {
	t.Helper()
	resp := w.alice.do(t, http.MethodPost, "/api/v1/projects",
		fmt.Sprintf(`{"slug":%q,"name":%q,"purpose":"knowledge publication","visibility":%q,"organization_id":%q}`,
			slug, slug, visibility, w.orgID))
	mustStatus(t, resp, http.StatusCreated)
	var created projectResponse
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("project create payload: %v", err)
	}
	id := created.Project.ID
	main := w.createBranch(t, ctx, id, "", "main")
	var genesis string
	if err := w.pool.QueryRow(ctx,
		`SELECT id FROM project_states WHERE project_id = $1 AND parent_state_id IS NULL`, id).Scan(&genesis); err != nil {
		t.Fatalf("read the genesis state of %s: %v", slug, err)
	}
	return &knowledgeProject{id: id, main: main, probe: w.createBranch(t, ctx, id, genesis, "probe"), genesis: genesis}
}

// createBranch seeds one branch through the real RSG service.
func (w *knowledgeWorld) createBranch(t *testing.T, ctx context.Context, projectID, baseRef, name string) string {
	t.Helper()
	b, err := w.svc.CreateBranch(ctx, domain.User{ID: w.aliceID}, projectID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseRef,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch %q in %s: %v", name, projectID, err)
	}
	return b.ID
}

// seedVersion creates one research_question — a knowledge object type
// docs/03 defines as publishable — on the given branch through the real RSG
// service, and returns (objectID, versionID, stateID).
func (w *knowledgeWorld) seedVersion(t *testing.T, ctx context.Context, projectID, branchID, statement string) (string, string, string) {
	t.Helper()
	res, err := w.svc.CreateObject(ctx, domain.User{ID: w.aliceID}, projectID, branchID, rsg.CreateObjectInput{
		ObjectType: "research_question",
		// statement + purpose + question_state are the fields the schema
		// requires of a research_question at draft.
		Payload: json.RawMessage(fmt.Sprintf(
			`{"statement":%q,"purpose":%q,"question_state":"open"}`,
			statement+"?", "answer "+statement)),
	})
	if err != nil {
		t.Fatalf("CreateObject on branch %s: %v", branchID, err)
	}
	return res.Object.ID, res.Version.ID, res.Version.StateID
}

// seedReviewedVersion is the fixture every publishable case starts from: one
// version on the project's research branch, plus the research PR that
// proposes its state to main, plus the two approved reviews docs/09 §5
// requires. The publication reads that record with the release gate's own
// query, so this is the same evidence a release would demand.
//
// reviewed=false stops after the version, which is the "no publication
// review" case of docs/43.
func (w *knowledgeWorld) seedReviewedVersion(t *testing.T, ctx context.Context, p *knowledgeProject, statement string, number int64, reviewed bool) (objectID, versionID, stateID string) {
	t.Helper()
	objectID, versionID, stateID = w.seedVersion(t, ctx, p.id, p.probe, statement)
	if !reviewed {
		return objectID, versionID, stateID
	}
	pr := w.seedPR(t, ctx, p, number, stateID)
	w.approve(t, ctx, pr, stateID, "scientific")
	w.approve(t, ctx, pr, stateID, "integrity")
	return objectID, versionID, stateID
}

// seedPR seeds one pull_request row proposing proposedState to main from the
// project's research branch, and returns its id.
func (w *knowledgeWorld) seedPR(t *testing.T, ctx context.Context, p *knowledgeProject, number int64, proposedState string) string {
	t.Helper()
	row, err := sqlc.New(w.pool).CreatePullRequest(ctx, sqlc.CreatePullRequestParams{
		ProjectID:       parseUUIDOrDie(p.id),
		Number:          number,
		SourceBranchID:  parseUUIDOrDie(p.probe),
		TargetBranchID:  parseUUIDOrDie(p.main),
		BaseStateID:     parseUUIDOrDie(p.genesis),
		ProposedStateID: parseUUIDOrDie(proposedState),
		Title:           "publication review fixture",
		Body:            "",
		CreatedBy:       parseUUIDOrDie(w.aliceID),
	})
	if err != nil {
		t.Fatalf("seed research PR for %s: %v", p.id, err)
	}
	return pgUUIDTextTest(row.ID)
}

// approve seeds one approved review row on a PR. reviewedState is the head
// the review evaluates (migration 00061: reviews pin reviewed_state_id to
// the PR's proposed head).
func (w *knowledgeWorld) approve(t *testing.T, ctx context.Context, prID, reviewedState, kind string) {
	t.Helper()
	if _, err := sqlc.New(w.pool).CreateReview(ctx, sqlc.CreateReviewParams{
		PullRequestID:   parseUUIDOrDie(prID),
		ReviewerID:      parseUUIDOrDie(w.bobID),
		ReviewKind:      kind,
		Decision:        "approved",
		ReviewedStateID: parseUUIDOrDie(reviewedState),
		Responsibility:  "",
		Body:            "",
	}); err != nil {
		t.Fatalf("seed %s review on %s: %v", kind, prID, err)
	}
}

// pinVisibilityPolicy gives one version a visibility axis of its own
// (scientific_object_versions.visibility_policy_id).
//
// It INSERTs rather than updates, and not for convenience: the version table
// is append-only (migration 00014 — an UPDATE is rejected by the database
// itself), and a version's identity is its content, so a pinned version is a
// new row. The insert copies the version the RSG service just wrote and pins
// the one column. See the file header for why a fixture has to write it at
// all.
func (w *knowledgeWorld) pinVisibilityPolicy(t *testing.T, ctx context.Context, baseVersionID string) (versionID string) {
	t.Helper()
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
			 lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
		SELECT object_id, version_no + 1, state_id, branch_id, schema_id, schema_version, title,
		       lifecycle_state, payload, gen_random_uuid(), integrity_hash, created_by
		FROM scientific_object_versions WHERE id = $1
		RETURNING id`, baseVersionID).Scan(&versionID); err != nil {
		t.Fatalf("pin a visibility policy on %s: %v", baseVersionID, err)
	}
	return versionID
}

// copyVersionWithLifecycle copies a version the RSG service wrote and puts
// the copy in another lifecycle state (docs/43's version vocabulary:
// active → aborted → reopened → active).
//
// It INSERTs for the same reason pinVisibilityPolicy does — the table is
// append-only (migration 00014) — and it copies state_id, which is what
// keeps the review record of the original's lineage into main applying to
// the copy: the review gate reads reviews over the STATE's lineage, not over
// a version row, so a copy of a reviewed version is a reviewed version and
// the lifecycle state is the only thing that differs.
func (w *knowledgeWorld) copyVersionWithLifecycle(t *testing.T, ctx context.Context, baseVersionID, lifecycle string) (versionID string) {
	t.Helper()
	if err := w.pool.QueryRow(ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version, title,
			 lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
		SELECT object_id, version_no + 1, state_id, branch_id, schema_id, schema_version, title,
		       $2, payload, visibility_policy_id, integrity_hash, created_by
		FROM scientific_object_versions WHERE id = $1
		RETURNING id`, baseVersionID, lifecycle).Scan(&versionID); err != nil {
		t.Fatalf("copy %s as lifecycle %q: %v", baseVersionID, lifecycle, err)
	}
	return versionID
}

// --------------------------------------------------------------------------
// The wire shapes this suite reads (declared here rather than reused from
// cmd/api/knowledgehttp, so a silent JSON tag change in a payload fails
// here)

type publishedKnowledgeWire struct {
	PID             string          `json:"pid"`
	ObjectVersionID string          `json:"object_version_id"`
	PublicVersion   string          `json:"public_version"`
	PublishedBy     string          `json:"published_by"`
	PublishedAt     string          `json:"published_at"`
	Rights          json.RawMessage `json:"rights"`
}

type knowledgeReadWire struct {
	PID           string `json:"pid"`
	PublicVersion string `json:"public_version"`
	Audience      string `json:"audience"`
	Object        struct {
		ID         string `json:"id"`
		ObjectType string `json:"object_type"`
	} `json:"object"`
	Version struct {
		ID             string `json:"id"`
		Title          string `json:"title"`
		LifecycleState string `json:"lifecycle_state"`
		IntegrityHash  string `json:"integrity_hash"`
	} `json:"version"`
	Project struct {
		ID         string `json:"id"`
		Visibility string `json:"visibility"`
	} `json:"project"`
}

// knowledgePreviewWire is the preview — and the report a refusal carries,
// which is the same document (knowledgepublish.PreviewOf).
type knowledgePreviewWire struct {
	ProjectID           string                  `json:"project_id"`
	ObjectVersionID     string                  `json:"object_version_id"`
	ObjectID            string                  `json:"object_id"`
	Audience            string                  `json:"audience"`
	ProjectVisibility   string                  `json:"project_visibility"`
	VisibilityPolicyID  *string                 `json:"visibility_policy_id"`
	ReviewApproved      bool                    `json:"review_approved"`
	ApprovedReviewKinds []string                `json:"approved_review_kinds"`
	RequiredReviewKinds []string                `json:"required_review_kinds"`
	Existing            *publishedKnowledgeWire `json:"existing_publication"`
	Blocking            []struct {
		Code   string `json:"code"`
		Detail string `json:"detail"`
	} `json:"blocking"`
	Publishable bool `json:"publishable"`
}

// knowledgeRefusalWire is the 409 body: the shared error envelope plus the
// complete preview computed over the state the publish itself saw.
type knowledgeRefusalWire struct {
	Code    string               `json:"code"`
	Message string               `json:"message"`
	Preview knowledgePreviewWire `json:"preview"`
	Reasons []string             `json:"reasons"`
}

type knowledgeErrorWire struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// --------------------------------------------------------------------------
// Requests

// knowledgeRights is a rights document as the request body carries it: the
// canonical bytes of the fail-closed default (rights.New() is
// metadata=project_policy, data_access=restricted), with the metadata axis
// optionally overridden.
func knowledgeRights(t *testing.T, metadata string) string {
	t.Helper()
	doc := rights.New()
	if metadata != "" {
		doc.Visibility.Metadata = rights.MetadataVisibility(metadata)
	}
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	return string(b)
}

// knowledgePublishBody is one publish request: the contract's
// knowledge_version_ref spelling, the rights document, and the publisher's
// name.
func knowledgePublishBody(t *testing.T, versionID, publicVersion, metadata string) string {
	t.Helper()
	name, err := json.Marshal(publicVersion)
	if err != nil {
		t.Fatalf("marshal public_version: %v", err)
	}
	return fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s,"public_version":%s}`,
		versionID, knowledgeRights(t, metadata), name)
}

// knowledgePreviewBody is the document specs/mcp/tools.json gives
// knowledge.publish_preview: knowledge_version_ref and rights, and nothing
// else — the tool is a proposal, so it names no public_version.
func knowledgePreviewBody(t *testing.T, versionID, metadata string) string {
	t.Helper()
	return fmt.Sprintf(`{"knowledge_version_ref":"object_version:%s","rights":%s}`,
		versionID, knowledgeRights(t, metadata))
}

// previewKnowledge posts one preview.
func (w *knowledgeWorld) previewKnowledge(t *testing.T, uc *testUserClient, projectID, body string) *http.Response {
	t.Helper()
	return uc.do(t, http.MethodPost, "/api/v1/projects/"+projectID+"/knowledge:publish-preview", body)
}

// publishKnowledge posts one publish; key == "" sends no Idempotency-Key.
func (w *knowledgeWorld) publishKnowledge(t *testing.T, uc *testUserClient, projectID, body, key string) *http.Response {
	t.Helper()
	return uc.doKeyed(t, http.MethodPost, "/api/v1/projects/"+projectID+"/knowledge:publish", body, key)
}

// mustPreview requires the 200 and decodes the proposal.
func (w *knowledgeWorld) mustPreview(t *testing.T, uc *testUserClient, projectID, body string) knowledgePreviewWire {
	t.Helper()
	resp := w.previewKnowledge(t, uc, projectID, body)
	mustStatus(t, resp, http.StatusOK)
	raw := readAll(t, resp)
	var got knowledgePreviewWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("preview payload: %v: %s", err, raw)
	}
	return got
}

// mustKnowledgePublish requires the 201 and returns the stored publication
// as the wire renders it.
func (w *knowledgeWorld) mustKnowledgePublish(t *testing.T, uc *testUserClient, projectID, body, key string) publishedKnowledgeWire {
	t.Helper()
	resp := w.publishKnowledge(t, uc, projectID, body, key)
	mustStatus(t, resp, http.StatusCreated)
	raw := readAll(t, resp)
	var got publishedKnowledgeWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("published knowledge payload: %v: %s", err, raw)
	}
	return got
}

// mustKnowledgeRefused requires the 409 + KNOWLEDGE_PUBLISH_BLOCKED refusal
// and returns the whole report.
func (w *knowledgeWorld) mustKnowledgeRefused(t *testing.T, uc *testUserClient, projectID, body, key string) knowledgePreviewWire {
	t.Helper()
	resp := w.publishKnowledge(t, uc, projectID, body, key)
	mustStatus(t, resp, http.StatusConflict)
	raw := readAll(t, resp)
	var got knowledgeRefusalWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("refusal payload: %v: %s", err, raw)
	}
	if got.Code != knowledgepublish.CodePublishBlocked {
		t.Fatalf("refusal code = %q, want %q: %s", got.Code, knowledgepublish.CodePublishBlocked, raw)
	}
	return got.Preview
}

// mustKnowledgeError requires one status + code and returns the raw body (the
// disclosure assertions compare raw bytes).
func (w *knowledgeWorld) mustKnowledgeError(t *testing.T, uc *testUserClient, projectID, body, key string, status int, code string) string {
	t.Helper()
	resp := w.publishKnowledge(t, uc, projectID, body, key)
	mustStatus(t, resp, status)
	raw := readAll(t, resp)
	var env knowledgeErrorWire
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("error envelope: %v: %s", err, raw)
	}
	if env.Code != code {
		t.Fatalf("code = %q, want %q: %s", env.Code, code, raw)
	}
	return raw
}

// blockedCode reports whether a report carries a blocking entry with the
// given code.
func blockedCode(p knowledgePreviewWire, code string) bool {
	for _, b := range p.Blocking {
		if b.Code == code {
			return true
		}
	}
	return false
}

// readKnowledge reads the public route; uc == nil reads anonymously (no
// session at all), and returns the status with the raw body.
func (w *knowledgeWorld) readKnowledge(t *testing.T, uc *testUserClient, pid string) (int, string) {
	t.Helper()
	if uc == nil {
		uc = newTestUserClient(w.ts.URL)
	}
	resp := uc.do(t, http.MethodGet, "/api/v1/knowledge/"+pid, "")
	return resp.StatusCode, readAll(t, resp)
}

// mustReadKnowledge requires the 200 and decodes the published page.
func (w *knowledgeWorld) mustReadKnowledge(t *testing.T, uc *testUserClient, pid string) knowledgeReadWire {
	t.Helper()
	status, raw := w.readKnowledge(t, uc, pid)
	if status != http.StatusOK {
		t.Fatalf("GET /api/v1/knowledge/%s = %d, want 200: %s", pid, status, raw)
	}
	var got knowledgeReadWire
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("knowledge read payload: %v: %s", err, raw)
	}
	return got
}

// --------------------------------------------------------------------------
// Database reads (the acceptance is about ROWS, not about status codes)

type publicationRow struct {
	ID              string
	PID             string
	ObjectVersionID string
	PublicVersion   string
	PublicVersionB  int
	RightsJSON      string
	PublishedBy     string
	PublishedAt     time.Time
}

// publicationsOf reads every knowledge_publications row of one version,
// oldest first.
func (w *knowledgeWorld) publicationsOf(t *testing.T, ctx context.Context, versionID string) []publicationRow {
	t.Helper()
	rows, err := w.pool.Query(ctx, `
		SELECT id, pid, object_version_id, public_version, octet_length(public_version),
		       rights_json::text, published_by, published_at
		FROM knowledge_publications
		WHERE object_version_id = $1
		ORDER BY published_at, id`, versionID)
	if err != nil {
		t.Fatalf("read publications: %v", err)
	}
	defer rows.Close()
	var out []publicationRow
	for rows.Next() {
		var r publicationRow
		if err := rows.Scan(&r.ID, &r.PID, &r.ObjectVersionID, &r.PublicVersion, &r.PublicVersionB,
			&r.RightsJSON, &r.PublishedBy, &r.PublishedAt); err != nil {
			t.Fatalf("scan publication: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read publications: %v", err)
	}
	return out
}

// mustOnePublication requires exactly one publication row for a version.
func (w *knowledgeWorld) mustOnePublication(t *testing.T, ctx context.Context, versionID string) publicationRow {
	t.Helper()
	rows := w.publicationsOf(t, ctx, versionID)
	if len(rows) != 1 {
		t.Fatalf("knowledge_publications rows for version %s = %d, want exactly 1", versionID, len(rows))
	}
	return rows[0]
}

// eventRow is one research_events row.
type eventRow struct {
	EventType     string
	ActorID       string
	ProjectID     string
	Visibility    string
	CorrelationID string
	Payload       map[string]any
}

// eventsOf reads the research events of one type, oldest first.
func (w *knowledgeWorld) eventsOf(t *testing.T, ctx context.Context, eventType string) []eventRow {
	t.Helper()
	rows, err := w.pool.Query(ctx, `
		SELECT event_type, coalesce(actor_id::text, ''), coalesce(project_id::text, ''),
		       visibility, correlation_id, payload
		FROM research_events WHERE event_type = $1 ORDER BY occurred_at, id`, eventType)
	if err != nil {
		t.Fatalf("read research events: %v", err)
	}
	defer rows.Close()
	var out []eventRow
	for rows.Next() {
		var r eventRow
		var payload []byte
		if err := rows.Scan(&r.EventType, &r.ActorID, &r.ProjectID, &r.Visibility, &r.CorrelationID, &payload); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		if err := json.Unmarshal(payload, &r.Payload); err != nil {
			t.Fatalf("decode event payload %s: %v", payload, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read research events: %v", err)
	}
	return out
}

// outboxRowsOf counts the outbox rows of one event type.
func (w *knowledgeWorld) outboxRowsOf(t *testing.T, ctx context.Context, eventType string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events WHERE event_type = $1`, eventType).Scan(&n); err != nil {
		t.Fatalf("count outbox rows: %v", err)
	}
	return n
}

// auditRowsOf counts the audit rows of one project and action.
func (w *knowledgeWorld) auditRowsOf(t *testing.T, ctx context.Context, projectID, action string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_log WHERE project_id = $1 AND action = $2`, projectID, action).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// ledgerRowsOf counts the publish ledger rows of one project and key.
func (w *knowledgeWorld) ledgerRowsOf(t *testing.T, ctx context.Context, projectID, key string) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM knowledge_publication_creations WHERE project_id = $1 AND idempotency_key = $2`,
		projectID, key).Scan(&n); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

// pidOfVersion reads the version's publication pid directly.
func (w *knowledgeWorld) pidOfVersion(t *testing.T, ctx context.Context, versionID string) string {
	t.Helper()
	var pid string
	if err := w.pool.QueryRow(ctx,
		`SELECT pid FROM knowledge_publications WHERE object_version_id = $1`, versionID).Scan(&pid); err != nil {
		t.Fatalf("read the publication pid: %v", err)
	}
	return pid
}

// --------------------------------------------------------------------------
// Case 1: the table has a writer, and a publish is one unit

// TestKnowledgePublishWritesTheRowAndTheEvent is criterion 1 (and, on the
// way, criterion 4's verbatim rule on the real column): the preview says the
// publication would be admitted, the publish lands the row, and the research
// event — read from the event TABLE, not inferred from a status code — names
// the publication, the version and the name, sharing one correlation id with
// the audit row. The public read then serves it.
func TestKnowledgePublishWritesTheRowAndTheEvent(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-open", "public")
	objectID, versionID, _ := w.seedReviewedVersion(t, ctx, p, "a published question", 1, true)

	// --- the proposal ----------------------------------------------------
	preview := w.mustPreview(t, w.alice, p.id, knowledgePreviewBody(t, versionID, ""))
	if !preview.Publishable || len(preview.Blocking) != 0 {
		t.Fatalf("preview = %+v, want a publishable version", preview)
	}
	if preview.Audience != "network" {
		t.Errorf("preview audience = %q, want network: the project is public and the version inherits its visibility", preview.Audience)
	}
	if !preview.ReviewApproved {
		t.Errorf("preview review_approved = false with both reviews seeded")
	}
	if preview.VisibilityPolicyID != nil {
		t.Errorf("preview visibility_policy_id = %q, want null (the version inherits)", *preview.VisibilityPolicyID)
	}
	if preview.ProjectVisibility != "public" {
		t.Errorf("preview project_visibility = %q, want public", preview.ProjectVisibility)
	}
	if preview.ObjectVersionID != versionID || preview.ObjectID != objectID {
		t.Errorf("preview names version %q object %q, want %q/%q", preview.ObjectVersionID, preview.ObjectID, versionID, objectID)
	}

	// --- the publish: an odd but legal name, stored verbatim -------------
	const odd = "  v1.0 — 预印本 (DRAFT)  "
	got := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionID, odd, ""), "kp-open-1")

	if got.PublicVersion != odd {
		t.Errorf("public_version on the wire = %q, want %q verbatim", got.PublicVersion, odd)
	}
	if got.ObjectVersionID != versionID {
		t.Errorf("object_version_id = %q, want the published version", got.ObjectVersionID)
	}
	if got.PublishedBy != w.aliceID {
		t.Errorf("published_by = %q, want alice", got.PublishedBy)
	}
	if !pidShape.MatchString(got.PID) {
		t.Errorf("pid = %q, want 26 lowercase Crockford base32 characters", got.PID)
	}

	// --- the row ---------------------------------------------------------
	row := w.mustOnePublication(t, ctx, versionID)
	if row.PID != got.PID {
		t.Errorf("stored pid = %q, want the pid the response carried (%q)", row.PID, got.PID)
	}
	if row.PublicVersion != odd {
		t.Errorf("stored public_version = %q, want %q", row.PublicVersion, odd)
	}
	if row.PublicVersionB != len(odd) {
		t.Errorf("stored public_version is %d bytes, want %d — the publisher's name is stored byte for byte", row.PublicVersionB, len(odd))
	}
	if row.PublishedBy != w.aliceID {
		t.Errorf("stored published_by = %q, want alice", row.PublishedBy)
	}
	if !strings.Contains(row.RightsJSON, `"project_policy"`) {
		t.Errorf("stored rights_json = %s, want the document the publish declared", row.RightsJSON)
	}

	// --- the event (the criterion: the ROW, not the status code) ---------
	events := w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)
	if len(events) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", knowledgeVersionPublishedEvent, len(events))
	}
	ev := events[0]
	if ev.ActorID != w.aliceID {
		t.Errorf("event actor = %q, want alice", ev.ActorID)
	}
	if ev.ProjectID != p.id {
		t.Errorf("event project = %q, want %q", ev.ProjectID, p.id)
	}
	if ev.Payload["publication_id"] != got.PID {
		t.Errorf("event payload publication_id = %v, want the publication's pid", ev.Payload["publication_id"])
	}
	if ev.Payload["object_version_id"] != versionID {
		t.Errorf("event payload object_version_id = %v, want the published version", ev.Payload["object_version_id"])
	}
	if ev.Payload["public_version"] != odd {
		t.Errorf("event payload public_version = %v, want %q", ev.Payload["public_version"], odd)
	}
	if ev.CorrelationID == "" {
		t.Errorf("event correlation id is empty")
	}
	if n := w.outboxRowsOf(t, ctx, knowledgeVersionPublishedEvent); n != 1 {
		t.Errorf("outbox rows = %d, want 1", n)
	}
	if n := w.ledgerRowsOf(t, ctx, p.id, "kp-open-1"); n != 1 {
		t.Errorf("ledger rows for the key = %d, want 1", n)
	}

	// --- the audit row names the publication and shares the trace --------
	var targetRef, correlation string
	if err := w.pool.QueryRow(ctx, `
		SELECT target_ref, correlation_id FROM audit_log
		WHERE project_id = $1 AND action = $2`, p.id, domain.ActionKnowledgeVersionPublished).
		Scan(&targetRef, &correlation); err != nil {
		t.Fatalf("read the audit row: %v", err)
	}
	if !strings.HasPrefix(targetRef, "knowledge_publication:") {
		t.Errorf("audit target_ref = %q, want knowledge_publication:<id>", targetRef)
	}
	if correlation != ev.CorrelationID {
		t.Errorf("audit correlation = %q, event correlation = %q, want one trace", correlation, ev.CorrelationID)
	}

	// --- the public read serves it to the network ------------------------
	read := w.mustReadKnowledge(t, nil, got.PID)
	if read.Audience != "network" {
		t.Errorf("read audience = %q, want network", read.Audience)
	}
	if read.Object.ID != objectID || read.Version.ID != versionID {
		t.Errorf("read names object %q version %q, want %q/%q", read.Object.ID, read.Version.ID, objectID, versionID)
	}
	if read.Project.ID != p.id || read.Project.Visibility != "public" {
		t.Errorf("read project = %+v, want the owning project's id and its public preset", read.Project)
	}
	if read.PublicVersion != odd {
		t.Errorf("read public_version = %q, want %q", read.PublicVersion, odd)
	}
	if read.Version.IntegrityHash == "" || read.Version.Title == "" {
		t.Errorf("read version = %+v, want the stored facts", read.Version)
	}
}

// --------------------------------------------------------------------------
// Case 2: one publication per version

// TestKnowledgePublishOncePerVersion is criterion 3: a version is published
// once. The second attempt is refused THROUGH THE APPLICATION — the table's
// own UNIQUE(object_version_id, public_version) would have accepted a second
// row under a different name, which is exactly the attempt this pins.
func TestKnowledgePublishOncePerVersion(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-once", "public")
	_, versionID, _ := w.seedReviewedVersion(t, ctx, p, "published once", 1, true)

	first := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.0", ""), "kp-once-1")

	// The same name: the report names the existing publication.
	preview := w.mustKnowledgeRefused(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.0", ""), "kp-once-2")
	if !blockedCode(preview, knowledgepublish.ReasonAlreadyPublished) {
		t.Fatalf("the refusal does not report already_published: %+v", preview.Blocking)
	}
	if preview.Existing == nil || preview.Existing.PID != first.PID {
		t.Fatalf("the refusal does not name the existing publication: %+v", preview.Existing)
	}

	// A DIFFERENT name: the unique index would accept this row, and the
	// application refuses it (owner ruling L3-20260916-1 #3).
	preview = w.mustKnowledgeRefused(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.1 (revised)", ""), "kp-once-3")
	if !blockedCode(preview, knowledgepublish.ReasonAlreadyPublished) {
		t.Fatalf("a second publication under a new name was not refused: %+v", preview.Blocking)
	}

	row := w.mustOnePublication(t, ctx, versionID)
	if row.PublicVersion != "v1.0" {
		t.Errorf("stored public_version = %q, want the first publication's", row.PublicVersion)
	}
	if n := len(w.publicationsOf(t, ctx, versionID)); n != 1 {
		t.Errorf("publications = %d, want 1", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 1 {
		t.Errorf("events = %d, want 1: a refused publish records nothing", n)
	}
	if n := w.auditRowsOf(t, ctx, p.id, domain.ActionKnowledgeVersionPublished); n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
}

// --------------------------------------------------------------------------
// Case 5: permission is fail-closed

// TestKnowledgePublishPermissionIsFailClosed is criterion 5: the owner
// publishes, the maintainer is refused (the matrix cell publish_private_to_
// public is `conditional` for maintainer and a condition no specification
// defines is a denial — authz.Permits admits only `allow`, issue #237), a
// signed-in stranger gets the SAME answer, and an anonymous caller is
// refused before the command runs.
//
// Nothing is written by any of the refusals, and the owner's publish
// afterwards succeeds — which is what makes the refusals a fact about the
// actors rather than about the version.
func TestKnowledgePublishPermissionIsFailClosed(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-perm", "public")
	_, versionID, _ := w.seedReviewedVersion(t, ctx, p, "permission probe", 1, true)
	seedMember(t, ctx, w.pool, p.id, w.bobID, "maintainer")
	body := knowledgePublishBody(t, versionID, "v1.0", "")

	// The maintainer: a member of the project, and refused.
	maintainer := w.mustKnowledgeError(t, w.bob, p.id, body, "kp-perm-bob",
		http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval)

	// A signed-in stranger: the SAME answer, so the refusal discloses
	// nothing about which projects exist.
	stranger := w.mustKnowledgeError(t, w.carol, p.id, body, "kp-perm-carol",
		http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval)
	if maintainer != stranger {
		t.Errorf("the maintainer's refusal and a stranger's differ:\n%s\n%s", maintainer, stranger)
	}

	// An anonymous caller: refused by the guard, before the handler.
	anon := newTestUserClient(w.ts.URL)
	resp := anon.do(t, http.MethodPost, "/api/v1/projects/"+p.id+"/knowledge:publish", body)
	mustStatus(t, resp, http.StatusUnauthorized)
	readAll(t, resp)

	// Nothing was written by any of them.
	if n := len(w.publicationsOf(t, ctx, versionID)); n != 0 {
		t.Fatalf("publications = %d after three refusals, want 0", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 0 {
		t.Fatalf("events = %d after three refusals, want 0", n)
	}
	if n := w.auditRowsOf(t, ctx, p.id, domain.ActionKnowledgeVersionPublished); n != 0 {
		t.Fatalf("audit rows = %d after three refusals, want 0", n)
	}

	// The owner's publish still works.
	w.mustKnowledgePublish(t, w.alice, p.id, body, "kp-perm-alice")
	w.mustOnePublication(t, ctx, versionID)
}

// --------------------------------------------------------------------------
// The ruling's headline: publishing may not widen visibility

// TestKnowledgePublishDoesNotWidenVisibility is 发布 ≠ 公开 (owner ruling
// L3-20260916-1 #1) over the three combinations that can disagree:
//
//	(a) a version that pins its OWN visibility policy, in a PUBLIC project —
//	    publishable, and afterwards members-only. A rule that read the
//	    project's preset alone would serve this one to the network, which is
//	    the defect the ruling was written against;
//	(b) an inherit-visibility version in a PRIVATE project — publishable
//	    (docs/12 §2), and members-only;
//	(c) a version in a PUBLIC project whose rights declaration pins a
//	    metadata visibility of its own — publishable, and members-only.
//
// Every case asserts the publication HAPPENED (the row exists) and that the
// network is answered the existence-hiding 404 while a member reads the body
// — which is the difference between "the publication was refused" and "the
// publication happened and nobody's visibility was widened".
func TestKnowledgePublishDoesNotWidenVisibility(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)

	public := w.newProject(t, ctx, "kp-public", "public")
	private := w.newProject(t, ctx, "kp-private", "private")
	seedMember(t, ctx, w.pool, public.id, w.bobID, "maintainer")
	seedMember(t, ctx, w.pool, private.id, w.bobID, "maintainer")

	// --- (a) a pinned version in a PUBLIC project ------------------------
	_, baseVersion, _ := w.seedReviewedVersion(t, ctx, public, "restricted in a public project", 1, true)
	pinnedVersion := w.pinVisibilityPolicy(t, ctx, baseVersion)

	preview := w.mustPreview(t, w.alice, public.id, knowledgePreviewBody(t, pinnedVersion, ""))
	if preview.ProjectVisibility != "public" {
		t.Fatalf("(a) the fixture's project is %q, want public", preview.ProjectVisibility)
	}
	if preview.VisibilityPolicyID == nil {
		t.Fatalf("(a) the fixture did not pin a visibility policy: %+v", preview)
	}
	if preview.Audience != "members" {
		t.Errorf("(a) preview audience = %q, want members: the version pins its own visibility in a public project", preview.Audience)
	}
	if !preview.Publishable {
		t.Fatalf("(a) a visibility-restricted version is not publishable: %+v", preview.Blocking)
	}
	pinned := w.mustKnowledgePublish(t, w.alice, public.id,
		knowledgePublishBody(t, pinnedVersion, "v1.0 (restricted)", ""), "kp-pinned-1")
	if row := w.mustOnePublication(t, ctx, pinnedVersion); row.PID != pinned.PID {
		t.Errorf("(a) stored pid = %q, want %q", row.PID, pinned.PID)
	}
	// The network: the same 404 an unknown pid gets.
	anonStatus, anonBody := w.readKnowledge(t, nil, pinned.PID)
	if anonStatus != http.StatusNotFound {
		t.Errorf("(a) anonymous read = %d, want 404: a pinned version in a public project must not become network-readable by being published: %s", anonStatus, anonBody)
	}
	strangerStatus, strangerBody := w.readKnowledge(t, w.carol, pinned.PID)
	if strangerStatus != http.StatusNotFound {
		t.Errorf("(a) a signed-in non-member read = %d, want 404: %s", strangerStatus, strangerBody)
	}
	for _, secret := range []string{pinned.PID, "v1.0 (restricted)", pinnedVersion} {
		if strings.Contains(anonBody, secret) {
			t.Errorf("(a) the withheld body discloses %q: %s", secret, anonBody)
		}
	}
	// The project's own member: the body.
	member := w.mustReadKnowledge(t, w.bob, pinned.PID)
	if member.Audience != "members" {
		t.Errorf("(a) member read audience = %q, want members", member.Audience)
	}
	if member.Version.ID != pinnedVersion {
		t.Errorf("(a) member read names version %q, want %q", member.Version.ID, pinnedVersion)
	}

	// --- (b) an inherit-visibility version in a PRIVATE project ----------
	_, privateVersion, _ := w.seedReviewedVersion(t, ctx, private, "inherit in a private project", 1, true)
	preview = w.mustPreview(t, w.alice, private.id, knowledgePreviewBody(t, privateVersion, ""))
	if preview.Audience != "members" {
		t.Errorf("(b) preview audience = %q, want members: the project is private", preview.Audience)
	}
	if !preview.Publishable {
		t.Fatalf("(b) a version in a private project is not publishable: %+v", preview.Blocking)
	}
	hidden := w.mustKnowledgePublish(t, w.alice, private.id,
		knowledgePublishBody(t, privateVersion, "v1.0 (private origin)", ""), "kp-private-1")
	if status, body := w.readKnowledge(t, nil, hidden.PID); status != http.StatusNotFound {
		t.Errorf("(b) anonymous read = %d, want 404: %s", status, body)
	}
	if got := w.mustReadKnowledge(t, w.bob, hidden.PID); got.Audience != "members" {
		t.Errorf("(b) member read audience = %q, want members", got.Audience)
	}

	// --- (c) a rights declaration that pins metadata, PUBLIC project -----
	_, rightsVersion, _ := w.seedReviewedVersion(t, ctx, public, "rights-pinned metadata", 2, true)
	const token = "members_only_policy_v1"
	preview = w.mustPreview(t, w.alice, public.id, knowledgePreviewBody(t, rightsVersion, token))
	if preview.Audience != "members" {
		t.Errorf("(c) preview audience = %q, want members: the rights declaration pins a metadata token of its own", preview.Audience)
	}
	if !preview.Publishable {
		t.Fatalf("(c) a rights-pinned version is not publishable: %+v", preview.Blocking)
	}
	rightsPinned := w.mustKnowledgePublish(t, w.alice, public.id,
		knowledgePublishBody(t, rightsVersion, "v1.0 (rights-pinned)", token), "kp-rights-1")
	if status, body := w.readKnowledge(t, nil, rightsPinned.PID); status != http.StatusNotFound {
		t.Errorf("(c) anonymous read = %d, want 404: %s", status, body)
	}
	if got := w.mustReadKnowledge(t, w.bob, rightsPinned.PID); got.Audience != "members" {
		t.Errorf("(c) member read audience = %q, want members", got.Audience)
	}

	// --- the control: the same project, a version open on every axis -----
	_, openVersion, _ := w.seedReviewedVersion(t, ctx, public, "open by every axis", 3, true)
	open := w.mustKnowledgePublish(t, w.alice, public.id, knowledgePublishBody(t, openVersion, "v1.0 (open)", ""), "kp-open-2")
	if got := w.mustReadKnowledge(t, nil, open.PID); got.Audience != "network" {
		t.Errorf("control read audience = %q, want network: every axis of this publication is open", got.Audience)
	}
}

// TestKnowledgeReadAnswersAnUnreadablePublicationExactlyAsAnUnknownPID is
// the existence-oracle regression, measured over real rows.
//
// GetMembership resolves the project through the project READ GATE before it
// reads a role, so a members-only publication in a PRIVATE project answered
// a signed-in non-member 503 "temporarily unavailable, retryable" — while an
// unknown pid answered 404. The only thing separating the two answers was
// that the first pid named a real publication in a real project, so a caller
// could walk the pid space and read the difference off the status code. The
// first two cases below are exactly the reviewer's two measurements:
//
//	(a) members-only because the version pins its own visibility, in a PUBLIC
//	    project — the non-member may read the project, so the membership read
//	    answers ErrMemberNotFound;
//	(b) members-only because the project is private — the non-member may not
//	    read the project at all, so the same read answers ErrProjectNotFound
//	    (docs/45: the existence of a private project is itself private).
//
// Both are "not a member", both are the 404, and both must be BYTE-IDENTICAL
// to the 404 an unknown pid gets — which is the third case, with the owner's
// own read as the control that it names nothing rather than being withheld.
func TestKnowledgeReadAnswersAnUnreadablePublicationExactlyAsAnUnknownPID(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	public := w.newProject(t, ctx, "kp-oracle-public", "public")
	private := w.newProject(t, ctx, "kp-oracle-private", "private")

	// --- (a) a pinned version in a PUBLIC project ------------------------
	_, baseVersion, _ := w.seedReviewedVersion(t, ctx, public, "pinned in a public project", 1, true)
	pinnedVersion := w.pinVisibilityPolicy(t, ctx, baseVersion)
	pinned := w.mustKnowledgePublish(t, w.alice, public.id,
		knowledgePublishBody(t, pinnedVersion, "v1.0 (pinned)", ""), "kp-oracle-1")

	// --- (b) an inherit-visibility version in a PRIVATE project ----------
	_, privateVersion, _ := w.seedReviewedVersion(t, ctx, private, "inherit in a private project", 1, true)
	hidden := w.mustKnowledgePublish(t, w.alice, private.id,
		knowledgePublishBody(t, privateVersion, "v1.0 (private)", ""), "kp-oracle-2")

	// --- the fixture's controls, read from the tables themselves ---------
	// (a) really is members-only for a reason other than the project preset,
	// and (b) really is a publication in a project carol cannot read.
	var pinnedPolicy *string
	if err := w.pool.QueryRow(ctx,
		`SELECT visibility_policy_id::text FROM scientific_object_versions WHERE id = $1`,
		pinnedVersion).Scan(&pinnedPolicy); err != nil {
		t.Fatalf("read the pinned version's visibility axis: %v", err)
	}
	if pinnedPolicy == nil {
		t.Fatalf("(a) the published version carries no visibility policy of its own: the case would be project-visibility-only")
	}
	var visibility string
	if err := w.pool.QueryRow(ctx, `SELECT visibility FROM projects WHERE id = $1`, private.id).Scan(&visibility); err != nil {
		t.Fatalf("read the private project's preset: %v", err)
	}
	if visibility != "private" {
		t.Fatalf("(b) the fixture's project is %q, want private", visibility)
	}
	// carol is a signed-in caller with no role in either project: if she had
	// one, the two 404s below would be testing something else.
	var roles int
	if err := w.pool.QueryRow(ctx,
		`SELECT count(*) FROM project_memberships WHERE user_id = $1 AND project_id IN ($2, $3)`,
		w.carolID, public.id, private.id).Scan(&roles); err != nil {
		t.Fatalf("count carol's roles: %v", err)
	}
	if roles != 0 {
		t.Fatalf("the fixture gave carol %d roles: the non-member cases would not be non-member cases", roles)
	}
	// The owner reads (b): the publication is real, and the 404 carol gets is
	// the read gate's hiding, not a publication that was never written.
	if got := w.mustReadKnowledge(t, w.alice, hidden.PID); got.PublicVersion != "v1.0 (private)" {
		t.Fatalf("the owner's read of (b) = %+v, want the publication", got)
	}

	// --- the unknown pid: well formed, and naming nothing ----------------
	// Flipping the last character of a real pid keeps the shape (Crockford
	// base32, lowercase) and resolves to no row — asserted in the table, not
	// assumed, and then asserted through the OWNER's read: a pid the owner
	// cannot read does not exist.
	unknown := pinned.PID[:len(pinned.PID)-1] + "z"
	if unknown == pinned.PID {
		unknown = pinned.PID[:len(pinned.PID)-1] + "y"
	}
	if !knowledgepublish.ValidPID(unknown) {
		t.Fatalf("the unknown pid %q is not well formed: the comparison would be between a 404 and a 400", unknown)
	}
	var rows int
	if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM knowledge_publications WHERE pid = $1`, unknown).Scan(&rows); err != nil {
		t.Fatalf("count publications of the unknown pid: %v", err)
	}
	if rows != 0 {
		t.Fatalf("the unknown pid %q names %d publications: it is not unknown", unknown, rows)
	}
	if status, body := w.readKnowledge(t, w.alice, unknown); status != http.StatusNotFound {
		t.Fatalf("the owner's read of the unknown pid = %d, want 404: %s", status, body)
	}

	// --- the three answers, for the same caller --------------------------
	type refusal struct {
		name   string
		pid    string
		status int
		body   string
	}
	cases := []refusal{
		{name: "members-only, public project", pid: pinned.PID},
		{name: "members-only, private project", pid: hidden.PID},
		{name: "unknown pid", pid: unknown},
	}
	for i := range cases {
		cases[i].status, cases[i].body = w.readKnowledge(t, w.carol, cases[i].pid)
	}
	for _, c := range cases {
		if c.status != http.StatusNotFound {
			t.Errorf("a signed-in non-member reading %s = %d, want 404: %s", c.name, c.status, c.body)
		}
		if !strings.Contains(c.body, knowledgehttp.CodeKnowledgeNotFound) {
			t.Errorf("a signed-in non-member reading %s was answered %s, want %s", c.name, c.body, knowledgehttp.CodeKnowledgeNotFound)
		}
		// Nothing of the publication leaks into a refusal.
		if c.name != "unknown pid" {
			for _, secret := range []string{c.pid, "v1.0", private.id, public.id} {
				if strings.Contains(c.body, secret) {
					t.Errorf("the refusal for %s discloses %q: %s", c.name, secret, c.body)
				}
			}
		}
	}
	for _, c := range cases[1:] {
		if c.status != cases[0].status || c.body != cases[0].body {
			t.Errorf("the refusal for %q differs from the refusal for %q:\n  %s: %s\n  %s: %s",
				c.name, cases[0].name, cases[0].name, cases[0].body, c.name, c.body)
		}
	}
}

// --------------------------------------------------------------------------
// Case 8: a retracted version cannot be presented as published knowledge

// TestKnowledgePublishRefusesARetractedVersion: a version the repository
// currently holds as retracted (lifecycle_state = 'aborted') may not be
// presented on the network as published knowledge, however well reviewed it
// is.
//
// The rule is NOT "aborted is terminal": docs/43's version lifecycle is
// active → aborted → reopened → active, so the SAME version reopened
// publishes — and that is this test's control. A rule that refused a version
// for having ever been aborted would pass the refusal assertions and fail
// the reopened one.
func TestKnowledgePublishRefusesARetractedVersion(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-retracted", "public")
	objectID, baseVersion, _ := w.seedReviewedVersion(t, ctx, p, "a retracted claim", 1, true)
	abortedVersion := w.copyVersionWithLifecycle(t, ctx, baseVersion, "aborted")

	// The fixture's control: the copy is another version of the SAME object,
	// in the state the rule reads, and it is a reviewed version — so the
	// refusal below cannot be the review rule in disguise.
	var gotObject, gotState string
	if err := w.pool.QueryRow(ctx,
		`SELECT object_id, lifecycle_state FROM scientific_object_versions WHERE id = $1`,
		abortedVersion).Scan(&gotObject, &gotState); err != nil {
		t.Fatalf("read the retracted copy: %v", err)
	}
	if gotObject != objectID {
		t.Fatalf("the retracted copy belongs to object %q, want %q", gotObject, objectID)
	}
	if gotState != "aborted" {
		t.Fatalf("the copy's lifecycle_state = %q, want aborted", gotState)
	}

	// --- the preview -----------------------------------------------------
	preview := w.mustPreview(t, w.alice, p.id, knowledgePreviewBody(t, abortedVersion, ""))
	if !preview.ReviewApproved {
		t.Fatalf("the fixture's retracted version is not reviewed: the refusal below would not be attributable to the lifecycle state: %+v", preview)
	}
	if preview.Publishable {
		t.Errorf("a retracted version is publishable: %+v", preview)
	}
	if len(preview.Blocking) != 1 || preview.Blocking[0].Code != knowledgepublish.ReasonLifecycleAborted {
		t.Errorf("the retracted version's blocking report = %+v, want exactly %q", preview.Blocking, knowledgepublish.ReasonLifecycleAborted)
	}

	// --- the publish: refused inside its own transaction ------------------
	report := w.mustKnowledgeRefused(t, w.alice, p.id,
		knowledgePublishBody(t, abortedVersion, "v1.0 (retracted)", ""), "kp-retracted-1")
	if !blockedCode(report, knowledgepublish.ReasonLifecycleAborted) {
		t.Errorf("the publish refusal does not report %q: %+v", knowledgepublish.ReasonLifecycleAborted, report.Blocking)
	}
	if report.Publishable {
		t.Errorf("the refusal report says the version is publishable: %+v", report)
	}

	// --- nothing was written ---------------------------------------------
	if n := len(w.publicationsOf(t, ctx, abortedVersion)); n != 0 {
		t.Errorf("a refused publish wrote %d publication rows", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 0 {
		t.Errorf("a refused publish recorded %d events", n)
	}
	if n := w.outboxRowsOf(t, ctx, knowledgeVersionPublishedEvent); n != 0 {
		t.Errorf("a refused publish wrote %d outbox rows", n)
	}
	if n := w.auditRowsOf(t, ctx, p.id, domain.ActionKnowledgeVersionPublished); n != 0 {
		t.Errorf("a refused publish wrote %d audit rows", n)
	}
	if n := w.ledgerRowsOf(t, ctx, p.id, "kp-retracted-1"); n != 0 {
		t.Errorf("a refused publish wrote %d ledger rows", n)
	}

	// --- the way out (docs/43: aborted → reopened) -----------------------
	reopenedVersion := w.copyVersionWithLifecycle(t, ctx, abortedVersion, "reopened")
	published := w.mustKnowledgePublish(t, w.alice, p.id,
		knowledgePublishBody(t, reopenedVersion, "v1.0 (reopened)", ""), "kp-retracted-2")
	if row := w.mustOnePublication(t, ctx, reopenedVersion); row.PID != published.PID {
		t.Errorf("stored pid = %q, want %q", row.PID, published.PID)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 1 {
		t.Errorf("events after the reopened publish = %d, want 1", n)
	}
	// The reopened publication is the one that reaches the network: the
	// retracted row was never published, and reopening is a NEW version.
	if got := w.mustReadKnowledge(t, nil, published.PID); got.Version.ID != reopenedVersion {
		t.Errorf("the published read names version %q, want the reopened one (%q)", got.Version.ID, reopenedVersion)
	}
	// And the retracted version is STILL refused after the reopen: the rule
	// is about the state that version is in, not a one-time gate on the
	// object.
	again := w.mustKnowledgeRefused(t, w.alice, p.id,
		knowledgePublishBody(t, abortedVersion, "v1.0 (retracted)", ""), "kp-retracted-3")
	if !blockedCode(again, knowledgepublish.ReasonLifecycleAborted) {
		t.Errorf("after the reopen, the retracted version is no longer refused: %+v", again.Blocking)
	}
}

// --------------------------------------------------------------------------
// Case 7: no published without review

// TestKnowledgePublishRequiresReview is criterion 7 (docs/43 §Publication:
// private candidate → publication_review → published, and no automatic
// published). A version with no review record at all is refused; a version
// with ONE of the two required dimensions is still refused (docs/09 §5
// forbids an aggregate approval); and the same version publishes once both
// dimensions carry an approved review — which is what makes the review the
// thing that gated it.
func TestKnowledgePublishRequiresReview(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-review", "public")
	_, versionID, stateID := w.seedReviewedVersion(t, ctx, p, "not yet reviewed", 1, false)

	preview := w.mustKnowledgeRefused(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v0.1", ""), "kp-review-1")
	if !blockedCode(preview, knowledgepublish.ReasonReviewRequired) {
		t.Fatalf("the refusal does not report review_required: %+v", preview.Blocking)
	}
	if preview.ReviewApproved {
		t.Errorf("review_approved = true for a version with no review record")
	}
	if preview.Existing != nil {
		t.Errorf("a version that was never published reports an existing publication: %+v", preview.Existing)
	}
	if n := len(w.publicationsOf(t, ctx, versionID)); n != 0 {
		t.Fatalf("a refused publish wrote a row: %d", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 0 {
		t.Fatalf("a refused publish recorded an event: %d", n)
	}

	// One dimension is not enough.
	pr := w.seedPR(t, ctx, p, 1, stateID)
	w.approve(t, ctx, pr, stateID, "scientific")
	preview = w.mustKnowledgeRefused(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v0.1", ""), "kp-review-2")
	if !blockedCode(preview, knowledgepublish.ReasonReviewRequired) {
		t.Fatalf("a version with only a scientific review was admitted: %+v", preview.Blocking)
	}
	if got := strings.Join(preview.ApprovedReviewKinds, ","); got != "scientific" {
		t.Errorf("approved_review_kinds = %q, want scientific alone", got)
	}
	if got := strings.Join(preview.RequiredReviewKinds, ","); got != "scientific,integrity" {
		t.Errorf("required_review_kinds = %q, want scientific,integrity", got)
	}

	// Both dimensions: the publication is admitted.
	w.approve(t, ctx, pr, stateID, "integrity")
	got := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.0", ""), "kp-review-3")
	if got.PID == "" {
		t.Errorf("the admitted publication has no pid")
	}
	w.mustOnePublication(t, ctx, versionID)
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 1 {
		t.Errorf("events = %d, want 1", n)
	}
}

// --------------------------------------------------------------------------
// Case 6: the pid is stable

// TestKnowledgePublishPidSurvivesRenameAndTransfer is criterion 6: the pid
// is minted at publication, is not derived from any revisable attribute, and
// does not change when the project is renamed and moved to another
// organization — the acceptance 00064 records for assets.
func TestKnowledgePublishPidSurvivesRenameAndTransfer(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-rename", "public")
	_, versionID, _ := w.seedReviewedVersion(t, ctx, p, "identity probe", 1, true)

	published := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionID, "v1.0", ""), "kp-rename-1")
	pid := w.pidOfVersion(t, ctx, versionID)
	if pid != published.PID {
		t.Fatalf("stored pid = %q, published pid = %q", pid, published.PID)
	}
	if !pidShape.MatchString(pid) {
		t.Fatalf("pid = %q, want 26 lowercase Crockford base32 characters", pid)
	}
	if pid != strings.ToLower(pid) {
		t.Errorf("pid = %q, want lowercase", pid)
	}

	// Rename the project and move it to another organization.
	otherOrg := w.createOrg(t, "kp-labs-two", "Knowledge Publish Labs Two")
	if _, err := w.pool.Exec(ctx,
		`UPDATE projects SET slug = 'kp-renamed', name = 'Renamed Project', organization_id = $2 WHERE id = $1`,
		p.id, otherOrg); err != nil {
		t.Fatalf("rename and transfer the project: %v", err)
	}

	if again := w.pidOfVersion(t, ctx, versionID); again != pid {
		t.Errorf("pid after the rename and transfer = %q, want %q", again, pid)
	}
	read := w.mustReadKnowledge(t, nil, pid)
	if read.PID != pid {
		t.Errorf("read pid = %q, want %q", read.PID, pid)
	}
	if read.PublicVersion != "v1.0" {
		t.Errorf("read public_version = %q, want v1.0", read.PublicVersion)
	}
}

// --------------------------------------------------------------------------
// The docs/22 half: the Idempotency-Key

// TestKnowledgePublishIdempotencyKeyReplays: the same Idempotency-Key
// answers the publication the first call wrote — same pid, one row, one
// event, one audit row, one ledger row — and a key that already published
// one version is a CONFLICT when reused for another (it never answers with a
// publication the caller did not ask for).
func TestKnowledgePublishIdempotencyKeyReplays(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	p := w.newProject(t, ctx, "kp-idem", "public")
	_, versionA, _ := w.seedReviewedVersion(t, ctx, p, "idempotency A", 1, true)
	_, versionB, _ := w.seedReviewedVersion(t, ctx, p, "idempotency B", 2, true)

	first := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionA, "v1.0", ""), "kp-idem-a")
	replay := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionA, "v1.0", ""), "kp-idem-a")
	if replay.PID != first.PID {
		t.Errorf("the replay answered pid %q, want the first call's %q", replay.PID, first.PID)
	}
	if n := len(w.publicationsOf(t, ctx, versionA)); n != 1 {
		t.Errorf("publications after a replay = %d, want 1", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 1 {
		t.Errorf("events after a replay = %d, want 1: a replay is a read", n)
	}
	if n := w.auditRowsOf(t, ctx, p.id, domain.ActionKnowledgeVersionPublished); n != 1 {
		t.Errorf("audit rows after a replay = %d, want 1", n)
	}
	if n := w.ledgerRowsOf(t, ctx, p.id, "kp-idem-a"); n != 1 {
		t.Errorf("ledger rows after a replay = %d, want 1", n)
	}

	// The key is scoped to the publication it wrote.
	w.mustKnowledgeError(t, w.alice, p.id, knowledgePublishBody(t, versionB, "v2.0", ""), "kp-idem-a",
		http.StatusConflict, knowledgepublish.CodeIdempotencyConflict)
	if n := len(w.publicationsOf(t, ctx, versionB)); n != 0 {
		t.Errorf("the conflicting key published version B: %d rows", n)
	}

	// A second key publishes the second version.
	second := w.mustKnowledgePublish(t, w.alice, p.id, knowledgePublishBody(t, versionB, "v2.0", ""), "kp-idem-b")
	if second.PID == first.PID {
		t.Errorf("two publications share one pid: %q", second.PID)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 2 {
		t.Errorf("events = %d, want 2", n)
	}
}

// --------------------------------------------------------------------------
// The boundary

// TestKnowledgePublishNeverCrossesTheProjectBoundary: a publish resolves the
// version INSIDE the project the route names.
//
//   - a version that belongs to another project resolves as NO version: the
//     query takes the project id as an input, so the caller who owns both
//     projects is answered "not found in this project", exactly as they
//     would be for an id that names nothing at all;
//   - a project the caller is not a member of is answered the PERMISSION
//     refusal, and the refusal answers before any target lookup, so an id
//     that names no project at all gets the same bytes: this route cannot be
//     used to probe which projects exist;
//   - and the same version, published in its own project by its owner,
//     succeeds — the refusals are about the boundary, not about the version.
func TestKnowledgePublishNeverCrossesTheProjectBoundary(t *testing.T) {
	ctx := testCtx(t)
	w := newKnowledgeWorld(t, ctx)
	w.signups(t)
	mine := w.newProject(t, ctx, "kp-mine", "public")
	_, versionID, _ := w.seedReviewedVersion(t, ctx, mine, "boundary probe", 1, true)
	other := w.newProject(t, ctx, "kp-other", "public")
	body := knowledgePublishBody(t, versionID, "v1.0", "")

	// A version of ANOTHER project: the caller owns `other` (the fixture
	// created it), so the authorization passes and the version resolution is
	// what refuses. It answers not-found, which is the same answer an id
	// that names no version gets — the project boundary is not disclosed.
	w.mustKnowledgeError(t, w.alice, other.id, body, "kp-boundary-1",
		http.StatusNotFound, knowledgepublish.CodeVersionNotFound)

	// A project alice is NOT a member of. She created it, so the fixture
	// removes her membership row (project_memberships is mutable by design —
	// the settings surface edits it); from the command's side she is a
	// signed-in stranger to a project that exists.
	foreignProject := w.newProject(t, ctx, "kp-foreign", "public")
	if _, err := w.pool.Exec(ctx,
		`DELETE FROM project_memberships WHERE project_id = $1 AND user_id = $2`,
		foreignProject.id, w.aliceID); err != nil {
		t.Fatalf("remove alice's membership in the foreign project: %v", err)
	}
	foreign := w.mustKnowledgeError(t, w.alice, foreignProject.id, body, "kp-boundary-2",
		http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval)

	// An id that names nothing: the SAME answer, byte for byte.
	unknown := w.mustKnowledgeError(t, w.alice, "99999999-9999-4999-8999-999999999999", body, "kp-boundary-3",
		http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval)
	if foreign != unknown {
		t.Errorf("a foreign project and an unknown one are distinguishable:\n%s\n%s", foreign, unknown)
	}

	// Nothing was written by any of the refusals.
	if n := len(w.publicationsOf(t, ctx, versionID)); n != 0 {
		t.Fatalf("publications = %d, want 0", n)
	}
	if n := len(w.eventsOf(t, ctx, knowledgeVersionPublishedEvent)); n != 0 {
		t.Fatalf("events = %d, want 0", n)
	}

	// The same version, published in its own project by its owner, works.
	w.mustKnowledgePublish(t, w.alice, mine.id, body, "kp-boundary-4")
	w.mustOnePublication(t, ctx, versionID)
}
