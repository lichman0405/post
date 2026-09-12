package profilehttp

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/profile"
)

// Deps carries the adapters (ports, docs/52) the profile API needs.
type Deps struct {
	Profiles profile.ProfileStore
}

// API is the mounted /api/v1/users profile subtree.
type API struct {
	handlers *handlers
}

// New wires the profile service.
func New(deps Deps) *API {
	return &API{handlers: &handlers{svc: profile.NewService(deps.Profiles)}}
}

// handlers owns the profile routes.
type handlers struct {
	svc *profile.Service
}

// Register mounts the profile routes on the guarded /api/v1 mux. The
// canonical profile URL is id-keyed (/users/{userID}/profile) so it never
// changes when the owner renames the handle (docs/21 §2: stable ids are
// the reference identity, slugs may vary); /users/by-handle/{handle}/profile
// is a convenience lookup that resolves the CURRENT handle to the same
// profile resource. The two patterns are segment-disjoint (3 vs 4 path
// segments), so ServeMux registration cannot conflict.
func (a *API) Register(mux *http.ServeMux) {
	h := a.handlers
	mux.HandleFunc("GET /api/v1/users/by-handle/{handle}/profile", h.handleGetByHandle)
	mux.HandleFunc("GET /api/v1/users/{userID}/profile", h.handleGetProfile)
	mux.HandleFunc("PATCH /api/v1/users/{userID}/profile", h.handleUpdateProfile)
}

// handleGetProfile: GET /api/v1/users/{userID}/profile — the public
// profile, readable without a session (acceptance "未登录可读公开
// profile"). The payload is the public shape for every caller: signed-in,
// anonymous, and the owner alike (the owner's private view is the auth
// session payload).
func (h *handlers) handleGetProfile(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.Get(r.Context(), r.PathValue("userID"))
	if err != nil {
		h.profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, publicProfileFromDomain(p))
}

// handleGetByHandle: GET /api/v1/users/by-handle/{handle}/profile —
// resolves the current handle to the public profile (same payload as the
// id-keyed route). Unknown handle: 404.
func (h *handlers) handleGetByHandle(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetByHandle(r.Context(), r.PathValue("handle"))
	if err != nil {
		h.profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, publicProfileFromDomain(p))
}

// profileUpdateRequest is the editable-field patch. All fields optional;
// unknown fields are ignored (the T0101 decode contract). Email is not a
// field: it is identity, not an editable profile field (profile.Update).
type profileUpdateRequest struct {
	Handle      *string `json:"handle"`
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
}

// handleUpdateProfile: PATCH /api/v1/users/{userID}/profile — the guard
// already required a valid session + CSRF token; the service enforces
// that the actor is the profile owner (403 AUTH_FORBIDDEN otherwise).
func (h *handlers) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	var req profileUpdateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(&req); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, profile.CodeValidation,
			"request body must be valid JSON")
		return
	}
	p, err := h.svc.Update(r.Context(),
		authhttp.PrincipalID(r.Context()), r.PathValue("userID"),
		profile.Update{Handle: req.Handle, DisplayName: req.DisplayName, Bio: req.Bio})
	if err != nil {
		h.profileError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, publicProfileFromDomain(p))
}

// profileError maps Service errors to the wire (docs/45: stable codes, no
// dependency detail). Unknown errors are answered with the generic
// SERVICE_UNAVAILABLE envelope.
func (h *handlers) profileError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, profile.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, profile.CodeUserNotFound,
			"no such user")
	case errors.Is(err, profile.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, profile.CodeForbidden,
			"only the profile owner may edit these fields")
	case errors.Is(err, profile.ErrValidation):
		// Validation messages are client-safe fixed strings by
		// construction (the T0101 authError contract).
		authhttp.WriteError(w, r, http.StatusBadRequest, profile.CodeValidation, err.Error())
	case errors.Is(err, profile.ErrHandleTaken):
		authhttp.WriteError(w, r, http.StatusConflict, profile.CodeHandleTaken,
			"this handle is already taken")
	case errors.Is(err, profile.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, profile.CodeUnavailable,
			"profiles are temporarily unavailable")
	default:
		authhttp.WriteError(w, r, http.StatusInternalServerError, profile.CodeUnavailable,
			"profiles are temporarily unavailable")
	}
}
