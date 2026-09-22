// Package main is T1208's portable-export driver: it exports one released
// project as a portable bundle, checks that bundle's integrity, imports it
// into a clean environment, and compares the four identifier classes
// CLAUDE.md §8 says a Release pins.
//
// It is a Go main package under tests/acceptance/ for the same reason
// tests/acceptance/seeddemo is: a shell script cannot call the product's own
// canonicalization, and the whole point here is that the export side and the
// import side derive their hashes with the SAME code the product uses
// (internal/rsg/manifest for the RSG snapshot hash, internal/application/
// releases for the release manifest hash). Re-implementing either would make
// "the import side agrees with the export side" a statement about two
// implementations rather than about the data.
//
// The driver is deliberately NOT named *-real-services-e2e.sh: that glob is
// wired to gates.json jobs by
// internal/devorchestrator/gate_spec_test.go:TestEveryRealServicesGateScriptIsWiredAsAJob,
// and adding a job to specs/orchestrator/gates.json is outside a Worker's
// scope. The wiring this script needs is reported as a follow-up instead.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// env is one named set of real services. PostgresURL/Blob*/Gitea* carry the
// dev stack's own variable names so a developer can source the same shell
// they use for `make run`.
type env struct {
	Name         string
	PostgresURL  string
	BlobEndpoint string
	BlobBucket   string
	BlobAccess   string
	BlobSecret   string
	GiteaBase    string
	GiteaOwner   string
	GiteaToken   string
}

// loadEnv reads one environment from the process environment.
//
// The SOURCE environment wears the product's own variable names, so a
// developer who can run `make run` can export without learning a second set.
// The TARGET environment is the same names under a POST_TGT_ prefix.
//
// There is deliberately NO fallback from one to the other: a missing target
// variable is an error naming it, never a silent default to the source. An
// importer that fell back to the source database would import a project into
// the database it was exported from and report success.
func loadEnv(name, prefix string) (env, error) {
	get := func(suffix string) string { return strings.TrimSpace(os.Getenv(prefix + suffix)) }
	e := env{
		Name:         name,
		PostgresURL:  get("DATABASE_URL"),
		BlobEndpoint: get("BLOB_ENDPOINT"),
		BlobBucket:   get("BLOB_BUCKET"),
		BlobAccess:   get("BLOB_ACCESS_KEY"),
		BlobSecret:   get("BLOB_SECRET_KEY"),
		GiteaBase:    strings.TrimRight(get("GITEA_BASE_URL"), "/"),
		GiteaOwner:   get("GITEA_OWNER"),
		GiteaToken:   get("GITEA_TOKEN"),
	}
	if e.BlobBucket == "" {
		e.BlobBucket = "post"
	}
	var missing []string
	for _, f := range []struct{ key, val string }{
		{prefix + "DATABASE_URL", e.PostgresURL},
		{prefix + "BLOB_ENDPOINT", e.BlobEndpoint},
		{prefix + "BLOB_ACCESS_KEY", e.BlobAccess},
		{prefix + "BLOB_SECRET_KEY", e.BlobSecret},
		{prefix + "GITEA_BASE_URL", e.GiteaBase},
		{prefix + "GITEA_TOKEN", e.GiteaToken},
	} {
		if f.val == "" {
			missing = append(missing, f.key)
		}
	}
	if len(missing) > 0 {
		return env{}, fmt.Errorf("environment %s is missing %s — this driver talks to real services only (docs/67: no in-memory store stands in for the critical hop)", name, strings.Join(missing, ", "))
	}
	return e, nil
}

// workDir makes one temp directory for a run stage. Callers remove it; the
// driver removes every one it makes so the next run starts clean.
func workDir(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", "t1208-"+prefix+"-")
	if err != nil {
		return "", err
	}
	return dir, nil
}

// mustAbs resolves a path against the current directory, so a message names
// the file a reader can open.
func mustAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}
