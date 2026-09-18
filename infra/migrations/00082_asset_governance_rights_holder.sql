-- +goose Up
-- Asset governance (T0711): the parties a published asset version credits,
-- and the asset's rights-holder chain.
--
-- # The gap this closes
--
-- The publish gate validates the creator ids a publish declares
-- (internal/assets.PublishCandidate.CreatorIDs, checked by
-- validation.CheckAssetContributors) and NOTHING stored them: the version
-- row's only actor is published_by. The platform therefore asked a
-- publisher for a signature list at publish time, checked it, and dropped
-- it — and the asset page could only name the publisher as the credited
-- party, under a role that said so (internal/assets/page.go's PageCreator
-- comment, which names the missing half as "a schema change").
-- asset_version_parties is that half: the declaration lands in a row.
--
-- # Six roles, three shapes, and never one merged "owner"
--
-- docs/11 §6 is one sentence that fixes the whole model: "分离 Rights
-- Holder、Custodian、Maintainer、Creator、Contributor、Originating Project。
-- Ownership transfer 是 append-only governance event；Creator/history 永久
-- 保留。" Six roles, separated — and the two sentences after the commas
-- are why they do not all live in one table:
--
--   - creator, contributor, custodian, maintainer are facts about a
--     VERSION: who is credited for this published artifact. They are part
--     of the immutable version record (00014's row trigger on
--     research_asset_versions), written once, and never revised —
--     "Creator/history 永久保留". That is asset_version_parties.
--   - the rights holder is a fact about the ASSET: who holds it, which
--     CHANGES, and every change is its own append-only event. A transfer
--     publishes no version and touches no version row, so the holder
--     cannot live in a version-scoped table without either falsifying the
--     version it was attached to or forcing every transfer to write a
--     version. That is asset_rights_holder_events.
--   - the originating project is the asset's own origin
--     (research_assets.origin_project_id, NOT NULL since 00010) — a
--     project, which is neither a person nor an organization. Storing it
--     a second time here would give one fact two sources that can
--     disagree, so it is not duplicated; a reader renders it as a party
--     of kind "project" from the column that already owns it.
--
-- The three shapes are deliberate, and so is the absence of a fourth: no
-- polymorphic actor_id column, and no new "party" table that would hold
-- users and organizations in one place again. A party is recorded as
-- WHICH KIND of identity it is AND WHICH ROW — two columns, always
-- together — so no reader can hold an id without knowing whether it names
-- a person or an organization. owner ruling L3-20260916-1 (tasks/
-- decisions.md: "权利人可以是组织；人和组织分开记") is exactly this, and
-- migration 00002 already stores the two kinds in two tables (users,
-- organizations); the kind column is what keeps them apart here.
--
-- # Both tables admit exactly the two IDENTITY kinds — user and organization
--
-- A row of either table names a person or an institution, and the kind
-- column says which. Neither admits a project, and that is the point
-- rather than an omission: docs/11 §6 lists Originating Project as one of
-- the six roles precisely because it is NOT a person and NOT an
-- organization, and its value (and every reader of it) is
-- research_assets.origin_project_id, which owns that fact. A party id
-- pointing into projects from here would be a second home for the
-- originating project, in a table whose readers would then have to decide
-- which of the two was authoritative.
--
-- The vocabulary of kinds is three (internal/domain.PartyKind: user,
-- organization, project — the three identity tables the platform has),
-- and each storage site admits the subset it can hold. That asymmetry is
-- deliberate and stated in both places rather than implied by one.
--
-- The writer that exists today is the publish path, whose declaration is
-- a list of user ids (internal/assets.validCreatorIDs: "the version
-- credits at least one user"), so 'user' is what it writes; 'organization'
-- is admitted because the rights-holder ruling L3-20260916-1 requires an
-- organization to be recordable as a holder, and because group and
-- institutional credit is a party kind this model must be able to say.
--
-- # Neither table carries a foreign key to its party, deliberately
--
-- A (kind, id) reference cannot be a single-column foreign key: the id
-- points into users for one kind and organizations for the other (and
-- projects for the third). The alternative — a trigger, or a CHECK
-- against a subquery, which PostgreSQL forbids — would put a second
-- definition of "a party exists" in the database, where the two tables it
-- would have to read are the very thing the kind column exists to keep
-- apart. The reference's integrity is the writing command's business, and
-- the two writing paths guarantee different halves of it:
--
--   - asset_version_parties (a credit) is written by the publish
--     transaction, whose gate checks the id's SHAPE and nothing else
--     (internal/assets.validCreatorIDs: a uuid, because the column is
--     one). The gate cannot look anything up, and the publish path does
--     not resolve the user either, so a credit may name a user id no
--     users row holds.
--   - asset_rights_holder_events is written by
--     internal/application/assetrights, which resolves the named party
--     against the table its kind names BEFORE it writes, and refuses a
--     kind/id that names no row.
--
-- 00066 says the same thing about the rights document's vocabulary: the
-- one definition lives in Go, and a second spelling of it in SQL drifts.
--
-- # Append-only, both tables, both halves
--
-- Both tables are history: a credit is part of a version's permanent
-- record, and a held relationship is a governance event that a later
-- event supersedes rather than edits. Both therefore take the 00014 row
-- guard (BEFORE UPDATE OR DELETE) and the 00015 statement guard (BEFORE
-- TRUNCATE — a row trigger does not fire on TRUNCATE, and a table that
-- can be truncated is not append-only). "转移之后，转移之前的持有关系仍要
-- 读得出来" is the acceptance this enforces: the previous holder is a row
-- that cannot be overwritten.

CREATE TABLE asset_version_parties (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_version_id uuid NOT NULL REFERENCES research_asset_versions(id) ON DELETE RESTRICT,
  role text NOT NULL CHECK (role IN ('creator', 'contributor', 'custodian', 'maintainer')),
  party_kind text NOT NULL CHECK (party_kind IN ('user', 'organization')),
  party_id uuid NOT NULL,
  -- position is the order the declaring caller sent this party in. It is
  -- stored rather than reconstructed from created_at or from the row id,
  -- because the acceptance for a credit list is that it reads back
  -- "逐项一致" (item by item) with the request: a signature list has an
  -- order, an unordered set does not reproduce one, and gen_random_uuid
  -- does not sort the way anyone declared anything.
  position integer NOT NULL CHECK (position >= 0),
  -- recorded_by is the authenticated actor whose request declared the
  -- credit (the publisher, for the publish path). NOT NULL: a credit with
  -- no attributable declarer would be an anonymous assertion about
  -- someone else's authorship.
  recorded_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  -- One party per role per position, and no party twice in one role: a
  -- declaration that names the same user as creator at two positions is a
  -- malformed list, not a stronger claim, and the database refusing it is
  -- cheaper than every reader having to decide which of two identical
  -- rows to render.
  UNIQUE (asset_version_id, role, party_id),
  UNIQUE (asset_version_id, role, position)
);

COMMENT ON TABLE asset_version_parties IS
  'The parties one published asset version credits, per docs/11 §6 roles (creator, contributor, custodian, maintainer). Append-only: a credit is part of the immutable version record and is never revised — "Creator/history 永久保留". Written by the publish path (T0711) inside the publish transaction, so a version either carries its declared credits or does not exist.';

CREATE TRIGGER asset_version_parties_append_only
  BEFORE UPDATE OR DELETE ON asset_version_parties
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER asset_version_parties_no_truncate
  BEFORE TRUNCATE ON asset_version_parties FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE INDEX asset_version_parties_version_role
  ON asset_version_parties (asset_version_id, role, position);

CREATE TABLE asset_rights_holder_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES research_assets(id) ON DELETE RESTRICT,
  -- ordinal orders the chain. It is a per-asset sequence rather than a
  -- timestamp because the chain's whole meaning is an order, and two
  -- events in one transaction share now() exactly: ordering by
  -- recorded_at alone would leave the current holder to a tie-break
  -- nobody declared. The transfer takes the asset row lock
  -- (SELECT … FOR UPDATE) before reading the current maximum, so the
  -- sequence is serialized per asset rather than computed in a race.
  ordinal integer NOT NULL CHECK (ordinal >= 1),
  holder_kind text NOT NULL CHECK (holder_kind IN ('user', 'organization')),
  holder_id uuid NOT NULL,
  -- The holder this event supersedes. NULL means "there was no holder
  -- before this event" — the first designation of an asset nobody held.
  -- It is an honest absence and never a fabricated party: the column
  -- admits no default, and the pair is all-or-nothing (the CHECK below).
  previous_holder_kind text CHECK (previous_holder_kind IN ('user', 'organization')),
  previous_holder_id uuid,
  -- who asked for the change (the owner whose request it was), and when.
  recorded_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  recorded_at timestamptz NOT NULL DEFAULT now(),
  -- One event per ordinal: the chain is a sequence, and two events at one
  -- position would make "the event after this one" ambiguous.
  UNIQUE (asset_id, ordinal),
  -- The previous holder is a kind AND an id, or neither.
  CONSTRAINT asset_rights_holder_events_previous_complete
    CHECK ((previous_holder_kind IS NULL) = (previous_holder_id IS NULL)),
  -- A change that changes nothing is not a change. Refusing it here, and
  -- not only in the command, means the chain never carries an event whose
  -- before and after are the same party — which is what makes "the
  -- previous holder" a real fact about every row after the first.
  CONSTRAINT asset_rights_holder_events_previous_differs
    CHECK (previous_holder_kind IS NULL
           OR (previous_holder_kind, previous_holder_id) IS DISTINCT FROM (holder_kind, holder_id))
);

COMMENT ON TABLE asset_rights_holder_events IS
  'The append-only chain of rights-holder designations and transfers for one research asset (docs/11 §6: "Ownership transfer 是 append-only governance event"; docs/26 §5 lists ownership transfer among the highest-risk audited actions). Each row is one event: the holder it names, the holder it supersedes (NULL for the first), who asked and when. The current holder is the row with the greatest ordinal; every earlier holder remains readable, which is the acceptance "转移之后，转移之前的持有关系仍要读得出来". Nothing here is updated or deleted — a later transfer appends.';

CREATE TRIGGER asset_rights_holder_events_append_only
  BEFORE UPDATE OR DELETE ON asset_rights_holder_events
  FOR EACH ROW EXECUTE FUNCTION append_only_guard();

CREATE TRIGGER asset_rights_holder_events_no_truncate
  BEFORE TRUNCATE ON asset_rights_holder_events FOR EACH STATEMENT
  EXECUTE FUNCTION append_only_guard();

CREATE INDEX asset_rights_holder_events_asset_ordinal
  ON asset_rights_holder_events (asset_id, ordinal DESC);
