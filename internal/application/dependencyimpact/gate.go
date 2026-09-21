package dependencyimpact

import (
	"context"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// ProjectsGate adapts a *projects.Service to ProjectGate.
//
// It lives in this package rather than at the composition root for the
// reason internal/application/audit/adapters.go's ProjectsReadGate does: a
// second copy would be a second thing to keep in step, and the translation
// it performs — the read gate's existence hiding, plus membership — is a
// statement about the gate rather than about any caller.
//
// The two questions it answers are asked through the project service's own
// two reads, in this order:
//
//  1. Get with the caller's projects.Reader. A denied read is
//     ErrProjectNotFound and is answered Visible:false — the same answer
//     as a project that does not exist, which is the point (docs/45). A
//     store failure is NOT turned into Visible:false: "could not look" is
//     not "may not see", and reporting an outage as a hidden entity would
//     silently shrink an impact report on the strength of an error.
//  2. GetMembership, for a caller who may read the project. It re-runs the
//     read gate itself (projects/service.go), which is a second evaluation
//     of an answer already known — accepted here because the alternative
//     is this package reading the membership table directly, which would
//     put the permission model in two places. A caller with no role gets
//     ErrMemberNotFound, which is "not a member" rather than a failure: it
//     is the answer the second axis of the asset-dependency rule needs.
//
// An anonymous caller is never a member, so the membership query is
// skipped for them: it would answer "no" for a reason the caller already
// knows, and an anonymous reader is a legitimate reader of a public
// project (the matrix's read_public_project class).
func ProjectsGate(svc *projects.Service) ProjectGate {
	return projectsGate{svc: svc}
}

type projectsGate struct{ svc *projects.Service }

func (g projectsGate) Access(ctx context.Context, reader projects.Reader, projectID string) (ProjectAccess, error) {
	if _, err := g.svc.Get(ctx, reader, projectID); err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return ProjectAccess{}, nil
		}
		return ProjectAccess{}, fmt.Errorf("%w: read project %s: %v", ErrStore, projectID, err)
	}
	out := ProjectAccess{Visible: true}
	if !reader.Authenticated || reader.UserID == "" {
		return out, nil
	}
	// GetMembership only reads actor.ID (projects/service.go: it builds its
	// own Reader from it), so the domain.User here is a carrier for the id
	// the caller already gave, not a second lookup.
	if _, err := g.svc.GetMembership(ctx, domain.User{ID: reader.UserID}, projectID); err != nil {
		if errors.Is(err, projects.ErrMemberNotFound) {
			return out, nil
		}
		if errors.Is(err, projects.ErrProjectNotFound) {
			// The project became unreadable between the two calls: the
			// caller may not see it, and the honest answer is the one the
			// first call would now give.
			return ProjectAccess{}, nil
		}
		return ProjectAccess{}, fmt.Errorf("%w: membership of %s: %v", ErrStore, projectID, err)
	}
	out.Member = true
	return out, nil
}

var _ ProjectGate = projectsGate{}
