package audit

import (
	"context"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// ProjectsReadGate adapts a *projects.Service to ProjectReadGate.
//
// T0106 made the project read take a projects.Reader, because the visibility
// matrix decides what an ANONYMOUS reader sees, while this gate's vocabulary is
// an authenticated actor: the activity feed is member-only. The two are the
// same call with the authenticated end of the matrix filled in — which is what
// this states, in one place, instead of teaching either package the other's
// vocabulary.
//
// It lives here rather than at the composition root because two callers need
// it (cmd/api and the audit integration test) and a second copy would be a
// second thing to keep in step.
func ProjectsReadGate(svc *projects.Service) ProjectReadGate {
	return projectsReadGate{svc: svc}
}

type projectsReadGate struct{ svc *projects.Service }

func (g projectsReadGate) Get(ctx context.Context, actor domain.User, projectID string) (domain.Project, error) {
	return g.svc.Get(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, projectID)
}
