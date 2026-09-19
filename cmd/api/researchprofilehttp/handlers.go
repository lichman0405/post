package researchprofilehttp

import (
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/researchprofile"
)

// handlers owns the two research-profile routes.
type handlers struct {
	svc *researchprofile.Service
}

// handleGetPersonProfile: GET /api/v1/users/{userID}/research-profile — one
// person's Research Profile (docs/42's content list), readable without a
// session.
//
// The body is the model's own JSON (researchprofile.PersonProfile). There is
// no payload-builder function between the model and the wire on purpose: a
// second mapping is a second place for a field to appear, and the no-score
// rule (docs/13 §4, CLAUDE.md §9 invariant 13) is asserted on the bytes this
// handler writes (tests/e2e's "research profile e2e"). A builder would sit
// between that assertion and the model.
func (h *handlers) handleGetPersonProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := h.svc.PersonProfile(r.Context(), r.PathValue("userID"))
	if err != nil {
		h.profileError(w, r, err, researchprofile.CodeUserNotFound, "no such user")
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, profile)
}

// handleGetOrganizationProfile: GET /api/v1/organizations/{slug}/profile —
// one organization's public research identity, readable without a session.
func (h *handlers) handleGetOrganizationProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := h.svc.OrganizationProfile(r.Context(), r.PathValue("slug"))
	if err != nil {
		h.profileError(w, r, err, researchprofile.CodeOrgNotFound, "no such organization")
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, profile)
}

// profileError maps Service errors to the wire (docs/45: stable codes, no
// dependency detail).
//
// notFoundCode is the caller's — USER_NOT_FOUND on the person route,
// ORG_NOT_FOUND on the organization one — so the message names what the
// caller asked about while the BODY stays the same shape for every
// not-public outcome. That is the point: an unknown id, a disabled account,
// an unknown slug and a deactivated organization all leave here as one 404
// with no detail, so a caller cannot tell "does not exist" from "exists and
// is not public" (the same existence hiding the project read applies to a
// private project).
func (h *handlers) profileError(w http.ResponseWriter, r *http.Request, err error, notFoundCode, notFoundMessage string) {
	switch {
	case errors.Is(err, researchprofile.ErrNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, notFoundCode, notFoundMessage)
	case errors.Is(err, researchprofile.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, researchprofile.CodeUnavailable,
			"research profiles are temporarily unavailable")
	default:
		authhttp.WriteError(w, r, http.StatusInternalServerError, researchprofile.CodeUnavailable,
			"research profiles are temporarily unavailable")
	}
}
