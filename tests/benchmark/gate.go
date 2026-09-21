package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// GateCheck is one deterministic check's outcome.
type GateCheck struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	// Detail is what the check observed: the actual counts, the plan lines it
	// read, the ceiling it applied. It is printed on success too — a check that
	// only speaks when it fails cannot be audited.
	Detail string `json:"detail"`
}

// GateResult is the whole deterministic gate.
type GateResult struct {
	Passed bool        `json:"passed"`
	Checks []GateCheck `json:"checks"`
	// Plans is every plan the gate EXPLAINed, verbatim, whether or not a check
	// depended on it. A plan check is only as trustworthy as the plan it read,
	// so the plans travel with the verdict.
	Plans []PlanReport `json:"plans"`
}

// PlanReport is one EXPLAIN of one statement an operation actually issued.
type PlanReport struct {
	Operation string `json:"operation"`
	Statement string `json:"statement"`
	// Mode says how the planner was configured for this EXPLAIN:
	//
	//	"natural"     — nothing changed; this is the plan production runs.
	//	"no_seqscan"  — SET LOCAL enable_seqscan = off.
	//
	// Both are reported. The plan CHECKS read the no_seqscan one, because the
	// question a plan check can answer deterministically is "is this index
	// CHOOSABLE for this query", and at a small tier the natural plan is a
	// sequential scan the planner is right to choose. Read PlanCheck for why
	// that is the question worth asking, and the report's own timing table for
	// the other one.
	Mode string `json:"mode"`
	Plan string `json:"plan"`
	// Error is set when the EXPLAIN itself failed. It is never treated as a
	// pass: a statement that cannot be explained is a statement whose plan
	// nobody has seen.
	Error string `json:"error,omitempty"`
}

// expectedCounts is the corpus a Scale asks for, as exact table counts.
//
// Exact, not "at least", because every one of these numbers is a sentence in
// docs/27:20 or a direct consequence of one, and a seed that quietly produced
// a tenth of the rows would make every latency in the report describe a
// smaller database than the budget is written for. The seed's own row counts
// are already the second opinion — this table is the first.
//
// The relations:
//
//	projects              = 1 hot + NetworkProjects
//	project_memberships   = 1 owner + 1 actor on the hot project, 1 owner each elsewhere
//	branches              = HotBranches hot + 1 main each for NetworkProjects
//	project_states        = one genesis per branch + HotStateCommits chain states
//	state_commits         = HotStateCommits (the chain; a genesis state has no commit)
//	scientific_objects    = HotObjects + one per network document
//	…_versions            = the same, one version each
//	relations/…_versions  = HotRelations, one version each
//	search_documents      = NetworkDocuments (the hot project carries none)
//	evidence_assertions   = 0 (nothing in the corpus authors one)
func expectedCounts(sc Scale) map[string]int {
	return map[string]int{
		"users":                      2,
		"projects":                   1 + sc.NetworkProjects,
		"project_memberships":        sc.NetworkProjects + 2,
		"branches":                   sc.HotBranches + sc.NetworkProjects,
		"project_states":             sc.HotBranches + sc.HotStateCommits + sc.NetworkProjects,
		"state_commits":              sc.HotStateCommits,
		"scientific_objects":         sc.HotObjects + sc.NetworkDocuments,
		"scientific_object_versions": sc.HotObjects + sc.NetworkDocuments,
		"relations":                  sc.HotRelations,
		"relation_versions":          sc.HotRelations,
		"pull_requests":              sc.HotPullRequests,
		"search_documents":           sc.NetworkDocuments,
		"evidence_assertions":        0,
	}
}

// PlanCheck declares what one captured statement's plan has to be able to do.
//
// # What it asserts: an index path, not an index
//
// "Under `SET LOCAL enable_seqscan = off`, this statement's plan does not
// sequentially scan table T." With sequential scanning switched off, the
// planner uses a sequential scan only when NO index can serve the query — it
// still falls back to one rather than failing. So the assertion is exactly
// "table T has an index path for this query", which is a capability, and it is
// the capability docs/27:20's capacity baseline needs: at 100k relation rows a
// read either has a way in through an index or it reads the table.
//
// This replaced an earlier version that named a SPECIFIC index per check,
// written from the ci tier's observed plans. It was wrong, and the first
// spec-tier run is the evidence: at 110k version rows the planner reads
// scientific_object_versions through scientific_object_versions_object_id_version_no_key
// where at 2.4k rows it read scientific_object_versions_state_idx, and it reads
// branches through branches_project_id_name_key where at 50 rows it read
// branches_project_created_idx. Both choices are legitimate — two indexes can
// serve the same predicate, and which is cheaper depends on the statistics —
// so a check that named one of them went red on a correct database at the tier
// the budget is written for. A gate that pins a plan is also a gate that fails
// when the planner improves, which is the same objection
// tests/integration/research_profile_test.go raises when it checks that a
// migration's index is CHOOSABLE rather than that a query chose it.
//
// What the specific-index version was trying to say is still said, in the
// report: every captured statement's plan is carried verbatim, in both modes,
// so which index each query chose is visible whether or not a check named it.
type PlanCheck struct {
	// Operation is the measured operation whose captured statement this
	// describes. Naming it is what keeps the check attached to a real caller.
	Operation string
	// Match is a substring that identifies the statement: the sqlc `-- name:`
	// marker, not a fragment of SQL, so a query that is reformatted does not
	// silently stop matching. If no captured statement matches, the check
	// FAILS — a plan check that stops covering anything must not pass by
	// finding nothing to look at.
	Match string
	// Table is the table the plan must reach through an index rather than a
	// sequential scan.
	Table string
	// Why is the measurement this check exists because of.
	Why string
}

// seqScanOn reports whether a plan sequentially scans table, reading the plan's
// own lines. The table name is compared as a whole token: `Seq Scan on
// branches` must not be satisfied by a plan that scans a table whose name
// merely starts with it.
func seqScanOn(plan, table string) bool {
	for _, line := range strings.Split(plan, "\n") {
		i := strings.Index(line, "Seq Scan on ")
		if i < 0 {
			continue
		}
		rest := line[i+len("Seq Scan on "):]
		name, _, _ := strings.Cut(rest, " ")
		if strings.TrimSpace(name) == table {
			return true
		}
	}
	return false
}

// Label names the check in the report: the table it asserts an index path for.
func (pc PlanCheck) Label() string { return pc.Table }

// planChecks is empty until the first run's plans are read. It is filled by
// gatePlanChecks in plans.go, which is where the observed plans are recorded
// next to the expectations they justify.
var planChecks = gatePlanChecks()

// regressionCeiling is a REGRESSION CEILING, not an SLO.
//
// docs/27's budgets are engineering targets for a development machine
// (docs/27:3 "V1 是工程目标，不承诺公网 SLA"). Asserting them on a shared CI
// runner would produce a gate that goes red when the runner is busy, and a
// gate that goes red at random is a gate somebody eventually loosens — which
// CLAUDE.md forbids in as many words (禁止为让 Gate 通过 放宽 assertion). So
// this ceiling is set an order of magnitude above the budget: passing it means
// "no collapse", and the report's SLO table is where "is the budget met" is
// answered.
//
// 20× is chosen against the smallest budget in play (400ms): 8 seconds is far
// beyond any correct plan on this corpus and far below the cost of a lost
// index on 100k rows, which is what the ceiling is for. It is a factor and not
// an absolute so that an operation added later cannot accidentally inherit a
// ceiling sized for a different query.
//
// Probes have no docs/27 budget, so their ceilings are stated one by one in
// probeCeilings and labelled as this harness's own.
const regressionFactor = 20

// probeCeilings are the harness's own ceilings for the measured items docs/27
// gives no budget to. Each one is an order-of-magnitude tripwire, not a target:
// they exist so a probe that goes from "slow" to "broken" is a red gate rather
// than a footnote in a report nobody re-reads.
var probeCeilings = map[string]time.Duration{
	// The deepest page of a 100k-row corpus. A sequential scan with an offset
	// has to walk 100k rows; the ceiling admits a slow plan and refuses one
	// that has to touch the heap 100k times over.
	"probe_deep_offset": 10 * time.Second,
	// One answer page of ranking facts. The correlated subquery is per row of
	// the page, so this is a small number of index probes.
	"probe_ranking_facts": 5 * time.Second,
	// One EXACT vector scan: the column is compared against every embedded
	// document in scope. This is the item 决定三 is about, and its ceiling is
	// deliberately loose — the docs/27:12 candidate-retrieval budget is what
	// the report compares it to.
	"vector_scan": 20 * time.Second,
}

// The deterministic half runs in five stages, and the ORDER is part of the
// design rather than an implementation detail:
//
//	0. selfChecks   — the harness's own gate logic (see selfcheck.go).
//	1. gateCorpus   — the database really holds the rows docs/27:20 asks for.
//	2. gatePlans    — every measured operation, run once, really issues the
//	                  statement each plan check describes, and that statement's
//	                  plan can use the index it is supposed to.
//	3. (measure)
//	4. gateCeilings — nothing collapsed by an order of magnitude.
//
// gateTimings rides beside them: it is not a check about the database or about
// a query but about this report's own accounting.
//
// The corpus check has to run before the plan check because the plan check's
// capture pass INCLUDES the semantic-command write: a corpus check run after it
// would be checking the corpus the capture left behind, and would go red by
// exactly one row per capture. Splitting the stages is what makes that ordering
// possible; a single runGate could not, because it would have to hold the
// measurement it needs for stage 4 while performing stages 1 and 2.

// gateCorpus is stage 1.
//
// # Why the three appended tables are checked as a LOWER bound
//
// semantic_command is a WRITE: every call appends a project state, an object
// version and a state commit. A run makes 1 + Warmup + 1 + Samples of them per
// writing operation (the capture pass, the warmup calls, the cold sample and
// the timed samples), and they are not undone — the append-only guard on those
// tables is a product invariant and the harness does not fight it. So after a
// run the corpus is a known number of rows larger than the seed produced, and an
// EXACT check would call a correct database wrong on any second run over the
// same corpus — which is what `--no-seed` exists to do.
//
// # Why the upper bound is not the check
//
// The first version of this check banded those tables to `seeded <= got <=
// seeded + writeAllowance`, i.e. one run's worth of appends. That is a band only
// one run wide, and every `--no-seed` re-run appends another run's worth, so the
// THIRD run over an untouched ci-tier corpus went red on three tables with a
// database that was entirely correct. The excess was the harness's own earlier
// writes. A gate that goes red on a correct database is a gate somebody
// eventually loosens (CLAUDE.md: 禁止为让 Gate 通过 放宽 assertion), so the pass
// condition for an appendable table is now `got >= seeded`, and writeAllowance —
// whose meaning is unchanged, one run's appends — is used to CLASSIFY the excess
// in the message instead of to bound it:
//
//	got == seeded              the seed's rows and nothing else: nothing has run here yet
//	seeded < got <= +allowance ONE run's own appends
//	got > seeded + allowance   MORE than one run's appends: this corpus has been
//	                           measured against before
//
// All three pass, and the message says which one it is, so "correct" and "grown
// by earlier runs" are two different sentences rather than one ambiguous tick.
// The job the dropped upper bound would have had — noticing that a read
// collapsed — belongs to the regression ceilings in stage 4, which are applied
// per operation and do not depend on the corpus being pristine.
//
// What still fails is a table SHORT of rows, which is a seed that did not finish.
// The tables no operation writes (projects, branches, relations, …) are still
// checked exactly: growth is only legitimate where something appends.
func gateCorpus(ctx context.Context, w *Workload, sc Scale) ([]GateCheck, error) {
	// A fresh read of the live tables. The seed's in-memory tally would pass
	// even if every COPY had silently gone nowhere, which is the failure mode
	// a corpus check exists for.
	if err := countRows(ctx, w.Pool, w.Corpus); err != nil {
		return nil, err
	}
	want := expectedCounts(sc)
	allowance := writeAllowance(w)
	names := make([]string, 0, len(want))
	for name := range want {
		names = append(names, name)
	}
	sort.Strings(names)

	checks := make([]GateCheck, 0, len(names))
	for _, name := range names {
		got := w.Corpus.Counts[name]
		passed, d := corpusVerdict(name, want[name], got, allowance[name])
		checks = append(checks, GateCheck{Name: "corpus_rows:" + name, Passed: passed, Detail: d})
	}
	return checks, nil
}

// corpusVerdict is the corpus check's decision, as a pure function of the three
// numbers it has, so that the sequence of runs it has to admit can be asserted
// without a database (selfcheck.go does exactly that: seeded, then one run, then
// two, then three, all pass — the sequence the previous band failed on the
// third).
//
// `extra` is writeAllowance's entry for this table: how many rows ONE run of the
// workload can append to it. Zero means nothing writes it, and zero is also what
// makes the check exact — the two cases are "an appendable table" and "a table
// whose count is a property of the scale alone", and they must not be confused,
// which is why this is a function and not an `if` at the call site.
func corpusVerdict(name string, seeded, got, extra int) (bool, string) {
	if extra == 0 {
		if got == seeded {
			return true, fmt.Sprintf("%s = %d, want %d: exact (nothing in the workload writes this table)", name, got, seeded)
		}
		return false, fmt.Sprintf("%s = %d, want %d: EXACT mismatch, off by %d. No operation in this workload "+
			"appends here, so the count is a property of the scale and nothing else can explain a difference",
			name, got, seeded, got-seeded)
	}
	switch {
	case got < seeded:
		return false, fmt.Sprintf("%s = %d, want at least %d: SHORT by %d. The corpus does not hold the rows "+
			"this scale asks for — a seed that did not finish. (More rows than the seed produced is legal "+
			"here; fewer is not.)", name, got, seeded, seeded-got)
	case got == seeded:
		return true, fmt.Sprintf("%s = %d, want at least %d: the seed's rows exactly — nothing has run "+
			"against this corpus yet", name, got, seeded)
	case got <= seeded+extra:
		return true, fmt.Sprintf("%s = %d, want at least %d: %d rows past the seed, which is ONE run's own "+
			"appends to an append-only table (nothing else writes it, and they cannot be removed) — "+
			"this is a `--no-seed` re-run over a corpus that is right", name, got, seeded, got-seeded)
	default:
		return true, fmt.Sprintf("%s = %d, want at least %d: %d rows past the seed, which is MORE than one "+
			"run's appends (one run appends at most %d). The corpus is correct and has been measured "+
			"against before; the excess is this harness's own earlier writes, not a corpus defect. "+
			"Only a shortness check is applied to this table", name, got, seeded, got-seeded, extra)
	}
}

// writeAllowance is how many rows each table can gain from one full run of the
// workload, from the operations' own definitions rather than from a constant.
//
// It is table-keyed because a write is not one row: one semantic command appends
// to three tables, each by one row, and a table nobody writes to gets no entry
// (and therefore an exact check).
func writeAllowance(w *Workload) map[string]int {
	var writes int
	for _, op := range w.operations() {
		if !op.Writes {
			continue
		}
		// 1 for the plan stage's capture pass, Warmup, 1 for the cold sample,
		// Samples for the timed ones. Every one of them is a call the harness
		// makes and accounts for.
		writes += 1 + op.Warmup + 1 + op.Samples
	}
	if writes == 0 {
		return nil
	}
	return map[string]int{
		"project_states":             writes,
		"state_commits":              writes,
		"scientific_object_versions": writes,
	}
}

// gatePlans is stage 2. It returns the checks and every plan it read.
func gatePlans(ctx context.Context, w *Workload) ([]GateCheck, []PlanReport, error) {
	captured, err := capturePass(ctx, w)
	if err != nil {
		return nil, nil, err
	}

	var plans []PlanReport
	for _, op := range append(w.operations(), w.probes()...) {
		for _, st := range captured[op.Name] {
			if !explainable(st.SQL) {
				plans = append(plans, PlanReport{
					Operation: op.Name, Statement: st.SQL, Mode: modeNatural,
					Error: "not a read statement; nothing to EXPLAIN",
				})
				continue
			}
			plan, planErr := explain(ctx, w, st.SQL, st.Args)
			rep := PlanReport{Operation: op.Name, Statement: st.SQL, Mode: modeNatural, Plan: plan}
			if planErr != nil {
				rep.Error = planErr.Error()
			}
			plans = append(plans, rep)
		}
	}

	// The index-availability pass: every read is planned a second time with
	// sequential scans discouraged, in ONE transaction so SET LOCAL cannot leak
	// onto a pooled connection a later measurement will use.
	if err := withNoSeqScan(ctx, w, func(tx pgx.Tx) error {
		for _, op := range append(w.operations(), w.probes()...) {
			for _, st := range captured[op.Name] {
				if !explainable(st.SQL) {
					continue
				}
				plan, planErr := explainVia(ctx, tx, st.SQL, st.Args)
				rep := PlanReport{Operation: op.Name, Statement: st.SQL, Mode: modeNoSeqScan, Plan: plan}
				if planErr != nil {
					rep.Error = planErr.Error()
				}
				plans = append(plans, rep)
			}
		}
		return nil
	}); err != nil {
		return nil, nil, err
	}

	checks := make([]GateCheck, 0, len(planChecks)+1)
	if len(planChecks) == 0 {
		// An empty check list would make this stage a check that cannot fail.
		// It is a failure, loudly, so the list cannot be left empty by accident.
		checks = append(checks, GateCheck{
			Name:   "plan:declared",
			Passed: false,
			Detail: "no plan checks are declared: this stage would assert nothing. See gatePlanChecks in plans.go",
		})
	}
	for _, pc := range planChecks {
		check := GateCheck{Name: "plan:" + pc.Operation + ":" + pc.Label()}
		rep := findPlan(plans, pc.Operation, pc.Match)
		switch {
		case rep == nil:
			check.Detail = fmt.Sprintf("%s issued no statement containing %q, so this check had nothing "+
				"to look at. A plan check that finds nothing is a check that cannot fail. Why it exists: %s",
				pc.Operation, firstLine(pc.Match), pc.Why)
		case rep.Error != "":
			check.Detail = fmt.Sprintf("the captured statement could not be planned: %s. Why this check exists: %s",
				rep.Error, pc.Why)
		case seqScanOn(rep.Plan, pc.Table):
			check.Detail = fmt.Sprintf("the plan sequentially scans %s even with sequential scanning switched "+
				"off, which is PostgreSQL's way of saying no index can serve this query. Why this check "+
				"exists: %s\nplan:\n%s", pc.Table, pc.Why, rep.Plan)
		default:
			check.Passed = true
			check.Detail = fmt.Sprintf("with enable_seqscan = off the plan reaches %s through an index, so "+
				"the query has an index path at this tier. Why: %s", pc.Table, pc.Why)
		}
		checks = append(checks, check)
	}
	return checks, plans, nil
}

// gateTimings refuses an accounting that does not add up, and refuses a
// reseeded run that reports no corpus load.
//
// # Why this is a check and not a formatting concern
//
// The defect it exists for is real and was in this file's own report: the run
// timed the DROP/CREATE/migrate step, called it "seeded in", and never timed the
// corpus load at all — so a spec-tier run printed 0.5s about a stage that took
// ~36s, and that figure is what the CI cost note and the `--scale` guidance were
// built on. Nothing about the number looked wrong; only its provenance did. A
// non-zero TotalSeconds with a zero CorpusLoadSeconds on a run that DID seed is
// exactly that failure, and it is worth a red gate rather than a comment,
// because the next person to add a stage will face the same temptation.
//
// These are wall-clock NUMBERS, not wall-clock ASSERTIONS: nothing here compares
// a duration to a target, so no runner speed can turn any of it red. The two
// questions asked are "is the load's cost accounted for" and "do the stages fit
// inside the run", and both have the same answer on every machine.
func gateTimings(t Timing) []GateCheck {
	sum := t.accounted()
	checks := []GateCheck{
		{
			Name:   "timings:stages_fit_in_total",
			Passed: t.Total >= sum,
			Detail: fmt.Sprintf("provision %s + load %s + ANALYZE %s + measure %s = %s, total %s "+
				"(the difference is the harness's own overhead: cold workloads, plan capture, EXPLAINs, counts)",
				t.Provision.Round(time.Millisecond), t.CorpusLoad.Round(time.Millisecond),
				t.Analyze.Round(time.Millisecond), t.Measure.Round(time.Millisecond),
				sum.Round(time.Millisecond), t.Total.Round(time.Millisecond)),
		},
	}
	if t.Reseeded {
		checks = append(checks, GateCheck{
			Name:   "timings:corpus_load_timed",
			Passed: t.CorpusLoad > 0,
			Detail: fmt.Sprintf("this run seeded, and the corpus load is reported as %s "+
				"(provision %s). A zero here means the load's wall clock is not wired into the report — "+
				"the defect that made a 36-second spec-tier load print as 0.5 seconds",
				t.CorpusLoad.Round(time.Millisecond), t.Provision.Round(time.Millisecond)),
		})
	}
	return checks
}

// gateCeilings is stage 4.
func gateCeilings(w *Workload, measured map[string]Measurement) []GateCheck {
	var checks []GateCheck
	for _, op := range append(w.operations(), w.probes()...) {
		m, ok := measured[op.Name]
		if !ok || m.Samples == 0 {
			// Not measured is not a pass. A ceiling silently skipped because
			// the operation did not run is a gate with a hole in it.
			checks = append(checks, GateCheck{
				Name:   "ceiling:" + op.Name,
				Passed: false,
				Detail: "not measured in this run, so no ceiling could be applied",
			})
			continue
		}
		limit, why := ceilingFor(op)
		checks = append(checks, GateCheck{
			Name:   "ceiling:" + op.Name,
			Passed: limit > 0 && m.P95 <= limit,
			Detail: fmt.Sprintf("p95 %s <= ceiling %s (%s). This is a regression ceiling, NOT an SLO: "+
				"the docs/27 comparison is in the report, next to this number",
				m.P95.Round(time.Millisecond), limit.Round(time.Millisecond), why),
		})
	}
	return checks
}

// finishGate folds the stages into one verdict.
func finishGate(stages ...[]GateCheck) GateResult {
	var g GateResult
	for _, s := range stages {
		g.Checks = append(g.Checks, s...)
	}
	g.Passed = len(g.Checks) > 0
	for _, c := range g.Checks {
		if !c.Passed {
			g.Passed = false
		}
	}
	return g
}

// ceilingFor resolves an operation's ceiling and says where it comes from.
func ceilingFor(op *Operation) (time.Duration, string) {
	if op.SLOTarget > 0 {
		return op.SLOTarget * regressionFactor,
			fmt.Sprintf("%d× the docs/27 budget for %s", regressionFactor, op.Name)
	}
	if d, ok := probeCeilings[op.Name]; ok {
		return d, "this harness's own ceiling; docs/27 states no budget for it"
	}
	// An operation with neither a budget nor a declared ceiling is a hole in
	// the gate, and it says so rather than defaulting to something generous.
	return 0, "NO CEILING DECLARED — add one to probeCeilings or give the operation a docs/27 budget"
}

// capturePass runs every measured operation once and records the statements it
// issued. One run per operation, because the capture exists to find the
// statements, not to time them.
func capturePass(ctx context.Context, w *Workload) (map[string][]CapturedStatement, error) {
	out := map[string][]CapturedStatement{}
	for _, op := range append(w.operations(), w.probes()...) {
		w.tracer.reset()
		if err := op.Run(ctx); err != nil {
			return nil, fmt.Errorf("benchmark: capture pass for %s: %w", op.Name, err)
		}
		out[op.Name] = w.tracer.snapshot()
	}
	return out, nil
}

// The two EXPLAIN modes. They are named constants because a plan check that
// read the wrong mode would assert index-availability against a plan the
// planner chose freely, which at a small tier is a sequential scan.
const (
	modeNatural   = "natural"
	modeNoSeqScan = "no_seqscan"
)

// querier is what both the pool and a transaction can satisfy.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// explain runs EXPLAIN (COSTS OFF) over a statement with the very arguments the
// operation issued it with, and returns every plan line joined.
//
// Everything is read, not just the node type: research_profile_test.go reads
// every line of the plan for the same reason — an index name can appear in a
// line other than the first (an Index Cond, a Bitmap Index Scan under a Bitmap
// Heap Scan), and a check that looked only at the top node would call a
// sequential scan with a filter "using the index".
func explain(ctx context.Context, w *Workload, sql string, args []any) (string, error) {
	return explainVia(ctx, w.Pool, sql, args)
}

func explainVia(ctx context.Context, q querier, sql string, args []any) (string, error) {
	rows, err := q.Query(ctx, "EXPLAIN (COSTS OFF) "+sql, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("EXPLAIN returned no plan lines")
	}
	return strings.Join(lines, "\n"), nil
}

// withNoSeqScan runs fn inside one transaction with
// `SET LOCAL enable_seqscan = off`.
//
// # Why the plan checks need this, and why it is not "rigging the test"
//
// At the ci tier this harness loads a fraction of docs/27:20's rows, and on a
// table of a few hundred rows every one of these queries is correctly answered
// by a sequential scan. Asserting "the plan names this index" against the
// natural plan would therefore be asserting the planner's cost model at a
// particular size — a check that is red on a correct database, which is the
// kind of check that gets loosened (CLAUDE.md: 禁止为让 Gate 通过 放宽
// assertion).
//
// With sequential scans discouraged, the question becomes the one that is
// answerable at any size: CAN this query use this index at all? PostgreSQL
// still falls back to a sequential scan when no index can serve the query, so
// the check still fails when the index is missing, wrongly typed, or not
// applicable to the predicate — which is what a mutation of this check shows.
//
// What the check does NOT claim: that production picks this index. The report's
// timing table and the natural-mode plans in the same report are the evidence
// about production's choice, and at the spec tier the planner picks the indexes
// on its own.
func withNoSeqScan(ctx context.Context, w *Workload, fn func(pgx.Tx) error) error {
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("benchmark: begin the plan-check transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		return fmt.Errorf("benchmark: SET LOCAL enable_seqscan: %w", err)
	}
	return fn(tx)
}

// explainable reports whether a captured statement is a read worth planning.
//
// The capture is raw: it holds whatever the pool sent, including the
// transaction control the state store's commit issues. "EXPLAIN begin" is a
// syntax error, and a gate that treated that error as a failed check would be
// red on a correct database. Only statements whose first keyword is SELECT or
// WITH are planned; everything else is recorded in the report with the reason
// instead.
// The captured text is the sqlc constant VERBATIM, which means it starts with
// the `-- name: Foo :many` comment the .sql file carries (sqlc keeps it in the
// generated string). Reading only the first characters therefore classified
// every query in the tree as "not a read statement". The scan below skips blank
// lines and `--` comments to find the first keyword, and the statement is still
// EXPLAINed with its comment intact — PostgreSQL parses that happily, and
// stripping it would mean planning a string that is not the one the pool sent.
func explainable(sql string) bool {
	for _, line := range strings.Split(sql, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "--") {
			continue
		}
		upper := strings.ToUpper(line)
		return strings.HasPrefix(upper, "SELECT") || strings.HasPrefix(upper, "WITH")
	}
	return false
}

// findPlan returns the recorded plan for the operation's statement that
// contains match, or nil when the operation issued no such statement.
//
// It reads the no_seqscan plans: see PlanReport.Mode and withNoSeqScan for why
// the checks ask about index availability rather than production's choice.
func findPlan(plans []PlanReport, operation, match string) *PlanReport {
	for i := range plans {
		if plans[i].Mode != modeNoSeqScan {
			continue
		}
		if plans[i].Operation == operation && strings.Contains(plans[i].Statement, match) {
			return &plans[i]
		}
	}
	return nil
}

// firstLine keeps a multi-line statement description readable in a failure
// message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}
