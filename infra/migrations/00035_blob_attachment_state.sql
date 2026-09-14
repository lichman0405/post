-- +goose Up
-- Blob attachments carry the state they were created in (T0206). The domain
-- contract names blob attachments among the state members whose state_id
-- column makes a state's content enumerable from its id alone
-- (internal/domain/state.go), but 00008 created blob_attachments without the
-- column. Without it the manifest export (ListManifestBlobRefs) can only
-- filter on the owning object version's state, so an attachment made in a
-- later state would appear in earlier states' manifests and move their
-- hashes — a state's hash must be a pure function of the state's own
-- recorded content, so the attachment's own state is the only column the
-- export may filter on.
--
-- Existing rows are backfilled from the owning object version's state_id —
-- the only defensible source available. This is an approximation for
-- pre-existing rows, not a claim about when they were attached: an
-- attachment created in state S for a version that lives in an ancestor
-- state is recorded under the ancestor's state. New attachments (AttachBlob)
-- always write the committing state, so the approximation applies only to
-- rows predating this migration.
ALTER TABLE blob_attachments
  ADD COLUMN state_id uuid REFERENCES project_states(id) ON DELETE RESTRICT;
UPDATE blob_attachments ba
SET state_id = sov.state_id
FROM scientific_object_versions sov
WHERE sov.id = ba.scientific_object_version_id;
ALTER TABLE blob_attachments
  ALTER COLUMN state_id SET NOT NULL;

-- The manifest export's blob-ref filter (ListManifestBlobRefs) walks this
-- column on every export; the index keeps it an indexed lineage lookup
-- instead of a per-export seq scan (same shape as the version tables'
-- state indexes from 00026).
CREATE INDEX blob_attachments_state_idx ON blob_attachments(state_id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
