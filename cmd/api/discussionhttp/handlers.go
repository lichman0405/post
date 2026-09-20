package discussionhttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/branches"
	"github.com/lichman0405/post/internal/application/discussions"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/rsg"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// CommandPort is the discussion command slice the transport needs. The
// production implementation is *discussions.Command; the interface exists so
// the handlers are unit-testable against a fake.
type CommandPort interface {
	OpenThread(ctx context.Context, actor domain.User, in discussions.OpenThreadParams) (discussions.ThreadResult, error)
	ListThreads(ctx context.Context, r projects.Reader, projectID string, kind domain.DiscussionTargetKind, targetID string) ([]domain.DiscussionThread, error)
	GetThread(ctx context.Context, r projects.Reader, projectID, threadID string) (discussions.ThreadResult, error)
	AddComment(ctx context.Context, actor domain.User, in discussions.AddCommentParams) (domain.DiscussionComment, error)
	DeleteComment(ctx context.Context, actor domain.User, projectID, commentID string) (domain.DiscussionComment, error)
	Promote(ctx context.Context, actor domain.User, in discussions.PromoteParams) (discussions.PromotionResult, error)
	Promotion(ctx context.Context, r projects.Reader, projectID, promotionID string) (domain.PromotionOrigin, error)
	PromotionsForRef(ctx context.Context, r projects.Reader, projectID, ref string) ([]domain.PromotionOrigin, error)
}

// handlers owns the discussion routes.
type handlers struct {
	cmd CommandPort
}

// threadPayload is the client-visible thread shape. comment_count and
// last_comment_at are the list read's rendering fields; they are zero on a
// thread read by id, which carries its comments instead.
type threadPayload struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	TargetType    string     `json:"target_type"`
	TargetID      string     `json:"target_id"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	CommentCount  int64      `json:"comment_count"`
	LastCommentAt *time.Time `json:"last_comment_at"`
}

// commentPayload is the client-visible comment shape. Body is null once the
// comment is withdrawn: the row keeps its text (nothing disappears) and the
// surface stops serving it, which is what the tombstone means.
type commentPayload struct {
	ID        string     `json:"id"`
	ThreadID  string     `json:"thread_id"`
	ProjectID string     `json:"project_id"`
	Body      *string    `json:"body"`
	CreatedBy string     `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	Deleted   bool       `json:"deleted"`
	DeletedAt *time.Time `json:"deleted_at"`
	DeletedBy *string    `json:"deleted_by"`
}

// promotionPayload is the client-visible promotion shape: what a proposal
// became (promoted_kind + promoted_ref, "<kind>:<uuid>"), and who promoted
// it when.
type promotionPayload struct {
	ID           string    `json:"id"`
	ProjectID    string    `json:"project_id"`
	ThreadID     string    `json:"thread_id"`
	CommentID    string    `json:"comment_id"`
	PromotedKind string    `json:"promoted_kind"`
	PromotedRef  string    `json:"promoted_ref"`
	PromotedBy   string    `json:"promoted_by"`
	PromotedAt   time.Time `json:"promoted_at"`
}

// issuePayload is the created Issue a promotion to kind=issue answers with.
type issuePayload struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Number    int64     `json:"number"`
	IssueType string    `json:"issue_type"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	State     string    `json:"state"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// objectPayload is the created scientific object a promotion to
// kind=hypothesis answers with: the container row, version 1 the state
// commit produced, and the advisory semantic hints (the same shape the RSG
// surface serves).
type objectPayload struct {
	ID             string           `json:"id"`
	ProjectID      string           `json:"project_id"`
	ObjectType     string           `json:"object_type"`
	CurrentVersion int              `json:"current_version"`
	VersionID      string           `json:"version_id"`
	VersionNo      int              `json:"version_no"`
	StateID        string           `json:"state_id"`
	BranchID       *string          `json:"branch_id"`
	Title          string           `json:"title"`
	LifecycleState string           `json:"lifecycle_state"`
	SchemaRef      schemaRefPayload `json:"schema_ref"`
	Payload        json.RawMessage  `json:"payload"`
	Hints          []hintPayload    `json:"hints"`
}

type schemaRefPayload struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type hintPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// assertionPayload is the created evidence assertion a promotion to
// kind=external_evidence answers with. ReviewState is the point of the
// shape: the row lands 'unreviewed', which is what a PROPOSAL is here.
type assertionPayload struct {
	ID                      string    `json:"id"`
	ProjectID               string    `json:"project_id"`
	StateID                 string    `json:"state_id"`
	TargetObjectVersionID   string    `json:"target_object_version_id"`
	EvidenceObjectVersionID string    `json:"evidence_object_version_id"`
	RelationType            string    `json:"relation_type"`
	EvidenceType            string    `json:"evidence_type"`
	Directness              string    `json:"directness"`
	InferenceNature         string    `json:"inference_nature"`
	ReasoningNote           string    `json:"reasoning_note"`
	ReviewState             string    `json:"review_state"`
	EvidenceOrigin          string    `json:"evidence_origin"`
	Visibility              string    `json:"visibility"`
	CreatedBy               string    `json:"created_by"`
	CreatedAt               time.Time `json:"created_at"`
}

func threadPayloadFromDomain(t domain.DiscussionThread) threadPayload {
	return threadPayload{
		ID:            t.ID,
		ProjectID:     t.ProjectID,
		TargetType:    string(t.TargetKind),
		TargetID:      t.TargetID,
		CreatedBy:     t.CreatedBy,
		CreatedAt:     t.CreatedAt,
		CommentCount:  t.CommentCount,
		LastCommentAt: t.LastCommentAt,
	}
}

func commentPayloadFromDomain(c domain.DiscussionComment) commentPayload {
	var body *string
	if !c.Deleted() {
		b := c.Body
		body = &b
	}
	return commentPayload{
		ID:        c.ID,
		ThreadID:  c.ThreadID,
		ProjectID: c.ProjectID,
		Body:      body,
		CreatedBy: c.CreatedBy,
		CreatedAt: c.CreatedAt,
		Deleted:   c.Deleted(),
		DeletedAt: c.DeletedAt,
		DeletedBy: c.DeletedBy,
	}
}

func commentsPayloadFromDomain(comments []domain.DiscussionComment) []commentPayload {
	out := make([]commentPayload, 0, len(comments))
	for _, c := range comments {
		out = append(out, commentPayloadFromDomain(c))
	}
	return out
}

func promotionPayloadFromDomain(p domain.DiscussionPromotion) promotionPayload {
	return promotionPayload{
		ID:           p.ID,
		ProjectID:    p.ProjectID,
		ThreadID:     p.ThreadID,
		CommentID:    p.CommentID,
		PromotedKind: string(p.Kind),
		PromotedRef:  p.Ref,
		PromotedBy:   p.PromotedBy,
		PromotedAt:   p.PromotedAt,
	}
}

// originPayload renders one provenance answer: the promotion, the thread it
// happened in and the comment it promoted (whose author is the proposal's
// author).
func originPayload(o domain.PromotionOrigin) map[string]any {
	return map[string]any{
		"promotion": promotionPayloadFromDomain(o.Promotion),
		"thread":    threadPayloadFromDomain(o.Thread),
		"comment":   commentPayloadFromDomain(o.Comment),
	}
}

func issuePayloadFromDomain(i domain.Issue) issuePayload {
	return issuePayload{
		ID:        i.ID,
		ProjectID: i.ProjectID,
		Number:    i.Number,
		IssueType: i.IssueType,
		Title:     i.Title,
		Body:      i.Body,
		State:     string(i.State),
		CreatedBy: i.CreatedBy,
		CreatedAt: i.CreatedAt,
	}
}

func objectPayloadFromResult(res rsg.ObjectResult) objectPayload {
	out := objectPayload{
		ID:             res.Object.ID,
		ProjectID:      res.Object.ProjectID,
		ObjectType:     res.Object.ObjectType,
		CurrentVersion: res.Object.CurrentVersionNo,
		VersionID:      res.Version.ID,
		VersionNo:      res.Version.VersionNo,
		StateID:        res.Version.StateID,
		BranchID:       res.Version.BranchID,
		Title:          res.Version.Title,
		LifecycleState: string(res.Version.LifecycleState),
		SchemaRef:      schemaRefPayload{ID: res.Version.SchemaID, Version: res.Version.SchemaVersion},
		Payload:        res.Version.Payload,
		Hints:          make([]hintPayload, 0, len(res.Hints)),
	}
	for _, h := range res.Hints {
		out.Hints = append(out.Hints, hintPayload{Code: h.Code, Message: h.Message})
	}
	return out
}

func assertionPayloadFromResult(res rsg.EvidenceAssertionResult) assertionPayload {
	row := res.Assertion
	return assertionPayload{
		ID:                      row.ID,
		ProjectID:               row.ProjectID,
		StateID:                 row.StateID,
		TargetObjectVersionID:   row.TargetObjectVersionID,
		EvidenceObjectVersionID: row.EvidenceObjectVersionID,
		RelationType:            row.RelationType,
		EvidenceType:            row.EvidenceType,
		Directness:              row.Directness,
		InferenceNature:         row.InferenceNature,
		ReasoningNote:           row.ReasoningNote,
		ReviewState:             row.ReviewState,
		EvidenceOrigin:          row.EvidenceOrigin,
		Visibility:              row.Visibility,
		CreatedBy:               row.CreatedBy,
		CreatedAt:               row.CreatedAt,
	}
}

// principal resolves the authenticated actor for a write; a missing session
// is answered in place with 401 (the guard enforces the same before routing
// — this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// reader resolves the caller for the visibility-aware reads: the same
// resolution every other project read runs (anonymous stays anonymous — a
// public project's discussions are public).
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// decodeBody parses a request body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, discussions.CodeDiscussionValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// writeDiscussionError maps a command error onto the wire envelope (docs/45):
// one code per failure shape. Dependency failures answer a generic 503
// naming nothing internal.
func writeDiscussionError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, discussions.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, discussions.CodeDiscussionValidationFailed, err.Error())
	case errors.Is(err, discussions.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, discussions.CodeDiscussionForbidden,
			"the actor may not do this here")
	case errors.Is(err, discussions.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionProjectNotFound,
			"project not found")
	case errors.Is(err, discussions.ErrThreadNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionThreadNotFound,
			"discussion thread not found")
	case errors.Is(err, discussions.ErrCommentNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionCommentNotFound,
			"discussion comment not found")
	case errors.Is(err, discussions.ErrCommentDeleted):
		authhttp.WriteError(w, r, http.StatusConflict, discussions.CodeDiscussionCommentDeleted,
			"the comment was withdrawn")
	case errors.Is(err, discussions.ErrTargetNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionTargetNotFound,
			"the discussion target does not exist in this project")
	case errors.Is(err, discussions.ErrBranchNotFound), errors.Is(err, branches.ErrBranchNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionBranchNotFound,
			"the research branch does not exist in this project")
	case errors.Is(err, discussions.ErrPromotionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, discussions.CodeDiscussionPromotionNotFound,
			"promotion not found")
	case errors.Is(err, discussions.ErrStore), errors.Is(err, projects.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, discussions.CodeDiscussionServiceUnavailable,
			"discussion data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("discussion handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// openThreadRequest is the POST body of a new conversation: what it is about
// (the target's own addressing scheme) and its opening comment.
type openThreadRequest struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Body       string `json:"body"`
}

// handleOpenThread: POST /api/v1/projects/{projectId}/discussions — open one
// thread with its first comment, in one transaction. 201 answers the thread
// and its opening comment.
func (h *handlers) handleOpenThread(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req openThreadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.cmd.OpenThread(r.Context(), actor, discussions.OpenThreadParams{
		ProjectID:  r.PathValue("projectId"),
		TargetKind: domain.DiscussionTargetKind(req.TargetType),
		TargetID:   req.TargetID,
		Body:       req.Body,
	})
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"thread":   threadPayloadFromDomain(res.Thread),
		"comments": commentsPayloadFromDomain(res.Comments),
	})
}

// handleListThreads: GET /api/v1/projects/{projectId}/discussions?target_type=&target_id=
// — the threads of one target, oldest first, with each thread's live comment
// count and last activity. The target filter is a query pair because a
// thread's target is polymorphic (see wiring.go).
func (h *handlers) handleListThreads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	threads, err := h.cmd.ListThreads(r.Context(), reader(r), r.PathValue("projectId"),
		domain.DiscussionTargetKind(q.Get("target_type")), q.Get("target_id"))
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	out := make([]threadPayload, 0, len(threads))
	for _, t := range threads {
		out = append(out, threadPayloadFromDomain(t))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"threads": out})
}

// handleGetThread: GET
// /api/v1/projects/{projectId}/discussions/{threadId} — the thread and its
// comments, withdrawn ones included (their bodies withheld).
func (h *handlers) handleGetThread(w http.ResponseWriter, r *http.Request) {
	res, err := h.cmd.GetThread(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("threadId"))
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{
		"thread":   threadPayloadFromDomain(res.Thread),
		"comments": commentsPayloadFromDomain(res.Comments),
	})
}

// addCommentRequest is the POST body of one appended comment.
type addCommentRequest struct {
	Body string `json:"body"`
}

// handleAddComment: POST
// /api/v1/projects/{projectId}/discussions/{threadId}/comments.
func (h *handlers) handleAddComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req addCommentRequest
	if !decodeBody(w, r, &req) {
		return
	}
	comment, err := h.cmd.AddComment(r.Context(), actor, discussions.AddCommentParams{
		ProjectID: r.PathValue("projectId"),
		ThreadID:  r.PathValue("threadId"),
		Body:      req.Body,
	})
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, commentPayloadFromDomain(comment))
}

// handleDeleteComment: DELETE
// /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId} —
// withdraw one's own comment. The row stays; 200 answers the tombstone.
func (h *handlers) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	comment, err := h.cmd.DeleteComment(r.Context(), actor, r.PathValue("projectId"), r.PathValue("commentId"))
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, commentPayloadFromDomain(comment))
}

// promoteRequest is the POST body of a promotion: the kind, the declarations
// that kind requires, and nothing else — a field belonging to another kind
// is refused by the command rather than ignored.
type promoteRequest struct {
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	IssueType  string `json:"issue_type"`
	BranchID   string `json:"branch_id"`
	Hypothesis *struct {
		QuestionID string `json:"question_id"`
	} `json:"hypothesis"`
	Evidence *struct {
		TargetVersionRef   string          `json:"target_version_ref"`
		EvidenceVersionRef string          `json:"evidence_version_ref"`
		Relation           string          `json:"relation"`
		EvidenceType       string          `json:"evidence_type"`
		Scope              json.RawMessage `json:"scope"`
		Directness         string          `json:"directness"`
		InferenceNature    string          `json:"inference_nature"`
		ReasoningNote      string          `json:"reasoning_note"`
	} `json:"evidence"`
}

// versionRefID strips the platform's `object_version:` ref prefix if the
// caller sent one — the two evidence pins of a promotion are the SAME two
// pins specs/schemas/evidence-assertion.schema.json names, so a caller
// spelling them the way every other surface spells them must not be refused
// here. It does not validate: the command refuses a reference that is not a
// version uuid, and doing the shape check in two places is how the two come
// to disagree about what a legal reference is (cmd/api/rsghttp's versionRefID
// and the publish surface's versionID do exactly this, for exactly this
// reason).
func versionRefID(ref string) string {
	return strings.TrimPrefix(strings.TrimSpace(ref), "object_version:")
}

// handlePromote: POST
// /api/v1/projects/{projectId}/discussions/{threadId}/comments/{commentId}/promotions
// — turn one comment into a proposed research object and record where it
// came from. The path names the thread as well as the comment, and the
// command refuses a pair that does not belong together (a comment id of
// another thread is not found here, docs/45).
//
// 201 answers the promotion, its provenance (the thread and the comment,
// whose author is the proposal's author) and the created object under the
// key of its kind.
func (h *handlers) handlePromote(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req promoteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	in := discussions.PromoteParams{
		ProjectID: r.PathValue("projectId"),
		ThreadID:  r.PathValue("threadId"),
		CommentID: r.PathValue("commentId"),
		Kind:      domain.PromotionKind(req.Kind),
		Title:     req.Title,
		IssueType: req.IssueType,
		BranchID:  req.BranchID,
	}
	if req.Hypothesis != nil {
		in.Hypothesis = &discussions.HypothesisProposal{QuestionID: req.Hypothesis.QuestionID}
	}
	if req.Evidence != nil {
		in.Evidence = &discussions.EvidenceProposal{
			TargetVersionRef:   versionRefID(req.Evidence.TargetVersionRef),
			EvidenceVersionRef: versionRefID(req.Evidence.EvidenceVersionRef),
			Relation:           req.Evidence.Relation,
			EvidenceType:       req.Evidence.EvidenceType,
			Scope:              req.Evidence.Scope,
			Directness:         req.Evidence.Directness,
			InferenceNature:    req.Evidence.InferenceNature,
			ReasoningNote:      req.Evidence.ReasoningNote,
		}
	}
	res, err := h.cmd.Promote(r.Context(), actor, in)
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	body := originPayload(res.Origin)
	switch {
	case res.Issue != nil:
		body["issue"] = issuePayloadFromDomain(*res.Issue)
	case res.Object != nil:
		body["scientific_object"] = objectPayloadFromResult(*res.Object)
	case res.Assertion != nil:
		body["evidence_assertion"] = assertionPayloadFromResult(*res.Assertion)
	}
	authhttp.WriteJSON(w, http.StatusCreated, body)
}

// handleListPromotions: GET
// /api/v1/projects/{projectId}/discussions/promotions?ref=<kind>:<id> — the
// reverse provenance read: every promotion that produced the object a ref
// names, oldest first.
func (h *handlers) handleListPromotions(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("ref"))
	origins, err := h.cmd.PromotionsForRef(r.Context(), reader(r), r.PathValue("projectId"), ref)
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	out := make([]map[string]any, 0, len(origins))
	for _, o := range origins {
		out = append(out, originPayload(o))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"promotions": out})
}

// handleGetPromotion: GET
// /api/v1/projects/{projectId}/discussions/promotions/{promotionId}.
func (h *handlers) handleGetPromotion(w http.ResponseWriter, r *http.Request) {
	origin, err := h.cmd.Promotion(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("promotionId"))
	if err != nil {
		writeDiscussionError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, originPayload(origin))
}
