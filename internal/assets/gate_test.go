package assets

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	"github.com/lichman0405/post/internal/rsg/validation"
)

// Task T0702 required test "asset validators": the publish gate. This file
// is where the task's acceptance criterion — 缺 provenance / rights /
// version pin 阻止发布 (a version missing provenance, rights or a version
// pin does not get published) — is decided, twice over:
//
//  1. Gate refuses a candidate that is missing one of them, naming the
//     rule (the codes and facts asserted below), and
//  2. the refusal survives the LADDER — the asset gate of
//     internal/rsg/validation, run over the facts Gate produced, comes
//     back blocked (TestGateRefusalReachesTheAssetGate). That second half
//     is what makes this more than an agreement between two of my own
//     functions: the engine that gates a real publish reads an
//     AssetFacts struct this package has no say in.

// validRightsJSON is the stored rights document of an acceptable
// candidate: the template default (internal/rights.New), marshalled the
// way the publish path stores it.
func validRightsJSON(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := rights.New().Marshal()
	if err != nil {
		t.Fatalf("rights.New().Marshal: %v", err)
	}
	return raw
}

// validCandidate is a publish candidate that breaks no rule: the fixture
// every test below mutates by exactly one field.
func validCandidate(t *testing.T) PublishCandidate {
	t.Helper()
	m := manifestFor(TypeDataset)
	pin, ok := NewDependencyPin(anotherPID, "2.1")
	if !ok {
		t.Fatal("fixture: the sample pids must form a pin")
	}
	m.DependencyPins = []DependencyPin{pin}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	return PublishCandidate{
		AssetPID:      samplePID,
		AssetType:     TypeDataset,
		Version:       "1.0",
		Manifest:      raw,
		RightsJSON:    validRightsJSON(t),
		OriginRefs:    []string{"release:" + sampleUUID},
		Visibility:    VisibilityPublic,
		IntegrityHash: hash,
		CreatorIDs:    []string{sampleUUID},
	}
}

// mustGate runs the gate and fails when the candidate was refused.
func mustGate(t *testing.T, c PublishCandidate) GateResult {
	t.Helper()
	res, err := Gate(c)
	if err != nil {
		t.Fatalf("Gate refused a candidate it must accept: %v", err)
	}
	return res
}

// refusalCodes returns the codes of a gate refusal, in order.
func refusalCodes(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the gate to refuse, got nil")
	}
	return codesOf(validationErrors(err))
}

// refusalFields returns the field paths of a gate refusal, in order.
func refusalFields(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		t.Fatalf("expected the gate to refuse, got nil")
	}
	return fieldsOf(validationErrors(err))
}

// factsProbe renders the eight fact booleans as a comparable value. It
// exists because AssetFacts carries a json.RawMessage (the entity
// document, which Gate deliberately leaves empty) and so is not a
// comparable struct: comparing the probe is how a test asserts "exactly
// these facts, and no others".
type factsProbe struct {
	SourcePinned, VersionPinned, Contributors, Rights bool
	Visibility, DependencyPins, IntegrityHash         bool
	Metadata                                          bool
	DocumentLen                                       int
}

func probeFacts(f validation.AssetFacts) factsProbe {
	return factsProbe{
		SourcePinned: f.SourcePinned, VersionPinned: f.VersionPinned,
		Contributors: f.Contributors, Rights: f.Rights,
		Visibility: f.Visibility, DependencyPins: f.DependencyPins,
		IntegrityHash: f.IntegrityHash, Metadata: f.Metadata,
		DocumentLen: len(f.Document),
	}
}

// allTrue is the probe of a candidate that breaks no rule.
var allTrue = factsProbe{
	SourcePinned: true, VersionPinned: true, Contributors: true, Rights: true,
	Visibility: true, DependencyPins: true, IntegrityHash: true, Metadata: true,
	DocumentLen: 0, // Gate leaves the entity document to the publish command
}

// TestGateAcceptsACompleteCandidate is the positive control every refusal
// test below leans on: without it, a Gate that refused everything would
// pass all of them.
func TestGateAcceptsACompleteCandidate(t *testing.T) {
	c := validCandidate(t)
	res := mustGate(t, c)
	if got := probeFacts(res.Facts); got != allTrue {
		t.Errorf("facts = %+v, want %+v", got, allTrue)
	}
	if len(res.ManifestJSON) == 0 {
		t.Error("a passing candidate must yield the canonical manifest bytes to store")
	}
	if res.Rights.Version != rights.DocumentVersion {
		t.Errorf("rights document version = %d, want the parsed document", res.Rights.Version)
	}
	// The bytes handed back are the bytes the hash covers: storing them
	// keeps the row verifiable, which is what the hash rule buys.
	fromStored, err := ManifestHash(res.ManifestJSON)
	if err != nil {
		t.Fatalf("ManifestHash(the bytes Gate says to store): %v", err)
	}
	if fromStored != c.IntegrityHash {
		t.Errorf("the stored bytes hash to %s, the acceptable candidate claimed %s", fromStored, c.IntegrityHash)
	}
}

// TestGateRefusesMissingProvenance is one third of the acceptance
// criterion: 缺 provenance 阻止发布.
func TestGateRefusesMissingProvenance(t *testing.T) {
	c := validCandidate(t)
	c.OriginRefs = nil
	res, err := Gate(c)
	if res.Facts.SourcePinned {
		t.Error("SourcePinned must be false when the version pins no origin")
	}
	if codes := refusalCodes(t, err); len(codes) != 1 || codes[0] != CodeMissingProvenance {
		t.Fatalf("missing provenance = %v, want exactly [%s]", codes, CodeMissingProvenance)
	}
	if fields := refusalFields(t, err); fields[0] != "origin_refs" {
		t.Errorf("refusal names %q, want origin_refs", fields[0])
	}

	// An empty (but non-nil) list is the same absence.
	c.OriginRefs = []string{}
	if codes := refusalCodes(t, mustErr(Gate(c))); len(codes) != 1 || codes[0] != CodeMissingProvenance {
		t.Errorf("an empty origin list = %v, want exactly [%s]", codes, CodeMissingProvenance)
	}
}

// TestGateRefusesProvenanceThatPinsNoAcceptedSource pins the second half
// of the provenance rule: refs that exist but point at nothing accepted.
// docs/11 §3 wants "source accepted state/release", so a version that only
// names its project has no provenance a reader can resolve.
func TestGateRefusesProvenanceThatPinsNoAcceptedSource(t *testing.T) {
	for _, refs := range [][]string{
		{"project:" + sampleUUID},
		{"object_version:" + sampleUUID},
		{"project:" + sampleUUID, "object_version:" + sampleUUID},
	} {
		c := validCandidate(t)
		c.OriginRefs = refs
		res, err := Gate(c)
		if res.Facts.SourcePinned {
			t.Errorf("%v: SourcePinned = true, want false — no release or state is pinned", refs)
		}
		codes := refusalCodes(t, err)
		if len(codes) != 1 || codes[0] != CodeUnpinnedSource {
			t.Errorf("%v = %v, want exactly [%s]", refs, codes, CodeUnpinnedSource)
		}
	}

	// A state ref is as good as a release ref, and one of either is enough
	// even beside refs that pin nothing accepted.
	for _, refs := range [][]string{
		{"state:" + sampleUUID},
		{"project:" + sampleUUID, "state:" + sampleUUID},
		{"project:" + sampleUUID, "release:" + sampleUUID},
	} {
		c := validCandidate(t)
		c.OriginRefs = refs
		if res := mustGate(t, c); !res.Facts.SourcePinned {
			t.Errorf("%v: SourcePinned = false, want true", refs)
		}
	}
}

// TestGateRefusesMalformedProvenanceRef pins that a ref this platform
// cannot read is refused as such rather than counted either way.
func TestGateRefusesMalformedProvenanceRef(t *testing.T) {
	c := validCandidate(t)
	c.OriginRefs = []string{"release:" + sampleUUID, "release:not-a-uuid", "nonsense"}
	_, err := Gate(c)
	errs := validationErrors(err)
	if len(errs) != 2 {
		t.Fatalf("two malformed refs produced %d refusals %v, want 2", len(errs), codesOf(errs))
	}
	if errs[0].Field != "origin_refs[1]" || errs[1].Field != "origin_refs[2]" {
		t.Errorf("refusals name %v, want the two malformed indices", fieldsOf(errs))
	}
	for _, e := range errs {
		if e.Code != CodeInvalidProvenanceRef {
			t.Errorf("%s has code %s, want %s", e.Field, e.Code, CodeInvalidProvenanceRef)
		}
	}
}

// TestGateRefusesMissingRights is the second third of the acceptance
// criterion: 缺 rights 阻止发布.
func TestGateRefusesMissingRights(t *testing.T) {
	c := validCandidate(t)
	c.RightsJSON = nil
	res, err := Gate(c)
	if res.Facts.Rights {
		t.Error("Rights must be false when the version carries no rights document")
	}
	if codes := refusalCodes(t, err); len(codes) != 1 || codes[0] != CodeMissingRights {
		t.Fatalf("missing rights = %v, want exactly [%s]", codes, CodeMissingRights)
	}
	if fields := refusalFields(t, err); fields[0] != "rights_json" {
		t.Errorf("refusal names %q, want rights_json", fields[0])
	}

	// Whitespace is not a document either.
	c.RightsJSON = json.RawMessage("  \n")
	if codes := refusalCodes(t, mustErr(Gate(c))); len(codes) != 1 || codes[0] != CodeMissingRights {
		t.Errorf("blank rights bytes = %v, want exactly [%s]", codes, CodeMissingRights)
	}
}

// TestGateRefusesRightsThatDoNotRead pins that a document present but
// unusable is refused too — the difference between "no declaration" and
// "a declaration nothing can read" is the code, not the outcome.
func TestGateRefusesRightsThatDoNotRead(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty object", `{}`},
		{"no version", `{"usage":{},"visibility":{}}`},
		{"unknown version", `{"version":99}`},
		{"unknown field", `{"version":1,"licence":"MIT"}`},
		{"not an object", `[]`},
		{"truncated", `{"version":1`},
		{"trailing content", `{"version":1}{"version":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := validCandidate(t)
			cand.RightsJSON = json.RawMessage(c.raw)
			res, err := Gate(cand)
			if res.Facts.Rights {
				t.Errorf("%s: Rights = true, want false", c.raw)
			}
			codes := refusalCodes(t, err)
			if len(codes) != 1 || codes[0] != CodeInvalidRights {
				t.Fatalf("%s = %v, want exactly [%s]", c.raw, codes, CodeInvalidRights)
			}
			// The rights package's own code must survive into the detail,
			// so the publisher learns which rule the document broke.
			details := validationErrors(err)[0].Detail
			if !strings.Contains(details, "RIGHTS_") {
				t.Errorf("detail %q does not carry the rights refusal", details)
			}
		})
	}
}

// TestGateRefusesAMissingVersionPin is the third third of the acceptance
// criterion: 缺 version pin 阻止发布.
//
// "No version pin" is two things in one row — a label the platform cannot
// store as an immutable version, and a manifest whose dependency pins are
// not exact versions — and both are refused.
func TestGateRefusesAMissingVersionPin(t *testing.T) {
	for _, version := range []string{"", "   ", ".", "..", strings.Repeat("v", MaxVersionLen+1), "1.0/../2.0", "1.0 beta"} {
		c := validCandidate(t)
		c.Version = version
		res, err := Gate(c)
		if res.Facts.VersionPinned {
			t.Errorf("version %q: VersionPinned = true, want false", version)
		}
		codes := refusalCodes(t, err)
		if len(codes) != 1 || codes[0] != CodeInvalidVersionLabel {
			t.Errorf("version %q = %v, want exactly [%s]", version, codes, CodeInvalidVersionLabel)
		}
	}
	// And the labels that DO pin a version stay accepted.
	for _, version := range []string{"1", "1.0", "v2", "2026-09-15", "a..b"} {
		c := validCandidate(t)
		c.Version = version
		if res := mustGate(t, c); !res.Facts.VersionPinned {
			t.Errorf("version %q: VersionPinned = false, want true", version)
		}
	}
}

// TestGateRefusesFloatingDependencyPins pins the dependency half of the
// version-pin rule: a pin that is not an exact version is refused, and the
// candidate's dependency fact stays false.
func TestGateRefusesFloatingDependencyPins(t *testing.T) {
	cases := []struct {
		pins []DependencyPin
		code string
	}{
		{[]DependencyPin{"latest"}, CodeInvalidDependencyPin},
		{[]DependencyPin{DependencyPin(string(anotherPID) + "@^2.0.0")}, CodeInvalidDependencyPin},
		{[]DependencyPin{"not-a-pin"}, CodeInvalidDependencyPin},
	}
	for _, c := range cases {
		cand := validCandidate(t)
		m := manifestFor(TypeDataset)
		m.DependencyPins = c.pins
		raw, err := m.CanonicalJSON()
		if err != nil {
			t.Fatalf("canonical manifest: %v", err)
		}
		hash, err := m.Hash()
		if err != nil {
			t.Fatalf("manifest hash: %v", err)
		}
		cand.Manifest, cand.IntegrityHash = raw, hash

		res, gerr := Gate(cand)
		if res.Facts.DependencyPins {
			t.Errorf("%v: DependencyPins = true, want false", c.pins)
		}
		codes := refusalCodes(t, gerr)
		if len(codes) != 1 || codes[0] != c.code {
			t.Errorf("%v = %v, want exactly [%s]", c.pins, codes, c.code)
		}
	}
}

// TestGateRefusesASelfPin pins the rule that only the gate can apply: the
// manifest does not know which version it is being published as.
func TestGateRefusesASelfPin(t *testing.T) {
	self, ok := NewDependencyPin(samplePID, "1.0")
	if !ok {
		t.Fatal("fixture: the sample pid and version must form a pin")
	}
	c := validCandidate(t)
	m := manifestFor(TypeDataset)
	m.DependencyPins = []DependencyPin{self}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	c.Manifest = raw
	c.IntegrityHash, err = m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	res, gerr := Gate(c)
	if res.Facts.DependencyPins {
		t.Error("DependencyPins = true, want false for a self pin")
	}
	codes := refusalCodes(t, gerr)
	if len(codes) != 1 || codes[0] != CodeSelfDependencyPin {
		t.Fatalf("a self pin = %v, want exactly [%s]", codes, CodeSelfDependencyPin)
	}
}

// TestGateRefusesAManifestOfAnotherAssetType pins the quiet rule: the
// required metadata table is chosen by the manifest's own asset_type, so a
// manifest of one type published under an asset of another would be held
// to the wrong table.
func TestGateRefusesAManifestOfAnotherAssetType(t *testing.T) {
	c := validCandidate(t)
	m := manifestFor(TypeBenchmark) // valid on its own terms, wrong for this asset
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	c.Manifest = raw
	c.IntegrityHash, err = m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	res, gerr := Gate(c)
	if res.Facts.Metadata {
		t.Error("Metadata = true, want false when the manifest is of another type")
	}
	errs := validationErrors(gerr)
	if len(errs) != 1 || errs[0].Code != CodeManifestTypeMismatch || errs[0].Field != "asset_type" {
		t.Fatalf("type mismatch = %v, want exactly [%s] at asset_type", codesOf(errs), CodeManifestTypeMismatch)
	}
}

// TestGateRefusesAManifestOfTheVersionTypesOwnRequiredMetadata pins that
// the gate holds a candidate to the required metadata of ITS asset type:
// the same document is refused under one type and accepted under another.
func TestGateRefusesAManifestOfTheVersionTypesOwnRequiredMetadata(t *testing.T) {
	c := validCandidate(t)
	// Drop a dataset-only required field, keeping everything the benchmark
	// table wants.
	m := manifestFor(TypeDataset)
	delete(m.Metadata, "access_level")
	delete(m.Metadata, "quality_notes")
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	c.Manifest = raw
	c.IntegrityHash, err = m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	res, gerr := Gate(c)
	if res.Facts.Metadata {
		t.Error("Metadata = true, want false")
	}
	errs := validationErrors(gerr)
	if len(errs) != 2 {
		t.Fatalf("two missing required fields produced %d refusals %v, want 2", len(errs), codesOf(errs))
	}
	if errs[0].Field != metadataPath("access_level") || errs[1].Field != metadataPath("quality_notes") {
		t.Errorf("refusals name %v, want the two dropped fields", fieldsOf(errs))
	}
	for _, e := range errs {
		if e.Code != CodeMissingMetadata {
			t.Errorf("%s has code %s, want %s", e.Field, e.Code, CodeMissingMetadata)
		}
	}
}

// TestGateRefusesAManifestThatDoesNotParse pins the document boundary of
// the gate: the bytes that would land in the column are what is judged,
// and unusable bytes are refused before anything is read out of them.
func TestGateRefusesAManifestThatDoesNotParse(t *testing.T) {
	cases := []struct {
		name string
		raw  json.RawMessage
		code string
	}{
		{"absent", nil, CodeMalformedManifest},
		{"blank", json.RawMessage(" \n"), CodeMalformedManifest},
		{"not JSON", json.RawMessage(`nope`), CodeMalformedManifest},
		{"not an object", json.RawMessage(`[]`), CodeMalformedManifest},
		{"unknown field", json.RawMessage(`{"version":1,"asset_type":"dataset","metadata":{},"extra":1}`), CodeMalformedManifest},
		{"trailing content", json.RawMessage(`{"version":1,"asset_type":"dataset","metadata":{}}{}`), CodeMalformedManifest},
		{"unsupported format version", json.RawMessage(`{"version":7,"asset_type":"dataset","metadata":{}}`), CodeUnsupportedManifestVersion},
		{"unknown asset type", json.RawMessage(`{"version":1,"asset_type":"model","metadata":{}}`), CodeUnknownAssetType},
		{"missing metadata", json.RawMessage(`{"version":1,"asset_type":"dataset","metadata":{"purpose":"p"}}`), CodeMissingMetadata},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := validCandidate(t)
			cand.Manifest = c.raw
			res, err := Gate(cand)
			if res.Facts.Metadata {
				t.Errorf("%s: Metadata = true, want false", c.name)
			}
			codes := refusalCodes(t, err)
			if !contains(codes, c.code) {
				t.Errorf("%s = %v, want %s among them", c.name, codes, c.code)
			}
			// A manifest that does not parse leaves nothing to verify the
			// hash against, and the fact must stay false.
			if res.Facts.IntegrityHash {
				t.Errorf("%s: IntegrityHash = true, want false — there is no canonical document to cover", c.name)
			}
		})
	}
}

// TestGateRefusesAHashThatDoesNotCoverWhatIsPublished pins the strict
// reading of "the asset must carry its integrity hash": well-formed is not
// enough, the hash has to be the digest of the manifest being published.
//
// A shape-only check would accept all of these, so this test fails if the
// rule is ever weakened to one.
func TestGateRefusesAHashThatDoesNotCoverWhatIsPublished(t *testing.T) {
	other := manifestFor(TypeDataset)
	other.Metadata["purpose"] = "a different purpose"
	otherRaw, err := other.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	otherHash, err := other.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}

	// The canonical bytes without the canonical FORM: the same document
	// with whitespace after its colons. Its digest is a 64-character hex
	// string that covers this very manifest, and it is still wrong —
	// because the hash covers the CANONICAL bytes (the ones Gate hands
	// back to store), so a verifier that re-renders the stored document
	// derives the publisher's digest and nothing else.
	spaced := bytes.ReplaceAll(mustGate(t, validCandidate(t)).ManifestJSON, []byte(`":`), []byte(`": `))
	goodDigest := sha256Hex(mustGate(t, validCandidate(t)).ManifestJSON)

	cases := []struct {
		name string
		hash string
		code string
	}{
		{"absent", "", CodeInvalidIntegrityHash},
		{"too short", strings.Repeat("a", 63), CodeInvalidIntegrityHash},
		{"too long", strings.Repeat("a", 65), CodeInvalidIntegrityHash},
		{"uppercase hex", strings.ToUpper(goodDigest), CodeInvalidIntegrityHash},
		{"not hex", strings.Repeat("z", 64), CodeInvalidIntegrityHash},
		{"prefixed", "sha256:" + strings.Repeat("a", 64), CodeInvalidIntegrityHash},
		{"a digest of another document", otherHash, CodeIntegrityHashMismatch},
		{"the digest of the same document in another spelling", sha256Hex(spaced), CodeIntegrityHashMismatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := validCandidate(t)
			cand.IntegrityHash = c.hash
			res, err := Gate(cand)
			if res.Facts.IntegrityHash {
				t.Errorf("%s: IntegrityHash = true, want false", c.name)
			}
			codes := refusalCodes(t, err)
			if len(codes) != 1 || codes[0] != c.code {
				t.Fatalf("hash %q = %v, want exactly [%s]", c.hash, codes, c.code)
			}
		})
	}
	if string(otherRaw) == string(mustGate(t, validCandidate(t)).ManifestJSON) {
		t.Fatal("fixture: the two manifests must differ for the mismatch case to mean anything")
	}
}

// TestGateRefusesAVersionWithNoContributors pins the credit rule
// (docs/11 §3, CLAUDE.md invariant 14), the SHAPE half: at least one
// entry, none blank, and each one a user id — {"alice"} is the case that
// separates "not blank" from "storable", the rule T0711 added when the
// declaration got a uuid column to land in.
func TestGateRefusesAVersionWithNoContributors(t *testing.T) {
	for _, ids := range [][]string{nil, {}, {""}, {"  "}, {sampleUUID, ""}, {"alice"}, {sampleUUID, "not-a-uuid"}} {
		c := validCandidate(t)
		c.CreatorIDs = ids
		res, err := Gate(c)
		if res.Facts.Contributors {
			t.Errorf("creator ids %v: Contributors = true, want false", ids)
		}
		codes := refusalCodes(t, err)
		if len(codes) != 1 || codes[0] != CodeNoContributors {
			t.Errorf("creator ids %v = %v, want exactly [%s]", ids, codes, CodeNoContributors)
		}
	}
}

// secondCreatorUUID is a second real user id for the control case — a
// duplicate rule whose control list repeated one user could not tell
// "refused the repeat" from "refused two creators".
const secondCreatorUUID = "8f14e45f-ceea-4a1b-8d5c-1b0f9a2e6d31"

// TestGateRefusesADuplicateCreator pins the rule that keeps the credit
// table's own UNIQUE (asset_version_id, role, party_id) from being the
// first thing to notice a repeated entry: a publish that credits the same
// user twice is refused by the GATE, with a code of its own, so the
// preview blocks what the store could not have written.
//
// The case-differing pair is the one a text comparison misses: a uuid is
// one value whatever case its text is written in, so "A1B2…" and "a1b2…"
// are the same row, and the two different uuids after it are the control —
// the rule must not refuse a list of distinct users. The pair that is
// case-differing AND padded is the one the T0711 review caught: the gate
// admitted it as two entries after trimming and lowercasing both, and the
// store's conversion saw the padding it does not accept.
//
// A list that is BOTH blank and repetitive is not in the table: the shape
// rule answers first (`{sampleUUID, "", sampleUUID}` is refused as
// ASSET_NO_CONTRIBUTORS), because a list with a blank entry is not a
// declarable creator list at all — there is no pair of positions to name
// in a list the caller has to rewrite anyway.
func TestGateRefusesADuplicateCreator(t *testing.T) {
	upper := strings.ToUpper(sampleUUID)
	if upper == sampleUUID {
		t.Fatal("fixture: the sample uuid must have a case to change")
	}
	// The control first: two distinct creators pass, so a rule that
	// refused everything would not be read as a rule that refused the
	// repeat.
	control := validCandidate(t)
	control.CreatorIDs = []string{sampleUUID, secondCreatorUUID}
	res, err := Gate(control)
	if err != nil {
		t.Fatalf("two distinct creators were refused: %v", err)
	}
	if !res.Facts.Contributors {
		t.Error("two distinct creators: Contributors = false, want true")
	}

	for _, c := range []struct {
		name string
		ids  []string
	}{
		{"the same id twice", []string{sampleUUID, sampleUUID}},
		{"the same id in another case", []string{sampleUUID, upper}},
		{"the same id in another case, padded", []string{sampleUUID, " " + upper + " "}},
		{"the repeat in the middle", []string{sampleUUID, secondCreatorUUID, sampleUUID}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cand := validCandidate(t)
			cand.CreatorIDs = c.ids
			res, err := Gate(cand)
			if res.Facts.Contributors {
				t.Errorf("%s: Contributors = true, want false", c.name)
			}
			codes := refusalCodes(t, err)
			if len(codes) != 1 || codes[0] != CodeDuplicateCreatorID {
				t.Fatalf("creator ids %v = %v, want exactly [%s]", c.ids, codes, CodeDuplicateCreatorID)
			}
			// The refusal says WHICH entry repeats which, not only that
			// something is wrong: a publisher fixing the list needs the two
			// positions.
			ves := validationErrors(err)
			if len(ves) != 1 {
				t.Fatalf("the refusal = %v, want one rule", err)
			}
			if !strings.HasPrefix(ves[0].Field, "creator_ids[") {
				t.Errorf("%s: the refusal field = %q, want the repeated position creator_ids[i]", c.name, ves[0].Field)
			}
			if !strings.Contains(ves[0].Detail, "again as entry") {
				t.Errorf("%s: the refusal detail = %q, want it to name the entry the repeat credits again", c.name, ves[0].Detail)
			}
		})
	}
}

// TestGateAcceptsAPaddedCreatorID pins the half of the credit rule that is
// not a refusal: "  <uuid>  " is a user id, and the gate admits it. The
// padding is trimmed to decide — validCreatorIDs validates
// CanonicalCreatorID(id), and duplicateCreatorID compares the same form —
// and the writing path trims it to store
// (internal/persistence.insertVersionCredits calls the same function, so
// the gate's decision and the row's key are about one string).
//
// Refusing the padded spelling HERE instead was the alternative the T0711
// review named and ruled against: the request succeeded before 00082
// existed, duplicateCreatorID already folds case and trims, and the store
// can hold the id as well as the gate can judge it.
func TestGateAcceptsAPaddedCreatorID(t *testing.T) {
	padded := []string{"  " + sampleUUID + "  ", "\t" + secondCreatorUUID + "\n"}
	cand := validCandidate(t)
	cand.CreatorIDs = padded
	res, err := Gate(cand)
	if err != nil {
		t.Fatalf("a creator list with whitespace around the ids was refused: %v", err)
	}
	if !res.Facts.Contributors {
		t.Error("a padded creator list: Contributors = false, want true")
	}
	// And the canonical form both layers use is the id itself, not the
	// caller's spelling of it.
	for i, want := range []string{sampleUUID, secondCreatorUUID} {
		if got := CanonicalCreatorID(padded[i]); got != want {
			t.Errorf("CanonicalCreatorID(%q) = %q, want the id %q", padded[i], got, want)
		}
	}
	if got := CanonicalCreatorID("  " + strings.ToUpper(sampleUUID) + " "); got != sampleUUID {
		t.Errorf("CanonicalCreatorID of the padded uppercase spelling = %q, want %q", got, sampleUUID)
	}
}

// TestGateRefusesAVersionWithNoVisibility pins the visibility rule: the
// column admits two values and nothing else.
func TestGateRefusesAVersionWithNoVisibility(t *testing.T) {
	for _, v := range []Visibility{"", "Public", "unlisted", "internal", "restricted"} {
		c := validCandidate(t)
		c.Visibility = v
		res, err := Gate(c)
		if res.Facts.Visibility {
			t.Errorf("visibility %q: Visibility = true, want false", v)
		}
		codes := refusalCodes(t, err)
		if len(codes) != 1 || codes[0] != CodeInvalidVisibility {
			t.Errorf("visibility %q = %v, want exactly [%s]", v, codes, CodeInvalidVisibility)
		}
	}
}

// TestGateRefusesAnUnstorableAssetPID pins the identity precondition: a
// candidate whose pid is not a pid is refused before any of the ladder's
// facts are considered meaningful.
func TestGateRefusesAnUnstorableAssetPID(t *testing.T) {
	for _, pid := range []PID{"", "not-a-pid", PID(strings.ToUpper(string(samplePID))), PID(string(samplePID) + "x")} {
		c := validCandidate(t)
		c.AssetPID = pid
		_, err := Gate(c)
		codes := refusalCodes(t, err)
		if !contains(codes, CodeInvalidPID) {
			t.Errorf("pid %q = %v, want %s among them", pid, codes, CodeInvalidPID)
		}
	}
}

// TestGateReportsEveryBrokenRuleInOnePass pins the reporting rule at the
// gate level: a candidate that is wrong in five ways reports five rules,
// so the publisher fixes one round of problems rather than five.
func TestGateReportsEveryBrokenRuleInOnePass(t *testing.T) {
	c := PublishCandidate{
		AssetPID:   samplePID,
		AssetType:  TypeDataset,
		Version:    "..",
		Manifest:   nil,
		Visibility: "unlisted",
	}
	res, err := Gate(c)
	if err == nil {
		t.Fatal("a candidate wrong in seven ways must be refused")
	}
	if got := probeFacts(res.Facts); got != (factsProbe{}) {
		t.Errorf("every fact of a candidate wrong in seven ways must be false, got %+v", got)
	}
	codes := refusalCodes(t, err)
	want := []string{
		CodeInvalidVersionLabel,  // version ".."
		CodeMissingProvenance,    // no origin refs
		CodeMissingRights,        // no rights document
		CodeInvalidVisibility,    // "unlisted"
		CodeMalformedManifest,    // no manifest at all
		CodeInvalidIntegrityHash, // no hash
		CodeNoContributors,       // no creators
	}
	if strings.Join(codes, ",") != strings.Join(want, ",") {
		t.Errorf("refusals = %v, want %v (deterministic order, every rule)", codes, want)
	}
}

// TestGateRefusalReachesTheAssetGate is the end-to-end half of the
// acceptance criterion, run in-process: the facts Gate produces are handed
// to the ladder's own asset gate (internal/rsg/validation), which is the
// engine a real publish is gated by, and the report it returns must be
// BLOCKED for each of the three missing pieces — with the corresponding
// check named among the blocking failures.
//
// The engine is deliberately not re-implemented here: this test would pass
// just as well against two of my own functions, so it calls the real
// validator, the real schema registry and the real gate specification.
func TestGateRefusalReachesTheAssetGate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PublishCandidate)
		check  validation.CheckID
	}{
		{"no provenance", func(c *PublishCandidate) { c.OriginRefs = nil }, validation.CheckAssetSourcePinned},
		{"provenance that pins nothing accepted", func(c *PublishCandidate) {
			c.OriginRefs = []string{"project:" + sampleUUID}
		}, validation.CheckAssetSourcePinned},
		{"no rights", func(c *PublishCandidate) { c.RightsJSON = nil }, validation.CheckAssetRights},
		{"rights that do not read", func(c *PublishCandidate) {
			c.RightsJSON = json.RawMessage(`{}`)
		}, validation.CheckAssetRights},
		{"no version label", func(c *PublishCandidate) { c.Version = "" }, validation.CheckAssetVersionPinned},
		{"no integrity hash", func(c *PublishCandidate) { c.IntegrityHash = "" }, validation.CheckAssetIntegrityHash},
		{"a hash that covers another document", func(c *PublishCandidate) {
			c.IntegrityHash = strings.Repeat("a", 64)
		}, validation.CheckAssetIntegrityHash},
		{"no required metadata", func(c *PublishCandidate) {
			c.Manifest = json.RawMessage(`{"version":1,"asset_type":"dataset","metadata":{}}`)
			c.IntegrityHash = sha256Hex(c.Manifest)
		}, validation.CheckAssetMetadata},
		{"no contributors", func(c *PublishCandidate) { c.CreatorIDs = nil }, validation.CheckAssetContributors},
		{"no visibility", func(c *PublishCandidate) { c.Visibility = "" }, validation.CheckAssetVisibility},
		{"a self pin", func(c *PublishCandidate) {
			self, _ := NewDependencyPin(samplePID, "1.0")
			m := manifestFor(TypeDataset)
			m.DependencyPins = []DependencyPin{self}
			c.Manifest, _ = m.CanonicalJSON()
			c.IntegrityHash, _ = m.Hash()
		}, validation.CheckAssetDependencyPins},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cand := validCandidate(t)
			c.mutate(&cand)
			res, err := Gate(cand)
			if err == nil {
				t.Fatal("the gate accepted the candidate; the ladder would publish it")
			}
			rep := assetGateReport(t, res.Facts)
			if !rep.Blocked() {
				t.Fatalf("asset gate verdict = %s, want blocked; explanation:\n%s", rep.Verdict, rep.Explanation)
			}
			var found bool
			for _, f := range rep.BlockingFailures() {
				if f.Check == c.check {
					found = true
				}
			}
			if !found {
				t.Errorf("asset gate blocking failures %v do not name %s", blockingChecks(rep.BlockingFailures()), c.check)
			}
		})
	}
}

// TestGateAcceptanceReachesTheAssetGate is the positive control of the
// test above: the same engine over the facts of an acceptable candidate
// blocks nothing of its own.
func TestGateAcceptanceReachesTheAssetGate(t *testing.T) {
	cand := validCandidate(t)
	res := mustGate(t, cand)
	// The publish command renders the research-asset-version document from
	// the row it is about to write (T0705); this test stands in for it with
	// the fields the candidate already determines, so the asset gate is
	// given a document to validate.
	res.Facts.Document = assetVersionDocument(t, cand, res)
	res.Facts.Ref = schemareg.Ref{ID: schemareg.CanonicalNamespace + "research-asset-version.schema.json", Version: schemareg.CanonicalV1}

	rep := assetGateReport(t, res.Facts)
	for _, f := range rep.BlockingFailures() {
		t.Errorf("asset gate blocks %s: %s", f.Check, f.Detail)
	}
	if rep.Blocked() {
		t.Fatalf("asset gate verdict = %s over an acceptable candidate:\n%s", rep.Verdict, rep.Explanation)
	}
}

// assetVersionDocument renders the research-asset-version entity document
// a publish would store, from the fields the candidate determines. The
// server-authoritative fields the row adds (id, project_id, created_by,
// created_at) are absent: they are T0705's, and the schema does not
// require them.
func assetVersionDocument(t *testing.T, c PublishCandidate, res GateResult) json.RawMessage {
	t.Helper()
	rightsDoc, err := rights.Parse(c.RightsJSON)
	if err != nil {
		t.Fatalf("rights: %v", err)
	}
	rightsRaw, err := rightsDoc.Marshal()
	if err != nil {
		t.Fatalf("rights: %v", err)
	}
	doc, err := json.Marshal(map[string]any{
		"asset_id":        string(c.AssetPID),
		"version":         c.Version,
		"asset_type":      string(c.AssetType),
		"origin_refs":     c.OriginRefs,
		"rights":          json.RawMessage(rightsRaw),
		"integrity_hash":  c.IntegrityHash,
		"creator_ids":     c.CreatorIDs,
		"dependency_refs": res.Manifest.DependencyPins,
	})
	if err != nil {
		t.Fatalf("render the asset-version document: %v", err)
	}
	return doc
}

// assetGateReport runs the real asset gate over a set of facts. The
// release facts are supplied because the asset gate also runs the release
// checks (an asset descends from a release); nothing else about the chain
// matters to what is being asserted, which is why the snapshot carries no
// chain.
func assetGateReport(t *testing.T, facts validation.AssetFacts) validation.Report {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	v := validation.NewValidator(reg)
	return v.Validate(validation.GateAsset, validation.Snapshot{
		ProjectID: sampleUUID,
		BranchID:  sampleUUID,
		Release:   &validation.ReleaseFacts{ReviewApproved: true, RightsSnapshot: true, FromMainBranch: true},
		Asset:     &facts,
	})
}

// blockingChecks lists the checks a report blocks on, for failure
// messages.
func blockingChecks(failures []validation.Result) []string {
	out := make([]string, 0, len(failures))
	for _, f := range failures {
		out = append(out, string(f.Check))
	}
	return out
}

// contains reports whether the list holds the value.
func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// mustErr returns the error of a gate call, for the call sites that only
// need the refusal.
func mustErr(_ GateResult, err error) error { return err }
