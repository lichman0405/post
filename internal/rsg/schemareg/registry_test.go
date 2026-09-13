package schemareg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// canonicalID builds the $id of a canonical schema from its file stem.
func canonicalID(name string) string {
	return CanonicalNamespace + name + ".schema.json"
}

// canonicalNames is the complete V1 schema catalog (specs/schemas).
var canonicalNames = []string{
	"calculation",
	"claim",
	"core-scientific-object",
	"dataset",
	"evidence-assertion",
	"experiment",
	"external_reference",
	"finding",
	"hypothesis",
	"material",
	"protocol",
	"relation",
	"research_question",
	"research-asset-version",
	"rsg-manifest",
	"sample",
}

// TestNewLoadsCanonicalV1 proves the registry loads the whole V1 catalog at
// startup, keyed by $id and registered under version "1".
func TestNewLoadsCanonicalV1(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs := r.List()
	if len(refs) != len(canonicalNames) {
		t.Fatalf("registered %d schemas, want %d", len(refs), len(canonicalNames))
	}
	byID := make(map[string][]string)
	for _, ref := range refs {
		byID[ref.ID] = append(byID[ref.ID], ref.Version)
	}
	for _, name := range canonicalNames {
		id := canonicalID(name)
		versions := byID[id]
		if len(versions) != 1 || versions[0] != CanonicalV1 {
			t.Errorf("%s: registered under %v, want exactly [%s]", id, versions, CanonicalV1)
		}
		s, ok := r.Get(Ref{ID: id, Version: CanonicalV1})
		if !ok {
			t.Errorf("%s: Get failed", id)
			continue
		}
		if s.ContentHash == "" {
			t.Errorf("%s: empty content hash", id)
		}
	}
}

// TestNewLoadsCanonicalV1Deterministically proves two registries hold
// identical content for every canonical id.
func TestNewLoadsCanonicalV1Deterministically(t *testing.T) {
	a, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ref := range a.List() {
		sa, _ := a.Get(ref)
		sb, ok := b.Get(ref)
		if !ok || sa.ContentHash != sb.ContentHash {
			t.Errorf("%s: registries disagree (a=%q b=%q ok=%v)", ref, sa.ContentHash, sb.ContentHash, ok)
		}
	}
}

// TestCanonicalSchemasAreNamespaced proves every embedded schema self-
// identifies inside the reserved canonical namespace (New would have failed
// otherwise, but this pins the property explicitly).
func TestCanonicalSchemasAreNamespaced(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, ref := range r.List() {
		if !strings.HasPrefix(ref.ID, CanonicalNamespace) {
			t.Errorf("%s: outside canonical namespace %q", ref, CanonicalNamespace)
		}
	}
}

// TestRegisterExtensionPath proves the namespaced extension path: a
// non-canonical schema can be registered, listed, looked up and validated.
func TestRegisterExtensionPath(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const extDoc = `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "project:acme:profile-v1",
	  "title": "ProjectProfile",
	  "type": "object",
	  "required": ["lab"],
	  "properties": {"lab": {"type": "string"}},
	  "additionalProperties": false
	}`
	ref := Ref{ID: "project:acme:profile-v1", Version: "1"}
	if _, err := r.Register(ref.ID, ref.Version, []byte(extDoc)); err != nil {
		t.Fatalf("Register: %v", err)
	}
	s, ok := r.Get(ref)
	if !ok {
		t.Fatal("Get after Register failed")
	}
	if err := s.Validate([]byte(`{"lab":"ACME lab"}`)); err != nil {
		t.Errorf("valid extension document rejected: %v", err)
	}
	if err := s.Validate([]byte(`{}`)); err == nil {
		t.Error("invalid extension document accepted")
	} else {
		var verr *ValidationError
		if !errors.As(err, &verr) {
			t.Errorf("want *ValidationError, got %T: %v", err, err)
		}
	}
	// The extension must be visible through the registry-wide views.
	if got := r.Versions(ref.ID); len(got) != 1 || got[0] != "1" {
		t.Errorf("Versions(%q) = %v", ref.ID, got)
	}
	found := false
	for _, listed := range r.List() {
		if listed == ref {
			found = true
		}
	}
	if !found {
		t.Errorf("List() does not include %s", ref)
	}
}

// TestRegisterRejectsUnnamespacedID proves bare extension ids fail: the
// extension path is namespaced by construction.
func TestRegisterRejectsUnnamespacedID(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := r.Register("custom-profile", "1", []byte(`{"type":"object"}`)); err == nil {
		t.Error("bare id accepted, want namespaced-id rejection")
	}
	if _, err := r.Register("", "1", []byte(`{"type":"object"}`)); err == nil {
		t.Error("empty id accepted")
	}
	if _, err := r.Register("project:acme:x", "", []byte(`{"type":"object"}`)); err == nil {
		t.Error("empty version accepted")
	}
}

// TestRegisterRejectsReservedNamespace proves runtime registrations cannot
// squat on official schema ids, including case variants: URI hosts are
// case-insensitive, so the guard folds case.
func TestRegisterRejectsReservedNamespace(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, id := range []string{
		canonicalID("calculation"),
		"https://OPEN-RD.EXAMPLE/schemas/claim.schema.json",
		"HTTPS://open-rd.example/schemas/material.schema.json",
	} {
		_, err := r.Register(id, "1", []byte(`{"type":"object"}`))
		if !errors.Is(err, ErrReservedID) {
			t.Errorf("id %q: want ErrReservedID, got %v", id, err)
		}
	}
	// A lookalike that merely contains the host as a suffix is still a
	// legitimate namespaced extension id.
	if _, err := r.Register("project:open-rd.example:lookalike", "1", []byte(`{"type":"object"}`)); err != nil {
		t.Errorf("non-reserved lookalike id rejected: %v", err)
	}
}

// TestRegisterImmutableContent proves old schema versions are never
// overwritten: a conflicting re-registration fails and the original content
// keeps validating.
func TestRegisterImmutableContent(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := Ref{ID: "project:acme:form", Version: "1"}
	docA := []byte(`{"type":"object","required":["a"],"properties":{"a":{"type":"string"}},"additionalProperties":false}`)
	docB := []byte(`{"type":"object","required":["b"],"properties":{"b":{"type":"string"}},"additionalProperties":false}`)

	if _, err := r.Register(ref.ID, ref.Version, docA); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	// Identical re-registration is an idempotent no-op.
	if _, err := r.Register(ref.ID, ref.Version, docA); err != nil {
		t.Errorf("identical re-registration failed: %v", err)
	}
	// Different content must be rejected, never overwrite.
	if _, err := r.Register(ref.ID, ref.Version, docB); !errors.Is(err, ErrAlreadyRegistered) {
		t.Fatalf("want ErrAlreadyRegistered, got %v", err)
	}
	// The original content still validates: doc A's shape passes, doc B's
	// shape fails.
	s, ok := r.Get(ref)
	if !ok {
		t.Fatal("original registration vanished after conflicting re-register")
	}
	if err := s.Validate([]byte(`{"a":"x"}`)); err != nil {
		t.Errorf("original content stopped validating: %v", err)
	}
	if err := s.Validate([]byte(`{"b":"y"}`)); err == nil {
		t.Error("rejected content validates, so the entry was overwritten")
	}
	// New content goes under a new version; the old version stays usable.
	if _, err := r.Register(ref.ID, "2", docB); err != nil {
		t.Fatalf("register v2: %v", err)
	}
	if got := r.Versions(ref.ID); len(got) != 2 || got[0] != "1" || got[1] != "2" {
		t.Errorf("Versions(%q) = %v, want [1 2]", ref.ID, got)
	}
	if _, ok := r.Get(Ref{ID: ref.ID, Version: "1"}); !ok {
		t.Error("version 1 no longer retrievable after registering version 2")
	}
}

// TestRegisterRejectsBadSchemaDocuments proves the loader fails loudly on
// documents that are not valid JSON or not valid JSON Schemas, and on $id
// mismatches.
func TestRegisterRejectsBadSchemaDocuments(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cases := []struct {
		name string
		doc  string
	}{
		{"malformed JSON", `{"type": `},
		{"invalid schema", `{"type": 42}`},
		{"$id mismatch", `{"$id": "project:other:id", "type": "object"}`},
	}
	for _, tc := range cases {
		ref := Ref{ID: "project:acme:bad-" + strings.ReplaceAll(tc.name, " ", "-"), Version: "1"}
		if _, err := r.Register(ref.ID, ref.Version, []byte(tc.doc)); err == nil {
			t.Errorf("%s: accepted, want rejection", tc.name)
		}
	}
	// A rejected registration must not leave a half-registered entry.
	for _, tc := range cases {
		ref := Ref{ID: "project:acme:bad-" + strings.ReplaceAll(tc.name, " ", "-"), Version: "1"}
		if _, ok := r.Get(ref); ok {
			t.Errorf("%s: rejected registration left an entry behind", tc.name)
		}
	}
}

// TestContentHashIsDocumentDigest pins ContentHash to the sha256 of the
// exact registered bytes (docs/21 §10).
func TestContentHashIsDocumentDigest(t *testing.T) {
	doc := []byte(`{"type":"object"}`)
	s, err := compileSchema(Ref{ID: "project:acme:digest", Version: "1"}, doc, nil)
	if err != nil {
		t.Fatalf("compileSchema: %v", err)
	}
	sum := sha256.Sum256(doc)
	if s.ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("ContentHash = %q, want sha256 of doc", s.ContentHash)
	}
}

// TestListAndVersionsSorted proves the registry views are deterministic.
func TestListAndVersionsSorted(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, version := range []string{"10", "2", "1"} {
		if _, err := r.Register("project:acme:sort", version, []byte(`{"type":"object"}`)); err != nil {
			t.Fatalf("Register v%s: %v", version, err)
		}
	}
	// Versions are strings and sort lexically; the property under test is
	// determinism, not numeric ordering.
	if got := r.Versions("project:acme:sort"); len(got) != 3 || got[0] != "1" || got[1] != "10" || got[2] != "2" {
		t.Errorf("Versions = %v, want [1 10 2]", got)
	}
	refs := r.List()
	for i := 1; i < len(refs); i++ {
		if refs[i].ID < refs[i-1].ID || (refs[i].ID == refs[i-1].ID && refs[i].Version < refs[i-1].Version) {
			t.Errorf("List not sorted at %d: %v after %v", i, refs[i], refs[i-1])
		}
	}
}

// TestSnapshotRefsResolveDeterministically proves a $ref to a multi-version
// extension id resolves to the lowest registered version — the oldest, most
// stable content (docs/21 §8) — not to whatever map iteration yields.
func TestSnapshotRefsResolveDeterministically(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const targetID = "project:acme:target"
	if _, err := r.Register(targetID, "1", []byte(`{"type":"object","required":["x"],"properties":{"x":{"type":"string"}},"additionalProperties":false}`)); err != nil {
		t.Fatalf("register target v1: %v", err)
	}
	if _, err := r.Register(targetID, "10", []byte(`{"type":"object","required":["y"],"properties":{"y":{"type":"string"}},"additionalProperties":false}`)); err != nil {
		t.Fatalf("register target v10: %v", err)
	}
	consumer := `{
	  "$schema": "https://json-schema.org/draft/2020-12/schema",
	  "$id": "project:acme:consumer-v1",
	  "type": "object",
	  "required": ["core"],
	  "properties": {"core": {"$ref": "` + targetID + `"}},
	  "additionalProperties": false
	}`
	ref := Ref{ID: "project:acme:consumer-v1", Version: "1"}
	if _, err := r.Register(ref.ID, ref.Version, []byte(consumer)); err != nil {
		t.Fatalf("register consumer: %v", err)
	}
	// v1 is the lowest version, so its required field is "x".
	if err := r.Validate(ref, []byte(`{"core":{"x":"1"}}`)); err != nil {
		t.Errorf("lowest-version shape rejected: %v", err)
	}
	if err := r.Validate(ref, []byte(`{"core":{"y":"1"}}`)); err == nil {
		t.Error("v10-only shape accepted: the $ref did not resolve to the lowest version")
	}
}

// TestConcurrentConflictingRegister proves the immutability rule holds under
// contention: when two goroutines race to register one id+version with
// different content, exactly one wins and the other gets
// ErrAlreadyRegistered — the loser's content is never observable.
func TestConcurrentConflictingRegister(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ref := Ref{ID: "project:acme:race", Version: "1"}
	shapes := []string{"a", "b"}
	docs := make([][]byte, len(shapes))
	for i, shape := range shapes {
		docs[i] = []byte(`{"type":"object","required":["` + shape + `"],"properties":{"` + shape + `":{"type":"string"}},"additionalProperties":false}`)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(docs))
	for i := range docs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Register(ref.ID, ref.Version, docs[i])
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrAlreadyRegistered):
			// rejected — fine
		default:
			t.Fatalf("doc %d: unexpected error %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one winning registration, got %d (errs: %v)", winners, errs)
	}
	// The surviving entry matches the winner's content: the winner's shape
	// validates, the loser's does not.
	s, ok := r.Get(ref)
	if !ok {
		t.Fatal("no entry after raced registration")
	}
	var winnerShape string
	for i, err := range errs {
		if err == nil {
			winnerShape = shapes[i]
		}
	}
	if err := s.Validate([]byte(`{"` + winnerShape + `":"x"}`)); err != nil {
		t.Errorf("winner's shape rejected: %v", err)
	}
	loserShape := "a"
	if winnerShape == "a" {
		loserShape = "b"
	}
	if err := s.Validate([]byte(`{"` + loserShape + `":"y"}`)); err == nil {
		t.Error("loser's shape validates: content was overwritten")
	}
}

// TestConcurrentValidateAndRegister exercises the registry under parallel
// reads and writes (run with -race in CI).
func TestConcurrentValidateAndRegister(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	claim := Ref{ID: canonicalID("claim"), Version: CanonicalV1}
	doc, err := json.Marshal(claimFixture())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if err := r.Validate(claim, doc); err != nil {
					t.Errorf("concurrent Validate: %v", err)
					return
				}
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ref := Ref{ID: fmt.Sprintf("project:acme:concurrent-%d", n), Version: "1"}
			if _, err := r.Register(ref.ID, ref.Version, []byte(`{"type":"object"}`)); err != nil {
				t.Errorf("concurrent Register: %v", err)
			}
		}(i)
	}
	wg.Wait()
}
