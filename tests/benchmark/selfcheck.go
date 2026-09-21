package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// selfchecks are assertions about THIS PACKAGE, run without a database.
//
// # What they are for
//
// Everything else in this directory checks a database. This file checks the
// code that does the checking, and it exists because that code has already been
// wrong in ways no database run noticed:
//
//   - the corpus check banded the three append-only tables to one run's worth of
//     appends, so a THIRD `--no-seed` run went red on an entirely correct
//     database. It took a deliberate three-run sequence to see it.
//   - seedPullRequests computed `1 + i%(HotBranches-1)`, which is a division by
//     zero the moment a scale has one branch. Nothing in the shipped tiers
//     reaches it, so no run would ever have found it either.
//
// Both are logic defects, both are invisible to any single run, and both are
// decidable without PostgreSQL. So the assertions live here, they run inside
// `all`'s gate as stage 0 (see run in main.go) and they run on their own under
// `benchmark selfcheck`, which is the command to use when there is no database
// — including from CI's unit job, which has none.
//
// # What they are NOT
//
// They are not a substitute for the database checks and they do not measure
// anything: no duration here is compared to a budget, and nothing here talks to
// PostgreSQL. They are the harness's own regression guards, and the one thing
// they must never become is a place where a check is skipped because the
// environment is inconvenient — every assertion below is unconditional.
func selfChecks() []GateCheck {
	var checks []GateCheck
	add := func(name string, ok bool, detail string) {
		checks = append(checks, GateCheck{Name: "selfcheck:" + name, Passed: ok, Detail: detail})
	}

	// ---- the corpus band admits every number of prior runs ----------------
	//
	// The sequence is the one that was broken: a fresh seed, then two
	// `--no-seed` re-runs over it, each appending one writeAllowance's worth.
	// The third read is the one the old band called a corpus defect.
	{
		const seeded, allowance = 110_000, 35
		got := []int{seeded, seeded + allowance, seeded + 2*allowance, seeded + 3*allowance}
		allPass, why := true, ""
		for run, n := range got {
			ok, detail := corpusVerdict("scientific_object_versions", seeded, n, allowance)
			if !ok {
				allPass = false
				why = fmt.Sprintf("run %d of the --no-seed sequence was refused: %s", run, detail)
				break
			}
		}
		add("corpus_band_admits_reruns", allPass,
			fmt.Sprintf("counts %v over one seed were all admitted by corpusVerdict (seeded %d, one run's "+
				"appends %d). %s", got, seeded, allowance, why))
	}

	// ---- and still refuses a corpus that is short -------------------------
	//
	// The other direction, asserted separately so that "everything passes" can
	// never be mistaken for the fix: a count below the seed's is a seed that did
	// not finish, whichever run it is seen on.
	{
		short := []int{109_999, 55_000, 0}
		allRefused, why := true, ""
		for _, n := range short {
			ok, detail := corpusVerdict("scientific_object_versions", 110_000, n, 35)
			if ok {
				allRefused = false
				why = fmt.Sprintf("count %d was admitted: %s", n, detail)
				break
			}
		}
		add("corpus_band_refuses_short", allRefused,
			fmt.Sprintf("counts %v against a seed of 110000 were all refused as SHORT. %s", short, why))
	}

	// ---- a table nothing writes is still exact ----------------------------
	{
		ok, detail := corpusVerdict("relations", 100_000, 100_001, 0)
		add("exact_tables_reject_growth", !ok,
			fmt.Sprintf("an appendable table's rule must not leak onto a table no operation writes; "+
				"relations +1 was refused: %v. %s", !ok, firstLine(detail)))
	}

	// ---- a scale with one branch must not divide by zero ------------------
	//
	// The panic was `1 + i%(HotBranches-1)`. The guard is what makes a
	// one-branch scale produce a corpus instead of a crashed seed, so it is
	// asserted for the degenerate scale AND for a normal one.
	{
		one := &Corpus{Scale: Scale{Name: "selfcheck", HotBranches: 1}, HotMainBranchID: "main-branch"}
		gotOne, panicOne := hotBranchSafely(one, 0)
		okOne := gotOne == "main-branch" && panicOne == ""

		many := &Corpus{Scale: Scale{Name: "selfcheck", HotBranches: 1000}, HotMainBranchID: "main-branch"}
		seen := map[string]bool{}
		okMany := true
		for i := 0; i < 3000; i++ {
			b, p := hotBranchSafely(many, i)
			if p != "" {
				okMany, panicOne = false, p
				break
			}
			if b == "main-branch" || b == "" {
				okMany = false
				break
			}
			seen[b] = true
		}
		add("hot_branch_no_division_by_zero", okOne && okMany,
			fmt.Sprintf("HotBranches=1 -> %q (want the main branch, and no panic), HotBranches=1000 -> "+
				"%d distinct branches over 3000 rows (want 999); panic seen: %q", gotOne, len(seen), panicOne))
	}

	// ---- percentile is nearest-rank over the sorted samples ---------------
	//
	// The published numbers rest on this function, and a silent change from
	// nearest-rank to interpolation would produce p95s no call ever observed.
	{
		s := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
		cases := []struct {
			p    float64
			want time.Duration
		}{{0.5, 5}, {0.95, 10}, {0.99, 10}, {0.0, 1}}
		ok := true
		for _, c := range cases {
			if got := percentile(s, c.p); got != c.want {
				ok = false
			}
		}
		if percentile(nil, 0.95) != 0 {
			ok = false
		}
		add("percentile_is_nearest_rank", ok,
			"percentile over [1..10]: p50=5, p95=10, p99=10, p0=1 (nearest rank, i.e. a duration some call "+
				"actually took), and 0 over an empty set")
	}

	// ---- redactURL removes the password and keeps the rest ----------------
	//
	// The fixture password is deliberately NOT the repository's development
	// password: this detail string is printed in the report and carried in the
	// report's JSON, and a check whose own output contains a real credential is
	// the defect it exists to prevent. The assertion is about the redactor's
	// behaviour, and any password proves that equally.
	{
		const secret = "s3cr3t-not-a-credential"
		got := redactURL("postgres://postgres:" + secret + "@127.0.0.1:5432/post_bench")
		want := "postgres://postgres:xxxxx@127.0.0.1:5432/post_bench"
		unparseable := redactURL(":::not a url:::")
		leaked := strings.Contains(got, secret)
		add("redact_url_hides_password", got == want && !leaked && unparseable == "<unparseable url>",
			fmt.Sprintf("a password-bearing URL redacts to %q (want %q) and the password is gone from it "+
				"(%v); an unparseable URL reads %q", got, want, !leaked, unparseable))
	}

	// ---- seqScanOn matches a whole table name -----------------------------
	//
	// The plan check's whole verdict rests on this predicate, and a substring
	// match would let `Seq Scan on branches_archive` satisfy a check written for
	// `branches`.
	{
		plan := "Limit\n  ->  Seq Scan on branches_archive\n"
		hit := seqScanOn(plan, "branches")
		miss := seqScanOn(plan, "branches_archive")
		indexed := seqScanOn("Index Scan using branches_project_id_name_key on branches\n", "branches")
		add("seq_scan_matches_whole_table", !hit && miss && !indexed,
			fmt.Sprintf("a plan scanning branches_archive does not count as scanning branches (%v), does "+
				"count as scanning branches_archive (%v); an Index Scan on branches is not a seq scan (%v)",
				!hit, miss, !indexed))
	}

	// ---- explainable reads through the sqlc comment -----------------------
	{
		sqlcRead := "-- name: SearchDocuments :many\nSELECT 1\n"
		ctrl := "begin"
		add("explainable_skips_comments", explainable(sqlcRead) && !explainable(ctrl),
			"a captured statement carrying sqlc's `-- name:` header is planned (the header is a comment, "+
				"not a keyword); transaction control is not")
	}

	// ---- every measured operation has a ceiling ---------------------------
	//
	// stage 4 fails an operation whose ceiling is missing, so this asserts the
	// failure is reachable rather than finding out at the end of a long run.
	{
		missing, _ := ceilingFor(&Operation{Name: "an_operation_with_no_budget"})
		present, why := ceilingFor(&Operation{Name: "project_overview", SLOTarget: 500 * time.Millisecond})
		probe, _ := ceilingFor(&Operation{Name: "probe_deep_offset"})
		add("ceilings_are_declared", missing == 0 && present == 500*time.Millisecond*regressionFactor && probe == 10*time.Second,
			fmt.Sprintf("an operation with neither a budget nor a probeCeilings entry resolves to %s (stage 4 "+
				"refuses that); a 500ms budget gives %s (%s); probe_deep_offset's declared ceiling is %s",
				missing, present, why, probe))
	}

	return checks
}

// hotBranchSafely calls hotBranchFor and turns a panic into a value, so the
// assertion above reports a FAIL rather than taking the whole command down with
// it.
//
// It exists because of what the mutant does: with the guard removed,
// `1 + i%(HotBranches-1)` is an integer division by zero, so the call panics.
// Without this wrapper the selfcheck process dies with a runtime panic — a red
// result, but one that names neither the check nor the cause, which is the
// difference between a gate that says "no" and a gate that crashes.
func hotBranchSafely(c *Corpus, i int) (branch string, panicked string) {
	defer func() {
		if r := recover(); r != nil {
			panicked = fmt.Sprint(r)
		}
	}()
	return c.hotBranchFor(i), ""
}

// selfCheckMain is `benchmark selfcheck`: the same assertions, without a
// database, with a human-readable result and the package's exit-code contract.
func selfCheckMain() int {
	checks := selfChecks()
	failed := 0
	for _, c := range checks {
		mark := "ok  "
		if !c.Passed {
			mark = "FAIL"
			failed++
		}
		fmt.Fprintf(os.Stderr, "[%s] %-38s %s\n", mark, c.Name, c.Detail)
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "benchmark: selfcheck FAILED: %d of %d assertions\n", failed, len(checks))
		return exitFail
	}
	fmt.Fprintf(os.Stderr, "benchmark: selfcheck ok: %d assertions, no database involved\n", len(checks))
	return exitOK
}
