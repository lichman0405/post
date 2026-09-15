package assetshttp

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rights"
)

// PostgresStateStore is the production StateReader: it resolves the things
// a candidate names against the repository's current state, with the
// canonical queries of internal/persistence/queries/asset_preview.sql
// (generated code under internal/persistence/sqlc).
//
// It lives in this package because internal/persistence's root package
// files are outside T0704's allowed_scope — the adapter travels with the
// transport, the way cmd/api/provenancehttp's projection store does (T0505;
// L1, recorded in both task results). The QUERIES do not travel: they are
// in the canonical query directory, so the drift check covers them like
// every other query in the tree.
//
// Every method here is a read. Nothing in this file writes a row, opens a
// transaction, or takes a lock; the package doc says what the integration
// suite does to prove that rather than trusting it.
type PostgresStateStore struct {
	queries *sqlc.Queries
}

// NewPostgresStateStore wires the adapter over any sqlc executor (the
// production value is the pgx pool cmd/api already builds).
func NewPostgresStateStore(db sqlc.DBTX) *PostgresStateStore {
	return &PostgresStateStore{queries: sqlc.New(db)}
}

// ResolvePreviewState implements StateReader.
//
// The resolutions are independent reads, in a fixed order (asset, then
// refs, then the manifest's pins and blobs): the preview's answer must be a
// function of the state, and a fixed read order is part of that being
// checkable rather than merely intended. Nothing here is transactional, and
// deliberately so: the preview is a statement about the repository as it
// was read, and a snapshot taken at one instant would not make it a better
// statement — the publish it previews will re-check everything it acts on
// inside its own transaction (T0705).
//
// The refs are resolved BEFORE the manifest is read, and independently of
// it, because they do not come from it: candidate.OriginRefs is the
// candidate's own field. A manifest this reader cannot parse therefore
// costs the preview the pins and the blobs — which really are declared in
// the manifest and are genuinely unknowable without it — and nothing else.
// The candidate's declared refs are still resolved and still reported, and
// the gate's own manifest refusal appears in publish_blockers with
// publishable=false, so the preview never answers an unreadable manifest
// with a quiet "nothing to see here": it answers "this cannot be published,
// here is the refusal, and here is the impact of the parts that ARE
// readable".
//
// A read failure fails the whole preview. There is no partial answer to
// give: a preview missing the pins would report "no private dependency
// here" for a repository nobody finished looking at, which is exactly the
// report this route exists to not produce. A document that cannot be parsed
// is a different thing entirely — it is a finding about the CANDIDATE, and
// it is reported as one.
func (s *PostgresStateStore) ResolvePreviewState(ctx context.Context, candidate assets.PublishCandidate) (assets.CurrentState, error) {
	state := assets.CurrentState{}

	asset, err := s.assetByPID(ctx, candidate.AssetPID)
	if err != nil {
		return assets.CurrentState{}, err
	}
	state.Asset = asset

	state.Refs, err = s.refs(ctx, candidate.OriginRefs)
	if err != nil {
		return assets.CurrentState{}, err
	}

	// The pins and the blob ids DO come from the manifest, and it is parsed
	// here with the same parser the publish gate uses
	// (assets.ParseManifest) — so a manifest this reader can read is exactly
	// a manifest the gate reads, and a manifest it cannot read is one the
	// gate already refuses. The refusal is the gate's to report (ASSET_*);
	// this reader's answer is to resolve nothing further, which is the truth
	// about that document rather than a gap in the answer.
	manifest, err := assets.ParseManifest(candidate.Manifest)
	if err != nil {
		return state, nil
	}
	state.Pins, err = s.pins(ctx, manifest.DependencyPins)
	if err != nil {
		return assets.CurrentState{}, err
	}
	state.Blobs, err = s.blobs(ctx, assets.DeclaredBlobIDs(manifest))
	if err != nil {
		return assets.CurrentState{}, err
	}
	return state, nil
}

// assetByPID resolves the asset a version would be published under. A pid
// that names no asset is nil, not an error: "the asset does not exist" is
// a state the preview reports (PREVIEW_ASSET_UNKNOWN), and the preview
// still computes the impact of the version that would have been published
// under it.
//
// The row is answered WITH the visibility of the project it belongs to
// (projectVisibility), because the preview may not render an asset's title
// or its project for every caller who can name its pid: the pid is an
// identity a caller can hold from anywhere, and T0712 closed exactly that
// disclosure. The visibility is what decides it, and it is read here rather
// than in the model, because the model has no connection to read it with.
func (s *PostgresStateStore) assetByPID(ctx context.Context, pid assets.PID) (*assets.StoredAsset, error) {
	row, err := s.queries.GetPreviewAssetByPID(ctx, string(pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assetshttp: resolve asset %q: %w", pid, err)
	}
	visibility, err := s.projectVisibility(ctx, row.OriginProjectID)
	if err != nil {
		return nil, err
	}
	return &assets.StoredAsset{
		ID:                      row.ID,
		PID:                     assets.PID(row.Pid),
		Type:                    assets.Type(row.AssetType),
		Title:                   row.Title,
		OriginProjectID:         row.OriginProjectID,
		OriginProjectVisibility: visibility,
	}, nil
}

// projectVisibility asks the projects table for one project's visibility
// (docs/12 §2's preset), through the SAME canonical query the project: origin
// refs are resolved with — ListPreviewProjectRefs, an id lookup on projects.
// No new query: what is asked is the identical question about the identical
// row, and a second query for it would be a second answer to one question.
//
// Two absences are answers rather than failures, and both are the fail-closed
// one ("" = not public, so the preview withholds the project's identity):
//
//   - an id that is not uuid text names no row, so there is nothing to look
//     up. research_assets.origin_project_id is a uuid column, so this cannot
//     happen for a stored asset; the branch exists so that a hypothetical
//     one is answered rather than sent to the database.
//   - no row for a uuid. The column is NOT NULL and a foreign key, so the
//     project exists and this cannot happen either — and it is still not an
//     error here: the asset row is a fact about the asset, and a preview that
//     failed over it would report a repository it could not read. Answering
//     "no visibility" withholds the identity, which is the safe direction.
func (s *PostgresStateStore) projectVisibility(ctx context.Context, projectID string) (assets.Visibility, error) {
	ids := textUUIDs([]string{projectID})
	if len(ids) == 0 {
		return "", nil
	}
	rows, err := s.queries.ListPreviewProjectRefs(ctx, ids)
	if err != nil {
		return "", fmt.Errorf("assetshttp: resolve the visibility of project %q: %w", projectID, err)
	}
	if len(rows) == 0 {
		return "", nil
	}
	return assets.Visibility(rows[0].Visibility), nil
}

// pins resolves the dependency pins that name a stored version, with the
// visibility each has now. A pin that resolves to nothing is simply absent
// from the result — the preview reports the absence as an unresolved
// dependency, which is a finding about the CANDIDATE (it pins a version
// nobody can resolve) rather than an error.
func (s *PostgresStateStore) pins(ctx context.Context, pins []assets.DependencyPin) ([]assets.StoredPin, error) {
	if len(pins) == 0 {
		return nil, nil
	}
	text := make([]string, 0, len(pins))
	for _, pin := range pins {
		text = append(text, string(pin))
	}
	rows, err := s.queries.ListPreviewPins(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("assetshttp: resolve dependency pins: %w", err)
	}
	out := make([]assets.StoredPin, 0, len(rows))
	for _, row := range rows {
		out = append(out, assets.StoredPin{
			Pin:        assets.DependencyPin(row.Pin),
			Visibility: assets.Visibility(row.Visibility),
		})
	}
	return out, nil
}

// refs resolves every canonical origin ref the candidate declares.
//
// The result has one entry per canonical ref, in the candidate's own
// order, whether or not the ref resolved — CurrentState's contract. A ref
// that is NOT canonical gets no entry at all: there is no entity it could
// name, and the preview reports its shape refusal from the gate instead.
//
// The four kinds are looked up in a fixed order rather than by iterating a
// map, so the reads this method issues (and therefore what a reader sees
// in a query log) are the same for the same candidate.
func (s *PostgresStateStore) refs(ctx context.Context, refs []string) ([]assets.StoredRef, error) {
	byRef := map[assets.OriginRef]assets.StoredRef{}
	for _, kind := range []assets.OriginKind{assets.KindProject, assets.KindRelease, assets.KindState, assets.KindObjectVersion} {
		refs := canonicalRefsOfKind(refs, kind)
		if len(refs) == 0 {
			continue
		}
		ids := textUUIDs(refs)
		switch kind {
		case assets.KindProject:
			rows, err := s.queries.ListPreviewProjectRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("assetshttp: resolve project refs: %w", err)
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
			rows, err := s.queries.ListPreviewReleaseRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("assetshttp: resolve release refs: %w", err)
			}
			for _, row := range rows {
				byRef[refOf(kind, row.EntityID)] = projectRef(kind, row.EntityID, row.ProjectID, row.ProjectVisibility)
			}
		case assets.KindState:
			rows, err := s.queries.ListPreviewStateRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("assetshttp: resolve state refs: %w", err)
			}
			for _, row := range rows {
				byRef[refOf(kind, row.EntityID)] = projectRef(kind, row.EntityID, row.ProjectID, row.ProjectVisibility)
			}
		case assets.KindObjectVersion:
			rows, err := s.queries.ListPreviewObjectVersionRefs(ctx, ids)
			if err != nil {
				return nil, fmt.Errorf("assetshttp: resolve object version refs: %w", err)
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

// blobs resolves the blob ids a manifest declares. Absent ids are not rows
// in the result (the preview reports them as unresolved with an unknown
// access); a resolved id carries whether it is openly attached anywhere.
func (s *PostgresStateStore) blobs(ctx context.Context, ids []string) ([]assets.StoredBlob, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.queries.ListPreviewBlobs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("assetshttp: resolve blobs: %w", err)
	}
	out := make([]assets.StoredBlob, 0, len(rows))
	for _, row := range rows {
		// blob_attachments.access_level is the rights model's own data
		// access vocabulary (open | restricted, migration 00008), which is
		// why StoredBlob carries that type: the same two values mean the
		// same thing in the storage layer and in a rights declaration.
		access := rights.DataAccessRestricted
		if row.IsOpen {
			access = rights.DataAccessOpen
		}
		out = append(out, assets.StoredBlob{BlobID: row.BlobID, Access: access})
	}
	return out, nil
}

// canonicalRefsOfKind returns the VALUES of every canonical ref of one
// kind, without repeats — the input a uuid[] lookup wants. A ref that is
// not canonical, or is of another kind, is skipped: this method is called
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

// textUUIDs converts the id texts of a lookup into the pgtype form the
// generated queries take. Every caller passes values that came from
// ParseOriginRef, i.e. canonical uuid text; a value that somehow did not
// convert is dropped rather than sent as a zero uuid, which would answer
// for a different entity than the one asked about.
func textUUIDs(ids []string) []pgtype.UUID {
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		var u pgtype.UUID
		if err := u.Scan(id); err != nil {
			continue
		}
		out = append(out, u)
	}
	return out
}
