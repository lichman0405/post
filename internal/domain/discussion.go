package domain

import (
	"strings"
	"time"
)

// The discussion domain (T0811; canonical tables discussion_threads,
// discussion_comments, discussion_promotions, migration 00104).
//
// docs/09 §4 and docs/42 §PR list "discussion" among a pull request's
// parts; this file is what that word means in storage. The one property
// everything else here follows from: a discussion is a VERSION-LESS member
// row. A thread and a comment carry no version number, no state_id and no
// branch, they take no part in a state commit
// (internal/application/states.Commit), and nothing about them is
// registered in an append-only ledger.
//
// That is also why a discussion's provenance is NOT a relation: the repo's
// provenance representation is a relation version (docs/44), and both of
// its endpoints are foreign keys to scientific_object_versions — a
// discussion has no version, so it cannot be an endpoint at all. Where a
// discussion leads somewhere (a promoted Proposal, below), the origin is
// recorded in discussion_promotions instead.

// DiscussionTargetKind is the kind of thing a thread is about. Three kinds
// exist because three surfaces carry a discussion (the task's requirement):
// a project, a published knowledge object, and a pull request.
type DiscussionTargetKind string

const (
	// DiscussionTargetProject: the thread is about the project itself.
	// Its TargetID is the project's uuid — the same one in the thread's
	// own project_id column (00104's CHECK enforces exactly that pair).
	DiscussionTargetProject DiscussionTargetKind = "project"
	// DiscussionTargetKnowledge: the thread is about a published knowledge
	// object. Its TargetID is the publication's PID — the identity
	// GET /knowledge/{knowledgeId} resolves (00083), not an internal row
	// id.
	DiscussionTargetKnowledge DiscussionTargetKind = "knowledge"
	// DiscussionTargetPullRequest: the thread is about a research pull
	// request. Its TargetID is the PR's per-project NUMBER (00009's
	// UNIQUE(project_id, number)) — the identity every PR route displays.
	DiscussionTargetPullRequest DiscussionTargetKind = "pull_request"
)

// ValidDiscussionTargetKind reports whether k is one of the three kinds
// (discussion_threads.target_type CHECK — keep the two in step).
func ValidDiscussionTargetKind(k DiscussionTargetKind) bool {
	switch k {
	case DiscussionTargetProject, DiscussionTargetKnowledge, DiscussionTargetPullRequest:
		return true
	}
	return false
}

// DiscussionThread is one conversation about one target. It has no title:
// the target names the conversation (the PR page's Discussion section, the
// knowledge page's discussion, the project's), and a second name for the
// same thing would be a second answer to "what is this about".
type DiscussionThread struct {
	// ID is the uuid v4 text form (discussion_threads.id).
	ID string
	// ProjectID is the project the thread lives in — the scope every
	// authorization decision, read gate and target check uses.
	ProjectID string
	// TargetKind and TargetID name what the thread is about, in that
	// target's OWN addressing scheme (see the kind constants above).
	TargetKind DiscussionTargetKind
	TargetID   string
	// CreatedBy is the user id of the actor who opened the thread (the
	// author of its first comment too — the two are written together).
	CreatedBy string
	CreatedAt time.Time
	// CommentCount and LastCommentAt are the list read's rendering
	// fields: how many comments still stand, and the newest one's time
	// (nil when none does). They are zero values on a thread read by id.
	CommentCount  int64
	LastCommentAt *time.Time
}

// DiscussionComment is one message in a thread. It is never physically
// deleted: DeletedAt/DeletedBy are the tombstone (CLAUDE.md §9.8 — nothing
// disappears, state only evolves), the body stays in the row, and the wire
// stops serving it once the tombstone is set.
type DiscussionComment struct {
	// ID is the uuid v4 text form (discussion_comments.id).
	ID string
	// ThreadID names the thread this comment belongs to.
	ThreadID string
	// ProjectID repeats the thread's project so every comment read is
	// scoped by it (an id of another project is not-found, docs/45).
	ProjectID string
	// Body is the comment's text. It is withheld from the wire after
	// deletion, never erased from the row.
	Body string
	// CreatedBy is the user id of the comment's author — the actor the
	// promotion provenance resolves to ("who originally proposed this").
	CreatedBy string
	CreatedAt time.Time
	// DeletedAt is when the comment was withdrawn and DeletedBy who
	// withdrew it; both nil while it stands (the CHECK in 00104 makes
	// them move together).
	DeletedAt *time.Time
	DeletedBy *string
}

// Deleted reports whether the comment carries a tombstone.
func (c DiscussionComment) Deleted() bool { return c.DeletedAt != nil }

// PromotionKind is what a promoted discussion became. The vocabulary is
// closed (discussion_promotions.promoted_kind CHECK): the three targets
// the task requires, and nothing else — a fourth kind would need its own
// creation path, its own provenance answer and its own contract entry.
type PromotionKind string

const (
	// PromotionIssue: the proposal became an Issue row
	// (00009's issues table).
	PromotionIssue PromotionKind = "issue"
	// PromotionHypothesis: the proposal became a Hypothesis scientific
	// object, created through the ordinary RSG write path (a real state
	// commit — internal/application/rsg.CreateObject).
	PromotionHypothesis PromotionKind = "hypothesis"
	// PromotionExternalEvidence: the proposal became an external evidence
	// proposal — one evidence_assertions row in its default
	// review_state='unreviewed' (00007, 00058: reviewing it later is a
	// legitimate in-place state change). There is deliberately no
	// "proposal" table: an unreviewed assertion IS the pending proposal.
	PromotionExternalEvidence PromotionKind = "external_evidence"
)

// ValidPromotionKind reports whether k is one of the three kinds
// (discussion_promotions.promoted_kind CHECK).
func ValidPromotionKind(k PromotionKind) bool {
	switch k {
	case PromotionIssue, PromotionHypothesis, PromotionExternalEvidence:
		return true
	}
	return false
}

// PromotionRef renders the promoted object's reference from the promotion's
// kind and the created object's id: "<kind>:<uuid>", the same
// "<subject>:<id>" shape audit_log.target_ref uses. 00104's CHECK derives
// the prefix from the kind, so a promotion cannot name a kind it is not.
func PromotionRef(kind PromotionKind, id string) string {
	return string(kind) + ":" + id
}

// SplitPromotionRef splits a ref back into its kind and id. ok is false for
// a ref that is not "<kind>:<non-empty id>" — a caller that must resolve a
// ref (the provenance read) refuses it instead of guessing.
func SplitPromotionRef(ref string) (PromotionKind, string, bool) {
	kind, id, found := strings.Cut(ref, ":")
	if !found || id == "" {
		return "", "", false
	}
	k := PromotionKind(kind)
	if !ValidPromotionKind(k) {
		return "", "", false
	}
	return k, id, true
}

// DiscussionPromotion is one recorded promotion: the explicit provenance
// chain from a discussion to the thing it became. It exists because the
// relation graph cannot carry it (a discussion is not a scientific object
// and has no version to be an endpoint with), and because the promoted
// object's own tables — issues, scientific_objects, evidence_assertions —
// have no column naming a discussion, and would each need one.
type DiscussionPromotion struct {
	// ID is the uuid v4 text form (discussion_promotions.id).
	ID string
	// ProjectID is the project the promotion happened in.
	ProjectID string
	// ThreadID and CommentID name the discussion and the exact proposal
	// within it.
	ThreadID  string
	CommentID string
	// Kind is what it became, and Ref names the created object
	// ("<kind>:<uuid>", see PromotionRef).
	Kind PromotionKind
	Ref  string
	// PromotedBy and PromotedAt are the promotion's own provenance: which
	// authorized actor accepted the proposal, and when.
	PromotedBy string
	PromotedAt time.Time
}

// PromotionOrigin is the provenance answer for one promotion: the record
// itself plus everything the chain resolves through it — the thread (what
// was being discussed), the comment (the proposal) and the comment's
// author ("who originally proposed this").
type PromotionOrigin struct {
	Promotion DiscussionPromotion
	Thread    DiscussionThread
	Comment   DiscussionComment
}

// MaxDiscussionBodyLen bounds one comment's body. It bounds a single
// discussion message, not a document: a proposal that needs more than this
// is a scientific object, and promoting it is exactly what this flow is
// for.
const MaxDiscussionBodyLen = 8000

// ValidDiscussionBody reports whether a raw comment body is storable: the
// stored form is the trimmed one (00104 rejects a body that is blank after
// trimming), and it is bounded.
func ValidDiscussionBody(body string) bool {
	trimmed := strings.TrimSpace(body)
	return trimmed != "" && len(trimmed) <= MaxDiscussionBodyLen
}

// MaxPromotionTitleLen bounds the title a promotion gives the object it
// creates (an Issue title, a Hypothesis title). It is the same order as
// the other one-line titles in this package.
const MaxPromotionTitleLen = 300

// ValidPromotionTitle reports whether a raw title is storable as a created
// object's title.
func ValidPromotionTitle(title string) bool {
	trimmed := strings.TrimSpace(title)
	return trimmed != "" && len(trimmed) <= MaxPromotionTitleLen
}

// PromotionTitleFromBody derives the default title of a promoted proposal
// from the comment that proposed it: the comment's first non-blank line,
// bounded to MaxPromotionTitleLen.
//
// It is a DERIVATION of the proposal's own words, never an invention: the
// promotion request may name a title explicitly, and this runs only when
// it did not. A body whose first line is longer than the bound is cut at
// the bound, because a title that grows with its input is not a title.
func PromotionTitleFromBody(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(line) > MaxPromotionTitleLen {
			return line[:MaxPromotionTitleLen]
		}
		return line
	}
	return ""
}
