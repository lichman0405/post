-- +goose Up
-- Public feed reads (T1004, docs/18 §4 "RSS/Atom（公开对象/项目）").
--
-- This migration adds NO table and NO column: a feed is DERIVED from rows
-- that already exist — a project's publicly published asset versions and
-- knowledge publications — so there is nothing here to keep in sync and
-- nothing that can drift from the canonical store (CLAUDE.md §8: PostgreSQL
-- holds the semantic truth; a feed is a view of it, not a second copy).
--
-- What it adds is the indexing those reads need, and nothing else. The two
-- feed routes are ANONYMOUS: any client on the network may ask for a public
-- project's feed, repeatedly, and the read behind it walks every asset and
-- every object of that project. Without these two indexes both walks are
-- sequential scans of tables that grow with the whole platform's output —
-- the shape docs/27 rules out for a public read, and the reason migration
-- 00036 exists for the RSG query surface.
--
-- Neither index carries semantic content: dropping both leaves every answer
-- identical and only changes how long the database takes to give it.

-- research_assets.origin_project_id is the join key of the project feed
-- ("the public versions of this project's assets") and until now it had no
-- index at all — the column is written once at asset creation (migration
-- 00010) and read by the asset page's pins. The project feed reads it on
-- every anonymous request for the newest N published versions.
CREATE INDEX research_assets_origin_project_idx
  ON research_assets (origin_project_id);

-- The per-asset version read, restricted to the versions a public feed may
-- render. The partial predicate is the feed's own filter
-- (research_asset_versions.visibility, migration 00010), so the index holds
-- exactly the rows a public reader can be served and skips the private ones
-- — which also means the index size tracks the platform's PUBLIC output
-- rather than its total output.
--
-- The ordering columns are the feed's ordering (published_at DESC, id DESC):
-- two entries published in the same instant are ordered by their immutable
-- id, so one state renders one document, byte for byte, on every request.
CREATE INDEX research_asset_versions_public_published_idx
  ON research_asset_versions (asset_id, published_at DESC, id DESC)
  WHERE visibility = 'public';

-- +goose Down
DROP INDEX research_asset_versions_public_published_idx;
DROP INDEX research_assets_origin_project_idx;
