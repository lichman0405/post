// Package explore owns the Explore surface (task T0802): the aggregated
// discovery index over the network's PUBLIC entities.
//
// # What it is
//
// docs/05 §6 fixes the six dimensions — Projects, Assets, Knowledge,
// People, Organizations, Open Contributions — and docs/14 §6 names the same
// six for the open network ("面向开放网络：Open Projects、Assets、Knowledge
// Objects、Open Contributions、People、Organizations"). docs/02 §3 makes the
// whole surface V1-required ("Explore Public
// Projects/Assets/Knowledge/People/Organizations", "Public web access
// without login"). This package answers that surface with ONE read: it
// aggregates the index the rest of the platform already publishes, rather
// than inventing a second opinion about what is public.
//
// # The index is the entities' own reads, not a new decision
//
// Every section is sourced from a read that already exists and already
// carries its own disclosure rule; this package reuses them instead of
// restating them (a second implementation of the rule in a layer nobody
// tests is what T0709 warned about, internal/assets/browse.go):
//
//   - Projects — projects.Service.List over ListPublicProjects
//     (docs/12 §2, T0106: the visibility predicate IS the read policy).
//   - Assets — the asset hub's browse read, whole: internal/assets
//     BuildBrowse over ListBrowseAssets (T0709). An asset is in the index
//     when it has a public version, and it names its project only when the
//     project is public.
//   - Open Contributions — contribution.Service.ListPublic over
//     ListPublicOpportunities (T0803): publicized AND open rows only, which
//     only the explicit, audited publicize action can produce (docs/12 §3).
//   - Knowledge — knowledge_publications rows: a knowledge object version
//     published onto the network. docs/12 §2 is what makes this a public
//     fact: "Private Project：project/RSG/private blobs 默认不可见；可显式
//     Publish Asset/Knowledge/Attestation" — a private project may publish,
//     so the publication is public while the project that published it may
//     not be. The asset rule is applied verbatim: the row is rendered, the
//     project is named only when the project itself is public. The query
//     never selects a private project's name, slug or id, so there is no
//     path by which one could reach the answer.
//   - People — users with a profile row, disabled accounts excluded.
//     docs/02 §3 ("注册、登录、公开 Research Profile") and T0102's acceptance
//     ("未登录可读公开 profile") make the profile a public read in V1; the
//     payload here renders identity fields only (handle, display name, bio)
//     and never the email, which is identity rather than profile
//     (internal/application/profile).
//   - Organizations — active organizations only (deactivated_at IS NULL;
//     migration 00018 makes deactivation the one "delete" this domain has).
//     V1 has no organization-private compartment (docs/12 §6 leaves
//     "organization-private compartment" to the future), so an active
//     organization is the public one, and this package renders no count of
//     members, projects or assets that could say anything else about it
//     (docs/23 §5: a private object's count may not leak through a public
//     API either).
//
// # Ranking: freshness, and nothing else
//
// docs/05 §6: "排序不得以'点赞数'为核心；允许 freshness、reuse、reproduction、
// match、curated relevance"; docs/14 §3 adds "不得主要按 popularity/star/
// organization prestige"; docs/13 §6 rules likes out of reputation
// altogether ("不使用 Like 数做信誉"). The index has one ranking input:
// freshness — each section is ordered by the time the entity entered the
// public index (creation for projects/people/organizations, publication for
// knowledge/contributions, latest publication for assets), newest first,
// ties broken by identity so two reads of one state render the same list.
// No like, star, vote or popularity field exists anywhere in this package,
// in the payload it renders, or in the schema behind it — and BuildIndex
// sorts the rows itself rather than trusting the readers' order, so the
// ranking is a property of this package and is pinned by its tests.
//
// # Layering
//
// This is an application-layer read: the service orchestrates against the
// Reader port (docs/52), and the transport (cmd/api/explorehttp) only
// translates. The PostgreSQL adapter for the three sections that had no
// read at all (knowledge, people, organizations) lives in store.go —
// subsystem-owned like internal/contribution's opportunity store (T0803),
// because T0802's allowed scope covers internal/application/** and not
// internal/persistence/**.
package explore
