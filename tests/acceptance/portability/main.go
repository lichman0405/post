package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Exit codes. A refusal is NOT a crash and must not look like one: the
// acceptance criterion is "the import side refuses AND names where", and a
// caller has to be able to tell that apart from "the driver broke".
//
//	0  the command did what it says
//	1  the command could not run (a missing service, a bad argument)
//	3  a deliberate NO: the bundle does not verify, or the target refuses it
const (
	exitOK      = 0
	exitFailed  = 1
	exitRefused = 3
	refusalLine = "REFUSED: %v"
)

func usage() {
	fmt.Fprint(os.Stderr, `portability — T1208's portable export / import driver.

Usage:
  portability assemble    --project <slug|id> [--plan examples/seed-demo/demo-plan.json]
  portability export      --project <slug|id> [--release <version>] --out <dir>
  portability check       --bundle <dir>
  portability compare     --a <dir> --b <dir>
  portability fingerprint --project <slug|id> [--out <file>]
  portability import      --bundle <dir> [--repo-name <name>]
  portability verify      --bundle <dir>
  portability tamper      --bundle <dir> --out <dir> --kind blob|manifest|commit

Source services come from POST_DATABASE_URL, POST_BLOB_*, POST_GITEA_*;
the target from the same names under POST_TGT_. There is no in-memory mode:
every command below talks to a real PostgreSQL, a real S3-compatible store
and a real GitProvider (docs/67).
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitFailed)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		var r refusal
		if errors.As(err, &r) {
			fmt.Fprintf(os.Stderr, refusalLine+"\n", err)
			os.Exit(exitRefused)
		}
		fmt.Fprintf(os.Stderr, "portability: %v\n", err)
		os.Exit(exitFailed)
	}
}

// run dispatches one subcommand. Each returns an error; nothing here calls
// os.Exit, so every path out of the program goes through main's classifier.
func run(cmd string, args []string) error {
	ctx := context.Background()
	switch cmd {
	case "assemble":
		fs := flags("assemble")
		project := fs.String("project", "", "project slug or id")
		plan := fs.String("plan", "examples/seed-demo/demo-plan.json", "fixture plan documenting the blob contents")
		prefix := fs.String("env-prefix", "POST_", "environment variable prefix")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *project == "" {
			return fmt.Errorf("assemble: --project is required")
		}
		e, err := loadEnv("source", *prefix)
		if err != nil {
			return err
		}
		pool, done, err := connect(ctx, e)
		if err != nil {
			return err
		}
		defer done()
		return Assemble(ctx, pool, e, *project, *plan)

	case "export":
		fs := flags("export")
		project := fs.String("project", "", "project slug or id")
		release := fs.String("release", "", "release version (default: the most recent)")
		out := fs.String("out", "", "bundle directory to write")
		prefix := fs.String("env-prefix", "POST_", "environment variable prefix")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *project == "" || *out == "" {
			return fmt.Errorf("export: --project and --out are required")
		}
		e, err := loadEnv("source", *prefix)
		if err != nil {
			return err
		}
		pool, done, err := connect(ctx, e)
		if err != nil {
			return err
		}
		defer done()
		x, err := Export(ctx, pool, e, *project, *release, *out)
		if err != nil {
			return err
		}
		digest, err := x.ContentDigest(*out)
		if err != nil {
			return err
		}
		fmt.Printf("EXPORT project=%s release=%s state=%s\n", x.ProjectSlug, x.ReleaseVersion, x.StateID)
		fmt.Printf("EXPORT rsg_state_hash=%s\n", x.Identifiers.RSGStateHash)
		fmt.Printf("EXPORT release_manifest_hash=%s\n", x.Identifiers.ReleaseManifestHash)
		fmt.Printf("EXPORT git=%s %s=%s\n", x.Identifiers.Git.Repository, x.Identifiers.Git.HeadRef, x.Identifiers.Git.CommitSHA)
		fmt.Printf("EXPORT blob_rows=%d\n", len(x.Blobs))
		fmt.Printf("EXPORT content_digest=%s\n", digest)
		fmt.Printf("EXPORT bundle=%s\n", mustAbs(*out))
		return nil

	case "compare":
		fs := flags("compare")
		a := fs.String("a", "", "first bundle directory")
		b := fs.String("b", "", "second bundle directory")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *a == "" || *b == "" {
			return fmt.Errorf("compare: --a and --b are required")
		}
		return Compare(*a, *b)

	case "check":
		fs := flags("check")
		bundle := fs.String("bundle", "", "bundle directory")
		if err := fs.Parse(args); err != nil {
			return err
		}
		x, err := Check(*bundle)
		if err != nil {
			return err
		}
		fmt.Printf("CHECK OK bundle=%s rsg_state_hash=%s release_manifest_hash=%s git=%s@%s blobs=%d files=%d\n",
			mustAbs(*bundle), x.Identifiers.RSGStateHash, x.Identifiers.ReleaseManifestHash,
			x.Identifiers.Git.HeadRef, short(x.Identifiers.Git.CommitSHA), len(x.Blobs), len(x.Files))
		return nil

	case "fingerprint":
		fs := flags("fingerprint")
		project := fs.String("project", "", "project slug or id")
		out := fs.String("out", "", "write the fingerprint here (default: stdout)")
		prefix := fs.String("env-prefix", "POST_", "environment variable prefix")
		if err := fs.Parse(args); err != nil {
			return err
		}
		e, err := loadEnv("source", *prefix)
		if err != nil {
			return err
		}
		pool, done, err := connect(ctx, e)
		if err != nil {
			return err
		}
		defer done()
		f, err := Fingerprint0(ctx, pool, e, *project)
		if err != nil {
			return err
		}
		body, err := json.MarshalIndent(f, "", "  ")
		if err != nil {
			return err
		}
		if *out == "" {
			fmt.Println(string(body))
			return nil
		}
		return os.WriteFile(*out, append(body, '\n'), 0o644)

	case "import":
		fs := flags("import")
		bundle := fs.String("bundle", "", "bundle directory")
		repoName := fs.String("repo-name", "", "target repository name (default: <source>-import)")
		targetOrg := fs.String("target-org", "", "Gitea organisation to import into (default: the token's own namespace)")
		prefix := fs.String("env-prefix", "POST_TGT_", "environment variable prefix for the target")
		if err := fs.Parse(args); err != nil {
			return err
		}
		e, err := loadEnv("target", *prefix)
		if err != nil {
			return err
		}
		pool, done, err := connect(ctx, e)
		if err != nil {
			return err
		}
		defer done()
		x, ids, err := ImportBundle(ctx, pool, e, *bundle, *repoName, *targetOrg)
		if err != nil {
			return err
		}
		return CompareIdentifiers(x, *ids)

	case "verify":
		fs := flags("verify")
		bundle := fs.String("bundle", "", "bundle directory")
		prefix := fs.String("env-prefix", "POST_TGT_", "environment variable prefix for the target")
		if err := fs.Parse(args); err != nil {
			return err
		}
		e, err := loadEnv("target", *prefix)
		if err != nil {
			return err
		}
		pool, done, err := connect(ctx, e)
		if err != nil {
			return err
		}
		defer done()
		x, err := Check(*bundle)
		if err != nil {
			return err
		}
		ids, err := DeriveIdentifiers(ctx, pool, e, x.ProjectID)
		if err != nil {
			return err
		}
		// The repository the target was imported into is recorded in the
		// target's own project row, so a standalone verify reads it there
		// rather than being told.
		var external string
		if err := pool.QueryRow(ctx, `SELECT coalesce(git_repository_external_id, '') FROM projects WHERE id = $1`, x.ProjectID).Scan(&external); err != nil {
			return err
		}
		parts := strings.SplitN(external, "/", 2)
		if len(parts) != 2 {
			return fmt.Errorf("the target's project row carries no repository (%q)", external)
		}
		home, err := workDir("git-verify")
		if err != nil {
			return err
		}
		defer os.RemoveAll(home)
		refs, err := remoteRefs(repoURL(e.GiteaBase, parts[0], parts[1]), e.GiteaToken, home)
		if err != nil {
			return err
		}
		ids.Git = x.Identifiers.Git
		ids.Git.Repository = external
		if refs[ids.Git.HeadRef] != ids.Git.CommitSHA {
			return fmt.Errorf("the target's %s is %s, the export pins %s", ids.Git.HeadRef, short(refs[ids.Git.HeadRef]), short(ids.Git.CommitSHA))
		}
		return CompareIdentifiers(x, ids)

	case "tamper":
		fs := flags("tamper")
		bundle := fs.String("bundle", "", "the bundle to copy")
		out := fs.String("out", "", "where the damaged copy goes")
		kind := fs.String("kind", "", "blob|manifest|commit")
		if err := fs.Parse(args); err != nil {
			return err
		}
		if *bundle == "" || *out == "" || *kind == "" {
			return fmt.Errorf("tamper: --bundle, --out and --kind are required")
		}
		return Tamper(*bundle, *out, *kind)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func connect(ctx context.Context, e env) (*pgxpool.Pool, func(), error) {
	pool, err := pgxpool.New(ctx, e.PostgresURL)
	if err != nil {
		return nil, nil, fmt.Errorf("connect to the %s database: %w", e.Name, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, nil, fmt.Errorf("ping the %s database: %w", e.Name, err)
	}
	return pool, pool.Close, nil
}
