package attestationhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The publish pair (T0812):
//
//	POST /api/v1/projects/{projectId}/attestations:publish-preview
//	POST /api/v1/projects/{projectId}/attestations:publish
//
// # One request body for both routes
//
// The preview takes everything the publish takes, which is a difference from
// the knowledge pair rather than an oversight. There, the preview is a
// PROPOSAL (specs/mcp/tools.json) and the public_version is part of executing
// it, so the preview can be asked without it. Here, what a caller is
// previewing is precisely the difference between what the statement says and
// what that discloses — a preview that could not see org_visibility could not
// answer "would my organization be named?", which is the one question this
// preview exists for. So the same document goes to both routes, and a publish
// is executed against exactly the document the preview was computed for.

// attestationRequest is the request body of both routes.
type attestationRequest struct {
	// Target names the version the attestation is about, as
	// `object_version:<uuid>` (a Protocol or Claim version) or
	// `asset_version:<uuid>` (an Asset version). The prefix is required:
	// unlike a knowledge_version_ref, it is what says which of two tables
	// the uuid indexes, and a bare uuid is ambiguous
	// (attestations.ParseTargetRef).
	Target string `json:"target"`
	// ValidationType is what was validated: reproduction, method_validation,
	// data_audit or rights_review.
	ValidationType string `json:"validation_type"`
	// ValidationResult is how it went: confirmed, refuted or inconclusive.
	ValidationResult string `json:"validation_result"`
	// OrgVisibility is the attribution recorded for THIS statement:
	// anonymous or named. "named" is admitted only when the organization's
	// own standing setting permits it, and only when the project has an
	// organization at all.
	OrgVisibility string `json:"org_visibility"`
	// BasisStateID names the attesting project's OWN state the statement
	// rests on — the private evidence behind it. It is recorded on the row
	// and never published.
	BasisStateID string `json:"basis_state_id"`
	// InternalReviewID names the review of exactly that state, on a pull
	// request of the attesting project, that authorised the statement.
	InternalReviewID string `json:"internal_review_id"`
}

func (req attestationRequest) params(projectID string) attestations.PublishParams {
	return attestations.PublishParams{
		ProjectID:        projectID,
		TargetRef:        req.Target,
		ValidationType:   req.ValidationType,
		ValidationResult: req.ValidationResult,
		OrgVisibility:    req.OrgVisibility,
		BasisStateID:     req.BasisStateID,
		InternalReviewID: req.InternalReviewID,
	}
}

// previewPayload is the 200 body of the preview: the command's Preview,
// rendered as-is.
//
// A field dropped here would be a second implementation of the disclosure
// rule in the layer with no test for it — and Preview is the command's own
// type, so its JSON shape is the command's decision (the publication and
// asset previews make the same choice).
type previewPayload = attestations.Preview

// attestedPayload is the 201 body: the attestation the publish stored, as the
// identities that name it.
//
// The row's internal uuid is NOT on the wire, and neither is the attesting
// project: the pid is the identity a client addresses and cites, and the
// project is the private side. org_visibility is the RECORDED attribution —
// the promise — and not the answer to "is the organization named now": that
// is a read-time decision, and it can be narrowed by the organization at any
// moment (attestations.Present). A client that wants the current answer reads
// the attestation back by pid.
type attestedPayload struct {
	PID              string `json:"pid"`
	ValidationType   string `json:"validation_type"`
	ValidationResult string `json:"validation_result"`
	OrgVisibility    string `json:"org_visibility"`
	CreatedAt        string `json:"created_at"`
}

func attestedFromDomain(a attestations.Attested) attestedPayload {
	return attestedPayload{
		PID:              a.PID,
		ValidationType:   a.ValidationType,
		ValidationResult: a.ValidationResult,
		OrgVisibility:    a.OrgVisibility,
		CreatedAt:        attestations.FormatInstant(a.CreatedAt),
	}
}

// handlePreview: POST
// /api/v1/projects/{projectId}/attestations:publish-preview.
//
// The order is shape, principal, command — and NOT the project read gate the
// knowledge preview runs. The difference is deliberate and it is about what
// the preview reads: that one answers a question about the project's own
// state (what the repository holds and who would see it) and is therefore
// gated exactly as a project read is; this one answers a question about a
// GOVERNANCE ACT the caller is about to take, and the command authorizes the
// actor against the permission matrix before it resolves anything. A caller
// who may not attest cannot take this preview either, which is the same
// answer the publish gives and therefore discloses nothing new — while a
// preview gated on the project READ alone would let any member of a public
// project enumerate its internal reviews and states.
func (h *handlers) handlePreview(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	var req attestationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	preview, err := h.attest.Preview(r.Context(),
		attestations.Actor{User: user, IsAgent: false},
		req.params(r.PathValue("projectId")))
	if err != nil {
		writeAttestError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, previewPayload(preview))
}

// handlePublish: POST /api/v1/projects/{projectId}/attestations:publish.
//
// IsAgent is false, and that is a statement about THIS BUILD rather than about
// the product rule: V1 authenticates sessions, so no request reaching this
// handler arrived as a platform agent. The command's backstop is consulted on
// every call regardless (it is the first thing Publish does after the shape
// check), so the field being false here does not make it dead code — agents
// reach the command through the MCP surface, which builds its own Actor.
func (h *handlers) handlePublish(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	var req attestationRequest
	if !decodeBody(w, r, &req) {
		return
	}
	stored, err := h.attest.Publish(r.Context(),
		attestations.Actor{User: user, IsAgent: false},
		req.params(r.PathValue("projectId")))
	if err != nil {
		writeAttestError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, attestedFromDomain(stored))
}

// principalUser resolves the authenticated actor; a missing session is
// answered in place with 401 (the v1 guard enforces the same before routing —
// this is the handler-level backstop the other publish surfaces keep).
func principalUser(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// decodeBody parses the request body (bounded; unknown fields ignored) and
// reports success — a malformed body is answered in place with the command's
// own validation code, so a client branches on one code for "this body is not
// an attestation candidate".
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, attestations.CodeValidationFailed,
			"request body must be one JSON object describing the proposed attestation")
		return false
	}
	return true
}

// blockedEnvelope is the refusal body of a publish the server-side re-run
// refused: the shared error envelope (docs/22 §5, docs/45) with the COMPLETE
// preview attached.
//
// The extra field is the point of the response. A client told only "refused"
// cannot see which entry blocked it — nor check that the refusal was about the
// entry it thinks it was. The refusal therefore carries the same document
// POST .../attestations:publish-preview answers with, computed over the state
// the publish itself saw, so a caller can compare what it previewed against
// what the publish refused over.
//
// The envelope is spelled out rather than reused from authhttp because
// authhttp's is unexported and has no extension point; the four shared fields
// are copied verbatim so the two shapes stay one shape for a client that
// branches on `code` first.
//
// # What may appear in Preview, and what may not
//
// Everything in it describes either the ATTESTING project (which the caller
// is authorized to act for — the command resolves the membership before it
// reads anything) or a target the caller could already read. The target half
// is not a policy this layer applies: the store resolves the target through
// the reader (ResolveRequest.ReaderID), so a version the caller may not read
// never becomes facts, never reaches BuildPreview, and answers the
// ErrTargetNotFound 404 instead of this refusal. An unreadable target is
// therefore not "a preview that is filtered" — it is the 404, by the same
// code path a random uuid takes.
type blockedEnvelope struct {
	Code      string               `json:"code"`
	Message   string               `json:"message"`
	RequestID string               `json:"request_id"`
	Retryable bool                 `json:"retryable"`
	Preview   attestations.Preview `json:"preview"`
	Reasons   []string             `json:"reasons,omitempty"`
}

// writeAttestBlocked renders the refusal with its report.
func writeAttestBlocked(w http.ResponseWriter, r *http.Request, refused *attestations.Refused) {
	message := "the attestation was refused by the server-side re-check"
	if len(refused.Reasons) > 0 {
		message = "the attestation was refused: " + strings.Join(refused.Reasons, "; ")
	}
	w.Header().Set("Content-Type", "application/json")
	// nosniff is stated here rather than inherited from the edge: this exit
	// carries a server-authored JSON body, and an exit that only looks safe
	// because a layer above adds the header is unsafe everywhere else it is
	// mounted (tests/security/exits_test.go:TestEveryRegisteredExitStatesItsOwnHeaders).
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(blockedEnvelope{
		Code:      attestations.CodeAttestationRefused,
		Message:   message,
		RequestID: requestID(r),
		Retryable: false,
		Preview:   refused.Preview,
		Reasons:   refused.Reasons,
	})
}

// requestID is the observability request id of the shared envelope
// (authhttp's own helper is unexported; this reads the same value).
func requestID(r *http.Request) string {
	id, ok := observability.FromContext(r.Context())
	if !ok {
		return ""
	}
	return id.String()
}

// writeAttestError maps the command's outcomes onto the wire (docs/45: one
// stable code per outcome, no dependency detail).
//
// The statuses follow the vocabulary the rest of the product already fixes:
// 400 for a body that is not a candidate, 403 for every decision that says
// "not you", 404 for a record this caller may not learn about, and 409 for
// every decision that says "not in this state". Those are different answers
// to a client — an unauthorized caller is not being asked to retry — so they
// are not collapsed.
func writeAttestError(w http.ResponseWriter, r *http.Request, err error) {
	// Matched first, by TYPE: the refusals that carry a report are the only
	// outcomes whose response body is more than the envelope.
	var refused *attestations.Refused
	if errors.As(err, &refused) {
		writeAttestBlocked(w, r, refused)
		return
	}
	var agentDenied *attestations.AgentNotPermittedError
	switch {
	case errors.As(err, &agentDenied):
		// The domain backstop, answered with a code of its own: a governance
		// change may edit a matrix cell, and this refusal is the one that
		// survives the edit (docs/23 §4). A client that cannot tell the two
		// apart cannot tell which line of defence refused it.
		authhttp.WriteError(w, r, http.StatusForbidden, attestations.CodeAgentAttestDenied,
			"a platform agent may not issue an attestation — attesting is a human governance action")
	case errors.Is(err, attestations.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, attestations.CodeValidationFailed, err.Error())
	case errors.Is(err, attestations.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, attestations.CodeForbidden,
			"the actor may not issue attestations in this project")
	case errors.Is(err, attestations.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, attestations.CodeProjectNotFound, "project not found")
	case errors.Is(err, attestations.ErrTargetNotFound):
		// One code for "no such version" and "a version this caller may not
		// read", and the two ARE one answer rather than two answers this
		// layer declines to distinguish: the store's target read is
		// reader-relative, so an unreadable version resolves to no row
		// exactly as a nonexistent one does. A caller cannot ask "does this
		// version exist" here, and it cannot learn it from the 409 either —
		// that body is only ever computed from a target the caller can
		// already read (docs/45).
		authhttp.WriteError(w, r, http.StatusNotFound, attestations.CodeTargetNotFound,
			"the target version was not found")
	case errors.Is(err, attestations.ErrBasisNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, attestations.CodeBasisStateNotFound,
			"the basis state was not found in this project")
	case errors.Is(err, attestations.ErrReviewNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, attestations.CodeInternalReviewNeeded,
			"the internal review was not found in this project")
	case errors.Is(err, attestations.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("attestation: store failure", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, attestations.CodeServiceUnavailable,
			"attestation data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("attestation: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
