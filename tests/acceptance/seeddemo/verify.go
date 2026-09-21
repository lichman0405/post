package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// check is one line of the demo checklist, measured. Every check carries the
// SQL it ran, so the number in the report can be re-derived by hand and a
// reader never has to trust the script.
type check struct {
	Item     string `json:"item"`
	Required int    `json:"required"`
	Actual   int    `json:"actual"`
	Status   string `json:"status"` // pass | fail | unavailable
	Detail   string `json:"detail,omitempty"`
	Query    string `json:"query"`
}

type verifyReport struct {
	Plan        string   `json:"plan"`
	Database    string   `json:"database"`
	ProjectID   string   `json:"project_id"`
	ProjectSlug string   `json:"project_slug"`
	Checks      []check  `json:"checks"`
	Failed      []string `json:"failed"`
	Unavailable []string `json:"unavailable"`
	PassedCount int      `json:"passed"`
	FailedCount int      `json:"failed_count"`
	ExternalOn  bool     `json:"external_checked"`
	Notes       []string `json:"notes"`
}

// floors are the minimums docs/34_SEED_DEMO_PROJECT.md states. They are
// floors, not targets: a build that exceeds them passes, a build that drops
// one item fails and the report names which class is short.
var objectFloors = []struct {
	Type  string
	Floor int
	Spec  string
}{
	{"material", 3, "docs/34 §Objects: 至少 3 Materials"},
	{"sample", 5, "docs/34 §Objects: 至少 5 Samples"},
	{"experiment", 8, "docs/34 §Objects: 至少 8 Experiments"},
	{"calculation", 6, "docs/34 §Objects: 至少 6 Calculations"},
	{"dataset", 5, "docs/34 §Objects: 至少 5 Datasets"},
	{"protocol", 1, "docs/34 §Objects: Protocol（另计 3 个版本）"},
	{"claim", 6, "docs/34 §Objects: 至少 6 Claims"},
	{"finding", 3, "docs/34 §Objects: 至少 3 Findings"},
	{"external_reference", 3, "docs/34 §Objects: 至少 3 External References"},
	{"research_question", 1, "docs/34 §Research Question"},
	{"hypothesis", 2, "docs/34 §Hypotheses: H1, H2"},
}

func runVerify(ctx context.Context, cfg verifyConfig) (*verifyReport, error) {
	db, err := newDiscover(ctx, cfg.DBURL)
	if err != nil {
		return nil, fmt.Errorf("connect to the database: %w", err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		return nil, fmt.Errorf("database %s is not reachable: %w", cfg.DBURL, err)
	}
	// The three lists start empty rather than nil so the JSON report carries
	// [] instead of null: a consumer reading it with jq (or any typed parser)
	// should not have to special-case "nothing failed" as a missing field.
	rep := &verifyReport{
		Plan: cfg.PlanPath, Database: redactURL(cfg.DBURL), ExternalOn: cfg.External,
		Failed: []string{}, Unavailable: []string{}, Notes: []string{},
	}
	pool := db.pool

	if err := pool.QueryRow(ctx, `select id, slug from projects where slug = $1`, cfg.ProjectSlug).
		Scan(&rep.ProjectID, &rep.ProjectSlug); err != nil {
		if err == pgx.ErrNoRows {
			rep.Checks = append(rep.Checks, check{
				Item: "project " + cfg.ProjectSlug, Required: 1, Actual: 0, Status: "fail",
				Detail: "the demo project does not exist in this database — nothing was built, or it was built elsewhere",
				Query:  fmt.Sprintf("select id, slug from projects where slug = '%s'", cfg.ProjectSlug),
			})
			rep.Failed = append(rep.Failed, "project "+cfg.ProjectSlug)
			rep.FailedCount = 1
			return rep, nil
		}
		return nil, err
	}
	rep.Checks = append(rep.Checks, check{
		Item: "project " + cfg.ProjectSlug, Required: 1, Actual: 1, Status: "pass",
		Query: fmt.Sprintf("select id, slug from projects where slug = '%s'", cfg.ProjectSlug),
	})

	add := func(c check) {
		if c.Status == "pass" {
			rep.PassedCount++
		} else {
			rep.FailedCount++
			rep.Failed = append(rep.Failed, c.Item)
		}
		if c.Status == "unavailable" {
			rep.Unavailable = append(rep.Unavailable, c.Item)
		}
		rep.Checks = append(rep.Checks, c)
	}

	// --- object counts, per docs/34's per-item checklist -------------------
	for _, f := range objectFloors {
		var n int
		q := fmt.Sprintf(`select count(*) from scientific_objects o
  join scientific_object_versions v on v.object_id = o.id and v.version_no = o.current_version_no
 where o.project_id = '%s' and o.object_type = '%s'`, rep.ProjectID, f.Type)
		if err := pool.QueryRow(ctx, `select count(*) from scientific_objects
  where project_id = $1 and object_type = $2`, rep.ProjectID, f.Type).Scan(&n); err != nil {
			return nil, err
		}
		c := check{Item: f.Type + " (" + f.Spec + ")", Required: f.Floor, Actual: n, Query: collapse(q)}
		if n >= f.Floor {
			c.Status = "pass"
		} else {
			c.Status = "fail"
			c.Detail = fmt.Sprintf("missing %d %s", f.Floor-n, f.Type)
		}
		add(c)
	}

	// --- protocol versions -------------------------------------------------
	var protocolVersions int
	if err := pool.QueryRow(ctx, `
		select count(*) from scientific_object_versions v
		join scientific_objects o on o.id = v.object_id
		where o.project_id = $1 and o.object_type = 'protocol'`, rep.ProjectID).Scan(&protocolVersions); err != nil {
		return nil, err
	}
	add(check{
		Item: "protocol versions (docs/34 §Objects: 至少 3 Protocol versions)", Required: 3, Actual: protocolVersions,
		Status: passFail(protocolVersions >= 3),
		Query: collapse(`select count(*) from scientific_object_versions v
  join scientific_objects o on o.id = v.object_id
 where o.project_id = '<project>' and o.object_type = 'protocol'`),
	})

	// --- branches ----------------------------------------------------------
	wantBranches := []string{"humidity-40rh", "humidity-70rh", "mechanism-water-binding", "protocol-activation-180c"}
	var branchCount int
	if err := pool.QueryRow(ctx, `select count(*) from branches where project_id = $1 and name = any($2)`,
		rep.ProjectID, wantBranches).Scan(&branchCount); err != nil {
		return nil, err
	}
	add(check{
		Item:     "branches (docs/34 §Branches: humidity-40rh, humidity-70rh, mechanism-water-binding, protocol-activation-180c)",
		Required: 4, Actual: branchCount, Status: passFail(branchCount >= 4),
		Query: collapse(`select count(*) from branches where project_id = '<project>'
  and name = any(array['humidity-40rh','humidity-70rh','mechanism-water-binding','protocol-activation-180c'])`),
	})

	// --- pull requests -----------------------------------------------------
	var mergedPRs int
	if err := pool.QueryRow(ctx, `select count(*) from pull_requests where project_id = $1 and state = 'merged'`,
		rep.ProjectID).Scan(&mergedPRs); err != nil {
		return nil, err
	}
	add(check{
		Item: "pull request with a conflict-free merge (docs/34 §PR)", Required: 1, Actual: mergedPRs,
		Status: passFail(mergedPRs >= 1),
		Query:  collapse(`select count(*) from pull_requests where project_id = '<project>' and state = 'merged'`),
	})

	// The scientific conflict: an open PR whose source branch and main hold
	// different values of the same protocol parameter. That is the fact the
	// merge engine classifies; the builder additionally records the product's
	// own refusal verbatim in the build report.
	var conflictPRs int
	if err := pool.QueryRow(ctx, `
		with divergent as (
		  select v.payload -> 'parameters' ->> 'temperature_c' as t, v.branch_id
		  from scientific_object_versions v
		  join scientific_objects o on o.id = v.object_id
		  where o.project_id = $1 and o.object_type = 'protocol'
		)
		select count(distinct pr.id)
		from pull_requests pr
		join branches src on src.id = pr.source_branch_id
		join branches tgt on tgt.id = pr.target_branch_id
		where pr.project_id = $1 and pr.state <> 'merged'
		  and src.name = 'protocol-activation-180c'
		  and exists (select 1 from divergent d1 where d1.branch_id = src.id)
		  and exists (select 1 from divergent d2 where d2.branch_id = tgt.id and d2.t is not null
		              and d2.t is distinct from (select d1.t from divergent d1 where d1.branch_id = src.id limit 1))`,
		rep.ProjectID).Scan(&conflictPRs); err != nil {
		return nil, err
	}
	add(check{
		Item: "pull request with a protocol scientific conflict (docs/34 §PR)", Required: 1, Actual: conflictPRs,
		Status: passFail(conflictPRs >= 1),
		Query:  collapse("open PR from protocol-activation-180c to main where both branches carry a protocol version with a different parameters.temperature_c"),
	})

	// Selective publication: a private branch whose proposed versions were
	// published rather than merged wholesale.
	var publishedFromPrivate int
	if err := pool.QueryRow(ctx, `
		select count(*) from knowledge_publications kp
		join scientific_object_versions v on v.id = kp.object_version_id
		join scientific_objects o on o.id = v.object_id
		join branches b on b.id = v.branch_id
		where o.project_id = $1 and b.visibility = 'private'`, rep.ProjectID).Scan(&publishedFromPrivate); err != nil {
		return nil, err
	}
	add(check{
		Item: "selective publication from the private branch (docs/34 §PR)", Required: 1, Actual: publishedFromPrivate,
		Status: passFail(publishedFromPrivate >= 1),
		Query:  collapse(`select count(*) from knowledge_publications kp join scientific_object_versions v on v.id = kp.object_version_id join scientific_objects o on o.id = v.object_id join branches b on b.id = v.branch_id where o.project_id = '<project>' and b.visibility = 'private'`),
	})

	// --- releases ----------------------------------------------------------
	var releases int
	if err := pool.QueryRow(ctx, `select count(*) from releases where project_id = $1 and version = any($2)`,
		rep.ProjectID, []string{"R0.1", "R1.0"}).Scan(&releases); err != nil {
		return nil, err
	}
	add(check{
		Item: "releases R0.1 and R1.0 (docs/34 §Releases)", Required: 2, Actual: releases,
		Status: passFail(releases >= 2),
		Query:  collapse(`select count(*) from releases where project_id = '<project>' and version = any(array['R0.1','R1.0'])`),
	})

	// --- assets: the four V1 types ----------------------------------------
	var assetTypes int
	if err := pool.QueryRow(ctx, `select count(distinct asset_type) from research_assets where origin_project_id = $1`,
		rep.ProjectID).Scan(&assetTypes); err != nil {
		return nil, err
	}
	var assetTotal int
	if err := pool.QueryRow(ctx, `select count(*) from research_assets where origin_project_id = $1`,
		rep.ProjectID).Scan(&assetTotal); err != nil {
		return nil, err
	}
	add(check{
		Item:     "assets of all four V1 types (docs/34 §Assets: Dataset/Protocol/Material Collection/Benchmark)",
		Required: 4, Actual: assetTypes, Status: passFail(assetTypes >= 4),
		Detail: fmt.Sprintf("%d assets published in total", assetTotal),
		Query:  collapse(`select count(distinct asset_type) from research_assets where origin_project_id = '<project>'`),
	})

	// --- evidence kinds ----------------------------------------------------
	evidenceKinds := map[string]string{
		"supporting":      "supports",
		"contradicting":   "contradicts",
		"consistent_with": "consistent_with",
		"reproduction":    "reproduces",
	}
	for label, rel := range evidenceKinds {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from evidence_assertions where project_id = $1 and relation_type = $2`,
			rep.ProjectID, rel).Scan(&n); err != nil {
			return nil, err
		}
		detail := ""
		if n == 0 {
			detail = "no evidence assertion of this kind was recorded"
		}
		add(check{
			Item: "evidence kind " + label + " (" + rel + ") (docs/34 §Evidence)", Required: 1, Actual: n,
			Status: passFail(n >= 1), Detail: detail,
			Query: collapse(fmt.Sprintf(`select count(*) from evidence_assertions where project_id = '<project>' and relation_type = '%s'`, rel)),
		})
	}

	// --- the deliberately contested finding -------------------------------
	// Read from the VERSION PAYLOAD, not from the findings projection table.
	// The projection (infra/migrations/00057) is a rebuildable queryable face
	// of exactly this field, and migration 00057 says where the truth is:
	// "the payload in scientific_object_versions stays the historical source
	// of truth". Nothing in this build writes the projection — after a full
	// demo build `select count(*) from findings` is 0 while every finding
	// object exists with its assessment — so a check reading it would report
	// a shortfall the seed cannot fix through any product route, and would
	// report it whether or not the contested finding was created. Reported as
	// a follow-up issue; the payload is the field's own source, and this check
	// goes red the moment the contested finding is dropped from the plan.
	var contestedFindings int
	if err := pool.QueryRow(ctx, `
		select count(*) from scientific_objects o
		join scientific_object_versions v on v.object_id = o.id and v.version_no = o.current_version_no
		where o.project_id = $1 and o.object_type = 'finding'
		  and v.payload ->> 'assessment' = 'contested'`, rep.ProjectID).Scan(&contestedFindings); err != nil {
		return nil, err
	}
	add(check{
		Item:     "deliberately contested finding (docs/34 §Evidence: 故意构造一个 contested finding)",
		Required: 1, Actual: contestedFindings, Status: passFail(contestedFindings >= 1),
		Detail: "read from the finding version's payload; the findings projection table (00057) has no writer in this build — see the follow-up issue",
		Query:  collapse(`select count(*) from scientific_objects o join scientific_object_versions v on v.object_id = o.id and v.version_no = o.current_version_no where o.project_id = '<project>' and o.object_type = 'finding' and v.payload ->> 'assessment' = 'contested'`),
	})

	// --- everything went through the product path -------------------------
	// Every seeded object version must have been written by a state commit
	// whose origin is the API. A row written by any other path (a script, a
	// fixture loader, a git compatibility import) fails this check.
	var nonAPI int
	if err := pool.QueryRow(ctx, `
		select count(*) from scientific_object_versions v
		join scientific_objects o on o.id = v.object_id
		where o.project_id = $1
		  and not exists (select 1 from state_commits c where c.result_state_id = v.state_id and c.via = 'api')`,
		rep.ProjectID).Scan(&nonAPI); err != nil {
		return nil, err
	}
	add(check{
		Item:     "every object version came from an API state commit (state_commits.via = 'api')",
		Required: 0, Actual: nonAPI, Status: passFail(nonAPI == 0),
		Detail: "the number of versions NOT backed by an api-originated state commit",
		Query:  collapse(`select count(*) from scientific_object_versions v join scientific_objects o on o.id = v.object_id where o.project_id = '<project>' and not exists (select 1 from state_commits c where c.result_state_id = v.state_id and c.via = 'api')`),
	})

	// --- the synthetic marker is selectable in one query ------------------
	var marked, totalObjects int
	if err := pool.QueryRow(ctx, `
		select
		  count(*) filter (where v.payload -> 'tags' ? 'synthetic'
		                     and v.payload -> 'metadata' ->> 'seed_key' is not null),
		  count(*)
		from scientific_objects o
		join scientific_object_versions v on v.object_id = o.id and v.version_no = o.current_version_no
		where o.project_id = $1`, rep.ProjectID).Scan(&marked, &totalObjects); err != nil {
		return nil, err
	}
	add(check{
		Item:     "synthetic marker on every seeded object (docs/34 + examples/seed-demo/README.md)",
		Required: totalObjects, Actual: marked, Status: passFail(marked == totalObjects && totalObjects > 0),
		Detail: "objects carrying tags['synthetic'] and metadata.seed_key",
		Query:  collapse(`select count(*) filter (where v.payload -> 'tags' ? 'synthetic' and v.payload -> 'metadata' ->> 'seed_key' is not null) from scientific_objects o join scientific_object_versions v on v.object_id = o.id and v.version_no = o.current_version_no where o.project_id = '<project>'`),
	})

	// --- the external contribution ----------------------------------------
	if !cfg.External {
		for _, item := range []string{
			"external fork (docs/34 §External contribution)",
			"external contradictory evidence (evidence_origin = 'external')",
			"the fork's reference to the parent's H1 (docs/34 §External contribution: Reference H1)",
			"external pull request into the parent",
		} {
			rep.Checks = append(rep.Checks, check{
				Item: item, Required: 1, Actual: 0, Status: "unavailable",
				Detail: "the verifier ran with --external=0, so the fork-dependent items were not checked",
			})
			rep.Unavailable = append(rep.Unavailable, item)
		}
	} else {
		var forks int
		if err := pool.QueryRow(ctx, `
			select count(*) from project_forks f
			join users u on u.id = f.forked_by
			where f.parent_project_id = $1 and u.email = $2`, rep.ProjectID, cfg.ExternalEmail).Scan(&forks); err != nil {
			return nil, err
		}
		add(check{
			Item: "external fork (docs/34 §External contribution: 第二个 demo user/org Fork)", Required: 1, Actual: forks,
			Status: passFail(forks >= 1),
			Query:  collapse(`select count(*) from project_forks f join users u on u.id = f.forked_by where f.parent_project_id = '<project>' and u.email = '<external>'`),
		})

		// The contradictory assertion specifically: docs/34 asks the external
		// group for "一个 contradictory experiment evidence", so the relation
		// is part of the requirement, not an editorial choice. Counting any
		// external-origin assertion would pass on a reference to H1 alone.
		var externalContradiction int
		if err := pool.QueryRow(ctx, `
			select count(*) from evidence_assertions ea
			where ea.evidence_origin = 'external'
			  and ea.relation_type = 'contradicts'
			  and ea.target_object_version_id in (
			    select v.id from scientific_object_versions v
			    join scientific_objects o on o.id = v.object_id
			    where o.project_id = $1)`, rep.ProjectID).Scan(&externalContradiction); err != nil {
			return nil, err
		}
		add(check{
			Item:     "external contradictory evidence against the parent's claim (docs/34 §External contribution)",
			Required: 1, Actual: externalContradiction, Status: passFail(externalContradiction >= 1),
			Detail: "assertions whose evidence_origin is 'external', whose relation is 'contradicts', and whose target is a version of this project",
			Query:  collapse(`select count(*) from evidence_assertions ea where ea.evidence_origin = 'external' and ea.relation_type = 'contradicts' and ea.target_object_version_id in (select v.id from scientific_object_versions v join scientific_objects o on o.id = v.object_id where o.project_id = '<project>')`),
		})

		// The fork references the parent's H1. This is the "Reference H1" half
		// of docs/34's external contribution, and it is an EVIDENCE ASSERTION
		// rather than an RSG relation because that is the only cross-project
		// reference this product provides: a relation endpoint must be a
		// version of the same project (internal/application/rsg's
		// requireEndpoint answers OBJECT_VERSION_NOT_FOUND otherwise), while an
		// evidence assertion may target another project's published version and
		// is then recorded with evidence_origin = 'external'.
		//
		// The target is identified by the marker the seed stamped on the
		// object, not by a hardcoded uuid: the object has to be the H1 the plan
		// declares.
		var h1Refs int
		if err := pool.QueryRow(ctx, `
			select count(*) from evidence_assertions ea
			join scientific_object_versions v on v.id = ea.target_object_version_id
			join scientific_objects o on o.id = v.object_id
			where ea.evidence_origin = 'external'
			  and o.project_id = $1
			  and o.object_type = 'hypothesis'
			  and v.payload -> 'metadata' ->> 'seed_key' = 'hyp-h1'`, rep.ProjectID).Scan(&h1Refs); err != nil {
			return nil, err
		}
		add(check{
			Item:     "the fork's reference to the parent's H1 (docs/34 §External contribution: Reference H1)",
			Required: 1, Actual: h1Refs, Status: passFail(h1Refs >= 1),
			Detail: "external-origin assertions whose target is this project's H1 hypothesis version",
			Query:  collapse(`select count(*) from evidence_assertions ea join scientific_object_versions v on v.id = ea.target_object_version_id join scientific_objects o on o.id = v.object_id where ea.evidence_origin = 'external' and o.project_id = '<project>' and o.object_type = 'hypothesis' and v.payload -> 'metadata' ->> 'seed_key' = 'hyp-h1'`),
		})

		var externalPRs int
		if err := pool.QueryRow(ctx, `
			select count(*) from pull_requests pr
			join project_forks f on f.fork_project_id = (select b.project_id from branches b where b.id = pr.source_branch_id)
			where pr.project_id = $1 and f.parent_project_id = $1`, rep.ProjectID).Scan(&externalPRs); err != nil {
			return nil, err
		}
		add(check{
			Item:     "external pull request from the fork into the parent (docs/34 §External contribution)",
			Required: 1, Actual: externalPRs, Status: passFail(externalPRs >= 1),
			Query: collapse(`select count(*) from pull_requests pr join project_forks f on f.fork_project_id = (select b.project_id from branches b where b.id = pr.source_branch_id) where pr.project_id = '<project>' and f.parent_project_id = '<project>'`),
		})
	}

	rep.Notes = append(rep.Notes,
		fmt.Sprintf("%d checks passed, %d failed, %d not checked", rep.PassedCount, rep.FailedCount, len(rep.Unavailable)))
	return rep, nil
}

type verifyConfig struct {
	DBURL         string
	PlanPath      string
	ProjectSlug   string
	External      bool
	ExternalEmail string
}

func passFail(ok bool) string {
	if ok {
		return "pass"
	}
	return "fail"
}

func collapse(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// redactURL keeps a password out of the report.
func redactURL(u string) string {
	at := strings.LastIndex(u, "@")
	scheme := strings.Index(u, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return u
	}
	return u[:scheme+3] + "***" + u[at:]
}
