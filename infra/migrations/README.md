# POST database migrations

Forward-only, numbered SQL migrations for the canonical semantic store
(PostgreSQL, docs/53_DATABASE_STANDARD.md).

**These migrations are the canonical schema history.** `specs/database/postgres.sql`
is their generated snapshot, not a hand-maintained source: it is produced by
`scripts/gen_schema_snapshot.py` and `make check-schema-snapshot` fails when the
two diverge. The direction used to be the other way round — the snapshot was
declared the source of truth and the migrations its "faithful decomposition" —
and the result was a snapshot that froze while the schema moved five migrations
past it, in a file everything still trusted. A canonical artifact that silently
lags is worse than none, because it is believed.

## Tool (L1 decision, task T0005)

**pressly/goose v3.28.0** as a library, driven by
`internal/persistence.Migrate` / `MigrateTo`:

- numbered SQL files applied in lexical order, each inside its own transaction;
- a session-level advisory lock makes concurrent migrators safe;
- goose's own `goose_db_version` table is the only non-canonical table the
  runner creates;
- no goose CLI is needed at runtime — migrations are embedded
  (`migrations.go`) and shipped inside the binary.

Alternatives considered: golang-migrate (fine, but its up-only file handling
is quirkier), a hand-rolled runner (baseline requires an explicit migration
tool, docs/7 CLAUDE.md), atlas (declarative, changes the numbered-SQL model).

## Forward-only policy

- Every migration is **immutable once written**: shipped files are never
  edited. A change to an already-released database must be a **new, higher
  number**.
- There are **no down migrations** by design. An upgrade is always "apply the
  remaining migrations in order" — there is exactly one path forward, never an
  ambiguous up/down pair.
- The documented upgrade path: a database at version *n* is brought to head
  with `persistence.MigrateTo(ctx, url, n+1)`, `n+2`, … — or simply
  `persistence.Migrate(ctx, url, ...)` to run all pending migrations. Each
  step is transactional; the end state is identical to a fresh install
  (verified by `TestUpgradePath` in tests/integration, which migrates to
  version 6, then to head, and compares the full pg_catalog state against a
  fresh install and against the canonical fixture).

## Deviations from the canonical file — and why

`schema.sql` / `postgres.sql` is a *seed*, not an executable script: it
creates `project_states` (which references `branches(id)`) before `branches`
exists, which PostgreSQL rejects. The migration set therefore creates
`branches` first. **No constraint was changed** — only statement order was
made executable, and `branches.base_state_id` is still added by the same
`ALTER TABLE … ADD CONSTRAINT branches_base_state_fk` the canonical seed uses.
If the canonical schema is updated, port the delta as a new numbered
migration and extend the fixture in `tests/integration/migration_test.go`.

The `specs/database/postgres.sql` snapshot is the one place a Worker writes
under `specs/`: it is a declared derived artifact of `infra/migrations/**`
(`specs/orchestrator/derived-artifacts.json`), regenerated mechanically, and
never edited by hand.

Additional deviation (T0103): `00018_organization_governance.sql` adds
`organizations.deactivated_at` and the `organization_memberships(user_id)`
index, which the canonical seed does not declare yet. The seed stays frozen,
as it has since the initial spec commit: T0013's append-only triggers and
T0101's 00016 are equally absent from it, and the migration set - not the
seed - is the living schema. Nothing is back-ported.

Additional deviation (T0104): `00019_project_provisioning.sql` adds
`projects.provision_status` and the partial unique index
`projects_personal_slug_idx` (personal projects have organization_id NULL,
which the canonical `UNIQUE (organization_id, slug)` constraint cannot
cover — PostgreSQL treats NULLs as distinct). Same seed policy: the
migration set is the living schema.

## Layout

| File | Content (canonical lines) |
|---|---|
| `00001_extensions.sql` | `vector`, `pgcrypto` extensions |
| `00002_identity.sql` | users, organizations, organization_memberships |
| `00003_projects.sql` | programs, projects, project_memberships, policy_versions |
| `00004_rsg_state.sql` | branches, project_states (+base_state FK), state_commits |
| `00005_scientific_objects.sql` | scientific_objects, scientific_object_versions |
| `00006_relations.sql` | relations, relation_versions |
| `00007_evidence.sql` | evidence_assertions |
| `00008_blobs.sql` | blobs, blob_attachments |
| `00009_issues_pull_requests.sql` | issues, pull_requests, reviews |
| `00010_releases_assets.sql` | validation_results, releases, research_assets, asset versions/lineage/dependencies, knowledge_publications |
| `00011_external_contribution.sql` | external_references(+snapshots), contribution_events, credit_disputes |
| `00012_events_audit.sql` | research_events, outbox_events, subscriptions, webhook_deliveries, audit_log |
| `00013_search_projection.sql` | search_documents (rebuildable projection) |
| `00014_append_only_enforcement.sql` | append-only triggers on the version/history/ledger tables (T0013) |
| `00015_append_only_truncate.sql` | TRUNCATE refused on the same tables (T0013) |
| `00016_auth_password.sql` | users.password_hash (T0101 email+password auth) |
| `00017_profiles.sql` | research profile columns (T0102) |
| `00018_organization_governance.sql` | organizations.deactivated_at, organization_memberships(user_id) index (T0103) |
| `00019_project_provisioning.sql` | projects.provision_status (pending/provisioned/failed), personal-project slug index (T0104) |

`migrations.go` embeds the files (`//go:embed *.sql`) for the runner.

## Adding a migration

1. Name it with **the migration number in your task package** — the Supervisor
   reserves it at dispatch. Do not choose one yourself and do not read one out
   of this file's index: two Workers in parallel once both created
   `00017_*.sql`, each following this README's old advice of "the next number"
   against an index that had stopped five migrations earlier. A number is a
   shared resource, so it is allocated, not inferred. Write
   `NNNNN_description.sql` with `-- +goose Up` and the DDL.
2. If it touches any table a query uses, regenerate:
   `sqlc generate` (v1.31.1 — pinned; see sqlc.yaml).
3. Extend the expected-catalog fixture in `tests/integration/migration_test.go`
   if the canonical schema changed.
4. Regenerate the canonical schema snapshot — it is derived from these files,
   so adding a migration without it leaves the snapshot stale and CI fails:
   ```bash
   python3 scripts/gen_schema_snapshot.py       # regenerate specs/database/postgres.sql
   ```
5. Run the gates:
   ```bash
   go test ./tests/integration/ -count=1        # fresh install, repeat, upgrade path, constraints
   tests/integration/check-sqlc-drift.sh        # generation drift (fails on mismatch)
   make check-schema-snapshot                   # snapshot == ordered migrations
   ```

Never edit an existing numbered file. Never add a down migration.
