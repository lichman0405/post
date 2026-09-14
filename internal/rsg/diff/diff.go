package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// FormatV1 is the diff format version this package emits.
const FormatV1 = "v1"

// StateRef names one state of the three-way input: its identity and its
// recorded GitProvider commit (project_states.git_commit_sha, nil when the
// state came through a channel without a git commit). The git ref is the
// state's own recorded content — the engine takes it as input exactly like
// the manifest's purity invariant requires (a state's diff never reads
// mutable project data).
type StateRef struct {
	ID string `json:"id"`
	// GitRef is the state's recorded GitProvider commit sha; nil when the
	// state has none.
	GitRef *string `json:"git_ref"`
}

// Inputs carries one three-way diff computation: the three states (their
// ids and recorded git refs) and each state's lineage snapshot rows in
// stored form — the same manifest.Snapshot shape the persistence layer
// reads. BlobRefs are accepted and ignored: the V1 diff covers objects and
// relations (package doc).
type Inputs struct {
	ProjectID string
	Base      StateRef
	Source    StateRef
	Target    StateRef
	// BaseSnapshot is the base state's lineage rows.
	BaseSnapshot manifest.Snapshot
	// SourceSnapshot is the source state's lineage rows.
	SourceSnapshot manifest.Snapshot
	// TargetSnapshot is the target state's lineage rows.
	TargetSnapshot manifest.Snapshot
}

// ChangeKind names what happened to one object or relation between the
// base and the source lineage. The vocabulary is the canonical write
// classification of docs/09 §2 (objects created/updated/aborted/reopened,
// relations changed).
type ChangeKind string

const (
	// ChangeCreated: the object/relation has no version in the base
	// lineage and at least one in the source lineage.
	ChangeCreated ChangeKind = "created"
	// ChangeUpdated: the head version differs from the base head and the
	// lifecycle did not flip (or the entity is a relation, which carries
	// no lifecycle).
	ChangeUpdated ChangeKind = "updated"
	// ChangeAborted: the source head's lifecycle is 'aborted' while the
	// base head's is not — the research path closed this object.
	ChangeAborted ChangeKind = "aborted"
	// ChangeReopened: the base head was 'aborted' and the source head is
	// not — the object was brought back.
	ChangeReopened ChangeKind = "reopened"
)

// Diff is one three-way Research State Diff: the source branch's proposed
// change against the base, with the target branch's concurrent state
// recorded on each change. The field order below is canonical — it is the
// serialization order of the diff document.
type Diff struct {
	FormatVersion   string           `json:"format_version"`
	ProjectID       string           `json:"project_id"`
	Base            StateRef         `json:"base"`
	Source          StateRef         `json:"source"`
	Target          StateRef         `json:"target"`
	ObjectChanges   []ObjectChange   `json:"object_changes"`
	RelationChanges []RelationChange `json:"relation_changes"`
	FileDiffRefs    []FileDiffRef    `json:"file_diff_refs"`
	Summary         Summary          `json:"summary"`
}

// ObjectChange is one scientific object's source-side change: the head
// versions in the three lineages, the change kind, the fields that moved
// on each side, and the source-side version trail (every version beyond
// the base head, in version order — the commits that introduced the
// change).
type ObjectChange struct {
	ObjectID   string     `json:"object_id"`
	ObjectType string     `json:"object_type"`
	Kind       ChangeKind `json:"kind"`
	// BaseVersion is the object's head version in the base lineage; nil
	// when the object does not exist there.
	BaseVersion *manifest.ObjectVersion `json:"base_version"`
	// SourceVersion is the head version in the source lineage (always
	// present on a change).
	SourceVersion manifest.ObjectVersion `json:"source_version"`
	// TargetVersion is the head version in the target lineage; nil when
	// the object does not exist there.
	TargetVersion *manifest.ObjectVersion `json:"target_version"`
	// TargetMoved reports whether the target's head differs from the
	// base's head: the target branch touched this object since the base.
	// It is the three-way information the conflict detector consumes; this
	// engine reports it, it never judges it.
	TargetMoved bool `json:"target_moved"`
	// ChangedFields lists the object fields whose value differs between
	// the base head and the source head, in the canonical field order.
	// Empty for created changes (there is no base value to differ from).
	ChangedFields []string `json:"changed_fields"`
	// TargetChangedFields lists the fields that differ between the base
	// head and the target head (canonical order); empty when the target
	// did not move the object or the base head does not exist.
	TargetChangedFields []string `json:"target_changed_fields"`
	// NewVersions is the source-side version trail of the change: every
	// source-lineage version beyond the base head, ascending by
	// version_no. Its last element is SourceVersion when the source
	// lineage extends the base head (the normal, ancestor case); empty
	// otherwise.
	NewVersions []manifest.ObjectVersion `json:"new_versions"`
}

// RelationChange is one relation's source-side change. Relations carry no
// lifecycle column in V1, so the kinds are created and updated only
// (docs/44's relation semantics are version-level).
type RelationChange struct {
	RelationID string `json:"relation_id"`
	// Kind is created or updated.
	Kind ChangeKind `json:"kind"`
	// BaseVersion is the relation's head version in the base lineage; nil
	// when the relation does not exist there.
	BaseVersion *manifest.RelationVersion `json:"base_version"`
	// SourceVersion is the head version in the source lineage.
	SourceVersion manifest.RelationVersion `json:"source_version"`
	// TargetVersion is the head version in the target lineage; nil when
	// the relation does not exist there.
	TargetVersion *manifest.RelationVersion `json:"target_version"`
	// TargetMoved reports whether the target's head differs from the
	// base's head (see ObjectChange.TargetMoved).
	TargetMoved bool `json:"target_moved"`
	// ChangedFields lists the relation fields whose value differs between
	// the base head and the source head, in canonical field order.
	ChangedFields []string `json:"changed_fields"`
	// TargetChangedFields lists the base→target field differences;
	// empty when the target did not move the relation.
	TargetChangedFields []string `json:"target_changed_fields"`
	// NewVersions is the source-side version trail beyond the base head,
	// ascending by version_no (see ObjectChange.NewVersions).
	NewVersions []manifest.RelationVersion `json:"new_versions"`
}

// FileDiffRef pins one raw file-level diff the UI renders next to the
// semantic diff (docs/06 §6: raw Git diff is the secondary view): the
// recorded git refs of its two ends. The paths and patch bytes themselves
// are GitProvider material — this engine records the refs only, so it
// stays pure and provider-free.
type FileDiffRef struct {
	// Kind names which side of the three-way this ref describes:
	// "source" (base→source, the PR's proposed file change) or "target"
	// (base→target, what the target branch's files did meanwhile).
	Kind string `json:"kind"`
	// BaseGitRef is the base state's recorded git commit sha; "" when
	// that state has none (the consumer renders the empty base as the
	// empty tree, the git convention for a born-in-push diff).
	BaseGitRef string `json:"base_git_ref"`
	// HeadGitRef is the head state's recorded git commit sha; "" when
	// that state has none.
	HeadGitRef string `json:"head_git_ref"`
}

// fileRefKind names the two sides of the three-way file refs.
const (
	fileRefSource = "source"
	fileRefTarget = "target"
)

// Summary is the categorized count of the diff — the scientific summary
// categories of docs/06 §6 that are mechanically derivable: objects by
// change kind and scientific type, relations by change kind and relation
// type, plus the schema-reference and visibility moves the PR page names
// as their own categories ("Protocol/Schema changes", "visibility
// changes"). The field order below is canonical.
type Summary struct {
	ObjectsCreated   int `json:"objects_created"`
	ObjectsUpdated   int `json:"objects_updated"`
	ObjectsAborted   int `json:"objects_aborted"`
	ObjectsReopened  int `json:"objects_reopened"`
	RelationsCreated int `json:"relations_created"`
	RelationsUpdated int `json:"relations_updated"`
	// SchemaChanged counts object changes whose schema reference moved
	// between the base head and the source head.
	SchemaChanged int `json:"schema_changed"`
	// VisibilityChanged counts object changes whose visibility policy
	// moved between the base head and the source head.
	VisibilityChanged int `json:"visibility_changed"`
	// ObjectTypes is the per-scientific-type change counts, sorted by
	// type — the knowledge categories (claim, hypothesis,
	// research_question, …) are the types themselves.
	ObjectTypes []TypeCount `json:"object_types"`
	// RelationTypes is the per-relation-type change counts, sorted by
	// type — the dependency/provenance categories are the types
	// themselves.
	RelationTypes []TypeCount `json:"relation_types"`
}

// TypeCount is one entry of a per-type summary breakdown.
type TypeCount struct {
	Type  string `json:"type"`
	Count int    `json:"count"`
}

// Compute derives the three-way diff from the input states and snapshots.
// It canonicalizes every payload before comparison and output, applies the
// stable orderings, and renders the summary and the file diff refs. The
// same inputs always yield the same Diff — CanonicalJSON pins the bytes.
func Compute(in Inputs) (*Diff, error) {
	if err := validateInputs(in); err != nil {
		return nil, err
	}
	base, err := canonicalizeSnapshot(in.BaseSnapshot)
	if err != nil {
		return nil, err
	}
	source, err := canonicalizeSnapshot(in.SourceSnapshot)
	if err != nil {
		return nil, err
	}
	target, err := canonicalizeSnapshot(in.TargetSnapshot)
	if err != nil {
		return nil, err
	}
	canon := &inputsCanon{base: base, source: source, target: target}

	d := &Diff{
		FormatVersion:   FormatV1,
		ProjectID:       in.ProjectID,
		Base:            in.Base,
		Source:          in.Source,
		Target:          in.Target,
		ObjectChanges:   []ObjectChange{},
		RelationChanges: []RelationChange{},
		FileDiffRefs:    []FileDiffRef{},
	}
	d.ObjectChanges = canon.objectChanges()
	d.RelationChanges = canon.relationChanges()
	d.FileDiffRefs = fileDiffRefs(in.Base, in.Source, in.Target)
	d.Summary = summarize(d)
	return d, nil
}

// CanonicalJSON renders the diff document in its canonical form: fixed
// field order, payloads already canonicalized, changes in their stable
// order. Two computations over the same inputs marshal to the same bytes.
func (d *Diff) CanonicalJSON() ([]byte, error) {
	return json.Marshal(d)
}

// validateInputs checks the computation's shape. The state ids and the
// project id are required; anything else about the three states is
// semantic content, not shape.
func validateInputs(in Inputs) error {
	if in.ProjectID == "" {
		return fmt.Errorf("diff: project_id is required")
	}
	if in.Base.ID == "" {
		return fmt.Errorf("diff: base state id is required")
	}
	if in.Source.ID == "" {
		return fmt.Errorf("diff: source state id is required")
	}
	if in.Target.ID == "" {
		return fmt.Errorf("diff: target state id is required")
	}
	return nil
}

// inputsCanon holds the three snapshots with every payload re-encoded into
// canonical form, so comparison and output never depend on stored key
// order.
type inputsCanon struct {
	base   snapshot
	source snapshot
	target snapshot
}

// snapshot is one state's lineage rows with canonical payloads.
type snapshot struct {
	objects   []manifest.ObjectVersion
	relations []manifest.RelationVersion
}

// canonicalizeSnapshot re-encodes every payload of the snapshot rows into
// canonical form. A payload that is not exactly one JSON value is an
// error: stored payloads are jsonb text, and a corrupted row must surface
// here, never be silently compared as "different bytes".
func canonicalizeSnapshot(s manifest.Snapshot) (snapshot, error) {
	out := snapshot{
		objects:   make([]manifest.ObjectVersion, 0, len(s.ObjectVersions)),
		relations: make([]manifest.RelationVersion, 0, len(s.RelationVersions)),
	}
	for _, v := range s.ObjectVersions {
		c, err := manifest.CanonicalJSON(v.Payload)
		if err != nil {
			return snapshot{}, fmt.Errorf("diff: object version %s payload is not valid JSON: %w", v.ID, err)
		}
		v.Payload = c
		out.objects = append(out.objects, v)
	}
	for _, v := range s.RelationVersions {
		c, err := manifest.CanonicalJSON(v.Payload)
		if err != nil {
			return snapshot{}, fmt.Errorf("diff: relation version %s payload is not valid JSON: %w", v.ID, err)
		}
		v.Payload = c
		out.relations = append(out.relations, v)
	}
	return out, nil
}

// objectChanges derives the object change list in its stable order
// (object id ascending): every object whose head version differs between
// the base and the source lineage, with the target head recorded.
func (c *inputsCanon) objectChanges() []ObjectChange {
	baseHeads := headObjects(c.base.objects)
	sourceHeads := headObjects(c.source.objects)
	targetHeads := headObjects(c.target.objects)
	sourceTrails := versionTrails(c.source.objects)

	ids := make([]string, 0, len(baseHeads)+len(sourceHeads))
	seen := make(map[string]bool, len(baseHeads)+len(sourceHeads))
	for id := range baseHeads {
		ids, seen[id] = append(ids, id), true
	}
	for id := range sourceHeads {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	changes := make([]ObjectChange, 0, len(ids))
	for _, id := range ids {
		base, source := baseHeads[id], sourceHeads[id]
		kind, changed := classifyObject(base, source)
		if kind == "" {
			continue // unchanged on the source side
		}
		if changed == nil {
			changed = []string{}
		}
		changes = append(changes, ObjectChange{
			ObjectID:            id,
			ObjectType:          source.ObjectType,
			Kind:                kind,
			BaseVersion:         base,
			SourceVersion:       *source,
			TargetVersion:       targetHeads[id],
			TargetMoved:         objectHeadMoved(base, targetHeads[id]),
			ChangedFields:       changed,
			TargetChangedFields: objectFieldDiff(base, targetHeads[id]),
			NewVersions:         sourceTrails.trail(id, base),
		})
	}
	return changes
}

// relationChanges derives the relation change list in its stable order
// (relation id ascending): every relation whose head version differs
// between the base and the source lineage, with the target head recorded.
func (c *inputsCanon) relationChanges() []RelationChange {
	baseHeads := headRelations(c.base.relations)
	sourceHeads := headRelations(c.source.relations)
	targetHeads := headRelations(c.target.relations)
	sourceTrails := relationVersionTrails(c.source.relations)

	ids := make([]string, 0, len(baseHeads)+len(sourceHeads))
	seen := make(map[string]bool, len(baseHeads)+len(sourceHeads))
	for id := range baseHeads {
		ids, seen[id] = append(ids, id), true
	}
	for id := range sourceHeads {
		if !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	changes := make([]RelationChange, 0, len(ids))
	for _, id := range ids {
		base, source := baseHeads[id], sourceHeads[id]
		kind, changed := classifyRelation(base, source)
		if kind == "" {
			continue
		}
		if changed == nil {
			changed = []string{}
		}
		changes = append(changes, RelationChange{
			RelationID:          id,
			Kind:                kind,
			BaseVersion:         base,
			SourceVersion:       *source,
			TargetVersion:       targetHeads[id],
			TargetMoved:         relationHeadMoved(base, targetHeads[id]),
			ChangedFields:       changed,
			TargetChangedFields: relationFieldDiff(base, targetHeads[id]),
			NewVersions:         sourceTrails.trail(id, base),
		})
	}
	return changes
}

// classifyObject decides the change kind of one object from its base and
// source head versions. It returns ("", nil) when the heads are identical
// — the object did not move on the source side and is not listed. An
// object absent from the source is not listed either: nothing disappears
// (package doc).
func classifyObject(base, source *manifest.ObjectVersion) (ChangeKind, []string) {
	if source == nil {
		return "", nil
	}
	if base == nil {
		return ChangeCreated, nil
	}
	if base.ID == source.ID {
		return "", nil
	}
	diff := objectFieldDiff(base, source)
	switch {
	case source.LifecycleState == string(domain.LifecycleAborted) && base.LifecycleState != string(domain.LifecycleAborted):
		return ChangeAborted, diff
	case base.LifecycleState == string(domain.LifecycleAborted) && source.LifecycleState != string(domain.LifecycleAborted):
		return ChangeReopened, diff
	default:
		return ChangeUpdated, diff
	}
}

// classifyRelation decides the change kind of one relation. Relations
// carry no lifecycle column, so the kinds are created and updated only.
func classifyRelation(base, source *manifest.RelationVersion) (ChangeKind, []string) {
	if source == nil {
		return "", nil
	}
	if base == nil {
		return ChangeCreated, nil
	}
	if base.ID == source.ID {
		return "", nil
	}
	return ChangeUpdated, relationFieldDiff(base, source)
}

// headObjects maps each object id to its head version: the highest
// version_no, ties broken by version row id (deterministic even though the
// unique constraint makes ties impossible).
func headObjects(rows []manifest.ObjectVersion) map[string]*manifest.ObjectVersion {
	heads := make(map[string]*manifest.ObjectVersion, len(rows))
	for i := range rows {
		v := &rows[i]
		cur, ok := heads[v.ObjectID]
		if !ok || v.VersionNo > cur.VersionNo || (v.VersionNo == cur.VersionNo && v.ID > cur.ID) {
			heads[v.ObjectID] = v
		}
	}
	return heads
}

func headRelations(rows []manifest.RelationVersion) map[string]*manifest.RelationVersion {
	heads := make(map[string]*manifest.RelationVersion, len(rows))
	for i := range rows {
		v := &rows[i]
		cur, ok := heads[v.RelationID]
		if !ok || v.VersionNo > cur.VersionNo || (v.VersionNo == cur.VersionNo && v.ID > cur.ID) {
			heads[v.RelationID] = v
		}
	}
	return heads
}

// objectHeadMoved reports whether the object's head version differs
// between two lineages: present on exactly one side, or different version
// rows on both.
func objectHeadMoved(base, head *manifest.ObjectVersion) bool {
	return (base == nil) != (head == nil) || (base != nil && head != nil && base.ID != head.ID)
}

func relationHeadMoved(base, head *manifest.RelationVersion) bool {
	return (base == nil) != (head == nil) || (base != nil && head != nil && base.ID != head.ID)
}

// objectTrails indexes every object's source-lineage version trail
// ascending by version_no.
type objectTrails map[string][]manifest.ObjectVersion

func versionTrails(rows []manifest.ObjectVersion) objectTrails {
	trails := make(objectTrails, len(rows))
	for _, v := range rows {
		trails[v.ObjectID] = append(trails[v.ObjectID], v)
	}
	for id, trail := range trails {
		sort.Slice(trail, func(i, j int) bool {
			if trail[i].VersionNo != trail[j].VersionNo {
				return trail[i].VersionNo < trail[j].VersionNo
			}
			return trail[i].ID < trail[j].ID
		})
		trails[id] = trail
	}
	return trails
}

// trail returns the versions beyond the base head: every version with a
// higher version_no than base's head (all of them when the base head is
// absent — the created case). Empty when the base head is ahead of or
// equal to every source version (a base that is not an ancestor — the
// pathological case this engine reports honestly instead of guessing).
func (t objectTrails) trail(objectID string, base *manifest.ObjectVersion) []manifest.ObjectVersion {
	trail := t[objectID]
	if base == nil {
		return trail
	}
	from := sort.Search(len(trail), func(i int) bool { return trail[i].VersionNo > base.VersionNo })
	if from == len(trail) {
		return []manifest.ObjectVersion{}
	}
	return trail[from:]
}

// relationTrails mirrors objectTrails for relation versions.
type relationTrails map[string][]manifest.RelationVersion

func relationVersionTrails(rows []manifest.RelationVersion) relationTrails {
	trails := make(relationTrails, len(rows))
	for _, v := range rows {
		trails[v.RelationID] = append(trails[v.RelationID], v)
	}
	for id, trail := range trails {
		sort.Slice(trail, func(i, j int) bool {
			if trail[i].VersionNo != trail[j].VersionNo {
				return trail[i].VersionNo < trail[j].VersionNo
			}
			return trail[i].ID < trail[j].ID
		})
		trails[id] = trail
	}
	return trails
}

func (t relationTrails) trail(relationID string, base *manifest.RelationVersion) []manifest.RelationVersion {
	trail := t[relationID]
	if base == nil {
		return trail
	}
	from := sort.Search(len(trail), func(i int) bool { return trail[i].VersionNo > base.VersionNo })
	if from == len(trail) {
		return []manifest.RelationVersion{}
	}
	return trail[from:]
}

// objectFieldDiff lists the fields whose value differs between two object
// versions, in the canonical field order. A nil side compares as "absent":
// no fields are reported (there is no value to differ from).
func objectFieldDiff(base, head *manifest.ObjectVersion) []string {
	if base == nil || head == nil {
		return []string{}
	}
	diff := []string{}
	if base.Title != head.Title {
		diff = append(diff, "title")
	}
	if !bytes.Equal(base.Payload, head.Payload) {
		diff = append(diff, "payload")
	}
	if base.LifecycleState != head.LifecycleState {
		diff = append(diff, "lifecycle_state")
	}
	if base.SchemaRef != head.SchemaRef {
		diff = append(diff, "schema_ref")
	}
	if !ptrStringEqual(base.VisibilityPolicyID, head.VisibilityPolicyID) {
		diff = append(diff, "visibility_policy_id")
	}
	return diff
}

// relationFieldDiff mirrors objectFieldDiff for relation versions.
func relationFieldDiff(base, head *manifest.RelationVersion) []string {
	if base == nil || head == nil {
		return []string{}
	}
	diff := []string{}
	if base.RelationType != head.RelationType {
		diff = append(diff, "relation_type")
	}
	if base.SourceObjectVersionID != head.SourceObjectVersionID {
		diff = append(diff, "source_object_version_id")
	}
	if base.TargetObjectVersionID != head.TargetObjectVersionID {
		diff = append(diff, "target_object_version_id")
	}
	if !bytes.Equal(base.Payload, head.Payload) {
		diff = append(diff, "payload")
	}
	return diff
}

func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// fileDiffRefs renders the file-level refs of the two sides of the
// three-way diff, source first. A side is listed only when at least one
// end carries a recorded git ref — a side with no git material at all has
// no raw diff to render.
func fileDiffRefs(base, source, target StateRef) []FileDiffRef {
	refs := []FileDiffRef{}
	for _, side := range []struct {
		kind string
		head StateRef
	}{
		{fileRefSource, source},
		{fileRefTarget, target},
	} {
		baseRef, headRef := "", ""
		if base.GitRef != nil {
			baseRef = *base.GitRef
		}
		if side.head.GitRef != nil {
			headRef = *side.head.GitRef
		}
		if baseRef == "" && headRef == "" {
			continue
		}
		refs = append(refs, FileDiffRef{Kind: side.kind, BaseGitRef: baseRef, HeadGitRef: headRef})
	}
	return refs
}

// summarize renders the categorized counts of the change lists: objects by
// kind and scientific type, relations by kind and relation type, plus the
// schema-reference and visibility moves.
func summarize(d *Diff) Summary {
	s := Summary{
		ObjectTypes:   []TypeCount{},
		RelationTypes: []TypeCount{},
	}
	objTypes := make(map[string]int)
	relTypes := make(map[string]int)
	for _, c := range d.ObjectChanges {
		switch c.Kind {
		case ChangeCreated:
			s.ObjectsCreated++
		case ChangeUpdated:
			s.ObjectsUpdated++
		case ChangeAborted:
			s.ObjectsAborted++
		case ChangeReopened:
			s.ObjectsReopened++
		}
		objTypes[c.ObjectType]++
		for _, f := range c.ChangedFields {
			switch f {
			case "schema_ref":
				s.SchemaChanged++
			case "visibility_policy_id":
				s.VisibilityChanged++
			}
		}
	}
	for _, c := range d.RelationChanges {
		switch c.Kind {
		case ChangeCreated:
			s.RelationsCreated++
		case ChangeUpdated:
			s.RelationsUpdated++
		}
		relTypes[c.SourceVersion.RelationType]++
	}
	s.ObjectTypes = sortedTypeCounts(objTypes)
	s.RelationTypes = sortedTypeCounts(relTypes)
	return s
}

func sortedTypeCounts(m map[string]int) []TypeCount {
	out := make([]TypeCount, 0, len(m))
	for typ, count := range m {
		out = append(out, TypeCount{Type: typ, Count: count})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}
