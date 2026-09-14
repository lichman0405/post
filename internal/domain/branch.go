package domain

import (
	"strings"
	"time"
)

// MainBranchName is the canonical integration branch name (docs/09 §3:
// main is the current accepted research state, frozen and updated only
// through Research PR merges). The first branch of every project is main;
// lifecycle transitions (merge/abort) never apply to it — its evolution is
// the project's, not one research path's.
const MainBranchName = "main"

// Branch is one research evolution path of a project (docs/03 §2:
// "Research Branch：一条研发演化路径"; canonical table branches): a named
// fork of a project state whose head evolves independently of every other
// branch, including main. A branch is not a personal workspace — it is a
// research path (docs/09 §1).
//
// Head tracking: BaseStateID is the branch's current head — the state the
// next commit builds on. It is the docs/21 §5 "branch current state"
// projection, advanced only through T0204's CommitState compare-and-swap.
// When a branch is created from a base state, that state becomes its
// initial head; the base state itself keeps the branch it was committed on
// (branches fork existing states, they never copy them), so the state
// chains of main and the branch stay disjoint while the shared ancestor
// stays visible on the branch that created it.
type Branch struct {
	// ID is the uuid v4 text form (matches the branches.id uuid column).
	ID string
	// ProjectID is the research boundary the branch belongs to.
	ProjectID string
	// Name is the branch name, unique per project (branches UNIQUE
	// (project_id, name)). "main" names the canonical integration branch.
	Name string
	// Visibility is the branch's public/private visibility. Defaults to
	// the project's preset at creation (docs/09 §1); a private project
	// never gets a public branch, because widening visibility beyond the
	// project preset is the Publish flow's job, not branch creation's
	// (docs/12 §3).
	Visibility BranchVisibility
	// Purpose optionally states the research-path intent in one sentence
	// (nil when none).
	Purpose *string
	// GitRef is the git-compatibility ref the branch maps to
	// (refs/heads/<name>, derived 1:1 from Name); T0303 syncs it with the
	// GitProvider.
	GitRef string
	// BaseStateID is the branch's current head state (the branches.
	// base_state_id projection); nil only before the branch has any state
	// of its own — never for a branch created through the domain service,
	// which requires a base state.
	BaseStateID *string
	// Lifecycle is active, merged or aborted (docs/43: "active → merged |
	// aborted；merged/aborted history immutable"). A branch that is merged
	// or aborted accepts no further commits and its head no longer moves.
	Lifecycle BranchLifecycle
	// CreatedBy names the user who created the branch.
	CreatedBy string
	CreatedAt time.Time
}

// IsMain reports whether the branch is the project's main branch.
func (b Branch) IsMain() bool { return b.Name == MainBranchName }

// BranchVisibility is a branch's public/private visibility
// (branches.visibility CHECK). It defaults to the project's preset
// (docs/09 §1); explicit requests may only stay within the project's
// preset — a private project cannot host a public branch (docs/12 §3: any
// visibility widening goes through an explicit, audited publication flow).
type BranchVisibility string

const (
	BranchVisibilityPublic  BranchVisibility = "public"
	BranchVisibilityPrivate BranchVisibility = "private"
)

// ValidBranchVisibility reports whether v is one of the two canonical
// values.
func ValidBranchVisibility(v BranchVisibility) bool {
	return v == BranchVisibilityPublic || v == BranchVisibilityPrivate
}

// BranchLifecycle is a branch's lifecycle state (branches.lifecycle_state
// CHECK; docs/43 §Branch): active while the research path evolves, merged
// once its diff became part of main, aborted when the path is closed
// without merging. merged/aborted are terminal: the branch history is
// immutable from then on.
type BranchLifecycle string

const (
	BranchLifecycleActive  BranchLifecycle = "active"
	BranchLifecycleMerged  BranchLifecycle = "merged"
	BranchLifecycleAborted BranchLifecycle = "aborted"
)

// ValidBranchLifecycle reports whether l is one of the three canonical
// states.
func ValidBranchLifecycle(l BranchLifecycle) bool {
	switch l {
	case BranchLifecycleActive, BranchLifecycleMerged, BranchLifecycleAborted:
		return true
	}
	return false
}

// AcceptsCommits reports whether new state commits may land on the branch:
// only active branches evolve (docs/43: merged/aborted history immutable).
func (l BranchLifecycle) AcceptsCommits() bool { return l == BranchLifecycleActive }

// ValidBranchName reports whether name has the branch-name shape: the
// git-ref-safe form the branch is addressed by (and that T0303 publishes
// as refs/heads/<name>). Rules, mirroring git's own check-ref-format
// subset without being exhaustive:
//
//   - 1..128 characters of [a-zA-Z0-9._/-] (ASCII only in V1);
//   - no leading/trailing '/', no '//' (empty path segments);
//   - no '.' or '..' path segment, no '.lock' suffix (git plumbing names);
//   - not "@{…}"-shaped (git reflog syntax is not part of a name);
//   - not "HEAD" in any case (git reserves it).
//
// The trimmed form must equal the input: outer whitespace is never part of
// a branch name. "main" is a valid name — it is the canonical one.
func ValidBranchName(name string) bool {
	n := strings.TrimSpace(name)
	if n == "" || n != name || len(n) > 128 {
		return false
	}
	if strings.HasPrefix(n, "/") || strings.HasSuffix(n, "/") || strings.Contains(n, "//") {
		return false
	}
	if strings.HasSuffix(n, ".lock") || strings.HasSuffix(n, ".") || strings.HasPrefix(n, ".") {
		return false
	}
	if strings.Contains(n, "@{") {
		return false
	}
	if strings.EqualFold(n, "HEAD") {
		return false
	}
	for _, segment := range strings.Split(n, "/") {
		if segment == "." || segment == ".." || segment == "" {
			return false
		}
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-', r == '/':
		default:
			return false
		}
	}
	return true
}

// ValidBranchPurpose reports whether purpose is a usable branch intent
// statement: nil, or non-blank and bounded (generous but finite — a
// hostile client must not be able to store megabytes per branch).
func ValidBranchPurpose(purpose *string) bool {
	if purpose == nil {
		return true
	}
	p := strings.TrimSpace(*purpose)
	return p != "" && len(p) <= 2000
}
