// Task T0013: append-only / version immutability enforced at the storage
// layer by infra/migrations/00014_append_only_enforcement.sql (BEFORE UPDATE
// OR DELETE triggers that RAISE EXCEPTION). These tests prove, against a REAL
// PostgreSQL, that:
//
//   - every covered table has its guard trigger present and ENABLED
//     (pg_trigger.tgenabled = 'O'), and no other table carries a trigger;
//   - an UPDATE of an existing version row is rejected with the exact
//     SQLSTATE (P0001 raise_exception) and an error naming table + operation;
//   - a DELETE of an existing version row is rejected the same way;
//   - a legitimate INSERT of a NEW version row still succeeds;
//   - the same rejections hold on every other covered append-only table;
//   - the upgrade path (previous head 13 → new head 14) introduces exactly
//     these triggers, the upgraded database enforces the guard, and its
//     trigger set matches a fresh install.
//
// Exempt tables (mutable by design; see RESULT.json for the full decision):
// current state and workflow tables (users, projects, branches, issues, pull
// requests, reviews, evidence_assertions, credit_disputes, subscriptions,
// blobs, blob_attachments, asset_dependencies, external_references, …), the
// worker bookkeeping tables (outbox_events, webhook_deliveries) and the
// rebuildable search projection (search_documents).
package integration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// appendOnlyTaskID namespaces this task's test databases
// (test_T0013_<run_id>, docs/66 §3).
const appendOnlyTaskID = "T0013"

// appendOnlyTables is the set migration 00014 guards: every table that is
// append-only BY DESIGN (docs/21 §4, docs/46, ADR-007/008/021, Master
// Acceptance Gate A). Migration 00014/00015 guards the original set; tables
// added later create their own guard pair in their own migration (00038 adds
// project_schema_profiles, T0213; 00056 adds project_template_instantiations,
// T0214). Migration 00053 joins release_creations to the set (the T0606
// Idempotency-Key ledger — a replay is a read, never a rewrite). The same list
// drives the catalog assertion and the per-table rejection loop.
var appendOnlyTables = []string{
	"scientific_object_versions",
	"relation_versions",
	"project_states",
	"state_commits",
	"releases",
	"release_creations",
	"research_asset_versions",
	"asset_lineage",
	"policy_versions",
	"validation_results",
	"contribution_events",
	"audit_log",
	"research_events",
	"external_reference_snapshots",
	"git_push_ingestions",
	"git_push_changes",
	"project_schema_profiles",
	"project_template_instantiations",
}

// targetedGuardTriggers are the NON-append-only row guards added after
// 00014/00015, keyed "table:trigger" → the ":enabled:tgtype" suffix the
// catalog row must end with. Migration 00028 adds the branch lifecycle
// guard (BEFORE UPDATE, FOR EACH ROW → tgtype 19): a merged/aborted
// branch's lifecycle and head pointer are immutable for ANY update path.
// Migration 00031 adds the branch→git-ref guards (T0303): the derived
// git_ref and immutable name guard on branches, the per-row mapping and
// close-direction triggers, and the git_branch_refs sync-state machine.
// Migration 00034 adds the push-ingestion history tables (T0305): the
// ingestion record and the inspected change set join the append-only set
// (both halves of the guard), while the candidate table carries its own
// content-immutable guard (status-only transitions) plus the TRUNCATE half.
// Migration 00040 (T0501) adds the two DEFERRED constraint triggers
// (AFTER INSERT, FOR EACH ROW → tgtype 5: row bit 1 + insert bit 4,
// AFTER being the absence of the BEFORE bit) on the append-only tables
// themselves: knowledge-relation endpoint types and the
// question_id/parent_question_id references. They pin presence and event
// set here even though the tables already carry the append-only pair.
// Migration 00042 adds the T0306 semantic-state guards: the flag row is
// born with the branch (AFTER INSERT map, like 00031's), and the two
// BEFORE-gates refuse a PR from an unstructured branch (INSERT → tgtype 7)
// and the merge transition into merged (UPDATE → tgtype 19).
// Migration 00045 adds the external-reference live-identity guards (T0508):
// the identity validation/normalization on external_references (BEFORE
// INSERT OR UPDATE), the snapshot shape/hash guard (BEFORE INSERT — the
// append-only pair above already pins UPDATE/DELETE), the two deferred
// AFTER INSERT constraint triggers that sync the identity from version
// payloads and police relation endpoints, and the seal on the admitted
// relation-type policy table (BEFORE INSERT OR UPDATE OR DELETE — the
// policy set changes only through a migration that drops the seal first).
// Migration 00047 adds the reconciliation guard (T0309): a finding is
// content-immutable with a forward-only status transition, and both
// reconciliation tables refuse TRUNCATE (runs are bookkeeping, not
// history, but a pass log is still not truncated away).
// Migration 00051 adds the pull request guard (T0402, BEFORE INSERT OR
// UPDATE, FOR EACH ROW → tgtype 23): the docs/43 state machine, the
// fixed base/proposed states (the head moves only through the explicit
// flagged refresh) and merged_at consistency, for ANY write path.
// Migration 00057 (T0503) adds five deferred constraint triggers on the
// findings projection family — refs must pin claim versions of the
// finding's own project (INSERT OR UPDATE → tgtype 21), a findings row
// must have refs (INSERT OR UPDATE → 21), a findings row's object_id must
// be the object its own version belongs to (INSERT OR UPDATE → 21), and a
// finding may not be stripped of its refs by any route: deleting the last
// ref, deleting the findings row while refs remain, or RE-POINTING either
// identity column away from rows that still exist (the last two are
// DELETE OR UPDATE OF <identity column> → 25). Column-scoped events, not
// a bare UPDATE: the invariant depends on the identity column alone, and
// a bare UPDATE would re-run the guard on a legal rewrite that keeps it.
// The findings tables are NOT in
// appendOnlyTables: like the claims projection (00041) they are
// rebuildable (docs/21 §5), so they carry targeted guards instead of the
// append-only pair, and truncate stays open as their rebuild path.
var targetedGuardTriggers = map[string]string{
	"branches:branch_lifecycle_guard_trigger":                                           ":O:19",
	"branches:branch_git_ref_guard_trigger":                                             ":O:23",
	"branches:branch_git_ref_map_trigger":                                               ":O:5",
	"branches:branch_git_ref_close_trigger":                                             ":O:17",
	"branches:branch_semantic_state_map_trigger":                                        ":O:5",
	"branches:branch_merge_semantic_gate_trigger":                                       ":O:19",
	"pull_requests:pull_request_semantic_gate_trigger":                                  ":O:7",
	"git_branch_refs:git_branch_ref_guard_trigger":                                      ":O:23",
	"git_push_semantic_candidates:git_push_semantic_candidate_guard_trigger":            ":O:27",
	"git_push_semantic_candidates:git_push_semantic_candidates_no_truncate":             ":O:34",
	"scientific_object_versions:scientific_object_versions_reference_guard":             ":O:5",
	"relation_versions:relation_versions_knowledge_endpoints":                           ":O:5",
	"external_references:external_references_identity_guard":                            ":O:23",
	"external_reference_snapshots:external_reference_snapshots_guard":                   ":O:7",
	"scientific_object_versions:scientific_object_versions_external_reference_identity": ":O:5",
	"relation_versions:relation_versions_external_reference_endpoints":                  ":O:5",
	"external_reference_relation_types:external_reference_relation_types_seal":          ":O:31",
	"git_reconciliation_findings:git_reconciliation_finding_guard_trigger":              ":O:27",
	"git_reconciliation_findings:git_reconciliation_findings_no_truncate":               ":O:34",
	"git_reconciliation_runs:git_reconciliation_runs_no_truncate":                       ":O:34",
	"pull_requests:pull_request_guard_trigger":                                          ":O:23",
	"findings:findings_refs_present":                                                    ":O:21",
	"findings:findings_version_object_pairing":                                          ":O:21",
	"findings:findings_no_orphan_refs":                                                  ":O:25",
	"finding_claim_versions:finding_claim_versions_ref_guard":                           ":O:21",
	"finding_claim_versions:finding_claim_versions_refs_remain":                         ":O:25",
}

// triggerRows returns every user trigger in the public schema as sorted
// "relname:tgname:tgenabled:tgtype" strings, from pg_trigger itself.
func triggerRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx, `
		SELECT c.relname, t.tgname, t.tgenabled, t.tgtype
		FROM pg_trigger t
		JOIN pg_class c ON c.oid = t.tgrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public' AND NOT t.tgisinternal
		ORDER BY c.relname, t.tgname`)
	if err != nil {
		t.Fatalf("triggerRows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel, name, enabled string
		var tgtype int16
		if err := rows.Scan(&rel, &name, &enabled, &tgtype); err != nil {
			t.Fatalf("triggerRows: scan: %v", err)
		}
		out = append(out, fmt.Sprintf("%s:%s:%s:%d", rel, name, enabled, tgtype))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("triggerRows: %v", err)
	}
	sort.Strings(out)
	return out
}

// assertTriggers verifies that exactly the append-only set carries guard
// triggers, that each is ENABLED ('O'), and that BOTH halves of the guard are
// present on every guarded table:
//
//	<table>_append_only  BEFORE UPDATE OR DELETE, FOR EACH ROW   (tgtype 27)
//	<table>_no_truncate  BEFORE TRUNCATE,         FOR EACH STATEMENT (tgtype 34)
//
// The TRUNCATE half was added by 00015: row triggers do not fire on TRUNCATE,
// so the 00014 guard alone could still be bypassed wholesale. A table carrying
// only one of the two is no longer sufficient, so both are asserted rather
// than just the first trigger found for the table.
//
// The set stays exact: besides the append-only pairs, the targeted guards in
// targetedGuardTriggers are expected — anything else is a surprise.
func assertTriggers(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tables []string) {
	t.Helper()
	// Keyed by "table:trigger" — a table legitimately has more than one now.
	got := map[string]string{}
	for _, tr := range triggerRows(t, ctx, pool) {
		parts := strings.Split(tr, ":")
		got[parts[0]+":"+parts[1]] = tr
	}
	for _, tbl := range tables {
		row, ok := got[tbl+":"+tbl+"_append_only"]
		if !ok {
			t.Errorf("table %s: append-only UPDATE/DELETE trigger missing", tbl)
		} else if !strings.HasSuffix(row, ":O:27") {
			t.Errorf("table %s: trigger not enabled/before-update-or-delete: %s", tbl, row)
		}
		trow, ok := got[tbl+":"+tbl+"_no_truncate"]
		if !ok {
			t.Errorf("table %s: TRUNCATE guard missing (history could be erased wholesale)", tbl)
		} else if !strings.HasSuffix(trow, ":O:34") {
			t.Errorf("table %s: TRUNCATE trigger not enabled/before-truncate: %s", tbl, trow)
		}
	}
	for rel, tr := range got {
		name := strings.SplitN(rel, ":", 2)[0]
		found := false
		for _, tbl := range tables {
			if name == tbl {
				found = true
				break
			}
		}
		if _, ok := targetedGuardTriggers[rel]; ok {
			continue
		}
		if !found {
			t.Errorf("unexpected trigger on table %s (guards must cover exactly the expected set): %s", name, tr)
		}
	}
	for key, want := range targetedGuardTriggers {
		tr, ok := got[key]
		if !ok {
			t.Errorf("targeted guard %s missing", key)
		} else if !strings.HasSuffix(tr, want) {
			t.Errorf("targeted guard %s not enabled/expected event set: %s", key, tr)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'append_only_guard'`).Scan(&n); err != nil {
		t.Fatalf("assertTriggers: guard function lookup: %v", err)
	}
	if n != 1 {
		t.Errorf("append_only_guard function: found %d, want 1", n)
	}
}

// wantAppendOnlyErr asserts the guard rejected the statement with SQLSTATE
// P0001 and an error naming both the table and the forbidden operation.
func wantAppendOnlyErr(t *testing.T, stmt, table, op string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a PostgreSQL error from the append-only guard, got %v", stmt, err)
	}
	if pgErr.Code != "P0001" {
		t.Errorf("%s: SQLSTATE = %s, want P0001 (raise_exception)", stmt, pgErr.Code)
	}
	if !strings.Contains(pgErr.Message, table) {
		t.Errorf("%s: error %q must name the table %s", stmt, pgErr.Message, table)
	}
	if !strings.Contains(pgErr.Message, op) {
		t.Errorf("%s: error %q must name the forbidden operation %s", stmt, pgErr.Message, op)
	}
	if pgErr.Code == "P0001" && strings.Contains(pgErr.Message, table) && strings.Contains(pgErr.Message, op) {
		t.Logf("%s → rejected: SQLSTATE %s: %s", stmt, pgErr.Code, pgErr.Message)
	}
}

// TestAppendOnlyEnforcement proves the guard actually fires on a fresh
// install: UPDATE and DELETE of an existing version row are rejected, a new
// version INSERT still succeeds, and the same holds on every other covered
// append-only table (a legitimate INSERT into each one succeeds first).
func TestAppendOnlyEnforcement(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), appendOnlyTaskID)

	assertTriggers(t, ctx, pool, appendOnlyTables)

	mustQueryUUID := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("setup query failed: %s: %v", sql, err)
		}
		return id
	}

	// Minimal RSG graph: user → project → branch → state → object → version.
	u1 := mustQueryUUID(`INSERT INTO users (handle, display_name) VALUES ('alice', 'Alice') RETURNING id`)
	o1 := mustQueryUUID(`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	p1 := mustQueryUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'p1', 'P1', 'testing append-only', 'private', $2) RETURNING id`, o1, u1)
	b1 := mustQueryUUID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, p1, u1)
	s1 := mustQueryUUID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-1', 'v1') RETURNING id`, p1, b1)
	so1 := mustQueryUUID(`INSERT INTO scientific_objects (project_id, object_type, created_by)
		VALUES ($1, 'dataset', $2) RETURNING id`, p1, u1)
	sov := func(versionNo int) string {
		t.Helper()
		return mustQueryUUID(`INSERT INTO scientific_object_versions
			(object_id, version_no, state_id, schema_id, schema_version, title,
			 lifecycle_state, payload, integrity_hash, created_by)
			VALUES ($1, $2, $3, 'core/dataset', '1.0', 'Dataset A', 'active',
			        '{}'::jsonb, 'ih-1', $4) RETURNING id`, so1, versionNo, s1, u1)
	}
	sov1 := sov(1)
	r1 := mustQueryUUID(`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, p1)

	// --- The explicit acceptance cases on scientific_object_versions. ---

	// 1. UPDATE of an existing version row is rejected (the exact tamper the
	//    Supervisor demonstrated pre-T0013).
	_, err := pool.Exec(ctx, `UPDATE scientific_object_versions
		SET title = 'REWRITTEN HISTORY', integrity_hash = 'tampered' WHERE id = $1`, sov1)
	wantAppendOnlyErr(t, "UPDATE scientific_object_versions", "scientific_object_versions", "UPDATE", err)

	// 2. DELETE of an existing version row is rejected.
	_, err = pool.Exec(ctx, `DELETE FROM scientific_object_versions WHERE id = $1`, sov1)
	wantAppendOnlyErr(t, "DELETE FROM scientific_object_versions", "scientific_object_versions", "DELETE", err)

	// 3. A legitimate INSERT of a NEW version row still succeeds.
	sov2 := sov(2)
	var versionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scientific_object_versions WHERE object_id = $1`, so1).Scan(&versionCount); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versionCount != 2 {
		t.Errorf("append-only flow: %d versions after appending v2, want 2", versionCount)
	}
	if sov1 == sov2 {
		t.Error("append-only flow: v1 and v2 share an id; the INSERT must create a new row")
	}

	// --- Every other covered table: legitimate INSERT succeeds, then UPDATE
	//     and DELETE are rejected. ---

	// Shared dependencies for the later cases.
	s2 := mustQueryUUID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-2', 'v1') RETURNING id`, p1, b1)
	ra1 := mustQueryUUID(`INSERT INTO research_assets (asset_type, slug, title, origin_project_id)
		VALUES ('dataset', 'ds-1', 'DS1', $1) RETURNING id`, p1)
	av1 := mustQueryUUID(`INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
		VALUES ($1, '1.0', '{}'::jsonb, '{}'::jsonb, 'private', 'h', $2) RETURNING id`, ra1, u1)
	av2 := mustQueryUUID(`INSERT INTO research_asset_versions
		(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
		VALUES ($1, '2.0', '{}'::jsonb, '{}'::jsonb, 'private', 'h', $2) RETURNING id`, ra1, u1)
	er1 := mustQueryUUID(`INSERT INTO external_references (source_type, external_identifier)
		VALUES ('Publication/DOI', '10.1000/1') RETURNING id`)

	type rowCase struct {
		table  string
		insert func() string // returns a stable row identifier for the case
		update func(id string) error
		del    func(id string) error
	}
	// ingID and relID are captured by the git_push_ingestions / releases
	// cases (the loop runs the cases in order) so the git_push_changes /
	// release_creations cases can reference their FK rows.
	var ingID, relID string
	cases := []rowCase{
		{
			table: "relation_versions",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO relation_versions
					(relation_id, version_no, state_id, relation_type,
					 source_object_version_id, target_object_version_id,
					 integrity_hash, created_by)
					VALUES ($1, 1, $2, 'uses', $3, $4, 'h', $5) RETURNING id`,
					r1, s1, sov1, sov2, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE relation_versions SET payload = '{"x":1}'::jsonb WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM relation_versions WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "project_states",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
					VALUES ($1, $2, 'hash-3', 'v1') RETURNING id`, p1, b1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE project_states SET state_hash = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM project_states WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "state_commits",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO state_commits
					(project_id, branch_id, base_state_id, result_state_id, actor_id, via, message, operation_summary)
					VALUES ($1, $2, $3, $4, $5, 'web', 'create dataset', '{}'::jsonb) RETURNING id`,
					p1, b1, s1, s2, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE state_commits SET message = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM state_commits WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "releases",
			insert: func() string {
				relID = mustQueryUUID(`INSERT INTO releases
					(project_id, version, title, state_id, manifest, manifest_hash, created_by)
					VALUES ($1, '0.1.0', 'R1', $2, '{}', 'mh', $3) RETURNING id`, p1, s1, u1)
				return relID
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE releases SET title = 'REWRITTEN' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM releases WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "release_creations",
			insert: func() string {
				// The releases case ran first: relID carries its row.
				return mustQueryUUID(`INSERT INTO release_creations
					(project_id, idempotency_key, release_id)
					VALUES ($1, 'key-1', $2) RETURNING id`, p1, relID)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE release_creations SET idempotency_key = 'REWRITTEN' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM release_creations WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "research_asset_versions",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO research_asset_versions
					(asset_id, version, manifest, rights_json, visibility, integrity_hash, published_by)
					VALUES ($1, '3.0', '{}'::jsonb, '{}'::jsonb, 'private', 'h', $2) RETURNING id`, ra1, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE research_asset_versions SET version = 'REWRITTEN' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM research_asset_versions WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "asset_lineage",
			insert: func() string {
				if _, err := pool.Exec(ctx, `INSERT INTO asset_lineage
					(parent_asset_version_id, child_asset_version_id, relation_type)
					VALUES ($1, $2, 'forked_from')`, av1, av2); err != nil {
					t.Fatalf("setup: INSERT asset_lineage: %v", err)
				}
				return "forked_from"
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE asset_lineage SET relation_type = 'supersedes'
					WHERE parent_asset_version_id = $1 AND child_asset_version_id = $2 AND relation_type = $3`,
					av1, av2, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM asset_lineage
					WHERE parent_asset_version_id = $1 AND child_asset_version_id = $2 AND relation_type = $3`,
					av1, av2, id)
				return err
			},
		},
		{
			table: "policy_versions",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO policy_versions
					(organization_id, version, policy_json, created_by)
					VALUES ($1, 'v1', '{}'::jsonb, $2) RETURNING id`, o1, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE policy_versions SET policy_json = '{"tampered":true}'::jsonb WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM policy_versions WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "validation_results",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO validation_results
					(project_id, state_id, gate, status, result_json)
					VALUES ($1, $2, 'pr', 'passed', '{}'::jsonb) RETURNING id`, p1, s1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE validation_results SET status = 'warning' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM validation_results WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "contribution_events",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO contribution_events (actor_id, event_type)
					VALUES ($1, 'object.created') RETURNING id`, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE contribution_events SET event_type = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM contribution_events WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "audit_log",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO audit_log (via, action, correlation_id)
					VALUES ('web', 'object.create', 'corr-1') RETURNING id`)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE audit_log SET action = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM audit_log WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "research_events",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO research_events
					(event_type, visibility, payload, correlation_id)
					VALUES ('project.created', 'private', '{}'::jsonb, 'corr-2') RETURNING id`)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE research_events SET payload = '{"tampered":true}'::jsonb WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM research_events WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "external_reference_snapshots",
			insert: func() string {
				// snapshot_hash follows the 00045 contract: NULL derives
				// server-side from the stored metadata bytes (a made-up
				// hash would be refused by the snapshot guard).
				return mustQueryUUID(`INSERT INTO external_reference_snapshots
					(external_reference_id, accessed_at, metadata, snapshot_hash)
					VALUES ($1, now(), '{}'::jsonb, NULL) RETURNING id`, er1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE external_reference_snapshots SET metadata = '{"tampered":true}'::jsonb WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM external_reference_snapshots WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "git_push_ingestions",
			insert: func() string {
				id := mustQueryUUID(`INSERT INTO git_push_ingestions
					(delivery_id, gitea_repo_id, project_id, branch_name, git_ref,
					 before_sha, after_sha, commit_count, pusher)
					VALUES ('d-1', 42, $1, 'main', 'refs/heads/main', 'aaa', 'bbb', 1, 'svc')
					RETURNING id`, p1)
				ingID = id
				return id
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE git_push_ingestions SET after_sha = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM git_push_ingestions WHERE id = $1`, id)
				return err
			},
		},
		{
			table: "git_push_changes",
			insert: func() string {
				// The FK row exists: the loop ran the ingestions case first.
				return mustQueryUUID(`INSERT INTO git_push_changes
					(ingestion_id, path, change_kind, file_kind)
					VALUES ($1, 'x.json', 'added', 'unstructured') RETURNING ingestion_id`, ingID)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE git_push_changes SET change_kind = 'rewritten'
					WHERE ingestion_id = $1 AND path = 'x.json'`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM git_push_changes
					WHERE ingestion_id = $1 AND path = 'x.json'`, id)
				return err
			},
		},
		{
			// T0213 (00038): a registered schema profile version is
			// immutable by design — schema versions are never overwritten,
			// new content takes a new version (docs/21 §8).
			table: "project_schema_profiles",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO project_schema_profiles
					(project_id, schema_id, version, base_schema_id, base_schema_version,
					 content, content_hash, created_by)
					VALUES ($1, 'project:x:experiment_ext', '1',
					        'https://open-rd.example/schemas/experiment.schema.json', '1',
					        '{}', repeat('a', 64), $2) RETURNING id`, p1, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE project_schema_profiles SET version = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM project_schema_profiles WHERE id = $1`, id)
				return err
			},
		},
		{
			// T0214 (00056): the template provenance fact is immutable by
			// design — "project X was created from template Y vZ" never
			// changes and never disappears, even if the catalog retires
			// that version.
			table: "project_template_instantiations",
			insert: func() string {
				return mustQueryUUID(`INSERT INTO project_template_instantiations
					(project_id, template_id, template_version, template_name, created_by)
					VALUES ($1, 'materials-discovery', 'v1', 'Materials Discovery', $2)
					RETURNING id`, p1, u1)
			},
			update: func(id string) error {
				_, err := pool.Exec(ctx, `UPDATE project_template_instantiations SET template_version = 'rewritten' WHERE id = $1`, id)
				return err
			},
			del: func(id string) error {
				_, err := pool.Exec(ctx, `DELETE FROM project_template_instantiations WHERE id = $1`, id)
				return err
			},
		},
	}

	for _, c := range cases {
		id := c.insert() // legitimate INSERT into the covered table succeeds
		wantAppendOnlyErr(t, fmt.Sprintf("UPDATE %s", c.table), c.table, "UPDATE", c.update(id))
		wantAppendOnlyErr(t, fmt.Sprintf("DELETE FROM %s", c.table), c.table, "DELETE", c.del(id))
	}

	// Sanity: current-state tables stay mutable (the guard must not leak onto
	// them) — the earlier version INSERTs would already have failed otherwise,
	// but an in-place pointer move is the documented mechanism (docs/21 §5).
	if _, err := pool.Exec(ctx, `UPDATE branches SET base_state_id = $1 WHERE id = $2`, s2, b1); err != nil {
		t.Errorf("current-state UPDATE (branches.base_state_id pointer) must remain allowed: %v", err)
	}
}

// TestGitPushSemanticCandidateGuard proves the candidate guard's three
// semantics as BEHAVIOR (the trigger-set assertions in assertTriggers only
// prove it exists): the status may transition (candidate → validated |
// applied | rejected — the validation pipeline's later tasks), everything
// else is immutable, and a candidate row is never deleted.
func TestGitPushSemanticCandidateGuard(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), appendOnlyTaskID)

	// An ingestion row to hang the candidate on (project_id is nullable —
	// the guard is about the candidate row itself, not the project graph).
	var ingID string
	if err := pool.QueryRow(ctx, `INSERT INTO git_push_ingestions
		(delivery_id, gitea_repo_id, project_id, branch_name, git_ref, before_sha, after_sha, commit_count, pusher)
		VALUES ('d-cand', 7, NULL, 'main', 'refs/heads/main', 'aaa', 'bbb', 1, 'svc')
		RETURNING id`).Scan(&ingID); err != nil {
		t.Fatalf("candidate guard: insert ingestion: %v", err)
	}
	var candID string
	if err := pool.QueryRow(ctx, `INSERT INTO git_push_semantic_candidates
		(ingestion_id, path, change_kind, schema_id, candidate)
		VALUES ($1, 'mat.json', 'added', 'https://open-rd.example/schemas/material.schema.json', '{"id":"mat-0001"}')
		RETURNING id`, ingID).Scan(&candID); err != nil {
		t.Fatalf("candidate guard: insert candidate: %v", err)
	}

	wantGuardErr := func(stmt, wantMsg string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: expected a PostgreSQL error from the candidate guard, got %v", stmt, err)
		}
		if pgErr.Code != "P0001" {
			t.Errorf("%s: SQLSTATE = %s, want P0001 (raise_exception)", stmt, pgErr.Code)
		}
		if !strings.Contains(pgErr.Message, wantMsg) {
			t.Errorf("%s: message %q must contain %q", stmt, pgErr.Message, wantMsg)
		}
	}

	// 1. The status is the ONE legal mutation: every transition the
	// validation pipeline's later tasks perform lands, and none of them
	// disturb the content.
	for _, st := range []string{"validated", "applied", "rejected", "candidate"} {
		if _, err := pool.Exec(ctx,
			`UPDATE git_push_semantic_candidates SET status = $1 WHERE id = $2`, st, candID); err != nil {
			t.Fatalf("candidate guard: status → %s: %v", st, err)
		}
	}
	var gotStatus, gotKind, gotPath string
	var gotCandidate []byte
	if err := pool.QueryRow(ctx, `SELECT status, change_kind, path, candidate
		FROM git_push_semantic_candidates WHERE id = $1`, candID).
		Scan(&gotStatus, &gotKind, &gotPath, &gotCandidate); err != nil {
		t.Fatalf("candidate guard: probe row: %v", err)
	}
	if gotStatus != "candidate" || gotKind != "added" || gotPath != "mat.json" ||
		string(gotCandidate) != `{"id": "mat-0001"}` {
		t.Errorf("candidate guard: row after status transitions = %s/%s/%s/%s, want candidate/added/mat.json with the content intact",
			gotStatus, gotKind, gotPath, gotCandidate)
	}

	// 2. Content is immutable: every non-status column is pinned by the
	// guard — the candidate itself and each identity/classification cell.
	_, err := pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET candidate = '{"tampered":true}' WHERE id = $1`, candID)
	wantGuardErr("UPDATE candidate (candidate)", "immutable", err)
	_, err = pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET path = 'other.json' WHERE id = $1`, candID)
	wantGuardErr("UPDATE candidate (path)", "immutable", err)
	_, err = pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET change_kind = 'removed' WHERE id = $1`, candID)
	wantGuardErr("UPDATE candidate (change_kind)", "immutable", err)
	_, err = pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET schema_id = 'other' WHERE id = $1`, candID)
	wantGuardErr("UPDATE candidate (schema_id)", "immutable", err)
	// ingestion_id to ANOTHER existing ingestion (the FK is satisfied, so
	// only the guard can reject the move).
	var ing2ID string
	if err := pool.QueryRow(ctx, `INSERT INTO git_push_ingestions
		(delivery_id, gitea_repo_id, project_id, branch_name, git_ref, before_sha, after_sha, commit_count, pusher)
		VALUES ('d-cand2', 8, NULL, 'main', 'refs/heads/main', 'ccc', 'ddd', 1, 'svc')
		RETURNING id`).Scan(&ing2ID); err != nil {
		t.Fatalf("candidate guard: insert second ingestion: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET ingestion_id = $1 WHERE id = $2`, ing2ID, candID)
	wantGuardErr("UPDATE candidate (ingestion_id)", "immutable", err)

	// 3. A candidate is never deleted.
	_, err = pool.Exec(ctx, `DELETE FROM git_push_semantic_candidates WHERE id = $1`, candID)
	wantGuardErr("DELETE candidate", "never deleted", err)

	// The status domain itself is a CHECK: an unknown status is rejected
	// before any transition could land (23514, the constraint — not the
	// guard, which only admits the four legal values).
	_, err = pool.Exec(ctx, `UPDATE git_push_semantic_candidates SET status = 'bogus' WHERE id = $1`, candID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Errorf("candidate guard: status = 'bogus': err = %v, want SQLSTATE 23514", err)
	}

	// Nothing above may have landed: the row is exactly as inserted, with
	// its final status transition.
	var finalStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM git_push_semantic_candidates WHERE id = $1`, candID).
		Scan(&finalStatus); err != nil {
		t.Fatalf("candidate guard: final probe: %v", err)
	}
	if finalStatus != "candidate" {
		t.Errorf("candidate guard: final status = %q, want candidate", finalStatus)
	}
}

// TestAppendOnlyUpgradePath proves the migration upgrade path for 00014: a
// database at the previous head (13) has no guard, migrating to the new head
// (14) introduces exactly the guard triggers (present and ENABLED per
// pg_trigger), the upgraded database actually enforces them, and its trigger
// set matches a fresh install.
func TestAppendOnlyUpgradePath(t *testing.T) {
	ctx := testCtx(t)

	// Intermediate database at the previous head, version 13.
	pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), appendOnlyTaskID)
	applied, err := persistence.MigrateTo(ctx, url, 13)
	if err != nil {
		t.Fatalf("upgrade path: migrate to 13: %v", err)
	}
	if applied != 13 {
		t.Errorf("upgrade path: applied %d to reach version 13, want 13", applied)
	}
	if v := appliedVersion(t, ctx, pool); v != 13 {
		t.Fatalf("upgrade path: version after MigrateTo(13) = %d, want 13", v)
	}
	if tr := triggerRows(t, ctx, pool); len(tr) != 0 {
		t.Errorf("upgrade path: %d triggers at version 13, want 0 (00014 must be the sole source): %v", len(tr), tr)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'append_only_guard'`).Scan(&n); err != nil {
		t.Fatalf("upgrade path: guard function lookup at v13: %v", err)
	}
	if n != 0 {
		t.Errorf("upgrade path: append_only_guard exists at version 13, want none")
	}

	// Continue to the new head.
	applied, err = persistence.Migrate(ctx, url)
	if err != nil {
		t.Fatalf("upgrade path: migrate to head: %v", err)
	}
	// Derived, not hardcoded: this used to say "want 1" and went stale the
	// moment a second migration was added above 00013. The count comes from
	// the embedded set (appliedAbove) rather than "head - 13": numbering is
	// sparse while parallel tasks hold reserved numbers, and the runner
	// applies exactly the files that exist.
	if want := appliedAbove(13); applied != want {
		t.Errorf("upgrade path: applied %d on the way from 13 to head, want %d", applied, want)
	}
	if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
		t.Fatalf("upgrade path: version after head = %d, want %d", v, maxVersionNo)
	}

	assertTriggers(t, ctx, pool, appendOnlyTables)

	// The upgraded database must actually ENFORCE the guard, not merely show
	// it in pg_catalog: an in-place UPDATE of a ledger row is rejected.
	var auditID string
	if err := pool.QueryRow(ctx, `INSERT INTO audit_log (via, action, correlation_id)
		VALUES ('web', 'object.create', 'corr-upgrade') RETURNING id`).Scan(&auditID); err != nil {
		t.Fatalf("upgrade path: insert audit_log row: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE audit_log SET action = 'rewritten' WHERE id = $1`, auditID)
	wantAppendOnlyErr(t, "UPDATE audit_log (upgraded database)", "audit_log", "UPDATE", err)

	// Fresh reference install: identical trigger set.
	freshPool, _ := testdb.Setup(t, ctx, adminURL(t), appendOnlyTaskID)
	upgradedTr := triggerRows(t, ctx, pool)
	freshTr := triggerRows(t, ctx, freshPool)
	if len(upgradedTr) != len(freshTr) {
		t.Fatalf("upgrade path: trigger count upgraded=%d fresh=%d", len(upgradedTr), len(freshTr))
	}
	for i := range upgradedTr {
		if upgradedTr[i] != freshTr[i] {
			t.Errorf("upgrade path: trigger %d differs from fresh install: %q vs %q", i, upgradedTr[i], freshTr[i])
		}
	}

	// And the full catalog matches a fresh install, exactly as TestUpgradePath
	// requires for any upgrade (takeSnapshot reads pg_catalog, not exit codes).
	upgradedSnap := takeSnapshot(t, ctx, pool)
	freshSnap := takeSnapshot(t, ctx, freshPool)
	if snapshotJSON(t, upgradedSnap) != snapshotJSON(t, freshSnap) {
		t.Error("upgrade path: upgraded catalog differs from fresh install")
	}
}
