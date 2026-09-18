package persistence

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// ProjectDependencyStore answers the project-side dependency read (T0707):
// which fixed asset versions one project depends on, over the canonical query
// of internal/persistence/queries/asset_dependencies.sql.
//
// It answers ONE question — "what does this project's recorded use look like,
// row by row?" — and it answers it RAW: private usages come back with the
// public ones, and the decision about what may be rendered belongs to
// internal/assets (BuildProjectDependencies), where the rule has unit tests
// that name the doc it comes from (docs/23 §5). The SQL file's header records
// the same split.
//
// Every method is a READ. Nothing here writes a row or takes a lock — the
// write half of the same table is the publish transaction's
// (AssetPublishStore, RecordAssetDependency).
type ProjectDependencyStore struct {
	queries *sqlc.Queries
}

// NewProjectDependencyStore wires the reader over any sqlc executor — the
// production value is the pgx pool cmd/api already builds.
func NewProjectDependencyStore(db sqlc.DBTX) *ProjectDependencyStore {
	return &ProjectDependencyStore{queries: sqlc.New(db)}
}

// ListProjectDependencies resolves one project's recorded dependencies,
// oldest first. An empty answer is a real answer: a project that has declared
// no usage (or whose asset versions pin nothing) has an empty list, never an
// error.
//
// A project id that is not a uuid text cannot have recorded anything — the
// column is a uuid and the foreign key makes every row name a real project —
// so this answers an empty list rather than an error, the same reading
// AssetPublishStore.LookupCreation takes for the same reason. The caller has
// already run the project read gate, so this can never be used to probe.
func (s *ProjectDependencyStore) ListProjectDependencies(ctx context.Context, projectID string) ([]assets.ProjectDependencyState, error) {
	id, err := textUUID(projectID)
	if err != nil {
		return nil, nil
	}
	rows, err := s.queries.ListProjectAssetUsages(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("persistence: list project asset usages: %w", err)
	}
	out := make([]assets.ProjectDependencyState, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.ProjectDependencyState{
			Pin:                      row.Pin,
			Title:                    row.Title,
			Type:                     assets.Type(row.AssetType),
			VersionVisibility:        assets.Visibility(row.VersionVisibility),
			VersionProjectVisibility: assets.Visibility(row.ProjectVisibility),
			DependencyType:           row.DependencyType,
			VisibilityOfUsage:        assets.Visibility(row.VisibilityOfUsage),
			CreatedAt:                row.CreatedAt.Time,
		})
	}
	return out, nil
}
