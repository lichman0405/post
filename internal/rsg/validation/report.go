package validation

import (
	"fmt"
	"strings"
)

// Verdict of a report. "pass" means every blocking check passed (warnings
// may still be absent); "pass_with_warnings" means nothing blocks but at
// least one warning fired; "blocked" means at least one blocking check
// failed and the guarded transition is refused.
const (
	VerdictPass         = "pass"
	VerdictPassWithWarn = "pass_with_warnings"
	VerdictBlocked      = "blocked"
)

// Result is the outcome of one check on one subject (a version, or the
// snapshot as a whole). The Spec it came from is flattened into it, so a
// serialized report is self-explanatory without the package.
type Result struct {
	// Check is the check id (CheckID).
	Check CheckID `json:"check"`
	// Severity is what a failure would have meant at the gate the check
	// ran at.
	Severity Severity `json:"severity"`
	// Passed reports whether the check held. A warning-severity failure
	// has Passed=false and does not block; a blocking-severity failure
	// blocks the report.
	Passed bool `json:"passed"`
	// Subject names what was checked: "hypothesis v1 (obj-…)" for a
	// version, "" for snapshot-level checks.
	Subject string `json:"subject,omitempty"`
	// Detail is the concrete failure (which field is missing, which hash
	// mismatches); empty when the check passed. Client-safe: no storage
	// or dependency internals.
	Detail string `json:"detail,omitempty"`
	// Why is the Spec.Why of the check — the rationale rendered into
	// every result, so the report explains itself.
	Why string `json:"why"`
}

// Report is the full outcome of one gate run over one snapshot. It is the
// wire shape of the validate endpoint (docs/22 §7: returns the complete
// result, modifies no state).
type Report struct {
	// Gate is the gate that ran.
	Gate Gate `json:"gate"`
	// Verdict is VerdictPass, VerdictPassWithWarn or VerdictBlocked.
	Verdict string `json:"verdict"`
	// Results holds every check outcome, in gate-spec order, per-object
	// checks grouped by subject before the snapshot-level checks.
	Results []Result `json:"results"`
	// Explanation is the human-readable rendering of the verdict: the
	// gate's purpose and every failed check with its why. It is derived,
	// never caller-supplied.
	Explanation string `json:"explanation"`
}

// Blocked reports whether any blocking check failed.
func (r Report) Blocked() bool { return r.Verdict == VerdictBlocked }

// Failures returns the failed results, blocking failures first.
func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if !res.Passed {
			out = append(out, res)
		}
	}
	return out
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
// from the results, and the explanation from the gate's declaration plus
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
	fmt.Fprintf(&b, "gate %s: %s\n", r.Gate, r.Verdict)
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
		fmt.Fprintf(&b, "- [%s] %s%s: %s (%s)\n", res.Severity, res.Check, subject, res.Detail, res.Why)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// GateBlockedError reports a gate run whose blocking checks failed. It is
// the error the guarded commands return: the command was refused, and the
// full report — every check, every why — travels with the refusal so the
// caller can see exactly what to fix.
type GateBlockedError struct {
	Report Report
}

// Error implements error.
func (e *GateBlockedError) Error() string {
	n := len(e.Report.BlockingFailures())
	return fmt.Sprintf("validation: gate %s blocked: %d blocking check(s) failed (see the report)", e.Report.Gate, n)
}

// Code maps the refusal to the stable wire codes of docs/45: a refusal
// whose blocking failures are all schema-family checks is
// SCHEMA_VALIDATION_FAILED; all provenance-family is PROVENANCE_INCOMPLETE;
// anything mixed or beyond is RSG_VALIDATION_FAILED.
func (e *GateBlockedError) Code() string {
	schema, provenance, other := false, false, false
	for _, res := range e.Report.BlockingFailures() {
		switch res.Check {
		case CheckSchemaKnown, CheckSchemaCore, CheckSchemaTyped, CheckAssetSchema:
			schema = true
		case CheckStateLinkage, CheckCommitLinkage, CheckActorConsistency,
			CheckPayloadIntegrity, CheckChainIntegrity, CheckAuthoritativeFields:
			provenance = true
		default:
			other = true
		}
	}
	switch {
	case schema && !provenance && !other:
		return "SCHEMA_VALIDATION_FAILED"
	case provenance && !schema && !other:
		return "PROVENANCE_INCOMPLETE"
	default:
		return "RSG_VALIDATION_FAILED"
	}
}
