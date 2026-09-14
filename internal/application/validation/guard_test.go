package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// rsgPayload builds a payload from a raw JSON string or any marshalable
// value.
func rsgPayload(v any) json.RawMessage {
	if s, ok := v.(string); ok {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(v)
	return b
}

func timeNow() time.Time { return time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC) }

func rsgHash(p json.RawMessage) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

func rsgOps(t *testing.T, ops ...domain.StateOperation) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(ops)
	if err != nil {
		t.Fatalf("marshal ops: %v", err)
	}
	return b
}

// stubProbe is a TxProbe over canned rows, keyed by branch/state id.
type stubProbe struct {
	states    []domain.ProjectState
	commits   []domain.StateCommit
	objects   []domain.ScientificObjectVersion
	relations []domain.RelationVersion
}

func (p *stubProbe) ListStatesTx(context.Context, TxQuerier, string) ([]domain.ProjectState, error) {
	return p.states, nil
}
func (p *stubProbe) ListCommitsTx(context.Context, TxQuerier, string) ([]domain.StateCommit, error) {
	return p.commits, nil
}
func (p *stubProbe) ListStateObjectVersionsTx(_ context.Context, _ TxQuerier, _ string) ([]domain.ScientificObjectVersion, error) {
	return p.objects, nil
}
func (p *stubProbe) ListStateRelationVersionsTx(context.Context, TxQuerier, string) ([]domain.RelationVersion, error) {
	return p.relations, nil
}

func newTestGuard(t *testing.T, probe TxProbe) *Guard {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return NewGuard(rsgvalidation.NewValidator(reg), probe)
}

// guardFacts is a CommitFacts matching the stub rows: one commit of an
// incomplete hypothesis (statement only — question_id missing) on a fresh
// branch whose base predates the branch.
func guardFacts() CommitFacts {
	return CommitFacts{
		ProjectID:     "proj-1",
		BranchID:      "br-1",
		ActorID:       "alice",
		BaseStateID:   strPtrOf("st-gen"),
		ResultStateID: "st-1",
		Operations: []domain.StateOperation{
			{Kind: domain.OperationObjectVersionCreated, EntityID: "object-1", VersionNo: 1},
		},
	}
}

func guardProbeRows(payload json.RawMessage, integrityHash string) *stubProbe {
	branch := "br-1"
	return &stubProbe{
		states: []domain.ProjectState{{ID: "st-1", BranchID: &branch, ParentStateID: strPtrOf("st-gen")}},
		objects: []domain.ScientificObjectVersion{{
			ID: "v-1", ObjectID: "object-1", VersionNo: 1, StateID: "st-1",
			SchemaID: schemareg.CanonicalNamespace + "hypothesis.schema.json", SchemaVersion: "1",
			Title: "H1", LifecycleState: domain.LifecycleActive, Payload: payload,
			IntegrityHash: integrityHash, CreatedBy: "alice", CreatedAt: time.Now().UTC(),
		}},
	}
}

// TestGuardBlocksWhatTheCallerCouldHavePrechecked is the unit-level pin of
// "command 再次 server validate": the guard reads the rows through the
// transaction — a version the caller wrote incomplete blocks the pr gate
// even though the caller chose to commit it, and the refusal carries the
// full report.
func TestGuardBlocksWhatTheCallerCouldHavePrechecked(t *testing.T) {
	ctx := context.Background()
	incomplete := rsgPayload(map[string]string{"statement": "probe"})
	probe := guardProbeRows(incomplete, rsgHash(incomplete))
	guard := newTestGuard(t, probe)

	err := guard.RequireCommitGate(ctx, nil, rsgvalidation.GatePR, guardFacts())
	var blocked *rsgvalidation.GateBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("RequireCommitGate(pr) = %v, want *GateBlockedError", err)
	}
	if blocked.Report.Gate != rsgvalidation.GatePR {
		t.Errorf("report gate = %s, want pr", blocked.Report.Gate)
	}
	if blocked.Code() != "SCHEMA_VALIDATION_FAILED" {
		t.Errorf("Code() = %s, want SCHEMA_VALIDATION_FAILED", blocked.Code())
	}
	if len(blocked.Report.BlockingFailures()) == 0 {
		t.Error("no blocking failures in the report")
	}

	// The same commit passes the draft gate: the ladder differs by gate,
	// and the guard honours the gate the commit was named with.
	if err := guard.RequireCommitGate(ctx, nil, rsgvalidation.GateDraft, guardFacts()); err != nil {
		t.Errorf("RequireCommitGate(draft) = %v, want nil (incomplete domain fields warn at draft)", err)
	}
}

// TestGuardBlocksTamperedHashAtMain pins the second half of the acceptance
// criterion: a version row whose integrity hash does not re-hash its stored
// payload is refused at main by payload_integrity — the server recomputes,
// it does not trust whatever hash the caller wrote.
func TestGuardBlocksTamperedHashAtMain(t *testing.T) {
	ctx := context.Background()
	payload := rsgPayload(map[string]any{
		"statement":       "probe",
		"question_id":     "q-1",
		"hypothesis_type": "mechanistic",
		"scope":           map[string]any{},
	})
	probe := guardProbeRows(payload, strings.Repeat("0", 64)) // tampered
	guard := newTestGuard(t, probe)

	err := guard.RequireCommitGate(ctx, nil, rsgvalidation.GateMain, guardFacts())
	var blocked *rsgvalidation.GateBlockedError
	if !errors.As(err, &blocked) {
		t.Fatalf("RequireCommitGate(main) = %v, want *GateBlockedError", err)
	}
	found := false
	for _, res := range blocked.Report.BlockingFailures() {
		if res.Check == rsgvalidation.CheckPayloadIntegrity {
			found = true
		}
	}
	if !found {
		t.Errorf("payload_integrity not among the blocking failures: %+v", blocked.Report.BlockingFailures())
	}
}

func TestGuardPassesCompleteCommit(t *testing.T) {
	ctx := context.Background()
	payload := rsgPayload(map[string]any{
		"statement":       "probe",
		"question_id":     "q-1",
		"hypothesis_type": "mechanistic",
		"scope":           map[string]any{"detail": "x"},
	})
	probe := guardProbeRows(payload, rsgHash(payload))
	guard := newTestGuard(t, probe)
	if err := guard.RequireCommitGate(ctx, nil, rsgvalidation.GateMain, guardFacts()); err != nil {
		t.Fatalf("RequireCommitGate(main) = %v, want nil", err)
	}
}

func TestGuardRejectsNonCommitGates(t *testing.T) {
	guard := newTestGuard(t, &stubProbe{})
	ctx := context.Background()
	for _, gate := range []rsgvalidation.Gate{rsgvalidation.GateRelease, rsgvalidation.GateAsset, "gold"} {
		err := guard.RequireCommitGate(ctx, nil, gate, guardFacts())
		if !errors.Is(err, ErrValidation) {
			t.Errorf("gate %s: error = %v, want ErrValidation", gate, err)
		}
	}
}

func TestGuardRequiresResultStateOnBranch(t *testing.T) {
	guard := newTestGuard(t, &stubProbe{states: []domain.ProjectState{{ID: "st-other"}}})
	err := guard.RequireCommitGate(context.Background(), nil, rsgvalidation.GateDraft, guardFacts())
	if !errors.Is(err, ErrStore) {
		t.Fatalf("missing result state: %v, want ErrStore", err)
	}
}
