package main

// gatePlanChecks is the declared plan checks: one per (operation, statement,
// table) that the hot reads go through at capacity.
//
// # How these were written
//
// Not from the indexes the migrations create, and not from the SQL text: from
// the statements the harness CAPTURED while running each operation for real
// (pgx.QueryTracer), and from the tables those statements' plans touch. The
// Match field is a sqlc `-- name:` marker, so a check is attached to a
// statement identity rather than to a fragment of text that a reformat would
// move out from under it; the report carries every captured plan verbatim, in
// both EXPLAIN modes, so which index each one chose is checkable by a reader.
//
// The first draft of this list failed the spec tier, and the failure is worth
// recording because it is the reason the checks assert a capability instead of
// an index. That draft named one specific index per read, taken from the plans
// observed at the ci tier. At the spec tier the planner picked different
// indexes for the same predicates — scientific_object_versions_object_id_version_no_key
// instead of scientific_object_versions_state_idx for the lineage read,
// branches_project_id_name_key instead of branches_project_created_idx for the
// branch list — both correctly, since two indexes can serve one predicate and
// which is cheaper moves with the statistics. Three checks went red on a
// database that is right. See PlanCheck for what replaced them.
//
// # What each check buys
//
// The spec tier's numbers are the reason the checks exist: at 100k relation
// versions and 10k state transitions, a read either has a way in through an
// index or it reads the table. The check is the deterministic half of that
// claim; the p95 next to it in the report is the other half.
//
// # What is deliberately NOT checked
//
// ListPublicProjects (projects.sql) sequentially scans `projects` even with
// enable_seqscan = off, because no index on `projects(visibility)` exists. At
// 101 projects that is the right plan and the measured cost is under 1ms, so
// there is no check to write and no index to add — but it is in the report's
// plans, and it is noted in RESULT.json as a finding rather than quietly
// dropped, because at a larger project count it is where the list would first
// degrade.
//
// # The blind spot this shape of check has, stated so it is not mistaken later
//
// "An index path exists" is a CAPABILITY, and a capability survives an index
// being dropped when a second index can serve the same predicate. Dropping
// relation_versions_state_idx and re-running leaves this gate GREEN — verified,
// not assumed — because the planner substitutes another index for it. So a
// green plan check does not mean every index the migration creates is still
// there; it means the read has not been left with no way in. What catches a
// dropped index is the migration's own CHOOSABLE-index assertion in
// tests/integration (research_profile_test.go), and what catches a slow read is
// the p95 in the same report.
//
// # What the natural plans do at the spec tier, since they surprise readers
//
// Several captured statements are sequentially scanned in their NATURAL plan
// (mode "natural" in the report) — the overview's relation lineage among them —
// even though an index path exists and is asserted. That is not a contradiction:
// at the spec tier the hot project holds 100,000 relation versions, which is
// every row of relation_versions, because docs/27:20's capacity baseline IS one
// project with 10k objects / 100k relations / 10k transitions / 1k branches. A
// query that wants essentially a whole table is right to scan it, and the index
// path matters for the shape a real network has, where those rows are spread
// across projects. The rule this leaves: read the report's plans, not just the
// check marks.
func gatePlanChecks() []PlanCheck {
	return []PlanCheck{
		{
			Operation: "project_overview",
			Match:     "-- name: ListObjectVersionsAsOf",
			Table:     "scientific_object_versions",
			Why: "ProjectOverview's outline reads every object version the project's state lineage carries " +
				"(rsg_query.sql:44, reached from internal/application/rsg/outline.go:178 through the " +
				"rsg.RSGQueries port to internal/persistence/rsg_query_store.go:95). The predicate is " +
				"`sov.state_id IN (SELECT ps.id FROM project_states ps WHERE ps.project_id = $1)`; at the spec " +
				"tier that is the hot project's 10,000 version rows out of the corpus's 110,000, so this read " +
				"has to be index-driven or the overview reads every version row in the database to find one " +
				"project's.",
		},
		{
			Operation: "project_overview",
			Match:     "-- name: ListRelationVersionsAsOf",
			Table:     "relation_versions",
			Why: "The overview's other half: the lineage's relation versions (rsg_query.sql:61, called from " +
				"outline.go:182). At the spec tier this is the largest single read in the whole workload — " +
				"100,000 relation versions in the hot project's lineage, each joining two object versions, two " +
				"objects and two project states — and the overview's measured p95 (a few hundred ms over budget, " +
				"per the same report's SLO COMPARISON) is almost entirely this read. Without an index path on " +
				"state_id it becomes a scan of the whole relation table per render.",
		},
		{
			Operation: "project_overview",
			Match:     "-- name: ListRelationVersionsAsOf",
			Table:     "project_states",
			Why: "The same statement joins project_states twice, once per endpoint, to report which project " +
				"carries each side of a relation. Both are looked up by primary key, and the check exists " +
				"because at 100k rows a nested loop that cannot reach project_states by id turns into one full " +
				"scan of the state table per relation row.",
		},
		{
			Operation: "project_overview",
			Match:     "-- name: ListStateCommitsByBranch",
			Table:     "state_commits",
			Why: "ProjectOverview lists every commit on the branch for its activity summary (rsg.sql:97, " +
				"through state_store.go:363 from overview.go:194). It has no LIMIT, so at the spec tier the " +
				"baseline is 10,000 rows per render and its `ORDER BY created_at, id` has to come from an " +
				"index rather than from sorting the table.",
		},
		{
			Operation: "project_overview",
			Match:     "-- name: ListBranchesByProject",
			Table:     "branches",
			Why: "Every project render lists the project's branches before it can render anything else " +
				"(rsg.sql:50, through branch_store.go:151 from overview.go:173). 1,100 branches at the spec " +
				"tier is not a latency problem, and its natural plan is a sequential scan the planner is right " +
				"to choose at that size — this is the check that the list keeps an index path as the branch " +
				"count grows, which is the only thing about it that could regress.",
		},
		{
			Operation: "object_detail",
			Match:     "-- name: ListScientificObjectVersions",
			Table:     "scientific_object_versions",
			Why: "GetObjectDetail reads the object's version log (scientific_objects.sql:66) and its latest " +
				"version. Both are point lookups on one object, and the page's measured p95 at the spec tier " +
				"is in the same report's MEASURED ITEMS table — a page that reads one object should not be " +
				"among the more expensive reads, which is what makes its plan worth asserting.",
		},
		{
			Operation: "object_detail",
			Match:     "-- name: ListRelationVersionsForObject",
			Table:     "relation_versions",
			Why: "ListRelationVersionsForObject (relations.sql:83) tests " +
				"`source_object_version_id = $1 OR target_object_version_id = $1`, so either 00006:22's source " +
				"index or its target index can serve it. This is the check that was originally pinned to the " +
				"source index and went red at the spec tier, where the planner chose the target one; what is " +
				"asserted now is the thing that is not a free choice, that the OR is served by an index at all. " +
				"With 100k relations against 10k objects, most of what the object page costs is this join.",
		},
		{
			Operation: "research_map",
			Match:     "-- name: ListObjectVersionsAsOf",
			Table:     "scientific_object_versions",
			Why: "docs/27:9's Research Map budget is the same read as the overview's outline " +
				"(cmd/api/rsghttp/researchmap.go: the map adds no read of its own), and at the spec tier the " +
				"map's p95 STRADDLES its 1s budget — observed between 925ms and 2287ms across runs, so it is " +
				"recorded as NOT met rather than as met-on-a-good-run (the report's SLO COMPARISON carries " +
				"this run's own number, whichever side of the budget it lands on). " +
				"Checking the read under BOTH operations is " +
				"deliberate: the map is the surface a reader experiences the budget on, and a check that named " +
				"only the overview would stay green if the map's route stopped sharing that read.",
		},
		{
			Operation: "research_map",
			Match:     "-- name: ListRelationVersionsAsOf",
			Table:     "relation_versions",
			Why: "Same reason as the object half directly above, for the map's relation lineage: the map's " +
				"other read, and the one that costs the most, asserted under the map's own operation so a " +
				"route change cannot move it out from under the check.",
		},
		{
			Operation: "candidate_retrieval",
			Match:     "-- name: SearchDocumentsFullText",
			Table:     "search_documents",
			Why: "The full-text signal is the only recall signal the production pipeline runs (no embedder is " +
				"wired), so docs/27:12's candidate-retrieval budget is decided here. The index is a GIN over " +
				"the same expression the query matches with plainto_tsquery('simple', ...); without it every " +
				"search reads the network projection's 100k rows. It is the one docs/27 budget the spec tier " +
				"is comfortably inside; the report carries the run's own p95 and ratio.",
		},
		{
			Operation: "probe_deep_offset",
			Match:     "-- name: SearchDocuments",
			Table:     "search_documents",
			Why: "The first of the two things this task was asked to verify: SearchDocuments " +
				"(internal/persistence/queries/search.sql:25) pages with `LIMIT @page_size OFFSET @page_offset` " +
				"(:45-46), and the probe asks for OFFSET 99,980 of the 100k-row corpus. Whether that is a " +
				"problem depends on whether the ordering comes from the index or from a sort of the whole " +
				"table, so the check asserts the index path and the probe's measured p95 in the same report " +
				"says what the paging costs: over a second at the spec tier, with the index in the plan — " +
				"the offset still has to walk the rows it skips, which is the finding, not the index's absence.",
		},
		{
			Operation: "probe_ranking_facts",
			Match:     "-- name: SearchRankingFacts",
			Table:     "scientific_object_versions",
			Why: "The second thing this task was asked to verify: SearchRankingFacts (search.sql:400-417) " +
				"carries a per-row correlated subquery at :412-417 that takes max(version_no) over " +
				"scientific_object_versions for each row. A correlated subquery per row is exactly the shape " +
				"that turns into a per-row scan, and at the ci tier the observed plan answered it with `Index " +
				"Only Scan Backward` plus `Limit`, i.e. one backward index probe per row rather than a scan. " +
				"The probe's measured p95 at the spec tier is a couple of milliseconds over 15 samples (the " +
				"same report's PROBES section), which is the timing half of the same answer: the subquery is " +
				"not what the ranking facts call costs.",
		},
	}
}
