package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// The prompt renderer turns a DAG entry into the Worker's task package:
// task-package.json (validated against specs/orchestrator/task-package.schema.json,
// read from the repo at spawn so the schema and its users can never drift
// apart), plus prompt.md (the task narrative) and system.md (the Worker
// contract). The Worker receives no product history and no Supervisor
// context — only this package.

// TaskPackage is the validated task package handed to the Worker.
type TaskPackage struct {
	TaskID             string   `json:"task_id"`
	Goal               string   `json:"goal"`
	BaselineSHA        string   `json:"baseline_sha"`
	AllowedScope       []string `json:"allowed_scope"`
	ForbiddenScope     []string `json:"forbidden_scope"`
	Requirements       []string `json:"requirements"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	RequiredTests      []string `json:"required_tests"`
	RelevantSpecs      []string `json:"relevant_specs"`
	DecisionLevelMax   string   `json:"decision_level_max"`
	MaxTurns           *int     `json:"max_turns,omitempty"`
	MaxBudgetUSD       *float64 `json:"max_budget_usd,omitempty"`
}

// RenderTaskPackage builds the package from the DAG entry and the spawn
// overrides. The package is validated against the schema before it is
// written — a package that would not validate fails spawn instead of
// dispatching a Worker with an invalid contract.
func RenderTaskPackage(t *TaskSpec, baselineSHA string, maxTurns *int, maxBudgetUSD *float64) (*TaskPackage, error) {
	pkg := &TaskPackage{
		TaskID:       t.ID,
		Goal:         t.Title,
		BaselineSHA:  baselineSHA,
		AllowedScope: t.AllowedScope,
		// nil slices would marshal as JSON null, which the schema's array
		// types reject — normalize to [] so any valid DAG entry renders a
		// valid package.
		ForbiddenScope:     strSlice(t.ForbiddenScope),
		Requirements:       strSlice(t.Requirements),
		AcceptanceCriteria: strSlice(t.AcceptanceCriteria),
		RequiredTests:      strSlice(t.Tests),
		RelevantSpecs:      strSlice(t.RelevantSpecs),
		DecisionLevelMax:   t.DecisionLevelMax,
		MaxTurns:           maxTurns,
		MaxBudgetUSD:       maxBudgetUSD,
	}
	if pkg.DecisionLevelMax == "" {
		pkg.DecisionLevelMax = "L1"
	}
	if len(pkg.AllowedScope) == 0 {
		return nil, fmt.Errorf("task %s declares no allowed_scope — refusing to render a task package that cannot be collected", t.ID)
	}
	return pkg, nil
}

// strSlice normalizes nil to an empty slice so JSON encoding never emits null
// where the schema requires an array.
func strSlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// ValidateTaskPackage checks pkg against the schema file at schemaPath. Only
// the JSON Schema constructs the task-package schema actually uses are
// implemented; any other keyword fails closed with an explicit error instead
// of being silently ignored.
func ValidateTaskPackage(pkg *TaskPackage, schemaPath string) error {
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		return fmt.Errorf("reading task package schema %s: %w", schemaPath, err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		return fmt.Errorf("parsing task package schema %s: %w", schemaPath, err)
	}
	pkgJSON, err := json.Marshal(pkg)
	if err != nil {
		return fmt.Errorf("encoding task package: %w", err)
	}
	var doc any
	if err := json.Unmarshal(pkgJSON, &doc); err != nil {
		return fmt.Errorf("decoding task package: %w", err)
	}
	if err := validateAgainst(doc, schema, "task-package"); err != nil {
		return fmt.Errorf("task package for %s does not validate against %s: %w", pkg.TaskID, schemaPath, err)
	}
	return nil
}

// validateAgainst implements the subset of draft 2020-12 used by
// task-package.schema.json: type (object/string/array/integer/number/
// boolean/null), required, additionalProperties, properties, items,
// minItems/minLength/minimum, enum, pattern. Unsupported keywords are an
// error — validating less than the schema demands would be fail-open.
func validateAgainst(doc any, schema map[string]any, where string) error {
	if t, ok := schema["type"]; ok {
		if !typeMatches(doc, t) {
			return fmt.Errorf("%s: value %v is not of type %v", where, doc, t)
		}
	}
	if req, ok := schema["required"].([]any); ok {
		obj, isObj := doc.(map[string]any)
		if !isObj {
			return fmt.Errorf("%s: required is set but value is not an object", where)
		}
		for _, r := range req {
			key := r.(string)
			if _, present := obj[key]; !present {
				return fmt.Errorf("%s: missing required field %q", where, key)
			}
		}
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		obj, isObj := doc.(map[string]any)
		if !isObj {
			return fmt.Errorf("%s: properties is set but value is not an object", where)
		}
		for key, sub := range props {
			v, present := obj[key]
			if !present {
				continue
			}
			subSchema, isMap := sub.(map[string]any)
			if !isMap {
				return fmt.Errorf("%s.%s: schema entry is not an object", where, key)
			}
			if err := validateAgainst(v, subSchema, where+"."+key); err != nil {
				return err
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		arr, isArr := doc.([]any)
		if !isArr {
			return fmt.Errorf("%s: items is set but value is not an array", where)
		}
		for i, v := range arr {
			if err := validateAgainst(v, items, where+"["+strconv.Itoa(i)+"]"); err != nil {
				return err
			}
		}
	}
	if n, ok := schema["minItems"].(float64); ok {
		if arr, isArr := doc.([]any); !isArr || len(arr) < int(n) {
			return fmt.Errorf("%s: expected at least %d items", where, int(n))
		}
	}
	if n, ok := schema["minLength"].(float64); ok {
		if s, isStr := doc.(string); !isStr || len(s) < int(n) {
			return fmt.Errorf("%s: expected a string of at least %d characters", where, int(n))
		}
	}
	if n, ok := schema["minimum"].(float64); ok {
		if num, isNum := doc.(float64); !isNum || num < n {
			return fmt.Errorf("%s: %v is below minimum %v", where, doc, n)
		}
	}
	if enum, ok := schema["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if doc == e {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%s: %v is not one of the allowed values %v", where, doc, enum)
		}
	}
	if pat, ok := schema["pattern"].(string); ok {
		s, isStr := doc.(string)
		if !isStr {
			return fmt.Errorf("%s: pattern is set but value is not a string", where)
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Errorf("%s: schema pattern %q does not compile: %w", where, pat, err)
		}
		if !re.MatchString(s) {
			return fmt.Errorf("%s: %q does not match pattern %q", where, s, pat)
		}
	}
	if ap, ok := schema["additionalProperties"]; ok {
		if ap == false {
			obj, isObj := doc.(map[string]any)
			if !isObj {
				return fmt.Errorf("%s: additionalProperties is set but value is not an object", where)
			}
			known := map[string]bool{}
			if props, ok := schema["properties"].(map[string]any); ok {
				for k := range props {
					known[k] = true
				}
			}
			for k := range obj {
				if !known[k] {
					return fmt.Errorf("%s: unknown field %q", where, k)
				}
			}
		}
	}
	// Fail closed on constructs this validator does not implement.
	for k := range schema {
		switch k {
		case "$schema", "title", "description", "type", "required",
			"additionalProperties", "properties", "items", "minItems",
			"minLength", "minimum", "enum", "pattern":
			// implemented above
		default:
			return fmt.Errorf("%s: schema keyword %q is not supported by the task-package validator — refusing to validate less than the schema demands", where, k)
		}
	}
	return nil
}

func typeMatches(doc any, t any) bool {
	switch want := t.(type) {
	case string:
		return matchesType(doc, want)
	case []any:
		for _, alt := range want {
			if s, ok := alt.(string); ok && matchesType(doc, s) {
				return true
			}
		}
		return false
	}
	return false
}

// matchesType reports whether doc has JSON type want. Numbers decode to
// float64, so "integer" means a whole float64 while "number" means any
// float64 (a whole value satisfies both).
func matchesType(doc any, want string) bool {
	switch want {
	case "integer":
		f, ok := doc.(float64)
		return ok && f == float64(int64(f))
	case "number":
		_, ok := doc.(float64)
		return ok
	}
	switch doc.(type) {
	case map[string]any:
		return want == "object"
	case []any:
		return want == "array"
	case string:
		return want == "string"
	case bool:
		return want == "boolean"
	case nil:
		return want == "null"
	}
	return false
}

// RenderPrompt renders prompt.md — the task narrative the Worker receives.
func RenderPrompt(pkg *TaskPackage, worktree, resultDir string, worktreesDir, workersDir string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task %s: %s\n\n", pkg.TaskID, pkg.Goal)
	b.WriteString("You are an independent Claude Code Worker for the POST development system.\n")
	b.WriteString("Your worktree (a Supervisor-owned git worktree, never shared with another Worker):\n\n")
	fmt.Fprintf(&b, "  %s\n\n", worktree)
	fmt.Fprintf(&b, "Your RESULT.json (writable by you, outside the worktree):\n\n  %s\n\n", filepath.Join(resultDir, "RESULT.json"))
	b.WriteString("## Requirements\n\n")
	for _, r := range pkg.Requirements {
		fmt.Fprintf(&b, "- %s\n", r)
	}
	b.WriteString("\n## Acceptance criteria\n\n")
	for _, a := range pkg.AcceptanceCriteria {
		fmt.Fprintf(&b, "- %s\n", a)
	}
	b.WriteString("\n## Allowed scope (write ONLY inside these)\n\n")
	for _, s := range pkg.AllowedScope {
		fmt.Fprintf(&b, "- %s\n", s)
	}
	if len(pkg.ForbiddenScope) > 0 {
		b.WriteString("\n## Forbidden scope\n\n")
		for _, s := range pkg.ForbiddenScope {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	b.WriteString("\n## Required tests\n\n")
	for _, rt := range pkg.RequiredTests {
		fmt.Fprintf(&b, "- %s\n", rt)
	}
	if len(pkg.RelevantSpecs) > 0 {
		b.WriteString("\n## Relevant specs\n\n")
		for _, s := range pkg.RelevantSpecs {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	fmt.Fprintf(&b, "\n## Constraints\n\n- Decision level: up to %s; anything above must be reported, never decided.\n", pkg.DecisionLevelMax)
	fmt.Fprintf(&b, "- Baseline commit: %s — your diff will be collected against it.\n", pkg.BaselineSHA)
	if pkg.MaxBudgetUSD != nil {
		fmt.Fprintf(&b, "- Budget: $%.2f (hard stop).\n", *pkg.MaxBudgetUSD)
	}
	if pkg.MaxTurns != nil {
		fmt.Fprintf(&b, "- Max turns: %d.\n", *pkg.MaxTurns)
	}
	b.WriteString("\nWrite your RESULT.json EARLY and keep it updated — do not leave it to the\nlast operation. `completed` means \"submitted for acceptance\", not accepted:\nthe Supervisor re-runs your tests and re-reads your diff.\n")
	b.WriteString("\nVerify by trying to make things fail, not by running the happy path.\n")
	return b.String()
}

// RenderSystemPrompt renders system.md — the Worker contract appended to the
// Worker's system prompt.
func RenderSystemPrompt(taskID, repoRoot, worktree, resultDir, worktreesDir string) string {
	var b strings.Builder
	b.WriteString("# You are an independent Claude Code Worker in the POST development system.\n\n")
	b.WriteString("## Hard rules\n\n")
	b.WriteString("- No Git control-plane: never commit, push, merge, rebase, tag, branch,\n  or mutate worktrees, refs, remotes or git config. Read-only git is available.\n")
	b.WriteString("- No remote credentials.\n")
	b.WriteString("- Read the whole worktree, but write only inside `allowed_scope`. Anything\n  outside it is rejected at collection — a rejected task wastes the attempt.\n")
	b.WriteString("- Never weaken, skip or delete tests to make a gate pass.\n")
	b.WriteString("- Never modify product specifications to fit your implementation.\n")
	b.WriteString("- Never implement `follow_up_issues` yourself.\n")
	b.WriteString("- A PreToolUse guard hook confines your shell writes, file reads and\n  control-plane commands; a blocked tool call is the isolation working as\n  designed — adjust your approach, never try to bypass it.\n\n")
	fmt.Fprintf(&b, "## Paths\n\n- Repo root: %s\n- Your worktree: %s\n- Your RESULT.json: %s\n- Other Workers' worktrees: %s (readable, never writable)\n", repoRoot, worktree, filepath.Join(resultDir, "RESULT.json"), worktreesDir)
	b.WriteString("\n## RESULT.json\n\nWrite it EARLY and overwrite it as evidence improves. `completed` means\n\"submitted for acceptance\", not accepted. Report exact commands and real\nobserved output; anything not executed is `not_run` with a reason. If blocked,\nreturn `status: \"blocked\"` with the concrete blocker.\n")
	b.WriteString("\nThe worktree root contains CLAUDE.md — that is the Supervisor's\nconstitutional document, not yours. Your contract is your task package, then\nthis prompt.\n")
	return b.String()
}
