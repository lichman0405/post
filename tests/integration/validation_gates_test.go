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

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/projectshttp"
	"github.com/lichman0405/post/cmd/api/validationhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/states"
	"github.com/lichman0405/post/internal/application/validation"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/memstore"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0207: Progressive Validation Gates — over a REAL PostgreSQL, the
// five-gate ladder (draft ⊂ pr ⊂ main ⊂ release ⊂ asset) is exercised
// end-to-end: the commit guard re-validates server-side inside the commit
// transaction, and the :validate service renders the explainable ladder.
//
// The branch fixture (newStateFixture) already wires the real guard: every
// Commit below runs its gate over the rows as written, through the real
// transaction probe.

// newValidationService wires the :validate read surface over the same
// database the commits write.
func newValidationService(t *testing.T, pool *pgxpool.Pool) *validation.Service {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return validation.NewService(
		persistence.NewValidationSnapshotRepository(persistence.NewStateStore(pool)),
		rsgvalidation.NewValidator(reg),
	)
}

// writeHypothesis is a WriteFunc that writes one hypothesis object + version
// 1 with a FIXED object id (the operation summary is fixed before the write
// runs, so the object id must be known in advance). The stored integrity
// hash re-hashes the PostgreSQL-canonical payload bytes — computed by
// round-tripping the payload through jsonb, exactly the bytes the validator
// will read back. tamperHash stores a wrong hash on purpose.
func (f *stateFixture) writeHypothesis(t *testing.T, objectID, payload string, tamperHash bool, record *string) states.WriteFunc {
	t.Helper()
	return func(ctx context.Context, tx states.Transaction, stateID string) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO scientific_objects (id, project_id, object_type, created_by)
			VALUES ($1, $2, 'hypothesis', $3)`,
			objectID, f.project.ID, f.alice.ID); err != nil {
			return err
		}
		// The validator hashes the stored (jsonb-canonical) payload bytes;
		// canonicalize through the database so the hash matches exactly.
		var canonical string
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb::text`, payload).Scan(&canonical); err != nil {
			return err
		}
		hash := integrityHashOf(canonical)
		if tamperHash {
			hash = strings.Repeat("0", 64)
		}
		version, err := sqlc.New(tx).CreateScientificObjectVersion(ctx, sqlc.CreateScientificObjectVersionParams{
			ObjectID:       parseUUIDOrDie(objectID),
			VersionNo:      1,
			StateID:        parseUUIDOrDie(stateID),
			SchemaID:       "https://open-rd.example/schemas/hypothesis.schema.json",
			SchemaVersion:  "1",
			Title:          "Hypothesis " + objectID[:8],
			LifecycleState: "active",
			Payload:        []byte(payload),
			IntegrityHash:  hash,
			CreatedBy:      parseUUIDOrDie(f.alice.ID),
		})
		if err != nil {
			return err
		}
		if record != nil {
			*record = pgUUIDTextTest(version.ID)
		}
		return nil
	}
}

// writeQuestion is the research_question sibling of writeHypothesis: one
// question object + version 1 with a FIXED object id inside the commit
// transaction. migration 00040 refuses a hypothesis whose question_id
// does not name an existing research_question in the same project, so
// tests that commit hypotheses with a question_id seed their question
// through this writer first.
func (f *stateFixture) writeQuestion(t *testing.T, objectID, payload string, record *string) states.WriteFunc {
	t.Helper()
	return func(ctx context.Context, tx states.Transaction, stateID string) error {
		if _, err := tx.Exec(ctx, `
			INSERT INTO scientific_objects (id, project_id, object_type, created_by)
			VALUES ($1, $2, 'research_question', $3)`,
			objectID, f.project.ID, f.alice.ID); err != nil {
			return err
		}
		// The validator hashes the stored (jsonb-canonical) payload bytes;
		// canonicalize through the database so the hash matches exactly.
		var canonical string
		if err := tx.QueryRow(ctx, `SELECT $1::jsonb::text`, payload).Scan(&canonical); err != nil {
			return err
		}
		version, err := sqlc.New(tx).CreateScientificObjectVersion(ctx, sqlc.CreateScientificObjectVersionParams{
			ObjectID:       parseUUIDOrDie(objectID),
			VersionNo:      1,
			StateID:        parseUUIDOrDie(stateID),
			SchemaID:       "https://open-rd.example/schemas/research_question.schema.json",
			SchemaVersion:  "1",
			Title:          "Research Question " + objectID[:8],
			LifecycleState: "active",
			Payload:        []byte(payload),
			IntegrityHash:  integrityHashOf(canonical),
			CreatedBy:      parseUUIDOrDie(f.alice.ID),
		})
		if err != nil {
			return err
		}
		if record != nil {
			*record = pgUUIDTextTest(version.ID)
		}
		return nil
	}
}

// TestGatesDifferInStrictnessAndSayWhy (不同 gate 严格度不同且可解释): one
// domain-incomplete branch is validated at all five gates over the SAME
// persisted snapshot. The verdicts climb the ladder — draft passes with
// warnings, pr blocks on the type schema, main additionally blocks on the
// docs/08 domain fields, release and asset add their own blocking checks —
// and every report, every result, and every pair of gates explains why.
func TestGatesDifferInStrictnessAndSayWhy(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)
	validate := newValidationService(t, f.pool)

	// Seed one DRAFT commit whose hypothesis is domain-incomplete: it passes
	// at draft (schema_typed only warns — docs/08: a Draft may lack domain
	// fields), which is the baseline the ladder climbs from.
	incomplete := `{"statement":"probe"}`
	objectID := "44444444-4444-4444-8444-444444444444"
	ops := []domain.StateOperation{
		{Kind: domain.OperationObjectVersionCreated, EntityID: objectID, VersionNo: 1},
	}
	if _, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaWeb,
		Message:         "draft the incomplete hypothesis",
		Operations:      ops,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, f.writeHypothesis(t, objectID, incomplete, false, nil)); err != nil {
		t.Fatalf("draft Commit: %v", err)
	}

	gates := []rsgvalidation.Gate{
		rsgvalidation.GateDraft, rsgvalidation.GatePR, rsgvalidation.GateMain,
		rsgvalidation.GateRelease, rsgvalidation.GateAsset,
	}
	reports := make([]rsgvalidation.Report, len(gates))
	for i, gate := range gates {
		report, err := validate.ValidateBranch(ctx, f.project.ID, f.branch, gate)
		if err != nil {
			t.Fatalf("ValidateBranch(%s): %v", gate, err)
		}
		reports[i] = report
	}

	// The verdicts differ by gate: draft admits the incomplete hypothesis,
	// every later gate refuses it.
	if reports[0].Blocked() {
		t.Fatalf("draft verdict = %s, want pass_with_warnings (docs/08: a Draft may lack domain fields)\n%s",
			reports[0].Verdict, reports[0].Explanation)
	}
	for i := 1; i < len(gates); i++ {
		if !reports[i].Blocked() {
			t.Errorf("%s verdict = %s, want blocked (the same incomplete branch)\n%s",
				gates[i], reports[i].Verdict, reports[i].Explanation)
		}
	}

	// The SAME check moves from warning to blocking as the ladder climbs:
	// the draft's only failure is a schema_typed warning; at pr the same
	// check is blocking.
	if warns := reports[0].Warnings(); len(warns) != 1 || warns[0].Check != rsgvalidation.CheckSchemaTyped {
		t.Fatalf("draft warnings = %+v, want exactly one schema_typed warning", reports[0].Warnings())
	}
	if !hasBlocking(reports[1], rsgvalidation.CheckSchemaTyped) {
		t.Errorf("pr report does not block on schema_typed: %+v", reports[1].BlockingFailures())
	}

	// The blocking check sets grow monotonically and strictly: each gate
	// blocks on everything the previous gate blocked on, plus at least one
	// more check.
	sets := make([]map[rsgvalidation.CheckID]bool, len(gates))
	for i, report := range reports {
		sets[i] = map[rsgvalidation.CheckID]bool{}
		for _, res := range report.BlockingFailures() {
			sets[i][res.Check] = true
		}
	}
	for i := 1; i < len(gates); i++ {
		for check := range sets[i-1] {
			if !sets[i][check] {
				t.Errorf("gate %s dropped the blocking check %s of gate %s — later gates must never be looser",
					gates[i], check, gates[i-1])
			}
		}
		if len(sets[i]) <= len(sets[i-1]) {
			t.Errorf("gate %s blocks on %v, gate %s on %v — the ladder must get strictly stricter at every step",
				gates[i-1], sets[i-1], gates[i], sets[i])
		}
	}

	// Every pair of gates says exactly why the later one is stricter.
	for i := 1; i < len(gates); i++ {
		diff, err := rsgvalidation.StrictnessDiff(gates[i-1], gates[i])
		if err != nil {
			t.Fatalf("StrictnessDiff(%s, %s): %v", gates[i-1], gates[i], err)
		}
		if len(diff) == 0 {
			t.Errorf("StrictnessDiff(%s, %s) is empty — two gates of different strictness must name the difference", gates[i-1], gates[i])
		}
	}
	if diff, err := rsgvalidation.StrictnessDiff(rsgvalidation.GateDraft, rsgvalidation.GatePR); err != nil {
		t.Fatalf("StrictnessDiff(draft, pr): %v", err)
	} else if !containsLine(diff, "schema_typed: warning -> blocking") {
		t.Errorf("StrictnessDiff(draft, pr) = %v, want it to name schema_typed moving to blocking", diff)
	}

	// Every report explains itself: the gate it ran, its verdict, and every
	// failed check's rationale rendered into the explanation.
	for i, gate := range gates {
		report := reports[i]
		if !strings.Contains(report.Explanation, "gate "+string(gate)) {
			t.Errorf("%s explanation does not name the gate: %q", gate, report.Explanation)
		}
		if !strings.Contains(report.Explanation, string(report.Verdict)) {
			t.Errorf("%s explanation does not name the verdict %q: %q", gate, report.Verdict, report.Explanation)
		}
		for _, res := range report.Failures() {
			if res.Why == "" {
				t.Errorf("%s: result %s has no rationale (why) — every check must say why it runs at this gate", gate, res.Check)
			}
			if !strings.Contains(report.Explanation, string(res.Check)) {
				t.Errorf("%s explanation does not mention the failed check %s: %q", gate, res.Check, report.Explanation)
			}
			// The explanation must carry the check's rationale text, not
			// just its id: deleting the why from the renderer must fail
			// this test (可解释 pinned end-to-end over real services).
			if !strings.Contains(report.Explanation, res.Why) {
				t.Errorf("%s explanation does not carry the %s rationale %q: %q",
					gate, res.Check, res.Why, report.Explanation)
			}
		}
	}
}

// TestServerRevalidatesWhatTheCommandValidated (command 再次 server
// validate): the caller's own precheck over the branch's current head
// passes — and the server still re-runs the gate inside the commit
// transaction, over the rows as written, refusing what the precheck could
// not see:
//
//  1. a pr commit whose hypothesis is schema-incomplete is refused with
//     *GateBlockedError (schema_typed blocking) and writes nothing;
//  2. the same transition with the completed payload passes at pr;
//  3. a main commit whose payload is complete but whose stored integrity
//     hash is tampered is refused by payload_integrity — the server
//     recomputes the hash, it does not trust the one the caller stored;
//  4. the honest main commit succeeds.
func TestServerRevalidatesWhatTheCommandValidated(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)
	validate := newValidationService(t, f.pool)

	// Seed a COMPLETE research question on a sibling branch of the same
	// project: migration 00040's reference guard refuses a hypothesis
	// whose question_id does not name an existing research_question in
	// the same project at commit time. Keeping the question off f.branch
	// means every gate snapshot below stays exactly as it was — the
	// question is not a member of this branch's chain (state_linkage /
	// commit_linkage see only f.branch's states plus its head).
	var seedBranch string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO branches (project_id, name, visibility, git_ref, base_state_id, created_by)
		VALUES ($1, 'seed-questions', 'private', 'refs/heads/seed-questions', $2, $3) RETURNING id`,
		f.project.ID, f.genesis.ID, f.alice.ID).Scan(&seedBranch); err != nil {
		t.Fatalf("seed branch: %v", err)
	}
	questionID := "55555555-5555-4555-8555-555555555555"
	opsQ := []domain.StateOperation{
		{Kind: domain.OperationObjectVersionCreated, EntityID: questionID, VersionNo: 1},
	}
	if _, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        seedBranch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaWeb,
		Message:         "seed the research question the hypotheses will name",
		Operations:      opsQ,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateDraft,
	}, f.writeQuestion(t, questionID, `{"statement":"Which MOF maximizes CO2 uptake at 298 K?","purpose":"guide screening","question_state":"open"}`, nil)); err != nil {
		t.Fatalf("seed question commit: %v", err)
	}

	// Phase 0: the caller's own precheck over the branch as it stands — the
	// genesis root, no members — passes. A passing precheck must never be
	// accepted as the server's verdict: the commit below writes rows the
	// precheck could not see.
	pre, err := validate.ValidateBranch(ctx, f.project.ID, f.branch, rsgvalidation.GatePR)
	if err != nil {
		t.Fatalf("precheck ValidateBranch: %v", err)
	}
	if pre.Blocked() {
		t.Fatalf("precheck over the genesis-only branch = blocked (%s); the fixture must start from a passable snapshot", pre.Explanation)
	}

	incomplete := `{"statement":"probe"}`
	complete := fmt.Sprintf(`{"statement":"probe","question_id":"%s","hypothesis_type":"mechanistic","scope":{"detail":"probe"}}`, questionID)
	object1 := "22222222-2222-4222-8222-222222222222"
	ops1 := []domain.StateOperation{
		{Kind: domain.OperationObjectVersionCreated, EntityID: object1, VersionNo: 1},
	}

	// Phase 1: the commit at pr with the schema-incomplete hypothesis — the
	// exact write the precheck could not see — is refused server-side, and
	// the whole transition rolls back.
	beforeStates, beforeCommits, beforeObjects, beforeRelations := f.memberCounts(t, ctx)
	_, _, err = f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaWeb,
		Message:         "incomplete hypothesis must not land at pr",
		Operations:      ops1,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GatePR,
	}, f.writeHypothesis(t, object1, incomplete, false, nil))
	var blocked *rsgvalidation.GateBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("pr Commit with schema-incomplete hypothesis = %v, want *GateBlockedError (the server must re-validate, not trust the precheck)", err)
	}
	if blocked.Report.Gate != rsgvalidation.GatePR {
		t.Errorf("refusal gate = %s, want pr", blocked.Report.Gate)
	}
	if !hasBlocking(blocked.Report, rsgvalidation.CheckSchemaTyped) {
		t.Errorf("refusal does not block on schema_typed: %+v", blocked.Report.BlockingFailures())
	}
	if afterStates, afterCommits, afterObjects, afterRelations := f.memberCounts(t, ctx); afterStates != beforeStates ||
		afterCommits != beforeCommits || afterObjects != beforeObjects || afterRelations != beforeRelations {
		t.Fatalf("refused pr commit left rows behind: states %d→%d, commits %d→%d, object versions %d→%d, relation versions %d→%d",
			beforeStates, afterStates, beforeCommits, afterCommits, beforeObjects, afterObjects, beforeRelations, afterRelations)
	}
	if head, err := f.service.GetBranchHead(ctx, f.branch); err != nil || head.ID != f.genesis.ID {
		t.Errorf("branch head after refusal = %v (%v), want the untouched genesis", head.ID, err)
	}

	// Phase 2: the same transition with the completed payload passes at pr.
	state1, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaWeb,
		Message:         "complete the hypothesis",
		Operations:      ops1,
		BaseStateID:     &f.genesis.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GatePR,
	}, f.writeHypothesis(t, object1, complete, false, nil))
	if err != nil {
		t.Fatalf("pr Commit with the completed payload: %v", err)
	}

	// Phase 3: a main commit whose payload is complete but whose stored
	// integrity hash is tampered — refused by payload_integrity (blocking at
	// main), nothing written.
	object2 := "33333333-3333-4333-8333-333333333333"
	ops2 := []domain.StateOperation{
		{Kind: domain.OperationObjectVersionCreated, EntityID: object2, VersionNo: 1},
	}
	beforeStates, beforeCommits, beforeObjects, beforeRelations = f.memberCounts(t, ctx)
	_, _, err = f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaAPI,
		Message:         "tampered bytes must not land on main",
		Operations:      ops2,
		BaseStateID:     &state1.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateMain,
	}, f.writeHypothesis(t, object2, complete, true, nil))
	if !errors.As(err, &blocked) {
		t.Fatalf("main Commit with tampered integrity hash = %v, want *GateBlockedError", err)
	}
	if !hasBlocking(blocked.Report, rsgvalidation.CheckPayloadIntegrity) {
		t.Errorf("refusal does not block on payload_integrity: %+v", blocked.Report.BlockingFailures())
	}
	if blocked.Code() != "PROVENANCE_INCOMPLETE" {
		t.Errorf("refusal code = %s, want PROVENANCE_INCOMPLETE (payload_integrity is a provenance check)", blocked.Code())
	}
	if afterStates, afterCommits, afterObjects, afterRelations := f.memberCounts(t, ctx); afterStates != beforeStates ||
		afterCommits != beforeCommits || afterObjects != beforeObjects || afterRelations != beforeRelations {
		t.Fatalf("refused main commit left rows behind: states %d→%d, commits %d→%d, object versions %d→%d, relation versions %d→%d",
			beforeStates, afterStates, beforeCommits, afterCommits, beforeObjects, afterObjects, beforeRelations, afterRelations)
	}
	if head, err := f.service.GetBranchHead(ctx, f.branch); err != nil || head.ID != state1.ID {
		t.Errorf("branch head after tampered refusal = %v (%v), want state1 %s", head.ID, err, state1.ID)
	}

	// Phase 4: the honest main commit — same payload, correct hash —
	// succeeds, and the endpoint-visible main validation over the final
	// snapshot agrees.
	state2, _, err := f.service.Commit(ctx, states.CommitParams{
		ProjectID:       f.project.ID,
		BranchID:        f.branch,
		ActorID:         f.alice.ID,
		Via:             domain.ViaAPI,
		Message:         "land the honest main commit",
		Operations:      ops2,
		BaseStateID:     &state1.ID,
		ManifestVersion: "v1",
		Gate:            rsgvalidation.GateMain,
	}, f.writeHypothesis(t, object2, complete, false, nil))
	if err != nil {
		t.Fatalf("honest main Commit: %v", err)
	}
	head, err := f.service.GetBranchHead(ctx, f.branch)
	if err != nil || head.ID != state2.ID {
		t.Fatalf("branch head = %v (%v), want the honest main state %s", head.ID, err, state2.ID)
	}
	report, err := validate.ValidateBranch(ctx, f.project.ID, f.branch, rsgvalidation.GateMain)
	if err != nil {
		t.Fatalf("ValidateBranch(main): %v", err)
	}
	if report.Blocked() {
		t.Fatalf("main validation over the honest final snapshot = blocked:\n%s", report.Explanation)
	}
}

// TestValidateEndpointVisibilityOverRealServices (defect 2 of the rework):
// the :validate route runs the production graph over a REAL PostgreSQL — the
// credential store behind the real signup, the real project store, the real
// T0106 matrix engine and the real validation service on one shared pool.
// The fixture's project is PRIVATE, so the route must answer an
// authenticated non-member with the same existence-hiding 404 every other
// project read answers (T0106), and only assemble the report for a member:
// a report about a branch is exactly as visible as its project.
func TestValidateEndpointVisibilityOverRealServices(t *testing.T) {
	ctx := testCtx(t)
	f := newStateFixture(t, ctx)

	// Production composition (identical to cmd/api/main.go, the same shape
	// as privacy_test.go): real credential/project/org stores over the
	// fixture's database, in-memory sessions and limiter, the real guard.
	cfg := authn.Config{
		WebOrigin:          "http://web.test",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 1000,
		LoginLimitPerIP:    10000,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   10000,
	}
	authAPI := authhttp.New(authhttp.Deps{
		Users:      persistence.NewCredentialStore(f.pool),
		Sessions:   memstore.NewSessions(),
		Limiter:    memstore.NewLimiter(),
		OIDCClient: nil,
		Cfg:        cfg,
		Secure:     false,
	})
	projectAPI := projectshttp.New(projectshttp.Deps{
		Store: persistence.NewProjectStore(f.pool),
		Orgs:  persistence.NewOrgStore(f.pool),
		Authz: authz.NewMatrixEngine(),
	})
	validationAPI := validationhttp.New(validationhttp.Deps{
		Validator: newValidationService(t, f.pool),
		Projects:  projectAPI.Service(),
	})
	apiMux := http.NewServeMux()
	apiMux.Handle("/api/v1/auth/", authAPI.Routes())
	apiMux.Handle("/api/v1/projects", projectAPI.Routes())
	apiMux.Handle("/api/v1/projects/", projectAPI.Routes())
	validationAPI.Register(apiMux)
	ts := httptest.NewServer(authAPI.Guard(apiMux))
	defer ts.Close()

	path := "/api/v1/projects/" + f.project.ID + "/branches/" + f.branch + ":validate"

	// Two real accounts. The member is granted the owner role the same way
	// the project store writes it (a real membership row in the same
	// database); the outsider is a stranger to the private project.
	member, memberID := signup(t, ts.URL, "validate-member@example.com", "validate-member")
	outsider, _ := signup(t, ts.URL, "validate-outsider@example.com", "validate-outsider")
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1, $2, 'owner')`,
		parseUUIDOrDie(f.project.ID), parseUUIDOrDie(memberID)); err != nil {
		t.Fatalf("seed membership: %v", err)
	}

	// The private project hides the report from a non-member: the existence-
	// hiding 404, never a report about the project's branch.
	resp := outsider.do(t, http.MethodPost, path, `{"gate":"pr"}`)
	mustStatus(t, resp, http.StatusNotFound)
	mustEnvelope(t, resp, projects.CodeProjectNotFound)

	// The member gets the real report over the real services: the pr gate
	// over the genesis-only fixture branch passes.
	resp = member.do(t, http.MethodPost, path, `{"gate":"pr"}`)
	mustStatus(t, resp, http.StatusOK)
	var report rsgvalidation.Report
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		t.Fatalf("decode report: %v", err)
	}
	if report.Gate != rsgvalidation.GatePR {
		t.Errorf("report gate = %s, want pr", report.Gate)
	}
	if report.Blocked() {
		t.Fatalf("member's pr validation of the genesis-only branch = blocked:\n%s", report.Explanation)
	}
}

func hasBlocking(report rsgvalidation.Report, check rsgvalidation.CheckID) bool {
	for _, res := range report.BlockingFailures() {
		if res.Check == check {
			return true
		}
	}
	return false
}

func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}
