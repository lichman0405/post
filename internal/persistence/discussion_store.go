package persistence

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/discussions"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// DiscussionStore is the production discussion adapter over PostgreSQL
// (queries/discussions.sql, migration 00104): the threads and comments of a
// project's conversations, the target existence check, and the promotion
// records that carry a discussion's provenance across to the object it
// became.
//
// # Two properties this adapter holds, and where each one lives
//
//   - The ordinary writes (a thread with its opening comment, an appended
//     comment, a tombstone) touch discussion_threads and discussion_comments
//     and nothing else. No statement in queries/discussions.sql names
//     scientific_object_versions, relation_versions or contribution_events,
//     so "a comment does not move scientific state" holds at the SQL layer
//     too — not only because the command has no state-commit port.
//
//   - The promotion write that CREATES an object is the Issue one:
//     PromoteToIssue writes the issues row (funded by the per-project number
//     allocator), the promotion row and the audit row in one transaction.
//     The other two kinds are created by the services that own them (the RSG
//     write path and the evidence write path), and this store only records
//     the promotion afterwards (RecordPromotion).
type DiscussionStore struct {
	pool *pgxpool.Pool
}

// NewDiscussionStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewDiscussionStore(pool *pgxpool.Pool) *DiscussionStore {
	return &DiscussionStore{pool: pool}
}

// CreateThreadWithComment implements discussions.ThreadPort. The thread and
// its opening comment are one transaction: a thread nobody wrote in is an
// empty conversation that no surface renders, and a comment whose thread was
// not written cannot exist.
func (s *DiscussionStore) CreateThreadWithComment(ctx context.Context, t domain.DiscussionThread, c domain.DiscussionComment) (domain.DiscussionThread, domain.DiscussionComment, error) {
	projectID, err := textUUID(t.ProjectID)
	if err != nil {
		return domain.DiscussionThread{}, domain.DiscussionComment{}, discussions.ErrProjectNotFound
	}
	createdBy, err := textUUID(t.CreatedBy)
	if err != nil {
		return domain.DiscussionThread{}, domain.DiscussionComment{}, fmt.Errorf("persistence: discussion author id: %w", err)
	}
	var thread domain.DiscussionThread
	var comment domain.DiscussionComment
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		row, err := q.CreateDiscussionThread(ctx, sqlc.CreateDiscussionThreadParams{
			ProjectID:  projectID,
			TargetType: string(t.TargetKind),
			TargetID:   t.TargetID,
			CreatedBy:  createdBy,
		})
		if err != nil {
			return err
		}
		thread, err = discussionThreadFromRow(row)
		if err != nil {
			return err
		}
		comment, err = createCommentInTx(ctx, q, sqlc.CreateDiscussionCommentParams{
			ThreadID:  row.ID,
			ProjectID: projectID,
			Body:      c.Body,
			CreatedBy: createdBy,
		})
		return err
	})
	if err != nil {
		return domain.DiscussionThread{}, domain.DiscussionComment{}, err
	}
	return thread, comment, nil
}

// GetThread implements discussions.ThreadPort. The project id is in the
// query's predicate, never a filter applied afterwards: a thread of another
// project is not-found, not a foreign thread (existence hiding, docs/45).
func (s *DiscussionStore) GetThread(ctx context.Context, projectID, threadID string) (domain.DiscussionThread, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return domain.DiscussionThread{}, discussions.ErrThreadNotFound
	}
	id, err := textUUID(threadID)
	if err != nil {
		return domain.DiscussionThread{}, discussions.ErrThreadNotFound
	}
	row, err := sqlc.New(s.pool).GetDiscussionThread(ctx, sqlc.GetDiscussionThreadParams{ProjectID: project, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.DiscussionThread{}, discussions.ErrThreadNotFound
		}
		return domain.DiscussionThread{}, err
	}
	return discussionThreadFromRow(row)
}

// ListThreads implements discussions.ThreadPort: one target's threads,
// oldest first, each with its live comment count and last activity. The
// count includes only comments that still stand — a withdrawn comment is
// retained but is no longer part of the conversation the list renders.
func (s *DiscussionStore) ListThreads(ctx context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return nil, discussions.ErrProjectNotFound
	}
	rows, err := sqlc.New(s.pool).ListDiscussionThreads(ctx, sqlc.ListDiscussionThreadsParams{
		ProjectID:  project,
		TargetType: string(kind),
		TargetID:   targetID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.DiscussionThread, 0, len(rows))
	for _, row := range rows {
		thread, err := discussionThreadFromListRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, thread)
	}
	return out, nil
}

// CreateComment implements discussions.ThreadPort: one appended comment.
func (s *DiscussionStore) CreateComment(ctx context.Context, c domain.DiscussionComment) (domain.DiscussionComment, error) {
	projectID, err := textUUID(c.ProjectID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrProjectNotFound
	}
	threadID, err := textUUID(c.ThreadID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrThreadNotFound
	}
	createdBy, err := textUUID(c.CreatedBy)
	if err != nil {
		return domain.DiscussionComment{}, fmt.Errorf("persistence: discussion comment author id: %w", err)
	}
	return createCommentInTx(ctx, sqlc.New(s.pool), sqlc.CreateDiscussionCommentParams{
		ThreadID:  threadID,
		ProjectID: projectID,
		Body:      c.Body,
		CreatedBy: createdBy,
	})
}

// GetComment implements discussions.ThreadPort. A comment of another
// project — or an unknown id, or an id that is not a uuid at all — answers
// ErrCommentNotFound: one outcome, no foreign existence leak (docs/45).
func (s *DiscussionStore) GetComment(ctx context.Context, projectID, commentID string) (domain.DiscussionComment, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrCommentNotFound
	}
	id, err := textUUID(commentID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrCommentNotFound
	}
	row, err := sqlc.New(s.pool).GetDiscussionComment(ctx, sqlc.GetDiscussionCommentParams{ProjectID: project, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.DiscussionComment{}, discussions.ErrCommentNotFound
		}
		return domain.DiscussionComment{}, err
	}
	return discussionCommentFromRow(row)
}

// ListComments implements discussions.ThreadPort: one thread's comments in
// creation order. Withdrawn comments are RETURNED — the row is never removed
// (CLAUDE.md §9.8) and the caller renders the tombstone — and the caller has
// already resolved the thread inside its project.
func (s *DiscussionStore) ListComments(ctx context.Context, threadID string) ([]domain.DiscussionComment, error) {
	id, err := textUUID(threadID)
	if err != nil {
		return nil, discussions.ErrThreadNotFound
	}
	rows, err := sqlc.New(s.pool).ListDiscussionComments(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]domain.DiscussionComment, 0, len(rows))
	for _, row := range rows {
		comment, err := discussionCommentFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, comment)
	}
	return out, nil
}

// DeleteComment implements discussions.ThreadPort: the tombstone is the only
// write, and the UPDATE's `deleted_at IS NULL` predicate is the
// compare-and-swap that keeps a second withdrawal from overwriting the first
// tombstone's author.
//
// A CAS that matched nothing means either "no such comment" or "already
// withdrawn", and the two are different answers — so the store reads once
// more to tell them apart rather than reporting both as not-found. That read
// races nothing that matters: the only way to reach it is a concurrent
// withdrawal, whose outcome either way is "this comment carries a
// tombstone".
func (s *DiscussionStore) DeleteComment(ctx context.Context, projectID, commentID, deletedBy string) (domain.DiscussionComment, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrCommentNotFound
	}
	id, err := textUUID(commentID)
	if err != nil {
		return domain.DiscussionComment{}, discussions.ErrCommentNotFound
	}
	by, err := textUUID(deletedBy)
	if err != nil {
		return domain.DiscussionComment{}, fmt.Errorf("persistence: discussion comment deleter id: %w", err)
	}
	row, err := sqlc.New(s.pool).SoftDeleteDiscussionComment(ctx, sqlc.SoftDeleteDiscussionCommentParams{
		DeletedBy: by,
		ProjectID: project,
		ID:        id,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			_, readErr := s.GetComment(ctx, projectID, commentID)
			if readErr == nil {
				return domain.DiscussionComment{}, discussions.ErrCommentDeleted
			}
			return domain.DiscussionComment{}, readErr
		}
		return domain.DiscussionComment{}, err
	}
	return discussionCommentFromRow(row)
}

// TargetExists implements discussions.ThreadPort for the two kinds whose
// existence is decided by another table: a knowledge target is a publication
// of THIS project (addressed by its pid), and a pull request target is a
// pull_requests row of this project (addressed by its number). The project
// kind is decided by comparing the target id to the project itself — the
// same pair 00104's CHECK pins — so it needs no table of its own.
func (s *DiscussionStore) TargetExists(ctx context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) (bool, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return false, nil
	}
	q := sqlc.New(s.pool)
	switch kind {
	case domain.DiscussionTargetProject:
		target, err := textUUID(targetID)
		if err != nil {
			return false, nil
		}
		return target.Bytes == project.Bytes, nil
	case domain.DiscussionTargetKnowledge:
		owner, err := q.GetDiscussionKnowledgeTargetProject(ctx, targetID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return owner.Bytes == project.Bytes, nil
	case domain.DiscussionTargetPullRequest:
		number, err := strconv.ParseInt(strings.TrimSpace(targetID), 10, 64)
		if err != nil {
			return false, nil
		}
		_, err = q.GetPullRequestByProjectAndNumber(ctx, sqlc.GetPullRequestByProjectAndNumberParams{
			ProjectID: project,
			Number:    number,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	default:
		// Unknown kinds are not this port's to judge — the command validates
		// the kind before asking — and a false here keeps the caller's
		// answer fail-closed rather than admitting a target nothing checked.
		return false, nil
	}
}

// PromoteToIssue implements discussions.PromotionPort. One transaction
// writes the issue row, the promotion record and the audit row:
//
//  1. the project row is locked (GetProjectByIDForUpdate — the same lock
//     every project-scoped create takes), so the number allocation below
//     cannot race a concurrent create;
//  2. the number is the project's next issue number (MAX(number)+1 under
//     that lock; per-project and starting at 1, like the PR surface's own
//     allocation), never a global sequence and never a chosen value;
//  3. the issue row is written with state 'open' (the column's default) and
//     the promoted proposal's own words;
//  4. the promotion row renders its ref from the assigned issue id, so the
//     pair the CHECK in 00104 pins cannot disagree with what was created;
//  5. the audit row names that ref as its target (the store writes it after
//     the rows exist), so the governance record of the promotion and the
//     promotion itself commit or roll back together.
func (s *DiscussionStore) PromoteToIssue(ctx context.Context, in discussions.IssueProposal, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.Issue, domain.DiscussionPromotion, error) {
	projectID, err := textUUID(in.ProjectID)
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, discussions.ErrProjectNotFound
	}
	createdBy, err := textUUID(in.CreatedBy)
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, fmt.Errorf("persistence: promoted issue creator id: %w", err)
	}
	threadID, err := textUUID(promotion.ThreadID)
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, discussions.ErrThreadNotFound
	}
	commentID, err := textUUID(promotion.CommentID)
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, discussions.ErrCommentNotFound
	}
	promotedBy, err := textUUID(promotion.PromotedBy)
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, fmt.Errorf("persistence: promoting actor id: %w", err)
	}
	var issue domain.Issue
	var recorded domain.DiscussionPromotion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return discussions.ErrProjectNotFound
			}
			return err
		}
		number, err := q.NextIssueNumber(ctx, projectID)
		if err != nil {
			return err
		}
		row, err := q.CreateIssue(ctx, sqlc.CreateIssueParams{
			ProjectID: projectID,
			Number:    number,
			IssueType: in.IssueType,
			Title:     in.Title,
			Body:      in.Body,
			CreatedBy: createdBy,
		})
		if err != nil {
			return err
		}
		issue, err = issueFromRow(row)
		if err != nil {
			return err
		}
		recorded, err = createPromotionInTx(ctx, q, sqlc.CreateDiscussionPromotionParams{
			ProjectID:    projectID,
			ThreadID:     threadID,
			CommentID:    commentID,
			PromotedKind: string(promotion.Kind),
			PromotedRef:  domain.PromotionRef(promotion.Kind, issue.ID),
			PromotedBy:   promotedBy,
		})
		if err != nil {
			return err
		}
		audit.TargetRef = recorded.Ref
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.Issue{}, domain.DiscussionPromotion{}, err
	}
	return issue, recorded, nil
}

// RecordPromotion implements discussions.PromotionPort: one transaction
// writes the promotion row for an object another service has already
// created, and the audit row that names it. The ref arrives from the
// caller, which is the only party that knows the created object's id.
func (s *DiscussionStore) RecordPromotion(ctx context.Context, promotion domain.DiscussionPromotion, audit domain.AuditEntry) (domain.DiscussionPromotion, error) {
	projectID, err := textUUID(promotion.ProjectID)
	if err != nil {
		return domain.DiscussionPromotion{}, discussions.ErrProjectNotFound
	}
	threadID, err := textUUID(promotion.ThreadID)
	if err != nil {
		return domain.DiscussionPromotion{}, discussions.ErrThreadNotFound
	}
	commentID, err := textUUID(promotion.CommentID)
	if err != nil {
		return domain.DiscussionPromotion{}, discussions.ErrCommentNotFound
	}
	promotedBy, err := textUUID(promotion.PromotedBy)
	if err != nil {
		return domain.DiscussionPromotion{}, fmt.Errorf("persistence: promoting actor id: %w", err)
	}
	var recorded domain.DiscussionPromotion
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		var err error
		recorded, err = createPromotionInTx(ctx, q, sqlc.CreateDiscussionPromotionParams{
			ProjectID:    projectID,
			ThreadID:     threadID,
			CommentID:    commentID,
			PromotedKind: string(promotion.Kind),
			PromotedRef:  promotion.Ref,
			PromotedBy:   promotedBy,
		})
		if err != nil {
			return err
		}
		audit.TargetRef = recorded.Ref
		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		return domain.DiscussionPromotion{}, err
	}
	return recorded, nil
}

// GetPromotion implements discussions.PromotionPort.
func (s *DiscussionStore) GetPromotion(ctx context.Context, projectID, promotionID string) (domain.DiscussionPromotion, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return domain.DiscussionPromotion{}, discussions.ErrPromotionNotFound
	}
	id, err := textUUID(promotionID)
	if err != nil {
		return domain.DiscussionPromotion{}, discussions.ErrPromotionNotFound
	}
	row, err := sqlc.New(s.pool).GetDiscussionPromotion(ctx, sqlc.GetDiscussionPromotionParams{ProjectID: project, ID: id})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.DiscussionPromotion{}, discussions.ErrPromotionNotFound
		}
		return domain.DiscussionPromotion{}, err
	}
	return discussionPromotionFromRow(row)
}

// ListPromotionsByRef implements discussions.PromotionPort: every promotion
// that produced the object a ref names, oldest first. The ref is parsed
// rather than matched as a string, so the query's kind and ref always agree
// with each other — the same pair the CHECK in 00104 pins on the way in.
func (s *DiscussionStore) ListPromotionsByRef(ctx context.Context, projectID, ref string) ([]domain.DiscussionPromotion, error) {
	project, err := textUUID(projectID)
	if err != nil {
		return nil, discussions.ErrProjectNotFound
	}
	kind, _, ok := domain.SplitPromotionRef(ref)
	if !ok {
		return nil, fmt.Errorf("%w: a promoted ref is \"<kind>:<id>\" with a known kind", discussions.ErrValidation)
	}
	rows, err := sqlc.New(s.pool).ListDiscussionPromotionsByRef(ctx, sqlc.ListDiscussionPromotionsByRefParams{
		ProjectID:    project,
		PromotedKind: string(kind),
		PromotedRef:  ref,
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.DiscussionPromotion, 0, len(rows))
	for _, row := range rows {
		promotion, err := discussionPromotionFromRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, promotion)
	}
	return out, nil
}

// createCommentInTx writes one comment through the caller's queries, so it
// composes into the thread's transaction and into a standalone append.
func createCommentInTx(ctx context.Context, q *sqlc.Queries, params sqlc.CreateDiscussionCommentParams) (domain.DiscussionComment, error) {
	row, err := q.CreateDiscussionComment(ctx, params)
	if err != nil {
		return domain.DiscussionComment{}, err
	}
	return discussionCommentFromRow(row)
}

// createPromotionInTx writes one promotion record through the caller's
// queries and converts it.
func createPromotionInTx(ctx context.Context, q *sqlc.Queries, params sqlc.CreateDiscussionPromotionParams) (domain.DiscussionPromotion, error) {
	row, err := q.CreateDiscussionPromotion(ctx, params)
	if err != nil {
		return domain.DiscussionPromotion{}, err
	}
	return discussionPromotionFromRow(row)
}

// discussionThreadFromRow converts the stored thread row.
func discussionThreadFromRow(row sqlc.DiscussionThread) (domain.DiscussionThread, error) {
	kind, err := discussionTargetKind(row.TargetType)
	if err != nil {
		return domain.DiscussionThread{}, err
	}
	return domain.DiscussionThread{
		ID:         pgUUIDToText(row.ID),
		ProjectID:  pgUUIDToText(row.ProjectID),
		TargetKind: kind,
		TargetID:   row.TargetID,
		CreatedBy:  pgUUIDToText(row.CreatedBy),
		CreatedAt:  row.CreatedAt.Time,
	}, nil
}

// discussionThreadFromListRow converts the list row (the thread plus the
// two aggregate columns the list renders).
func discussionThreadFromListRow(row sqlc.ListDiscussionThreadsRow) (domain.DiscussionThread, error) {
	kind, err := discussionTargetKind(row.TargetType)
	if err != nil {
		return domain.DiscussionThread{}, err
	}
	return domain.DiscussionThread{
		ID:            pgUUIDToText(row.ID),
		ProjectID:     pgUUIDToText(row.ProjectID),
		TargetKind:    kind,
		TargetID:      row.TargetID,
		CreatedBy:     pgUUIDToText(row.CreatedBy),
		CreatedAt:     row.CreatedAt.Time,
		CommentCount:  row.CommentCount,
		LastCommentAt: timestamptzPtr(row.LastCommentAt),
	}, nil
}

// discussionCommentFromRow converts the stored comment row, tombstone and
// all (the tombstone is data — it is converted, never dropped).
func discussionCommentFromRow(row sqlc.DiscussionComment) (domain.DiscussionComment, error) {
	return domain.DiscussionComment{
		ID:        pgUUIDToText(row.ID),
		ThreadID:  pgUUIDToText(row.ThreadID),
		ProjectID: pgUUIDToText(row.ProjectID),
		Body:      row.Body,
		CreatedBy: pgUUIDToText(row.CreatedBy),
		CreatedAt: row.CreatedAt.Time,
		DeletedAt: timestamptzPtr(row.DeletedAt),
		DeletedBy: uuidPtr(row.DeletedBy),
	}, nil
}

// discussionPromotionFromRow converts the stored promotion row.
func discussionPromotionFromRow(row sqlc.DiscussionPromotion) (domain.DiscussionPromotion, error) {
	kind, _, ok := domain.SplitPromotionRef(row.PromotedRef)
	if !ok {
		return domain.DiscussionPromotion{}, fmt.Errorf("persistence: stored promotion ref %q is not \"<kind>:<id>\"", row.PromotedRef)
	}
	if string(kind) != row.PromotedKind {
		return domain.DiscussionPromotion{}, fmt.Errorf("persistence: stored promotion kind %q disagrees with its ref %q", row.PromotedKind, row.PromotedRef)
	}
	return domain.DiscussionPromotion{
		ID:         pgUUIDToText(row.ID),
		ProjectID:  pgUUIDToText(row.ProjectID),
		ThreadID:   pgUUIDToText(row.ThreadID),
		CommentID:  pgUUIDToText(row.CommentID),
		Kind:       kind,
		Ref:        row.PromotedRef,
		PromotedBy: pgUUIDToText(row.PromotedBy),
		PromotedAt: row.PromotedAt.Time,
	}, nil
}

// issueFromRow converts the stored issue row.
func issueFromRow(row sqlc.Issue) (domain.Issue, error) {
	state := domain.IssueState(row.State)
	if !domain.ValidIssueState(state) {
		return domain.Issue{}, fmt.Errorf("persistence: stored issue state %q is not a known state", row.State)
	}
	return domain.Issue{
		ID:        pgUUIDToText(row.ID),
		ProjectID: pgUUIDToText(row.ProjectID),
		Number:    row.Number,
		IssueType: row.IssueType,
		Title:     row.Title,
		Body:      row.Body,
		State:     state,
		CreatedBy: pgUUIDToText(row.CreatedBy),
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

// discussionTargetKind parses a stored target_type back into the domain
// value, refusing a token the domain does not know (the column's CHECK makes
// it impossible, and a store that silently accepted one would be the only
// place the two vocabularies could drift).
func discussionTargetKind(stored string) (domain.DiscussionTargetKind, error) {
	kind := domain.DiscussionTargetKind(stored)
	if !domain.ValidDiscussionTargetKind(kind) {
		return "", fmt.Errorf("persistence: stored discussion target type %q is not a known kind", stored)
	}
	return kind, nil
}
