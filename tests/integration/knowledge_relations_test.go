// Task T0501: Research Question ↔ Hypothesis relationship model.
//
// The knowledge-relation tests required by the task. They run against REAL
// PostgreSQL in a task-scoped namespace database (test_T0501_<run_id>) and
// prove, by making things fail, that migration 00040's guards hold for ANY
// write path (raw SQL here — the app services are covered elsewhere):
//
//   - knowledge relations may only pin the target endpoint their names
//     state (addresses_question -> research_question, tests_hypothesis ->
//     hypothesis); the source end is unconstrained, and every edge stays
//     inside the relation's own project;
//   - a hypothesis.question_id / research_question.parent_question_id
//     reference, when present, must be a uuid naming an existing
//     research_question of the same project, and parent chains must be
//     acyclic;
//   - the DB endpoint-type table and the Go relation catalog cannot drift;
//   - the schema enums (question_state, assessment) and the domain types
//     cannot drift;
//   - closing an Issue does not change a question's state (acceptance
//     criterion: the assessment is NOT an issue state).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const knowledgeTaskID = "T0501"

// knowledgeFixture seeds the minimal raw-SQL graph every test needs:
// alice → org → project → branch → state. The guards under test are
// database triggers, so the tests write SQL directly — deliberately NOT
// through the application services, to prove the invariant holds for any
// write path.
type knowledgeFixture struct {
	pool    *pgxpool.Pool
	alice   string
	project string
	branch  string
	state   string
}

func newKnowledgeFixture(t *testing.T, ctx context.Context) *knowledgeFixture {
	t.Helper()
	pool, _ := testdb.Setup(t, ctx, adminURL(t), knowledgeTaskID)
	uid := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %s: %v", sql, err)
		}
		return id
	}
	alice := uid(`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	org := uid(`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	project := uid(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'kp', 'KP', 'testing knowledge relations', 'private', $2) RETURNING id`, org, alice)
	branch := uid(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, project, alice)
	state := uid(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-k', 'v1') RETURNING id`, project, branch)
	return &knowledgeFixture{pool: pool, alice: alice, project: project, branch: branch, state: state}
}

// object creates a scientific object with a FIXED id and one version whose
// payload is given; returns the version id.
func (f *knowledgeFixture) object(t *testing.T, ctx context.Context, objectID, objectType, payload string) string {
	t.Helper()
	if _, err := f.pool.Exec(ctx, `INSERT INTO scientific_objects (id, project_id, object_type, created_by)
		VALUES ($1, $2, $3, $4)`, objectID, f.project, objectType, f.alice); err != nil {
		t.Fatalf("seed object %s: %v", objectID, err)
	}
	return f.version(t, ctx, objectID, objectType, payload, 1)
}

// version appends one more version row to an existing object.
func (f *knowledgeFixture) version(t *testing.T, ctx context.Context, objectID, objectType, payload string, versionNo int) string {
	t.Helper()
	var versionID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, $2, $3, $4, '1', $5, 'active', $6::jsonb, 'ih-k', $7) RETURNING id`,
		objectID, versionNo, f.state, "core/"+objectType, objectType+" title", payload, f.alice).Scan(&versionID); err != nil {
		t.Fatalf("seed version %d for %s: %v", versionNo, objectID, err)
	}
	return versionID
}

// relation inserts a relation container row; relationVersion pins one edge
// on it and returns the insert error (nil when the guard let it pass).
func (f *knowledgeFixture) relationVersion(ctx context.Context, relationID, relationType, sourceVersionID, targetVersionID string) error {
	if _, err := f.pool.Exec(ctx, `INSERT INTO relations (id, project_id) VALUES ($1, $2)`, relationID, f.project); err != nil {
		return err
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO relation_versions
		(relation_id, version_no, state_id, relation_type,
		 source_object_version_id, target_object_version_id, integrity_hash, created_by)
		VALUES ($1, 1, $2, $3, $4, $5, 'ih-k', $6)`,
		relationID, f.state, relationType, sourceVersionID, targetVersionID, f.alice)
	return err
}

// wantGuardErr asserts the deferred guard refused the statement at COMMIT
// with SQLSTATE P0001 and a message carrying the expected fragment.
func wantGuardErr(t *testing.T, what string, err error, wantSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: the guard did not fire (insert committed)", what)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: %v (not a PostgreSQL error)", what, err)
	}
	if pgErr.Code != "P0001" {
		t.Errorf("%s: SQLSTATE = %s, want P0001 (raise_exception)", what, pgErr.Code)
	}
	if !strings.Contains(pgErr.Message, wantSubstr) {
		t.Errorf("%s: message %q must contain %q", what, pgErr.Message, wantSubstr)
	}
	if pgErr.Code == "P0001" && strings.Contains(pgErr.Message, wantSubstr) {
		t.Logf("%s → rejected: SQLSTATE %s: %s", what, pgErr.Code, pgErr.Message)
	}
}

// TestKnowledgeRelationEndpointGuard: the two knowledge edges may only pin
// the target endpoint their name itself states (addresses_question: the
// target research question; tests_hypothesis: the target hypothesis) — the
// source end is unconstrained, because no spec says who may point at a
// question or a hypothesis. Every edge must stay inside the relation's own
// project; every other relation type stays unconstrained.
func TestKnowledgeRelationEndpointGuard(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	qV := f.object(t, ctx, "11111111-1111-4111-8111-111111111111", "research_question", `{"statement":"Which MOF maximizes CO2 uptake at 298 K?","question_state":"open"}`)
	hV := f.object(t, ctx, "22222222-2222-4222-8222-222222222222", "hypothesis", `{"statement":"MOF-5 maximizes uptake","question_id":"11111111-1111-4111-8111-111111111111"}`)
	eV := f.object(t, ctx, "33333333-3333-4333-8333-333333333333", "experiment", `{"name":"isotherm run"}`)
	cV := f.object(t, ctx, "44444444-4444-4444-8444-444444444444", "calculation", `{"name":"GCMC screen"}`)
	clV := f.object(t, ctx, "55555555-5555-4555-8555-555555555555", "claim", `{"statement":"MOF-5 has high uptake.","claim_type":"quantitative"}`)

	// A question of ANOTHER project: knowledge edges may not cross the
	// project boundary even when the types line up.
	var otherProject, otherState string
	if err := f.pool.QueryRow(ctx, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ((SELECT id FROM organizations WHERE slug = 'acme'), 'kp2', 'KP2', 'other project', 'private', $1) RETURNING id`, f.alice).Scan(&otherProject); err != nil {
		t.Fatalf("seed other project: %v", err)
	}
	var otherBranch string
	if err := f.pool.QueryRow(ctx, `INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, otherProject, f.alice).Scan(&otherBranch); err != nil {
		t.Fatalf("seed other branch: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-o', 'v1') RETURNING id`, otherProject, otherBranch).Scan(&otherState); err != nil {
		t.Fatalf("seed other state: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO scientific_objects (id, project_id, object_type, created_by)
		VALUES ('66666666-6666-4666-8666-666666666666', $1, 'research_question', $2)`, otherProject, f.alice); err != nil {
		t.Fatalf("seed other-project question object: %v", err)
	}
	var otherQV string
	if err := f.pool.QueryRow(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ('66666666-6666-4666-8666-666666666666', 1, $1, 'core/research_question', '1', 'q title', 'active',
		        '{"statement":"other"}', 'ih-k', $2) RETURNING id`, otherState, f.alice).Scan(&otherQV); err != nil {
		t.Fatalf("seed other-project question version: %v", err)
	}

	cases := []struct {
		name, relID, relType, src, tgt, wantSubstr string
		wantOK                                     bool
	}{
		{"valid addresses_question", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa1", "addresses_question", hV, qV, "", true},
		{"valid tests_hypothesis experiment", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa2", "tests_hypothesis", eV, hV, "", true},
		{"valid tests_hypothesis calculation", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa3", "tests_hypothesis", cV, hV, "", true},
		{"addresses_question source unrestricted", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa4", "addresses_question", eV, qV, "", true},
		{"wrong target type", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa5", "addresses_question", hV, clV, "target object type claim is not allowed", false},
		{"tests_hypothesis source unrestricted", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa6", "tests_hypothesis", hV, hV, "", true},
		{"tests_hypothesis wrong target", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa7", "tests_hypothesis", eV, qV, "target object type research_question is not allowed", false},
		{"cross-project source", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa8", "addresses_question", otherQV, qV, "different project", false},
		{"cross-project target", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa9", "tests_hypothesis", eV, otherQV, "different project", false},
		{"undeclared type stays unconstrained", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa10", "supports", clV, hV, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := f.relationVersion(ctx, c.relID, c.relType, c.src, c.tgt)
			if c.wantOK {
				if err != nil {
					t.Fatalf("%s %s -> %s refused: %v", c.relType, c.src, c.tgt, err)
				}
				return
			}
			wantGuardErr(t, c.relType, err, c.wantSubstr)
		})
	}

	// The deferred design: a relation written in the SAME transaction as
	// its endpoint versions passes — the guard fires at COMMIT, with every
	// row visible (the application's CreateRelation writes exactly this
	// shape).
	t.Run("one transaction object and relation", func(t *testing.T) {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		expID := "77777777-7777-4777-8777-777777777777"
		if _, err := tx.Exec(ctx, `INSERT INTO scientific_objects (id, project_id, object_type, created_by)
			VALUES ($1, $2, 'experiment', $3)`, expID, f.project, f.alice); err != nil {
			t.Fatalf("seed experiment object in tx: %v", err)
		}
		var expV string
		if err := tx.QueryRow(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 1, $2, 'core/experiment', '1', 'e title', 'active', '{}', 'ih-k', $3) RETURNING id`,
			expID, f.state, f.alice).Scan(&expV); err != nil {
			t.Fatalf("seed experiment version in tx: %v", err)
		}
		var relID string
		if err := tx.QueryRow(ctx, `INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, f.project).Scan(&relID); err != nil {
			t.Fatalf("seed relation in tx: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO relation_versions
			(relation_id, version_no, state_id, relation_type,
			 source_object_version_id, target_object_version_id, integrity_hash, created_by)
			VALUES ($1, 1, $2, 'tests_hypothesis', $3, $4, 'ih-k', $5)`,
			relID, f.state, expV, hV, f.alice); err != nil {
			t.Fatalf("insert relation version in tx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit with endpoint + relation in one transaction: %v", err)
		}
	})
}

// TestAddressesQuestionAcceptsFindingSource pins the main-line shape that
// T0209 depends on (rsg_query_test.go:317): a finding addresses a research
// question. The addresses_question name pins only its target ("addresses
// the target research question"); no spec says who may point at a
// question, so the source end must stay unconstrained — a previous
// version of this guard refused this edge (P0001: "source object type
// finding is not allowed") and turned the main-line test red. This test
// keeps anyone from quietly tightening the edge again.
func TestAddressesQuestionAcceptsFindingSource(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	qV := f.object(t, ctx, "88888888-8888-4888-8888-888888888888", "research_question", `{"statement":"Which MOF maximizes CO2 uptake at 298 K?","question_state":"open"}`)
	fndV := f.object(t, ctx, "99999999-9999-4999-8999-999999999999", "finding", `{"statement":"MOF-5 maximizes uptake","claim_version_refs":["cccccccc-cccc-4ccc-8ccc-cccccccccccc"]}`)

	if err := f.relationVersion(ctx, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa20", "addresses_question", fndV, qV); err != nil {
		t.Fatalf("finding -> question addresses_question refused: %v (the source end must be unconstrained)", err)
	}
}

// TestQuestionReferenceGuard: parent_question_id and question_id, when
// present, must be uuids naming an existing research_question of the same
// project; parent chains must be acyclic. Absence stays allowed at the
// database layer — the draft tolerance is the gate ladder's decision, the
// present-but-empty spelling is the semantics layer's hard error.
func TestQuestionReferenceGuard(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	qaID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb1"
	qbID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb2"
	qcID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb3"
	qdID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb4"
	qeID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb5"
	hID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbb6"

	// A three-level sub-question chain: qA (root) ← qB ← qC.
	f.object(t, ctx, qaID, "research_question", `{"statement":"root question"}`)
	f.object(t, ctx, qbID, "research_question", `{"statement":"subquestion","parent_question_id":"`+qaID+`"}`)
	f.object(t, ctx, qcID, "research_question", `{"statement":"sub-subquestion","parent_question_id":"`+qbID+`"}`)
	f.object(t, ctx, hID, "hypothesis", `{"statement":"probe"}`)

	t.Run("deep subquestion chain is valid", func(t *testing.T) {
		// A fourth level extends the chain — acyclic nesting is the point
		// of subquestions.
		f.object(t, ctx, qdID, "research_question", `{"statement":"depth four","parent_question_id":"`+qcID+`"}`)
	})

	t.Run("dangling parent refused", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 2, $2, 'core/research_question', '1', 'q title', 'active',
			        '{"parent_question_id":"cccccccc-cccc-4ccc-8ccc-cccccccccccc"}', 'ih-k', $3)`,
			qdID, f.state, f.alice)
		wantGuardErr(t, "dangling parent", err, "does not name an existing object")
	})

	t.Run("parent naming a non-question refused", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 2, $2, 'core/research_question', '1', 'q title', 'active',
			        $3, 'ih-k', $4)`,
			qdID, f.state, `{"parent_question_id":"`+hID+`"}`, f.alice)
		wantGuardErr(t, "parent naming a hypothesis", err, "names a hypothesis object, not a research_question")
	})

	t.Run("parent of another project refused", func(t *testing.T) {
		// qE lives in another project and names qA of f.project.
		var otherProject string
		if err := f.pool.QueryRow(ctx, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
			VALUES ((SELECT id FROM organizations WHERE slug = 'acme'), 'kp3', 'KP3', 'other project', 'private', $1) RETURNING id`, f.alice).Scan(&otherProject); err != nil {
			t.Fatalf("seed other project: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO scientific_objects (id, project_id, object_type, created_by)
			VALUES ($1, $2, 'research_question', $3)`, qeID, otherProject, f.alice); err != nil {
			t.Fatalf("seed cross-project question object: %v", err)
		}
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 1, $2, 'core/research_question', '1', 'q title', 'active',
			        $3, 'ih-k', $4)`,
			qeID, f.state, `{"parent_question_id":"`+qaID+`"}`, f.alice)
		wantGuardErr(t, "cross-project parent", err, "research question of another project")
	})

	t.Run("self-parent refused as a cycle", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 2, $2, 'core/research_question', '1', 'q title', 'active',
			        $3, 'ih-k', $4)`,
			qdID, f.state, `{"parent_question_id":"`+qdID+`"}`, f.alice)
		wantGuardErr(t, "self-parent", err, "cycle")
	})

	t.Run("two-question cycle refused", func(t *testing.T) {
		// qD's current version names qC. Give qC a NEW version naming qD:
		// the walk (qC → qD → qC) must be refused, not loop forever.
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 2, $2, 'core/research_question', '1', 'q title', 'active',
			        $3, 'ih-k', $4)`,
			qcID, f.state, `{"parent_question_id":"`+qdID+`"}`, f.alice)
		wantGuardErr(t, "two-question cycle", err, "cycle")
	})

	t.Run("non-uuid reference refused", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 2, $2, 'core/hypothesis', '1', 'h title', 'active',
			        '{"question_id":"q-1"}', 'ih-k', $3)`,
			hID, f.state, f.alice)
		wantGuardErr(t, "non-uuid question_id", err, "not a uuid")
	})

	t.Run("hypothesis naming its question is valid", func(t *testing.T) {
		// The hypothesis gains a version naming qA: the guard resolves it.
		f.version(t, ctx, hID, "hypothesis", `{"statement":"probe v2","question_id":"`+qaID+`"}`, 2)
	})

	t.Run("hypothesis question_id naming a non-question refused", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 3, $2, 'core/hypothesis', '1', 'h title', 'active',
			        $3, 'ih-k', $4)`,
			hID, f.state, `{"question_id":"`+hID+`"}`, f.alice)
		wantGuardErr(t, "question_id naming a hypothesis", err, "names a hypothesis object, not a research_question")
	})

	t.Run("dangling hypothesis question_id refused", func(t *testing.T) {
		_, err := f.pool.Exec(ctx, `INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, 3, $2, 'core/hypothesis', '1', 'h title', 'active',
			        '{"question_id":"dddddddd-dddd-4ddd-8ddd-dddddddddddd"}', 'ih-k', $3)`,
			hID, f.state, f.alice)
		wantGuardErr(t, "dangling question_id", err, "does not name an existing object")
	})

	t.Run("absent reference stays allowed at the database layer", func(t *testing.T) {
		// A draft hypothesis without question_id and a root question
		// without parent_question_id commit fine; the gate ladder (and the
		// semantics hint) are the authorities on whether that omission is
		// acceptable — the guard polices only references that ARE given.
		f.object(t, ctx, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee1", "hypothesis", `{"statement":"draft without question"}`)
		f.object(t, ctx, "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeee2", "research_question", `{"statement":"another root"}`)
	})

	t.Run("question and hypothesis in one transaction, child first", func(t *testing.T) {
		// The deferred guard sees the whole transaction at COMMIT: the
		// hypothesis naming a question that is inserted AFTER it in the
		// same transaction still passes (the question_id is payload data,
		// not a foreign key — the guard is what relates the rows).
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx)
		childQ := "ffffffff-ffff-4fff-8fff-fffffffffff1"
		parentQ := "ffffffff-ffff-4fff-8fff-fffffffffff2"
		for _, q := range []struct {
			id, payload string
		}{
			{childQ, `{"statement":"child first","parent_question_id":"` + parentQ + `"}`},
			{parentQ, `{"statement":"parent second"}`},
		} {
			if _, err := tx.Exec(ctx, `INSERT INTO scientific_objects (id, project_id, object_type, created_by)
				VALUES ($1, $2, 'research_question', $3)`, q.id, f.project, f.alice); err != nil {
				t.Fatalf("seed question object in tx: %v", err)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO scientific_object_versions
				(object_id, version_no, state_id, schema_id, schema_version, title,
				 lifecycle_state, payload, integrity_hash, created_by)
				VALUES ($1, 1, $2, 'core/research_question', '1', 'q title', 'active', $3::jsonb, 'ih-k', $4)`,
				q.id, f.state, q.payload, f.alice); err != nil {
				t.Fatalf("seed question version in tx: %v", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit with child before parent in one transaction: %v", err)
		}
	})
}

// TestKnowledgeRelationEndpointTypesMatchCatalog pins the DB declaration
// (knowledge_relation_endpoint_types) to the Go relation catalog: the two
// must agree on the declared knowledge types and their name-pinned target
// lists (the source end stays NULL/unconstrained on both sides), or one of
// them drifted.
func TestKnowledgeRelationEndpointTypesMatchCatalog(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	want := map[string]struct{ sources, targets []string }{
		"addresses_question": {targets: []string{"research_question"}}, // sources nil: the name pins only the target
		"tests_hypothesis":   {targets: []string{"hypothesis"}},        // sources nil: ditto
	}

	rows, err := f.pool.Query(ctx, `SELECT relation_type, source_object_types, target_object_types
		FROM knowledge_relation_endpoint_types ORDER BY relation_type`)
	if err != nil {
		t.Fatalf("read knowledge_relation_endpoint_types: %v", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var relType string
		var sources, targets []string
		if err := rows.Scan(&relType, &sources, &targets); err != nil {
			t.Fatalf("scan endpoint type row: %v", err)
		}
		seen[relType] = true
		if sources != nil {
			t.Errorf("DB declares source endpoint types for %s (%v); the edge's name pins only its target", relType, sources)
		}
		w, ok := want[relType]
		if !ok {
			t.Errorf("DB declares endpoint types for %s, which the catalog does not constrain", relType)
			continue
		}
		if len(sources) != len(w.sources) || len(targets) != len(w.targets) {
			t.Errorf("DB %s = %v -> %v, catalog wants %v -> %v", relType, sources, targets, w.sources, w.targets)
			continue
		}
		for i := range w.sources {
			if sources[i] != w.sources[i] {
				t.Errorf("DB %s source [%d] = %s, catalog wants %s", relType, i, sources[i], w.sources[i])
			}
		}
		for i := range w.targets {
			if targets[i] != w.targets[i] {
				t.Errorf("DB %s target [%d] = %s, catalog wants %s", relType, i, targets[i], w.targets[i])
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate endpoint type rows: %v", err)
	}
	for relType := range want {
		if !seen[relType] {
			t.Errorf("DB declares no endpoint types for %s; the catalog constrains it", relType)
		}
	}
	// The Go-side declaration and the DB guard must also agree on the
	// actual verdicts: the catalog's EndpointsValid mirrors the trigger.
	// The source end is unconstrained, so ANY source type must pass the
	// declared target — and a target the name does not pin must fail.
	for _, e := range relationcatalog.Types() {
		if e.Type != "addresses_question" && e.Type != "tests_hypothesis" {
			continue
		}
		w := want[e.Type]
		ok, _ := relationcatalog.EndpointsValid(e.Type, "finding", w.targets[0])
		if !ok {
			t.Errorf("catalog EndpointsValid(%s, finding, %s) = false, want true (catalog drifted from its own declaration)", e.Type, w.targets[0])
		}
		ok, _ = relationcatalog.EndpointsValid(e.Type, "finding", "claim")
		if ok {
			t.Errorf("catalog EndpointsValid(%s, finding, claim) = true, want false (the target is pinned by the edge's name)", e.Type)
		}
	}
}

// assembledQuestionDoc / assembledHypothesisDoc build the full documents
// the validator assembles (server-authoritative fields + payload), so the
// schema's own enum can be interrogated through the public registry API.
func assembledQuestionDoc(questionState string) map[string]any {
	return map[string]any{
		"id": "11111111-1111-4111-8111-111111111111", "type": "research_question",
		"version": 1, "project_id": "22222222-2222-4222-8222-222222222222",
		"title": "q", "lifecycle_state": "active",
		"schema_ref": map[string]any{"id": "https://open-rd.example/schemas/research_question.schema.json", "version": "1"},
		"created_by": "33333333-3333-4333-8333-333333333333",
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"statement":  "Which MOF maximizes CO2 uptake at 298 K?", "question_state": questionState,
	}
}

func assembledHypothesisDoc(assessment string) map[string]any {
	return map[string]any{
		"id": "11111111-1111-4111-8111-111111111111", "type": "hypothesis",
		"version": 1, "project_id": "22222222-2222-4222-8222-222222222222",
		"title": "h", "lifecycle_state": "active",
		"schema_ref": map[string]any{"id": "https://open-rd.example/schemas/hypothesis.schema.json", "version": "1"},
		"created_by": "33333333-3333-4333-8333-333333333333",
		"created_at": time.Now().UTC().Format(time.RFC3339),
		"statement":  "MOF-5 maximizes uptake", "question_id": "44444444-4444-4444-8444-444444444444",
		"assessment": assessment,
	}
}

// TestSchemaEnumsMatchDomainTypes pins the domain's question-state and
// assessment vocabularies to the runtime schema registry: every canonical
// value must validate, and the issue states must never validate — the
// assessment is NOT an issue state.
func TestSchemaEnumsMatchDomainTypes(t *testing.T) {
	ctx := testCtx(t)
	_ = newKnowledgeFixture(t, ctx) // runs in a real migrated T0501 database, like the rest
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	rq, ok := reg.Get(schemareg.Ref{ID: "https://open-rd.example/schemas/research_question.schema.json", Version: "1"})
	if !ok {
		t.Fatal("research_question schema not registered")
	}
	h, ok := reg.Get(schemareg.Ref{ID: "https://open-rd.example/schemas/hypothesis.schema.json", Version: "1"})
	if !ok {
		t.Fatal("hypothesis schema not registered")
	}

	for _, s := range domain.CanonicalQuestionStates() {
		doc, err := json.Marshal(assembledQuestionDoc(s))
		if err != nil {
			t.Fatalf("marshal question doc: %v", err)
		}
		if err := rq.Validate(doc); err != nil {
			t.Errorf("schema refuses canonical question_state %q: %v", s, err)
		}
	}
	for _, s := range []string{"closed", "in_progress", "partially-answered", "answered"} {
		doc, err := json.Marshal(assembledQuestionDoc(s))
		if err != nil {
			t.Fatalf("marshal question doc: %v", err)
		}
		if err := rq.Validate(doc); err == nil {
			t.Errorf("schema ACCEPTS non-canonical question_state %q — the domain/schema vocabulary drifted", s)
		}
	}
	for _, a := range domain.CanonicalHypothesisAssessments() {
		doc, err := json.Marshal(assembledHypothesisDoc(a))
		if err != nil {
			t.Fatalf("marshal hypothesis doc: %v", err)
		}
		if err := h.Validate(doc); err != nil {
			t.Errorf("schema refuses canonical assessment %q: %v", a, err)
		}
	}
	for _, a := range []string{"open", "closed", "in_progress", "Under_test"} {
		doc, err := json.Marshal(assembledHypothesisDoc(a))
		if err != nil {
			t.Fatalf("marshal hypothesis doc: %v", err)
		}
		if err := h.Validate(doc); err == nil {
			t.Errorf("schema ACCEPTS non-canonical assessment %q — the domain/schema vocabulary drifted", a)
		}
	}
}

// TestIssueCloseDoesNotChangeQuestionState is the acceptance criterion:
// closing an Issue never changes a research question's state. The
// question's versioned content (payload bytes, integrity hash, version
// log) must be untouched by the issue lifecycle — the assessment is NOT
// an issue state — while a legitimate content evolution (a new version)
// still goes through.
func TestIssueCloseDoesNotChangeQuestionState(t *testing.T) {
	ctx := testCtx(t)
	f := newKnowledgeFixture(t, ctx)

	qID := "99999999-9999-4999-8999-999999999999"
	f.object(t, ctx, qID, "research_question", `{"statement":"Which MOF maximizes CO2 uptake?","question_state":"open"}`)

	snapshot := func() (payload string, integrityHash string, currentVersion int, versionCount int) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT v.payload::text, v.integrity_hash, o.current_version_no
			FROM scientific_object_versions v JOIN scientific_objects o ON o.id = v.object_id
			WHERE o.id = $1 ORDER BY v.version_no DESC LIMIT 1`, qID).Scan(&payload, &integrityHash, &currentVersion); err != nil {
			t.Fatalf("snapshot question: %v", err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM scientific_object_versions WHERE object_id = $1`, qID).Scan(&versionCount); err != nil {
			t.Fatalf("count question versions: %v", err)
		}
		return
	}
	payloadBefore, hashBefore, currentBefore, versionsBefore := snapshot()
	if !strings.Contains(payloadBefore, `"question_state": "open"`) {
		t.Fatalf("fixture payload = %s, want question_state open", payloadBefore)
	}

	// An issue about the question exists and gets closed — the normal
	// project-coordination lifecycle.
	var issueID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO issues (project_id, number, issue_type, title, state, created_by)
		VALUES ($1, 1, 'research', 'reconsider the question', 'open', $2) RETURNING id`, f.project, f.alice).Scan(&issueID); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE issues SET state = 'closed' WHERE id = $1`, issueID); err != nil {
		t.Fatalf("close issue: %v", err)
	}

	payloadAfter, hashAfter, currentAfter, versionsAfter := snapshot()
	if payloadAfter != payloadBefore {
		t.Errorf("issue close rewrote the question payload: %s → %s", payloadBefore, payloadAfter)
	}
	if hashAfter != hashBefore {
		t.Errorf("issue close changed the integrity hash: %s → %s", hashBefore, hashAfter)
	}
	if currentAfter != currentBefore {
		t.Errorf("issue close moved the version head: %d → %d", currentBefore, currentAfter)
	}
	if versionsAfter != versionsBefore {
		t.Errorf("issue close appended/removed version rows: %d → %d", versionsBefore, versionsAfter)
	}
	var qs string
	if err := f.pool.QueryRow(ctx, `SELECT payload->>'question_state' FROM scientific_object_versions v
		JOIN scientific_objects o ON o.id = v.object_id
		WHERE o.id = $1 ORDER BY v.version_no DESC LIMIT 1`, qID).Scan(&qs); err != nil {
		t.Fatalf("read question_state: %v", err)
	}
	if qs != "open" {
		t.Errorf("question_state after issue close = %q, want open (the issue lifecycle must not touch the assessment)", qs)
	}

	// The legitimate path still works: scientific progress is a NEW
	// version with a new assessment — never derived from, and never
	// blocked by, the issue's state.
	f.version(t, ctx, qID, "research_question", `{"statement":"Which MOF maximizes CO2 uptake?","question_state":"partially_answered"}`, 2)
	payloadV2, _, _, versionsV2 := snapshot()
	if !strings.Contains(payloadV2, `"question_state": "partially_answered"`) {
		t.Errorf("version 2 payload = %s, want question_state partially_answered", payloadV2)
	}
	// The log's own truth: exactly two append-only rows, the newest with
	// version_no 2. (current_version_no is the repository-maintained
	// projection, which raw SQL does not touch; the version log is what
	// the guard and the acceptance criterion care about.)
	var maxVersionNo int
	if err := f.pool.QueryRow(ctx, `SELECT max(version_no) FROM scientific_object_versions WHERE object_id = $1`, qID).Scan(&maxVersionNo); err != nil {
		t.Fatalf("read max version_no: %v", err)
	}
	if maxVersionNo != 2 || versionsV2 != 2 {
		t.Errorf("after the assessment update: max version_no = %d, rows = %d, want 2/2", maxVersionNo, versionsV2)
	}
}
