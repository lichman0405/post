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
//
// The default port is 5432, the port `make infra-up` actually publishes:
// docker-compose.yml defaults POSTGRES_PORT to it and CI's service container
// uses it. This line used to say 15432 — the port the compose file documents
// as the override for a *second* stack — so `go test ./...` with no
// environment and the documented stack up failed every test in this package
// at testdb.Setup with "connection refused" while `make test-integration`,
// which passes 5432 on the command line, was green. It is the same stale
// default the test-integration target fixed for itself; a test that cannot
// reach the stack it names is not a stricter test, only a louder one.
func adminURL(t *testing.T) string {
	t.Helper()
	if u := os.Getenv("POSTGRES_TEST_ADMIN_URL"); u != "" {
		return u
	}
	return "postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post"
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
		cols:    []colExp{c("id", u, false, true), c("handle", txt, false, false), c("email", txt, true, false), c("display_name", txt, false, false), c("created_at", ts, false, true), c("disabled_at", ts, true, false), c("password_hash", txt, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"handle"}, {"email"}},
	},
	"profiles": {
		cols: []colExp{c("user_id", u, false, false), c("bio", txt, false, true), c("updated_at", ts, false, true)},
		pk:   []string{"user_id"},
		fks:  []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	"organizations": {
		// deactivated_at is the T0103 addition (00017) — the only "delete"
		// the domain offers; the canonical seed will be back-ported.
		cols:    []colExp{c("id", u, false, true), c("slug", txt, false, false), c("name", txt, false, false), c("description", txt, true, false), c("created_at", ts, false, true), c("deactivated_at", ts, true, false)},
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
		// provision_status is the T0104 addition (00019): every new project
		// is provision-pending until T0301 provisions the GitProvider repo.
		// The consistency CHECK is the T0301 addition (00022); the failure
		// reason deliberately has no projects column (see 00022's header).
		cols:    []colExp{c("id", u, false, true), c("organization_id", u, true, false), c("program_id", u, true, false), c("slug", txt, false, false), c("name", txt, false, false), c("purpose", txt, false, false), c("activity_status", txt, false, true), c("visibility", txt, false, false), c("main_frozen", bl, false, true), c("git_repository_external_id", txt, true, false), c("created_by", u, false, false), c("created_at", ts, false, true), c("provision_status", txt, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"organization_id", "slug"}},
		checks:  []string{"activity_status = ANY", "visibility = ANY", "provision_status = ANY", "provision_status <> 'provisioned'"},
		fks:     []fkExp{fk("organization_id", "organizations", "RESTRICT"), fk("program_id", "programs", "SET NULL"), fk("created_by", "users", "RESTRICT")},
	},
	"git_repository_provisions": {
		// T0301 (00022): the platform→GitProvider repository mapping and the
		// push-webhook HMAC secret. project_id PK IS the project→repo 1:1
		// invariant.
		cols:    []colExp{c("project_id", u, false, false), c("owner", txt, false, false), c("name", txt, false, false), c("gitea_repo_id", i8, false, false), c("webhook_id", i8, false, false), c("webhook_secret", txt, false, false), c("provisioned_at", ts, false, true)},
		pk:      []string{"project_id"},
		uniques: [][]string{{"owner", "name"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
	"git_user_identities": {
		// T0304 (00032): the platform→GitProvider user identity mapping
		// (one shadow account "u-<user uuid>" per platform user).
		cols:    []colExp{c("user_id", u, false, false), c("gitea_username", txt, false, false), c("created_at", ts, false, true)},
		pk:      []string{"user_id"},
		uniques: [][]string{{"gitea_username"}},
		fks:     []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	"git_repo_access": {
		// T0304 (00032): the repo access mapping (permission derived from
		// the platform role; revocation sets revoked_at, never deletes).
		cols:   []colExp{c("project_id", u, false, false), c("user_id", u, false, false), c("permission", txt, false, false), c("granted_at", ts, false, true), c("revoked_at", ts, true, false)},
		pk:     []string{"project_id", "user_id"},
		checks: []string{"permission = ANY", "revoked_at >= granted_at"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("user_id", "users", "RESTRICT")},
	},
	"git_access_tokens": {
		// T0304 (00032): minted scoped tokens — the value is never stored,
		// gitea_token_id is the revocation handle, rows flip to revoked.
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("user_id", u, false, false), c("gitea_username", txt, false, false), c("token_name", txt, false, false), c("gitea_token_id", i8, false, false), c("scope", txt, false, false), c("status", txt, false, true), c("issued_at", ts, false, true), c("revoked_at", ts, true, false)},
		pk:     []string{"id"},
		checks: []string{"scope = ANY", "status = ANY", "revoked_at >= issued_at", "status = 'active'"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("user_id", "users", "RESTRICT"), {col: "gitea_username", refTable: "git_user_identities", refCol: "gitea_username", onDelete: "RESTRICT"}},
	},
	"project_memberships": {
		cols:   []colExp{c("project_id", u, false, false), c("user_id", u, false, false), c("role", txt, false, false), c("created_at", ts, false, true)},
		pk:     []string{"project_id", "user_id"},
		checks: []string{"role = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("user_id", "users", "RESTRICT")},
	},
	"project_schema_profiles": {
		// T0213 (00038): namespaced, versioned JSON Schema profiles that
		// extend the official base schemas — append-only (the 00014 guard
		// trigger rejects UPDATE/DELETE), content is the exact generated
		// document bytes (TEXT so the hash pins byte equality).
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("schema_id", txt, false, false), c("version", txt, false, false), c("base_schema_id", txt, false, false), c("base_schema_version", txt, false, false), c("content", txt, false, false), c("content_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "schema_id", "version"}},
		checks:  []string{"schema_id ~~", "version ~", "base_schema_version", "jsonb_typeof", "content_hash"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"policy_versions": {
		cols:   []colExp{c("id", u, false, true), c("organization_id", u, true, false), c("project_id", u, true, false), c("version", txt, false, false), c("policy_json", jb, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"(organization_id IS NOT NULL) <>", "char_length(version)"}, // XOR: exactly one scope set; version is a bounded label (00033)
		fks:    []fkExp{fk("organization_id", "organizations", "RESTRICT"), fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"branches": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("name", txt, false, false), c("visibility", txt, false, false), c("purpose", txt, true, false), c("git_ref", txt, false, false), c("base_state_id", u, true, false), c("lifecycle_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "name"}},
		checks:  []string{"visibility = ANY", "lifecycle_state = ANY"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"git_branch_refs": {
		// T0303 (00031): the semantic branch → Git ref mapping and sync
		// record, maintained by the branches triggers — one row per branch,
		// born with work to do, closed only through 'closing'.
		cols:   []colExp{c("branch_id", u, false, false), c("git_ref", txt, false, false), c("fork_sha", txt, true, false), c("head_sha", txt, true, false), c("sync_state", txt, false, true), c("close_requested_at", ts, true, false), c("synced_at", ts, true, false), c("closed_at", ts, true, false), c("created_at", ts, false, true), c("updated_at", ts, false, true)},
		pk:     []string{"branch_id"},
		checks: []string{"sync_state = ANY"},
		fks:    []fkExp{fk("branch_id", "branches", "RESTRICT")},
	},
	"git_push_ingestions": {
		// T0305 (00034): one row per accepted push delivery; the dedupe key
		// (gitea_repo_id, git_ref, after_sha) makes redelivered webhooks a
		// no-op; head_skip_reason records a refused (guarded) head advance.
		cols:    []colExp{c("id", u, false, true), c("delivery_id", txt, true, false), c("gitea_repo_id", i8, false, false), c("project_id", u, true, false), c("branch_name", txt, false, false), c("git_ref", txt, false, false), c("before_sha", txt, false, false), c("after_sha", txt, false, false), c("commit_count", i4, false, true), c("pusher", txt, true, false), c("commits", jb, false, true), c("head_skip_reason", txt, true, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"gitea_repo_id", "git_ref", "after_sha"}},
		checks:  []string{"head_skip_reason = ANY"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
	"git_push_changes": {
		// T0305 (00034): the inspected changed-file set of one ingestion,
		// classified semantic_manifest vs unstructured.
		cols:   []colExp{c("ingestion_id", u, false, false), c("path", txt, false, false), c("change_kind", txt, false, false), c("file_kind", txt, false, false), c("schema_id", txt, true, false), c("content_sha256", txt, true, false)},
		pk:     []string{"ingestion_id", "path"},
		checks: []string{"change_kind = ANY", "file_kind = ANY"},
		fks:    []fkExp{fk("ingestion_id", "git_push_ingestions", "RESTRICT")},
	},
	"git_push_semantic_candidates": {
		// T0305 (00034): the candidate semantic diff per changed manifest —
		// content-immutable, status-only transitions.
		cols:    []colExp{c("id", u, false, true), c("ingestion_id", u, false, false), c("path", txt, false, false), c("change_kind", txt, false, false), c("schema_id", txt, false, false), c("candidate", jb, false, false), c("status", txt, false, true), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"ingestion_id", "path"}},
		checks:  []string{"change_kind = ANY", "status = ANY"},
		fks:     []fkExp{fk("ingestion_id", "git_push_ingestions", "RESTRICT")},
	},
	"git_branch_semantic_states": {
		// T0306 (00042): the branch semantic completeness flag — a projection
		// of the ingested push evidence, upserted per branch, read by the
		// PR/merge gates.
		cols:   []colExp{c("branch_id", u, false, false), c("semantic_state", txt, false, true), c("updated_at", ts, false, true)},
		pk:     []string{"branch_id"},
		checks: []string{"semantic_state = ANY"},
		fks:    []fkExp{fk("branch_id", "branches", "RESTRICT")},
	},
	"git_reconciliation_runs": {
		// T0309 (00047): one row per reconciliation pass — the reconciler's
		// bookkeeping, opened at pass start, counts filled at pass end; a
		// crashed pass stays unfinished (visible, never silent).
		cols: []colExp{c("id", u, false, true), c("started_at", ts, false, true), c("finished_at", ts, true, false), c("refs_checked", i4, false, true), c("states_checked", i4, false, true), c("mapping_violations", i4, false, true), c("repositories_checked", i4, false, true), c("findings_opened", i4, false, true), c("findings_open", i4, false, true), c("findings_resolved", i4, false, true), c("provider_error", txt, true, false)},
		pk:   []string{"id"},
	},
	"git_reconciliation_findings": {
		// T0309 (00047): one row per drift instance — severity pinned to
		// 'high' at the storage layer, content immutable with a forward-only
		// status transition, one open finding per (kind, project, subject)
		// via the partial unique index below.
		cols:   []colExp{c("id", u, false, true), c("run_id", u, false, false), c("project_id", u, false, false), c("kind", txt, false, false), c("severity", txt, false, true), c("subject_ref", txt, false, false), c("detail", jb, false, false), c("repair_proposal", jb, false, false), c("status", txt, false, true), c("created_at", ts, false, true), c("resolved_at", ts, true, false)},
		pk:     []string{"id"},
		checks: []string{"kind = ANY", "severity = 'high'", "status = ANY"},
		fks:    []fkExp{fk("run_id", "git_reconciliation_runs", "RESTRICT"), fk("project_id", "projects", "RESTRICT")},
	},
	"project_states": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("branch_id", u, true, false), c("parent_state_id", u, true, false), c("state_hash", txt, false, false), c("git_commit_sha", txt, true, false), c("manifest_version", txt, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "state_hash"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("parent_state_id", "project_states", "RESTRICT")},
	},
	"project_template_instantiations": {
		// T0214 (00056): the append-only provenance record of a project's
		// template origin — one row per project (UNIQUE project_id), the
		// template id/version/name are labels (the catalog is platform code,
		// not a table), and both FK targets RESTRICT so neither the project
		// nor the creator can vanish under the record.
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("template_id", txt, false, false), c("template_version", txt, false, false), c("template_name", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id"}},
		checks:  []string{"template_id ~", "template_version ~", "char_length(template_name)"},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"state_commits": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("branch_id", u, false, false), c("base_state_id", u, true, false), c("result_state_id", u, false, false), c("actor_id", u, false, false), c("via", txt, false, false), c("message", txt, false, false), c("operation_summary", jb, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"via = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("result_state_id", "project_states", "RESTRICT"), fk("actor_id", "users", "RESTRICT")},
	},
	"scientific_objects": {
		// current_version_no is the T0202 addition (00024): the
		// materialized head pointer that doubles as the expected_version
		// compare-and-swap cell.
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("object_type", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true), c("current_version_no", i4, false, true)},
		pk:     []string{"id"},
		checks: []string{"current_version_no >= 0"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"scientific_object_versions": {
		cols:    []colExp{c("id", u, false, true), c("object_id", u, false, false), c("version_no", i4, false, false), c("state_id", u, false, false), c("branch_id", u, true, false), c("schema_id", txt, false, false), c("schema_version", txt, false, false), c("title", txt, false, false), c("lifecycle_state", txt, false, false), c("payload", jb, false, false), c("visibility_policy_id", u, true, false), c("integrity_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"object_id", "version_no"}},
		checks:  []string{"version_no > 0", "lifecycle_state = ANY"},
		fks:     []fkExp{fk("object_id", "scientific_objects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("branch_id", "branches", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"relations": {
		// current_version_no is the T0203 addition (00025): the
		// materialized head pointer that doubles as the expected_version
		// compare-and-swap cell, exactly as 00024 does for scientific
		// objects (T0202).
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("created_at", ts, false, true), c("current_version_no", i4, false, true)},
		pk:     []string{"id"},
		checks: []string{"current_version_no >= 0"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
	"relation_versions": {
		cols:    []colExp{c("id", u, false, true), c("relation_id", u, false, false), c("version_no", i4, false, false), c("state_id", u, false, false), c("relation_type", txt, false, false), c("source_object_version_id", u, false, false), c("target_object_version_id", u, false, false), c("payload", jb, false, true), c("integrity_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"relation_id", "version_no"}},
		checks:  []string{"version_no > 0"},
		fks:     []fkExp{fk("relation_id", "relations", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("source_object_version_id", "scientific_object_versions", "RESTRICT"), fk("target_object_version_id", "scientific_object_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	// knowledge_relation_endpoint_types is the T0501 addition (00040): the
	// DB-side endpoint-type declaration of the two knowledge edges, kept in
	// lockstep with internal/rsg/relationcatalog by an integration test.
	"knowledge_relation_endpoint_types": {
		cols: []colExp{c("relation_type", txt, false, false), arr("source_object_types", true, false), arr("target_object_types", false, false)},
		pk:   []string{"relation_type"},
	},
	"evidence_assertions": {
		cols: []colExp{c("id", u, false, true), c("project_id", u, false, false), c("state_id", u, false, false), c("target_object_version_id", u, false, false), c("evidence_object_version_id", u, false, false), c("relation_type", txt, false, false), c("evidence_type", txt, false, false), c("scope", jb, false, true), c("directness", txt, false, true), c("inference_nature", txt, false, true), c("reasoning_note", txt, true, false), c("review_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:   []string{"id"},
		// T0504 (00058): the full evidence-assertion schema enum surface as
		// CHECKs — evidence_type, directness, inference_nature and the
		// scope object on top of 00007's relation/review-state enums —
		// plus the directed-edge rule (the two version pins must differ),
		// which mirrors domain.EvidenceAssertion.Validate.
		// Deliberately NO unique on the (target, evidence) pair: supports
		// and contradicts coexist on the same pair (task acceptance).
		checks: []string{"relation_type = ANY", "review_state = ANY", "evidence_type = ANY", "directness = ANY", "inference_nature = ANY", "jsonb_typeof(scope) = 'object'", "target_object_version_id <> evidence_object_version_id"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("target_object_version_id", "scientific_object_versions", "RESTRICT"), fk("evidence_object_version_id", "scientific_object_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"blobs": {
		cols:    []colExp{c("id", u, false, true), c("content_hash", txt, false, false), c("size_bytes", i8, false, false), c("media_type", txt, true, false), c("storage_key", txt, false, false), c("integrity_state", txt, false, true), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"content_hash", "size_bytes"}},
		checks:  []string{"size_bytes >= 0", "integrity_state = ANY"},
		fks:     []fkExp{fk("created_by", "users", "RESTRICT")},
	},
	"claims": {
		// T0502: the structured claim projection (00041) — one row per
		// claim version, the schema's claim_type/assessment enums as
		// CHECKs and the jsonb fields pinned to their jsonb_typeof.
		cols: []colExp{
			c("version_id", u, false, false), c("object_id", u, false, false),
			c("claim_type", txt, false, false), c("subject_ref", txt, true, false),
			c("property", txt, true, false), c("value", jb, true, false),
			c("scope", jb, false, true), c("scope_conditions", jb, false, true),
			c("basis", jb, false, true), c("assessment", txt, true, false),
			c("created_at", ts, false, true),
		},
		pk:     []string{"version_id"},
		checks: []string{"claim_type = ANY", "assessment = ANY", "jsonb_typeof(scope) = 'object'", "jsonb_typeof(scope_conditions) = 'array'", "jsonb_typeof(basis) = 'array'"},
		fks:    []fkExp{fk("version_id", "scientific_object_versions", "RESTRICT"), fk("object_id", "scientific_objects", "RESTRICT")},
	},
	"findings": {
		// T0503: the structured finding projection (00057) — one row per
		// finding version, the schema's finding_type/assessment enums as
		// CHECKs. The pinned claim version refs live in the
		// finding_claim_versions child table, guarded at commit.
		// finding_type is nullable because the finding schema leaves it
		// optional (unlike claim_type, which claim.schema.json requires
		// and 00041 therefore holds NOT NULL).
		cols: []colExp{
			c("version_id", u, false, false), c("object_id", u, false, false),
			c("finding_type", txt, true, false), c("assessment", txt, true, false),
			c("created_at", ts, false, true),
		},
		pk:     []string{"version_id"},
		checks: []string{"finding_type = ANY", "assessment = ANY"},
		fks:    []fkExp{fk("version_id", "scientific_object_versions", "RESTRICT"), fk("object_id", "scientific_objects", "RESTRICT")},
	},
	"finding_claim_versions": {
		// T0503: the projection's materialized claim_version_refs — one
		// row per pinned claim VERSION (both FKs point at version rows,
		// never objects: a later claim version is a different row and can
		// never silently rewrite the pin). position carries the array
		// order.
		cols: []colExp{
			c("finding_version_id", u, false, false), c("claim_version_id", u, false, false),
			c("position", i4, false, true),
		},
		pk:      []string{"finding_version_id", "claim_version_id"},
		uniques: [][]string{{"finding_version_id", "position"}},
		// "position" is a PostgreSQL keyword, so the catalog renders the
		// column quoted in the CHECK def.
		checks: []string{">= 0"},
		fks:    []fkExp{fk("finding_version_id", "scientific_object_versions", "RESTRICT"), fk("claim_version_id", "scientific_object_versions", "RESTRICT")},
	},
	"blob_attachments": {
		// state_id is the T0206 addition (00035): the state the attachment
		// was created in, like every member row (object versions, relation
		// versions, evidence). The manifest filters blob refs on THIS
		// column — the attachment's own creating state — never on the
		// owning version's state, so a later attachment cannot leak into
		// earlier states' manifests.
		cols:   []colExp{c("blob_id", u, false, false), c("scientific_object_version_id", u, false, false), c("attachment_role", txt, false, false), c("access_level", txt, false, false), c("state_id", u, false, false)},
		pk:     []string{"blob_id", "scientific_object_version_id", "attachment_role"},
		checks: []string{"access_level = ANY"},
		fks:    []fkExp{fk("blob_id", "blobs", "RESTRICT"), fk("scientific_object_version_id", "scientific_object_versions", "RESTRICT"), fk("state_id", "project_states", "RESTRICT")},
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
		// The state CHECK is the T0402 addition (00051): the canonical
		// docs/43 vocabulary, enforced for any write path.
		checks: []string{"state = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("source_branch_id", "branches", "RESTRICT"), fk("target_branch_id", "branches", "RESTRICT"), fk("base_state_id", "project_states", "RESTRICT"), fk("proposed_state_id", "project_states", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	"reviews": {
		// T0404 (00061): per-dimension review records — reviewed_state_id
		// pins the exact head the reviewer evaluated, responsibility
		// carries the reviewer-responsibility label, and the unique
		// constraint scopes one decision per person per kind per head
		// (the acceptance "一人不同 review kind 可记录" as a database
		// fact). The kind CHECK is narrowed to the two documented
		// dimensions and the decision vocabulary to the task's three
		// tokens; both had zero rows to migrate.
		cols:    []colExp{c("id", u, false, true), c("pull_request_id", u, false, false), c("reviewer_id", u, false, false), c("review_kind", txt, false, false), c("decision", txt, false, false), c("body", txt, false, true), c("created_at", ts, false, true), c("reviewed_state_id", u, false, false), c("responsibility", txt, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"pull_request_id", "reviewer_id", "review_kind", "reviewed_state_id"}},
		checks:  []string{"review_kind = ANY", "decision = ANY"},
		fks:     []fkExp{fk("pull_request_id", "pull_requests", "RESTRICT"), fk("reviewer_id", "users", "RESTRICT"), fk("reviewed_state_id", "project_states", "RESTRICT")},
	},
	"contribution_opportunities": {
		// T0803 (00062): the open-contribution opportunity row — target
		// (issue | research_question) with difficulty/capability
		// metadata, the suggested → open → closed machine, and the
		// internal → public visibility machine (publicize-only, flagged
		// path; publicized rows frozen). target_id has no FK (the target
		// is polymorphic — the deferred constraint trigger pins it).
		cols: []colExp{
			c("id", u, false, true), c("project_id", u, false, false),
			c("target_type", txt, false, false), c("target_id", u, false, false),
			c("title", txt, false, false), c("description", txt, false, true),
			c("difficulty", txt, false, false), arr("required_capabilities", false, true),
			c("state", txt, false, true), c("visibility", txt, false, true),
			c("created_by", u, false, false), c("suggested_by", u, true, false),
			c("approved_by", u, true, false), c("publicized_by", u, true, false),
			c("publicized_at", ts, true, false),
			c("created_at", ts, false, true), c("updated_at", ts, false, true),
		},
		pk:     []string{"id"},
		checks: []string{"target_type = ANY", "difficulty = ANY", "cardinality(required_capabilities) <= 10", "state = ANY", "visibility = ANY"},
		fks:    []fkExp{fk("approved_by", "users", "RESTRICT"), fk("created_by", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT"), fk("publicized_by", "users", "RESTRICT"), fk("suggested_by", "users", "RESTRICT")},
	},
	"validation_results": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("state_id", u, false, false), c("gate", txt, false, false), c("status", txt, false, false), c("result_json", jb, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"gate = ANY", "status = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT")},
	},
	"releases": {
		// 00053: manifest became text (content-addressed bytes — jsonb's key
		// normalization breaks manifest_hash verification) and the release
		// pins the organization policy version beside the project one
		// (docs/11 §1); both land at the end, ALTER ADD COLUMN appends.
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("version", txt, false, false), c("title", txt, false, false), c("state_id", u, false, false), c("policy_version_id", u, true, false), c("manifest", txt, false, false), c("manifest_hash", txt, false, false), c("created_by", u, false, false), c("created_at", ts, false, true), c("org_policy_version_id", u, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "version"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("state_id", "project_states", "RESTRICT"), fk("policy_version_id", "policy_versions", "RESTRICT"), fk("org_policy_version_id", "policy_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	// 00053 (T0606): the Idempotency-Key ledger — a key replays the release
	// it created, forever; append-only (both trigger halves).
	"release_creations": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("idempotency_key", txt, false, false), c("release_id", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "idempotency_key"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("release_id", "releases", "RESTRICT")},
	},
	// 00063 (T0609): project milestones — research-timeline markers,
	// separate from releases; the release link is optional (nullable, not
	// required). kind carries the canonical vocabulary plus custom, with
	// the custom-label rule as a database CHECK (the "btrim" substring
	// distinguishes it from the kind CHECK, which also mentions custom).
	"project_milestones": {
		cols:   []colExp{c("id", u, false, true), c("project_id", u, false, false), c("kind", txt, false, false), c("label", txt, true, false), c("occurred_at", ts, false, false), c("release_id", u, true, false), c("created_by", u, false, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"kind = ANY", "btrim"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("release_id", "releases", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	// 00063 (T0609): the milestone Idempotency-Key ledger — a key replays
	// the milestone it created, forever.
	"project_milestone_creations": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("idempotency_key", txt, false, false), c("milestone_id", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "idempotency_key"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("milestone_id", "project_milestones", "RESTRICT")},
	},
	"research_assets": {
		// pid is the T0701 addition (00064): the persistent identifier
		// the public URLs are built from — random, fixed-shape (CHECK),
		// unique, never derived from the slug or the owning organization.
		cols:   []colExp{c("id", u, false, true), c("asset_type", txt, false, false), c("slug", txt, false, false), c("title", txt, false, false), c("origin_project_id", u, false, false), c("created_at", ts, false, true), c("pid", txt, false, true)},
		pk:     []string{"id"},
		checks: []string{"asset_type = ANY", "pid ~"},
		fks:    []fkExp{fk("origin_project_id", "projects", "RESTRICT")},
	},
	"research_asset_versions": {
		// origin_refs is the T0701 addition (00064): the mandatory
		// provenance pins (kind:value) of a published version — NOT NULL,
		// at least one element, and no NULL element (the version schema's
		// array of strings with origin_refs minItems 1).
		//
		// rights_json's jsonb_typeof check is the T0703 addition (00066):
		// the rights document is a JSON object and nothing else. The
		// vocabulary inside it is deliberately NOT a storage rule — its
		// one definition is internal/rights (see the migration's
		// comment).
		//
		// manifest's jsonb_typeof check is the T0702 addition (00067):
		// the version manifest is a JSON object and nothing else — the
		// same boundary, for the other document of the row. Its
		// vocabulary (the four asset types' required metadata, the
		// dependency pins) has one definition too, internal/assets.
		cols:    []colExp{c("id", u, false, true), c("asset_id", u, false, false), c("version", txt, false, false), c("source_release_id", u, true, false), c("manifest", jb, false, false), c("rights_json", jb, false, false), c("visibility", txt, false, false), c("integrity_hash", txt, false, false), c("published_by", u, false, false), c("published_at", ts, false, true), arr("origin_refs", false, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"asset_id", "version"}},
		checks:  []string{"visibility = ANY", "cardinality", "array_position", "jsonb_typeof(rights_json) = 'object'", "jsonb_typeof(manifest) = 'object'"},
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
	"asset_version_parties": {
		// 00082 (T0711). The six roles of docs/11 §6 are three shapes, and
		// this is the version-scoped one: the credited parties. The two
		// UNIQUEs are the shape's meaning — a party appears once per role,
		// and each position is one party — and party_id carries NO foreign
		// key, because a (kind, id) reference points into users for one kind
		// and organizations for the other (00082's note).
		cols:    []colExp{c("id", u, false, true), c("asset_version_id", u, false, false), c("role", txt, false, false), c("party_kind", txt, false, false), c("party_id", u, false, false), c("position", i4, false, false), c("recorded_by", u, false, false), c("recorded_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"asset_version_id", "role", "party_id"}, {"asset_version_id", "role", "position"}},
		// `"position"` is quoted in the definition because position is a
		// reserved word in that context; the substring has to match the
		// constraint as PostgreSQL renders it.
		checks: []string{"role = ANY", "party_kind = ANY", `"position" >= 0`},
		fks:    []fkExp{fk("asset_version_id", "research_asset_versions", "RESTRICT"), fk("recorded_by", "users", "RESTRICT")},
	},
	"asset_rights_holder_events": {
		// 00082 (T0711). The asset-scoped shape: the append-only chain of
		// designations and transfers. The previous holder is NULLABLE — the
		// first designation of an unheld asset has no predecessor — and the
		// two CHECKs are what make "no previous holder" an all-or-nothing
		// pair and refuse an event that moves nothing.
		cols:    []colExp{c("id", u, false, true), c("asset_id", u, false, false), c("ordinal", i4, false, false), c("holder_kind", txt, false, false), c("holder_id", u, false, false), c("previous_holder_kind", txt, true, false), c("previous_holder_id", u, true, false), c("recorded_by", u, false, false), c("recorded_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"asset_id", "ordinal"}},
		// Each substring below matches exactly ONE of the five definitions
		// (the harness counts matches), so the leading parenthesis is what
		// tells holder_kind's constraint from previous_holder_kind's.
		checks: []string{
			"ordinal >= 1",
			"(holder_kind = ANY",
			"(previous_holder_kind = ANY",
			"(previous_holder_kind IS NULL) = (previous_holder_id IS NULL)",
			"IS DISTINCT FROM holder_kind",
		},
		fks: []fkExp{fk("asset_id", "research_assets", "RESTRICT"), fk("recorded_by", "users", "RESTRICT")},
	},
	"knowledge_publications": {
		// rights_json carries the same rights document as an asset
		// version's, so 00066 (T0703) constrains it the same way: a JSON
		// object, with the vocabulary left to internal/rights.
		cols:    []colExp{c("id", u, false, true), c("object_version_id", u, false, false), c("public_version", txt, false, false), c("rights_json", jb, false, false), c("published_by", u, false, false), c("published_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"object_version_id", "public_version"}},
		checks:  []string{"jsonb_typeof(rights_json) = 'object'"},
		fks:     []fkExp{fk("object_version_id", "scientific_object_versions", "RESTRICT"), fk("published_by", "users", "RESTRICT")},
	},
	"external_references": {
		cols:    []colExp{c("id", u, false, true), c("source_type", txt, false, false), c("external_identifier", txt, false, false), c("canonical_url", txt, true, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"source_type", "external_identifier"}},
	},
	"external_reference_snapshots": {
		// snapshot_hash became derivable in T0508 (00045): NULL means
		// "derive it server-side from the stored metadata bytes".
		cols: []colExp{c("id", u, false, true), c("external_reference_id", u, false, false), c("accessed_at", ts, false, false), c("upstream_version", txt, true, false), c("metadata", jb, false, false), c("snapshot_hash", txt, true, false), c("blob_id", u, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("external_reference_id", "external_references", "RESTRICT"), fk("blob_id", "blobs", "RESTRICT")},
	},
	"external_reference_relation_types": {
		// T0508 (00045): the relation types that may point AT an external
		// reference — the citation/dependency pair of docs/19 §3. The Go
		// relation catalog declares the same set
		// (relationcatalog.ExternalRefTargetTypes); the drift test pins
		// the two copies together.
		cols: []colExp{c("relation_type", txt, false, false)},
		pk:   []string{"relation_type"},
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
		cols: []colExp{c("id", u, false, true), c("event_type", txt, false, false), c("actor_id", u, true, false), c("project_id", u, true, false), c("visibility", txt, false, false), c("payload", jb, false, false), c("correlation_id", txt, false, false), c("occurred_at", ts, false, true), c("outbox_event_id", u, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("actor_id", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT"), fk("outbox_event_id", "outbox_events", "RESTRICT")},
	},
	"outbox_events": {
		// T1006 (00059) appended webhook_fanned_out_at: the fan-out cursor.
		cols: []colExp{c("id", u, false, true), c("event_type", txt, false, false), c("payload", jb, false, false), c("correlation_id", txt, false, false), c("created_at", ts, false, true), c("published_at", ts, true, false), c("attempts", i4, false, true), c("actor_id", u, true, false), c("project_id", u, true, false), c("visibility", txt, false, true), c("last_error", txt, true, false), c("webhook_fanned_out_at", ts, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("actor_id", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT")},
	},
	"subscriptions": {
		// T1002 (00078): the follow/watch row grew updated_at (the PATCH
		// path stamps it) and deleted_at — unsubscribing is SOFT, so the
		// deliveries the subscription produced keep naming it and the row
		// survives as history (CLAUDE.md §9). The four CHECKs close the
		// vocabulary the pre-00078 table left open: target_type is one of
		// the five followable kinds, target_id is non-empty and shaped for
		// its type (a uuid for everything addressed by a canonical id, the
		// 26-character asset pid for an asset — the same rule 00064's
		// research_assets_pid_format carries), and channels is a non-empty
		// subset of the V1 channel set. The target_id shape is what makes
		// the audience queries' ::uuid casts safe rather than a query error
		// waiting on a hand-written row.
		cols: []colExp{c("id", u, false, true), c("user_id", u, false, false), c("target_type", txt, false, false), c("target_id", txt, false, false), arr("event_filters", false, true), arr("channels", false, true), c("created_at", ts, false, true), c("updated_at", ts, false, true), c("deleted_at", ts, true, false)},
		pk:   []string{"id"},
		checks: []string{
			"cardinality(channels) > 0", "'organization'",
			"target_id <> ''", "[0-9a-hjkmnp-tv-z]{26}",
		},
		fks: []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	"subscription_deliveries": {
		// T1002 (00078): one row per (subscription, event, channel) that
		// was fanned out — the research inbox's read model (web rows are
		// born delivered; email rows are born pending for the digest
		// sender). The three CHECKs: closed channel and status
		// vocabularies, and the status/timestamp agreement that makes
		// "delivered at" and "cancelled at" mean exactly what they say.
		// subscription_id is RESTRICT, not CASCADE: an unsubscribe is a
		// soft delete, so nothing may depend on the row ever going away.
		//
		// T1003 (00079) adds read_at, plus a fourth agreement CHECK: it is
		// NULL (unread) or the row is 'delivered'. A pending row is an
		// email the digest has not sent and a cancelled row is one the
		// revocation path withdrew, so neither was ever SHOWN to anyone
		// and neither can be read.
		// T1005 (00081): the row grew the digest sender's claim state —
		// attempts (how often it has been claimed, the number the
		// withdrawal rule counts) and leased_until (the claim lease, so a
		// send outside a transaction cannot be started twice) — plus the
		// CHECK that keeps attempts non-negative.
		cols: []colExp{c("id", u, false, true), c("subscription_id", u, false, false), c("user_id", u, false, false), c("event_id", u, false, false), c("channel", txt, false, false), c("event_type", txt, false, false), c("target_type", txt, false, false), c("target_id", txt, false, false), c("status", txt, false, true), c("created_at", ts, false, true), c("delivered_at", ts, true, false), c("cancelled_at", ts, true, false), c("read_at", ts, true, false), c("attempts", i4, false, true), c("leased_until", ts, true, false)},
		pk:   []string{"id"},
		checks: []string{
			"ARRAY['web'", "ARRAY['pending'",
			// The delivered pairing is spelled "= (delivered_at IS NOT
			// NULL)": the whole pairing, not the bare "delivered_at IS NOT
			// NULL" (which would match the same one definition but drop the
			// "delivered <=> delivered_at" agreement this entry is about,
			// while "status = 'delivered'" alone would match two — the
			// T1003 read_at CHECK also contains it, and a matcher has to
			// identify ONE definition).
			"= (delivered_at IS NOT NULL)", "status = 'cancelled'",
			"read_at IS NULL",
			"attempts >= 0",
		},
		fks: []fkExp{
			fk("event_id", "research_events", "RESTRICT"),
			fk("subscription_id", "subscriptions", "RESTRICT"),
			fk("user_id", "users", "RESTRICT"),
		},
	},
	"notification_preferences": {
		// T1005 (00081): how often ONE account receives its email
		// notifications. Absence of a row is the default cadence, not "no
		// email" (events.DefaultCadence) — the sender reads the default,
		// so no account is ever left without a defined behaviour. The
		// CHECK is the cadence vocabulary; events.ValidateCadence checks
		// the same three values on the application path. last_digest_at is
		// the interval anchor a digest cadence is due against, NULL until
		// the first digest goes out (which is due immediately).
		cols: []colExp{c("user_id", u, false, false), c("cadence", txt, false, true), c("last_digest_at", ts, true, false), c("created_at", ts, false, true), c("updated_at", ts, false, true)},
		pk:   []string{"user_id"},
		checks: []string{
			// The IN-list folds into ONE check constraint, so this is one
			// entry, not three (the catalog compares the count). The values
			// themselves are the product decision and are pinned where they
			// are decided (internal/events' ValidateCadence suite); what the
			// catalog adds is that the column cannot hold anything else —
			// including through a hand-written INSERT.
			"ARRAY['immediate'",
		},
		fks: []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	"subscription_fanned_events": {
		// T1002 (00078): the subscription fan-out's per-consumer cursor,
		// the shape T1006's webhook_fanned_out_at gave the webhook
		// pipeline — one row per consumed outbox event, keyed by it, so
		// two consumers of the same outbox cannot see each other's
		// progress and a re-run is a no-op.
		cols: []colExp{c("outbox_event_id", u, false, false), c("fanned_at", ts, false, true)},
		pk:   []string{"outbox_event_id"},
		fks:  []fkExp{fk("outbox_event_id", "outbox_events", "RESTRICT")},
	},
	"webhook_deliveries": {
		// T1006 (00059): the delivery log grew endpoint_id (SET NULL, so an
		// endpoint delete keeps the log rows — the endpoint column preserves
		// the fan-out-time URL snapshot), event_type, retry/delivery
		// timestamps and last_error; status gained a 'pending' default (a
		// fanned-out delivery is pending by definition).
		cols: []colExp{c("id", u, false, true), c("event_id", u, false, false), c("endpoint", txt, false, false), c("status", txt, false, true), c("response_code", i4, true, false), c("attempts", i4, false, true), c("last_attempt_at", ts, true, false), c("endpoint_id", u, true, false), c("event_type", txt, false, true), c("created_at", ts, false, true), c("next_retry_at", ts, true, false), c("last_error", txt, true, false), c("delivered_at", ts, true, false)},
		pk:   []string{"id"},
		fks:  []fkExp{fk("event_id", "research_events", "RESTRICT"), fk("endpoint_id", "webhook_endpoints", "SET NULL")},
	},
	"webhook_endpoints": {
		// T1006 (00059): the endpoint registry. The secret is the HMAC
		// signing key (plaintext by necessity) and is only ever returned at
		// create/rotation — the CHECKs keep degenerate rows out. Deletion
		// is soft (deleted_at): the endpoint row survives so its
		// delivery-log rows keep their endpoint_id and stay readable.
		cols:   []colExp{c("id", u, false, true), c("user_id", u, false, false), c("url", txt, false, false), c("secret", txt, false, false), arr("event_filters", false, true), c("enabled", bl, false, true), c("consecutive_failures", i4, false, true), c("disabled_at", ts, true, false), c("deleted_at", ts, true, false), c("created_at", ts, false, true)},
		pk:     []string{"id"},
		checks: []string{"url <> ''", "secret <> ''"},
		fks:    []fkExp{fk("user_id", "users", "RESTRICT")},
	},
	// T0407: the conflict resolution decisions (migration 00054). The
	// per-conflict identity uniqueness is an explicit NULLS NOT DISTINCT
	// index (a constraint UNIQUE would treat two NULL other-object keys as
	// distinct and the overwrite would append) — covered in
	// explicitIndexes, so this table carries no constraint uniques.
	"conflict_resolutions": {
		cols: []colExp{
			c("id", u, false, true),
			c("project_id", u, false, false),
			c("base_state_id", u, false, false),
			c("source_state_id", u, false, false),
			c("target_state_id", u, false, false),
			c("target_kind", txt, false, false),
			c("target_id", u, false, false),
			c("conflict_code", txt, false, false),
			c("conflict_fields", jb, false, true),
			c("conflict_payload_keys", jb, false, true),
			c("conflict_other_object_id", u, true, false),
			c("resolution", txt, false, false),
			c("note", txt, false, true),
			c("decided_by", u, false, false),
			c("decided_at", ts, false, true),
			c("updated_at", ts, false, true),
		},
		pk:     []string{"id"},
		checks: []string{"target_kind", "resolution"},
		fks: []fkExp{
			fk("project_id", "projects", "RESTRICT"),
			fk("base_state_id", "project_states", "RESTRICT"),
			fk("source_state_id", "project_states", "RESTRICT"),
			fk("target_state_id", "project_states", "RESTRICT"),
			fk("decided_by", "users", "RESTRICT"),
		},
	},
	"audit_log": {
		cols: []colExp{c("id", u, false, true), c("actor_id", u, true, false), c("via", txt, false, false), c("action", txt, false, false), c("target_ref", txt, true, false), c("project_id", u, true, false), c("correlation_id", txt, false, false), c("before_summary", jb, true, false), c("after_summary", jb, true, false), c("metadata", jb, false, true), c("occurred_at", ts, false, true), c("organization_id", u, true, false)},
		pk:   []string{"id"},
		// T0110: organization_id arrived in migration 00020; the canonical
		// seed (specs/database/postgres.sql) will be back-ported, like the
		// organizations.deactivated_at column.
		fks: []fkExp{fk("actor_id", "users", "RESTRICT"), fk("project_id", "projects", "RESTRICT"), fk("organization_id", "organizations", "RESTRICT")},
	},
	"search_documents": {
		cols: []colExp{c("entity_ref", txt, false, false), c("entity_type", txt, false, false), c("visibility", txt, false, false), c("project_id", u, true, false), c("title", txt, false, false), c("content", txt, false, false), c("structured", jb, false, true), colExp{name: "embedding", dataType: vec, udtName: "vector", nullable: true}, c("updated_at", ts, false, true)},
		pk:   []string{"entity_ref"},
		fks:  []fkExp{fk("project_id", "projects", "RESTRICT")},
	},
	"provenance_edges": {
		// T0505 (00043): the rebuildable provenance graph projection — one
		// row per provenance-category relation version with both endpoint
		// labels resolved. created_at deliberately has no default: the
		// trigger and the rebuild copy the relation version's timestamp
		// (the projection's row is born the moment its source row is).
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("relation_id", u, false, false), c("relation_version_id", u, false, false), c("relation_type", txt, false, false), c("source_object_id", u, false, false), c("source_object_version_id", u, false, false), c("source_object_type", txt, false, false), c("source_title", txt, false, false), c("source_version_no", i4, false, false), c("target_object_id", u, false, false), c("target_object_version_id", u, false, false), c("target_object_type", txt, false, false), c("target_title", txt, false, false), c("target_version_no", i4, false, false), c("created_by", u, false, false), c("created_at", ts, false, false)},
		pk:      []string{"id"},
		uniques: [][]string{{"relation_version_id"}},
		// T0505 (00043): the membership invariant the table itself
		// enforces — relation_type must be a provenance type (the same
		// IMMUTABLE list the trigger and the rebuild consult).
		checks: []string{"relation_type = ANY"},
		fks:    []fkExp{fk("project_id", "projects", "RESTRICT"), fk("relation_id", "relations", "RESTRICT"), fk("relation_version_id", "relation_versions", "RESTRICT"), fk("source_object_id", "scientific_objects", "RESTRICT"), fk("source_object_version_id", "scientific_object_versions", "RESTRICT"), fk("target_object_id", "scientific_objects", "RESTRICT"), fk("target_object_version_id", "scientific_object_versions", "RESTRICT"), fk("created_by", "users", "RESTRICT")},
	},
	// T0406 (00069): one Research PR merge — the record of the transition
	// that advanced an accepted state, and the plan it executed. The Git
	// saga columns are the only mutable ones (see semantic_merge_guard).
	"semantic_merges": {
		cols: []colExp{
			c("id", u, false, true),
			c("project_id", u, false, false),
			c("pull_request_id", u, false, false),
			c("source_branch_id", u, false, false),
			c("target_branch_id", u, false, false),
			c("base_state_id", u, false, false),
			c("source_state_id", u, false, false),
			c("target_state_id", u, false, false),
			c("result_state_id", u, false, false),
			c("actor_id", u, false, false),
			c("plan_version", txt, false, false),
			c("plan", jb, false, false),
			c("plan_digest", txt, false, false),
			c("applied_count", i4, false, true),
			c("kept_target_count", i4, false, true),
			c("carried_count", i4, false, true),
			c("aborted_count", i4, false, true),
			c("withheld_count", i4, false, true),
			c("git_ref", txt, true, false),
			c("git_sha", txt, true, false),
			c("git_state", txt, false, true),
			c("git_error", txt, false, true),
			c("git_attempts", i4, false, true),
			c("created_at", ts, false, true),
			c("updated_at", ts, false, true),
		},
		pk:      []string{"id"},
		uniques: [][]string{{"pull_request_id"}},
		checks:  []string{"git_state"},
		fks: []fkExp{
			fk("project_id", "projects", "RESTRICT"),
			fk("pull_request_id", "pull_requests", "RESTRICT"),
			fk("source_branch_id", "branches", "RESTRICT"),
			fk("target_branch_id", "branches", "RESTRICT"),
			fk("base_state_id", "project_states", "RESTRICT"),
			fk("source_state_id", "project_states", "RESTRICT"),
			fk("target_state_id", "project_states", "RESTRICT"),
			fk("result_state_id", "project_states", "RESTRICT"),
			fk("actor_id", "users", "RESTRICT"),
		},
	},
	// T0406 (00069): the conflicts a merge CARRIES instead of resolving —
	// append-only (00069's guard pair, in appendOnlyTables): a later
	// decision is a new merge, never an edit of a carried row.
	"semantic_merge_conflicts": {
		cols: []colExp{
			c("id", u, false, true),
			c("merge_id", u, false, false),
			c("project_id", u, false, false),
			c("result_state_id", u, false, false),
			c("target_kind", txt, false, false),
			c("target_id", u, false, false),
			c("conflict_code", txt, false, false),
			c("conflict_category", txt, false, false),
			c("conflict_fields", jb, false, true),
			c("conflict_payload_keys", jb, false, true),
			c("other_object_id", u, true, false),
			c("detail", txt, false, true),
			c("decision", txt, false, false),
			c("decided_by", u, false, false),
			c("note", txt, false, true),
			c("source_version_id", u, false, false),
			c("target_version_id", u, true, false),
			c("created_at", ts, false, true),
		},
		pk:     []string{"id"},
		checks: []string{"target_kind", "decision"},
		fks: []fkExp{
			fk("merge_id", "semantic_merges", "RESTRICT"),
			fk("project_id", "projects", "RESTRICT"),
			fk("result_state_id", "project_states", "RESTRICT"),
			fk("decided_by", "users", "RESTRICT"),
		},
	},
	// T0409 (00070): the merge Idempotency-Key ledger — a key replays the
	// merge it created, forever; append-only (both trigger halves), the
	// same contract release_creations implements. merge_id is RESTRICT:
	// a ledger entry without its merge would turn a replay into a miss.
	"merge_creations": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("idempotency_key", txt, false, false), c("merge_id", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "idempotency_key"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("merge_id", "semantic_merges", "RESTRICT")},
	},
	// 00072 (T0705): the Idempotency-Key ledger of a research asset
	// version's PUBLICATION (docs/22, docs/23 §4). It is the third ledger
	// of this shape and the third target: the version row it points at is
	// append-only and immutable, so a retried publish must answer with the
	// version the first request wrote rather than write a second one — the
	// UNIQUE(project_id, idempotency_key) below is that guarantee.
	"asset_publish_creations": {
		cols:    []colExp{c("id", u, false, true), c("project_id", u, false, false), c("idempotency_key", txt, false, false), c("asset_version_id", u, false, false), c("created_at", ts, false, true)},
		pk:      []string{"id"},
		uniques: [][]string{{"project_id", "idempotency_key"}},
		fks:     []fkExp{fk("project_id", "projects", "RESTRICT"), fk("asset_version_id", "research_asset_versions", "RESTRICT")},
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
	// T0203: the query-by-type paths join relation_versions to relations
	// on the project boundary (migration 00025).
	"relations_project_idx":             {"project_id"},
	"search_documents_fts_idx":          {"USING gin", "to_tsvector"},
	"search_documents_structured_gin":   {"USING gin", "structured"},
	"organization_memberships_user_idx": {"user_id"},
	// T0204: the state snapshot projections — a state's member versions by
	// state_id and a branch's state/commit lineage (migration 00026).
	"scientific_object_versions_state_idx": {"state_id"},
	"relation_versions_state_idx":          {"state_id"},
	"project_states_branch_created_idx":    {"branch_id", "created_at"},
	"state_commits_branch_created_idx":     {"branch_id", "created_at"},
	// T0205: branch listing scans (migration 00028).
	"branches_project_created_idx": {"project_id", "created_at"},
	// T0206: the manifest export's blob-ref filter walks
	// blob_attachments.state_id per export (migration 00035).
	"blob_attachments_state_idx": {"state_id"},
	// T0303: the sync backlog scan (boot sweep + redelivered jobs) —
	// partial on the non-terminal states, so 'closed' rows stay out of it
	// (migration 00031).
	"git_branch_refs_sync_backlog_idx": {"sync_state", "WHERE"},
	// T0209: the query surface's project-scoped object scan, with the
	// object-type filter as the second column (migration 00036).
	// T0309: open-finding dedupe — one open finding per (kind, project,
	// subject), the key the reconciler's insert conflicts on to avoid alert
	// spam (migration 00047).
	"scientific_objects_project_type_idx":         {"project_id", "object_type"},
	"git_reconciliation_findings_open_dedupe_idx": {"kind", "project_id", "subject_ref", "WHERE"},
	// T0104: personal projects (organization_id NULL) escape the
	// UNIQUE(organization_id, slug) constraint, so their slug uniqueness is
	// a partial unique index instead.
	"projects_personal_slug_idx": {"organization_id IS NULL", "UNIQUE"},
	// T0803: one active (suggested/open) opportunity per target — the key
	// the creation insert conflicts on (migration 00062); closed rows stay
	// as history.
	"contribution_opportunities_target_active_idx": {"target_type", "target_id", "WHERE"},
	// T0110: Activity page scan paths (newest-first, keyset on
	// (occurred_at, id)) per scope and per actor.
	"audit_log_project_occurred_idx":      {"project_id", "occurred_at"},
	"audit_log_organization_occurred_idx": {"organization_id", "occurred_at"},
	"audit_log_actor_occurred_idx":        {"actor_id", "occurred_at"},
	// T0603: per-scope policy version uniqueness (00033) — a version
	// string is never reused within one policy line.
	"policy_versions_org_version_idx":     {"organization_id", "UNIQUE"},
	"policy_versions_project_version_idx": {"project_id", "UNIQUE"},
	// T0213: profile resolution paths (00038) — the newest registered
	// version of (project, schema id), keyset-ordered.
	// T0502: claim projection query paths (00041) — by object, by type,
	// by subject (partial: only rows that name one), and GIN over the
	// normalized scope conditions and the declared bases.
	// T0505: the provenance projection's read paths (00043) — the
	// whole-project graph scan and the per-endpoint lineage walks.
	// T0508: the snapshot log is read per identity; 00011 never indexed
	// the FK (00045).
	// T1001: the outbox publish dedupe (one research_event per outbox
	// row, ever) and the dispatcher's pending-backlog scan (00046).
	// T0606: the release list's newest-first scan (00053).
	// T0407: one decision per conflict identity per triple (00054) — the
	// NULLS NOT DISTINCT unique index the upsert's ON CONFLICT targets, and
	// the plan read's scan path.
	// T0214: template provenance lookups (00056) — which projects were
	// created from a template version (catalog page / audit questions).
	// T0503: finding projection query paths (00057) — by object, by type,
	// and the reverse lookup from a pinned claim version to the findings
	// that pin it (the impact analysis input, docs/19 §3).
	// T0504: evidence assertion query paths (00058) — the target listing
	// (keyset order) and the reverse lookups of what an evidence version
	// backs.
	// T1006 (00059): the fan-out idempotency guarantee (one delivery row
	// per endpoint per event, ever), the deliverer's due-work scan, and the
	// fan-out backlog scan over published-but-unfanned outbox rows.
	// T0609: the milestone timeline scan — occurred_at, creation order
	// and id (00063).
	// T0701: pid is the asset's public identity and the lookup key of the
	// persistent URLs — the unique index is the uniqueness half of that
	// identity (the shape CHECK is the other).
	"project_schema_profiles_project_idx":          {"project_id", "schema_id", "created_at"},
	"claims_object_idx":                            {"object_id"},
	"claims_type_idx":                              {"claim_type"},
	"claims_subject_idx":                           {"subject_ref", "WHERE"},
	"claims_scope_conditions_gin":                  {"USING gin", "scope_conditions"},
	"claims_basis_gin":                             {"USING gin", "basis"},
	"provenance_edges_project_idx":                 {"project_id", "relation_type"},
	"provenance_edges_source_version_idx":          {"source_object_version_id"},
	"provenance_edges_target_version_idx":          {"target_object_version_id"},
	"external_reference_snapshots_ref_idx":         {"external_reference_id"},
	"research_events_outbox_event_uniq":            {"outbox_event_id", "UNIQUE", "WHERE"},
	"outbox_events_pending_idx":                    {"published_at IS NULL"},
	"releases_project_created_idx":                 {"project_id", "created_at"},
	"conflict_resolutions_identity_idx":            {"UNIQUE", "NULLS NOT DISTINCT", "project_id"},
	"conflict_resolutions_plan_idx":                {"project_id", "base_state_id", "target_kind"},
	"project_template_instantiations_template_idx": {"template_id", "template_version"},
	"findings_object_idx":                          {"object_id"},
	"findings_type_idx":                            {"finding_type"},
	"finding_claim_versions_claim_idx":             {"claim_version_id"},
	"evidence_assertions_target_idx":               {"target_object_version_id", "created_at", "id"},
	"evidence_assertions_evidence_idx":             {"evidence_object_version_id"},
	"webhook_deliveries_endpoint_event_uniq":       {"endpoint_id", "UNIQUE", "WHERE"},
	"webhook_deliveries_due_idx":                   {"status", "next_retry_at"},
	"outbox_events_fanout_pending_idx":             {"webhook_fanned_out_at IS NULL"},
	"project_milestones_timeline_idx":              {"project_id", "occurred_at", "created_at", "id"},
	"research_assets_pid_uniq":                     {"pid", "UNIQUE"},
	// T0406: the merge record's read paths (00069) — a project's merges
	// newest-first, and the saga's retry scan over merges whose Git step has
	// not happened yet (partial: an updated merge never needs the step
	// again).
	"semantic_merges_project_created_idx": {"project_id", "created_at", "id"},
	"semantic_merges_git_pending_idx":     {"git_state", "WHERE"},
	// T0406: the carried conflicts (00069) — read per merge (the merge
	// page) and per target ("is this object still contested in the accepted
	// state?").
	"semantic_merge_conflicts_merge_idx":  {"merge_id", "target_kind", "target_id"},
	"semantic_merge_conflicts_target_idx": {"project_id", "target_kind", "target_id", "created_at"},
	// T1002 (00078): one LIVE subscription per (user, target) — the partial
	// unique index the create insert conflicts on, so unsubscribing and
	// following again is a new row rather than a resurrection; the fan-out's
	// candidate scan by target; the fan-out's idempotency guarantee (one
	// delivery row per subscription per event per channel, ever); the
	// inbox's newest-first read per (owner, status); and the withdraw
	// scan over one subscription's undelivered rows.
	"subscriptions_live_uniq":             {"user_id", "target_type", "target_id", "UNIQUE", "WHERE"},
	"subscriptions_live_target_idx":       {"target_type", "target_id", "WHERE"},
	"subscription_deliveries_uniq":        {"subscription_id", "event_id", "channel", "UNIQUE"},
	"subscription_deliveries_inbox_idx":   {"user_id", "status", "created_at DESC", "id DESC"},
	"subscription_deliveries_pending_idx": {"subscription_id", "WHERE"},
	// T1003 (00079): the mass "mark my inbox read" update — one
	// subscriber's unread delivered web rows. Partial, because the whole
	// statement it answers (markInboxAllRead in internal/events/inbox_store.go)
	// is defined by read_at IS NULL. It is NOT the entries read or the
	// badge: those need the read rows too, so they cannot use a partial
	// index that excludes them.
	"subscription_deliveries_unread_idx": {"user_id", "created_at DESC", "id DESC", "WHERE"},
	// T1004 (00080): the two reads behind the anonymous public feeds. A
	// project's feed walks its assets by origin_project_id, which had no
	// index at all before this migration; one asset's feed reads its
	// versions newest-first through the PARTIAL index, whose predicate is
	// the feed's own visibility filter — so the index holds exactly the
	// rows a public reader may be served, and its ordering columns
	// (published_at DESC, id DESC) are the feed's total order, which is
	// what makes one database state render one document. The predicate is
	// the same filter the canonical query applies in
	// internal/persistence/queries/feeds.sql (T1004 ruling one: the
	// visibility filter is pushed into SQL, so LIMIT falls after it).
	"research_assets_origin_project_idx":           {"origin_project_id"},
	"research_asset_versions_public_published_idx": {"asset_id", "published_at DESC", "id DESC", "WHERE"},
	// T1005 (00081): the digest sender's claim scan — pending email rows,
	// oldest first. Partial, so the scan never has to look at the web rows
	// (born delivered) or at rows already sent.
	"subscription_deliveries_email_pending_idx": {"created_at", "WHERE"},
	// T0711 (00082): the two governance indexes. The credits are read per
	// version and role in declaration order (the asset page's creators
	// block, item by item), and the holder chain is read per asset from the
	// newest end — the current holder is the greatest ordinal, which is the
	// one read that must not scan a chain that grows.
	"asset_version_parties_version_role":       {"asset_version_id", "role", "position"},
	"asset_rights_holder_events_asset_ordinal": {"asset_id", "ordinal DESC"},
}

// migrationVersions returns the numeric prefix of every embedded
// migration file. Derived, never hand-maintained (see appliedAbove); the
// prefix is the version the runner records when it applies the file.
func migrationVersions() []int64 {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		panic("reading embedded migrations: " + err.Error())
	}
	var out []int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		var v int64
		for _, r := range e.Name() {
			if r < '0' || r > '9' {
				break
			}
			v = v*10 + int64(r-'0')
		}
		out = append(out, v)
	}
	return out
}

// appliedAbove counts the embedded migrations with a prefix above floor —
// the exact set the runner applies next from a database at floor. Sparse
// numbering (parallel tasks holding reserved numbers) makes "head - floor"
// wrong, so the count comes from the same embedded set the runner uses.
func appliedAbove(floor int64) int64 {
	var n int64
	for _, v := range migrationVersions() {
		if v > floor {
			n++
		}
	}
	return n
}

// maxVersionNo is the highest migration version NUMBER in infra/migrations,
// DERIVED from the embedded set, like appliedAbove. "Highest number" and
// "file count" differ once the numbering space has gaps: the Supervisor
// reserves numbers for parallel Workers (T0202 was assigned 00024 while
// 00021-00023 were held for others), so goose's max(version_id) is the
// largest number, not the file count.
var maxVersionNo = func() int64 {
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		panic("reading embedded migrations: " + err.Error())
	}
	var max int64
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		underscore := strings.IndexByte(e.Name(), '_')
		if underscore < 1 {
			panic("migration filename without numeric prefix: " + e.Name())
		}
		var n int64
		for _, c := range e.Name()[:underscore] {
			if c < '0' || c > '9' {
				panic("migration filename without numeric prefix: " + e.Name())
			}
			n = n*10 + int64(c-'0')
		}
		if n > max {
			max = n
		}
	}
	return max
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

	if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
		t.Errorf("fresh install: applied version = %d, want %d", v, maxVersionNo)
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
		"external_reference_snapshots", "external_reference_relation_types",
		"contribution_events",
		"credit_disputes", "research_events", "outbox_events",
		"subscriptions", "webhook_deliveries", "webhook_endpoints",
		"audit_log", "search_documents",
		"profiles", "git_repository_provisions", "git_branch_refs",
		"project_schema_profiles",
		"git_branch_semantic_states",
		"project_template_instantiations",
		"contribution_opportunities",
		"project_milestones", "project_milestone_creations",
		"asset_publish_creations",
		"asset_version_parties", "asset_rights_holder_events",
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

	// T0102 data-level upgrade check: identities that exist before the
	// profiles migration (00017) must get a profile row when it lands —
	// the 1:1 identity/profile shape holds for pre-existing data, not just
	// for accounts created after head.
	var preProfileUsers []string
	for _, handle := range []string{"pre-profile-1", "pre-profile-2"} {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (handle, display_name) VALUES ($1, $1) RETURNING id`, handle).Scan(&id); err != nil {
			t.Fatalf("upgrade path: seed pre-profile user: %v", err)
		}
		preProfileUsers = append(preProfileUsers, id)
	}

	// Continue to head.
	applied, err = persistence.Migrate(ctx, url)
	if err != nil {
		t.Fatalf("upgrade path: migrate to head: %v", err)
	}
	if applied != appliedAbove(6) {
		t.Errorf("upgrade path: applied %d on the way to head, want %d", applied, appliedAbove(6))
	}
	if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
		t.Fatalf("upgrade path: version after head = %d, want %d", v, maxVersionNo)
	}
	upgraded := takeSnapshot(t, ctx, pool)

	// The pre-00017 identities were backfilled with profile rows.
	for _, id := range preProfileUsers {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM profiles WHERE user_id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("upgrade path: probe profile backfill: %v", err)
		}
		if n != 1 {
			t.Errorf("upgrade path: profile row not backfilled for pre-migration user %s", id)
		}
	}

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

// ---------------------------------------------------------------------------
//  4. Version-counter backfills (00024/00025): the T0102 'data-level upgrade
//     check' shape, applied to the two counter migrations. Each phase brings
//     a database to the last version BEFORE its migration, writes version-log
//     rows through raw SQL (the pre-migration schema has no counter column to
//     lean on yet), then upgrades to head and asserts every seeded object's
//     current_version_no equals the specific head of its own log — concrete
//     values, not row counts. Deleting the backfill UPDATE from either
//     migration must turn its phase red.
func TestMigrationVersionCounterBackfill(t *testing.T) {
	t.Run("00024 scientific objects", func(t *testing.T) {
		ctx := testCtx(t)

		// Version 20 is the last migration before 00024 (21-23 are reserved
		// for parallel Workers): scientific_objects exists but has no
		// current_version_no column, so the seed can only reach the counter
		// through the version log.
		intermediate := int64(20)
		pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), taskID)
		toIntermediate, err := persistence.MigrateTo(ctx, url, intermediate)
		if err != nil {
			t.Fatalf("00024 backfill: migrate to 20: %v", err)
		}
		if toIntermediate != intermediate {
			t.Errorf("00024 backfill: applied %d to reach version %d, want %d", toIntermediate, intermediate, intermediate)
		}
		if v := appliedVersion(t, ctx, pool); v != intermediate {
			t.Fatalf("00024 backfill: version after MigrateTo(%d) = %d, want %d", intermediate, v, intermediate)
		}

		// Seed rows written BEFORE the migration: three objects whose
		// version logs have different depths — one with three versions, one
		// with two, one with none (its counter must stay at the column
		// default, 0).
		projectID, stateID, userID := seedCounterGraph(t, ctx, pool, "24")
		seedObject := func() string {
			t.Helper()
			var id string
			if err := pool.QueryRow(ctx,
				`INSERT INTO scientific_objects (project_id, object_type, created_by)
				 VALUES ($1, 'dataset', $2) RETURNING id`, projectID, userID).Scan(&id); err != nil {
				t.Fatalf("00024 backfill: seed object: %v", err)
			}
			return id
		}
		seedVersion := func(objectID string, versionNo int) {
			t.Helper()
			if _, err := pool.Exec(ctx, `INSERT INTO scientific_object_versions
				(object_id, version_no, state_id, schema_id, schema_version, title,
				 lifecycle_state, payload, integrity_hash, created_by)
				VALUES ($1, $2, $3, 'core/dataset', '1.0', 'Dataset', 'active',
				        '{}'::jsonb, 'ih-counter', $4)`,
				objectID, versionNo, stateID, userID); err != nil {
				t.Fatalf("00024 backfill: seed version %d: %v", versionNo, err)
			}
		}
		objDeep := seedObject()
		seedVersion(objDeep, 1)
		seedVersion(objDeep, 2)
		seedVersion(objDeep, 3)
		objShallow := seedObject()
		seedVersion(objShallow, 1)
		seedVersion(objShallow, 2)
		objEmpty := seedObject()

		// Continue to head: 00024's backfill UPDATE must set each counter
		// from the version log that exists at that moment. The applied count
		// comes from the embedded set (appliedAbove), not "head - 20":
		// numbering is sparse (21 and 23 are reserved; T0301's 00022 sits
		// between), and the runner applies exactly the files that exist.
		toHead, err := persistence.Migrate(ctx, url)
		if err != nil {
			t.Fatalf("00024 backfill: migrate to head: %v", err)
		}
		if toHead != appliedAbove(intermediate) {
			t.Errorf("00024 backfill: applied %d on the way to head, want %d", toHead, appliedAbove(intermediate))
		}
		if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
			t.Fatalf("00024 backfill: version after head = %d, want %d", v, maxVersionNo)
		}

		// Data-level upgrade check: each pre-migration object's counter must
		// equal the specific head of ITS OWN version log.
		wantCounter := map[string]int32{objDeep: 3, objShallow: 2, objEmpty: 0}
		for objectID, want := range wantCounter {
			var counter, logMax int32
			if err := pool.QueryRow(ctx, `
				SELECT o.current_version_no,
				       COALESCE((SELECT max(v.version_no)
				                   FROM scientific_object_versions v
				                  WHERE v.object_id = o.id), 0)
				  FROM scientific_objects o
				 WHERE o.id = $1`, objectID).Scan(&counter, &logMax); err != nil {
				t.Fatalf("00024 backfill: probe counter: %v", err)
			}
			if counter != logMax {
				t.Errorf("00024 backfill: object %s: current_version_no = %d, want max(version_no) = %d",
					objectID, counter, logMax)
			}
			if counter != want {
				t.Errorf("00024 backfill: object %s: current_version_no = %d, want %d", objectID, counter, want)
			}
		}
	})

	t.Run("00025 relations", func(t *testing.T) {
		ctx := testCtx(t)

		// Version 24 is the last migration before 00025: relations exists
		// but has no current_version_no column yet. 00024 has already
		// landed, so scientific_objects carries its counter here — the
		// relation backfill must read relation_versions, not the object
		// pointer.
		intermediate := int64(24)
		pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), taskID)
		_, err := persistence.MigrateTo(ctx, url, intermediate)
		if err != nil {
			t.Fatalf("00025 backfill: migrate to 24: %v", err)
		}
		// No exact applied count here: migrations numbered <= 24 that are
		// not 00025 itself ride along (T0301's 00022 does today; the
		// reserved numbers 21-23 would, if they land later). The semantic
		// check is the version.
		if v := appliedVersion(t, ctx, pool); v != intermediate {
			t.Fatalf("00025 backfill: version after MigrateTo(%d) = %d, want %d", intermediate, v, intermediate)
		}

		// Seed rows written BEFORE the migration: the two scientific-object
		// endpoints relation_versions references, then three relations whose
		// version logs have different depths — one with two versions, one
		// with one, one with none (its counter must stay at the column
		// default, 0).
		projectID, stateID, userID := seedCounterGraph(t, ctx, pool, "25")
		seedEndpoint := func() string {
			t.Helper()
			var objectID, versionID string
			if err := pool.QueryRow(ctx,
				`INSERT INTO scientific_objects (project_id, object_type, created_by)
				 VALUES ($1, 'dataset', $2) RETURNING id`, projectID, userID).Scan(&objectID); err != nil {
				t.Fatalf("00025 backfill: seed endpoint object: %v", err)
			}
			if err := pool.QueryRow(ctx, `INSERT INTO scientific_object_versions
				(object_id, version_no, state_id, schema_id, schema_version, title,
				 lifecycle_state, payload, integrity_hash, created_by)
				VALUES ($1, 1, $2, 'core/dataset', '1.0', 'Dataset', 'active',
				        '{}'::jsonb, 'ih-counter', $3) RETURNING id`,
				objectID, stateID, userID).Scan(&versionID); err != nil {
				t.Fatalf("00025 backfill: seed endpoint version: %v", err)
			}
			return versionID
		}
		sourceV := seedEndpoint()
		targetV := seedEndpoint()
		seedRelation := func() string {
			t.Helper()
			var id string
			if err := pool.QueryRow(ctx,
				`INSERT INTO relations (project_id) VALUES ($1) RETURNING id`, projectID).Scan(&id); err != nil {
				t.Fatalf("00025 backfill: seed relation: %v", err)
			}
			return id
		}
		seedVersion := func(relationID string, versionNo int) {
			t.Helper()
			if _, err := pool.Exec(ctx, `INSERT INTO relation_versions
				(relation_id, version_no, state_id, relation_type,
				 source_object_version_id, target_object_version_id,
				 payload, integrity_hash, created_by)
				VALUES ($1, $2, $3, 'depends_on', $4, $5, '{}'::jsonb, 'ih-counter', $6)`,
				relationID, versionNo, stateID, sourceV, targetV, userID); err != nil {
				t.Fatalf("00025 backfill: seed version %d: %v", versionNo, err)
			}
		}
		relDeep := seedRelation()
		seedVersion(relDeep, 1)
		seedVersion(relDeep, 2)
		relShallow := seedRelation()
		seedVersion(relShallow, 1)
		relEmpty := seedRelation()

		// Continue to head: 00025's backfill UPDATE must set each counter
		// from the relation_versions log that exists at that moment. The
		// applied count is appliedAbove(intermediate) — the embedded files
		// numbered above 24 — not "head - 24": maxVersionNo is the largest
		// numeric prefix (sparse numbering: 21/23 reserved, 00022 present),
		// and the runner applies exactly the files that exist.
		toHead, err := persistence.Migrate(ctx, url)
		if err != nil {
			t.Fatalf("00025 backfill: migrate to head: %v", err)
		}
		if toHead != appliedAbove(intermediate) {
			t.Errorf("00025 backfill: applied %d on the way to head, want %d", toHead, appliedAbove(intermediate))
		}
		if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
			t.Fatalf("00025 backfill: version after head = %d, want %d", v, maxVersionNo)
		}

		// Data-level upgrade check: each pre-migration relation's counter
		// must equal the specific head of ITS OWN version log.
		wantCounter := map[string]int32{relDeep: 2, relShallow: 1, relEmpty: 0}
		for relationID, want := range wantCounter {
			var counter, logMax int32
			if err := pool.QueryRow(ctx, `
				SELECT r.current_version_no,
				       COALESCE((SELECT max(v.version_no)
				                   FROM relation_versions v
				                  WHERE v.relation_id = r.id), 0)
				  FROM relations r
				 WHERE r.id = $1`, relationID).Scan(&counter, &logMax); err != nil {
				t.Fatalf("00025 backfill: probe counter: %v", err)
			}
			if counter != logMax {
				t.Errorf("00025 backfill: relation %s: current_version_no = %d, want max(version_no) = %d",
					relationID, counter, logMax)
			}
			if counter != want {
				t.Errorf("00025 backfill: relation %s: current_version_no = %d, want %d", relationID, counter, want)
			}
		}
	})
}

// ---------------------------------------------------------------------------
//  5. Branch-ref backfill direction (00031): branches that predate the
//     mapping table get their row from the backfill — the create direction
//     ('pending') for live branches, but the close direction ('closing' +
//     close_requested_at) for branches already merged/aborted at upgrade
//     time. Mapping a closed branch as 'pending' would make the boot sweep
//     create (or adopt-and-keep) a provider ref for a semantic path that
//     must never accept pushes again — the exact state the task's close
//     strategy forbids, and no close trigger can ever fire for those
//     terminal rows again (00028). The born-closed insert trigger
//     (branch_git_ref_map) has always known this; the backfill must mirror
//     it. Deleting the CASE from the backfill must turn this test red.
func TestMigrationBranchRefBackfillDirection(t *testing.T) {
	ctx := testCtx(t)

	// Version 28 is the last migration before 00031 (29-30 are reserved):
	// branches exist but the mapping table does not, so the seed can only
	// reach the mapping rows through the backfill.
	intermediate := int64(28)
	pool, url := testdb.SetupEmpty(t, ctx, adminURL(t), taskID)
	toIntermediate, err := persistence.MigrateTo(ctx, url, intermediate)
	if err != nil {
		t.Fatalf("00031 backfill: migrate to 28: %v", err)
	}
	if toIntermediate != appliedAbove(0)-appliedAbove(intermediate) {
		t.Errorf("00031 backfill: applied %d to reach version %d, want %d",
			toIntermediate, intermediate, appliedAbove(0)-appliedAbove(intermediate))
	}
	if v := appliedVersion(t, ctx, pool); v != intermediate {
		t.Fatalf("00031 backfill: version after MigrateTo(28) = %d, want %d", v, intermediate)
	}

	// Seed the pre-migration world: one branch in every lifecycle state.
	projectID, stateID, userID := seedCounterGraph(t, ctx, pool, "31")
	seedBranch := func(name, lifecycle string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO branches
			(project_id, name, visibility, git_ref, base_state_id, lifecycle_state, created_by)
			VALUES ($1, $2, 'private', 'refs/heads/' || $2, $3, $4, $5)`,
			projectID, name, stateID, lifecycle, userID); err != nil {
			t.Fatalf("00031 backfill: seed branch %s: %v", name, err)
		}
	}
	seedBranch("active-b", "active")
	seedBranch("merged-b", "merged")
	seedBranch("aborted-b", "aborted")

	toHead, err := persistence.Migrate(ctx, url)
	if err != nil {
		t.Fatalf("00031 backfill: migrate to head: %v", err)
	}
	if toHead != appliedAbove(intermediate) {
		t.Errorf("00031 backfill: applied %d on the way to head, want %d", toHead, appliedAbove(intermediate))
	}
	if v := appliedVersion(t, ctx, pool); v != maxVersionNo {
		t.Fatalf("00031 backfill: version after head = %d, want %d", v, maxVersionNo)
	}

	// Data-level upgrade check: the backfill must carry each pre-migration
	// branch's lifecycle into the mapping row — live branches wait for
	// creation, closed branches wait for deletion.
	want := map[string]struct {
		state          string
		closeRequested bool
	}{
		"active-b":  {"pending", false},
		"merged-b":  {"closing", true},
		"aborted-b": {"closing", true},
	}
	for name, exp := range want {
		var gitRef, state string
		var closeRequested bool
		if err := pool.QueryRow(ctx, `
			SELECT git_ref, sync_state, close_requested_at IS NOT NULL
			  FROM git_branch_refs
			 WHERE branch_id = (SELECT id FROM branches WHERE name = $1)`, name).
			Scan(&gitRef, &state, &closeRequested); err != nil {
			t.Fatalf("00031 backfill: probe mapping row for %s: %v", name, err)
		}
		if gitRef != "refs/heads/"+name {
			t.Errorf("00031 backfill: %s git_ref = %q, want derived refs/heads/%s", name, gitRef, name)
		}
		if state != exp.state {
			t.Errorf("00031 backfill: %s sync_state = %q, want %q (the lifecycle-carrying direction)", name, state, exp.state)
		}
		if closeRequested != exp.closeRequested {
			t.Errorf("00031 backfill: %s close_requested = %v, want %v", name, closeRequested, exp.closeRequested)
		}
	}
}

// seedCounterGraph writes the minimal user → organization → project → branch →
// state graph that version rows reference (the same shape
// TestConstraintEnforcement uses), through raw SQL so it works against any
// intermediate schema. Returns the project, state and user ids.
func seedCounterGraph(t *testing.T, ctx context.Context, pool interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, suffix string) (projectID, stateID, userID string) {
	t.Helper()
	mustID := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed counter graph: %s: %v", sql, err)
		}
		return id
	}
	userID = mustID(`INSERT INTO users (handle, display_name) VALUES ($1, $1) RETURNING id`, "counter-"+suffix)
	orgID := mustID(`INSERT INTO organizations (slug, name) VALUES ($1, $1) RETURNING id`, "counter-org-"+suffix)
	projectID = mustID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, $2, 'Counter project', 'testing version counter backfill', 'private', $3) RETURNING id`,
		orgID, "counter-"+suffix, userID)
	branchID := mustID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'main', 'private', 'refs/heads/main', $2) RETURNING id`, projectID, userID)
	stateID = mustID(`INSERT INTO project_states (project_id, branch_id, state_hash, manifest_version)
		VALUES ($1, $2, 'hash-counter', 'v1') RETURNING id`, projectID, branchID)
	return projectID, stateID, userID
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
//  5. Append-only / constraint enforcement: each constraint must actually
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

	// T0303: the derived-ref guard (00031) — git_ref must always equal
	// 'refs/heads/' || name on ANY insert path (P0001), and the mapping
	// row was created by the trigger for the seeded branch.
	_, err = pool.Exec(ctx, `INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'feature-x', 'private', 'refs/heads/wrong', $2)`, p1, u1)
	wantErr("INSERT branch with mismatched git_ref", err, "P0001")

	var mapState, mapRef string
	if err := pool.QueryRow(ctx,
		`SELECT sync_state, git_ref FROM git_branch_refs WHERE branch_id = $1`, b1).Scan(&mapState, &mapRef); err != nil {
		t.Fatalf("probe git_branch_refs mapping row: %v", err)
	}
	if mapState != "pending" || mapRef != "refs/heads/main" {
		t.Errorf("mapping row = %s/%s, want pending/refs/heads/main", mapState, mapRef)
	}

	// T0303: the branch name is immutable — renaming would be a new branch
	// (the git ref is the branch's address), even when the row is updated
	// consistently (P0001).
	_, err = pool.Exec(ctx, `UPDATE branches SET name = 'renamed', git_ref = 'refs/heads/renamed' WHERE id = $1`, b1)
	wantErr("UPDATE branches SET name (immutable)", err, "P0001")

	// T0303: the mapping row's stored ref is immutable too (P0001).
	_, err = pool.Exec(ctx, `UPDATE git_branch_refs SET git_ref = 'refs/heads/other' WHERE branch_id = $1`, b1)
	wantErr("UPDATE git_branch_refs SET git_ref (immutable)", err, "P0001")

	// T0303: a mapping row cannot be born 'closed' — closed is only reached
	// through 'closing' (P0001, raised by the BEFORE trigger ahead of the
	// PK conflict with the trigger-created row).
	b2 := mustQueryUUID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'feature-y', 'private', 'refs/heads/feature-y', $2) RETURNING id`, p1, u1)
	_, err = pool.Exec(ctx, `INSERT INTO git_branch_refs (branch_id, git_ref, sync_state)
		VALUES ($1, 'refs/heads/feature-y', 'closed')`, b2)
	wantErr("INSERT git_branch_refs born closed", err, "P0001")

	// T0303: the sync state machine only moves forward — pending may reach
	// synced, failed or closing, never 'closed' directly (P0001).
	_, err = pool.Exec(ctx, `UPDATE git_branch_refs SET sync_state = 'closed' WHERE branch_id = $1`, b2)
	wantErr("UPDATE git_branch_refs pending→closed (forbidden jump)", err, "P0001")

	// T0303: closing → closed is the one path to terminal, and 'closed' is
	// frozen — even a timestamps-only update of the closed row is rejected.
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs SET sync_state = 'closing' WHERE branch_id = $1`, b2); err != nil {
		t.Fatalf("pending→closing: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs SET sync_state = 'closed' WHERE branch_id = $1`, b2); err != nil {
		t.Fatalf("closing→closed: %v", err)
	}
	_, err = pool.Exec(ctx, `UPDATE git_branch_refs SET synced_at = now() WHERE branch_id = $1`, b2)
	wantErr("UPDATE git_branch_refs after closed (terminal)", err, "P0001")

	// T0303: closing the semantic lifecycle (active → merged) moves the
	// mapping row to 'closing' with close_requested_at set — the syncer's
	// delete direction (the merge itself commits; the ref deletion follows
	// in the job loop).
	if _, err := pool.Exec(ctx, `UPDATE branches SET lifecycle_state = 'merged' WHERE id = $1`, b1); err != nil {
		t.Fatalf("merge branch: %v", err)
	}
	var closeRequested bool
	if err := pool.QueryRow(ctx,
		`SELECT sync_state, close_requested_at IS NOT NULL FROM git_branch_refs WHERE branch_id = $1`, b1).Scan(&mapState, &closeRequested); err != nil {
		t.Fatalf("probe closing row: %v", err)
	}
	if mapState != "closing" || !closeRequested {
		t.Errorf("mapping row after merge = %s (close_requested %v), want closing with close_requested_at set", mapState, closeRequested)
	}

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

// TestGitBranchRefGuardFastPathWhitelist proves the 00034 fast path admits
// EXACTLY the tip pointer: a head_sha-only update passes in a non-terminal
// state, but an update touching any other column — including the two the
// original fixed enumeration missed, created_at and the primary key
// branch_id — is not a head-pointer refresh and falls to the state machine,
// which rejects it ('synced may only move to failed or closing'). The
// branch_id cell needs a second, same-named branch in another project: the
// derived-ref check would reject any other target before the whitelist is
// consulted.
func TestGitBranchRefGuardFastPathWhitelist(t *testing.T) {
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

	u1 := mustQueryUUID(`INSERT INTO users (handle, display_name) VALUES ('whitelist', 'Whitelist') RETURNING id`)
	o1 := mustQueryUUID(`INSERT INTO organizations (slug, name) VALUES ('whitelist-org', 'Whitelist Org') RETURNING id`)
	p1 := mustQueryUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'whitelist-1', 'Whitelist 1', 'testing the fast-path whitelist', 'private', $2) RETURNING id`, o1, u1)
	p2 := mustQueryUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1, 'whitelist-2', 'Whitelist 2', 'testing the fast-path whitelist', 'private', $2) RETURNING id`, o1, u1)
	b1 := mustQueryUUID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'feature-z', 'private', 'refs/heads/feature-z', $2) RETURNING id`, p1, u1)
	b2 := mustQueryUUID(`INSERT INTO branches (project_id, name, visibility, git_ref, created_by)
		VALUES ($1, 'feature-z', 'private', 'refs/heads/feature-z', $2) RETURNING id`, p2, u1)

	// b1's mapping row, driven to 'synced' through the machine.
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs
		SET sync_state = 'synced', synced_at = now(), updated_at = now()
		WHERE branch_id = $1`, b1); err != nil {
		t.Fatalf("whitelist: drive b1 to synced: %v", err)
	}

	// The positive cell: a head-only update IS the fast path's purpose —
	// it must pass (otherwise the rejection cells below would be vacuous).
	if _, err := pool.Exec(ctx, `UPDATE git_branch_refs SET head_sha = 'cafe'
		WHERE branch_id = $1`, b1); err != nil {
		t.Fatalf("whitelist: head-only refresh rejected: %v", err)
	}

	wantMachineErr := func(stmt string, err error) {
		t.Helper()
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("%s: expected a PostgreSQL error from the guard, got %v", stmt, err)
		}
		if pgErr.Code != "P0001" {
			t.Errorf("%s: SQLSTATE = %s, want P0001 (raise_exception)", stmt, pgErr.Code)
		}
		if !strings.Contains(pgErr.Message, "synced may only move to failed or closing") {
			t.Errorf("%s: message %q must carry the machine rejection (the fast path must NOT have admitted the change)",
				stmt, pgErr.Message)
		}
	}

	// created_at-only: not a tip refresh — rejected.
	_, err := pool.Exec(ctx, `UPDATE git_branch_refs SET created_at = now() WHERE branch_id = $1`, b1)
	wantMachineErr("UPDATE git_branch_refs SET created_at", err)

	// branch_id-only: not a tip refresh — rejected. The target is the
	// same-named branch in p2, so the derived-ref check passes and the
	// whitelist is the thing on trial.
	_, err = pool.Exec(ctx, `UPDATE git_branch_refs SET branch_id = $1 WHERE branch_id = $2`, b2, b1)
	wantMachineErr("UPDATE git_branch_refs SET branch_id", err)
}
