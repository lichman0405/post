package persistence

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// ManifestStore is the production manifests.SnapshotPort adapter over
// PostgreSQL: the lineage snapshot one manifest is built from. The
// lineage — the state plus every ancestor on the per-branch chain — is
// walked by the recursive CTEs in queries/manifest.sql; the rows come back
// unordered on purpose, because the canonical ordering of the manifest
// arrays is the manifest package's rule, not the store's.
type ManifestStore struct {
	pool *pgxpool.Pool
}

// NewManifestStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewManifestStore(pool *pgxpool.Pool) *ManifestStore {
	return &ManifestStore{pool: pool}
}

// GetManifestSnapshot implements manifests.SnapshotPort. An unknown state
// (a UUID that names no row) has an empty lineage: an empty snapshot, not
// an error (the same read discipline as the direct-member reads on
// StateStore — the caller's state read already reported not-found). A
// malformed id is an error, never a silent empty manifest: an empty
// snapshot would look like a valid export to any caller that skipped the
// state read.
func (s *ManifestStore) GetManifestSnapshot(ctx context.Context, stateID string) (manifest.Snapshot, error) {
	id, err := textUUID(stateID)
	if err != nil {
		return manifest.Snapshot{}, fmt.Errorf("persistence: invalid state id %q: %w", stateID, err)
	}
	q := sqlc.New(s.pool)
	snap := manifest.Snapshot{
		ObjectVersions:   []manifest.ObjectVersion{},
		RelationVersions: []manifest.RelationVersion{},
		BlobRefs:         []manifest.BlobRef{},
	}
	objRows, err := q.ListManifestObjectVersions(ctx, id)
	if err != nil {
		return manifest.Snapshot{}, fmt.Errorf("persistence: list manifest object versions: %w", err)
	}
	for _, row := range objRows {
		snap.ObjectVersions = append(snap.ObjectVersions, manifest.ObjectVersion{
			ID:                 pgUUIDToText(row.ID),
			ObjectID:           pgUUIDToText(row.ObjectID),
			ObjectType:         row.ObjectType,
			VersionNo:          int(row.VersionNo),
			StateID:            pgUUIDToText(row.StateID),
			BranchID:           uuidPtr(row.BranchID),
			SchemaRef:          manifest.SchemaRef{ID: row.SchemaID, Version: row.SchemaVersion},
			Title:              row.Title,
			LifecycleState:     row.LifecycleState,
			Payload:            json.RawMessage(row.Payload),
			VisibilityPolicyID: uuidPtr(row.VisibilityPolicyID),
			IntegrityHash:      row.IntegrityHash,
			CreatedBy:          pgUUIDToText(row.CreatedBy),
			CreatedAt:          row.CreatedAt.Time,
		})
	}
	relRows, err := q.ListManifestRelationVersions(ctx, id)
	if err != nil {
		return manifest.Snapshot{}, fmt.Errorf("persistence: list manifest relation versions: %w", err)
	}
	for _, row := range relRows {
		snap.RelationVersions = append(snap.RelationVersions, manifest.RelationVersion{
			ID:                    pgUUIDToText(row.ID),
			RelationID:            pgUUIDToText(row.RelationID),
			VersionNo:             int(row.VersionNo),
			StateID:               pgUUIDToText(row.StateID),
			RelationType:          row.RelationType,
			SourceObjectVersionID: pgUUIDToText(row.SourceObjectVersionID),
			TargetObjectVersionID: pgUUIDToText(row.TargetObjectVersionID),
			Payload:               json.RawMessage(row.Payload),
			IntegrityHash:         row.IntegrityHash,
			CreatedBy:             pgUUIDToText(row.CreatedBy),
			CreatedAt:             row.CreatedAt.Time,
		})
	}
	blobRows, err := q.ListManifestBlobRefs(ctx, id)
	if err != nil {
		return manifest.Snapshot{}, fmt.Errorf("persistence: list manifest blob refs: %w", err)
	}
	for _, row := range blobRows {
		snap.BlobRefs = append(snap.BlobRefs, manifest.BlobRef{
			ID:   pgUUIDToText(row.ID),
			Hash: row.ContentHash,
		})
	}
	return snap, nil
}
