// Package integration holds the G1/G3 database gates for the migration set
// and the pgx/sqlc data-access layer (task T0005).
//
// Every test runs against REAL PostgreSQL in a task/run-scoped namespace
// database (test_T0005_<run_id>, docs/66 §3), created and dropped per test by
// internal/persistence/testdb — mocks are not used anywhere in this package.
//
// Required tests (task T0005):
//
//   - fresh install: TestFreshInstallCatalog — migrate an empty database to
//     head and verify the expected tables, columns, constraints, FKs, checks
//     and unique indexes exist by querying pg_catalog (not "exit code 0").
//   - repeat migrate: TestRepeatMigrateIsNoop — migrating an already-migrated
//     database is safe and applies nothing.
//   - upgrade path: TestUpgradePath — migrate to an intermediate version,
//     then to head; the result must be indistinguishable from a fresh install.
//   - append-only/constraint enforcement: TestConstraintEnforcement — a
//     constraint actually rejects a forbidden UPDATE/DELETE (SQLSTATE
//     asserted), not merely existing.
package integration

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/lichman0405/post/infra/migrations"
	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

const taskID = "T0005"

// adminURL returns the PostgreSQL admin connection URL for tests. Default is
// the local dev stack (infra/docker); override with POSTGRES_TEST_ADMIN_URL.
func adminURL(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("POSTGRES_TEST_ADMIN_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres_dev_pw@127.0.0.1:15432/post"
}

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// ---------------------------------------------------------------------------
// Expected catalog, derived 1:1 from specs/database/postgres.sql (the
// canonical source of truth). If the canonical schema changes without a
// migration, this fixture makes the drift fail loudly here.

type colExp struct {
	name, dataType, udtName string
	nullable, hasDefault    bool
}

type fkExp struct {
	col, refTable, refCol, onDelete string
}

type tableExp struct {
	cols    []colExp
	pk      []string
	uniques [][]string
	checks  []string // distinguishing substrings of pg_get_constraintdef
	fks     []fkExp
}

func c(name, dataType string, nullable, hasDefault bool) colExp {
	return colExp{name: name, dataType: dataType, nullable: nullable, hasDefault: hasDefault}
}

func arr(name string, nullable, hasDefault bool) colExp {
	return colExp{name: name, dataType: "ARRAY", udtName: "_text", nullable: nullable, hasDefault: hasDefault}
}

func fk(col, refTable, onDelete string) fkExp {
	return fkExp{col: col, refTable: refTable, refCol: "id", onDelete: onDelete}
}

const (
	ts  = "timestamp with time zone"
	txt = "text"
	u   = "uuid"
	bl  = "boolean"
	dt  = "date"
	i8  = "bigint"
	i4  = "integer"
	jb  = "jsonb"
	vec = "USER-DEFINED"
)

// canonicalTables is the full expected shape of the migrated database. The
// goose bookkeeping table is separate (gooseTable) and asserted as the only
// allowed addition.
var canonicalTables = map[string]tableExp{
	"users": {
		cols:    []colExp{c("id", u, false, true), c("handle", txt, false, false), c("email", txt, true, false), c("display_name", txt, false, false), c("created_at", ts, false, true), c("disabled_at", ts, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"handle"}, {"email"}},
	},
	"organizations": {
		cols:    []colExp{c("id", u, false, true), c("slug", txt, false, false), c("name", txt, false, false), c("description", txt, true, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"slug"}},
	},
	"organization_memberships": {
		cols:   []colExp{c("organization_id", u, false, false), c("user_id", u, false, false), c("role", txt, false, false), c("affiliation_start", dt, true, false), c("affiliation_end", dt, true, false), c("verified", bl, false, true)},
		pk:     []string{"organization_id", "user_id"},
		checks: []string{"role = ANY"},
		fks:    []fkExp{fk("organization_id", "organizations", "RESTRICT"), fk("user_id", "users", "RESTRICT")},
	},
	"programs": {
		cols:    []colExp{c("id", u, false, true), c("organization_id", u, true, false), c("slug", txt, false, false), c("name", txt, false, false), c("description", txt, true, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"organization_id", "slug"}},
		fks:     []fkExp{fk("organization_id", "organizations", "RESTRICT")},
	},
	"projects": {
		cols:    []colExp{c("id", u, false, true), c("organization_id", u, true, false), c("program_id", u, true, false), c("slug", txt, false, false), c("name", txt, false, false), c("purpose", txt, false, false), c("activity_status", txt, false, true), c("visibility", txt, false, false), c("main_frozen", bl, false, true), c("git_repository_external_id", txt, true, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"organization_id", "slug"}},
		checks:  []string{"activity_status = ANY", "visibility = ANY"},
		fks:     []fkExp{fk("organization_id", "organizations", "RESTRICT"), fk("program_id", "programs", "SET NULL"), fk("created_by", "users", "RESTRICT")},
	},
	"project_memberships": {
		cols:   []colExp{c("project_id", u, false, false), c("user_id", u, false, false), c("role", txt, false, false), c("created_at", ts, false, true)},
		pk:     []string{"project_id", "user_id"},
		checks: []string{"role = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("user_id", "users", "RESTRICT")},
	},
	"policy_versions": {
		cols:   []colExp{c("id", u, false, true), c("organization_id", u, true, false), c("project_id", u, true, false), c("version", txt, false, false), c("policy_json", jb, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"(organization_id IS NOT NULL) <>"}, // XOR: exactly one scope set
		fks:    []fkExp{fk("organization_id", "organizations", "RESTRICT"), fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"branches": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("name", txt, false, false), c("visibility", txt, false, false), c("purpose", txt, true, false), c("git_ref", txt, false, false), c("base_state_id", u, true, false), c("lifecycle_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "name"}},
		checks:  []string{"visibility = ANY", "lifecycle_state = ANY"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"project_states": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("branch_id", u, true, false), c("parent_state_id", u, true, false), c("state_hash", txt, false, false), c("git_commit_sha", txt, true, false), c("manifest_version", txt, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "state_hash"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("parent_state_id", "project_states", "RESTRICT")},
	},
	"state_commits": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("branch_id", u, false, false), c("base_state_id", u, true, false), c("result_state_id", u, false, false), c("actor_id", u, false, false), c("via", txt, false, false), c("message", txt, false, false), c("operation_summary", jb, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"via = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("result_state_id", "project_states", "RESTRICT"), fk("actor_id", "users", "RESTRICT")},
	},
	"scientific_objects": {
		cols: []colExp{c("id", u, false, true), c("project_id", u, false, false), c("object_type", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"scientific_object_versions": {
		cols:    []colExp{c("id", u, false, true), c("object_id", u, false, false), c("version_no", i4, false, false), c("state_id", u, false, false), c("branch_id", u, true, false), c("schema_id", txt, false, false), c("schema_version", txt, false, false), c("title", txt, false, false), c("lifecycle_state", txt, false, false), c("payload", jb, false, false), c("visibility_policy_id", u, true, false), c("integrity_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"object_id", "version_no"}},
		checks:  []string{"version_no > 0", "lifecycle_state = ANY"},
		fks:     []fkExp{fk("object_id", "scientific_objects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"relations": {
		cols: []colExp{c("id", u, false, true), c("project_id", u, false, false), c("created_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
	"relation_versions": {
		cols:    []colExp{c("id", u, false, true), c("relation_id", u, false, false), c("version_no", i4, false, false), c("state_id", u, false, false), c("relation_type", txt, false, false), c("source_object_version_id", u, false, false), c("target_object_version_id", u, false, false), c("payload", jb, false, true), c("integrity_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"relation_id", "version_no"}},
		checks:  []string{"version_no > 0"},
		fks:     []fkExp{fk("relation_id", "relations", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("source_object_version_id", "scientific_object_versions", "RESTRICT"), fk("target_object_version_id", "scientific_object_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"evidence_assertions": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("state_id", u, false, false), c("target_object_version_id", u, false, false), c("evidence_object_version_id", u, false, false), c("relation_type", txt, false, false), c("evidence_type", txt, false, false), c("scope", jb, false, true), c("directness", txt, false, true), c("inference_nature", txt, false, true), c("reasoning_note", txt, true, false), c("review_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"relation_type = ANY", "review_state = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("target_object_version_id", "scientific_object_versions", "RESTRICT"), fk("evidence_object_version_id", "scientific_object_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"blobs": {
		cols:    []colExp{c("id", u, false, true), c("content_hash", txt, false, false), c("size_bytes", i8, false, false), c("media_type", txt, true, false), c("storage_key", txt, false, false), c("integrity_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"content_hash", "size_bytes"}},
		checks:  []string{"size_bytes >= 0", "integrity_state = ANY"},
		fks:     []fkExp{fk("created_by", "users", "RESTRICT")},
	},
	"blob_attachments": {
		cols:   []colExp{c("blob_id", u, false, false), c("scientific_object_version_id", u, false, false), c("attachment_role", txt, false, false), c("access_level", txt, false, false)},
		pk:     []string{"blob_id", "scientific_object_version_id", "attachment_role"},
		checks: []string{"access_level = ANY"},
		fks:    []fkExp{fk("blob_id", "blobs", "RESTRICT"), fk("scientific_object_version_id", "scientific_object_versions", "RESTRICT")},
	},
	"issues": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("number", i8, false, false), c("issue_type", txt, false, false), c("title", txt, false, false), c("body", txt, false, true), c("state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "number"}},
		checks:  []string{"state = ANY"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"pull_requests": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("number", i8, false, false), c("source_branch_id", u, false, false), c("target_branch_id", u, false, false), c("base_state_id", u, false, false), c("proposed_state_id", u, false, false), c("title", txt, false, false), c("body", txt, false, true), c("state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true), c("merged_at", ts, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "number"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("source_branch_id", "branches", "RESTRICT"), fk("target_branch_id", "branches", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("proposed_state_id", "project_states", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"reviews": {
		cols:   []colExp{c("id", u, false, true), c("pull_request_id", u, false, false), c("reviewer_id", u, false, false), c("review_kind", txt, false, false), c("decision", txt, false, false), c("body", txt, false, true), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"review_kind = ANY", "decision = ANY"},
		fks:    []fkExp{fk("pull_request_id", "pull_requests", "RESTRICT"), fk("reviewer_id", "users", "RESTRICT")},
	},
	"validation_results": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("state_id", u, false, false), c("gate", txt, false, false), c("status", txt, false, false), c("result_json", jb, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"gate = ANY", "status = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT")},
	},
	"releases": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("version", txt, false, false), c("title", txt, false, false), c("state_id", u, false, false), c("policy_version_id", u, true, false), c("manifest", jb, false, false), c("manifest_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "version"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("policy_version_id", "policy_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"research_assets": {
		cols:   []colExp{c("id", u, false, true), c("asset_type", txt, false, false), c("slug", txt, false, false), c("title", txt, false, false), c("origin_project_id", u, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"asset_type = ANY"},
		fks:    []fkExp{fk("origin_project_id", "projects", "RESTRICT")},
	},
	"research_asset_versions": {
		cols:    []colExp{c("id", u, false, true), c("asset_id", u, false, false), c("version", txt, false, false), c("source_release_id", u, true, false), c("manifest", jb, false, false), c("rights_json", jb, false, false), c("visibility", txt, false, false), c("integrity_hash", txt, false, false), c("published_by", u, false, false), c("published_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"asset_id", "version"}},
		checks:  []string{"visibility = ANY"},
		fks:     []fkExp{fk("asset_id", "research_assets", "RESTRICT"), fk("source_release_id", "releases", "RESTRICT"), fk("published_by", "users", "RESTRICT")},
	},
	"asset_lineage": {
		cols:   []colExp{c("parent_asset_version_id", u, false, false), c("child_asset_version_id", u, false, false), c("relation_type", txt, false, false)},
		pk:     []string{"parent_asset_version_id", "child_asset_version_id", "relation_type"},
		checks: []string{"relation_type = ANY"},
		fks:    []fkExp{fk("parent_asset_version_id", "research_asset_versions", "RESTRICT"), fk("child_asset_version_id", "research_asset_versions", "RESTRICT")},
	},
	"asset_dependencies": {
		cols:   []colExp{c("project_id", u, false, false), c("asset_version_id", u, false, false), c("dependency_type", txt, false, false), c("visibility_of_usage", txt, false, true), c("created_at", ts, false, true)},
		pk:     []string{"project_id", "asset_version_id", "dependency_type"},
		checks: []string{"visibility_of_usage = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("asset_version_id", "research_asset_versions", "RESTRICT")},
	},
	"knowledge_publications": {
		cols:    []colExp{c("id", u, false, true), c("object_version_id", u, false, false), c("public_version", txt, false, false), c("rights_json", jb, false, false), c("published_by", u, false, false), c("published_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"object_version_id", "public_version"}},
		fks:     []fkExp{fk("object_version_id", "scientific_object_versions", "RESTRICT"), fk("published_by", "users", "RESTRICT")},
	},
	"external_references": {
		cols:    []colExp{c("id", u, false, true), c("source_type", txt, false, false), c("external_identifier", txt, false, false), c("canonical_url", txt, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"source_type", "external_identifier"}},
	},
	"external_reference_snapshots": {
		cols: []colExp{c("id", u, false, true), c("external_reference_id", u, false, false), c("accessed_at", ts, false, false), c("upstream_version", txt, true, false), c("metadata", jb, false, false), c("snapshot_hash", txt, false, false), c("blob_id", u, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("external_reference_id", "external_references", "RESTRICT"), fk("blob_id", "blobs", "RESTRICT")},
	},
	"contribution_events": {
		cols: []colExp{c("id", u, false, true), c("actor_id", u, false, false), c("organization_id_at_time", u, true, false), c("project_id", u, true, false), c("event_type", txt, false, false), arr("role_codes", false, true), c("object_refs", jb, false, true), c("accepted_context", bl, false, true), c("released_context", bl, false, true), c("occurred_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("actor_id", "users", "RESTRICT"), fk("organization_id_at_time", "organizations", "RESTRICT"), fk("project_id", "projects", "RESTRICT")},
	},
	"credit_disputes": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, true, false), c("opened_by", u, false, false), c("target_ref", txt, false, false), c("claim", txt, false, false), c("state", txt, false, true), c("resolution", txt, true, false), c("opened_at", ts, false, true), c("resolved_at", ts, true, false)},
		pk:     []string{"id"},
		checks: []string{"state = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("opened_by", "users", "RESTRICT")},
	},
	"research_events": {
		cols: []colExp{c("id", u, false, true), c("event_type", txt, false, false), c("actor_id", u, true, false), c("project_id", u, true, false), c("visibility", txt, false, false), c("payload", jb, false, false), c("correlation_id", txt, false, false), c("occurred_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("actor_id", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT")},
	},
	"outbox_events": {
		cols: []colExp{c("id", u, false, true), c("event_type", txt, false, false), c("payload", jb, false, false), c("correlation_id", txt, false, false), c("created_at", ts, false, true), c("published_at", ts, true, false), c("attempts", i4, false, true)},
		pk:   []string{"id"},
	},
	"subscriptions": {
		cols: []colExp{c("id", u, false, true), c("user_id", u, false, false), c("target_type", txt, false, false), c("target_id", txt, false, false), arr("event_filters", false, true), arr("channels", false, true), c("created_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	"webhook_deliveries": {
		cols: []colExp{c("id", u, false, true), c("event_id", u, false, false), c("endpoint", txt, false, false), c("status", txt, false, false), c("response_code", i4, true, false), c("attempts", i4, false, true), c("last_attempt_at", ts, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("event_id", "research_events", "RESTRICT")},
	},
	"audit_log": {
		cols: []colExp{c("id", u, false, true), c("actor_id", u, true, false), c("via", txt, false, false), c("action", txt, false, false), c("target_ref", txt, true, false), c("project_id", u, true, false), c("correlation_id", txt, false, false), c("before_summary", jb, true, false), c("after_summary", jb, true, false), c("metadata", jb, false, true), c("occurred_at", ts, false, true)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("actor_id", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT")},
	},
	"search_documents": {
		cols: []colExp{c("entity_ref", txt, false, false), c("entity_type", txt, false, false), c("visibility", txt, false, false), c("project_id", u, true, false), c("title", txt, false, false), c("content", txt, false, false), c("structured", jb, false, true), colExp{name: "embedding", dataType: vec, udtName: "vector", nullable: true}, c("updated_at", ts, false, true)},
		pk:   []string{"entity_ref"},
		fks:  []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
}

// gooseTable is the only non-canonical table the runner may create.
var gooseTable = tableExp{
	cols: []colExp{c("id", i4, false, false), c("version_id", i8, false, false), c("is_applied", bl, false, false), c("tstamp", "timestamp without time zone", false, true)},
	pk:   []string{"id"},
}

// explicitIndexes are the non-constraint indexes the canonical schema creates
// by name; each maps to substrings its indexdef must contain.
var explicitIndexes = map[string][]string{
	"scientific_object_versions_payload_gin": {"USING gin", "payload"},
	"relation_versions_source_idx":           {"source_object_version_id", "relation_type"},
	"relation_versions_target_idx":           {"target_object_version_id", "relation_type"},
	"search_documents_fts_idx":               {"USING gin", "to_tsvector"},
	"search_documents_structured_gin":        {"USING gin", "structured"},
}

// headVersion is the number of migrations in infra/migrations, DERIVED from the
// embedded set rather than hand-maintained. A hardcoded number silently
// invalidated three tests the first time a migration was added (T0013's 00014,
// then its TRUNCATE follow-up 00015); deriving it means the tests track the
// head automatically and can never go stale.
var headVersion = func() int64 {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		panic("reading embedded migrations: " + err.Error())
	}
	var n int64
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}()

// ---------------------------------------------------------------------------
//  1. Fresh install: empty database → head, then the catalog IS the canonical
//     schema (columns, nullability, defaults, PKs, uniques, checks, FKs,
//     indexes, extensions) — verified by querying pg_catalog.
func TestFreshInstallCatalog(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)

	got := takeSnapshot(t, ctx, pool)

	want := make(map[string]tableExp, len(canonicalTables)+1)
	for name, exp := range canonicalTables {
		want[name] = exp
	}
	want["goose_db_version"] = gooseTable
	compareCatalog(t, got, want)

	if v := appliedVersion(t, ctx, pool); v != headVersion {
		t.Errorf("fresh install: applied version = %d, want %d", v, headVersion)
	}
}

// ---------------------------------------------------------------------------
//  2. Repeat migrate: running the migration on an already-migrated database is
//     a safe no-op — zero applied, same version, byte-identical catalog.
func TestRepeatMigrateIsNoop(t *testing.T) {
	ctx := testCtx(t)
	pool, url := testdb.Setup(t, ctx, adminURL(t), taskID)

	before := takeSnapshot(t, ctx, pool)
	beforeVersion := appliedVersion(t, ctx, pool)

	applied, err := persistence.Migrate(ctx, url)
	if err != nil {
		t.Fatalf("repeat migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("repeat migrate applied %d migrations, want 0", applied)
	}

	after := takeSnapshot(t, ctx, pool)
	afterVersion := appliedVersion(t, ctx, pool)
	if afterVersion != beforeVersion {
		t.Errorf("version changed on repeat migrate: %d → %d", beforeVersion, afterVersion)
	}
	if snapshotJSON(t, before) != snapshotJSON(t, after) {
		t.Error("catalog changed on repeat migrate (want byte-identical)")
	}
}

// ---------------------------------------------------------------------------
//  3. Upgrade path: migrate to an intermediate version, verify the intermediate
//     state, then continue to head — the result must be indistinguishable from
//     a fresh install (docs/67: "migration 必须 fresh install + upgrade path").
func TestUpgradePath(t *testing.T) {
	ctx := testCtx(t)

	// Intermediate database, migrated only to version 6.
	pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), taskID)
	applied, err := persistence.MigrateTo(ctx, url, 6)
	if err != nil {
		t.Fatalf("upgrade path: migrate to 6: %v", err)
	}
	if applied != 6 {
		t.Errorf("upgrade path: applied %d to reach version 6, want 6", applied)
	}
	if v := appliedVersion(t, ctx, pool); v != 6 {
		t.Fatalf("upgrade path: version after MigrateTo(6) = %d, want 6", v)
	}

	intermediate := takeSnapshot(t, ctx, pool)
	present := []string{
		"users", "organizations", "organization_memberships", "programs",
		"projects", "project_memberships", "policy_versions", "branches",
		"project_states", "state_commits", "scientific_objects",
		"scientific_object_versions", "relations", "relation_versions",
	}
	absent := []string{
		"evidence_assertions", "blobs", "blob_attachments", "issues",
		"pull_requests", "reviews", "validation_results", "releases",
		"research_assets", "research_asset_versions", "asset_lineage",
		"asset_dependencies", "knowledge_publications", "external_references",
		"external_reference_snapshots", "contribution_events",
		"credit_disputes", "research_events", "outbox_events",
		"subscriptions", "webhook_deliveries", "audit_log", "search_documents",
	}
	for _, name := range present {
		if _, ok := intermediate.Tables[name]; !ok {
			t.Errorf("upgrade path: table %s missing at intermediate version 6", name)
		}
	}
	for _, name := range absent {
		if _, ok := intermediate.Tables[name]; ok {
			t.Errorf("upgrade path: table %s must not exist at intermediate version 6", name)
		}
	}
	if st, ok := intermediate.Tables["relation_versions"]; ok {
		found := false
		for _, idx := range st.ExplicitIndexes {
			if strings.HasPrefix(idx, "relation_versions_source_idx") {
				found = true
			}
		}
		if !found {
			t.Error("upgrade path: relation_versions_source_idx missing at intermediate version 6")
		}
	}

	// Continue to head.
	applied, err = persistence.Migrate(ctx, url)
	if err != nil {
		t.Fatalf("upgrade path: migrate to head: %v", err)
	}
	if applied != headVersion-6 {
		t.Errorf("upgrade path: applied %d on the way to head, want %d", applied, headVersion-6)
	}
	if v := appliedVersion(t, ctx, pool); v != headVersion {
		t.Fatalf("upgrade path: version after head = %d, want %d", v, headVersion)
	}
	upgraded := takeSnapshot(t, ctx, pool)

	// Fresh reference install.
	freshPool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)
	fresh := takeSnapshot(t, ctx, freshPool)

	if snapshotJSON(t, upgraded) != snapshotJSON(t, fresh) {
		t.Error("upgrade path: upgraded catalog differs from fresh install")
	}
	// And both must equal the canonical fixture, not merely each other.
	want := make(map[string]tableExp, len(canonicalTables)+1)
	for name, exp := range canonicalTables {
		want[name] = exp
	}
	want["goose_db_version"] = gooseTable
	compareCatalog(t, upgraded, want)
}

// appliedVersion reads the last applied migration version from the goose
// bookkeeping table through the pool.
func appliedVersion(t *testing.T, ctx context.Context, pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}) int64 {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT COALESCE(MAX(version_id), 0) FROM goose_db_version WHERE is_applied`)
	if err != nil {
		t.Fatalf("appliedVersion: %v", err)
	}
	defer rows.Close()
	var v int64
	if !rows.Next() {
		t.Fatal("appliedVersion: no row")
	}
	if err := rows.Scan(&v); err != nil {
		t.Fatalf("appliedVersion: scan: %v", err)
	}
	return v
}

// ---------------------------------------------------------------------------
//  4. Append-only / constraint enforcement: each constraint must actually
//     REJECT a forbidden UPDATE/DELETE/INSERT, with the exact SQLSTATE —
//     not merely exist in the catalog.
func TestConstraintEnforcement(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)

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
	u2 := mustQueryUUID(`INSERT INTO users (handle, display_name) VALUES ('bob', 'Bob') RETURNING id`)
	o1 := mustQueryUUID(`INSERT INTO organizations (slug, name) VALUES ('acme', 'Acme') RETURNING id`)
	p1 := mustQueryUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'p1', 'P1', 'testing constraints', 'private', $2) RETURNING id`, o1, u1)
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

	// wantErr asserts err carries the expected SQLSTATE.
	wantErr := func(stmt string, err error, wantState string) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: expected a PostgreSQL error (SQLSTATE %s), got %v", stmt, wantState, err)
		}
		if pgErr.Code != wantState {
			t.Errorf("%s: SQLSTATE = %s, want %s", stmt, pgErr.Code, wantState)
		} else {
			t.Logf("%s → rejected with SQLSTATE %s (%s), as required", stmt, pgErr.Code, pgErr.Message)
		}
	}

	// FK RESTRICT rejects DELETE of a user that projects reference — history
	// cannot lose its author (23503 = foreign_key_violation).
	_, err := pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, u1)
	wantErr("DELETE FROM users (referenced by projects.created_by)", err, "23503")

	// The append-only guard (00014) rejects DELETE of a project state even
	// before FK RESTRICT is consulted — history rows are pinned by the
	// database, not merely by referential integrity (P0001 raise_exception).
	_, err = pool.Exec(ctx, `DELETE FROM project_states WHERE id = $1`, s1)
	wantErr("DELETE FROM project_states (append-only guard)", err, "P0001")

	// UNIQUE rejects an UPDATE that would duplicate the other user's handle
	// (23505).
	_, err = pool.Exec(ctx, `UPDATE users SET handle = 'bob' WHERE id = $1`, u1)
	_ = u2
	wantErr("UPDATE users SET handle (duplicate)", err, "23505")

	// CHECK rejects a forbidden UPDATE of a lifecycle state (23514).
	_, err = pool.Exec(ctx, `UPDATE branches SET lifecycle_state = 'bogus' WHERE id = $1`, b1)
	wantErr("UPDATE branches SET lifecycle_state = 'bogus'", err, "23514")

	// UNIQUE(object_id, version_no) rejects a duplicate version insert —
	// versions are append-only, never overwritten (23505).
	_, err = pool.Exec(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 1, $2, 'core/dataset', '1.0', 'Dataset A', 'active', '{}'::jsonb, 'ih-1', $3)`,
		so1, s1, u1)
	wantErr("INSERT duplicate scientific_object_version version_no=1", err, "23505")

	// CHECK(version_no > 0) rejects version zero (23514).
	_, err = pool.Exec(ctx, `INSERT INTO scientific_object_versions
		(object_id, version_no, state_id, schema_id, schema_version, title,
		 lifecycle_state, payload, integrity_hash, created_by)
		VALUES ($1, 0, $2, 'core/dataset', '1.0', 'Dataset A', 'active', '{}'::jsonb, 'ih-1', $3)`,
		so1, s1, u1)
	wantErr("INSERT scientific_object_version version_no=0", err, "23514")

	// CHECK on membership roles rejects an unknown role (23514).
	_, err = pool.Exec(ctx, `INSERT INTO organization_memberships (organization_id, user_id, role)
		VALUES ($1, $2, 'admin')`, o1, u1)
	wantErr("INSERT organization_memberships role='admin'", err, "23514")

	// CHECK on blobs rejects negative sizes (23514).
	_, err = pool.Exec(ctx, `INSERT INTO blobs (content_hash, size_bytes, storage_key, created_by)
		VALUES ('h', -1, 'k', $1)`, u1)
	wantErr("INSERT blobs size_bytes=-1", err, "23514")

	// CHECK on policy_versions rejects setting both scopes (23514).
	_, err = pool.Exec(ctx, `INSERT INTO policy_versions (organization_id, project_id, version, policy_json, created_by)
		VALUES ($1, $2, 'v1', '{}'::jsonb, $3)`, o1, p1, u1)
	wantErr("INSERT policy_versions with both organization_id and project_id", err, "23514")

	// The append-only flow itself still works: version 2 lands cleanly and
	// both rows remain intact.
	sov2 := sov(2)
	var versionCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM scientific_object_versions WHERE object_id = $1`, so1).Scan(&versionCount); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versionCount != 2 {
		t.Errorf("append-only flow: %d versions after appending v2, want 2", versionCount)
	}
	_ = sov1
	_ = sov2
}
