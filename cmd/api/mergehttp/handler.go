package mergehttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/cmd/api/pullrequestshttp"
	"github.com/lichman0405/post/internal/application/merge"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// mergeVerb is the literal suffix the route's last segment carries
// (specs/api/openapi.yaml: /pull-requests/{prId}:merge). The mux cannot
// express it as a wildcard, so the handler splits it off.
const mergeVerb = ":merge"

// idempotencyHeader is the header the contract requires on this route
// (components.parameters.IdempotencyKey: required, minLength 8).
const idempotencyHeader = "Idempotency-Key"

// minIdempotencyKeyLen is that parameter's minLength. The bound is the
// contract's, not a preference: the key names one merge forever, so it has to
// be long enough not to collide by accident, and the merge ledger's unique
// index on (project_id, idempotency_key) is what enforces it beyond this check.
const minIdempotencyKeyLen = 8

// mergeCommand is the command slice this transport needs — the T0406 semantic
// merge engine. The interface exists so the handlers are unit-testable against
// a fake; the production value is *merge.Service.
type mergeCommand interface {
	Merge(ctx context.Context, actor domain.User, in merge.Input) (*merge.Result, error)
}

// projectReader is the project read gate the route runs before the command:
// the same existence-hiding boundary every other project route runs, so a
// caller who may not read the project cannot learn whether its PR exists (nor
// burn a merge attempt on it). The production value is projects.Service.
type projectReader interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// handlers owns the merge route.
type handlers struct {
	cmd      mergeCommand
	projects projectReader
	// review serves the collection's other suffix verb, ":request-review".
	// Optional wiring: nil means this mux mounts the merge surface alone, and
	// that verb fails closed (503).
	review http.HandlerFunc
}

// mergeRequest is the optional body: the state commit's message. Nothing else
// is accepted — the merge's inputs are the PR row and the recorded human
// decisions, and a client that could hand in a plan would be handing in the
// thing the server is supposed to derive (docs/22 §7).
type mergeRequest struct {
	Message string `json:"message"`
}

// mergePayload is the client-visible result of a merge. It names the records
// the merge produced so the caller can read them through the ordinary routes;
// the plan itself is not embedded (its digest is the reference an auditor
// recomputes from the recorded triple).
type mergePayload struct {
	Number          int64              `json:"number"`
	MergeID         string             `json:"merge_id"`
	StateID         string             `json:"state_id"`
	CommitID        string             `json:"commit_id,omitempty"`
	PlanDigest      string             `json:"plan_digest"`
	TargetBranchID  string             `json:"target_branch_id"`
	GitState        string             `json:"git_state"`
	GitSHA          *string            `json:"git_sha"`
	GitError        string             `json:"git_error,omitempty"`
	Applied         int                `json:"applied"`
	KeptTarget      int                `json:"kept_target"`
	Carried         int                `json:"carried"`
	Aborted         int                `json:"aborted"`
	Withheld        int                `json:"withheld"`
	SourceClosed    bool               `json:"source_branch_closed"`
	WrittenVersions []mergedVersionRow `json:"written_versions,omitempty"`
	// Replayed reports that this call replayed a previous request with the
	// same Idempotency-Key instead of merging anything (docs/22 §3): the
	// merge named here is the one the FIRST call produced.
	Replayed bool `json:"replayed"`
	// MergedAt is the merge row's own timestamp.
	MergedAt time.Time `json:"merged_at"`
}

// mergedVersionRow is one version row the merge wrote into the accepted state.
type mergedVersionRow struct {
	TargetKind string `json:"target_kind"`
	TargetID   string `json:"target_id"`
	VersionID  string `json:"version_id"`
	VersionNo  int    `json:"version_no"`
}

func payloadFromResult(res *merge.Result) mergePayload {
	m := res.Merge
	out := mergePayload{
		// The merge row identifies the PR by id; the number the wire
		// addresses is the one this call carried (and the one the result
		// echoes).
		Number:         res.Number,
		MergeID:        m.ID,
		StateID:        m.ResultStateID,
		CommitID:       res.Commit.ID,
		PlanDigest:     m.PlanDigest,
		TargetBranchID: m.TargetBranchID,
		GitState:       string(m.GitState),
		GitSHA:         m.GitSHA,
		GitError:       m.GitError,
		Applied:        m.Applied,
		KeptTarget:     m.KeptTarget,
		Carried:        m.Carried,
		Aborted:        m.Aborted,
		Withheld:       m.Withheld,
		SourceClosed:   res.SourceBranchClosed,
		Replayed:       res.Replayed,
		MergedAt:       m.CreatedAt,
	}
	if out.StateID == "" {
		out.StateID = res.State.ID
	}
	for _, w := range res.Written {
		out.WrittenVersions = append(out.WrittenVersions, mergedVersionRow{
			TargetKind: string(w.TargetKind), TargetID: w.TargetID,
			VersionID: w.VersionID, VersionNo: w.VersionNo,
		})
	}
	return out
}

// reader resolves the caller for the project read gate (T0106): a session makes
// them authenticated, its absence makes them anonymous.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// pathNumber splits the ":merge" suffix off the route's last segment and parses
// the remainder as the PR number. A segment without the suffix was routed here
// by prefix alone (the remainder wildcard matches everything below
// /pull-requests/): that is a 404 — the URL names nothing. A non-numeric number
// is a 400: the suffix makes the intent unambiguous, so the caller is told the
// part that is wrong.
func pathNumber(w http.ResponseWriter, r *http.Request) (int64, bool) {
	segment := r.PathValue("number")
	if !strings.HasSuffix(segment, mergeVerb) {
		http.NotFound(w, r)
		return 0, false
	}
	raw := strings.TrimSuffix(segment, mergeVerb)
	if raw == "" || strings.Contains(raw, "/") {
		http.NotFound(w, r)
		return 0, false
	}
	number, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || number < 1 {
		authhttp.WriteError(w, r, http.StatusBadRequest, merge.CodeValidation,
			"the pull request number must be a positive integer")
		return 0, false
	}
	return number, true
}

// decodeMessage reads the optional body. An absent body is the common case
// (the contract defines none) and means "generate the message"; a body that is
// present must be a JSON object with at most the message in it.
func decodeMessage(w http.ResponseWriter, r *http.Request, dst *mergeRequest) bool {
	if r.Body == nil || r.ContentLength == 0 {
		return true
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		authhttp.WriteError(w, r, http.StatusBadRequest, merge.CodeValidation,
			"request body must be a JSON object")
		return false
	}
	return true
}

// mergeKey reads the contract-required Idempotency-Key, refusing a missing or
// too-short one in place (400). The route moves frozen main: a request that
// cannot be replayed onto the merge it already produced is refused rather than
// merged without one. The zero value of the returned pointer means "refused".
func mergeKey(w http.ResponseWriter, r *http.Request) (*string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		authhttp.WriteError(w, r, http.StatusBadRequest, merge.CodeValidation,
			idempotencyHeader+" is required on a merge: it is what makes a repeated request return the merge it already made instead of advancing main twice")
		return nil, false
	}
	if len(key) < minIdempotencyKeyLen {
		authhttp.WriteError(w, r, http.StatusBadRequest, merge.CodeValidation,
			idempotencyHeader+" must be at least "+strconv.Itoa(minIdempotencyKeyLen)+" characters (specs/api/openapi.yaml)")
		return nil, false
	}
	return &key, true
}

// handlePullRequestVerb: POST /api/v1/projects/{projectId}/pull-requests/
// {number...} — every suffix verb the contract spells inside this collection's
// last path segment.
//
// The route is registered as a REMAINDER wildcard because ServeMux cannot match
// a suffix inside a segment ("{number}:merge" panics with "bad wildcard
// segment"), and a prefix has exactly ONE remainder owner: a second
// registration under it panics with "conflicts with pattern". This handler is
// therefore the dispatcher for the whole segment, not only for ":merge". The
// collection's second verb, ":request-review", is served by the package that
// owns the collection and the pull-request document it answers with
// (cmd/api/pullrequestshttp.RequestReviewHandler, wired in as
// Deps.ReviewRequest); the merge command is not involved in it. Every other
// suffix is the 404 it has always been — no route is widened by the trick.
func (h *handlers) handlePullRequestVerb(w http.ResponseWriter, r *http.Request) {
	switch segment := r.PathValue("number"); {
	case strings.HasSuffix(segment, pullrequestshttp.RequestReviewVerb):
		if h.review == nil {
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, merge.CodeUnavailable,
				"pull request review unavailable")
			return
		}
		h.review.ServeHTTP(w, r)
	case strings.HasSuffix(segment, mergeVerb):
		h.handleMerge(w, r)
	default:
		http.NotFound(w, r)
	}
}

// handleMerge: POST /api/v1/projects/{projectId}/pull-requests/{number}:merge.
// The only write on the merge surface: it hands the command the actor and the
// key and maps the command's own outcome onto the wire. There is no update, no
// delete and no second way to move main.
func (h *handlers) handleMerge(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	number, ok := pathNumber(w, r)
	if !ok {
		return
	}
	key, ok := mergeKey(w, r)
	if !ok {
		return
	}
	var req mergeRequest
	if !decodeMessage(w, r, &req) {
		return
	}
	principal, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED",
			"authentication required")
		return
	}
	// The project read gate, before the command: a project the caller cannot
	// read answers the same existence-hiding 404 every other project route
	// answers, and the command is never reached for it.
	if h.projects == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, merge.CodeUnavailable,
			"merge service unavailable")
		return
	}
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		switch {
		case errors.Is(err, projects.ErrProjectNotFound):
			authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
				"project not found")
		default:
			authhttp.WriteError(w, r, http.StatusServiceUnavailable, merge.CodeUnavailable,
				"merge service unavailable")
		}
		return
	}

	res, err := h.cmd.Merge(r.Context(), principal.User, merge.Input{
		ProjectID:      projectID,
		Number:         number,
		Message:        req.Message,
		IdempotencyKey: key,
	})
	if err != nil {
		writeMergeError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, payloadFromResult(res))
}

// wireStatus maps the merge package's own wire codes (docs/45: one outcome,
// one stable code, whichever layer reports it) onto HTTP statuses. A code
// missing from the table is a 500: the transport must not invent a status for
// an outcome it does not know.
var wireStatus = map[string]int{
	merge.CodeForbidden:           http.StatusForbidden,
	merge.CodeValidation:          http.StatusBadRequest,
	merge.CodePullRequestNotFound: http.StatusNotFound,
	merge.CodeBranchNotFound:      http.StatusNotFound,
	merge.CodeNotMergeable:        http.StatusConflict,
	merge.CodeMergeBlocked:        http.StatusConflict,
	merge.CodeMergeStale:          http.StatusConflict,
	merge.CodeAlreadyMerged:       http.StatusConflict,
	"BRANCH_NOT_ACTIVE":           http.StatusConflict,
	merge.CodeIntegrityBlocked:    http.StatusConflict,
	merge.CodePolicyRefused:       http.StatusForbidden,
	merge.CodeUnavailable:         http.StatusServiceUnavailable,
	projects.CodeProjectNotFound:  http.StatusNotFound,
}

// coded is the shape every merge refusal shares: a stable wire code.
type coded interface {
	Code() string
}

// writeMergeError maps one command error onto the wire envelope. The gate
// refusal carries the integrity report's own explanation (client-safe: the
// engine renders every failed check with its subject, detail and why) instead
// of a generic line — the author has to see which checks blocked the merge.
// The complete report stays on the in-process error (merge.GateRefused.Report);
// what the wire carries is the engine's rendering of it.
func writeMergeError(w http.ResponseWriter, r *http.Request, err error) {
	var refused *merge.GateRefused
	if errors.As(err, &refused) {
		message := refused.Error()
		if refused.Report.Explanation != "" {
			message = refused.Report.Explanation
		}
		authhttp.WriteError(w, r, http.StatusConflict, merge.CodeIntegrityBlocked, message)
		return
	}
	// The package's sentinel errors carry no code of their own (the service
	// raises them where the outcome is structural, not per-record), so the
	// transport names their wire code here. Each case RETURNS: the sentinels
	// below are also values of the coded interface, and falling through would
	// write a second envelope over the first.
	switch {
	case errors.Is(err, merge.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, merge.CodeValidation, err.Error())
		return
	case errors.Is(err, merge.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, merge.CodeForbidden,
			"the actor may not merge into this branch")
		return
	case errors.Is(err, merge.ErrPullRequestNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, merge.CodePullRequestNotFound,
			"pull request not found")
		return
	case errors.Is(err, merge.ErrBranchNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, merge.CodeBranchNotFound,
			"branch not found in the project")
		return
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound, "project not found")
		return
	case errors.Is(err, merge.ErrStore):
		// An unreachable store is reported by its code, never by its text:
		// the driver's message must not reach the wire.
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, merge.CodeUnavailable,
			"the merge could not be completed; nothing was written")
		return
	}
	var c coded
	if errors.As(err, &c) {
		code := c.Code()
		status, ok := wireStatus[code]
		if !ok {
			observability.LoggerFromContext(r.Context()).Error("merge handler: unmapped wire code",
				"error", err, "code", code)
			authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
				"an internal error occurred")
			return
		}
		// A dependency outage is reported by its code, never by its text: an
		// unreachable store must not leak driver detail onto the wire.
		message := err.Error()
		if code == merge.CodeUnavailable {
			message = "the merge could not be completed; nothing was written"
		}
		authhttp.WriteError(w, r, status, code, message)
		return
	}
	observability.LoggerFromContext(r.Context()).Error("merge handler: unexpected error", "error", err)
	authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
		"an internal error occurred")
}
