package schemareg

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// objectDoc builds a minimal valid CoreScientificObject-shaped document for
// the schema named by name, with extra carrying the type-specific fields
// (including the "type" const for typed object schemas).
func objectDoc(name string, extra map[string]any) map[string]any {
	doc := map[string]any{
		"id":              "obj-00000001",
		"version":         1,
		"project_id":      "proj-00000001",
		"title":           "Fixture " + name,
		"lifecycle_state": "active",
		"schema_ref": map[string]any{
			"id":      canonicalID(name),
			"version": "1",
		},
		"created_by": "user-00000001",
		"created_at": "2026-01-01T00:00:00Z",
	}
	for k, v := range extra {
		doc[k] = v
	}
	return doc
}

// v1Fixtures holds one minimal valid document per canonical V1 schema — the
// acceptance fixture set: every V1 schema must validate something.
func v1Fixtures() map[string]map[string]any {
	return map[string]map[string]any{
		"calculation": objectDoc("calculation", map[string]any{
			"type":      "calculation",
			"objective": "Compute the band gap",
			"method":    "DFT/PBE",
		}),
		"claim": objectDoc("claim", map[string]any{
			"type":       "claim",
			"statement":  "X increases Y",
			"claim_type": "causal",
		}),
		"core-scientific-object": objectDoc("core-scientific-object", map[string]any{
			"type": "scientific_object",
		}),
		"dataset": objectDoc("dataset", map[string]any{
			"type":         "dataset",
			"purpose":      "Raw measurements",
			"access_level": "restricted",
		}),
		"evidence-assertion": {
			"id":                   "ev-00000001",
			"target_version_ref":   "obj-00000001",
			"evidence_version_ref": "obj-00000002",
			"relation":             "supports",
			"evidence_type":        "experimental",
			"review_state":         "unreviewed",
			"created_by":           "user-00000001",
			"created_at":           "2026-01-01T00:00:00Z",
		},
		"experiment": objectDoc("experiment", map[string]any{
			"type":      "experiment",
			"objective": "Measure conductivity",
		}),
		"external_reference": objectDoc("external_reference", map[string]any{
			"type":                "external_reference",
			"source_type":         "publication",
			"external_identifier": "doi:10.1000/xyz",
		}),
		"finding": objectDoc("finding", map[string]any{
			"type":               "finding",
			"statement":          "Y doubled under X",
			"claim_version_refs": []any{"obj-00000001"},
		}),
		"hypothesis": objectDoc("hypothesis", map[string]any{
			"type":        "hypothesis",
			"statement":   "X causes Y",
			"question_id": "rq-00000001",
		}),
		"material": objectDoc("material", map[string]any{
			"type": "material",
		}),
		"protocol": objectDoc("protocol", map[string]any{
			"type":    "protocol",
			"purpose": "Synthesis",
		}),
		"relation": {
			"id":                 "rel-00000001",
			"version":            1,
			"type":               "uses",
			"source_version_ref": "obj-00000001",
			"target_version_ref": "obj-00000002",
			"created_by":         "user-00000001",
			"created_at":         "2026-01-01T00:00:00Z",
		},
		"research_question": objectDoc("research_question", map[string]any{
			"type":      "research_question",
			"statement": "Why does X increase Y?",
		}),
		"research-asset-version": {
			"asset_id":       "asset-00000001",
			"version":        "1.0.0",
			"asset_type":     "dataset",
			"origin_refs":    []any{"obj-00000001"},
			"rights":         map[string]any{"holder": "org-00000001"},
			"integrity_hash": "sha256:abc123",
		},
		"rsg-manifest": {
			"format_version": "v1",
			"project_id":     "proj-00000001",
			"state_id":       "state-00000001",
			"generated_at":   "2026-01-01T00:00:00Z",
			"object_versions": []any{map[string]any{
				"id":                   "ov-00000001",
				"object_id":            "obj-00000001",
				"object_type":          "experiment",
				"version_no":           1,
				"state_id":             "state-00000001",
				"branch_id":            nil,
				"schema_ref":           map[string]any{"id": "https://open-rd.example/schemas/experiment.schema.json", "version": "1"},
				"title":                "E1",
				"lifecycle_state":      "active",
				"payload":              map[string]any{"objective": "Measure conductivity"},
				"visibility_policy_id": nil,
				"integrity_hash":       "sha256:abc123",
				"created_by":           "user-00000001",
				"created_at":           "2026-01-01T00:00:00Z",
			}},
			"relation_versions": []any{map[string]any{
				"id":                       "rv-00000001",
				"relation_id":              "rel-00000001",
				"version_no":               1,
				"state_id":                 "state-00000001",
				"relation_type":            "uses",
				"source_object_version_id": "ov-00000001",
				"target_object_version_id": "ov-00000002",
				"payload":                  map[string]any{},
				"integrity_hash":           "sha256:abc123",
				"created_by":               "user-00000001",
				"created_at":               "2026-01-01T00:00:00Z",
			}},
			"schema_refs": []any{"https://open-rd.example/schemas/experiment.schema.json@1"},
			"policy_refs": []any{},
			"blob_refs":   []any{map[string]any{"id": "blob-00000001", "hash": "sha256:abc123"}},
			"git_ref":     "abc123",
			"state_hash":  "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		"sample": objectDoc("sample", map[string]any{
			"type":        "sample",
			"material_id": "mat-00000001",
		}),
	}
}

// claimFixture is the valid claim document shared with the concurrency test.
func claimFixture() map[string]any {
	return v1Fixtures()["claim"]
}

// TestAllV1SchemasValidateFixtures is the acceptance test: every canonical
// V1 schema must compile and validate a minimal valid document.
func TestAllV1SchemasValidateFixtures(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if len(v1Fixtures()) != len(canonicalNames) {
		t.Fatalf("fixture table covers %d schemas, catalog has %d", len(v1Fixtures()), len(canonicalNames))
	}
	for name, doc := range v1Fixtures() {
		doc := doc
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("marshal fixture: %v", err)
			}
			ref := Ref{ID: canonicalID(name), Version: CanonicalV1}
			if err := r.Validate(ref, raw); err != nil {
				t.Errorf("valid %s document rejected: %v", name, err)
			}
		})
	}
}

// TestAllV1SchemasRejectViolations proves every V1 schema fails invalid
// documents (an empty object violates every schema's required fields).
func TestAllV1SchemasRejectViolations(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range canonicalNames {
		name := name
		t.Run(name, func(t *testing.T) {
			ref := Ref{ID: canonicalID(name), Version: CanonicalV1}
			err := r.Validate(ref, []byte(`{}`))
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("empty document: want *ValidationError, got %v", err)
			}
			if verr.Ref != ref {
				t.Errorf("ValidationError.Ref = %s, want %s", verr.Ref, ref)
			}
			if !strings.Contains(verr.Error(), "SCHEMA_VALIDATION_FAILED") {
				t.Errorf("error %q lacks the SCHEMA_VALIDATION_FAILED code", verr.Error())
			}
		})
	}
}

// TestSpecificSchemaConstraints proves the schemas' own constraints bite:
// enums, minLength/minItems, additionalProperties and format assertions.
func TestSpecificSchemaConstraints(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		doc  map[string]any
		why  string
	}{
		{"claim", mutate(claimFixture(), "claim_type", "wrong"), "claim_type enum"},
		{"core-scientific-object", mutate(objectDoc("core-scientific-object", map[string]any{"type": "scientific_object"}), "lifecycle_state", "deleted"), "lifecycle_state enum"},
		{"core-scientific-object", mutate(objectDoc("core-scientific-object", map[string]any{"type": "scientific_object"}), "created_at", "2026-01-01"), "date-time format assertion"},
		{"calculation", mutate(objectDoc("calculation", map[string]any{"type": "calculation", "objective": "o", "method": "m"}), "bogus", 1), "additionalProperties"},
		{"dataset", mutate(objectDoc("dataset", map[string]any{"type": "dataset", "purpose": "p"}), "access_level", "secret"), "access_level enum"},
		{"evidence-assertion", mutate(v1Fixtures()["evidence-assertion"], "relation", "causes"), "relation enum"},
		{"external_reference", mutate(objectDoc("external_reference", map[string]any{"type": "external_reference", "source_type": "publication", "external_identifier": "x"}), "source_type", "blog"), "source_type enum"},
		{"finding", mutate(objectDoc("finding", map[string]any{"type": "finding", "statement": "s", "claim_version_refs": []any{"x"}}), "claim_version_refs", []any{}), "claim_version_refs minItems"},
		{"hypothesis", mutate(objectDoc("hypothesis", map[string]any{"type": "hypothesis", "statement": "h", "question_id": "q"}), "assessment", "wrong"), "assessment enum"},
		{"material", mutate(objectDoc("material", map[string]any{"type": "material"}), "bogus", 1), "additionalProperties"},
		{"protocol", mutate(objectDoc("protocol", map[string]any{"type": "protocol", "purpose": "p"}), "bogus", 1), "additionalProperties"},
		{"relation", mutate(v1Fixtures()["relation"], "version", 0), "version minimum"},
		{"research_question", mutate(objectDoc("research_question", map[string]any{"type": "research_question", "statement": "Why?"}), "statement", ""), "statement minLength"},
		{"research-asset-version", mutate(v1Fixtures()["research-asset-version"], "asset_type", "software"), "asset_type enum"},
		{"rsg-manifest", mutate(v1Fixtures()["rsg-manifest"], "generated_at", "yesterday"), "date-time format assertion"},
		{"sample", without(objectDoc("sample", map[string]any{"type": "sample", "material_id": "m"}), "material_id"), "missing material_id required"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name+"/"+tc.why, func(t *testing.T) {
			raw, err := json.Marshal(tc.doc)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			ref := Ref{ID: canonicalID(tc.name), Version: CanonicalV1}
			err = r.Validate(ref, raw)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want *ValidationError, got %v", err)
			}
			if !strings.Contains(verr.Error(), tc.why) && !strings.Contains(strings.ToLower(verr.Error()), strings.ToLower(tc.why)) {
				t.Logf("error %q may not mention %q (informational only)", verr.Error(), tc.why)
			}
		})
	}
}

// TestRegisterResolvesRefsAgainstCatalog proves extension schemas can $ref
// canonical schemas and resolve them locally — the extension path T0213
// builds on. The reference must not hit the network.
func TestRegisterResolvesRefsAgainstCatalog(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const extDoc = `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "project:acme:profile-v1",
	  "title": "ProjectProfile",
	  "type": "object",
	  "required": ["lab", "core"],
	  "properties": {
	    "lab": {"type": "string"},
	    "core": {"$ref": "https://open-rd.example/schemas/core-scientific-object.schema.json"}
	  },
	  "additionalProperties": false
	}`
	ref := Ref{ID: "project:acme:profile-v1", Version: "1"}
	if _, err := r.Register(ref.ID, ref.Version, []byte(extDoc)); err != nil {
		t.Fatalf("Register with $ref to canonical schema: %v", err)
	}
	core, err := json.Marshal(v1Fixtures()["core-scientific-object"])
	if err != nil {
		t.Fatal(err)
	}
	valid := []byte(`{"lab":"ACME","core":` + string(core) + `}`)
	if err := r.Validate(ref, valid); err != nil {
		t.Errorf("valid extension document with $ref content rejected: %v", err)
	}
	broken := []byte(`{"lab":"ACME","core":{"id":"short"}}`)
	err = r.Validate(ref, broken)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("broken $ref content: want *ValidationError, got %v", err)
	}
	if !strings.Contains(verr.Error(), "minLength") {
		t.Errorf("error %q does not carry the referenced schema's constraint", verr.Error())
	}
}

// TestValidateUnknownSchemaFailsExplicitly is the acceptance test for the
// unknown-schema path: unknown ids and unknown versions fail loudly, with
// the next step in the message (docs/45).
func TestValidateUnknownSchemaFailsExplicitly(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Unknown id: ErrUnknownSchema, message points at the extension path.
	err = r.Validate(Ref{ID: "project:nobody:ghost", Version: "1"}, []byte(`{}`))
	if !errors.Is(err, ErrUnknownSchema) {
		t.Fatalf("unknown id: want ErrUnknownSchema, got %v", err)
	}
	if !strings.Contains(err.Error(), "Register") {
		t.Errorf("unknown-id error %q does not point at the extension path", err)
	}
	// Known id, unknown version: ErrUnknownVersion, message lists what exists.
	err = r.Validate(Ref{ID: canonicalID("claim"), Version: "999"}, []byte(`{}`))
	if !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("unknown version: want ErrUnknownVersion, got %v", err)
	}
	if !strings.Contains(err.Error(), CanonicalV1) {
		t.Errorf("unknown-version error %q does not list registered versions", err)
	}
	// The two failures must be distinguishable from each other.
	if errors.Is(err, ErrUnknownSchema) {
		t.Error("unknown version also matches ErrUnknownSchema")
	}
}

// TestValidateMalformedDocument proves documents that are not valid JSON
// fail before schema validation, with a plain (non-ValidationError) error.
func TestValidateMalformedDocument(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := Ref{ID: canonicalID("claim"), Version: CanonicalV1}
	for _, doc := range []string{``, `{`, `{"statement": }`, `[1,`} {
		err := r.Validate(ref, []byte(doc))
		if err == nil {
			t.Errorf("doc %q accepted", doc)
			continue
		}
		var verr *ValidationError
		if errors.As(err, &verr) {
			t.Errorf("doc %q: want plain error, got *ValidationError", doc)
		}
	}
}

// mutate copies doc, overrides key and returns the copy.
func mutate(doc map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(doc)+1)
	for k, v := range doc {
		out[k] = v
	}
	out[key] = value
	return out
}

// without copies doc with key removed.
func without(doc map[string]any, key string) map[string]any {
	out := make(map[string]any, len(doc))
	for k, v := range doc {
		if k != key {
			out[k] = v
		}
	}
	return out
}
