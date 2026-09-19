package search

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/domain"
)

// This file is the one place an ACTOR becomes the project scope a search runs
// under (T0903). It exists because the read query's scope argument had no
// producer: SearchDocuments takes @allowed_project_ids
// (internal/persistence/queries/search.sql:36) and, before this, nothing in
// the tree ever built a value for it — a filter nobody applies is the same as
// no filter.
//
// The guarantee this layer does NOT provide, and the one it does:
//
//   - Authorization is enforced in the database, by the query's own predicate
//     `visibility = 'public' OR project_id = ANY(@allowed_project_ids)`
//     (docs/21 §9: every repository/service query takes a principal and
//     filters in the DB, never "hide it after the fact"; docs/23 §5: every
//     query/search/export/download is policy-filtered). Nothing in this file
//     decides what a row's visibility is, and nothing here filters rows after
//     they are read.
//
//   - What this file provides is the SCOPE ITSELF: the fail-closed derivation
//     of that parameter from an authenticated user, so a caller cannot invent
//     a scope at the call site. Scope's fields are unexported and ResolveScope
//     is the only constructor, so "a search with no principal" is not
//     representable — the zero Scope is what a caller holds BEFORE it has
//     resolved one, and it carries no projects at all.

// ErrNoActor is returned by ResolveScope when the caller has no authenticated
// user id.
//
// It is an error and not an empty scope on purpose. "No actor" has exactly
// one safe meaning — there is no scope, so there is no search — and the
// alternative reading ("no actor, so let them see the public rows") is the
// fail-open shape docs/12 forbids. The V1 contract agrees: specs/api's global
// `security: [{bearerAuth: []}]` applies to POST /search, which is not marked
// `security: []`, so an unauthenticated search is a 401 before it reaches
// this layer (owner ruling 2026-09-18: the docs/05 §1「登录前保留 Search」
// reading is recorded in tasks/decisions.md and is not this layer's to
// decide).
var ErrNoActor = errors.New("search: no actor")

// ProjectScopeReader reads the projects a user belongs to. It is satisfied
// structurally by *persistence.ProjectStore and by projects.ProjectStore's
// own test doubles, so a caller passes the project store it already holds
// rather than a search-specific adapter.
//
// The interface is deliberately ONE method. The public half of "what may this
// actor read" is not read here and must not be added to the scope — the
// reason is spelled out on ResolveScope, and it is a leak rather than a
// completeness gap.
type ProjectScopeReader interface {
	// ListProjectsForUser returns the projects the user belongs to (any
	// membership), most recently created first
	// (internal/persistence/queries/projects.sql, ListProjectsForUser).
	// An id that names no user, or no membership, yields an empty list.
	ListProjectsForUser(ctx context.Context, userID string) ([]domain.Project, error)
}

// Scope is the authorization context of one search: the authenticated actor
// and the projects whose rows that actor may read regardless of the row's own
// visibility.
//
// The zero Scope is "unresolved" — Authenticated reports false and the
// project list is empty — which is exactly the state a caller is in before
// ResolveScope returns, and the state the planner refuses to plan under.
type Scope struct {
	actorID    string
	projectIDs []pgtype.UUID
}

// ResolveScope builds the scope of an authenticated actor: the projects they
// are a member of, and nothing else.
//
// # Why the public projects are NOT part of this list
//
// The obvious-looking composition is "memberships plus every public project",
// and it leaks. search_documents.visibility is a CONJUNCTION of two axes —
// the entity's own and its project's (projection.go, projectedVisibility) —
// so a row in a PUBLIC project can still be private: an asset whose latest
// published version is private (sources.go, assetSource: "a public asset
// version in a private project is still not the network's", and the converse
// holds too), or a members-only knowledge publication inside a public project
// (feeds.sql:36-42 records that case as legal and common — a private project
// may publish, 发布不等于公开, docs/12 §2). Such a row carries
// visibility='private' AND project_id=<a public project>. Naming public
// projects in @allowed_project_ids therefore hands that row to any searcher,
// which is docs/54's #1 scenario ("Private Project/Branch 内容出现在
// Search") in its members-only form.
//
// The public half is already handled, one layer down and better: the read
// query's own predicate returns a row whenever `visibility = 'public'`,
// without consulting the scope at all (search.sql:21-31 — "Passing an empty
// array therefore yields public rows only — never the whole table"). Naming
// public projects here would add exactly zero rows and open the members-only
// hole; leaving them out keeps the scope equal to "projects this actor is
// inside", which is the only thing membership can grant.
//
// # Fail-closed rules
//
//   - An empty actorID is ErrNoActor: no actor, no scope, no search.
//   - A reader failure is returned, never swallowed into an empty scope. An
//     empty scope is not a safe substitute for an unreadable one: it silently
//     downgrades a member to "public only", and the caller cannot tell the
//     difference. The caller fails the request (the planner refuses — see
//     internal/search/planner).
//   - A project id that is not a uuid is an error. The store's ids are the
//     read query's own key type, so an id that cannot be one means the
//     membership cannot be expressed as a scope: dropping it would silently
//     shrink a member's access, and there is no safe way to guess what it
//     meant.
//   - A store that cannot parse the actor id reports no memberships
//     (persistence.ProjectStore.ListProjectsForUser), which resolves to the
//     public-only floor — a bogus id grants nothing.
func ResolveScope(ctx context.Context, reader ProjectScopeReader, actorID string) (Scope, error) {
	if actorID == "" {
		return Scope{}, ErrNoActor
	}
	if reader == nil {
		return Scope{}, fmt.Errorf("search: resolve scope for %s: %w", actorID, ErrNoActor)
	}
	projects, err := reader.ListProjectsForUser(ctx, actorID)
	if err != nil {
		return Scope{}, fmt.Errorf("search: resolve scope for %s: %w", actorID, err)
	}
	ids := make([]pgtype.UUID, 0, len(projects))
	seen := make(map[pgtype.UUID]bool, len(projects))
	for _, p := range projects {
		if p.ID == "" {
			return Scope{}, fmt.Errorf("search: resolve scope for %s: a membership has no project id", actorID)
		}
		var id pgtype.UUID
		if err := id.Scan(p.ID); err != nil {
			return Scope{}, fmt.Errorf("search: resolve scope for %s: project id %q: %w", actorID, p.ID, err)
		}
		// A duplicate would not change the query's answer, but it would make
		// the scope's text form depend on the store's row count; the scope is
		// a SET of projects.
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return Scope{actorID: actorID, projectIDs: ids}, nil
}

// ActorID returns the authenticated user this scope was resolved for ("" for
// the zero Scope). docs/26 §2 keeps the actor on the search's correlation
// trail; it is also what makes a search attributable.
func (s Scope) ActorID() string { return s.actorID }

// Authenticated reports whether this scope was resolved for an actor. The
// planner requires it: a Scope that has not been through ResolveScope is not
// a scope.
func (s Scope) Authenticated() bool { return s.actorID != "" }

// AllowedProjectUUIDs returns the projects the actor is a member of, as the
// pgtype form the sqlc query takes
// (sqlc.SearchDocumentsParams.AllowedProjectIds), in the order the reader
// returned them (newest project first). It is the value the retrieval layer
// passes, and it exists so that layer does not re-implement the conversion.
//
// The slice is a copy: a caller cannot add a project to a resolved scope.
func (s Scope) AllowedProjectUUIDs() []pgtype.UUID {
	out := make([]pgtype.UUID, len(s.projectIDs))
	copy(out, s.projectIDs)
	return out
}

// AllowedProjectIDs returns the same set as uuid text — the form the project
// ids carry everywhere else in the platform, and what a test or a log line
// can be compared against.
func (s Scope) AllowedProjectIDs() []string {
	out := make([]string, 0, len(s.projectIDs))
	for _, id := range s.projectIDs {
		b := id.Bytes
		out = append(out, fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
	}
	return out
}
