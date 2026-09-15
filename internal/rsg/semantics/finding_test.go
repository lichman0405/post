package semantics

import (
	"strings"
	"testing"
)

// TestFindingWithoutRefsIsADraftNotARefusal: an ABSENT claim_version_refs
// is a draft, not a caller error. docs/08 §CoreScientificObject line 9:
// "Draft 可缺部分 domain field；进入 PR/main/release 时按 validation gate
// 逐步增强" — and the gate ladder implements exactly that, reporting an
// incomplete draft instead of refusing it and blocking it only at PR
// (internal/rsg/validation/spec.go:90 CheckSchemaTyped = SeverityWarning at
// the draft gate, :100 the same check = SeverityBlocking at the PR gate).
// The two sibling checks in this package already make that call for their
// own absent fields: checkHypothesis hints when question_id is absent
// (semantics.go checkHypothesis) and checkExternalReference treats the
// absent source_type/external_identifier pair as "a valid draft (the pr
// gate demands the pair)". Hard-failing here would refuse a write the gate
// ladder says is reportable.
func TestFindingWithoutRefsIsADraftNotARefusal(t *testing.T) {
	// JSON null is the registry's way of saying absent (checkResearchQuestion
	// states the rule: "JSON null means absent and stays legal"), so it is
	// held to the same standard as omission.
	cases := []struct {
		name    string
		payload map[string]any
	}{
		{"field absent", map[string]any{"finding_type": "observation"}},
		{"field null", map[string]any{"finding_type": "observation", "claim_version_refs": nil}},
	}
	for _, tc := range cases {
		errs, hints := Check("finding", "", tc.payload)
		if len(errs) != 0 {
			t.Fatalf("%s: Check(finding) produced hard failures %v, want none — an absent domain field is the draft allowance's business (spec.go:90)", tc.name, errs)
		}
		if len(hints) != 1 {
			t.Fatalf("%s: hints = %+v, want exactly one: the absence must still be REPORTED, not silently accepted", tc.name, hints)
		}
		if hints[0].Code != HintFindingClaimVersionRefs {
			t.Errorf("%s: hint code = %q, want %q", tc.name, hints[0].Code, HintFindingClaimVersionRefs)
		}
		if !strings.Contains(hints[0].Message, "claim_version_refs") {
			t.Errorf("%s: hint %q does not name the field the finding is missing", tc.name, hints[0].Message)
		}
	}
}

// TestFindingRefsPresentButUnusableStillFails: the draft allowance covers
// ABSENCE only. A payload that does state what it pins, and states it
// wrongly, is decided mechanically from the payload alone — the hard-failure
// half of this package's boundary (semantics.go's package doc) — and is
// refused at every gate, draft included. Present-but-empty is the same rule
// checkHypothesis's question_id and checkResearchQuestion's
// parent_question_id enforce: "no claim versions" is expressed by omitting
// the field, never by an empty list; a malformed value could never name a
// scientific_object_versions row at all.
func TestFindingRefsPresentButUnusableStillFails(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		wantMsg string
	}{
		{"empty array", map[string]any{"claim_version_refs": []any{}}, "must pin at least one claim version"},
		{"string refs", map[string]any{"claim_version_refs": "11111111-1111-4111-8111-111111111111"}, "must be an array"},
		{"object refs", map[string]any{"claim_version_refs": map[string]any{"version": "11111111-1111-4111-8111-111111111111"}}, "must be an array"},
		{"non-string entry", map[string]any{"claim_version_refs": []any{42}}, "is not a string"},
		{"malformed uuid entry", map[string]any{"claim_version_refs": []any{"claim-v1"}}, "is not a canonical uuid"},
		{"empty string entry", map[string]any{"claim_version_refs": []any{""}}, "is not a canonical uuid"},
	}
	for _, tc := range cases {
		tc.payload["finding_type"] = "observation"
		errs, hints := Check("finding", "", tc.payload)
		if len(errs) != 1 {
			t.Fatalf("%s: hard failures = %v, want exactly one", tc.name, errs)
		}
		if !strings.Contains(errs[0].Error(), tc.wantMsg) {
			t.Errorf("%s: failure %q does not contain %q", tc.name, errs[0].Error(), tc.wantMsg)
		}
		if len(hints) != 0 {
			t.Errorf("%s: hints = %+v, want none — a present refs value is never excused as a draft", tc.name, hints)
		}
	}
}

func TestFindingValidPayloadPasses(t *testing.T) {
	payload := map[string]any{
		"statement":          "The claimed uptake was not reproduced under the stated conditions.",
		"finding_type":       "negative_finding",
		"assessment":         "accepted",
		"claim_version_refs": []any{"11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"},
	}
	errs, hints := Check("finding", "", payload)
	if len(errs) != 0 {
		t.Fatalf("valid finding produced hard failures %v", errs)
	}
	if len(hints) != 0 {
		t.Fatalf("valid finding produced hints %+v, want none", hints)
	}
}

func TestFindingChecksArePayloadLocal(t *testing.T) {
	// Existence is storage's truth, never this package's: a ref that is a
	// well-formed uuid passes here whether or not a row exists — the
	// database guard (00057) is what rejects a ref to a missing or
	// non-claim version.
	payload := map[string]any{
		"finding_type":       "trend",
		"claim_version_refs": []any{"99999999-9999-4999-8999-999999999999"},
	}
	errs, _ := Check("finding", "", payload)
	if len(errs) != 0 {
		t.Fatalf("well-formed ref to a possibly nonexistent version produced hard failures %v; existence is the database's call", errs)
	}
}

func TestFindingDispatchReachesCheckFinding(t *testing.T) {
	// The Check dispatch table must route "finding" to the finding
	// checks. A payload with a malformed refs value is the canary — it is
	// valid for every OTHER type and hard-fails only for findings — and
	// the draft hint goes the same way: a finding-shaped payload that
	// lacks the field must warn only on the finding branch.
	errs, hints := Check("finding", "", map[string]any{"claim_version_refs": "not-an-array"})
	if len(errs) != 1 {
		t.Fatalf("Check(finding) with a malformed claim_version_refs produced %v, want exactly one hard failure; the dispatch is not wired", errs)
	}
	if len(hints) != 0 {
		t.Fatalf("Check(finding) with a malformed claim_version_refs produced hints %+v, want one hard failure only", hints)
	}
	errs, hints = Check("finding", "", map[string]any{"finding_type": "observation"})
	if len(errs) != 0 || len(hints) != 1 {
		t.Fatalf("Check(finding) without claim_version_refs = (%v, %+v), want (no failures, one draft hint)", errs, hints)
	}
	for _, typ := range []string{"claim", "hypothesis", "research_question", "experiment", "dataset"} {
		errs, hints := Check(typ, "", map[string]any{"finding_type": "observation"})
		if len(errs) != 0 {
			t.Errorf("Check(%s) of a finding-shaped payload produced hard failures %v; only the finding type should apply the finding rules", typ, errs)
		}
		for _, h := range hints {
			// Other types may well have something of their own to say
			// (a hypothesis with no question_id hints too); what must NOT
			// happen is the finding rule leaking across the dispatch.
			if h.Code == HintFindingClaimVersionRefs {
				t.Errorf("Check(%s) of a finding-shaped payload produced the finding hint %+v; only the finding type should apply the finding rules", typ, h)
			}
		}
	}
}

func TestFindingErrorMessageNamesTheRule(t *testing.T) {
	// Hard failures use fixed, client-safe messages (docs/22 §5): the
	// message must name the rule that failed, not storage detail. The
	// present-but-empty case is the one to pin, because it is the case
	// the new draft hint must NOT swallow.
	errs, _ := Check("finding", "", map[string]any{"claim_version_refs": []any{}})
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want exactly one", errs)
	}
	msg := errs[0].Error()
	for _, want := range []string{"finding", "claim_version_refs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message %q does not name %q", msg, want)
		}
	}
}
