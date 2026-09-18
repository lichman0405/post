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
-- that has one (research_asset_versions) or from the PROJECT that owns the
-- row for publications, which have no visibility column of their own.
--
-- # The knowledge halves read the audience rule's inputs and apply the two cheap ones
--
-- A publication's visibility is NOT its project's alone, and assuming it was
-- is a leak this file used to carry: owner ruling L3-20260916-1 #1 has
-- knowledgepublish.AudienceFor decide over THREE inputs — the version's own
-- visibility axis (scientific_object_versions.visibility_policy_id), the
-- owning project's preset, and the published rights declaration's metadata
-- token — and a members-only publication inside a PUBLIC project is legal and
-- common (docs/12 §2: a private project may publish; 发布不等于公开).
--
-- Two of the three are predicates SQL can spell exactly, and the knowledge
-- halves apply them:
--
--   p.visibility = 'public'                   -- axis 2
--   sov.visibility_policy_id IS NULL          -- axis 1: a version that pins
--                                             -- a policy of its own is
--                                             -- governed by that policy,
--                                             -- and this build resolves no
--                                             -- policy into a public grant
--
-- The third is not spellable here, and a SQL approximation of it would be
-- the defect in another form. The rule is "the rights document's metadata
-- token is exactly rights.MetadataProjectPolicy, and a document this build
-- cannot READ states no token at all" — a Go parse (internal/rights.Parse
-- refuses unknown fields, so the same bytes can mean different things to a
-- different build), not a JSON predicate. The declaration therefore travels
-- back RAW (`rights_json`), and internal/application/feeds decides over it
-- (renderableRows), fail-closed: a document that does not parse, or whose
-- token is anything else, is not public. Both halves report `visibility` as
-- the owning project's preset — for a publication that is one of the rule's
-- three INPUTS, not the answer.
--
-- The consequence for the window is named rather than hidden, because the
-- LIMIT is what this file's whole arrangement is about: it is EXACT for the
-- asset half (research_asset_versions.visibility IS that half's rule) and a
-- window over CANDIDATES for the knowledge half — a row that survives the two
-- predicates above may still be dropped by the model's rights axis, so a
-- project whose newest knowledge rows are all members-only can spend part of
-- the window on rows the feed does not render, and the feed may be SHORTER
-- than @row_limit. It can never be longer and never wrong: the model renders
-- exactly the rows it decides are public. Widening the predicates to win the
-- window back is not an option — that would be a second, weaker copy of the
-- rule, which is what this split exists to prevent.

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
-- The feed's TITLE is deliberately not read here. A knowledge object has no
-- title of its own — the title lives on each version — and the title the
-- feed may carry is the one of the newest publication the feed RENDERS,
-- which is the audience rule's decision, not this query's: naming the feed
-- from "the newest publication" would let a members-only revision published
-- above an open one supply the <title> of a public document.
-- internal/application/feeds.BuildFeed names a knowledge feed from the
-- newest row that survived the rule (see the file header).
--
-- The owning project travels with the row because its visibility is both one
-- of the three inputs the audience rule decides with and what the feed's
-- existence is decided from.
SELECT so.id::text AS id,
       so.object_type,
       so.created_at,
       p.visibility AS project_visibility,
       p.name AS project_name
FROM scientific_objects so
JOIN projects p ON p.id = so.project_id
WHERE so.id = @object_id::uuid;

-- name: ListFeedProjectEntries :many
-- The newest versions of one project that a public feed may render — its
-- assets' public versions and its objects' knowledge publications — newest
-- first. Private asset versions are filtered out by the read strategy above;
-- the knowledge half applies the two cheap predicates and hands the rights
-- document back raw for the model to decide over (see the file header).
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
-- The last two columns are the audience rule's remaining inputs, and they
-- are NULL on the asset half rather than defaulted: an asset version has no
-- visibility policy of its own and no rights declaration, and inventing a
-- value there would be a value a reader could mistake for a fact. The model
-- reads them for knowledge rows only (feeds.EntryState).
SELECT 'asset_version'::text AS entry_kind,
       av.id::text AS entry_id,
       av.version,
       ra.pid AS asset_pid,
       ra.asset_type AS subject_type,
       ra.title,
       av.visibility,
       av.published_at,
       COALESCE(u.handle, '')::text AS publisher,
       NULL::uuid AS visibility_policy_id,
       NULL::jsonb AS rights_json
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
       p.visibility,                        -- the audience rule's project axis
       kp.published_at,
       COALESCE(u.handle, '')::text,
       sov.visibility_policy_id,            -- the audience rule's version axis
       kp.rights_json                       -- read RAW: internal/rights decides
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
LEFT JOIN users u ON u.id = kp.published_by
WHERE so.project_id = @project_id::uuid
  AND p.visibility = 'public'
  AND sov.visibility_policy_id IS NULL
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
-- One knowledge object's publications that a public feed may render, newest
-- first: the project half above, one target narrower.
--
-- The two predicates are the audience rule's first two axes and nothing
-- else (see the file header): a version that pins a visibility policy of its
-- own is governed by that policy, and this build resolves no policy into a
-- public grant; a private project's content is invisible outside it (docs/12
-- §2 — a private project may publish, and publishing does not widen). The
-- rights axis is NOT spelled here: the declaration travels back raw and
-- internal/application/feeds decides over it, fail-closed on a document this
-- build cannot read.
--
-- `visibility` is the owning project's preset — one of the rule's three
-- INPUTS, never the answer — and it is the same value for every row of one
-- target, because every row belongs to the same project.
--
-- visibility_policy_id comes back with it even though the predicate above
-- has already excluded every row that pins one: the model re-checks the axis
-- it is given (feeds.EntryState), so a query edit that dropped the predicate
-- would render a shorter feed rather than a wider one.
SELECT kp.id::text AS entry_id,
       kp.public_version AS version,
       sov.title,
       so.object_type AS subject_type,
       p.visibility,
       kp.published_at,
       COALESCE(u.handle, '')::text AS publisher,
       sov.visibility_policy_id AS visibility_policy_id,
       kp.rights_json
FROM knowledge_publications kp
JOIN scientific_object_versions sov ON sov.id = kp.object_version_id
JOIN scientific_objects so ON so.id = sov.object_id
JOIN projects p ON p.id = so.project_id
LEFT JOIN users u ON u.id = kp.published_by
WHERE so.id = @object_id::uuid
  AND p.visibility = 'public'
  AND sov.visibility_policy_id IS NULL
ORDER BY kp.published_at DESC, kp.id DESC
LIMIT @row_limit;
