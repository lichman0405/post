// Task T1007 — dependency impact analysis, over REAL PostgreSQL.
//
// The required test of this task is labelled "impact golden/e2e". docs/18 §5's
// 「当上游 dependency abort/supersede/new version/rights restriction 时，分析受影响
// 下游，并创建 alert。系统只标记 review required，不自动改科学结论」 has four parts,
// and every one of them needs a database to be a claim rather than a
// description:
//
//  1. THE TRIGGER IS A RECORDED CHANGE, not a request. The graph is built
//     through the real RSG service, the upstream object is really aborted
//     through the real abort command, and the analysis runs off the published
//     event log the outbox dispatcher writes — the test never hands the
//     analysis a subject. The closure it answers with is compared against a
//     HAND-COMPUTED set, in both directions, so a walk that invented a
//     dependent and a walk that lost one are both failures, and a walk that
//     found the right NUMBER of the wrong things is a failure too.
//
//  2. WHICH EDGES COUNT IS THE CATALOG'S FLAG, not a relation type's name
//     (docs/19 §3: `references` is background knowledge, `depends_on` is an
//     actual input). The fixture therefore contains BOTH: the depends_on
//     closure that must be found, and a references-only chain that must not
//     be — plus a depends_on edge pointing the other way (the material the
//     protocol itself depends on), which is upstream of the change and must
//     not be reached either. The measured red of the flag being flipped in
//     the catalog is recorded in this task's RESULT as a mutation check.
//
//  3. THE ANSWER IS DATA, NOT A RENDERING: direct and indirect arrive as
//     different VALUES on the affected entities, with their hop counts, in
//     the alerts the analysis writes and in the report the PR first screen
//     reads.
//
//  4. THE ANALYSIS ONLY READS. Every table except the two log tables is
//     dumped before and after a pass and compared row for row, which is what
//     「不自动改科学结论」 has to mean in a system that records everything: the
//     pass adds messages and moves nothing.
//
// The alert is asserted in the same two places a subscriber sees it: as the
// outbox row the analysis writes (payload field by field) and as the research
// event the dispatcher publishes from it. Replaying the change writes no
// second alert, and the analysis does not cascade on its own output.
//
// The asset landing point (asset_dependencies, the table T0707 writes through
// the real publish route) and the privacy rule of the read surface are the
// second test. A real cmd/worker process, driven with the test never calling
// the pass itself, is the third.

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/dependencyimpact"
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

// dependencyImpactTaskID namespaces this task's test databases
// (test_T1007_<run_id>).
const dependencyImpactTaskID = "T1007"

// --------------------------------------------------------------------------
// The fixture

// impactFixture is one project's research graph, wired the way cmd/api wires
// it: the real RSG service (objects and relations), the real abort command,
// the real outbox dispatcher and the real analysis. Nothing in this file
// writes a relation_versions, branches or outbox_events row by hand — the
// graph and the events are the ones production would have, which is what
// makes the closure the analysis reports a statement about the platform
// rather than about the fixture.
type impactFixture struct {
	pool        *pgxpool.Pool
	dbURL       string
	svc         *rsg.Service
	aborts      *aborts.Service
	projectSvc  *projects.Service
	stateStore  *persistence.StateStore
	branchStore *persistence.BranchStore
	policyStore *persistence.PolicyStore
	registry    *schemareg.Registry
	dispatcher  *events.Dispatcher
	project     domain.Project
	alice       domain.User
	// main is the main branch's id: the line the objects are created on, and
	// the line the abort command resolves and requires its target to be on
	// (aborts.propose refuses a version that main does not carry).
	main string
}

func newImpactFixture(t *testing.T, ctx context.Context) *impactFixture {
	t.Helper()
	pool, dbURL := testdb.Setup(t, ctx, adminURL(t), dependencyImpactTaskID)

	alice, err := persistence.NewCredentialStore(pool).CreateWithPassword(
		ctx, "impact-alice@example.com", "hash", "impact-alice", "Alice")
	if err != nil {
		t.Fatalf("seed alice: %v", err)
	}
	org, _, err := persistence.NewOrgStore(pool).CreateOrganization(ctx, domain.Organization{
		Slug: "impact-fixture", Name: "Impact Fixture",
	}, alice.ID, todayUTC())
	if err != nil {
		t.Fatalf("create the fixture org: %v", err)
	}
	projectStore := persistence.NewProjectStore(pool)
	// CreateProject writes the creator's owner membership inside its own
	// transaction (it returns the membership, and seedDependencyProject in
	// the T0707 suite reproduces the same row for the projects it inserts
	// directly) — which is what the abort's authorization step reads.
	project, _, err := projectStore.CreateProject(ctx, domain.Project{
		OrganizationID:  &org.ID,
		Slug:            "impact-lab",
		Name:            "Impact Lab",
		Purpose:         "T1007 fixture",
		Visibility:      domain.VisibilityPublic,
		ProvisionStatus: domain.ProvisionPending,
	}, alice.ID)
	if err != nil {
		t.Fatalf("create the fixture project: %v", err)
	}

	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	stateStore := persistence.NewStateStore(pool)
	statesSvc := states.NewService(stateStore, newCommitGuard(t))
	branchStore := persistence.NewBranchStore(pool)
	branchSvc := branches.NewService(branchStore)
	projectSvc := projects.NewService(projectStore, persistence.NewOrgStore(pool), authz.NewMatrixEngine())
	prStore := persistence.NewPullRequestStore(pool)

	svc := rsg.NewService(rsg.Deps{
		Projects:  projectSvc,
		Branches:  branchSvc,
		States:    statesSvc,
		Latest:    stateStore,
		Objects:   persistence.NewScientificObjectStore(pool),
		Relations: persistence.NewRelationStore(pool),
		Authz:     authz.NewMatrixEngine(),
		Schemas:   reg,
		Events:    events.Recorder{},
	})
	// The main branch is made through the real branch command, so the branch
	// row, its genesis state and the resolution every path below performs
	// ("main" by name, domain.Branch.IsMain, the abort's main-line read) are
	// production's. CreateBranch with an empty BaseRef creates the project's
	// genesis state when there is none and forks it otherwise.
	mainBranch, err := svc.CreateBranch(ctx, alice, project.ID, rsg.CreateBranchInput{
		Name: "main", BaseRef: "", Visibility: domain.BranchVisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create the main branch: %v", err)
	}

	return &impactFixture{
		pool:        pool,
		dbURL:       dbURL,
		svc:         svc,
		projectSvc:  projectSvc,
		stateStore:  stateStore,
		branchStore: branchStore,
		policyStore: persistence.NewPolicyStore(pool),
		registry:    reg,
		dispatcher:  events.NewDispatcher(pool, events.WithLogger(outboxTestLogger())),
		project:     project,
		alice:       alice,
		main:        mainBranch.ID,
		aborts: aborts.NewService(aborts.Deps{
			Members:      projectStore,
			Authz:        authz.NewMatrixEngine(),
			Objects:      persistence.NewScientificObjectStore(pool),
			Branches:     branchSvc,
			PullRequests: pullrequests.NewService(prStore),
			Commits:      statesSvc,
			Events:       events.Recorder{},
		}),
	}
}

// impactNode is one seeded object: its identity, the version the edges pin,
// and the type the alerts must report.
type impactNode struct {
	key        string
	objectType string
	id         string
	versionID  string
	versionNo  int
}

// object creates one object on main through the real RSG service and returns
// the identities a relation and an abort need.
func (f *impactFixture) object(t *testing.T, ctx context.Context, objectType, key string) impactNode {
	t.Helper()
	res, err := f.svc.CreateObject(ctx, f.alice, f.project.ID, f.main, rsg.CreateObjectInput{
		ObjectType: objectType,
		Payload:    impactPayload(t, objectType, key),
	})
	if err != nil {
		t.Fatalf("create %s %q: %v", objectType, key, err)
	}
	if res.Object.CurrentVersionNo != res.Version.VersionNo {
		t.Fatalf("%s %q: created version no %d is not the object's current (%d)",
			objectType, key, res.Version.VersionNo, res.Object.CurrentVersionNo)
	}
	return impactNode{
		key: key, objectType: objectType, id: res.Object.ID,
		versionID: res.Version.ID, versionNo: res.Version.VersionNo,
	}
}

// relation creates one typed edge through the real RSG service, named as its
// source and target: "source depends_on target" is the catalog's own
// direction for the type (dependencyimpact.DependentTypes reads it from
// there — no direction is invented here).
func (f *impactFixture) relation(t *testing.T, ctx context.Context, relationType string, source, target impactNode) {
	t.Helper()
	if _, err := f.svc.CreateRelation(ctx, f.alice, f.project.ID, f.main, rsg.CreateRelationInput{
		RelationType:          relationType,
		SourceObjectVersionID: source.versionID,
		TargetObjectVersionID: target.versionID,
	}); err != nil {
		t.Fatalf("create %s %s -> %s: %v", relationType, source.key, target.key, err)
	}
}

// impactPayload is a minimal valid document per object type: the commit gate
// assembles the entity document by injecting the server-authoritative fields
// (id, version, project_id, title, ...) around this, so a payload carries the
// type's own CONTENT fields and nothing else.
//
// `title` in particular is NOT here: it is a server-authoritative field
// (rsgvalidation.authoritativeFields) that the commit path derives from
// name/statement/objective/purpose, and a payload that supplied it would be
// smuggling an authoritative value through the content channel.
func impactPayload(t *testing.T, objectType, key string) json.RawMessage {
	t.Helper()
	var doc map[string]any
	switch objectType {
	case "protocol":
		doc = map[string]any{"purpose": "T1007 fixture protocol " + key}
	case "experiment":
		doc = map[string]any{"objective": "T1007 fixture experiment " + key}
	case "dataset":
		doc = map[string]any{"purpose": "T1007 fixture dataset " + key}
	case "claim":
		doc = map[string]any{"statement": "T1007 fixture claim " + key, "claim_type": "descriptive"}
	case "material":
		doc = map[string]any{"name": "T1007 fixture material " + key}
	default:
		t.Fatalf("impactPayload has no document for object type %q", objectType)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("render the %s payload: %v", objectType, err)
	}
	return raw
}

// abort aborts one object through the real abort command and dispatches the
// events it recorded, returning the abort's own result.
//
// The command is the production one, with its authorization step, its
// idempotency read and its own outbox recorder: what the analysis consumes is
// exactly what a real abort leaves in the log.
func (f *impactFixture) abort(t *testing.T, ctx context.Context, node impactNode, key string) aborts.Result {
	t.Helper()
	res, err := f.aborts.AbortProposal(ctx, aborts.Actor{User: f.alice}, aborts.Input{
		ProjectID:        f.project.ID,
		ObjectID:         node.id,
		ObjectVersionRef: node.versionID,
		ReasonCode:       "upstream_defect",
		Explanation:      "T1007: the protocol this work was built on is withdrawn.",
		IdempotencyKey:   key,
	})
	if err != nil {
		t.Fatalf("abort %s: %v", node.key, err)
	}
	if res.AbortedVersionNo != node.versionNo {
		t.Fatalf("the abort aborted version %d, want the fixture's current version %d",
			res.AbortedVersionNo, node.versionNo)
	}
	if res.PullRequestNumber == 0 {
		t.Fatalf("the abort opened no proposal: %+v", res)
	}
	f.dispatch(t, ctx)
	return res
}

// dispatch drains the outbox: the log the analysis reads is the one the
// production dispatcher writes. It loops because one pass claims a bounded
// batch — a caller that ran it once and assumed the whole backlog had been
// published would be reading a log that is still filling.
func (f *impactFixture) dispatch(t *testing.T, ctx context.Context) int {
	t.Helper()
	total := 0
	for i := 0; i < 200; i++ {
		n, err := f.dispatcher.RunOnce(ctx)
		if err != nil {
			t.Fatalf("dispatcher RunOnce: %v", err)
		}
		total += n
		if n == 0 {
			return total
		}
	}
	t.Fatalf("the outbox never drained after 200 passes (%d published)", total)
	return 0
}

// impactPass runs one analysis pass through the worker's own projector — the
// same constructor cmd/worker mounts — and returns its batch.
func (f *impactFixture) impactPass(t *testing.T, ctx context.Context) dependencyimpact.Batch {
	t.Helper()
	projector := dependencyimpact.NewProjector(
		persistence.NewDependencyImpactStore(f.pool), dependencyimpact.WithLogger(outboxTestLogger()))
	batch, err := projector.RunOnce(ctx)
	if err != nil {
		t.Fatalf("dependency impact pass: %v", err)
	}
	return batch
}

// impactStore is the analysis's data side, as cmd/api and cmd/worker build it.
func (f *impactFixture) impactStore() *persistence.DependencyImpactStore {
	return persistence.NewDependencyImpactStore(f.pool)
}

// impactService is the read surface as cmd/api wires it: the same store, the
// same walk, behind the real project read gate.
func (f *impactFixture) impactService() *dependencyimpact.Service {
	return dependencyimpact.NewService(f.impactStore(), dependencyimpact.ProjectsGate(f.projectSvc))
}

// prChecks is the PR first screen's service as cmd/api wires it, with the
// analysis behind it.
func (f *impactFixture) prChecks() *prchecks.Service {
	return prchecks.NewService(prchecks.Deps{
		PRs:      persistence.NewPullRequestStore(f.pool),
		Projects: persistence.NewProjectStore(f.pool),
		States:   f.stateStore,
		Branches: f.branchStore,
		Manifest: persistence.NewManifestStore(f.pool),
		Policies: f.policyStore,
		Engine:   integrity.New(f.registry),
		Impact:   f.impactService(),
	})
}

// --------------------------------------------------------------------------
// Reading the analysis's own output

// impactAlertRow is one alert as the outbox stores it: the envelope columns
// the dispatcher will copy and the payload the analysis wrote.
type impactAlertRow struct {
	ID            string
	ProjectID     *string
	ActorID       *string
	Visibility    string
	CorrelationID string
	Payload       map[string]any
}

// impactAlertsFor reads every alert one trigger event produced, ordered by the
// affected entity.
func impactAlertsFor(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerEventID string) []impactAlertRow {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT id::text, project_id::text, actor_id::text, visibility, correlation_id, payload
		  FROM outbox_events
		 WHERE event_type = $1
		   AND payload->>'trigger_event_id' = $2
		 ORDER BY payload->>'affected_id'`,
		dependencyimpact.EventImpactDetected, triggerEventID)
	if err != nil {
		t.Fatalf("read the impact alerts: %v", err)
	}
	defer rows.Close()
	var out []impactAlertRow
	for rows.Next() {
		var r impactAlertRow
		var raw []byte
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ActorID, &r.Visibility, &r.CorrelationID, &raw); err != nil {
			t.Fatalf("scan an impact alert: %v", err)
		}
		if err := json.Unmarshal(raw, &r.Payload); err != nil {
			t.Fatalf("alert payload is not a JSON object: %v: %s", err, raw)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the impact alerts: %v", err)
	}
	return out
}

// triggerEventID resolves the research event a change produced. It reads the
// LOG rather than a command's return value: what the analysis works from is a
// row, and a test that used the command's own result could agree with the
// analysis while the log said something else.
func triggerEventID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, eventType, objectID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `
		SELECT id::text FROM research_events
		 WHERE event_type = $1 AND payload->>'object_id' = $2
		 ORDER BY occurred_at, id LIMIT 1`, eventType, objectID).Scan(&id); err != nil {
		t.Fatalf("find the %s event of object %s: %v", eventType, objectID, err)
	}
	return id
}

// publishedImpactEvent is one dependency.impact_detected research event as the
// log stores it: the envelope columns a subscriber receives plus the payload.
type publishedImpactEvent struct {
	EventType     string
	ProjectID     *string
	ActorID       *string
	Visibility    string
	CorrelationID string
	Payload       map[string]any
}

func publishedImpactEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerEventID string) []publishedImpactEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT event_type, project_id::text, actor_id::text, visibility, correlation_id, payload
		  FROM research_events
		 WHERE event_type = $1 AND payload->>'trigger_event_id' = $2
		 ORDER BY payload->>'affected_id'`, dependencyimpact.EventImpactDetected, triggerEventID)
	if err != nil {
		t.Fatalf("read the published alerts: %v", err)
	}
	defer rows.Close()
	var out []publishedImpactEvent
	for rows.Next() {
		var e publishedImpactEvent
		var raw []byte
		if err := rows.Scan(&e.EventType, &e.ProjectID, &e.ActorID, &e.Visibility, &e.CorrelationID, &raw); err != nil {
			t.Fatalf("scan a published alert: %v", err)
		}
		if err := json.Unmarshal(raw, &e.Payload); err != nil {
			t.Fatalf("published alert payload: %v: %s", err, raw)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the published alerts: %v", err)
	}
	return out
}

// impactWant is one member of the hand-computed closure: where it sits
// relative to the change.
type impactWant struct {
	directness string
	hops       int
}

// impactAlertSet folds the alerts into "which entities, how far away", failing
// on a duplicated entity rather than keeping the last one: the alert's key is
// (trigger, affected), so two rows for one entity would be the idempotency
// failing rather than a tie to break.
func impactAlertSet(t *testing.T, alerts []impactAlertRow) map[string]impactWant {
	t.Helper()
	out := make(map[string]impactWant, len(alerts))
	for _, a := range alerts {
		id, _ := a.Payload["affected_id"].(string)
		if id == "" {
			t.Fatalf("an alert names no affected_id: %v", a.Payload)
		}
		if _, dup := out[id]; dup {
			t.Fatalf("two alerts name the same affected entity %s under one trigger", id)
		}
		hops, _ := a.Payload["hops"].(float64)
		directness, _ := a.Payload["directness"].(string)
		out[id] = impactWant{directness: directness, hops: int(hops)}
	}
	return out
}

// reportSet folds a read surface's report into the same shape, so the screen's
// answer and the analysis's answer are compared as sets of the same facts.
func reportSet(t *testing.T, report dependencyimpact.Report) map[string]impactWant {
	t.Helper()
	out := map[string]impactWant{}
	for _, group := range report.Groups {
		for _, imp := range group.Impacts {
			if _, dup := out[imp.ID]; dup {
				t.Fatalf("the report names %s twice", imp.ID)
			}
			out[imp.ID] = impactWant{directness: string(imp.Directness), hops: imp.Hops}
		}
	}
	return out
}

// --------------------------------------------------------------------------
// The tables, before and after one pass

// impactLogTables are the two tables an analysis pass may write: its own
// messages. Everything else in the schema is read-only for it, which is the
// claim the snapshot comparison below makes.
var impactLogTables = []string{"outbox_events", "research_events"}

// impactSnapshot dumps every table of the public schema except the two log
// tables, as a table → sorted row-JSON list map.
//
// Row JSON rather than a count, and the whole schema rather than the rows the
// fixture knows about: 「不自动改科学结论」 is a claim about everything the pass
// touched, and a comparison that only looked at the objects the test
// remembered would miss the row it did not.
func impactSnapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
		   AND table_name <> ALL($1)
		 ORDER BY table_name`, impactLogTables)
	if err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan a table name: %v", err)
		}
		names = append(names, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("list the tables: %v", err)
	}
	if len(names) < 20 {
		t.Fatalf("found only %d tables — the snapshot is not reading the schema, so the comparison below would be vacuous", len(names))
	}
	out := make(map[string][]string, len(names))
	for _, name := range names {
		row, err := pool.Query(ctx, `SELECT row_to_json(t)::text FROM "`+name+`" t`)
		if err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		var dump []string
		for row.Next() {
			var s string
			if err := row.Scan(&s); err != nil {
				t.Fatalf("scan a %s row: %v", name, err)
			}
			dump = append(dump, s)
		}
		row.Close()
		if err := row.Err(); err != nil {
			t.Fatalf("dump %s: %v", name, err)
		}
		sort.Strings(dump)
		out[name] = dump
	}
	return out
}

// impactSnapshotDiff reports the difference between two snapshots row by row,
// so a failure names the table and the rows rather than "not equal".
func impactSnapshotDiff(before, after map[string][]string) string {
	var b strings.Builder
	for name, wantRows := range before {
		gotRows, ok := after[name]
		if !ok {
			fmt.Fprintf(&b, "table %s disappeared\n", name)
			continue
		}
		if reflect.DeepEqual(wantRows, gotRows) {
			continue
		}
		fmt.Fprintf(&b, "table %s: %d rows before, %d after\n", name, len(wantRows), len(gotRows))
		beforeSet := map[string]bool{}
		for _, r := range wantRows {
			beforeSet[r] = true
		}
		afterSet := map[string]bool{}
		for _, r := range gotRows {
			afterSet[r] = true
		}
		shown := 0
		for _, r := range wantRows {
			if !afterSet[r] {
				fmt.Fprintf(&b, "  gone:  %s\n", r)
				if shown++; shown >= 3 {
					break
				}
			}
		}
		shown = 0
		for _, r := range gotRows {
			if !beforeSet[r] {
				fmt.Fprintf(&b, "  added: %s\n", r)
				if shown++; shown >= 3 {
					break
				}
			}
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			fmt.Fprintf(&b, "table %s appeared\n", name)
		}
	}
	return b.String()
}

// --------------------------------------------------------------------------
// 1. The golden scenario

// TestDependencyImpactGoldenClosure is this task's required test — label:
// "impact golden/e2e". One protocol, aborted for real, and the closure the
// analysis reports compared against the closure a human computes from the
// graph.
//
// The graph (every edge created through the real RSG service):
//
//	protocol ──  depends_on ──▶ material     (the protocol's own input: UPSTREAM)
//	experiment ─ depends_on ──▶ protocol     (direct)
//	dataset ──── depends_on ──▶ protocol     (direct)
//	claim-1 ──── depends_on ──▶ experiment   (indirect, 2)
//	claim-2 ──── depends_on ──▶ dataset      (indirect, 2)
//	claim-3 ──── depends_on ──▶ claim-1      (indirect, 3)
//	cites-proto references ───▶ protocol     (background knowledge: NOT followed)
//	cites-exper references ───▶ experiment   (a citation edge mid-graph)
//
// The aborted protocol is the change; the hand-computed downstream set is
// exactly {experiment, dataset, claim-1, claim-2, claim-3}. The material is not
// in it (it is upstream), the two citations are not in it (their catalog flag
// says they are not dependency edges), and the protocol itself is not in it
// (the changed thing is not a thing its own change reaches).
func TestDependencyImpactGoldenClosure(t *testing.T) {
	ctx := testCtx(t)
	f := newImpactFixture(t, ctx)

	proto := f.object(t, ctx, "protocol", "protocol")
	exper := f.object(t, ctx, "experiment", "experiment")
	dataset := f.object(t, ctx, "dataset", "dataset")
	claim1 := f.object(t, ctx, "claim", "claim-1")
	claim2 := f.object(t, ctx, "claim", "claim-2")
	claim3 := f.object(t, ctx, "claim", "claim-3")
	citeProto := f.object(t, ctx, "claim", "cites-the-protocol")
	citeExper := f.object(t, ctx, "claim", "cites-the-experiment")
	material := f.object(t, ctx, "material", "upstream-material")

	f.relation(t, ctx, "depends_on", exper, proto)
	f.relation(t, ctx, "depends_on", dataset, proto)
	f.relation(t, ctx, "depends_on", claim1, exper)
	f.relation(t, ctx, "depends_on", claim2, dataset)
	f.relation(t, ctx, "depends_on", claim3, claim1)
	f.relation(t, ctx, "references", citeProto, proto)
	f.relation(t, ctx, "references", citeExper, exper)
	f.relation(t, ctx, "depends_on", proto, material)

	// Every id rendered as the fixture's own name for it, so a failure reads
	// as "the closure is missing claim-1" rather than as a uuid.
	names := map[string]string{}
	for _, n := range []impactNode{proto, exper, dataset, claim1, claim2, claim3, citeProto, citeExper, material} {
		names[n.id] = n.key
	}
	named := func(id string) string {
		if k, ok := names[id]; ok {
			return k
		}
		return id
	}

	// The hand-computed closure. Set equality in both directions below is what
	// makes this a comparison against a thought-through answer rather than
	// against a count.
	want := map[string]impactWant{
		exper.id:   {directness: string(dependencyimpact.Direct), hops: 1},
		dataset.id: {directness: string(dependencyimpact.Direct), hops: 1},
		claim1.id:  {directness: string(dependencyimpact.Indirect), hops: 2},
		claim2.id:  {directness: string(dependencyimpact.Indirect), hops: 2},
		claim3.id:  {directness: string(dependencyimpact.Indirect), hops: 3},
	}
	// Entities that must appear NOWHERE in the analysis's output at all.
	forbidden := map[string]string{
		material.id: "the material is what the protocol DEPENDS ON: upstream of the change, not downstream of it, and the walk follows the edge the other way",
		citeProto.id: "a `references` edge is background knowledge, not an input dependency (docs/19 §3) — " +
			"the catalog's DependencyInference flag is what decides, and `references` does not carry it",
		citeExper.id: "a `references` edge is not followed mid-graph either",
	}

	// The graph's own creation events are triggers too (every object version
	// creation is one), so the backlog is drained first: the pass below is then
	// about the abort and nothing else.
	//
	// The counts are the graph's own and they are exact. Five of the nine
	// version_created events have a dependent, and each produces one alert per
	// object in its TRANSITIVE closure (the walk is a closure, not a
	// neighbours list): protocol 5 (experiment+dataset at 1, claim-1+claim-2
	// at 2, claim-3 at 3), experiment 2 (claim-1, claim-3), dataset 1
	// (claim-2), claim-1 1 (claim-3), material 6 (the whole graph downstream
	// of it, protocol included). The four objects nothing depends on produce
	// none, and are reported as NoDependents rather than dropped in silence.
	// Pinning the numbers is what makes the quiet pass below a statement about
	// a drained log rather than about a scan that never worked.
	if n := f.dispatch(t, ctx); n == 0 {
		t.Fatal("the graph produced no events at all: the analysis below would be reading an empty log")
	}
	drained := f.impactPass(t, ctx)
	if drained.Candidates != 5 || drained.Emitted != 15 {
		t.Fatalf("the graph's own version_created events produced %+v, want 5 candidates and 15 alerts — anything else and the pass below is not about the abort alone", drained)
	}
	if drained.Pending != 0 {
		t.Fatalf("the drain left %d triggers pending: the pass below would be about somebody else's backlog too", drained.Pending)
	}
	f.dispatch(t, ctx)
	if quiet := f.impactPass(t, ctx); quiet.Candidates != 0 || quiet.Emitted != 0 || quiet.Pending != 0 {
		t.Fatalf("a second pass over a drained backlog reported %+v, want nothing: the analysis does not consume its own output", quiet)
	}

	// The change: the real abort command, through its own authorization,
	// idempotency and outbox recording.
	abort := f.abort(t, ctx, proto, "impact-golden-1")
	triggerID := triggerEventID(t, ctx, f.pool, "scientific_object.aborted", proto.id)

	// Everything except the two log tables, before the analysis runs.
	before := impactSnapshot(t, ctx, f.pool)

	batch := f.impactPass(t, ctx)
	if batch.Candidates != 1 {
		t.Fatalf("the abort's pass saw %d candidates, want the one change: %+v", batch.Candidates, batch)
	}
	if batch.Emitted != len(want) {
		t.Fatalf("the abort's pass emitted %d alerts, want the hand-computed closure of %d: %+v", batch.Emitted, len(want), batch)
	}
	if batch.HitDepthCap != 0 {
		t.Errorf("the walk reached the depth cap: %+v", batch)
	}
	alerts := impactAlertsFor(t, ctx, f.pool, triggerID)
	if len(alerts) != len(want) {
		t.Fatalf("%d alerts name the abort as their trigger, want %d", len(alerts), len(want))
	}

	// Set equality, both directions: every entity promised is named, every
	// entity named was promised, and each carries its own distance.
	got := impactAlertSet(t, alerts)
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("the closure is missing %s (%s at %d hops)", named(id), w.directness, w.hops)
			continue
		}
		if g != w {
			t.Errorf("%s is reported %s/%d, want %s/%d", named(id), g.directness, g.hops, w.directness, w.hops)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("the closure names %s, which the hand-computed set does not", named(id))
		}
	}
	// Nothing downstream — asserted by id AND by the reason each is excluded.
	rawAlerts, err := json.Marshal(alerts)
	if err != nil {
		t.Fatalf("render the alerts: %v", err)
	}
	if _, ok := got[proto.id]; ok {
		t.Errorf("the closure names the changed object itself: %s is not a thing its own change reaches", named(proto.id))
	}
	for id, why := range forbidden {
		if _, ok := got[id]; ok {
			t.Errorf("the closure names %s: %s", named(id), why)
		}
		if strings.Contains(string(rawAlerts), id) {
			t.Errorf("the alerts mention %s somewhere in their payloads: %s", named(id), why)
		}
	}

	// The direct/indirect distinction is in the DATA: two different values on
	// entities whose only difference is the distance.
	byDistance := map[string]map[string]bool{}
	for id, w := range got {
		if byDistance[w.directness] == nil {
			byDistance[w.directness] = map[string]bool{}
		}
		byDistance[w.directness][id] = true
	}
	if !byDistance[string(dependencyimpact.Direct)][exper.id] || !byDistance[string(dependencyimpact.Indirect)][claim3.id] {
		t.Fatalf("direct/indirect are not separable in the returned data: %v", byDistance)
	}

	// The tables moved for nobody: the pass read the graph and wrote messages.
	after := impactSnapshot(t, ctx, f.pool)
	if diff := impactSnapshotDiff(before, after); diff != "" {
		t.Errorf("the analysis pass changed stored state:\n%s", diff)
	}

	// --------------------------------------------------------------------
	// The alert as the subscriber receives it: field by field.

	assertImpactAlertPayload(t, ctx, f.pool, alerts, f.project.ID, proto, claim3, triggerID)

	// --------------------------------------------------------------------
	// The alert in the event log, and the replay that must not double it.

	if n := f.dispatch(t, ctx); n == 0 {
		t.Fatal("the dispatcher published nothing: the alerts never reached the log")
	}
	published := publishedImpactEvents(t, ctx, f.pool, triggerID)
	if len(published) != len(want) {
		t.Fatalf("the log carries %d dependency.impact_detected events for the abort, want %d", len(published), len(want))
	}
	triggerCorrelation := correlationIDOf(t, ctx, f.pool, triggerID)
	for _, ev := range published {
		if ev.Visibility != events.VisibilityPrivate {
			t.Errorf("a published alert has visibility %q, want private: the alert names two entities and the fan-out authorizes only its target", ev.Visibility)
		}
		if ev.ProjectID == nil || *ev.ProjectID != f.project.ID {
			t.Errorf("a published alert is addressed to %v, want the affected project %s", ev.ProjectID, f.project.ID)
		}
		if ev.ActorID != nil {
			t.Errorf("a published alert carries actor %q: nobody acted", *ev.ActorID)
		}
		if ev.CorrelationID != triggerCorrelation {
			t.Errorf("a published alert carries correlation id %q, want the trigger's %q", ev.CorrelationID, triggerCorrelation)
		}
	}

	// The replay: the same change, analysed again. Nothing new, and the
	// analysis does not cascade on the events it just produced.
	replay := f.impactPass(t, ctx)
	if replay.Candidates != 0 || replay.Emitted != 0 || replay.Duplicates != 0 {
		t.Errorf("replaying the change reported %+v, want an empty pass: one alert per (trigger, affected) pair, ever", replay)
	}
	if again := impactAlertsFor(t, ctx, f.pool, triggerID); len(again) != len(want) {
		t.Errorf("after the replay the abort has %d alerts, want the %d it already had", len(again), len(want))
	}
	if settled := impactSnapshot(t, ctx, f.pool); impactSnapshotDiff(after, settled) != "" {
		t.Errorf("the replay changed stored state:\n%s", impactSnapshotDiff(after, settled))
	}

	// --------------------------------------------------------------------
	// The two reads the acceptance names, both from the service layer.

	// (a) The analysis's own answer for the changed object.
	analysis, err := f.impactService().Analyze(ctx, dependencyimpact.Subject{
		Kind: dependencyimpact.SubjectObject, ID: proto.id,
	})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(analysis.Impacts) != len(want) {
		t.Fatalf("the walk returns %d impacts, want %d: %+v", len(analysis.Impacts), len(want), analysis.Impacts)
	}
	walked := map[string]impactWant{}
	for _, imp := range analysis.Impacts {
		if imp.Kind != dependencyimpact.AffectedObject {
			t.Errorf("the walk reported kind %q for %s, want %q", imp.Kind, named(imp.ID), dependencyimpact.AffectedObject)
		}
		if imp.ObjectID != imp.ID || imp.ObjectType == "" {
			t.Errorf("the impact on %s carries ObjectID=%q ObjectType=%q, want the affected object's own identity", named(imp.ID), imp.ObjectID, imp.ObjectType)
		}
		if imp.ProjectID != f.project.ID {
			t.Errorf("the impact on %s names project %s, want the fixture's %s", named(imp.ID), imp.ProjectID, f.project.ID)
		}
		walked[imp.ID] = impactWant{directness: string(imp.Directness), hops: imp.Hops}
	}
	if !reflect.DeepEqual(walked, want) {
		t.Errorf("the walk's closure = %v, want the hand-computed %v", walked, want)
	}

	// (b) The PR first screen: the abort proposal's own PR, read through the
	// PR check service (prchecks.PullRequestImpact), which derives the changed
	// objects from the PR's two manifests and asks the analysis about them.
	report, err := f.prChecks().PullRequestImpact(ctx,
		projects.Reader{UserID: f.alice.ID, Authenticated: true}, f.project.ID, abort.PullRequestNumber)
	if err != nil {
		t.Fatalf("PullRequestImpact: %v", err)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("the screen's report has %d groups, want the one changed object: %+v", len(report.Groups), report.Groups)
	}
	if group := report.Groups[0]; group.Subject.ID != proto.id {
		t.Errorf("the screen asked about %s, want the aborted protocol %s", named(group.Subject.ID), named(proto.id))
	}
	if screen := reportSet(t, report); !reflect.DeepEqual(screen, want) {
		t.Errorf("the screen's impact = %v, want the hand-computed %v", screen, want)
	}
}

// TestDependencyImpactAlertDedupeLivesInTheDatabase asks the acceptance's
// idempotency question at the layer that is supposed to answer it.
//
// The golden test replays the CHANGE and finds nothing to do: the pass's
// candidate read skips any trigger event an alert already names, so the
// second pass never reaches an insert. That is a real protection, and it is
// also not the one the design rests on. The claim the migration (00111), the
// index and events.RecordIdempotent make together is stronger: even when the
// analysis DOES re-derive the same alert — a worker restarted mid-batch, two
// processes over one log, a candidate read that ran before the other writer's
// insert landed — the second write is refused by the DATABASE, on rows that
// exist, not by the analysis remembering anything.
//
// So this test hands the insert a payload that is already stored, byte for
// byte the one the analysis wrote, in its own transaction, and reads the
// answer back. It then proves the probe can say yes as well as no: with the
// conflicting row removed inside the same transaction, the same payload is
// accepted, and the one right after it is refused again. Three verdicts on
// one payload, differing only in what the table contains — which is what
// makes "the database dedupes" a measurement rather than a reading of the
// SQL.
//
// The transaction is rolled back, so the row the control deletes is still
// there afterwards and the fixture is left exactly as the analysis wrote it.
func TestDependencyImpactAlertDedupeLivesInTheDatabase(t *testing.T) {
	ctx := testCtx(t)
	f := newImpactFixture(t, ctx)

	proto := f.object(t, ctx, "protocol", "dedupe-protocol")
	exper := f.object(t, ctx, "experiment", "dedupe-experiment")
	f.relation(t, ctx, "depends_on", exper, proto)
	f.dispatch(t, ctx)
	f.impactPass(t, ctx) // the graph's own version_created events, drained

	f.abort(t, ctx, proto, "impact-dedupe-1")
	triggerID := triggerEventID(t, ctx, f.pool, "scientific_object.aborted", proto.id)
	if batch := f.impactPass(t, ctx); batch.Emitted != 1 || batch.Duplicates != 0 {
		t.Fatalf("the abort's pass reported %+v, want the one alert and no duplicate: the probe below needs a stored alert to collide with", batch)
	}
	alerts := impactAlertsFor(t, ctx, f.pool, triggerID)
	if len(alerts) != 1 {
		t.Fatalf("%d alerts name the abort, want the one the probe replays", len(alerts))
	}
	original := alerts[0]
	if got := original.Payload[dependencyimpact.AlertFieldAffectedID]; got != exper.id {
		t.Fatalf("the alert is about %v, want the dependent %s", got, exper.id)
	}
	if original.ActorID != nil {
		t.Fatalf("the stored alert carries actor %q: nobody acted", *original.ActorID)
	}
	if original.ProjectID == nil {
		t.Fatal("the stored alert names no project: it is addressed to the affected project")
	}
	// The row was never dispatched, so nothing in research_events references
	// it and the control below can remove it inside the transaction. Asserted
	// rather than assumed: a published alert would be the FK's business, and
	// the failure would otherwise read as "the delete failed".
	if n := countRows(t, ctx, f.pool, `SELECT count(*) FROM research_events WHERE outbox_event_id = $1`, original.ID); n != 0 {
		t.Fatalf("the alert was already published (%d research event(s) reference it): the control needs the unpublished row", n)
	}

	// The replay: the same event type, the same envelope, the payload the
	// analysis wrote — re-marshalled from the stored document, so the two
	// cannot differ by a field the test forgot to copy.
	raw, err := json.Marshal(original.Payload)
	if err != nil {
		t.Fatalf("render the stored payload: %v", err)
	}
	replay := events.Event{
		EventType:     dependencyimpact.EventImpactDetected,
		ProjectID:     *original.ProjectID,
		Visibility:    original.Visibility,
		CorrelationID: original.CorrelationID,
		Payload:       raw,
	}
	key := []any{
		dependencyimpact.EventImpactDetected,
		original.Payload[dependencyimpact.AlertFieldTriggerEventID],
		original.Payload[dependencyimpact.AlertFieldAffectedKind],
		original.Payload[dependencyimpact.AlertFieldAffectedID],
	}

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the probe transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	inserted, err := events.RecordIdempotent(ctx, tx, replay)
	if err != nil {
		t.Fatalf("record the replayed alert: %v", err)
	}
	if inserted {
		t.Fatal("a payload identical to the stored alert was accepted a second time: nothing in the database is deduplicating, and a second process over the same log would write a second alert")
	}

	// The key the index dedupes on is the alert's identity, and this is that
	// claim measured: exactly one row carries these three values.
	//
	// Read through the transaction, not the pool: the control below deletes
	// the row, and the count has to describe the state the replay collided
	// with rather than the pool's.
	var keyed int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE event_type = $1 AND payload->>'trigger_event_id' = $2
		  AND payload->>'affected_kind' = $3 AND payload->>'affected_id' = $4`, key...).Scan(&keyed); err != nil {
		t.Fatalf("count the rows carrying the alert's key: %v", err)
	}
	if keyed != 1 {
		t.Fatalf("%d rows carry the alert's (trigger, affected) key, want exactly the one the replay collided with", keyed)
	}

	// The control: the same payload with nothing left to collide with. If
	// this also came back false, the verdict above would be about the call
	// never writing rather than about the row already being there.
	if _, err := tx.Exec(ctx, `DELETE FROM outbox_events WHERE id = $1`, original.ID); err != nil {
		t.Fatalf("remove the alert the replay collided with: %v", err)
	}
	inserted, err = events.RecordIdempotent(ctx, tx, replay)
	if err != nil {
		t.Fatalf("record the replayed alert with the row gone: %v", err)
	}
	if !inserted {
		t.Fatal("with the conflicting row deleted inside the same transaction, the same payload was still refused: the check above cannot tell an idempotent insert from one that writes nothing")
	}
	inserted, err = events.RecordIdempotent(ctx, tx, replay)
	if err != nil {
		t.Fatalf("record the replayed alert a third time: %v", err)
	}
	if inserted {
		t.Fatal("the row written one statement earlier did not stop the next identical write")
	}

	// The probe left nothing behind: the transaction is rolled back, so the
	// fixture's tables are exactly as the analysis left them.
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll the probe transaction back: %v", err)
	}
	after := impactAlertsFor(t, ctx, f.pool, triggerID)
	if len(after) != 1 || after[0].ID != original.ID {
		t.Fatalf("after the rolled-back probe the abort has %d alerts (first id %s), want the original %s", len(after), after[0].ID, original.ID)
	}
	if n := countRows(t, ctx, f.pool, `SELECT count(*) FROM research_events WHERE outbox_event_id = $1`, original.ID); n != 0 {
		t.Errorf("the rolled-back probe published %d research event(s)", n)
	}
}

// assertImpactAlertPayload is the acceptance's "asserted field by field": the
// alert's whole payload for one affected entity — the exact key set and every
// value.
//
// The key set is asserted as a SET rather than by reading fields out of the
// JSON, so a field that stops being written fails here instead of going
// missing silently downstream, and a field added without updating this test
// fails too: the payload is a contract, and this is where it is written down.
func assertImpactAlertPayload(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	alerts []impactAlertRow, projectID string, upstream, affected impactNode, triggerID string) {
	t.Helper()
	var found *impactAlertRow
	for i := range alerts {
		if alerts[i].Payload["affected_id"] == affected.id {
			found = &alerts[i]
		}
	}
	if found == nil {
		t.Fatalf("no alert in the batch is about %s", affected.key)
	}
	want := map[string]any{
		"trigger_event_id":    triggerID,
		"trigger_event_type":  "scientific_object.aborted",
		"trigger_occurred_at": occurredAtOf(t, ctx, pool, triggerID),
		"upstream_kind":       string(dependencyimpact.SubjectObject),
		"upstream_id":         upstream.id,
		"upstream_project_id": projectID,
		// The payload names the version that WAS aborted, not the version the
		// abort appended: "version 1 was aborted" is the message, and a
		// consumer reading the recording version's number would be told that
		// something still present had gone.
		"upstream_version_no":  float64(upstream.versionNo),
		"affected_kind":        string(dependencyimpact.AffectedObject),
		"affected_id":          affected.id,
		"affected_project_id":  projectID,
		"affected_object_id":   affected.id,
		"affected_object_type": affected.objectType,
		"directness":           string(dependencyimpact.Indirect),
		"hops":                 float64(3),
		"review_required":      true,
		// The envelope's own field, added by the events package when the row
		// is written (withPayloadVersion) and therefore present in every
		// stored payload whatever the producer put in. It is asserted here
		// because this is the STORED row: the producer's payload is asserted
		// key for key in internal/application/dependencyimpact's own test,
		// and this one starts where the envelope's contribution ends.
		"payload_version": events.DefaultPayloadVersion,
	}
	keys := make([]string, 0, len(found.Payload))
	for k := range found.Payload {
		keys = append(keys, k)
	}
	wantKeys := make([]string, 0, len(want))
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(keys)
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("alert payload keys = %v, want %v (the payload is a contract: this test is where it is written down)", keys, wantKeys)
	}
	for k, wantV := range want {
		if gotV := found.Payload[k]; gotV != wantV {
			t.Errorf("alert payload[%q] = %#v, want %#v", k, gotV, wantV)
		}
	}
	// The marker docs/18 §5 names, asserted as a FACT and not as presence:
	// the analysis never changes a scientific conclusion, so a human is the
	// only thing it can be asking for.
	if found.Payload[dependencyimpact.AlertFieldReviewRequired] != true {
		t.Error("review_required is not true: the analysis never resolves an impact by itself, so the only thing it can ask for is a review")
	}
	// The envelope. The address is the AFFECTED project — the people who owe
	// the review — never the upstream one, which would tell a publisher that
	// something of theirs is depended on.
	if found.ProjectID == nil || *found.ProjectID != projectID {
		t.Errorf("the alert's envelope project = %v, want the affected project %s", found.ProjectID, projectID)
	}
	if found.Visibility != events.VisibilityPrivate {
		t.Errorf("the alert's envelope visibility = %q, want private", found.Visibility)
	}
	if found.ActorID != nil {
		t.Errorf("the alert's envelope actor = %q: nobody acted, and the abort's actor is reachable through the trigger event", *found.ActorID)
	}
	if found.CorrelationID != correlationIDOf(t, ctx, pool, triggerID) {
		t.Errorf("the alert carries correlation id %q, want the trigger's: docs/26 §2's trace back to the request that caused it", found.CorrelationID)
	}
}

// occurredAtOf reads the instant the trigger event is stamped with. The alert
// must carry the CHANGE's time, not the analysis's: an alert stamped with the
// moment the worker got round to it would order a backlog wrongly.
func occurredAtOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerEventID string) string {
	t.Helper()
	var at time.Time
	if err := pool.QueryRow(ctx,
		`SELECT occurred_at FROM research_events WHERE id = $1`, triggerEventID).Scan(&at); err != nil {
		t.Fatalf("read the trigger's instant: %v", err)
	}
	return at.UTC().Format(time.RFC3339)
}

func correlationIDOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerEventID string) string {
	t.Helper()
	var id *string
	if err := pool.QueryRow(ctx,
		`SELECT correlation_id FROM research_events WHERE id = $1`, triggerEventID).Scan(&id); err != nil {
		t.Fatalf("read the trigger's correlation id: %v", err)
	}
	if id == nil {
		t.Fatalf("the trigger event %s carries no correlation id, so the alert cannot inherit one", triggerEventID)
	}
	return *id
}

// --------------------------------------------------------------------------
// 2. The asset landing point, and the read surface's privacy rule

// TestDependencyImpactAssetLandingPointAndPrivacy covers the second landing
// point and the disclosure rule at once, over the fixture T0707 wrote (real
// publish routes, real asset_dependencies rows) — so the rows this analysis
// reads are the rows production would have written.
//
// The graph:
//
//	up (public)      publishes asset X 1.0  — the subject, the changed version
//	down (public)    publishes asset Y 1.0, declaring a pin on X 1.0
//	hidden (private) publishes asset Z 1.0, declaring a pin on X 1.0
//
// The analysis with no reader in the way answers both declarers; the read
// surface answers a caller only what they may see. Nothing about the private
// project may reach a caller who cannot read it — not its name, not its slug,
// not its id, and not a count of what was withheld (docs/23 §5) — so the two
// UPDATE controls at the end flip one half of the rule each, which is what
// makes the absence a statement about the rule rather than about a check that
// can never see anything.
func TestDependencyImpactAssetLandingPointAndPrivacy(t *testing.T) {
	ctx := testCtx(t)
	fx := newDependencyFixture(t, ctx)
	pool := fx.world.pool

	x1 := mustPin(t, fx.xPID, "1.0")
	fx.publishVersion(t, fx.alice, fx.up, fx.xPID, "1.0", "public", fx.aliceID)
	fx.publishVersion(t, fx.bob, fx.down, fx.yPID, "1.0", "public", fx.bobID, x1)
	fx.publishVersion(t, fx.carol, fx.hidden, fx.zPID, "1.0", "public", fx.carolID, x1)
	xVersion := mustVersionID(t, ctx, pool, fx.xPID, "1.0")

	// The assertion strings are REAL rows, or every negative assertion below
	// would pass on an empty answer.
	hiddenName := projectColumn(t, ctx, pool, fx.hidden.ID, "name")
	hiddenSlug := projectColumn(t, ctx, pool, fx.hidden.ID, "slug")
	downName := projectColumn(t, ctx, pool, fx.down.ID, "name")
	if hiddenName == "" || hiddenSlug == "" || downName == "" {
		t.Fatal("the fixture's project names or slug are empty, so a negative assertion about them would be vacuous")
	}
	// Both declarations really exist, naming the version that changed — read
	// out of the table rather than assumed from the publish's 201.
	for _, p := range []dependencyProject{fx.down, fx.hidden} {
		row := rowFor(t, ctx, pool, p.ID)
		if row.AssetVersionID != xVersion || row.DependencyType != "depends_on" {
			t.Fatalf("%s's row is %+v, want a depends_on declaration of version %s", p.Slug, row, xVersion)
		}
	}

	store := persistence.NewDependencyImpactStore(pool)
	projectSvc := projects.NewService(
		persistence.NewProjectStore(pool), persistence.NewOrgStore(pool), authz.NewMatrixEngine())

	// The analysis itself, with no reader in the way: the asset landing point
	// answers the projects that pin the changed version — both of them. This
	// is the walk the worker runs; the gate is what the read below adds.
	analysis, err := dependencyimpact.NewService(store, nil).Analyze(ctx, dependencyimpact.Subject{
		Kind: dependencyimpact.SubjectAssetVersion, ID: xVersion,
	})
	if err != nil {
		t.Fatalf("Analyze of the published asset version: %v", err)
	}
	if len(analysis.Impacts) != 2 {
		t.Fatalf("the asset's closure = %+v, want the two projects that declared it", analysis.Impacts)
	}
	declarers := map[string]bool{}
	for _, imp := range analysis.Impacts {
		if imp.Kind != dependencyimpact.AffectedProject || imp.ProjectID != imp.ID {
			t.Errorf("the asset impact %+v is not a project dependent carrying its own project id", imp)
		}
		// An asset dependency is the project's own declaration, one hop away
		// (asset_dependencies' key is (project_id, asset_version_id,
		// dependency_type), and there is no further dependent of a project):
		// direct/1 is the whole truth of this landing point, not a shortcut.
		if imp.Hops != 1 || imp.Directness != dependencyimpact.Direct {
			t.Errorf("the asset impact on %s is %s/%d hops, want direct/1", imp.ID, imp.Directness, imp.Hops)
		}
		declarers[imp.ID] = true
	}
	if !declarers[fx.down.ID] || !declarers[fx.hidden.ID] {
		t.Fatalf("the asset's closure = %v, want both the public and the private declarer (no gate is in this call)", declarers)
	}

	readSvc := dependencyimpact.NewService(store, dependencyimpact.ProjectsGate(projectSvc))

	// The read surface: a caller who is a member of NOTHING.
	_, outsiderID := signup(t, fx.world.ts.URL, "impact-outsider@example.com", "impact-outsider")
	reader := projects.Reader{UserID: outsiderID, Authenticated: true}
	subjects := dependencyimpact.Subjects{{Kind: dependencyimpact.SubjectAssetVersion, ID: xVersion}}

	report, err := readSvc.Read(ctx, reader, subjects)
	if err != nil {
		t.Fatalf("Read as the outsider: %v", err)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("the outsider's report has %d groups, want the one subject asked about", len(report.Groups))
	}
	if impacts := report.Groups[0].Impacts; len(impacts) != 1 || impacts[0].ID != fx.down.ID {
		t.Fatalf("the outsider sees %+v, want only the public project's declaration (%s)", impacts, fx.down.ID)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("render the outsider's report: %v", err)
	}
	// The positive half of the assertion, and the reason the negative half is
	// not vacuous: the report DOES carry a real project identity — the public
	// declarer's row id, read out of the database — so "the private one is
	// absent" is a statement about this caller's access rather than about a
	// type that could never carry one.
	if !strings.Contains(string(raw), fx.down.ID) {
		t.Fatalf("the report does not carry the public declarer's id %s, so the absence assertions below would hold for any answer at all: %s", fx.down.ID, raw)
	}
	// Nothing about the private project — not its identity, and no field that
	// could carry a tally of it. Its NAME and SLUG cannot appear in this type
	// at all (Impact carries ids, not names); asserting them anyway is what
	// makes the guard survive a future field that does carry one. The
	// empty-string trap forbidInBody would silently accept is why they were
	// read from the table and checked non-empty above.
	forbidInBody(t, "the outsider's impact report", string(raw),
		hiddenName, hiddenSlug, fx.hidden.ID, fx.zPID, fx.zTitle)
	assertNoCountKeys(t, "the outsider's impact report", string(raw))

	// A member of the private project sees both: the withheld entry was never
	// missing from the answer, only from this caller's answer.
	carolReport, err := readSvc.Read(ctx,
		projects.Reader{UserID: fx.carolID, Authenticated: true}, subjects)
	if err != nil {
		t.Fatalf("Read as carol: %v", err)
	}
	if n := len(carolReport.Groups[0].Impacts); n != 2 {
		t.Fatalf("the private project's own member sees %d impacts, want both declarations", n)
	}

	// Control (a): the using project's visibility. Flip ONLY the hidden
	// project — carol's declaration is still the public declaration it was —
	// and the outsider's report names it.
	if _, err := pool.Exec(ctx, `UPDATE projects SET visibility = 'public' WHERE id = $1`, fx.hidden.ID); err != nil {
		t.Fatalf("flip the private project's visibility: %v", err)
	}
	control, err := readSvc.Read(ctx, reader, subjects)
	if err != nil {
		t.Fatalf("Read after the control: %v", err)
	}
	if n := len(control.Groups[0].Impacts); n != 2 {
		t.Fatalf("after the project was made public the outsider sees %d impacts (%+v), want both", n, control.Groups[0].Impacts)
	}
	controlRaw, err := json.Marshal(control)
	if err != nil {
		t.Fatalf("render the control report: %v", err)
	}
	if !strings.Contains(string(controlRaw), fx.hidden.ID) {
		t.Fatalf("the control report does not carry %s — the check above cannot see a qualifying declaration at all, so its absence proved nothing: %s", fx.hidden.ID, controlRaw)
	}

	// Control (b): the declaration's own visibility axis. `up` is public, so
	// the outsider may read it; flip only the ROW's visibility_of_usage to
	// private and the declaration becomes the project's own business
	// (assets.ProjectDependencyViewer's rule, borrowed by the read surface).
	if _, err := pool.Exec(ctx,
		`UPDATE asset_dependencies SET visibility_of_usage = 'private' WHERE project_id = $1`, fx.down.ID); err != nil {
		t.Fatalf("flip the declaration's visibility: %v", err)
	}
	second, err := readSvc.Read(ctx, reader, subjects)
	if err != nil {
		t.Fatalf("Read after the second control: %v", err)
	}
	if remaining := second.Groups[0].Impacts; len(remaining) != 1 || remaining[0].ID != fx.hidden.ID {
		t.Fatalf("with down's declaration made private the outsider sees %+v, want only %s", remaining, fx.hidden.ID)
	}
	rawSecond, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("render the second control's report: %v", err)
	}
	// Both directions on one answer: the declaration that just became private
	// is gone, and the other one — an unchanged row — is still there, so the
	// disappearance is the axis flipping rather than the read failing.
	if !strings.Contains(string(rawSecond), fx.hidden.ID) {
		t.Fatalf("the second control's report does not carry %s, the unchanged declaration: %s", fx.hidden.ID, rawSecond)
	}
	forbidInBody(t, "the outsider's report after the second control", string(rawSecond), downName, fx.down.ID)
	assertNoCountKeys(t, "the outsider's report after the second control", string(rawSecond))

	// And the declaring project's own member still sees its declaration: the
	// second axis hides it from non-members, not from the project.
	memberRead, err := readSvc.Read(ctx,
		projects.Reader{UserID: fx.bobID, Authenticated: true}, subjects)
	if err != nil {
		t.Fatalf("Read as bob: %v", err)
	}
	seen := map[string]bool{}
	for _, imp := range memberRead.Groups[0].Impacts {
		seen[imp.ID] = true
	}
	if !seen[fx.down.ID] || !seen[fx.hidden.ID] {
		t.Fatalf("the declaring project's own member sees %v, want both declarations", seen)
	}
}

// --------------------------------------------------------------------------
// 3. The analysis runs in the worker process

// TestDependencyImpactRunsInTheWorkerProcess is the acceptance's "runs in the
// worker", read the strongest way available: a real cmd/worker process,
// started with the test database's own configuration, whose startup line names
// the analysis — and the alert appearing in the log without this test ever
// calling the pass.
//
// It is the counterpart of internal/application/dependencyimpact/wiring_test.go's
// source-level check: that one proves cmd/api registers no route that could
// trigger an analysis, and this one proves a process exists that does trigger
// it, off the recorded change and nothing else.
func TestDependencyImpactRunsInTheWorkerProcess(t *testing.T) {
	ctx := testCtx(t)
	f := newImpactFixture(t, ctx)

	proto := f.object(t, ctx, "protocol", "protocol")
	exper := f.object(t, ctx, "experiment", "experiment")
	f.relation(t, ctx, "depends_on", exper, proto)

	// The change, and its event. Nothing below calls the analysis.
	f.abort(t, ctx, proto, "impact-worker-1")
	triggerID := triggerEventID(t, ctx, f.pool, "scientific_object.aborted", proto.id)

	// The worker needs Redis for its queue consumer, and the same
	// configuration cmd/worker validates in production.
	redis := startMiniRedis(t)
	binary := buildPostWorker(t)
	worker := startPostWorker(t, binary, f.dbURL, redis.Addr())

	waitFor(t, 90*time.Second, func() string {
		if impactAlertCount(t, ctx, f.pool, triggerID) == 0 {
			return "no dependency.impact_detected alert has been written yet"
		}
		return ""
	}, worker.output)

	// What the worker produced is the alert the pass produces: the dependent,
	// addressed to its project, asking for a review.
	alerts := impactAlertsFor(t, ctx, f.pool, triggerID)
	if len(alerts) != 1 {
		t.Fatalf("the worker wrote %d alerts for the abort, want the one dependent", len(alerts))
	}
	payload := alerts[0].Payload
	if payload["affected_id"] != exper.id || payload["directness"] != string(dependencyimpact.Direct) ||
		payload["hops"] != float64(1) || payload["review_required"] != true {
		t.Fatalf("the worker's alert is %v, want a direct, review-required impact on %s", payload, exper.id)
	}
	if alerts[0].ProjectID == nil || *alerts[0].ProjectID != f.project.ID {
		t.Fatalf("the worker's alert is addressed to %v, want the affected project %s", alerts[0].ProjectID, f.project.ID)
	}

	// The startup line, so an operator can tell a running analysis from one
	// that was never mounted.
	waitFor(t, 30*time.Second, func() string {
		log := worker.output()
		if !strings.Contains(log, "dependency_impact") ||
			!strings.Contains(log, "research_events -> dependency.impact_detected alerts") {
			return "the worker's startup line does not name the impact analysis"
		}
		return ""
	}, worker.output)
}

// impactAlertCount counts the alerts one trigger produced, for the poll above:
// an error answers -1 rather than failing the test, because a wait loop must
// not abort on a transient read while the worker is still starting.
func impactAlertCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, triggerEventID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM outbox_events
		  WHERE event_type = $1 AND payload->>'trigger_event_id' = $2`,
		dependencyimpact.EventImpactDetected, triggerEventID).Scan(&n); err != nil {
		return -1
	}
	return n
}
