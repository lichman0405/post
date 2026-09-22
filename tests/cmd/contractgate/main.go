// Command contractgate is the T1205 READER: it runs the route/comparator
// instrument over the real tree and reports whether every mounted /api/v1
// route is either declared in specs/api/openapi.yaml or exempted in
// specs/api/openapi-exemptions.yaml with a citation that resolves.
//
// WHY THIS IS A COMMAND AND NOT A TEST — read this before wiring it into CI.
//
// CI's `go` job is `go test $(go list ./... | grep -v '/tests/integration')`,
// and every task's G2 re-runs exactly that (specs/orchestrator/gates.json,
// pinned to .github/workflows/ci.yml by TestGatesSpecSyncsWithCIWorkflow). A
// _test.go asserting "zero undocumented routes" over the real tree would
// therefore fail inside every other task's acceptance gate on a gap those
// tasks did not create and cannot close: specs/** is Supervisor-only (CLAUDE.md
// §8.1), so the contract and the exemption list are written by the Supervisor
// and by nobody else.
//
// The instrument is green (tests/contract, synthetic fixtures). This reading
// is red while the tree has undocumented routes, and it says so out loud:
//
//	exit 0  every mounted route is contracted or exempted, and every contract
//	        entry is mounted
//	exit 3  findings exist (they are listed; this is a measurement, not a
//	        crash)
//	exit 1  the instrument could not read its inputs
//
// Nothing in this file is wired into `make check`, into CI, or into any go
// test. Wiring it into the total gate is the Supervisor's closing step, taken
// after the contract and the exemption list are complete — at which point this
// command exits 0 and can be trusted to stay exit 0.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lichman0405/post/tests/contract"
)

const (
	exitOK         = 0
	exitInstrument = 1
	exitFindings   = 3
	exitInventory  = 4
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	fs := flag.NewFlagSet("contractgate", flag.ContinueOnError)
	root := fs.String("root", ".", "repository root to read")
	asJSON := fs.Bool("json", false, "emit the gate report as JSON")
	writeInventory := fs.String("write-inventory", "", "write/refresh the route inventory at this path (keeps existing judgements)")
	checkInventory := fs.String("check-inventory", "", "verify an inventory file against the tree")
	writeMCP := fs.String("write-mcp", "", "write the MCP reconciliation at this path")
	skipExempt := fs.Bool("skip-exemptions", false, "do not read the exemption list (diagnostic: shows what a missing exemption file would look like)")
	contractPath := fs.String("contract", filepath.Join("specs", "api", "openapi.yaml"), "contract path, relative to -root")
	exemptPath := fs.String("exemptions", filepath.Join("specs", "api", "openapi-exemptions.yaml"), "exemption list path, relative to -root")
	resolve := fs.String("resolve", "", "resolve one `decision:` citation against -root and print what it landed on, then stop")
	source := fs.String("source", "run against .", "what the inventory records as its source (a commit, a tree, a run)")
	if err := fs.Parse(args); err != nil {
		return exitInstrument
	}

	// -resolve exists because a suggested exemption is only worth pasting if its
	// citation lands on text: the Supervisor can check a citation before writing
	// it, and so can the next Worker.
	if *resolve != "" {
		ref, err := contract.ResolveDecision(*root, *resolve)
		if err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: %v\n", err)
			return exitInstrument
		}
		fmt.Printf("citation: %s\n  file:    %s\n  kind:    %s\n  locator: %s\n  heading: %s\n",
			*resolve, ref.Path, ref.Kind, ref.Locator, ref.Heading)
		return exitOK
	}

	g, err := contract.Run(contract.Options{
		Root: *root, ContractPath: *contractPath, ExemptionPath: *exemptPath,
		SkipExemptions: *skipExempt,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "contractgate: cannot read its inputs: %v\n", err)
		return exitInstrument
	}

	if *writeInventory != "" {
		invPath := resolvePath(*root, *writeInventory)
		prev, err := contract.LoadInventory(invPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: %v\n", err)
			return exitInstrument
		}
		inv := contract.BuildInventory(g, prev, *source)
		if err := contract.WriteInventory(invPath, inv); err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: writing inventory: %v\n", err)
			return exitInstrument
		}
		printSummary(os.Stdout, g)
		fmt.Printf("inventory written: %s (%d routes, %d suggested exemptions)\n", invPath, len(inv.Routes), len(inv.Suggested))
		return exitOK
	}

	if *writeMCP != "" {
		mcpPath := resolvePath(*root, *writeMCP)
		rep, err := contract.ReconcileMCP(contract.MCPOptions{Root: *root})
		if err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: MCP reconciliation: %v\n", err)
			return exitInstrument
		}
		if *asJSON {
			raw, _ := json.MarshalIndent(rep, "", "  ")
			fmt.Println(string(raw))
		} else {
			printMCP(os.Stdout, rep)
		}
		if err := contract.WriteMCPInventory(mcpPath, rep); err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: writing MCP inventory: %v\n", err)
			return exitInstrument
		}
		fmt.Printf("mcp inventory written: %s\n", mcpPath)
		return exitOK
	}

	if *checkInventory != "" {
		invPath := resolvePath(*root, *checkInventory)
		inv, err := contract.LoadInventory(invPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: %v\n", err)
			return exitInstrument
		}
		problems := contract.CheckInventory(g, inv, *root)
		if len(problems) == 0 {
			fmt.Printf("inventory check: OK — %s agrees with this run (%d mounted, %d undocumented)\n", invPath, g.Counts.Mounted, g.Counts.Undocumented)
			return exitOK
		}
		fmt.Fprintf(os.Stderr, "inventory check: %d problem(s):\n", len(problems))
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "  - %s\n", p)
		}
		return exitInventory
	}

	if *asJSON {
		g.Contract = nil
		g.Exempt = nil
		raw, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "contractgate: %v\n", err)
			return exitInstrument
		}
		fmt.Println(string(raw))
	} else {
		printReport(os.Stdout, g)
	}
	if !g.OK() {
		return exitFindings
	}
	return exitOK
}

func printSummary(w *os.File, g *contract.Gate) {
	c := g.Counts
	fmt.Fprintf(w, "summary: mounted=%d in_contract=%d catchall_resolved=%d exempted=%d undocumented=%d | contract_ops=%d unmounted=%d | exemptions=%d cited=%d | files_scanned=%d\n",
		c.Mounted, c.InContract, c.Catchall, c.InExemptions, c.Undocumented,
		c.ContractOps, c.Unmounted, c.Exemptions, c.Cited, c.ScannedFiles)
}

func printReport(w *os.File, g *contract.Gate) {
	fmt.Fprintf(w, "contractgate: %s (server prefix %s, contract %s, exemptions %s)\n", g.Root, g.Prefix, "specs/api/openapi.yaml", g.Exempt.Path)
	printSummary(w, g)
	for _, kind := range []string{contract.Undocumented, contract.Unmounted, contract.ExemptUncited, contract.ExemptUnused, contract.ExemptionCatchall, contract.UnresolvedKind, contract.Malformed} {
		found := g.FindingsOfKind(kind)
		if len(found) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s (%d)\n", strings.ToUpper(kind), len(found))
		for _, f := range found {
			where := f.File
			if f.Line > 0 {
				where = fmt.Sprintf("%s:%d", f.File, f.Line)
			}
			label := strings.TrimSpace(f.Method + " " + f.Path)
			if label == "" {
				label = where
				where = ""
			}
			fmt.Fprintf(w, "  %-70s %s\n", label, strings.TrimSpace(where))
			fmt.Fprintf(w, "      %s\n", f.Detail)
		}
	}
	if len(g.Resolved) > 0 {
		fmt.Fprintf(w, "\nCATCH-ALLS RESOLVED TO A CONTRACTED :VERB OPERATION (%d) — documented, never exempted\n", len(g.Resolved))
		for _, r := range g.Resolved {
			fmt.Fprintf(w, "  %-8s %-72s -> %s\n", r.Method, r.Path, r.Matched)
		}
	}
	if len(g.Outside) > 0 {
		lines := make([]string, 0, len(g.Outside))
		for _, r := range g.Outside {
			lines = append(lines, fmt.Sprintf("%-8s %-40s %s:%d", r.Method, r.Path, r.File, r.Line))
		}
		sort.Strings(lines)
		fmt.Fprintf(w, "\nOUTSIDE THE GATE (not under %s, %d registrations) — enumerated, gated by nothing\n", g.Prefix, len(g.Outside))
		for _, l := range lines {
			fmt.Fprintf(w, "  %s\n", l)
		}
	}
	if len(g.Advisory) > 0 {
		fmt.Fprintf(w, "\nADVISORY: /api/v1 literals the scan did not classify (%d) — read, not hidden\n", len(g.Advisory))
		for _, a := range g.Advisory {
			fmt.Fprintf(w, "  %s:%d in %s\n      expr: %s\n      why:  %s\n", a.File, a.Line, a.In, a.Expr, a.Why)
		}
	}
	if g.OK() {
		fmt.Fprintf(w, "\nno findings: every mounted route is contracted or exempted, every contract entry is mounted.\n")
		return
	}
	fmt.Fprintf(w, "\n%d finding(s). Exit 3.\n", len(g.Findings))
}

// resolvePath makes a possibly relative path absolute against root, so that
// -write-inventory, -check-inventory and -write-mcp are interpreted the same
// way regardless of the working directory. Absolute paths are left untouched.
func resolvePath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}

func printMCP(w *os.File, rep *contract.MCPReport) {
	fmt.Fprintf(w, "MCP reconciliation: catalog %s declares %d tool(s); %d have a dispatch site; %d do not.\n",
		rep.CatalogPath, rep.Declared, rep.Implemented, rep.Missing)
	for _, t := range rep.Tools {
		state := "NOT IMPLEMENTED"
		if t.Dispatched {
			state = "dispatch site found"
		}
		fmt.Fprintf(w, "  %-28s %-12s %-16s %s\n", t.Name, t.Mode, state, strings.Join(t.Args, ","))
	}
	if len(rep.OutOfCatalog) > 0 {
		fmt.Fprintf(w, "implemented-but-not-declared candidates (%d):\n", len(rep.OutOfCatalog))
		for _, s := range rep.OutOfCatalog {
			fmt.Fprintf(w, "  %s:%d %s\n", s.File, s.Line, s.In)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(w, "note: %s\n", n)
	}
}
