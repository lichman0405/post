package persistence

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// RSGQueryStore is the production rsg.QueryPort adapter over PostgreSQL
// (T0209): the state-lineage walk, the as-of version selections and the
// traversal's adjacency reads. All shapes are rebuildable from the
// canonical append-only history — this store reads only, never writes.
type RSGQueryStore struct {
	pool *pgxpool.Pool
}

// NewRSGQueryStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewRSGQueryStore(pool *pgxpool.Pool) *RSGQueryStore {
	return &RSGQueryStore{pool: pool}
}

// lineageSQL is the recursive-CTE state ancestry walk (CLAUDE.md §7: RSG
// graph relations run on the relation tables / recursive CTE). It is a raw
// pgx query, not a sqlc one, because the sqlc analyzer (v1.31.1) cannot
// resolve a recursive CTE's self-reference and rejects this valid
// PostgreSQL with "column reference id is ambiguous". The walk is
// project-verified at the seed AND at every parent hop: a missing state
// and a state of another project both yield an empty lineage, so the
// caller reports the same "not found" outcome for them (never leak
// another project's state existence, docs/45).
const lineageSQL = `
WITH RECURSIVE lineage AS (
  SELECT id AS state_id FROM project_states
  WHERE id = $1 AND project_id = $2
  UNION ALL
  SELECT ps.parent_state_id AS state_id
  FROM project_states ps, lineage l
  WHERE ps.id = l.state_id
    AND ps.parent_state_id IS NOT NULL
    AND ps.project_id = $2
)
SELECT state_id FROM lineage`

// ListStateLineage implements rsg.QueryPort.
func (s *RSGQueryStore) ListStateLineage(ctx context.Context, projectID, stateID string) ([]string, error) {
	stateUUID, err := textUUID(stateID)
	if err != nil {
		return nil, nil // unparseable ids name no state; empty lineage, same as missing
	}
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list state lineage: %w", err)
	}
	rows, err := s.pool.Query(ctx, lineageSQL, stateUUID, projectUUID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list state lineage: %w", err)
	}
	defer rows.Close()
	var lineage []string
	for rows.Next() {
		var id pgtype.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("persistence: list state lineage: %w", err)
		}
		lineage = append(lineage, pgUUIDToText(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("persistence: list state lineage: %w", err)
	}
	return lineage, nil
}

// ListObjectVersions implements rsg.QueryPort: one row per object the
// project's STATE LINEAGE carries, at its as-of version (the newest version
// whose state is in the lineage; nil lineage = the newest version among the
// project's states). The container the version hangs on is reported as it is
// — after an external fork's merge it is the contributor's (ADR-027).
func (s *RSGQueryStore) ListObjectVersions(ctx context.Context, projectID string, objectTypes, lineage []string) ([]rsg.ObjectQueryRow, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list object versions: %w", err)
	}
	// nil = every type (the port contract); an empty slice must mean the
	// same, never "a filter that matches nothing".
	if len(objectTypes) == 0 {
		objectTypes = nil
	}
	rows, err := sqlc.New(s.pool).ListObjectVersionsAsOf(ctx, sqlc.ListObjectVersionsAsOfParams{
		ProjectID:   projectUUID,
		ObjectTypes: objectTypes,
		Lineage:     textUUIDs(lineage),
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list object versions: %w", err)
	}
	out := make([]rsg.ObjectQueryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, rsg.ObjectQueryRow{
			Object: domain.ScientificObject{
				ID:               pgUUIDToText(row.ID),
				ProjectID:        pgUUIDToText(row.ProjectID),
				ObjectType:       row.ObjectType,
				CurrentVersionNo: int(row.CurrentVersionNo),
				CreatedBy:        pgUUIDToText(row.CreatedBy),
				CreatedAt:        row.CreatedAt.Time,
			},
			Version: domain.ScientificObjectVersion{
				ID:                 pgUUIDToText(row.ID_2),
				ObjectID:           pgUUIDToText(row.ObjectID),
				VersionNo:          int(row.VersionNo),
				StateID:            pgUUIDToText(row.StateID),
				BranchID:           pgUUIDTextPtr(row.BranchID),
				SchemaID:           row.SchemaID,
				SchemaVersion:      row.SchemaVersion,
				Title:              row.Title,
				LifecycleState:     domain.LifecycleState(row.LifecycleState),
				Payload:            row.Payload,
				VisibilityPolicyID: pgUUIDTextPtr(row.VisibilityPolicyID),
				IntegrityHash:      row.IntegrityHash,
				CreatedBy:          pgUUIDToText(row.CreatedBy_2),
				CreatedAt:          row.CreatedAt_2.Time,
			},
		})
	}
	return out, nil
}

// ListRelationVersions implements rsg.QueryPort: one row per relation the
// project's state lineage carries, at its as-of version, with both endpoints'
// object context — the containers the pinned endpoint versions hang on AND the
// projects whose states carry those pins (EndpointContext.CarriedBy), because
// the caller authorizes a seed edge by its pins' carriers, not by the
// containers (ADR-027: a landed version's container is the contributor's).
func (s *RSGQueryStore) ListRelationVersions(ctx context.Context, projectID string, relationTypes, lineage []string) ([]rsg.RelationQueryRow, error) {
	projectUUID, err := textUUID(projectID)
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions: %w", err)
	}
	// Same nil-not-empty contract as ListObjectVersions.
	if len(relationTypes) == 0 {
		relationTypes = nil
	}
	rows, err := sqlc.New(s.pool).ListRelationVersionsAsOf(ctx, sqlc.ListRelationVersionsAsOfParams{
		ProjectID:     projectUUID,
		RelationTypes: relationTypes,
		Lineage:       textUUIDs(lineage),
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list relation versions: %w", err)
	}
	out := make([]rsg.RelationQueryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, rsg.RelationQueryRow{
			Relation: domain.Relation{
				ID:        pgUUIDToText(row.RelationID),
				ProjectID: pgUUIDToText(row.RelationProjectID),
				CreatedAt: row.CreatedAt.Time,
			},
			Version: domain.RelationVersion{
				ID:                    pgUUIDToText(row.RelationVersionID),
				RelationID:            pgUUIDToText(row.RelationID),
				VersionNo:             int(row.VersionNo),
				StateID:               pgUUIDToText(row.StateID),
				RelationType:          row.RelationType,
				SourceObjectVersionID: pgUUIDToText(row.SourceObjectVersionID),
				TargetObjectVersionID: pgUUIDToText(row.TargetObjectVersionID),
				Payload:               row.Payload,
				IntegrityHash:         row.IntegrityHash,
				CreatedBy:             pgUUIDToText(row.CreatedBy),
				CreatedAt:             row.CreatedAt.Time,
			},
			Source: rsg.EndpointContext{
				VersionID:  pgUUIDToText(row.SourceObjectVersionID),
				ObjectID:   pgUUIDToText(row.SourceObjectID),
				ObjectType: row.SourceObjectType,
				ProjectID:  pgUUIDToText(row.SourceProjectID),
				CarriedBy:  pgUUIDToText(row.SourceCarriedBy),
			},
			Target: rsg.EndpointContext{
				VersionID:  pgUUIDToText(row.TargetObjectVersionID),
				ObjectID:   pgUUIDToText(row.TargetObjectID),
				ObjectType: row.TargetObjectType,
				ProjectID:  pgUUIDToText(row.TargetProjectID),
				CarriedBy:  pgUUIDToText(row.TargetCarriedBy),
			},
		})
	}
	return out, nil
}

// ListAdjacentRelationVersions implements rsg.QueryPort: the as-of
// relation versions touching any of versionIDs, with both endpoints'
// object context. Not project-filtered (the service authorizes each hop).
func (s *RSGQueryStore) ListAdjacentRelationVersions(ctx context.Context, versionIDs, lineage []string) ([]rsg.AdjacentRelationRow, error) {
	rows, err := sqlc.New(s.pool).ListAdjacentRelationVersions(ctx, sqlc.ListAdjacentRelationVersionsParams{
		VersionIds: textUUIDs(versionIDs),
		Lineage:    textUUIDs(lineage),
	})
	if err != nil {
		return nil, fmt.Errorf("persistence: list adjacent relation versions: %w", err)
	}
	out := make([]rsg.AdjacentRelationRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, rsg.AdjacentRelationRow{
			Relation: domain.Relation{
				ID:        pgUUIDToText(row.RelationID),
				ProjectID: pgUUIDToText(row.RelationProjectID),
				CreatedAt: row.CreatedAt.Time,
			},
			Version: domain.RelationVersion{
				ID:                    pgUUIDToText(row.RelationVersionID),
				RelationID:            pgUUIDToText(row.RelationID),
				VersionNo:             int(row.VersionNo),
				StateID:               pgUUIDToText(row.StateID),
				RelationType:          row.RelationType,
				SourceObjectVersionID: pgUUIDToText(row.SourceObjectVersionID),
				TargetObjectVersionID: pgUUIDToText(row.TargetObjectVersionID),
				Payload:               row.Payload,
				IntegrityHash:         row.IntegrityHash,
				CreatedBy:             pgUUIDToText(row.CreatedBy),
				CreatedAt:             row.CreatedAt.Time,
			},
			Source: rsg.EndpointContext{
				VersionID:  pgUUIDToText(row.SourceObjectVersionID),
				ObjectID:   pgUUIDToText(row.SourceObjectID),
				ObjectType: row.SourceObjectType,
				ProjectID:  pgUUIDToText(row.SourceProjectID),
			},
			Target: rsg.EndpointContext{
				VersionID:  pgUUIDToText(row.TargetObjectVersionID),
				ObjectID:   pgUUIDToText(row.TargetObjectID),
				ObjectType: row.TargetObjectType,
				ProjectID:  pgUUIDToText(row.TargetProjectID),
			},
		})
	}
	return out, nil
}

// ListObjectVersionsByIDs implements rsg.QueryPort: batch fetch of pinned
// object + version rows. Unfiltered by project (the caller passes only
// version ids whose projects passed the per-hop authorization).
func (s *RSGQueryStore) ListObjectVersionsByIDs(ctx context.Context, versionIDs []string) ([]rsg.ObjectQueryRow, error) {
	rows, err := sqlc.New(s.pool).ListObjectVersionsByIDs(ctx, textUUIDs(versionIDs))
	if err != nil {
		return nil, fmt.Errorf("persistence: list object versions by ids: %w", err)
	}
	out := make([]rsg.ObjectQueryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, rsg.ObjectQueryRow{
			Object: domain.ScientificObject{
				ID:               pgUUIDToText(row.ID),
				ProjectID:        pgUUIDToText(row.ProjectID),
				ObjectType:       row.ObjectType,
				CurrentVersionNo: int(row.CurrentVersionNo),
				CreatedBy:        pgUUIDToText(row.CreatedBy),
				CreatedAt:        row.CreatedAt.Time,
			},
			Version: domain.ScientificObjectVersion{
				ID:                 pgUUIDToText(row.ID_2),
				ObjectID:           pgUUIDToText(row.ObjectID),
				VersionNo:          int(row.VersionNo),
				StateID:            pgUUIDToText(row.StateID),
				BranchID:           pgUUIDTextPtr(row.BranchID),
				SchemaID:           row.SchemaID,
				SchemaVersion:      row.SchemaVersion,
				Title:              row.Title,
				LifecycleState:     domain.LifecycleState(row.LifecycleState),
				Payload:            row.Payload,
				VisibilityPolicyID: pgUUIDTextPtr(row.VisibilityPolicyID),
				IntegrityHash:      row.IntegrityHash,
				CreatedBy:          pgUUIDToText(row.CreatedBy_2),
				CreatedAt:          row.CreatedAt_2.Time,
			},
		})
	}
	return out, nil
}

// textUUIDs converts a text uuid list (nil and empty stay nil — the
// lineage queries treat NULL as "no pinning", never as "empty set").
func textUUIDs(ids []string) []pgtype.UUID {
	if len(ids) == 0 {
		return nil
	}
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := textUUID(id)
		if err != nil {
			continue // unparseable ids can never match a row
		}
		out = append(out, u)
	}
	return out
}
