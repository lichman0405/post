-- Public feed reads (T1004, docs/18 §4 "RSS/Atom（公开对象/项目）").
--
-- The read behind the three anonymous feed routes: a public project's
-- published output, one asset's published versions, one knowledge object's
-- publications. Every query here is a READ: nothing in this file writes a
-- row or takes a lock.
--
-- # What the visibility predicates here are, and are not
--
-- The entry lists filter by visibility, and that filter is a READ STRATEGY,
-- not the disclosure rule. The lists are BOUNDED (LIMIT @row_limit), and a
-- bounded window read raw is a resource a writer can exhaust: more than
-- @row_limit newer PRIVATE versions of a public asset push its public ones
-- out of the window, and the feed of a public, published asset answers as if
-- there were nothing to subscribe to (tests/integration
-- TestPrivateVersionsDoNotSuppressPublicOnes). Filtering here makes the LIMIT
-- mean "the newest @row_limit rows a public feed may RENDER", which is the
-- question the caller is actually asking.
--
-- The disclosure rule itself stays in internal/application/feeds (BuildFeed),
-- where it is a pure function with unit tests that name the criteria they
-- come from, and where it applies to EVERY entry regardless of what a query
-- returned. Dropping the predicates above would cost entries, not
-- correctness: the model would render exactly the same document from the
-- rows that survived its own filter. That is the property this split is for —
-- a query change cannot by itself publish a private row, which is
-- internal/assets' arrangement too (see the header of asset_page.sql).
--
-- What IS decided here is what a row MEANS: `visibility` comes from the row
-- that has one (research_asset_versions) or from the project that owns the
-- row for publications, which have no visibility column of their own —
-- publishing IS the public act (docs/12 §2) and the owning project is what
-- decides whether anyone outside it may reach the publication. The knowledge
-- halves need no predicate of their own: a publication's visibility IS its
-- project's, so within one target every knowledge row shares one value, and
-- the model's project gate settles all of them at once.

-- name: GetFeedProject :one
-- The project a project feed is about. An index lookup on the primary key:
-- a feed's target id is the project's uuid, not its slug (a slug is
-- revisable; a feed's identity must not be — internal/application/feeds.
-- feedID).
--
-- purpose travels with the row because it is the project's own one-line
-- description and the feed's subtitle (a project's purpose is public
-- whenever the project is: the public project read renders it).
SELECT p.id::text AS id,
       p.name,
       p.purpose,
       p.visibility,
       p.created_at
FROM projects p
WHERE p.id = @project_id::uuid;

-- name: GetFeedAsset :one
-- The asset an asset feed is about, addressed by its pid — the persistent
-- identity every asset URL is built from (migration 00064,
-- internal/assets/url.go), never the slug or the owning organization.
--
-- The origin project's visibility comes back with it because that project
-- is what decides whether the feed exists at all (the same rule the asset
-- page's read gate applies and the same one the subscription audience
-- resolution uses, T1002). An INNER join on a NOT NULL foreign key, so a
-- row exists here exactly when the asset does.
SELECT ra.pid,
       ra.title,
       ra.asset_type,
       ra.created_at,
       p.visibility AS project_visibility,
       p.name AS project_name
FROM research_assets ra
JOIN projects p ON p.id = ra.origin_project_id
WHERE ra.pid = @pid;

-- name: GetFeedKnowledgeObject :one
-- The scientific object a knowledge feed is about, addressed by its uuid.
--
-- A knowledge object has no title of its own — the title lives on each
-- version (scientific_object_versions.title) — so the feed's title is the
-- one the object is published under: the newest publication's version
-- title, coerced to '' when the object has never been published. That empty
-- string is a state the model renders as "no feed" (a knowledge feed needs
-- at least one publication to exist at all), never as a nameless feed.
--
-- The owning project travels with the row for the same reason it does on
-- the asset: its visibility is what the feed's existence is decided from.
SELECT so.id::text AS id,
       so.object_type,
       so.created_at,
       p.visibility AS project_visibility,
       p.name AS project_name,
       COALESCE((
         SELECT sov.title
         FROM knowledge_publications kp
         JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
         WHERE sov.object_id = so.id
         ORDER BY kp.published_at DESC, kp.id DESC
         LIMIT 1
       ), '')::text AS title
FROM scientific_objects so
JOIN projects p ON p.id = so.project_id
WHERE so.id = @object_id::uuid;

-- name: ListFeedProjectEntries :many
-- The newest versions of one project that a public feed may render — its
-- assets' public versions and its objects' knowledge publications — newest
-- first. Private asset versions are filtered out by the read strategy above;
-- the knowledge half needs no predicate because a publication's visibility
-- IS its owning project's.
--
-- The two halves are UNION ALL'd rather than merged in Go so that ONE
-- ordering and ONE limit apply to the union: taking the newest N of each
-- half separately and merging them would have to read 2N rows to answer a
-- question about N, and would silently mis-order the boundary between the
-- halves if the two queries disagreed about ties.
--
-- Ordering is (published_at DESC, entry_id DESC): the id tiebreak makes the
-- order total, so one database state renders one document — the property
-- the transport's ETag depends on.
--
-- The publisher's handle is LEFT JOINed for the entry's author. A handle is
-- public identity (a live profile is a public read, T0102); a missing user
-- row renders as no author rather than as an error, because the feed's
-- subject is the version, not the account.
SELECT 'asset_version'::text AS entry_kind,
       av.id::text AS entry_id,
       av.version,
       ra.pid AS asset_pid,
       ra.asset_type AS subject_type,
       ra.title,
       av.visibility,
       av.published_at,
       COALESCE(u.handle, '')::text AS publisher
FROM research_asset_versions av
JOIN research_assets ra ON ra.id = av.asset_id
LEFT JOIN users u ON u.id = av.published_by
WHERE ra.origin_project_id = @project_id::uuid
  AND av.visibility = 'public'
UNION ALL
SELECT 'knowledge_publication'::text,
       kp.id::text,
       kp.public_version,
       ''::text,
       so.object_type,
       sov.title,
       p.visibility,
       kp.published_at,
       COALESCE(u.handle, '')::text
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
LEFT JOIN users u ON u.id = kp.published_by
WHERE so.project_id = @project_id::uuid
ORDER BY published_at DESC, entry_id DESC
LIMIT @row_limit;

-- name: ListFeedAssetEntries :many
-- One asset's PUBLIC versions, newest first — the rows this feed exists for,
-- which is why the predicate above and migration 00080's partial index have
-- the same shape. Indexed by (asset_id, published_at DESC, id DESC).
SELECT av.id::text AS entry_id,
       av.version,
       ra.pid AS asset_pid,
       ra.title,
       ra.asset_type AS subject_type,
       av.visibility,
       av.published_at,
       COALESCE(u.handle, '')::text AS publisher
FROM research_asset_versions av
JOIN research_assets ra ON ra.id = av.asset_id
LEFT JOIN users u ON u.id = av.published_by
WHERE ra.pid = @pid
  AND av.visibility = 'public'
ORDER BY av.published_at DESC, av.id DESC
LIMIT @row_limit;

-- name: ListFeedKnowledgeEntries :many
-- One knowledge object's publications, newest first.
--
-- A publication has no visibility column: publishing a version onto the
-- network IS the public act (docs/12 §2), and the owning project's
-- visibility is what decides whether anyone outside it may reach it — so
-- that is the value this query reports as the entry's visibility, exactly
-- as the subscription audience resolution does for the same target type
-- (internal/events/subscription_store.go).
SELECT kp.id::text AS entry_id,
       kp.public_version AS version,
       sov.title,
       so.object_type AS subject_type,
       p.visibility,
       kp.published_at,
       COALESCE(u.handle, '')::text AS publisher
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
LEFT JOIN users u ON u.id = kp.published_by
WHERE so.id = @object_id::uuid
ORDER BY kp.published_at DESC, kp.id DESC
LIMIT @row_limit;
