// Package schemaprofileshttp owns the project schema profile routes
// (T0213): register, list and read the project's namespaced, versioned
// JSON Schema profiles that extend official base schemas with
// project-specific typed fields.
//
// Routes (all under the guarded /api/v1 subtree, so every write is
// session + CSRF protected by construction):
//
//	POST /api/v1/projects/{projectId}/schema-profiles
//	GET  /api/v1/projects/{projectId}/schema-profiles
//	GET  /api/v1/projects/{projectId}/schema-profiles/{profileName}
//	GET  /api/v1/projects/{projectId}/schema-profiles/{profileName}/versions/{version}
//
// Reads are exactly as visible as their project (the service runs the
// T0106 read gate); registration answers the maintainer-or-above gate with
// the settings surface's stable outcomes.
package schemaprofileshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/schemaprofiles"
	"github.com/lichman0405/post/internal/domain"
)

// Service is the slice of the schemaprofiles application service the
// routes need.
type Service interface {
	Register(ctx context.Context, actor domain.User, projectID string, in schemaprofiles.RegisterInput) (domain.ProjectSchemaProfile, error)
	Get(ctx context.Context, r projects.Reader, projectID, name string) (domain.ProjectSchemaProfile, error)
	GetVersion(ctx context.Context, r projects.Reader, projectID, name, version string) (domain.ProjectSchemaProfile, error)
	List(ctx context.Context, r projects.Reader, projectID string) ([]domain.ProjectSchemaProfile, error)
}

// handlers owns the schema profile routes.
type handlers struct {
	svc Service
}

// schemaRefPayload is the client-visible {id, version} pin.
type schemaRefPayload struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// profilePayload is the client-visible profile shape: the row facts, the
// two pins (the profile's own registry ref and the base it extends), the
// exact registered content and its integrity hash.
type profilePayload struct {
	ID          string           `json:"id"`
	ProjectID   string           `json:"project_id"`
	SchemaRef   schemaRefPayload `json:"schema_ref"`
	Base        schemaRefPayload `json:"base"`
	Content     json.RawMessage  `json:"content"`
	ContentHash string           `json:"content_hash"`
	CreatedBy   string           `json:"created_by"`
	CreatedAt   time.Time        `json:"created_at"`
}

// profilePayloadFromDomain renders one profile row.
func profilePayloadFromDomain(p domain.ProjectSchemaProfile) profilePayload {
	return profilePayload{
		ID:          p.ID,
		ProjectID:   p.ProjectID,
		SchemaRef:   schemaRefPayload{ID: p.SchemaID, Version: p.Version},
		Base:        schemaRefPayload{ID: p.BaseSchemaID, Version: p.BaseSchemaVersion},
		Content:     json.RawMessage(p.Content),
		ContentHash: p.ContentHash,
		CreatedBy:   p.CreatedBy,
		CreatedAt:   p.CreatedAt,
	}
}

// registerRequest is the wire body of a profile registration: the name,
// the version label, the official base pin, and the project's custom field
// definitions. The server derives the schema id and generates the full
// profile document.
type registerRequest struct {
	Name       string                     `json:"name"`
	Version    string                     `json:"version"`
	Base       schemaRefPayload           `json:"base"`
	Properties map[string]json.RawMessage `json:"properties"`
	Required   []string                   `json:"required"`
}

// handleRegister: POST /api/v1/projects/{projectId}/schema-profiles
func (h *handlers) handleRegister(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req registerRequest
	if !decodeBody(w, r, &req) {
		return
	}
	properties := make(map[string]any, len(req.Properties))
	for name, raw := range req.Properties {
		var def any
		if err := json.Unmarshal(raw, &def); err != nil {
			authhttp.WriteError(w, r, http.StatusBadRequest, schemaprofiles.CodeValidation,
				"properties must be valid JSON Schema fragments")
			return
		}
		properties[name] = def
	}
	profile, err := h.svc.Register(r.Context(), actor, r.PathValue("projectId"), schemaprofiles.RegisterInput{
		Name:       req.Name,
		Version:    req.Version,
		Base:       schemaprofiles.Ref{ID: req.Base.ID, Version: req.Base.Version},
		Properties: properties,
		Required:   req.Required,
	})
	if err != nil {
		profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, profilePayloadFromDomain(profile))
}

// handleList: GET /api/v1/projects/{projectId}/schema-profiles
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.svc.List(r.Context(), reader(r), r.PathValue("projectId"))
	if err != nil {
		profileError(w, r, err)
		return
	}
	out := make([]profilePayload, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, profilePayloadFromDomain(p))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

// handleGet: GET /api/v1/projects/{projectId}/schema-profiles/{profileName}
// — the profile's newest registered version.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	profile, err := h.svc.Get(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("profileName"))
	if err != nil {
		profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, profilePayloadFromDomain(profile))
}

// handleGetVersion: GET
// /api/v1/projects/{projectId}/schema-profiles/{profileName}/versions/{version}
// — one profile version by label, any age: old versions stay queryable
// forever, so a profile v2 never invalidates history written under v1
// (docs/21 §8).
func (h *handlers) handleGetVersion(w http.ResponseWriter, r *http.Request) {
	profile, err := h.svc.GetVersion(r.Context(), reader(r), r.PathValue("projectId"), r.PathValue("profileName"), r.PathValue("version"))
	if err != nil {
		profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, profilePayloadFromDomain(profile))
}

// decodeBody parses a JSON body (bounded) and reports success — a
// malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, schemaprofiles.CodeValidation,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor for writes; a missing session
// is answered in place with 401 (the guard enforces the same before
// routing — this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// reader resolves the caller for the visibility-aware reads (T0106): the
// same resolution every other project read runs.
func reader(r *http.Request) projects.Reader {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return projects.Reader{}
	}
	return projects.Reader{UserID: p.User.ID, Authenticated: true}
}

// profileError maps service outcomes to the wire (docs/45: stable codes,
// no dependency detail). Unknown errors are answered with the generic 503.
func profileError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, schemaprofiles.ErrProjectNotFound) || errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, schemaprofiles.ErrProfileNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, schemaprofiles.CodeProfileNotFound,
			"schema profile not found")
	case errors.Is(err, schemaprofiles.ErrProfileVersionExists):
		authhttp.WriteError(w, r, http.StatusConflict, schemaprofiles.CodeProfileVersionExists,
			"this schema profile version is already registered; schema versions are immutable — register the new content under a new version")
	case errors.Is(err, schemaprofiles.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, schemaprofiles.CodeForbidden,
			"you are not permitted to register schema profiles in this project")
	case errors.Is(err, schemaprofiles.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, schemaprofiles.CodeValidation, err.Error())
	default:
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, schemaprofiles.CodeUnavailable,
			"service unavailable")
	}
}
