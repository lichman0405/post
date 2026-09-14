package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Snapshot is the complete set of facts one gate run validates: the
// branch's persisted state chain and its members, plus the release/asset
// facts the later gates demand. It is assembled by the application layer
// from the persistence layer (internal/application/validation); this
// package never reads storage itself.
type Snapshot struct {
	// ProjectID is the research boundary every member belongs to; it is
	// injected into assembled entity documents (server-authoritative,
	// never from a payload).
	ProjectID string
	// BranchID is the branch being validated.
	BranchID string
	// Head is the branch's head state; nil when the branch has no states
	// yet (an empty branch validates with branch_empty warnings).
	Head *domain.ProjectState
	// States is the branch's state chain (every state whose branch is
	// this branch), including Head.
	States []domain.ProjectState
	// Commits is the branch's commit history.
	Commits []domain.StateCommit
	// ObjectVersions is every scientific object version the branch's
	// states created — the snapshot's object members.
	ObjectVersions []domain.ScientificObjectVersion
	// RelationVersions is every relation version the branch's states
	// created.
	RelationVersions []domain.RelationVersion
	// Release holds the release-domain facts (docs/11 §1); nil means
	// "not provided", which the release and asset gates fail with the
	// facts-missing explanation.
	Release *ReleaseFacts
	// Asset holds the asset-domain facts (docs/11 §4); nil means "not
	// provided" and fails the asset gate.
	Asset *AssetFacts
}

// ReleaseFacts carries the release-domain facts the release gate checks
// (docs/11 §1): the review/approval record, the rights/policy snapshot,
// and the accepted main-branch origin. It is filled by the release domain
// service (a later task); the gate engine only checks it.
type ReleaseFacts struct {
	// ReviewApproved: the scientific and integrity reviews are recorded
	// and approved.
	ReviewApproved bool
	// RightsSnapshot: the rights/policy snapshot is fixed for the
	// release.
	RightsSnapshot bool
	// FromMainBranch: the release's source state is accepted state on the
	// project's main branch.
	FromMainBranch bool
}

// AssetFacts carries the asset-domain facts the asset gate checks
// (docs/11 §4): source pin, fixed version, contributors, rights/license,
// visibility, dependency pins, hash, required metadata, and the asset
// document itself for schema validation.
type AssetFacts struct {
	SourcePinned   bool
	VersionPinned  bool
	Contributors   bool
	Rights         bool
	Visibility     bool
	DependencyPins bool
	IntegrityHash  bool
	Metadata       bool
	// Document is the research-asset-version entity document, validated
	// against the schema named by Ref by the asset_schema check.
	Document json.RawMessage
	// Ref addresses the asset schema (the canonical
	// research-asset-version schema unless an extension registers its
	// own).
	Ref schemareg.Ref
}

// authoritativeFields are the entity-document fields the server derives
// (docs/23 §3, docs/12 §3): a payload carrying any of them is smuggling
// server-authoritative content, which the authoritative_fields check
// refuses.
var authoritativeFields = map[string]bool{
	"id": true, "version": true, "project_id": true, "branch_id": true,
	"title": true, "lifecycle_state": true, "schema_ref": true,
	"created_by": true, "created_at": true, "visibility_policy_id": true,
	"type": true,
}

// Validator runs the five progressive gates over snapshots. It is pure:
// everything it needs (registry, snapshot) is injected; it never reads or
// writes storage. Concurrent use is safe.
type Validator struct {
	reg *schemareg.Registry
}

// NewValidator wires the validator over the schema registry.
func NewValidator(reg *schemareg.Registry) *Validator {
	return &Validator{reg: reg}
}

// Validate runs gate over snap and returns the full report: every check of
// the gate's spec, per subject, with severity and why, and the derived
// verdict and explanation. An unknown gate is a caller error: the report
// carries a single blocking result naming it, so it can never be mistaken
// for a pass.
func (v *Validator) Validate(gate Gate, snap Snapshot) Report {
	if !ValidGate(gate) {
		return Report{
			Gate:    gate,
			Results: []Result{{Check: "gate", Severity: SeverityBlocking, Passed: false, Detail: fmt.Sprintf("gate %q is not canonical (draft, pr, main, release, asset)", gate), Why: "an unknown gate can never pass silently."}},
		}.finalized()
	}
	report := Report{Gate: gate}
	for _, spec := range GateSpecs(gate) {
		v.runCheck(&report, spec, snap)
	}
	report.finalize()
	return report
}

// finalized is the internal counterpart of Validate's finalize step that
// works on a value.
func (r Report) finalized() Report {
	r.finalize()
	return r
}

// runCheck executes one spec against the snapshot and appends its results.
// Per-object checks fan out to one result per subject; snapshot-level
// checks produce one result.
func (v *Validator) runCheck(report *Report, spec Spec, snap Snapshot) {
	switch spec.Check {
	case CheckSchemaKnown, CheckSchemaCore, CheckSchemaTyped,
		CheckAuthoritativeFields, CheckIdentityFields, CheckRequiredFields,
		CheckPayloadIntegrity:
		for _, mv := range snap.ObjectVersions {
			report.Results = append(report.Results, v.checkObject(spec, snap, mv))
		}
	case CheckStateLinkage:
		report.Results = append(report.Results, checkStateLinkage(spec, snap))
	case CheckCommitLinkage:
		report.Results = append(report.Results, checkCommitLinkage(spec, snap))
	case CheckActorConsistency:
		report.Results = append(report.Results, checkActorConsistency(spec, snap))
	case CheckChainIntegrity:
		report.Results = append(report.Results, checkChainIntegrity(spec, snap))
	case CheckBranchEmpty:
		report.Results = append(report.Results, checkBranchEmpty(spec, snap))
	case CheckReleaseReview:
		report.Results = append(report.Results, checkReleaseFact(spec, snap, func(f *ReleaseFacts) (bool, string) {
			return f.ReviewApproved, "scientific and integrity review must be recorded and approved (docs/11 §1)"
		}))
	case CheckReleaseRights:
		report.Results = append(report.Results, checkReleaseFact(spec, snap, func(f *ReleaseFacts) (bool, string) {
			return f.RightsSnapshot, "the rights/policy snapshot must be fixed (docs/11 §1)"
		}))
	case CheckReleaseFromMain:
		report.Results = append(report.Results, checkReleaseFact(spec, snap, func(f *ReleaseFacts) (bool, string) {
			return f.FromMainBranch, "the release must snapshot accepted state on main (docs/11 §1)"
		}))
	case CheckAssetSourcePinned, CheckAssetVersionPinned, CheckAssetContributors,
		CheckAssetRights, CheckAssetVisibility, CheckAssetDependencyPins,
		CheckAssetIntegrityHash, CheckAssetMetadata:
		report.Results = append(report.Results, checkAssetFact(spec, snap))
	case CheckAssetSchema:
		report.Results = append(report.Results, v.checkAssetSchema(spec, snap))
	}
}

// subject renders the per-object subject label of a version: the object
// type, the version number and the version id — enough to find the row,
// never storage detail.
func (v *Validator) subject(mv domain.ScientificObjectVersion) string {
	return fmt.Sprintf("%s v%d (%s)", objectTypeName(mv.SchemaID), mv.VersionNo, mv.ObjectID)
}

// objectTypeName derives the human type name from the schema id: the last
// path segment without the .schema.json suffix. It is only a label — the
// authoritative type is the schema's const (assembled into the document).
func objectTypeName(schemaID string) string {
	name := schemaID
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSuffix(name, ".schema.json")
	if name == "" {
		name = "object"
	}
	return name
}

// checkObject runs one per-object check against one version. The document
// under validation is assembled from the version row plus the stored
// payload — the server's own authoritative facts, never a caller-supplied
// whole document.
func (v *Validator) checkObject(spec Spec, snap Snapshot, mv domain.ScientificObjectVersion) Result {
	ref := schemareg.Ref{ID: mv.SchemaID, Version: mv.SchemaVersion}
	switch spec.Check {
	case CheckSchemaKnown:
		_, err := v.reg.Lookup(ref)
		if err != nil {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: v.subject(mv), Detail: fmt.Sprintf("schema %s is not registered: %v", ref, err), Why: spec.Why}
		}
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: v.subject(mv), Why: spec.Why}
	case CheckAuthoritativeFields:
		return checkAuthoritativeFields(spec, v.subject(mv), mv.Payload)
	case CheckIdentityFields:
		return checkIdentityFields(spec, v.subject(mv), mv)
	case CheckPayloadIntegrity:
		return checkPayloadIntegrity(spec, v.subject(mv), mv)
	case CheckSchemaCore, CheckSchemaTyped, CheckRequiredFields:
		// These need the assembled document; the schema_known gate above
		// (same spec order) already refused unknown schemas, so here an
		// unknown schema yields one clear failure instead of a cascade.
		if _, err := v.reg.Lookup(ref); err != nil {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: v.subject(mv), Detail: fmt.Sprintf("schema %s is not registered, cannot validate the document", ref), Why: spec.Why}
		}
		doc := v.assembleDoc(snap, mv)
		switch spec.Check {
		case CheckSchemaCore:
			return v.checkSchema(spec, v.subject(mv), schemareg.Ref{ID: schemareg.CanonicalNamespace + "core-scientific-object.schema.json", Version: schemareg.CanonicalV1}, doc)
		case CheckSchemaTyped:
			return v.checkSchema(spec, v.subject(mv), ref, doc)
		default:
			// The authoritative type is the schema's const, never the id's
			// label: a runtime-registered extension schema (T0201 Register)
			// may have any id, and the docs/08 table only knows the
			// canonical type names. A schema that declares no const cannot
			// be mapped to the table — fail closed with an explicit
			// failure instead of silently passing a blocking gate.
			typeConst, ok := v.reg.TypeConst(ref)
			if !ok {
				return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: v.subject(mv), Detail: fmt.Sprintf("schema %s declares no authoritative type (properties.type.const); the docs/08 required fields cannot be checked", ref), Why: spec.Why}
			}
			return checkRequiredFields(spec, v.subject(mv), doc, typeConst)
		}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: v.subject(mv), Why: spec.Why}
}

// checkSchema validates the assembled entity document against the named
// schema.
func (v *Validator) checkSchema(spec Spec, subject string, ref schemareg.Ref, doc map[string]any) Result {
	raw, err := json.Marshal(doc)
	if err != nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: fmt.Sprintf("document does not marshal: %v", err), Why: spec.Why}
	}
	if err := v.reg.Validate(ref, raw); err != nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: err.Error(), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// assembleDoc builds the entity document a schema validates: the version
// row's server-authoritative facts plus the stored payload's content
// fields. Payload keys that collide with authoritative fields are NOT
// merged — the row wins, and the collision is reported by the
// authoritative_fields check.
func (v *Validator) assembleDoc(snap Snapshot, mv domain.ScientificObjectVersion) map[string]any {
	doc := map[string]any{
		"id":              mv.ObjectID,
		"version":         mv.VersionNo,
		"project_id":      snap.ProjectID,
		"title":           mv.Title,
		"lifecycle_state": string(mv.LifecycleState),
		"schema_ref":      map[string]any{"id": mv.SchemaID, "version": mv.SchemaVersion},
		"created_by":      mv.CreatedBy,
		"created_at":      mv.CreatedAt.Format(time.RFC3339),
	}
	if mv.BranchID != nil {
		doc["branch_id"] = *mv.BranchID
	}
	if mv.VisibilityPolicyID != nil {
		doc["visibility_policy_id"] = *mv.VisibilityPolicyID
	}
	if typeConst, ok := v.reg.TypeConst(schemareg.Ref{ID: mv.SchemaID, Version: mv.SchemaVersion}); ok {
		doc["type"] = typeConst
	}
	var payload map[string]any
	if err := json.Unmarshal(mv.Payload, &payload); err == nil {
		for k, val := range payload {
			if authoritativeFields[k] {
				continue // the row wins (docs/23 §3)
			}
			doc[k] = val
		}
	}
	return doc
}

// checkAuthoritativeFields reports payloads that smuggle server-authoritative
// fields (docs/23 §3): the assembled document ignores them, but their
// presence means the client tried to control server truth.
func checkAuthoritativeFields(spec Spec, subject string, payload json.RawMessage) Result {
	var content map[string]any
	if err := json.Unmarshal(payload, &content); err != nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: fmt.Sprintf("payload is not a JSON object: %v", err), Why: spec.Why}
	}
	var found []string
	for k := range content {
		if authoritativeFields[k] {
			found = append(found, k)
		}
	}
	if len(found) > 0 {
		sort.Strings(found)
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: fmt.Sprintf("payload carries server-authoritative field(s) %s (ignored by the server; remove them)", strings.Join(found, ", ")), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// checkIdentityFields checks the row-level identity facts (docs/08
// §CoreScientificObject): title, lifecycle state, creator, timestamp.
func checkIdentityFields(spec Spec, subject string, mv domain.ScientificObjectVersion) Result {
	var missing []string
	if strings.TrimSpace(mv.Title) == "" {
		missing = append(missing, "title")
	}
	if !domain.ValidLifecycleState(string(mv.LifecycleState)) {
		missing = append(missing, "lifecycle_state")
	}
	if mv.CreatedBy == "" {
		missing = append(missing, "created_by")
	}
	if mv.CreatedAt.IsZero() {
		missing = append(missing, "created_at")
	}
	if len(missing) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: "missing identity fact(s): " + strings.Join(missing, ", "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// checkRequiredFields checks the per-type docs/08 domain fields beyond the
// schema's required array against the assembled document.
func checkRequiredFields(spec Spec, subject string, doc map[string]any, objectType string) Result {
	required := RequiredFieldsFor(objectType)
	var missing []string
	for _, field := range required {
		val, ok := doc[field]
		if !ok || isZeroValue(val) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: fmt.Sprintf("%s: missing docs/08 field(s): %s", objectType, strings.Join(missing, ", ")), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// checkPayloadIntegrity recomputes the sha256 of the stored payload bytes
// and compares it to the row's stored integrity hash. Stored payload bytes
// are already PostgreSQL-canonical (the store canonicalizes before insert),
// so the recomputation is exact for stored rows.
func checkPayloadIntegrity(spec Spec, subject string, mv domain.ScientificObjectVersion) Result {
	if mv.IntegrityHash == "" {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: "no integrity hash stored", Why: spec.Why}
	}
	sum := sha256.Sum256(mv.Payload)
	if hex.EncodeToString(sum[:]) != mv.IntegrityHash {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Subject: subject, Detail: "stored payload bytes do not re-hash to the stored integrity hash", Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// checkStateLinkage checks that every member names a state of the branch's
// chain.
func checkStateLinkage(spec Spec, snap Snapshot) Result {
	chain := stateIDSet(snap)
	var unlinked []string
	for _, mv := range snap.ObjectVersions {
		if !chain[mv.StateID] {
			unlinked = append(unlinked, fmt.Sprintf("%s v%d (state %s)", objectTypeName(mv.SchemaID), mv.VersionNo, mv.StateID))
		}
	}
	for _, rv := range snap.RelationVersions {
		if !chain[rv.StateID] {
			unlinked = append(unlinked, fmt.Sprintf("relation v%d (state %s)", rv.VersionNo, rv.StateID))
		}
	}
	if len(unlinked) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "member(s) name a state outside the branch chain: " + strings.Join(unlinked, ", "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkCommitLinkage checks the two directions of traceability (docs/09
// §2): every member is named by some commit's operation summary, and every
// version-creating operation names a member of the snapshot.
func checkCommitLinkage(spec Spec, snap Snapshot) Result {
	type opKey struct {
		entity string
		verNo  int
	}
	ops := make(map[opKey]bool)
	for _, c := range snap.Commits {
		var summary []domain.StateOperation
		if err := json.Unmarshal(c.OperationSummary, &summary); err != nil {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("commit %s: operation summary is not parseable: %v", c.ID, err), Why: spec.Why}
		}
		for _, op := range summary {
			ops[opKey{op.EntityID, op.VersionNo}] = true
		}
	}
	var unnamed []string
	for _, mv := range snap.ObjectVersions {
		if !ops[opKey{mv.ObjectID, mv.VersionNo}] {
			unnamed = append(unnamed, fmt.Sprintf("%s v%d (%s)", objectTypeName(mv.SchemaID), mv.VersionNo, mv.ObjectID))
		}
	}
	for _, rv := range snap.RelationVersions {
		if !ops[opKey{rv.RelationID, rv.VersionNo}] {
			unnamed = append(unnamed, fmt.Sprintf("relation v%d (%s)", rv.VersionNo, rv.RelationID))
		}
	}
	if len(unnamed) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "member(s) not named by any commit's operation summary: " + strings.Join(unnamed, ", "), Why: spec.Why}
	}
	// The reverse direction: every object_version_created /
	// relation_version_created operation must have its member row.
	members := make(map[opKey]bool)
	for _, mv := range snap.ObjectVersions {
		members[opKey{mv.ObjectID, mv.VersionNo}] = true
	}
	for _, rv := range snap.RelationVersions {
		members[opKey{rv.RelationID, rv.VersionNo}] = true
	}
	var dangling []string
	for _, c := range snap.Commits {
		var summary []domain.StateOperation
		if err := json.Unmarshal(c.OperationSummary, &summary); err != nil {
			continue // reported above
		}
		for _, op := range summary {
			switch op.Kind {
			case domain.OperationObjectVersionCreated, domain.OperationRelationVersionCreated:
				if !members[opKey{op.EntityID, op.VersionNo}] {
					dangling = append(dangling, fmt.Sprintf("%s %s v%d (commit %s)", op.Kind, op.EntityID, op.VersionNo, c.ID))
				}
			}
		}
	}
	if len(dangling) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "operation(s) without their member row: " + strings.Join(dangling, ", "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkActorConsistency checks that a member's creator is the actor of the
// commit whose result state the member was created in.
func checkActorConsistency(spec Spec, snap Snapshot) Result {
	var mismatched []string
	for _, mv := range snap.ObjectVersions {
		actor, ok := commitActorFor(snap, mv.StateID)
		if !ok {
			continue // linkage failures are reported by their own check
		}
		if actor != mv.CreatedBy {
			mismatched = append(mismatched, fmt.Sprintf("%s v%d: created_by %s, commit actor %s", objectTypeName(mv.SchemaID), mv.VersionNo, mv.CreatedBy, actor))
		}
	}
	if len(mismatched) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "member creator differs from the commit actor: " + strings.Join(mismatched, "; "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// commitActorFor finds the actor of the commit whose result state is
// stateID.
func commitActorFor(snap Snapshot, stateID string) (string, bool) {
	for _, c := range snap.Commits {
		if c.ResultStateID == stateID {
			return c.ActorID, true
		}
	}
	return "", false
}

// checkChainIntegrity verifies the branch state chain is unbroken: walking
// parents from the head terminates at a single root, every state is an
// ancestor of the head, and every commit's base→result edge matches the
// states' parent edges.
func checkChainIntegrity(spec Spec, snap Snapshot) Result {
	if snap.Head == nil {
		if len(snap.States) == 0 {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
		}
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "states exist but the head is missing", Why: spec.Why}
	}
	byID := make(map[string]domain.ProjectState, len(snap.States))
	for _, s := range snap.States {
		byID[s.ID] = s
	}
	if _, ok := byID[snap.Head.ID]; !ok && len(snap.States) > 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("head %s is not among the branch's states", snap.Head.ID), Why: spec.Why}
	}
	// Walk from the head; the walk exits the set through exactly one
	// state (the root), whose parent is nil or outside the set — a linear
	// walk has exactly one exit, and the all-ancestors check below catches
	// every fork.
	walked := make(map[string]bool)
	cur := snap.Head.ID
	for {
		walked[cur] = true
		s, ok := byID[cur]
		if !ok {
			// The head's chain references a state this snapshot does not
			// contain — a gap.
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("state %s is missing from the branch chain (broken parent link)", cur), Why: spec.Why}
		}
		if s.ParentStateID == nil {
			break // the root: nothing before it
		}
		if _, inSet := byID[*s.ParentStateID]; !inSet {
			break // the boundary: the base state predates the branch
		}
		if walked[*s.ParentStateID] {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("parent cycle at state %s", cur), Why: spec.Why}
		}
		cur = *s.ParentStateID
	}
	// Every state must be an ancestor of the head — a state outside the
	// walk is a fork.
	for id := range byID {
		if !walked[id] {
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("state %s is not an ancestor of the head (forked or dangling chain)", id), Why: spec.Why}
		}
	}
	// Commit edges must match the states' parent edges: the base a commit
	// was built on is exactly the state's parent (the two are written from
	// the same transition; tests/integration/state_commit_test.go pins the
	// invariant).
	for _, c := range snap.Commits {
		rs, ok := byID[c.ResultStateID]
		if !ok {
			continue // the missing state is reported by the head/gap checks
		}
		if (rs.ParentStateID == nil) != (c.BaseStateID == nil) ||
			(rs.ParentStateID != nil && *rs.ParentStateID != *c.BaseStateID) {
			base, parent := "<none>", "<none>"
			if c.BaseStateID != nil {
				base = *c.BaseStateID
			}
			if rs.ParentStateID != nil {
				parent = *rs.ParentStateID
			}
			return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("commit %s base→result edge (%s→%s) does not match the state's parent edge (%s→%s)", c.ID, base, c.ResultStateID, parent, rs.ID), Why: spec.Why}
		}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkBranchEmpty observes the empty-branch case: nothing to validate.
func checkBranchEmpty(spec Spec, snap Snapshot) Result {
	if len(snap.States) == 0 && snap.Head == nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "the branch has no states to validate", Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkReleaseFact runs one release-gate fact check; a nil ReleaseFacts
// means the release domain did not supply facts, which fails the check.
func checkReleaseFact(spec Spec, snap Snapshot, probe func(*ReleaseFacts) (bool, string)) Result {
	if snap.Release == nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "release facts were not provided", Why: spec.Why}
	}
	ok, missing := probe(snap.Release)
	if !ok {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: missing, Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkAssetFact runs one asset-gate fact check; a nil AssetFacts means
// the asset domain did not supply facts, which fails the check.
func checkAssetFact(spec Spec, snap Snapshot) Result {
	if snap.Asset == nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "asset facts were not provided", Why: spec.Why}
	}
	facts := snap.Asset
	field := map[CheckID]struct {
		ok   bool
		name string
	}{
		CheckAssetSourcePinned:   {facts.SourcePinned, "the asset must pin its source accepted state/release"},
		CheckAssetVersionPinned:  {facts.VersionPinned, "the asset version must be fixed and immutable"},
		CheckAssetContributors:   {facts.Contributors, "creators/contributors must be recorded"},
		CheckAssetRights:         {facts.Rights, "rights/license must be set"},
		CheckAssetVisibility:     {facts.Visibility, "visibility must be set"},
		CheckAssetDependencyPins: {facts.DependencyPins, "dependency pins must be present"},
		CheckAssetIntegrityHash:  {facts.IntegrityHash, "the asset must carry its integrity hash"},
		CheckAssetMetadata:       {facts.Metadata, "required metadata must be present"},
	}[spec.Check]
	if !field.ok {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: field.name, Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkAssetSchema validates the asset document against the asset schema
// (docs/11 §4: schema validation is part of the publish checklist).
func (v *Validator) checkAssetSchema(spec Spec, snap Snapshot) Result {
	if snap.Asset == nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "asset facts were not provided", Why: spec.Why}
	}
	if len(snap.Asset.Document) == 0 {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: "the asset document was not provided", Why: spec.Why}
	}
	if err := v.reg.Validate(snap.Asset.Ref, snap.Asset.Document); err != nil {
		return Result{Check: spec.Check, Severity: spec.Severity, Passed: false, Detail: err.Error(), Why: spec.Why}
	}
	return Result{Check: spec.Check, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// stateIDSet builds the branch chain's state id set.
func stateIDSet(snap Snapshot) map[string]bool {
	set := make(map[string]bool, len(snap.States)+1)
	for _, s := range snap.States {
		set[s.ID] = true
	}
	if snap.Head != nil {
		set[snap.Head.ID] = true
	}
	return set
}

// isZeroValue reports whether val is an absent-equivalent JSON value (nil,
// empty string, empty array, empty object).
func isZeroValue(val any) bool {
	switch v := val.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}
