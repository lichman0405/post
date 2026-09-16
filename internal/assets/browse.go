package assets

import (
	"sort"
	"time"
)

// The asset hub browse read (T0709): the list behind /assets, the network's
// index of published assets.
//
// docs/42 §Asset Page fixes the page itself; the browse page it belongs to
// is docs/11 §2's hub, and its one stated filter is the asset TYPE — the
// closed V1 set of four (Type, AllTypes), so a caller cannot ask for a
// fifth and get an empty list that looks like "no such assets".
//
// # Which assets are listed
//
// An asset appears here when at least one of its versions is public, and
// the row renders that public state: the newest PUBLIC version and how
// many public versions there are. An asset whose every version is private
// is not listed and not counted — docs/23 §5 keeps private objects out of
// the public API's answers, and "there is an asset you cannot see" is one
// of those answers.
//
// The count is of the versions THIS list may see, which is what a public
// index can honestly say. It is not a total the reader has to take on
// trust: the versions are public, so anyone may enumerate them one by one
// through the asset's own page. A TOTAL version count would be a
// different number, and the difference between the two ("3 of 5") is
// exactly the private-object count docs/23 §5 forbids — so the list
// renders no total.
//
// # Whose project may be named
//
// The same identity rule the asset page applies, and it is the STRICTER
// half of it: a browse row names its originating project only when that
// project is PUBLIC.
//
// A private project may publish an asset (docs/12 §2), so rows like that
// exist, and the asset that results is public — but whose it is, is not.
// The asset page can afford the member case (it has already read that one
// project to answer the request, and the reader is by definition looking
// at that one asset); this list would have to ask "is this caller a member
// of THIS row's project" once per row, turning one public listing into N
// authorization questions about N private projects — which is itself the
// disclosure docs/23 §5 forbids, whatever the answers are. So the list
// renders no private project's identity to anyone, and the row's
// origin_project is null for those.
//
// Withheld is rendered as null and never as an empty object, and the row
// carries nothing else about the project — no slug, no id, no
// visibility — so a reader cannot tell a withheld project from an absent
// one, and no count of withheld rows is rendered either.

// BrowseRowState is one row of the browse read: the asset's public state
// as a store resolved it. Like PageState it is RAW — it carries the
// project's own visibility, and BuildBrowse decides what may be printed.
type BrowseRowState struct {
	PID  PID
	Type Type
	// Title and Slug are the asset's display metadata.
	Title string
	Slug  string
	// OriginProjectID, OriginProjectName, OriginProjectSlug and
	// OriginProjectVisibility are the originating project, empty when the
	// store could not resolve it. An EMPTY visibility is not public (the
	// fail-closed reading), so a row whose project could not be resolved
	// renders no project.
	OriginProjectID         string
	OriginProjectName       string
	OriginProjectSlug       string
	OriginProjectVisibility Visibility
	// PublicVersions is the number of versions of this asset whose stored
	// visibility is public — the count of the versions a reader of this
	// list can go and see. It is at least 1 for every row (a row exists
	// only when one does).
	PublicVersions int
	// LatestVersion is the newest public version's label and
	// LatestPublishedAt when it was published. The order is the browse
	// order: the list is newest first, so the reader sees the network's
	// most recent publications on top.
	LatestVersion     string
	LatestPublishedAt time.Time
}

// BrowseProject is the project identity a row may print. It is the same
// shape the asset page uses (PageProject) — one project identity, one
// rendering.
type BrowseProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	Visibility string `json:"visibility"`
}

// BrowseItem is one asset in the browse list.
type BrowseItem struct {
	PID  PID  `json:"pid"`
	Type Type `json:"type"`
	// Title and Slug are the asset's display metadata (docs/11 §4).
	Title string `json:"title"`
	Slug  string `json:"slug"`
	// URL is the asset's persistent web URL (/assets/{pid}).
	URL string `json:"url"`
	// OriginProject is nil when the project may not be named — see the
	// file comment.
	OriginProject *BrowseProject `json:"origin_project"`
	// PublicVersions is how many versions of this asset are public.
	PublicVersions int `json:"public_versions"`
	// LatestVersion is the newest public version's label and URL — the
	// version a reader lands on from this list.
	LatestVersion string `json:"latest_version"`
	LatestURL     string `json:"latest_url"`
	// LatestPublishedAt is when that version was published.
	LatestPublishedAt time.Time `json:"latest_published_at"`
}

// BrowseList is the whole answer: the filter that was applied and the rows
// it selected.
type BrowseList struct {
	// Type is the type filter that produced this list, nil when the list is
	// unfiltered (the caller asked for every type, which is the default —
	// an absent filter is not a filter).
	Type *Type `json:"type"`
	// Types is the closed V1 type set, in declaration order: the values the
	// filter accepts, so a client's filter control is built from the
	// platform's set rather than a copy of it.
	Types []Type `json:"types"`
	// Assets are the matching assets, newest publication first, ties broken
	// by pid ascending so two reads of one state render the same list.
	Assets []BrowseItem `json:"assets"`
}

// BuildBrowse renders the browse list from the rows a store resolved.
//
// filter is the requested type, nil for unfiltered; a row whose type
// differs from the filter is dropped here rather than in the store, so the
// rule the caller is subject to ("which type is this row?") is the same
// one the model states, and a store that ignored the filter cannot make
// this list wrong.
func BuildBrowse(rows []BrowseRowState, filter *Type) BrowseList {
	out := BrowseList{Type: filter, Types: AllTypes(), Assets: []BrowseItem{}}
	for _, row := range rows {
		if filter != nil && row.Type != *filter {
			continue
		}
		if row.PublicVersions < 1 {
			// A row that claims no public version is not one this list may
			// render: the list is the network's index of what it can open,
			// and a row saying "0 public versions" beside an asset it names
			// is either a reader defect or a version count the reader could
			// not resolve. Dropped, the fail-closed way — the exclusion is
			// applied in the query as well (ListAssetBrowse), so this guard
			// costs a reader bug its symptom rather than its effect.
			continue
		}
		item := BrowseItem{
			PID:               row.PID,
			Type:              row.Type,
			Title:             row.Title,
			Slug:              row.Slug,
			URL:               AssetURL(row.PID),
			PublicVersions:    row.PublicVersions,
			LatestVersion:     row.LatestVersion,
			LatestURL:         AssetVersionURL(row.PID, row.LatestVersion),
			LatestPublishedAt: row.LatestPublishedAt.UTC(),
		}
		if row.OriginProjectVisibility == VisibilityPublic && row.OriginProjectID != "" {
			item.OriginProject = &BrowseProject{
				ID:         row.OriginProjectID,
				Name:       row.OriginProjectName,
				Slug:       row.OriginProjectSlug,
				Visibility: string(row.OriginProjectVisibility),
			}
		}
		out.Assets = append(out.Assets, item)
	}
	sort.SliceStable(out.Assets, func(i, j int) bool {
		a, b := out.Assets[i], out.Assets[j]
		if !a.LatestPublishedAt.Equal(b.LatestPublishedAt) {
			return a.LatestPublishedAt.After(b.LatestPublishedAt)
		}
		return a.PID < b.PID
	})
	return out
}

// ParseBrowseFilter reads the browse read's type filter: an absent value is
// no filter, values are validated against the closed V1 set, and anything
// else is reported false rather than silently ignored.
//
// The refusal matters: an unrecognised type that quietly returned an empty
// list would tell a client "there are no such assets", which is a claim
// this platform cannot make about a type it does not have. The transport
// answers 400 for it (cmd/api/assetshttp), the way
// provenance.ParseWalkDirection's false is answered there.
func ParseBrowseFilter(raw string) (*Type, bool) {
	if raw == "" {
		return nil, true
	}
	t, ok := ParseType(raw)
	if !ok {
		return nil, false
	}
	return &t, true
}
