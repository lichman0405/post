package search

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// The scope's unit tests. The database-level half of the same property — that
// the canonical read query, run under a scope this file produced, returns no
// row the actor may not see — is an integration test
// (tests/integration/search_scope_test.go); nothing here can prove that
// without a database, and nothing there can prove the fail-closed rules
// below without a store that misbehaves.

const (
	scopeUser     = "11111111-1111-4111-8111-111111111111"
	scopeMine     = "22222222-2222-4222-8222-222222222222"
	scopeMineToo  = "33333333-3333-4333-8333-333333333333"
	scopeNotUUID  = "not-a-uuid"
	scopeMissing  = "44444444-4444-4444-8444-444444444444"
	scopeNoMember = "55555555-5555-4555-8555-555555555555"
)

// reader is the smallest thing that satisfies ProjectScopeReader, with the
// two misbehaviours a fake store can have (an error, and ids that are not
// uuids).
type reader struct {
	projects []domain.Project
	err      error
	calls    int
}

func (r *reader) ListProjectsForUser(context.Context, string) ([]domain.Project, error) {
	r.calls++
	return r.projects, r.err
}

// TestResolveScopeRefusesWithoutActor: no actor is no scope. The refusal must
// happen BEFORE the store is consulted, and it must be an error — resolving
// an empty scope instead would be the fail-open shape ("nobody, so show the
// public rows") that the owner ruling on anonymous search rejects.
func TestResolveScopeRefusesWithoutActor(t *testing.T) {
	r := &reader{}
	if _, err := ResolveScope(context.Background(), r, ""); !errors.Is(err, ErrNoActor) {
		t.Fatalf("err = %v, want ErrNoActor", err)
	}
	if r.calls != 0 {
		t.Errorf("the store was consulted %d times for an actor-less scope, want 0", r.calls)
	}

	// The zero Scope is the same state, and the planner reads it through
	// Authenticated.
	var zero Scope
	if zero.Authenticated() {
		t.Error("the zero Scope reports itself authenticated")
	}
	if got := zero.AllowedProjectIDs(); len(got) != 0 {
		t.Errorf("the zero Scope carries projects: %v", got)
	}
}

// TestResolveScopeFailsClosedOnUnreadableStore: a store failure must not be
// smoothed into an empty scope. An empty scope would silently downgrade a
// member to public-only rows, and the caller could not tell the difference
// between that and a user who belongs to nothing.
func TestResolveScopeFailsClosedOnUnreadableStore(t *testing.T) {
	sentinel := errors.New("connection reset")
	r := &reader{err: sentinel}
	scope, err := ResolveScope(context.Background(), r, scopeUser)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the store error", err)
	}
	if scope.Authenticated() || len(scope.AllowedProjectIDs()) != 0 {
		t.Errorf("an unreadable store produced a usable scope: %+v", scope)
	}
}

// TestResolveScopeCarriesTheActorsMemberships: the happy path is the whole
// technical delivery — an actor becomes the query's parameter — so it asserts
// the exact set, its order, and that the two accessors (text and pgtype)
// describe the same projects.
func TestResolveScopeCarriesTheActorsMemberships(t *testing.T) {
	r := &reader{projects: []domain.Project{
		{ID: scopeMine},
		{ID: scopeMineToo},
		// A duplicate row (two membership rows on one project) must not
		// change the scope: it is a set of projects.
		{ID: scopeMine},
	}}
	scope, err := ResolveScope(context.Background(), r, scopeUser)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if !scope.Authenticated() || scope.ActorID() != scopeUser {
		t.Fatalf("scope is not attributable to the actor: authenticated=%v actor=%q", scope.Authenticated(), scope.ActorID())
	}
	want := []string{scopeMine, scopeMineToo}
	if got := scope.AllowedProjectIDs(); !reflect.DeepEqual(got, want) {
		t.Errorf("AllowedProjectIDs = %v, want %v", got, want)
	}
	uuids := scope.AllowedProjectUUIDs()
	if len(uuids) != len(want) {
		t.Fatalf("AllowedProjectUUIDs = %v, want %d ids", uuids, len(want))
	}
	for i, u := range uuids {
		text, err := u.Value()
		if err != nil {
			t.Fatalf("uuid %d: %v", i, err)
		}
		if text != want[i] {
			t.Errorf("uuid %d = %v, want %s", i, text, want[i])
		}
	}
}

// TestResolveScopeRejectsUnusableProjectIDs: a project id the platform cannot
// express as a scope key is an error, not a dropped entry. Dropping it would
// silently shrink a member's access — the quieter of the two possible wrong
// answers, and the one nobody would notice.
func TestResolveScopeRejectsUnusableProjectIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"not a uuid", scopeNotUUID},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &reader{projects: []domain.Project{{ID: scopeMine}, {ID: tc.id}}}
			scope, err := ResolveScope(context.Background(), r, scopeUser)
			if err == nil {
				t.Fatalf("ResolveScope accepted a project id %q: %+v", tc.id, scope)
			}
			if len(scope.AllowedProjectIDs()) != 0 {
				t.Errorf("a rejected scope still carries projects: %v", scope.AllowedProjectIDs())
			}
		})
	}
}

// TestResolveScopeIsNotInfluencedByThePublicProjects is the tripwire for the
// decision this file's doc comment argues: the public half of "what may this
// actor read" is the READ QUERY's business (search.sql's `visibility =
// 'public'`), and putting public projects into the scope would hand a
// members-only row inside a public project to any searcher.
//
// The property is structural — the port has one method — so the test pins the
// port's shape: adding a public-project read here is the change that leaks,
// and it has to be deliberate.
func TestResolveScopeIsNotInfluencedByThePublicProjects(t *testing.T) {
	iface := reflect.TypeOf((*ProjectScopeReader)(nil)).Elem()
	got := make([]string, 0, iface.NumMethod())
	for i := 0; i < iface.NumMethod(); i++ {
		got = append(got, iface.Method(i).Name)
	}
	want := []string{"ListProjectsForUser"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ProjectScopeReader methods = %v, want %v: the scope is the actor's MEMBERSHIPS, "+
			"and a public-project read here would widen it past what membership grants", got, want)
	}

	// And the resolved scope contains exactly what that one method returned,
	// whatever the deployment's public projects are: a store that knows about
	// a public project the actor is not in cannot express it here.
	r := &reader{projects: []domain.Project{{ID: scopeMine}}}
	scope, err := ResolveScope(context.Background(), r, scopeUser)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	if got := scope.AllowedProjectIDs(); !reflect.DeepEqual(got, []string{scopeMine}) {
		t.Errorf("AllowedProjectIDs = %v, want just the membership", got)
	}
}

// TestScopeAccessorsReturnCopies: a resolved scope is immutable, so a caller
// cannot widen its own authorization by writing into the slice it was handed.
func TestScopeAccessorsReturnCopies(t *testing.T) {
	r := &reader{projects: []domain.Project{{ID: scopeMine}}}
	scope, err := ResolveScope(context.Background(), r, scopeUser)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	ids := scope.AllowedProjectIDs()
	ids[0] = scopeNoMember
	if got := scope.AllowedProjectIDs(); got[0] != scopeMine {
		t.Errorf("AllowedProjectIDs handed out its backing array: %v", got)
	}
	uuids := scope.AllowedProjectUUIDs()
	if err := uuids[0].Scan(scopeNoMember); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := scope.AllowedProjectIDs(); got[0] != scopeMine {
		t.Errorf("AllowedProjectUUIDs handed out its backing array: %v", got)
	}
}
