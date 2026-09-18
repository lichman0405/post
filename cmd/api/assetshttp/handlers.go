package assetshttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/observability"
)

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodePreviewValidationFailed: the request is not a publish candidate at
	// all — its body is not one JSON object.
	CodePreviewValidationFailed = "ASSET_PREVIEW_VALIDATION_FAILED"
	// CodePreviewUnavailable: the state read failed, so no honest preview can
	// be built. The preview is a statement about the state NOW, and a state
	// that could not be read has no preview — answering an empty or partial
	// one would report "nothing is private here" for a repository nobody
	// looked at.
	CodePreviewUnavailable = "ASSET_PREVIEW_UNAVAILABLE"
)

// handlers owns the preview route and the publish route (T0705), the asset
// hub's read routes (T0709: page.go) and the project-side dependency read
// (T0707: dependencies.go).
type handlers struct {
	state        StateReader
	projects     Gate
	publish      PublishCommand
	pages        PageReader
	members      Membership
	dependencies DependencyReader
}

// previewRequest is the request body: the proposed publish, in the shape
// the publish gate takes it (assets.PublishCandidate).
//
// The two documents arrive as raw JSON objects — manifest and rights are
// stored documents, and what a preview has to be able to show is exactly
// the bytes a publish would store (assets.PublishCandidate hands them over
// unparsed for the same reason). "rights" is the version schema's own field
// name (specs/schemas/research-asset-version.schema.json), not the column's
// (rights_json): the client is describing a version, not a row.
//
// NOTHING here is shaped or validated on the transport's side. Every field
// is mandatory for a publishable version, and the publish gate refuses a
// candidate that is missing any of them or has one out of shape — so a
// malformed body is not answered 400, it is answered 200 with the refusals
// listed. That is the point of a preview: "this cannot execute, here is
// why, and here is what it would have exposed anyway" is the answer a human
// deciding needs, and a preview that answered 400 would tell them nothing
// about the impact.
//
// Unknown fields are ignored, the way every other product route's decoder
// treats them. The usual cost of that — a misspelled field silently taking
// a default — cannot be paid here: there is no optional field to default
// (see above), so a misspelled name arrives as an absent name and the gate
// refuses the candidate for the field that is missing.
type previewRequest struct {
	AssetPID      string          `json:"asset_pid"`
	AssetType     string          `json:"asset_type"`
	Version       string          `json:"version"`
	Manifest      json.RawMessage `json:"manifest"`
	Rights        json.RawMessage `json:"rights"`
	OriginRefs    []string        `json:"origin_refs"`
	Visibility    string          `json:"visibility"`
	IntegrityHash string          `json:"integrity_hash"`
	CreatorIDs    []string        `json:"creator_ids"`
}

// candidate renders the body as the gate's input value.
func (req previewRequest) candidate() assets.PublishCandidate {
	return assets.PublishCandidate{
		AssetPID:      assets.PID(req.AssetPID),
		AssetType:     assets.Type(req.AssetType),
		Version:       req.Version,
		Manifest:      req.Manifest,
		RightsJSON:    req.Rights,
		OriginRefs:    req.OriginRefs,
		Visibility:    assets.Visibility(req.Visibility),
		IntegrityHash: req.IntegrityHash,
		CreatorIDs:    req.CreatorIDs,
	}
}

// principal resolves the authenticated actor; a missing session is answered
// in place with 401 (the v1 guard enforces the same before routing — this
// is the handler-level backstop, the same one releasehttp keeps).
//
// The preview requires an actor rather than serving anonymous callers,
// although the project read gate would allow an anonymous read of a public
// project: what a preview returns is the resolution of entities and the
// visibilities of projects that the response names — a disclosure made to a
// specific person who is about to decide whether to make a publication.
// The party to disclose that to is the authenticated caller, not the
// network.
func principal(w http.ResponseWriter, r *http.Request) bool {
	if _, ok := authhttp.PrincipalFrom(r.Context()); !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return false
	}
	return true
}

// reader resolves the caller for the project read gate: the same resolution
// every other project read runs.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// decodeBody parses the request body (bounded; unknown fields ignored) and
// reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, CodePreviewValidationFailed,
			"request body must be one JSON object describing the proposed publish")
		return false
	}
	return true
}

// writePreviewError answers a failure that is not a refusal of the
// candidate: an unreadable project, an unreadable state, a bug.
func writePreviewError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound), errors.Is(err, projects.ErrForbidden),
		errors.Is(err, projects.ErrStore):
		// The project gate's own vocabulary, answered the way every other
		// project read answers it (existence-hiding 404 for not-found and
		// forbidden alike).
		writeProjectGateError(w, r, err)
	case errors.Is(err, assets.ErrIncompleteState):
		// The reader dropped a ref it was required to answer for. That is a
		// defect in this service, not a finding about the publication: a
		// preview built over it would report another project's entity as
		// nonexistent. Answered 500 and logged, never rendered as a
		// dependency.
		observability.LoggerFromContext(r.Context()).Error("asset preview: state reader dropped a ref", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	default:
		observability.LoggerFromContext(r.Context()).Error("asset preview: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodePreviewUnavailable,
			"repository state is temporarily unavailable")
	}
}

// writeProjectGateError relays the project surface's own refusals, so a
// hidden project and a nonexistent one stay indistinguishable here exactly
// as they are there.
func writeProjectGateError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, projects.CodeProjectForbidden,
			"the caller may not read this project")
	case errors.Is(err, projects.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("asset preview: project gate store error", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodePreviewUnavailable,
			"project data is temporarily unavailable")
	default:
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	}
}

// handlePublishPreview: POST
// /api/v1/projects/{projectId}/assets:publish-preview.
//
// The order of the steps is the order of what may be disclosed: the caller
// is identified, then the project it is asking about is gated, and only then
// is the body read and resolved. A refusal at either of the first two steps
// answers without touching the candidate at all.
func (h *handlers) handlePublishPreview(w http.ResponseWriter, r *http.Request) {
	if !principal(w, r) {
		return
	}
	projectID := r.PathValue("projectId")
	if _, err := h.projects.Get(r.Context(), reader(r), projectID); err != nil {
		writePreviewError(w, r, err)
		return
	}
	var req previewRequest
	if !decodeBody(w, r, &req) {
		return
	}
	candidate := req.candidate()

	// The state is read first and whole: a preview is a statement about the
	// repository NOW, and there is no honest partial version of it.
	state, err := h.state.ResolvePreviewState(r.Context(), candidate)
	if err != nil {
		writePreviewError(w, r, err)
		return
	}
	preview, err := assets.Preview(assets.PreviewRequest{ProjectID: projectID, Candidate: candidate}, state)
	if err != nil {
		writePreviewError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, preview)
}
