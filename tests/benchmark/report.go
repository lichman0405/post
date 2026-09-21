package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Report is the whole run, in the form `--json` prints.
//
// Everything the harness observed is here, including what it could NOT observe:
// NotMeasured names each item this run does not cover and why. A report that
// lists only what it measured invites the reader to assume the rest passed.
type Report struct {
	Task  string `json:"task"`
	Scale string `json:"scale"`
	// SLOComparison is false for the ci tier. Every ratio in the rows is
	// meaningless when it is false, and the flag is carried in the document
	// rather than left to the reader to infer from the scale name.
	SLOComparison bool   `json:"slo_comparison"`
	Boundary      string `json:"boundary"`
	// SessionSettings are the PostgreSQL settings every measured connection
	// was opened with. They belong in the document, next to the numbers they
	// condition: a latency measured with parallel query disabled is not the
	// same number as one measured with it enabled, and a reader who has to
	// find that out from a GUC somewhere else will not.
	SessionSettings []string `json:"session_settings"`

	Server    string `json:"server"`
	StartedAt string `json:"started_at"`
	// Reseeded is false for --no-seed, where Provision and CorpusLoad are both
	// zero because THIS process did not pay them. Without the flag a reader
	// cannot tell "the corpus loaded instantly" from "the corpus was already
	// there", and the first reading is the one that would be wrong.
	Reseeded bool `json:"reseeded"`
	// ProvisionSeconds is DROP DATABASE + CREATE DATABASE + migrate to head.
	ProvisionSeconds float64 `json:"provision_seconds"`
	// CorpusLoadSeconds is seedCorpus end to end: every COPY the corpus needs,
	// plus the seed's own ANALYZE and its row counts. It is reported separately
	// from ProvisionSeconds because the two are paid by different things — a
	// migration's cost is fixed and a load's grows with the scale — and a reader
	// deciding which tier CI can afford has to see which one is being paid for.
	//
	// It is a real measurement of the stage that dominates a spec-tier run.
	// The first version of this report had one SeedSeconds that accumulated only
	// the provisioning and never timed the load at all, so a spec-tier run
	// printed "seeded in 0.5s" while the COPY of 110k objects and 100k documents
	// took tens of seconds: wrong by two orders of magnitude, on the very number
	// the CI cost estimate rested on.
	CorpusLoadSeconds float64 `json:"corpus_load_seconds"`
	// SeedSeconds is ProvisionSeconds + CorpusLoadSeconds: what seeding from
	// nothing costs. The parts are in the document too, so this aggregate can be
	// checked rather than trusted.
	SeedSeconds float64 `json:"seed_seconds"`
	// AnalyzeSeconds is the ANALYZE the workload runs before timing. It is
	// reported because it is real wall clock a caller pays for, and because a
	// report that hid it would make the harness look faster than it is.
	AnalyzeSeconds float64 `json:"analyze_seconds"`
	MeasureSeconds float64 `json:"measure_seconds"`
	// TotalSeconds is the whole process's wall clock when the report was built.
	// The gate refuses a total smaller than the stages it contains.
	TotalSeconds float64 `json:"total_seconds"`

	Corpus      map[string]int    `json:"corpus"`
	CorpusWant  map[string]int    `json:"corpus_expected"`
	Rows        []RowReport       `json:"rows"`
	Probes      []RowReport       `json:"probes"`
	Plans       []PlanReport      `json:"plans,omitempty"`
	Gate        *GateResult       `json:"gate,omitempty"`
	NotMeasured []string          `json:"not_measured"`
	Extra       map[string]string `json:"extra,omitempty"`
}

// RowReport is one measured item.
type RowReport struct {
	Name    string `json:"name"`
	SLOItem string `json:"slo_item,omitempty"`
	// SLOTargetMs is 0 for a probe: docs/27 states no budget for it.
	SLOTargetMs float64 `json:"slo_target_ms"`
	// Ratio is p95 / target. It is 0 when there is no target, and it is
	// computed but explicitly NOT a verdict at the ci tier.
	Ratio float64 `json:"ratio,omitempty"`
	// Met is the docs/27 comparison. At the ci tier it is always false and
	// Ratio always 0: comparing a ci-tier p95 to a spec-tier budget is the
	// comparison this flag exists to prevent.
	Met bool `json:"met"`

	Samples int     `json:"samples"`
	MinMs   float64 `json:"min_ms"`
	P50Ms   float64 `json:"p50_ms"`
	P95Ms   float64 `json:"p95_ms"`
	P99Ms   float64 `json:"p99_ms"`
	MaxMs   float64 `json:"max_ms"`
	// ColdMs is one observation on a freshly wired workload, not a percentile.
	ColdMs  float64 `json:"cold_ms"`
	ColdErr string  `json:"cold_error,omitempty"`

	Surface  string `json:"surface"`
	Boundary string `json:"boundary"`
	Error    string `json:"error,omitempty"`
	// Statements are the statements the operation actually issued, one entry
	// per distinct statement. They are the evidence that the number above
	// belongs to the query it claims to.
	Statements []string `json:"statements,omitempty"`
}

// Timing is the run's wall clock, stage by stage.
//
// # Why the stages are kept apart
//
// The first version of this report carried one number, taken from the
// DROP/CREATE/migrate step (db.go's provision) while the corpus load — the stage
// that actually dominates a capacity-tier run — was never timed at all. The
// report then printed "corpus: seeded in 0.5s" for a run whose seed took ~36s:
// a figure wrong by two orders of magnitude, and wrong on the exact number that
// decides which tier CI can afford. Splitting the stages is what makes that
// failure visible: an untimed stage shows up as a zero next to a non-zero
// total, instead of being absorbed into a plausible-looking aggregate.
//
// Total is deliberately included so the gate can refuse an accounting that does
// not add up (gateTimings) — a report whose stages claim more wall clock than
// the process ran is a report whose numbers are being attributed to the wrong
// place.
type Timing struct {
	// Provision is DROP DATABASE + CREATE DATABASE + migrate to head.
	Provision time.Duration
	// CorpusLoad is seedCorpus end to end (every COPY, its ANALYZE, its counts).
	CorpusLoad time.Duration
	// Analyze is the workload's own ANALYZE, through the measurement pool.
	Analyze time.Duration
	// Measure is every measured operation, warmups and cold samples included.
	Measure time.Duration
	// Total is the whole process's wall clock when the report was built. It is
	// >= the four above; the remainder is the harness's own overhead — wiring a
	// cold workload per operation, the plan capture, the EXPLAINs, the row
	// counts, and printing.
	Total time.Duration
	// Reseeded says whether Provision and CorpusLoad were paid by THIS process.
	// Both are zero under --no-seed, where an earlier process loaded the corpus
	// and this one only read it.
	Reseeded bool
}

// seedTotal is the "what does seeding from nothing cost" figure.
func (t Timing) seedTotal() time.Duration { return t.Provision + t.CorpusLoad }

// accounted is everything the stages own between them.
func (t Timing) accounted() time.Duration {
	return t.Provision + t.CorpusLoad + t.Analyze + t.Measure
}

// boundarySentence is printed next to every table this harness produces,
// because a latency number without its boundary is a number that will be
// quoted as something it is not.
const boundarySentence = "MEASUREMENT BOUNDARY: every latency below is the DATABASE work of the named " +
	"application operation — the real service, the real store adapters, the real sqlc queries against real " +
	"PostgreSQL. The HTTP layer in front of it (routing, the auth guard, JSON encode/decode, middleware) is " +
	"NOT timed. docs/27:6 and :7 say 'p95 API', so a budget compared against these numbers is compared " +
	"against a strict LOWER BOUND of what the API costs."

func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// buildReport assembles the document from what the run observed.
func buildReport(sc Scale, corpus *Corpus, want map[string]int, ops []*Operation, probes []*Operation,
	measured map[string]Measurement, t Timing,
	gate *GateResult, server string, started time.Time) *Report {

	rep := &Report{
		Task:              "T1108",
		Scale:             sc.Name,
		SLOComparison:     sc.SLOComparison(),
		Boundary:          boundarySentence,
		SessionSettings:   append([]string{}, measurementSettings...),
		Server:            server,
		StartedAt:         started.UTC().Format(time.RFC3339),
		Reseeded:          t.Reseeded,
		ProvisionSeconds:  t.Provision.Seconds(),
		CorpusLoadSeconds: t.CorpusLoad.Seconds(),
		SeedSeconds:       t.seedTotal().Seconds(),
		AnalyzeSeconds:    t.Analyze.Seconds(),
		MeasureSeconds:    t.Measure.Seconds(),
		TotalSeconds:      t.Total.Seconds(),
		Corpus:            corpus.Counts,
		CorpusWant:        want,
		Gate:              gate,
	}
	if gate != nil {
		rep.Plans = gate.Plans
	}
	for _, op := range ops {
		rep.Rows = append(rep.Rows, rowFor(sc, op, measured[op.Name]))
	}
	for _, op := range probes {
		rep.Probes = append(rep.Probes, rowFor(sc, op, measured[op.Name]))
	}
	rep.NotMeasured = notMeasured(sc)
	return rep
}

func rowFor(sc Scale, op *Operation, m Measurement) RowReport {
	r := RowReport{
		Name:     op.Name,
		SLOItem:  op.SLOItem,
		Samples:  m.Samples,
		MinMs:    ms(m.Min),
		P50Ms:    ms(m.P50),
		P95Ms:    ms(m.P95),
		P99Ms:    ms(m.P99),
		MaxMs:    ms(m.Max),
		ColdMs:   ms(m.Cold),
		ColdErr:  m.ColdErr,
		Surface:  op.Surface,
		Boundary: op.Boundary,
	}
	if op.SLOTarget > 0 {
		r.SLOTargetMs = ms(op.SLOTarget)
		// The ratio is only computed where it means something. At the ci tier
		// the corpus is a fraction of the size docs/27:20 names, so the p95 is
		// a p95 of a smaller database; printing a ratio there would be the
		// "scale shrinkage dressed up as an SLO comparison" the task forbids.
		if sc.SLOComparison() {
			r.Ratio = r.P95Ms / r.SLOTargetMs
			r.Met = m.P95 <= op.SLOTarget
		}
	}
	seen := map[string]bool{}
	for _, st := range m.Statements {
		if seen[st.SQL] {
			continue
		}
		seen[st.SQL] = true
		r.Statements = append(r.Statements, st.SQL)
	}
	sort.Strings(r.Statements)
	return r
}

// notMeasured names what this harness does not cover. It is data, not a
// comment, because the report is the artifact somebody reads later.
func notMeasured(sc Scale) []string {
	out := []string{
		"docs/27:13 Evidence-backed Answer 首个 loading state < 200ms (若调用 LLM): the first-loading-state " +
			"budget is a browser paint in apps/web, and no LLM answer path exists in this tree. " +
			"NOT MEASURED — apps/web is outside this task's allowed scope and the frontend is not timed here. " +
			"Recorded as an uncovered docs/27 item rather than reported as met.",
		"docs/27:13 完整答案 < 10s and its structured-results fallback: same reason.",
		"docs/27:17 PR validate 可异步: it is specified as a job with an id and progress, not as a latency, " +
			"so there is no budget to measure.",
		"docs/27:23 Blob multipart upload / 10GB single blob / Web preview: needs MinIO, which this harness " +
			"does not provision, and the blob paths are not in this task's scope.",
		"The HTTP round trip in front of every measured operation (routing, auth guard, JSON, middleware): " +
			"see the boundary sentence.",
	}
	if !sc.SLOComparison() {
		out = append(out, fmt.Sprintf("THE docs/27 BUDGETS THEMSELVES: this run is the %q tier, not the %q tier. "+
			"No ratio is computed and no 'met' is recorded. Run --scale %s for the SLO comparison.",
			sc.Name, specScale.Name, specScale.Name))
	}
	return out
}

// writeJSON prints the machine-readable document.
func writeJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

// writeHuman prints the table. It goes to stderr so that stdout stays a clean
// JSON document a caller can pipe.
func writeHuman(w io.Writer, rep *Report) {
	p := func(format string, a ...any) { fmt.Fprintf(w, format+"\n", a...) }

	p("POST V1 performance baseline (T1108) — scale %q", rep.Scale)
	p("%s", rep.Boundary)
	p("")
	// The stages are printed one by one, never as a single "seeded in" figure:
	// the whole point of Timing is that a stage whose cost is not wired in shows
	// up as a zero here rather than hiding inside an aggregate.
	if rep.Reseeded {
		p("corpus: provision %.1fs + load %.1fs = %.1fs to seed, ANALYZE %.1fs, measured in %.1fs (server %s)",
			rep.ProvisionSeconds, rep.CorpusLoadSeconds, rep.SeedSeconds,
			rep.AnalyzeSeconds, rep.MeasureSeconds, rep.Server)
	} else {
		p("corpus: NOT seeded by this run (--no-seed): provision 0.0s + load 0.0s is NOT a fast seed — "+
			"this process did not pay for one, and the corpus it read was loaded by an earlier run (server %s)",
			rep.Server)
		p("        ANALYZE %.1fs, measured in %.1fs", rep.AnalyzeSeconds, rep.MeasureSeconds)
	}
	p("wall clock: %.1fs total, %.1fs of it in the stages above, %.1fs harness overhead",
		rep.TotalSeconds,
		rep.ProvisionSeconds+rep.CorpusLoadSeconds+rep.AnalyzeSeconds+rep.MeasureSeconds,
		rep.TotalSeconds-(rep.ProvisionSeconds+rep.CorpusLoadSeconds+rep.AnalyzeSeconds+rep.MeasureSeconds))
	p("session settings (conditions of every number below): %s", strings.Join(rep.SessionSettings, ", "))
	p("")

	names := make([]string, 0, len(rep.Corpus))
	for n := range rep.Corpus {
		names = append(names, n)
	}
	sort.Strings(names)
	p("%-28s %10s %10s", "table", "rows", "expected")
	for _, n := range names {
		want, has := rep.CorpusWant[n]
		mark := ""
		if has && want != rep.Corpus[n] {
			mark = "  <-- MISMATCH"
		}
		p("%-28s %10d %10s%s", n, rep.Corpus[n], itoa(want, has), mark)
	}
	p("")

	p("MEASURED ITEMS (docs/27 budgets%s)", map[bool]string{true: "", false: "; THIS TIER IS NOT AN SLO COMPARISON"}[rep.SLOComparison])
	p("%-22s %6s %9s %9s %9s %9s %9s %8s %5s",
		"item", "n", "min", "p50", "p95", "p99", "max", "cold", "met")
	for _, r := range append(append([]RowReport{}, rep.Rows...), rep.Probes...) {
		met := "n/a"
		if r.SLOTargetMs > 0 {
			if rep.SLOComparison {
				met = "NO"
				if r.Met {
					met = "yes"
				}
			} else {
				met = "-"
			}
		}
		p("%-22s %6d %9s %9s %9s %9s %9s %8s %5s",
			r.Name, r.Samples, fms(r.MinMs), fms(r.P50Ms), fms(r.P95Ms), fms(r.P99Ms), fms(r.MaxMs), fms(r.ColdMs), met)
		// The row's name says nothing about WHERE the number comes from, and
		// "project_overview p95 981ms" is not checkable by a reader who cannot
		// see which route, which service call and which queries it is. Printed
		// under the row, indented like the PROBES section's descriptions, so the
		// table and the JSON make the same claim.
		//
		// The discriminator is SLOItem, not SLOTargetMs: an SLOItem is set
		// exactly for the rows that quote a docs/27 budget line, and a probe has
		// none. The probes' own surface and boundary are printed in their
		// section below rather than twice.
		if r.SLOItem != "" {
			p("    %s", r.Surface)
			p("    boundary: %s", r.Boundary)
		}
	}
	p("")
	p("SLO COMPARISON (p95 / docs/27 target)")
	if !rep.SLOComparison {
		p("  NOT PERFORMED: scale %q is smaller than the %q capacity baseline docs/27:20 names.", rep.Scale, specScale.Name)
		p("  A p95 of a smaller corpus is not a p95 of the budget's corpus, and printing the ratio anyway")
		p("  would be a scale comparison wearing an SLO comparison's name.")
	} else {
		p("  %-22s %9s %9s %7s %s", "item", "p95", "target", "ratio", "met")
		for _, r := range rep.Rows {
			if r.SLOTargetMs == 0 {
				continue
			}
			verdict := "NO"
			if r.Met {
				verdict = "yes"
			}
			p("  %-22s %9s %9s %7s %s", r.Name, fms(r.P95Ms), fms(r.SLOTargetMs), fmt.Sprintf("%.2fx", r.Ratio), verdict)
			p("      %s", r.SLOItem)
		}
	}
	p("")
	p("PROBES (no docs/27 budget; ceilings in the gate are this harness's own)")
	for _, r := range rep.Probes {
		p("  %-22s p95 %s over %d samples", r.Name, fms(r.P95Ms), r.Samples)
		p("      %s", firstLine(r.Surface))
		p("      boundary: %s", r.Boundary)
	}
	p("")
	p("NOT MEASURED")
	for _, s := range rep.NotMeasured {
		p("  - %s", s)
	}
	if rep.Gate != nil {
		p("")
		p("GATE: %s", map[bool]string{true: "PASS", false: "FAIL"}[rep.Gate.Passed])
		for _, c := range rep.Gate.Checks {
			mark := "ok  "
			if !c.Passed {
				mark = "FAIL"
			}
			p("  [%s] %-46s %s", mark, c.Name, firstLine(c.Detail))
		}
	}
}

func fms(v float64) string {
	if v == 0 {
		return "-"
	}
	if v < 10 {
		return fmt.Sprintf("%.2fms", v)
	}
	return fmt.Sprintf("%.0fms", v)
}

func itoa(v int, ok bool) string {
	if !ok {
		return "-"
	}
	return fmt.Sprintf("%d", v)
}

// The ratio's caveat — that a ratio exists only at the spec tier, and that a
// ci-tier "Met: false" is "not compared" rather than "failed" — is deliberately
// not a package-level constant. It is printed where a reader meets it (the SLO
// COMPARISON section, which says NOT PERFORMED and why) and carried for a
// machine by Report.SLOComparison plus the Ratio/Met doc comments above. A
// constant would be a third copy, held by nobody, free to drift from both.
