// T0703 required test "rights tests" — the Go half of the rights model
// (docs/12 §4, ADR-009, specs/policies/rights-template.yaml).
//
// What is pinned here, and where the other half lives:
//
//   - the acceptance criterion 可表达 commercial/derivative/redistribution/
//     model training/attribution: every axis is expressible, every value of
//     every vocabulary round-trips through the canonical JSON, and the
//     five declarations the criterion names are the template's.
//   - the document shape is the template's: field names, the null-vs-value
//     encoding, and the struct order a marshalled document reads in.
//   - the validation rules, each one attributed to the rule that refused it
//     (a case that could be refused by a neighbouring rule does not count).
//   - what the model deliberately does NOT decide (a well-formed but
//     unknown license id, an agreement ref that resolves to nothing).
//
// The storage half — that both rights_json columns hold a JSON object and
// nothing more — is pinned against a real PostgreSQL in
// tests/integration/rights_model_test.go.

package rights

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// templateDocument is specs/policies/rights-template.yaml written as a
// JSON right-hand side, field for field, with its default values.
// TestNewMatchesRightsTemplate is the one test that asserts against it:
// New() produces exactly this document and Marshal emits exactly this JSON,
// so the Go model and the spec file cannot drift apart without a test
// naming the other side. TestParseRefusesBytesItCannotRead also uses it, as
// a document that IS well-formed, to show that Parse still refuses it once
// something is appended to it.
const templateDocument = `{"version":1,"standard_license_id":null,"custom_agreement_ref":null,` +
	`"usage":{"commercial_use":"unspecified","derivatives":"unspecified","redistribution":"unspecified",` +
	`"model_training":"unspecified","attribution":"required","patent_grant":"none"},` +
	`"visibility":{"metadata":"project_policy","data_access":"restricted"},"notes":null}`

func TestNewMatchesRightsTemplate(t *testing.T) {
	raw, err := New().Marshal()
	if err != nil {
		t.Fatalf("New().Marshal() = %v, want nil error", err)
	}
	if string(raw) != templateDocument {
		t.Errorf("New() = %s\nwant  %s", raw, templateDocument)
	}
	if err := New().Validate(); err != nil {
		t.Errorf("New().Validate() = %v, want nil", err)
	}
}

func TestDocumentRoundTrips(t *testing.T) {
	lic := "CC-BY-4.0"
	ref := "https://example.org/agreements/mof-2026.pdf"
	notes := "Covers the 2026 release only."
	doc := New()
	doc.StandardLicenseID = &lic
	doc.CustomAgreementRef = &ref
	doc.Usage.CommercialUse = PermissionRestricted
	doc.Usage.Derivatives = PermissionAllowed
	doc.Usage.Redistribution = PermissionRestricted
	doc.Usage.ModelTraining = PermissionRestricted
	doc.Usage.Attribution = AttributionRequired
	doc.Usage.PatentGrant = PatentGrantSeeAgreement
	doc.Visibility.Metadata = MetadataProjectPolicy
	doc.Visibility.DataAccess = DataAccessRestricted
	doc.Notes = &notes

	raw, err := doc.Marshal()
	if err != nil {
		t.Fatalf("Marshal() = %v, want nil error", err)
	}
	back, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%s) = %v, want nil error", raw, err)
	}
	if !reflect.DeepEqual(back, doc) {
		t.Errorf("round trip = %+v\nwant %+v", back, doc)
	}
	// The canonical form is stable: parsing and re-marshalling is a fixed
	// point, which is what makes the stored bytes comparable.
	again, err := back.Marshal()
	if err != nil {
		t.Fatalf("second Marshal() = %v, want nil error", err)
	}
	if string(again) != string(raw) {
		t.Errorf("re-marshal = %s\nwant      %s", again, raw)
	}
}

// TestEveryUsageAxisIsExpressible is the acceptance criterion itself: each
// of the five declarations (commercial, derivative, redistribution, model
// training, attribution) can be set to each value of its vocabulary, and
// the value survives the canonical encoding.
func TestEveryUsageAxisIsExpressible(t *testing.T) {
	for _, p := range AllPermissions() {
		for _, set := range []struct {
			field string
			apply func(*Document, Permission)
		}{
			{"usage.commercial_use", func(d *Document, p Permission) { d.Usage.CommercialUse = p }},
			{"usage.derivatives", func(d *Document, p Permission) { d.Usage.Derivatives = p }},
			{"usage.redistribution", func(d *Document, p Permission) { d.Usage.Redistribution = p }},
			{"usage.model_training", func(d *Document, p Permission) { d.Usage.ModelTraining = p }},
		} {
			doc := New()
			set.apply(&doc, p)
			raw, err := doc.Marshal()
			if err != nil {
				t.Errorf("%s = %q: Marshal() = %v, want nil", set.field, p, err)
				continue
			}
			back, err := Parse(raw)
			if err != nil {
				t.Errorf("%s = %q: Parse(%s) = %v, want nil", set.field, p, raw, err)
				continue
			}
			if got := fieldOf(back, set.field); got != string(p) {
				t.Errorf("%s round trip = %q, want %q", set.field, got, p)
			}
		}
	}

	for _, a := range AllAttributions() {
		doc := New()
		doc.Usage.Attribution = a
		raw, err := doc.Marshal()
		if err != nil {
			t.Errorf("usage.attribution = %q: Marshal() = %v, want nil", a, err)
			continue
		}
		back, err := Parse(raw)
		if err != nil {
			t.Errorf("usage.attribution = %q: Parse(%s) = %v, want nil", a, raw, err)
			continue
		}
		if got := fieldOf(back, "usage.attribution"); got != string(a) {
			t.Errorf("usage.attribution round trip = %q, want %q", got, a)
		}
	}
}

// TestPatentGrantSeeAgreementNeedsTheAgreement pins the one cross-field
// rule, in both directions: the pair is refused with the ref absent, and
// accepted — with the same values otherwise — once a ref is named.
func TestPatentGrantSeeAgreementNeedsTheAgreement(t *testing.T) {
	doc := New()
	doc.Usage.PatentGrant = PatentGrantSeeAgreement
	err := doc.Validate()
	if code := codeOf(t, err); code != CodePatentGrantWithoutAgreement {
		t.Errorf("Validate() code = %q, want %q", code, CodePatentGrantWithoutAgreement)
	}
	if _, err := doc.Marshal(); err == nil {
		t.Error("Marshal() of a document with see_agreement and no ref succeeded, want a validation error")
	}

	ref := "agreements/mof-2026.pdf"
	doc.CustomAgreementRef = &ref
	if err := doc.Validate(); err != nil {
		t.Errorf("Validate() with the agreement named = %v, want nil", err)
	}
}

// TestValidateReportsEveryBrokenRule checks that a document broken in
// several places reports all of them in one pass (a caller fixing a form
// should not need one round trip per field), and that each reported code
// belongs to its own field.
func TestValidateReportsEveryBrokenRule(t *testing.T) {
	empty := ""
	long := strings.Repeat("x", MaxNotesLen+1)
	doc := Document{
		Version:            2,
		StandardLicenseID:  &empty,
		CustomAgreementRef: &empty,
		Usage: Usage{
			CommercialUse:  "yes",
			Derivatives:    PermissionAllowed,
			Redistribution: "no",
			ModelTraining:  PermissionUnspecified,
			Attribution:    "maybe",
			PatentGrant:    "sometimes",
		},
		Visibility: Visibility{Metadata: "", DataAccess: "private"},
		Notes:      &long,
	}
	err := doc.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want the broken rules reported")
	}
	want := map[string]string{
		"version":                CodeUnsupportedVersion,
		"standard_license_id":    CodeInvalidLicenseID,
		"custom_agreement_ref":   CodeInvalidAgreementRef,
		"usage.commercial_use":   CodeInvalidUsageValue,
		"usage.redistribution":   CodeInvalidUsageValue,
		"usage.attribution":      CodeInvalidUsageValue,
		"usage.patent_grant":     CodeInvalidUsageValue,
		"visibility.metadata":    CodeInvalidMetadataVisibility,
		"visibility.data_access": CodeInvalidDataAccess,
		"notes":                  CodeNotesTooLong,
	}
	got := map[string]string{}
	for _, e := range flatten(err) {
		got[e.Field] = e.Code
	}
	if len(got) != len(want) {
		t.Errorf("reported %d fields (%v), want %d (%v)", len(got), got, len(want), want)
	}
	for field, code := range want {
		if got[field] != code {
			t.Errorf("%s reported as %q, want %q", field, got[field], code)
		}
	}
	// The fields that were fine must not be reported at all: a rule that
	// fires on valid input is as wrong as one that never fires.
	if _, reported := got["usage.derivatives"]; reported {
		t.Error("usage.derivatives = allowed was reported, want it left alone")
	}
	if _, reported := got["usage.model_training"]; reported {
		t.Error("usage.model_training = unspecified was reported, want it left alone")
	}
}

// TestParseRefusesBytesItCannotRead pins the reader against inputs that
// are not one rights document at all — bad syntax, a JSON value of the
// wrong type, a field this format does not have, content after the
// document. Every one of them can physically reach the column (or a
// caller), and each must be refused rather than partly read.
func TestParseRefusesBytesItCannotRead(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		code string
	}{
		{"empty bytes", "", CodeMalformedDocument},
		{"not json", "MIT", CodeMalformedDocument},
		{"json array", "[]", CodeMalformedDocument},
		{"json string", `"MIT"`, CodeMalformedDocument},
		{"json number", "7", CodeMalformedDocument},
		{"unknown top-level field", `{"version":1,"embargo":"2027-01-01"}`, CodeMalformedDocument},
		{"unknown usage field", `{"version":1,"usage":{"commercial_use":"allowed","ai_training":"allowed"}}`, CodeMalformedDocument},
		{"version as string", `{"version":"1"}`, CodeMalformedDocument},
		{"trailing content", templateDocument + "{}", CodeMalformedDocument},
		// The two documents with nothing in them: the empty object states
		// no version, and JSON null decodes to the zero Document.
		{"empty object", "{}", CodeUnsupportedVersion},
		{"json null", "null", CodeUnsupportedVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := Parse([]byte(tc.raw))
			if err == nil {
				t.Fatalf("Parse(%s) = %+v, want %s", tc.raw, doc, tc.code)
			}
			if code := codeOf(t, err); code != tc.code {
				t.Errorf("Parse(%s) code = %q, want %q (detail: %v)", tc.raw, code, tc.code, err)
			}
		})
	}
}

// TestParseRefusesBrokenDeclarations is the rule-attribution half: each
// case starts from the valid template document and breaks exactly ONE
// field, so the refusal is attributable. The assertion is that the broken
// field is reported with the expected code AND that nothing else is — a
// rule that also fires on an untouched field fails here, which is the
// defect this test exists to catch (a neighbouring rule refusing a case
// would otherwise let the intended rule be deleted with the suite green).
//
// The bytes are produced by encoding the mutated struct, not by
// Document.Marshal: Marshal validates first, which is exactly the refusal
// these cases need to see from Parse.
func TestParseRefusesBrokenDeclarations(t *testing.T) {
	long := strings.Repeat("x", MaxNotesLen+1)
	empty := ""
	cases := []struct {
		name   string
		mutate func(*Document)
		field  string
		code   string
	}{
		{"version the platform does not read", func(d *Document) { d.Version = 2 },
			"version", CodeUnsupportedVersion},
		{"empty license id", func(d *Document) { d.StandardLicenseID = &empty },
			"standard_license_id", CodeInvalidLicenseID},
		{"license id with a character no id is spelled with", func(d *Document) { s := "MIT; Apache-2.0"; d.StandardLicenseID = &s },
			"standard_license_id", CodeInvalidLicenseID},
		{"empty agreement ref", func(d *Document) { d.CustomAgreementRef = &empty },
			"custom_agreement_ref", CodeInvalidAgreementRef},
		{"agreement ref with a control character", func(d *Document) { s := "a\u0000b"; d.CustomAgreementRef = &s },
			"custom_agreement_ref", CodeInvalidAgreementRef},
		{"commercial use outside the vocabulary", func(d *Document) { d.Usage.CommercialUse = "yes" },
			"usage.commercial_use", CodeInvalidUsageValue},
		{"derivatives outside the vocabulary", func(d *Document) { d.Usage.Derivatives = "nope" },
			"usage.derivatives", CodeInvalidUsageValue},
		{"redistribution outside the vocabulary", func(d *Document) { d.Usage.Redistribution = "nope" },
			"usage.redistribution", CodeInvalidUsageValue},
		{"model training outside the vocabulary", func(d *Document) { d.Usage.ModelTraining = "nope" },
			"usage.model_training", CodeInvalidUsageValue},
		{"attribution from the permission vocabulary", func(d *Document) { d.Usage.Attribution = "allowed" },
			"usage.attribution", CodeInvalidUsageValue},
		{"patent grant outside the vocabulary", func(d *Document) { d.Usage.PatentGrant = "granted" },
			"usage.patent_grant", CodeInvalidUsageValue},
		{"see_agreement with no agreement", func(d *Document) { d.Usage.PatentGrant = PatentGrantSeeAgreement },
			"usage.patent_grant", CodePatentGrantWithoutAgreement},
		{"data access outside the vocabulary", func(d *Document) { d.Visibility.DataAccess = "private" },
			"visibility.data_access", CodeInvalidDataAccess},
		{"blank metadata token", func(d *Document) { d.Visibility.Metadata = "  " },
			"visibility.metadata", CodeInvalidMetadataVisibility},
		{"metadata token with inner whitespace", func(d *Document) { d.Visibility.Metadata = "project policy" },
			"visibility.metadata", CodeInvalidMetadataVisibility},
		{"notes over the bound", func(d *Document) { d.Notes = &long },
			"notes", CodeNotesTooLong},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := New()
			tc.mutate(&doc)
			raw, err := json.Marshal(doc)
			if err != nil {
				t.Fatalf("encoding the case's document: %v", err)
			}
			_, err = Parse(raw)
			if err == nil {
				t.Fatalf("Parse(%s) succeeded, want %s on %s", raw, tc.code, tc.field)
			}
			if reported := reportedFields(t, err); len(reported) != 1 || reported[tc.field] != tc.code {
				t.Errorf("Parse(%s) reported %v, want exactly {%s: %s}", raw, reported, tc.field, tc.code)
			}
		})
	}
}

// TestParseAcceptsWhatTheModelDoesNotJudge pins the other side of the
// boundary: the reader refuses malformed declarations, not unusual ones.
// An id that names no real license and an agreement ref that resolves to
// nothing are stored as given — deciding either is a legal judgement, and
// docs/38 §2 puts it outside the platform.
func TestParseAcceptsWhatTheModelDoesNotJudge(t *testing.T) {
	raw := `{"version":1,"standard_license_id":"NOT-A-REAL-LICENSE-1.0",` +
		`"custom_agreement_ref":"agreements/does-not-exist.pdf",` +
		`"usage":{"commercial_use":"restricted","derivatives":"restricted","redistribution":"restricted",` +
		`"model_training":"restricted","attribution":"not_required","patent_grant":"explicit"},` +
		`"visibility":{"metadata":"policy:0f8a1c2e","data_access":"open"},"notes":"Reviewed 2026-09-15."}`
	doc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse(%s) = %v, want nil error", raw, err)
	}
	if got := fieldOf(doc, "standard_license_id"); got != "NOT-A-REAL-LICENSE-1.0" {
		t.Errorf("standard_license_id = %q, want it stored verbatim", got)
	}
	if got := fieldOf(doc, "visibility.metadata"); got != "policy:0f8a1c2e" {
		t.Errorf("visibility.metadata = %q, want the unknown token preserved verbatim", got)
	}
}

// TestZeroDocumentIsRefused pins the empty Document value — what a caller
// gets from Document{} without New(). It must not validate: the zero value
// is not the default declaration, and a publish path that forgot to call
// New() has to fail rather than store a document full of empty strings.
func TestZeroDocumentIsRefused(t *testing.T) {
	err := Document{}.Validate()
	if err == nil {
		t.Fatal("Document{}.Validate() = nil, want a refusal")
	}
	codes := map[string]bool{}
	for _, e := range flatten(err) {
		codes[e.Code] = true
	}
	for _, want := range []string{CodeUnsupportedVersion, CodeInvalidUsageValue, CodeInvalidDataAccess, CodeInvalidMetadataVisibility} {
		if !codes[want] {
			t.Errorf("Document{} reported %v, want it to include %s", codes, want)
		}
	}
}

// fieldOf reads one field of a parsed document by its JSON path, so the
// tests above compare stored values rather than Go struct fields whose
// names could shadow a wrong json tag.
func fieldOf(d Document, path string) string {
	switch path {
	case "standard_license_id":
		if d.StandardLicenseID == nil {
			return "<null>"
		}
		return *d.StandardLicenseID
	case "custom_agreement_ref":
		if d.CustomAgreementRef == nil {
			return "<null>"
		}
		return *d.CustomAgreementRef
	case "usage.commercial_use":
		return string(d.Usage.CommercialUse)
	case "usage.derivatives":
		return string(d.Usage.Derivatives)
	case "usage.redistribution":
		return string(d.Usage.Redistribution)
	case "usage.model_training":
		return string(d.Usage.ModelTraining)
	case "usage.attribution":
		return string(d.Usage.Attribution)
	case "usage.patent_grant":
		return string(d.Usage.PatentGrant)
	case "visibility.metadata":
		return string(d.Visibility.Metadata)
	case "visibility.data_access":
		return string(d.Visibility.DataAccess)
	}
	return "<unknown field " + path + ">"
}

// codeOf returns the code of the first *ValidationError in err, failing
// the test when err is not a validation error at all — a test that
// accepted any error would pass on a panic-free but wrong refusal.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	return verr.Code
}

// reportedFields maps every joined validation error to its field and code,
// so a case can assert not only that the intended rule fired but that no
// neighbouring rule did.
func reportedFields(t *testing.T, err error) map[string]string {
	t.Helper()
	reported := map[string]string{}
	for _, e := range flatten(err) {
		reported[e.Field] = e.Code
	}
	return reported
}

// flatten unpacks a joined validation error into its parts.
func flatten(err error) []*ValidationError {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []*ValidationError
		for _, e := range joined.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	var verr *ValidationError
	if errors.As(err, &verr) {
		return []*ValidationError{verr}
	}
	return nil
}

// TestLengthBoundsCountCharactersNotBytes pins the unit of every bound this
// package states in characters.
//
// The bounds exist to answer "is this a sentence or a book?" (notes), "is
// this a reference or the agreement itself?" (the agreement ref) and "is
// this a token or a document?" (metadata visibility). All three questions
// are about what a human wrote, so all three are counted in characters —
// and this repository's working language is Chinese, where the difference
// is not a rounding error: a note of 700 汉字 is 2100 bytes, and a bound
// measured in bytes would refuse it while telling the publisher it has 2100
// characters, which is false twice over.
//
// The discriminating cases are the ones that are OVER the bound in bytes
// and UNDER it in characters: they are accepted, and they were refused
// before the unit was fixed. The bound values themselves (2000, 512, 64)
// are not part of this test's subject and were not changed.
func TestLengthBoundsCountCharactersNotBytes(t *testing.T) {
	// A string of exactly `n` characters, every one of them three bytes.
	wide := func(n int) string { return strings.Repeat("说", n) }

	if got := len(wide(1)); got != 3 {
		t.Fatalf("the fixture assumes a 3-byte character, got %d bytes", got)
	}

	// --- notes -----------------------------------------------------------
	// Exactly MaxNotesLen characters, 3x that in bytes.
	atLimit := New()
	noteAtLimit := wide(MaxNotesLen)
	if utf8.RuneCountInString(noteAtLimit) != MaxNotesLen {
		t.Fatalf("fixture: %d characters, want %d", utf8.RuneCountInString(noteAtLimit), MaxNotesLen)
	}
	atLimit.Notes = &noteAtLimit
	if err := atLimit.Validate(); err != nil {
		t.Errorf("a note of exactly %d characters was refused: %v\n"+
			"the bound is stated in characters, so it must be measured in characters (%d bytes)",
			MaxNotesLen, err, len(noteAtLimit))
	}

	// One character over: refused, and the count in the message is the
	// character count, not the byte count.
	overLimit := New()
	noteOver := wide(MaxNotesLen + 1)
	overLimit.Notes = &noteOver
	err := overLimit.Validate()
	if err == nil {
		t.Fatalf("a note of %d characters was accepted", MaxNotesLen+1)
	}
	var verr *ValidationError
	if !errors.As(err, &verr) || verr.Code != CodeNotesTooLong {
		t.Fatalf("Validate() = %v, want %s", err, CodeNotesTooLong)
	}
	if want := "got " + itoa(MaxNotesLen+1); !strings.Contains(verr.Detail, want) {
		t.Errorf("the rejection reads %q, want it to report %q characters\n"+
			"(it reports %d bytes instead, which is not what the sentence says it is)",
			verr.Detail, want, len(noteOver))
	}

	// The ASCII case is unchanged, so the fix did not move the bound for
	// the documents that already existed.
	ascii := New()
	asciiNote := strings.Repeat("x", MaxNotesLen)
	ascii.Notes = &asciiNote
	if err := ascii.Validate(); err != nil {
		t.Errorf("a note of exactly %d ASCII characters was refused: %v", MaxNotesLen, err)
	}
	asciiOver := New()
	asciiTooLong := strings.Repeat("x", MaxNotesLen+1)
	asciiOver.Notes = &asciiTooLong
	if err := asciiOver.Validate(); err == nil {
		t.Errorf("a note of %d ASCII characters was accepted", MaxNotesLen+1)
	}

	// --- custom agreement ref --------------------------------------------
	refAtLimit := wide(MaxAgreementRefLen)
	if !ValidAgreementRef(refAtLimit) {
		t.Errorf("an agreement ref of exactly %d characters (%d bytes) was refused: "+
			"the bound is in characters", MaxAgreementRefLen, len(refAtLimit))
	}
	if ValidAgreementRef(wide(MaxAgreementRefLen + 1)) {
		t.Errorf("an agreement ref of %d characters was accepted", MaxAgreementRefLen+1)
	}
	if !ValidAgreementRef(strings.Repeat("a", MaxAgreementRefLen)) {
		t.Errorf("an agreement ref of exactly %d ASCII characters was refused", MaxAgreementRefLen)
	}
	if ValidAgreementRef(strings.Repeat("a", MaxAgreementRefLen+1)) {
		t.Errorf("an agreement ref of %d ASCII characters was accepted", MaxAgreementRefLen+1)
	}

	// --- metadata visibility ---------------------------------------------
	// The alphabet is ASCII-only ([A-Za-z0-9_.:-]), so every token the rule
	// can accept has bytes == characters and no multibyte case can reach
	// the bound; the pair is asserted so the bound is exercised on both
	// sides and stated in the same unit as the two above.
	if !ValidMetadataVisibility(strings.Repeat("a", MaxMetadataVisibilityLen)) {
		t.Errorf("a metadata token of exactly %d characters was refused", MaxMetadataVisibilityLen)
	}
	if ValidMetadataVisibility(strings.Repeat("a", MaxMetadataVisibilityLen+1)) {
		t.Errorf("a metadata token of %d characters was accepted", MaxMetadataVisibilityLen+1)
	}

	// --- the license id --------------------------------------------------
	// Same unit for the fourth bound of the same kind. Its alphabet is
	// ASCII-only too, so this is consistency rather than a behaviour
	// change: no multibyte string was ever a well-formed id.
	if !ValidLicenseID(strings.Repeat("A", MaxLicenseIDLen)) {
		t.Errorf("a license id of exactly %d characters was refused", MaxLicenseIDLen)
	}
	if ValidLicenseID(strings.Repeat("A", MaxLicenseIDLen+1)) {
		t.Errorf("a license id of %d characters was accepted", MaxLicenseIDLen+1)
	}
}
