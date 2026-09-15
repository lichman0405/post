package contribution

import (
	"regexp"
	"slices"
	"strings"
	"time"
)

// The Contribution Opportunity domain (task T0803, docs/02 "Open Network —
// Open Contribution Opportunity"): a maintainer marks an issue or a
// research need (a research_question scientific object) as open for
// contribution, carrying difficulty/capability metadata, and later
// publicizes it onto the open network. The row's lifecycle and its
// visibility are two separate machines:
//
//   - State (suggested → open → closed): "suggested" rows are proposals —
//     an agent (or member) suggests an opportunity and it becomes real
//     only through a maintainer's approval (requirement "agent
//     suggestions require approval"). "open" rows are real opportunities
//     accepting contributions; "closed" is terminal ("nothing disappears,
//     state only evolves" — closed rows stay as history, a re-mark
//     creates a new row).
//   - Visibility (internal → public): a new row is always internal
//     (visible to the project only). Making it visible on the open
//     network is the publicize action — a docs/12 §3 visibility-widening:
//     it requires an explicit decision by an authorized person, produces
//     an audit record, and is NEVER automatic (acceptance "Agent 不能
//     automatically publicize opportunity"). The service refuses agent
//     actors; migration 00062's guard makes any other write path fail at
//     the database, whatever the caller.
//
// A publicized row is frozen except for its state: metadata (title,
// description, difficulty, capabilities) and the publicize stamps are
// immutable once public — the open network may index the row, so its
// content must not drift underneath it (the same fixity discipline as
// T0402's base state and the release model, docs/11).
type ContributionOpportunity struct {
	// ID is the uuid v4 text form (contribution_opportunities.id).
	ID string
	// ProjectID is the research boundary the opportunity belongs to.
	ProjectID string
	// TargetType names what the opportunity is about: an issue (project
	// coordination row) or a research need (research_question scientific
	// object).
	TargetType OpportunityTargetType
	// TargetID is the uuid of the target row (issues.id or
	// scientific_objects.id). Existence and the target's membership in
	// the project are enforced by the database itself (migration 00062
	// deferred guard).
	TargetID string
	// Title is the one-line summary. The store snapshots it from the
	// target at creation (the issue's title / the research question's
	// current version title) — never caller-supplied; it can be edited
	// through UpdateMetadata while internal.
	Title string
	// Description carries the maintainer's/suggester's context: what a
	// good contribution looks like, where to start. Empty when none
	// (domain.ValidOpportunityDescription bounds it).
	Description string
	// Difficulty is the skill-level metadata of the opportunity
	// (requirement "difficulty/capability metadata").
	Difficulty Difficulty
	// RequiredCapabilities are the capability tags a contributor should
	// have (requirement "difficulty/capability metadata"). Free-form
	// lowercase tags, bounded (ValidRequiredCapabilities).
	RequiredCapabilities []string
	// State is the opportunity's lifecycle state (the suggested → open →
	// closed machine above).
	State OpportunityState
	// Visibility is internal (project-only) or public (open network).
	// internal → public happens ONLY through the explicit publicize
	// action; public is terminal (no un-publish in V1).
	Visibility OpportunityVisibility
	// CreatedBy names the actor who created the row (a maintainer for a
	// mark, the suggester for a suggestion).
	CreatedBy string
	// SuggestedBy is the suggester, set only for suggested rows (nil for
	// a maintainer mark). It stays set after approval — the row keeps
	// its origin.
	SuggestedBy *string
	// ApprovedBy is the maintainer who approved the suggestion (nil
	// until approval; immutable once set).
	ApprovedBy *string
	// PublicizedBy is the maintainer who publicized the row (nil until
	// publicize; immutable once set).
	PublicizedBy *string
	// PublicizedAt is when the row was publicized (nil until publicize;
	// immutable once set).
	PublicizedAt *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// OpportunityTargetType names the two kinds of targets an opportunity can
// be about (contribution_opportunities.target_type CHECK).
type OpportunityTargetType string

const (
	// TargetIssue: the opportunity is about an issue row (project
	// coordination, canonical table issues).
	TargetIssue OpportunityTargetType = "issue"
	// TargetResearchQuestion: the opportunity is about a research need —
	// a research_question scientific object (the question model T0501
	// built on, docs/07).
	TargetResearchQuestion OpportunityTargetType = "research_question"
)

// ValidOpportunityTargetType reports whether t is one of the two targets.
func ValidOpportunityTargetType(t OpportunityTargetType) bool {
	return t == TargetIssue || t == TargetResearchQuestion
}

// Difficulty is the skill-level metadata of an opportunity
// (contribution_opportunities.difficulty CHECK). The vocabulary is a
// skill level, not a judgement of the target's complexity: beginner /
// intermediate / advanced (L1 decision recorded in the task result — no
// spec pins a difficulty vocabulary yet).
type Difficulty string

const (
	DifficultyBeginner     Difficulty = "beginner"
	DifficultyIntermediate Difficulty = "intermediate"
	DifficultyAdvanced     Difficulty = "advanced"
)

// ValidDifficulty reports whether d is one of the three levels.
func ValidDifficulty(d Difficulty) bool {
	switch d {
	case DifficultyBeginner, DifficultyIntermediate, DifficultyAdvanced:
		return true
	}
	return false
}

// OpportunityState is the opportunity's lifecycle state
// (contribution_opportunities.state CHECK):
//
//   - suggested: a proposal awaiting a maintainer's approval — the only
//     state an agent's action can create (requirement "agent suggestions
//     require approval");
//   - open: a real opportunity accepting contributions;
//   - closed: terminal (rejected suggestion, or a fulfilled/withdrawn
//     opportunity). Closed rows stay as history; nothing is deleted.
type OpportunityState string

const (
	OpportunityStateSuggested OpportunityState = "suggested"
	OpportunityStateOpen      OpportunityState = "open"
	OpportunityStateClosed    OpportunityState = "closed"
)

// ValidOpportunityState reports whether s is one of the three states.
func ValidOpportunityState(s OpportunityState) bool {
	switch s {
	case OpportunityStateSuggested, OpportunityStateOpen, OpportunityStateClosed:
		return true
	}
	return false
}

// opportunityStateTransitions is the suggested → open → closed map.
// closed is terminal; suggested is a creation state (no transition
// targets it). Migration 00062's guard enforces the same map for any
// database write path.
var opportunityStateTransitions = map[OpportunityState][]OpportunityState{
	OpportunityStateSuggested: {OpportunityStateOpen, OpportunityStateClosed},
	OpportunityStateOpen:      {OpportunityStateClosed},
	OpportunityStateClosed:    nil,
}

// CanTransitionOpportunityState reports whether the lifecycle move
// from → to is allowed.
func CanTransitionOpportunityState(from, to OpportunityState) bool {
	for _, t := range opportunityStateTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// IsTerminalOpportunityState reports whether the state accepts no further
// transitions: closed.
func IsTerminalOpportunityState(s OpportunityState) bool {
	return s == OpportunityStateClosed
}

// OpportunityVisibility is the opportunity's visibility
// (contribution_opportunities.visibility CHECK): internal (project
// members only) or public (open network). internal → public happens
// exclusively through the explicit publicize action (docs/12 §3: any
// visibility widening requires an authorized person's explicit
// confirmation, produces an audit record, and is never automatic — and
// never performed by an agent); public is terminal.
type OpportunityVisibility string

const (
	OpportunityVisibilityInternal OpportunityVisibility = "internal"
	OpportunityVisibilityPublic   OpportunityVisibility = "public"
)

// ValidOpportunityVisibility reports whether v is one of the two values.
func ValidOpportunityVisibility(v OpportunityVisibility) bool {
	return v == OpportunityVisibilityInternal || v == OpportunityVisibilityPublic
}

// Actor names the actor performing a governance-relevant opportunity
// action (mark, approve, publicize). The consuming API resolves both
// fields from VERIFIED facts — the session principal and the agent flag
// (the authz.ClassOf discipline: an agent acts under the agent_default
// column, never a human membership) — never from client-supplied
// strings. The service trusts the port's resolved identity; a client
// that lies to the API cannot lie to the service, because the API never
// forwards client claims.
type Actor struct {
	// UserID is the resolved actor's user id.
	UserID string
	// IsAgent reports whether the actor is a platform agent (MCP/API
	// client acting under an agent identity). Mark, approve and
	// publicize refuse agent actors: an agent's only path into the
	// opportunity model is Suggest, and its suggestion becomes real only
	// through a human maintainer's Approve (requirement "agent
	// suggestions require approval"); publicizing is a docs/12 §3
	// visibility widening, which agents never execute (acceptance
	// "Agent 不能自动 publicize opportunity").
	IsAgent bool
}

// capabilityTagRe is the capability-tag shape: lowercase ascii, word
// characters and hyphens, no leading hyphen, bounded. The vocabulary is
// deliberately open (free-form tags) — pinning an enum would invent
// product semantics no spec provides; the shape keeps hostile input
// bounded and renderable.
var capabilityTagRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// MaxRequiredCapabilities bounds the capability list (generous but
// finite — a hostile client must not be able to store unbounded arrays).
const MaxRequiredCapabilities = 10

// ValidRequiredCapabilities reports whether caps is a storable
// capability list: at most MaxRequiredCapabilities lowercase
// hyphenated tags, no duplicates, no blanks.
func ValidRequiredCapabilities(caps []string) bool {
	if len(caps) > MaxRequiredCapabilities {
		return false
	}
	seen := make(map[string]bool, len(caps))
	for _, c := range caps {
		if !capabilityTagRe.MatchString(c) {
			return false
		}
		if seen[c] {
			return false
		}
		seen[c] = true
	}
	return true
}

// ValidOpportunityTitle reports whether title is a usable one-line
// summary (the same discipline as the PR title: non-blank and bounded).
func ValidOpportunityTitle(title string) bool {
	t := strings.TrimSpace(title)
	return t != "" && len(t) <= 200
}

// ValidOpportunityDescription reports whether description is storable
// context: empty, or bounded (the same discipline as the PR body).
func ValidOpportunityDescription(description string) bool {
	return len(description) <= 50_000
}

// SortedCapabilities returns a copy of caps sorted (the store keeps the
// array canonical so two lists of the same tags compare equal). The
// canonical form of an empty list is an empty array, never nil: the
// store inserts the value verbatim, and a nil slice would write NULL
// where the column requires '{}'.
func SortedCapabilities(caps []string) []string {
	out := slices.Clone(caps)
	if out == nil {
		return []string{}
	}
	slices.Sort(out)
	return out
}
