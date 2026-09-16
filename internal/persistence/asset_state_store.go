package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
	"github.com/lichman0405/post/internal/rights"
)

// AssetStateStore resolves the things a publish candidate names against
// the repository's current state, with the canonical queries of
// internal/persistence/queries/asset_preview.sql (generated code under
// internal/persistence/sqlc).
//
// It answers ONE question — "what does this candidate name, and how
// visible is it right now?" — for the two callers that must agree about
// it: the read-only publication impact preview (POST
// /api/v1/projects/{projectId}/assets:publish-preview, T0704) and the
// publish command's own server-side re-run of that preview inside its
// write transaction (T0705, docs/22 §7: a command never trusts a
// precheck). One resolver, one answer: a preview a human approved and a
// preview the publish acts on cannot disagree about what exists.
//
// It moved here from cmd/api/assetshttp in T0705. It lived with the
// transport because internal/persistence was outside T0704's scope and
// the ONLY caller was the preview route; the publish command needs the
// same resolution over a pgx.Tx, and a second copy of it in the store
// would be two definitions of "does this pin resolve" — exactly the drift
// the canonical query directory exists to prevent. The SQL did not move:
// it was always here.
//
// Every method is a READ. Nothing in this file writes a row or takes a
// lock. That guarantee is what the preview route's integration suite
// proves rather than trusts: it runs the whole route over a session whose
// connections carry default_transaction_read_only=on, where a write is
// refused by the server with SQLSTATE 25006.
type AssetStateStore struct {
	queries *sqlc.Queries
}

// NewAssetStateStore wires the resolver over any sqlc executor — the
// production preview value is the pgx pool cmd/api already builds, and the
// production publish value is the transaction the publish opened.
func NewAssetStateStore(db sqlc.DBTX) *AssetStateStore {
	return &AssetStateStore{queries: sqlc.New(db)}
}

// ResolvePreviewState implements the preview's StateReader.
//
// The resolutions are independent reads, in a fixed order (asset, then
// refs, then the manifest's pins and blobs): the preview's answer must be a
// function of the state, and a fixed read order is part of that being
// checkable rather than merely intended. The PREVIEW call is deliberately
// not transactional (the state is read over a pool, one statement at a
// time): the preview is a statement about the repository as it was read,
// and a snapshot taken at one instant would not make it a better statement
// — the publish it previews re-checks everything it acts on inside its own
// transaction (T0705), which is where the atomicity belongs.
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
// What the transactional caller gets for free: every statement below runs
// on the executor it was built over, so a publish that passes this
// resolver into its transaction reads the state its own writes will land
// in, under the project row lock it already holds — no interleaving write
// can slip between the re-check and the insert.
//
// A read failure fails the whole preview. There is no partial answer to
// give: a preview missing the pins would report "no private dependency
// here" for a repository nobody finished looking at, which is exactly the
// report this resolver exists to not produce. A document that cannot be
// parsed is a different thing entirely — it is a finding about the
// CANDIDATE, and it is reported as one.
func (s *AssetStateStore) ResolvePreviewState(ctx context.Context, candidate assets.PublishCandidate) (assets.CurrentState, error) {
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
func (s *AssetStateStore) assetByPID(ctx context.Context, pid assets.PID) (*assets.StoredAsset, error) {
	row, err := s.queries.GetPreviewAssetByPID(ctx, string(pid))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve asset %q: %w", pid, err)
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
// (docs/12 §2's preset), through the SAME canonical query the project:
// origin refs are resolved with — ListPreviewProjectRefs, an id lookup on
// projects. No new query: what is asked is the identical question about the
// identical row, and a second query for it would be a second answer to one
// question.
//
// Two absences are answers rather than failures, and both are the
// fail-closed one ("" = not public, so the preview withholds the project's
// identity):
//
//   - an id that is not uuid text names no row, so there is nothing to look
//     up. research_assets.origin_project_id is a uuid column, so this cannot
//     happen for a stored asset; the branch exists so that a hypothetical
//     one is answered rather than sent to the database.
//   - no row for a uuid. The column is NOT NULL and a foreign key, so the
//     project exists and this cannot happen either — and it is still not an
//     error here: the asset row is a fact about the asset, and a preview
//     that failed over it would report a repository it could not read.
//     Answering "no visibility" withholds the identity, which is the safe
//     direction.
func (s *AssetStateStore) projectVisibility(ctx context.Context, projectID string) (assets.Visibility, error) {
	ids := textUUIDs([]string{projectID})
	if len(ids) == 0 {
		return "", nil
	}
	rows, err := s.queries.ListPreviewProjectRefs(ctx, ids)
	if err != nil {
		return "", fmt.Errorf("persistence: resolve the visibility of project %q: %w", projectID, err)
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
func (s *AssetStateStore) pins(ctx context.Context, pins []assets.DependencyPin) ([]assets.StoredPin, error) {
	if len(pins) == 0 {
		return nil, nil
	}
	text := make([]string, 0, len(pins))
	for _, pin := range pins {
		text = append(text, string(pin))
	}
	rows, err := s.queries.ListPreviewPins(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve dependency pins: %w", err)
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

// refs resolves every canonical origin ref the candidate declares, in the
// candidate's own order, with one entry per canonical ref whether or not the
// ref resolved — CurrentState's contract. The resolution itself lives in
// asset_refs.go, where the asset page (T0709) reads the same question
// through the same queries; this method is the preview's shape of it.
func (s *AssetStateStore) refs(ctx context.Context, refs []string) ([]assets.StoredRef, error) {
	return resolveRefsInOrder(ctx, s.queries, refs)
}

// blobs resolves the blob ids a manifest declares. Absent ids are not rows
// in the result (the preview reports them as unresolved with an unknown
// access); a resolved id carries whether it is openly attached anywhere.
func (s *AssetStateStore) blobs(ctx context.Context, ids []string) ([]assets.StoredBlob, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.queries.ListPreviewBlobs(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("persistence: resolve blobs: %w", err)
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
