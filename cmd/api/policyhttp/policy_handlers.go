package policyhttp

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// handlers owns the policy routes.
type handlers struct {
	svc *policy.Service
}

// policyVersionPayload is the client-visible policy version shape. The
// policy document renders deterministically (sorted keys — the domain
// marshaller), so clients can hash what they read.
type policyVersionPayload struct {
	ID             string          `json:"id"`
	OrganizationID *string         `json:"organization_id"`
	ProjectID      *string         `json:"project_id"`
	Version        string          `json:"version"`
	Policy         json.RawMessage `json:"policy"`
	CreatedBy      string          `json:"created_by"`
	CreatedAt      time.Time       `json:"created_at"`
}

func policyVersionPayloadFromDomain(v domain.PolicyVersion) (policyVersionPayload, error) {
	doc, err := v.Policy.MarshalJSON()
	if err != nil {
		return policyVersionPayload{}, err
	}
	out := policyVersionPayload{
		ID:        v.ID,
		Version:   v.Version,
		Policy:    json.RawMessage(doc),
		CreatedBy: v.CreatedBy,
		CreatedAt: v.CreatedAt,
	}
	if v.Scope.OrganizationID != "" {
		out.OrganizationID = &v.Scope.OrganizationID
	}
	if v.Scope.ProjectID != "" {
		out.ProjectID = &v.Scope.ProjectID
	}
	return out, nil
}

// effectivePolicyPayload is the evaluation view of one project: the org
// lower bound, the project policy, and their merge.
type effectivePolicyPayload struct {
	Org             *policyVersionPayload `json:"org_policy"`
	Project         *policyVersionPayload `json:"project_policy"`
	EffectivePolicy json.RawMessage       `json:"effective_policy"`
}

// setPolicyRequest is the PUT body: a new version string plus the policy
// document.
type setPolicyRequest struct {
	Version string          `json:"version"`
	Policy  json.RawMessage `json:"policy"`
}

// principal resolves the authenticated actor; reads require a session
// too (policy visibility is member-only), so the 401 is written here.
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// decodeBody parses a JSON body (bounded; unknown fields ignored per
// contract) and reports success — a malformed body is answered in place.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, policy.CodeValidationFailed,
			"request body must be valid JSON")
		return false
	}
	return true
}

// writePolicyError maps a service error onto the wire envelope
// (docs/45): one code per failure shape, so a client can branch without
// parsing messages.
func writePolicyError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, policy.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, policy.CodeValidationFailed, err.Error())
	case errors.Is(err, policy.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, policy.CodePolicyForbidden,
			"the actor may not perform this policy action")
	case errors.Is(err, policy.ErrOrgNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, policy.CodePolicyOrgNotFound, err.Error())
	case errors.Is(err, policy.ErrOrgDeactivated):
		authhttp.WriteError(w, r, http.StatusConflict, policy.CodePolicyOrgDeactivated, err.Error())
	case errors.Is(err, policy.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, policy.CodePolicyProjectNotFound, err.Error())
	case errors.Is(err, policy.ErrProjectRelaxesOrg):
		authhttp.WriteError(w, r, http.StatusUnprocessableEntity, policy.CodePolicyRelaxesOrg, err.Error())
	case errors.Is(err, policy.ErrVersionTaken):
		authhttp.WriteError(w, r, http.StatusConflict, policy.CodePolicyVersionTaken, err.Error())
	case errors.Is(err, policy.ErrPolicyNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, policy.CodePolicyNotFound, err.Error())
	case errors.Is(err, policy.ErrStore):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, policy.CodeServiceUnavailable,
			"policy data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("policy handler: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// handleGetOrgPolicy: GET /api/v1/organizations/{organizationId}/policy.
func (h *handlers) handleGetOrgPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.svc.GetOrgPolicy(r.Context(), actor, r.PathValue("organizationId"))
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload, err := policyVersionPayloadFromDomain(v)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleListOrgPolicyVersions: GET
// /api/v1/organizations/{organizationId}/policy/versions.
func (h *handlers) handleListOrgPolicyVersions(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	versions, err := h.svc.ListVersions(r.Context(), actor,
		domain.PolicyScope{OrganizationID: r.PathValue("organizationId")})
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	out := make([]policyVersionPayload, 0, len(versions))
	for _, v := range versions {
		payload, err := policyVersionPayloadFromDomain(v)
		if err != nil {
			writePolicyError(w, r, err)
			return
		}
		out = append(out, payload)
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"versions": out})
}

// handleSetOrgPolicy: PUT /api/v1/organizations/{organizationId}/policy.
func (h *handlers) handleSetOrgPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req setPolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	v, err := h.svc.SetOrgPolicy(r.Context(), actor, r.PathValue("organizationId"), req.Version, req.Policy)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload, err := policyVersionPayloadFromDomain(v)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, payload)
}

// handleGetProjectPolicy: GET /api/v1/projects/{projectId}/policy.
func (h *handlers) handleGetProjectPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	v, err := h.svc.GetProjectPolicy(r.Context(), actor, r.PathValue("projectId"))
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload, err := policyVersionPayloadFromDomain(v)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleGetEffectivePolicy: GET /api/v1/projects/{projectId}/policy/effective.
func (h *handlers) handleGetEffectivePolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	eff, err := h.svc.EffectivePolicy(r.Context(), actor, r.PathValue("projectId"))
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload := effectivePolicyPayload{}
	if eff.Org != nil {
		orgPayload, err := policyVersionPayloadFromDomain(*eff.Org)
		if err != nil {
			writePolicyError(w, r, err)
			return
		}
		payload.Org = &orgPayload
	}
	if eff.Project != nil {
		projectPayload, err := policyVersionPayloadFromDomain(*eff.Project)
		if err != nil {
			writePolicyError(w, r, err)
			return
		}
		payload.Project = &projectPayload
	}
	doc, err := eff.Effective.MarshalJSON()
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload.EffectivePolicy = json.RawMessage(doc)
	authhttp.WriteJSON(w, http.StatusOK, payload)
}

// handleListProjectPolicyVersions: GET
// /api/v1/projects/{projectId}/policy/versions.
func (h *handlers) handleListProjectPolicyVersions(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	versions, err := h.svc.ListVersions(r.Context(), actor,
		domain.PolicyScope{ProjectID: r.PathValue("projectId")})
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	out := make([]policyVersionPayload, 0, len(versions))
	for _, v := range versions {
		payload, err := policyVersionPayloadFromDomain(v)
		if err != nil {
			writePolicyError(w, r, err)
			return
		}
		out = append(out, payload)
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"versions": out})
}

// handleSetProjectPolicy: PUT /api/v1/projects/{projectId}/policy.
func (h *handlers) handleSetProjectPolicy(w http.ResponseWriter, r *http.Request) {
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	var req setPolicyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	v, err := h.svc.SetProjectPolicy(r.Context(), actor, r.PathValue("projectId"), req.Version, req.Policy)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	payload, err := policyVersionPayloadFromDomain(v)
	if err != nil {
		writePolicyError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, payload)
}
