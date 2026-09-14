package gittokenshttp

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/gitprovider"
	"github.com/lichman0405/post/internal/observability"
)

// Audit actions for the git-credential surface (the audit store records
// them like any other project action; the token VALUE never appears in
// any summary — docs/55 SECRET class).
const (
	actionTokenIssued   = "git.token_issued"
	actionTokenRevoked  = "git.token_revoked"
	actionAccessRevoked = "git.access_revoked"
)

// Wire error codes (docs/45): stable codes on the standard envelope.
const (
	codeTokenNotFound       = "GIT_TOKEN_NOT_FOUND"
	codeAccessForbidden     = "GIT_ACCESS_FORBIDDEN"
	codeNotProvisioned      = "PROJECT_NOT_PROVISIONED"
	codeProviderUnavailable = "GIT_PROVIDER_UNAVAILABLE"
	codeDisabled            = "GIT_ACCESS_DISABLED"
)

// AuditRecorder is the slice of the audit store the handlers need
// (persistence.AuditStore.Record in production). Optional: nil skips the
// best-effort audit write.
type AuditRecorder interface {
	Record(ctx context.Context, e domain.AuditEntry) error
}

// Deps carries the adapters the git-token surface needs.
type Deps struct {
	// Projects is the projects service instance that serves
	// /api/v1/projects — shared on purpose, so membership and visibility
	// rules are the same object in both surfaces.
	Projects *projects.Service
	// Access is the T0304 service. Nil means the feature is disabled
	// (missing configuration): every route answers 503 naming the
	// missing keys instead of a misleading 404.
	Access *gitprovider.UserAccess
	// Audit is the best-effort audit sink (nil in unit tests).
	Audit AuditRecorder
	// Missing names the configuration keys that disabled the feature
	// (key names only — never values).
	Missing []string
}

// API is the mounted git-token surface.
type API struct {
	handlers *handlers
}

// handlers owns the routes.
type handlers struct {
	projects *projects.Service
	access   *gitprovider.UserAccess
	audit    AuditRecorder
	missing  []string
}

// New wires the surface.
func New(deps Deps) *API {
	return &API{handlers: &handlers{
		projects: deps.Projects,
		access:   deps.Access,
		audit:    deps.Audit,
		missing:  deps.Missing,
	}}
}

// Register mounts the git-token routes on the guarded /api/v1 mux. The
// guard (session + CSRF) runs before routing, so every handler here
// starts from an authenticated-actor resolution — the same structural
// guarantee as the projects surface.
func (a *API) Register(mux *http.ServeMux) {
	h := a.handlers
	mux.HandleFunc("POST /api/v1/projects/{projectId}/git-tokens", h.handleIssue)
	mux.HandleFunc("GET /api/v1/projects/{projectId}/git-tokens", h.handleList)
	mux.HandleFunc("DELETE /api/v1/projects/{projectId}/git-tokens/{tokenId}", h.handleRevokeToken)
	mux.HandleFunc("DELETE /api/v1/projects/{projectId}/git-access", h.handleRevokeAccess)
}

// issuedPayload is the shown-once issue response. Token and CloneURL
// carry the credential — they exist only in this payload and in the
// user's client.
type issuedPayload struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"project_id"`
	Scope     string    `json:"scope"`
	Name      string    `json:"name"`
	Token     string    `json:"token"`
	CloneURL  string    `json:"clone_url"`
	IssuedAt  time.Time `json:"issued_at"`
}

// tokenPayload is the listed-token shape (no credential, ever).
type tokenPayload struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	Scope     string     `json:"scope"`
	Name      string     `json:"name"`
	Status    string     `json:"status"`
	IssuedAt  time.Time  `json:"issued_at"`
	RevokedAt *time.Time `json:"revoked_at"`
}

func tokenPayloadFromRecord(r gitprovider.TokenRecord) tokenPayload {
	return tokenPayload{
		ID:        r.ID,
		ProjectID: r.ProjectID,
		Scope:     string(r.Scope),
		Name:      r.TokenName,
		Status:    r.Status,
		IssuedAt:  r.IssuedAt,
		RevokedAt: r.RevokedAt,
	}
}

// enabled answers 503 in place when the feature is disabled, returning
// false. The message names the missing configuration keys — the same
// fail-loud convention as provisioning's startup warning.
func (h *handlers) enabled(w http.ResponseWriter, r *http.Request) bool {
	if h.access != nil {
		return true
	}
	authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeDisabled,
		"git access is disabled: missing configuration "+strings.Join(h.missing, ", "))
	return false
}

// principal resolves the authenticated actor (the guard put the
// principal there; this is the handler-level backstop).
func principal(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// handleIssue: POST /api/v1/projects/{projectId}/git-tokens — issues one
// scoped token for the actor. The body is optional (no fields are read —
// identity-only payloads, docs/52 §17); the scope derives from the
// actor's verified membership role, never from the client.
func (h *handlers) handleIssue(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	projectID := r.PathValue("projectId")

	// Membership and role facts come from the projects service: the
	// project read hides existence for non-members of private projects,
	// and GetMembership distinguishes "no role" from "role".
	if _, err := h.projects.Get(r.Context(),
		projects.Reader{UserID: actor.ID, Authenticated: true}, projectID); err != nil {
		h.projectsError(w, r, err)
		return
	}
	membership, err := h.projects.GetMembership(r.Context(), actor, projectID)
	if err != nil {
		h.projectsError(w, r, err)
		return
	}
	level := levelForRole(membership.Role)

	issued, err := h.access.IssueToken(r.Context(), actor.ID, projectID, level)
	if err != nil {
		h.accessError(w, r, err)
		return
	}
	h.recordAudit(r.Context(), actor, projectID, actionTokenIssued, "token:"+issued.Record.ID,
		map[string]any{"user_id": actor.ID, "scope": string(level)},
		map[string]any{"token_id": issued.Record.ID, "scope": string(level)})
	authhttp.WriteJSON(w, http.StatusCreated, issuedPayload{
		ID:        issued.Record.ID,
		ProjectID: projectID,
		Scope:     string(level),
		Name:      issued.Record.TokenName,
		Token:     issued.Value,
		CloneURL:  issued.CloneURL,
		IssuedAt:  issued.Record.IssuedAt,
	})
}

// levelForRole projects the platform role onto the token scope through
// the permission matrix facts (specs/policies/permissions-matrix.csv):
// write_scientific_state permits contributor and above, so a viewer's
// token is read-only while every role above can push.
func levelForRole(role domain.ProjectRole) gitprovider.AccessLevel {
	switch role {
	case domain.ProjectRoleContributor, domain.ProjectRoleMaintainer, domain.ProjectRoleOwner:
		return gitprovider.AccessWrite
	default:
		return gitprovider.AccessRead
	}
}

// handleList: GET /api/v1/projects/{projectId}/git-tokens — the actor's
// own tokens for the project, newest first (active and revoked — the
// history stays visible). Reading the list requires the same visibility
// as reading the project.
func (h *handlers) handleList(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	projectID := r.PathValue("projectId")
	if _, err := h.projects.Get(r.Context(),
		projects.Reader{UserID: actor.ID, Authenticated: true}, projectID); err != nil {
		h.projectsError(w, r, err)
		return
	}
	records, err := h.access.ListTokens(r.Context(), actor.ID, projectID)
	if err != nil {
		h.accessError(w, r, err)
		return
	}
	out := make([]tokenPayload, 0, len(records))
	for _, rec := range records {
		out = append(out, tokenPayloadFromRecord(rec))
	}
	authhttp.WriteJSON(w, http.StatusOK, map[string]any{"git_tokens": out})
}

// handleRevokeToken: DELETE /api/v1/projects/{projectId}/git-tokens/
// {tokenId} — revokes one of the actor's own tokens (the service enforces
// ownership). Not gated on project visibility: a former member must
// still be able to kill their own credential.
func (h *handlers) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	projectID, tokenID := r.PathValue("projectId"), r.PathValue("tokenId")
	if err := h.access.RevokeToken(r.Context(), actor.ID, projectID, tokenID); err != nil {
		h.accessError(w, r, err)
		return
	}
	h.recordAudit(r.Context(), actor, projectID, actionTokenRevoked, "token:"+tokenID,
		map[string]any{"user_id": actor.ID, "token_id": tokenID}, nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeAccess: DELETE /api/v1/projects/{projectId}/git-access —
// the actor disconnects their entire git access on the project: the
// collaborator grant and every token die provider-side, the canonical
// rows flip to revoked. Self-service only in V1 (the maintainer-level
// "revoke any" arrives with membership removal, which this service is
// the hook for).
func (h *handlers) handleRevokeAccess(w http.ResponseWriter, r *http.Request) {
	if !h.enabled(w, r) {
		return
	}
	actor, ok := principal(w, r)
	if !ok {
		return
	}
	projectID := r.PathValue("projectId")
	revoked, err := h.access.RevokeAccess(r.Context(), actor.ID, projectID)
	if err != nil {
		h.accessError(w, r, err)
		return
	}
	// The audit entry is written only when a live grant was actually
	// revoked: a no-op disconnect (no grant, or one already revoked)
	// must not fabricate a revocation event in the project's audit
	// stream (review finding — the previous unconditional write let any
	// authenticated user plant a fake git.access_revoked entry).
	if revoked {
		h.recordAudit(r.Context(), actor, projectID, actionAccessRevoked, "project:"+projectID,
			map[string]any{"user_id": actor.ID}, nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// recordAudit writes one audit entry best-effort (like the auth surface: the
// credential store owns its transactions, the audit trail is
// supplementary). The summaries never carry the token value.
func (h *handlers) recordAudit(ctx context.Context, actor domain.User, projectID, action, targetRef string, before, after any) {
	if h.audit == nil {
		return
	}
	correlationID, ok := observability.FromContext(ctx)
	if !ok {
		return
	}
	err := h.audit.Record(ctx, domain.AuditEntry{
		ActorID:       actor.ID,
		Via:           domain.ViaSession,
		Action:        action,
		TargetRef:     targetRef,
		ProjectID:     projectID,
		CorrelationID: correlationID.String(),
		BeforeSummary: before,
		AfterSummary:  after,
	})
	if err != nil {
		observability.LoggerFromContext(ctx).Warn(
			"gittokens: audit write failed", "action", action, "error", err.Error())
	}
}

// projectsError maps projects-service errors with the same codes the
// projects surface uses (a shared service deserves a shared vocabulary).
func (h *handlers) projectsError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, projects.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, projects.ErrMemberNotFound):
		authhttp.WriteError(w, r, http.StatusForbidden, codeAccessForbidden,
			"you are not a member of this project")
	case errors.Is(err, projects.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, codeAccessForbidden,
			"you may not manage git access for this project")
	default:
		observability.LoggerFromContext(r.Context()).Error(
			"gittokens: project lookup failed", "error", err.Error())
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE",
			"the project store could not answer")
	}
}

// accessError maps service/provider errors to the wire. Provider
// credential rejections are an operator problem (the admin pair or the
// service token is wrong) — 503 naming it, never a 401 to the caller.
func (h *handlers) accessError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, gitprovider.ErrTokenNotFound),
		errors.Is(err, gitprovider.ErrTokenNotOwned):
		// Existence-hiding: a token that does not exist and a token that
		// belongs to someone else answer identically.
		authhttp.WriteError(w, r, http.StatusNotFound, codeTokenNotFound,
			"git token not found")
	case errors.Is(err, gitprovider.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, projects.CodeProjectNotFound,
			"project not found")
	case errors.Is(err, gitprovider.ErrRepoNotProvisioned):
		authhttp.WriteError(w, r, http.StatusConflict, codeNotProvisioned,
			"the project's repository is not provisioned yet — retry after provisioning completes")
	case errors.Is(err, gitprovider.ErrConflict):
		authhttp.WriteError(w, r, http.StatusBadRequest, "VALIDATION_FAILED", err.Error())
	case errors.Is(err, gitprovider.ErrUnauthorized):
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeProviderUnavailable,
			"the git provider rejected the platform's credentials — check POST_GITEA_ADMIN_USER/POST_GITEA_ADMIN_PASSWORD/POST_GITEA_TOKEN")
	default:
		observability.LoggerFromContext(r.Context()).Error(
			"gittokens: access service failed", "error", err.Error())
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, codeProviderUnavailable,
			"the git access service could not answer")
	}
}
