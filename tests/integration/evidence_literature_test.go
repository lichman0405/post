// Task T0509 required test "literature evidence negative".
//
// docs/10 §6 forbids `DOI -> supports Claim` outright: a literature assertion
// must name the specific evidence unit it cites (figure, table, results
// assertion, supplementary dataset, method); docs/19 §4 writes the same rule
// from the external-reference side ("Evidence Assertion 指向具体
// location/excerpt/figure/table/dataset/method"). V1's schema carries no
// excerpt field, so the reasoning note is the only place that unit can be
// named — internal/rsg/semantics/evidence_assertion.go says so verbatim — and
// the write path therefore REFUSES a literature assertion whose note is empty.
//
// # What this file pins, and what only this file can
//
// The unit suite (internal/application/rsg) pins the decision over fakes. What
// only real PostgreSQL and the whole composed path can settle is pinned here,
// through the contract's own route: the request really is refused, the refused
// write really leaves no row, and the boundary of the rule is where it is
// claimed to be.
//
// The boundary is the point of this file, and it has two sides:
//
//   - "Nothing was written" is refused (an empty note, a whitespace-only note);
//   - "not written well enough" is ACCEPTED. Whether a note names a SUFFICIENT
//     unit is a scientific call the check must not make for the author —
//     internal/rsg/semantics/evidence_assertion.go: "Both rules warn only:
//     whether the evidence genuinely establishes causation and whether a note
//     names a sufficient unit are scientific calls the check must not make for
//     the author" — and docs/10 §4 forbids this platform assigning a value to
//     it (V1 不自动赋数值权重; CLAUDE.md §9.12). A test with only the negative
//     case passes for an implementation that refuses every literature
//     assertion, which is a rule the repository never made.
//
// The last case is the control: the same empty note on a NON-literature
// assertion is accepted, exactly as it was before this rule existed, because
// docs/10 §6 is about literature evidence and the command branches on the
// evidence TYPE, never on the text.
package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/rsg/semantics"
)

const evidenceLiteratureTaskID = "T0509"

// literatureAssertionBody is the contract's route body with the evidence type
// this file is about. note is written through verbatim, so a case can send an
// empty note, a whitespace-only one, or a real location.
func literatureAssertionBody(targetVersion, evidenceVersion, note string) string {
	raw, err := json.Marshal(map[string]any{
		"target_version_ref":   "object_version:" + targetVersion,
		"evidence_version_ref": "object_version:" + evidenceVersion,
		"relation":             "supports",
		"evidence_type":        "literature",
		"scope":                map[string]any{"conditions": "298 K, 1 bar"},
		"directness":           "direct",
		"inference_nature":     "observational",
		"reasoning_note":       note,
	})
	if err != nil {
		panic(err) // a map of strings cannot fail to marshal
	}
	return string(raw)
}

// TestLiteratureEvidenceWithoutAUnitIsRefused pins the required negative on
// the contract's own route, against real rows.
func TestLiteratureEvidenceWithoutAUnitIsRefused(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixtureFor(t, ctx, evidenceLiteratureTaskID)

	for _, tc := range []struct {
		name string
		note string
	}{
		{"no reasoning note at all", ""},
		{"a whitespace-only reasoning note", " \t\n "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := assertionRowsIn(t, ctx, f.w.pool, f.alpha.id)

			resp := f.postEvidenceAssertion(t, f.w.alice, f.alpha.id, f.alphaPub,
				literatureAssertionBody(f.alphaVersion, f.alphaEvidence, tc.note))
			status := resp.StatusCode
			raw := readAll(t, resp)

			// A caller's own permanent input mistake is a 4xx, never the
			// retryable 503: a client that believes "retryable: true" resends
			// a request that can never succeed (docs/45).
			if status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: a literature assertion with no evidence unit is refused: %s", status, raw)
			}
			var env struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			}
			if err := json.Unmarshal([]byte(raw), &env); err != nil {
				t.Fatalf("refusal envelope: %v: %s", err, raw)
			}
			if env.Code != semantics.HintLiteratureEvidenceUnitUnnamed {
				t.Errorf("code = %q, want %q", env.Code, semantics.HintLiteratureEvidenceUnitUnnamed)
			}
			if env.Retryable {
				t.Errorf("retryable = true for a request that can never succeed: %s", raw)
			}
			// The advisory the refusal promotes is IN the answer: the author
			// is told which unit to locate, not merely that the write failed.
			// "figure" is docs/10 §6's own list.
			if !strings.Contains(env.Message, "figure") {
				t.Errorf("the refusal message does not carry the guidance to locate the unit: %q", env.Message)
			}
			// A refused write leaves nothing: no row, not even a partial one.
			if after := assertionRowsIn(t, ctx, f.w.pool, f.alpha.id); after != before {
				t.Errorf("rows after the refusal = %d, want %d: a refused write leaves nothing", after, before)
			}
		})
	}
}

// TestLiteratureEvidenceThatNamesAUnitIsAccepted is the other half, and the
// half a refusal-only implementation would fail. Notes that no reviewer would
// credit are accepted all the same: judging them is the author's and the
// reviewer's call, and the assertion is stored with its relation and its
// review_state, never folded into a verdict.
func TestLiteratureEvidenceThatNamesAUnitIsAccepted(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixtureFor(t, ctx, evidenceLiteratureTaskID)

	for _, note := range []string{
		"figure 3b, 298 K isotherm",
		"supplementary dataset S2",
		"the method section",
		// Deliberately weak: a location a DOI would not stand behind. It is
		// still a location the author named, so it is stored, untouched.
		"the paper",
	} {
		t.Run(note, func(t *testing.T) {
			resp := f.postEvidenceAssertion(t, f.w.alice, f.alpha.id, f.alphaPub,
				literatureAssertionBody(f.alphaVersion, f.alphaEvidence, note))
			status := resp.StatusCode
			raw := readAll(t, resp)
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want 201: a literature assertion that names a unit is accepted, however weak the unit: %s", status, raw)
			}
			// Decoded locally rather than through the shared wire type: this
			// file reads the two fields the rule is about (the note that was
			// stored, and the advisories that did NOT fire), and nothing else.
			var got struct {
				EvidenceType  string `json:"evidence_type"`
				ReasoningNote string `json:"reasoning_note"`
				Hints         []struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"hints"`
			}
			if err := json.Unmarshal([]byte(raw), &got); err != nil {
				t.Fatalf("evidence assertion payload: %v: %s", err, raw)
			}
			if got.EvidenceType != "literature" {
				t.Errorf("evidence_type = %q, want literature", got.EvidenceType)
			}
			if got.ReasoningNote != note {
				t.Errorf("reasoning_note = %q, want %q: the location is the author's text, never rewritten", got.ReasoningNote, note)
			}
			// The unit-location advisory is not attached to an assertion that
			// has a note to locate it in.
			for _, hint := range got.Hints {
				if hint.Code == semantics.HintLiteratureEvidenceUnitUnnamed {
					t.Errorf("the unnamed-unit advisory fired for a note that IS present: %+v", hint)
				}
			}
		})
	}
}

// TestANonLiteratureAssertionNeedsNoNote is the control: the rule is scoped by
// the evidence type (docs/10 §6 is about literature evidence), so an
// experimental assertion with no note is accepted exactly as it was before.
func TestANonLiteratureAssertionNeedsNoNote(t *testing.T) {
	ctx := testCtx(t)
	f := newExternalEvidenceFixtureFor(t, ctx, evidenceLiteratureTaskID)

	body := fmt.Sprintf(`{"target_version_ref":"object_version:%s","evidence_version_ref":"object_version:%s",`+
		`"relation":"supports","evidence_type":"experimental","scope":{"conditions":"298 K, 1 bar"},`+
		`"directness":"direct","inference_nature":"observational"}`,
		f.alphaVersion, f.alphaEvidence)

	resp := f.postEvidenceAssertion(t, f.w.alice, f.alpha.id, f.alphaPub, body)
	status := resp.StatusCode
	raw := readAll(t, resp)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201: a non-literature assertion with no note is not this rule's business: %s", status, raw)
	}
}
