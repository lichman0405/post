package conflict

import (
	"fmt"
	"sort"
	"strings"

	"github.com/lichman0405/post/internal/rsg/diff"
	"github.com/lichman0405/post/internal/rsg/manifest"
	"github.com/lichman0405/post/internal/rsg/relationcatalog"
)

// canonicalObjectFields is the object field order the classification walks
// (the diff engine's own canonical field order).
var canonicalObjectFields = []string{
	"title",
	"payload",
	"lifecycle_state",
	"schema_ref",
	"visibility_policy_id",
}

// canonicalRelationFields is the relation field order the classification
// walks (the diff engine's own canonical field order).
var canonicalRelationFields = []string{
	"relation_type",
	"source_object_version_id",
	"target_object_version_id",
	"payload",
}

// knowledgeObjectTypes names the scientific types whose payload content is
// knowledge (docs/09 §6: "Knowledge conflict: Claim assessment/evidence
// conflict" — the claim-shaped types docs/08 defines as knowledge).
var knowledgeObjectTypes = map[string]bool{
	"claim":             true,
	"hypothesis":        true,
	"research_question": true,
	"finding":           true,
}

// classifyObject derives one object change's verdict. The change is
// auto-mergeable exactly when nothing conflicts (package doc's boundary):
// the target did not move the object (or only its non-overlapping fields),
// converged fields and mergeable appends are not conflicts, and every
// divergence is classified into the taxonomy.
func classifyObject(c diff.ObjectChange, targetCreated []manifest.ObjectVersion) ObjectVerdict {
	v := ObjectVerdict{
		ObjectID:   c.ObjectID,
		ObjectType: c.ObjectType,
		Kind:       c.Kind,
		Conflicts:  []Conflict{},
	}
	// Identity: a source-side creation with the same type and trimmed
	// title as a target-side creation is a suspected duplicate.
	if c.Kind == diff.ChangeCreated {
		for _, t := range targetCreated {
			if t.ObjectType != c.ObjectType {
				continue
			}
			if strings.TrimSpace(t.Title) != strings.TrimSpace(c.SourceVersion.Title) {
				continue
			}
			v.Conflicts = append(v.Conflicts, Conflict{
				Category:      CategoryIdentity,
				Code:          CodeIdentitySuspectedDuplicate,
				Fields:        []string{},
				PayloadKeys:   []string{},
				OtherObjectID: t.ObjectID,
				Detail: fmt.Sprintf(
					"target branch created %s %s with the same title — suspected to be the same object (docs/09 §6 identity conflict); a human decides whether they are one object or two",
					t.ObjectType, t.ObjectID),
			})
		}
	}
	if !c.TargetMoved {
		// Nothing on the target side to diverge from — except a
		// visibility move, which is never auto regardless (docs/12 §3).
		if contains(c.ChangedFields, "visibility_policy_id") {
			v.Conflicts = append(v.Conflicts, rightsVisibilityChange())
		}
		return finishObject(v)
	}
	// Group diverging fields by their conflict category so one conflict
	// per category carries all its fields, in canonical field order.
	groups := newConflictGroups()
	for _, field := range canonicalObjectFields {
		if !contains(c.ChangedFields, field) || !contains(c.TargetChangedFields, field) {
			continue
		}
		keys, diverged := objectFieldDiverged(c, field)
		if !diverged {
			continue
		}
		groups.add(objectFieldCategory(c.ObjectType, field), field, keys)
	}
	for _, g := range groups.list {
		v.Conflicts = append(v.Conflicts, Conflict{
			Category:    g.cat,
			Code:        codeForCategory(g.cat),
			Fields:      g.fields,
			PayloadKeys: g.keys,
			Detail:      objectConflictDetail(c, g.cat, g.fields),
		})
	}
	// A visibility move the target did not diverge on is still never auto
	// (docs/12 §3): it needs explicit authorized confirmation. A diverged
	// policy already produced a rights conflict above.
	if contains(c.ChangedFields, "visibility_policy_id") && !groups.hasField("visibility_policy_id") {
		v.Conflicts = append(v.Conflicts, rightsVisibilityChange())
	}
	return finishObject(v)
}

// objectFieldDiverged reports whether the field changed to different
// values on the two branches, and — for payload — which top-level keys
// diverged.
func objectFieldDiverged(c diff.ObjectChange, field string) ([]string, bool) {
	src, tgt := c.SourceVersion, *c.TargetVersion
	switch field {
	case "title":
		return nil, src.Title != tgt.Title
	case "payload":
		// Protocol content never merges appends (no branch validated the
		// combined steps, docs/09 §7); every other payload merges
		// append-only changes.
		keys := divergedPayloadKeys(c.BaseVersion.Payload, src.Payload, tgt.Payload, c.ObjectType != "protocol")
		return keys, len(keys) > 0
	case "lifecycle_state":
		return nil, src.LifecycleState != tgt.LifecycleState
	case "schema_ref":
		return nil, src.SchemaRef != tgt.SchemaRef
	case "visibility_policy_id":
		return nil, !ptrStringEqual(src.VisibilityPolicyID, tgt.VisibilityPolicyID)
	}
	return nil, false
}

// objectFieldCategory maps one diverging object field to its conflict
// category. Protocol content fields are scientific conflicts (docs/09 §7:
// Protocol 冲突数值折中禁止自动 — task acceptance criterion); knowledge
// objects' payloads are knowledge conflicts; everything else is an
// attribute conflict. Schema and visibility moves keep their own
// categories whatever the object type (schema compatibility and rights
// are their own review dimensions, docs/09 §6).
func objectFieldCategory(objectType, field string) ConflictCategory {
	switch field {
	case "schema_ref":
		return CategorySchema
	case "visibility_policy_id":
		return CategoryRights
	case "payload":
		if objectType == "protocol" {
			return CategoryScientific
		}
		if knowledgeObjectTypes[objectType] {
			return CategoryKnowledge
		}
		return CategoryAttribute
	default: // title, lifecycle_state
		if objectType == "protocol" {
			return CategoryScientific
		}
		return CategoryAttribute
	}
}

// classifyRelation derives one relation change's verdict (the same
// boundary as objects). Diverging endpoint pins are dependency conflicts
// (which upstream version the edge depends on, docs/19 §3); diverging
// type/payload are relation conflicts — refined to knowledge on
// knowledge-category edge types, whose payload is evidence metadata
// (docs/09 §6 "Knowledge conflict: ... evidence conflict").
func classifyRelation(c diff.RelationChange) RelationVerdict {
	v := RelationVerdict{
		RelationID: c.RelationID,
		Kind:       c.Kind,
		Conflicts:  []Conflict{},
	}
	if !c.TargetMoved {
		return finishRelation(v)
	}
	knowledgeEdge := isKnowledgeRelation(baseRelationType(c))
	groups := newConflictGroups()
	for _, field := range canonicalRelationFields {
		if !contains(c.ChangedFields, field) || !contains(c.TargetChangedFields, field) {
			continue
		}
		keys, diverged := relationFieldDiverged(c, field)
		if !diverged {
			continue
		}
		groups.add(relationFieldCategory(field, knowledgeEdge), field, keys)
	}
	for _, g := range groups.list {
		v.Conflicts = append(v.Conflicts, Conflict{
			Category:    g.cat,
			Code:        codeForCategory(g.cat),
			Fields:      g.fields,
			PayloadKeys: g.keys,
			Detail:      relationConflictDetail(c, g.cat, g.fields),
		})
	}
	return finishRelation(v)
}

// baseRelationType returns the relation type to categorize by: the base
// head's (always present when the target moved), else the source's.
func baseRelationType(c diff.RelationChange) string {
	if c.BaseVersion != nil {
		return c.BaseVersion.RelationType
	}
	return c.SourceVersion.RelationType
}

// isKnowledgeRelation reports whether the relation type is a
// knowledge-category edge (evidence-shaped: supports, contradicts, …).
func isKnowledgeRelation(relationType string) bool {
	e, ok := relationcatalog.Lookup(relationType)
	return ok && e.Category == relationcatalog.CategoryKnowledge
}

// relationFieldDiverged mirrors objectFieldDiverged for relation fields.
func relationFieldDiverged(c diff.RelationChange, field string) ([]string, bool) {
	src, tgt := c.SourceVersion, *c.TargetVersion
	switch field {
	case "relation_type":
		return nil, src.RelationType != tgt.RelationType
	case "source_object_version_id":
		return nil, src.SourceObjectVersionID != tgt.SourceObjectVersionID
	case "target_object_version_id":
		return nil, src.TargetObjectVersionID != tgt.TargetObjectVersionID
	case "payload":
		keys := divergedPayloadKeys(c.BaseVersion.Payload, src.Payload, tgt.Payload, true)
		return keys, len(keys) > 0
	}
	return nil, false
}

// relationFieldCategory maps one diverging relation field to its conflict
// category.
func relationFieldCategory(field string, knowledgeEdge bool) ConflictCategory {
	switch field {
	case "source_object_version_id", "target_object_version_id":
		return CategoryDependency
	case "payload":
		if knowledgeEdge {
			return CategoryKnowledge
		}
		return CategoryRelation
	default: // relation_type
		if knowledgeEdge {
			return CategoryKnowledge
		}
		return CategoryRelation
	}
}

// rightsVisibilityChange is the conflict a source-side visibility move
// always carries when the target did not diverge on it: never
// auto-merged, needs explicit authorized confirmation (docs/12 §3).
func rightsVisibilityChange() Conflict {
	return Conflict{
		Category:    CategoryRights,
		Code:        CodeRightsVisibilityChange,
		Fields:      []string{"visibility_policy_id"},
		PayloadKeys: []string{},
		Detail: "the change moves the object's visibility policy — visibility moves are never auto-merged: " +
			"any expansion requires explicit authorized confirmation with an audit event (docs/12 §3)",
	}
}

// codeForCategory maps a category to its stable conflict code.
func codeForCategory(cat ConflictCategory) string {
	switch cat {
	case CategoryAttribute:
		return CodeAttributeFieldDiverges
	case CategoryRelation:
		return CodeRelationFieldDiverges
	case CategorySchema:
		return CodeSchemaFieldDiverges
	case CategoryKnowledge:
		return CodeKnowledgeFieldDiverges
	case CategoryRights:
		return CodeRightsFieldDiverges
	case CategoryDependency:
		return CodeDependencyFieldDiverges
	case CategoryScientific:
		return CodeScientificFieldDiverges
	}
	return ""
}

// conflictGroup is the diverging-field roll-up of one category: one
// conflict per category, carrying all its fields in canonical field order
// and its payload keys sorted by name.
type conflictGroup struct {
	cat    ConflictCategory
	fields []string
	keys   []string
}

type conflictGroups struct {
	list    []*conflictGroup
	byCat   map[ConflictCategory]*conflictGroup
	byField map[string]bool
}

func newConflictGroups() *conflictGroups {
	return &conflictGroups{
		byCat:   make(map[ConflictCategory]*conflictGroup),
		byField: make(map[string]bool),
	}
}

// add records one diverging field under its category, preserving
// first-seen (canonical field) order.
func (g *conflictGroups) add(cat ConflictCategory, field string, payloadKeys []string) {
	grp, ok := g.byCat[cat]
	if !ok {
		grp = &conflictGroup{cat: cat, keys: []string{}}
		g.byCat[cat] = grp
		g.list = append(g.list, grp)
	}
	grp.fields = append(grp.fields, field)
	grp.keys = append(grp.keys, payloadKeys...)
	sort.Strings(grp.keys)
	g.byField[field] = true
}

// hasField reports whether any grouped conflict covers the field.
func (g *conflictGroups) hasField(field string) bool {
	return g.byField[field]
}

// objectConflictDetail renders the human explanation of one grouped
// object conflict.
func objectConflictDetail(c diff.ObjectChange, cat ConflictCategory, fields []string) string {
	joined := strings.Join(fields, ", ")
	switch cat {
	case CategorySchema:
		return fmt.Sprintf("both branches moved the schema reference of %s %s to different schema versions", c.ObjectType, c.ObjectID)
	case CategoryRights:
		return fmt.Sprintf("both branches moved the visibility policy of %s %s to different policies — a rights conflict (docs/09 §7): never auto-resolved", c.ObjectType, c.ObjectID)
	case CategoryKnowledge:
		return fmt.Sprintf("both branches changed the payload of %s %s to different values — a knowledge conflict (docs/09 §6): claim assessment/evidence diverged", c.ObjectType, c.ObjectID)
	case CategoryScientific:
		return fmt.Sprintf("both branches changed %s of protocol %s to different values — a scientific conflict (docs/09 §7): scientific conflicts are never auto-resolved", joined, c.ObjectID)
	default:
		return fmt.Sprintf("both branches changed %s of %s %s to different values", joined, c.ObjectType, c.ObjectID)
	}
}

// relationConflictDetail renders the human explanation of one grouped
// relation conflict.
func relationConflictDetail(c diff.RelationChange, cat ConflictCategory, fields []string) string {
	joined := strings.Join(fields, ", ")
	switch cat {
	case CategoryDependency:
		return fmt.Sprintf("both branches moved the endpoint pins (%s) of relation %s to different object versions — a dependency conflict (docs/19 §3)", joined, c.RelationID)
	case CategoryKnowledge:
		return fmt.Sprintf("both branches changed %s of the knowledge relation %s to different values — an evidence conflict (docs/09 §6)", joined, c.RelationID)
	default:
		return fmt.Sprintf("both branches changed %s of relation %s to different values", joined, c.RelationID)
	}
}

func finishObject(v ObjectVerdict) ObjectVerdict {
	v.AutoMergeable = len(v.Conflicts) == 0
	return v
}

func finishRelation(v RelationVerdict) RelationVerdict {
	v.AutoMergeable = len(v.Conflicts) == 0
	return v
}

func contains(fields []string, field string) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
