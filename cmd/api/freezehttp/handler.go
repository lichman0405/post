package freezehttp

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/mainfreeze"
	"github.com/lichman0405/post/internal/observability"
)

// idempotencyHeader is the header the contract requires on this route
// (components.parameters.IdempotencyKey: required, minLength 8).
const idempotencyHeader = "Idempotency-Key"

// minIdempotencyKeyLen is that parameter's minLength (the command checks
// the same bound; the transport refuses first so the answer is the
// contract's 400 rather than a validation line from deeper in).
const minIdempotencyKeyLen = mainfreeze.MinIdempotencyKeyLen

// freezeCommand is the command slice this transport needs. The interface
// exists so the handlers are unit-testable against a fake; the production
// value is *mainfreeze.Command.
type freezeCommand interface {
	Freeze(ctx context.Context, actor mainfreeze.Actor, in mainfreeze.Input) (mainfreeze.Result, error)
}

// handlers owns the freeze route.
type handlers struct {
	cmd freezeCommand
}

// freezePayload is the client-visible result of a freeze. It names the
// project and its state; it carries no actor or audit detail, which the
// project's Activity surface answers from the audit log itself.
type freezePayload struct {
	ProjectID string `json:"project_id"`
	// MainFrozen is the flag's value after the call: true, always — a call
	// that could not reach that state is an error, never a 200.
	MainFrozen bool `json:"main_frozen"`
	// AlreadyFrozen reports that main was already frozen, so this call
	// changed nothing and wrote no audit row and no event (docs/22 §3: the
	// repeated request returns the state the first call produced).
	AlreadyFrozen bool `json:"already_frozen"`
}

// freezeKey reads the contract-required Idempotency-Key, refusing a
// missing or too-short one in place (400). The route freezes main: a
// request that cannot be replayed onto the freeze it already produced is
// refused rather than performed without one.
func freezeKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.Header.Get(idempotencyHeader)
	if key == "" {
		authhttp.WriteError(w, r, http.StatusBadRequest, mainfreeze.CodeValidationFailed,
			idempotencyHeader+" is required on a freeze: it is what makes a repeated request return the state it already produced instead of freezing twice")
		return "", false
	}
	if len(key) < minIdempotencyKeyLen {
		authhttp.WriteError(w, r, http.StatusBadRequest, mainfreeze.CodeValidationFailed,
			idempotencyHeader+" must be at least "+strconv.Itoa(minIdempotencyKeyLen)+" characters (specs/api/openapi.yaml)")
		return "", false
	}
	return key, true
}

// handleFreeze: POST /api/v1/projects/{projectId}/main:freeze. The only
// write on this surface, and the only direction it has: it hands the
// command the actor and the key and maps the command's own outcome onto
// the wire. There is no unfreeze route, no body parameter that clears the
// flag, and no second endpoint that touches it (docs/09 §3: V1 provides no
// unfreeze, so that no bypass path exists).
func (h *handlers) handleFreeze(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	key, ok := freezeKey(w, r)
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
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, mainfreeze.CodeServiceUnavailable,
			"freeze service unavailable")
		return
	}
	// No project read gate precedes this call, deliberately: an unknown
	// project id must be refused as a permission outcome, not disclosed as
	// a missing project (see the package doc).
	res, err := h.cmd.Freeze(r.Context(), mainfreeze.Actor{User: principal.User}, mainfreeze.Input{
		ProjectID:      projectID,
		IdempotencyKey: key,
	})
	if err != nil {
		writeFreezeError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, freezePayload{
		ProjectID:     res.ProjectID,
		MainFrozen:    res.MainFrozen,
		AlreadyFrozen: res.AlreadyFrozen,
	})
}

// wireStatus maps the freeze package's own wire codes (docs/45: one
// outcome, one stable code, whichever layer reports it) onto HTTP
// statuses. A code missing from the table is a 500: the transport must not
// invent a status for an outcome it does not know.
var wireStatus = map[string]int{
	mainfreeze.CodeValidationFailed:   http.StatusBadRequest,
	mainfreeze.CodeForbidden:          http.StatusForbidden,
	mainfreeze.CodeAgentFreezeDenied:  http.StatusForbidden,
	mainfreeze.CodeProjectNotFound:    http.StatusNotFound,
	mainfreeze.CodePolicyRefused:      http.StatusForbidden,
	mainfreeze.CodeServiceUnavailable: http.StatusServiceUnavailable,
}

// coded is the shape every freeze refusal shares: a stable wire code.
type coded interface {
	Code() string
}

// writeFreezeError maps one command error onto the wire envelope.
//
// The authorization refusal answers AUTH_FORBIDDEN for everybody the
// matrix does not admit — a stranger, a viewer, a contributor, and a
// caller naming a project that does not exist. That last case is the
// point: the refusal is resolved before any target lookup, so it cannot
// disclose existence, and the transport must not undo that by answering
// 404 for an unknown id.
func writeFreezeError(w http.ResponseWriter, r *http.Request, err error) {
	// The package's sentinel errors carry no code of their own (the service
	// raises them where the outcome is structural, not per-record), so the
	// transport names their wire code here. Each case RETURNS: the sentinels
	// below are also values of the coded interface, and falling through
	// would write a second envelope over the first.
	switch {
	case errors.Is(err, mainfreeze.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, mainfreeze.CodeValidationFailed, err.Error())
		return
	case errors.Is(err, mainfreeze.ErrAgentNotPermitted):
		authhttp.WriteError(w, r, http.StatusForbidden, mainfreeze.CodeAgentFreezeDenied,
			"an agent may not freeze main")
		return
	case errors.Is(err, mainfreeze.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, mainfreeze.CodeForbidden,
			"you are not permitted to freeze main in this project")
		return
	case errors.Is(err, mainfreeze.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, mainfreeze.CodeProjectNotFound,
			"project not found")
		return
	case errors.Is(err, mainfreeze.ErrPolicyRefused):
		authhttp.WriteError(w, r, http.StatusForbidden, mainfreeze.CodePolicyRefused, err.Error())
		return
	case errors.Is(err, mainfreeze.ErrStore):
		// An unreachable store is reported by its code, never by its text:
		// the driver's message must not reach the wire.
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, mainfreeze.CodeServiceUnavailable,
			"the freeze could not be completed; main was not frozen")
		return
	}
	var c coded
	if errors.As(err, &c) {
		code := c.Code()
		status, ok := wireStatus[code]
		if !ok {
			observability.LoggerFromContext(r.Context()).Error("freeze handler: unmapped wire code",
				"error", err, "code", code)
			authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
				"an internal error occurred")
			return
		}
		message := err.Error()
		if code == mainfreeze.CodeServiceUnavailable {
			message = "the freeze could not be completed; main was not frozen"
		}
		authhttp.WriteError(w, r, status, code, message)
		return
	}
	observability.LoggerFromContext(r.Context()).Error("freeze handler: unexpected error", "error", err)
	authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
		"an internal error occurred")
}
