package discussions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
)

// Command is the discussion use case (T0811): a conversation attached to a
// project, a published knowledge object or a research pull request, and the
// one thing that conversation can do besides accumulate messages — promote
// one comment into a proposed research object.
//
// # The shape of the whole feature, in one place
//
// A discussion is a version-less member row (internal/domain/discussion.go).
// Opening a thread, writing a comment and withdrawing a comment therefore
// end at ThreadPort, which writes the three 00104 tables and nothing else:
// no state commit runs, no scientific_object_versions or relation_versions
// row is written, and no contribution_events row is produced. Nothing in
// this package can violate that, because the machine that would do any of
// those things is not wired into it (ports.go).
//
// Promoting a comment is the opposite act by design: it CREATES a row in
// another surface's table — an Issue (issues), a Hypothesis (scientific
// objects, through the RSG write path's real state commit) or an external
// evidence proposal (evidence_assertions, stored unreviewed) — and records
// the origin in discussion_promotions. The created object belongs to the
// surface that owns it; this command calls that surface's own command and
// never writes its tables itself.
//
// # Authorization
//
// Every method runs the project read gate first (projects.Reader — the same
// resolution every other project-scoped surface runs), and every WRITE then
// needs an authenticated actor. A promotion additionally evaluates the
// matrix cell of write_scientific_state for the actor's class — the ruling
// that a promotion is a scientific-state-adjacent governance action — and
// resolves the conditional cell exactly as the RSG write path does. There
// is no action of this feature's own: the discussion surface adds no row to
// the permission matrix (the task's ruling; specs/policies is untouched).
type Command struct {
	projects   ProjectAccessPort
	threads    ThreadPort
	promotions PromotionPort
	hypotheses HypothesisPort
	evidence   EvidencePort
	authz      authz.Engine
	forks      ForkGate
}

// Deps wires the discussion command over its ports. Threads, Promotions,
// Projects and Authz are required; a command missing one refuses at call
// time rather than guessing (fail closed).
//
// Hypotheses and Evidence are the promotion creators and are only needed by
// the promotion kinds that use them — but a wired command that lacks the
// one a request names refuses that request (it never falls back to creating
// the object itself). ForkGate resolves the own_fork_only cell of
// write_scientific_state; nil refuses that conditional path (fail closed),
// exactly like the RSG service.
type Deps struct {
	Projects   ProjectAccessPort
	Threads    ThreadPort
	Promotions PromotionPort
	Hypotheses HypothesisPort
	Evidence   EvidencePort
	Authz      authz.Engine
	ForkGate   ForkGate
}

// NewCommand wires the command over its ports.
func NewCommand(deps Deps) *Command {
	return &Command{
		projects:   deps.Projects,
		threads:    deps.Threads,
		promotions: deps.Promotions,
		hypotheses: deps.Hypotheses,
		evidence:   deps.Evidence,
		authz:      deps.Authz,
		forks:      deps.ForkGate,
	}
}

// ThreadResult is one thread with the comments that belong to it, in
// creation order. Comments carry their tombstones when they were withdrawn
// (the row is never removed); the transport stops serving the body.
type ThreadResult struct {
	Thread   domain.DiscussionThread
	Comments []domain.DiscussionComment
}

// OpenThreadParams names a new conversation: the project it lives in, what
// it is about (in that target's own addressing scheme — see the
// DiscussionTargetKind constants) and its first comment.
type OpenThreadParams struct {
	ProjectID  string
	TargetKind domain.DiscussionTargetKind
	TargetID   string
	// Body is the opening comment. A thread is never empty: the target
	// names the conversation, the first comment starts it.
	Body string
}

// OpenThread opens one thread and writes its first comment in one
// transaction:
//
//  1. validate the shape (a known target kind, a non-blank target id, a
//     non-blank bounded body);
//  2. require an authenticated actor and run the project read gate — a
//     caller who may not read the project gets not-found (existence
//     hiding, docs/45), which is also the whole of "may comment here":
//     reading the target is the permission commenting it needs;
//  3. check the target exists in THIS project — the project itself for the
//     project kind, a publication of this project for a knowledge target,
//     a pull request of this project for the PR kind. An unknown target, or
//     one belonging to another project, answers ErrTargetNotFound (one
//     outcome, no foreign existence leak);
//  4. write the thread and its opening comment together.
func (c *Command) OpenThread(ctx context.Context, actor domain.User, in OpenThreadParams) (ThreadResult, error) {
	kind, targetID, body, err := validateOpenThread(in)
	if err != nil {
		return ThreadResult{}, err
	}
	project, err := c.visibleToActor(ctx, actor, in.ProjectID)
	if err != nil {
		return ThreadResult{}, err
	}
	if err := c.requireTarget(ctx, project.ID, kind, targetID); err != nil {
		return ThreadResult{}, err
	}
	if c.threads == nil {
		return ThreadResult{}, fmt.Errorf("%w: no discussion store configured", ErrStore)
	}
	thread, comment, err := c.threads.CreateThreadWithComment(ctx,
		domain.DiscussionThread{
			ProjectID:  project.ID,
			TargetKind: kind,
			TargetID:   targetID,
			CreatedBy:  actor.ID,
		},
		domain.DiscussionComment{
			ProjectID: project.ID,
			Body:      body,
			CreatedBy: actor.ID,
		})
	if err != nil {
		return ThreadResult{}, mapStoreError(err)
	}
	return ThreadResult{Thread: thread, Comments: []domain.DiscussionComment{comment}}, nil
}

// ListThreads returns one target's threads, oldest first, with each
// thread's live comment count and last activity. It runs the project read
// gate: a discussion is exactly as visible as the project carrying it.
//
// The target is NOT re-checked for existence here: a target that has
// threads necessarily exists (a thread is only written for a target the
// command verified), and a target that has none renders an empty list —
// which is the same answer whether the target is gone or was never
// discussed. Reads of a foreign target stay inside the project's own scope
// either way.
func (c *Command) ListThreads(ctx context.Context, r projects.Reader, projectID string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error) {
	if !domain.ValidDiscussionTargetKind(kind) {
		return nil, fmt.Errorf("%w: unknown discussion target kind %q", ErrValidation, kind)
	}
	if strings.TrimSpace(targetID) == "" {
		return nil, fmt.Errorf("%w: target_id is required", ErrValidation)
	}
	project, err := c.visibleTo(ctx, r, projectID)
	if err != nil {
		return nil, err
	}
	if c.threads == nil {
		return nil, fmt.Errorf("%w: no discussion store configured", ErrStore)
	}
	threads, err := c.threads.ListThreads(ctx, project.ID, kind, targetID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return threads, nil
}

// GetThread returns one thread and its comments. A thread of another
// project — or an unknown id — answers ErrThreadNotFound (existence
// hiding, docs/45); visibility was resolved by the read gate.
func (c *Command) GetThread(ctx context.Context, r projects.Reader, projectID, threadID string) (ThreadResult, error) {
	if strings.TrimSpace(threadID) == "" {
		return ThreadResult{}, fmt.Errorf("%w: thread_id is required", ErrValidation)
	}
	project, err := c.visibleTo(ctx, r, projectID)
	if err != nil {
		return ThreadResult{}, err
	}
	thread, err := c.thread(ctx, project.ID, threadID)
	if err != nil {
		return ThreadResult{}, err
	}
	comments, err := c.threads.ListComments(ctx, thread.ID)
	if err != nil {
		return ThreadResult{}, mapStoreError(err)
	}
	return ThreadResult{Thread: thread, Comments: comments}, nil
}

// AddCommentParams names one message appended to an existing thread.
type AddCommentParams struct {
	ProjectID string
	ThreadID  string
	Body      string
}

// AddComment appends one comment to a thread. It requires an authenticated
// actor and the project read gate, resolves the thread inside the project,
// and writes exactly one discussion_comments row — the write that must not
// move scientific state, and structurally cannot: no port this command
// holds reaches a state commit (ports.go).
//
// A comment is written to a deleted thread's parent? There is no deleted
// thread: a thread is never withdrawn, only its comments are, so
// "may I comment here" is decided by the read gate alone.
func (c *Command) AddComment(ctx context.Context, actor domain.User, in AddCommentParams) (domain.DiscussionComment, error) {
	if strings.TrimSpace(in.ThreadID) == "" {
		return domain.DiscussionComment{}, fmt.Errorf("%w: thread_id is required", ErrValidation)
	}
	body := strings.TrimSpace(in.Body)
	if !domain.ValidDiscussionBody(body) {
		return domain.DiscussionComment{}, fmt.Errorf("%w: a comment body is required and is at most %d characters",
			ErrValidation, domain.MaxDiscussionBodyLen)
	}
	project, err := c.visibleToActor(ctx, actor, in.ProjectID)
	if err != nil {
		return domain.DiscussionComment{}, err
	}
	thread, err := c.thread(ctx, project.ID, in.ThreadID)
	if err != nil {
		return domain.DiscussionComment{}, err
	}
	comment, err := c.threads.CreateComment(ctx, domain.DiscussionComment{
		ThreadID:  thread.ID,
		ProjectID: project.ID,
		Body:      body,
		CreatedBy: actor.ID,
	})
	if err != nil {
		return domain.DiscussionComment{}, mapStoreError(err)
	}
	return comment, nil
}

// DeleteComment withdraws one comment: it writes the tombstone (when, by
// whom) and never deletes the row (CLAUDE.md §9.8 — nothing disappears,
// state only evolves). Only the comment's author may withdraw it.
//
// # The author-only rule, and why it is flagged rather than assumed
//
// The task package does not settle who may withdraw a comment. Author-only
// is the reading this command implements: withdrawal is the author taking
// back their own words, and a project maintainer who could withdraw someone
// else's words would need a governance action the matrix does not name
// (specs/policies has no "moderate discussion" cell, and this task may not
// add one). The open question — whether a maintainer should be able to hide
// a comment administratively — is recorded for the Supervisor rather than
// answered by inventing a rule here.
//
// A second withdrawal of the same comment answers ErrCommentDeleted: the
// tombstone names who withdrew it, and letting another actor overwrite it
// would rewrite that record.
func (c *Command) DeleteComment(ctx context.Context, actor domain.User, projectID, commentID string) (domain.DiscussionComment, error) {
	if strings.TrimSpace(commentID) == "" {
		return domain.DiscussionComment{}, fmt.Errorf("%w: comment_id is required", ErrValidation)
	}
	project, err := c.visibleToActor(ctx, actor, projectID)
	if err != nil {
		return domain.DiscussionComment{}, err
	}
	comment, err := c.comment(ctx, project.ID, commentID)
	if err != nil {
		return domain.DiscussionComment{}, err
	}
	if comment.Deleted() {
		return domain.DiscussionComment{}, ErrCommentDeleted
	}
	if comment.CreatedBy != actor.ID {
		return domain.DiscussionComment{}, ErrForbidden
	}
	deleted, err := c.threads.DeleteComment(ctx, project.ID, comment.ID, actor.ID)
	if err != nil {
		return domain.DiscussionComment{}, mapStoreError(err)
	}
	return deleted, nil
}

// The one object type a promotion to a hypothesis creates. It is written
// out rather than passed in: the promoted object's KIND is what the
// promotion decision chose (the matrix of promotion kinds has one member
// for it), and a caller free to name any object type would make
// "promote to Hypothesis" mean "promote to whatever".
const hypothesisObjectType = "hypothesis"

// HypothesisProposal carries the one thing a promotion to a Hypothesis must
// add to the comment it promotes: the research question the hypothesis
// addresses (the hypothesis schema's question_id). Everything else the
// created object says is the proposal's own text — the comment's body
// becomes the statement, and the RSG write path derives the title from it
// (rsg's titleFromPayload, the rule every hypothesis write already
// follows).
type HypothesisProposal struct {
	// QuestionID names the research question this hypothesis answers. It is
	// required: the hypothesis schema carries question_id as a required
	// field, and docs/08 makes naming the question what lets a reviewer
	// trace the hypothesis to it.
	QuestionID string
}

// EvidenceProposal carries what a promotion to an external-evidence
// proposal must add to the comment it promotes: the two version pins, the
// canonical relation and evidence type, and the optional declarations the
// evidence-assertion write path already accepts. The comment's body becomes
// the assertion's reasoning note — the proposal's words are the author's
// explanation of the evidence.
//
// The created row is created by that surface's own command
// (rsg.Service.CreateEvidenceAssertion) and lands in its default
// review_state 'unreviewed': the assertion IS the pending proposal, which
// is why this flow creates no proposal table (the task's ruling).
type EvidenceProposal struct {
	// TargetVersionRef and EvidenceVersionRef are the object version ids the
	// assertion joins — the same two pins the evidence-assertion route takes
	// (specs/mcp/tools.json's target_version_ref / evidence_version_ref).
	TargetVersionRef   string
	EvidenceVersionRef string
	// Relation and EvidenceType are the canonical vocabulary of docs/10 §4;
	// the write path is the authority on which values exist.
	Relation     string
	EvidenceType string
	// Scope, Directness and InferenceNature are optional declarations
	// (empty means "not declared" to the write path).
	Scope           json.RawMessage
	Directness      string
	InferenceNature string
	// ReasoningNote overrides the comment body as the assertion's note. It
	// is normally EMPTY: the point of promoting a comment is that the
	// comment's own words carry, and the write path refuses a literature
	// assertion with an empty note.
	ReasoningNote string
}

// PromoteParams names one promotion: which proposal, into what kind of
// object, and the declarations that kind requires.
//
// The per-kind fields are mutually exclusive and enforced that way: a field
// that belongs to another kind is refused rather than ignored, because a
// caller that sent it believes it took effect (fail closed).
type PromoteParams struct {
	ProjectID string
	CommentID string
	// ThreadID is the thread the caller believes the comment lives in. It
	// is optional (the comment's own thread_id is the authority and is what
	// the promotion records), and when it is set a mismatch is refused:
	// the surface's route names both ids, and a request that pairs a
	// comment with somebody else's thread is answered "not found" rather
	// than promoted under a thread it does not belong to.
	ThreadID string
	Kind     domain.PromotionKind
	// Title names the created object when the kind takes a title (an
	// Issue). Empty derives it from the comment's first line; setting it
	// for a kind that does not take one (a hypothesis, whose title the RSG
	// write path derives from the statement) is refused.
	Title string
	// IssueType is the Issue's free-text kind (issues.issue_type, required
	// by the schema and never defaulted — no specification chooses a word
	// for it).
	IssueType string
	// BranchID is the research branch the state commit lands in. Required
	// for the two kinds that commit state (hypothesis, external evidence);
	// refused for an Issue, which is not a state commit at all.
	BranchID   string
	Hypothesis *HypothesisProposal
	Evidence   *EvidenceProposal
}

// PromotionResult is one promotion outcome: the recorded provenance (the
// promotion row plus the thread and comment it resolves to, the comment
// carrying its author) and what was created — exactly one of the three is
// set, matching the kind.
type PromotionResult struct {
	Origin domain.PromotionOrigin
	// Issue is set for a PromotionIssue: the row the promotion created.
	Issue *domain.Issue
	// Object is set for a PromotionHypothesis: the object and version 1 the
	// RSG write path committed.
	Object *rsg.ObjectResult
	// Assertion is set for a PromotionExternalEvidence: the assertion the
	// evidence write path committed, in its unreviewed state.
	Assertion *rsg.EvidenceAssertionResult
}

// Promote turns one discussion comment into a proposed research object and
// records where it came from:
//
//  1. validate the request shape, including the per-kind field rules;
//  2. require an authenticated actor and run the project read gate;
//  3. authorize the promotion as a write of scientific state (the matrix
//     cell of write_scientific_state for the actor's class — the same
//     action the RSG writes use; the discussion surface adds no action);
//  4. resolve the comment and its thread inside the project; a withdrawn
//     comment is refused (ErrCommentDeleted: the author took the proposal
//     back);
//  5. create the object the kind names, through the surface that owns it:
//     an Issue row, a Hypothesis by a real state commit, or an
//     unreviewed evidence assertion;
//  6. record the promotion row (and the audit row) naming the comment, the
//     object and the promoting actor.
//
// The order of 5 and 6 is deliberate: the promoted object is the fact and
// the promotion row records where it came from, so a failure between them
// can only lose the origin, never leave a record pointing at an object that
// does not exist. That gap is recorded as a follow-up in the task result.
func (c *Command) Promote(ctx context.Context, actor domain.User, in PromoteParams) (PromotionResult, error) {
	kind, err := validatePromoteShape(in)
	if err != nil {
		return PromotionResult{}, err
	}
	project, err := c.visibleToActor(ctx, actor, in.ProjectID)
	if err != nil {
		return PromotionResult{}, err
	}
	if err := c.requireWrite(ctx, actor, project.ID); err != nil {
		return PromotionResult{}, err
	}
	if c.promotions == nil {
		return PromotionResult{}, fmt.Errorf("%w: no promotion store configured", ErrStore)
	}
	comment, err := c.comment(ctx, project.ID, in.CommentID)
	if err != nil {
		return PromotionResult{}, err
	}
	if comment.Deleted() {
		return PromotionResult{}, ErrCommentDeleted
	}
	if named := strings.TrimSpace(in.ThreadID); named != "" && named != comment.ThreadID {
		// The request named a thread the comment is not in. One outcome for
		// "no such comment here" and "that is not this comment's thread":
		// the pair the caller asked about does not exist (docs/45).
		return PromotionResult{}, ErrCommentNotFound
	}
	thread, err := c.thread(ctx, project.ID, comment.ThreadID)
	if err != nil {
		return PromotionResult{}, err
	}
	audit := promotionAudit(actor, thread, comment, kind, in.Title)

	switch kind {
	case domain.PromotionIssue:
		return c.promoteToIssue(ctx, actor, project.ID, thread, comment, in, audit)
	case domain.PromotionHypothesis:
		return c.promoteToHypothesis(ctx, actor, project.ID, thread, comment, in, audit)
	case domain.PromotionExternalEvidence:
		return c.promoteToEvidence(ctx, actor, project.ID, thread, comment, in, audit)
	default:
		// Unreachable: validatePromoteShape admitted the kind.
		return PromotionResult{}, fmt.Errorf("%w: unknown promotion kind %q", ErrValidation, kind)
	}
}

// promoteToIssue creates the Issue, the promotion record and the audit row
// in one store transaction. The issue number is allocated inside it under
// the project row lock (the same lock every project-scoped create takes),
// so two promotions racing for the same project cannot collide on
// issues(project_id, number).
func (c *Command) promoteToIssue(ctx context.Context, actor domain.User, projectID string, thread domain.DiscussionThread, comment domain.DiscussionComment, in PromoteParams, audit domain.AuditEntry) (PromotionResult, error) {
	title, err := issueTitle(in.Title, comment.Body)
	if err != nil {
		return PromotionResult{}, err
	}
	issue, promotion, err := c.promotions.PromoteToIssue(ctx, IssueProposal{
		ProjectID: projectID,
		IssueType: strings.TrimSpace(in.IssueType),
		Title:     title,
		Body:      comment.Body,
		CreatedBy: actor.ID,
	}, domain.DiscussionPromotion{
		ProjectID: projectID,
		ThreadID:  thread.ID,
		CommentID: comment.ID,
		Kind:      domain.PromotionIssue,
		// Ref is left empty: the issue row's uuid is assigned by the
		// database, so the store renders "<kind>:<id>" from the row it
		// just wrote (the same place that fills the audit row's target).
		PromotedBy: actor.ID,
	}, audit)
	if err != nil {
		return PromotionResult{}, mapStoreError(err)
	}
	return PromotionResult{
		Origin: domain.PromotionOrigin{Promotion: promotion, Thread: thread, Comment: comment},
		Issue:  &issue,
	}, nil
}

// promoteToHypothesis creates the scientific object through the RSG write
// path — a real state commit (states.Commit, gate draft, one state.committed
// event, one scientific_object_versions row) — and then records the
// promotion. The discussion command performs none of that itself: it hands
// the RSG service a payload derived from the proposal and lets the surface
// that owns scientific state do what it owns.
func (c *Command) promoteToHypothesis(ctx context.Context, actor domain.User, projectID string, thread domain.DiscussionThread, comment domain.DiscussionComment, in PromoteParams, audit domain.AuditEntry) (PromotionResult, error) {
	if c.hypotheses == nil {
		return PromotionResult{}, fmt.Errorf("%w: no scientific-object writer configured", ErrStore)
	}
	payload, err := json.Marshal(map[string]any{
		"statement":   comment.Body,
		"question_id": strings.TrimSpace(in.Hypothesis.QuestionID),
	})
	if err != nil {
		return PromotionResult{}, fmt.Errorf("%w: encoding the hypothesis payload: %v", ErrStore, err)
	}
	res, err := c.hypotheses.CreateObject(ctx, actor, projectID, in.BranchID, rsg.CreateObjectInput{
		ObjectType: hypothesisObjectType,
		Payload:    payload,
		// SchemaRef is empty: the RSG write path resolves the canonical
		// schema of the object type itself (rsg.resolveSchemaRef), which is
		// the one authority on it.
	})
	if err != nil {
		return PromotionResult{}, mapCreatorError(err)
	}
	promotion, err := c.recordPromotion(ctx, projectID, thread, comment,
		domain.PromotionHypothesis, res.Object.ID, actor.ID, audit)
	if err != nil {
		return PromotionResult{}, err
	}
	return PromotionResult{
		Origin: domain.PromotionOrigin{Promotion: promotion, Thread: thread, Comment: comment},
		Object: &res,
	}, nil
}

// promoteToEvidence creates the external-evidence proposal through the
// evidence write path and then records the promotion. The created assertion
// carries the column's default review_state 'unreviewed' — a proposal
// waiting for a reviewer, changed in place when one reviews it (00058).
func (c *Command) promoteToEvidence(ctx context.Context, actor domain.User, projectID string, thread domain.DiscussionThread, comment domain.DiscussionComment, in PromoteParams, audit domain.AuditEntry) (PromotionResult, error) {
	if c.evidence == nil {
		return PromotionResult{}, fmt.Errorf("%w: no evidence writer configured", ErrStore)
	}
	note := strings.TrimSpace(in.Evidence.ReasoningNote)
	if note == "" {
		note = comment.Body
	}
	res, err := c.evidence.CreateEvidenceAssertion(ctx, actor, projectID, in.BranchID, rsg.CreateEvidenceAssertionInput{
		TargetVersionRef:   strings.TrimSpace(in.Evidence.TargetVersionRef),
		EvidenceVersionRef: strings.TrimSpace(in.Evidence.EvidenceVersionRef),
		Relation:           strings.TrimSpace(in.Evidence.Relation),
		EvidenceType:       strings.TrimSpace(in.Evidence.EvidenceType),
		Scope:              in.Evidence.Scope,
		Directness:         strings.TrimSpace(in.Evidence.Directness),
		InferenceNature:    strings.TrimSpace(in.Evidence.InferenceNature),
		ReasoningNote:      note,
	})
	if err != nil {
		return PromotionResult{}, mapCreatorError(err)
	}
	promotion, err := c.recordPromotion(ctx, projectID, thread, comment,
		domain.PromotionExternalEvidence, res.Assertion.ID, actor.ID, audit)
	if err != nil {
		return PromotionResult{}, err
	}
	return PromotionResult{
		Origin:    domain.PromotionOrigin{Promotion: promotion, Thread: thread, Comment: comment},
		Assertion: &res,
	}, nil
}

// recordPromotion renders the promoted object's ref from the id the creating
// service returned and stores the promotion row with its audit row.
func (c *Command) recordPromotion(ctx context.Context, projectID string, thread domain.DiscussionThread, comment domain.DiscussionComment, kind domain.PromotionKind, objectID, actorID string, audit domain.AuditEntry) (domain.DiscussionPromotion, error) {
	promotion, err := c.promotions.RecordPromotion(ctx, domain.DiscussionPromotion{
		ProjectID: projectID,
		ThreadID:  thread.ID,
		CommentID: comment.ID,
		Kind:      kind,
		Ref:       domain.PromotionRef(kind, objectID),
		// The promoting actor is the AUTHORIZATION's subject, not the
		// comment's author: who promoted a proposal and who proposed it are
		// two different facts, and a provenance record that lost the first
		// would be unable to say who is accountable for the promotion.
		PromotedBy: actorID,
	}, audit)
	if err != nil {
		return domain.DiscussionPromotion{}, mapStoreError(err)
	}
	return promotion, nil
}

// Promotion returns one promotion with the provenance chain it resolves to:
// the thread (what was being discussed) and the comment (the proposal, with
// its author). It is the read that answers "where did this object come
// from" for a promotion id, and it runs the project read gate like every
// other read here.
func (c *Command) Promotion(ctx context.Context, r projects.Reader, projectID, promotionID string) (domain.PromotionOrigin, error) {
	if strings.TrimSpace(promotionID) == "" {
		return domain.PromotionOrigin{}, fmt.Errorf("%w: promotion_id is required", ErrValidation)
	}
	project, err := c.visibleTo(ctx, r, projectID)
	if err != nil {
		return domain.PromotionOrigin{}, err
	}
	if c.promotions == nil {
		return domain.PromotionOrigin{}, fmt.Errorf("%w: no promotion store configured", ErrStore)
	}
	promotion, err := c.promotions.GetPromotion(ctx, project.ID, promotionID)
	if err != nil {
		return domain.PromotionOrigin{}, mapStoreError(err)
	}
	return c.origin(ctx, promotion)
}

// PromotionsForRef returns every promotion that produced the object a ref
// names ("<kind>:<uuid>" — domain.PromotionRef), oldest first. This is the
// reverse direction of the provenance: given a promoted Issue, Hypothesis or
// evidence assertion, which discussion proposed it, in whose words, and who
// promoted it.
//
// The ref is parsed before anything is read: a ref that does not name a
// known promotion kind is a caller error, never an empty answer.
func (c *Command) PromotionsForRef(ctx context.Context, r projects.Reader, projectID, ref string) ([]domain.PromotionOrigin, error) {
	if _, _, ok := domain.SplitPromotionRef(ref); !ok {
		return nil, fmt.Errorf("%w: a promoted ref is \"<kind>:<id>\" with a known kind", ErrValidation)
	}
	project, err := c.visibleTo(ctx, r, projectID)
	if err != nil {
		return nil, err
	}
	if c.promotions == nil {
		return nil, fmt.Errorf("%w: no promotion store configured", ErrStore)
	}
	promotions, err := c.promotions.ListPromotionsByRef(ctx, project.ID, ref)
	if err != nil {
		return nil, mapStoreError(err)
	}
	out := make([]domain.PromotionOrigin, 0, len(promotions))
	for _, p := range promotions {
		origin, err := c.origin(ctx, p)
		if err != nil {
			return nil, err
		}
		out = append(out, origin)
	}
	return out, nil
}

// origin resolves one promotion row into its provenance chain: the thread it
// names and the comment it promoted. Both are read inside the promotion's
// project, so a promotion cannot resolve into another project's discussion.
func (c *Command) origin(ctx context.Context, promotion domain.DiscussionPromotion) (domain.PromotionOrigin, error) {
	thread, err := c.thread(ctx, promotion.ProjectID, promotion.ThreadID)
	if err != nil {
		return domain.PromotionOrigin{}, err
	}
	comment, err := c.comment(ctx, promotion.ProjectID, promotion.CommentID)
	if err != nil {
		return domain.PromotionOrigin{}, err
	}
	return domain.PromotionOrigin{Promotion: promotion, Thread: thread, Comment: comment}, nil
}

// visibleToActor is the write-side gate: every write here needs an
// authenticated actor, and then the project read rule decides whether they
// may act in it at all. An actor who may not read the project is refused
// with not-found rather than forbidden, so the outcome never discloses
// whether the project exists (docs/45).
func (c *Command) visibleToActor(ctx context.Context, actor domain.User, projectID string) (domain.Project, error) {
	if actor.ID == "" {
		return domain.Project{}, ErrForbidden
	}
	return c.visibleTo(ctx, projects.Reader{UserID: actor.ID, Authenticated: true}, projectID)
}

// visibleTo runs the project read gate (T0106's resolution, the same one
// every project-scoped surface runs) and maps its outcomes onto this
// package's.
func (c *Command) visibleTo(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error) {
	if strings.TrimSpace(projectID) == "" {
		return domain.Project{}, fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if c.projects == nil {
		return domain.Project{}, fmt.Errorf("%w: no project gate configured", ErrStore)
	}
	project, err := c.projects.Get(ctx, r, projectID)
	if err != nil {
		if errors.Is(err, projects.ErrProjectNotFound) {
			return domain.Project{}, ErrProjectNotFound
		}
		return domain.Project{}, fmt.Errorf("%w: read project: %v", ErrStore, err)
	}
	return project, nil
}

// thread reads one thread of the project through the store.
func (c *Command) thread(ctx context.Context, projectID, threadID string) (domain.DiscussionThread, error) {
	if c.threads == nil {
		return domain.DiscussionThread{}, fmt.Errorf("%w: no discussion store configured", ErrStore)
	}
	thread, err := c.threads.GetThread(ctx, projectID, threadID)
	if err != nil {
		return domain.DiscussionThread{}, mapStoreError(err)
	}
	return thread, nil
}

// comment reads one comment of the project through the store.
func (c *Command) comment(ctx context.Context, projectID, commentID string) (domain.DiscussionComment, error) {
	if c.threads == nil {
		return domain.DiscussionComment{}, fmt.Errorf("%w: no discussion store configured", ErrStore)
	}
	comment, err := c.threads.GetComment(ctx, projectID, commentID)
	if err != nil {
		return domain.DiscussionComment{}, mapStoreError(err)
	}
	return comment, nil
}

// requireWrite authorizes a promotion: resolve the actor's membership role,
// evaluate the matrix cell of write_scientific_state for their class, and
// resolve the conditional cell (own_fork_only) against the fork lineage —
// the exact enforcement the RSG write path runs at its own write sites
// (rsg.Service.requireWrite), reached through this package's own ports so
// the discussion surface depends on no authz machinery of its own.
//
// The denial precedes every lookup of the comment and the thread, so a
// refused promotion never discloses whether the proposal exists (docs/45).
func (c *Command) requireWrite(ctx context.Context, actor domain.User, projectID string) error {
	membership, err := c.projects.GetMembership(ctx, actor, projectID)
	var role *domain.ProjectRole
	switch {
	case err == nil:
		r := membership.Role
		role = &r
	case errors.Is(err, projects.ErrMemberNotFound):
		role = nil
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound // existence hiding, never "forbidden"
	default:
		return fmt.Errorf("%w: read membership: %v", ErrStore, err)
	}
	if c.authz == nil {
		return fmt.Errorf("%w: no policy engine configured", ErrStore)
	}
	decision, err := c.authz.Authorize(ctx, authz.Request{
		Action: authz.ActionWriteScientificState,
		Class:  authz.ClassOf(true, role, false),
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
	switch {
	case decision.Permits():
		return nil
	case decision.Verdict == authz.VerdictOwnForkOnly:
		if c.forks == nil {
			// Unresolved is not permitted (fail closed). The message names
			// the wiring gap for the operator; the caller sees the
			// ordinary denial.
			return fmt.Errorf("%w: write_scientific_state is own_fork_only for a non-member and no fork gate is wired to resolve it", ErrForbidden)
		}
		owned, err := c.forks.OwnedFork(ctx, projectID, actor.ID)
		if err != nil {
			return fmt.Errorf("%w: resolve fork lineage: %v", ErrStore, err)
		}
		if !owned {
			return fmt.Errorf("%w: write_scientific_state is own_fork_only for a non-member, and project %s is not this actor's own fork", ErrForbidden, projectID)
		}
		return nil
	default:
		return fmt.Errorf("%w: write_scientific_state on project %s is %s for this actor", ErrForbidden, projectID, decision.Verdict)
	}
}

// requireTarget verifies that a thread's target exists in the project. It
// covers the two kinds whose existence is decided by another table; the
// project kind is decided by comparing the target id to the project — the
// same pair 00104's CHECK pins.
func (c *Command) requireTarget(ctx context.Context, projectID string, kind domain.DiscussionTargetKind, targetID string) error {
	if kind == domain.DiscussionTargetProject {
		if targetID != projectID {
			return ErrTargetNotFound
		}
		return nil
	}
	if c.threads == nil {
		return fmt.Errorf("%w: no discussion store configured", ErrStore)
	}
	exists, err := c.threads.TargetExists(ctx, projectID, kind, targetID)
	if err != nil {
		return fmt.Errorf("%w: check discussion target: %v", ErrStore, err)
	}
	if !exists {
		return ErrTargetNotFound
	}
	return nil
}

// validateOpenThread checks the opening shape and returns the stored forms
// (the trimmed target id and body).
func validateOpenThread(in OpenThreadParams) (domain.DiscussionTargetKind, string, string, error) {
	if strings.TrimSpace(in.ProjectID) == "" {
		return "", "", "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if !domain.ValidDiscussionTargetKind(in.TargetKind) {
		return "", "", "", fmt.Errorf("%w: unknown discussion target kind %q", ErrValidation, in.TargetKind)
	}
	targetID := strings.TrimSpace(in.TargetID)
	if targetID == "" {
		return "", "", "", fmt.Errorf("%w: target_id is required", ErrValidation)
	}
	body := strings.TrimSpace(in.Body)
	if !domain.ValidDiscussionBody(body) {
		return "", "", "", fmt.Errorf("%w: an opening comment is required and is at most %d characters",
			ErrValidation, domain.MaxDiscussionBodyLen)
	}
	return in.TargetKind, targetID, body, nil
}

// validatePromoteShape checks the request shape and the per-kind field
// rules. A field that belongs to another kind is refused (fail closed): a
// caller that sent it believes it took effect, and silently dropping it is
// how a request and its result come to disagree.
func validatePromoteShape(in PromoteParams) (domain.PromotionKind, error) {
	if strings.TrimSpace(in.ProjectID) == "" {
		return "", fmt.Errorf("%w: project_id is required", ErrValidation)
	}
	if strings.TrimSpace(in.CommentID) == "" {
		return "", fmt.Errorf("%w: comment_id is required", ErrValidation)
	}
	if !domain.ValidPromotionKind(in.Kind) {
		return "", fmt.Errorf("%w: unknown promotion kind %q", ErrValidation, in.Kind)
	}
	branchID := strings.TrimSpace(in.BranchID)
	issueType := strings.TrimSpace(in.IssueType)
	title := strings.TrimSpace(in.Title)
	switch in.Kind {
	case domain.PromotionIssue:
		if in.Hypothesis != nil || in.Evidence != nil || branchID != "" {
			return "", fmt.Errorf("%w: an Issue is not a state commit — a branch_id belongs to a hypothesis or an external-evidence promotion", ErrValidation)
		}
		if issueType == "" {
			return "", fmt.Errorf("%w: issue_type is required and is never defaulted", ErrValidation)
		}
		if len(issueType) > domain.MaxPromotionTitleLen {
			return "", fmt.Errorf("%w: issue_type is at most %d characters", ErrValidation, domain.MaxPromotionTitleLen)
		}
		if len(title) > domain.MaxPromotionTitleLen {
			return "", fmt.Errorf("%w: a title is at most %d characters", ErrValidation, domain.MaxPromotionTitleLen)
		}
	case domain.PromotionHypothesis:
		if in.Hypothesis == nil {
			return "", fmt.Errorf("%w: a hypothesis promotion carries the research question it addresses", ErrValidation)
		}
		if in.Evidence != nil || issueType != "" {
			return "", fmt.Errorf("%w: an issue_type belongs to an Issue promotion", ErrValidation)
		}
		if title != "" {
			return "", fmt.Errorf("%w: a hypothesis takes no title here — the RSG write path derives it from the statement", ErrValidation)
		}
		if branchID == "" {
			return "", fmt.Errorf("%w: branch_id is required — a hypothesis is created by a state commit on a branch", ErrValidation)
		}
		if strings.TrimSpace(in.Hypothesis.QuestionID) == "" {
			return "", fmt.Errorf("%w: question_id is required — the hypothesis schema requires the research question it addresses", ErrValidation)
		}
	case domain.PromotionExternalEvidence:
		if in.Evidence == nil {
			return "", fmt.Errorf("%w: an external-evidence promotion carries the assertion's pins and vocabulary", ErrValidation)
		}
		if in.Hypothesis != nil || issueType != "" {
			return "", fmt.Errorf("%w: an issue_type belongs to an Issue promotion", ErrValidation)
		}
		if title != "" {
			return "", fmt.Errorf("%w: an evidence assertion takes no title here", ErrValidation)
		}
		if branchID == "" {
			return "", fmt.Errorf("%w: branch_id is required — an evidence assertion is created by a state commit on a branch", ErrValidation)
		}
		ev := in.Evidence
		if strings.TrimSpace(ev.TargetVersionRef) == "" || strings.TrimSpace(ev.EvidenceVersionRef) == "" {
			return "", fmt.Errorf("%w: target_version_ref and evidence_version_ref are both required", ErrValidation)
		}
		if strings.TrimSpace(ev.Relation) == "" || strings.TrimSpace(ev.EvidenceType) == "" {
			return "", fmt.Errorf("%w: relation and evidence_type are both required", ErrValidation)
		}
	}
	return in.Kind, nil
}

// issueTitle resolves the title of a promoted Issue: the caller's, or the
// comment's first non-blank line. A comment whose first line is blank and
// which named no title cannot become an Issue — issues.title is NOT NULL in
// the schema, and an empty one would be a row no surface can render.
func issueTitle(requested, body string) (string, error) {
	title := strings.TrimSpace(requested)
	if title == "" {
		title = domain.PromotionTitleFromBody(body)
	}
	if !domain.ValidPromotionTitle(title) {
		return "", fmt.Errorf("%w: a promoted Issue needs a title — name one, or write one on the comment's first line", ErrValidation)
	}
	return title, nil
}

// promotionAudit renders the discussion.promoted audit row the store writes
// in the same transaction as the promotion record. The target ref is filled
// by the store: it names the created object's ref, which for an Issue is
// known only after the insert assigns the row's id (the same division the
// milestone store uses).
//
// The summary carries what the promotion was made OF — the thread, the
// comment and the target — because that is what the audit trail has to be
// able to answer later, long after the promoted object has moved on.
func promotionAudit(actor domain.User, thread domain.DiscussionThread, comment domain.DiscussionComment, kind domain.PromotionKind, title string) domain.AuditEntry {
	summary := map[string]any{
		"promotion_kind": string(kind),
		"thread_id":      thread.ID,
		"comment_id":     comment.ID,
		"comment_author": comment.CreatedBy,
		"target_type":    string(thread.TargetKind),
		"target_id":      thread.TargetID,
	}
	if title != "" {
		summary["title"] = title
	}
	return domain.AuditEntry{
		ActorID:      actor.ID,
		Via:          domain.ViaSession,
		Action:       domain.ActionDiscussionPromoted,
		ProjectID:    thread.ProjectID,
		AfterSummary: summary,
	}
}

// mapStoreError keeps the store's own sentinels and turns everything else
// into ErrStore (cause kept for the log).
func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrValidation),
		errors.Is(err, ErrForbidden),
		errors.Is(err, ErrProjectNotFound),
		errors.Is(err, ErrThreadNotFound),
		errors.Is(err, ErrCommentNotFound),
		errors.Is(err, ErrCommentDeleted),
		errors.Is(err, ErrTargetNotFound),
		errors.Is(err, ErrPromotionNotFound),
		errors.Is(err, ErrBranchNotFound):
		return err
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}

// mapCreatorError maps a failure of one of the creating services (the RSG
// write path, the branch resolution behind it, the projects gate) onto this
// package's outcomes. Everything it does not recognize becomes ErrStore:
// state is unknowable, so the promotion is refused rather than guessed
// (default deny, docs/12).
func mapCreatorError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, rsg.ErrForbidden):
		return fmt.Errorf("%w: %v", ErrForbidden, err)
	case errors.Is(err, rsg.ErrValidation):
		return fmt.Errorf("%w: %v", ErrValidation, err)
	case errors.Is(err, branches.ErrBranchNotFound):
		return fmt.Errorf("%w: %v", ErrBranchNotFound, err)
	case errors.Is(err, projects.ErrProjectNotFound):
		return ErrProjectNotFound
	default:
		return fmt.Errorf("%w: %v", ErrStore, err)
	}
}
