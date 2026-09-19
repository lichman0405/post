package search

import (
	"encoding/json"
	"sort"
	"testing"
)

// Task T0901 unit suite: the projection's decision surface, without a
// database. What is pinned here is what a database test cannot say as
// directly — that the rule table, the source readers and the rebuild cover
// the same vocabulary (a rule with no reader projects nothing, silently),
// and that the fail-closed branches really are the defaults.

// TestRuleTableAndSourcesCoverTheSameEntityTypes is the closure the
// incremental path and the rebuild both depend on: every rule has a source
// reader (or its event would be consumed and project nothing), and every
// reader is reachable from a rule (or the rebuild would index a class of
// entities no event ever addresses).
func TestRuleTableAndSourcesCoverTheSameEntityTypes(t *testing.T) {
	fromRules := map[string]bool{}
	for _, r := range projectionRules {
		fromRules[r.EntityType] = true
	}
	for entityType := range fromRules {
		if _, err := sourceFor(entityType); err != nil {
			t.Errorf("rule entity type %q has no source reader: %v", entityType, err)
		}
	}
	for entityType := range documentSources {
		if !fromRules[entityType] {
			t.Errorf("source reader %q is reachable from no rule; a rebuild would index it, "+
				"no event would", entityType)
		}
	}
	if len(fromRules) != len(documentSources) {
		t.Errorf("rule entity types = %d, source readers = %d", len(fromRules), len(documentSources))
	}
	// The rebuild walks the rules' entity types, so it must scan exactly the
	// readers' keys.
	want := make([]string, 0, len(documentSources))
	for entityType := range documentSources {
		want = append(want, entityType)
	}
	sort.Strings(want)
	if got := rebuildableEntityTypes(); !equalStrings(got, want) {
		t.Errorf("rebuildableEntityTypes() = %v, want %v", got, want)
	}
}

// TestRuleTableIsClosedAndUnique pins the table's own invariants: no event
// type twice (the lookup takes the first, so a duplicate row would hide a
// second), every row carrying the citation that justifies its payload key,
// and every mapped event type being one the producers actually write.
func TestRuleTableIsClosedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range projectionRules {
		if seen[r.EventType] {
			t.Errorf("event type %q is mapped twice", r.EventType)
		}
		seen[r.EventType] = true
		if r.PayloadKey == "" {
			t.Errorf("rule for %s names no payload key", r.EventType)
		}
		if r.Why == "" {
			t.Errorf("rule for %s carries no citation", r.EventType)
		}
		if !validIdentity(r.EntityType, sampleIdentity(r.EntityType)) {
			t.Errorf("rule for %s: entity type %q has no identity shape", r.EventType, r.EntityType)
		}
	}
	for _, eventType := range mappedEventTypes() {
		if _, ok := RuleFor(eventType); !ok {
			t.Errorf("mappedEventTypes reports %q, RuleFor does not know it", eventType)
		}
	}
	if _, ok := RuleFor("project.created"); ok {
		t.Error("RuleFor claims a rule for an event type the projection does not map")
	}
	if mapped := mappedEventTypes(); len(mapped) != len(projectionRules) {
		t.Errorf("mappedEventTypes() = %v, want one entry per rule (%d)", mapped, len(projectionRules))
	}
}

// TestProjectedVisibilityIsFailClosed is the unit half of the leak
// protection: search_documents.visibility has no CHECK constraint
// (00013), so the default branch of projectedVisibility is the only thing
// between a mis-derived value and a private row being served publicly.
// Every case that is not BOTH axes saying public must answer private.
func TestProjectedVisibilityIsFailClosed(t *testing.T) {
	cases := []struct {
		name              string
		ownAxisPublic     bool
		projectVisibility string
		want              string
	}{
		{"public axis in a public project", true, VisibilityPublic, VisibilityPublic},
		{"private axis in a public project", false, VisibilityPublic, VisibilityPrivate},
		{"public axis in a private project", true, VisibilityPrivate, VisibilityPrivate},
		{"private axis in a private project", false, VisibilityPrivate, VisibilityPrivate},
		{"unknown project visibility", true, "internal", VisibilityPrivate},
		{"empty project visibility", true, "", VisibilityPrivate},
		{"unknown token with a closed axis", false, "internal", VisibilityPrivate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectedVisibility(tc.ownAxisPublic, tc.projectVisibility); got != tc.want {
				t.Errorf("projectedVisibility(%v, %q) = %q, want %q",
					tc.ownAxisPublic, tc.projectVisibility, got, tc.want)
			}
			if got := projectedVisibility(tc.ownAxisPublic, tc.projectVisibility); got != VisibilityPublic && got != VisibilityPrivate {
				t.Errorf("visibility %q is outside the vocabulary the read query compares against", got)
			}
		})
	}
}

// TestPayloadIdentityFor pins the identity read: only a non-empty string of
// the entity type's own shape counts as "the event names this entity".
// Anything else means the event projects nothing — the fail-closed
// direction, because the alternative is indexing whatever the payload
// happened to contain.
func TestPayloadIdentityFor(t *testing.T) {
	const (
		pid  = "01j9z6k3m4n5p6q7r8s9t0v1w2"
		uuid = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	)
	asset, _ := RuleFor(EventTypeAssetVersionPublished)
	state, _ := RuleFor(EventTypeStateCommitted)
	cases := []struct {
		name   string
		rule   projectionRule
		body   string
		want   string
		reason string
	}{
		{"an asset pid", asset, `{"asset_id":"` + pid + `"}`, pid, ""},
		{"a uuid where a pid belongs", asset, `{"asset_id":"` + uuid + `"}`, "", ""},
		{"a pid that is not the right length", asset, `{"asset_id":"01j9z6k3m4n5p6q7r8s9t0v1w"}`, "", ""},
		{"a pid with an excluded letter", asset, `{"asset_id":"01j9z6k3m4n5p6q7r8s9t0v1wL"}`, "", ""},
		{"an absent key", asset, `{"asset_version_id":"` + uuid + `"}`, "", ""},
		{"an empty value", asset, `{"asset_id":""}`, "", ""},
		{"a whitespace value", asset, `{"asset_id":"   "}`, "", ""},
		{"a null value", asset, `{"asset_id":null}`, "", ""},
		{"a numeric value", asset, `{"asset_id":123}`, "", ""},
		{"an object value", asset, `{"asset_id":{"pid":"` + pid + `"}}`, "", ""},
		{"a nested-only key", asset, `{"asset":{"id":"` + pid + `"}}`, "", ""},
		{"a malformed payload", asset, `{not json`, "", ""},
		{"an empty payload", asset, ``, "", ""},
		{"an array payload", asset, `["` + pid + `"]`, "", ""},
		{"a state uuid", state, `{"state_id":"` + uuid + `"}`, uuid, ""},
		{"a state addressed by a pid", state, `{"state_id":"` + pid + `"}`, "", ""},
		{"an upper-case uuid", state, `{"state_id":"3F8A1C62-9B4D-4F1E-8A77-0C2D5E6F7A80"}`, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.payloadIdentityFor([]byte(tc.body)); got != tc.want {
				t.Errorf("%s.payloadIdentityFor(%s) = %q, want %q", tc.rule.EventType, tc.body, got, tc.want)
			}
		})
	}
}

// TestValidIdentity pins the shapes per entity type, including the types
// with no rule (a value here would be a vocabulary the projection invented).
func TestValidIdentity(t *testing.T) {
	const (
		pid  = "01j9z6k3m4n5p6q7r8s9t0v1w2"
		uuid = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	)
	cases := []struct {
		entityType, id string
		want           bool
	}{
		{EntityAsset, pid, true},
		{EntityAsset, uuid, false},
		{EntityKnowledge, pid, true},
		{EntityKnowledge, uuid, false},
		{EntityRelease, uuid, true},
		{EntityRelease, pid, false},
		{EntityState, uuid, true},
		{EntityState, pid, false},
		{"dataset", pid, false},
		{EntityAsset, "", false},
	}
	for _, tc := range cases {
		if got := validIdentity(tc.entityType, tc.id); got != tc.want {
			t.Errorf("validIdentity(%q, %q) = %v, want %v", tc.entityType, tc.id, got, tc.want)
		}
	}
}

// TestEntityRefIsTheDocumentKey pins the reference shape: the entity type
// and its identity joined by one colon, the same "kind:value" convention
// the contribution ledger's refs use.
func TestEntityRefIsTheDocumentKey(t *testing.T) {
	if got := EntityRef(EntityAsset, "abc"); got != "asset:abc" {
		t.Errorf("EntityRef = %q, want %q", got, "asset:abc")
	}
	if EntityRef(EntityAsset, "x") == EntityRef(EntityKnowledge, "x") {
		t.Error("two entity types render the same ref; the primary key collides")
	}
}

// TestStructuredFacetsAreStable pins the facets a document carries: the
// entity type and project id are always present, the caller's facets are
// kept, and the bytes are stable across calls (a map is marshalled with
// sorted keys), so an incremental projection and a rebuild write identical
// structured values.
func TestStructuredFacetsAreStable(t *testing.T) {
	first, err := structuredFacets(EntityAsset, "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80",
		map[string]string{"pid": "01j9z6k3m4n5p6q7r8s9t0v1w2", "slug": "s", "asset_type": "dataset", "version": "v1"})
	if err != nil {
		t.Fatalf("structuredFacets: %v", err)
	}
	second, err := structuredFacets(EntityAsset, "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80",
		map[string]string{"version": "v1", "asset_type": "dataset", "slug": "s", "pid": "01j9z6k3m4n5p6q7r8s9t0v1w2"})
	if err != nil {
		t.Fatalf("structuredFacets: %v", err)
	}
	if string(first) != string(second) {
		t.Errorf("facets are not stable across insertion order:\n%s\n%s", first, second)
	}
	var got map[string]string
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatalf("facets are not a JSON object: %v (%s)", err, first)
	}
	for _, key := range []string{"entity_type", "project_id", "pid", "slug", "asset_type", "version"} {
		if _, ok := got[key]; !ok {
			t.Errorf("facets are missing %q: %s", key, first)
		}
	}
	if got["entity_type"] != EntityAsset {
		t.Errorf("facets entity_type = %q, want %q", got["entity_type"], EntityAsset)
	}
	// The facet map must not leak the caller's map into the encoding: a
	// caller that reuses its map must not be able to change a document.
	facets := map[string]string{"pid": "p"}
	if _, err := structuredFacets(EntityAsset, "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80", facets); err != nil {
		t.Fatalf("structuredFacets: %v", err)
	}
	if len(facets) != 1 {
		t.Errorf("structuredFacets mutated the caller's map: %v", facets)
	}
}

// TestStateTitleAndJoinContent pin the text a document carries.
func TestStateTitleAndJoinContent(t *testing.T) {
	cases := []struct {
		name, branch, message, wantTitle, wantContent string
	}{
		{"branch and one-line message", "main", "Accept the run", "[main] Accept the run", "Accept the run\nmain"},
		{"only the first line becomes the title", "main", "Accept the run\n\nwith details", "[main] Accept the run", "Accept the run\n\nwith details\nmain"},
		{"an empty branch", "", "Accept the run", "Accept the run", "Accept the run"},
		{"an empty message", "main", "", "[main] ", "main"},
		{"whitespace is trimmed out of the content", "main", "  spaced  ", "[main] spaced", "spaced\nmain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stateTitle(tc.branch, tc.message); got != tc.wantTitle {
				t.Errorf("stateTitle(%q, %q) = %q, want %q", tc.branch, tc.message, got, tc.wantTitle)
			}
			if got := joinContent(tc.message, tc.branch); got != tc.wantContent {
				t.Errorf("joinContent(%q, %q) = %q, want %q", tc.message, tc.branch, got, tc.wantContent)
			}
		})
	}
	if got := joinContent("", "  ", ""); got != "" {
		t.Errorf("joinContent of empty parts = %q, want the empty string", got)
	}
}

// TestMappedEventTypesAreTheSpecVocabulary pins the four event types the
// projection maps against the spellings specs/events/event-types.yaml uses.
// A producer that starts emitting under a different name is a coverage gap
// the consumer counts (PassReport.Unmapped), not one this table hides.
func TestMappedEventTypesAreTheSpecVocabulary(t *testing.T) {
	want := []string{
		"knowledge.version_published",
		"release.published",
		"research_asset.version_published",
		"state.committed",
	}
	if got := mappedEventTypes(); !equalStrings(got, want) {
		t.Errorf("mappedEventTypes() = %v, want %v", got, want)
	}
}

func sampleIdentity(entityType string) string {
	switch entityType {
	case EntityAsset, EntityKnowledge:
		return "01j9z6k3m4n5p6q7r8s9t0v1w2"
	default:
		return "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
