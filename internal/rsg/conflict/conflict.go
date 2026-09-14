package conflict

import (
	"encoding/json"
	"sort"

	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
)

// FormatV1 is the report format version this package emits.
const FormatV1 = "v1"

// ConflictCategory names one class of the docs/09 §6 taxonomy (plus the
// scientific category docs/09 §7/§8 name explicitly). The value is the
// stable wire spelling the merge engine and the resolution UI switch on.
type ConflictCategory string

const (
	// CategoryAttribute: the same field was changed to different values on
	// the two branches (docs/09 §6 "Attribute conflict").
	CategoryAttribute ConflictCategory = "attribute"
	// CategoryIdentity: objects created on opposite sides of the fork look
	// like the same object (docs/09 §6 "Identity conflict: 疑似同对象/不同对象").
	CategoryIdentity ConflictCategory = "identity"
	// CategoryRelation: a relation's own semantics (type, scope payload)
	// diverged on the two branches (docs/09 §6 "Relation/provenance conflict").
	CategoryRelation ConflictCategory = "relation"
	// CategorySchema: the schema reference moved to different schema
	// versions on the two branches (docs/09 §6 "Schema compatibility conflict").
	CategorySchema ConflictCategory = "schema"
	// CategoryKnowledge: knowledge content diverged — a knowledge object's
	// payload or a knowledge-category relation (docs/09 §6 "Knowledge
	// conflict: Claim assessment/evidence conflict").
	CategoryKnowledge ConflictCategory = "knowledge"
	// CategoryRights: the visibility policy diverged, or the source moved
	// it at all (docs/09 §6 "Rights/visibility conflict", §7: private→public
	// and rights conflicts are never auto).
	CategoryRights ConflictCategory = "rights"
	// CategoryDependency: a relation's endpoint pins diverged — which
	// upstream object version the edge depends on (docs/09 §6 "Dependency
	// conflict", docs/19 §3).
	CategoryDependency ConflictCategory = "dependency"
	// CategoryScientific: a protocol's content field diverged — a
	// scientific conflict no machine may resolve (docs/09 §7/§8, docs/07
	// §8: 科学冲突不得自动选 winner).
	CategoryScientific ConflictCategory = "scientific"
)

// Conflict codes: the stable machine-readable vocabulary callers can
// switch on without parsing detail text.
const (
	// CodeIdentitySuspectedDuplicate: an object created on the source side
	// has the same type and trimmed title as one created on the target
	// side — a suspected duplicate a human must decide about.
	CodeIdentitySuspectedDuplicate = "IDENTITY_SUSPECTED_DUPLICATE"
	// CodeAttributeFieldDiverges: a non-content object field (title,
	// lifecycle) changed to different values on both branches.
	CodeAttributeFieldDiverges = "ATTRIBUTE_FIELD_DIVERGES"
	// CodeRelationFieldDiverges: a relation's type or payload changed to
	// different values on both branches.
	CodeRelationFieldDiverges = "RELATION_FIELD_DIVERGES"
	// CodeSchemaFieldDiverges: the schema reference moved to different
	// schema versions on both branches.
	CodeSchemaFieldDiverges = "SCHEMA_FIELD_DIVERGES"
	// CodeKnowledgeFieldDiverges: knowledge content (a knowledge object's
	// payload or a knowledge relation's payload/type) changed to different
	// values on both branches.
	CodeKnowledgeFieldDiverges = "KNOWLEDGE_FIELD_DIVERGES"
	// CodeRightsFieldDiverges: the visibility policy moved to different
	// policies on both branches — a rights conflict, never auto-resolved.
	CodeRightsFieldDiverges = "RIGHTS_FIELD_DIVERGES"
	// CodeRightsVisibilityChange: the source moved the visibility policy
	// and the target did not diverge on it; visibility moves are never
	// auto-merged (docs/12 §3: expansion requires explicit authorized
	// confirmation with an audit event).
	CodeRightsVisibilityChange = "RIGHTS_VISIBILITY_CHANGE"
	// CodeDependencyFieldDiverges: a relation's endpoint pins moved to
	// different object versions on both branches.
	CodeDependencyFieldDiverges = "DEPENDENCY_FIELD_DIVERGES"
	// CodeScientificFieldDiverges: a protocol's content field changed to
	// different values on both branches — a scientific conflict, never
	// auto-resolved.
	CodeScientificFieldDiverges = "SCIENTIFIC_FIELD_DIVERGES"
)

// Conflict is one classified conflict on one change. The field order
// below is canonical — it is the serialization order of the report.
type Conflict struct {
	Category ConflictCategory `json:"category"`
	// Code is the stable machine-readable conflict id (the constants
	// above).
	Code string `json:"code"`
	// Fields names the object/relation fields the conflict covers, in the
	// canonical field order. Empty for identity conflicts.
	Fields []string `json:"fields"`
	// PayloadKeys names the top-level payload keys that diverged, sorted
	// by name. Empty unless "payload" is in Fields.
	PayloadKeys []string `json:"payload_keys"`
	// OtherObjectID names the target-side object an identity conflict
	// pairs with (the suspected duplicate); "" otherwise.
	OtherObjectID string `json:"other_object_id"`
	// Detail is the human-readable explanation, rendered by the
	// resolution UI.
	Detail string `json:"detail"`
}

// ObjectVerdict is the conflict verdict of one source-side object change:
// whether it can merge without a human decision and, when it cannot, the
// classified conflicts.
type ObjectVerdict struct {
	ObjectID   string          `json:"object_id"`
	ObjectType string          `json:"object_type"`
	Kind       diff.ChangeKind `json:"kind"`
	// AutoMergeable reports the docs/09 §7 boundary: true only when the
	// change is safe to merge without a human decision. Every conflict
	// clears it; it is never set for changes carrying conflicts.
	AutoMergeable bool       `json:"auto_mergeable"`
	Conflicts     []Conflict `json:"conflicts"`
}

// RelationVerdict is the conflict verdict of one source-side relation
// change (see ObjectVerdict).
type RelationVerdict struct {
	RelationID    string          `json:"relation_id"`
	Kind          diff.ChangeKind `json:"kind"`
	AutoMergeable bool            `json:"auto_mergeable"`
	Conflicts     []Conflict      `json:"conflicts"`
}

// Report is the detector's output: the three-way diff it classified (the
// same document diff.Compute renders — consumers get the change list and
// the verdicts from one call), the per-change verdicts, and the summary.
// The field order below is canonical.
type Report struct {
	FormatVersion    string            `json:"format_version"`
	ProjectID        string            `json:"project_id"`
	Diff             *diff.Diff        `json:"diff"`
	ObjectVerdicts   []ObjectVerdict   `json:"object_verdicts"`
	RelationVerdicts []RelationVerdict `json:"relation_verdicts"`
	// AutoMergeable reports whether every change in the report is
	// auto-mergeable — the whole proposed change needs no human
	// resolution decision.
	AutoMergeable bool    `json:"auto_mergeable"`
	Summary       Summary `json:"summary"`
}

// Summary counts the verdicts and the conflict categories (the
// machine-readable roll-up the PR page renders ahead of the detail).
type Summary struct {
	ObjectsAutoMergeable   int `json:"objects_auto_mergeable"`
	ObjectsConflicted      int `json:"objects_conflicted"`
	RelationsAutoMergeable int `json:"relations_auto_mergeable"`
	RelationsConflicted    int `json:"relations_conflicted"`
	// ConflictsByCategory counts conflicts by category, sorted by category
	// name.
	ConflictsByCategory []CategoryCount `json:"conflicts_by_category"`
}

// CategoryCount is one entry of the per-category roll-up.
type CategoryCount struct {
	Category ConflictCategory `json:"category"`
	Count    int              `json:"count"`
}

// Detect computes the three-way diff of the inputs and classifies every
// source-side change: safe changes are marked auto_mergeable, conflicts
// are classified into the docs/09 §6 taxonomy (package doc). The same
// inputs always yield the same report — CanonicalJSON pins the bytes.
func Detect(in diff.Inputs) (*Report, error) {
	d, err := diff.Compute(in)
	if err != nil {
		return nil, err
	}
	targetCreated := createdOnTarget(in)
	r := &Report{
		FormatVersion:    FormatV1,
		ProjectID:        in.ProjectID,
		Diff:             d,
		ObjectVerdicts:   make([]ObjectVerdict, 0, len(d.ObjectChanges)),
		RelationVerdicts: make([]RelationVerdict, 0, len(d.RelationChanges)),
	}
	for _, c := range d.ObjectChanges {
		r.ObjectVerdicts = append(r.ObjectVerdicts, classifyObject(c, targetCreated))
	}
	for _, c := range d.RelationChanges {
		r.RelationVerdicts = append(r.RelationVerdicts, classifyRelation(c))
	}
	r.AutoMergeable = allAuto(r.ObjectVerdicts, r.RelationVerdicts)
	r.Summary = summarize(r.ObjectVerdicts, r.RelationVerdicts)
	return r, nil
}

// CanonicalJSON renders the report in its canonical form: fixed field
// order, verdicts in the diff's identity-sorted change order, conflicts in
// their deterministic per-change order. Two detections over the same
// inputs marshal to the same bytes.
func (r *Report) CanonicalJSON() ([]byte, error) {
	return json.Marshal(r)
}

// createdOnTarget lists the head version of every object the target
// branch created since the base — the target-side rows the identity check
// pairs source-side creations against (the diff's change list carries
// source-side changes only). Sorted by object id for deterministic output.
func createdOnTarget(in diff.Inputs) []manifest.ObjectVersion {
	baseHeads := make(map[string]bool, len(in.BaseSnapshot.ObjectVersions))
	for _, v := range in.BaseSnapshot.ObjectVersions {
		baseHeads[v.ObjectID] = true
	}
	targetHeads := headObjects(in.TargetSnapshot.ObjectVersions)
	created := make([]manifest.ObjectVersion, 0)
	for id, v := range targetHeads {
		if !baseHeads[id] {
			created = append(created, *v)
		}
	}
	sort.Slice(created, func(i, j int) bool { return created[i].ObjectID < created[j].ObjectID })
	return created
}

// headObjects maps each object id to its head version: the highest
// version_no (the diff engine's own head rule, ties broken by version row
// id).
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

// allAuto reports whether every verdict is auto-mergeable.
func allAuto(objects []ObjectVerdict, relations []RelationVerdict) bool {
	for _, v := range objects {
		if !v.AutoMergeable {
			return false
		}
	}
	for _, v := range relations {
		if !v.AutoMergeable {
			return false
		}
	}
	return true
}

// summarize rolls up the verdicts: auto/conflicted counts and the
// per-category conflict counts.
func summarize(objects []ObjectVerdict, relations []RelationVerdict) Summary {
	s := Summary{ConflictsByCategory: []CategoryCount{}}
	counts := make(map[ConflictCategory]int)
	for _, v := range objects {
		if v.AutoMergeable {
			s.ObjectsAutoMergeable++
		} else {
			s.ObjectsConflicted++
		}
		for _, c := range v.Conflicts {
			counts[c.Category]++
		}
	}
	for _, v := range relations {
		if v.AutoMergeable {
			s.RelationsAutoMergeable++
		} else {
			s.RelationsConflicted++
		}
		for _, c := range v.Conflicts {
			counts[c.Category]++
		}
	}
	cats := make([]ConflictCategory, 0, len(counts))
	for cat := range counts {
		cats = append(cats, cat)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i] < cats[j] })
	for _, cat := range cats {
		s.ConflictsByCategory = append(s.ConflictsByCategory, CategoryCount{Category: cat, Count: counts[cat]})
	}
	return s
}
