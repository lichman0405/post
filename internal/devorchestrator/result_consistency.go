package devorchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
)

// RESULT consistency (T0012 requirement 1). One Worker delivered
// status: completed on a RESULT whose own first line said "INTERIM SNAPSHOT"
// and which had two required tests not_run — collection trusted the claim
// instead of checking it against the evidence. These checks are mechanical:
// a claim of completion is verified against the document itself, and any
// contradiction is a rejection with a precise reason. They are the G1 gate's
// executor (the rest of G1 is the collect invariant suite).

// WorkerResultDoc is the parsed RESULT.json subset these checks read (the
// full schema validation happens separately).
type WorkerResultDoc struct {
	TaskID  string `json:"task_id"`
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Tests   []struct {
		Command  string `json:"command"`
		Label    string `json:"label"`
		Status   string `json:"status"`
		Evidence string `json:"evidence"`
	} `json:"tests"`
	Acceptance []struct {
		Criterion string `json:"criterion"`
		Status    string `json:"status"`
		Evidence  string `json:"evidence"`
	} `json:"acceptance"`
}

// ResultConsistencyCheck is one named finding of the consistency review.
type ResultConsistencyCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"` // passed | failed
	Detail string `json:"detail"`
}

// CheckResultConsistency reads RESULT.json at resultPath and verifies the
// claims against the document: an INTERIM-marked file is rejected outright;
// a `completed` status must have every listed test passed (no not_run, no
// failed), every required test covered by a passed entry, and every
// acceptance entry passed, covering at least the package's criteria count.
// A `failed` or `blocked` status is not a contradiction — it is reported by
// the caller as a rejected run.
//
// packageCriteria is the authoritative acceptance-criteria list and
// requiredTests the authoritative test list; nil means "not available"
// (only the self-consistency rules apply).
func CheckResultConsistency(resultPath string, packageCriteria, requiredTests []string) ([]ResultConsistencyCheck, bool, error) {
	raw, err := os.ReadFile(resultPath)
	if err != nil {
		return nil, false, fmt.Errorf("reading RESULT.json for the consistency check: %w", err)
	}
	checks := []ResultConsistencyCheck{}
	fail := func(name, detail string) {
		checks = append(checks, ResultConsistencyCheck{Name: name, Status: "failed", Detail: detail})
	}
	pass := func(name, detail string) {
		checks = append(checks, ResultConsistencyCheck{Name: name, Status: "passed", Detail: detail})
	}

	// The INTERIM marker: the file's own first line announcing it is not a
	// deliverable. A completion claim must never ride on a document that
	// marks itself as work in progress.
	firstLine := ""
	for _, ln := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(ln) != "" {
			firstLine = ln
			break
		}
	}
	if strings.Contains(strings.ToUpper(firstLine), "INTERIM") {
		fail("result-marker", fmt.Sprintf("the first line of RESULT.json is %q — the document marks itself as an interim snapshot and cannot claim completion", strings.TrimSpace(firstLine)))
	} else {
		pass("result-marker", "RESULT.json does not mark itself as an interim snapshot")
	}

	var doc WorkerResultDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		// The schema check reports the parse problem; consistency has nothing
		// further to say about an unreadable document.
		return checks, false, nil
	}

	// A failed/blocked RESULT is an honest non-completion: no consistency
	// contradiction, but it can never become verification (the caller uses
	// completedOK=false to reject the run).
	if doc.Status != "completed" {
		pass("result-status", fmt.Sprintf("status is %q — an honest non-completion, not a contradiction", doc.Status))
		return checks, false, nil
	}

	// status == completed: every claim inside the document must back it.
	pass("result-status", "status is completed — every claim inside the document is now checked against it")
	var notRun, failedTests []string
	for _, t := range doc.Tests {
		if t.Status != "passed" {
			entry := t.Command
			if t.Status == "not_run" {
				notRun = append(notRun, entry)
			} else if t.Status == "failed" {
				failedTests = append(failedTests, entry)
			}
		}
	}
	switch {
	case len(notRun) > 0 || len(failedTests) > 0:
		fail("result-tests", fmt.Sprintf(
			"status completed but %d test(s) not_run (%s) and %d test(s) failed (%s) — a completed claim with unrun or failing tests is a contradiction; run them or report status blocked/failed",
			len(notRun), strings.Join(notRun, "; "), len(failedTests), strings.Join(failedTests, "; ")))
	default:
		pass("result-tests", fmt.Sprintf("status completed and all %d listed test(s) passed", len(doc.Tests)))
	}

	// Every REQUIRED test must have a passed entry — the omission neighbour of
	// the shipped not_run defect: dropping a required test from tests[]
	// entirely must not smuggle a completion through.
	//
	// The entry declares which requirement it covers (`label`), or names it in
	// the command. The explicit field is the point: the DAG names required
	// tests as labels ("auth unit") while `command` holds the command that was
	// run, so before `label` existed the two could only be matched by
	// coincidence. T0101 was rejected twice on this check while running both
	// required suites both times — once with the label spelled inside the
	// command and once without. The entry must still be `passed`; nothing about
	// what counts as evidence changed.
	var missing []string
	if len(requiredTests) > 0 {
		for _, rt := range requiredTests {
			var statuses []string
			for _, t := range doc.Tests {
				if testCovers(t.Label, t.Command, rt) {
					statuses = append(statuses, t.Status)
				}
			}
			switch {
			case len(statuses) == 0:
				missing = append(missing, rt)
			case !slices.Contains(statuses, "passed"):
				missing = append(missing, rt+" ("+strings.Join(statuses, ", ")+")")
			}
		}
	}
	if len(missing) > 0 {
		fail("result-tests-coverage", fmt.Sprintf(
			"status completed but %d required test(s) have no passed entry: %s — every required test needs a passed entry with evidence",
			len(missing), strings.Join(missing, "; ")))
	} else if len(requiredTests) > 0 {
		pass("result-tests-coverage", fmt.Sprintf("all %d required test(s) have a passed entry", len(requiredTests)))
	}

	var notPassed []string
	for _, a := range doc.Acceptance {
		if a.Status != "passed" {
			notPassed = append(notPassed, fmt.Sprintf("%s (%s)", a.Criterion, a.Status))
		}
	}
	if len(notPassed) > 0 {
		fail("result-acceptance", fmt.Sprintf("status completed but acceptance entries not passed: %s", strings.Join(notPassed, "; ")))
	} else if len(packageCriteria) > 0 && len(doc.Acceptance) < len(packageCriteria) {
		fail("result-acceptance", fmt.Sprintf(
			"status completed but only %d acceptance entr(ies) for %d criterion(a) in the task package (%s) — every criterion needs an entry with evidence",
			len(doc.Acceptance), len(packageCriteria), strings.Join(packageCriteria, "; ")))
	} else {
		pass("result-acceptance", fmt.Sprintf("status completed and all %d acceptance entr(ies) passed", len(doc.Acceptance)))
	}

	consistent := true
	for _, c := range checks {
		if c.Status == "failed" {
			consistent = false
		}
	}
	return checks, consistent, nil
}

// testCovers reports whether a RESULT tests[] entry covers the required test
// label, either because it declares the label explicitly or because its
// command names the label.
//
// The explicit field is the point. The DAG names required tests as labels
// ("auth unit") while the contract's command holds what was run, so a Worker
// that runs exactly the right suite has no way to say which requirement it
// satisfied — T0101 was rejected twice for that reason, once with the label
// spelled in the command and once without, while having run both suites both
// times. Matching on a substring of a free-text field is a convention nobody
// wrote down; `label` is the same fact stated rather than inferred.
func testCovers(label, command, required string) bool {
	if required == "" {
		return false
	}
	if label == required {
		return true
	}
	return commandNamesTest(command, required)
}

// commandNamesTest reports whether a RESULT tests[].command names the required
// test label.
//
// The DAG lists required tests as labels ("auth unit") and the contract's
// command field holds the command that was run, so the two are never literally
// equal. Matching by label mention is what makes the coverage rule satisfiable
// by an honest Worker; the caller still requires the matched entry to be
// `passed`, so this loosens only the way a label is located, not what counts
// as evidence.
//
// What this is NOT — stated plainly because a security review rightly asked: it
// is a self-contradiction detector, not an anti-forgery control. A Worker that
// invents an entry ("command": "true  # auth unit", "status": "passed") defeats
// label matching exactly as easily as it defeats string equality; a Worker that
// admits not_run is caught either way. Nothing in a RESULT.json is evidence —
// it is a claim, and the subject writes it. The enforcement of "the required
// tests were really run" is the Supervisor's independent G2 re-run of those
// tests (CLAUDE.md §6), which is why an unsatisfiable check here was the worse
// failure: it rejected every honest run while stopping no dishonest one.
func commandNamesTest(command, label string) bool {
	if label == "" {
		return false
	}
	if command == label {
		return true
	}
	return strings.Contains(strings.ToLower(command), strings.ToLower(label))
}
