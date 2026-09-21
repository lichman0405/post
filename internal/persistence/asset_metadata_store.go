package persistence

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/application/assetmetadata"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/persistence/sqlc"
)

// AssetMetadataStore is the production adapter for the asset metadata
// revision command (T0706): the one transaction that revises an asset's
// metadata and appends the audit row recording it.
//
// # One transaction, and what it is
//
//	lock the project        →  the project the authorization was resolved
//	                           against must exist (GetProjectByIDForUpdate)
//	lock the asset by pid   →  serializes every write to this asset
//	                           (GetResearchAssetByPIDForUpdate)
//	verify the asset's origin project is the one the caller named
//	read the current values →  the BEFORE half of the audit row
//	update the six columns  →  the revision itself
//	append the audit row    →  with the before/after pair read above
//
// The two locks are taken in the same order assetrights.ChangeHolder takes
// them, so a metadata revision and a rights-holder transfer of one asset
// cannot deadlock against each other. The asset lock is what makes the
// before values the truth: a concurrent revision waits, reads this one's
// values as its own before, and writes its own — nothing here reads a row
// it does not hold.
//
// # Why the before values are read HERE and not by the command
//
// projects.UpdateSettings fills its audit summaries from a read the
// command made before the store ran (settings.go:151-161). That is safe
// for a project row because nothing else writes those two columns; it is
// not safe here, where the columns being recorded as "before" are the very
// ones the transaction is about to overwrite. Reading them under the same
// lock that writes them makes the audit row's before a value no other
// writer could have moved in between — the window the same-transaction
// rule exists to close, closed on the value as well as on the row.
//
// The row still commits with its audit or not at all: appendAudit runs
// inside this transaction, and a failed audit write fails the revision
// (audit_store.go's fail-closed rule).
//
// # What the write does not touch
//
// Nothing outside research_assets' six metadata columns and audit_log. In
// particular no research_asset_versions row — not an insert, not an
// update, not a read — because docs/11 §4's revision 不产生新的 scientific
// version. That is also why the table's own guard is never worked around:
// research_asset_versions carries the 00014 append-only trigger and
// research_assets does not, so the in-place update is legal exactly where
// it must be legal. And no pid, no asset_type and no origin_project_id:
// those are the asset's identity, docs/11 §2 makes the pid stable, and
// 00064's note is explicit that a slug rename leaves the pid — and
// therefore /assets/{pid} — untouched.
type AssetMetadataStore struct {
	pool *pgxpool.Pool
}

// NewAssetMetadataStore builds the store on pool. The pool may be lazy
// (OpenLazy): the API keeps starting while PostgreSQL is down.
func NewAssetMetadataStore(pool *pgxpool.Pool) *AssetMetadataStore {
	return &AssetMetadataStore{pool: pool}
}

// ReviseMetadata implements assetmetadata.StorePort. See the type doc for
// the transaction it runs; this method is the transaction.
func (s *AssetMetadataStore) ReviseMetadata(ctx context.Context, req assetmetadata.RevisionRequest) (assetmetadata.Revision, error) {
	projectID, err := textUUID(req.ProjectID)
	if err != nil {
		return assetmetadata.Revision{}, assetmetadata.ErrProjectNotFound
	}
	// The actor id is parsed only to fail closed on an id the audit row
	// could not store: actor_id is a uuid column with a foreign key to
	// users, so a malformed value would fail the INSERT inside the
	// transaction and take the whole revision down with it. Refusing here
	// says why.
	if _, err := textUUID(req.ActorID); err != nil {
		return assetmetadata.Revision{}, fmt.Errorf("%w: acting user id: %v", assetmetadata.ErrStore, err)
	}

	var out assetmetadata.Revision
	err = WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		q := sqlc.New(tx)
		if _, err := q.GetProjectByIDForUpdate(ctx, projectID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assetmetadata.ErrProjectNotFound
			}
			return err
		}

		asset, err := q.GetResearchAssetByPIDForUpdate(ctx, req.AssetPID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return assetmetadata.ErrAssetNotFound
			}
			return err
		}
		// Two PARSED uuids are compared, not the request's text against the
		// row's text: a uuid has one value and several spellings (case), and
		// prepare trims the request without folding it — the same comparison
		// assetrights.ChangeHolder makes for the same reason.
		if projectID != asset.OriginProjectID {
			// The asset exists but not in the project the caller's
			// authorization was resolved against. Refused as not-found for
			// the same reason the project read gate refuses: the caller has
			// established no right to know where else this pid points.
			return assetmetadata.ErrAssetNotFound
		}

		before := metadataSnapshot(asset)
		after := applyChanges(before, req.Changes)
		// The audit row carries what CHANGED, so the two summaries are the
		// metadata as it stood and as it now stands — the shape
		// UpdateSettings gives its purpose/activity_status pair. The entry's
		// actor, via, action, target, project and correlation id are the
		// command's (assetmetadata.auditEntry); nothing here overwrites
		// them.
		req.Audit.BeforeSummary = metadataSummary(before)
		req.Audit.AfterSummary = metadataSummary(after)

		row, err := q.ReviseResearchAssetMetadata(ctx, sqlc.ReviseResearchAssetMetadataParams{
			ID:            asset.ID,
			Title:         after.Title,
			Slug:          after.Slug,
			Description:   after.Description,
			Keywords:      nonNilList(after.Keywords),
			Contact:       nonNilList(after.Contact),
			Documentation: nonNilList(after.Documentation),
		})
		if err != nil {
			return err
		}
		if err := appendAudit(ctx, q, req.Audit); err != nil {
			return err
		}
		out = assetmetadata.Revision{
			ProjectID: req.ProjectID,
			AssetPID:  req.AssetPID,
			Metadata:  metadataFromRow(row),
		}
		return nil
	})
	if err != nil {
		return assetmetadata.Revision{}, mapAssetMetadataError(err)
	}
	return out, nil
}

// metadataFromRow renders one research_assets row's metadata as the
// domain value. Lists are normalised to non-nil: the columns are NOT NULL
// with a '{}' default (migration 00125), and a caller iterating the
// result should not have to branch on nil for a field the database
// guarantees is a list.
func metadataFromRow(row sqlc.ResearchAsset) assets.AssetMetadata {
	return assets.AssetMetadata{
		PID:           assets.PID(row.Pid),
		Title:         row.Title,
		Slug:          row.Slug,
		Description:   row.Description,
		Keywords:      nonNilList(row.Keywords),
		Contact:       nonNilList(row.Contact),
		Documentation: nonNilList(row.Documentation),
		CoverBlobID:   pgUUIDToText(row.CoverBlobID),
	}
}

// metadataSnapshot renders only the REVISABLE half of a row — the six
// columns this surface writes. It is deliberately narrower than
// metadataFromRow: CoverBlobID is a reserved slot nothing writes, and
// putting a value no path can set into an audit before/after pair would
// record a change that cannot happen.
func metadataSnapshot(row sqlc.ResearchAsset) assets.AssetMetadata {
	return assets.AssetMetadata{
		Title:         row.Title,
		Slug:          row.Slug,
		Description:   row.Description,
		Keywords:      nonNilList(row.Keywords),
		Contact:       nonNilList(row.Contact),
		Documentation: nonNilList(row.Documentation),
	}
}

// applyChanges folds a request's fields onto the stored values: a nil
// pointer means "leave it as it is", a non-nil one means "write this"
// (including the empty value, which is how a field is cleared). The
// command has already validated and trimmed every non-nil field, so this
// function copies and never judges.
func applyChanges(current assets.AssetMetadata, c assetmetadata.Changes) assets.AssetMetadata {
	out := current
	if c.Title != nil {
		out.Title = *c.Title
	}
	if c.Slug != nil {
		out.Slug = *c.Slug
	}
	if c.Description != nil {
		out.Description = *c.Description
	}
	if c.Keywords != nil {
		out.Keywords = *c.Keywords
	}
	if c.Contact != nil {
		out.Contact = *c.Contact
	}
	if c.Documentation != nil {
		out.Documentation = *c.Documentation
	}
	out.Keywords = nonNilList(out.Keywords)
	out.Contact = nonNilList(out.Contact)
	out.Documentation = nonNilList(out.Documentation)
	return out
}

// metadataSummary renders one metadata value as the audit row's
// before/after jsonb document (00012:52-53). The keys are the column
// names, which is what makes a stored summary readable without a decoder
// dictionary — the rule UpdateSettings follows with its purpose /
// activity_status keys.
func metadataSummary(m assets.AssetMetadata) map[string]any {
	return map[string]any{
		"title":         m.Title,
		"slug":          m.Slug,
		"description":   m.Description,
		"keywords":      nonNilList(m.Keywords),
		"contact":       nonNilList(m.Contact),
		"documentation": nonNilList(m.Documentation),
	}
}

// nonNilList renders a list column's value as a non-nil slice. []string is
// the value an empty text[] column carries; nil is the zero value Go gives
// it when a row is scanned from a database that always has one.
func nonNilList(items []string) []string {
	if items == nil {
		return []string{}
	}
	return items
}

// mapAssetMetadataError keeps the sentinels of the metadata package and
// turns everything else into ErrStore.
func mapAssetMetadataError(err error) error {
	switch {
	case err == nil,
		errors.Is(err, assetmetadata.ErrValidation),
		errors.Is(err, assetmetadata.ErrForbidden),
		errors.Is(err, assetmetadata.ErrProjectNotFound),
		errors.Is(err, assetmetadata.ErrAssetNotFound),
		errors.Is(err, assetmetadata.ErrCoverNotSupported),
		errors.Is(err, assetmetadata.ErrStore):
		return err
	}
	return fmt.Errorf("%w: %v", assetmetadata.ErrStore, err)
}
