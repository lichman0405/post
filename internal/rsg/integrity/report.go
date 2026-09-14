package integrity

import (
	"fmt"
	"strings"
)

// Verdict of a report. "pass" means every blocking check passed (warnings
// may still be absent); "pass_with_warnings" means nothing blocks but at
// least one warning fired; "blocked" means at least one blocking check
// failed and the proposal must not merge until it is resolved. The
// vocabulary matches the gate ladder's report (internal/rsg/validation),
// so the PR page renders one style for both.
const (
	VerdictPass         = "pass"
	VerdictPassWithWarn = "pass_with_warnings"
	VerdictBlocked      = "blocked"
)

// Result is the outcome of one check on one subject (an object version, a
// relation version, or the proposal as a whole). The Spec it came from is
// flattened into it, so a serialized report is self-explanatory without
// the package.
type Result struct {
	// Dimension is the check's dimension (Dimension).
	Dimension Dimension `json:"dimension"`
	// Check is the check id (CheckID).
	Check CheckID `json:"check"`
	// Severity is what a failure means at this check.
	Severity Severity `json:"severity"`
	// Passed reports whether the check held. A warning-severity failure
	// has Passed=false and does not block; a blocking-severity failure
	// blocks the report.
	Passed bool `json:"passed"`
	// Subject names what was checked: "dataset v1 (obj-…)" for a version,
	// "" for proposal-level checks.
	Subject string `json:"subject,omitempty"`
	// Detail is the concrete failure (which endpoint is dangling, which
	// policy does not resolve); empty when the check passed.
	Detail string `json:"detail,omitempty"`
	// Why is the Spec.Why of the check — the rationale rendered into
	// every result, so the report explains itself.
	Why string `json:"why"`
}

// Report is the full machine result of one integrity check run over one
// PR. It is the wire shape of the checks endpoint and the document the PR
// page's checks section renders. It is derived, never caller-supplied:
// the engine computes every field from the snapshot.
type Report struct {
	// Kind is the report's format marker ("integrity"), so the page can
	// distinguish it from gate reports.
	Kind string `json:"kind"`
	// ProjectID is the research boundary the PR belongs to.
	ProjectID string `json:"project_id"`
	// PRNumber is the PR's per-project number.
	PRNumber int64 `json:"pr_number"`
	// BaseStateID / ProposedStateID are the pinned states the checks ran
	// over — the report self-describes what it checked.
	BaseStateID     string `json:"base_state_id"`
	ProposedStateID string `json:"proposed_state_id"`
	// Verdict is VerdictPass, VerdictPassWithWarn or VerdictBlocked.
	Verdict string `json:"verdict"`
	// Results holds every check outcome, in spec order, per-object checks
	// grouped by subject before the proposal-level checks.
	Results []Result `json:"results"`
	// Explanation is the human-readable rendering of the verdict: every
	// failed check with its why. Derived, never caller-supplied.
	Explanation string `json:"explanation"`
	// ComputedAt is when the report was assembled (the application
	// layer's stamp). The check outcomes themselves are deterministic:
	// re-running over the same rows derives the same results.
	ComputedAt string `json:"computed_at"`
}

// Blocked reports whether any blocking check failed.
func (r Report) Blocked() bool { return r.Verdict == VerdictBlocked }

// Failures returns the failed results, blocking failures first.
func (r Report) Failures() []Result {
	var blocking, warnings []Result
	for _, res := range r.Results {
		if !res.Passed {
			if res.Severity == SeverityBlocking {
				blocking = append(blocking, res)
			} else {
				warnings = append(warnings, res)
			}
		}
	}
	return append(blocking, warnings...)
}

// BlockingFailures returns the failed results whose severity blocks.
func (r Report) BlockingFailures() []Result {
	var out []Result
	for _, res := range r.Results {
		if !res.Passed && res.Severity == SeverityBlocking {
			out = append(out, res)
		}
	}
	return out
}

// Warnings returns the failed results whose severity is a warning.
func (r Report) Warnings() []Result {
	var out []Result
	for _, res := range r.Results {
		if !res.Passed && res.Severity == SeverityWarning {
			out = append(out, res)
		}
	}
	return out
}

// finalize computes the derived fields of a report in place: the verdict
// from the results, and the explanation from the check declaration plus
// the failures. It is the single place the "why" of a verdict is rendered.
func (r *Report) finalize() {
	blocked := false
	warned := false
	for _, res := range r.Results {
		if res.Passed {
			continue
		}
		if res.Severity == SeverityBlocking {
			blocked = true
		} else {
			warned = true
		}
	}
	switch {
	case blocked:
		r.Verdict = VerdictBlocked
	case warned:
		r.Verdict = VerdictPassWithWarn
	default:
		r.Verdict = VerdictPass
	}
	r.Explanation = explainReport(*r)
}

// explainReport renders the human-readable verdict rationale.
func explainReport(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "integrity check for PR #%d: %s\n", r.PRNumber, r.Verdict)
	failures := r.Failures()
	if len(failures) == 0 {
		b.WriteString("all checks passed")
		if r.Verdict == VerdictPassWithWarn {
			b.WriteString("; see warnings above")
		}
		b.WriteString(".")
		return b.String()
	}
	for _, res := range failures {
		subject := ""
		if res.Subject != "" {
			subject = " on " + res.Subject
		}
		fmt.Fprintf(&b, "- [%s] %s/%s%s: %s (%s)\n", res.Severity, res.Dimension, res.Check, subject, res.Detail, res.Why)
	}
	return strings.TrimSuffix(b.String(), "\n")
}
