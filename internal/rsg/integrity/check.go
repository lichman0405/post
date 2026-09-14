package integrity

import "fmt"

// Dimension names one of the six canonical integrity dimensions of the
// task (schema, provenance, dependency, rights, visibility, blob). The
// report groups results by dimension, and the PR page renders one section
// per dimension.
type Dimension string

const (
	// DimensionSchema: do the proposal's objects reference real schemas
	// and satisfy them (docs/21 §8, internal/rsg/schemareg).
	DimensionSchema Dimension = "schema"
	// DimensionProvenance: is the proposal's history traceable — pinned
	// states on real branch chains, an unbroken state chain, complete
	// commit linkage, intact stored payload bytes (docs/07 §5, docs/09 §2).
	DimensionProvenance Dimension = "provenance"
	// DimensionDependency: does the proposal's relation graph hold —
	// known edge types, version-pinned endpoints that exist in the
	// proposal, endpoint types the catalog allows (docs/07 §3, docs/44).
	DimensionDependency Dimension = "dependency"
	// DimensionRights: do the proposal's rights/visibility policy pins
	// resolve to real policy versions of the project or its organization
	// (docs/12 §5).
	DimensionRights Dimension = "rights"
	// DimensionVisibility: does the proposal move visibility anywhere
	// without explicit human confirmation (docs/12 §3: 不扩大可见性 —
	// any private→public, restricted→open needs an authorized person;
	// the machine flags, the human review decision records).
	DimensionVisibility Dimension = "visibility"
	// DimensionBlob: do the proposal's payload blob references resolve
	// to blobs the proposal actually knows (docs/07 §2, docs/17).
	DimensionBlob Dimension = "blob"
)

// ValidDimension reports whether d is one of the six canonical dimensions.
func ValidDimension(d Dimension) bool {
	switch d {
	case DimensionSchema, DimensionProvenance, DimensionDependency,
		DimensionRights, DimensionVisibility, DimensionBlob:
		return true
	}
	return false
}

// CheckID names one check of the integrity engine. The ids are the stable
// vocabulary of every report: the same id renders the same meaning on the
// API and the PR page, so a reviewer can name a failure ("relation
// endpoints do not exist in the proposal") without consulting anything
// else.
type CheckID string

const (
	// schema dimension.
	CheckSchemaRegistered      CheckID = "schema_registered"
	CheckSchemaPayloadConforms CheckID = "schema_payload_conforms"
	// provenance dimension.
	CheckBaseOnTargetChain     CheckID = "base_on_target_chain"
	CheckProposedOnSourceChain CheckID = "proposed_on_source_chain"
	CheckSourceChainUnbroken   CheckID = "source_chain_unbroken"
	CheckCommitLinkage         CheckID = "commit_linkage"
	CheckPayloadIntegrity      CheckID = "payload_integrity"
	// dependency dimension.
	CheckRelationTypeKnown           CheckID = "relation_type_known"
	CheckRelationEndpointsInProposal CheckID = "relation_endpoints_in_proposal"
	CheckRelationEndpointTypesValid  CheckID = "relation_endpoint_types_valid"
	// rights dimension.
	CheckRightsPolicyResolves     CheckID = "rights_policy_resolves"
	CheckRightsPinInheritsDefault CheckID = "rights_pin_inherits_default"
	// visibility dimension.
	CheckVisibilityChangeFlagged CheckID = "visibility_change_flagged"
	// blob dimension.
	CheckBlobRefsResolve CheckID = "blob_refs_resolve"
)

// Severity is what a failed check means for the proposal: a warning is
// reported and nothing is refused; a blocking failure blocks the proposal
// — the integrity review reports "blocked" and the merge governance
// (T0409) refuses to proceed on it.
type Severity string

const (
	// SeverityWarning: the failure is reported, nothing is refused.
	SeverityWarning Severity = "warning"
	// SeverityBlocking: the failure blocks the proposal.
	SeverityBlocking Severity = "blocking"
)

// Spec declares one check: its dimension, its severity, and why the check
// runs at that severity — the "why" is part of the declaration, so every
// report can explain its verdict without consulting anything else (the
// same discipline as the gate ladder's Spec in internal/rsg/validation).
type Spec struct {
	Check     CheckID
	Dimension Dimension
	Severity  Severity
	// Why is the human rationale for running this check at this severity.
	// It is rendered into every result, so the PR page's checks section
	// explains itself.
	Why string
}

// specs is the declarative check list, in report order. Per-object checks
// fan out to one result per subject in the canonical object/relation
// order; snapshot-level checks produce one result.
var specs = []Spec{
	{CheckSchemaRegistered, DimensionSchema, SeverityBlocking,
		"a proposed object version must reference a schema that is actually registered; an unknown schema would make the proposal unreviewable (docs/21 §8)."},
	{CheckSchemaPayloadConforms, DimensionSchema, SeverityBlocking,
		"a proposed object version's payload must satisfy its registered schema — a proposal that carries schema-invalid content must not merge."},
	{CheckBaseOnTargetChain, DimensionProvenance, SeverityBlocking,
		"the PR's pinned base state must lie on the target branch's chain — the base is fixed from the target's head at creation (migration 00051), and a base outside the target's history would make the proposal unmeasurable."},
	{CheckProposedOnSourceChain, DimensionProvenance, SeverityBlocking,
		"the PR's pinned proposed state must lie on the source branch's chain — the proposal is the source branch's own history, never a foreign state."},
	{CheckSourceChainUnbroken, DimensionProvenance, SeverityBlocking,
		"the source branch's state chain must be unbroken from its root to its head: every parent link present, no cycles, no forks — the proposal's history must be reconstructible (docs/09 §3)."},
	{CheckCommitLinkage, DimensionProvenance, SeverityBlocking,
		"every state the source branch built must be named by its state commit, and every commit's base→result edge must match the state's parent edge — the proposal's traceability (docs/09 §2)."},
	{CheckPayloadIntegrity, DimensionProvenance, SeverityWarning,
		"stored payload bytes must re-hash to their stored integrity hash (docs/23 §3); a mismatch is reported here and blocks at the main gate — catch it before it matters."},
	{CheckRelationTypeKnown, DimensionDependency, SeverityBlocking,
		"every proposed relation must use a type of the relation catalog (docs/44); an unknown edge type degrades the graph to an uninterpretable link."},
	{CheckRelationEndpointsInProposal, DimensionDependency, SeverityBlocking,
		"every proposed relation must pin source and target object versions that exist in the proposal — version-pinned edges never dangle (docs/07 §3)."},
	{CheckRelationEndpointTypesValid, DimensionDependency, SeverityBlocking,
		"the pinned endpoints' object types must be ones the catalog allows for the relation type (docs/44); a mismatched edge makes no scientific sense."},
	{CheckRightsPolicyResolves, DimensionRights, SeverityBlocking,
		"every rights/visibility policy a proposed version pins must resolve to a real policy version of the project or its organization — a proposal that references a policy that does not exist cannot state its rights (docs/12 §5)."},
	{CheckRightsPinInheritsDefault, DimensionRights, SeverityWarning,
		"a proposed version without a policy pin inherits the project default (docs/07 §2 lists the visibility policy ref among a version's minimum fields); reported so the reviewer sees the effective rights explicitly."},
	{CheckVisibilityChangeFlagged, DimensionVisibility, SeverityBlocking,
		"every visibility/rights policy change between the base and the proposal is flagged for explicit human confirmation — the machine cannot prove a policy change is not a widening, and docs/12 §3 forbids widening without an authorized person (the PR review is where that confirmation is recorded)."},
	{CheckBlobRefsResolve, DimensionBlob, SeverityBlocking,
		"every blob reference in a proposed payload must resolve, by blob id or content hash, to a blob the proposal actually knows (its manifest blob refs) — a dangling data reference would merge unreviewable content (docs/17)."},
}

// Specs returns the check list in report order.
func Specs() []Spec {
	out := make([]Spec, len(specs))
	copy(out, specs)
	return out
}

// Explain renders the whole check list with severity and why — the text a
// report's explanation is built from, so a verdict can always be traced
// back to this declaration.
func Explain() string {
	var b []byte
	for _, spec := range specs {
		b = fmt.Appendf(b, "  [%s] %s/%s — %s\n", spec.Severity, spec.Dimension, spec.Check, spec.Why)
	}
	return string(b)
}
