package assets

import "testing"

// Origin refs are the mandatory provenance pins of a published asset
// version: kind:value, value = uuid. The tests pin the closed kind set,
// the uuid shape, and the round trip through the canonical text form.

const sampleUUID = "4c9a9e2f-1b6d-4b7a-9c3e-7f5a1d2b3c4d"

func TestNewOriginRefRoundTrip(t *testing.T) {
	for _, kind := range []OriginKind{KindProject, KindRelease, KindState, KindObjectVersion} {
		ref, ok := NewOriginRef(kind, sampleUUID)
		if !ok {
			t.Fatalf("NewOriginRef(%s, uuid) = not ok", kind)
		}
		if !ref.Valid() {
			t.Fatalf("NewOriginRef(%s, uuid) = %q, not valid", kind, ref)
		}
		gotKind, gotValue, ok := ParseOriginRef(string(ref))
		if !ok || gotKind != kind || gotValue != sampleUUID {
			t.Fatalf("ParseOriginRef(%q) = (%q, %q, %v), want (%q, %q, true)", ref, gotKind, gotValue, ok, kind, sampleUUID)
		}
	}
}

func TestNewOriginRefRejectsBadInput(t *testing.T) {
	rejects := []struct {
		kind  OriginKind
		value string
	}{
		{"", sampleUUID},
		{"knowledge", sampleUUID},
		{"PROJECT", sampleUUID},
		{"project ", sampleUUID},
		{KindProject, ""},
		{KindProject, "my-dataset-slug"},                      // slugs are not uuids
		{KindProject, "4c9a9e2f1b6d4b7a9c3e7f5a1d2b3c4d"},     // no dashes
		{KindProject, "4C9A9E2F-1B6D-4B7A-9C3E-7F5A1D2B3C4D"}, // uppercase
		{KindRelease, sampleUUID + ":" + sampleUUID},
	}
	for _, tc := range rejects {
		if ref, ok := NewOriginRef(tc.kind, tc.value); ok {
			t.Errorf("NewOriginRef(%q, %q) = %q accepted, want rejected", tc.kind, tc.value, ref)
		}
	}
}

func TestParseOriginRefRejectsBadShape(t *testing.T) {
	rejects := []string{
		"",
		"project",                      // no kind:value split
		"project:",                     // empty value
		":uuid",                        // empty kind
		sampleUUID,                     // value without kind
		"project:" + sampleUUID + ":x", // extra colon: value is not a uuid
		"unknown:" + sampleUUID,        // unknown kind
		"project:not-a-uuid",           // value shape
		"PROJECT:" + sampleUUID,        // uppercase kind
		"project: " + sampleUUID,       // whitespace kind
	}
	for _, s := range rejects {
		if OriginRef(s).Valid() {
			t.Errorf("OriginRef(%q).Valid() = true, want false", s)
		}
		if _, _, ok := ParseOriginRef(s); ok {
			t.Errorf("ParseOriginRef(%q) ok = true, want false", s)
		}
	}
}

func TestValidOriginRefs(t *testing.T) {
	refs := []string{
		"project:" + sampleUUID,
		"release:" + sampleUUID,
		"object_version:" + sampleUUID,
	}
	if !ValidOriginRefs(refs) {
		t.Fatalf("ValidOriginRefs(%v) = false, want true", refs)
	}
	// The version schema demands minItems 1: empty is unrepresentable.
	if ValidOriginRefs(nil) {
		t.Error("ValidOriginRefs(nil) = true, want false (origin refs are mandatory)")
	}
	if ValidOriginRefs([]string{}) {
		t.Error("ValidOriginRefs([]) = true, want false (origin refs are mandatory)")
	}
	// One bad ref poisons the set.
	if ValidOriginRefs([]string{"project:" + sampleUUID, "project:slug"}) {
		t.Error("ValidOriginRefs with a slug-shaped ref = true, want false")
	}
}
