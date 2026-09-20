package pullrequestshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/prdiff"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/integrity"
)

// handlers owns the pull-request routes.
type handlers struct {
	prs      PullRequests
	create   PRCreator
	checks   CheckRunner
	diff     DiffRunner
	projects ProjectReader
}

// PullRequests is the PR read surface (list + get). The production
// implementation is internal/application/pullrequests.Service.
type PullRequests interface {
	List(ctx context.Context, projectID string) ([]domain.PullRequest, error)
	Get(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// PRCreator opens a proposal: POST
// /api/v1/projects/{projectId}/pull-requests (specs/api/openapi.yaml, "Open
// pull request with RSG diff").
//
// The port is deliberately NOT pullrequests.Service.Create. Opening a pull
// request is a governance action — specs/policies/permissions-matrix.csv
// gives it its own cell (open_pr: deny / allow_from_fork / deny / allow /
// allow / allow / allow) — and the package that resolves that cell against
// a REAL project, a REAL membership and (for a non-member) the fork lineage
// is internal/application/forks, whose OpenExternalPR proposes through the
// very same pull-request path. The production implementation is
// *forks.Service; a handler that authorized by itself would be a second,
// drifting copy of the matrix.
type PRCreator interface {
	OpenExternalPR(ctx context.Context, actor domain.User, in forks.OpenPRRequest) (domain.PullRequest, error)
}

// CheckRunner runs the integrity review. The production implementation is
// internal/application/prchecks.Service.
type CheckRunner interface {
	CheckPullRequest(ctx context.Context, projectID string, number int64) (integrity.Report, error)
}

// DiffRunner computes the PR's Research State Diff (the three pinned
// states of the PR plus the target branch's current head). The production
// implementation is internal/application/prdiff.Service.
type DiffRunner interface {
	PullRequestDiff(ctx context.Context, projectID string, number int64) (*diff.Diff, error)
}

// ProjectReader is the visibility gate the handler runs before anything
// else (see wiring.go).
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// codeForbidden is every matrix refusal's wire code (docs/45): rsg, reviews,
// resolutions, merge and mainfreeze all answer AUTH_FORBIDDEN for a denied
// action cell, and the open_pr cell is one of them. Defined here rather than
// taken from a neighbour so this surface names the string it emits — the
// same reason gittokenshttp defines its own access-forbidden code.
const codeForbidden = "AUTH_FORBIDDEN"

// reader resolves the caller for the project read gate (T0106): a session
// makes them authenticated, its absence makes them anonymous.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// prPayload is the wire shape of one pull request (the detail endpoint's
// document; the list endpoint returns an array of it). The state's raw
// values are the stable wire vocabulary of docs/43.
type prPayload struct {
	ID              string    `json:"id"`
	Number          int64     `json:"number"`
	Title           string    `json:"title"`
	Body            string    `json:"body"`
	State           string    `json:"state"`
	SourceBranchID  string    `json:"source_branch_id"`
	TargetBranchID  string    `json:"target_branch_id"`
	BaseStateID     string    `json:"base_state_id"`
	ProposedStateID string    `json:"proposed_state_id"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
}

func prToPayload(pr domain.PullRequest) prPayload {
	return prPayload{
		ID:              pr.ID,
		Number:          pr.Number,
		Title:           pr.Title,
		Body:            pr.Body,
		State:           string(pr.State),
		SourceBranchID:  pr.SourceBranchID,
		TargetBranchID:  pr.TargetBranchID,
		BaseStateID:     pr.BaseStateID,
		ProposedStateID: pr.ProposedStateID,
		CreatedBy:       pr.CreatedBy,
		CreatedAt:       pr.CreatedAt,
	}
}

// gateProject runs the project read boundary: a denied read — a private
// project the caller is not a member of — answers the same
// existence-hiding 404 as every other project read. The projects port is
// required wiring: without it the route fails closed.
func (h *handlers) gateProject(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return false
	}
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
				"project not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
				"pull requests unavailable")
		}
		return false
	}
	return true
}

// prNumber parses the {number} segment. A segment that does not parse is
// a 404, never a 400: the URL simply names nothing (docs/45 existence
// hiding).
func prNumber(w http.ResponseWriter, r *http.Request, raw string) (int64, bool) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		http.NotFound(w, r)
		return 0, false
	}
	return n, true
}

// handleList: GET /api/v1/projects/{projectId}/pull-requests
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.gateProject(w, r, projectID) {
		return
	}
	if h.prs == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	prs, err := h.prs.List(r.Context(), projectID)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	out := make([]prPayload, 0, len(prs))
	for _, pr := range prs {
		out = append(out, prToPayload(pr))
	}
	authhttp.WriteJSON(w, http.StatusOK, out)
}

// handleGet: GET /api/v1/projects/{projectId}/pull-requests/{number}
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, ok := prNumber(w, r, r.PathValue("number"))
	if !ok {
		return
	}
	if !h.gateProject(w, r, projectID) {
		return
	}
	if h.prs == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	pr, err := h.prs.Get(r.Context(), projectID, number)
	if err != nil {
		switch {
		case errors.Is(err, pullrequests.ErrPullRequestNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, pullrequests.CodePullRequestNotFound,
				"pull request not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
				"pull requests unavailable")
		}
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, prToPayload(pr))
}

// createPullRequestRequest is the wire body of the open-pull-request route.
// The contract (specs/api/openapi.yaml, "Open pull request with RSG diff")
// declares the route's parameters and its 201 answer; the body is this
// package's own shape, and it names only what the command reads: the branch
// pair and the proposal's text. The project comes from the path, the opener
// from the guarded principal — never from the client.
type createPullRequestRequest struct {
	// SourceBranchID names the branch the proposed changes live on. For a
	// member it is a branch of the project; for an external contributor it
	// must be a branch of their own fork of it (open_pr =
	// allow_from_fork), which the command resolves.
	SourceBranchID string `json:"source_branch_id"`
	// TargetBranchID names the branch the proposal merges into (main,
	// docs/09 §3).
	TargetBranchID string `json:"target_branch_id"`
	Title          string `json:"title"`
	Body           string `json:"body"`
}

// idempotencyHeader is the contract-required creation key
// (components.parameters.IdempotencyKey: required, minLength 8).
const idempotencyHeader = "Idempotency-Key"

// creationKey reads the Idempotency-Key off the header (and nowhere else:
// a body field would be a second, undocumented way to name the same
// thing). A missing or too-short key is refused in place — the route
// promises that a repeat returns the proposal the first request opened,
// and a request that cannot carry a key cannot be given that promise.
func creationKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			idempotencyHeader+" is required on this route: it is what makes a repeated request return the pull request it already opened instead of opening a second")
		return "", false
	}
	if len(key) < pullrequests.MinCreationKeyLen {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			idempotencyHeader+" must be at least "+strconv.Itoa(pullrequests.MinCreationKeyLen)+" characters (specs/api/openapi.yaml)")
		return "", false
	}
	return key, true
}

// handleCreate: POST /api/v1/projects/{projectId}/pull-requests.
//
// The only write on this surface, and the entry point of the
// open → review_required → approved → merge_ready → merged walk: without it
// the machine's first transition has no product path, which is the gap this
// route closes.
//
// The project read gate is NOT run here, deliberately: the command resolves
// the caller's visibility itself (a private project the caller may not read
// answers the existence-hiding project-not-found), and it resolves the
// open_pr matrix cell beside it. A gate in front would either duplicate that
// resolution or, worse, refuse a non-member whose fork lineage is exactly
// what makes their proposal legitimate.
func (h *handlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	actor, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED",
			"authentication required")
		return
	}
	key, ok := creationKey(w, r)
	if !ok {
		return
	}
	if h.create == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	var req createPullRequestRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
			"request body must be valid JSON")
		return
	}
	pr, err := h.create.OpenExternalPR(r.Context(), actor.User, forks.OpenPRRequest{
		ProjectID:      projectID,
		SourceBranchID: req.SourceBranchID,
		TargetBranchID: req.TargetBranchID,
		Title:          req.Title,
		Body:           req.Body,
		CreationKey:    key,
	})
	if err != nil {
		openError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, prToPayload(pr))
}

// openError renders one proposal-opening outcome as the standard envelope
// (docs/45: one outcome, one stable code, whichever layer reports it). The
// codes are the shared strings of the packages the outcomes belong to, so
// the same refusal carries the same name whether it came from this route or
// from the review and merge routes downstream of it.
func openError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := openErrorOutcome(err)
	authhttp.WriteError(w, r, status, code, message)
}

func openErrorOutcome(err error) (status int, code, message string) {
	switch {
	case errors.Is(err, forks.ErrForbidden):
		// The matrix refusal. A caller who may not propose here learns
		// nothing about what exists: the same answer covers a project they
		// may read but not propose to and one they may not read at all is
		// answered below as project-not-found, which is the distinction
		// docs/45 draws (an unreadable project's existence is not
		// confirmed).
		return http.StatusForbidden, codeForbidden, "you are not permitted to open a pull request in this project"
	case errors.Is(err, forks.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		return http.StatusNotFound, projects.CodeProjectNotFound, "project not found"
	case errors.Is(err, forks.ErrBranchNotFound), errors.Is(err, pullrequests.ErrBranchNotFound):
		return http.StatusNotFound, pullrequests.CodeBranchNotFound, "branch not found in the project"
	case errors.As(err, new(*pullrequests.BranchNotActiveError)):
		// A closed research path (merged/aborted) accepts no proposal. The
		// status matches the RSG write surface's answer for the same fact
		// (rsghttp: branch lifecycle not active => 409).
		return http.StatusConflict, pullrequests.CodeBranchNotActive, "branch lifecycle is not active"
	case errors.Is(err, pullrequests.ErrBranchHeadMissing):
		return http.StatusConflict, pullrequests.CodeBranchHeadMissing, "branch has no head state"
	case errors.Is(err, pullrequests.ErrBranchUnstructuredChanges):
		// The source branch's recorded semantic state is
		// unstructured_changes (00042's pull_request_semantic_gate): its
		// content carries changes the platform cannot parse, and docs/16
		// §4 forbids proposing them until the required scientific
		// semantics are filled. 409, the contract's answer for this state,
		// and the same status the closed-branch and missing-head cases
		// above use — the request is well-formed, the branch is simply not
		// in a state that can carry a formal proposal. The message names
		// the fix, because the caller is the one who has to make it.
		return http.StatusConflict, pullrequests.CodeBranchUnstructuredChanges,
			"the source branch carries changes the platform cannot parse; fill the required scientific semantics before proposing from it"
	case errors.Is(err, forks.ErrValidation), errors.Is(err, pullrequests.ErrValidation):
		return http.StatusBadRequest, pullrequests.CodeValidation, "validation failed"
	default:
		return http.StatusServiceUnavailable, pullrequests.CodeUnavailable, "pull requests unavailable"
	}
}

// handleChecks: GET /api/v1/projects/{projectId}/pull-requests/{number}/checks
func (h *handlers) handleChecks(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, ok := prNumber(w, r, r.PathValue("number"))
	if !ok {
		return
	}
	if !h.gateProject(w, r, projectID) {
		return
	}
	if h.checks == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	report, err := h.checks.CheckPullRequest(r.Context(), projectID, number)
	if err != nil {
		switch {
		case errors.Is(err, prchecks.ErrPullRequestNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, pullrequests.CodePullRequestNotFound,
				"pull request not found")
		case errors.Is(err, prchecks.ErrValidation):
			// A fixed user-facing line, never err.Error(): the service's
			// validation errors carry the package prefix ("prchecks:
			// validation failed: ..."), which belongs in the log, not on
			// the wire. The code is the stable vocabulary (docs/45).
			authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
				"invalid project or pull request number")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
				"pull requests unavailable")
		}
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, report)
}

// handleDiff: GET /api/v1/projects/{projectId}/pull-requests/{number}/diff
//
// The PR's Research State Diff — the canonical document of
// internal/rsg/diff (objects created/updated/aborted/reopened, relation
// changes, the categorized summary, and the recorded file-level git refs
// the raw-file view links from). It is the API contract's own shape
// (specs/api/openapi.yaml: "Get Research State Diff and raw file diff
// references"), written verbatim: the engine's fixed field order is the
// wire order, so two renders of the same PR are byte-identical.
func (h *handlers) handleDiff(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, ok := prNumber(w, r, r.PathValue("number"))
	if !ok {
		return
	}
	if !h.gateProject(w, r, projectID) {
		return
	}
	if h.diff == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
			"pull requests unavailable")
		return
	}
	document, err := h.diff.PullRequestDiff(r.Context(), projectID, number)
	if err != nil {
		switch {
		case errors.Is(err, prdiff.ErrPullRequestNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, pullrequests.CodePullRequestNotFound,
				"pull request not found")
		case errors.Is(err, prdiff.ErrStateNotFound):
			// One of the three states the diff compares no longer
			// resolves. The code is the shared missing-state vocabulary
			// (branches and states speak the same string); the message
			// names no identifier beyond the outcome (docs/45).
			authhttp.WriteError(w, r, http.StatusNotFound, prdiff.CodeStateNotFound,
				"one of the compared states does not exist")
		case errors.Is(err, prdiff.ErrValidation):
			// A fixed user-facing line, never err.Error(): the service's
			// validation errors carry the package prefix, which belongs in
			// the log, not on the wire (docs/45).
			authhttp.WriteError(w, r, http.StatusBadRequest, pullrequests.CodeValidation,
				"invalid project or pull request number")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, pullrequests.CodeUnavailable,
				"pull requests unavailable")
		}
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, document)
}
