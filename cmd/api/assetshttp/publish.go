package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The publish surface (T0705): POST
// /api/v1/projects/{projectId}/assets:publish. The route the preview route
// previews — one governed write, and the only write this package registers.
//
// # What the handler decides, and what it does not
//
// Nothing about the publication is decided here. The handler reads the
// body, takes the Idempotency-Key off the header, resolves the principal
// and calls the command; every governance decision — whether this actor
// may publish, whether the policy in force permits it, whether the current
// state would leak a private dependency, and what the row it writes
// contains — belongs to internal/application/assetpublish and commits or
// refuses inside that command's transaction. A transport that decided any
// of it would be the second, weaker copy of a security decision the
// preview's package doc already refuses to keep.
//
// # The request body
//
// publishRequest embeds previewRequest, and that is deliberate rather than
// convenient: a publish is executed against exactly the document the
// preview was computed for, so the two bodies are the same candidate
// spelled the same way. Every field the gate takes is accepted here under
// the name the preview accepts it under, so a client can send the body it
// previewed without rewriting it. The two additions are title and slug —
// the display fields of an asset the publish CREATES (see the command's
// PublishParams: they are required when the request names no asset, and
// refused when it does).
type publishRequest struct {
	previewRequest
	// Title and Slug are the display fields of an asset this publish
	// creates. Neither is stored on the version row: a publish that names
	// an existing asset refuses them (asset metadata is revised on its own
	// surface, docs/11 §4), and a publish that creates one requires them,
	// because a research asset without a name is not one.
	Title string `json:"title"`
	Slug  string `json:"slug"`
}

// PublishCommand is the publish use case this surface drives (the
// production value is *assetpublish.Command).
type PublishCommand interface {
	Publish(ctx context.Context, actor assetpublish.Actor, in assetpublish.PublishParams) (assetpublish.PublishedVersion, error)
}

// publishedPayload is the 201 body: the version the publish stored, as the
// public identities that name it.
//
// The version row's internal uuid and the asset row's internal uuid are
// NOT on the wire, and that is the same decision the preview's response
// already makes: an asset is named by its pid and a version by its label
// (research_assets.pid, migration 00064), and the pair is what every URL
// and every later reference resolves through. Publishing an internal key
// would make the pid's stability optional in practice.
//
// manifest and rights are the STORED documents, echoed back as they were
// written — the canonical manifest bytes the integrity hash covers, and
// the rights document the gate validated. A client checking that what it
// published is what the repository holds needs the stored bytes, not its
// own request echoed.
type publishedPayload struct {
	AssetPID      string            `json:"asset_pid"`
	Version       string            `json:"version"`
	Visibility    assets.Visibility `json:"visibility"`
	IntegrityHash string            `json:"integrity_hash"`
	OriginRefs    []string          `json:"origin_refs"`
	PublishedBy   string            `json:"published_by"`
	PublishedAt   string            `json:"published_at"`
	Manifest      json.RawMessage   `json:"manifest"`
	Rights        json.RawMessage   `json:"rights"`
}

// publishedFromDomain renders the stored version as the response body.
func publishedFromDomain(v assetpublish.PublishedVersion) publishedPayload {
	return publishedPayloadOf(v.AssetPID, v.Version, v.Visibility, v.IntegrityHash,
		v.OriginRefs, v.PublishedBy, v.PublishedAt, v.Manifest, v.RightsJSON)
}

// publishedPayloadOf renders the nine fields a published version's response
// carries, from the values both write paths return.
//
// It exists as one function because TWO responses carry these fields since
// T0708: the publish's, and the derive's (derivedPayload embeds
// publishedPayload and adds the three edge fields). A version's wire shape
// is one shape — the timestamp format and the empty-list rule below are the
// parts a second copy would sooner or later spell differently — and the
// derivation's response is the publication's response plus its lineage.
func publishedPayloadOf(assetPID, version string, visibility assets.Visibility, integrityHash string,
	originRefs []string, publishedBy string, publishedAt time.Time, manifest, rightsJSON json.RawMessage) publishedPayload {
	refs := originRefs
	if refs == nil {
		// A version with no provenance pins is a legal version (the gate
		// asks only that a ref it is given be well-formed), and the wire
		// spells an empty list rather than null so a client iterating the
		// field does not have to branch.
		refs = []string{}
	}
	return publishedPayload{
		AssetPID:      assetPID,
		Version:       version,
		Visibility:    visibility,
		IntegrityHash: integrityHash,
		OriginRefs:    refs,
		PublishedBy:   publishedBy,
		PublishedAt:   publishedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		Manifest:      manifest,
		Rights:        rightsJSON,
	}
}

// handlePublish: POST /api/v1/projects/{projectId}/assets:publish.
//
// The order is the preview's order, for the same reason: the principal is
// resolved, then the body is read, then the command runs. The project is
// NOT gated here — the command authorizes the actor against the permission
// matrix before it reads anything, which is the only question that decides
// whether the write may happen, and answering it first is what keeps a
// denial from disclosing which projects exist (the releases rule: the
// denial is resolved before any target lookup, so it never discloses
// whether the project does).
//
// IsAgent is false, and that is a statement about THIS BUILD rather than
// about the product rule: V1 authenticates sessions, so no request
// reaching this handler arrived as a platform agent. The command's
// backstop is consulted on every call regardless (it is the first thing
// Publish does), so the field being false here does not make it dead code:
// internal/application/assetpublish's non-HTTP callers do carry an agent
// flag, and the matrix denies agents independently
// (publish_private_to_public, agent column: deny).
func (h *handlers) handlePublish(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	var req publishRequest
	if !decodePublishBody(w, r, &req) {
		return
	}
	var key *string
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		key = &v
	}
	stored, err := h.publish.Publish(r.Context(),
		assetpublish.Actor{User: user, IsAgent: false},
		assetpublish.PublishParams{
			ProjectID:      r.PathValue("projectId"),
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
			IdempotencyKey: key,
		})
	if err != nil {
		writePublishError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusCreated, publishedFromDomain(stored))
}

// principalUser resolves the authenticated actor for the write; a missing
// session is answered in place with 401 (the v1 guard enforces the same
// before routing — this is the handler-level backstop, the same one the
// preview and the release surface keep).
//
// It returns the USER rather than the command's Actor value, because the
// agent flag is a fact about the build's authentication (V1 resolves
// sessions and nothing else) that this function cannot read off a session:
// the one caller that knows it is false is handlePublish, which sets it
// there, in one place. The flag is never read from the request body — a
// caller that could set it could clear it, and the only thing it does is
// refuse.
func principalUser(w http.ResponseWriter, r *http.Request) (domain.User, bool) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		authhttp.WriteError(w, r, http.StatusUnauthorized, authn.CodeUnauthenticated,
			"authentication required")
		return domain.User{}, false
	}
	return p.User, true
}

// decodePublishBody parses the request body (bounded; unknown fields
// ignored) and reports success — a malformed body is answered in place
// with the same code the command's own validation refuses with, so a
// client branches on one code for "this body is not a publish candidate".
func decodePublishBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, assetpublish.CodeValidationFailed,
			"request body must be one JSON object describing the version to publish")
		return false
	}
	return true
}

// blockedEnvelope is the refusal body of a publish the server-side impact
// re-check refused: the shared error envelope (docs/22 §5, docs/45) with
// the COMPLETE preview attached.
//
// The extra field is the point of the response. docs/23 §4's rule is that
// no hidden private dependency rides along into a published version, and a
// client told only "refused" cannot see which entry blocked it — nor check
// that the refusal was about the entry it thinks it was. The refusal
// therefore carries the same document POST .../assets:publish-preview
// answers with, computed over the state the publish itself saw, so a
// caller can compare what it previewed against what the publish refused
// over. This mirrors the release surface, where the gate's report is what
// the caller is shown (releases.GateRefused); here the report is
// structured rather than rendered as a sentence, because the entries it
// names are the caller's own inputs and it has to be able to act on each
// one.
//
// The envelope is spelled out rather than reused from authhttp because
// authhttp's errorEnvelope is unexported and has no extension point; the
// four shared fields are copied verbatim so the two shapes stay one shape
// for a client that branches on `code` first.
type blockedEnvelope struct {
	Code      string               `json:"code"`
	Message   string               `json:"message"`
	RequestID string               `json:"request_id"`
	Retryable bool                 `json:"retryable"`
	Preview   assets.ImpactPreview `json:"preview"`
}

// writePublishBlocked renders the refusal with its report.
func writePublishBlocked(w http.ResponseWriter, r *http.Request, refused *assetpublish.PublishRefused) {
	message := "the publication was refused by the server-side impact re-check"
	if len(refused.Reasons) > 0 {
		message = "the publication was refused: " + strings.Join(refused.Reasons, "; ")
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(blockedEnvelope{
		Code:      assetpublish.CodePublishBlocked,
		Message:   message,
		RequestID: requestID(r),
		Retryable: false,
		Preview:   refused.Preview,
	})
}

// requestID is the observability request id of the shared envelope
// (authhttp's own helper is unexported; this reads the same value).
func requestID(r *http.Request) string {
	id, ok := observability.FromContext(r.Context())
	if !ok {
		return ""
	}
	return id.String()
}

// writePublishError maps the command's outcomes onto the wire (docs/45:
// one stable code per outcome, no dependency detail).
//
// The statuses follow the vocabulary the rest of the product already
// fixes: 400 for a body that is not a candidate, 403 for every decision
// that says "not you", and 409 for every decision that says "not in this
// state". The last two are different answers to a client — an
// unauthorized caller is not being asked to retry — so they are not
// collapsed.
func writePublishError(w http.ResponseWriter, r *http.Request, err error) {
	// Matched first, by TYPE: the refusals that carry a report are the only
	// outcomes whose response body is more than the envelope.
	var refused *assetpublish.PublishRefused
	if errors.As(err, &refused) {
		writePublishBlocked(w, r, refused)
		return
	}
	switch {
	case errors.Is(err, assetpublish.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, assetpublish.CodeValidationFailed, err.Error())
	case errors.Is(err, assetpublish.ErrAgentNotPermitted):
		// The domain backstop, answered with a code of its own: a governance
		// change may edit a matrix cell, and this refusal is the one that
		// survives the edit (docs/23 §4). A client that cannot tell the two
		// apart cannot tell which line of defence refused it.
		authhttp.WriteError(w, r, http.StatusForbidden, assetpublish.CodeAgentPublishDenied,
			"a platform agent may not publish a research asset")
	case errors.Is(err, assetpublish.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, assetpublish.CodePublishPrivateToPublicRequiresApproval,
			"the actor may not publish research assets in this project")
	case errors.Is(err, assetpublish.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, assetpublish.CodeProjectNotFound, "project not found")
	case errors.Is(err, assetpublish.ErrAssetNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, assetpublish.CodeAssetNotFound,
			"the asset this version belongs to was not found in this project")
	case errors.Is(err, assetpublish.ErrAssetExists):
		authhttp.WriteError(w, r, http.StatusConflict, assetpublish.CodeAssetExists,
			"an asset with that identifier already exists")
	case errors.Is(err, assetpublish.ErrVersionImmutable):
		authhttp.WriteError(w, r, http.StatusConflict, assetpublish.CodeAssetVersionImmutable,
			"this asset version is already published, and a published version is immutable")
	case errors.Is(err, assetpublish.ErrIdempotencyConflict):
		authhttp.WriteError(w, r, http.StatusConflict, assetpublish.CodeIdempotencyConflict,
			"this Idempotency-Key was used for a different publication")
	case errors.Is(err, assetpublish.ErrPolicyRefused):
		authhttp.WriteError(w, r, http.StatusForbidden, assetpublish.CodePolicyRefused,
			"the policy in force does not permit this publication")
	case errors.Is(err, assetpublish.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("asset publish: store failure", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, assetpublish.CodeServiceUnavailable,
			"publish data is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("asset publish: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
