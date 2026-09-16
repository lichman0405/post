// Task T0705 required test "asset publish tests" — the unit half of the
// publish store's document gate.
//
// The store's publish transaction (asset_publish_store.go) renders the
// research-asset-version ENTITY DOCUMENT of the row it is about to write
// and runs the validation ladder's asset gate over it, refusing the
// publication on a blocking result. Those two halves live in this package
// and are unexported, so they are pinned here; the composition — that a
// real publish over real rows reaches them and that the six acceptance
// criteria hold end to end — is pinned in tests/integration
// (asset_publish_test.go).
//
// What this file exists to settle, and why it is not covered by the
// integration suite:
//
//   - THE INSTRUMENT CAN SAY NO. A check that has never been observed
//     refusing is not known to check anything, and the schema check behind
//     a gate-accepted candidate is one an integration test would only ever
//     see PASS (assets.Gate and the schema agree on a well-formed
//     publication). Each refusal below is produced by driving
//     requirePublishedAssetDocument over a document the schema really
//     rejects — and the injections are into the CANDIDATE, which is what
//     makes the refusal evidence that the document is rendered from the
//     row's own values rather than from a constant.
//
//   - THE TWO CALLER-OWNED FACTS ARE FILLED. validation.AssetFacts.Document
//     and .Ref are left to the caller by design (assets/gate.go), and
//     leaving them empty fails asset_schema closed ("the asset document
//     was not provided"). The failing direction is asserted HERE, so that
//     the passing direction in the store is a fact about the store filling
//     them rather than about a check that cannot fail.
//
//   - THE FILTER IS THE WHOLE FILTER. requirePublishedAssetDocument runs
//     the whole GateAsset and refuses on publishJudgesChecks. That would be
//     unfalsifiable if the rest of the spec passed anyway, so the raw
//     report is run here too and the checks it refuses on — the
//     release-lineage facts a publication has no source for — are asserted
//     to be present and to be outside the filter. Without this, a filter
//     that was silently widened to "everything" would look identical to one
//     that is exactly the publish's own checklist.
package persistence

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/assetpublish"
	"github.com/lichman0405/post/internal/assets"
	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/schemareg"
	rsgvalidation "github.com/lichman0405/post/internal/rsg/validation"
)

// assetDocProjectID is any project id: the ladder's asset gate reads it
// for the snapshot's identity, and every check under test here is about the
// document.
const assetDocProjectID = "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"

// assetDocPID is the pid the fixture publishes under, in the Crockford
// base32 shape assets.ValidPID accepts.
const assetDocPID = "01j9z6k3m4n5p6q7r8s9t0v1w2"

// assetDocReleaseID is the release the fixture's provenance pins — a ref of
// the kind the gate requires (docs/11 §3: source accepted state/release).
const assetDocReleaseID = "11111111-2222-3333-4444-555555555555"

// assetDocValidator returns the ladder over the canonical registry, the
// same pair cmd/api/main.go wires into the store.
func assetDocValidator(t *testing.T) *rsgvalidation.Validator {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return rsgvalidation.NewValidator(reg)
}

// assetDocCandidate builds a publication the publish gate ACCEPTS, through
// the production documents (assets.Manifest canonical form, rights.New),
// so that every refusal asserted below is attributable to the injection
// and not to a fixture the gate would have refused anyway.
func assetDocCandidate(t *testing.T) (assets.PublishCandidate, assets.GateResult) {
	t.Helper()
	m := assets.Manifest{
		Version:   assets.ManifestFormatVersion,
		AssetType: assets.TypeDataset,
		Metadata: assets.Metadata{
			"purpose":       "publish store unit fixture",
			"data_type":     "table",
			"blob_ids":      []any{"33333333-4444-5555-6666-777777777777"},
			"access_level":  "open",
			"quality_notes": "checked",
		},
	}
	raw, err := m.CanonicalJSON()
	if err != nil {
		t.Fatalf("canonical manifest: %v", err)
	}
	hash, err := m.Hash()
	if err != nil {
		t.Fatalf("manifest hash: %v", err)
	}
	doc := rights.New()
	doc.Visibility.DataAccess = rights.DataAccessOpen
	rightsJSON, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal rights: %v", err)
	}
	cand := assets.PublishCandidate{
		AssetPID:      assets.PID(assetDocPID),
		AssetType:     assets.TypeDataset,
		Version:       "1.0",
		Manifest:      raw,
		RightsJSON:    rightsJSON,
		OriginRefs:    []string{"release:" + assetDocReleaseID},
		Visibility:    assets.VisibilityPublic,
		IntegrityHash: hash,
		CreatorIDs:    []string{"44444444-5555-6666-7777-888888888888"},
	}
	gate, err := assets.Gate(cand)
	if err != nil {
		t.Fatalf("the fixture candidate is not publishable: %v", err)
	}
	return cand, gate
}

// TestPublishedAssetDocumentPassesItsOwnSchema is the passing direction:
// the document rendered from the row about to be written validates against
// the research-asset-version schema, and the ladder refuses nothing.
func TestPublishedAssetDocumentPassesItsOwnSchema(t *testing.T) {
	cand, gate := assetDocCandidate(t)
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}

	raw, err := renderAssetVersionDocument(cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("renderAssetVersionDocument: %v", err)
	}
	// The document is the row: every field the schema requires is rendered
	// from the candidate the publish carries, and the rights bytes embedded
	// are the ones the rights_json column stores.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered document is not an object: %v: %s", err, raw)
	}
	want := []string{"asset_id", "version", "asset_type", "origin_refs", "rights", "integrity_hash"}
	for _, key := range want {
		if _, ok := doc[key]; !ok {
			t.Errorf("the rendered document has no %q: %s", key, raw)
		}
	}
	if got := string(doc["asset_id"]); got != `"`+assetDocPID+`"` {
		t.Errorf("asset_id = %s, want the candidate's pid", got)
	}
	if got := string(doc["rights"]); strings.TrimSpace(got) != strings.TrimSpace(string(rightsJSON)) {
		t.Errorf("rights = %s, want the bytes the rights_json column stores: %s", got, rightsJSON)
	}

	// The schema itself, called directly: the instrument the ladder runs is
	// the same one, and asking it the same question here keeps a failure
	// from being attributed to the ladder's plumbing.
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	if err := reg.Validate(publishedAssetDocumentSchema(), raw); err != nil {
		t.Errorf("the rendered document does not validate against its own schema: %v\n%s", err, raw)
	}

	reasons, err := requirePublishedAssetDocument(assetDocValidator(t), assetDocProjectID, cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("requirePublishedAssetDocument: %v", err)
	}
	if len(reasons) != 0 {
		t.Errorf("the ladder refused a publishable candidate: %v", reasons)
	}
}

// TestAssetDocumentGateRefusesABrokenDocument is the failing direction, one
// injection per case. Every case drives the real
// requirePublishedAssetDocument and every case must come back with a reason
// naming asset_schema — the check that can only run here, because it is the
// only one that reads the rendered document.
func TestAssetDocumentGateRefusesABrokenDocument(t *testing.T) {
	val := assetDocValidator(t)
	cand, gate := assetDocCandidate(t)
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}

	cases := []struct {
		name string
		// inject breaks the candidate the document is rendered from.
		inject func(c assets.PublishCandidate) assets.PublishCandidate
	}{
		{
			// The row has NOT NULL + a CHECK on this column
			// (research_asset_versions_origin_refs_nonempty), so this is the
			// case where the ladder and the database agree: neither will
			// accept a version with no provenance.
			name:   "no provenance at all",
			inject: func(c assets.PublishCandidate) assets.PublishCandidate { c.OriginRefs = nil; return c },
		},
		{
			name:   "an empty provenance list",
			inject: func(c assets.PublishCandidate) assets.PublishCandidate { c.OriginRefs = []string{}; return c },
		},
		{
			// An asset type outside the schema's enum. The gate would have
			// refused this candidate earlier (the four V1 types), so it is
			// injected HERE to isolate the claim the case is about: the
			// document's asset_type is the CANDIDATE's value, and the schema
			// judges it — not a constant the renderer supplies.
			name: "an asset type the schema's enum does not carry",
			inject: func(c assets.PublishCandidate) assets.PublishCandidate {
				c.AssetType = assets.Type("software")
				return c
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			broken := tc.inject(cand)
			// The injection must really break the document: if it did not,
			// the refusal below would be a refusal of something else.
			raw, err := renderAssetVersionDocument(broken, gate, rightsJSON)
			if err != nil {
				t.Fatalf("renderAssetVersionDocument: %v", err)
			}
			reg, err := schemareg.New()
			if err != nil {
				t.Fatalf("schemareg.New: %v", err)
			}
			if err := reg.Validate(publishedAssetDocumentSchema(), raw); err == nil {
				t.Fatalf("the injected document still validates: %s", raw)
			}

			reasons, err := requirePublishedAssetDocument(val, assetDocProjectID, broken, gate, rightsJSON)
			if err != nil {
				t.Fatalf("requirePublishedAssetDocument: %v", err)
			}
			if len(reasons) == 0 {
				t.Fatalf("a document the schema rejects was not refused: %s", raw)
			}
			joined := strings.Join(reasons, "\n")
			if !strings.Contains(joined, string(rsgvalidation.CheckAssetSchema)) {
				t.Errorf("the refusal does not name %s: %s", rsgvalidation.CheckAssetSchema, joined)
			}
			// The refusal is a REASON the caller can act on, not a bare
			// code: the ladder's own detail (which field, which constraint)
			// travels with it.
			if !strings.Contains(joined, ":") || len(joined) < len(string(rsgvalidation.CheckAssetSchema))+2 {
				t.Errorf("the refusal carries no detail: %q", joined)
			}
		})
	}
}

// TestAssetDocumentFactsMustBeFilled pins the two caller-owned facts in the
// direction that matters: left empty, the asset gate fails asset_schema
// closed, so a publish path that forgot to render the document would refuse
// every publication rather than write it unchecked.
//
// It is the antecedent of the store's own call — the store fills both
// fields (requirePublishedAssetDocument), and this test is what makes that
// fill a decision with a consequence instead of a formality.
func TestAssetDocumentFactsMustBeFilled(t *testing.T) {
	val := assetDocValidator(t)
	cand, gate := assetDocCandidate(t)
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}
	doc, err := renderAssetVersionDocument(cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("renderAssetVersionDocument: %v", err)
	}

	// The facts exactly as assets.Gate returns them: correct on all nine
	// checklist items, and with the document and the ref — which are the
	// caller's to fill — still zero.
	report := val.Validate(rsgvalidation.GateAsset, rsgvalidation.Snapshot{
		ProjectID: assetDocProjectID,
		Asset:     &gate.Facts,
	})
	failure := blockingFor(t, report, rsgvalidation.CheckAssetSchema)
	if !strings.Contains(failure.Detail, "not provided") {
		t.Errorf("the empty-document refusal reads %q, want a refusal about the absent document", failure.Detail)
	}
	if failure.Passed {
		t.Error("asset_schema passed with no document: the check would not notice a publish path that skipped the render")
	}

	// One field at a time, so neither is redundant: the document without the
	// ref, and the ref without the document.
	noRef := gate.Facts
	noRef.Document = doc
	if got := blockingFor(t, val.Validate(rsgvalidation.GateAsset, rsgvalidation.Snapshot{
		ProjectID: assetDocProjectID, Asset: &noRef,
	}), rsgvalidation.CheckAssetSchema); got.Detail == "" {
		t.Error("asset_schema passed with a document but no schema ref")
	}
	noDoc := gate.Facts
	noDoc.Ref = publishedAssetDocumentSchema()
	if got := blockingFor(t, val.Validate(rsgvalidation.GateAsset, rsgvalidation.Snapshot{
		ProjectID: assetDocProjectID, Asset: &noDoc,
	}), rsgvalidation.CheckAssetSchema); !strings.Contains(got.Detail, "not provided") {
		t.Errorf("asset_schema with a ref but no document = %q, want a refusal about the absent document", got.Detail)
	}
}

// TestAssetDocumentGateJudgesOnlyTheChecksThePublishOwns is the control for
// the filter: the raw asset gate refuses this same snapshot on facts a
// PUBLICATION cannot answer, and requirePublishedAssetDocument refuses
// nothing. Both halves are needed — the first shows the filter is doing
// work, the second shows it excludes exactly the unimplementable facts and
// not the publish's own checklist.
func TestAssetDocumentGateJudgesOnlyTheChecksThePublishOwns(t *testing.T) {
	val := assetDocValidator(t)
	cand, gate := assetDocCandidate(t)
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}
	doc, err := renderAssetVersionDocument(cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("renderAssetVersionDocument: %v", err)
	}
	facts := gate.Facts
	facts.Document = doc
	facts.Ref = publishedAssetDocumentSchema()

	raw := val.Validate(rsgvalidation.GateAsset, rsgvalidation.Snapshot{
		ProjectID: assetDocProjectID,
		Asset:     &facts,
	})
	blocking := raw.BlockingFailures()
	if len(blocking) == 0 {
		t.Fatal("the asset gate refuses nothing without a branch and a release: the filter below would be " +
			"unfalsifiable, and this test would be measuring nothing")
	}
	// The release-lineage facts: the review record and the rights/policy
	// snapshot of the state the content descends from. A publish has no
	// source for them (releases.Service.ReleaseFacts does), and asserting
	// them here would invent a product rule the task's specs do not state.
	for _, check := range []rsgvalidation.CheckID{
		rsgvalidation.CheckReleaseReview,
		rsgvalidation.CheckReleaseRights,
		rsgvalidation.CheckReleaseFromMain,
	} {
		if got := blockingFor(t, raw, check); got.Detail != "release facts were not provided" {
			t.Errorf("%s = %q, want the unimplemented-fact refusal these are filtered for", check, got.Detail)
		}
		if publishJudgesChecks[check] {
			t.Errorf("%s is inside publishJudgesChecks: the filter no longer excludes the facts a publish "+
				"cannot answer, and every publish would be refused", check)
		}
	}

	reasons, err := requirePublishedAssetDocument(val, assetDocProjectID, cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("requirePublishedAssetDocument: %v", err)
	}
	if len(reasons) != 0 {
		t.Errorf("the publish path refused a publishable candidate over facts it cannot answer: %v", reasons)
	}

	// And the other direction of the same boundary: every check the filter
	// DOES admit is a failure this path must act on. The nine facts are
	// assets.Gate's own checklist, so the mapping is not a place for a
	// check to be dropped — the count is asserted rather than the names,
	// because the names are pinned above where they are used.
	if len(publishJudgesChecks) != 9 {
		t.Errorf("publishJudgesChecks carries %d checks, want the nine asset facts plus asset_schema",
			len(publishJudgesChecks))
	}
	for check := range publishJudgesChecks {
		if check == rsgvalidation.CheckReleaseReview || check == rsgvalidation.CheckReleaseRights ||
			check == rsgvalidation.CheckReleaseFromMain {
			t.Errorf("publishJudgesChecks admits %s, a release-lineage fact", check)
		}
	}
}

// TestRequirePublishedAssetDocumentFailsClosedOnAnUnwiredEngine pins the
// wiring refusal: a store built without the validation engine refuses the
// publish with a store error rather than writing an unchecked row. "Cannot
// check" is not "passed", and the difference is only visible if the wiring
// is exercised with the engine absent.
func TestRequirePublishedAssetDocumentFailsClosedOnAnUnwiredEngine(t *testing.T) {
	cand, gate := assetDocCandidate(t)
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}
	reasons, err := requirePublishedAssetDocument(nil, assetDocProjectID, cand, gate, rightsJSON)
	if err == nil {
		t.Fatalf("an unwired engine judged the document (%v reasons): a publish that cannot be checked must not "+
			"be written", reasons)
	}
	if !errors.Is(err, assetpublish.ErrStore) {
		t.Errorf("the wiring refusal = %v, want it to wrap assetpublish.ErrStore so the transport answers "+
			"a service defect rather than a caller error", err)
	}
	if len(reasons) != 0 {
		t.Errorf("reasons = %v alongside an error, want none", reasons)
	}
}

// TestAssetVersionDocumentRendersEmptyListsAsArrays pins the one rendering
// decision the schema makes visible: the three list fields are spelled []
// rather than null. null is not an array, so a null would refuse the
// document for a difference that means nothing — and the difference is
// reachable, because the pins come from a manifest that may declare none
// and the creators from a candidate that may credit nobody.
//
// The assertion is per FIELD and on the top-level document, not on the raw
// bytes: the rights document the publication stores carries nulls of its
// own (optional license fields, notes), and a check that searched the whole
// rendering for "null" would be asserting about rights rather than about
// this renderer.
func TestAssetVersionDocumentRendersEmptyListsAsArrays(t *testing.T) {
	cand, gate := assetDocCandidate(t)
	cand.CreatorIDs = nil
	gate.Manifest.DependencyPins = nil
	rightsJSON, err := rightsBytes(gate)
	if err != nil {
		t.Fatalf("rightsBytes: %v", err)
	}
	raw, err := renderAssetVersionDocument(cand, gate, rightsJSON)
	if err != nil {
		t.Fatalf("renderAssetVersionDocument: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the rendered document does not parse: %v: %s", err, raw)
	}
	for _, field := range []string{"creator_ids", "dependency_refs", "origin_refs"} {
		got := strings.TrimSpace(string(doc[field]))
		if got == "null" {
			t.Errorf("%s rendered as null, which the schema's array field refuses: %s", field, raw)
			continue
		}
		if !strings.HasPrefix(got, "[") || !strings.HasSuffix(got, "]") {
			t.Errorf("%s = %s, want a JSON array", field, got)
		}
	}
	if got := string(doc["creator_ids"]); got != "[]" {
		t.Errorf("creator_ids = %s, want the empty list the candidate carries", got)
	}
	if got := string(doc["dependency_refs"]); got != "[]" {
		t.Errorf("dependency_refs = %s, want the empty list the manifest declares", got)
	}
}

// blockingFor returns the blocking failure of one check, and fails when the
// report has none — the reports asserted over here are ones that must
// refuse, so a missing refusal is the failure rather than an absent value.
func blockingFor(t *testing.T, report rsgvalidation.Report, check rsgvalidation.CheckID) rsgvalidation.Result {
	t.Helper()
	for _, failure := range report.BlockingFailures() {
		if failure.Check == check {
			return failure
		}
	}
	t.Fatalf("no blocking %s in %v", check, report.BlockingFailures())
	return rsgvalidation.Result{}
}
