package orgshttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/orgs"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The org HTTP surface. Every handler: resolve the principal (the guard
// put it there — reads require a session too, org visibility is
// member-only), parse the request, call the Service, render the payload
// or the standard error envelope.

// handlers owns the org routes.
type handlers struct {
	svc *orgs.Service
}

// orgPayload is the client-visible organization shape.
type orgPayload struct {
	ID            string     `json:"id"`
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	CreatedAt     time.Time  `json:"created_at"`
	DeactivatedAt *time.Time `json:"deactivated_at"`
}

func orgPayloadFromDomain(o domain.Organization) orgPayload {
	return orgPayload{
		ID:            o.ID,
		Slug:          o.Slug,
		Name:          o.Name,
		Description:   o.Description,
		CreatedAt:     o.CreatedAt,
		DeactivatedAt: o.DeactivatedAt,
	}
}

// membershipPayload is the client-visible membership shape. Affiliation
// dates are calendar dates ("YYYY-MM-DD") — the canonical columns are
// date, not timestamptz.
type membershipPayload struct {
	OrganizationID   string  `json:"organization_id"`
	UserID           string  `json:"user_id"`
	Role             string  `json:"role"`
	AffiliationStart string  `json:"affiliation_start"`
	AffiliationEnd   *string `json:"affiliation_end"`
	Verified         bool    `json:"verified"`
}

func membershipPayloadFromDomain(m domain.OrganizationMembership) membershipPayload {
	var end *string
	if m.AffiliationEnd != nil {
		s := m.AffiliationEnd.Format("2006-01-02")
		end = &s
	}
	return membershipPayload{
		OrganizationID:   m.OrganizationID,
		UserID:           m.UserID,
		Role:             string(m.Role),
		AffiliationStart: m.AffiliationStart.Format("2006-01-02"),
		AffiliationEnd:   end,
		Verified:         m.Verified,
	}
}

// decodeBody parses a JSON body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, orgs.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// principal resolves the authenticated actor; reads require a session
// too (org visibility is member-only), so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// parseDate reads an optional "YYYY-MM-DD" string.
func parseDate(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

type createOrgRequest struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleCreate: POST /api/v1/organizations — the actor becomes owner.
func (h *handlers) handleCreate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req createOrgRequest
	if !decodeBody(w, r, &req) {
		return
	}
	org, membership, err := h.svc.Create(r.Context(), actor, req.Slug, req.Name, req.Description)
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, map[string]any{
		"organization": orgPayloadFromDomain(org),
		"membership":   membershipPayloadFromDomain(membership),
	})
}

// handleList: GET /api/v1/organizations — the actor's organizations.
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), actor)
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	out := make([]orgPayload, 0, len(list))
	for _, o := range list {
		out = append(out, orgPayloadFromDomain(o))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"organizations": out})
}

// handleGet: GET /api/v1/organizations/{orgId} — member-only read.
func (h *handlers) handleGet(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	org, err := h.svc.Get(r.Context(), actor, r.PathValue("orgId"))
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, orgPayloadFromDomain(org))
}

type updateOrgRequest struct {
	// Pointer fields: PATCH partial-update semantics — an absent field
	// means "unchanged" (a name-only PATCH must not wipe the
	// description), an explicit empty description clears it.
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// handleUpdate: PATCH /api/v1/organizations/{orgId} — rename/describe,
// owner only. The slug is not mutable (stable public identity).
func (h *handlers) handleUpdate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req updateOrgRequest
	if !decodeBody(w, r, &req) {
		return
	}
	org, err := h.svc.Update(r.Context(), actor, r.PathValue("orgId"), req.Name, req.Description)
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, orgPayloadFromDomain(org))
}

// handleDeactivate: DELETE /api/v1/organizations/{orgId} — soft delete
// (nothing disappears), owner only.
func (h *handlers) handleDeactivate(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.svc.Deactivate(r.Context(), actor, r.PathValue("orgId")); err != nil {
		h.orgError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListMembers: GET /api/v1/organizations/{orgId}/members — current
// and historical memberships, any current member.
func (h *handlers) handleListMembers(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	members, err := h.svc.ListMembers(r.Context(), actor, r.PathValue("orgId"))
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	out := make([]membershipPayload, 0, len(members))
	for _, m := range members {
		out = append(out, membershipPayloadFromDomain(m))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"members": out})
}

type inviteRequest struct {
	Handle           string `json:"handle"`
	Role             string `json:"role"`
	AffiliationStart string `json:"affiliation_start"`
	Verified         bool   `json:"verified"`
}

// handleInvite: POST /api/v1/organizations/{orgId}/members — owner only.
func (h *handlers) handleInvite(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req inviteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	start, err := parseDate(req.AffiliationStart)
	if err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, orgs.CodeValidationFailed,
			"affiliation_start must be a YYYY-MM-DD date")
		return
	}
	membership, err := h.svc.Invite(r.Context(), actor, r.PathValue("orgId"), req.Handle,
		domain.OrgRole(req.Role), start, req.Verified)
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, membershipPayloadFromDomain(membership))
}

type updateMembershipRequest struct {
	Role             *string `json:"role"`
	AffiliationStart string  `json:"affiliation_start"`
	Verified         *bool   `json:"verified"`
}

// handleUpdateMembership: PATCH /api/v1/organizations/{orgId}/members/{userId}
// — adjust role/start/verified, owner only (a non-owner cannot promote
// anyone, themselves included).
func (h *handlers) handleUpdateMembership(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req updateMembershipRequest
	if !decodeBody(w, r, &req) {
		return
	}
	var role *domain.OrgRole
	if req.Role != nil {
		r := domain.OrgRole(*req.Role)
		role = &r
	}
	var start *time.Time
	if req.AffiliationStart != "" {
		t, err := time.Parse("2006-01-02", req.AffiliationStart)
		if err != nil {
			authhttp.WriteError(w, r, http.StatusBadRequest, orgs.CodeValidationFailed,
				"affiliation_start must be a YYYY-MM-DD date")
			return
		}
		start = &t
	}
	membership, err := h.svc.UpdateMembership(r.Context(), actor, r.PathValue("orgId"),
		r.PathValue("userId"), role, start, req.Verified)
	if err != nil {
		h.orgError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, membershipPayloadFromDomain(membership))
}

// handleRemoveMember: DELETE /api/v1/organizations/{orgId}/members/{userId}
// — end the affiliation (row kept: 离职不删除历史). An owner may remove
// anyone; a member may remove themselves (leave).
func (h *handlers) handleRemoveMember(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	if err := h.svc.RemoveMember(r.Context(), actor, r.PathValue("orgId"),
		r.PathValue("userId")); err != nil {
		h.orgError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// orgError maps Service errors to the wire (docs/45: stable codes, no
// dependency detail). Unknown errors are logged with detail and answered
// with a generic envelope.
func (h *handlers) orgError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, orgs.ErrOrgNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, orgs.CodeOrgNotFound,
			"organization not found")
	case errors.Is(err, orgs.ErrSlugTaken):
		authhttp.WriteError(w, r, http.StatusConflict, orgs.CodeOrgSlugTaken,
			"an organization with this slug already exists")
	case errors.Is(err, orgs.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, orgs.CodeOrgForbidden,
			"only organization owners may perform this action")
	case errors.Is(err, orgs.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, orgs.CodeValidationFailed, err.Error())
	case errors.Is(err, orgs.ErrOrgDeactivated):
		authhttp.WriteError(w, r, http.StatusConflict, orgs.CodeOrgDeactivated,
			"the organization is deactivated")
	case errors.Is(err, orgs.ErrMemberNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, orgs.CodeMemberNotFound,
			"membership not found")
	case errors.Is(err, orgs.ErrAlreadyMember):
		authhttp.WriteError(w, r, http.StatusConflict, orgs.CodeMemberAlreadyExists,
			"the user is already a member of this organization")
	case errors.Is(err, orgs.ErrLastOwner):
		authhttp.WriteError(w, r, http.StatusConflict, orgs.CodeLastOwner,
			"the organization must keep at least one active owner")
	case errors.Is(err, orgs.ErrUserNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, orgs.CodeUserNotFound,
			"no user with this handle")
	case errors.Is(err, orgs.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, orgs.CodeServiceUnavailable,
			"organization data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("orgs handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
