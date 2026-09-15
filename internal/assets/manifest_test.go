package assets

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task T0702 required test "asset validators": the four manifest shapes'
// required metadata, the manifest document rules, and the publish gate.
// This file covers the document half (required metadata, format version,
// canonical bytes and hash); gate_test.go covers the gate half, and
// tests/integration/asset_manifest_test.go pins the storage half (the
// jsonb constraint, the canonical bytes surviving a jsonb round trip, and
// a real publish refused over a missing provenance/rights/version pin).

// completeMetadata fills every required field of an asset type with a
// shape-valid value.
//
// It is a scaffold, not a fixture: it is built from RequiredMetadata
// rather than written out, so a field added to the table is filled here
// automatically and the tests below keep asking their real question ("is
// THIS refusal about the right field?") instead of failing on a stale
// fixture.
func completeMetadata(assetType Type) Metadata {
	m := Metadata{}
	for _, f := range RequiredMetadata(assetType) {
		m[f.Key] = sampleValue(f)
	}
	return m
}

// sampleValue returns one value of the shape a field declares.
func sampleValue(f MetadataField) any {
	switch f.Kind {
	case KindText:
		return "a value"
	case KindTextList:
		return []any{"a value"}
	case KindTextObject:
		return map[string]any{"a key": "a value"}
	case KindEnum:
		if len(f.Values) > 0 {
			return f.Values[0]
		}
	}
	return nil
}

// manifestFor builds the smallest valid manifest of an asset type: the
// format version, the type, and exactly its required metadata.
func manifestFor(assetType Type) Manifest {
	return Manifest{
		Version:   ManifestFormatVersion,
		AssetType: assetType,
		Metadata:  completeMetadata(assetType),
	}
}

// mustValidationErrors unwraps the joined error every validator here
// returns into the flat list of refusals. It fails when there is no error
// at all: every call site is asserting that something WAS refused, so a
// nil here is a broken fixture, not a passing case.
func mustValidationErrors(t *testing.T, err error) []*ValidationError {
	t.Helper()
	if err == nil {
		t.Fatalf("expected a validation error, got nil")
	}
	return validationErrors(err)
}

// validationErrors flattens a joined validation error into its parts.
// Every element must be a *ValidationError: a bare errors.New from this
// package would be a refusal a caller cannot match on.
func validationErrors(err error) []*ValidationError {
	var out []*ValidationError
	for _, e := range flattenErrors(err) {
		ve, ok := e.(*ValidationError)
		if !ok {
			panic("joined error holds a non-validation error: " + e.Error())
		}
		out = append(out, ve)
	}
	return out
}

// flattenErrors unpacks a joined error into its leaves.
func flattenErrors(err error) []error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range joined.Unwrap() {
			out = append(out, flattenErrors(e)...)
		}
		return out
	}
	return []error{err}
}

// samplePID is a well-formed pid for fixtures: 26 characters of the
// Crockford alphabet, the shape NewPID produces.
const samplePID PID = "01j9z6k3m4n5p6q7r8s9t0v1w2"

// fieldsOf returns the field paths of the errors, in order.
func fieldsOf(errs []*ValidationError) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Field)
	}
	return out
}

// codesOf returns the codes of the errors, in order.
func codesOf(errs []*ValidationError) []string {
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Code)
	}
	return out
}

// TestRequiredMetadataTableIsWellFormed checks the table itself: every V1
// type has requirements, the keys are unique within a type, every kind is
// one of the four defined shapes, and only an enum carries a vocabulary.
//
// The last one matters more than it looks: a KindTextList field carrying
// Values would be a vocabulary nothing checks, and a KindEnum field
// without Values could never be satisfied by anything.
func TestRequiredMetadataTableIsWellFormed(t *testing.T) {
	knownKinds := map[MetadataKind]bool{KindText: true, KindTextList: true, KindTextObject: true, KindEnum: true}
	for _, assetType := range AllTypes() {
		fields := RequiredMetadata(assetType)
		if len(fields) == 0 {
			t.Errorf("%s: no required metadata; every asset type must declare what a published version has to say", assetType)
			continue
		}
		seen := map[string]bool{}
		for _, f := range fields {
			if f.Key == "" {
				t.Errorf("%s: a required field has no key", assetType)
			}
			if seen[f.Key] {
				t.Errorf("%s: %q is required twice", assetType, f.Key)
			}
			seen[f.Key] = true
			if !knownKinds[f.Kind] {
				t.Errorf("%s.%s: kind %q is not one of the four defined shapes", assetType, f.Key, f.Kind)
			}
			switch {
			case f.Kind == KindEnum && len(f.Values) == 0:
				t.Errorf("%s.%s: an enum field with no values cannot be satisfied", assetType, f.Key)
			case f.Kind != KindEnum && len(f.Values) > 0:
				t.Errorf("%s.%s: %v is a vocabulary for a %s field, where nothing reads it", assetType, f.Key, f.Values, f.Kind)
			}
		}
		// The one field docs/08 gives both listed object types: a published
		// asset has to state why it exists.
		if !seen["purpose"] {
			t.Errorf("%s: purpose is not required; docs/08 requires it of every object type", assetType)
		}
	}
}

// TestRequiredMetadataTableContentIsPinned is the literal pin of the four
// manifest shapes. Every other test in this file derives its expectations
// FROM the table (completeMetadata fills it, the missing-field test walks
// it), so they cannot see a field quietly disappear: a table with fewer
// requirements is self-consistently satisfiable, and self-consistently
// testable. This test is what makes a removal visible — it has to be
// edited on purpose, in the same commit that changes the requirement.
//
// The dataset and protocol rows are the fields docs/08 gives those two
// object types, named as specs/schemas/dataset.schema.json and
// protocol.schema.json name them (pinned by
// TestDatasetAndProtocolMetadataMatchesTheObjectSchemas below).
// material_collection and benchmark have no object type behind them in
// V1; their rows are this package's choice, and this is where that choice
// is written down.
func TestRequiredMetadataTableContentIsPinned(t *testing.T) {
	want := map[Type][]metadataExpectation{
		TypeDataset: {
			{"purpose", KindText, nil},
			{"data_type", KindText, nil},
			{"blob_ids", KindTextList, nil},
			{"access_level", KindEnum, []string{"open", "restricted"}},
			{"quality_notes", KindText, nil},
		},
		TypeProtocol: {
			{"purpose", KindText, nil},
			{"domain", KindText, nil},
			{"steps", KindTextList, nil},
			{"parameters", KindTextObject, nil},
			{"requirements", KindTextList, nil},
		},
		TypeMaterialCollection: {
			{"purpose", KindText, nil},
			{"member_refs", KindTextList, nil},
			{"selection_criteria", KindText, nil},
			{"custodian", KindText, nil},
		},
		TypeBenchmark: {
			{"purpose", KindText, nil},
			{"task", KindText, nil},
			{"metrics", KindTextList, nil},
			{"dataset_refs", KindTextList, nil},
		},
	}
	for _, assetType := range AllTypes() {
		expect, ok := want[assetType]
		if !ok {
			t.Fatalf("%s has required metadata but no expectation here: a new asset type is a deliberate addition, not a silent one", assetType)
		}
		got := RequiredMetadata(assetType)
		if len(got) != len(expect) {
			t.Errorf("%s requires %d fields %v, want %d %v", assetType, len(got), keysOf(got), len(expect), keysOfExpectations(expect))
			continue
		}
		for i, e := range expect {
			if got[i].Key != e.key || got[i].Kind != e.kind || strings.Join(got[i].Values, "|") != strings.Join(e.values, "|") {
				t.Errorf("%s field %d = %s (%s %v), want %s (%s %v)",
					assetType, i, got[i].Key, got[i].Kind, got[i].Values, e.key, e.kind, e.values)
			}
		}
	}
	if len(want) != len(AllTypes()) {
		t.Errorf("this test pins %d types, the V1 set has %d", len(want), len(AllTypes()))
	}
}

func keysOf(fields []MetadataField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Key)
	}
	return out
}

// metadataExpectation is the literal form of one required field, declared
// here rather than reusing MetadataField so the pin above cannot become a
// comparison of the value under test with itself.
type metadataExpectation struct {
	key    string
	kind   MetadataKind
	values []string
}

// keysOfExpectations is keysOf for the literal expectation table.
func keysOfExpectations(rows []metadataExpectation) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.key)
	}
	return out
}

// TestRequiredMetadataReturnsACopy pins the accessor: a caller that sorts
// or edits the returned slice (a form renderer, an error list) must not be
// able to reach back into the table and change what every later validation
// demands.
func TestRequiredMetadataReturnsACopy(t *testing.T) {
	before := RequiredMetadata(TypeDataset)
	got := RequiredMetadata(TypeDataset)
	got[0].Key = "tampered"
	got[0].Kind = KindEnum
	got[0].Values = append(got[0].Values, "tampered")
	if each := RequiredMetadata(TypeDataset); each[0].Key != before[0].Key || each[0].Kind != before[0].Kind {
		t.Fatalf("RequiredMetadata is not a copy: second call = %+v, want %+v", each[0], before[0])
	}
	enumFields := RequiredMetadata(TypeDataset)
	for _, f := range enumFields {
		if f.Kind != KindEnum {
			continue
		}
		if strings.Contains(strings.Join(f.Values, ","), "tampered") {
			t.Fatalf("%s: the values of an enum field were mutated through an earlier call: %v", f.Key, f.Values)
		}
	}
}

// TestManifestAcceptsEachTypesRequiredMetadata proves the table is
// satisfiable: for every one of the four types, a document carrying
// exactly the required fields — nothing more — validates and parses.
//
// Without this, TestManifestRefusesEachMissingRequiredField below would
// pass for a table that no document could ever satisfy.
func TestManifestAcceptsEachTypesRequiredMetadata(t *testing.T) {
	for _, assetType := range AllTypes() {
		m := manifestFor(assetType)
		if err := m.Validate(); err != nil {
			t.Errorf("%s: a manifest with exactly the required metadata must validate: %v", assetType, err)
		}
		raw, err := m.CanonicalJSON()
		if err != nil {
			t.Fatalf("%s: canonical JSON: %v", assetType, err)
		}
		if _, err := ParseManifest(raw); err != nil {
			t.Errorf("%s: the canonical bytes of a valid manifest must parse: %v", assetType, err)
		}
	}
}

// TestManifestRefusesEachMissingRequiredField is the core of "required
// metadata": every required field of every type, removed one at a time,
// must produce exactly one refusal naming that field.
//
// Removing one field at a time (rather than emptying the block) is what
// makes the test able to fail for the right reason: it would catch a
// field that is demanded in the table but never actually checked, and it
// would catch a check that refuses a neighbouring field's absence instead.
func TestManifestRefusesEachMissingRequiredField(t *testing.T) {
	for _, assetType := range AllTypes() {
		for _, f := range RequiredMetadata(assetType) {
			// absent
			absent := manifestFor(assetType)
			delete(absent.Metadata, f.Key)
			errs := mustValidationErrors(t, absent.Validate())
			if len(errs) != 1 {
				t.Fatalf("%s: removing %q produced %d refusals %v, want exactly 1",
					assetType, f.Key, len(errs), codesOf(errs))
			}
			if errs[0].Code != CodeMissingMetadata || errs[0].Field != metadataPath(f.Key) {
				t.Errorf("%s: removing %q = %s (code %s), want %s at %s",
					assetType, f.Key, errs[0].Error(), errs[0].Code, CodeMissingMetadata, metadataPath(f.Key))
			}

			// present as null: a declaration that declares nothing
			nulled := manifestFor(assetType)
			nulled.Metadata[f.Key] = nil
			errs = mustValidationErrors(t, nulled.Validate())
			if len(errs) != 1 || errs[0].Code != CodeMissingMetadata || errs[0].Field != metadataPath(f.Key) {
				t.Errorf("%s: %q = null = %v, want one %s at %s",
					assetType, f.Key, codesOf(errs), CodeMissingMetadata, metadataPath(f.Key))
			}
		}
	}
}

// TestManifestReportsEveryMissingFieldInOnePass pins the one-pass
// reporting rule: an empty metadata block reports every required key of
// the type, not the first one — a publisher fixing a manifest sees the
// whole list, and a caller can render it as a form's field errors.
func TestManifestReportsEveryMissingFieldInOnePass(t *testing.T) {
	for _, assetType := range AllTypes() {
		m := manifestFor(assetType)
		m.Metadata = Metadata{}
		errs := mustValidationErrors(t, m.Validate())
		want := RequiredMetadata(assetType)
		if len(errs) != len(want) {
			t.Fatalf("%s: empty metadata produced %d refusals %v, want one per required field (%d)",
				assetType, len(errs), codesOf(errs), len(want))
		}
		got := strings.Join(fieldsOf(errs), ",")
		var paths []string
		for _, f := range want {
			paths = append(paths, metadataPath(f.Key))
		}
		if got != strings.Join(paths, ",") {
			t.Errorf("%s: refusals name %s, want %s (declaration order)", assetType, got, strings.Join(paths, ","))
		}
		for _, e := range errs {
			if e.Code != CodeMissingMetadata {
				t.Errorf("%s: %s has code %s, want %s", assetType, e.Field, e.Code, CodeMissingMetadata)
			}
		}
	}
}

// TestManifestRefusesWronglyShapedMetadataValue walks the value shapes a
// required field must NOT accept, through the decoder rather than through
// Go structs — these are the bytes a client actually sends.
//
// Every case declares the other required fields of its type correctly, so
// the refusal it produces is about the value under test and nothing else:
// a document whose metadata is complete except for this one field must be
// refused for this one field.
func TestManifestRefusesWronglyShapedMetadataValue(t *testing.T) {
	cases := []struct {
		assetType Type
		field     string
		value     string
		reason    string
		// code is the refusal expected; empty means
		// CodeInvalidMetadataValue. Only the null case differs: a null is
		// refused by the MISSING rule, because "the key is there" is not a
		// declaration (validateMetadata treats null as absent).
		code string
	}{
		{TypeDataset, "purpose", `""`, "a blank declaration declares nothing", ""},
		{TypeDataset, "purpose", `"   "`, "whitespace is not a declaration", ""},
		{TypeDataset, "purpose", `300`, "numbers do not survive the jsonb round trip the hash depends on", ""},
		{TypeDataset, "purpose", `true`, "a boolean is not a statement", ""},
		{TypeDataset, "purpose", `null`, "null is the absent key, spelled to look present", CodeMissingMetadata},
		{TypeDataset, "purpose", `["a"]`, "the field declares a single string", ""},
		{TypeDataset, "purpose", `{"a":"b"}`, "the field declares a single string", ""},
		{TypeDataset, "blob_ids", `[]`, "a list with no members names nothing to fetch", ""},
		{TypeDataset, "blob_ids", `[1,2]`, "blob ids are references, not numbers", ""},
		{TypeDataset, "blob_ids", `["a",2]`, "one non-string member makes the list unusable", ""},
		{TypeDataset, "blob_ids", `["a",""]`, "a blank member names nothing", ""},
		{TypeDataset, "blob_ids", `["a",null]`, "null is not a member", ""},
		{TypeDataset, "blob_ids", `"a"`, "the field declares a list", ""},
		{TypeDataset, "access_level", `"closed"`, "access_level is open|restricted", ""},
		{TypeDataset, "access_level", `"Open"`, "the vocabulary is lower case", ""},
		{TypeDataset, "access_level", `1`, "access_level is a string from its vocabulary", ""},
		{TypeProtocol, "parameters", `{}`, "an empty parameter block declares no parameters", ""},
		{TypeProtocol, "parameters", `{"temperature":300}`, "parameter values stay text the hash can cover", ""},
		{TypeProtocol, "parameters", `{"flag":true}`, "parameter values stay text", ""},
		{TypeProtocol, "parameters", `{"block":{"a":"b"}}`, "the canonical form holds one level", ""},
		{TypeProtocol, "parameters", `{"block":[]}`, "an empty list declares nothing", ""},
		{TypeProtocol, "parameters", `"temperature=300"`, "the field declares an object", ""},
	}
	for _, c := range cases {
		t.Run(string(c.assetType)+"."+c.field+" = "+c.value, func(t *testing.T) {
			metadata := map[string]any{}
			for key, value := range completeMetadata(c.assetType) {
				metadata[key] = value
			}
			var bad any
			if err := json.Unmarshal([]byte(c.value), &bad); err != nil {
				t.Fatalf("fixture: %s is not JSON: %v", c.value, err)
			}
			metadata[c.field] = bad

			raw, err := json.Marshal(map[string]any{
				"version":    ManifestFormatVersion,
				"asset_type": string(c.assetType),
				"metadata":   metadata,
			})
			if err != nil {
				t.Fatalf("build document: %v", err)
			}
			_, perr := ParseManifest(raw)
			errs := mustValidationErrors(t, perr)
			if len(errs) != 1 {
				t.Fatalf("%s = %s produced %d refusals %v, want exactly 1", c.field, c.value, len(errs), codesOf(errs))
			}
			got := errs[0]
			if got.Field != metadataPath(c.field) {
				t.Errorf("%s = %s names field %q, want %q", c.field, c.value, got.Field, metadataPath(c.field))
			}
			wantCode := c.code
			if wantCode == "" {
				wantCode = CodeInvalidMetadataValue
			}
			if got.Code != wantCode {
				t.Errorf("%s = %s has code %s, want %s (%s)", c.field, c.value, got.Code, wantCode, c.reason)
			}
		})
	}
}

// TestManifestRefusesUnknownTopLevelFieldAndTrailingContent pins the
// fail-closed reader: a field this package does not know and content after
// the document are both refused, because a reader that drops them hands
// back a document that still validates while missing a declaration.
func TestManifestRefusesUnknownTopLevelFieldAndTrailingContent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"unknown field", `{"version":1,"asset_type":"benchmark","metadata":{},"licence":"MIT"}`},
		{"second document", `{"version":1,"asset_type":"benchmark","metadata":{}}{"version":1,"asset_type":"benchmark","metadata":{}}`},
		{"trailing scalar", `{"version":1,"asset_type":"benchmark","metadata":{}} 1`},
		{"array document", `[]`},
		{"string document", `"a manifest"`},
		{"truncated", `{"version":1,"asset_type":"benchmark"`},
		{"empty bytes", ``},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(c.raw))
			errs := mustValidationErrors(t, err)
			if len(errs) != 1 || errs[0].Code != CodeMalformedManifest {
				t.Fatalf("ParseManifest(%s) = %v, want one %s", c.raw, codesOf(errs), CodeMalformedManifest)
			}
		})
	}
}

// TestManifestFormatVersionIsClosed pins the version rule: only the
// version this package reads is accepted, and any other one is refused
// rather than read as if it were that one.
func TestManifestFormatVersionIsClosed(t *testing.T) {
	for _, version := range []int{0, 2, 99, -1} {
		m := manifestFor(TypeBenchmark)
		m.Version = version
		errs := mustValidationErrors(t, m.Validate())
		if len(errs) != 1 || errs[0].Code != CodeUnsupportedManifestVersion || errs[0].Field != "version" {
			t.Errorf("version %d = %v, want one %s at version", version, codesOf(errs), CodeUnsupportedManifestVersion)
		}
	}
	// An absent version decodes to 0 and takes the same refusal — the zero
	// value is not a version this platform reads.
	if _, err := ParseManifest([]byte(`{"asset_type":"benchmark","metadata":{}}`)); err == nil {
		t.Error("a document with no version must be refused")
	}
	// The accepted version is the exported constant, not a literal.
	if ManifestFormatVersion != 1 {
		t.Errorf("ManifestFormatVersion = %d; changing the accepted format version is a format migration, not a constant edit", ManifestFormatVersion)
	}
}

// TestManifestRefusesUnknownAssetType pins the closed type set at the
// document level.
func TestManifestRefusesUnknownAssetType(t *testing.T) {
	for _, ty := range []string{"", "Dataset", "DATASET", "model", "asset", "material collection"} {
		m := manifestFor(TypeDataset)
		m.AssetType = Type(ty)
		errs := mustValidationErrors(t, m.Validate())
		if len(errs) != 1 || errs[0].Code != CodeUnknownAssetType || errs[0].Field != "asset_type" {
			t.Errorf("asset_type %q = %v, want one %s at asset_type", ty, codesOf(errs), CodeUnknownAssetType)
		}
	}
}

// TestManifestExtraMetadataKeysAreAllowedButShapeChecked pins the floor
// rule: a publisher may declare more than the required set (so it cannot
// be a closed set), and the extra keys are held to the same value shapes
// (so the document stays one the canonical form and the hash can cover).
func TestManifestExtraMetadataKeysAreAllowedButShapeChecked(t *testing.T) {
	m := manifestFor(TypeDataset)
	m.Metadata["instrument_id"] = "TPS-2026-07"
	m.Metadata["wavelengths"] = []any{"0.5 A", "1.0 A"}
	m.Metadata["calibration"] = map[string]any{"reference": "SRM-660c"}
	if err := m.Validate(); err != nil {
		t.Fatalf("extra metadata of a supported shape must be allowed: %v", err)
	}

	bad := []struct {
		value any
		why   string
	}{
		{nil, "null is the absent key, spelled to look present"},
		{3.5, "a number does not survive the jsonb round trip the hash depends on"},
		{true, "a boolean is not a statement"},
		{[]any{}, "an empty list declares nothing"},
		{map[string]any{}, "an empty object declares nothing"},
		{map[string]any{"inner": map[string]any{"deeper": "x"}}, "the canonical form holds one level"},
	}
	for _, c := range bad {
		withBad := manifestFor(TypeDataset)
		withBad.Metadata["instrument_id"] = c.value
		errs := mustValidationErrors(t, withBad.Validate())
		if len(errs) != 1 {
			t.Fatalf("extra key = %#v produced %d refusals %v, want 1 (%s)", c.value, len(errs), codesOf(errs), c.why)
		}
		if errs[0].Code != CodeInvalidMetadataValue || errs[0].Field != metadataPath("instrument_id") {
			t.Errorf("extra key = %#v: %s, want %s at %s", c.value, errs[0].Error(), CodeInvalidMetadataValue, metadataPath("instrument_id"))
		}
	}
}

// TestManifestCanonicalJSONIsIndependentOfConstruction pins the bytes the
// integrity hash covers: two documents built differently (map insertion
// order, nil vs empty collections, decoded vs constructed) render to the
// same canonical bytes, and the bytes parse back to the same bytes.
//
// This is what makes the strict hash rule (gate.go: the integrity hash
// must equal the canonical manifest digest) usable: a verifier that reads
// the stored document and re-renders it computes the publisher's digest,
// so a hash that covers the manifest is checkable without the original
// bytes.
func TestManifestCanonicalJSONIsIndependentOfConstruction(t *testing.T) {
	built := manifestFor(TypeProtocol)
	built.Metadata["equipment"] = map[string]any{"furnace": "tube", "balance": "analytical"}

	// The same document, built in another order, with an explicit empty
	// dependency list instead of the nil one.
	other := Manifest{
		Version:   built.Version,
		AssetType: built.AssetType,
		Metadata:  Metadata{},
		// deliberately built in the reverse order of `built`
	}
	for _, key := range []string{"requirements", "parameters", "steps", "domain", "purpose"} {
		other.Metadata[key] = built.Metadata[key]
	}
	other.Metadata["equipment"] = map[string]any{"balance": "analytical", "furnace": "tube"}
	other.DependencyPins = []DependencyPin{}

	first, err := built.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	second, err := other.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("canonical bytes depend on how the document was built:\n%s\n%s", first, second)
	}

	// Metadata keys are sorted, so the bytes are stable across re-render.
	if !strings.Contains(string(first), `"equipment":{"balance":"analytical","furnace":"tube"}`) {
		t.Errorf("nested metadata keys are not in canonical order: %s", first)
	}
	if !strings.Contains(string(first), `"dependency_pins":[]`) {
		t.Errorf("a nil dependency list must render as the empty list it means: %s", first)
	}

	// Idempotence: parsing the canonical bytes and re-rendering gives the
	// same bytes, which is exactly what a verifier does.
	reparsed, err := ParseManifest(first)
	if err != nil {
		t.Fatalf("the canonical bytes of a valid manifest must parse: %v", err)
	}
	again, err := reparsed.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical JSON of the reparsed document: %v", err)
	}
	if string(again) != string(first) {
		t.Errorf("canonical bytes are not idempotent:\n%s\n%s", first, again)
	}

	// Hash and ManifestHash agree, and both are the digest of those bytes.
	h, err := built.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if h != sha256Hex(first) {
		t.Errorf("Hash = %s, want the sha256 hex digest of the canonical bytes", h)
	}
	fromRaw, err := ManifestHash(first)
	if err != nil {
		t.Fatalf("ManifestHash: %v", err)
	}
	if fromRaw != h {
		t.Errorf("ManifestHash(stored bytes) = %s, Hash = %s — a verifier must derive the publisher's digest", fromRaw, h)
	}
	if !isSHA256Hex(h) {
		t.Errorf("Hash = %q, want 64 lowercase hex characters", h)
	}
	// A different document hashes differently — the property the gate's
	// strict hash rule rests on.
	changed := built
	changed.Metadata = completeMetadata(TypeProtocol)
	changed.Metadata["domain"] = "materials"
	changedHash, err := changed.Hash()
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	if changedHash == h {
		t.Error("two different documents hash the same; the integrity hash would not cover the content")
	}
}

// TestManifestRefusesPinsByDocumentRules pins that the document-level pin
// rules travel with the manifest (the self-pin rule needs the version's
// identity and lives in the gate instead).
func TestManifestRefusesPinsByDocumentRules(t *testing.T) {
	pin, ok := NewDependencyPin(samplePID, "1.0")
	if !ok {
		t.Fatal("fixture: sample pid/version must form a pin")
	}

	m := manifestFor(TypeProtocol)
	m.DependencyPins = []DependencyPin{pin, pin}
	errs := mustValidationErrors(t, m.Validate())
	if len(errs) != 1 || errs[0].Code != CodeDuplicateDependencyPin || errs[0].Field != "dependency_pins[1]" {
		t.Errorf("a repeated pin = %v, want one %s at dependency_pins[1]", codesOf(errs), CodeDuplicateDependencyPin)
	}

	m = manifestFor(TypeProtocol)
	m.DependencyPins = []DependencyPin{"not-a-pin", ""}
	errs = mustValidationErrors(t, m.Validate())
	if len(errs) != 2 {
		t.Fatalf("two malformed pins produced %d refusals %v, want 2", len(errs), codesOf(errs))
	}
	for i, e := range errs {
		if e.Code != CodeInvalidDependencyPin || e.Field != dependencyPinPath(i) {
			t.Errorf("malformed pin %d: %s at %s, want %s", i, e.Code, e.Field, CodeInvalidDependencyPin)
		}
	}
}

// TestDatasetAndProtocolMetadataMatchesTheObjectSchemas pins the two
// types whose metadata is not this package's to invent: every key
// required for dataset and protocol must be a property of the
// corresponding scientific-object schema, and the access_level
// vocabulary must be that schema's enum.
//
// The schema files are read from specs/schemas (the canonical copies),
// so this test fails if either side moves: it is the drift alarm the
// "quoted from the specification" claim in manifest.go otherwise rests on.
func TestDatasetAndProtocolMetadataMatchesTheObjectSchemas(t *testing.T) {
	cases := []struct {
		assetType Type
		schema    string
	}{
		{TypeDataset, "dataset.schema.json"},
		{TypeProtocol, "protocol.schema.json"},
	}
	for _, c := range cases {
		path := filepath.Join("..", "..", "specs", "schemas", c.schema)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var doc struct {
			Required   []string `json:"required"`
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(doc.Properties) == 0 {
			t.Fatalf("%s: no properties — the schema moved and this test would pass vacuously", path)
		}
		// docs/08 requires purpose of both object types, and the two
		// schemas say so themselves. Pinning it in the schema's own
		// required list is what keeps the table below from being read as
		// "whatever this package felt like requiring": the one field the
		// specification mandates for both types is the one field the table
		// cannot drop without failing here.
		if !contains(doc.Required, "purpose") {
			t.Errorf("%s no longer requires purpose; the required metadata table quotes docs/08, and this test exists to notice when the specification moves", path)
		}
		for _, f := range RequiredMetadata(c.assetType) {
			prop, ok := doc.Properties[f.Key]
			if !ok {
				t.Errorf("%s requires %q, but %s has no such property: the required metadata must be the object schema's own field names",
					c.assetType, f.Key, c.schema)
				continue
			}
			if f.Kind != KindEnum {
				continue
			}
			if strings.Join(prop.Enum, "|") != strings.Join(f.Values, "|") {
				t.Errorf("%s.%s vocabulary = %v, but %s declares %v — the two must be the same set in the same order",
					c.assetType, f.Key, f.Values, c.schema, prop.Enum)
			}
		}
	}
}
