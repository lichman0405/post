package embedding

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// allowedImports is the EXACT set of packages this package's product code
// may import.
//
// It is the mechanical half of the boundary T0902 was given: the port, the
// in-process embedder and the batch job are the whole provider surface, no
// implementation may reach the network, and there is no configuration that
// could add one. Prose in a comment cannot hold that line — a later task
// adds `net/http` and a real client in four lines — so the line is held here,
// where adding an import is a deliberate act with a failing test attached
// (docs/20 §5: providers go through a port; the port is the only seam, and
// this task's seam is in-process).
//
// A new entry here is not forbidden. It is a decision, and this list is where
// it gets made visible: the nearest legal addition is a fake provider inside
// a test file, which this check does not read.
var allowedImports = map[string]bool{
	// standard library
	"context":         true,
	"crypto/sha256":   true,
	"encoding/binary": true,
	"errors":          true,
	"fmt":             true,
	"log/slog":        true,
	"math":            true,
	"strconv":         true,
	"strings":         true,
	"time":            true,
	"unicode":         true,
	// the repository's own generated query layer and the driver it runs on
	"github.com/jackc/pgx/v5/pgxpool":                       true,
	"github.com/lichman0405/post/internal/persistence/sqlc": true,
}

// outboundCapable are import prefixes that would give this package a way to
// reach something outside the process over a network. They are listed
// separately from the allowlist so that the failure message can say WHY the
// import is refused, not merely that it is new.
var outboundCapable = []string{"net", "net/", "http", "grpc", "openai", "anthropic", "cohere", "huggingface"}

// outboundReason returns the prefix that makes path an outbound-capable
// import, or "" when it is not one. `net` covers `net/http` and the rest of
// the net tree; the vendor names cover a client SDK arriving from outside
// the standard library.
func outboundReason(path string) string {
	for _, forbidden := range outboundCapable {
		if path == forbidden || strings.HasPrefix(path, forbidden) {
			return forbidden
		}
	}
	return ""
}

// TestPackageImportsNothingThatCanCallOut reads this package's own source
// files and checks two things about every non-test file's imports: nothing
// that can open a connection, and nothing outside the allowlist (so a new
// import is reviewed rather than absorbed).
func TestPackageImportsNothingThatCanCallOut(t *testing.T) {
	imports := packageImports(t)
	if len(imports) == 0 {
		t.Fatalf("no imports were found: the check would pass on an empty package, so it is measuring nothing")
	}
	for path := range imports {
		if reason := outboundReason(path); reason != "" {
			t.Errorf("package embedding imports %q (matched %q): it can reach outside the process, and the embedding port has no outbound implementation in this task", path, reason)
		}
		if !allowedImports[path] {
			t.Errorf("package embedding imports %q, which is not in allowedImports — adding an import here is a decision, and this test is where it is recorded", path)
		}
	}
}

// TestAllowedImportListMatchesThePackage is the other direction: the
// allowlist must describe the package as it is, so a REMOVED import cannot
// leave a stale permission behind for a later change to reuse unnoticed.
func TestAllowedImportListMatchesThePackage(t *testing.T) {
	imports := packageImports(t)
	for path := range allowedImports {
		if !imports[path] {
			t.Errorf("allowedImports permits %q, which no source file imports — the list has drifted from the package", path)
		}
	}
}

// packageImports parses every non-test .go file in this directory and returns
// the set of import paths it declares.
func packageImports(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("list the package's files: %v", err)
	}
	imports := map[string]bool{}
	parsed := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue // tests may hold a fake provider; the check is about product code
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if path == "C" {
				continue
			}
			imports[path] = true
		}
	}
	if parsed < 3 {
		t.Fatalf("only %d non-test source file(s) were parsed (doc.go, port.go, deterministic.go, batch.go are expected): the check is not reading the package", parsed)
	}
	return imports
}

// TestImportCheckCanFail is the instrument's own control: it proves that the
// parser sees imports at all, by parsing a source string with a known
// outbound import and finding it. Without this, TestPackageImportsNothing-
// ThatCanCallOut could be green because it read nothing.
func TestImportCheckCanFail(t *testing.T) {
	const src = `package probe

import (
	"net/http"
	"context"
)

var _ = http.DefaultClient
var _ = context.Background
`
	file, err := parser.ParseFile(token.NewFileSet(), "probe.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse the probe source: %v", err)
	}
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		if spec, ok := n.(*ast.ImportSpec); ok {
			found = append(found, strings.Trim(spec.Path.Value, `"`))
		}
		return true
	})
	sort.Strings(found)
	if len(found) != 2 || found[0] != "context" || found[1] != "net/http" {
		t.Fatalf("the import reader returned %v, so it cannot be trusted to see a real import", found)
	}
	var flagged []string
	for _, path := range found {
		if outboundReason(path) != "" {
			flagged = append(flagged, path)
		}
	}
	if len(flagged) != 1 || flagged[0] != "net/http" {
		t.Fatalf("the outbound list flagged %v out of %v, so it would not refuse a real outbound import", flagged, found)
	}
}
