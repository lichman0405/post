package assetshttp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The asset metadata revision surface (T0706): PATCH /api/v1/assets/{assetId}.
//
// # The route, and why it is this one
//
// docs/11 §4 gives asset metadata its own surface, and the transport that
// serves it has to be addressable by the asset's own identity: the pid is
// what docs/11 §2 makes stable and what every URL is built from, so the
// route is the asset page's path (assets.AssetAPIPath, the openapi
// /assets/{assetId} endpoint) under PATCH. The projects surface spells the
// same relationship PATCH /api/v1/projects/{projectId} for its own
// settings, and the two read as what they are: the mutable half of an
// entity addressed by its identity.
//
// The pattern is NOT in specs/api/openapi.yaml, and that is recorded
// rather than hidden: the contract declares POST
// .../assets:publish-preview, POST .../assets:publish and GET
// /assets/{assetId}. specs/** is outside this task's scope, so the
// metadata revision is mounted here the way the browse list and the
// project dependency read already are (GET /api/v1/assets and GET
// /api/v1/projects/{projectId}/dependencies, page.go and
// dependencies.go) — a surface the product needs and the contract has not
// caught up to. It coexists with the page route because the method
// selects the pattern.
//
// # The project is a FIELD of the request, not a path segment
//
// metadataRequest.ProjectID is required, and that is the authorization's
// shape rather than a transport convenience — the same field
// assetrights.ChangeParams.ProjectID exists for: the actor's membership
// is resolved for THAT project before the asset is looked up, so a denial
// precedes the pid lookup and answers the same thing whether or not the
// pid names anything. The command's store then verifies the asset's
// origin project is the one named, so the pair cannot be used to
// authorize in one project and write in another.
//
// # What the handler decides, and what it does not
//
// Nothing about the revision is decided here. The handler resolves the
// principal, reads the body, reads the pid out of the path, and calls the
// command; whether this actor may revise this asset's metadata, whether
// the asset exists in that project, whether each value is storable, and
// what the audit row records all belong to
// internal/application/assetmetadata and commit or refuse inside that
// command's transaction.
//
// # Why PATCH, and why that makes the body's absent fields mean "unchanged"
//
// A metadata revision changes SOME of an asset's metadata. The command's
// Changes value is a set of optional fields — nil means unchanged — and
// the transport has to be able to express "this field was not mentioned",
// which is exactly what a JSON field's absence gives and what a PUT's
// full-document semantics would take away. The body therefore uses
// POINTERS, and the three list fields use pointers to slices: a bare
// slice cannot be told from an empty one, and "clear the keywords" is a
// different request from "leave the keywords alone". This is the shape
// the projects settings route uses for the same reason (its
// projectshttp settings body), and the one the command's Changes
// documents.

// metadataRequest is the revision body. Every field is optional; absent
// means unchanged.
//
// cover is accepted as a named field and refused BY NAME by the command
// (errCoverNotSupported) rather than left out of the struct entirely: a
// client that sends it is told the channel does not exist yet, where a
// field the decoder never sees would be silently discarded. Unknown
// fields are still ignored, the way every other product route's decoder
// treats them.
type metadataRequest struct {
	ProjectID     string    `json:"project_id"`
	Title         *string   `json:"title"`
	Slug          *string   `json:"slug"`
	Description   *string   `json:"description"`
	Keywords      *[]string `json:"keywords"`
	Contact       *[]string `json:"contact"`
	Documentation *[]string `json:"documentation"`
	Cover         *string   `json:"cover"`
}

// MetadataCommand is the metadata revision use case this surface drives
// (the production value is *assetmetadata.Command).
type MetadataCommand interface {
	Revise(ctx context.Context, actor domain.User, in assetmetadata.ReviseParams) (assetmetadata.Revision, error)
}

// metadataPayload is the response body: the metadata as it now stands,
// plus the persistent identity of the asset it belongs to.
//
// There is deliberately NO version field. docs/11 §4's revision produces
// no scientific version, and a payload with a version slot would invite a
// client to read one — or to send a revise and then fetch a version that
// does not exist. The asset is named by its pid, and the url field is
// built from that pid (assets.AssetURL), so a caller can see for itself
// that the address it already held is the address it still holds after
// the rename: the slug is display metadata and appears in no URL.
type metadataPayload struct {
	AssetPID      string   `json:"asset_pid"`
	URL           string   `json:"url"`
	Title         string   `json:"title"`
	Slug          string   `json:"slug"`
	Description   string   `json:"description"`
	Keywords      []string `json:"keywords"`
	Contact       []string `json:"contact"`
	Documentation []string `json:"documentation"`
	Cover         string   `json:"cover"`
}

// metadataFromDomain renders the stored metadata as the response body.
func metadataFromDomain(r assetmetadata.Revision) metadataPayload {
	m := r.Metadata
	return metadataPayload{
		AssetPID:      r.AssetPID,
		URL:           assets.AssetURL(assets.PID(r.AssetPID)),
		Title:         m.Title,
		Slug:          m.Slug,
		Description:   m.Description,
		Keywords:      nonNil(m.Keywords),
		Contact:       nonNil(m.Contact),
		Documentation: nonNil(m.Documentation),
		Cover:         m.CoverBlobID,
	}
}

// nonNil spells an empty list rather than null, so a client iterating a
// list field does not have to branch (the rule
// publishedFromDomain states for origin_refs).
func nonNil(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// handleAssetMetadata: PATCH /api/v1/assets/{assetId}.
//
// The order is the publish route's order: the principal is resolved, the
// path segment is resolved as a pid, the body is read, and the command
// runs. The project is NOT gated here — the command resolves the actor's
// membership in it before it looks the asset up, which is the only
// question that decides whether the write may happen (the releases rule
// the publish handler states: the denial is resolved before any target
// lookup, so it never discloses whether the target exists).
//
// A path segment that is not a pid is refused here, before the body is
// read, because it names no asset at all — the rule
// assets.APIAssetIDFromPath documents and the page route applies. It is
// answered 404, the same answer an unknown pid gets: a slug-shaped
// segment cannot resolve as an asset, and the pid is the only identity
// this route accepts.
func (h *handlers) handleAssetMetadata(w http.ResponseWriter, r *http.Request) {
	user, ok := principalUser(w, r)
	if !ok {
		return
	}
	pid, ok := assets.APIAssetIDFromPath(r.URL.Path)
	if !ok {
		// Reached only for a segment that is not a pid. The command would
		// refuse it as a validation failure, which would answer 400 for
		// what is really "no such asset" — the page route's rule is the
		// right one: the segment names nothing.
		writeAssetNotFound(w, r)
		return
	}
	var req metadataRequest
	if !decodeMetadataBody(w, r, &req) {
		return
	}
	revision, err := h.metadata.Revise(r.Context(), user, assetmetadata.ReviseParams{
		ProjectID: req.ProjectID,
		AssetPID:  string(pid),
		Changes: assetmetadata.Changes{
			Title:         req.Title,
			Slug:          req.Slug,
			Description:   req.Description,
			Keywords:      req.Keywords,
			Contact:       req.Contact,
			Documentation: req.Documentation,
			Cover:         req.Cover,
		},
	})
	if err != nil {
		writeMetadataError(w, r, err)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, metadataFromDomain(revision))
}

// decodeMetadataBody parses the revision body (bounded; unknown fields
// ignored) and reports success — a malformed body is answered in place
// with the command's own validation code, so a client branches on one
// code for "this body is not a metadata revision".
func decodeMetadataBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(dst); err != nil {
		authhttp.WriteError(w, r, http.StatusBadRequest, assetmetadata.CodeValidationFailed,
			"request body must be one JSON object naming the metadata fields to revise")
		return false
	}
	return true
}

// writeMetadataError maps the command's outcomes onto the wire (docs/45:
// one stable code per outcome, no dependency detail).
//
// The statuses follow the vocabulary the rest of the product fixes: 400
// for a body that is not a revision, 403 for every decision that says
// "not you", 404 for the two existence-hiding answers, 409 for the one
// refusal that says "not in this state" — the reserved cover, which is a
// conflict between the request and what this build can do rather than a
// permission or a shape.
//
// NOT-MEMBER AND ROLE-TOO-LOW ARE ONE ANSWER. Both reach
// assetmetadata.ErrForbidden, and both leave this function through the
// same case, with the same status and the same code. A transport that
// distinguished them would undo the property the command took care to
// build (internal/application/projects/settings.go:174-188): a caller
// probing a project id would learn from the response alone whether it
// holds a membership there that is merely too junior.
func writeMetadataError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, assetmetadata.ErrValidation):
		authhttp.WriteError(w, r, http.StatusBadRequest, assetmetadata.CodeValidationFailed, err.Error())
	case errors.Is(err, assetmetadata.ErrForbidden):
		authhttp.WriteError(w, r, http.StatusForbidden, assetmetadata.CodeForbidden,
			"the actor may not revise this asset's metadata")
	case errors.Is(err, assetmetadata.ErrCoverNotSupported):
		// The refusal carries its own reason verbatim: "the cover is
		// reserved and this build has no channel to serve it" is the whole
		// point of answering instead of dropping the field.
		authhttp.WriteError(w, r, http.StatusConflict, assetmetadata.CodeCoverNotSupported, err.Error())
	case errors.Is(err, assetmetadata.ErrProjectNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, assetmetadata.CodeProjectNotFound, "project not found")
	case errors.Is(err, assetmetadata.ErrAssetNotFound):
		authhttp.WriteError(w, r, http.StatusNotFound, assetmetadata.CodeAssetNotFound,
			"the asset was not found in this project")
	case errors.Is(err, assetmetadata.ErrStore):
		observability.LoggerFromContext(r.Context()).Error("asset metadata: store failure", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, assetmetadata.CodeServiceUnavailable,
			"asset metadata is temporarily unavailable")
	default:
		observability.LoggerFromContext(r.Context()).Error("asset metadata: unexpected error", "error", err)
		authhttp.WriteError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR",
			"an internal error occurred")
	}
}
