package aborthttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/aborts"
	"github.com/lichman0405/post/internal/observability"
)

// idempotencyHeader is the header the contract requires on this route
// (components.parameters.IdempotencyKey: required, minLength 8). The abort
// route's own declaration does not name it; see the package doc for why it
// is required here anyway and where the bound comes from.
const idempotencyHeader = "Idempotency-Key"

// abortVerb is the literal suffix the route's last segment must carry. The
// mux sorts every POST under /objects/ onto this handler (the wildcard is
// the rest of the path), so a segment without the suffix is a URL that
// names nothing — a 404, not a malformed abort request.
const abortVerb = ":abort-proposal"

// maxBodyBytes bounds the request body. The body is four short fields; the
// explanation is bounded at 4096 by the command, so this is the outer
// envelope bound rather than a scientific-content bound.
const maxBodyBytes = 64 << 10

// abortCommand is the command slice this transport needs. The interface
// exists so the handlers are unit-testable against a fake; the production
// value is *aborts.Service.
type abortCommand interface {
	AbortProposal(ctx context.Context, actor aborts.Actor, in aborts.Input) (aborts.Result, error)
}

// handlers owns the abort route.
type handlers struct {
	cmd abortCommand
}

// abortRequest is the request body. Field names are the MCP tool's argument
// names (specs/mcp/tools.json:23) plus docs/46:7's optional replacement ref
// — see the package doc.
type abortRequest struct {
	// ObjectVersionRef names the version to abort, as the platform spells
	// version refs: `object_version:<uuid>`. The transport strips the
	// prefix and the command receives the bare uuid, the same separation
	// the publish and evidence surfaces keep.
	ObjectVersionRef string `json:"object_version_ref"`
	// ReasonCode is an open token in V1: the body does not enumerate it and
	// must not start (internal/domain's AbortRecord carries the ruling and
	// its cost).
	ReasonCode string `json:"reason_code"`
	// Explanation is the human explanation docs/46:7 requires.
	Explanation string `json:"explanation"`
	// ReplacementRef is optional; absent and "" mean the same thing (none
	// was given) and neither is stored as an empty string.
	ReplacementRef string `json:"replacement_ref"`
}

// abortPayload is the client-visible result of a proposal. It reports the
// version the abort is about, the version row the abort wrote, the proposal
// branch and PR that carry it, and the record docs/46:7 requires. The
// lifecycle state is reported as the ROW's state — 'aborted' — never as
// "the object is aborted": on main the object is aborted only once the
// proposal merges, and that is what `pull_request_number` names.
type abortPayload struct {
	ProjectID         string `json:"project_id"`
	ObjectID          string `json:"object_id"`
	AbortedVersionID  string `json:"aborted_version_id"`
	AbortedVersionNo  int    `json:"aborted_version_no"`
	VersionID         string `json:"version_id"`
	VersionNo         int    `json:"version_no"`
	LifecycleState    string `json:"lifecycle_state"`
	BranchID          string `json:"branch_id"`
	BranchName        string `json:"branch_name"`
	PullRequestNumber int64  `json:"pull_request_number,omitempty"`
	PRState           string `json:"pull_request_state,omitempty"`
	ReasonCode        string `json:"reason_code"`
	Explanation       string `json:"explanation"`
	ReplacementRef    string `json:"replacement_ref,omitempty"`
	DecidedBy         string `json:"decided_by"`
	DecidedAt         string `json:"decided_at"`
	// Replayed reports that this call found the proposal an earlier request
	// with the same Idempotency-Key created and wrote nothing: no second
	// version, no second audit row, no second event.
	Replayed bool `json:"replayed"`
}

// handleAbortProposal: POST
// /api/v1/projects/{projectId}/objects/{objectId}:abort-proposal.
func (h *handlers) handleAbortProposal(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	objectID, ok := pathObjectID(w, r)
	if !ok {
		return
	}
	key, ok := abortKey(w, r)
	if !ok {
		return
	}
	principal, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, "AUTH_UNAUTHENTICATED",
			"authentication required")
		return
	}
	if h.cmd == nil {
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, aborts.CodeServiceUnavailable,
			"abort service unavailable")
		return
	}
	var req abortRequest
	if !decodeAbortBody(w, r, &req) {
		return
	}
	// No project read gate precedes this call, deliberately: an unknown
	// project id must be refused as a permission outcome, not disclosed as
	// a missing project (see the package doc).
	res, err := h.cmd.AbortProposal(r.Context(),
		// IsAgent is false, and that is a statement about THIS BUILD rather
		// than about the product rule: V1 authenticates sessions, so no
		// request reaching this handler arrived as a platform agent. The
		// command's backstop is consulted on every call regardless (it is
		// the first thing AbortProposal does after the shape check), and it
		// is what an agent-actored non-HTTP caller meets.
		aborts.Actor{User: principal.User, IsAgent: false},
		aborts.Input{
			ProjectID:        projectID,
			ObjectID:         objectID,
			ObjectVersionRef: versionRefID(req.ObjectVersionRef),
			ReasonCode:       req.ReasonCode,
			Explanation:      req.Explanation,
			ReplacementRef:   req.ReplacementRef,
			IdempotencyKey:   key,
		})
	if err != nil {
		writeAbortError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, payloadFrom(res))
}

// pathObjectID splits the ":abort-proposal" suffix off the route's last
// segment. A segment without the suffix was routed here only because it
// matches the wildcard's prefix: that is a 404 — the URL names nothing. A
// segment with the suffix must still leave a non-empty object id, and the
// id must not have swallowed further path segments (the wildcard matches
// the rest of the path, slashes included).
func pathObjectID(w http.ResponseWriter, r *http.Request) (string, bool) {
	segment := r.PathValue("objectRef")
	if !strings.HasSuffix(segment, abortVerb) {
		http.NotFound(w, r)
		return "", false
	}
	id := strings.TrimSuffix(segment, abortVerb)
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return "", false
	}
	return id, true
}

// abortKey reads the Idempotency-Key. It is REQUIRED here, and the route
// refuses a request without one rather than performing it without the
// ability to repeat safely (docs/22 §3 lists the governed commands whose
// retry must be answerable; an abort is one).
func abortKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		authhttp.WriteError(w, r, http.StatusBadRequest, aborts.CodeValidationFailed,
			idempotencyHeader+" is required on an abort proposal: it is what makes a repeated request return the proposal it already created instead of proposing a second abort")
		return "", false
	}
	if len(key) < aborts.MinIdempotencyKeyLen {
		authhttp.WriteError(w, r, http.StatusBadRequest, aborts.CodeValidationFailed,
			idempotencyHeader+" must be at least "+strconv.Itoa(aborts.MinIdempotencyKeyLen)+" characters (specs/api/openapi.yaml)")
		return "", false
	}
	return key, true
}

// versionRefID strips the platform's `object_version:` prefix from a
// version ref, leaving a bare uuid (the same strip rsghttp and
// knowledgehttp perform for the same ref spelling).
func versionRefID(ref string) string {
	ref = strings.TrimSpace(ref)
	return strings.TrimPrefix(ref, "object_version:")
}

// decodeAbortBody parses the request body (bounded) and reports success — a
// malformed body is answered in place.
func decodeAbortBody(w http.ResponseWriter, r *http.Request, dst *abortRequest) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, aborts.CodeValidationFailed,
			"request body must be one JSON object naming object_version_ref, reason_code and explanation")
		return false
	}
	return true
}

// payloadFrom renders the command's result for the wire.
func payloadFrom(res aborts.Result) abortPayload {
	out := abortPayload{
		ProjectID:         res.ProjectID,
		ObjectID:          res.ObjectID,
		AbortedVersionID:  res.AbortedVersionID,
		AbortedVersionNo:  res.AbortedVersionNo,
		VersionID:         res.VersionID,
		VersionNo:         res.VersionNo,
		LifecycleState:    res.LifecycleState,
		BranchID:          res.BranchID,
		BranchName:        res.BranchName,
		PullRequestNumber: res.PullRequestNumber,
		PRState:           res.PRState,
		ReasonCode:        res.ReasonCode,
		Explanation:       res.Explanation,
		ReplacementRef:    res.ReplacementRef,
		DecidedBy:         res.DecidedBy,
		Replayed:          res.Replayed,
	}
	if !res.DecidedAt.IsZero() {
		out.DecidedAt = res.DecidedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// coded is the shape every abort refusal shares: a stable wire code.
type coded interface {
	Code() string
}

// wireStatus maps the abort package's wire codes (docs/45: one outcome, one
// stable code, whichever layer reports it) onto HTTP statuses. A code
// missing from the table is a 500: the transport must not invent a status
// for an outcome it does not know.
var wireStatus = map[string]int{
	aborts.CodeValidationFailed:   http.StatusBadRequest,
	aborts.CodeObjectNotFound:     http.StatusNotFound,
	aborts.CodeVersionNotFound:    http.StatusNotFound,
	aborts.CodeNotMainObject:      http.StatusConflict,
	aborts.CodeForbidden:          http.StatusForbidden,
	aborts.CodeAgentDenied:        http.StatusForbidden,
	aborts.CodeConflict:           http.StatusConflict,
	aborts.CodeServiceUnavailable: http.StatusServiceUnavailable,
}

// writeAbortError maps one command error onto the wire envelope.
//
// The authorization refusal answers AUTH_FORBIDDEN for everybody the matrix
// does not admit — a stranger, a viewer, a contributor, and a caller naming
// a project that does not exist. That last case is the point: the refusal is
// resolved before any target lookup, so it cannot disclose existence, and
// the transport must not undo that by answering 404 for an unknown id.
//
// The agent refusal answers its OWN code (OBJECT_ABORT_AGENT_DENIED), not
// AUTH_FORBIDDEN: the caller must be able to tell that authorization stopped
// it and which of the two defence lines did.
func writeAbortError(w http.ResponseWriter, r *http.Request, err error) {
	// The package's sentinel errors carry no code of their own (the service
	// raises them where the outcome is structural, not per-record), so the
	// transport names their wire code here. Each case RETURNS: the sentinels
	// below are also values of the coded interface, and falling through
	// would write a second envelope over the first.
	switch {
	case errors.Is(err, aborts.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, aborts.CodeValidationFailed, err.Error())
		return
	case errors.Is(err, aborts.ErrAgentNotPermitted):
		authhttp.WriteError(w, r, http.StatusForbidden, aborts.CodeAgentDenied,
			"an agent may not abort a main object")
		return
	case errors.Is(err, aborts.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, aborts.CodeForbidden,
			"you are not permitted to abort a main object in this project")
		return
	case errors.Is(err, aborts.ErrObjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, aborts.CodeObjectNotFound,
			"scientific object not found")
		return
	case errors.Is(err, aborts.ErrVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, aborts.CodeVersionNotFound,
			"object version not found")
		return
	case errors.Is(err, aborts.ErrNotMainObject):
		authhttp.WriteError(w, r, http.StatusConflict, aborts.CodeNotMainObject,
			"the object's current version is not on the project's main line")
		return
	case errors.Is(err, aborts.ErrConflict):
		authhttp.WriteError(w, r, http.StatusConflict, aborts.CodeConflict,
			"the object's version log moved; re-read and retry")
		return
	case errors.Is(err, aborts.ErrStore):
		// An unreachable store is reported by its code, never by its text:
		// the driver's message must not reach the wire.
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, aborts.CodeServiceUnavailable,
			"the abort proposal could not be completed; nothing was written")
		return
	}
	var c coded
	if errors.As(err, &c) {
		code := c.Code()
		status, ok := wireStatus[code]
		if !ok {
			observability.LoggerFromContext(r.Context()).Error("abort handler: unmapped wire code",
				"error", err, "code", code)
			authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
				"an internal error occurred")
			return
		}
		message := err.Error()
		if code == aborts.CodeServiceUnavailable {
			message = "the abort proposal could not be completed; nothing was written"
		}
		authhttp.WriteError(w, r, status, code, message)
		return
	}
	observability.LoggerFromContext(r.Context()).Error("abort handler: unexpected error", "error", err)
	authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
		"an internal error occurred")
}
