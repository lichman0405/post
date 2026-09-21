package main

import "fmt"

// Scale is one capacity tier of the corpus.
//
// docs/27_PERFORMANCE_SLO.md:20 names the baseline in two sentences, and they
// are two different shapes rather than one number, so they are two workloads
// here rather than one:
//
//	单 Project 至少支持：10k Scientific Objects、100k relations、
//	10k state transitions、1k branches/PR history
//	Network seed 至少 100 Projects/100k searchable entities 仍可完成 V1 benchmark
//
// The single-project half is materialized in ONE project ("the hot project")
// because every read budget in docs/27 §页面 is a per-project page: the
// overview, the list, the object detail and the research map all read one
// project's state, and a corpus spread over many projects would measure a
// query nobody runs. The network half is 100 separate projects, because the
// sentence that names 100k searchable entities names 100 projects with them.
//
// The two halves share a database and nothing else: the hot project carries
// no search documents (docs/27's search budget is stated for the network
// seed), and the network projects carry one branch and one object each. A
// corpus that mixed them would make "how many objects does this project
// have" unanswerable, which is the one number the first sentence is about.
type Scale struct {
	// Name is what --scale accepts and what every report prints next to a
	// number, so a reader can never mistake a ci-tier figure for a spec-tier
	// one. spec is the docs/27 baseline; ci is a deliberately smaller tier
	// whose numbers are NOT an SLO comparison (see report.go).
	Name string

	// The single-project workload (docs/27:20, first sentence).
	HotObjects      int // 10k Scientific Objects
	HotRelations    int // 100k relations
	HotStateCommits int // 10k state transitions (CLAUDE.md §9.4: Commit = State Transition)
	HotBranches     int // 1k branches/PR history
	HotPullRequests int

	// The network workload (docs/27:20, second sentence).
	NetworkProjects  int // 100 Projects
	NetworkDocuments int // 100k searchable entities
}

// specScale is docs/27:20 verbatim. Every number in the SLO comparison comes
// from a run at this tier; nothing else is allowed to stand in for it.
var specScale = Scale{
	Name:             "spec",
	HotObjects:       10000,
	HotRelations:     100000,
	HotStateCommits:  10000,
	HotBranches:      1000,
	HotPullRequests:  1000,
	NetworkProjects:  100,
	NetworkDocuments: 100000,
}

// ciScale is the tier CI can afford. It is NOT a smaller SLO: a p95 measured
// here is a p95 of a query that read a tenth of the rows, and report.go marks
// every number it produces as not-an-SLO-comparison rather than comparing it
// to a docs/27 budget. The ratio between the two tiers is what the regression
// ceiling in gate.go is sized from.
//
// The one thing the small tier is allowed to decide is the seed's own wall
// clock: the numbers below are what makes `make bench`'s default run finish in
// a couple of minutes on a CI runner, and the spec tier is a separate,
// explicitly-requested run.
var ciScale = Scale{
	Name:             "ci",
	HotObjects:       400,
	HotRelations:     4000,
	HotStateCommits:  400,
	HotBranches:      40,
	HotPullRequests:  40,
	NetworkProjects:  10,
	NetworkDocuments: 2000,
}

// scaleByName resolves --scale. An unknown name is a usage error rather than a
// silent fall back to a default: a run that quietly measured the wrong tier
// would produce a report nobody could trust, and the name is printed in the
// report so a wrong one has to be caught here.
func scaleByName(name string) (Scale, error) {
	switch name {
	case specScale.Name:
		return specScale, nil
	case ciScale.Name:
		return ciScale, nil
	default:
		return Scale{}, fmt.Errorf("unknown scale %q (want %q or %q)", name, specScale.Name, ciScale.Name)
	}
}

// SLOComparison reports whether a run at this tier may be compared against the
// docs/27 budgets. Only the spec tier may. Every caller that prints a ratio
// asks this first.
func (s Scale) SLOComparison() bool { return s.Name == specScale.Name }

// batchRows is how many rows one COPY or one multi-row INSERT carries before
// the transaction commits.
//
// The commit boundary is not a tuning knob: the four append-only tables cannot
// be DELETEd or TRUNCATEed (infra/migrations/00014, 00015), so a failed seed
// is a failed seed — the database is dropped and rebuilt rather than cleaned.
// Batching keeps a seed failure from leaving one enormous transaction behind
// and keeps the deferred constraint triggers on scientific_object_versions and
// relation_versions (00040, 00045) firing over a bounded queue instead of
// 200k queued rows at one COMMIT.
const batchRows = 2000
