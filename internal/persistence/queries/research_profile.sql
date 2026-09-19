-- Research Profile / Organization Profile reads (T0808, docs/42 "Research
-- Profile", docs/05 §4 "Organization Research Profile").
--
-- Ten reads, and every one of them asks a question the platform never asked
-- before this task: "everything about THIS person" and "everything about THIS
-- organization". Until now every read of these tables went the other way
-- round — by project, by asset, by state.
--
-- # Two layers: the window is counted in rows this surface may RENDER
--
-- Every list read here states the RENDER PREDICATE of the dimension it feeds,
-- and its LIMIT comes AFTER that predicate. The predicate is a READ STRATEGY,
-- not the disclosure rule the surface is held to:
--
--   - The predicate is what makes the LIMIT mean "the newest @row_limit rows
--     this surface may RENDER". Read raw, a bounded window is a resource a
--     writer can exhaust — more than @row_limit newer rows that no rule
--     renders push the renderable ones out of it, and the profile answers as
--     if there were nothing to show (tests/integration
--     TestResearchProfileWindowIsCountedInRenderableRows; the same finding as
--     T1004's for the feed reads, internal/persistence/queries/feeds.sql).
--   - The disclosure rule itself stays in
--     internal/application/researchprofile (BuildPersonProfile /
--     BuildOrganizationProfile), where it is a pure function with unit tests
--     that name the document each rule comes from, and where it applies to
--     EVERY row regardless of what a query returned. Removing a predicate
--     below would cost entries, not correctness: the model renders the same
--     document from the rows that survived its own filter. That is the
--     property the split is for, and it is why the model's re-check is not
--     redundant and must not be deleted.
--
-- # Each predicate says exactly what its rule says, and no more
--
-- The judgement is "does this predicate exclude ONLY rows the model would
-- drop?", because a predicate that excluded more would delete rows the surface
-- is required to render. Two axes are therefore deliberately NOT filtered by
-- the project's visibility, and a future edit must not "tidy" them into one
-- `visibility = 'public'`:
--
--   - ListPersonAssetCredits / ListOrganizationAssetCredits: a private project
--     may publish a public version (docs/12 §2), and that version is listed
--     WITHOUT its project (internal/assets.BuildBrowse's hub rule — model.go
--     states it as `publicVersion(row.VersionVisibility)` and nothing else).
--   - ListPersonReproductions: an assertion whose project is private is still
--     the person's own act and renders with the project withheld. What this
--     read must filter on is the assertion's OWN axis (evidence_assertions
--     .visibility, 00091): "an assertion nothing explicitly made public is not
--     rendered anywhere". Both surfaces are anonymous reads, so that axis is
--     not optional.
--
-- ListPersonAffiliations states no predicate because it has none to state: a
-- membership carries no visibility axis, an employer's deactivation withholds
-- its IDENTITY without dropping the person's row (00018, docs/04 §6), and the
-- INNER JOIN on organizations is the whole of its render predicate.
--
-- # Every read is bounded
--
-- contribution_events is append-only and only grows (00014's guards), and a
-- researcher's ledger will outlive any page that renders it. So every list
-- read takes @row_limit (internal/application/researchprofile.FetchLimit) and
-- ends in a UNIQUE tiebreaker — a LIMIT whose ORDER BY can tie returns a
-- different set on each read of the same state, which would make a profile
-- flicker between two truths.
--
-- # What is NOT selected
--
-- users.email appears in no query here: it is identity, not a profile field,
-- and the public profile read already refuses to render it. No query totals
-- anything either — no COUNT, no aggregate of any kind — because docs/13 §4
-- forbids a single score and CLAUDE.md §9 invariant 13 forbids a Truth Score;
-- a total in a read would become the score this platform is not allowed to
-- have. A count of rendered rows is len() of the rendered list, in Go, where
-- nothing can be subtracted from it.

-- name: GetResearchProfilePerson :one
-- One account with its profile content. The LEFT JOIN is the one
-- internal/persistence.ProfileStore uses and for the same reason: a user
-- whose profile row is missing (created by raw SQL, or before 00017's
-- backfill) still reads, with an empty bio — an identity must never become
-- unreadable because of the profile projection (docs/21 §6).
--
-- disabled_at comes back because the decision it feeds is the model's
-- (researchprofile.PersonVisible), not the query's.
SELECT u.id, u.handle, u.display_name, COALESCE(p.bio, ''), u.disabled_at
FROM users u
LEFT JOIN profiles p ON p.user_id = u.id
WHERE u.id = @id;

-- name: ListPersonAffiliations :many
-- Every membership of one person: current AND ended.
--
-- Not "WHERE affiliation_end IS NULL": docs/04 §6 is the reason this
-- dimension exists at all — "Person identity 跨 Organization 长期存在。
-- Affiliation 具有 start/end 时间和 verification status。组织不能删除个人
-- 历史贡献；离职只终止 affiliation/role。" An ended membership is history the
-- profile renders, and the organization's own deactivated_at comes back with
-- it so the model can withhold a deactivated employer's identity while
-- keeping the person's own row (role, dates, verification).
--
-- The order is display order (newest start first, undated last, employer slug
-- as the tiebreaker); the model re-sorts by the same key, so the two cannot
-- disagree about which affiliation comes first.
--
-- The INNER JOIN is this read's render predicate (the header says why it has
-- no visibility predicate): a membership always names an organization and a
-- role (00002: both NOT NULL), and a row whose organization the join cannot
-- resolve is a row with no affiliation to render. The model still drops such a
-- row itself, so a reader defect renders a shorter list rather than a blank
-- employer.
SELECT om.organization_id,
       o.slug,
       o.name,
       o.deactivated_at,
       om.role,
       om.affiliation_start,
       om.affiliation_end,
       om.verified
FROM organization_memberships om
JOIN organizations o ON o.id = om.organization_id
WHERE om.user_id = @user_id
ORDER BY om.affiliation_start DESC NULLS LAST, o.slug, om.organization_id
LIMIT @row_limit;

-- name: ListActorContributions :many
-- The ledger rows one actor wrote, newest first (docs/13 §1, docs/42's
-- "public contribution dimensions").
--
-- The actor's own identity columns are selected even though the person
-- surface does not render them: the organization surface's read
-- (ListOrganizationActivity) returns the same column list, so the two share
-- one scanner, and the shared shape is worth one join on a primary key per
-- row. It is the same row either way — selecting a column that a given
-- surface ignores discloses nothing.
--
-- p.visibility = 'public' is the render predicate: a contribution into a
-- project this surface may not name is DROPPED by the model (the event is a
-- fact about that project's work rather than a public object of its own), so
-- the window is counted in rows the profile may actually render. It is also
-- the whole of the model's rule for this dimension — the row is the person's
-- own and nothing else about it is withheld — so nothing the model would
-- render is excluded here.
SELECT ce.event_type,
       ce.role_codes,
       ce.occurred_at,
       ce.accepted_context,
       ce.released_context,
       COALESCE(ce.via, ''),
       ce.actor_id,
       u.handle,
       u.display_name,
       u.disabled_at,
       COALESCE(ce.project_id::text, '')::text AS project_id,
       COALESCE(p.slug, ''),
       COALESCE(p.name, ''),
       COALESCE(p.visibility, '')
FROM contribution_events ce
JOIN users u ON u.id = ce.actor_id
LEFT JOIN projects p ON p.id = ce.project_id
WHERE ce.actor_id = @actor_id
  AND p.visibility = 'public'
ORDER BY ce.occurred_at DESC, ce.id DESC
LIMIT @row_limit;

-- name: ListOrganizationActivity :many
-- The ledger rows recorded while their actor was affiliated with one
-- organization: organization_id_at_time, the column 00011 named for exactly
-- this question ("affiliation at time", docs/13 §1) and the T0807 projection
-- resolves at event time from the membership window.
--
-- This is what makes "离职后个人历史保留" readable from the organization's side
-- too: a membership that ends stops resolving NEW events to the organization,
-- and the rows already written keep naming it — the organization's record of
-- that person's work does not disappear, it stops growing. Same column list
-- as ListActorContributions, same scanner in Go.
--
-- The render predicate is the model's rule for this surface, in the same
-- order: the project must be public (an institution's public record does not
-- name a project it may not name) AND the row must be attributable — the
-- actor's account must still be part of the network, because an
-- unattributable row is not rendered as "someone" (u.disabled_at IS NULL;
-- users is an INNER join, so the actor is always resolved).
SELECT ce.event_type,
       ce.role_codes,
       ce.occurred_at,
       ce.accepted_context,
       ce.released_context,
       COALESCE(ce.via, ''),
       ce.actor_id,
       u.handle,
       u.display_name,
       u.disabled_at,
       COALESCE(ce.project_id::text, '')::text AS project_id,
       COALESCE(p.slug, ''),
       COALESCE(p.name, ''),
       COALESCE(p.visibility, '')
FROM contribution_events ce
JOIN users u ON u.id = ce.actor_id
LEFT JOIN projects p ON p.id = ce.project_id
WHERE ce.organization_id_at_time = @organization_id
  AND p.visibility = 'public'
  AND u.disabled_at IS NULL
ORDER BY ce.occurred_at DESC, ce.id DESC
LIMIT @row_limit;

-- name: ListPersonAssetCredits :many
-- The published versions one person is credited on (docs/11 §6:
-- "Creator/history 永久保留" — a credit is a fact about a version and is never
-- revised).
--
-- The asset's ORIGIN project comes back with both of its facts (id and
-- visibility) because a private project may publish a public version
-- (docs/12 §2, internal/assets.BuildBrowse): the version renders, the project
-- is withheld. A version label without its project is still a complete
-- citation — pid@version identifies published bytes forever (CLAUDE.md
-- invariant 5).
--
-- One row per CREDIT: the table's UNIQUE is on (version, role, party), so a
-- person who is both creator and custodian of one version has two rows here
-- and the profile renders both — collapsing them would have to choose which
-- role to print.
--
-- The render predicate is the version's OWN axis and nothing else: the model
-- drops a credit exactly when `!publicVersion(row.VersionVisibility)`. The
-- origin project's visibility is deliberately NOT part of it — a private
-- project's public version is listed with the project withheld, and filtering
-- it here would delete a row the profile is required to show (the header).
SELECT ra.pid,
       ra.title,
       ra.asset_type,
       rav.version,
       rav.visibility,
       avp.role,
       ra.origin_project_id::text,
       p.slug,
       p.name,
       p.visibility,
       rav.published_at
FROM asset_version_parties avp
JOIN research_asset_versions rav ON rav.id = avp.asset_version_id
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p ON p.id = ra.origin_project_id
WHERE avp.party_kind = 'user' AND avp.party_id = @user_id
  AND rav.visibility = 'public'
ORDER BY rav.published_at DESC, ra.pid, rav.version, avp.role
LIMIT @row_limit;

-- name: ListOrganizationAssetCredits :many
-- The published versions the projects of one organization produced. Read from
-- the version side rather than from asset_version_parties: the organization
-- surface states what an institution's projects PUBLISHED, not who is
-- credited on someone else's version.
--
-- project_id is the origin project, so the model can hold the version to the
-- set of PUBLIC projects it already rendered (BuildOrganizationProfile): an
-- asset is drawn on an organization's profile only when the project that
-- produced it may be named there, because this surface would otherwise
-- publish a link between the institution and a project it may not name.
-- role is absent by construction and is selected as an empty literal so this
-- read returns the same column list as ListPersonAssetCredits and the two
-- share one scanner.
--
-- Two render predicates, and the second is this surface's own rule: the
-- version must be public AND its origin project must be one of the
-- organization's PUBLIC projects — a version is never drawn on here when the
-- project that produced it may not be named (see OrganizationProfile.Assets).
-- The model holds the version to the set of public projects it rendered, so
-- everything this read returns for a public project is a row the model may
-- keep; a project outside that set is dropped there, which is the fail-closed
-- direction and not a leak.
SELECT ra.pid,
       ra.title,
       ra.asset_type,
       rav.version,
       rav.visibility,
       ''::text AS role,
       ra.origin_project_id::text,
       p.slug,
       p.name,
       p.visibility,
       rav.published_at
FROM research_asset_versions rav
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p ON p.id = ra.origin_project_id
WHERE p.organization_id = @organization_id
  AND p.visibility = 'public'
  AND rav.visibility = 'public'
ORDER BY rav.published_at DESC, ra.pid, rav.version
LIMIT @row_limit;

-- name: ListPersonReuses :many
-- The recorded usages of the versions one person is credited on — docs/42's
-- "used/derived public links" read from the far side: "who publicly uses this
-- version" (internal/assets/usage.go).
--
-- Both visibility axes of the USED version come back (its own and its asset's
-- origin project's), plus the USING project's own visibility, which is the
-- second condition internal/assets.PageUsage states: a private project may
-- use a public asset, and printing its name, slug or id would disclose the
-- existence of that project.
--
-- DISTINCT because a person credited twice on one version would otherwise
-- produce the same usage row twice. The usage is one fact about the using
-- project and the used version, and the credit that led here is not part of
-- it — the profile's assets dimension is where the credits themselves are
-- rendered, one per credit.
--
-- Three render predicates, which are exactly the three conditions the model
-- tests in this order: the declaration itself is public
-- (ad.visibility_of_usage), the version may be LINKED to — both it and its
-- asset's origin project public (internal/assets.mayLinkVersion: the URL this
-- row prints answers an existence-hiding 404 outside the project), and the
-- USING project is public (PageUsage's second condition: a private project
-- may use a public asset, and printing its name would disclose its
-- existence). All three must be here: this dimension links out of the
-- profile, so it is the one whose rows can be spent on links that would not
-- resolve.
SELECT DISTINCT ra.pid,
                ra.title,
                ra.asset_type,
                rav.version,
                rav.visibility AS version_visibility,
                p_asset.visibility AS version_project_visibility,
                ad.visibility_of_usage,
                ad.dependency_type,
                ad.created_at,
                p_use.id::text AS project_id,
                p_use.slug,
                p_use.name,
                p_use.visibility
FROM asset_version_parties avp
JOIN research_asset_versions rav ON rav.id = avp.asset_version_id
JOIN research_assets ra ON ra.id = rav.asset_id
JOIN projects p_asset ON p_asset.id = ra.origin_project_id
JOIN asset_dependencies ad ON ad.asset_version_id = rav.id
JOIN projects p_use ON p_use.id = ad.project_id
WHERE avp.party_kind = 'user' AND avp.party_id = @user_id
  AND ad.visibility_of_usage = 'public'
  AND rav.visibility = 'public'
  AND p_asset.visibility = 'public'
  AND p_use.visibility = 'public'
ORDER BY ad.created_at DESC, ra.pid, rav.version, p_use.slug
LIMIT @row_limit;

-- name: ListPersonReproductions :many
-- The evidence assertions one person created with a reproduction relation
-- (docs/10 §4): the two relations the profile's "reproduced evidence"
-- dimension is made of. Both are read and neither is preferred — docs/10 §4
-- ends with "V1 不自动赋数值权重", and a failed reproduction is evidence about
-- a claim, not a demerit for the person who recorded it.
--
-- No target object comes back. Naming what was reproduced would require the
-- knowledge-publish audience rule (knowledgepublish.AudienceFor), which is
-- that surface's application-layer policy; this read answers the part it can
-- answer on its own and the model renders the rest of the row.
--
-- The render predicate is the assertion's OWN axis, and it has to be here: an
-- assertion is a statement an author makes about someone else's work, and
-- 00091 gave evidence_assertions its own visibility column whose header says
-- "an assertion nothing explicitly made public is not rendered anywhere"
-- (DEFAULT 'private'). Both profile surfaces are ANONYMOUS reads, so a row
-- that crossed this boundary without its visibility being public would
-- disclose an assertion its author never published. The published-assertion
-- read of the knowledge surface (internal/persistence/queries/evidence.sql:73,
-- ListPublishedEvidenceForTarget, predicate at :109) applies the identical
-- predicate — these reads answer the same question of the same table and must
-- not disagree about who may read a row.
--
-- ea.visibility is SELECTED as well as filtered, on purpose: the model
-- re-checks the axis itself, and a re-check can only be independent of the
-- query's predicate if the query hands it the value. Two defences, not one.
--
-- The project's visibility is deliberately NOT a predicate here: a private
-- project's assertion is still the person's own act, and the model renders it
-- with the project withheld (`publicProject(row.ProjectID) == nil` withholds
-- the name, it does not drop the row — model.go). Filtering it here would
-- delete a row the profile is required to show, which is exactly the mistake
-- the header warns about.
SELECT ea.relation_type,
       ea.review_state,
       ea.created_at,
       ea.visibility AS assertion_visibility,
       ea.project_id::text,
       p.slug,
       p.name,
       p.visibility
FROM evidence_assertions ea
JOIN projects p ON p.id = ea.project_id
WHERE ea.created_by = @user_id
  AND ea.relation_type IN ('reproduces', 'fails_to_reproduce')
  AND ea.visibility = 'public'
ORDER BY ea.created_at DESC, ea.id DESC
LIMIT @row_limit;

-- name: ListOrganizationProjects :many
-- The projects one organization owns, in the organization's own naming order
-- (slug) — an institution's projects are listed as the institution named
-- them, not ranked by anything. Visibility is still selected even though it is
-- now filtered, so the model can re-check the row it renders (the same two
-- defences as ListPersonReproductions).
--
-- visibility = 'public' is the render predicate: BuildOrganizationProfile
-- keeps a project only when `isPublicVisibility(row.Visibility)`, so a private
-- project is a row this surface would drop, and letting such rows spend the
-- window would let a member hide the institution's public projects behind
-- private ones.
SELECT id, slug, name, purpose, activity_status, visibility
FROM projects
WHERE organization_id = @organization_id
  AND visibility = 'public'
ORDER BY slug, id
LIMIT @row_limit;
