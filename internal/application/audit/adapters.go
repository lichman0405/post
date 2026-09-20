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
// an authenticated actor: the Activity surface requires a session. The two are
// the same call with the authenticated end of the matrix filled in — which is
// what this states, in one place, instead of teaching either package the other's
// vocabulary.
//
// What the gate answers is "may this reader read this PROJECT", and that is all
// it answers: a public project's read is allowed for every matrix class, so
// passing it does not establish membership. The research rows of the feed carry
// a visibility of their own and are narrower than the project (ADR-024, T0613);
// that predicate belongs to the read and lives with the rows, not here.
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
