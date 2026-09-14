package provenancehttp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/rsg/provenance"
)

// ProjectionStore reads the provenance projection (migration 00043) and the
// object/version rows behind it, with explicit SQL over pgx (docs/52: the
// graph traversal reads the projection; the labels come resolved from the
// rebuild). It lives in this package because T0505's allowed scope excludes
// internal/persistence — the adapter travels with the transport (L1,
// recorded in the task result).
type ProjectionStore struct {
	pool *pgxpool.Pool
}

// NewProjectionStore wires the adapter.
func NewProjectionStore(pool *pgxpool.Pool) *ProjectionStore {
	return &ProjectionStore{pool: pool}
}

// listEdgesSQL reads the projected edge set of one project, oldest first
// (the deterministic walk order's seed; the walk re-sorts its result).
const listEdgesSQL = `
SELECT relation_id, relation_version_id, relation_type,
       source_object_id, source_object_version_id, source_object_type,
       source_title, source_version_no,
       target_object_id, target_object_version_id, target_object_type,
       target_title, target_version_no
FROM provenance_edges
WHERE project_id = $1::uuid
ORDER BY created_at, relation_id`

// ListEdges implements Store: every projected provenance edge of the
// project. An unparseable project id answers ErrValidation — the gate runs
// first in the handlers, so this is a backstop, not the normal path.
func (s *ProjectionStore) ListEdges(ctx context.Context, projectID string) ([]provenance.Edge, error) {
	if !isUUID(projectID) {
		return nil, fmt.Errorf("%w: project_id is malformed", ErrValidation)
	}
	rows, err := s.pool.Query(ctx, listEdgesSQL, projectID)
	if err != nil {
		return nil, fmt.Errorf("provenance: list projected edges: %w", err)
	}
	defer rows.Close()
	var edges []provenance.Edge
	for rows.Next() {
		var e provenance.Edge
		if err := rows.Scan(
			&e.RelationID, &e.RelationVersionID, &e.RelationType,
			&e.Source.ObjectID, &e.Source.VersionID, &e.Source.ObjectType,
			&e.Source.Title, &e.Source.VersionNo,
			&e.Target.ObjectID, &e.Target.VersionID, &e.Target.ObjectType,
			&e.Target.Title, &e.Target.VersionNo,
		); err != nil {
			return nil, fmt.Errorf("provenance: scan projected edge: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provenance: read projected edges: %w", err)
	}
	return edges, nil
}

// ObjectStart implements Store. The object is resolved first, its
// membership in the path project is checked second, and only then is the
// version resolved: a foreign object under any project prefix answers
// ErrObjectNotFound — the same outcome a nonexistent object gets — for
// every versionNo, so the error codes never disclose that the object
// exists elsewhere or how many versions it has (docs/45: no existence or
// version-count oracle). The two lookups stay separate so an unknown
// object and an unknown version answer their own sentinels (the rsg
// surface's outcomes, reused so the wire codes stay one vocabulary).
func (s *ProjectionStore) ObjectStart(ctx context.Context, objectID, projectID string, versionNo *int) (ObjectStart, error) {
	if !isUUID(objectID) {
		return ObjectStart{}, sciobjects.ErrObjectNotFound
	}
	var start ObjectStart
	var objectProjectID string
	if err := s.pool.QueryRow(ctx, `
		SELECT project_id, id, object_type
		FROM scientific_objects WHERE id = $1::uuid`,
		objectID).Scan(&objectProjectID, &start.Node.ObjectID, &start.Node.ObjectType); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ObjectStart{}, sciobjects.ErrObjectNotFound
		}
		return ObjectStart{}, fmt.Errorf("provenance: read object: %w", err)
	}
	// The path project scopes the object, and this check runs BEFORE the
	// version lookup: a foreign object answers the object 404 whether or
	// not versionNo pins a version — the version-pinned axis cannot leak
	// existence either (docs/45). The comparison is casing-insensitive:
	// the row yields the DB-canonical lowercase uuid while the path value
	// may spell the hex in any case the uuid grammar allows — the gate and
	// every other read surface resolve path ids through uuid casts, which
	// are case-insensitive, so membership must not depend on the caller's
	// spelling (EqualFold over two same-shape uuids is exactly the uuid
	// cast's case-insensitive equality).
	if !strings.EqualFold(objectProjectID, projectID) {
		return ObjectStart{}, sciobjects.ErrObjectNotFound
	}
	if err := s.pool.QueryRow(ctx, `
		SELECT id, version_no, title
		FROM scientific_object_versions
		WHERE object_id = $1::uuid AND ($2::int IS NULL OR version_no = $2::int)
		ORDER BY version_no DESC
		LIMIT 1`,
		objectID, versionNo).Scan(
		&start.Node.VersionID, &start.Node.VersionNo, &start.Node.Title); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ObjectStart{}, sciobjects.ErrVersionNotFound
		}
		return ObjectStart{}, fmt.Errorf("provenance: read object version: %w", err)
	}
	return start, nil
}

// isUUID rejects anything that cannot be a uuid column value before it
// reaches PostgreSQL (the same shape persistence stores use).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
				return false
			}
		}
	}
	return true
}
