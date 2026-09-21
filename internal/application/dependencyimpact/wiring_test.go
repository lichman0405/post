package dependencyimpact

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// What this file checks is the SHAPE of the feature, not its behaviour:
//
//   - the analysis runs in the background worker, off the published event
//     log (docs/20 §"Go worker" names impact analysis in the worker's job
//     list; docs/19 §3's 「上游变更触发」 says the trigger is the change, not
//     a user);
//   - the API binary registers NO route that runs it, because 「上游变更触发」
//     has no user to authenticate: an endpoint would change the trigger to
//     "somebody remembered to press it", which is a different feature with
//     the same name.
//
// It is a source-level check, and deliberately so. The alternative — probing
// a running mux for a route — can only prove that one particular path does
// not exist; what the requirement forbids is the CATEGORY, and the only
// place the category is visible is the set of route patterns the API
// registers plus the constructors each binary calls.

// TestAnalysisRunsInTheWorkerAndNotInTheAPI is the acceptance's "runs in the
// worker, no new manual-trigger HTTP endpoint", asserted against the two
// binaries' own source.
func TestAnalysisRunsInTheWorkerAndNotInTheAPI(t *testing.T) {
	workerMain := readRepoFile(t, "cmd", "worker", "main.go")
	apiFiles := goFilesUnder(t, "cmd", "api")

	// The worker mounts the projector, and says so where an operator looks.
	if !strings.Contains(workerMain, "dependencyimpact.NewProjector(") {
		t.Error("cmd/worker does not mount dependencyimpact.NewProjector: the analysis is delivered but nothing drives it in a running process")
	}
	if !strings.Contains(workerMain, "persistence.NewDependencyImpactStore(") {
		t.Error("cmd/worker mounts no dependency impact store")
	}

	// No API file registers a route whose path mentions impact, and none
	// constructs the projector or calls the batch directly: the analysis is
	// not something a request can start.
	for path, src := range apiFiles {
		for _, pattern := range routePatterns(src) {
			lower := strings.ToLower(pattern)
			if strings.Contains(lower, "impact") || strings.Contains(lower, "dependency") {
				t.Errorf("%s registers route %q: the impact analysis is triggered by an upstream change, not by a request (docs/19 §3)", path, pattern)
			}
		}
		if strings.Contains(src, "dependencyimpact.NewProjector(") {
			t.Errorf("%s constructs the analysis projector; it belongs in cmd/worker", path)
		}
		if strings.Contains(src, ".AnalyzeBatch(") {
			t.Errorf("%s drives an analysis pass; the pass belongs to the worker's projector", path)
		}
	}

	// The API DOES read the analysis, and that is not the forbidden shape:
	// the PR first screen renders what a change reaches, computed on demand
	// for a reader (prchecks.PullRequestImpact). What must not exist is a
	// route that RUNS an analysis (writes alerts); reading one is a read.
	if !strings.Contains(concatenate(apiFiles), "dependencyimpact.NewService(") {
		t.Error("cmd/api does not wire the analysis read surface: the PR first screen's dependency impact line has nothing behind it")
	}

	// A control on the extraction itself: if routePatterns found nothing, the
	// loop above would pass vacuously.
	if len(routePatterns(workerMain)) > 0 {
		t.Error("the route extractor matched a pattern in a binary that registers no routes — it is scanning something other than route registrations")
	}
	apiPatterns := 0
	for _, src := range apiFiles {
		apiPatterns += len(routePatterns(src))
	}
	if apiPatterns < 10 {
		t.Fatalf("found only %d route patterns under cmd/api — the extractor is not reading the registrations", apiPatterns)
	}
}

// routePatterns extracts the route patterns an API source registers: the
// first string literal of every http.ServeMux registration, which is where
// this repository writes them ("GET /api/v1/projects/{projectId}/...").
//
// It reads them as text rather than by importing the packages, because the
// point is the SOURCE of every registration (including ones behind a build
// tag or an untaken branch), and because importing cmd/api's tree would drag
// in a database.
func routePatterns(src string) []string {
	var out []string
	for _, marker := range []string{"HandleFunc(\"", "Handle(\"", "HandleFunc(`", "Handle(`"} {
		rest := src
		for {
			i := strings.Index(rest, marker)
			if i < 0 {
				break
			}
			rest = rest[i+len(marker):]
			quote := marker[len(marker)-1]
			end := strings.IndexByte(rest, quote)
			if end < 0 {
				break
			}
			pattern := rest[:end]
			// Route patterns are method-qualified paths; a string that is
			// not one is something else a handler is registered with (a
			// variable, a helper call) and is not a route this test can
			// judge.
			if strings.HasPrefix(pattern, "/") || (strings.Contains(pattern, " /api/")) {
				out = append(out, pattern)
			}
			rest = rest[end:]
		}
	}
	return out
}

// TestMigrationKeysTheDedupeOnTheFieldsThePayloadCarries pins the partial
// unique index to the payload fields AlertEvent writes.
//
// Idempotency here is the database's, not this package's: the alert's INSERT
// carries ON CONFLICT DO NOTHING and the conflict it is meant to hit is this
// index. If the index extracted a different key — or none — the ON CONFLICT
// would never fire and the analysis would write one alert per pass, growing
// without bound. The integration test proves the write converges; this
// proves the two halves name the same key.
func TestMigrationKeysTheDedupeOnTheFieldsThePayloadCarries(t *testing.T) {
	sql := readRepoFile(t, "infra", "migrations", "00111_dependency_impact.sql")
	for _, want := range []string{
		"CREATE UNIQUE INDEX",
		"outbox_events",
		EventImpactDetected,
		"payload->>'" + AlertFieldTriggerEventID + "'",
		"payload->>'" + AlertFieldAffectedKind + "'",
		"payload->>'" + AlertFieldAffectedID + "'",
		"WHERE event_type = ",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("00111 does not contain %q; the alert's dedupe key and its payload must be the same key", want)
		}
	}
	// The migration must not add a table: the alert is a message, and
	// docs/18 §5's 「只标记 review required」 has no lifecycle to store.
	for _, forbidden := range []string{"CREATE TABLE", "ALTER TABLE"} {
		if strings.Contains(strings.ToUpper(sql), forbidden) {
			t.Errorf("00111 contains %q; this task's migrations may add indexes and constraints only", forbidden)
		}
	}
}

// readRepoFile reads a file from the repository root (the test runs in its
// package directory).
func readRepoFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// goFilesUnder reads every .go file of a directory tree, keyed by its
// repo-relative path.
func goFilesUnder(t *testing.T, parts ...string) map[string]string {
	t.Helper()
	root := filepath.Join(append([]string{"..", "..", ".."}, parts...)...)
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(filepath.Join("..", "..", ".."), path)
		if relErr != nil {
			rel = path
		}
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(out) == 0 {
		t.Fatalf("found no Go files under %s", root)
	}
	return out
}

func concatenate(files map[string]string) string {
	var b strings.Builder
	for _, src := range files {
		b.WriteString(src)
	}
	return b.String()
}
