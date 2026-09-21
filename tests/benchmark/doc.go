// Command benchmark is the V1 performance baseline (T1108): it generates a
// capacity-scale corpus, measures the queries docs/27_PERFORMANCE_SLO.md names
// against that corpus, and runs the deterministic half of the gate.
//
// # Why this is a main package and not a _test.go file
//
// CI's `go` job runs `go test $(go list ./... | grep -v '/tests/integration')`
// (Makefile's GO_UNIT_PKGS, .github/workflows/ci.yml) on a runner with NO
// database. Every package under tests/ is in that list, so a benchmark that
// needed PostgreSQL from `go test` would make the unit job red — and the
// tempting escape hatches are both forbidden. One is Go's short-testing flag,
// a skip switch this repository has never used and that T1108 has to leave at
// zero occurrences under tests/ and internal/ (the literal identifier is
// deliberately not spelled out here, so that grep stays empty); the other is
// "skip when there is no database", which is how a gate stops being able to
// fail. This package therefore has no test files at all. `go test` reports
// `no test files` for it and exits 0 without a database, while the real work
// runs behind the `bench` make target, where a missing database is a loud
// failure rather than a skip.
//
// The checks live in exit codes rather than in assertions, which is the same
// contract `rddev` uses: 0 ok, 1 a check failed, 2 usage error.
//
// # What the deterministic layer may and may not assert
//
// docs/27 is an engineering target for V1 — "V1 是工程目标，不承诺公网 SLA"
// (:3) — and a wall-clock assertion on a shared CI runner is a test that goes
// red at random, which is a test that eventually gets "loosened" to make the
// gate green. That is precisely what CLAUDE.md forbids (禁止为让 Gate 通过
// 放宽 assertion). So the split is:
//
//   - The GATE (blocking, deterministic): the corpus really has the rows the
//     capacity baseline names; every measured query has an index it can use,
//     asserted the way tests/integration/research_profile_test.go asserts it
//     (EXPLAIN (COSTS OFF), "CHOOSABLE, not pinned", reading every plan line);
//     and one deliberately loose regression CEILING whose only job is to catch
//     an order-of-magnitude collapse.
//   - The REPORT (recorded, not asserted): each measured p95 next to its
//     docs/27 target and the ratio between them. Whether an SLO is met is a
//     measurement conclusion that gets written down, never a CI assertion.
//
// The ceiling is not an SLO and is named that way in the code that applies
// it (see regressionCeiling in gate.go) so that nobody later reads a green
// `bench` as "the SLOs pass".
//
// # What is measured
//
// The measured statements are the sqlc-generated ones from
// internal/persistence/sqlc — the same Go methods the application calls, not
// a copy of the SQL — because a benchmark over hand-written SQL is a
// benchmark of the benchmark. The gate EXPLAINs the exact statement text it
// captured at run time (through a pgx.QueryTracer), so a plan assertion can
// never drift away from the query it claims to describe.
//
// The one deliberate boundary: what is timed is the DATABASE work each named
// endpoint performs, not the HTTP round trip. doc.go of measure.go says why,
// and the report says it again next to every number, because a p95 that
// silently means "the query behind the endpoint" is a claim nobody can check.
package main
