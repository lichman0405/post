package integration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestSearchDocumentsEnforcesAccessControl is the regression test for the
// security review finding on internal/persistence/queries/search.sql.
//
// SearchDocuments previously had no visibility or project filter at all, so it
// returned every row matching the text regardless of visibility. docs/54 ranks
// "private project content appearing in Search" as its top-severity scenario,
// and docs/23 §5 requires policy filtering on every query/search/export.
//
// The query now takes the caller's scope explicitly and returns a row only if
// it is public or its project is in that scope. This test proves the fail-closed
// property directly: an empty scope yields public rows only, never the table.
func TestSearchDocumentsEnforcesAccessControl(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)
	q := sqlc.New(pool)

	mustUUID := func(sql string, args ...any) pgtype.UUID {
		t.Helper()
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("setup query failed: %s: %v", sql, err)
		}
		if !id.Valid {
			t.Fatalf("setup query returned a null uuid: %s", sql)
		}
		return id
	}

	alice := mustUUID(`INSERT INTO users (handle, display_name) VALUES ('alice','Alice') RETURNING id`)
	acme := mustUUID(`INSERT INTO organizations (slug, name) VALUES ('acme','Acme') RETURNING id`)
	// The project the searcher IS allowed to see.
	mine := mustUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1,'mine','Mine','p','private',$2) RETURNING id`, acme, alice)
	// A project the searcher is NOT allowed to see.
	theirs := mustUUID(`INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
		VALUES ($1,'theirs','Theirs','p','private',$2) RETURNING id`, acme, alice)

	insert := func(ref, visibility string, projectID pgtype.UUID, title string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO search_documents
			(entity_ref, entity_type, visibility, project_id, title, content, structured)
			VALUES ($1,'dataset',$2,$3,$4,'needle','{}'::jsonb)`, ref, visibility, projectID, title); err != nil {
			t.Fatalf("insert search document %s: %v", ref, err)
		}
	}
	insert("pub-1", "public", pgtype.UUID{}, "Public document")
	insert("mine-1", "private", mine, "My private document")
	insert("theirs-1", "private", theirs, "Their private document")

	search := func(allowed []pgtype.UUID) map[string]bool {
		t.Helper()
		rows, err := q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
			Query:             "needle",
			AllowedProjectIds: allowed,
			PageSize:          100,
			PageOffset:        0,
		})
		if err != nil {
			t.Fatalf("SearchDocuments: %v", err)
		}
		got := make(map[string]bool, len(rows))
		for _, r := range rows {
			got[r.EntityRef] = true
		}
		return got
	}

	// 1. Empty scope: public only. This is the fail-closed property — the case
	//    that used to return everything.
	empty := search(nil)
	if !empty["pub-1"] {
		t.Errorf("empty scope dropped the public document: %v", empty)
	}
	if empty["mine-1"] || empty["theirs-1"] {
		t.Errorf("empty scope leaked private documents: %v", empty)
	}

	// 2. Scope containing only my project: public + mine, never theirs.
	scoped := search([]pgtype.UUID{mine})
	if !scoped["pub-1"] || !scoped["mine-1"] {
		t.Errorf("scoped search missing expected rows: %v", scoped)
	}
	if scoped["theirs-1"] {
		t.Errorf("scoped search leaked another project's private document: %v", scoped)
	}

	// 3. A scope naming the other project must not resurrect mine.
	other := search([]pgtype.UUID{theirs})
	if !other["theirs-1"] {
		t.Errorf("scope naming a project did not return that project's document: %v", other)
	}
	if other["mine-1"] {
		t.Errorf("scope for one project leaked another's: %v", other)
	}
}
