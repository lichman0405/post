package domain

import (
	"encoding/json"
	"testing"
)

func TestValidStateVia(t *testing.T) {
	for _, v := range []StateVia{ViaWeb, ViaAPI, ViaMCP, ViaClaudeCode, ViaGitCompat, ViaSystem} {
		if !ValidStateVia(v) {
			t.Errorf("ValidStateVia(%q) = false, want true", v)
		}
	}
	for _, v := range []StateVia{"", "cli", "Web", "webhook", "system "} {
		if ValidStateVia(v) {
			t.Errorf("ValidStateVia(%q) = true, want false", v)
		}
	}
}

func TestValidStateOperationKind(t *testing.T) {
	for _, k := range []StateOperationKind{
		OperationObjectVersionCreated,
		OperationRelationVersionCreated,
		OperationEvidenceAsserted,
		OperationBlobAttached,
		OperationPolicyApplied,
		OperationSchemaReferenceSet,
	} {
		if !ValidStateOperationKind(k) {
			t.Errorf("ValidStateOperationKind(%q) = false, want true", k)
		}
	}
	for _, k := range []StateOperationKind{"", "object_created", "object_version_created ", "related_to"} {
		if ValidStateOperationKind(k) {
			t.Errorf("ValidStateOperationKind(%q) = true, want false", k)
		}
	}
}

func TestComputeStateHashDeterministicAndContentSensitive(t *testing.T) {
	base := "11111111-1111-1111-1111-111111111111"
	ops := []StateOperation{
		{Kind: OperationObjectVersionCreated, EntityID: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", VersionNo: 1},
		{Kind: OperationRelationVersionCreated, EntityID: "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", VersionNo: 1,
			Detail: json.RawMessage(`{"relation_type":"derived_from"}`)},
	}
	h1, err := ComputeStateHash(&base, ops)
	if err != nil {
		t.Fatalf("ComputeStateHash: %v", err)
	}
	h2, err := ComputeStateHash(&base, ops)
	if err != nil {
		t.Fatalf("ComputeStateHash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("hash not deterministic: %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Fatalf("hash %q has length %d, want 64 (sha256 hex)", h1, len(h1))
	}
	// Every dimension of the transition content must move the hash.
	otherParent := "22222222-2222-2222-2222-222222222222"
	if hp, _ := ComputeStateHash(&otherParent, ops); hp == h1 {
		t.Error("parent change did not change the hash")
	}
	if hn, _ := ComputeStateHash(nil, ops); hn == h1 {
		t.Error("nil parent did not change the hash")
	}
	if he, _ := ComputeStateHash(&base, nil); he == h1 {
		t.Error("empty operations did not change the hash")
	}
	oneOp := ops[:1]
	if ho, _ := ComputeStateHash(&base, oneOp); ho == h1 {
		t.Error("operation count change did not change the hash")
	}
	reordered := []StateOperation{ops[1], ops[0]}
	if hr, _ := ComputeStateHash(&base, reordered); hr == h1 {
		t.Error("operation order change did not change the hash")
	}
	changed := append([]StateOperation(nil), ops...)
	changed[0].VersionNo = 2
	if hv, _ := ComputeStateHash(&base, changed); hv == h1 {
		t.Error("version_no change did not change the hash")
	}
}

func TestComputeStateHashGenesis(t *testing.T) {
	g1, err := ComputeStateHash(nil, nil)
	if err != nil {
		t.Fatalf("ComputeStateHash(nil, nil): %v", err)
	}
	g2, err := ComputeStateHash(nil, []StateOperation{})
	if err != nil {
		t.Fatalf("ComputeStateHash(nil, []): %v", err)
	}
	// The genesis hash is canonical: nil and empty operations render the
	// same (the empty array, never null).
	if g1 != g2 {
		t.Fatalf("genesis hash differs between nil and empty operations: %s vs %s", g1, g2)
	}
}

func TestComputeStateHashRejectsInvalidDetail(t *testing.T) {
	ops := []StateOperation{{Kind: OperationBlobAttached, EntityID: "x", Detail: json.RawMessage(`{not json`)}}
	if _, err := ComputeStateHash(nil, ops); err == nil {
		t.Fatal("ComputeStateHash accepted a non-JSON operation detail")
	}
}

func TestComputeStateHashKeyOrderStable(t *testing.T) {
	// The same logical detail in different textual forms must NOT be
	// treated as equal content: the hash pins the exact stored bytes.
	// (Canonicalization of detail JSON is the caller's job, like the
	// payload discipline of the version logs.)
	a := []StateOperation{{Kind: OperationBlobAttached, EntityID: "x", Detail: json.RawMessage(`{"a":1,"b":2}`)}}
	b := []StateOperation{{Kind: OperationBlobAttached, EntityID: "x", Detail: json.RawMessage(`{"b":2,"a":1}`)}}
	ha, _ := ComputeStateHash(nil, a)
	hb, _ := ComputeStateHash(nil, b)
	if ha == hb {
		t.Error("semantically different operation detail bytes hashed identically")
	}
}
