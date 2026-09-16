package assetshttp

import (
	"context"
	"errors"
	"net/http"

	"github.com/lichman0405/post/cmd/api/authhttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/observability"
)

// The asset hub's read surface (T0709): the asset page data of docs/42
// §Asset Page and the browse list behind /assets (docs/11 §2).
//
// # Where the routes come from
//
// GET /api/v1/assets/{assetId} is the CONTRACT's path (specs/api/openapi.yaml:
// "/assets/{assetId}", `security: []`, "Public/authorized asset page data")
// with the contract's server prefix (/api/v1), so it is registered verbatim
// and anonymous callers reach it — the v1 guard lets an unauthenticated read
// through and the handler decides what that caller may see, which is what
// `security: []` means everywhere else in this tree.
//
// GET /api/v1/assets is NOT in the contract: it is the browse list, and the
// contract does not declare it. It is mounted on the v1 mux anyway, the way
// cmd/api/provenancehttp mounts its walk (T0505) — the browse page needs a
// list read, the contract has no path for one, and inventing a contract
// declaration from an implementation task is not this task's to do (the
// contract is a product artifact; see the task result). Nothing about the
// asset page's own path is affected: the two are distinct ServeMux patterns
// and the more specific one wins for a pid segment.
//
// # The order of the steps is the order of what may be disclosed
//
// The pid is parsed, the state is read, the asset's OWN project is put
// through the project read gate (the same one every project read runs,
// T0106), and only then is the page built. A caller who may not read the
// project is answered the existence-hiding 404 the project surface answers
// (docs/45), which is the same answer an unknown pid gets: from outside,
// "this asset is in a project you may not read" is indistinguishable from
// "there is no such asset". That is deliberate and it is why the 404 is
// produced in two different places here with one code.
//
// # What the response may contain is the model's decision, not this file's
//
// Every rule about what may be rendered — which project identity, which
// version, which usage, which lineage edge, which event — lives in
// internal/assets (BuildPage, BuildBrowse) with unit tests that name the docs
// they come from, and this file serializes the model's output and nothing
// else. In particular it does not filter: a handler that dropped a field on
// its way out would be a second implementation of the disclosure rule, in the
// layer that has no test for it and every reason to be believed.

// Wire codes (docs/45: stable codes, no dependency detail).
const (
	// CodeAssetNotFound: the pid names no asset this caller may see. It is
	// one code for the three cases that answer it — no such pid, an asset
	// whose project this caller may not read, and an asset with no version
	// this caller may see — because they must be indistinguishable.
	CodeAssetNotFound = "ASSET_NOT_FOUND"
	// CodeAssetPageUnavailable: the state read failed, so no honest page can
	// be built. A page is a statement about the repository as it was read,
	// and one built over a failed read would report "no public usage, no
	// lineage, no events" for a repository nobody finished looking at.
	CodeAssetPageUnavailable = "ASSET_PAGE_UNAVAILABLE"
	// CodeAssetListValidationFailed: the browse request is not answerable as
	// given (today: a type outside the closed V1 set). Answered rather than
	// ignored, because an ignored type would return an empty list — which
	// says "there are no such assets", a claim this platform cannot make
	// about a type it does not have.
	CodeAssetListValidationFailed = "ASSET_LIST_VALIDATION_FAILED"
	// CodeAssetPageValidationFailed: the asset page request is not answerable
	// as given (today: a version query parameter that is not a version
	// label). It is its own code rather than the list's because the two are
	// different surfaces: a client that asked for one asset's page and sent a
	// bad label has nothing in common with a client that asked for a listing
	// with an unknown type filter, and a code that told them apart is what
	// lets each be answered — and logged — as what it is. The package's
	// convention is one code per surface (ASSET_PREVIEW_VALIDATION_FAILED is
	// the third).
	CodeAssetPageValidationFailed = "ASSET_PAGE_VALIDATION_FAILED"
)

// PageReader resolves the state one asset page or one browse list is built
// from. The production value is *persistence.AssetPageStore.
//
// LoadAssetPage reports ok=false when the pid names no stored asset — a
// state, not an error. ListBrowseAssets takes the already-validated type
// filter, nil for every type.
type PageReader interface {
	LoadAssetPage(ctx context.Context, pid assets.PID) (assets.PageState, bool, error)
	ListBrowseAssets(ctx context.Context, filter *assets.Type) ([]assets.BrowseRowState, error)
}

// Membership answers whether an authenticated caller belongs to a project —
// the one bit that separates reading the network's view of an asset from
// reading its own project's. The production value is *projects.Service.
//
// A caller with no membership answers projects.ErrMemberNotFound, which is
// "no role" rather than a failure.
type Membership interface {
	GetMembership(ctx context.Context, actor domain.User, projectID string) (domain.ProjectMembership, error)
}

// handleAssetBrowse serves GET /api/v1/assets — the asset hub's list.
//
// The type filter is validated against the closed V1 set before anything is
// read (assets.ParseBrowseFilter), and the response echoes the set back
// (assets.BrowseList.Types) so a client's filter control is built from the
// platform's own set rather than a copy of it. The list is the same for
// every caller: an asset is in it when it has a public version, and a row
// names its project only when that project is public
// (assets.BuildBrowse) — there is no per-caller variant to compute, which is
// the whole reason a listing can be a public read.
func (h *handlers) handleAssetBrowse(w http.ResponseWriter, r *http.Request) {
	filter, ok := assets.ParseBrowseFilter(r.URL.Query().Get("type"))
	if !ok {
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeAssetListValidationFailed,
			"type must be one of "+validTypeList())
		return
	}
	rows, err := h.pages.ListBrowseAssets(r.Context(), filter)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("asset browse: list failed", "error", err)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetPageUnavailable,
			"asset data is temporarily unavailable")
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, assets.BuildBrowse(rows, filter))
}

// handleAssetPage serves GET /api/v1/assets/{assetId} — the asset page data.
//
// The version query parameter names the version to render
// (/assets/{pid}/{version} is a persistent address, internal/assets/url.go):
// absent means the newest version the caller may see. A label that is not a
// version label at all is refused 400 rather than looked up, because it
// cannot name a stored version — can never be stored, ValidVersionLabel is
// the only gate the column has — while a well-formed label that names no
// visible version is the page's 404.
func (h *handlers) handleAssetPage(w http.ResponseWriter, r *http.Request) {
	pid, ok := assets.APIAssetIDFromPath(r.URL.Path)
	if !ok {
		// The segment is not a pid, so it names no asset: answered the 404
		// the package doc of url.go prescribes ("callers answer 'not found'
		// for it"), and the same one a well-formed pid that resolves to
		// nothing gets. A slug-shaped segment cannot resolve here by
		// construction.
		writeAssetNotFound(w, r)
		return
	}
	want := r.URL.Query().Get("version")
	if want != "" && !assets.ValidVersionLabel(want) {
		authhttp.WriteError(w, r, http.StatusBadRequest, CodeAssetPageValidationFailed,
			"version must be a version label (1..64 characters of letters, digits, '.', '_' or '-')")
		return
	}

	state, found, err := h.pages.LoadAssetPage(r.Context(), pid)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("asset page: state read failed", "error", err, "pid", pid)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetPageUnavailable,
			"asset data is temporarily unavailable")
		return
	}
	if !found {
		writeAssetNotFound(w, r)
		return
	}

	// The project read gate: the asset's own project, through the same
	// service every project read uses. A refusal here is the same 404 an
	// unknown pid gets, so the page cannot be used to learn that a private
	// project has published an asset.
	if _, err := h.projects.Get(r.Context(), reader(r), state.Asset.OriginProjectID); err != nil {
		writeAssetGateError(w, r, err, pid)
		return
	}

	viewer, err := h.viewer(r, state.Asset.OriginProjectID)
	if err != nil {
		observability.LoggerFromContext(r.Context()).Error("asset page: membership read failed", "error", err, "pid", pid)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetPageUnavailable,
			"asset data is temporarily unavailable")
		return
	}

	page, ok := assets.BuildPage(state, viewer, want)
	if !ok {
		// Nothing this caller may see: an asset whose every version is
		// private, or a version the caller asked for that they may not see.
		// Both answer the same 404 as an unknown pid.
		writeAssetNotFound(w, r)
		return
	}
	authhttp.WriteJSON(w, http.StatusOK, page)
}

// viewer resolves the membership bit for the project the page is about.
//
// An anonymous caller is never a member — there is no membership to read and
// no user to read it for. For an authenticated caller the answer comes from
// the project service's own GetMembership, which re-runs the project read
// gate and then the membership read: "no role" (ErrMemberNotFound) is the
// false this needs, and anything else is a failure rather than a silent
// "not a member" — a store blip that quietly downgraded a member's view
// would be a page that is wrong in the direction nobody would report.
func (h *handlers) viewer(r *http.Request, projectID string) (assets.PageViewer, error) {
	p, ok := authhttp.PrincipalFrom(r.Context())
	if !ok {
		return assets.PageViewer{}, nil
	}
	_, err := h.members.GetMembership(r.Context(), p.User, projectID)
	switch {
	case err == nil:
		return assets.PageViewer{Member: true}, nil
	case errors.Is(err, projects.ErrMemberNotFound):
		return assets.PageViewer{}, nil
	default:
		return assets.PageViewer{}, err
	}
}

// writeAssetNotFound answers the one 404 this surface has: the pid names
// nothing this caller may see.
//
// It never names the asset, the project, the version or the reason — that is
// the point of the single code — and it is written for the three cases that
// must be indistinguishable (no such pid, an unreadable project, nothing
// visible).
func writeAssetNotFound(w http.ResponseWriter, r *http.Request) {
	authhttp.WriteError(w, r, http.StatusNotFound, CodeAssetNotFound, "asset not found")
}

// writeAssetGateError relays the project read gate's own refusals. Not-found,
// forbidden and unknown all become this surface's 404 (the gate already
// answers not-found for a denied read, T0106; the forbidden branch is kept
// because the gate's contract allows it and a 403 here would confirm that the
// asset's project exists); a store failure stays a 503, never a masked 404.
func writeAssetGateError(w http.ResponseWriter, r *http.Request, err error, pid assets.PID) {
	if errors.Is(err, projects.ErrStore) {
		observability.LoggerFromContext(r.Context()).Error("asset page: project gate store error", "error", err, "pid", pid)
		authhttp.WriteError(w, r, http.StatusServiceUnavailable, CodeAssetPageUnavailable,
			"project data is temporarily unavailable")
		return
	}
	writeAssetNotFound(w, r)
}

// validTypeList renders the closed V1 type set for an error message, quoted
// and comma-separated, from the set itself (assets.AllTypes) rather than a
// copy of it in a string.
func validTypeList() string {
	types := assets.AllTypes()
	out := make([]byte, 0, 64)
	for i, t := range types {
		if i > 0 {
			out = append(out, ',', ' ')
		}
		out = append(out, '"')
		out = append(out, t...)
		out = append(out, '"')
	}
	return string(out)
}
