-- Asset governance (T0711): the rows and reads of the two tables
-- migration 00082 adds — the parties a published version credits, and the
-- asset's append-only rights-holder chain.
--
-- # Why the credits are written INSIDE the publish transaction
--
-- InsertAssetVersionParty is called by persistence.AssetPublishStore.Publish,
-- in the same transaction that writes the version row, the idempotency
-- ledger entry, the audit row and the research event. It is not a second
-- step a caller could skip or repeat: a version that committed carries
-- exactly the credits its publish declared, and a refused publish leaves
-- no credit behind. The alternative — a credit write on its own
-- connection after the publish returned — would make "the version exists"
-- and "its declared creators are stored" separately true, which is the
-- same half-done state the credits exist to close.
--
-- # Why the holder chain is read by ordinal and never by timestamp
--
-- asset_rights_holder_events.ordinal is the per-asset sequence the table
-- comment describes; two events in one transaction share now() exactly, so
-- the current holder is the greatest ordinal and nothing else. The write
-- path (CreateAssetRightsHolderEvent) takes the asset row lock first
-- (GetResearchAssetByPIDForUpdate), which is what makes "the greatest
-- ordinal" a value no concurrent transfer can be computing at the same
-- time — the sequence is serialized per asset rather than raced.

-- name: InsertAssetVersionParty :one
-- One credited party of a published version. position is the caller's
-- declaration order (00082), stored so the list reads back item by item
-- the way it was declared.
INSERT INTO asset_version_parties (
  asset_version_id, role, party_kind, party_id, position, recorded_by
)
VALUES (
  @asset_version_id, @role, @party_kind, @party_id, @position, @recorded_by
)
RETURNING *;

-- name: ListAssetPageParties :many
-- The credited parties of EVERY version of the asset behind a pid, with
-- the version they belong to and the caller's declaration order. Raw rows
-- for the whole asset rather than for one version, for the same reason
-- ListAssetPageVersions is: the model renders ONE version and chooses it
-- itself, so a reader that resolved only the version it guessed would
-- starve the model of the state it needs for any other choice.
--
-- The party's identity is NOT joined in here. A party is a (kind, id)
-- pair whose id points into one of two tables depending on the kind
-- (users, organizations), and resolving it is the reader's separate step
-- — users through ListAssetPageUsers, organizations through
-- ListAssetPageOrganizations — because a join on a kind-dependent table
-- cannot be written as one SQL statement without a polymorphic union that
-- would spell the kind vocabulary a second time.
--
-- The join to research_asset_versions is INNER on a NOT NULL foreign key,
-- and the version join to research_assets is INNER on another, so no
-- credit row is dropped.
SELECT avp.asset_version_id::text AS asset_version_id,
       avp.role,
       avp.party_kind,
       avp.party_id::text AS party_id,
       avp.position
FROM asset_version_parties avp
JOIN research_asset_versions rav ON rav.id = avp.asset_version_id
JOIN research_assets ra ON ra.id = rav.asset_id
WHERE ra.pid = @pid
ORDER BY avp.asset_version_id, avp.role, avp.position;

-- name: ListAssetPageOrganizations :many
-- The identities behind the ORGANIZATION ids a party list names. The
-- sibling of ListAssetPageUsers for the second identity table, and an id
-- lookup for the same reason: the page resolves a set of ids it
-- discovered while reading the parties. An id with no row is absent from
-- the result and the model renders no name for it rather than an id-only
-- identity — the reader could not answer for that party, and an
-- organization's slug is not something to invent.
--
-- organizations carries no visibility axis (00002): an organization is
-- either present or not, and its identity is not gated the way a
-- project's is. That is why this lookup needs none of the disclosure
-- rules internal/assets applies to project identities.
SELECT o.id::text AS id,
       o.slug,
       o.name
FROM organizations o
WHERE o.id = ANY(@ids::uuid[]);

-- name: GetResearchAssetByPIDForUpdate :one
-- The asset row behind a pid, LOCKED, for the rights-holder chain. This is
-- the lock that serializes the chain: two transfers of one asset take it
-- in turn, so the second reads the first's holder as its previous holder
-- instead of racing it to the same ordinal.
--
-- A read that only resolves the row (GetResearchAssetByPIDRow, the publish
-- path's) does not lock, and must not: a publish does not write this
-- chain, and taking a write lock per publish for a table it never touches
-- would serialize publishes against a transfer that has nothing to do
-- with the version being published.
SELECT * FROM research_assets WHERE pid = @pid FOR UPDATE;

-- name: GetCurrentAssetRightsHolder :one
-- The current holder: the event with the greatest ordinal, or no row
-- (pgx.ErrNoRows) when the asset has never had one. "Never had one" is a
-- state and not an error — the first event of such an asset records a NULL
-- previous holder (00082) rather than being refused for the absence of a
-- predecessor, and it is never answered by inventing a default holder.
--
-- previous_holder_id is read as the uuid column it is, NOT cast to text:
-- the pair is nullable (00082's all-or-nothing CHECK), and `::text` hides
-- that from sqlc, which then generates a non-pointer string a NULL cannot
-- be scanned into — every read of a first designation failed with "cannot
-- scan NULL into *string". The cast belongs in Go (pgUUIDToText), where
-- the absence is a value the reader decides about.
SELECT id::text AS id,
       asset_id::text AS asset_id,
       ordinal,
       holder_kind,
       holder_id::text AS holder_id,
       previous_holder_kind,
       previous_holder_id,
       recorded_by::text AS recorded_by,
       recorded_at
FROM asset_rights_holder_events
WHERE asset_id = @asset_id
ORDER BY ordinal DESC
LIMIT 1;

-- name: ListAssetRightsHolderEvents :many
-- The asset's whole chain, oldest first. This is the read the acceptance
-- "转移之后，转移之前的持有关系仍要读得出来" is checked through: it returns
-- every designation and transfer the asset has ever had, because the table
-- is append-only (00082's triggers) and no event is ever overwritten by
-- the one that superseded it.
--
-- previous_holder_id is read uncast, for the reason GetCurrentAssetRightsHolder
-- states: the nullability of the pair has to survive into the generated type.
SELECT id::text AS id,
       asset_id::text AS asset_id,
       ordinal,
       holder_kind,
       holder_id::text AS holder_id,
       previous_holder_kind,
       previous_holder_id,
       recorded_by::text AS recorded_by,
       recorded_at
FROM asset_rights_holder_events
WHERE asset_id = @asset_id
ORDER BY ordinal ASC;

-- name: CreateAssetRightsHolderEvent :one
-- One governance event: the holder this change names, the holder it
-- supersedes (NULL for the first designation of an unheld asset), who
-- asked and when. The caller computes the ordinal under the asset row lock
-- and reads the previous holder from the current one, so neither value is
-- the caller's invention: both are read from the chain in the same
-- transaction and the unique (asset_id, ordinal) is the database's own
-- refusal of a chain that forked.
INSERT INTO asset_rights_holder_events (
  asset_id, ordinal, holder_kind, holder_id,
  previous_holder_kind, previous_holder_id, recorded_by
)
VALUES (
  @asset_id, @ordinal, @holder_kind, @holder_id,
  @previous_holder_kind, @previous_holder_id, @recorded_by
)
RETURNING *;

-- name: GetPartyUser :one
-- One user party's identity, by id — the existence proof and the display
-- fields in one read. pgx.ErrNoRows means the id names no user, which the
-- transfer refuses: a stored holder no user table has a row for is a
-- dangling reference, and 00082 explains why the two kind tables are not
-- foreign-keyed from the party columns.
SELECT u.id::text AS id,
       u.handle,
       u.display_name
FROM users u
WHERE u.id = @id;

-- name: GetPartyOrganization :one
-- The same for an organization party (00002's other identity table).
SELECT o.id::text AS id,
       o.slug,
       o.name
FROM organizations o
WHERE o.id = @id;
