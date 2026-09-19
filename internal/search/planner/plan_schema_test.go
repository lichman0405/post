package planner

import (
	"encoding/json"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/search"
)

// The schema is the validation authority, so it must agree with the two other
// places the same vocabulary is written down: the constants consumers switch
// on (plan.go) and the platform vocabularies this package copies from
// (internal/search's entity types, domain's evidence types). Each test below
// is one of those agreements. A disagreement is a silent hole — the schema
// would accept a value no consumer handles, or refuse one the platform has.

// schemaDoc decodes the packaged schema.
func schemaDoc(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(PlanSchema(), &doc); err != nil {
		t.Fatalf("plan schema is not valid JSON: %v", err)
	}
	return doc
}

// at walks a decoded document by key and index.
func at(t *testing.T, node any, path ...string) any {
	t.Helper()
	cur := node
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %T is not an object", path, cur)
		}
		next, ok := m[key]
		if !ok {
			t.Fatalf("path %v: no key %q", path, key)
		}
		cur = next
	}
	return cur
}

// strings3 converts a decoded JSON array of strings.
func strings3(t *testing.T, v any, what string) []string {
	t.Helper()
	arr, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: %T is not an array", what, v)
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("%s: array element %v is not a string", what, item)
		}
		out = append(out, s)
	}
	return out
}

// TestPlanSchemaVocabularyMatchesConstants pins every closed enum in the
// schema to the Go vocabulary, and the copied ones to their origin:
// target_object.entity_types to the search projection's entity types,
// evidence_preference.types to domain.CanonicalEvidenceTypes (which is itself
// pinned to the canonical schema by the integration drift test), and the
// ranking criteria to docs/14 §3's list.
//
// If the schema and a constant disagree, the planner would accept a document
// whose value no consumer knows; if the copied list drifts from its origin,
// the plan would propose filtering on a vocabulary the index does not have.
func TestPlanSchemaVocabularyMatchesConstants(t *testing.T) {
	doc := schemaDoc(t)
	props := at(t, doc, "properties")

	cases := []struct {
		what string
		path []string
		want []string
	}{
		{"intent", []string{"intent", "enum"}, []string{IntentAnswer, IntentCompare, IntentConditions, IntentOriginAssessment}},
		{"target_object.entity_types", []string{"target_object", "properties", "entity_types", "items", "enum"},
			[]string{search.EntityAsset, search.EntityKnowledge, search.EntityRelease, search.EntityState}},
		{"evidence_preference.types", []string{"evidence_preference", "properties", "types", "items", "enum"},
			domain.CanonicalEvidenceTypes()},
		{"evidence_preference.prefer", []string{"evidence_preference", "properties", "prefer", "items", "enum"},
			[]string{EvidencePreferReviewed, EvidencePreferIndependentlyReproduced, EvidencePreferContradictory}},
		{"condition_scope.comparators.op", []string{"condition_scope", "properties", "comparators", "items", "properties", "op", "enum"},
			[]string{ComparatorLT, ComparatorLTE, ComparatorEQ, ComparatorGTE, ComparatorGT}},
		{"network_scope", []string{"network_scope", "enum"}, []string{NetworkScopePlatform, NetworkScopeWithExternal}},
		{"visibility", []string{"visibility", "enum"}, []string{VisibilityPublic, VisibilityAccessible}},
		{"ranking_constraints.order_by", []string{"ranking_constraints", "properties", "order_by", "items", "enum"},
			[]string{RankQueryScopeMatch, RankEvidenceProfile, RankReviewState, RankIndependentReproduction,
				RankContradictoryEvidence, RankVersionFreshness}},
	}
	for _, tc := range cases {
		t.Run(tc.what, func(t *testing.T) {
			got := strings3(t, at(t, props, tc.path...), tc.what)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("schema %s enum = %v, want %v", tc.what, got, tc.want)
			}
		})
	}

	if got := at(t, props, "plan_version", "const"); got != PlanVersion {
		t.Errorf("plan_version const = %v, want %q", got, PlanVersion)
	}

	required := strings3(t, doc["required"], "required")
	want := []string{"plan_version", "intent", "target_object", "network_scope", "visibility"}
	if !reflect.DeepEqual(required, want) {
		t.Errorf("required = %v, want %v", required, want)
	}
}

// TestPlanSchemaIsClosedAtEveryLevel is the structural negative test: the
// schema refuses unknown keys on the root object AND on every nested object,
// at every depth. That property is what makes "a provider cannot declare a
// field for candidate entity ids" true rather than aspirational — an unknown
// key is refused instead of ignored, so it can never be quietly carried into
// a consumer that later starts reading it.
func TestPlanSchemaIsClosedAtEveryLevel(t *testing.T) {
	doc := schemaDoc(t)
	objects := 0

	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch n := node.(type) {
		case map[string]any:
			if n["type"] == "object" {
				objects++
				closed, ok := n["additionalProperties"]
				if !ok {
					t.Errorf("%s: object declares no additionalProperties; the plan vocabulary is closed", path)
				} else if closed != false {
					t.Errorf("%s: additionalProperties = %v, want false", path, closed)
				}
			}
			keys := make([]string, 0, len(n))
			for k := range n {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(n[k], path+"."+k)
			}
		case []any:
			for i, item := range n {
				walk(item, path+"["+strconv.Itoa(i)+"]")
			}
		}
	}
	walk(doc, "schema")

	// Guard against a walk that visits nothing and passes vacuously.
	if objects < 6 {
		t.Fatalf("only %d object nodes walked: the schema walk is not reaching the nested objects", objects)
	}
}

// TestPlanSchemaDeclaresNoIdentifierField is the vocabulary half of docs/54's
// scenario #7: the plan declares no field that could hold an entity identity,
// under any of the names such a field would plausibly take. Together with the
// additionalProperties:false walk above, it means a provider has no field to
// put a candidate id in — and the identifier guard in planner.go covers the
// free-text values that remain.
func TestPlanSchemaDeclaresNoIdentifierField(t *testing.T) {
	suspect := regexp.MustCompile(`(?i)^(id|ids|uuid|ref|refs|entity_ids?|entity_refs?|entity_uuids?|candidate.*|sources?|hits?)$|(?i)_(id|ids|uuid|ref|refs)$`)
	names := map[string]string{} // property name -> path

	var walk func(node any, path string)
	walk = func(node any, path string) {
		switch n := node.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				for name := range props {
					names[name] = path + ".properties." + name
					if suspect.MatchString(name) {
						t.Errorf("plan vocabulary declares %q at %s: a field that could carry an entity identity", name, path)
					}
				}
			}
			for k, v := range n {
				walk(v, path+"."+k)
			}
		case []any:
			for i, item := range n {
				walk(item, path+"["+strconv.Itoa(i)+"]")
			}
		}
	}
	doc := schemaDoc(t)
	walk(doc, "schema")

	// The walk has to have reached something for the check above to mean
	// anything: without this, a walk that visits nothing passes vacuously.
	if _, ok := names["condition_scope"]; !ok {
		t.Fatal("the walk found no nested properties: it is broken, not the schema")
	}

	// The ROOT vocabulary is exactly docs/14 §2's eight items plus the
	// version pin. Nested names (entity_types, comparators, order_by, ...)
	// are the shape of those items, not items themselves.
	root, ok := at(t, doc, "properties").(map[string]any)
	if !ok {
		t.Fatal("the schema root declares no properties")
	}
	want := []string{"condition_scope", "evidence_preference", "intent", "network_scope", "plan_version",
		"property", "ranking_constraints", "target_object", "visibility"}
	got := make([]string, 0, len(root))
	for name := range root {
		got = append(got, name)
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plan schema's top-level vocabulary = %v, want the eight spec items plus plan_version", got)
	}
}

// TestPlanSchemaIsTheOneThePlannerCompiles: the literal compiles, accepts a
// document in the vocabulary and refuses one outside it. It is the schema's
// own smoke test — if the shipped literal were broken, the planner's unit
// suite would otherwise report it as "every plan falls back", which reads
// like a provider problem.
func TestPlanSchemaIsTheOneThePlannerCompiles(t *testing.T) {
	compiled, err := compilePlanSchema()
	if err != nil {
		t.Fatalf("compilePlanSchema: %v", err)
	}
	if !strings.HasSuffix(planSchemaID, "/1.json") {
		t.Fatalf("plan schema id %q does not carry the vocabulary version", planSchemaID)
	}
	minimal := `{"plan_version":"1","intent":"answer","target_object":{"entity_types":["asset"]},"network_scope":"platform","visibility":"accessible"}`
	var ok any
	if err := json.Unmarshal([]byte(minimal), &ok); err != nil {
		t.Fatalf("minimal plan: %v", err)
	}
	if err := compiled.Validate(ok); err != nil {
		t.Fatalf("the compiled schema refused a document in its own vocabulary: %v", err)
	}
	var bad any
	if err := json.Unmarshal([]byte(strings.Replace(minimal, `"intent":"answer",`, `"intent":"answer","entity_ids":["x"],`, 1)), &bad); err != nil {
		t.Fatalf("plan with an extra field: %v", err)
	}
	if err := compiled.Validate(bad); err == nil {
		t.Fatal("the compiled schema accepted a document with a field outside the vocabulary")
	}
}
