package dependencyimpact

import (
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// The acceptance requires the alert's payload to be asserted FIELD BY
// FIELD, and this is that assertion: the exact key set, every value, and
// the four facts the requirement names (the affected entity's identity, the
// direct/indirect distinction, the triggering upstream object, the review
// marker).
//
// The key set is asserted as a SET rather than by reading fields out of the
// JSON, so a field that stops being written is a failure here and not a
// silent absence downstream — and a field added without updating this test
// fails too, which is the point: the payload is a contract, and a contract
// nobody wrote down is one nobody can rely on.
func TestAlertEventFields(t *testing.T) {
	occurred := time.Date(2026, 9, 20, 14, 3, 2, 0, time.UTC)
	trigger := Trigger{
		EventID:       "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa",
		EventType:     EventObjectAborted,
		OccurredAt:    occurred,
		ActorID:       "11111111-2222-4333-8444-555555555555",
		ProjectID:     "99999999-9999-4999-8999-999999999999",
		Visibility:    events.VisibilityPublic,
		CorrelationID: "corr-1",
		Subject:       Subject{Kind: SubjectObject, ID: "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb", VersionNo: 3},
	}
	impact := Impact{
		Kind:       AffectedObject,
		ID:         "cccccccc-1111-4111-8111-cccccccccccc",
		ProjectID:  "dddddddd-1111-4111-8111-dddddddddddd",
		ObjectID:   "cccccccc-1111-4111-8111-cccccccccccc",
		ObjectType: "experiment",
		Hops:       2,
		Directness: Indirect,
	}
	ev, err := AlertEvent(trigger, "eeeeeeee-1111-4111-8111-eeeeeeeeeeee", impact)
	if err != nil {
		t.Fatalf("AlertEvent: %v", err)
	}

	// The envelope. The address is the AFFECTED project — the people who owe
	// the review — never the upstream one, which would tell a publisher that
	// something of theirs is depended on.
	if ev.EventType != EventImpactDetected {
		t.Errorf("event type = %q, want %q", ev.EventType, EventImpactDetected)
	}
	if ev.ProjectID != impact.ProjectID {
		t.Errorf("envelope project = %q, want the affected project %q", ev.ProjectID, impact.ProjectID)
	}
	if ev.ProjectID == trigger.ProjectID || ev.ProjectID == "eeeeeeee-1111-4111-8111-eeeeeeeeeeee" {
		t.Error("envelope project names the upstream side; the alert is addressed to the affected project")
	}
	if ev.Visibility != events.VisibilityPrivate {
		t.Errorf("envelope visibility = %q, want private: the alert names two entities and the fan-out can authorize only the target", ev.Visibility)
	}
	if ev.ActorID != "" {
		t.Errorf("envelope actor = %q, want empty: nobody acted, and an actor would let the ledger attribute the alert to a person", ev.ActorID)
	}
	if ev.CorrelationID != trigger.CorrelationID {
		t.Errorf("correlation id = %q, want the trigger's %q (docs/26 §2: the alert traces back to the request that caused it)", ev.CorrelationID, trigger.CorrelationID)
	}

	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload is not a JSON object: %v (%s)", err, ev.Payload)
	}
	want := map[string]any{
		"trigger_event_id":     trigger.EventID,
		"trigger_event_type":   EventObjectAborted,
		"trigger_occurred_at":  occurred.Format(time.RFC3339),
		"upstream_kind":        string(SubjectObject),
		"upstream_id":          trigger.Subject.ID,
		"upstream_project_id":  "eeeeeeee-1111-4111-8111-eeeeeeeeeeee",
		"upstream_version_no":  float64(3),
		"affected_kind":        string(AffectedObject),
		"affected_id":          impact.ID,
		"affected_project_id":  impact.ProjectID,
		"affected_object_id":   impact.ObjectID,
		"affected_object_type": "experiment",
		"directness":           string(Indirect),
		"hops":                 float64(2),
		"review_required":      true,
	}
	gotKeys := make([]string, 0, len(payload))
	for k := range payload {
		gotKeys = append(gotKeys, k)
	}
	wantKeys := make([]string, 0, len(want))
	for k := range want {
		wantKeys = append(wantKeys, k)
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if !reflect.DeepEqual(gotKeys, wantKeys) {
		t.Fatalf("payload keys = %v, want %v", gotKeys, wantKeys)
	}
	for k, wantV := range want {
		if got := payload[k]; got != wantV {
			t.Errorf("payload[%q] = %#v, want %#v", k, got, wantV)
		}
	}
	// The two facts docs/18 §5 names, asserted as facts and not as
	// presence: review_required is TRUE (「系统只标记 review required」) and
	// the directness is the DATA's, carried through from the impact.
	if payload[AlertFieldReviewRequired] != true {
		t.Error("review_required is not true: the analysis never changes a scientific conclusion, so a human is the only thing it can be asking for")
	}
	if payload[AlertFieldDirectness] != string(Indirect) || payload[AlertFieldHops] != float64(2) {
		t.Errorf("directness/hops = %v/%v, want indirect/2", payload[AlertFieldDirectness], payload[AlertFieldHops])
	}
}

// TestAlertEventOmitsFieldsThatDoNotApply: a project dependent has no object
// id and no object type, and an object trigger that named no version has no
// version number. They are OMITTED rather than sent empty or zero.
//
// An empty string is a value a consumer has to interpret ("is this an empty
// id or a missing one?"), and a zero version number is a LIE — there is no
// version 0, and "version 0 was aborted" is a message about a version that
// cannot exist.
func TestAlertEventOmitsFieldsThatDoNotApply(t *testing.T) {
	trigger := Trigger{
		EventID:    "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa",
		EventType:  EventAssetVersionPublished,
		OccurredAt: time.Date(2026, 9, 20, 14, 3, 2, 0, time.UTC),
		Subject:    Subject{Kind: SubjectAssetVersion, ID: "bbbbbbbb-1111-4111-8111-bbbbbbbbbbbb"},
	}
	projectImpact := Impact{
		Kind: AffectedProject, ID: "dddddddd-1111-4111-8111-dddddddddddd",
		ProjectID: "dddddddd-1111-4111-8111-dddddddddddd", Hops: 1, Directness: Direct,
	}
	ev, err := AlertEvent(trigger, "eeeeeeee-1111-4111-8111-eeeeeeeeeeee", projectImpact)
	if err != nil {
		t.Fatalf("AlertEvent: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for _, absent := range []string{"affected_object_id", "affected_object_type", "upstream_version_no"} {
		if v, present := payload[absent]; present {
			t.Errorf("payload carries %q = %#v, want it omitted for a project dependent with no named version", absent, v)
		}
	}
	if payload["upstream_kind"] != string(SubjectAssetVersion) {
		t.Errorf("upstream_kind = %v, want %q", payload["upstream_kind"], SubjectAssetVersion)
	}
	if payload[AlertFieldDirectness] != string(Direct) || payload[AlertFieldHops] != float64(1) {
		t.Errorf("directness/hops = %v/%v, want direct/1", payload[AlertFieldDirectness], payload[AlertFieldHops])
	}
}

// TestAlertEventRefusesAHopCountThatNamesNoEntity: hops is the distance to
// the affected entity, so zero or negative describes nothing. The event is
// refused rather than written with a number no reader can act on.
func TestAlertEventRefusesAHopCountThatNamesNoEntity(t *testing.T) {
	trigger := Trigger{EventID: "a", EventType: EventObjectAborted, OccurredAt: time.Now()}
	for _, hops := range []int{0, -1} {
		imp := Impact{Kind: AffectedObject, ID: "x", ProjectID: "p", Hops: hops}
		if _, err := AlertEvent(trigger, "u", imp); !errors.Is(err, ErrValidation) {
			t.Errorf("AlertEvent with hops=%d returned %v, want ErrValidation", hops, err)
		}
	}
}

// TestAlertPayloadCarriesNoBulkData: events carry identity and reference,
// never the referenced content (the envelope rule docs/52 states for jobs
// and events.Event restates). The alert names ids, a type, a hop count and a
// marker; it must not grow a title, a payload body or a project name, each
// of which would put someone else's content in a message addressed to
// another project.
func TestAlertPayloadCarriesNoBulkData(t *testing.T) {
	trigger := Trigger{
		EventID: "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa", EventType: EventObjectAborted,
		OccurredAt: time.Now(), Subject: Subject{Kind: SubjectObject, ID: "b", VersionNo: 1},
	}
	imp := Impact{Kind: AffectedObject, ID: "c", ProjectID: "d", ObjectID: "c", ObjectType: "claim",
		Hops: 1, Directness: Direct}
	ev, err := AlertEvent(trigger, "e", imp)
	if err != nil {
		t.Fatalf("AlertEvent: %v", err)
	}
	// Every value in the payload is an identity, an enumeration or a
	// number. A field whose value is a long free-text string would be
	// content travelling between projects without its read gate.
	var payload map[string]any
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	for k, v := range payload {
		if s, ok := v.(string); ok && len(s) > 64 {
			t.Errorf("payload[%q] is a %d-character string; the alert carries identifiers, not content", k, len(s))
		}
	}
	// The upstream actor is NOT copied into the payload either: the alert
	// tells the affected project that something changed, not who changed it
	// (that is readable from the upstream entity itself, under the reader's
	// own access).
	if _, present := payload["actor_id"]; present {
		t.Error("payload carries actor_id; the change's actor is reachable through trigger_event_id under the reader's own access")
	}
}

// TestAlertEventFieldNamesAreStable pins the three payload field names the
// analysis's dedupe index is keyed on.
//
// Migration 00111's partial unique index extracts exactly
// trigger_event_id, affected_kind and affected_id from the payload; if the
// constants and the index disagreed, the ON CONFLICT would never fire and
// the analysis would write one alert per pass instead of one per change —
// a silent, growing duplicate. The integration test proves the write is
// idempotent; this proves the names it is idempotent ON are the ones the
// schema keys.
func TestAlertEventFieldNamesAreStable(t *testing.T) {
	got := []string{AlertFieldTriggerEventID, AlertFieldAffectedKind, AlertFieldAffectedID}
	want := []string{"trigger_event_id", "affected_kind", "affected_id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dedupe payload fields = %v, want %v (infra/migrations/00111_dependency_impact.sql keys on these)", got, want)
	}
	if AlertFieldReviewRequired != "review_required" {
		t.Errorf("review field = %q, want review_required (docs/18 §5)", AlertFieldReviewRequired)
	}
	if !strings.Contains(EventImpactDetected, "dependency.impact_detected") {
		t.Errorf("event name = %q, want the registered dependency.impact_detected", EventImpactDetected)
	}
}
