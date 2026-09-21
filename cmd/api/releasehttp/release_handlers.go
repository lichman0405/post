package releasehttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/releases"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// CommandPort is the release command slice the transport needs. The
// production implementation is *releases.Command; the interface exists
// so the handlers are unit-testable against a fake.
type CommandPort interface {
	Create(ctx context.Context, actor domain.User, in releases.CreateReleaseParams) (domain.Release, error)
	List(ctx context.Context, projectID string) ([]domain.Release, error)
	Get(ctx context.Context, projectID, releaseID string) (domain.Release, error)
	Manifest(ctx context.Context, projectID, releaseID string) ([]byte, error)
}

// ProjectVisibilityPort is the read gate every release read runs: a
// release is exactly as visible as its project (the T0106 read matrix —
// the same resolution every other project read runs). The production
// adapter is *projects.Service (its Get).
type ProjectVisibilityPort interface {
	Get(ctx context.Context, r projects.Reader, projectID string) (domain.Project, error)
}

// handlers owns the release routes.
type handlers struct {
	cmd      CommandPort
	projects ProjectVisibilityPort
}

// releasePayload is the client-visible release shape. The manifest
// itself is not embedded — it is exported, byte-identical, by the
// /manifest endpoint.
type releasePayload struct {
	ID                 string    `json:"id"`
	ProjectID          string    `json:"project_id"`
	Version            string    `json:"version"`
	Title              string    `json:"title"`
	StateID            string    `json:"state_id"`
	PolicyVersionID    *string   `json:"policy_version_id"`
	OrgPolicyVersionID *string   `json:"org_policy_version_id"`
	ManifestHash       string    `json:"manifest_hash"`
	CreatedBy          string    `json:"created_by"`
	CreatedAt          time.Time `json:"created_at"`
}

func releasePayloadFromDomain(r domain.Release) releasePayload {
	return releasePayload{
		ID:                 r.ID,
		ProjectID:          r.ProjectID,
		Version:            r.Version,
		Title:              r.Title,
		StateID:            r.StateID,
		PolicyVersionID:    r.PolicyVersionID,
		OrgPolicyVersionID: r.OrgPolicyVersionID,
		ManifestHash:       r.ManifestHash,
		CreatedBy:          r.CreatedBy,
		CreatedAt:          r.CreatedAt,
	}
}

// createReleaseRequest is the POST body: the version string and the
// optional display title. There is no state selector — the command
// releases main's current accepted head, nothing else.
type createReleaseRequest struct {
	Version string `json:"version"`
	Title   string `json:"title"`
}

// principal resolves the authenticated actor for the create; a missing
// session is answered in place with 401 (the guard enforces the same
// before routing — this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// reader resolves the caller for the visibility-aware reads: the same
// resolution every other project read runs (anonymous stays anonymous —
// a public project's releases are public).
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// decodeBody parses the create body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, releases.CodeReleaseValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// writeReleaseError maps a service error onto the wire envelope
// (docs/45): one code per failure shape. Dependency failures answer a
// generic 503 naming nothing internal; the gate refusal carries the
// gate's own explanation (client-safe, derived from the check specs).
func writeReleaseError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, releases.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, releases.CodeReleaseValidationFailed, err.Error())
	case errors.Is(err, releases.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, releases.CodeReleaseForbidden,
			"the actor may not create releases here")
	case errors.Is(err, releases.ErrProjectNotFound), errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, releases.CodeReleaseProjectNotFound,
			"project not found")
	case errors.Is(err, releases.ErrNotMainState):
		authhttp.WriteError(w, r, http.StatusConflict, releases.CodeReleaseNoMainState,
			"the project's main branch has no accepted state to release")
	case errors.Is(err, releases.ErrVersionTaken):
		authhttp.WriteError(w, r, http.StatusConflict, releases.CodeReleaseVersionTaken, err.Error())
	case errors.Is(err, releases.ErrReleaseNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, releases.CodeReleaseNotFound,
			"release not found")
	case errors.Is(err, releases.ErrReleaseGate):
		message := "the release gate refused the snapshot"
		var refused *releases.GateRefused
		if errors.As(err, &refused) && refused.Report.Explanation != "" {
			message = refused.Report.Explanation
		}
		authhttp.WriteError(w, r, http.StatusConflict, releases.CodeReleaseGateBlocked, message)
	case errors.Is(err, releases.ErrStore), errors.Is(err, projects.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, releases.CodeReleaseServiceUnavailable,
			"release data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("release handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// visible resolves the project read gate for the release reads: the
// release is exactly as visible as the project carrying it.
func (h *handlers) visible(w http.ResponseWriter, r *http.Request, projectID string) bool {
	_, err := h.projects.Get(r.Context(), reader(r), projectID)
	if err != nil {
		writeReleaseError(w, r, err)
		return false
	}
	return true
}

// handleListReleases: GET /api/v1/projects/{projectId}/releases.
func (h *handlers) handleListReleases(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.visible(w, r, projectID) {
		return
	}
	list, err := h.cmd.List(r.Context(), projectID)
	if err != nil {
		writeReleaseError(w, r, err)
		return
	}
	out := make([]releasePayload, 0, len(list))
	for _, rel := range list {
		out = append(out, releasePayloadFromDomain(rel))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"releases": out})
}

// handleCreateRelease: POST /api/v1/projects/{projectId}/releases. The
// only write of the surface: the release row is immutable — the surface
// registers no update or delete route, so the mux answers 405 for them.
// Idempotency-Key (docs/22) replays the create it names.
func (h *handlers) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createReleaseRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var key *string
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		key = &v
	}
	release, err := h.cmd.Create(r.Context(), actor, releases.CreateReleaseParams{
		ProjectID:      r.PathValue("projectId"),
		Version:        req.Version,
		Title:          req.Title,
		IdempotencyKey: key,
	})
	if err != nil {
		writeReleaseError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, releasePayloadFromDomain(release))
}

// handleGetRelease: GET /api/v1/projects/{projectId}/releases/{releaseId}.
func (h *handlers) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.visible(w, r, projectID) {
		return
	}
	release, err := h.cmd.Get(r.Context(), projectID, r.PathValue("releaseId"))
	if err != nil {
		writeReleaseError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, releasePayloadFromDomain(release))
}

// handleGetReleaseManifest: GET
// /api/v1/projects/{projectId}/releases/{releaseId}/manifest — the
// export. The bytes are the stored snapshot's canonical document,
// verified against its manifest hash before it is served; they are
// identical forever, no matter how the project's current state evolves.
func (h *handlers) handleGetReleaseManifest(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("projectId")
	if !h.visible(w, r, projectID) {
		return
	}
	releaseID := r.PathValue("releaseId")
	release, err := h.cmd.Get(r.Context(), projectID, releaseID)
	if err != nil {
		writeReleaseError(w, r, err)
		return
	}
	doc, err := h.cmd.Manifest(r.Context(), projectID, releaseID)
	if err != nil {
		writeReleaseError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="release-`+release.Version+`.manifest.json"`)
	// The manifest is the one JSON this API hands over rather than renders:
	// it is a state document a caller files away, so it leaves as a
	// download. nosniff is stated beside the disposition — the pair is what
	// the outbound-byte guard (tests/security) judges.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}
