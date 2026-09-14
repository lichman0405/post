package validation

import (
	"fmt"
	"strings"
)

// CheckID names one check of the gate ladder. The ids are the stable
// vocabulary of every Report: the same id appears at several gates, at
// increasing severity, which is exactly what makes the ladder explainable
// ("schema_typed warns at draft and blocks at pr").
type CheckID string

// The three check families of the task — schema, required fields,
// provenance — plus the release/asset additions:
const (
	// schema family: does the version reference a real schema and satisfy
	// it (internal/rsg/schemareg, T0201)?
	CheckSchemaKnown         CheckID = "schema_known"
	CheckSchemaCore          CheckID = "schema_core"
	CheckSchemaTyped         CheckID = "schema_typed"
	CheckAuthoritativeFields CheckID = "authoritative_fields"
	// required-field family: row-level identity facts and the per-type
	// domain fields docs/08 requires beyond the schema's own required
	// array.
	CheckIdentityFields CheckID = "identity_fields"
	CheckRequiredFields CheckID = "required_fields"
	// provenance family: who wrote what, through which transition, on an
	// unbroken chain, with intact stored bytes.
	CheckStateLinkage     CheckID = "state_linkage"
	CheckCommitLinkage    CheckID = "commit_linkage"
	CheckActorConsistency CheckID = "actor_consistency"
	CheckPayloadIntegrity CheckID = "payload_integrity"
	CheckChainIntegrity   CheckID = "chain_integrity"
	// branch-level observation (never blocking): nothing to validate yet.
	CheckBranchEmpty CheckID = "branch_empty"
	// release gate (docs/11 §1): the immutable release snapshot fixes the
	// review/approval record, the rights snapshot and the accepted
	// main-branch origin.
	CheckReleaseReview   CheckID = "release_review"
	CheckReleaseRights   CheckID = "release_rights"
	CheckReleaseFromMain CheckID = "release_from_main"
	// asset gate (docs/11 §4): the full publish checklist.
	CheckAssetSourcePinned   CheckID = "asset_source_pinned"
	CheckAssetVersionPinned  CheckID = "asset_version_pinned"
	CheckAssetContributors   CheckID = "asset_contributors"
	CheckAssetRights         CheckID = "asset_rights"
	CheckAssetVisibility     CheckID = "asset_visibility"
	CheckAssetDependencyPins CheckID = "asset_dependency_pins"
	CheckAssetIntegrityHash  CheckID = "asset_integrity_hash"
	CheckAssetMetadata       CheckID = "asset_metadata"
	CheckAssetSchema         CheckID = "asset_schema"
)

// Severity is what a failed check means at its gate: a warning is reported
// and the transition may proceed; a blocking failure refuses it. The same
// check moves from warning to blocking as the ladder climbs — that move IS
// the progressive strictness.
type Severity string

const (
	// SeverityWarning: the failure is reported, nothing is refused.
	SeverityWarning Severity = "warning"
	// SeverityBlocking: the failure refuses the transition the gate
	// guards.
	SeverityBlocking Severity = "blocking"
)

// Spec declares one check at one gate: what is checked, at which severity,
// and why — the "why" is part of the declaration, so every report can
// explain its verdict without consulting anything else.
type Spec struct {
	Check    CheckID
	Severity Severity
	// Why is the human rationale for running this check at this gate
	// (and at this severity). It is rendered into reports and gate
	// explanations — the explainability acceptance criterion is met by
	// construction.
	Why string
}

// gateSpecs is the declarative ladder: for each gate, the ordered list of
// checks it runs. Monotonicity along gateOrder is enforced by
// TestGateSpecsLadderIsMonotone: a check that blocks at a gate can never
// warn at a stricter one, and every consecutive pair must differ.
var gateSpecs = map[Gate][]Spec{
	GateDraft: {
		{CheckSchemaKnown, SeverityBlocking, "a version must reference a schema that is actually registered; unknown schemas never pass silently (T0201)."},
		{CheckSchemaCore, SeverityBlocking, "every version must satisfy the CoreScientificObject shape — identity, project, title, lifecycle, authorship — even a draft is a real scientific object (docs/08)."},
		{CheckSchemaTyped, SeverityWarning, "a draft may lack domain fields (docs/08: Draft 可缺部分 domain field), so a payload that does not yet satisfy the object type's full schema is reported, not refused."},
		{CheckAuthoritativeFields, SeverityWarning, "server-authoritative fields (id, version, schema_ref, lifecycle, created_by, created_at, visibility_policy_id, type) must not be smuggled through the payload (docs/23 §3); at draft this is reported, not refused."},
		{CheckIdentityFields, SeverityBlocking, "the version row's identity facts — title, lifecycle state, creator, timestamp — must exist; without them the object cannot be cited or traced."},
		{CheckStateLinkage, SeverityWarning, "every version names the state transition it was created in (docs/07); a draft may still be gathering this, so it warns."},
		{CheckCommitLinkage, SeverityWarning, "every version should be named by its commit's operation summary (docs/09 §2); a draft may still be gathering this, so it warns."},
		{CheckPayloadIntegrity, SeverityWarning, "stored payload bytes must re-hash to their stored integrity hash (docs/23 §3); a mismatch at draft warns so it is caught before it matters."},
	},
	GatePR: {
		{CheckSchemaKnown, SeverityBlocking, "a proposed change must reference a real schema."},
		{CheckSchemaCore, SeverityBlocking, "a proposed change must satisfy the CoreScientificObject shape."},
		{CheckSchemaTyped, SeverityBlocking, "entering PR closes the draft allowance: the payload must satisfy the object type's full schema, including its required domain fields (docs/08)."},
		{CheckAuthoritativeFields, SeverityBlocking, "a proposed change must not smuggle server-authoritative fields through the payload (docs/23 §3)."},
		{CheckIdentityFields, SeverityBlocking, "the version row's identity facts must exist."},
		{CheckRequiredFields, SeverityWarning, "the docs/08 domain fields beyond the schema's required array (per-type table) should be present in a proposed change; they become blocking at main."},
		{CheckStateLinkage, SeverityBlocking, "a proposed change must name the state transition it was created in — the review diff is computed over linked states (docs/07)."},
		{CheckCommitLinkage, SeverityBlocking, "a proposed change must be named by its commit's operation summary, or the review cannot trace what changed (docs/09 §2)."},
		{CheckActorConsistency, SeverityWarning, "the version's creator should match the commit's actor; a mismatch warns here and blocks at main."},
		{CheckPayloadIntegrity, SeverityWarning, "stored payload bytes must re-hash to their stored integrity hash; a mismatch warns here and blocks at main."},
		{CheckChainIntegrity, SeverityWarning, "the branch state chain should be unbroken; a gap warns here and blocks at main (frozen main must be reconstructible)."},
		{CheckBranchEmpty, SeverityWarning, "the branch has no states to validate — nothing here can pass or fail; validate again after the first commit."},
	},
	GateMain: {
		{CheckSchemaKnown, SeverityBlocking, "main holds accepted research state; a version there must reference a real schema."},
		{CheckSchemaCore, SeverityBlocking, "main holds accepted research state; a version there must satisfy the CoreScientificObject shape."},
		{CheckSchemaTyped, SeverityBlocking, "main holds accepted research state; the payload must satisfy the object type's full schema."},
		{CheckAuthoritativeFields, SeverityBlocking, "main holds accepted research state; server-authoritative fields must never come from a payload (docs/23 §3)."},
		{CheckIdentityFields, SeverityBlocking, "the version row's identity facts must exist."},
		{CheckRequiredFields, SeverityBlocking, "the docs/08 domain fields beyond the schema's required array must be present before the state is accepted into main — the merge is the last chance to complete them."},
		{CheckStateLinkage, SeverityBlocking, "every version on main must name the state transition it was created in (docs/07)."},
		{CheckCommitLinkage, SeverityBlocking, "every version on main must be named by its commit's operation summary, and every version-creating operation must have its member — the two directions of traceability (docs/09 §2)."},
		{CheckActorConsistency, SeverityBlocking, "on main, the version's creator must match the commit's actor: provenance is accepted research state, not best effort."},
		{CheckPayloadIntegrity, SeverityBlocking, "on main, stored payload bytes must re-hash to their stored integrity hash — accepted state is content-addressed (docs/21 §10)."},
		{CheckChainIntegrity, SeverityBlocking, "frozen main must be reconstructible: the branch state chain has to be unbroken from its root to the head (docs/09 §3)."},
		{CheckBranchEmpty, SeverityWarning, "the branch has no states to validate — nothing here can pass or fail; validate again after the first commit."},
	},
	GateRelease: {
		{CheckSchemaKnown, SeverityBlocking, "a release pins schema versions (docs/11 §1); every member must reference a real schema."},
		{CheckSchemaCore, SeverityBlocking, "a release member must satisfy the CoreScientificObject shape."},
		{CheckSchemaTyped, SeverityBlocking, "a release member must satisfy the object type's full schema."},
		{CheckAuthoritativeFields, SeverityBlocking, "a release member's payload must not carry server-authoritative fields (docs/23 §3)."},
		{CheckIdentityFields, SeverityBlocking, "the version row's identity facts must exist."},
		{CheckRequiredFields, SeverityBlocking, "the docs/08 domain fields beyond the schema's required array must be present in a release."},
		{CheckStateLinkage, SeverityBlocking, "a release member must name its state transition (docs/07)."},
		{CheckCommitLinkage, SeverityBlocking, "a release member must be named by its commit's operation summary, and every version-creating operation must have its member (docs/09 §2)."},
		{CheckActorConsistency, SeverityBlocking, "a release member's creator must match the commit's actor."},
		{CheckPayloadIntegrity, SeverityBlocking, "a release member's stored bytes must re-hash to their stored integrity hash."},
		{CheckChainIntegrity, SeverityBlocking, "a release snapshots an accepted state; the chain behind it must be unbroken."},
		{CheckBranchEmpty, SeverityWarning, "the branch has no states to validate — nothing here can pass or fail; validate again after the first commit."},
		{CheckReleaseReview, SeverityBlocking, "a release fixes the review/approval record (docs/11 §1): scientific and integrity review must be recorded and approved before the snapshot exists."},
		{CheckReleaseRights, SeverityBlocking, "a release fixes the rights/policy snapshot (docs/11 §1): without it the immutable snapshot would not say who may see and reuse it."},
		{CheckReleaseFromMain, SeverityBlocking, "a release snapshots an accepted state on main (docs/11 §1); a state outside main is not accepted research state."},
	},
	GateAsset: {
		{CheckSchemaKnown, SeverityBlocking, "an asset member must reference a real schema."},
		{CheckSchemaCore, SeverityBlocking, "an asset member must satisfy the CoreScientificObject shape."},
		{CheckSchemaTyped, SeverityBlocking, "an asset member must satisfy the object type's full schema."},
		{CheckAuthoritativeFields, SeverityBlocking, "an asset member's payload must not carry server-authoritative fields (docs/23 §3)."},
		{CheckIdentityFields, SeverityBlocking, "the version row's identity facts must exist."},
		{CheckRequiredFields, SeverityBlocking, "the docs/08 domain fields beyond the schema's required array must be present in a published asset."},
		{CheckStateLinkage, SeverityBlocking, "an asset member must name its state transition (docs/07)."},
		{CheckCommitLinkage, SeverityBlocking, "an asset member must be named by its commit's operation summary, and every version-creating operation must have its member (docs/09 §2)."},
		{CheckActorConsistency, SeverityBlocking, "an asset member's creator must match the commit's actor."},
		{CheckPayloadIntegrity, SeverityBlocking, "an asset member's stored bytes must re-hash to their stored integrity hash."},
		{CheckChainIntegrity, SeverityBlocking, "a published asset descends from accepted state; the chain behind it must be unbroken."},
		{CheckBranchEmpty, SeverityWarning, "the branch has no states to validate — nothing here can pass or fail; validate again after the first commit."},
		{CheckReleaseReview, SeverityBlocking, "an asset descends from a release (docs/11 §4: source accepted state/release); the review record must exist."},
		{CheckReleaseRights, SeverityBlocking, "an asset descends from a release; the rights snapshot must exist."},
		{CheckReleaseFromMain, SeverityBlocking, "an asset's source must be accepted state on main."},
		{CheckAssetSourcePinned, SeverityBlocking, "docs/11 §4: the asset must pin its source accepted state/release — provenance of the published content."},
		{CheckAssetVersionPinned, SeverityBlocking, "docs/11 §4: the asset version must be fixed; Asset Versions are immutable and reference by exact version only."},
		{CheckAssetContributors, SeverityBlocking, "docs/11 §4: creators/contributors must be recorded — credit is part of the publication, not an afterthought (CLAUDE.md §9.14)."},
		{CheckAssetRights, SeverityBlocking, "docs/11 §4: rights/license must be set — a reusable asset without rights cannot be reused lawfully."},
		{CheckAssetVisibility, SeverityBlocking, "docs/11 §4: visibility must be set explicitly; publication controls visibility (CLAUDE.md §9.7)."},
		{CheckAssetDependencyPins, SeverityBlocking, "docs/11 §4: dependency pins must be present — Reference/Use means exact version pins (docs/11 §5)."},
		{CheckAssetIntegrityHash, SeverityBlocking, "docs/11 §4: the asset must carry its hash — consumers verify the bytes they got are the bytes that were published."},
		{CheckAssetMetadata, SeverityBlocking, "docs/11 §4: required metadata must be present — an asset without it is not discoverable or citable."},
		{CheckAssetSchema, SeverityBlocking, "docs/11 §4: the asset document must validate against the research-asset-version schema."},
	},
}

// GateSpecs returns the ordered check list of gate. The order is the
// report order; the content is the gate's whole strictness, declaration
// and explanation in one place.
func GateSpecs(gate Gate) []Spec {
	specs := gateSpecs[gate]
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// typeRequiredFields is the per-object-type domain-field table beyond the
// schemas' own required arrays: the docs/08 field lists distilled to the
// fields a pr gate reports (warning) and a main gate requires (blocking).
// Field names are the schema property names of specs/schemas/. Types not
// listed have no extra requirements — their schema required array is the
// whole requirement.
var typeRequiredFields = map[string][]string{
	"research_question":  {"purpose", "question_state"},
	"hypothesis":         {"hypothesis_type", "scope"},
	"material":           {"name", "identifiers"},
	"sample":             {"sample_code", "protocol_version_id", "physical_form"},
	"experiment":         {"operator_ids", "sample_ids", "protocol_version_id", "conditions", "result_summary"},
	"calculation":        {"software", "input_object_ids", "parameters", "output_blob_ids", "result_summary"},
	"dataset":            {"data_type", "blob_ids", "access_level", "quality_notes"},
	"protocol":           {"domain", "steps", "parameters", "requirements"},
	"claim":              {"subject_ref", "property", "scope", "assessment"},
	"finding":            {"finding_type", "assessment"},
	"external_reference": {"canonical_url", "accessed_at", "upstream_version", "snapshot_hash"},
}

// RequiredFieldsFor returns the docs/08 domain fields the gate ladder
// demands beyond the type schema's required array, for the given object
// type. The field list is the same at every gate — the ladder changes the
// severity (warning at pr, blocking at main and beyond), not the content.
// Unknown types have no extra requirements.
func RequiredFieldsFor(objectType string) []string {
	fields := typeRequiredFields[objectType]
	out := make([]string, len(fields))
	copy(out, fields)
	return out
}

// ExplainGate renders the gate's whole rationale: its stage on the ladder,
// its purpose, and every check with its severity and why. The text is what
// a Report's explanation is built from — a verdict can always be traced
// back to this declaration.
func ExplainGate(gate Gate) string {
	stage, ok := Stage(gate)
	if !ok {
		return fmt.Sprintf("gate %q is not canonical", gate)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gate %s (stage %d of %d)\n", gate, stage, len(gateOrder))
	b.WriteString(gatePurpose[gate])
	b.WriteString("\nchecks:\n")
	for _, spec := range GateSpecs(gate) {
		fmt.Fprintf(&b, "  [%s] %s — %s\n", spec.Severity, spec.Check, spec.Why)
	}
	return b.String()
}

// gatePurpose is the one-line purpose of each gate, rendered by
// ExplainGate.
var gatePurpose = map[Gate]string{
	GateDraft:   "purpose: hold an everyday branch commit — structural validity, everything else reported as warnings (docs/08: a Draft may lack domain fields).",
	GatePR:      "purpose: hold a branch that is about to be proposed for review — domain fields and state/commit linkage become blocking (docs/22 §7).",
	GateMain:    "purpose: hold a branch that is about to merge into frozen main — provenance completeness becomes blocking (docs/09 §3).",
	GateRelease: "purpose: hold an accepted state that is about to become an immutable release snapshot — review, rights and main-branch origin become blocking (docs/11 §1).",
	GateAsset:   "purpose: hold a research asset version about to be published — the full docs/11 §4 checklist becomes blocking.",
}

// StrictnessDiff explains exactly why to is stricter than from: which
// checks moved from warning to blocking, which checks were added, and
// which stayed the same. An empty diff means the two gates are the same
// strictness — which the ladder forbids between neighbours, and
// TestGatesDifferInStrictnessAndSayWhy asserts.
func StrictnessDiff(from, to Gate) ([]string, error) {
	if !StricterThan(to, from) {
		return nil, fmt.Errorf("validation: %s is not stricter than %s", to, from)
	}
	fromSpecs := make(map[CheckID]Severity, len(gateSpecs[from]))
	for _, spec := range gateSpecs[from] {
		fromSpecs[spec.Check] = spec.Severity
	}
	var diff []string
	for _, spec := range gateSpecs[to] {
		prev, seen := fromSpecs[spec.Check]
		switch {
		case !seen:
			diff = append(diff, fmt.Sprintf("%s: added (%s)", spec.Check, spec.Severity))
		case prev == SeverityWarning && spec.Severity == SeverityBlocking:
			diff = append(diff, fmt.Sprintf("%s: warning -> blocking", spec.Check))
		}
	}
	return diff, nil
}
