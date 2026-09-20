package forkshttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/forks"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// handlers owns the fork route.
type handlers struct {
	forks ForkService
	// projects is the read the visibility condition of the request body
	// needs (see handleFork). Optional: without it a request that asks for
	// a public fork is refused rather than granted unchecked.
	projects ProjectReader
	// missing lists the environment keys provisioning needs but that are
	// unset. Non-empty means the fork route is disabled: the deployment
	// cannot provision the fork's repository, so the route answers 503
	// before any project row is written.
	missing []string
}

// ForkService is the external-contribution command. The production
// implementation is *forks.Service, whose Fork resolves the create_branch
// cell for the caller's class, refuses an unreadable parent with the
// existence-hiding not-found, and is idempotent against canonical state.
type ForkService interface {
	Fork(ctx context.Context, actor domain.User, in forks.ForkRequest) (forks.ForkResult, error)
}

// ProjectReader is the project read the visibility condition runs. The
// production implementation is *projects.Service (via projectshttp.API's
// Service), which applies the same read gate every other project read does.
type ProjectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// Wire codes (docs/45: stable names, no dependency detail). Outcomes shared
// with other packages carry those packages' own constants, so one outcome
// has one name on the wire whichever surface reports it; the two below are
// this surface's.
const (
	codeUnauthenticated = "AUTH_UNAUTHENTICATED"
	codeForbidden       = "AUTH_FORBIDDEN"
	// codeBranchNotFound is the 404 of "a source branch that is not in the
	// project being forked". Defined here rather than imported from the
	// pull-request surface so this package names the string it emits — the
	// same reason pullrequestshttp defines its own forbidden code. The
	// string is the shared vocabulary (docs/45), not a local invention.
	codeBranchNotFound = "BRANCH_NOT_FOUND"
	// CodeForkNameTaken is the 409 of the fork route: the name the fork's
	// project must have is held by a project that is not this (parent,
	// caller) pair's recorded fork. The contract states the two situations
	// that collapse into it (an own project holding the name, or both the
	// derived and the reserved name taken by others) and deliberately does
	// not claim one over the other.
	CodeForkNameTaken = "FORK_NAME_TAKEN"
)

// forkRequest is the wire body of the fork route. Every field is optional
// (specs/api/openapi.yaml: the request body is not required), and the shape
// names only what the command reads: the project comes from the path and
// the actor from the guarded principal, never from the client.
type forkRequest struct {
	// SourceBranchID names the parent line to fork; null or absent means
	// the parent's canonical line (main).
	SourceBranchID *string `json:"source_branch_id"`
	// Name and Purpose are the fork project's display name and stated
	// purpose; absent means the service derives them.
	Name    *string `json:"name"`
	Purpose *string `json:"purpose"`
	// Visibility is the fork project's visibility; null or absent means
	// private (fail closed — the fork's content is the forker's until they
	// publish it, and publishing is a governance action, not a side effect
	// of forking).
	Visibility *string `json:"visibility"`
	// BranchName names the branch the copy lands on in the fork's project;
	// absent means fork/<source branch name>.
	BranchName *string `json:"branch_name"`
}

// optString dereferences an optional JSON string field: null and absent are
// the same thing to this command (the service reads "" as "not stated").
func optString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// forkPayload is the response document of the fork route: the lineage row,
// the fork project and the fork branch, plus whether this request was the
// one that created them.
type forkPayload struct {
	Fork    forkLineagePayload `json:"fork"`
	Project forkProject        `json:"project"`
	Branch  forkBranch         `json:"branch"`
	// AlreadyForked is the 200/201 distinction (specs/api/openapi.yaml:
	// "Fork created" vs "Already forked"). It is the recorded state's
	// answer, not a remembered request key: a repeated request finds the
	// (parent, caller) pair's fork and writes nothing.
	AlreadyForked bool `json:"already_forked"`
	// Imported reports whether THIS request ran the content copy. False
	// when the fork already carried its content — a repeat never copies
	// twice.
	Imported bool `json:"imported"`
}

// forkLineagePayload is the lineage row (project_forks, 00086): the stored
// fact that the fork project was forked from the parent by this actor,
// along the branch pair the content travelled.
type forkLineagePayload struct {
	ForkProjectID   string  `json:"fork_project_id"`
	ParentProjectID string  `json:"parent_project_id"`
	ForkedBy        string  `json:"forked_by"`
	RelationType    string  `json:"relation_type"`
	SourceBranchID  string  `json:"source_branch_id"`
	ForkBranchID    string  `json:"fork_branch_id"`
	ForkedSHA       *string `json:"forked_sha"`
	CreatedAt       string  `json:"created_at"`
}

// forkProject is the fork project as the client sees it. It is this
// package's own shape rather than a re-export of the projects surface's:
// the fork route answers what it created, and a handler that reached into
// another package's payload would couple two surfaces that are allowed to
// move apart.
type forkProject struct {
	ID         string `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Purpose    string `json:"purpose"`
	Visibility string `json:"visibility"`
}

// forkBranch is the branch the copy landed on.
type forkBranch struct {
	ID         string `json:"id"`
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
	GitRef     string `json:"git_ref"`
	Visibility string `json:"visibility"`
	Lifecycle  string `json:"lifecycle"`
}

func forkPayloadFromResult(res forks.ForkResult) forkPayload {
	return forkPayload{
		Fork: forkLineagePayload{
			ForkProjectID:   res.Fork.ForkProjectID,
			ParentProjectID: res.Fork.ParentProjectID,
			ForkedBy:        res.Fork.ForkedBy,
			RelationType:    res.Fork.RelationType,
			SourceBranchID:  res.Fork.SourceBranchID,
			ForkBranchID:    res.Fork.ForkBranchID,
			ForkedSHA:       res.Fork.ForkedSHA,
			CreatedAt:       res.Fork.CreatedAt.UTC().Format(time.RFC3339Nano),
		},
		Project: forkProject{
			ID:         res.Project.ID,
			Slug:       res.Project.Slug,
			Name:       res.Project.Name,
			Purpose:    res.Project.Purpose,
			Visibility: string(res.Project.Visibility),
		},
		Branch: forkBranch{
			ID:         res.Branch.ID,
			ProjectID:  res.Branch.ProjectID,
			Name:       res.Branch.Name,
			GitRef:     res.Branch.GitRef,
			Visibility: string(res.Branch.Visibility),
			Lifecycle:  string(res.Branch.Lifecycle),
		},
		AlreadyForked: res.AlreadyForked,
		Imported:      res.Imported,
	}
}

// handleFork: POST /api/v1/projects/{projectId}/forks.
//
// The order of the checks is the contract's:
//
//  1. the caller — the route is for authenticated users (docs/04 §2), and
//     the matrix's public_anonymous column denies the cell. A missing
//     session is answered here as well as by the guard in front.
//  2. the command must be wired; without it the route fails closed rather
//     than forking with no authorization at all.
//  3. the body, when one is sent, must be an object of the declared shape.
//  4. the one condition the contract states at this layer: a fork of a
//     parent that is not PUBLIC may not be created public.
//  5. the command. Everything else — public/private, membership, the
//     existence-hiding not-found, the idempotent repeat — is its answer.
func (h *handlers) handleFork(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	principal, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, codeUnauthenticated, "authentication required")
		return
	}
	if len(h.missing) > 0 {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"forks unavailable: repository provisioning is disabled; set "+strings.Join(h.missing, ", "))
		return
	}
	if h.forks == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"forks unavailable")
		return
	}
	var req forkRequest
	if !decodeOptionalBody(w, r, &req) {
		return
	}
	visibility, ok := forkVisibility(w, r, req.Visibility)
	if !ok {
		return
	}
	if visibility == domain.VisibilityPublic && !h.publicParent(w, r, projectID) {
		return
	}
	result, err := h.forks.Fork(r.Context(), principal.User, forks.ForkRequest{
		ProjectID:      projectID,
		Name:           optString(req.Name),
		Purpose:        optString(req.Purpose),
		Visibility:     visibility,
		SourceBranchID: optString(req.SourceBranchID),
		BranchName:     optString(req.BranchName),
	})
	if err != nil {
		forkError(w, r, err)
		return
	}
	status := http.StatusCreated
	if result.AlreadyForked {
		// The contract's 200: the pair's fork already exists, and this
		// request created nothing — no project, branch or lineage row, and
		// no audit row or event.
		status = http.StatusOK
	}
	authhttp.WriteJSON(w, status, forkPayloadFromResult(result))
}

// decodeOptionalBody parses the request body when one was sent. The body is
// NOT required on this route (specs/api/openapi.yaml): an empty one is the
// ordinary "fork the parent's main, private" request. A body that is
// present but undecodable is refused in place — accepting it would mean
// forking a line or a name the caller did not ask for.
func decodeOptionalBody(w http.ResponseWriter, r *http.Request, dst *forkRequest) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true // no body at all: the defaults stand
		}
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	// Trailing content after the object is a body this route cannot read
	// whole; refusing it keeps "the request body is the object" true.
	if err := dec.Decode(new(struct{})); !errors.Is(err, io.EOF) {
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed,
			"request body must be a single JSON object")
		return false
	}
	return true
}

// forkVisibility reads the requested visibility: absent or null is the
// fail-closed private, the two declared values pass, and anything else is a
// request-shape error. A value outside the enum must not be folded into
// private silently — the caller asked for a visibility the contract does
// not define, and answering 201 with a different one would report a fork
// they did not request.
func forkVisibility(w http.ResponseWriter, r *http.Request, v *string) (domain.ProjectVisibility, bool) {
	if v == nil || *v == "" {
		return "", true // the service applies its own fail-closed default
	}
	switch domain.ProjectVisibility(*v) {
	case domain.VisibilityPublic:
		return domain.VisibilityPublic, true
	case domain.VisibilityPrivate:
		return domain.VisibilityPrivate, true
	default:
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed,
			"visibility must be public or private")
		return "", false
	}
}

// publicParent reports whether the project being forked is PUBLIC, and
// answers the request itself when that cannot be established.
//
// It runs ONLY for a request that asks for a public fork, which is the only
// case the contract's narrowing is about, so the ordinary request costs no
// extra read and its answers are exactly the command's.
//
// The condition is the contract's: a fork of a parent that is not PUBLIC
// must not be created public. The copy carries the parent's content, and a
// contributor of a private project holds allow on create_branch but deny on
// publish_private_to_public (specs/policies/permissions-matrix.csv), so
// granting public here would hand that class through a second door the
// capability the first one refuses. It is applied by the repository's own
// default-deny pending the open permission-semantics decision recorded in
// the contract — it creates no rule of its own.
//
// A project the caller may not read answers the existence-hiding 404 (the
// same answer the command would give: the read gate is the same one). A
// project that cannot be read at all — no reader wired, or a store failure
// — answers 503: an unevaluable condition is refused, never granted.
func (h *handlers) publicParent(w http.ResponseWriter, r *http.Request, projectID string) bool {
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"forks unavailable")
		return false
	}
	p, _ := authhttp.PrincipalFrom(r.Context()) // handleFork resolved the caller already
	project, err := h.projects.Get(r.Context(), projects.Reader{UserID: p.User.ID, Authenticated: true}, projectID)
	if err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound), errors.Is(err, forks.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound, "project not found")
		case errors.Is(err, projects.ErrForbidden):
			authhttp.WriteError(w, r, http.StatusForbidden, codeForbidden,
				"you are not permitted to fork this project")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
				"forks unavailable")
		}
		return false
	}
	if project.Visibility != domain.VisibilityPublic {
		authhttp.WriteError(w, r, http.StatusForbidden, codeForbidden,
			"a fork of a project that is not public cannot be created public: the copy carries the parent's content, and publishing it is a governance action (publish_private_to_public), not a side effect of forking")
		return false
	}
	return true
}

// forkError renders one fork outcome as the standard envelope (docs/45: one
// outcome, one stable code). Every branch here answers a status the
// contract declares for this route; the codes are the shared strings of the
// packages the outcomes belong to, so the same refusal carries the same
// name wherever it is reported.
func forkError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, forks.ErrForbidden):
		// create_branch is external_fork_only for a non-member: a project
		// the caller can read that is not public is refused here, and so is
		// a class the matrix denies outright (a viewer, an agent).
		authhttp.WriteError(w, r, http.StatusForbidden, codeForbidden,
			"you are not permitted to fork this project")
	case errors.Is(err, forks.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		// The existence-hiding answer: an unknown project, or one the
		// caller may not read. A non-member forking a private project is
		// refused HERE, before any condition is resolved, and never with a
		// 403 — a 403 would confirm that the project exists.
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound, "project not found")
	case errors.Is(err, forks.ErrBranchNotFound), errors.Is(err, forks.ErrNoSourceBranch):
		// A source branch that is not in the project being forked — said
		// the same way for an unknown branch and for a foreign one, and for
		// a project whose canonical line does not exist (there is no branch
		// there to name). No existence leaks through the difference.
		authhttp.WriteError(w, r, http.StatusNotFound, codeBranchNotFound,
			"branch not found in the project being forked")
	case errors.Is(err, forks.ErrForkSlugTaken):
		authhttp.WriteError(w, r, http.StatusConflict, CodeForkNameTaken,
			"the name this fork's project must have is already taken; a fork of this project by this account cannot be created under that name")
	case errors.Is(err, forks.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, projects.CodeValidationFailed, "validation failed")
	case errors.Is(err, forks.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"forks unavailable")
	default:
		// Nothing is claimed about an outcome this surface cannot place.
		observability.LoggerFromContext(r.Context()).Error("forks handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, projects.CodeServiceUnavailable,
			"forks unavailable")
	}
}
