package dependencyimpact

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestTriggerNamesAreRegisteredEvents reads the event registry and fails if
// this package names an event the registry does not have.
//
// It is the check every producer in this tree owes the vocabulary: an event
// type is registered in specs/events/event-types.yaml (which carries the
// names and the envelope, and no payload schemas), and a consumer that
// invented a name would consume something nothing can produce — silently,
// since a scan for an unregistered type simply returns nothing.
//
// The file is read rather than a constant imported: this is the registry,
// not another Go file's opinion of it (the pattern the ledger's own test
// established).
func TestTriggerNamesAreRegisteredEvents(t *testing.T) {
	registered := readRegisteredEvents(t)
	names := TriggerTypes()
	names = append(names, EventImpactDetected)
	for _, name := range names {
		if !registered[name] {
			t.Errorf("event type %q is not registered in specs/events/event-types.yaml", name)
		}
	}
	// The two dependency-group names this package reads about: the analysis
	// emits one and must never consume it as a trigger (a self-triggering
	// analysis walks the graph on its own output forever).
	if registered["dependency.status_changed"] && Triggers("dependency.status_changed") {
		t.Error("dependency.status_changed is treated as a trigger; it is a notification about dependency state, not an upstream change this analysis runs on (docs/18:17)")
	}
	if Triggers(EventImpactDetected) {
		t.Error("dependency.impact_detected is treated as a trigger for itself")
	}
	// The registry really is the registry: a name that cannot exist must
	// not be found, or this test would pass on an empty parse.
	if registered["definitely.not.an.event"] {
		t.Error("the registry parse accepted a name that cannot be in it — the assertion above would be vacuous")
	}
}

// readRegisteredEvents parses the `events:` list of the registry.
func readRegisteredEvents(t *testing.T) map[string]bool {
	t.Helper()
	path := filepath.Join("..", "..", "..", "specs", "events", "event-types.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	out := map[string]bool{}
	inEvents := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "events:") {
			inEvents = true
			continue
		}
		if !inEvents {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			break // the list ended
		}
		out[strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))] = true
	}
	if len(out) == 0 {
		t.Fatal("parsed no event names from specs/events/event-types.yaml — the file's shape changed")
	}
	return out
}

// TestResolveSubject reads each trigger's payload shape.
//
// The abort case is the one that matters most and the one a careless
// resolver gets wrong: the event carries two version numbers (the version
// that RECORDS the abort and the version that WAS aborted), and the subject
// is the aborted one. Getting it backwards would alert every dependent of
// the recording version — which nothing depends on — and the analysis would
// go quiet exactly when it was needed.
func TestResolveSubject(t *testing.T) {
	cases := []struct {
		name    string
		event   string
		payload string
		want    Subject
		wantOK  bool
	}{
		{
			name:    "version_created names the object and the new version",
			event:   EventObjectVersionCreated,
			payload: `{"object_id":"11111111-1111-1111-1111-111111111111","version_no":4}`,
			want:    Subject{Kind: SubjectObject, ID: "11111111-1111-1111-1111-111111111111", VersionNo: 4},
			wantOK:  true,
		},
		{
			name:    "aborted names the ABORTED version, not the recording one",
			event:   EventObjectAborted,
			payload: `{"object_id":"11111111-1111-1111-1111-111111111111","version_no":5,"aborted_version_no":3}`,
			want:    Subject{Kind: SubjectObject, ID: "11111111-1111-1111-1111-111111111111", VersionNo: 3},
			wantOK:  true,
		},
		{
			name:    "reopened names the object",
			event:   EventObjectReopened,
			payload: `{"object_id":"11111111-1111-1111-1111-111111111111","version_no":6}`,
			want:    Subject{Kind: SubjectObject, ID: "11111111-1111-1111-1111-111111111111", VersionNo: 6},
			wantOK:  true,
		},
		{
			name:    "an object trigger with no version number still resolves",
			event:   EventObjectVersionCreated,
			payload: `{"object_id":"11111111-1111-1111-1111-111111111111"}`,
			want:    Subject{Kind: SubjectObject, ID: "11111111-1111-1111-1111-111111111111"},
			wantOK:  true,
		},
		{
			name:    "an asset publish names the published version",
			event:   EventAssetVersionPublished,
			payload: `{"asset_id":"pid-1","asset_version_id":"22222222-2222-2222-2222-222222222222","version":2}`,
			want:    Subject{Kind: SubjectAssetVersion, ID: "22222222-2222-2222-2222-222222222222"},
			wantOK:  true,
		},
		{
			name:    "an object trigger with no object_id is not a subject",
			event:   EventObjectAborted,
			payload: `{"version_no":3}`,
			wantOK:  false,
		},
		{
			name:    "an asset trigger with no version id is not a subject",
			event:   EventAssetVersionPublished,
			payload: `{"asset_id":"pid-1"}`,
			wantOK:  false,
		},
		{
			name:    "a payload that is not JSON is not a subject",
			event:   EventObjectAborted,
			payload: `{`,
			wantOK:  false,
		},
		{
			name:    "an unregistered event type is not a trigger",
			event:   "dependency.status_changed",
			payload: `{"object_id":"11111111-1111-1111-1111-111111111111"}`,
			wantOK:  false,
		},
		{
			name:    "over an empty payload the resolver says no rather than guessing",
			event:   EventObjectVersionCreated,
			payload: `{}`,
			wantOK:  false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ResolveSubject(c.event, []byte(c.payload))
			if ok != c.wantOK {
				t.Fatalf("ResolveSubject(%s, %s) ok = %v, want %v (subject %+v)", c.event, c.payload, ok, c.wantOK, got)
			}
			if !c.wantOK {
				return
			}
			if got != c.want {
				t.Errorf("ResolveSubject(%s, %s) = %+v, want %+v", c.event, c.payload, got, c.want)
			}
		})
	}
}

// TestTriggerSubjectKindsAreDeclaredConsistently pins the trigger table's
// declared subject kind to what its reader actually returns.
//
// The declaration is not decoration: the store's candidate scan reads
// `payload->>'object_id'` for the object triggers and
// `payload->>'asset_version_id'` for the asset one, taking those two arrays
// from ObjectTriggerTypes/AssetTriggerTypes. A trigger listed on the wrong
// side would be a trigger the scan never picks up — the analysis would be
// silent about a change it claims to cover, and nothing else would notice.
func TestTriggerSubjectKindsAreDeclaredConsistently(t *testing.T) {
	byKind := map[SubjectKind][]string{}
	for typ, spec := range triggerTable {
		byKind[spec.kind] = append(byKind[spec.kind], typ)
	}
	for _, kind := range KnownSubjectKinds() {
		types := byKind[kind]
		sort.Strings(types)
		if len(types) == 0 {
			t.Errorf("KnownSubjectKinds reports %q but no trigger declares it", kind)
		}
		var declared []string
		switch kind {
		case SubjectObject:
			declared = ObjectTriggerTypes()
		case SubjectAssetVersion:
			declared = AssetTriggerTypes()
		default:
			t.Fatalf("subject kind %q has no accessor — the store's scan cannot read its payload field", kind)
		}
		if !reflect.DeepEqual(types, declared) {
			t.Errorf("%s triggers = %v from the table, %v from the accessor", kind, types, declared)
		}
		// Each trigger of this kind must actually resolve to it, from a
		// payload shaped the way its kind says.
		for _, typ := range types {
			payload := `{"object_id":"11111111-1111-1111-1111-111111111111"}`
			if kind == SubjectAssetVersion {
				payload = `{"asset_version_id":"22222222-2222-2222-2222-222222222222"}`
			}
			subject, ok := ResolveSubject(typ, []byte(payload))
			if !ok {
				t.Errorf("%s declares kind %q but did not resolve a %s-shaped payload", typ, kind, kind)
				continue
			}
			if subject.Kind != kind {
				t.Errorf("%s resolved a %s payload to subject kind %q, want %q", typ, kind, subject.Kind, kind)
			}
		}
	}
	// The two split arrays must together be exactly the trigger set: an
	// event type in TriggerTypes and in neither split is a candidate the
	// scan's own predicate can never match.
	combined := append(append([]string{}, ObjectTriggerTypes()...), AssetTriggerTypes()...)
	sort.Strings(combined)
	if !reflect.DeepEqual(combined, TriggerTypes()) {
		t.Errorf("object triggers %v + asset triggers %v != TriggerTypes %v",
			ObjectTriggerTypes(), AssetTriggerTypes(), TriggerTypes())
	}
}

// TestKnownSubjectKindsIsDerivedFromTheTable: the list of kinds the store
// must have a statement for is a statement about which triggers exist, not a
// second list kept in step by hand.
func TestKnownSubjectKindsIsDerivedFromTheTable(t *testing.T) {
	want := map[SubjectKind]bool{}
	for _, spec := range triggerTable {
		want[spec.kind] = true
	}
	got := KnownSubjectKinds()
	if len(got) != len(want) {
		t.Fatalf("KnownSubjectKinds() = %v, want %d distinct kinds", got, len(want))
	}
	for _, kind := range got {
		if !want[kind] {
			t.Errorf("KnownSubjectKinds() reports %q, which no trigger declares", kind)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("KnownSubjectKinds() = %v, want it sorted and deduplicated", got)
			break
		}
	}
}
