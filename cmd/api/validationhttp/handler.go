package validationhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/validation"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// handlers owns the validation route.
type handlers struct {
	svc      BranchValidator
	projects ProjectReader
}

// validateRequest is the wire body: the gate to run. The enum is
// pr|main|release|asset (specs/api/openapi.yaml); the draft gate is the
// everyday commit baseline and is not exposed here — it runs server-side
// on every commit instead.
type validateRequest struct {
	Gate string `json:"gate"`
}

// reader resolves the caller for the visibility gate (T0106): a session
// makes them authenticated, its absence makes them anonymous. The route is
// a guarded POST, so anonymous callers are answered 401 by the guard
// before routing — the Reader here is the product-level backstop, the same
// one every other project read runs.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// handleValidate: POST /api/v1/projects/{projectId}/branches/{branchId}:validate
func (h *handlers) handleValidate(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	// The route is a remainder wildcard ("{branch...}") because
	// "{branchId}:validate" is not a legal ServeMux segment; the suffix is
	// split off here, and a path without it was routed here by prefix only
	// — answer 404, not a validation of the wrong branch. (The PathValue
	// key of a remainder wildcard is the name without the dots: "branch".)
	segment := r.PathValue("branch")
	if !strings.HasSuffix(segment, ":validate") {
		http.NotFound(w, r)
		return
	}
	branchID := strings.TrimSuffix(segment, ":validate")
	if branchID == "" {
		http.NotFound(w, r)
		return
	}

	// The project read boundary first (T0106 read semantics): a report
	// about a branch is exactly as visible as its project, so the caller's
	// ability to read the project is decided before any validation runs.
	// A denied read — a private project the caller is not a member of —
	// answers the same existence-hiding 404 as every other project read,
	// never a disclosure of the project's contents. The gate itself is
	// required wiring: without it the route fails closed rather than
	// serving an ungated report.
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, validation.CodeUnavailable,
			"validation service unavailable")
		return
	}
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
				"project not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, validation.CodeUnavailable,
				"validation service unavailable")
		}
		return
	}

	var req validateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, validation.CodeValidation,
			"request body must be valid JSON with a gate")
		return
	}
	gate, err := rsgvalidation.ParseGate(req.Gate)
	if err != nil || gate == rsgvalidation.GateDraft {
		authhttp.WriteError(w, r, http.StatusBadRequest, validation.CodeValidation,
			"gate must be one of pr, main, release, asset")
		return
	}

	report, err := h.svc.ValidateBranch(r.Context(), projectID, branchID, gate)
	if err != nil {
		switch {
		case errors.Is(err, validation.ErrBranchNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, validation.CodeBranchNotFound,
				"branch not found")
		case errors.Is(err, validation.ErrValidation):
			authhttp.WriteError(w, r, http.StatusBadRequest, validation.CodeValidation, err.Error())
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, validation.CodeUnavailable,
				"validation service unavailable")
		}
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, report)
}
