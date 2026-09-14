package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

// Engine runs the integrity checks of one PR proposal. It is pure:
// everything it needs (schema registry, snapshot) is injected, it never
// reads or writes storage, and the same snapshot always derives the same
// report — inputs are sorted canonically, result order is the declaration
// order of specs, and every detail string is built from sorted keys.
// Concurrent use is safe.
type Engine struct {
	reg *schemareg.Registry
}

// New wires the engine over the schema registry.
func New(reg *schemareg.Registry) *Engine {
	return &Engine{reg: reg}
}

// Check runs every check of the spec table over snap and returns the full
// report: per-object checks fan out to one result per subject in canonical
// order, snapshot-level checks produce one result, and the verdict is
// derived from the results (finalize). The application layer stamps
// ComputedAt after the engine returns.
func (e *Engine) Check(snap Snapshot) Report {
	snap = snap.canonical()
	report := Report{
		Kind:            "integrity",
		ProjectID:       snap.ProjectID,
		PRNumber:        snap.PRNumber,
		BaseStateID:     snap.Base.ID,
		ProposedStateID: snap.Proposed.ID,
	}
	for _, spec := range specs {
		e.runCheck(&report, spec, snap)
	}
	report.finalize()
	return report
}

// canonical returns a copy of the snapshot whose slices are sorted by the
// manifest's canonical keys (object versions by (object_id, version_no,
// id), relation versions by (relation_id, version_no, id), blob refs by
// id), states and commits by id, and policy versions by id — the caller's
// slices are never reordered.
func (s Snapshot) canonical() Snapshot {
	s.BaseObjects = sortObjects(s.BaseObjects)
	s.ProposedObjects = sortObjects(s.ProposedObjects)
	s.BaseRelations = sortRelations(s.BaseRelations)
	s.ProposedRelations = sortRelations(s.ProposedRelations)
	s.BaseBlobRefs = sortBlobRefs(s.BaseBlobRefs)
	s.ProposedBlobRefs = sortBlobRefs(s.ProposedBlobRefs)
	s.TargetStates = sortStates(s.TargetStates)
	s.SourceStates = sortStates(s.SourceStates)
	s.SourceCommits = sortCommits(s.SourceCommits)
	s.PolicyVersions = sortPolicies(s.PolicyVersions)
	return s
}

func sortObjects(objs []manifest.ObjectVersion) []manifest.ObjectVersion {
	out := make([]manifest.ObjectVersion, len(objs))
	copy(out, objs)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		if a.VersionNo != b.VersionNo {
			return a.VersionNo < b.VersionNo
		}
		return a.ID < b.ID
	})
	return out
}

func sortRelations(rels []manifest.RelationVersion) []manifest.RelationVersion {
	out := make([]manifest.RelationVersion, len(rels))
	copy(out, rels)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.RelationID != b.RelationID {
			return a.RelationID < b.RelationID
		}
		if a.VersionNo != b.VersionNo {
			return a.VersionNo < b.VersionNo
		}
		return a.ID < b.ID
	})
	return out
}

func sortBlobRefs(refs []manifest.BlobRef) []manifest.BlobRef {
	out := make([]manifest.BlobRef, len(refs))
	copy(out, refs)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortStates(states []domain.ProjectState) []domain.ProjectState {
	out := make([]domain.ProjectState, len(states))
	copy(out, states)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortCommits(commits []domain.StateCommit) []domain.StateCommit {
	out := make([]domain.StateCommit, len(commits))
	copy(out, commits)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortPolicies(policies []domain.PolicyVersion) []domain.PolicyVersion {
	out := make([]domain.PolicyVersion, len(policies))
	copy(out, policies)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// runCheck executes one spec against the snapshot and appends its results.
// The switch mirrors the spec table order in check.go; per-object checks
// fan out in the canonical object/relation order.
func (e *Engine) runCheck(report *Report, spec Spec, snap Snapshot) {
	switch spec.Check {
	case CheckSchemaRegistered:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, e.checkSchemaRegistered(spec, ov))
		}
	case CheckSchemaPayloadConforms:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, e.checkSchemaConforms(spec, snap, ov))
		}
	case CheckBaseOnTargetChain:
		report.Results = append(report.Results, checkBaseOnTargetChain(spec, snap))
	case CheckProposedOnSourceChain:
		report.Results = append(report.Results, checkProposedOnSourceChain(spec, snap))
	case CheckSourceChainUnbroken:
		report.Results = append(report.Results, checkSourceChainUnbroken(spec, snap))
	case CheckCommitLinkage:
		report.Results = append(report.Results, checkCommitLinkage(spec, snap))
	case CheckPayloadIntegrity:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, checkPayloadIntegrity(spec, objectSubject(ov), ov.Payload, ov.IntegrityHash))
		}
		for _, rv := range snap.ProposedRelations {
			report.Results = append(report.Results, checkPayloadIntegrity(spec, relationSubject(rv), rv.Payload, rv.IntegrityHash))
		}
	case CheckRelationTypeKnown:
		for _, rv := range snap.ProposedRelations {
			report.Results = append(report.Results, checkRelationTypeKnown(spec, rv))
		}
	case CheckRelationEndpointsInProposal:
		for _, rv := range snap.ProposedRelations {
			report.Results = append(report.Results, checkRelationEndpoints(spec, snap, rv))
		}
	case CheckRelationEndpointTypesValid:
		for _, rv := range snap.ProposedRelations {
			report.Results = append(report.Results, checkRelationEndpointTypes(spec, snap, rv))
		}
	case CheckRightsPolicyResolves:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, checkRightsPolicyResolves(spec, snap, ov))
		}
	case CheckRightsPinInheritsDefault:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, checkRightsPinInheritsDefault(spec, ov))
		}
	case CheckVisibilityChangeFlagged:
		report.Results = append(report.Results, checkVisibilityChanges(spec, snap)...)
	case CheckBlobRefsResolve:
		for _, ov := range snap.ProposedObjects {
			report.Results = append(report.Results, checkBlobRefs(spec, snap, ov))
		}
	}
}

// objectSubject renders the per-object subject label: the object type, the
// version number and the object id — enough to find the version, never
// storage detail.
func objectSubject(ov manifest.ObjectVersion) string {
	return fmt.Sprintf("%s v%d (%s)", ov.ObjectType, ov.VersionNo, ov.ObjectID)
}

// relationSubject renders the per-relation subject label.
func relationSubject(rv manifest.RelationVersion) string {
	return fmt.Sprintf("relation v%d (%s)", rv.VersionNo, rv.RelationID)
}

// checkSchemaRegistered verifies the version's schema reference resolves in
// the registry. Unknown schemas always fail — there is no silent fallback.
func (e *Engine) checkSchemaRegistered(spec Spec, ov manifest.ObjectVersion) Result {
	ref := schemareg.Ref{ID: ov.SchemaRef.ID, Version: ov.SchemaRef.Version}
	if _, err := e.reg.Lookup(ref); err != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: fmt.Sprintf("schema %s is not registered: %v", ref, err), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
}

// checkSchemaConforms validates the assembled entity document (the version
// row's server-authoritative facts plus the stored payload's content
// fields) against the version's registered schema. An unknown schema fails
// here once (schema_registered already reported it) instead of cascading.
func (e *Engine) checkSchemaConforms(spec Spec, snap Snapshot, ov manifest.ObjectVersion) Result {
	ref := schemareg.Ref{ID: ov.SchemaRef.ID, Version: ov.SchemaRef.Version}
	if _, err := e.reg.Lookup(ref); err != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: fmt.Sprintf("schema %s is not registered, cannot validate the document", ref), Why: spec.Why}
	}
	doc := e.assembleDoc(snap, ov)
	raw, err := json.Marshal(doc)
	if err != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: fmt.Sprintf("document does not marshal: %v", err), Why: spec.Why}
	}
	if err := e.reg.Validate(ref, raw); err != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: err.Error(), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
}

// authoritativeFields mirrors internal/rsg/validation's table (docs/23
// §3): the entity-document fields the server derives from the version row.
// A payload key that collides with one of them is ignored here — the row
// wins — and the collision is reported by the payload's own checks.
var authoritativeFields = map[string]bool{
	"id": true, "version": true, "project_id": true, "branch_id": true,
	"title": true, "lifecycle_state": true, "schema_ref": true,
	"created_by": true, "created_at": true, "visibility_policy_id": true,
	"type": true,
}

// assembleDoc builds the entity document a schema validates, mirroring
// internal/rsg/validation's assembleDoc for the manifest's stored-form
// versions (docs/23 §3).
func (e *Engine) assembleDoc(snap Snapshot, ov manifest.ObjectVersion) map[string]any {
	doc := map[string]any{
		"id":              ov.ObjectID,
		"version":         ov.VersionNo,
		"project_id":      snap.ProjectID,
		"title":           ov.Title,
		"lifecycle_state": ov.LifecycleState,
		"schema_ref":      map[string]any{"id": ov.SchemaRef.ID, "version": ov.SchemaRef.Version},
		"created_by":      ov.CreatedBy,
		"created_at":      ov.CreatedAt.Format(time.RFC3339),
	}
	if ov.BranchID != nil {
		doc["branch_id"] = *ov.BranchID
	}
	if ov.VisibilityPolicyID != nil {
		doc["visibility_policy_id"] = *ov.VisibilityPolicyID
	}
	if typeConst, ok := e.reg.TypeConst(schemareg.Ref{ID: ov.SchemaRef.ID, Version: ov.SchemaRef.Version}); ok {
		doc["type"] = typeConst
	}
	var payload map[string]any
	if err := json.Unmarshal(ov.Payload, &payload); err == nil {
		for k, val := range payload {
			if authoritativeFields[k] {
				continue
			}
			doc[k] = val
		}
	}
	return doc
}

// checkBaseOnTargetChain verifies the PR's pinned base state lies on the
// target branch's chain — or is a legitimate boundary of THAT chain (the
// state the target roots from: its fork point, which for main may be the
// genesis root that no branch owns). Only TargetBoundaries is consulted:
// the source chain's boundaries are not the target's to accept.
func checkBaseOnTargetChain(spec Spec, snap Snapshot) Result {
	if stateIn(snap.TargetStates, snap.Base.ID) || snap.TargetBoundaries[snap.Base.ID] {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("base state %s is not on the target branch's chain", snap.Base.ID), Why: spec.Why}
}

// checkProposedOnSourceChain verifies the PR's pinned proposed state lies
// on the source branch's chain — or is a legitimate boundary of THAT chain
// (a source branch with no states of its own proposes its fork point).
// Only SourceBoundaries is consulted: the target chain's boundaries are
// not the source's to accept.
func checkProposedOnSourceChain(spec Spec, snap Snapshot) Result {
	if stateIn(snap.SourceStates, snap.Proposed.ID) || snap.SourceBoundaries[snap.Proposed.ID] {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("proposed state %s is not on the source branch's chain", snap.Proposed.ID), Why: spec.Why}
}

// checkSourceChainUnbroken verifies the source branch's state chain: a
// single head, a parent walk that terminates at a legitimate boundary (the
// fork point the chain roots from — resolved by the application layer),
// no parent cycles, no missing links, and no state outside the head's
// ancestor walk (forks).
func checkSourceChainUnbroken(spec Spec, snap Snapshot) Result {
	if len(snap.SourceStates) == 0 {
		// A source branch with no states of its own proposes only its fork
		// point (accepted by proposed_on_source_chain); there is no chain
		// to break.
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Why: spec.Why}
	}
	byID := make(map[string]domain.ProjectState, len(snap.SourceStates))
	for _, s := range snap.SourceStates {
		byID[s.ID] = s
	}
	// The head is the unique state nobody names as parent.
	children := make(map[string]bool, len(snap.SourceStates))
	for _, s := range snap.SourceStates {
		if s.ParentStateID != nil {
			children[*s.ParentStateID] = true
		}
	}
	var heads []string
	for _, s := range snap.SourceStates {
		if !children[s.ID] {
			heads = append(heads, s.ID)
		}
	}
	sort.Strings(heads)
	if len(heads) != 1 {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("the source chain has %d head(s), expected exactly one: %s", len(heads), strings.Join(heads, ", ")), Why: spec.Why}
	}
	// Walk parents from the head; the walk exits the set through exactly
	// one state (the root), whose parent must be a resolved boundary.
	walked := make(map[string]bool, len(snap.SourceStates))
	cur := heads[0]
	for {
		if walked[cur] {
			return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("parent cycle at state %s", cur), Why: spec.Why}
		}
		walked[cur] = true
		s := byID[cur]
		if s.ParentStateID == nil {
			return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("state %s has no parent and is not a resolved branch boundary", cur), Why: spec.Why}
		}
		if _, inSet := byID[*s.ParentStateID]; !inSet {
			if snap.SourceBoundaries[*s.ParentStateID] {
				break // the fork point: the chain roots from it
			}
			return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("state %s's parent %s is outside the source chain and is not a resolved branch boundary (broken parent link)", cur, *s.ParentStateID), Why: spec.Why}
		}
		cur = *s.ParentStateID
	}
	// Every state must be an ancestor of the head — anything else is a fork.
	var forked []string
	for _, s := range snap.SourceStates {
		if !walked[s.ID] {
			forked = append(forked, s.ID)
		}
	}
	if len(forked) > 0 {
		sort.Strings(forked)
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: fmt.Sprintf("state(s) not on the head's ancestor walk (forked chain): %s", strings.Join(forked, ", ")), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkCommitLinkage verifies the two directions of the source branch's
// traceability (docs/09 §2): every state of the chain is named by exactly
// one source commit's result, every source commit names a chain state, and
// every commit's base→result edge matches its result state's parent edge.
func checkCommitLinkage(spec Spec, snap Snapshot) Result {
	byID := make(map[string]domain.ProjectState, len(snap.SourceStates))
	for _, s := range snap.SourceStates {
		byID[s.ID] = s
	}
	namedBy := make(map[string][]string, len(snap.SourceStates))
	var problems []string
	for _, c := range snap.SourceCommits {
		if _, ok := byID[c.ResultStateID]; !ok {
			problems = append(problems, fmt.Sprintf("commit %s names result state %s outside the source chain", c.ID, c.ResultStateID))
		}
		namedBy[c.ResultStateID] = append(namedBy[c.ResultStateID], c.ID)
	}
	var unnamed, duplicated []string
	for _, s := range snap.SourceStates {
		commits := namedBy[s.ID]
		switch {
		case len(commits) == 0:
			unnamed = append(unnamed, s.ID)
		case len(commits) > 1:
			sort.Strings(commits)
			duplicated = append(duplicated, fmt.Sprintf("state %s is named by commits %s", s.ID, strings.Join(commits, ", ")))
		}
	}
	sort.Strings(unnamed)
	sort.Strings(duplicated)
	if len(unnamed) > 0 {
		problems = append(problems, "state(s) not named by any source commit: "+strings.Join(unnamed, ", "))
	}
	if len(duplicated) > 0 {
		problems = append(problems, strings.Join(duplicated, "; "))
	}
	// Commit edges must match the states' parent edges (the two are written
	// from the same transition; the main gate pins the invariant).
	for _, c := range snap.SourceCommits {
		s, ok := byID[c.ResultStateID]
		if !ok {
			continue // reported above
		}
		if (s.ParentStateID == nil) != (c.BaseStateID == nil) ||
			(s.ParentStateID != nil && *s.ParentStateID != *c.BaseStateID) {
			base, parent := "<none>", "<none>"
			if c.BaseStateID != nil {
				base = *c.BaseStateID
			}
			if s.ParentStateID != nil {
				parent = *s.ParentStateID
			}
			problems = append(problems, fmt.Sprintf("commit %s base→result edge (%s→%s) does not match the state's parent edge (%s→%s)", c.ID, base, c.ResultStateID, parent, s.ID))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Detail: strings.Join(problems, "; "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Why: spec.Why}
}

// checkPayloadIntegrity recomputes the sha256 of the stored payload bytes
// and compares it to the row's stored integrity hash (docs/23 §3). Stored
// payload bytes are already PostgreSQL-canonical, so the recomputation is
// exact for stored rows — the same discipline as the gate ladder's
// payload_integrity check.
func checkPayloadIntegrity(spec Spec, subject string, payload json.RawMessage, integrityHash string) Result {
	if integrityHash == "" {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: subject, Detail: "no integrity hash stored", Why: spec.Why}
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != integrityHash {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: subject, Detail: "stored payload bytes do not re-hash to the stored integrity hash", Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: subject, Why: spec.Why}
}

// checkRelationTypeKnown verifies the relation's type is in the relation
// catalog (docs/44).
func checkRelationTypeKnown(spec Spec, rv manifest.RelationVersion) Result {
	if !relationcatalog.Valid(rv.RelationType) {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: relationSubject(rv), Detail: fmt.Sprintf("relation type %q is not in the relation catalog", rv.RelationType), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: relationSubject(rv), Why: spec.Why}
}

// checkRelationEndpoints verifies both pinned endpoints exist among the
// proposal's object versions (docs/07 §3: version-pinned edges never
// dangle). The universe is the proposed state's full lineage — what the
// merged state will contain.
func checkRelationEndpoints(spec Spec, snap Snapshot, rv manifest.RelationVersion) Result {
	set := make(map[string]bool, len(snap.ProposedObjects))
	for _, ov := range snap.ProposedObjects {
		set[ov.ID] = true
	}
	var missing []string
	if !set[rv.SourceObjectVersionID] {
		missing = append(missing, "source "+rv.SourceObjectVersionID)
	}
	if !set[rv.TargetObjectVersionID] {
		missing = append(missing, "target "+rv.TargetObjectVersionID)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: relationSubject(rv), Detail: fmt.Sprintf("pinned endpoint(s) do not exist among the proposal's object versions: %s", strings.Join(missing, ", ")), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: relationSubject(rv), Why: spec.Why}
}

// checkRelationEndpointTypes verifies the pinned endpoints' object types
// are ones the catalog allows for the relation type (docs/44).
func checkRelationEndpointTypes(spec Spec, snap Snapshot, rv manifest.RelationVersion) Result {
	types := make(map[string]string, len(snap.ProposedObjects))
	for _, ov := range snap.ProposedObjects {
		types[ov.ID] = ov.ObjectType
	}
	src, srcOK := types[rv.SourceObjectVersionID]
	tgt, tgtOK := types[rv.TargetObjectVersionID]
	if !srcOK || !tgtOK {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: relationSubject(rv), Detail: "pinned endpoint version(s) not found; cannot check endpoint types", Why: spec.Why}
	}
	if ok, detail := relationcatalog.EndpointsValid(rv.RelationType, src, tgt); !ok {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: relationSubject(rv), Detail: fmt.Sprintf("%s (source %s, target %s)", detail, src, tgt), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: relationSubject(rv), Why: spec.Why}
}

// checkRightsPolicyResolves verifies a pinned rights/visibility policy is
// a real policy version of the project or its organization (docs/12 §5).
// Unpinned versions pass here — the inheritance warning covers them.
func checkRightsPolicyResolves(spec Spec, snap Snapshot, ov manifest.ObjectVersion) Result {
	if ov.VisibilityPolicyID == nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
	}
	resolvable := make(map[string]bool, len(snap.PolicyVersions))
	for _, pv := range snap.PolicyVersions {
		resolvable[pv.ID] = true
	}
	if resolvable[*ov.VisibilityPolicyID] {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: fmt.Sprintf("pinned policy %s is not a policy version of the project or its organization", *ov.VisibilityPolicyID), Why: spec.Why}
}

// checkRightsPinInheritsDefault reports an unpinned version as a warning:
// it inherits the project default, and the reviewer should see the
// effective rights explicitly (docs/07 §2 lists the visibility policy ref
// among a version's minimum fields).
func checkRightsPinInheritsDefault(spec Spec, ov manifest.ObjectVersion) Result {
	if ov.VisibilityPolicyID != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: "no visibility policy pinned; this version inherits the project default", Why: spec.Why}
}

// checkVisibilityChanges compares, for every object present in both
// lineages, the head version's (highest version number) rights/visibility
// policy pin. Any change is flagged — the machine cannot prove a policy
// change is not a widening, and docs/12 §3 forbids widening without an
// authorized person's explicit confirmation (recorded in the PR review).
// New objects (no base counterpart) are not flagged: their policy was set
// at creation through the normal governed write path.
func checkVisibilityChanges(spec Spec, snap Snapshot) []Result {
	baseHeads := headPolicyPins(snap.BaseObjects)
	proposedHeads := headPolicyPins(snap.ProposedObjects)
	var out []Result
	for objectID, basePin := range baseHeads {
		proposedPin, present := proposedHeads[objectID]
		if !present {
			continue
		}
		if basePin != proposedPin {
			out = append(out, Result{
				Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false,
				Subject: objectID,
				Detail:  fmt.Sprintf("rights/visibility policy pin changes from %s to %s; widening needs an authorized person's explicit confirmation (docs/12 §3)", basePin, proposedPin),
				Why:     spec.Why,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Subject < out[j].Subject })
	if len(out) == 0 {
		out = append(out, Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Detail: "no rights/visibility policy change between the base and the proposal", Why: spec.Why})
	}
	return out
}

// headPolicyPins maps objectID to the pin of the highest version, rendered
// with nil normalized to the project-default marker — the effective pin
// the object carries in that lineage.
func headPolicyPins(objs []manifest.ObjectVersion) map[string]string {
	highest := make(map[string]int, len(objs))
	for _, ov := range objs {
		if ov.VersionNo > highest[ov.ObjectID] {
			highest[ov.ObjectID] = ov.VersionNo
		}
	}
	out := make(map[string]string, len(objs))
	for _, ov := range objs {
		if ov.VersionNo == highest[ov.ObjectID] {
			out[ov.ObjectID] = pinString(ov.VisibilityPolicyID)
		}
	}
	return out
}

func pinString(pin *string) string {
	if pin == nil {
		return "<project default>"
	}
	return *pin
}

// blobRefFields maps an object type to its payload's blob-reference fields
// (the canonical schemas' properties, specs/schemas/*.schema.json).
var blobRefFields = map[string][]string{
	"dataset":            {"blob_ids"},
	"calculation":        {"output_blob_ids"},
	"experiment":         {"raw_blob_ids", "processed_blob_ids"},
	"material":           {"structure_blob_ids"},
	"external_reference": {"snapshot_blob_id"},
}

// checkBlobRefs verifies every blob reference in a payload resolves, by
// blob id or content hash, to a blob the proposal actually knows (its
// manifest blob refs, base ∪ proposed). Types without blob fields pass
// with nothing to check.
func checkBlobRefs(spec Spec, snap Snapshot, ov manifest.ObjectVersion) Result {
	fields := blobRefFields[ov.ObjectType]
	if len(fields) == 0 {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
	}
	var payload map[string]any
	if err := json.Unmarshal(ov.Payload, &payload); err != nil {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: fmt.Sprintf("payload is not a JSON object: %v", err), Why: spec.Why}
	}
	byID, byHash := make(map[string]bool), make(map[string]bool)
	for _, ref := range snap.ManifestRefs() {
		byID[ref.ID] = true
		byHash[ref.Hash] = true
	}
	var unresolved []string
	for _, f := range fields {
		val, ok := payload[f]
		if !ok || val == nil {
			continue
		}
		var items []any
		if arr, isArr := val.([]any); isArr {
			items = arr
		} else {
			items = []any{val} // snapshot_blob_id is a single string
		}
		for _, item := range items {
			s, isStr := item.(string)
			if !isStr {
				unresolved = append(unresolved, fmt.Sprintf("%s: %v is not a blob reference string", f, item))
				continue
			}
			if !byID[s] && !byHash[s] {
				unresolved = append(unresolved, fmt.Sprintf("%s: %q does not resolve to a manifest blob ref", f, s))
			}
		}
	}
	sort.Strings(unresolved)
	if len(unresolved) > 0 {
		return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: false, Subject: objectSubject(ov), Detail: strings.Join(unresolved, "; "), Why: spec.Why}
	}
	return Result{Check: spec.Check, Dimension: spec.Dimension, Severity: spec.Severity, Passed: true, Subject: objectSubject(ov), Why: spec.Why}
}

// stateIn reports whether the state id is a member of the chain.
func stateIn(states []domain.ProjectState, id string) bool {
	for _, s := range states {
		if s.ID == id {
			return true
		}
	}
	return false
}
