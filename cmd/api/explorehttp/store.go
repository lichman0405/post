package explorehttp

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/explore"
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
//   - listPublishedKnowledge never joins projects. A publication may come
//     from a private project (docs/12 §2 lets a private project publish
//     Knowledge), and the index names the publishing project only when that
//     project is public — which the model decides from the public project
//     set. A private project's name, slug and visibility therefore never
//     leave the database on this path: not "read and withheld", never
//     selected.
//   - listPublicPeople never reads users.email. The directory renders
//     handle, display name and bio; email is identity, not a profile field
//     (internal/application/profile).
//   - Neither list renders a count of anything.
//
// # Why the WHERE clauses exist although the model re-checks
//
// Each section's row type carries the lifecycle fact its filter uses
// (KnowledgeRow.PublicationID, PersonRow.DisabledAt,
// OrganizationRow.DeactivatedAt) and BuildIndex re-reads it, so a store
// regression renders a shorter index rather than a leak. The predicates here
// are the primary filter: they keep the LIMIT honest (a deactivated row
// consuming one of the twenty slots would silently shrink the section).
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
// reading more than it renders would only widen the read for no answer.
const knowledgeQuery = `
SELECT kp.id::text,
       so.id::text,
       so.object_type,
       kp.public_version,
       sov.title,
       so.project_id::text,
       kp.published_at,
       sov.lifecycle_state
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
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
		if err := rows.Scan(
			&row.PublicationID,
			&row.ObjectID,
			&row.ObjectType,
			&row.PublicVersion,
			&row.Title,
			&row.ProjectID,
			&row.PublishedAt,
			&row.LifecycleState,
		); err != nil {
			return nil, fmt.Errorf("explore: scan published knowledge: %w", err)
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
