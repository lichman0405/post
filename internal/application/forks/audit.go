package forks

import (
	"github.com/lichman0405/post/internal/domain"
)

// ActionProjectForked is the audit action of a fork: a project was forked
// into an actor's own space, and the lineage row records it.
//
// It lives HERE rather than beside the other Action* constants in
// internal/domain/audit.go because that package is outside this task's
// allowed scope. The vocabulary is a registry, not a schema (no CHECK
// constrains audit_log.action), so the row is well-formed either way; the
// name follows the registry's shape ("<subject>.<verb>", the same
// spelling domain.ActionProjectCreated and domain.ActionProjectMainFrozen
// use), and it is reported as a follow-up so a later task can move the
// constant next to the others.
const ActionProjectForked = "project.forked"

// relationForkedFrom is the canonical lineage relation name a fork is
// recorded with — the vocabulary's, not this package's invention
// (internal/rsg/relationcatalog lists it as a lineage/network type,
// docs/44, and project_forks.relation_type's CHECK pins the same value).
// A unit test asserts it against the catalog, so a rename there cannot
// leave this literal silently behind.
const relationForkedFrom = "forked_from"

// forkAudit renders the audit entry the lineage insert carries.
//
// The entry is written by the store INSIDE the lineage's transaction
// (docs/53): the row, the audit and the domain event are one fact, so a
// fork is never observable without the record of who forked what, and a
// repeated request — which writes no lineage row — writes no audit row
// either.
//
// The scope is the PARENT project: forking is an action taken on the
// project being forked, and the parent's activity is where an external
// fork appears (its maintainers are the ones who need to see it). The
// target ref names the fork itself, and the summaries carry the identity
// of both sides plus the branch pair the content travelled along.
func forkAudit(actor domain.User, parent, forkProject domain.Project, source, forkBranch domain.Branch) domain.AuditEntry {
	return domain.AuditEntry{
		ActorID:   actor.ID,
		Action:    ActionProjectForked,
		TargetRef: "project:" + forkProject.ID,
		ProjectID: parent.ID,
		AfterSummary: map[string]any{
			"fork_project_id":     forkProject.ID,
			"fork_project_slug":   forkProject.Slug,
			"fork_visibility":     string(forkProject.Visibility),
			"parent_project_id":   parent.ID,
			"parent_project_slug": parent.Slug,
			"source_branch_id":    source.ID,
			"source_branch_name":  source.Name,
			"fork_branch_id":      forkBranch.ID,
			"fork_branch_name":    forkBranch.Name,
		},
		Metadata: map[string]any{
			"relation_type": relationForkedFrom,
		},
	}
}
