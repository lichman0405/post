package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/lichman0405/post/internal/search/embedding"
)

// Exit codes. The same contract rddev uses (0 ok, 1 a check failed, 2 usage),
// so a caller can tell "the database is slow" from "you typed the flag wrong".
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

const usage = `benchmark — the POST V1 performance baseline (T1108).

	benchmark <command> [flags]

Commands:
  seed       provision the benchmark database from its migrations, then load the corpus
  measure    seed, then measure every operation and print the report
  all        seed, gate, measure, then print one report carrying both (this is make bench)
  selfcheck  assert the harness's own gate logic, with NO database (the same checks "all" runs as stage 0)

There is no separate "gate" command, and that is a decision rather than an
omission: the gate's last stage is the regression ceilings, and a ceiling can
only be applied to a measurement. A gate-only entry point would either have to
measure anyway — at which point it IS "all" — or report every ceiling as
unmeasurable, which is a gate that cannot pass. Run "all --no-seed" to re-check
a corpus that is already there; that is a supported mode, and the corpus check
distinguishes "fewer rows than the seed produced" from "more rows than the seed
produced, because earlier runs appended their own".

selfcheck exists because the gate's non-database half — the row-count
classification, the plan-test helpers, the percentile function — is logic that
can rot without any query noticing, and "all" exercises it only when a database
is available. It is the command to run when there is no PostgreSQL.

Flags:
  --scale NAME      corpus size: "spec" (docs/27:20, the SLO tier) or "ci" (small, not an SLO tier).
                    Default "ci" so an un-flagged run is the cheap one; the SLO comparison requires
                    an explicit --scale spec and says so in the report.
  --admin-url URL   PostgreSQL admin URL. Default $POSTGRES_TEST_ADMIN_URL, else the same
                    postgres://postgres:postgres_dev_pw@127.0.0.1:5432/post ` + "`make infra-up`" + ` publishes.
  --db NAME         benchmark database name (default ` + defaultDBName + `). Dropped and recreated
                    by ` + "`seed`" + `, so never point it at anything you want to keep.
  --json            write the machine-readable report to stdout (it is written either way when set;
                    the human table always goes to stderr).
  --no-seed         do not reseed; check/measure the corpus already in the database.
  --drop            drop the benchmark database on exit.
  --timeout D       overall timeout (default 30m).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(exitUsage)
	}
	cmd := os.Args[1]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		fmt.Print(usage)
		os.Exit(exitOK)
	}
	// The command is validated here rather than left to fall through the run
	// function's `if cmd == ...` ladder: an unrecognised command would take
	// every branch as false, do no work, and exit 0 — a no-op that reads as a
	// pass to anything driving this from a script.
	switch cmd {
	case "seed", "measure", "all", "selfcheck":
	default:
		fmt.Fprintf(os.Stderr, "benchmark: unknown command %q\n\n%s", cmd, usage)
		os.Exit(exitUsage)
	}

	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	scaleName := fs.String("scale", ciScale.Name, "corpus size: spec or ci")
	adminURL := fs.String("admin-url", "", "PostgreSQL admin URL")
	dbName := fs.String("db", defaultDBName, "benchmark database name")
	asJSON := fs.Bool("json", false, "write the JSON report to stdout")
	noSeed := fs.Bool("no-seed", false, "do not reseed; use the corpus already in the database")
	drop := fs.Bool("drop", false, "drop the benchmark database on exit")
	timeout := fs.Duration("timeout", 30*time.Minute, "overall timeout")
	if err := fs.Parse(os.Args[2:]); err != nil {
		os.Exit(exitUsage)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "benchmark: unexpected argument %q\n\n%s", fs.Arg(0), usage)
		os.Exit(exitUsage)
	}

	// selfcheck is answered before --scale is resolved: it touches no database
	// and no scale, so an unknown --scale name must not be able to turn a
	// no-database check into a usage error.
	if cmd == "selfcheck" {
		os.Exit(selfCheckMain())
	}
	sc, err := scaleByName(*scaleName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: %v\n", err)
		os.Exit(exitUsage)
	}
	// Resolution order, and nothing else happens here: run() prints the
	// resulting URL REDACTED as its first line, before it drops anything, so a
	// run aimed at the wrong server is visible in the first line of output.
	// (This block used to hold an `if` whose entire body was `_ = raw`, i.e. a
	// comment about printing the URL next to code that printed nothing, from
	// before the banner existed.)
	raw := *adminURL
	if raw == "" {
		raw = envAdminURL()
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	code, err := run(ctx, cmd, sc, raw, *dbName, *asJSON, *noSeed, *drop)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchmark: %v\n", err)
		if code == exitOK {
			code = exitFail
		}
	}
	os.Exit(code)
}

func run(ctx context.Context, cmd string, sc Scale, adminURL, dbName string, asJSON, noSeed, drop bool) (int, error) {
	started := time.Now()
	benchURL, err := withDatabase(adminURL, dbName)
	if err != nil {
		return exitUsage, err
	}

	// The one line that says which server is about to be written to. It is
	// printed HERE — by the code that owns redactURL — rather than by the make
	// target, because a second place that renders a URL is a second place that
	// can render a password: `make bench` used to echo the raw POSTGRES_TEST_ADMIN_URL
	// (credentials included) while every other output path in this harness
	// redacted it. There is now one renderer and this is what it prints.
	fmt.Fprintf(os.Stderr, "benchmark: %s --scale %s --db %s against %s\n",
		cmd, sc.Name, dbName, redactURL(benchURL))

	var t Timing
	t.Reseeded = !noSeed
	var corpus *Corpus

	if noSeed {
		// --no-seed reuses whatever is in the database. It exists so a corpus
		// can be re-checked without paying for a reseed, and it is the shape
		// the gate's own mutation check uses: running `--scale spec --no-seed`
		// against a ci-tier corpus is a real misuse, and the corpus check has
		// to catch it.
		pool, err := openSeed(ctx, benchURL)
		if err != nil {
			return exitFail, err
		}
		corpus, err = readCorpus(ctx, pool, sc)
		pool.Close()
		if err != nil {
			return exitFail, err
		}
	} else {
		if cmd == "measure" && sc.SLOComparison() {
			fmt.Fprintf(os.Stderr, "benchmark: seeding the %q tier takes a few minutes; measuring %d objects, "+
				"%d relations, %d state transitions, %d branches and %d search documents\n",
				sc.Name, sc.HotObjects, sc.HotRelations, sc.HotStateCommits, sc.HotBranches, sc.NetworkDocuments)
		}
		// The two stages are timed and REPORTED separately. `provision` is
		// DROP/CREATE/migrate; `seedCorpus` is the load. Timing only the first
		// and calling it "seeded in" is the defect this split fixes: at the
		// spec tier the load is minutes and the provisioning is half a second,
		// so the aggregate said "0.5s" about a 36-second stage — and that
		// aggregate is what a reader uses to decide which tier CI can run.
		benchURL, t.Provision, err = provision(ctx, adminURL, dbName)
		if err != nil {
			return exitFail, err
		}
		pool, err := openSeed(ctx, benchURL)
		if err != nil {
			return exitFail, err
		}
		loadStart := time.Now()
		corpus, err = seedCorpus(ctx, pool, sc)
		t.CorpusLoad = time.Since(loadStart)
		pool.Close()
		if err != nil {
			return exitFail, err
		}
	}

	if drop {
		defer func() {
			if err := dropBenchmarkDB(ctx, adminURL, dbName); err != nil {
				fmt.Fprintf(os.Stderr, "benchmark: dropping %s: %v\n", dbName, err)
			}
		}()
	}

	w, err := openWorkload(ctx, benchURL, corpus)
	if err != nil {
		return exitFail, err
	}
	defer w.Close()

	analyzeStart := time.Now()
	if _, err := w.Pool.Exec(ctx, `ANALYZE`); err != nil {
		return exitFail, fmt.Errorf("benchmark: ANALYZE: %w", err)
	}
	t.Analyze = time.Since(analyzeStart)

	if cmd == "seed" {
		t.Measure = 0
		t.Total = time.Since(started)
		rep := buildReport(sc, corpus, expectedCounts(sc), nil, nil, nil, t,
			nil, redactURL(benchURL), started)
		return emit(rep, asJSON, nil)
	}

	var corpusChecks []GateCheck
	var planChecksRun []GateCheck
	var plans []PlanReport
	if cmd == "all" {
		corpusChecks, err = gateCorpus(ctx, w, sc)
		if err != nil {
			return exitFail, err
		}
		planChecksRun, plans, err = gatePlans(ctx, w)
		if err != nil {
			return exitFail, err
		}
	}

	measureStart := time.Now()
	measured := map[string]Measurement{}
	ops := append(w.operations(), w.probes()...)
	if cmd == "measure" || cmd == "all" {
		if err := measureAll(ctx, w, ops, measured); err != nil {
			return exitFail, err
		}
	}
	t.Measure = time.Since(measureStart)
	t.Total = time.Since(started)

	var gate *GateResult
	if cmd == "all" {
		// selfChecks is a stage of the gate and not a convenience beside it:
		// the checks below rest on logic that lives in this binary (the row
		// count classification, seqScanOn, percentile), and `make bench` is the
		// only place most readers will ever run it. A harness that asserts
		// things about a database while never asserting anything about itself
		// is a harness whose own bug looks like a finding about the database.
		g := finishGate(selfChecks(), corpusChecks, planChecksRun, gateTimings(t), gateCeilings(w, measured))
		g.Plans = plans
		gate = &g
	}

	rep := buildReport(sc, corpus, expectedCounts(sc), w.operations(), w.probes(), measured,
		t, gate, redactURL(benchURL), started)
	return emit(rep, asJSON, gate)
}

// measureAll measures every operation, in order, and records the failures
// rather than aborting on the first one: a run where one operation fails is
// still a run whose other numbers are worth having, and the report says which
// one failed and why.
func measureAll(ctx context.Context, w *Workload, ops []*Operation, measured map[string]Measurement) error {
	for _, op := range ops {
		m, err := measure(ctx, w, op)
		measured[op.Name] = m
		if err != nil {
			fmt.Fprintf(os.Stderr, "benchmark: %v\n", err)
		}
	}
	return nil
}

func emit(rep *Report, asJSON bool, gate *GateResult) (int, error) {
	writeHuman(os.Stderr, rep)
	if asJSON {
		if err := writeJSON(os.Stdout, rep); err != nil {
			return exitFail, err
		}
	}
	if gate != nil && !gate.Passed {
		return exitFail, nil
	}
	return exitOK, nil
}

// readCorpus rebuilds a Corpus's identity for --no-seed.
//
// Every id in the corpus is a sha256 of a label (benchUUID), so the SAME scale
// always names the same rows, and the identity half can be recomputed exactly.
// The two values that are not a pure function of the scale are read back from
// the database instead of recomputed — main's head state, which the seed's
// ordering decides, and the query vector, which comes from the embedder — and
// the reads are checked, not assumed: an id derived and not verified would let
// --no-seed measure a corpus that is not there, and every operation would
// "succeed" against zero rows.
//
// The corpus's EXISTENCE is the only thing verified here. A request for a scale
// the database does not hold is caught further down, and by two different
// mechanisms — this comment used to claim the wrong one, so both are named:
//
//   - a corpus of a DIFFERENT TIER stops in openWorkload, whose scope assertion
//     (workload.go) compares the owner's project scope with 1+NetworkProjects.
//     `--scale spec --no-seed` over a ci-tier corpus therefore fails there, with
//     "the corpus owner's scope covers 11 projects, want 101" — a startup error,
//     not a red gate. Both directions (bigger and smaller corpus than asked for)
//     are caught this way, because the project counts differ between the tiers.
//   - a corpus of the RIGHT tier that is SHORT of rows — a seed that failed
//     partway, which is the case the scope check cannot see because the project
//     rows are all there — reaches the gate and is reported by the corpus check,
//     naming the tables that are short. That is the check's lower bound, and it
//     is the reason the check reads the live tables instead of trusting the
//     seed's in-memory tally.
func readCorpus(ctx context.Context, pool *pgxpool.Pool, sc Scale) (*Corpus, error) {
	c := &Corpus{
		Scale:            sc,
		OwnerID:          benchUUID("user/owner", 0),
		ActorID:          benchUUID("user/actor", 0),
		HotProjectID:     benchUUID("project/hot", 0),
		HotMainBranchID:  benchUUID("branch/main", 0),
		HotWriteBranchID: benchUUID("branch/main", 0),
		HotObjectID:      benchUUID("object/hot", 0),
		Counts:           map[string]int{},
		networkGenesis:   map[string]string{},
	}
	if sc.HotBranches > 1 {
		c.HotWriteBranchID = benchUUID("branch/hot", 1)
	}
	if sc.HotObjects > 0 {
		c.HotVersionObjectID = benchUUID("object/hot", sc.HotObjects-1)
	}
	c.NetworkProjectIDs = make([]string, 0, sc.NetworkProjects)
	for i := 0; i < sc.NetworkProjects; i++ {
		c.NetworkProjectIDs = append(c.NetworkProjectIDs, benchUUID("project/net", i))
	}

	var exists int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM projects WHERE id = $1`, c.HotProjectID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("benchmark: cannot read the corpus back (is the database migrated?): %w", err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("benchmark: no corpus in this database (the hot project %s is absent); "+
			"run `benchmark seed --scale %s` first, or drop --no-seed", c.HotProjectID, sc.Name)
	}
	if err := pool.QueryRow(ctx, `SELECT id FROM project_states WHERE project_id = $1 AND branch_id = $2
		ORDER BY created_at DESC, id DESC LIMIT 1`, c.HotProjectID, c.HotMainBranchID).Scan(&c.HotHeadStateID); err != nil {
		return nil, fmt.Errorf("benchmark: read main's head back: %w", err)
	}

	embedder, err := embedding.NewDeterministic(embedding.LocalModel)
	if err != nil {
		return nil, fmt.Errorf("benchmark: build the deterministic embedder: %w", err)
	}
	qv, err := embedder.Embed(ctx, []string{queryText})
	if err != nil {
		return nil, fmt.Errorf("benchmark: embed the query: %w", err)
	}
	c.QueryEmbedding = vectorLiteral(qv[0])
	c.QueryText = queryText
	return c, nil
}
