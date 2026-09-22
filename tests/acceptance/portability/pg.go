package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The PostgreSQL half: the rows the released state's scientific truth is
// rebuilt from, and the rebuild itself.
//
// The row set is the state's own lineage plus everything the release pins —
// not the whole database. What is deliberately absent (research asset
// versions, validation results, the review rows, semantic merge records) is
// named in the RESULT's honest-report section: none of it is an input to any
// of the four identifier classes, and the release manifest embeds the review
// record as bytes rather than by reference.
//
// Rows travel as JSON, one document per table, produced by `to_jsonb(t)`.
// Importing them goes through `jsonb_populate_record(NULL::<table>, ...)`,
// which maps by COLUMN NAME: `RETURNING *` and INSERT column order are not
// the declaration order for several of these tables (releases is the one the
// repository's own notes call out), so a positional copy would be wrong in a
// way that only shows up on the tables nobody looks at.

// rowQuery is one table's export query. Ordering is stable so that two
// exports of the same project produce byte-identical documents.
type rowQuery struct {
	Table string
	// Params names, in $1..$n order, which of the export's three values
	// this query binds: "project", "release" or "blobs". It is stated
	// rather than inferred because the extended protocol refuses a
	// statement handed a parameter it does not name AND cannot infer the
	// type of a parameter it is handed but does not use — so the count and
	// the order both have to be right per query, and a single shared
	// argument vector is wrong for at least one of these tables whichever
	// way it is built.
	Params []string
	SQL    string
}

// ExportTables is written in FOREIGN-KEY DEPENDENCY ORDER as far as the
// schema allows. Two constraints make a pure order impossible — branches →
// project_states and project_states → branches — so the import defers those
// two for the length of its transaction (see deferCyclicForeignKeys) rather
// than dropping or disabling anything.
var ExportTables = []rowQuery{
	{"organizations", []string{"project"}, `SELECT to_jsonb(t) FROM organizations t
		WHERE t.id IN (SELECT organization_id FROM projects WHERE id = $1)
		ORDER BY t.id`},
	{"users", nil, `SELECT to_jsonb(t) FROM users t ORDER BY t.id`},
	{"organization_memberships", []string{"project"}, `SELECT to_jsonb(t) FROM organization_memberships t
		WHERE t.organization_id IN (SELECT organization_id FROM projects WHERE id = $1)
		ORDER BY t.organization_id, t.user_id`},
	{"programs", []string{"project"}, `SELECT to_jsonb(t) FROM programs t
		WHERE t.id IN (SELECT program_id FROM projects WHERE id = $1)
		ORDER BY t.id`},
	{"projects", []string{"project"}, `SELECT to_jsonb(t) FROM projects t WHERE t.id = $1 ORDER BY t.id`},
	{"project_states", []string{"project"}, `SELECT to_jsonb(t) FROM project_states t WHERE t.project_id = $1 ORDER BY t.created_at, t.id`},
	{"branches", []string{"project"}, `SELECT to_jsonb(t) FROM branches t WHERE t.project_id = $1 ORDER BY t.created_at, t.id`},
	{"state_commits", []string{"project"}, `SELECT to_jsonb(t) FROM state_commits t WHERE t.project_id = $1 ORDER BY t.created_at, t.id`},
	{"scientific_objects", []string{"project"}, `SELECT to_jsonb(t) FROM scientific_objects t WHERE t.project_id = $1 ORDER BY t.id`},
	{"scientific_object_versions", []string{"project"}, `SELECT to_jsonb(t) FROM scientific_object_versions t
		WHERE t.object_id IN (SELECT id FROM scientific_objects WHERE project_id = $1)
		ORDER BY t.object_id, t.version_no, t.id`},
	{"relations", []string{"project"}, `SELECT to_jsonb(t) FROM relations t WHERE t.project_id = $1 ORDER BY t.id`},
	{"relation_versions", []string{"project"}, `SELECT to_jsonb(t) FROM relation_versions t
		WHERE t.relation_id IN (SELECT id FROM relations WHERE project_id = $1)
		ORDER BY t.relation_id, t.version_no, t.id`},
	{"blobs", []string{"blobs"}, `SELECT to_jsonb(t) FROM blobs t WHERE t.id = ANY($1::uuid[]) ORDER BY t.content_hash, t.id`},
	{"blob_attachments", []string{"project"}, `SELECT to_jsonb(t) FROM blob_attachments t
		WHERE t.scientific_object_version_id IN (
			SELECT id FROM scientific_object_versions
			WHERE object_id IN (SELECT id FROM scientific_objects WHERE project_id = $1))
		ORDER BY t.blob_id, t.scientific_object_version_id, t.attachment_role`},
	{"policy_versions", []string{"project", "release"}, `SELECT to_jsonb(t) FROM policy_versions t
		WHERE t.project_id = $1
		   OR t.id IN (SELECT policy_version_id FROM releases WHERE id = $2)
		   OR t.id IN (SELECT org_policy_version_id FROM releases WHERE id = $2)
		ORDER BY t.created_at, t.id`},
	{"releases", []string{"release"}, `SELECT to_jsonb(t) FROM releases t WHERE t.id = $1 ORDER BY t.id`},
	{"project_schema_profiles", []string{"project"}, `SELECT to_jsonb(t) FROM project_schema_profiles t WHERE t.project_id = $1 ORDER BY t.created_at, t.id`},
}

// tableRows is one table's exported rows.
type tableRows struct {
	Table string            `json:"table"`
	Rows  []json.RawMessage `json:"rows"`
}

// stateDocument is state.json.
type stateDocument struct {
	ProjectID string         `json:"project_id"`
	ReleaseID string         `json:"release_id"`
	Tables    []tableRows    `json:"tables"`
	Counts    map[string]int `json:"row_counts"`
}

// dumpRows reads the row set for one project and release.
func dumpRows(ctx context.Context, pool *pgxpool.Pool, projectID, releaseID string, blobIDs []string) (*stateDocument, error) {
	doc := &stateDocument{ProjectID: projectID, ReleaseID: releaseID, Counts: map[string]int{}}
	for _, q := range ExportTables {
		args := make([]any, 0, len(q.Params))
		for _, p := range q.Params {
			switch p {
			case "project":
				args = append(args, projectID)
			case "release":
				args = append(args, releaseID)
			case "blobs":
				args = append(args, blobIDs)
			default:
				return nil, fmt.Errorf("table %s binds an unknown parameter %q", q.Table, p)
			}
		}
		rows, err := pool.Query(ctx, q.SQL, args...)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", q.Table, err)
		}
		var out []json.RawMessage
		for rows.Next() {
			var raw json.RawMessage
			if err := rows.Scan(&raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan %s: %w", q.Table, err)
			}
			out = append(out, raw)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", q.Table, err)
		}
		if out == nil {
			out = []json.RawMessage{}
		}
		doc.Tables = append(doc.Tables, tableRows{Table: q.Table, Rows: out})
		doc.Counts[q.Table] = len(out)
	}
	return doc, nil
}

// cyclicForeignKeys are the constraints the import defers for the length of
// its transaction: branches.base_state_id → project_states.id and
// project_states.parent_state_id → project_states.id. The first makes a pure
// insert order impossible (each side needs the other), and the second needs
// it only because an unsorted state dump is a legitimate input.
//
// They are DEFERRED, never disabled: every one of these constraints still
// holds at COMMIT, and the append-only triggers on project_states,
// scientific_object_versions, relation_versions, state_commits, releases,
// policy_versions and project_schema_profiles are untouched — the import
// still cannot UPDATE or DELETE a single history row.
func deferCyclicForeignKeys(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `
		SELECT c.conname, c.conrelid::regclass::text
		FROM pg_constraint c
		WHERE c.contype = 'f'
		  AND c.conrelid IN ('branches'::regclass, 'project_states'::regclass)
		  AND NOT c.condeferrable`)
	if err != nil {
		return err
	}
	type con struct{ name, table string }
	var cons []con
	for rows.Next() {
		var c con
		if err := rows.Scan(&c.name, &c.table); err != nil {
			rows.Close()
			return err
		}
		cons = append(cons, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range cons {
		stmt := fmt.Sprintf("ALTER TABLE %s ALTER CONSTRAINT %s DEFERRABLE INITIALLY DEFERRED", c.table, pgx.Identifier{c.name}.Sanitize())
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("defer %s: %w", c.name, err)
		}
	}
	_, err = tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED")
	return err
}

// loadRows inserts an exported row set into a clean database.
//
// rewriteProject and rewriteBlob are applied to the two rows whose identity
// fields legitimately differ between environments, and only those two:
//
//   - projects.git_repository_external_id names the repository the project is
//     provisioned against. specs/schemas/rsg-manifest.schema.json says of the
//     repository id that it "is deliberately NOT part of the manifest — it is
//     mutable project data", so pointing the imported project at the target's
//     own repository cannot move a pinned hash; the re-derived state hash
//     after the import is what proves that rather than asserts it.
//   - blobs.storage_key addresses the bytes in the target's object store, the
//     same fact under a different path. The manifest pins {id, hash} and
//     never the bytes or their address ("The manifest pins the reference,
//     never the bytes", invariant 7).
func loadRows(ctx context.Context, pool *pgxpool.Pool, doc *stateDocument, rewriteProject func(map[string]any) error, rewriteBlob func(map[string]any) error) (map[string]int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if err := deferCyclicForeignKeys(ctx, tx); err != nil {
		return nil, err
	}

	inserted := map[string]int{}
	for _, t := range doc.Tables {
		n := 0
		for _, raw := range t.Rows {
			body := []byte(raw)
			if t.Table == "projects" || t.Table == "blobs" {
				var obj map[string]any
				if err := json.Unmarshal(raw, &obj); err != nil {
					return nil, fmt.Errorf("parse an exported %s row: %w", t.Table, err)
				}
				if t.Table == "projects" && rewriteProject != nil {
					if err := rewriteProject(obj); err != nil {
						return nil, err
					}
				}
				if t.Table == "blobs" && rewriteBlob != nil {
					if err := rewriteBlob(obj); err != nil {
						return nil, err
					}
				}
				if body, err = json.Marshal(obj); err != nil {
					return nil, err
				}
			}
			// jsonb_populate_record maps by column name, so a table
			// whose physical column order differs from its declaration
			// order (releases) is still restored correctly.
			stmt := fmt.Sprintf("INSERT INTO %s SELECT * FROM jsonb_populate_record(NULL::%s, $1::jsonb)", pgx.Identifier{t.Table}.Sanitize(), pgx.Identifier{t.Table}.Sanitize())
			if _, err := tx.Exec(ctx, stmt, body); err != nil {
				return nil, fmt.Errorf("insert into %s (row %d): %w", t.Table, n, err)
			}
			n++
		}
		inserted[t.Table] = n
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit the import: %w", err)
	}
	return inserted, nil
}

// projectBySlugOrID resolves the project a bundle is about. An id is
// preferred when it looks like one, so a re-run against a renamed slug still
// finds the same project.
func projectBySlugOrID(ctx context.Context, pool *pgxpool.Pool, ref string) (id, slug string, err error) {
	if strings.Contains(ref, "-") && len(ref) == 36 {
		err = pool.QueryRow(ctx, `SELECT id::text, slug FROM projects WHERE id = $1`, ref).Scan(&id, &slug)
	} else {
		err = pool.QueryRow(ctx, `SELECT id::text, slug FROM projects WHERE slug = $1`, ref).Scan(&id, &slug)
	}
	if err != nil {
		return "", "", fmt.Errorf("resolve project %q: %w", ref, err)
	}
	return id, slug, nil
}

// releaseRef is the row the export pins.
type releaseRef struct {
	ID                 string
	Version            string
	StateID            string
	Manifest           string
	ManifestHash       string
	PolicyVersionID    *string
	OrgPolicyVersionID *string
}

// resolveRelease reads the release to export: the named version, or the most
// recent one.
func resolveRelease(ctx context.Context, pool *pgxpool.Pool, projectID, version string) (releaseRef, error) {
	var r releaseRef
	q := `SELECT id::text, version, state_id::text, manifest, manifest_hash, policy_version_id::text, org_policy_version_id::text
	      FROM releases WHERE project_id = $1`
	args := []any{projectID}
	if version != "" {
		q += ` AND version = $2`
		args = append(args, version)
	} else {
		q += ` ORDER BY created_at DESC, id DESC LIMIT 1`
	}
	err := pool.QueryRow(ctx, q, args...).Scan(&r.ID, &r.Version, &r.StateID, &r.Manifest, &r.ManifestHash, &r.PolicyVersionID, &r.OrgPolicyVersionID)
	if err != nil {
		return r, fmt.Errorf("read the release of project %s (version %q): %w", projectID, version, err)
	}
	return r, nil
}

// blobIDsForProject lists every blob the project's own rows reference: the
// transitive closure of blob_attachments over the project's object versions.
//
// It is not the same set as the release manifest's blob_refs, and the
// difference is the reason the export carries the UNION rather than the
// manifest's list: the manifest pins the blobs of the RELEASED STATE, while
// blob_attachments spans every version the project holds, branches included.
// Exporting only the pinned ones leaves attachment rows whose blob row never
// travelled — which the importer's foreign key rejects, one row at a time,
// long after the bundle was declared good.
func blobIDsForProject(ctx context.Context, pool *pgxpool.Pool, projectID string) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT b.id::text
		FROM blobs b
		WHERE b.id IN (
			SELECT ba.blob_id FROM blob_attachments ba
			JOIN scientific_object_versions sov ON sov.id = ba.scientific_object_version_id
			JOIN scientific_objects so ON so.id = sov.object_id
			WHERE so.project_id = $1)
		ORDER BY b.content_hash, b.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// blobRowsFor reads the blob rows an export pins.
func blobRowsFor(ctx context.Context, pool *pgxpool.Pool, ids []string) ([]BlobRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT id::text, content_hash, size_bytes, coalesce(media_type, ''), coalesce(storage_key, '')
		FROM blobs WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BlobRow
	for rows.Next() {
		var b BlobRow
		if err := rows.Scan(&b.ID, &b.ContentHash, &b.SizeBytes, &b.MediaType, &b.StorageKey); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ContentHash != out[j].ContentHash {
			return out[i].ContentHash < out[j].ContentHash
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
