package evidencehttp

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/lichman0405/post/cmd/api/provenancehttp"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/application/sciobjects"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/evidence"
	"github.com/lichman0405/post/internal/rsg/provenance"
)

// Task T0506, acceptance criterion 3: the evidence read is a SEPARATE read
// path from the provenance graph, proved rather than asserted —
//
//	CLAUDE.md §9 invariant 10: Provenance Graph != Evidence Graph.
//
// Three checks, each able to fail:
//
//   - the two surfaces are mounted on the SAME mux (which panics on a
//     pattern collision) and every evidence URL resolves to the evidence
//     pattern, with the provenance surface's own URLs as the control that
//     the mux is really matching;
//   - a real request through that composed mux is answered by this surface
//     while a live provenance store, wired beside it, is never consulted;
//   - the read path's production code is parsed and scanned — comments
//     excluded by construction — so no identifier, import or string in it
//     names the provenance surface, its prefix or its table.
//
// Naming provenancehttp in THIS file is the point of the second and third
// checks: the test has to hold both surfaces to ask whether they touch, and
// the scan therefore reads production files only (a _test.go file is not
// shipped). The production packages name neither.

// ---- fakes -----------------------------------------------------------------

// fakeService answers the object read with one recognisable assertion and
// records the calls, so a request-level check can tell this surface's answer
// from any other surface's.
type fakeService struct {
	calls int
	// hypothesis reads go through HypothesisEvidence; the object read is the
	// one the request-level check issues.
	out evidence.ObjectEvidence
}

func (f *fakeService) ObjectEvidence(_ context.Context, projectID, objectID string, _ *int) (evidence.ObjectEvidence, error) {
	f.calls++
	out := f.out
	out.ProjectID = projectID
	out.Object.ObjectID = objectID
	return out, nil
}

func (f *fakeService) HypothesisEvidence(_ context.Context, _, _ string) (evidence.HypothesisEvidence, error) {
	f.calls++
	return evidence.HypothesisEvidence{}, nil
}

// fakeGate admits every read: this file is about which SURFACE answers, not
// about the gate (the transport's gate order is pinned in the integration
// test, where there is a real project to be a member of).
type fakeGate struct {
	calls int
}

func (f *fakeGate) Get(_ context.Context, _ projects.Reader, projectID string) (domain.Project, error) {
	f.calls++
	return domain.Project{ID: projectID}, nil
}

// countingProvStore is a LIVE provenance store wired beside this surface: it
// counts every read it is asked for, so "the evidence route never touches
// the provenance graph" is observed rather than inferred from the code.
type countingProvStore struct {
	calls int
}

func (s *countingProvStore) ListEdges(_ context.Context, _ string) ([]provenance.Edge, error) {
	s.calls++
	return nil, nil
}

func (s *countingProvStore) ObjectStart(_ context.Context, _, _ string, _ *int) (provenancehttp.ObjectStart, error) {
	s.calls++
	return provenancehttp.ObjectStart{}, sciobjects.ErrObjectNotFound
}

// ---- the routes ------------------------------------------------------------

const (
	sepProject = "11111111-1111-4111-8111-111111111111"
	sepObject  = "22222222-2222-4222-8222-222222222222"
)

// composedMux mounts both read surfaces on one mux the way cmd/api/main.go
// does. http.ServeMux panics when a pattern collides, so building this mux
// at all is the "the two surfaces do not claim the same route" proof.
func composedMux(t *testing.T, svc Service, gate Gate, prov provenancehttp.Store) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	New(Deps{Service: svc, Gate: gate}).Register(mux)
	provenancehttp.New(provenancehttp.Deps{Store: prov, Gate: gate}).Register(mux)
	return mux
}

// TestEvidenceRoutesResolveToTheEvidenceSurfaceAndNotTheProvenanceOne: both
// surfaces on one mux, and every URL asked of it must resolve to the pattern
// the right surface registered. The provenance surface's own routes are
// asked too — without them, "my URL resolved to my pattern" could be read
// off a mux that matched nothing at all.
func TestEvidenceRoutesResolveToTheEvidenceSurfaceAndNotTheProvenanceOne(t *testing.T) {
	mux := composedMux(t, &fakeService{}, &fakeGate{}, &countingProvStore{})

	cases := []struct {
		url  string
		want string
	}{
		{"/api/v1/projects/" + sepProject + "/objects/" + sepObject + "/evidence",
			"GET /api/v1/projects/{projectId}/objects/{objectId}/evidence"},
		{"/api/v1/projects/" + sepProject + "/hypotheses/" + sepObject + "/evidence",
			"GET /api/v1/projects/{projectId}/hypotheses/{objectId}/evidence"},
		// controls: the neighbouring surface's own routes.
		{"/api/v1/projects/" + sepProject + "/provenance/graph",
			"GET /api/v1/projects/{projectId}/provenance/graph"},
		{"/api/v1/projects/" + sepProject + "/objects/" + sepObject + "/lineage",
			"GET /api/v1/projects/{projectId}/objects/{objectId}/lineage"},
	}
	for _, c := range cases {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, c.url, nil))
		if pattern != c.want {
			t.Errorf("GET %s resolved to %q, want %q", c.url, pattern, c.want)
		}
	}

	// The negative direction: nothing of this read lives under the
	// provenance prefix.
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+sepProject+"/provenance/evidence", nil)); pattern != "" {
		t.Errorf("/provenance/evidence resolved to %q: the evidence read must not sit under the provenance prefix", pattern)
	}

	// The instrument, proved before its silence is trusted: a duplicate
	// pattern must panic. Without this, composedMux building without a
	// panic would prove nothing about the two surfaces' patterns.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("a duplicate pattern did not panic: the mux is not the collision detector this test assumes")
			}
		}()
		collide := http.NewServeMux()
		New(Deps{}).Register(collide)
		New(Deps{}).Register(collide)
	}()
}

// TestAnEvidenceRequestNeverConsultsTheProvenanceStore is the runtime half of
// "the two are separate": both surfaces are wired on one mux with a live
// provenance store behind the other one, and a request to the evidence route
// must be answered by this surface while that store is asked nothing.
func TestAnEvidenceRequestNeverConsultsTheProvenanceStore(t *testing.T) {
	svc := &fakeService{out: evidence.ObjectEvidence{
		Groups: evidence.GroupAll([]evidence.Target{{ObjectVersionID: "vvvvvvvv-1111-4111-8111-111111111111"}},
			[]evidence.Assertion{{
				ID: "aaaaaaaa-3333-4333-8333-333333333333", Relation: "contradicts",
				EvidenceType: "experimental", Scope: json.RawMessage(`{}`),
				TargetObjectVersionID: "vvvvvvvv-1111-4111-8111-111111111111",
			}}),
	}}
	prov := &countingProvStore{}
	gate := &fakeGate{}
	mux := composedMux(t, svc, gate, prov)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/projects/"+sepProject+"/objects/"+sepObject+"/evidence", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET evidence = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if svc.calls != 1 {
		t.Errorf("the evidence service was asked %d times, want 1: the request was answered by another surface", svc.calls)
	}
	if prov.calls != 0 {
		t.Errorf("the provenance store was consulted %d times by an evidence request", prov.calls)
	}
	if gate.calls != 1 {
		t.Errorf("the project gate ran %d times, want once before the read", gate.calls)
	}
	if !strings.Contains(rec.Body.String(), "aaaaaaaa-3333-4333-8333-333333333333") {
		t.Errorf("the answer is not this surface's document: %s", rec.Body.String())
	}
}

// ---- the source scan -------------------------------------------------------

// forbiddenSourceRefs are the names that would mean this read path is coupled
// to the provenance graph: the other surface's package, its projection table
// (infra/migrations/00043), its internal model package, and its URL prefix.
var forbiddenSourceRefs = []string{"provenancehttp", "provenance_edges", "rsg/provenance", "/provenance/"}

func namesProvenance(text string) bool {
	low := strings.ToLower(text)
	for _, bad := range forbiddenSourceRefs {
		if strings.Contains(low, bad) {
			return true
		}
	}
	return false
}

// scanSource reports every place a Go file NAMES one of the forbidden
// references in CODE.
//
// The parse deliberately omits parser.ParseComments: doc comments are not
// part of the AST it builds, so the paragraphs in this package and in
// internal/evidence that explain the separation can neither satisfy the scan
// nor trip it. That distinction is the whole point — the claim is about the
// code, and a scan that read the prose would be measuring the comment.
func scanSource(t *testing.T, filename string, src []byte) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	var found []string
	seen := map[string]bool{}
	report := func(pos token.Pos, what, text string) {
		line := fmt.Sprintf("%s: %s %q", fset.Position(pos), what, text)
		if seen[line] {
			return
		}
		seen[line] = true
		found = append(found, line)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.Ident:
			if namesProvenance(v.Name) {
				report(v.Pos(), "identifier", v.Name)
			}
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(v.Value)
			if err == nil && namesProvenance(text) {
				report(v.Pos(), "string", text)
			}
		}
		return true
	})
	return found
}

// TestTheScanCanSayNo proves the instrument before trusting its silence: a
// planted file that names the provenance surface in code must be caught, and
// the same words in comments must NOT be — otherwise "nothing found" in the
// test below would be indistinguishable from a scan that looks at nothing
// or at everything.
func TestTheScanCanSayNo(t *testing.T) {
	planted := []byte("package p\n\n" +
		"import \"github.com/lichman0405/post/cmd/api/provenancehttp\"\n\n" +
		"const route = \"/api/v1/projects/{projectId}/provenance/graph\"\n\n" +
		"func use() { _ = provenancehttp.Store(nil) }\n")
	if got := scanSource(t, "planted.go", planted); len(got) < 3 {
		t.Fatalf("the scan missed planted code-level references: %v", got)
	}

	commented := []byte("package p\n\n" +
		"// provenancehttp reads provenance_edges; its prefix is /provenance/.\n" +
		"// It lives in internal/rsg/provenance and is NOT this package.\n" +
		"import \"strings\"\n\n" +
		"var _ = strings.TrimSpace\n")
	if got := scanSource(t, "commented.go", commented); len(got) != 0 {
		t.Fatalf("the scan reads comments: %v", got)
	}
}

// readPathSources is the read path's production code: this package, the two
// packages behind it, and the one file this task adds to the shared
// persistence package.
//
// internal/persistence is named by FILE rather than by directory because it
// is not this read's package — it holds every store in the tree — so a scan
// of the whole directory would fail on code that has nothing to do with this
// read. cmd/api/main.go is excluded for the same reason in the other
// direction: it is the composition root and mounts BOTH surfaces, so it
// necessarily names both. What it must not do is give this read a provenance
// adapter, and the request-level check above observes that directly: the
// service the handler calls is this surface's own port, which no provenance
// store satisfies.
func readPathSources(t *testing.T) []string {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	dirs := []string{
		filepath.Join(root, "cmd", "api", "evidencehttp"),
		filepath.Join(root, "internal", "evidence"),
		filepath.Join(root, "internal", "application", "evidencegraph"),
	}
	var files []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			// Production code only: this file names provenancehttp on
			// purpose (see the header), and a test is not shipped.
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			files = append(files, filepath.Join(dir, name))
		}
	}
	files = append(files, filepath.Join(root, "internal", "persistence", "evidence_graph_store.go"))
	if len(files) < 6 {
		t.Fatalf("the read path has %d production files: the directory walk found nothing, so this scan would pass by looking at nothing", len(files))
	}
	return files
}

// TestTheReadPathNamesNoProvenanceCode — the code half of invariant 10.
func TestTheReadPathNamesNoProvenanceCode(t *testing.T) {
	files := readPathSources(t)
	// Control: the walk must include files this task wrote AND one it did
	// not, and each must be non-empty — a zero-byte read scans as clean.
	var sawHandlers bool
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if len(src) == 0 {
			t.Fatalf("%s is empty: a file that did not load scans as separated", path)
		}
		if strings.HasSuffix(path, "evidencehttp/handlers.go") {
			sawHandlers = true
		}
		for _, finding := range scanSource(t, path, src) {
			t.Errorf("the read path names the provenance graph: %s", finding)
		}
	}
	if !sawHandlers {
		t.Errorf("the scan never reached this package's handlers.go")
	}
}
