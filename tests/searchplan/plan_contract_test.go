// Task T0903 — required test "planner contract".
//
// This file is the golden half of the planner contract (docs/24 §6: "Search
// query plan" uses golden fixtures, and updating one needs an explicit
// review). Each fixture is a complete case — the question, what the provider
// answered (a document, an error, or silence), and the exact plan that comes
// out — so the diff a reviewer sees is the change itself, not a summary of it.
//
// Why the provider half is scripted: the planning MODEL is not wired in this
// round (no document in the repository names a vendor or a model, and letting
// a question leave the platform is a decision no spec settles), so the
// fixtures drive the deterministic fake. What that pins is everything from
// the provider's bytes onward, which is where this task's contract lives:
// schema validation, the identifier guard, the fallback exits, the typed
// document and its canonical rendering.

package searchplan

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
	"github.com/lichman0405/post/internal/search/planner"
	"github.com/lichman0405/post/internal/search/planner/plannertest"
)

// update rewrites the expected_plan half of every fixture.
var update = flag.Bool("update", false, "regenerate the fixtures' expected_plan bytes")

// fixture is one golden case. The documents are stored as raw JSON (not as
// strings) so a fixture file reads as the case it is, and they are compared
// byte-for-byte modulo insignificant whitespace, so key order, field names,
// enum values and string escaping are all pinned.
type fixture struct {
	Name string `json:"name"`
	// Note says what the case pins. A golden nobody can explain is a golden
	// nobody can review.
	Note string `json:"note"`
	// Query is the question put to the planner; it selects the script entry
	// the fake answers with.
	Query string `json:"query"`
	// ProviderDocument is what the provider answers with, verbatim. It is
	// deliberately NOT validated here: refusing a bad document is the
	// planner's job, and a fixture that pre-validated would make the
	// fallback cases tests of the fixture.
	ProviderDocument json.RawMessage `json:"provider_document,omitempty"`
	// ProviderError, when set, is what the provider fails with instead of
	// answering.
	ProviderError string `json:"provider_error,omitempty"`
	// ProviderDelayMS makes the provider answer late; with TimeoutMS below,
	// that is the hanging provider.
	ProviderDelayMS int `json:"provider_delay_ms,omitempty"`
	// TimeoutMS overrides the planner's budget for this case.
	TimeoutMS int `json:"timeout_ms,omitempty"`
	// ExpectedPlan is the plan's canonical rendering.
	ExpectedPlan json.RawMessage `json:"expected_plan"`

	// path is where the fixture was read from; used to keep -update honest.
	path string
}

const (
	fixtureDir = "testdata"

	// fixtureUser and fixtureProject are the actor the plan is resolved for.
	// They are in EVERY fixture on purpose: the planner refuses to plan
	// without an actor, so a golden case that could be replayed without one
	// would be pinning a state the production path never reaches.
	fixtureUser    = "9f0c1b2a-3d4e-4f50-8a91-b2c3d4e5f607"
	fixtureProject = "1a2b3c4d-5e6f-4708-9a1b-2c3d4e5f6071"
)

// TestGoldenSearchPlans runs every fixture through the real planner and
// compares the exact result.
func TestGoldenSearchPlans(t *testing.T) {
	fixtures := loadFixtures(t)
	if len(fixtures) == 0 {
		t.Fatal("no fixtures found: a golden test with no goldens passes without testing anything")
	}
	var planned int
	reasons := map[planner.Reason]int{}

	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			got := runFixture(t, f)
			canonical, err := got.CanonicalJSON()
			if err != nil {
				t.Fatalf("CanonicalJSON: %v", err)
			}
			if !bytes.HasSuffix(canonical, []byte("\n")) {
				t.Errorf("the canonical rendering does not end in a newline: %q", canonical[len(canonical)-1:])
			}
			if *update {
				f.ExpectedPlan = canonical
				writeFixture(t, f)
				return
			}

			want := compact(t, "expected_plan", f.ExpectedPlan)
			have := compact(t, "the plan the planner produced", canonical)
			if !bytes.Equal(want, have) {
				t.Errorf("the plan does not match the golden.\n--- want ---\n%s\n--- got ---\n%s",
					indent(want), indent(have))
			}
			switch got.Status {
			case planner.StatusPlanned:
				planned++
			case planner.StatusFallback:
				reasons[got.Reason]++
			default:
				t.Errorf("plan status = %q, want planned or fallback", got.Status)
			}
			if got.Query != f.Query {
				t.Errorf("the plan carries query %q, want the question asked (%q)", got.Query, f.Query)
			}
		})
	}

	if *update {
		return
	}
	// The golden set has to pin BOTH exits and every named cause: a fallback
	// whose reason nobody pins is a fallback a caller cannot count (docs/26
	// §3 lists "LLM planner failures" among the metrics), and a set with no
	// accepted plan would pin only the degraded path.
	if planned == 0 {
		t.Error("no fixture produces an accepted plan: the happy path is not pinned")
	}
	for _, reason := range []planner.Reason{
		planner.ReasonProviderError, planner.ReasonTimeout,
		planner.ReasonInvalidPlan, planner.ReasonIdentifier,
	} {
		if reasons[reason] == 0 {
			t.Errorf("no fixture pins the fallback reason %q: that exit is untested here", reason)
		}
	}
}

// TestGoldenFixturesAreWellFormed keeps the golden set honest independently of
// the planner: every fixture must parse, name itself, ask a question, say
// where its answer came from, and carry a plan that reads as one of the two
// statuses. Without this, a fixture could be "passing" because it is empty.
func TestGoldenFixturesAreWellFormed(t *testing.T) {
	for _, f := range loadFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			if f.Name == "" || f.Note == "" || f.Query == "" {
				t.Fatalf("fixture is incomplete: %+v", f)
			}
			// -update writes to <name>.json, so a fixture whose name and file
			// disagree would silently duplicate itself on the next update.
			if base := strings.TrimSuffix(filepath.Base(f.path), ".json"); base != f.Name {
				t.Errorf("fixture at %s calls itself %q: -update writes <name>.json", f.path, f.Name)
			}
			// A case must say where the answer came from — a document, an
			// error, or a delay the budget runs out on — and an error and a
			// document together would be a case with two answers.
			answered := len(f.ProviderDocument) > 0
			failed := f.ProviderError != ""
			if answered && failed {
				t.Error("the provider both answers and fails")
			}
			if !answered && !failed && f.ProviderDelayMS == 0 {
				t.Error("the fixture does not say what the provider did")
			}
			if f.ProviderDelayMS > 0 && f.TimeoutMS == 0 {
				t.Error("a delayed answer with no budget pins nothing about the timeout exit")
			}
			if len(f.ExpectedPlan) == 0 {
				t.Fatal("the fixture has no expected_plan: run go test ./tests/searchplan -run TestGolden -update")
			}

			var plan struct {
				Status   string          `json:"status"`
				Reason   string          `json:"reason"`
				Query    string          `json:"query"`
				Document json.RawMessage `json:"document"`
			}
			if err := json.Unmarshal(f.ExpectedPlan, &plan); err != nil {
				t.Fatalf("expected_plan is not valid JSON: %v", err)
			}
			if plan.Query != f.Query {
				t.Errorf("expected_plan carries query %q, fixture asks %q", plan.Query, f.Query)
			}
			switch plan.Status {
			case string(planner.StatusPlanned):
				if len(plan.Document) == 0 {
					t.Error("a planned fixture carries no document")
				}
				if plan.Reason != "" {
					t.Errorf("a planned fixture carries reason %q", plan.Reason)
				}
			case string(planner.StatusFallback):
				if plan.Reason == "" {
					t.Error("a fallback fixture names no reason")
				}
				if len(plan.Document) != 0 {
					t.Errorf("a fallback fixture carries a document: %s", plan.Document)
				}
			default:
				t.Errorf("status = %q, want planned or fallback", plan.Status)
			}
		})
	}
}

// TestGoldenFixturesPinTheRendering is the half of "canonical rendering" a
// whitespace-insensitive comparison cannot see.
//
// Go's default HTML escaping would rewrite a question about "CO2 -> CH4" into
// "CO2 -> CH4" — the plan would still be correct and would still round
// trip, so nothing else here would notice, but the plan is stored and shown.
// The check is written so -update cannot launder it: regenerating the fixture
// from an escaping implementation writes the escaped form, and the escape is
// what this test looks for, so the golden cannot be made to agree.
func TestGoldenFixturesPinTheRendering(t *testing.T) {
	fixtures := loadFixtures(t)
	questions := 0
	for _, f := range fixtures {
		if strings.ContainsAny(f.Query, "<>") {
			questions++
		}
		// The escapes are built rather than written out, so what this looks
		// for is unambiguous on the page: the six characters `\` `u` `0` `0`
		// `3` `c`, which is what an HTML-escaping encoder would emit for '<'.
		for _, r := range []rune{'<', '>', '&'} {
			escaped := fmt.Sprintf(`\u%04x`, r)
			if bytes.Contains(f.ExpectedPlan, []byte(escaped)) {
				t.Errorf("fixture %s renders %s instead of %q: the plan is stored and shown, so it must not be HTML-escaped",
					f.Name, escaped, r)
			}
		}
	}
	if questions == 0 {
		t.Fatal("no fixture asks a question containing '<' or '>': the escaping check above is vacuous")
	}
}

// TestGoldenFixturesReplayOffline: the fixtures are the proof that the port
// admits a deterministic implementation with no network and no credential.
//
// The check is deliberately blunt: it fails if a fixture ever records a
// provider answer that names a host, a URL or a key-shaped string, which is
// what a future "let us just call one real endpoint" change would look like.
func TestGoldenFixturesReplayOffline(t *testing.T) {
	for _, f := range loadFixtures(t) {
		joined := string(f.ProviderDocument) + string(f.ExpectedPlan)
		for _, forbidden := range []string{"http://", "https://", "api_key", "apiKey", "Bearer ", "sk-"} {
			if strings.Contains(joined, forbidden) {
				t.Errorf("fixture %s mentions %q: the planning path under test is offline and credential-free", f.Name, forbidden)
			}
		}
	}
}

// TestGoldenScopeIsTheOnlyWayIn: the fixtures resolve their actor through the
// same ResolveScope the production path uses. The two states that read alike
// and must not be confused are pinned here, because everything above assumes
// the fixtures are planning under a real actor:
//
//   - no actor at all is an error (owner ruling: anonymous search requires a
//     login), while
//   - an actor who belongs to nothing is a real, empty scope — the public-only
//     floor, not a refusal.
func TestGoldenScopeIsTheOnlyWayIn(t *testing.T) {
	if _, err := search.ResolveScope(context.Background(), plannertest.ScopeReader{}, ""); !errors.Is(err, search.ErrNoActor) {
		t.Fatalf("ResolveScope with no actor: err = %v, want ErrNoActor", err)
	}
	bare, err := search.ResolveScope(context.Background(), plannertest.ScopeReader{}, fixtureUser)
	if err != nil {
		t.Fatalf("ResolveScope for a user in no project: %v", err)
	}
	if !bare.Authenticated() || len(bare.AllowedProjectIDs()) != 0 {
		t.Errorf("a user in no project resolved to %+v, want an authenticated scope with no projects", bare)
	}

	scope := fixtureScope(t)
	if !scope.Authenticated() || scope.ActorID() != fixtureUser {
		t.Fatalf("fixture scope = %+v, want the fixture actor", scope)
	}
	if want := []string{fixtureProject}; !reflect.DeepEqual(scope.AllowedProjectIDs(), want) {
		t.Errorf("fixture scope projects = %v, want %v", scope.AllowedProjectIDs(), want)
	}
}

// runFixture drives one fixture through the real planner.
func runFixture(t *testing.T, f fixture) planner.Plan {
	t.Helper()
	entry := plannertest.Entry{Match: f.Query}
	if len(f.ProviderDocument) > 0 {
		entry.Document = string(f.ProviderDocument)
	}
	if f.ProviderError != "" {
		entry.Err = errors.New(f.ProviderError)
	}
	if f.ProviderDelayMS > 0 {
		entry.Delay = time.Duration(f.ProviderDelayMS) * time.Millisecond
	}
	prov := plannertest.New(entry)

	deps := planner.Deps{Provider: prov}
	if f.TimeoutMS > 0 {
		deps.Timeout = time.Duration(f.TimeoutMS) * time.Millisecond
	}
	p, err := planner.New(deps)
	if err != nil {
		t.Fatalf("planner.New: %v", err)
	}
	got, err := p.Plan(context.Background(), fixtureScope(t), planner.Request{Query: f.Query})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if prov.Calls() != 1 {
		t.Fatalf("the provider was called %d times, want 1: a retry is a second chance for a hallucinated plan", prov.Calls())
	}
	return got
}

// fixtureScope resolves the actor's scope the only way one can be built.
func fixtureScope(t *testing.T) search.Scope {
	t.Helper()
	reader := plannertest.ScopeReader{ByUser: map[string][]domain.Project{
		fixtureUser: {{ID: fixtureProject}},
	}}
	scope, err := search.ResolveScope(context.Background(), reader, fixtureUser)
	if err != nil {
		t.Fatalf("ResolveScope: %v", err)
	}
	return scope
}

// loadFixtures reads every fixture in testdata/, in file-name order.
func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureDir, "*.json"))
	if err != nil {
		t.Fatalf("glob fixtures: %v", err)
	}
	sortStrings(paths)
	out := make([]fixture, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var f fixture
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		f.path = path
		if f.Name == "" {
			f.Name = strings.TrimSuffix(filepath.Base(path), ".json")
		}
		out = append(out, f)
	}
	return out
}

// writeFixture rewrites one fixture file with its regenerated golden. HTML
// escaping is off for the same reason CanonicalJSON turns it off: the fixture
// records the plan's bytes, and a question containing '<' must not come back
// from -update rewritten.
func writeFixture(t *testing.T, f fixture) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(f); err != nil {
		t.Fatalf("marshal fixture %s: %v", f.Name, err)
	}
	path := filepath.Join(fixtureDir, f.Name+".json")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	t.Logf("updated %s", path)
}

// compact strips insignificant whitespace, so two renderings of the same
// document compare equal while field order, names, values and escaping stay
// significant.
func compact(t *testing.T, what string, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("%s is not valid JSON: %v", what, err)
	}
	return buf.Bytes()
}

// indent renders bytes for a failure message; a diff is unreadable without it.
func indent(raw []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// sortStrings is sort.Strings without the import, kept local so this file's
// imports stay the ones a reviewer checks (no dependency beyond the planner).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
