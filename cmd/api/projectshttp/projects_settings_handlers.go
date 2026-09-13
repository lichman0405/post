package projectshttp

import (
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
)

// The settings surface (T0109): the member list, member role changes and
// the project purpose/activity-status edit. Authorization is the
// service's (read path + maintainer gate); the handlers only translate
// wire shapes. Visibility stays preview-only: the update handler refuses
// any visibility field with a stable code instead of silently dropping
// it.

// memberPayload is the client-visible member-list row.
type memberPayload struct {
	UserID      string `json:"user_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joined_at"`
}

func memberPayloadFromDomain(m domain.ProjectMember) memberPayload {
	return memberPayload{
		UserID:      m.UserID,
		Handle:      m.Handle,
		DisplayName: m.DisplayName,
		Role:        string(m.Role),
		JoinedAt:    m.JoinedAt.Format("2006-01-02"),
	}
}

// handleListMembers: GET /api/v1/projects/{projectId}/members — the
// member list (identity + role + join date), oldest membership first.
// Owner/maintainer only; everyone else gets the same 403 (or the read
// path's existence-hiding 404).
func (h *handlers) handleListMembers(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	members, err := h.svc.ListMembers(r.Context(), actor, r.PathValue("projectId"))
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	out := make([]memberPayload, 0, len(members))
	for _, m := range members {
		out = append(out, memberPayloadFromDomain(m))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"members": out})
}

type setMemberRoleRequest struct {
	Role string `json:"role"`
}

// handleSetMemberRole: PUT /api/v1/projects/{projectId}/members/{userId}
// — change one membership's role. The service enforces the owner/
// maintainer rules (no self-change, owner management is owner-only, last
// owner protected); the write rides the guard's session + CSRF checks
// like every state change.
func (h *handlers) handleSetMemberRole(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req setMemberRoleRequest
	if !decodeBody(w, r, &req) {
		return
	}
	membership, err := h.svc.SetMemberRole(r.Context(), actor,
		r.PathValue("projectId"), r.PathValue("userId"), domain.ProjectRole(req.Role))
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, membershipPayloadFromDomain(membership))
}

type updateSettingsRequest struct {
	Purpose        *string `json:"purpose"`
	ActivityStatus *string `json:"activity_status"`
	Visibility     *string `json:"visibility"`
}

// handleUpdateSettings: PATCH /api/v1/projects/{projectId} — edit the
// purpose and/or the activity status. A visibility field is refused with
// VISIBILITY_CHANGE_NOT_SUPPORTED: the setting is preview-only until the
// publishing guard lands (T0109 requirement), and an explicit refusal
// beats a silent drop.
func (h *handlers) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req updateSettingsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	project, err := h.svc.UpdateSettings(r.Context(), actor, r.PathValue("projectId"),
		projects.UpdateSettingsInput{
			Purpose:        req.Purpose,
			ActivityStatus: req.ActivityStatus,
			Visibility:     req.Visibility,
		})
	if err != nil {
		h.projectError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, projectPayloadFromDomain(project))
}
