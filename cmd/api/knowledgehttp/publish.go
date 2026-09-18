package knowledgehttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/knowledgepublish"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The publish surface (T0805): POST .../knowledge:publish-preview and POST
// .../knowledge:publish — the pair docs/23 §4 describes, on the knowledge
// side of the platform.
//
// # The request bodies
//
// publishRequest embeds previewRequest for the same reason the asset pair
// does: a publish is executed against exactly the document the preview was
// computed for. The preview takes what specs/mcp/tools.json gives
// knowledge.publish_preview — knowledge_version_ref and rights — and nothing
// else; the publish adds public_version, the name the publication is shown
// under, which the tool deliberately does not carry: a proposal names WHAT
// would be published, and the name is part of executing it.
//
// knowledge_version_ref is the platform's ref spelling for a version
// (object_version:<uuid>, the same kind assets.ParseOriginRef writes into
// origin_refs and specs/mcp/tools.json's object.abort_proposal takes). A bare
// uuid is accepted too, because the ref spelling's prefix is a convenience for
// a reader rather than a second identity — and the strip is done here, at the
// transport, so the command takes one thing.
type previewRequest struct {
	// KnowledgeVersionRef names the scientific object version to publish:
	// object_version:<uuid>, or the uuid alone.
	KnowledgeVersionRef string `json:"knowledge_version_ref"`
	// Rights is the rights declaration the publication stores
	// (specs/policies/rights-template.yaml), as raw JSON: what a preview has
	// to be able to show is exactly the document a publish would store, and
	// the command parses it.
	Rights json.RawMessage `json:"rights"`
}

// publishRequest is the preview document plus the one field publishing adds.
type publishRequest struct {
	previewRequest
	// PublicVersion is the name the publication is shown under. It is the
	// PUBLISHER's string and is stored byte for byte (owner ruling
	// L3-20260916-1 #2): the command refuses a missing or blank one and
	// normalises nothing.
	PublicVersion string `json:"public_version"`
}

// versionID strips the ref prefix if the caller sent one. It does not
// validate: the command refuses a reference that is not a version uuid, and
// doing the shape check in two places is how the two come to disagree about
// what a legal reference is.
func versionID(ref string) string {
	ref = strings.TrimSpace(ref)
	return strings.TrimPrefix(ref, "object_version:")
}

// previewPayload is the 200 body of the preview: the command's Preview, which
// already carries every field a caller decides on (the audience the
// publication would have, the review dimensions it passed and needs, the
// existing publication when there is one, and every entry that blocks the
// publish).
//
// The transport renders it as-is. A field dropped here would be a second
// implementation of the disclosure rule in the layer with no test for it —
// and Preview is the command's own type, so its JSON shape is the command's
// decision (the asset preview makes the same choice with
// assets.ImpactPreview).
type previewPayload = knowledgepublish.Preview

// publishedPayload is the 201 body: the publication the publish stored, as
// the identities that name it.
//
// The row's internal uuid is NOT on the wire, and that is the same decision
// the asset publish makes: a publication is named by its pid and, for a
// human, by the public_version the publisher chose. Publishing an internal
// key would make the pid's stability optional in practice.
//
// rights is the STORED document, echoed back as it was written — a client
// checking that what it published is what the repository holds needs the
// stored bytes, not its own request echoed.
type publishedPayload struct {
	PID             string          `json:"pid"`
	ObjectVersionID string          `json:"object_version_id"`
	PublicVersion   string          `json:"public_version"`
	PublishedBy     string          `json:"published_by"`
	PublishedAt     string          `json:"published_at"`
	Rights          json.RawMessage `json:"rights"`
}

// publishedFromDomain renders the stored publication as the response body.
func publishedFromDomain(p knowledgepublish.Published) publishedPayload {
	return publishedPayload{
		PID:             p.PID,
		ObjectVersionID: p.ObjectVersionID,
		PublicVersion:   p.PublicVersion,
		PublishedBy:     p.PublishedBy,
		PublishedAt:     knowledgepublish.FormatInstant(p.PublishedAt),
		Rights:          p.RightsJSON,
	}
}

// handlePublishPreview: POST
// /api/v1/projects/{projectId}/knowledge:publish-preview.
//
// The order is the asset preview's order, for the same reason: the principal
// is resolved, the project read gate runs, the body is read, then the command
// answers.
//
// # Why the gate runs here and not on the publish
//
// The two routes take opposite decisions about who is refused first, and both
// are deliberate.
//
// The preview is a READ of the project's own state — it says what the
// repository currently holds and who would see it — so it is gated the way
// every project read is (the project read gate, T0106: a non-member of a
// public project is admitted, a non-member of a private one is answered the
// existence-hiding 404). Its answer is not a permission decision, so there is
// no reason to keep it beyond the project's own read boundary.
//
// The publish is a WRITE, and it is NOT gated here: the command authorizes the
// actor against the permission matrix before it reads anything, which is the
// only question that decides whether the write may happen, and answering it
// first is what keeps a denial from disclosing which projects exist (the
// releases rule: the denial is resolved before any target lookup, so it never
// discloses whether the project does). The asset publish makes the same
// choice for the same reason.
//
// # Agents
//
// The command's agent backstop is NOT consulted for a preview: specs/mcp/
// tools.json lists knowledge.publish_preview as a `proposal` while
// publish_private_to_public is in forbidden_default_agent_actions — an agent
// may ask what a publication would mean and may not perform it. Refusing the
// proposal to an agent would make the one thing the tool exists for
// impossible.
func (h *handlers) handlePublishPreview(w http.ResponseWriter, r *http.Request) {
	if _, ok := principalUser(w, r); !ok {
		return
	}
	projectID := r.PathValue("projectId")
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		writeGateError(w, r, err)
		return
	}
	var req previewRequest
	if !decodeBody(w, r, &req, "request body must be one JSON object describing the proposed publication") {
		return
	}
	preview, err := h.publish.Preview(r.Context(), knowledgepublish.PreviewParams{
		ProjectID:       projectID,
		ObjectVersionID: versionID(req.KnowledgeVersionRef),
		Rights:          req.Rights,
	})
	if err != nil {
		writePublishError(w, r, err)
		return
	}
	// Existing is a publication of its OWN — with an audience of its own,
	// which is not this project's preset and not the audience of the
	// publication the preview is about — and the project read gate above
	// admits a NON-MEMBER of a public project. So the identity of an existing
	// publication is handed over only to a caller who could read that
	// publication through the public read route: mayReadPID resolves it the
	// way that route does and applies the same decision.
	//
	// A caller who may not read it is told NOTHING about it here — the field
	// is dropped rather than blanked — while the rest of the preview stays:
	// what the version is, whether it passed review, and every entry that
	// blocks the publish are facts about a version in a project this caller
	// was admitted to read. The publish route's refusal report (the 409 that
	// also carries a Preview) needs no such gate: it is answered before any
	// read by an authorization that only a project member passes.
	if preview.Existing != nil {
		readable, err := h.mayReadPID(r, preview.Existing.PID)
		if err != nil {
			observability.LoggerFromContext(r.Context()).Error("knowledge preview: existing publication read failed",
				"error", err, "pid", preview.Existing.PID)
			writeKnowledgeUnavailable(w, r)
			return
		}
		if !readable {
			preview.Existing = nil
		}
	}
	authhttp.WriteJSON(w, http.StatusOK, previewPayload(preview))
}

// handlePublish: POST /api/v1/projects/{projectId}/knowledge:publish.
//
// IsAgent is false, and that is a statement about THIS BUILD rather than about
// the product rule: V1 authenticates sessions, so no request reaching this
// handler arrived as a platform agent. The command's backstop is consulted on
// every call regardless (it is the first thing Publish does), so the field
// being false here does not make it dead code: internal/application's non-HTTP
// callers do carry an agent flag, and the matrix denies agents independently
// (publish_private_to_public, agent column: deny).
func (h *handlers) handlePublish(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	var req publishRequest
	if !decodeBody(w, r, &req, "request body must be one JSON object describing the version to publish") {
		return
	}
	var key *string
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		key = &v
	}
	stored, err := h.publish.Publish(r.Context(),
		knowledgepublish.Actor{User: user, IsAgent: false},
		knowledgepublish.PublishParams{
			ProjectID:       r.PathValue("projectId"),
			ObjectVersionID: versionID(req.KnowledgeVersionRef),
			PublicVersion:   req.PublicVersion,
			Rights:          req.Rights,
			IdempotencyKey:  key,
		})
	if err != nil {
		writePublishError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, publishedFromDomain(stored))
}

// principalUser resolves the authenticated actor; a missing session is
// answered in place with 401 (the v1 guard enforces the same before routing —
// this is the handler-level backstop, the same one the asset and release
// surfaces keep).
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
// a publication candidate".
func decodeBody(w http.ResponseWriter, r *http.Request, dst any, message string) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, knowledgepublish.CodeValidationFailed, message)
		return false
	}
	return true
}

// reader renders the project read identity from the request (anonymous
// callers get the zero value, which the project read gate treats as
// unauthenticated — it is the same helper shape the asset surface uses).
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// blockedEnvelope is the refusal body of a publish the server-side re-run
// refused: the shared error envelope (docs/22 §5, docs/45) with the COMPLETE
// preview attached.
//
// The extra field is the point of the response. A client told only "refused"
// cannot see which entry blocked it — nor check that the refusal was about the
// entry it thinks it was. The refusal therefore carries the same document POST
// .../knowledge:publish-preview answers with, computed over the state the
// publish itself saw, so a caller can compare what it previewed against what
// the publish refused over.
//
// The envelope is spelled out rather than reused from authhttp because
// authhttp's is unexported and has no extension point; the four shared fields
// are copied verbatim so the two shapes stay one shape for a client that
// branches on `code` first.
type blockedEnvelope struct {
	Code      string                   `json:"code"`
	Message   string                   `json:"message"`
	RequestID string                   `json:"request_id"`
	Retryable bool                     `json:"retryable"`
	Preview   knowledgepublish.Preview `json:"preview"`
	Reasons   []string                 `json:"reasons,omitempty"`
}

// writePublishBlocked renders the refusal with its report.
func writePublishBlocked(w http.ResponseWriter, r *http.Request, refused *knowledgepublish.PublicationRefused) {
	message := "the publication was refused by the server-side re-check"
	if len(refused.Reasons) > 0 {
		message = "the publication was refused: " + strings.Join(refused.Reasons, "; ")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(blockedEnvelope{
		Code:      knowledgepublish.CodePublishBlocked,
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

// writePublishError maps the command's outcomes onto the wire (docs/45: one
// stable code per outcome, no dependency detail).
//
// The statuses follow the vocabulary the rest of the product already fixes:
// 400 for a body that is not a candidate, 403 for every decision that says
// "not you", and 409 for every decision that says "not in this state". The
// last two are different answers to a client — an unauthorized caller is not
// being asked to retry — so they are not collapsed.
func writePublishError(w http.ResponseWriter, r *http.Request, err error) {
	// Matched first, by TYPE: the refusals that carry a report are the only
	// outcomes whose response body is more than the envelope.
	var refused *knowledgepublish.PublicationRefused
	if errors.As(err, &refused) {
		writePublishBlocked(w, r, refused)
		return
	}
	var agentDenied *knowledgepublish.AgentNotPermittedError
	switch {
	case errors.As(err, &agentDenied):
		// The domain backstop, answered with a code of its own: a governance
		// change may edit a matrix cell, and this refusal is the one that
		// survives the edit (docs/23 §4). A client that cannot tell the two
		// apart cannot tell which line of defence refused it.
		authhttp.WriteError(w, r, http.StatusForbidden, knowledgepublish.CodeAgentPublishDenied,
			"a platform agent may not publish a knowledge object version")
	case errors.Is(err, knowledgepublish.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, knowledgepublish.CodeValidationFailed, err.Error())
	case errors.Is(err, knowledgepublish.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, knowledgepublish.CodePublishPrivateToPublicRequiresApproval,
			"the actor may not publish knowledge object versions in this project")
	case errors.Is(err, knowledgepublish.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, knowledgepublish.CodeProjectNotFound, "project not found")
	case errors.Is(err, knowledgepublish.ErrVersionNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, knowledgepublish.CodeVersionNotFound,
			"the knowledge object version was not found in this project")
	case errors.Is(err, knowledgepublish.ErrAlreadyPublished):
		authhttp.WriteError(w, r, http.StatusConflict, knowledgepublish.CodeAlreadyPublished,
			"this knowledge object version is already published, and a version is published once")
	case errors.Is(err, knowledgepublish.ErrReviewRequired):
		authhttp.WriteError(w, r, http.StatusConflict, knowledgepublish.CodeReviewRequired,
			"this knowledge object version has not passed publication review")
	case errors.Is(err, knowledgepublish.ErrIdempotencyConflict):
		authhttp.WriteError(w, r, http.StatusConflict, knowledgepublish.CodeIdempotencyConflict,
			"this Idempotency-Key was used for a different publication")
	case errors.Is(err, knowledgepublish.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("knowledge publish: store failure", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, knowledgepublish.CodeServiceUnavailable,
			"publication data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("knowledge publish: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// writeGateError maps a project read gate refusal (the preview route's own
// gate). A gate refusal is the existence-hiding 404 the project surface
// answers, whatever it was: from outside, "this project is not yours to read"
// and "there is no such project" must be indistinguishable (docs/45).
func writeGateError(w http.ResponseWriter, r *http.Request, err error) {
	observability.LoggerFromContext(r.Context()).Debug("knowledge preview: project gate refused", "error", err)
	authhttp.WriteError(w, r, http.StatusNotFound, knowledgepublish.CodeProjectNotFound, "project not found")
}
