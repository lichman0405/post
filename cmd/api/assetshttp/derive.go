package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/observability"
)

// The derive surface (T0708): POST
// /api/v1/projects/{projectId}/assets:derive. The second governed write this
// package registers, beside :publish, and the one that creates a NEW asset
// identity from an existing published version (docs/11 §5 "Fork/Derive：创建
// 新的 Asset/Object identity，保留 lineage").
//
// # What the handler decides, and what it does not
//
// Nothing about the derivation is decided here. The handler reads the body,
// takes the Idempotency-Key off the header, resolves the principal and calls
// the command; every decision — whether this actor may derive into this
// project, whether the caller may read the parent at all, what the parent's
// stored rights declaration permits, and what the rows it writes contain —
// belongs to internal/assets.DeriveCommand and commits or refuses inside its
// store's transaction. A transport that decided any of it would be a second,
// weaker copy of a security decision.
//
// # The path, and the contract
//
// The path is NOT in specs/api/openapi.yaml, and it is mounted anyway, the
// way the browse route and the project-side dependency read are. The reason
// is the same in all three cases: the contract is Supervisor-owned and this
// task may not edit specs/**, while the write path itself is the task. The
// shape follows the publish's custom-method route
// (/projects/{projectId}/assets:publish) verbatim — one colon method under
// the project's assets — so a client that knows the publish already knows
// where this lives, and the contract can adopt it with one path entry.
//
// # The request body
//
// The version document fields are the publish body's, under the publish's
// names, because a derivation stores a version document through the same
// gate and the same schema: a client can send the document it publishes
// with. Three fields are the derivation's own:
//
//   - parent — the ONE published version the new identity comes from, as a
//     canonical pid@version reference. It is required, and it is the
//     version, never the asset and never the slug.
//   - relation — forked_from or derived_from. The caller chooses, because
//     the caller knows what it made; 'supersedes' is not accepted (a
//     supersession does not create an identity, docs/11 §7).
//   - derivatives_confirmation — the explicit statement required when the
//     parent's declaration leaves derivation unspecified (see
//     assets.RequireDerivable). It is recorded with the actor in the audit
//     row.
//
// asset_pid is accepted and must be EMPTY: a derivation always creates a new
// identity, and a caller that names an existing asset is answered with a
// refusal rather than a second asset it did not ask for.
type deriveRequest struct {
	// Parent is the version the new identity is created from.
	Parent string `json:"parent"`
	// Relation is forked_from or derived_from.
	Relation string `json:"relation"`
	// AssetPID must be empty; see the type doc.
	AssetPID string `json:"asset_pid"`
	// The version document of the NEW asset, under the publish body's names.
	AssetType     string          `json:"asset_type"`
	Version       string          `json:"version"`
	Manifest      json.RawMessage `json:"manifest"`
	Rights        json.RawMessage `json:"rights"`
	OriginRefs    []string        `json:"origin_refs"`
	Visibility    string          `json:"visibility"`
	IntegrityHash string          `json:"integrity_hash"`
	CreatorIDs    []string        `json:"creator_ids"`
	// The new asset's display fields.
	Title string `json:"title"`
	Slug  string `json:"slug"`
	// DerivativesConfirmation is the caller's explicit statement for a parent
	// whose declaration leaves derivation unspecified.
	DerivativesConfirmation string `json:"derivatives_confirmation"`
}

// DeriveCommand is the derive use case this surface drives (the production
// value is *assets.DeriveCommand).
type DeriveCommand interface {
	Derive(ctx context.Context, actor assets.DeriveActor, in assets.DeriveParams) (assets.DerivedAsset, error)
}

// derivedPayload is the 201 body: the new asset's version as the public
// identities that name it, plus the edge.
//
// It embeds the publish's payload rather than restating its nine fields: a
// derivation's response IS a published version's response, and the three
// fields below are what the derivation adds. Internal uuids stay off the
// wire for the publish's reason — an asset is named by its pid.
type derivedPayload struct {
	publishedPayload
	// ParentAssetPID and ParentVersion name the version the new asset came
	// from, and Relation is the relation the stored edge records. All three
	// are read back from the stored derivation (the ledger's own account on
	// a replay), not echoed from the request, so a client can check what the
	// repository holds rather than what it asked for.
	ParentAssetPID string `json:"parent_asset_pid"`
	ParentVersion  string `json:"parent_version"`
	Relation       string `json:"relation"`
}

// derivedFromDomain renders the stored derivation as the response body: the
// child version through the shared published-version renderer, plus the edge
// the derivation recorded.
func derivedFromDomain(d assets.DerivedAsset) derivedPayload {
	return derivedPayload{
		publishedPayload: publishedPayloadOf(d.AssetPID, d.Version, d.Visibility, d.IntegrityHash,
			d.OriginRefs, d.PublishedBy, d.PublishedAt, d.Manifest, d.RightsJSON),
		ParentAssetPID: d.ParentAssetPID,
		ParentVersion:  d.ParentVersion,
		Relation:       string(d.Relation),
	}
}

// handleDerive: POST /api/v1/projects/{projectId}/assets:derive.
//
// The order is the publish handler's order: the principal is resolved, then
// the body is read, then the command runs. The project is NOT gated here —
// the command authorizes the actor against the permission matrix before it
// reads anything, which is what keeps a denial from disclosing which
// projects exist.
//
// IsAgent is false for the publish handler's reason: V1 authenticates
// sessions, so no request reaching this handler arrived as a platform agent,
// and the field is set in one place rather than read from a body.
func (h *handlers) handleDerive(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	var req deriveRequest
	if !decodeDeriveBody(w, r, &req) {
		return
	}
	var key *string
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		key = &v
	}
	stored, err := h.derive.Derive(r.Context(),
		assets.DeriveActor{User: user, IsAgent: false},
		assets.DeriveParams{
			ProjectID:      r.PathValue("projectId"),
			Parent:         req.Parent,
			Relation:       req.Relation,
			AssetPID:       req.AssetPID,
			AssetType:      req.AssetType,
			Version:        req.Version,
			Manifest:       req.Manifest,
			Rights:         req.Rights,
			OriginRefs:     req.OriginRefs,
			Visibility:     req.Visibility,
			IntegrityHash:  req.IntegrityHash,
			CreatorIDs:     req.CreatorIDs,
			Title:          req.Title,
			Slug:           req.Slug,
			Confirmation:   req.DerivativesConfirmation,
			IdempotencyKey: key,
		})
	if err != nil {
		writeDeriveError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, derivedFromDomain(stored))
}

// decodeDeriveBody parses the request body (bounded; unknown fields
// ignored) and reports success — a malformed body is answered in place with
// the command's own validation code, so a client branches on one code for
// "this body is not a derivation".
func decodeDeriveBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, assets.CodeDeriveValidationFailed,
			"request body must be one JSON object describing the version to derive")
		return false
	}
	return true
}

// writeDeriveError maps the command's outcomes onto the wire (docs/45: one
// stable code per outcome, no dependency detail).
//
// The statuses are the publish's statuses for the same outcomes: 400 for a
// body that is not a candidate, 403 for every decision that says "not you",
// 404 for the parent the caller may not read (answered exactly as an absent
// version is), and 409 for every decision that says "not in this state".
func writeDeriveError(w http.ResponseWriter, r *http.Request, err error) {
	// Matched first, by TYPE: the refusals that carry a report are the only
	// outcomes whose response body is more than the envelope.
	var refused *assets.DeriveRefused
	if errors.As(err, &refused) {
		writeDeriveBlocked(w, r, refused)
		return
	}
	// The rights refusals carry their own code (which of the three cases
	// fired, or "unreadable"), so they are answered from the error rather
	// than from a package constant.
	var rightsRefusal *assets.RightsRefusal
	if errors.As(err, &rightsRefusal) {
		status := http.StatusForbidden
		switch rightsRefusal.Code() {
		case assets.CodeDeriveValidationFailed:
			status = http.StatusBadRequest
		case assets.CodeDeriveParentRightsUnreadable:
			// The parent's declaration could not be read. It is answered 409
			// (not 403) because it is a fact about the STORED ROW rather than
			// about the caller: another version of the same asset, or the
			// same version once its publisher has published a document this
			// platform reads, would not have this problem.
			status = http.StatusConflict
		}
		authhttp.WriteError(w, r, status, rightsRefusal.Code(), rightsRefusal.Error())
		return
	}
	switch {
	case errors.Is(err, assets.ErrDeriveValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, assets.CodeDeriveValidationFailed, err.Error())
	case errors.Is(err, assets.ErrDeriveAgentNotPermitted):
		// The domain backstop, with a code of its own: a governance change
		// may edit a matrix cell, and this refusal is the one that survives
		// the edit.
		authhttp.WriteError(w, r, http.StatusForbidden, assets.CodeDeriveAgentDenied,
			"a platform agent may not derive a research asset")
	case errors.Is(err, assets.ErrDeriveForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, assets.CodeDeriveNotPermitted,
			"the actor may not derive research assets in this project")
	case errors.Is(err, assets.ErrDeriveProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, assets.CodeDeriveProjectNotFound, "project not found")
	case errors.Is(err, assets.ErrDeriveParentNotFound):
		// ONE answer for "no such version" and "a version you may not read":
		// the derivation path must not report which versions exist
		// (ADR-024), so the two are one code and one sentence.
		authhttp.WriteError(w, r, http.StatusNotFound, assets.CodeDeriveParentNotFound,
			"the parent version was not found")
	case errors.Is(err, assets.ErrDeriveAssetExists):
		authhttp.WriteError(w, r, http.StatusConflict, assets.CodeDeriveAssetExists,
			"an asset with that identifier already exists")
	case errors.Is(err, assets.ErrDeriveVersionImmutable):
		authhttp.WriteError(w, r, http.StatusConflict, assets.CodeDeriveVersionImmutable,
			"this asset version is already published, and a published version is immutable")
	case errors.Is(err, assets.ErrDeriveIdempotencyConflict):
		authhttp.WriteError(w, r, http.StatusConflict, assets.CodeDeriveIdempotencyConflict,
			"this Idempotency-Key was used for a different derivation")
	case errors.Is(err, assets.ErrDerivePolicyRefused):
		// The publish's own outcome for the publish's own rule, answered in
		// the publish's own words: RIGHTS_POLICY_BLOCKS_ACTION is the code a
		// client looks up to find the governance step it is missing, and the
		// step it is missing is one. The rule and the reason stay on the
		// error for the log rather than on the wire.
		authhttp.WriteError(w, r, http.StatusForbidden, assets.CodeDerivePolicyRefused,
			"the policy in force does not permit this derivation")
	case errors.Is(err, assets.ErrDeriveStore):
		observability.LoggerFromContext(r.Context()).Error("asset derive: store failure", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, assets.CodeDeriveServiceUnavailable,
			"derive data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("asset derive: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}

// writeDeriveBlocked renders the refusal of a derivation by the server-side
// impact re-check: the shared error envelope with the COMPLETE preview
// attached, shaped exactly as the publish's refusal is (blockedEnvelope).
// The extra field is the point there and here: a client told only "refused"
// cannot see which entry blocked it.
func writeDeriveBlocked(w http.ResponseWriter, r *http.Request, refused *assets.DeriveRefused) {
	message := "the derivation was refused by the server-side impact re-check"
	if len(refused.Reasons) > 0 {
		message = "the derivation was refused: " + strings.Join(refused.Reasons, "; ")
	}
	w.Header().Set("Content-Type", "application/json")
	// nosniff, stated here rather than inherited from the edge (T1106,
	// internal/security): this writer is a byte exit like the publish's, and
	// an exit that is safe only because a middleware above it is present is
	// unsafe everywhere that middleware is absent. tests/security keeps the
	// registry that made this omission visible.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(blockedEnvelope{
		Code:      assets.CodeDeriveBlocked,
		Message:   message,
		RequestID: requestID(r),
		Retryable: false,
		Preview:   refused.Preview,
	})
}
