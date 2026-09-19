-- +goose Up
-- Research Profile / Organization Profile read paths (task T0808, docs/42).

-- The Research Profile page (docs/42 "Research Profile": "Affiliations、public
-- contribution dimensions、accepted/released/reused/reproduced evidence、
-- assets/projects、confidential verified contribution summaries") and the
-- Organization Profile page (docs/05 §3 /orgs/{org}, "institutional research
-- identity") are the first readers that ask these tables "everything about
-- THIS person / THIS organization". Until now every read of them went the
-- other way round — by project, by asset, by state — so the columns the new
-- reads filter on carry no index at all, and every profile view would be a
-- sequential scan of an append-only table that only ever grows.
--
-- Five indexes, one per path the profile reads take. Nothing else changes:
-- no column, no constraint, no table. The two vocabulary columns that have
-- no CHECK keep having none (role_codes, event_type — the 00064/00066/00067/
-- 00045 convention), and no aggregate, counter, weight or rank column is
-- added anywhere, because docs/13 §4 forbids a single score and CLAUDE.md §9
-- invariant 13 forbids a Truth Score: the profile derives its dimensions from
-- rows, and a column that held a total would be the score this schema is not
-- allowed to have.

-- 1. A person's contributions (docs/13 §1): the ledger rows one actor wrote,
-- newest first. contribution_events has been read by actor exactly never —
-- 00087 gave it one index, the partial unique on research_event_id that makes
-- the projection idempotent — so this is the first index on the read path
-- that answers "what has this person contributed".
--
-- The order (actor_id, occurred_at DESC, id DESC) is the read's own: a
-- profile renders the newest rows first, and id closes the sort so two rows
-- written in the same transaction have a deterministic order.
CREATE INDEX contribution_events_actor_occurred_idx
  ON contribution_events (actor_id, occurred_at DESC, id DESC);

-- 2. An organization's public research activity (docs/05 §3, docs/42): the
-- ledger rows recorded while the actor was affiliated with one organization
-- — organization_id_at_time, which 00011 named for exactly that question
-- ("affiliation at time", docs/13 §1) and the projection resolves at event
-- time (internal/contribution/ledger_store.go).
--
-- This is also the index that makes "离职后个人历史保留" answerable at all: a
-- membership that ends stops resolving NEW events to the organization, and
-- the rows already written keep naming it, so the organization's history is
-- readable without consulting the membership table for anything but dates.
CREATE INDEX contribution_events_org_occurred_idx
  ON contribution_events (organization_id_at_time, occurred_at DESC, id DESC);

-- 3. The assets a person or an organization is credited on (docs/11 §6:
-- "Creator/history 永久保留"; asset_version_parties, 00082). The table's only
-- index is by version (asset_version_parties_version_role), which is the
-- direction the asset page reads it in — "who is credited on this version".
-- The profile asks the reverse, "which versions is this party credited on",
-- and party_id is not a leading column of anything.
--
-- (party_kind, party_id) leads with the kind because a party id is only
-- meaningful with it (00082: "no reader can hold an id without knowing
-- whether it names a person or an organization") — the index says the same
-- thing the column pair does.
CREATE INDEX asset_version_parties_party_idx
  ON asset_version_parties (party_kind, party_id);

-- 4. The reproductions a person asserted (docs/10 §4's relation vocabulary,
-- docs/13 §4's "independent reproductions" dimension): evidence_assertions
-- rows created by one author whose relation is reproduces /
-- fails_to_reproduce. 00041 indexed this table by target and by evidence —
-- the directions the evidence network reads it in — and created_by has no
-- index, so "the assertions this person made" is a scan today.
--
-- relation_type is the second column because the profile's read is always
-- the same pair ("this author, these two relations"), and the two columns
-- together are exactly the predicate.
CREATE INDEX evidence_assertions_author_relation_idx
  ON evidence_assertions (created_by, relation_type);

-- 5. The reuses of a version (docs/13 §4's "assets reused" dimension): the
-- asset_dependencies rows that name one asset version — "who publicly uses
-- this version". The table's primary key is (project_id, asset_version_id,
-- dependency_type), which answers the project-side question the publish path
-- and the asset preview ask; a lookup by asset_version_id alone is not a
-- prefix of it, so both this task's profile read and the asset page's
-- used_by block (internal/assets.PageUsage) scan today.
CREATE INDEX asset_dependencies_version_idx
  ON asset_dependencies (asset_version_id);

-- +goose Down
-- (forward-only: no down migration is provided, per docs/53)
