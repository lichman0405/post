package integration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/persistence/testdb"
)

// TestSQLCQueriesRoundTrip exercises the generated query layer end to end
// against real PostgreSQL: the same typed calls a future application layer
// will use (docs/68: pgx + sqlc).
func TestSQLCQueriesRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	pool, _ := testdb.Setup(t, ctx, adminURL(t), taskID)
	q := sqlc.New(pool)

	user, err := q.CreateUser(ctx, sqlc.CreateUserParams{
		Handle:      "alice",
		DisplayName: "Alice Researcher",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if user.ID.Valid == false {
		t.Fatal("CreateUser did not return an id")
	}

	byHandle, err := q.GetUserByHandle(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByHandle: %v", err)
	}
	if byHandle.DisplayName != "Alice Researcher" {
		t.Errorf("GetUserByHandle: display name %q", byHandle.DisplayName)
	}

	org, err := q.CreateOrganization(ctx, sqlc.CreateOrganizationParams{
		Slug: "acme",
		Name: "Acme Research",
	})
	if err != nil {
		t.Fatalf("CreateOrganization: %v", err)
	}
	bySlug, err := q.GetOrganizationBySlug(ctx, "acme")
	if err != nil {
		t.Fatalf("GetOrganizationBySlug: %v", err)
	}
	if bySlug.ID != org.ID {
		t.Error("organization id mismatch on read-back")
	}

	// The minimal RSG chain through generated queries: project → branch →
	// state → scientific object → version 1 → version 2 (append-only).
	project, err := q.CreateProject(ctx, sqlc.CreateProjectParams{
		OrganizationID: pgtype.UUID{Bytes: org.ID.Bytes, Valid: true},
		Slug:           "p1",
		Name:           "P1",
		Purpose:        "round trip",
		Visibility:     "private",
		CreatedBy:      user.ID,
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if _, err := q.GetProjectBySlug(ctx, sqlc.GetProjectBySlugParams{
		OrganizationID: pgtype.UUID{Bytes: org.ID.Bytes, Valid: true},
		Slug:           "p1",
	}); err != nil {
		t.Fatalf("GetProjectBySlug: %v", err)
	}

	branch, err := q.CreateBranch(ctx, sqlc.CreateBranchParams{
		ProjectID:  project.ID,
		Name:       "main",
		Visibility: "private",
		GitRef:     "refs/heads/main",
		CreatedBy:  user.ID,
	})
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}

	state, err := q.CreateProjectState(ctx, sqlc.CreateProjectStateParams{
		ProjectID:       project.ID,
		BranchID:        pgtype.UUID{Bytes: branch.ID.Bytes, Valid: true},
		StateHash:       "hash-1",
		ManifestVersion: "v1",
	})
	if err != nil {
		t.Fatalf("CreateProjectState: %v", err)
	}

	obj, err := q.CreateScientificObject(ctx, sqlc.CreateScientificObjectParams{
		ProjectID:  project.ID,
		ObjectType: "dataset",
		CreatedBy:  user.ID,
	})
	if err != nil {
		t.Fatalf("CreateScientificObject: %v", err)
	}

	createVersion := func(versionNo int32, title string) {
		t.Helper()
		_, err := q.CreateScientificObjectVersion(ctx, sqlc.CreateScientificObjectVersionParams{
			ObjectID:       obj.ID,
			VersionNo:      versionNo,
			StateID:        state.ID,
			BranchID:       pgtype.UUID{Bytes: branch.ID.Bytes, Valid: true},
			SchemaID:       "core/dataset",
			SchemaVersion:  "1.0",
			Title:          title,
			LifecycleState: "active",
			Payload:        []byte(`{}`),
			IntegrityHash:  "ih-1",
			CreatedBy:      user.ID,
		})
		if err != nil {
			t.Fatalf("CreateScientificObjectVersion v%d: %v", versionNo, err)
		}
	}
	createVersion(1, "Dataset A v1")
	createVersion(2, "Dataset A v2")

	latest, err := q.GetLatestScientificObjectVersion(ctx, obj.ID)
	if err != nil {
		t.Fatalf("GetLatestScientificObjectVersion: %v", err)
	}
	if latest.VersionNo != 2 || latest.Title != "Dataset A v2" {
		t.Errorf("latest version = %d (%q), want 2 (\"Dataset A v2\")", latest.VersionNo, latest.Title)
	}
	versions, err := q.ListScientificObjectVersions(ctx, obj.ID)
	if err != nil {
		t.Fatalf("ListScientificObjectVersions: %v", err)
	}
	if len(versions) != 2 {
		t.Errorf("append-only version log has %d rows, want 2", len(versions))
	}
}
