package projectshttp

import (
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
)

// The project shell surface (T0108): reads the client needs to render the
// project page — the project itself (handleGet) and the caller's own
// membership, which gates the Settings tab. The shell never decides
// permissions; it only renders what the service already authorized.

// handleMembership: GET /api/v1/projects/{projectId}/membership — the
// CALLER's own membership (the route carries no user id: an actor can
// never query someone else's role here; the member list arrives with
// T0109's management surface). A project the caller may read but is not a
// member of answers 404 PROJECT_MEMBERSHIP_NOT_FOUND — the shell renders
// "no role" instead of the Settings tab. A project the caller may not
// read answers the same 404 PROJECT_NOT_FOUND as handleGet (existence
// hiding: the membership of an invisible project leaks nothing).
//
// T0106 (in flight) gives reads an anonymous reader instead of 401-ing;
// when that merge lands, this handler adopts reader(r) the same way
// handleGet does — the service call inside is already read-policy
// shaped, only the session resolution here changes.
func (h *handlers) handleMembership(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	membership, err := h.svc.GetMembership(r.Context(), actor, r.PathValue("projectId"))
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, membershipPayloadFromDomain(membership))
}
