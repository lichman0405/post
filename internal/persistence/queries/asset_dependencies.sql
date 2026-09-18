-- Recorded usages of published asset versions (T0707, asset_dependencies,
-- migration 00010).
--
-- Two operations over one table, and they are the two halves of the same
-- fact: the WRITE a publish runs inside its own transaction (the project
-- that publishes a version declares the exact versions its work was built
-- against — internal/assets.UsageDeclaration), and the project-side READ of
-- what that project uses.
--
-- The asset-side read of the same table (the page's "used/derived public
-- links") is in asset_page.sql, where the page's other reads live: it is
-- keyed by an asset pid, this one is keyed by a project id, and the two
-- answer different questions (who uses THIS version / what does THIS project
-- use).
--
-- Like every other read in this directory, the project-side read below does
-- NOT filter by visibility: private rows come back with the public ones and
-- internal/assets.BuildProjectDependencies decides what may be rendered, where the
-- rule has unit tests that name the docs it comes from (docs/23 §5). The
-- write filters nothing either — it is handed declarations the application
-- layer already made.

-- name: RecordAssetDependency :exec
-- Record one usage: a project's use of one exact published asset version.
--
-- ON CONFLICT DO UPDATE rather than DO NOTHING, and the column is
-- visibility_of_usage: the row is the CURRENT STATE of a usage, not its
-- history (asset_page.sql records the same reading of this table, which
-- migration 00014 exempts from the append-only guards precisely because it
-- is mutable by design). A project that republishes a usage under a private
-- version is narrowing its own declaration, and a row that kept the older,
-- wider value would be the fail-OPEN direction — the platform would go on
-- announcing a usage the project has stopped announcing. The widening
-- direction cannot be reached this way alone: it happens only inside a
-- publish, which is the governed, owner-level action docs/23 §4 describes.
--
-- created_at is deliberately NOT updated: it records when the usage was
-- first declared, and a narrowing does not make it younger.
INSERT INTO asset_dependencies (project_id, asset_version_id, dependency_type, visibility_of_usage)
VALUES (@project_id, @asset_version_id, @dependency_type, @visibility_of_usage)
ON CONFLICT (project_id, asset_version_id, dependency_type)
DO UPDATE SET visibility_of_usage = EXCLUDED.visibility_of_usage;

-- name: ListProjectAssetUsages :many
-- The usages one project has declared, oldest first.
--
-- The used version comes back resolved: its canonical pid@version identity
-- (both halves from the stored rows, so the comparison and the rendering
-- cannot disagree about which version a pin names), its title and type, and
-- BOTH visibility axes — the version's own and its asset's originating
-- project's — because an entry may be rendered only when the network can
-- open the version (internal/assets.mayLinkVersion), and the row's own
-- visibility_of_usage, which decides what a non-member sees.
--
-- Every row of the project comes back, private ones included; see the file
-- header for why the filter is not here.
SELECT (ra.pid || '@' || rav.version)::text AS pin,
       ra.title,
       ra.asset_type,
       rav.visibility AS version_visibility,
       p.visibility AS project_visibility,
       ad.dependency_type,
       ad.visibility_of_usage,
       ad.created_at
FROM asset_dependencies ad
JOIN research_asset_versions rav ON rav.id = ad.asset_version_id
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p ON p.id = ra.origin_project_id
WHERE ad.project_id = @project_id
ORDER BY ad.created_at, pin, ad.dependency_type;
