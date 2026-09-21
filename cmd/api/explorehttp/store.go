package explorehttp

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/explore"
	"github.com/lichman0405/post/internal/rights"
)

// Store is the PostgreSQL adapter for the three Explore sections that had
// no read before this task: knowledge, people, organizations (explicit SQL
// over pgx, docs/52). It lives in this package because T0802's allowed
// scope excludes internal/persistence/** — the adapter travels with the
// transport, exactly as T0505's ProjectionStore does in provenancehttp
// (L1, recorded in the task result).
//
// It implements HALF of explore.Reader; the other half is the platform's
// existing public reads, adapted in adapters.go. Nothing here writes: the
// Explore index is a read, and every query is a SELECT with a LIMIT.
//
// # What the queries do NOT read
//
//   - listPublishedKnowledge never reads a private project's name or slug.
//     A publication may come from a private project (docs/12 §2 lets a
//     private project publish Knowledge), and the index names the
//     publishing project only when that project is public — which the model
//     decides from the public project set. What the query DOES read is
//     projects.visibility, as one of the three inputs of the audience rule
//     (knowledgepublish.AudienceFor): a publication exists from the moment
//     someone publishes it, and whether the network may see it is a separate
//     question the version's OWN axis answers. Reading the preset is what
//     lets that question be answered at all on an anonymous surface; the
//     project's identity still does not leave the database. Since T1107 the
//     preset is also a WHERE predicate, so a private project's publication
//     does not consume one of the twenty slots either — see knowledgeQuery.
//   - listPublicPeople never reads users.email. The directory renders
//     handle, display name and bio; email is identity, not a profile field
//     (internal/application/profile).
//   - No list renders a count of anything.
//
// # Why the WHERE clauses exist although the model re-checks
//
// Each section's row type carries the lifecycle fact its filter uses
// (KnowledgeRow.PublicationID, PersonRow.DisabledAt,
// OrganizationRow.DeactivatedAt) and BuildIndex re-reads it, so a store
// regression renders a shorter index rather than a leak. The predicates here
// are the primary filter: they keep the LIMIT honest (a row the surface will
// not render consuming one of the twenty slots would silently shrink the
// section, and — before T1107's fix to knowledgeQuery — the shape of that
// shrinkage was itself an existence oracle).
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds the store on pool. The pool may be lazy
// (persistence.OpenLazy): the API keeps starting while PostgreSQL is down.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// knowledgeQuery reads the published knowledge object versions, newest
// publication first.
//
// The LIMIT is explore.SectionLimit: the section renders the newest twenty, and
// reading more than it renders would only widen the read for no answer. That
// sentence is the reason for the two predicates below, and until T1107 it was
// also a claim this query did not honour: the LIMIT was taken over the whole
// corpus — every private project's publications included — and the audience
// rule was applied afterwards, in Go. A window read raw is a window a writer
// can spend (internal/persistence/queries/feeds.sql:8-27 argues the same
// point at length for the feed reads), and here it is worse than the
// suppression that file describes: a reader who can enumerate the network's
// publications through other public reads (GET /api/v1/knowledge/{pid}) sees
// this section come back SHORT of the public corpus, and the shortfall is an
// existence oracle for publications the reader may not see. docs/54's
// scenario #1 is "a private project's content showing up in
// Search/Explore/API error"; a count that shows up as an absence is the same
// disclosure with the sign flipped, and docs/23 §5 forbids it in the same
// breath as a rendered count.
//
// # What is filtered here and what is still decided in Go
//
// Two of the audience rule's three inputs are predicates SQL can spell
// EXACTLY, and they are spelled here — the same pair, for the same reason,
// that feeds.sql:198-199 applies to the feed's knowledge half:
//
//	p.visibility = 'public'               -- axis 2: the owning project's preset
//	sov.visibility_policy_id IS NULL      -- axis 1: a version that pins a
//	                                      -- policy of its own is governed by
//	                                      -- that policy, and this build
//	                                      -- resolves no policy into a public
//	                                      -- grant
//
// The third input is the published rights declaration's metadata token, and
// it is NOT spellable here: the rule is "the token is exactly
// rights.MetadataProjectPolicy, and a document this build cannot READ states
// no token at all" — a Go parse, not a JSON predicate. Its column is still
// read RAW (rights_json) and still travels to
// explore.KnowledgeRow.Published(), which applies knowledgepublish.AudienceFor
// — the same function the publication's own page applies. So the disclosure
// rule has exactly one implementation and this query is not it; the query
// only decides WHICH TWENTY rows that rule gets to see, and it is now the
// twenty most recent rows this surface could render rather than the twenty
// most recent rows in the table.
//
// The consequence is the one feeds.sql names for the same arrangement: the
// window is EXACT for the two axes above and a window over CANDIDATES for the
// rights axis, so a public project whose newest knowledge rows all carry a
// non-network rights declaration can still spend part of the window on rows
// the index does not render, and the section may be SHORTER than
// explore.SectionLimit. It can never be longer and never wrong: the model
// renders exactly the rows it decides are network-visible. Closing that last
// gap by approximating the rights parse in SQL would be a second, weaker copy
// of the rule — the defect feeds.sql refuses by name.
//
// A publication whose version pins a visibility policy of its own, or whose
// project is private, therefore never reaches the model at all, while still
// being a publication the project's own members read — which is exactly the
// 发布不等于公开 ruling (L3-20260916-1 #1).
const knowledgeQuery = `
SELECT kp.id::text,
       so.id::text,
       so.object_type,
       kp.public_version,
       sov.title,
       so.project_id::text,
       kp.published_at,
       sov.lifecycle_state,
       sov.visibility_policy_id::text,
       p.visibility,
       kp.rights_json
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE p.visibility = 'public'
  AND sov.visibility_policy_id IS NULL
ORDER BY kp.published_at DESC, kp.id
LIMIT $1`

// ListPublishedKnowledge returns the published knowledge object versions.
func (s *Store) ListPublishedKnowledge(ctx context.Context) ([]explore.KnowledgeRow, error) {
	rows, err := s.pool.Query(ctx, knowledgeQuery, explore.SectionLimit)
	if err != nil {
		return nil, fmt.Errorf("explore: list published knowledge: %w", err)
	}
	defer rows.Close()

	out := make([]explore.KnowledgeRow, 0, explore.SectionLimit)
	for rows.Next() {
		var row explore.KnowledgeRow
		var (
			policyID   *string
			projectVis string
			rightsJSON []byte
		)
		if err := rows.Scan(
			&row.PublicationID,
			&row.ObjectID,
			&row.ObjectType,
			&row.PublicVersion,
			&row.Title,
			&row.ProjectID,
			&row.PublishedAt,
			&row.LifecycleState,
			&policyID,
			&projectVis,
			&rightsJSON,
		); err != nil {
			return nil, fmt.Errorf("explore: scan published knowledge: %w", err)
		}
		row.VisibilityPolicyID = policyID
		row.ProjectVisibility = projectVis
		// An unreadable declaration is carried as unreadable rather than as
		// the zero document: the zero document's metadata axis is the empty
		// string, which AudienceFor refuses — but only because "" is not the
		// project-policy token. Saying so explicitly keeps "we could not read
		// this" a fact the model holds instead of an accident of zero values.
		if doc, err := rights.Parse(rightsJSON); err == nil {
			row.Rights = doc
			row.RightsValid = true
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explore: read published knowledge: %w", err)
	}
	return out, nil
}

// peopleQuery reads the accounts of the network, newest account first.
//
// The profile join is LEFT, exactly as internal/persistence's profileSelect
// joins it: a user without a profile row is still a person of this network
// (migration 00017 backfilled every existing row, but a row that predates or
// bypasses the backfill must not silently vanish from the directory — the
// public profile read serves it too), and the bio simply renders empty.
//
// WHERE u.disabled_at IS NULL is the account's own state: a disabled
// account is not part of the directory (the same fail-closed reading of
// "active only" as the organizations section's deactivated_at).
const peopleQuery = `
SELECT u.id::text,
       u.handle,
       u.display_name,
       coalesce(p.bio, ''),
       u.created_at,
       u.disabled_at
FROM users u
LEFT JOIN profiles p ON p.user_id = u.id
WHERE u.disabled_at IS NULL
ORDER BY u.created_at DESC, u.id
LIMIT $1`

// ListPublicPeople returns the network's accounts (see peopleQuery for what
// "account" means here and what the read deliberately never selects).
func (s *Store) ListPublicPeople(ctx context.Context) ([]explore.PersonRow, error) {
	rows, err := s.pool.Query(ctx, peopleQuery, explore.SectionLimit)
	if err != nil {
		return nil, fmt.Errorf("explore: list people: %w", err)
	}
	defer rows.Close()

	out := make([]explore.PersonRow, 0, explore.SectionLimit)
	for rows.Next() {
		var (
			row        explore.PersonRow
			disabledAt pgtype.Timestamptz
		)
		if err := rows.Scan(
			&row.ID,
			&row.Handle,
			&row.DisplayName,
			&row.Bio,
			&row.CreatedAt,
			&disabledAt,
		); err != nil {
			return nil, fmt.Errorf("explore: scan person: %w", err)
		}
		row.DisabledAt = timestamptzPtr(disabledAt)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explore: read people: %w", err)
	}
	return out, nil
}

// organizationsQuery reads the network's organizations, newest first.
//
// WHERE o.deactivated_at IS NULL: migration 00018 makes deactivation the
// only "delete" the domain offers (CLAUDE.md §9.8), and a deactivated
// organization is not a network identity a directory should still offer —
// its row stays as history, its listing does not.
const organizationsQuery = `
SELECT o.id::text,
       o.slug,
       o.name,
       coalesce(o.description, ''),
       o.created_at,
       o.deactivated_at
FROM organizations o
WHERE o.deactivated_at IS NULL
ORDER BY o.created_at DESC, o.id
LIMIT $1`

// ListPublicOrganizations returns the active organizations of the network.
func (s *Store) ListPublicOrganizations(ctx context.Context) ([]explore.OrganizationRow, error) {
	rows, err := s.pool.Query(ctx, organizationsQuery, explore.SectionLimit)
	if err != nil {
		return nil, fmt.Errorf("explore: list organizations: %w", err)
	}
	defer rows.Close()

	out := make([]explore.OrganizationRow, 0, explore.SectionLimit)
	for rows.Next() {
		var (
			row           explore.OrganizationRow
			deactivatedAt pgtype.Timestamptz
		)
		if err := rows.Scan(
			&row.ID,
			&row.Slug,
			&row.Name,
			&row.Description,
			&row.CreatedAt,
			&deactivatedAt,
		); err != nil {
			return nil, fmt.Errorf("explore: scan organization: %w", err)
		}
		row.DeactivatedAt = timestamptzPtr(deactivatedAt)
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("explore: read organizations: %w", err)
	}
	return out, nil
}

// timestamptzPtr converts a nullable timestamptz to *time.Time.
func timestamptzPtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
