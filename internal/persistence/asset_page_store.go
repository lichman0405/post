package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AssetPageStore answers the research asset page's and the asset hub's
// reads, with the canonical queries of
// internal/persistence/queries/asset_page.sql (generated code under
// internal/persistence/sqlc).
//
// It answers ONE question — "what does this pid's asset look like, row by
// row?" — and it answers it RAW: the private rows come back with the public
// ones, and the decision about what may be rendered belongs to
// internal/assets (BuildPage, BuildBrowse). See the header of the SQL file
// for why the visibility filter is not applied here; the short version is
// that the rule has unit tests where it lives and none inside a query.
//
// Every method is a READ. Nothing in this file writes a row or takes a
// lock, and the read-only guarantee is checked rather than asserted: the
// asset page's integration suite (T0709) runs the whole route over a
// session whose connections carry default_transaction_read_only=on, where
// the server refuses a write with SQLSTATE 25006.
type AssetPageStore struct {
	queries *sqlc.Queries
}

// NewAssetPageStore wires the reader over any sqlc executor — the production
// value is the pgx pool cmd/api already builds.
func NewAssetPageStore(db sqlc.DBTX) *AssetPageStore {
	return &AssetPageStore{queries: sqlc.New(db)}
}

// LoadAssetPage resolves one asset page's whole state.
//
// ok is false when the pid names no stored asset, which is a state rather
// than an error: the transport answers it the same existence-hiding 404 an
// unknown pid gets (docs/45), so a client cannot use the page as an oracle
// for which pids exist.
//
// The reads run in a fixed order (asset, versions, pins, refs, lineage,
// usages, events, parties, users, organizations) for two reasons. The
// version-aware reads cover EVERY version rather than one: the page renders
// ONE version — the newest the caller may see — and which one that is is
// the model's decision, so a reader that resolved only the newest version's
// pins, refs or credits would silently starve the model of the state it
// needs for any other choice. The identities come LAST, both of them,
// because the sets of ids to resolve are discovered while reading the
// versions, the events and the credits.
//
// A read failure fails the whole page. There is no partial answer to give:
// a page missing the usages would report "nobody uses this" for a repository
// nobody finished looking at, which is the report this reader exists not to
// produce. A version row whose manifest does not parse is a different thing
// — it costs the page that version's pins and metadata, both of which are
// genuinely unknowable without the document, and neither of which is a claim
// about the state the reader failed to read.
func (s *AssetPageStore) LoadAssetPage(ctx context.Context, pid assets.PID) (assets.PageState, bool, error) {
	row, err := s.queries.GetAssetPageAsset(ctx, string(pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return assets.PageState{}, false, nil
	}
	if err != nil {
		return assets.PageState{}, false, fmt.Errorf("persistence: resolve asset page %q: %w", pid, err)
	}
	page := assets.PageState{
		Asset: assets.PageAssetState{
			ID:              row.ID,
			PID:             assets.PID(row.Pid),
			Type:            assets.Type(row.AssetType),
			Title:           row.Title,
			Slug:            row.Slug,
			OriginProjectID: row.OriginProjectID,
			CreatedAt:       row.CreatedAt.Time,
		},
		Project: assets.PageProjectState{
			ID:         row.OriginProjectID,
			Name:       row.ProjectName,
			Slug:       row.ProjectSlug,
			Visibility: assets.Visibility(row.ProjectVisibility),
		},
		Users:         map[string]assets.PageUser{},
		Parties:       map[string][]assets.PagePartyState{},
		Organizations: map[string]assets.PageOrganization{},
		Pins:          map[assets.DependencyPin]assets.PagePinState{},
		Refs:          map[assets.OriginRef]assets.StoredRef{},
	}

	versions, err := s.queries.ListAssetPageVersions(ctx, string(pid))
	if err != nil {
		return assets.PageState{}, false, fmt.Errorf("persistence: read asset page versions %q: %w", pid, err)
	}
	page.Versions = make([]assets.PageVersionState, 0, len(versions))
	for _, v := range versions {
		page.Versions = append(page.Versions, assets.PageVersionState{
			ID:            v.ID,
			Version:       v.Version,
			Visibility:    assets.Visibility(v.Visibility),
			IntegrityHash: v.IntegrityHash,
			PublishedBy:   v.PublishedBy,
			PublishedAt:   v.PublishedAt.Time,
			Manifest:      v.Manifest,
			RightsJSON:    v.RightsJson,
			OriginRefs:    v.OriginRefs,
		})
	}

	// The pins and the refs of EVERY version, unioned: the model renders one
	// version and picks it itself, so resolving only one would make the
	// reader's answer depend on the model's choice.
	if page.Pins, err = s.pins(ctx, page.Versions); err != nil {
		return assets.PageState{}, false, err
	}
	if page.Refs, err = s.refs(ctx, page.Versions); err != nil {
		return assets.PageState{}, false, err
	}

	if page.Lineage, err = s.lineage(ctx, string(pid)); err != nil {
		return assets.PageState{}, false, err
	}
	if page.Usages, err = s.usages(ctx, string(pid)); err != nil {
		return assets.PageState{}, false, err
	}
	if page.Events, err = s.events(ctx, string(pid)); err != nil {
		return assets.PageState{}, false, err
	}
	// The credited parties come before the identities for the same reason
	// the pins do: the set of user ids to resolve is discovered while
	// reading them, and the credits' users are resolved through the same
	// lookup as the publishers'.
	if page.Parties, err = s.parties(ctx, string(pid)); err != nil {
		return assets.PageState{}, false, err
	}
	if page.Users, err = s.users(ctx, page.Versions, page.Events, page.Parties); err != nil {
		return assets.PageState{}, false, err
	}
	if page.Organizations, err = s.organizations(ctx, page.Parties); err != nil {
		return assets.PageState{}, false, err
	}
	return page, true, nil
}

// ListBrowseAssets resolves the asset hub's browse list (docs/11 §2): one
// row per asset with at least one public version, newest publication first.
//
// A nil filter is "every type"; the transport has already refused anything
// outside the four-type set, so a filter here is always one of them. The
// query takes the filter as a one-element array — the shape the other
// optional filters in this package use — so this method builds it.
func (s *AssetPageStore) ListBrowseAssets(ctx context.Context, filter *assets.Type) ([]assets.BrowseRowState, error) {
	var types []string
	if filter != nil {
		types = []string{string(*filter)}
	}
	rows, err := s.queries.ListAssetBrowse(ctx, types)
	if err != nil {
		return nil, fmt.Errorf("persistence: list browse assets: %w", err)
	}
	out := make([]assets.BrowseRowState, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.BrowseRowState{
			PID:                     assets.PID(row.Pid),
			Type:                    assets.Type(row.AssetType),
			Title:                   row.Title,
			Slug:                    row.Slug,
			OriginProjectID:         row.OriginProjectID,
			OriginProjectName:       row.ProjectName,
			OriginProjectSlug:       row.ProjectSlug,
			OriginProjectVisibility: assets.Visibility(row.ProjectVisibility),
			PublicVersions:          int(row.PublicVersions),
			LatestVersion:           row.LatestVersion,
			LatestPublishedAt:       row.LatestPublishedAt.Time,
		})
	}
	return out, nil
}

// pins resolves the dependency pins every version's manifest declares. The
// manifest is read with the same parser the publish gate uses
// (assets.ParseManifest), so a manifest this reader can read is exactly a
// manifest the gate would have published; one it cannot read contributes no
// pins, which is the truth about that document rather than a gap.
//
// The pin's identity is split back out of the canonical string
// (ParseDependencyPin) instead of being carried beside it, so the pid and the
// version the page renders cannot disagree with the pin it renders next to
// them.
func (s *AssetPageStore) pins(ctx context.Context, versions []assets.PageVersionState) (map[assets.DependencyPin]assets.PagePinState, error) {
	wanted := map[assets.DependencyPin]bool{}
	var list []string
	for _, v := range versions {
		manifest, err := assets.ParseManifest(v.Manifest)
		if err != nil {
			continue
		}
		for _, pin := range manifest.DependencyPins {
			if wanted[pin] {
				continue
			}
			wanted[pin] = true
			list = append(list, string(pin))
		}
	}
	out := make(map[assets.DependencyPin]assets.PagePinState, len(list))
	if len(list) == 0 {
		return out, nil
	}
	rows, err := s.queries.ListAssetPagePins(ctx, list)
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve asset page pins: %w", err)
	}
	for _, row := range rows {
		pin := assets.DependencyPin(row.Pin)
		pid, version, ok := assets.ParseDependencyPin(row.Pin)
		if !ok {
			// A stored pin that is not canonical cannot be resolved to an
			// asset version. The row is skipped rather than answered with
			// half an identity; the model reports the pin as unresolved.
			continue
		}
		out[pin] = assets.PagePinState{
			Pin:               pin,
			Resolved:          true,
			Visibility:        assets.Visibility(row.Visibility),
			ProjectVisibility: assets.Visibility(row.ProjectVisibility),
			Title:             row.Title,
			Type:              assets.Type(row.AssetType),
			PID:               pid,
			Version:           version,
		}
	}
	return out, nil
}

// refs resolves the origin refs of every version, unioned. It is the shared
// resolution (asset_refs.go) rather than a second implementation: the
// preview resolves the same question through the same queries, and two
// answers to one question is the drift the canonical query directory exists
// to prevent.
func (s *AssetPageStore) refs(ctx context.Context, versions []assets.PageVersionState) (map[assets.OriginRef]assets.StoredRef, error) {
	var list []string
	for _, v := range versions {
		list = append(list, v.OriginRefs...)
	}
	out, err := resolveRefs(ctx, s.queries, list)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// lineage resolves the fork/derive edges that touch any version of the
// asset, from either end.
func (s *AssetPageStore) lineage(ctx context.Context, pid string) ([]assets.PageLineageState, error) {
	rows, err := s.queries.ListAssetPageLineage(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("persistence: read asset page lineage %q: %w", pid, err)
	}
	out := make([]assets.PageLineageState, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.PageLineageState{
			Relation: row.RelationType,
			Parent: assets.PageLineageEnd{
				VersionID:         row.ParentVersionID,
				PID:               assets.PID(row.ParentPid),
				Version:           row.ParentVersion,
				Visibility:        assets.Visibility(row.ParentVisibility),
				ProjectVisibility: assets.Visibility(row.ParentProjectVisibility),
				Title:             row.ParentTitle,
			},
			Child: assets.PageLineageEnd{
				VersionID:         row.ChildVersionID,
				PID:               assets.PID(row.ChildPid),
				Version:           row.ChildVersion,
				Visibility:        assets.Visibility(row.ChildVisibility),
				ProjectVisibility: assets.Visibility(row.ChildProjectVisibility),
				Title:             row.ChildTitle,
			},
		})
	}
	return out, nil
}

// usages resolves the recorded usages of any version of the asset, oldest
// first.
func (s *AssetPageStore) usages(ctx context.Context, pid string) ([]assets.PageUsageState, error) {
	rows, err := s.queries.ListAssetPageUsages(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("persistence: read asset page usages %q: %w", pid, err)
	}
	out := make([]assets.PageUsageState, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.PageUsageState{
			AssetVersionID:    row.AssetVersionID,
			ProjectID:         row.ProjectID,
			ProjectName:       row.ProjectName,
			ProjectSlug:       row.ProjectSlug,
			ProjectVisibility: assets.Visibility(row.ProjectVisibility),
			VisibilityOfUsage: assets.Visibility(row.VisibilityOfUsage),
			DependencyType:    row.DependencyType,
			CreatedAt:         row.CreatedAt.Time,
		})
	}
	return out, nil
}

// events resolves the research events recorded about the asset, newest
// first. The event id is not carried: the page renders what happened, when
// and by whom, and an internal event id is not one of the eleven items.
func (s *AssetPageStore) events(ctx context.Context, pid string) ([]assets.PageEventState, error) {
	rows, err := s.queries.ListAssetPageEvents(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("persistence: read asset page events %q: %w", pid, err)
	}
	out := make([]assets.PageEventState, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.PageEventState{
			Type:         row.EventType,
			Visibility:   assets.Visibility(row.Visibility),
			ActorID:      row.ActorID,
			AssetVersion: row.Version,
			OccurredAt:   row.OccurredAt.Time,
		})
	}
	return out, nil
}

// parties resolves the credited parties of every version of the asset
// (asset_version_parties), filed under the version ROW id they belong to.
//
// The rows come back exactly as stored — role, kind and id — and the kind
// is not interpreted here: which table an id names is the model's question,
// and it is answered there by resolving the id against the map the kind
// selects (assets.creatorsOf). A reader that resolved the kind itself would
// put a second copy of the identity vocabulary in the adapter.
func (s *AssetPageStore) parties(ctx context.Context, pid string) (map[string][]assets.PagePartyState, error) {
	rows, err := s.queries.ListAssetPageParties(ctx, pid)
	if err != nil {
		return nil, fmt.Errorf("persistence: read asset page parties %q: %w", pid, err)
	}
	out := make(map[string][]assets.PagePartyState, len(rows))
	for _, row := range rows {
		out[row.AssetVersionID] = append(out[row.AssetVersionID], assets.PagePartyState{
			Role:    row.Role,
			Kind:    row.PartyKind,
			PartyID: row.PartyID,
		})
	}
	return out, nil
}

// organizations resolves the identities behind the ORGANIZATION-kind
// credited parties, in one lookup — the sibling of users() for the second
// identity table (00002). An id with no row is simply absent, and the model
// renders no entry for that party rather than an id-only identity.
//
// The ids of user-kind parties are not sent here and the ids of
// organization-kind parties are not sent to users(): an id is only an id
// against the table its kind names, which is why the two lookups are two
// lookups.
func (s *AssetPageStore) organizations(ctx context.Context, parties map[string][]assets.PagePartyState) (map[string]assets.PageOrganization, error) {
	seen := map[string]bool{}
	var ids []string
	for _, list := range parties {
		for _, p := range list {
			if p.Kind != string(domain.PartyOrganization) || p.PartyID == "" || seen[p.PartyID] {
				continue
			}
			seen[p.PartyID] = true
			ids = append(ids, p.PartyID)
		}
	}
	out := make(map[string]assets.PageOrganization, len(ids))
	uuids := textUUIDs(ids)
	if len(uuids) == 0 {
		return out, nil
	}
	rows, err := s.queries.ListAssetPageOrganizations(ctx, uuids)
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve asset page organizations: %w", err)
	}
	for _, row := range rows {
		out[row.ID] = assets.PageOrganization{ID: row.ID, Slug: row.Slug, Name: row.Name}
	}
	return out, nil
}

// users resolves the identities the page renders: every version's publisher,
// every event's actor and every USER-kind credited party, in one lookup. An
// id with no row is simply absent, and the model renders nil rather than an
// id-only identity.
func (s *AssetPageStore) users(ctx context.Context, versions []assets.PageVersionState, events []assets.PageEventState, parties map[string][]assets.PagePartyState) (map[string]assets.PageUser, error) {
	seen := map[string]bool{}
	var ids []string
	for _, v := range versions {
		if v.PublishedBy != "" && !seen[v.PublishedBy] {
			seen[v.PublishedBy] = true
			ids = append(ids, v.PublishedBy)
		}
	}
	for _, e := range events {
		if e.ActorID != "" && !seen[e.ActorID] {
			seen[e.ActorID] = true
			ids = append(ids, e.ActorID)
		}
	}
	for _, list := range parties {
		for _, p := range list {
			if p.Kind != string(domain.PartyUser) || p.PartyID == "" || seen[p.PartyID] {
				continue
			}
			seen[p.PartyID] = true
			ids = append(ids, p.PartyID)
		}
	}
	out := make(map[string]assets.PageUser, len(ids))
	uuids := textUUIDs(ids)
	if len(uuids) == 0 {
		return out, nil
	}
	rows, err := s.queries.ListAssetPageUsers(ctx, uuids)
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve asset page users: %w", err)
	}
	for _, row := range rows {
		out[row.ID] = assets.PageUser{UserID: row.ID, Handle: row.Handle, DisplayName: row.DisplayName}
	}
	return out, nil
}
