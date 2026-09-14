package pullrequestshttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/prchecks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/pullrequests"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/integrity"
)

// handlers owns the pull-request routes.
type handlers struct {
	prs      PullRequests
	checks   CheckRunner
	projects ProjectReader
}

// PullRequests is the PR read surface (list + get). The production
// implementation is internal/application/pullrequests.Service.
type PullRequests interface {
	List(ctx context.Context, projectID string) ([]domain.PullRequest, error)
	Get(ctx context.Context, projectID string, number int64) (domain.PullRequest, error)
}

// CheckRunner runs the integrity review. The production implementation is
// internal/application/prchecks.Service.
type CheckRunner interface {
	CheckPullRequest(ctx context.Context, projectID string, number int64) (integrity.Report, error)
}

// ProjectReader is the visibility gate the handler runs before anything
// else (see wiring.go).
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

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
