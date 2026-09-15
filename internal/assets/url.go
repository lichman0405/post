package assets

import "strings"

// The persistent URL scheme of the research asset network (T0701):
// asset pages and version pages are addressed by the pid, never by the
// slug, the owning organization, or any other revisable attribute.
// Because a pid is generated once and never changes (see PID), every
// URL built here stays valid across renames and ownership transfers —
// the "persistent URLs" requirement and the acceptance criterion
// (asset ID 不随 slug/org 变化) are the same guarantee.
//
// Shapes:
//
//	web asset page      /assets/{pid}
//	web version page    /assets/{pid}/{version}
//	API asset page data /api/v1/assets/{pid}
//
// The API path is public page data (security: []) and therefore
// parallel to the web page, not nested under /api/v1/projects/.
//
// The shapes above are THIS package's scheme: these builders emit the
// pid. The API contract is not there yet, and it is worth being exact
// about that, because the two ends have to meet. specs/api/openapi.yaml
// declares the path /assets/{assetId} with `schema: {type: string}` and
// no description — the word "pid" does not occur anywhere in that file
// (grep -rni pid specs/api/openapi.yaml prints nothing). Nothing in the
// spec says the parameter is a pid, and a passing check-openapi cannot
// say it either: scripts/validate_openapi.py checks that a parameter has
// a name and a valid `in:` and never looks at its schema. Binding
// assetId to the pid — in the spec and in the handler that reads it — is
// T0705/T0709's action and has not happened; until it does, a client
// that reads only the spec learns nothing about what assetId is.
//
// The web pages themselves are built in T0709; these builders are the
// canonical single source of the scheme so the API, the web app and any
// rendered link construct the same URL instead of three copies that can
// drift.

// AssetURL is the persistent web URL of one asset's page. It names the
// pid and nothing else — no slug, no organization — so it never
// changes while the asset exists.
func AssetURL(pid PID) string {
	return "/assets/" + string(pid)
}

// AssetVersionURL is the persistent web URL of one published version.
// The version label is immutable (UNIQUE(asset_id, version) and the
// append-only row guard), so the URL is stable forever once the
// version exists.
func AssetVersionURL(pid PID, version string) string {
	return "/assets/" + string(pid) + "/" + version
}

// AssetAPIPath is the v1 API path that serves the asset's public page
// data (the openapi /assets/{assetId} endpoint, whose parameter the
// spec declares as an unconstrained string — pinning it to the pid is
// T0705/T0709's, see the note at the top of this file). It is the
// API-side mirror of AssetURL — both carry the pid, so both are stable.
func AssetAPIPath(pid PID) string {
	return "/api/v1/assets/" + string(pid)
}

// APIAssetIDFromPath extracts the pid from an API request path of the
// shape /api/v1/assets/{pid} and reports whether the segment is a
// valid pid. A segment that is not a pid shape is reported false —
// callers answer "not found" for it. This package resolves the segment
// as a pid and never as a slug, so a slug-shaped segment cannot resolve
// here; note that the spec's assetId parameter constrains nothing
// (see the note at the top of this file), so this side is currently the
// only side that has an opinion.
func APIAssetIDFromPath(path string) (PID, bool) {
	const prefix = "/api/v1/assets/"
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok || strings.Contains(rest, "/") {
		return "", false
	}
	pid := PID(rest)
	if !pid.Valid() {
		return "", false
	}
	return pid, true
}
