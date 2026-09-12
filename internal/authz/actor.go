package authz

import "github.com/lichman0405/post/internal/domain"

// ActorClass is one permission-matrix column: the actor's relationship to
// the resource's project at request time
// (specs/policies/permissions-matrix.csv header). A class is resolved by
// ClassOf from facts the service already verified (session presence,
// membership row, agent flag) — never from client-supplied strings.
type ActorClass string

const (
	// ActorPublicAnonymous is a caller with no session.
	ActorPublicAnonymous ActorClass = "public_anonymous"
	// ActorAuthenticatedNonMember is a caller with a valid session but no
	// membership in the project.
	ActorAuthenticatedNonMember ActorClass = "authenticated_nonmember"
	ActorViewer                 ActorClass = "viewer"
	ActorContributor            ActorClass = "contributor"
	ActorMaintainer             ActorClass = "maintainer"
	ActorOwner                  ActorClass = "owner"
	// ActorAgent is the agent column (agent_default): platform agents
	// acting on a project act under their own scoping column, never a
	// human membership.
	ActorAgent ActorClass = "agent_default"
)

// ClassOf resolves the matrix column for an actor:
//
//   - isAgent → ActorAgent (agents act under the agent_default column);
//   - a membership role → the role's column;
//   - authenticated without a membership → ActorAuthenticatedNonMember;
//   - otherwise → ActorPublicAnonymous.
//
// An unknown role resolves to no class (""): the engine denies an
// unknown class by default, so a corrupt role grants nothing.
func ClassOf(authenticated bool, role *domain.ProjectRole, isAgent bool) ActorClass {
	if isAgent {
		return ActorAgent
	}
	if role != nil {
		switch *role {
		case domain.ProjectRoleOwner:
			return ActorOwner
		case domain.ProjectRoleMaintainer:
			return ActorMaintainer
		case domain.ProjectRoleContributor:
			return ActorContributor
		case domain.ProjectRoleViewer:
			return ActorViewer
		}
		return ""
	}
	if authenticated {
		return ActorAuthenticatedNonMember
	}
	return ActorPublicAnonymous
}
