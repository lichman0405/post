-- Publication impact preview reads (T0704).
--
-- Every query here is a READ: the preview answers "if this publish were
-- executed, who would see what?" without executing anything (docs/23 §4).
-- Nothing in this file writes a row or takes a lock. The guarantee in
-- production is that reading: no statement on this path writes, over the
-- pool cmd/api already has. The stronger guarantee — a session in which a
-- write is refused at the server (default_transaction_read_only, SQLSTATE
-- 25006) — is not a property of production; it is what tests/integration
-- builds around this route on purpose, and it is where that claim is
-- checked.
--
-- The long form of why each query is shaped the way it is — which identity
-- names which table, and why a project's visibility is the answer for all
-- four origin kinds — is in that adapter's package doc, where the reader of
-- the resolution actually is.
--
-- sqlc attaches the comment above a query to that query's generated
-- function, so this header must stay short: it lands on the first one.

-- name: GetPreviewAssetByPID :one
-- The asset a version would be published under: research_assets.pid is the
-- unique public identity (migration 00064), so this is an index lookup. The
-- row carries the two things the preview checks against it besides
-- existence: the asset's type (every version of an asset is of that
-- asset's type) and the project the asset belongs to (an asset's versions
-- are published in the asset's own project, docs/11 §2).
SELECT id::text AS id,
       pid,
       asset_type,
       title,
       origin_project_id::text AS origin_project_id
FROM research_assets
WHERE pid = @pid;

-- name: ListPreviewPins :many
-- The stored versions a candidate's dependency pins name (docs/11 §5), with
-- the visibility each has RIGHT NOW. A pin is canonical text
-- pid@version (internal/assets.DependencyPin), and both halves come from
-- the stored row (research_assets.pid, research_asset_versions.version), so
-- the comparison is exact rather than a parse on the database's side.
--
-- A pin with no row here does not exist; the caller reports that as an
-- unresolved dependency, which is why the query returns what it finds
-- rather than a row per requested pin.
--
-- The version row's own id comes back beside its visibility because the
-- resolution is shared: the publish's transaction runs this same read to
-- decide the preview's private-dependency blockers, and what it has to WRITE
-- afterwards — the asset_dependencies row recording the project's use of the
-- pinned version (T0707) — is keyed by that id. Carrying it here is what
-- keeps "does this pin resolve, and to which row" one answer instead of two
-- joins that could drift apart; the read-only preview surfaces ignore it.
SELECT (ra.pid || '@' || rav.version)::text AS pin,
       rav.id::text AS version_id,
       rav.visibility
FROM research_asset_versions rav
JOIN research_assets ra ON ra.id = rav.asset_id
WHERE (ra.pid || '@' || rav.version) = ANY(@pins::text[]);

-- name: ListPreviewReleaseRefs :many
-- release:<uuid> origin refs (docs/11 §1: the immutable release the version
-- was published from). The release resolves to its project, and the
-- project's visibility is what decides whether the ref is public today.
SELECT r.id::text AS entity_id,
       r.project_id::text AS project_id,
       p.visibility AS project_visibility
FROM releases r
JOIN projects p ON p.id = r.project_id
WHERE r.id = ANY(@ids::uuid[]);

-- name: ListPreviewStateRefs :many
-- state:<uuid> origin refs (the accepted project state whose snapshot the
-- version fixed). A project state belongs to its project directly
-- (project_states.project_id), so a state ref can point across projects —
-- which is exactly the case the preview has to name.
SELECT ps.id::text AS entity_id,
       ps.project_id::text AS project_id,
       p.visibility AS project_visibility
FROM project_states ps
JOIN projects p ON p.id = ps.project_id
WHERE ps.id = ANY(@ids::uuid[]);

-- name: ListPreviewObjectVersionRefs :many
-- object_version:<uuid> origin refs: the object versions the publication
-- would carry (docs/11 §3). The version's project is its object's
-- (scientific_objects.project_id — a version does not carry one of its
-- own), and the title comes with it because the preview shows what a
-- reader would see.
SELECT sov.id::text AS entity_id,
       sov.title AS title,
       so.id::text AS object_id,
       so.project_id::text AS project_id,
       p.visibility AS project_visibility
FROM scientific_object_versions sov
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
WHERE sov.id = ANY(@ids::uuid[]);

-- name: ListPreviewProjectRefs :many
-- project:<uuid> origin refs (docs/11 §2: the project the version came out
-- of). Here the referenced entity IS the project, so existence and
-- visibility come from one row.
SELECT id::text AS entity_id,
       visibility
FROM projects
WHERE id = ANY(@ids::uuid[]);

-- name: ListPreviewBlobs :many
-- The blobs a manifest's metadata names, and whether any attachment of each
-- records access_level = 'open' right now.
--
-- Blob access is a property of the ATTACHMENT, not of the blob
-- (blob_attachments.access_level, migration 00008: the same bytes attached
-- to two object versions may be open in one place and restricted in
-- another), so the question answered here is "is there an attachment that
-- says open" — exactly that, and no more.
--
-- That is a NECESSARY but NOT a SUFFICIENT reading of docs/17 §5's download
-- gate ("每次下载检查 Project/Object/Blob access policy ... Public metadata
-- 不代表 blob open download"). The object's policy and the project's
-- (docs/12 §2: a private project's blobs are invisible by default) do NOT
-- take part in this decision, so is_open=true means "some attachment says
-- open" and NOT "the network can download these bytes": a blob openly
-- attached only inside a private project answers true here. The predicate
-- is left on the one axis on purpose — what the full download gate is, and
-- how its inputs rank, is product semantics this task does not decide.
-- What the preview then refuses is narrower and stated where it is
-- enforced (internal/assets: a version whose documents promise open data
-- access while no attachment says open).
--
-- Aggregating with bool_or and defaulting to false is the fail-closed
-- answer on the axis that IS decided: a blob with no openly attached row —
-- including one with no attachment at all — is not open.
--
-- The comparison is on the blob id in text form, which is how a manifest
-- declares it (specs/schemas/dataset.schema.json: blob_ids is an array of
-- strings). An id that is not a stored blob's uuid text matches nothing and
-- is reported as unresolved; the manifest is free to name a foreign
-- reference (a DOI, a storage key) and this query is not the place to
-- decide that it may not.
SELECT b.id::text AS blob_id,
       COALESCE(bool_or(ba.access_level = 'open'), false)::boolean AS is_open
FROM blobs b
LEFT JOIN blob_attachments ba ON ba.blob_id = b.id
WHERE b.id::text = ANY(@blob_ids::text[])
GROUP BY b.id;
