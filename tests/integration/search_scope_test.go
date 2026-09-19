package integration

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/planner/plannertest"
)

// This file is the database-level half of T0903's authorization acceptance.
//
// tests/integration/search_access_test.go proves that the canonical read query
// enforces a scope it is handed. This file proves the other end of the same
// property, on a real PostgreSQL: that the scope a search actually runs under
// is DERIVED from an authenticated actor (search.ResolveScope over
// persistence.ProjectStore), and that a search run under THAT scope returns
// exactly the rows the actor may read — no more, and no fewer.
//
// Both directions are asserted, because only one of them can fail silently:
// a leaked row is invisible to a test that only checks "my rows came back",
// and a scope that resolved to nothing looks identical to a project with no
// documents if nobody checks that the allowed row IS returned.
//
// The scenario is the leak docs/54 ranks first ("private project content
// appearing in Search"), in the form that survives naive reasoning: a PUBLIC
// project is not a public row. internal/search/projection.go's
// projectedVisibility is a conjunction of the entity's own axis and the
// project's, and knowledgeProjectedVisibility (sources.go) maps every
// publication whose audience is the project — the ordinary case,
// feeds.sql:36-42 records it as legal and common, 发布不等于公开 — onto
// visibility='private'. Such a row therefore lives in a public project and is
// still members-only, which is why the scope must not name public projects.
func TestSearchScopeIsResolvedFromMembershipAndEnforcedByTheQuery(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), "T0903")
	q := sqlc.New(pool)
	store := persistence.NewProjectStore(pool)

	user := func(handle, display string) pgtype.UUID {
		t.Helper()
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO users (handle, display_name) VALUES ($1,$2) RETURNING id`,
			handle, display).Scan(&id); err != nil {
			t.Fatalf("insert user %s: %v", handle, err)
		}
		return id
	}
	var org pgtype.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO organizations (slug, name) VALUES ('scope-org','Scope Org') RETURNING id`).Scan(&org); err != nil {
		t.Fatalf("insert organization: %v", err)
	}
	project := func(slug, visibility string, owner pgtype.UUID) pgtype.UUID {
		t.Helper()
		var id pgtype.UUID
		if err := pool.QueryRow(ctx, `INSERT INTO projects (organization_id, slug, name, purpose, visibility, created_by)
			VALUES ($1,$2,$3,'p',$4,$5) RETURNING id`,
			org, slug, slug, visibility, owner).Scan(&id); err != nil {
			t.Fatalf("insert project %s: %v", slug, err)
		}
		return id
	}
	member := func(projectID, userID pgtype.UUID, role string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO project_memberships (project_id, user_id, role) VALUES ($1,$2,$3)`,
			projectID, userID, role); err != nil {
			t.Fatalf("insert membership %v/%v: %v", projectID, userID, err)
		}
	}
	document := func(ref, visibility string, projectID pgtype.UUID, title string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO search_documents
			(entity_ref, entity_type, visibility, project_id, title, content, structured)
			VALUES ($1,'dataset',$2,$3,$4,'needle','{}'::jsonb)`, ref, visibility, projectID, title); err != nil {
			t.Fatalf("insert search document %s: %v", ref, err)
		}
	}

	alice := user("alice-scope", "Alice")
	bob := user("bob-scope", "Bob")
	// The project Alice belongs to.
	aliceLab := project("alice-lab", "private", alice)
	// A project Alice does NOT belong to, and which is PUBLIC.
	openNetwork := project("open-network", "public", bob)
	// A project Alice does not belong to either.
	bobsLab := project("bobs-lab", "private", bob)
	member(aliceLab, alice, "owner")
	member(openNetwork, bob, "owner")
	member(bobsLab, bob, "owner")

	document("pub-1", "public", openNetwork, "Public needle document")
	document("mine-1", "private", aliceLab, "Alice's private needle document")
	document("theirs-1", "private", bobsLab, "Bob's private needle document")
	// The row that makes the design decision visible: members-only content in a
	// project the network may read. Alice is not a member of it.
	document("members-only-1", "private", openNetwork, "Members-only needle in a public project")

	// The scope is resolved the way the search path resolves it: from the
	// actor, through the project store, with no other input.
	scope, err := search.ResolveScope(ctx, store, uuidText(t, alice))
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if !scope.Authenticated() {
		t.Fatal("the resolved scope is not attributable to an actor")
	}
	if got, want := scope.AllowedProjectIDs(), []string{uuidText(t, aliceLab)}; len(got) != 1 || got[0] != want[0] {
		t.Fatalf("resolved scope = %v, want exactly the actor's membership %v: naming the public project here would admit its members-only rows", got, want)
	}

	// The search itself, under that scope.
	runSearch := func(question string) map[string]bool {
		t.Helper()
		rows, err := q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
			Query:             question,
			AllowedProjectIds: scope.AllowedProjectUUIDs(),
			PageSize:          100,
			PageOffset:        0,
		})
		if err != nil {
			t.Fatalf("SearchDocuments(%q): %v", question, err)
		}
		got := make(map[string]bool, len(rows))
		for _, r := range rows {
			got[r.EntityRef] = true
		}
		return got
	}

	got := runSearch("needle")
	// Direction 1 — the positive counter-proof. Without this, everything below
	// would also pass against a scope that resolved to nothing at all.
	if !got["pub-1"] {
		t.Errorf("the search dropped the public document: %v", got)
	}
	if !got["mine-1"] {
		t.Errorf("the search dropped a document from the actor's own project: %v", got)
	}
	// Direction 2 — nothing the actor may not read. This is the direction that
	// fails silently when it breaks.
	if got["theirs-1"] {
		t.Errorf("the search leaked another project's private document: %v", got)
	}
	if got["members-only-1"] {
		t.Errorf("the search leaked a members-only document that lives in a PUBLIC project: %v", got)
	}
	if len(got) != 2 {
		t.Errorf("the search returned %d documents (%v), want exactly the two the actor may read", len(got), got)
	}

	// The same search, told to run under the composition this design rejects:
	// the actor's memberships PLUS every public project. If that ever became
	// the scope, members-only-1 would reach a searcher who is not in its
	// project — the leak is real, and this is the observed proof rather than
	// the argument for it. (On the frozen tree it does not happen, because
	// ResolveScope never asks for the public projects.)
	public, err := store.ListPublicProjects(ctx)
	if err != nil {
		t.Fatalf("ListPublicProjects: %v", err)
	}
	var union []pgtype.UUID
	union = append(union, scope.AllowedProjectUUIDs()...)
	for _, p := range public {
		var id pgtype.UUID
		if err := id.Scan(p.ID); err != nil {
			t.Fatalf("scan public project id %q: %v", p.ID, err)
		}
		union = append(union, id)
	}
	rows, err := q.SearchDocuments(ctx, sqlc.SearchDocumentsParams{
		Query: "needle", AllowedProjectIds: union, PageSize: 100, PageOffset: 0,
	})
	if err != nil {
		t.Fatalf("SearchDocuments under the union scope: %v", err)
	}
	leaked := false
	for _, r := range rows {
		if r.EntityRef == "members-only-1" {
			leaked = true
		}
	}
	if !leaked {
		t.Errorf("members-only-1 was NOT returned under a union scope naming every public project: "+
			"the read query has changed shape (it may have become stricter), so re-read the argument on search.ResolveScope "+
			"before keeping the scope membership-only. rows=%d", len(rows))
	}

	// Anonymous: no actor, no scope, no search. The refusal comes from the
	// resolution layer, with the real store in hand, before any row is read.
	if _, err := search.ResolveScope(ctx, store, ""); !errors.Is(err, search.ErrNoActor) {
		t.Errorf("ResolveScope with no actor: err = %v, want ErrNoActor", err)
	}
	// And the planner refuses the same state, so a question cannot be planned
	// for nobody even if a caller reaches planning first (owner ruling:
	// anonymous search requires a login).
	prov := plannertest.Reply(`{}`)
	p, err := planner.New(planner.Deps{Provider: prov})
	if err != nil {
		t.Fatalf("planner.New: %v", err)
	}
	var unresolved search.Scope
	if _, err := p.Plan(ctx, unresolved, planner.Request{Query: "needle"}); !errors.Is(err, search.ErrNoActor) {
		t.Errorf("planner.Plan without a scope: err = %v, want ErrNoActor", err)
	}
	if prov.Calls() != 0 {
		t.Errorf("the provider was asked %d times for an unauthenticated search, want 0", prov.Calls())
	}

	// The degraded path stays usable: when the planner falls back, the caller
	// runs the structured query on the question as typed, under the SAME
	// scope (docs/27 §SLO: "超时提供 structured results fallback"). The
	// fallback costs the answer, never the results, and never the filter.
	failing := plannertest.Failing(errors.New("provider unreachable"))
	fp, err := planner.New(planner.Deps{Provider: failing})
	if err != nil {
		t.Fatalf("planner.New: %v", err)
	}
	fallback, err := fp.Plan(ctx, scope, planner.Request{Query: "needle"})
	if err != nil {
		t.Fatalf("Plan on a failing provider: %v", err)
	}
	if fallback.Planned() || fallback.Query != "needle" {
		t.Fatalf("fallback = %+v, want the question back with no document", fallback)
	}
	afterFallback := runSearch(fallback.Query)
	if !afterFallback["pub-1"] || !afterFallback["mine-1"] || len(afterFallback) != 2 {
		t.Errorf("the structured path after a planner fallback returned %v, want the same two readable documents", afterFallback)
	}

	// What a NATURAL-LANGUAGE question does to this query, recorded rather than
	// asserted: 'simple' is the FTS configuration, and plainto_tsquery ANDs
	// every token, so the raw question matches nothing the documents do not
	// literally contain. The planner's whole job is to turn that question into
	// structured filters (docs/14 §2), and its fallback path hands the raw
	// question on — so the fallback's usefulness depends on the retrieval
	// layer, which is T0904's. See notes_for_supervisor.
	t.Logf("the same search run on the raw question %q returned %d rows (planner fallback passes the question through unchanged)",
		"What CO2 uptake has been reported for Mg-MOF-74?", len(runSearch("What CO2 uptake has been reported for Mg-MOF-74?")))
}

// uuidText renders a uuid the way search.Scope does, so the two are
// comparable as text.
func uuidText(t *testing.T, id pgtype.UUID) string {
	t.Helper()
	text, err := id.Value()
	if err != nil {
		t.Fatalf("render uuid: %v", err)
	}
	s, ok := text.(string)
	if !ok {
		t.Fatalf("uuid rendered as %T, want string", text)
	}
	return s
}
