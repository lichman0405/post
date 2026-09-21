package dependencyimpact

import (
	"context"

	"github.com/lichman0405/post/internal/application/projects"
)

// Store is the analysis's read/write surface over PostgreSQL. It is one
// port rather than three because the walk it exposes has ONE definition —
// a recursive CTE over the dependency-marked relation edges — and the
// worker's pass, the system-wide analysis and the authorized read all have
// to traverse the same graph. A second traversal, however written, would
// be a second answer to "what does this change reach".
//
// Every method is a READ except AnalyzeBatch, which is the pass itself.
// There is no method here that changes a scientific object, a state, a
// release, a visibility or a review — not because they are unused, but
// because the port does not have them: the invariant this package owes
// docs/11 §7 ("impact 是提示，不是状态迁移") is a property of its type
// system, not of its discipline.
type Store interface {
	// SubjectAnalyzer is the walk itself. It is a separate interface
	// because the walk is a different capability from the pass: the
	// worker's transaction needs to walk a subject while it holds a
	// transaction, and it must run the SAME walk the service and the read
	// surface run (AnalyzeSubject) — one definition of "what does a change
	// to this reach", or the alert a pass writes and the answer a reader
	// gets could disagree.
	SubjectAnalyzer

	// SubjectInfo resolves the upstream subject to the project that owns
	// it, or ErrSubjectNotFound when it names nothing. The subject's own
	// project is what the read path authorizes the caller against.
	SubjectInfo(ctx context.Context, s Subject) (SubjectInfo, error)

	// AnalyzeBatch runs one analysis pass over at most limit trigger
	// events that have not been analyzed yet. It is idempotent: a trigger
	// whose alerts are already written is reported as a duplicate, never
	// written twice (migration 00111's partial unique index).
	AnalyzeBatch(ctx context.Context, limit int) (Batch, error)
}

// SubjectAnalyzer is the read surface one walk needs: the two landings a
// dependency has in this repository, each answering with the entities a
// change to it reaches.
type SubjectAnalyzer interface {
	// ObjectClosure returns every object that depends on objectID,
	// directly or transitively, with its hop count. It is the walk.
	ObjectClosure(ctx context.Context, objectID string) (Analysis, error)

	// AssetDependents returns the projects pinning assetVersionID, with
	// the visibility of each declaration.
	AssetDependents(ctx context.Context, assetVersionID string) ([]AssetDependent, error)
}

// SubjectInfo is the upstream subject's owning project.
type SubjectInfo struct {
	// ProjectID is the project the subject belongs to: the object's
	// project, or the asset's origin project (research_assets.
	// origin_project_id — the project whose read gate serves the asset's
	// page, so it is the project a reader must be able to open for the
	// subject to be readable at all).
	ProjectID string
	// Visibility is that project's stored visibility, carried so the
	// emitter does not have to ask for it a second time.
	Visibility string
}

// ProjectAccess answers the reader's standing in one project — the two
// questions the read surface's authorization needs and no more.
type ProjectAccess struct {
	// Visible reports whether the reader may read the project at all
	// (projects.Service.Get's answer, existence hiding included: a project
	// they may not read and one that does not exist are the same false).
	Visible bool
	// Member reports whether the reader is inside the project. A member of
	// a private project may read it, and a member of a public one may see
	// the declarations the project kept private (docs/23 §5,
	// assets.ProjectDependencyViewer's second rule).
	Member bool
}

// ProjectGate is the authorization the read surface runs per affected
// entity. It is one method wide because the answer is one bit wide plus
// membership, and because the production implementation can answer both
// through the project service without this package learning the permission
// matrix. The production adapter is internal/persistence's gate; the
// vocabulary it takes is projects.Reader, the same one every other read
// gate in this tree takes.
type ProjectGate interface {
	Access(ctx context.Context, reader projects.Reader, projectID string) (ProjectAccess, error)
}
