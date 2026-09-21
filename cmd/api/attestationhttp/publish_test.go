package attestationhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/attestations"
	"github.com/lichman0405/post/internal/domain"
	edgesec "github.com/lichman0405/post/internal/security"
)

// The refusal exit's contract, at the transport.
//
// tests/security/exits_test.go registers
// `attestationhttp/publish.go:writeAttestBlocked` in the byte-exit registry
// as an inline-text JSON exit. That file's own doctrine exempts the
// inline-text rows from the runtime probe because "their media type is not
// negotiable and their shape is pinned by TestEveryRegisteredExitStatesItsOwnHeaders
// above and by the envelope contract tests in their own packages" — and this
// package had no such test when the row was added, so the exemption would
// have been taken on credit. This is that test.
//
// It drives writeAttestError, not writeAttestBlocked directly, because the
// mapping from the command's Refused onto this exit is the part a change
// could break silently: a handler that stopped recognising the refusal would
// still compile, and the refusal would answer as a bare envelope with no
// report.
//
// The last assertion is this task's own (T0812): the exit is the one place a
// SERVER-side refusal becomes bytes a client holds, so it is where "the
// underlying private evidence is not in the response" has to be stated about
// the response rather than about the projection that built it.

// uuid-shaped, so the fixture satisfies the shape checks and the assertions
// below are about the exit rather than about the fixture. The two ids below
// are the PRIVATE half of an attestation — the basis state and the internal
// review — and appear in no field of the wire shape.
const (
	probeProjectID = "11111111-1111-4111-8111-111111111111"
	probeObjectID  = "77777777-7777-4777-8777-777777777777"
	probeVersionID = "33333333-3333-4333-8333-333333333333"
	probeStateID   = "44444444-4444-4444-8444-444444444444"
	probeReviewID  = "55555555-5555-4555-8555-555555555555"
)

// admissibleFacts is the same admissible shape the command's own tests start
// from: a public protocol version, resting on a state of the attesting
// project with an approved review of exactly that state.
func admissibleFacts() attestations.Facts {
	return attestations.Facts{
		ProjectID: probeProjectID,
		Target: attestations.TargetFacts{
			Kind:                    attestations.TargetKindProtocol,
			VersionID:               probeVersionID,
			ObjectID:                probeObjectID,
			Title:                   "A published protocol",
			LifecycleState:          string(domain.LifecycleActive),
			OwningProjectVisibility: "public",
		},
		Basis: attestations.BasisFacts{StateID: probeStateID, ProjectID: probeProjectID, StateHash: "sha256:deadbeef"},
		Review: attestations.ReviewFacts{ReviewID: probeReviewID, Kind: "scientific",
			Decision: string(domain.ReviewDecisionApproved), ReviewedStateID: probeStateID, PullRequestProjectID: probeProjectID},
	}
}

func TestRefusalExitStatesItsOwnHeaders(t *testing.T) {
	want := attestations.Want{
		ValidationType:   attestations.ValidationTypeReproduction,
		ValidationResult: attestations.ValidationResultConfirmed,
		OrgVisibility:    attestations.OrgVisibilityAnonymous,
	}
	preview := attestations.BuildPreview(admissibleFacts(), want)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+probeProjectID+"/attestations:publish", nil)
	writeAttestError(rec, req, &attestations.Refused{Preview: preview, Reasons: []string{"a reason"}})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want %q — the registry row classifies this exit as inline text", got, "application/json")
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want %q — the exit states it itself; it must not "+
			"depend on the edge above adding it", got, "nosniff")
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want none — the refusal is displayed, never downloaded", got)
	}

	kind, err := edgesec.JudgeExit(rec.Header())
	if err != nil {
		t.Fatalf("the exit is not a conforming byte exit: %v", err)
	}
	if kind != edgesec.ExitInlineText {
		t.Errorf("JudgeExit = %q, want %q", kind, edgesec.ExitInlineText)
	}

	var body blockedEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("the refusal body is not the documented envelope: %v (body %s)", err, rec.Body.String())
	}
	if body.Code != attestations.CodeAttestationRefused {
		t.Errorf("code = %q, want %q", body.Code, attestations.CodeAttestationRefused)
	}
	if body.Retryable {
		t.Error("retryable = true: a refusal is a decision, not a transient failure")
	}
	if body.Preview.ValidationResult != attestations.ValidationResultConfirmed {
		t.Errorf("the refusal carries preview.validation_result %q, want %q — a caller told only "+
			"\"refused\" cannot see what was refused over", body.Preview.ValidationResult, attestations.ValidationResultConfirmed)
	}
	if len(body.Preview.Withheld) == 0 {
		t.Error("preview.withheld is empty: a refusal has to be able to show that the private " +
			"half stays private")
	}

	// T0812: the private side is not merely absent from a field — it is not
	// in the bytes.
	raw := rec.Body.String()
	for _, secret := range []string{probeStateID, probeReviewID} {
		if strings.Contains(raw, secret) {
			t.Errorf("the refusal body carries %q, which is the private evidence the attestation "+
				"exists to keep out of the response", secret)
		}
	}
}
