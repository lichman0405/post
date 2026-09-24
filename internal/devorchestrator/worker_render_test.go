package devorchestrator

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// roundTripRulings marshals a package the way it is written to disk and decodes
// the field back, so the assertion measures what the Worker will actually read
// rather than what the struct held in memory.
func roundTripRulings(t *testing.T, pkg *TaskPackage) string {
	t.Helper()
	encoded, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	got, _ := back["supervisor_scope_narrowing"].(string)
	return got
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

// TestSupervisorRulingsReachTheWorker: the DAG's supervisor_scope_narrowing is
// prose addressed to the Worker, and it decides what the requirements leave
// open — so it must reach both the package and the prompt. It reaching neither
// is #274: T1003 shipped the opposite of 决定三, T1004 a link to a route that
// does not exist, T1005 an unignored mail sink, all three following a ruling
// no Worker had been shown.
func TestSupervisorRulingsReachTheWorker(t *testing.T) {
	const ruling = "RULE-MARKER: where the requirement reads two ways, this decides which."
	spec := testTaskSpec()
	spec.SupervisorScopeNarrowing = ruling
	pkg, err := RenderTaskPackage(spec, "0123456789abcdef", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The package is the authoritative record and is what gate-inputs pins;
	// a field that only lands in the prompt is not collected anywhere. The
	// assertion decodes the marshalled JSON rather than searching it: Go
	// escapes <, > and & on the way out, and a substring search for rulings
	// that mention `<token>` would report a loss that is not there.
	if got := roundTripRulings(t, pkg); got != ruling {
		t.Errorf("task-package.json dropped the ruling (got %q)", got)
	}
	if err := ValidateTaskPackage(pkg, taskPackageSchemaPath(t)); err != nil {
		t.Fatalf("a package carrying rulings failed the repo schema: %v", err)
	}
	prompt := RenderPrompt(pkg, "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees", "/repo/.rddev/workers")
	if !strings.Contains(prompt, ruling) {
		t.Error("prompt.md dropped the ruling — the Worker would never see it")
	}
	// Before the requirements: the ruling decides how they are read, so a
	// Worker that meets it afterwards has already chosen a reading.
	if ri, pi := strings.Index(prompt, ruling), strings.Index(prompt, "## Requirements"); ri > pi {
		t.Errorf("the ruling is rendered after the requirements (ruling@%d, requirements@%d)", ri, pi)
	}

	// A task without rulings renders exactly as it did before the field
	// existed — no empty section, no null key.
	bare, err := RenderTaskPackage(testTaskSpec(), "0123456789abcdef", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bareJSON, err := json.Marshal(bare)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bareJSON), "supervisor_scope_narrowing") {
		t.Errorf("a task with no rulings still emitted the key: %s", bareJSON)
	}
	barePrompt := RenderPrompt(bare, "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees", "/repo/.rddev/workers")
	if strings.Contains(barePrompt, "Supervisor rulings for this task") {
		t.Error("a task with no rulings rendered an empty rulings section")
	}
}

// TestNoTaskInTheLedgerLosesItsRulings is #274 stated as a property of the
// repository rather than of one synthetic spec: it walks the real tasks.json and
// requires that every task carrying supervisor_scope_narrowing carries it into
// the rendered package. On 18 Sep 2026 eight tasks — the whole serial migration
// chain — held rulings in the ledger and none of them reached its Worker, which
// is how T1003 shipped the opposite of 决定三.
//
// It tests the real file because the synthetic test above cannot catch the
// actual failure mode: the field was dropped by json.Unmarshal into a TaskSpec
// that had no such field, so the loss happened between the file and the code,
// not inside the renderer.
func TestNoTaskInTheLedgerLosesItsRulings(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "tasks", "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	dag, err := LoadDAG(path)
	if err != nil {
		t.Fatal(err)
	}
	carried := 0
	for _, spec := range dag.Tasks {
		if strings.TrimSpace(spec.SupervisorScopeNarrowing) == "" {
			continue
		}
		carried++
		pkg, err := RenderTaskPackage(&spec, "0123456789abcdef", nil, nil)
		if err != nil {
			t.Fatalf("%s carries rulings but would not render: %v", spec.ID, err)
		}
		if got := roundTripRulings(t, pkg); got != spec.SupervisorScopeNarrowing {
			t.Errorf("%s: the ledger's rulings did not survive into the task package (%d chars in, %d out)", spec.ID, len(spec.SupervisorScopeNarrowing), len(got))
		}
		prompt := RenderPrompt(pkg, "/repo/.rddev/worktrees/"+spec.ID, "/repo/.rddev/workers/"+spec.ID, "/repo/.rddev/worktrees", "/repo/.rddev/workers")
		if !strings.Contains(prompt, spec.SupervisorScopeNarrowing) {
			t.Errorf("%s: the ledger's rulings did not survive into the prompt", spec.ID)
		}
	}
	// A test that would pass on an empty ledger measures nothing; the ledger in
	// this repo carries rulings, so if none were found the walk is broken.
	if carried == 0 {
		t.Error("no task in tasks.json carries supervisor_scope_narrowing — either the field is being dropped on load or the ledger query is wrong")
	}
	t.Logf("rulings carried to the Worker for %d task(s)", carried)
}

// TestRenderSystemPromptContainsContract: the hard rules and paths.
func TestRenderSystemPromptContainsContract(t *testing.T) {
	s := RenderSystemPrompt("T0001", "/repo", "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees")
	for _, want := range []string{
		"No Git control-plane", "allowed_scope", "Never weaken",
		"/repo/.rddev/workers/T0001/RESULT.json",
		"EARLY", "not accepted",
		// The FOUR recording conventions a RESULT gets rejected for getting
		// wrong, and that nothing else in the package states: a deliberate
		// break-and-revert is a `passed` entry (T0709 lost an attempt to
		// recording one as `failed`, which contradicts a completed status);
		// a pre-existing failure is `blocked`, never hidden or worked
		// around (T0601 recorded one honestly as `failed` and was rejected
		// for the contradiction rather than for the report); an unexecuted
		// command is not a `tests[]` entry at all under `completed` (T0707
		// and T0806 were each sent back for listing one, both times with the
		// reasoning right and the field wrong — the rulebook said "anything
		// not executed is not_run with a reason", which the consistency check
		// refuses outright); and `acceptance[]` follows the SAME rule the
		// schema does not state — `not_applicable` is in the enum and is
		// still refused under `completed` (T1216 was rejected whole for it,
		// having followed the schema).
		// The third one is asserted by its distinctive phrases, not by the
		// token `not_run`: that word appears three times in the rendered
		// prompt, so a Contains on it stays green while the rule is deleted
		// (measured — the first version of this assertion survived exactly
		// that mutation).
		// The count word is not asserted here:
		// TestTheConventionsListStatesHowManyConventionsItHas owns it,
		// together with the fourth convention's phrases, and a Contains list
		// is exactly what that mutation (delete the bullet, fix the word)
		// slips past.
		"MUTATION CHECK", "predates your change", "blocked",
		"lists the commands you RAN", "has no place under", "notes_for_supervisor",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
}

// TestTheConventionsListStatesHowManyConventionsItHas: the recording
// conventions are a numbered claim, so the number is part of the contract. A
// heading that says "Three" above four bullets is the shape this test exists
// for: the Worker reads the count, and a list that grew without its heading
// is how "there are four rules here" stops being true — the fourth rule then
// reads as an aside to the third, which is exactly the reading that cost
// T1216 an attempt (a `not_applicable` acceptance entry, legal in the schema,
// refused by a collection rule no line of this text stated).
//
// The count is checked against the BULLETS, not against a hardcoded word, so
// it holds whichever way the order or the wording changes, and it is checked
// together with the fourth convention's phrases: a test that only counted
// would pass on a list that lost the rule and kept the arithmetic.
func TestTheConventionsListStatesHowManyConventionsItHas(t *testing.T) {
	s := RenderSystemPrompt("T0001", "/repo", "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees")
	word, heading, block, bullets := conventionsList(t, s)

	numbers := map[string]int{"one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6}
	want, ok := numbers[strings.ToLower(word)]
	if !ok {
		t.Fatalf("the conventions heading starts with %q, which is not a count word:\n%s", word, heading)
	}
	if len(bullets) != want {
		t.Errorf("the heading says %q (%d) and the list carries %d convention(s): the number and the rules it numbers have drifted apart\nheading: %s\nbullets: %v",
			word, want, len(bullets), heading, bullets)
	}

	// The fourth convention, asserted by phrases that appear in the rendered
	// system prompt NOWHERE else (it is built from no task package, so these
	// strings come only from this list): delete the rule and every one of them
	// disappears. `not_applicable` is the one that matters — the collection
	// rule is narrower than the schema's enum, and that gap is what the Worker
	// could not read anywhere in its package.
	for _, want := range []string{
		"`not_applicable`",
		"`acceptance[]`",
		"narrower than the schema",
		"rejects the whole run",
		"`notes_for_supervisor`",
		"belong to a `blocked` or `failed` report",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the conventions list does not state the acceptance[] completion rule (%q missing): a Worker that follows the schema and writes `not_applicable` under `completed` is rejected with nothing in its contract having said so\nlist:\n%s", want, block)
		}
	}

	// ORDER, because the prompt refers to the conventions by position: the
	// paragraph above the list says "the third recording convention below
	// decides where a command you could NOT run belongs". Inserting the new
	// rule anywhere but last would silently repoint that reference at the
	// acceptance[] rule — a sentence in the Worker's contract that sends it to
	// the wrong paragraph. (The task package says the fourth convention must be
	// parallel to the tests[] one, not that it must be fourth; the position is
	// what the surrounding text already assumed, and pinning it here is what
	// keeps the reference honest.)
	if len(bullets) >= 3 && !strings.Contains(bullets[2], "`tests[]` lists the commands you RAN") {
		t.Errorf("the third convention is %q, but the prompt above the list calls the third one the rule that says where an unrun command belongs — the cross-reference no longer points at the tests[] rule\nbullets: %v", bullets[2], bullets)
	}
	if len(bullets) >= 4 && !strings.Contains(bullets[3], "`not_applicable` in `acceptance[]`") {
		t.Errorf("the fourth convention is %q, want the acceptance[] completion rule\nbullets: %v", bullets[3], bullets)
	}
}

// conventionsList returns the count word of the "recording conventions"
// heading, the heading itself, every line of the list that follows it, and the
// top-level bullets among those lines.
//
// Parsing the rendered text rather than a constant is the point: the assertion
// has to be about what the Worker receives.
func conventionsList(t *testing.T, prompt string) (word, heading, block string, bullets []string) {
	t.Helper()
	lines := strings.Split(prompt, "\n")
	start := -1
	for i, ln := range lines {
		if strings.Contains(ln, "recording conventions") {
			start, heading = i, ln
			break
		}
	}
	if start < 0 {
		t.Fatalf("the rendered system prompt has no recording-conventions heading:\n%s", prompt)
	}
	fields := strings.Fields(heading)
	if len(fields) == 0 {
		t.Fatalf("the conventions heading is empty")
	}
	word = fields[0]
	// The heading itself wraps over several lines; skip to the blank line that
	// ends it before reading the list.
	i := start + 1
	for i < len(lines) && strings.TrimSpace(lines[i]) != "" {
		i++
	}
	list := []string{}
	for ; i < len(lines); i++ {
		ln := lines[i]
		switch {
		case strings.HasPrefix(ln, "- "):
			bullets = append(bullets, ln)
			list = append(list, ln)
		case strings.TrimSpace(ln) == "", strings.HasPrefix(ln, " "):
			// blank separators and the wrapped continuation lines of a bullet
			list = append(list, ln)
		default:
			// the paragraph after the list: the list is over
			return word, heading, strings.Join(list, "\n"), bullets
		}
	}
	return word, heading, strings.Join(list, "\n"), bullets
}

// TestTheGuardClaimInTheSystemPromptIsTrue: the contract tells every Worker
// that a PreToolUse guard hook confines its writes. That sentence is a promise
// about a session, not about a script, and it was false for two tools: with
// Write/Edit in permissions.allow and absent from the matcher, dontAsk ran
// them bare and the hook was never invoked (T1219).
//
// The two halves are asserted together on purpose. The claim alone would pass
// on the broken tree — the sentence was already there — and the matcher alone
// would pass with the claim deleted, leaving the Worker to discover the
// envelope by hitting it. A guarantee and the thing that enforces it have to
// move in the same commit, which is the lesson this whole task is an instance
// of.
func TestTheGuardClaimInTheSystemPromptIsTrue(t *testing.T) {
	const claim = "guard hook confines your writes"
	s := RenderSystemPrompt("T0001", "/repo", "/repo/.rddev/worktrees/T0001", "/repo/.rddev/workers/T0001", "/repo/.rddev/worktrees")
	if !strings.Contains(s, claim) {
		t.Fatalf("the Worker contract no longer states %q; a Worker that is refused a write without having been told writes are confined will read the refusal as a malfunction", claim)
	}
	// The sentence itself must name the tools it claims to cover; "your
	// writes" alone would not say whether the file tools are meant to be
	// inside the envelope or left outside it (which is how it read before
	// T1219, when the sentence said "shell writes" and meant only Bash).
	// The bullet wraps across lines, so the "sentence" is the claim and the
	// continuation lines that follow it (the same wrapped-continuation shape
	// conventionsList walks).
	sentence := s[strings.Index(s, claim):]
	if i := strings.Index(sentence, "\n\n"); i >= 0 {
		sentence = sentence[:i]
	}
	for _, tool := range []string{"Write", "Edit"} {
		if !strings.Contains(sentence, tool) {
			t.Errorf("the contract sentence %q does not name the %s tool, so a Worker using it cannot tell the envelope covers it", sentence, tool)
		}
		// ...and the sentence has to be TRUE: the named tool must reach the
		// hook, or this is the same promise-without-mechanism the task exists
		// to remove.
		if !slices.Contains(strings.Split(guardMatcher, "|"), tool) {
			t.Errorf("the contract promises the guard confines %s writes and the matcher %q does not name %s — the promise is prose again", tool, guardMatcher, tool)
		}
	}
}
