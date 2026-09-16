package devorchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repo's own schema is the validation truth source (no embedded copy to
// drift); tests resolve it relative to the package dir.
func taskPackageSchemaPath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "specs", "orchestrator", "task-package.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func testTaskSpec() *TaskSpec {
	return &TaskSpec{
		ID: "T0001", Phase: "P0", PhaseName: "phase", Title: "Source repo preflight",
		Requirements:       []string{"r1"},
		AcceptanceCriteria: []string{"a1"},
		Tests:              []string{"t1"},
		AllowedScope:       []string{"scripts/**"},
		ForbiddenScope:     []string{"product business code"},
		RelevantSpecs:      []string{"specs/orchestrator/source-repository.yaml"},
		DecisionLevelMax:   "L1",
	}
}

// TestRenderTaskPackageValidatesAgainstRepoSchema: a rendered package passes
// the schema the repo carries.
func TestRenderTaskPackageValidatesAgainstRepoSchema(t *testing.T) {
	budget := 1.5
	turns := 30
	pkg, err := RenderTaskPackage(testTaskSpec(), "0123456789abcdef", &turns, &budget)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTaskPackage(pkg, taskPackageSchemaPath(t)); err != nil {
		t.Fatalf("rendered package failed validation: %v", err)
	}
	// optional numeric fields round-trip through the JSON encoding the
	// validator sees: integer turns and (whole) float budget both validate
	wholeBudget := 2.0
	pkg2, err := RenderTaskPackage(testTaskSpec(), "0123456789abcdef", nil, &wholeBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTaskPackage(pkg2, taskPackageSchemaPath(t)); err != nil {
		t.Fatalf("whole-valued budget failed validation (integer/number confusion): %v", err)
	}
}

// TestRenderTaskPackageRefusesEmptyScope: a task without allowed_scope would
// produce a package that can never be collected — spawn must refuse it.
func TestRenderTaskPackageRefusesEmptyScope(t *testing.T) {
	spec := testTaskSpec()
	spec.AllowedScope = nil
	if _, err := RenderTaskPackage(spec, "0123456789abcdef", nil, nil); err == nil {
		t.Fatal("empty allowed_scope rendered a package")
	}
}

// TestValidateTaskPackageRejectsTampering: an unknown field must be rejected
// (additionalProperties: false) — not silently carried.
func TestValidateTaskPackageRejectsTampering(t *testing.T) {
	pkg, err := RenderTaskPackage(testTaskSpec(), "0123456789abcdef", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["sneaky_extra"] = "x"
	if err := validateAgainst(doc, loadSchema(t), "task-package"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field accepted: %v", err)
	}
}

// TestValidateTaskPackageRejectsBadValues: bad pattern and missing required.
func TestValidateTaskPackageRejectsBadValues(t *testing.T) {
	schema := loadSchema(t)
	// a doc with every required field, only the task_id shape broken
	valid := map[string]any{
		"task_id": "bad", "goal": "g", "baseline_sha": "0123456789abcdef",
		"allowed_scope": []any{"x"}, "forbidden_scope": []any{},
		"requirements": []any{}, "acceptance_criteria": []any{},
		"required_tests": []any{}, "relevant_specs": []any{},
		"decision_level_max": "L1",
	}
	if err := validateAgainst(valid, schema, "pkg"); err == nil || !strings.Contains(err.Error(), "pattern") {
		t.Errorf("bad task_id pattern accepted: %v", err)
	}
	if err := validateAgainst(map[string]any{}, schema, "pkg"); err == nil || !strings.Contains(err.Error(), "missing required") {
		t.Errorf("missing required fields accepted: %v", err)
	}
}

// TestValidatorFailsClosedOnUnknownKeywords: a schema using a keyword this
// validator does not implement must be refused, not skipped.
func TestValidatorFailsClosedOnUnknownKeywords(t *testing.T) {
	err := validateAgainst(map[string]any{"x": 1}, map[string]any{"maxLength": 3, "type": "object"}, "x")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unsupported keyword silently skipped: %v", err)
	}
}

// TestValidatorRefusesABoundItsTypeCanNeverCarry: bounding only the types a
// bound applies to must not become not bounding at all. A schema that sets a
// bound where it can never fire is an authoring error, and validating past it
// would be validating less than the schema demands.
func TestValidatorRefusesABoundItsTypeCanNeverCarry(t *testing.T) {
	cases := []struct {
		name   string
		doc    any
		schema map[string]any
	}{
		{"bound next to an incompatible type", "x", map[string]any{"type": "string", "minimum": 0}},
		{"bound with no type declared", "x", map[string]any{"minimum": 0}},
		// the value passes the type check (integer) and still cannot carry a
		// string-length bound
		{"bound no type in the list mentions", float64(5), map[string]any{"type": []any{"integer", "null"}, "minLength": 1}},
	}
	for _, tc := range cases {
		err := validateAgainst(tc.doc, tc.schema, "x")
		if err == nil || !strings.Contains(err.Error(), "can never carry") {
			t.Errorf("%s: a bound that can never fire was silently dropped: %v", tc.name, err)
		}
	}
}

// TestValidatorAppliesAKeywordOnlyToItsOwnType pins the defect that stopped
// T0215's review from collecting on 14 Sep 2026.
//
// review-verdict.schema.json declares a finding's line as
// {"type": ["integer","null"], "minimum": 0} — null is explicitly admissible,
// which is how a Reviewer says "this finding is not line-specific". The
// reviewer wrote exactly that, and collect refused the approve verdict with
// `RESULT.json.findings[0].line: <nil> is below minimum 0`.
//
// In draft 2020-12 `minimum` constrains numbers only; it says nothing about a
// null, and the type keyword is what decides whether a null may appear here.
// The document was accepted by the validator claude applied to the Reviewer's
// own final message and refused by this one, which is what made it a defect
// rather than a disagreement about style: the Worker was told to fix something
// that was not wrong.
//
// Each case is (what, schema, value) where the value's type is admitted by the
// schema's own type list and only the sibling keyword used to reject it.
func TestValidatorAppliesAKeywordOnlyToItsOwnType(t *testing.T) {
	cases := []struct {
		what   string
		schema map[string]any
		value  any
	}{
		{"null against integer|null with minimum", map[string]any{"type": []any{"integer", "null"}, "minimum": float64(0)}, nil},
		{"null against string|null with minLength", map[string]any{"type": []any{"string", "null"}, "minLength": float64(1)}, nil},
		{"null against string|null with a pattern", map[string]any{"type": []any{"string", "null"}, "pattern": "^a"}, nil},
		{"null against array|null with minItems", map[string]any{"type": []any{"array", "null"}, "minItems": float64(1)}, nil},
	}
	for _, tc := range cases {
		if err := validateAgainst(tc.value, tc.schema, "x"); err != nil {
			t.Errorf("%s: a value the schema's own type admits was rejected: %v", tc.what, err)
		}
	}

	// The same keywords must still fire on values of the type they govern, and
	// a type the schema does not admit must still be rejected — this is a
	// correction of the keyword's scope, not a loosening of validation.
	rejects := []struct {
		what   string
		schema map[string]any
		value  any
	}{
		{"a number below minimum", map[string]any{"type": []any{"integer", "null"}, "minimum": float64(0)}, float64(-1)},
		{"a string below minLength", map[string]any{"type": []any{"string", "null"}, "minLength": float64(2)}, "a"},
		{"a string not matching the pattern", map[string]any{"type": []any{"string", "null"}, "pattern": "^a"}, "b"},
		{"an array below minItems", map[string]any{"type": []any{"array", "null"}, "minItems": float64(1)}, []any{}},
		{"a type the schema does not admit", map[string]any{"type": []any{"integer", "null"}, "minimum": float64(0)}, "s"},
	}
	for _, tc := range rejects {
		if err := validateAgainst(tc.value, tc.schema, "x"); err == nil {
			t.Errorf("%s: accepted (validation weakened): %v", tc.what, tc.value)
		}
	}
}

// TestValidateReviewVerdictAcceptsAFindingWithoutALine runs the real schema
// against the real document shape, so the fix cannot pass against a
// hand-written schema that has drifted from the repo's.
func TestValidateReviewVerdictAcceptsAFindingWithoutALine(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "specs", "orchestrator", "review-verdict.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	// The T0215 shape: an approve verdict whose only finding has a file but no
	// line, because the finding is about a document rather than a source line.
	doc := map[string]any{
		"task_id": "T0215", "verdict": "approve", "summary": "s",
		"findings": []any{map[string]any{
			"severity": "nit", "file": "RESULT.json", "line": nil,
			"finding": "the quoted line numbers moved after a cosmetic edit",
		}},
		"risks": []any{},
	}
	if err := validateAgainst(doc, schema, "RESULT.json"); err != nil {
		t.Fatalf("a verdict with an unanchored finding was refused: %v", err)
	}
	// A negative line is still nonsense and must still be refused.
	doc["findings"] = []any{map[string]any{
		"severity": "nit", "file": "RESULT.json", "line": float64(-1), "finding": "x",
	}}
	if err := validateAgainst(doc, schema, "RESULT.json"); err == nil {
		t.Error("a negative line was accepted")
	}
}

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	return loadSchemaAt(t, taskPackageSchemaPath(t))
}

func loadSchemaAt(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestRenderPromptContainsContract: the prompt names the worktree, the
// RESULT path, the baseline, the budget and the early-write rule — the four
// facts a Worker must never have to guess.
func TestRenderPromptContainsContract(t *testing.T) {
	pkg, err := RenderTaskPackage(testTaskSpec(), "0123456789abcdef", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := RenderPrompt(pkg, "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees", "/repo/.rddev/workers")
	for _, want := range []string{
		"Task T0001", "Source repo preflight",
		"/repo/.rddev/worktrees/T0001",
		"/repo/.rddev/workers/T0001/RESULT.json",
		"scripts/**", "0123456789abcdef",
		"Write your RESULT.json EARLY",
		"submitted for acceptance",
		"trying to make things fail",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// TestRenderSystemPromptContainsContract: the hard rules and paths.
func TestRenderSystemPromptContainsContract(t *testing.T) {
	s := RenderSystemPrompt("T0001", "/repo", "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees")
	for _, want := range []string{
		"No Git control-plane", "allowed_scope", "Never weaken",
		"/repo/.rddev/workers/T0001/RESULT.json",
		"EARLY", "not accepted",
		// The two recording conventions a RESULT gets rejected for getting
		// wrong, and that nothing else in the package states: a deliberate
		// break-and-revert is a `passed` entry (T0709 lost an attempt to
		// recording one as `failed`, which contradicts a completed status),
		// and a pre-existing failure is `blocked`, never hidden or worked
		// around (T0601 recorded one honestly as `failed` and was rejected
		// for the contradiction rather than for the report).
		"MUTATION CHECK", "predates your change", "blocked",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}
