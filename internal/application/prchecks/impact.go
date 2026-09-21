package prchecks

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/lichman0405/post/internal/application/dependencyimpact"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// This file is the dependency impact line of the PR first screen.
//
// docs/06 §6 lists what a PR's first screen shows — 「新增/修改/Abort 的科研对象、
// 知识变化、Evidence 变化、Protocol/Schema changes、visibility changes、
// dependency impact」 — and docs/09 §4 lists what a PR contains: 「...visibility
// impact、dependency impact、merge strategy」. The other lines are the
// engine's (internal/rsg/integrity) or the diff service's; dependency impact
// is the analysis's, and this is the read that puts it on that screen.
//
// # What the screen asks
//
// It asks about the PR: this proposal changes some objects — what downstream
// work does that reach? So the read is a composition of two things that
// already exist and neither of which is re-implemented here:
//
//  1. which objects the proposal changes, derived from the base and proposed
//     manifests by changedObjects below;
//  2. what each of those changes reaches, which is
//     dependencyimpact.Service.Read — the same walk the background analysis
//     runs, through the same relation catalog, with the reader's access
//     applied.
//
// # Why it is a READ, and not the analysis
//
// The background analysis (cmd/worker) answers 「上游变更触发」 for changes that
// HAVE happened; a PR's changes have not been merged, so there is no event in
// the log for them and no alert — for a reason that is the platform's own
// rule: nothing about a proposal is a fact yet (docs/11 §6, merge controls
// acceptance). Asking the stored alerts "what is affected" would therefore
// answer about a different tree of facts, and asking the analysis to emit
// alerts for a proposal would record an event for a change that may never
// happen. So the screen runs the same walk as a query. It is the same
// function; only the trigger differs, and the record differs because the
// trigger does.
//
// # The reader
//
// The read takes the caller's projects.Reader and applies it per affected
// project (dependencyimpact.Service.Read): a PR page shows the impacts its
// reader could go and open, and nothing else. The subject side is gated too —
// a caller who cannot read the PR's own objects gets no report at all,
// which is the same answer as a PR that does not exist.

// ImpactReader is the dependency impact read surface, satisfied
// structurally by *dependencyimpact.Service. It is declared here rather than
// taking the concrete type for the reason every other port in this package
// is: the screen's question ("what does this proposal reach") is stable,
// while the analysis behind it owns its own walk, its own catalog reads and
// its own access rules.
type ImpactReader interface {
	Read(ctx context.Context, reader projects.Reader, subjects dependencyimpact.Subjects) (dependencyimpact.Report, error)
}

// PullRequestImpact answers the PR first screen's dependency impact line:
// what the objects this PR changes are depended on by, as this caller may
// see it.
//
// The returned Report is empty when the proposal changes nothing that has
// dependents — an answer, not an error, and one the screen renders as
// "nothing downstream is affected" (the same distinction
// dependencyimpact.Report draws for a subject with no visible impacts).
//
// Errors are the ones every PR read here returns: ErrPullRequestNotFound for
// an unknown PR number, ErrValidation for a malformed one, ErrStore for an
// unreadable database. A subject the caller may not read is
// dependencyimpact.ErrSubjectNotFound, which the transport maps to the same
// 404 shape (docs/45 existence hiding) — it is not mapped to ErrStore, since
// "you may not see this" is not "the database is unwell".
func (s *Service) PullRequestImpact(ctx context.Context, reader projects.Reader, projectID string, number int64) (dependencyimpact.Report, error) {
	if projectID == "" {
		return dependencyimpact.Report{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if number < 1 {
		return dependencyimpact.Report{}, fmt.Errorf("%w: number must be positive", ErrValidation)
	}
	if s.impact == nil {
		// Fail closed: a screen that cannot ask the analysis must not render
		// an empty impact line, which reads exactly like "nothing is
		// affected" (the rule every optional port in this tree follows).
		return dependencyimpact.Report{}, fmt.Errorf("%w: dependency impact is not wired", ErrStore)
	}
	pr, err := s.prs.GetPullRequest(ctx, projectID, number)
	if err != nil {
		return dependencyimpact.Report{}, WrapStoreError(err)
	}
	baseManifest, err := s.manifest.GetManifestSnapshot(ctx, pr.BaseStateID)
	if err != nil {
		return dependencyimpact.Report{}, WrapStoreError(err)
	}
	proposedManifest, err := s.manifest.GetManifestSnapshot(ctx, pr.ProposedStateID)
	if err != nil {
		return dependencyimpact.Report{}, WrapStoreError(err)
	}
	subjects := changedObjects(baseManifest, proposedManifest)
	if len(subjects) == 0 {
		// Nothing changed that a dependency could be affected by — no walk
		// to run and no subject to authorize. An empty report is the answer.
		return dependencyimpact.Report{}, nil
	}
	report, err := s.impact.Read(ctx, reader, subjects)
	if err != nil {
		if errors.Is(err, dependencyimpact.ErrSubjectNotFound) {
			// The proposal touches an object of a project the caller cannot
			// read: the same 404 an unknown PR gets. Wrapped so the transport
			// can tell it from the package's own not-found.
			return dependencyimpact.Report{}, fmt.Errorf("%w: %v", ErrPullRequestNotFound, err)
		}
		return dependencyimpact.Report{}, err
	}
	return report, nil
}

// changedObjects derives the proposal's changed objects from the two
// manifests: the container object ids whose recorded versions differ between
// the base state and the proposed state.
//
// Two differences count, and they are docs/06 §6's 新增/修改/Abort read as
// what the manifests can actually show:
//
//   - a version present in the proposal and absent from the base: a new
//     version of an object (新增 for an object the base did not have, 修改
//     for one it did) — both are a change to the object;
//   - a version present in BOTH whose LifecycleState differs: the same
//     version re-recorded with a different lifecycle position, which is how
//     an abort of a version appears in a lineage that does not re-version it.
//
// The result is sorted and deduplicated by object id, because it becomes the
// subject list of a read whose contract is deterministic, and because two
// versions of one object (a modify and an abort in one proposal) are ONE
// affected object — the walk is over objects (dependencyimpact's package
// comment), so listing the object twice would ask the same question twice.
//
// It is deliberately a pure function of the two snapshots: the screen's
// answer must be a function of the states the PR pins (the engine's own
// purity rule, internal/rsg/integrity), not of whatever the branch tip has
// moved on to.
func changedObjects(base, proposed manifest.Snapshot) dependencyimpact.Subjects {
	baseVersion := make(map[string]manifest.ObjectVersion, len(base.ObjectVersions))
	for _, v := range base.ObjectVersions {
		baseVersion[v.ID] = v
	}
	seen := map[string]bool{}
	out := make(dependencyimpact.Subjects, 0, len(proposed.ObjectVersions))
	for _, v := range proposed.ObjectVersions {
		if v.ObjectID == "" {
			// A member row whose container is unset names no object to walk
			// from. It cannot be produced by the RSG write path (every
			// version carries its object), so this is a guard, not a case.
			continue
		}
		before, existed := baseVersion[v.ID]
		if existed && before.LifecycleState == v.LifecycleState {
			continue
		}
		if seen[v.ObjectID] {
			continue
		}
		seen[v.ObjectID] = true
		out = append(out, dependencyimpact.Subject{
			Kind: dependencyimpact.SubjectObject,
			ID:   v.ObjectID,
			// The version the proposal records for this object. It is what
			// the alert's message names ("version 4 of this changed"), and
			// resolving it here rather than at the emitter keeps the walk
			// and the message from disagreeing about which version the
			// change is.
			VersionNo: v.VersionNo,
		})
	}
	sortSubjects(out)
	return out
}

// sortSubjects orders the subject list by id. The analysis's own
// sortImpacts orders impacts; this orders the INPUT, so two reads of the
// same PR produce the same request as well as the same answer.
func sortSubjects(subjects dependencyimpact.Subjects) {
	sort.Slice(subjects, func(i, j int) bool { return subjects[i].ID < subjects[j].ID })
}
