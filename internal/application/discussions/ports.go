package discussions

import (
	"context"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
)

// Ports (docs/52: the application orchestrates against ports; adapters live
// in internal/persistence).
//
// # What is deliberately ABSENT from this file
//
// There is no state-commit port here, and that is the structural half of
// this task's central guarantee: the comment path cannot change scientific
// state because the machinery that changes scientific state is not
// reachable from it. The package's own Deps (command.go) is asserted
// against this shape by a test that walks every port's method set and
// refuses a Commit; nothing in this file names
// internal/application/states, and the persistence adapter behind
// ThreadPort writes only the three discussion tables (00104).
//
// A promotion DOES create state in other tables — that is the point of it —
// and it does so by calling the services that own those tables: the RSG
// service for a hypothesis (a real state commit) and for an
// external-evidence proposal, and the promotion store for an Issue. Those
// are ordinary writes of the owning surfaces, reached through the narrow
// ports below, and the discussion package never performs a commit itself.

// ProjectAccessPort is the project read gate and the membership read: the
// SAME resolution every other project-scoped surface runs (T0106's read
// matrix). The production adapter is *projects.Service.
//
// Get answers the caller's own read rule — an anonymous caller reads a
// public project and is refused a private one — and is what makes
// "commenting needs an authenticated actor who may read the target" one
// decision rather than two. GetMembership answers the role the promotion's
// authorization is evaluated over.
type ProjectAccessPort interface {
	// Get returns the project as this reader may see it, or
	// projects.ErrProjectNotFound (unknown id, private project, reader not
	// a member — one outcome, existence hiding).
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
	// GetMembership returns the actor's membership or
	// projects.ErrMemberNotFound. It re-runs the read gate first, so an
	// invisible project answers projects.ErrProjectNotFound.
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// ForkGate resolves the fork condition of write_scientific_state
// (own_fork_only): whether a project is a fork OWNED by an actor — the
// condition specs/policies/permissions-matrix.csv names for an
// authenticated non-member. It is the same decision-shaped port the RSG
// service takes (the production implementation is *persistence.ForkStore),
// because a promotion is a write site of the same action and therefore has
// to resolve the same conditional cell.
//
// It is optional to construct — a command wired without one refuses the
// conditional path (fail closed), never assuming the condition holds.
type ForkGate interface {
	// OwnedFork reports whether projectID is a fork owned by actorID.
	OwnedFork(ctx context.Context, projectID, actorID string) (bool, error)
}

// IssueProposal is the Issue a promotion asks the store to create — the
// Issue surface's own fields, filled from the promoted proposal. It is a
// separate type from domain.Issue because the id, the number and the state
// are the store's to assign, and a caller that could name them could write
// a row the issues table's own rules never produced.
type IssueProposal struct {
	ProjectID string
	// IssueType is the caller's declaration, stored verbatim (trimmed,
	// non-blank; the column is free text and no specification chooses a
	// vocabulary for it).
	IssueType string
	Title     string
	Body      string
	CreatedBy string
}

// ThreadPort is the persistence port for the discussion rows themselves —
// threads, comments and the target existence check. The production adapter
// is persistence.DiscussionStore.
type ThreadPort interface {
	// CreateThreadWithComment opens one thread and writes its first
	// comment in ONE transaction: a thread with no comment is an empty
	// conversation, and a comment without its thread cannot exist.
	CreateThreadWithComment(ctx context.Context, t domain.DiscussionThread, c domain.DiscussionComment) (domain.DiscussionThread, domain.DiscussionComment, error)
	// GetThread returns one thread of the project, or ErrThreadNotFound
	// (a thread of another project answers the same).
	GetThread(ctx context.Context, projectID, threadID string) (domain.DiscussionThread, error)
	// ListThreads returns one target's threads, oldest first, with each
	// thread's live comment count and last activity.
	ListThreads(ctx context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error)
	// CreateComment appends one comment to an existing thread.
	CreateComment(ctx context.Context, c domain.DiscussionComment) (domain.DiscussionComment, error)
	// GetComment returns one comment of the project, or ErrCommentNotFound.
	GetComment(ctx context.Context, projectID, commentID string) (domain.DiscussionComment, error)
	// ListComments returns one thread's comments in creation order,
	// withdrawn ones included (the row is never removed).
	ListComments(ctx context.Context, threadID string) ([]domain.DiscussionComment, error)
	// DeleteComment writes the tombstone and returns the row. A comment
	// already carrying one answers ErrCommentDeleted; an unknown id (or
	// one of another project) answers ErrCommentNotFound. It never deletes
	// a row.
	DeleteComment(ctx context.Context, projectID, commentID, deletedBy string) (domain.DiscussionComment, error)
	// TargetExists reports whether a thread's target exists inside the
	// project. It covers the two kinds whose existence is a row in another
	// table: knowledge (a publication of this project, addressed by its
	// pid) and pull_request (a PR of this project, addressed by number).
	// The project kind does not need it — a thread on its own project
	// names that project, which the command has already resolved — and an
	// adapter may answer it by comparing the two ids.
	TargetExists(ctx context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) (bool, error)
}

// PromotionPort is the persistence port for the promotion records. The
// production adapter is persistence.DiscussionStore.
//
// # Why an Issue promotion is ONE method and the other two are two
//
// Promoting to an Issue creates no scientific state: the issue row, the
// promotion row and the audit row are three ordinary rows and they commit
// together or not at all, so the store owns the whole transaction
// (PromoteToIssue).
//
// A Hypothesis or an external-evidence proposal is created by the service
// that owns it, as a real state commit — the discussion command calls that
// service and then records the promotion (RecordPromotion). The order is
// deliberate and is the honest one: the promoted object is the fact, and
// the promotion row records where it came from. Writing the record first
// would name an object that does not exist yet; if the record write fails
// after the object exists, the object stands and only its origin is
// missing — never the other way round.
type PromotionPort interface {
	// PromoteToIssue writes the issue row, the promotion row and the audit
	// row in one transaction and returns the stored issue and promotion.
	// The issue number is allocated inside that transaction under the
	// project row lock.
	PromoteToIssue(ctx context.Context, in IssueProposal, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.Issue, domain.DiscussionPromotion, error)
	// RecordPromotion writes one promotion row (and its audit row) for an
	// object another service has already created.
	RecordPromotion(ctx context.Context, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.DiscussionPromotion, error)
	// GetPromotion returns one promotion of the project, or
	// ErrPromotionNotFound.
	GetPromotion(ctx context.Context, projectID, promotionID string) (domain.DiscussionPromotion, error)
	// ListPromotionsByRef returns every promotion of one object, oldest
	// first — the reverse read that answers "which discussion proposed
	// this" for a promoted Issue, Hypothesis or evidence proposal.
	ListPromotionsByRef(ctx context.Context, projectID, ref string) ([]domain.DiscussionPromotion, error)
}

// HypothesisPort creates a scientific object through the RSG write path:
// the production implementation is *rsg.Service, and its CreateObject
// authorizes, resolves the branch and the schema, and commits a real state
// transition (states.Commit) — the discussion command neither knows nor
// re-implements any of that.
type HypothesisPort interface {
	// CreateObject creates an object and its version 1 as one state
	// commit. It answers rsg.ErrForbidden / rsg.ErrValidation and the
	// owning packages' sentinels (a branch that does not exist, a payload
	// the semantic checks refuse), which this command maps onto its own.
	CreateObject(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateObjectInput) (rsg.ObjectResult, error)
}

// EvidencePort writes an evidence assertion through the RSG write path: the
// production implementation is *rsg.Service. CreateEvidenceAssertion
// derives the assertion's origin and visibility, refuses a literature
// assertion that names no evidence unit, and commits a real state
// transition.
//
// The created row carries review_state = 'unreviewed' — the column's own
// default (00007) and the shape of a PROPOSAL (00058: unreviewed →
// reviewed → rejected is a legitimate in-place change, so reviewing it
// later is an ordinary transition, not a new object). That is why this
// flow creates no "proposal" table: an unreviewed assertion IS the pending
// proposal.
type EvidencePort interface {
	CreateEvidenceAssertion(ctx context.Context, actor domain.User, projectID, branchID string, in rsg.CreateEvidenceAssertionInput) (rsg.EvidenceAssertionResult, error)
}
