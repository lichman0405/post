package persistence

import (
	"context"
	"fmt"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// Origin ref resolution, shared by the two readers that need it: the
// publication impact preview (AssetStateStore.ResolvePreviewState) and the
// asset page (AssetPageStore.LoadAssetPage).
//
// It lives at package level rather than as a method on one store because
// the question is the same question — "what does this canonical ref name,
// and how visible is it now?" — and the two callers must not be able to
// disagree about it. A second copy would be a second answer to one
// question, which is exactly the drift the canonical query directory
// (internal/persistence/queries/asset_preview.sql) exists to prevent; the
// SQL was already shared, and T0709 made the resolution around it shared
// too when the asset page needed it for the version's origin refs.

// resolveRefs resolves every canonical origin ref in refs, with the
// visibility of the project each one names.
//
// The result has one entry per canonical ref — whether or not it resolved —
// keyed by the canonical form, which is what a caller indexes by. A ref that
// is NOT canonical (an unknown kind, or a value that is not a uuid) gets no
// entry: there is no entity it could name. Both callers report that
// differently and neither needs it here — the preview reports the shape
// refusal from its gate, and the page omits the ref — so this function
// simply does not answer for it.
//
// The four kinds are looked up in a fixed order rather than by iterating a
// map, so the reads issued (and therefore what a reader sees in a query log)
// are the same for the same input.
func resolveRefs(ctx context.Context, q *sqlc.Queries, refs []string) (map[assets.OriginRef]assets.StoredRef, error) {
	byRef := map[assets.OriginRef]assets.StoredRef{}
	for _, kind := range []assets.OriginKind{assets.KindProject, assets.KindRelease, assets.KindState, assets.KindObjectVersion} {
		ofKind := canonicalRefsOfKind(refs, kind)
		if len(ofKind) == 0 {
			continue
		}
		ids := textUUIDs(ofKind)
		switch kind {
		case assets.KindProject:
			rows, err := q.ListPreviewProjectRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("persistence: resolve project refs: %w", err)
			}
			for _, row := range rows {
				ref, _ := assets.NewOriginRef(kind, row.EntityID)
				byRef[ref] = assets.StoredRef{
					Ref:               ref,
					Resolved:          true,
					ProjectID:         row.EntityID,
					ProjectVisibility: assets.Visibility(row.Visibility),
				}
			}
		case assets.KindRelease:
			rows, err := q.ListPreviewReleaseRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("persistence: resolve release refs: %w", err)
			}
			for _, row := range rows {
				byRef[refOf(kind, row.EntityID)] = projectRef(kind, row.EntityID, row.ProjectID, row.ProjectVisibility)
			}
		case assets.KindState:
			rows, err := q.ListPreviewStateRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("persistence: resolve state refs: %w", err)
			}
			for _, row := range rows {
				byRef[refOf(kind, row.EntityID)] = projectRef(kind, row.EntityID, row.ProjectID, row.ProjectVisibility)
			}
		case assets.KindObjectVersion:
			rows, err := q.ListPreviewObjectVersionRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("persistence: resolve object version refs: %w", err)
			}
			for _, row := range rows {
				ref := refOf(kind, row.EntityID)
				st := projectRef(kind, row.EntityID, row.ProjectID, row.ProjectVisibility)
				st.Object = &assets.StoredObject{
					ObjectVersionID: row.EntityID,
					ObjectID:        row.ObjectID,
					Title:           row.Title,
				}
				byRef[ref] = st
			}
		}
	}
	return byRef, nil
}

// resolveRefsInOrder is resolveRefs with the answered refs returned in the
// input's own order, one entry each, refs the reader did not answer for
// included with Resolved false. It is the preview's shape (CurrentState.Refs
// is a list the preview walks in order).
func resolveRefsInOrder(ctx context.Context, q *sqlc.Queries, refs []string) ([]assets.StoredRef, error) {
	byRef, err := resolveRefs(ctx, q, refs)
	if err != nil {
		return nil, err
	}
	out := make([]assets.StoredRef, 0, len(refs))
	for _, raw := range refs {
		ref := assets.OriginRef(raw)
		if !ref.Valid() {
			continue
		}
		st, ok := byRef[ref]
		if !ok {
			st = assets.StoredRef{Ref: ref}
		}
		out = append(out, st)
	}
	return out, nil
}

// canonicalRefsOfKind returns the VALUES of every canonical ref of one
// kind, without repeats — the input a uuid[] lookup wants. A ref that is
// not canonical, or is of another kind, is skipped: this function is called
// once per kind, over the whole list.
func canonicalRefsOfKind(refs []string, kind assets.OriginKind) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range refs {
		gotKind, value, ok := assets.ParseOriginRef(raw)
		if !ok || gotKind != kind || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// refOf rebuilds the canonical ref of one resolved entity. Every value the
// queries return came from a ref the caller already validated, so the
// rebuild cannot fail (a value that did not parse would not have been
// looked up); the empty ref on a hypothetical failure keeps a lookup from
// aliasing another entry rather than silently resolving something.
func refOf(kind assets.OriginKind, value string) assets.OriginRef {
	ref, _ := assets.NewOriginRef(kind, value)
	return ref
}

// projectRef renders one entity of a project as the state's answer for its
// ref: it resolved, and its visibility is the visibility of the project it
// belongs to (docs/09 §1: every V1 origin entity belongs to exactly one
// project).
func projectRef(kind assets.OriginKind, entityID, projectID, visibility string) assets.StoredRef {
	return assets.StoredRef{
		Ref:               refOf(kind, entityID),
		Resolved:          true,
		ProjectID:         projectID,
		ProjectVisibility: assets.Visibility(visibility),
	}
}
