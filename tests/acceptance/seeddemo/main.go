// Command seeddemo builds and verifies the POST demo seed
// (docs/34_SEED_DEMO_PROJECT.md) through the product's own HTTP API.
//
//	seeddemo build  --api URL --db URL --plan FILE [--external=1|0] [--report FILE]
//	seeddemo verify --db URL --plan FILE --project-slug SLUG [--external=1|0] [--json FILE]
//	seeddemo reset  --db URL
//
// build writes into an EMPTY database through the API; verify reads the counts
// back out of PostgreSQL. They are separate commands because the acceptance
// criterion is precisely that the second one can disagree with the first:
// a verifier that trusted the builder could never report the builder short.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "build":
		err = cmdBuild(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "reset":
		err = cmdReset(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "seeddemo: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "seeddemo: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `seeddemo — build and verify the POST demo seed through the product API

  seeddemo build  --api http://127.0.0.1:PORT --db postgres://... --plan examples/seed-demo/demo-plan.json
                  [--external=1|0] [--report FILE]
  seeddemo verify --db postgres://... --plan examples/seed-demo/demo-plan.json
                  --project-slug demo-mof-humidity-separation
                  [--external=1|0] [--external-email EMAIL] [--json FILE]
  seeddemo reset  --db postgres://...            (DROPS and recreates that database)
`)
}

func cmdBuild(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("build", flag.ExitOnError)
	api := fs.String("api", envOr("SEED_DEMO_API", "http://127.0.0.1:18099"), "base URL of the running product API")
	db := fs.String("db", os.Getenv("SEED_DEMO_DB_URL"), "PostgreSQL URL of the target database")
	plan := fs.String("plan", "examples/seed-demo/demo-plan.json", "path to the demo plan")
	external := fs.Int("external", 1, "1 to build the external contribution (fork + external evidence + PR), 0 to skip it")
	report := fs.String("report", "", "write the machine-readable build report here (empty: stdout)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return fmt.Errorf("--db (or SEED_DEMO_DB_URL) is required")
	}
	rep, err := runBuild(ctx, buildConfig{
		APIBase:  *api,
		DBURL:    *db,
		PlanPath: *plan,
		External: *external != 0,
	})
	if rep != nil {
		raw, _ := json.MarshalIndent(rep, "", "  ")
		if *report != "" {
			if werr := os.WriteFile(*report, append(raw, '\n'), 0o644); werr != nil {
				fmt.Fprintf(os.Stderr, "seeddemo: write report: %v\n", werr)
			}
		} else {
			fmt.Println(string(raw))
		}
		printBuildSummary(rep)
	}
	return err
}

func printBuildSummary(rep *buildReport) {
	created, reused, failed, refused := 0, 0, 0, 0
	for _, it := range rep.Items {
		switch it.Status {
		case "created":
			created++
		case "reused":
			reused++
		case "failed":
			failed++
		case "refused":
			refused++
		}
	}
	fmt.Fprintf(os.Stderr, "seeddemo build: %d items created, %d reused, %d refused (recorded product refusals), %d failed\n",
		created, reused, refused, failed)
	for _, it := range rep.Items {
		if it.Status == "failed" {
			fmt.Fprintf(os.Stderr, "  FAILED  %s — %s\n", it.Name, it.Detail)
		}
	}
	for _, it := range rep.Items {
		if it.Status == "refused" {
			fmt.Fprintf(os.Stderr, "  REFUSED %s — %s\n", it.Name, it.Detail)
		}
	}
	for _, n := range rep.Notes {
		fmt.Fprintf(os.Stderr, "  note: %s\n", n)
	}
}

func cmdVerify(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	db := fs.String("db", os.Getenv("SEED_DEMO_DB_URL"), "PostgreSQL URL of the database to read")
	plan := fs.String("plan", "examples/seed-demo/demo-plan.json", "path to the demo plan (for the slug and floors)")
	slug := fs.String("project-slug", "", "the demo project's slug (empty: read from the plan)")
	external := fs.Int("external", 1, "1 to also check the fork-dependent items, 0 to report them as unchecked")
	externalEmail := fs.String("external-email", "", "the external demo user's email (empty: read from the plan)")
	jsonOut := fs.String("json", "", "write the machine-readable verification report here")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *db == "" {
		return fmt.Errorf("--db (or SEED_DEMO_DB_URL) is required")
	}
	planDoc, err := LoadPlan(*plan)
	if err != nil {
		return err
	}
	if *slug == "" {
		*slug = planDoc.Project.Slug
	}
	if *externalEmail == "" {
		for _, u := range planDoc.Users {
			if u.Key == planDoc.External.User {
				*externalEmail = u.Email
			}
		}
	}
	rep, err := runVerify(ctx, verifyConfig{
		DBURL:         *db,
		PlanPath:      *plan,
		ProjectSlug:   *slug,
		External:      *external != 0,
		ExternalEmail: *externalEmail,
	})
	if rep == nil {
		return err
	}
	raw, _ := json.MarshalIndent(rep, "", "  ")
	if *jsonOut != "" {
		if werr := os.WriteFile(*jsonOut, append(raw, '\n'), 0o644); werr != nil {
			fmt.Fprintf(os.Stderr, "seeddemo: write report: %v\n", werr)
		}
	}
	printVerifyReport(rep)
	if len(rep.Failed) > 0 {
		return fmt.Errorf("the seeded demo does not match docs/34: %d item(s) short: %s",
			len(rep.Failed), strings.Join(rep.Failed, "; "))
	}
	return err
}

func printVerifyReport(rep *verifyReport) {
	fmt.Printf("%-4s %-8s %-8s %s\n", "STAT", "REQUIRED", "ACTUAL", "ITEM")
	for _, c := range rep.Checks {
		fmt.Printf("%-4s %-8d %-8d %s\n", strings.ToUpper(c.Status), c.Required, c.Actual, c.Item)
		if c.Status != "pass" && c.Detail != "" {
			fmt.Printf("       %s\n", c.Detail)
		}
	}
	fmt.Printf("\n%d passed, %d failed, %d not checked (database %s, project %s)\n",
		rep.PassedCount, rep.FailedCount, len(rep.Unavailable), rep.Database, rep.ProjectSlug)
	if len(rep.Failed) > 0 {
		fmt.Printf("SHORT: %s\n", strings.Join(rep.Failed, "; "))
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
