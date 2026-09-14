package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/events"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/integrity"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Task T0403: Integrity Review Checks — over a REAL PostgreSQL, with the
// same store composition cmd/api/main.go wires (the prchecks service over
// the pull-request, project, state, manifest and policy stores, the rsg
// service as the branch/commit write path). Proves the requirements and
// the acceptance criteria end to end:
//
//   - schema/provenance/dependency/rights/visibility/blob checks all run
//     and are MACHINE results: the report is derived from stored rows on
//     every call, re-running derives the same outcomes (asserted), and no
//     storage mutation happens anywhere in the read path;
//   - blocking/warning 区分: a payload integrity mismatch is a WARNING and
//     does not block (verdict pass_with_warnings), while every other
//     corruption below produces a BLOCKING failure (verdict blocked);
//   - the report is the PR page's document: per-check results with
//     subject, severity, detail and why — the wire shape apps/web renders.
//
// The corruptions are raw INSERTs, deliberately: the append-only guards
// (migrations 00014/00015) refuse UPDATE/DELETE on the version tables, and
// the rsg write path would refuse these rows by design — a direct INSERT
// is the only honest way to produce them, and it satisfies every foreign
// key, so each corruption is one specific defect, nothing else.

const prchecksTaskID = "T0403"

// allChecks is the engine's declared check list (integrity.Specs()) in
// report order — the happy path must produce at least one result per check.
var allChecks = []integrity.CheckID{
	integrity.CheckSchemaRegistered,
	integrity.CheckSchemaPayloadConforms,
	integrity.CheckBaseOnTargetChain,
	integrity.CheckProposedOnSourceChain,
	integrity.CheckSourceChainUnbroken,
	integrity.CheckCommitLinkage,
	integrity.CheckPayloadIntegrity,
	integrity.CheckRelationTypeKnown,
	integrity.CheckRelationEndpointsInProposal,
	integrity.CheckRelationEndpointTypesValid,
	integrity.CheckRightsPolicyResolves,
	integrity.CheckRightsPinInheritsDefault,
	integrity.CheckVisibilityChangeFlagged,
	integrity.CheckBlobRefsResolve,
}

// checksFixture seeds one private project owned by alice and wires the rsg
// service (branch/commit write path), the pullrequests service and the
// prchecks service over the real stores — the composition cmd/api/main.go
// uses.
type checksFixture struct {
	checks    *prchecks.Service
	prsvc     *pullrequests.Service
	svc       *rsg.Service
	statesSvc *states.Service
	pool      *pgxpool.Pool
	alice     domain.User
	project   domain.Project
	main      domain.Branch

	// happy-path artifacts the corruption subtests pin to
	m1, m2, m3 string // main's three chain states
	claimAV1   string // first claim's version id (relation endpoint)
	datasetObj string // the pinned dataset object
	datasetV1  string // its version 1 (pinned to policyX)
	policyX    string // the project policy version id
	pr1        domain.PullRequest
	c1Branch   string // C1's branch (used by the raw PR corruptions)
	c1State    string // C1's chain state (off main's chain)
	c1ClaimV   string // C1's claim version (the dangling endpoint)
}

func newChecksFixture(t *testing.T, ctx context.Context) *checksFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), prchecksTaskID)
	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "prchecks-alice@example.com", "hash", "prchecks-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	orgStore := persistence.NewOrgStore(pool)
	org, _, err := orgStore.CreateOrganization(ctx, domain.Organization{
		Slug: "prchecks-fixture", Name: "PRChecks Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "prchecks-project",
		Name:            "PRChecks Project",
		Purpose:         "fixture purpose",
		Visibility:      domain.VisibilityPrivate,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	branchStore := persistence.NewBranchStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	svc := rsg.NewService(rsg.Deps{
		Projects:  projects.NewService(projectStore, orgStore, authz.NewMatrixEngine()),
		Branches:  branches.NewService(branchStore),
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	main, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name:       "main",
		BaseRef:    "",
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch: %v", err)
	}

	return &checksFixture{
		checks: prchecks.NewService(prchecks.Deps{
			PRs:      persistence.NewPullRequestStore(pool),
			Projects: projectStore,
			States:   stateStore,
			// The branch's own head is the empty chain's boundary — the
			// read the composition root wires the same way.
			Branches: branchStore,
			Manifest: persistence.NewManifestStore(pool),
			Policies: persistence.NewPolicyStore(pool),
			Engine:   integrity.New(reg),
		}),
		prsvc:     pullrequests.NewService(persistence.NewPullRequestStore(pool)),
		svc:       svc,
		statesSvc: statesSvc,
		pool:      pool,
		alice:     alice,
		project:   project,
		main:      main,
	}
}

// commitObject creates one object on the branch through the rsg service
// (the real write path, commit guard included) and returns the branch's
// new head state id, the container object id and the new version id.
func (f *checksFixture) commitObject(t *testing.T, ctx context.Context, branch, objectType, payload string) (stateID, objectID, versionID string) {
	t.Helper()
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, branch, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    json.RawMessage(payload),
	})
	if err != nil {
		t.Fatalf("CreateObject on %s: %v", branch, err)
	}
	return *f.head(t, ctx, branch), res.Object.ID, res.Version.ID
}

// head returns the branch's current head state id.
func (f *checksFixture) head(t *testing.T, ctx context.Context, branch string) *string {
	t.Helper()
	state, err := f.statesSvc.GetBranchHead(ctx, branch)
	if err != nil {
		t.Fatalf("GetBranchHead: %v", err)
	}
	return &state.ID
}

// newFeature forks the named branch from main's current head.
func (f *checksFixture) newFeature(t *testing.T, ctx context.Context, name string) domain.Branch {
	t.Helper()
	return f.newBranchFrom(t, ctx, name, *f.head(t, ctx, f.main.ID))
}

// newBranchFrom forks the named branch from an arbitrary project state —
// the boundary-corruption subtests need a source chain whose fork point
// is not on main.
func (f *checksFixture) newBranchFrom(t *testing.T, ctx context.Context, name, baseStateID string) domain.Branch {
	t.Helper()
	branch, err := f.svc.CreateBranch(ctx, f.alice, f.project.ID, rsg.CreateBranchInput{
		Name:       name,
		BaseRef:    baseStateID,
		Visibility: domain.BranchVisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create branch %s from %s: %v", name, baseStateID, err)
	}
	return branch
}

// stateParent reads one state's parent_state_id straight from the row. The
// boundary subtests need main's genesis fork point: a real project state
// (the target chain roots from it) that no branch owns, so no chain read
// returns it.
func (f *checksFixture) stateParent(t *testing.T, ctx context.Context, stateID string) string {
	t.Helper()
	var parent *string
	if err := f.pool.QueryRow(ctx, `SELECT parent_state_id FROM project_states WHERE id = $1`, stateID).Scan(&parent); err != nil {
		t.Fatalf("read parent of state %s: %v", stateID, err)
	}
	if parent == nil {
		t.Fatalf("state %s has no parent; this fixture expects main's chain to root at the genesis state", stateID)
	}
	return *parent
}

// openPR opens a PR feature → main through the service.
func (f *checksFixture) openPR(t *testing.T, ctx context.Context, featureID, title string) domain.PullRequest {
	t.Helper()
	pr, err := f.prsvc.Create(ctx, pullrequests.CreatePullRequestParams{
		ProjectID:      f.project.ID,
		SourceBranchID: featureID,
		TargetBranchID: f.main.ID,
		Title:          title,
		Body:           "proposal context",
		CreatedBy:      f.alice.ID,
	})
	if err != nil {
		t.Fatalf("CreatePullRequest: %v", err)
	}
	return pr
}

// checkPR runs the integrity review of one PR through the service.
func (f *checksFixture) checkPR(t *testing.T, ctx context.Context, number int64) integrity.Report {
	t.Helper()
	report, err := f.checks.CheckPullRequest(ctx, f.project.ID, number)
	if err != nil {
		t.Fatalf("CheckPullRequest #%d: %v", number, err)
	}
	return report
}

// canonicalHash returns the sha256 hex digest of the payload's PostgreSQL
// canonical jsonb text — the exact bytes the store hashes (docs/21 §10).
func canonicalHash(t *testing.T, ctx context.Context, pool *pgxpool.Pool, raw string) string {
	t.Helper()
	var canonical string
	if err := pool.QueryRow(ctx, `SELECT ($1::jsonb)::text`, raw).Scan(&canonical); err != nil {
		t.Fatalf("canonicalize payload: %v", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

// claimPayload builds a schema-conforming claim payload.
func claimPayload(statement string) string {
	return fmt.Sprintf(`{"statement":%q,"claim_type":"descriptive"}`, statement)
}

// rawVersion carries one version row INSERTed directly. The rsg write path
// would refuse these rows (that is the point), and the append-only guards
// forbid UPDATEs — a direct INSERT is the only honest way to produce them.
type rawVersion struct {
	objectType    string // used when objectID is "" (a new scientific_objects row)
	objectID      string // "" = create a new object
	versionNo     int    // 0 = version 1
	stateID       string
	branchID      string // "" = NULL
	schemaID      string
	schemaVersion string // "" = schemareg.CanonicalV1
	title         string
	payload       string // JSON text; the database canonicalizes it
	pin           string // "" = NULL (unpinned)
	integrityHash string // "" = the payload's canonical hash
}

// insertVersion inserts the object row (when objectID is empty) and the
// version row, canonicalizing the payload and hashing the canonical text —
// the same discipline the store applies, so only the explicitly chosen
// field is wrong.
func (f *checksFixture) insertVersion(t *testing.T, ctx context.Context, v rawVersion) (objectID, versionID string) {
	t.Helper()
	if v.versionNo == 0 {
		v.versionNo = 1
	}
	if v.schemaVersion == "" {
		v.schemaVersion = schemareg.CanonicalV1
	}
	objectID = v.objectID
	if objectID == "" {
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO scientific_objects (project_id, object_type, created_by) VALUES ($1,$2,$3) RETURNING id`,
			f.project.ID, v.objectType, f.alice.ID).Scan(&objectID); err != nil {
			t.Fatalf("insert raw object: %v", err)
		}
	}
	if v.integrityHash == "" {
		v.integrityHash = canonicalHash(t, ctx, f.pool, v.payload)
	}
	var branchID, pin any
	if v.branchID != "" {
		branchID = v.branchID
	}
	if v.pin != "" {
		pin = v.pin
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, branch_id, schema_id, schema_version,
			 title, lifecycle_state, payload, visibility_policy_id, integrity_hash, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8::jsonb,$9,$10,$11)
		RETURNING id`,
		objectID, v.versionNo, v.stateID, branchID, v.schemaID, v.schemaVersion,
		v.title, v.payload, pin, v.integrityHash, f.alice.ID).Scan(&versionID); err != nil {
		t.Fatalf("insert raw version: %v", err)
	}
	return objectID, versionID
}

// insertProjectPolicy inserts one project-scoped policy version directly.
func (f *checksFixture) insertProjectPolicy(t *testing.T, ctx context.Context, version string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO policy_versions (project_id, version, policy_json, created_by) VALUES ($1,$2,$3::jsonb,$4) RETURNING id`,
		f.project.ID, version, `{"main_protected":true}`, f.alice.ID).Scan(&id); err != nil {
		t.Fatalf("insert policy %s: %v", version, err)
	}
	return id
}

// insertRelation inserts one relation and its version 1 directly.
func (f *checksFixture) insertRelation(t *testing.T, ctx context.Context, stateID, relationType, sourceV, targetV string) string {
	t.Helper()
	var relationID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, f.project.ID).Scan(&relationID); err != nil {
		t.Fatalf("insert raw relation: %v", err)
	}
	var versionID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO relation_versions
			(relation_id, version_no, state_id, relation_type,
			 source_object_version_id, target_object_version_id,
			 payload, integrity_hash, created_by)
		VALUES ($1,1,$2,$3,$4,$5,'{}'::jsonb,$6,$7)
		RETURNING id`,
		relationID, stateID, relationType, sourceV, targetV,
		canonicalHash(t, ctx, f.pool, `{}`), f.alice.ID).Scan(&versionID); err != nil {
		t.Fatalf("insert raw relation version: %v", err)
	}
	return versionID
}

// insertRawPR inserts one pull_requests row directly (bypassing the
// service's pin computation — the corruption is the pin itself). The
// number must not collide with the service-allocated sequence.
func (f *checksFixture) insertRawPR(t *testing.T, ctx context.Context, number int64, sourceBranchID, targetBranchID, baseStateID, proposedStateID string) {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO pull_requests (project_id, number, source_branch_id, target_branch_id,
		                           base_state_id, proposed_state_id, title, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,'raw corrupt PR',$7)`,
		f.project.ID, number, sourceBranchID, targetBranchID, baseStateID, proposedStateID, f.alice.ID); err != nil {
		t.Fatalf("insert raw PR #%d: %v", number, err)
	}
}

// resultsFor returns the report's results of one check id.
func resultsFor(report integrity.Report, check integrity.CheckID) []integrity.Result {
	var out []integrity.Result
	for _, r := range report.Results {
		if r.Check == check {
			out = append(out, r)
		}
	}
	return out
}

// failedFor returns the failed results of one check id.
func failedFor(report integrity.Report, check integrity.CheckID) []integrity.Result {
	var out []integrity.Result
	for _, r := range resultsFor(report, check) {
		if !r.Passed {
			out = append(out, r)
		}
	}
	return out
}

// mustFail asserts the check has at least one failed blocking result and
// returns the failures.
func mustFail(t *testing.T, report integrity.Report, check integrity.CheckID) []integrity.Result {
	t.Helper()
	failed := failedFor(report, check)
	if len(failed) == 0 {
		t.Fatalf("check %s: no failed results (verdict %s; explanation: %s)", check, report.Verdict, report.Explanation)
	}
	for _, r := range failed {
		if r.Severity != integrity.SeverityBlocking {
			t.Fatalf("check %s failure severity = %s, want blocking: %+v", check, r.Severity, r)
		}
	}
	return failed
}

// corruptPR seeds one fresh feature branch (forked from main's head) with
// one claim commit and an open PR — a clean canvas for one corruption.
func (f *checksFixture) corruptPR(t *testing.T, ctx context.Context, name string) (domain.Branch, string, string, domain.PullRequest) {
	t.Helper()
	branch := f.newFeature(t, ctx, name)
	stateID, _, claimV := f.commitObject(t, ctx, branch.ID, "claim", claimPayload("seed"))
	pr := f.openPR(t, ctx, branch.ID, name)
	return branch, stateID, claimV, pr
}

// TestIntegrityChecksIntegration is the T0403 integrity gate: the happy
// path derives a pass-with-warnings machine report (the unpinned versions'
// inheritance notes), and every corruption below is caught by exactly its
// check — blocking, except the payload integrity warning.
func TestIntegrityChecksIntegration(t *testing.T) {
	ctx := testCtx(t)
	f := newChecksFixture(t, ctx)

	// --- the happy path: two claims and the research question on main, one
	// project policy, one pinned dataset on main, a feature branch with one
	// hypothesis and one relation, and a PR proposing them ---
	f.m1, _, f.claimAV1 = f.commitObject(t, ctx, f.main.ID, "claim", claimPayload("alpha"))
	f.m2, _, _ = f.commitObject(t, ctx, f.main.ID, "claim", claimPayload("beta"))
	var questionObj string
	f.m3, questionObj, _ = f.commitObject(t, ctx, f.main.ID, "research_question", `{"statement":"does it hold?"}`)
	f.policyX = f.insertProjectPolicy(t, ctx, "v1")
	f.datasetObj, f.datasetV1 = f.insertVersion(t, ctx, rawVersion{
		objectType: "dataset",
		stateID:    f.m3, branchID: f.main.ID,
		schemaID: schemareg.CanonicalNamespace + "dataset.schema.json",
		title:    "Shared dataset",
		payload:  `{"purpose":"shared dataset"}`,
		pin:      f.policyX,
	})
	feature := f.newFeature(t, ctx, "feature")
	if _, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, feature.ID, rsg.CreateRelationInput{
		RelationType:          "uses",
		SourceObjectVersionID: f.claimAV1,
		TargetObjectVersionID: f.datasetV1,
	}); err != nil {
		t.Fatalf("CreateRelation: %v", err)
	}
	if _, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, feature.ID, rsg.CreateObjectInput{
		ObjectType: "hypothesis",
		Payload:    json.RawMessage(fmt.Sprintf(`{"statement":"the proposal","question_id":%q}`, questionObj)),
	}); err != nil {
		t.Fatalf("CreateObject hypothesis: %v", err)
	}
	f.pr1 = f.openPR(t, ctx, feature.ID, "Propose the hypothesis")

	t.Run("happy path derives a machine pass_with_warnings report", func(t *testing.T) {
		report := f.checkPR(t, ctx, f.pr1.Number)
		if report.Verdict != integrity.VerdictPassWithWarn {
			t.Fatalf("verdict = %s, want pass_with_warnings (blocking: %d)", report.Verdict, len(report.BlockingFailures()))
		}
		if n := len(report.BlockingFailures()); n != 0 {
			t.Fatalf("blocking failures = %d, want 0: %+v", n, report.BlockingFailures())
		}
		if report.ComputedAt == "" {
			t.Fatal("ComputedAt not stamped")
		}
		// Every declared check produced at least one result.
		seen := make(map[integrity.CheckID]bool, len(report.Results))
		for _, r := range report.Results {
			seen[r.Check] = true
		}
		for _, check := range allChecks {
			if !seen[check] {
				t.Errorf("check %s produced no result", check)
			}
		}
		// The warnings are exactly the unpinned versions' inheritance
		// notes — the pinned dataset does not warn.
		warns := report.Warnings()
		if len(warns) != 4 {
			t.Fatalf("warnings = %d, want 4 (the two claims, the research question and the hypothesis inherit the project default): %+v", len(warns), warns)
		}
		for _, w := range warns {
			if w.Check != integrity.CheckRightsPinInheritsDefault || w.Severity != integrity.SeverityWarning {
				t.Fatalf("warning = %+v, want a rights_pin_inherits_default warning", w)
			}
		}
		// The pinned dataset resolves against the project's policy.
		datasetSubject := fmt.Sprintf("dataset v1 (%s)", f.datasetObj)
		for _, r := range resultsFor(report, integrity.CheckRightsPolicyResolves) {
			if r.Subject == datasetSubject && !r.Passed {
				t.Fatalf("pinned dataset rights result = %+v, want passed", r)
			}
		}
		// Machine result: re-running derives the same outcomes (the rows
		// are append-only; nothing in the read path mutates anything).
		again := f.checkPR(t, ctx, f.pr1.Number)
		if !reflect.DeepEqual(report.Results, again.Results) || report.Verdict != again.Verdict {
			t.Fatalf("second run differs: verdict %s→%s", report.Verdict, again.Verdict)
		}
	})

	// --- the corruptions: each one INSERTs exactly one defect and must be
	// caught by exactly its check ---

	t.Run("unregistered schema blocks", func(t *testing.T) {
		branch, stateID, claimV, pr := f.corruptPR(t, ctx, "c1-unregistered-schema")
		f.c1Branch, f.c1State, f.c1ClaimV = branch.ID, stateID, claimV
		f.insertVersion(t, ctx, rawVersion{
			objectType: "dataset", stateID: stateID,
			schemaID: schemareg.CanonicalNamespace + "not-a-schema.schema.json",
			title:    "Bogus schema",
			payload:  `{}`,
		})
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckSchemaRegistered)
		if report.Verdict != integrity.VerdictBlocked {
			t.Fatalf("verdict = %s, want blocked", report.Verdict)
		}
	})

	t.Run("schema-invalid payload blocks exactly schema_payload_conforms", func(t *testing.T) {
		_, stateID, _, pr := f.corruptPR(t, ctx, "c2-missing-required")
		// The canonical dataset schema requires "purpose"; the payload
		// omits it.
		f.insertVersion(t, ctx, rawVersion{
			objectType: "dataset", stateID: stateID,
			schemaID: schemareg.CanonicalNamespace + "dataset.schema.json",
			title:    "Missing purpose",
			payload:  `{}`,
		})
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckSchemaPayloadConforms)
		if n := len(failedFor(report, integrity.CheckSchemaRegistered)); n != 0 {
			t.Fatalf("schema_registered failures = %d, want 0 (the schema IS registered): %+v", n, failedFor(report, integrity.CheckSchemaRegistered))
		}
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the conformance failure: %+v", n, report.BlockingFailures())
		}
	})

	t.Run("payload integrity mismatch is a warning and does not block (acceptance: blocking/warning 区分)", func(t *testing.T) {
		_, stateID, _, pr := f.corruptPR(t, ctx, "c3-payload-hash")
		f.insertVersion(t, ctx, rawVersion{
			objectType: "claim", stateID: stateID,
			schemaID: schemareg.CanonicalNamespace + "claim.schema.json",
			title:    "Tampered claim",
			payload:  claimPayload("tampered"),
			// the hash of DIFFERENT stored bytes
			integrityHash: canonicalHash(t, ctx, f.pool, claimPayload("original")),
		})
		report := f.checkPR(t, ctx, pr.Number)
		failed := failedFor(report, integrity.CheckPayloadIntegrity)
		if len(failed) == 0 {
			t.Fatalf("payload_integrity: no failed results")
		}
		if failed[0].Severity != integrity.SeverityWarning {
			t.Fatalf("payload_integrity severity = %s, want warning", failed[0].Severity)
		}
		if n := len(report.BlockingFailures()); n != 0 {
			t.Fatalf("blocking failures = %d, want 0 (a warning never blocks): %+v", n, report.BlockingFailures())
		}
		if report.Verdict != integrity.VerdictPassWithWarn {
			t.Fatalf("verdict = %s, want pass_with_warnings", report.Verdict)
		}
	})

	t.Run("unknown relation type blocks", func(t *testing.T) {
		_, stateID, claimV, pr := f.corruptPR(t, ctx, "c4-unknown-relation-type")
		f.insertRelation(t, ctx, stateID, "not-in-catalog", claimV, f.datasetV1)
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckRelationTypeKnown)
		if report.Verdict != integrity.VerdictBlocked {
			t.Fatalf("verdict = %s, want blocked", report.Verdict)
		}
	})

	t.Run("dangling relation endpoint blocks", func(t *testing.T) {
		_, stateID, claimV, pr := f.corruptPR(t, ctx, "c5-dangling-endpoint")
		// The target version EXISTS (the FK holds) but belongs to another
		// branch's chain — it is not part of this proposal.
		f.insertRelation(t, ctx, stateID, "uses", claimV, f.c1ClaimV)
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckRelationEndpointsInProposal)
	})

	t.Run("bogus policy pin blocks rights_policy_resolves, and a new object is never visibility-flagged", func(t *testing.T) {
		_, stateID, _, pr := f.corruptPR(t, ctx, "c6-bogus-policy-pin")
		f.insertVersion(t, ctx, rawVersion{
			objectType: "dataset", stateID: stateID,
			schemaID: schemareg.CanonicalNamespace + "dataset.schema.json",
			title:    "Bogus pin",
			payload:  `{"purpose":"pinned nowhere"}`,
			pin:      "22222222-2222-2222-2222-222222222222", // no FK: nothing stops the write
		})
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckRightsPolicyResolves)
		if n := len(failedFor(report, integrity.CheckVisibilityChangeFlagged)); n != 0 {
			t.Fatalf("visibility_change_flagged failures = %d, want 0 (new objects are not flagged): %+v", n, failedFor(report, integrity.CheckVisibilityChangeFlagged))
		}
	})

	t.Run("a policy pin change between base and proposal is flagged even between valid pins", func(t *testing.T) {
		_, stateID, _, pr := f.corruptPR(t, ctx, "c7-pin-change")
		policyY := f.insertProjectPolicy(t, ctx, "v2")
		// A new head version of the pinned dataset, pinning the OTHER
		// valid policy: rights resolve, visibility must flag.
		f.insertVersion(t, ctx, rawVersion{
			objectID: f.datasetObj, objectType: "dataset", versionNo: 2,
			stateID:  stateID,
			schemaID: schemareg.CanonicalNamespace + "dataset.schema.json",
			title:    "Shared dataset",
			payload:  `{"purpose":"revised"}`,
			pin:      policyY,
		})
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckVisibilityChangeFlagged)
		if n := len(failedFor(report, integrity.CheckRightsPolicyResolves)); n != 0 {
			t.Fatalf("rights_policy_resolves failures = %d, want 0 (the pin IS a real policy): %+v", n, failedFor(report, integrity.CheckRightsPolicyResolves))
		}
	})

	t.Run("unresolvable blob reference blocks", func(t *testing.T) {
		_, stateID, _, pr := f.corruptPR(t, ctx, "c8-blob-refs")
		f.insertVersion(t, ctx, rawVersion{
			objectType: "dataset", stateID: stateID,
			schemaID: schemareg.CanonicalNamespace + "dataset.schema.json",
			title:    "Dangling blob",
			payload:  `{"purpose":"blobs","blob_ids":["33333333-3333-3333-3333-333333333333"]}`,
		})
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckBlobRefsResolve)
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the blob failure: %+v", n, report.BlockingFailures())
		}
	})

	t.Run("a forked state breaks the source chain and the commit linkage", func(t *testing.T) {
		branch, _, _, pr := f.corruptPR(t, ctx, "c9-forked-state")
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO project_states (project_id, branch_id, parent_state_id, state_hash, manifest_version)
			VALUES ($1,$2,$3,'prchecks-forked-state','v1')`,
			f.project.ID, branch.ID, f.m1); err != nil {
			t.Fatalf("insert forked state: %v", err)
		}
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckSourceChainUnbroken)
		mustFail(t, report, integrity.CheckCommitLinkage)
	})

	t.Run("a duplicated state commit breaks the commit linkage", func(t *testing.T) {
		branch, stateID, _, pr := f.corruptPR(t, ctx, "c10-duplicate-commit")
		// The branch's single state is named by its real commit; name it
		// again. The base matches the state's parent (main's head at the
		// fork), so the duplication is the only defect.
		if _, err := f.pool.Exec(ctx, `
			INSERT INTO state_commits (project_id, branch_id, base_state_id, result_state_id,
			                           actor_id, via, message, operation_summary)
			VALUES ($1,$2,$3,$4,$5,'web','duplicate commit','{}'::jsonb)`,
			f.project.ID, branch.ID, f.m3, stateID, f.alice.ID); err != nil {
			t.Fatalf("insert duplicate commit: %v", err)
		}
		report := f.checkPR(t, ctx, pr.Number)
		mustFail(t, report, integrity.CheckCommitLinkage)
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the linkage failure: %+v", n, report.BlockingFailures())
		}
	})

	t.Run("a base pin off the target chain blocks base_on_target_chain", func(t *testing.T) {
		// A raw PR whose base names a state of the C1 feature branch —
		// real, but not main's chain.
		f.insertRawPR(t, ctx, 50, f.c1Branch, f.main.ID, f.c1State, f.c1State)
		report := f.checkPR(t, ctx, 50)
		mustFail(t, report, integrity.CheckBaseOnTargetChain)
		if report.Verdict != integrity.VerdictBlocked {
			t.Fatalf("verdict = %s, want blocked", report.Verdict)
		}
	})

	t.Run("a proposed pin off the source chain blocks proposed_on_source_chain", func(t *testing.T) {
		// A raw PR whose proposed names main's first state — real, but not
		// the source branch's chain.
		f.insertRawPR(t, ctx, 51, f.c1Branch, f.main.ID, f.m2, f.m1)
		report := f.checkPR(t, ctx, 51)
		mustFail(t, report, integrity.CheckProposedOnSourceChain)
	})

	t.Run("a proposed pin on the TARGET chain's boundary blocks proposed_on_source_chain", func(t *testing.T) {
		// The review's false PASS #1: proposed_state_id naming the target
		// chain's own legitimate boundary — main's genesis fork point, a
		// real project state the target roots from but which is nowhere
		// near the source branch's chain. Over ONE shared boundary set the
		// source-side check accepted it and this corrupt row passed the
		// machine review; the two sides must be separate sets.
		genesis := f.stateParent(t, ctx, f.m1)
		branch := f.newFeature(t, ctx, "c13-proposed-target-boundary")
		f.commitObject(t, ctx, branch.ID, "claim", claimPayload("cross-side pin"))
		// The base pin is the source's fork point (main's head), which IS
		// on the target chain — the proposed pin is the only defect.
		f.insertRawPR(t, ctx, 52, branch.ID, f.main.ID, *f.head(t, ctx, f.main.ID), genesis)
		report := f.checkPR(t, ctx, 52)
		mustFail(t, report, integrity.CheckProposedOnSourceChain)
		if n := len(failedFor(report, integrity.CheckBaseOnTargetChain)); n != 0 {
			t.Fatalf("base_on_target_chain failures = %d, want 0 (the base pin is main's head): %+v", n, failedFor(report, integrity.CheckBaseOnTargetChain))
		}
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the proposed-pin failure: %+v", n, report.BlockingFailures())
		}
	})

	t.Run("a base pin on the SOURCE chain's fork point blocks base_on_target_chain", func(t *testing.T) {
		// The review's false PASS #2: base_state_id naming the SOURCE
		// chain's fork point on a THIRD branch — a real project state the
		// source chain roots from, not on main's chain. Over the shared
		// set the target-side check accepted it because the source side
		// had resolved the very same state as its fork point.
		bridge := f.newFeature(t, ctx, "c14-bridge")
		bridgeState, _, _ := f.commitObject(t, ctx, bridge.ID, "claim", claimPayload("bridge"))
		branch := f.newBranchFrom(t, ctx, "c14-base-source-boundary", bridgeState)
		stateID, _, _ := f.commitObject(t, ctx, branch.ID, "claim", claimPayload("third branch"))
		f.insertRawPR(t, ctx, 53, branch.ID, f.main.ID, bridgeState, stateID)
		report := f.checkPR(t, ctx, 53)
		mustFail(t, report, integrity.CheckBaseOnTargetChain)
		if n := len(failedFor(report, integrity.CheckProposedOnSourceChain)); n != 0 {
			t.Fatalf("proposed_on_source_chain failures = %d, want 0 (the proposed pin is the source's head): %+v", n, failedFor(report, integrity.CheckProposedOnSourceChain))
		}
		if n := len(failedFor(report, integrity.CheckSourceChainUnbroken)); n != 0 {
			t.Fatalf("source_chain_unbroken failures = %d, want 0 (the chain roots at its resolved fork point): %+v", n, failedFor(report, integrity.CheckSourceChainUnbroken))
		}
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the base-pin failure: %+v", n, report.BlockingFailures())
		}
	})

	t.Run("an empty source chain blocks a proposed pin on a third branch's state", func(t *testing.T) {
		// The review's false PASS #3: a raw PR whose source branch has NO
		// states of its own (forked, never committed to) and whose
		// proposed pin names a state of an unrelated branch. Resolving an
		// empty chain's boundary from the row under review made the pin
		// equal to its own boundary — true for any value — so this corrupt
		// row passed the machine review. The boundary is the branch row's
		// head, and the pin must equal exactly that.
		third := f.newFeature(t, ctx, "c15-third")
		thirdState, _, _ := f.commitObject(t, ctx, third.ID, "claim", claimPayload("third branch"))
		empty := f.newFeature(t, ctx, "c15-empty-source")
		if states, err := f.statesSvc.ListStates(ctx, empty.ID); err != nil || len(states) != 0 {
			t.Fatalf("source chain = %d states (err %v), want the empty-chain shape", len(states), err)
		}
		mainHead := *f.head(t, ctx, f.main.ID)
		// The base pin is main's head, which IS on the target chain — the
		// proposed pin is the only defect.
		f.insertRawPR(t, ctx, 54, empty.ID, f.main.ID, mainHead, thirdState)
		report := f.checkPR(t, ctx, 54)
		mustFail(t, report, integrity.CheckProposedOnSourceChain)
		if report.Verdict != integrity.VerdictBlocked {
			t.Fatalf("verdict = %s, want blocked", report.Verdict)
		}
		if n := len(failedFor(report, integrity.CheckSourceChainUnbroken)); n != 0 {
			t.Fatalf("source_chain_unbroken failures = %d, want 0 (there is no chain to break): %+v", n, failedFor(report, integrity.CheckSourceChainUnbroken))
		}
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the proposed-pin failure: %+v", n, report.BlockingFailures())
		}

		// The legitimate shape the rule must keep accepting: the same
		// empty chain, pinned to the branch's own head.
		f.insertRawPR(t, ctx, 55, empty.ID, f.main.ID, mainHead, *f.head(t, ctx, empty.ID))
		if failures := f.checkPR(t, ctx, 55).BlockingFailures(); len(failures) != 0 {
			t.Fatalf("a pin equal to the empty source branch's head blocked: %+v", failures)
		}
	})

	t.Run("an empty target chain blocks a base pin on a third branch's state", func(t *testing.T) {
		// The same defect on the target side: main has no states of its
		// own in this raw PR and the base pin names an unrelated branch's
		// state.
		third := f.newFeature(t, ctx, "c16-third")
		thirdState, _, _ := f.commitObject(t, ctx, third.ID, "claim", claimPayload("third branch"))
		source := f.newFeature(t, ctx, "c16-source")
		f.commitObject(t, ctx, source.ID, "claim", claimPayload("proposal"))
		empty := f.newFeature(t, ctx, "c16-empty-target")
		if states, err := f.statesSvc.ListStates(ctx, empty.ID); err != nil || len(states) != 0 {
			t.Fatalf("target chain = %d states (err %v), want the empty-chain shape", len(states), err)
		}
		f.insertRawPR(t, ctx, 56, source.ID, empty.ID, thirdState, *f.head(t, ctx, source.ID))
		report := f.checkPR(t, ctx, 56)
		mustFail(t, report, integrity.CheckBaseOnTargetChain)
		if report.Verdict != integrity.VerdictBlocked {
			t.Fatalf("verdict = %s, want blocked", report.Verdict)
		}
		if n := len(failedFor(report, integrity.CheckProposedOnSourceChain)); n != 0 {
			t.Fatalf("proposed_on_source_chain failures = %d, want 0 (the proposed pin is the source's head): %+v", n, failedFor(report, integrity.CheckProposedOnSourceChain))
		}
		if n := len(report.BlockingFailures()); n != 1 {
			t.Fatalf("blocking failures = %d, want exactly the base-pin failure: %+v", n, report.BlockingFailures())
		}

		// The legitimate shape: the same empty target chain, pinned to the
		// target branch's own head.
		f.insertRawPR(t, ctx, 57, source.ID, empty.ID, *f.head(t, ctx, empty.ID), *f.head(t, ctx, source.ID))
		if failures := f.checkPR(t, ctx, 57).BlockingFailures(); len(failures) != 0 {
			t.Fatalf("a pin equal to the empty target branch's head blocked: %+v", failures)
		}
	})

	t.Run("service errors map to the wire vocabulary", func(t *testing.T) {
		if _, err := f.checks.CheckPullRequest(ctx, f.project.ID, 999); !errors.Is(err, prchecks.ErrPullRequestNotFound) {
			t.Fatalf("unknown PR error = %v, want ErrPullRequestNotFound", err)
		}
		if _, err := f.checks.CheckPullRequest(ctx, "", 1); !errors.Is(err, prchecks.ErrValidation) {
			t.Fatalf("empty project error = %v, want ErrValidation", err)
		}
		if _, err := f.checks.CheckPullRequest(ctx, f.project.ID, 0); !errors.Is(err, prchecks.ErrValidation) {
			t.Fatalf("number 0 error = %v, want ErrValidation", err)
		}
	})
}
