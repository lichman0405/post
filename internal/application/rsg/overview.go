package rsg

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// ProjectOverview (T0212) is the Overview page's data shape: the project's
// research summary — the first screen that explains what the project is
// doing. It composes the research outline's axes (key questions, key
// findings, the attention list derived from the graph's own state signals)
// with the branch surface (active research paths, current main). The
// overview is a summary with links into the outline and the object pages —
// never a second outline (the Research page owns the full question tree)
// and never a files/type listing (docs/06 §4).
//
// The attention list is derived from state signals only: contested or
// unresolved findings and unresolved questions. No score, no ranking, no
// generated "importance" — docs/07 forbids a Truth Score, and this
// summary computes none.
type ProjectOverview struct {
	ProjectID   string
	ProjectSlug string
	ProjectName string
	// Purpose is the project's one-sentence research goal — the "what is
	// this project about" of the first screen.
	Purpose string
	// Visibility is the project's Public/Private preset.
	Visibility string
	// MainFrozen reports whether the frozen main branch gate is on.
	MainFrozen bool
	// ActivityStatus is the project's lifecycle state (planning, active,
	// ... — the projects.activity_status CHECK values).
	ActivityStatus string
	// Counts is the auxiliary summary, same shape as the outline's.
	Counts OutlineCounts
	// KeyQuestions are the outline's question-tree roots, capped
	// (overviewQuestionCap). TotalQuestions carries the uncapped count
	// the "view all" link needs.
	KeyQuestions   []OutlineQuestion
	TotalQuestions int
	// KeyFindings are the project's findings ordered accepted-first,
	// capped (overviewFindingCap). TotalFindings carries the uncapped
	// count.
	KeyFindings   []OutlineFinding
	TotalFindings int
	// Branches carries the non-main branches: the active research paths
	// plus the closed-path counts.
	Branches OverviewBranches
	// NeedsAttention is the attention list (contested/unresolved findings,
	// unresolved questions), capped (overviewAttentionCap).
	// AttentionTotal carries the uncapped count.
	NeedsAttention []OverviewAttention
	AttentionTotal int
	// CurrentMain is the canonical integration branch's current state.
	// MainBranchID is empty when the project has no main branch yet.
	CurrentMain OverviewMain
	// Empty reports that the project carries no research state at all
	// (no scientific objects) — the page then renders the agent/project
	// start guide instead of the summary sections.
	Empty bool
}

// OverviewBranches is the overview's branch surface: the non-main active
// branches (research paths currently moving) and the counts of closed
// paths (their rows stay in the branch list API; the overview summarizes).
type OverviewBranches struct {
	Active  []OverviewBranch
	Merged  int
	Aborted int
}

// OverviewBranch is one active non-main branch of the overview.
type OverviewBranch struct {
	ID   string
	Name string
	// Purpose is the branch's research-path intent ("" when none).
	Purpose string
	// HeadStateID is the branch's current head state ("" when none —
	// never for a branch created through the domain service, which
	// forks an existing state).
	HeadStateID string
	CreatedAt   time.Time
}

// OverviewAttention is one row of the needs-attention list.
type OverviewAttention struct {
	// Kind is "finding" or "question" — the object's scientific role.
	Kind string
	// ObjectID / BranchID are the drill-down coordinates (BranchID empty
	// = no link, the outline's no-fabricated-coordinate rule).
	ObjectID string
	BranchID string
	Title    string
	// Signal is the state value that put the row on the list
	// (contested / unresolved).
	Signal string
}

// OverviewMain is the canonical integration branch's current state: its
// head (the accepted research state) and its newest commit.
type OverviewMain struct {
	BranchID   string
	BranchName string
	// HeadStateID is main's current head state ("" when main has no head
	// yet).
	HeadStateID string
	// StateHash is the head state's content address (sha256).
	StateHash string
	// GitCommitSHA pins the GitProvider commit when the latest transition
	// came through git_compat ("" otherwise).
	GitCommitSHA string
	// HeadCreatedAt is when the head state was created.
	HeadCreatedAt time.Time
	// LatestCommit carries main's newest commit facts; Present is false
	// when main has no commits of its own yet (the branch exists, nothing
	// was ever committed on it).
	LatestCommit OverviewCommit
}

// OverviewCommit is one state commit rendered on the overview.
type OverviewCommit struct {
	Present   bool
	Message   string
	Via       string
	ActorID   string
	CreatedAt time.Time
}

// Overview caps (L1): the overview is a summary with "view all" links into
// the Research page, so each section renders at most this many rows and
// counts the rest.
const (
	overviewQuestionCap  = 5
	overviewFindingCap   = 5
	overviewAttentionCap = 5
)

// ProjectOverview builds the project's research summary (T0212). The read
// is exactly as visible as the project — the entry gate is the same
// visibility-aware projects.Get requireRead every RSG read runs, so a
// caller who may not read the project answers projects.ErrProjectNotFound
// (existence hiding, docs/45). The outline and the branch surface are both
// project-scoped, so nothing of another project can enter the summary.
//
// A store failure anywhere aborts (fail closed): the caller never
// receives a silently partial overview.
func (s *Service) ProjectOverview(ctx context.Context, r projects.Reader, projectID string) (ProjectOverview, error) {
	if projectID == "" {
		return ProjectOverview{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	// Entry gate first — the same order as Query and ResearchOutline, so
	// the denial precedes every store read. The fetched project serves
	// both the overview facts and the outline builder below: the gate
	// runs once.
	project, err := s.projects.Get(ctx, r, projectID)
	if err != nil {
		return ProjectOverview{}, wrapError(err)
	}
	outline, err := s.buildResearchOutline(ctx, project)
	if err != nil {
		return ProjectOverview{}, wrapError(err)
	}
	if s.branches == nil {
		return ProjectOverview{}, fmt.Errorf("%w: no branch port configured", ErrStore)
	}
	branches, err := s.branches.List(ctx, projectID)
	if err != nil {
		return ProjectOverview{}, wrapError(err)
	}

	// Current main: the branch row carries the head state id (the
	// projection); the head state and the newest commit come from the
	// state surface. A main with no head (BaseStateID nil) renders with
	// an empty head — the page shows "no accepted state yet".
	var mainHead *domain.ProjectState
	var mainCommits []domain.StateCommit
	for _, b := range branches {
		if b.IsMain() {
			if b.BaseStateID != nil {
				if s.states == nil {
					return ProjectOverview{}, fmt.Errorf("%w: no state port configured", ErrStore)
				}
				head, err := s.states.GetBranchHead(ctx, b.ID)
				if err != nil {
					return ProjectOverview{}, wrapError(err)
				}
				commits, err := s.states.ListCommits(ctx, b.ID)
				if err != nil {
					return ProjectOverview{}, wrapError(err)
				}
				mainHead = &head
				mainCommits = commits
			}
			break
		}
	}

	ov := buildProjectOverview(project, outline, branches, mainHead, mainCommits)
	// Attribute main's newest commit to a person, not a raw id — the same
	// profile resolution the object detail page runs for creators (a
	// commit may predate the profile surface; the raw id stays then).
	if ov.CurrentMain.LatestCommit.Present {
		ov.CurrentMain.LatestCommit.ActorID = s.commitActorLabel(ctx, ov.CurrentMain.LatestCommit.ActorID)
	}
	return ov, nil
}

// commitActorLabel resolves a commit actor to a profile handle when one
// exists. The profile port is optional: an overview wired without it
// renders the raw id.
func (s *Service) commitActorLabel(ctx context.Context, userID string) string {
	if s.profiles == nil {
		return userID
	}
	profile, err := s.profiles.GetByUserID(ctx, userID)
	if err != nil || profile.User.Handle == "" {
		return userID
	}
	return profile.User.Handle
}

// buildProjectOverview assembles the summary from the already-read
// surfaces (pure — the unit tests drive it directly).
func buildProjectOverview(project domain.Project, out ResearchOutline, branches []domain.Branch, mainHead *domain.ProjectState, mainCommits []domain.StateCommit) ProjectOverview {
	ov := ProjectOverview{
		ProjectID:      project.ID,
		ProjectSlug:    project.Slug,
		ProjectName:    project.Name,
		Purpose:        project.Purpose,
		Visibility:     string(project.Visibility),
		MainFrozen:     project.MainFrozen,
		ActivityStatus: project.ActivityStatus,
		Counts:         out.Counts,
	}

	// Key questions: the outline's roots (the top level of the question
	// tree), in the outline's display order.
	ov.KeyQuestions = out.Questions
	ov.TotalQuestions = out.Counts.Questions
	if len(ov.KeyQuestions) > overviewQuestionCap {
		ov.KeyQuestions = ov.KeyQuestions[:overviewQuestionCap]
	}

	// Key findings: the human-confirmed knowledge first (accepted), then
	// the rest in assessment order — ordering by lifecycle state, never a
	// generated score.
	findings := slices.Clone(out.Findings)
	slices.SortFunc(findings, func(a, b OutlineFinding) int {
		if c := findingAssessmentRank(a.Assessment) - findingAssessmentRank(b.Assessment); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	ov.KeyFindings = findings
	ov.TotalFindings = out.Counts.Findings
	if len(ov.KeyFindings) > overviewFindingCap {
		ov.KeyFindings = ov.KeyFindings[:overviewFindingCap]
	}

	ov.Branches = buildOverviewBranches(branches)
	ov.NeedsAttention = buildOverviewAttention(out)
	ov.AttentionTotal = len(ov.NeedsAttention)
	if len(ov.NeedsAttention) > overviewAttentionCap {
		ov.NeedsAttention = ov.NeedsAttention[:overviewAttentionCap]
	}

	for _, b := range branches {
		if b.IsMain() {
			ov.CurrentMain = OverviewMain{
				BranchID:    b.ID,
				BranchName:  b.Name,
				HeadStateID: branchBaseStateID(b),
			}
			if mainHead != nil {
				ov.CurrentMain.StateHash = mainHead.StateHash
				if mainHead.GitCommitSHA != nil {
					ov.CurrentMain.GitCommitSHA = *mainHead.GitCommitSHA
				}
				ov.CurrentMain.HeadCreatedAt = mainHead.CreatedAt
			}
			// The commit history is oldest-first; main's newest commit is
			// its tail.
			if len(mainCommits) > 0 {
				c := mainCommits[len(mainCommits)-1]
				ov.CurrentMain.LatestCommit = OverviewCommit{
					Present:   true,
					Message:   c.Message,
					Via:       string(c.Via),
					ActorID:   c.ActorID,
					CreatedAt: c.CreatedAt,
				}
			}
			break
		}
	}

	// Empty: no scientific objects of any kind — the page then renders
	// the agent/project start guide instead of the summary sections.
	ov.Empty = out.Counts.Questions == 0 && out.Counts.Findings == 0 &&
		out.Counts.Hypotheses == 0 && out.Counts.Claims == 0 && out.Counts.OtherObjects == 0
	return ov
}

// findingAssessmentRank orders the key-findings list: the human-confirmed
// knowledge first, then the rest in assessment order. Ordering by
// lifecycle state, never a generated score.
func findingAssessmentRank(assessment string) int {
	switch assessment {
	case "accepted":
		return 0
	case "preliminary":
		return 1
	case "contested", "unresolved":
		return 2
	case "superseded", "aborted":
		return 3
	default:
		return 4
	}
}

// buildOverviewBranches splits the project's branches into the active
// non-main paths and the closed-path counts. The list is oldest-first
// (the store's order), so active paths render in creation order.
func buildOverviewBranches(branches []domain.Branch) OverviewBranches {
	var out OverviewBranches
	for _, b := range branches {
		if b.IsMain() {
			continue
		}
		switch b.Lifecycle {
		case domain.BranchLifecycleActive:
			out.Active = append(out.Active, OverviewBranch{
				ID:          b.ID,
				Name:        b.Name,
				Purpose:     branchPurpose(b),
				HeadStateID: branchBaseStateID(b),
				CreatedAt:   b.CreatedAt,
			})
		case domain.BranchLifecycleMerged:
			out.Merged++
		case domain.BranchLifecycleAborted:
			out.Aborted++
		}
	}
	return out
}

// buildOverviewAttention derives the needs-attention list from the graph's
// own state signals: findings whose assessment is contested or unresolved
// (a scientific disagreement a human must resolve — docs/07 §8), and
// questions whose state is unresolved. Sorted by role then title for a
// deterministic render.
func buildOverviewAttention(out ResearchOutline) []OverviewAttention {
	var items []OverviewAttention
	for _, f := range out.Findings {
		switch f.Assessment {
		case "contested", "unresolved":
			items = append(items, OverviewAttention{
				Kind:     "finding",
				ObjectID: f.ObjectID,
				BranchID: f.BranchID,
				Title:    f.Title,
				Signal:   f.Assessment,
			})
		}
	}
	var walk func(qs []OutlineQuestion)
	walk = func(qs []OutlineQuestion) {
		for _, q := range qs {
			if q.QuestionState == "unresolved" {
				items = append(items, OverviewAttention{
					Kind:     "question",
					ObjectID: q.ObjectID,
					BranchID: q.BranchID,
					Title:    q.Title,
					Signal:   q.QuestionState,
				})
			}
			walk(q.Children)
		}
	}
	walk(out.Questions)
	slices.SortFunc(items, func(a, b OverviewAttention) int {
		if c := strings.Compare(a.Kind, b.Kind); c != 0 {
			return c
		}
		return strings.Compare(a.Title, b.Title)
	})
	return items
}

// branchPurpose derefs the optional purpose ("" when none).
func branchPurpose(b domain.Branch) string {
	if b.Purpose == nil {
		return ""
	}
	return *b.Purpose
}

// branchBaseStateID derefs the optional head state id ("" when none).
func branchBaseStateID(b domain.Branch) string {
	if b.BaseStateID == nil {
		return ""
	}
	return *b.BaseStateID
}
