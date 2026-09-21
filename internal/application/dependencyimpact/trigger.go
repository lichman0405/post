package dependencyimpact

import (
	"encoding/json"
	"sort"
)

// The trigger vocabulary: which recorded upstream changes this analysis
// runs on, and how each names its subject.
//
// Every name below is registered in specs/events/event-types.yaml — this
// package coins none of them, and a test in this package reads that file
// and fails if a name here is not in it (the pattern
// internal/contribution/ledger.go's mapping table established). The event
// names are also spelled as constants in the packages that PRODUCE them
// (internal/application/rsg/events.go, internal/application/aborts/
// events.go); they are spelled again here rather than imported because a
// producer's constant is its own contract, and a consumer that imported it
// would be coupled to the producer's package rather than to the log.
const (
	// EventObjectVersionCreated is `scientific_object.version_created`
	// (specs/events/event-types.yaml:21), written by the RSG commit path.
	// docs/18 §5 names "new version" as a trigger.
	EventObjectVersionCreated = "scientific_object.version_created"
	// EventObjectAborted is `scientific_object.aborted` (:22), written by
	// the abort command. docs/18 §5 names "abort" as a trigger, and
	// docs/11 §7's supersede is the same event with a replacement
	// (AbortRecord.replacement_ref): an aborted version pointing at its
	// replacement IS the supersede, which is why one trigger covers both.
	EventObjectAborted = "scientific_object.aborted"
	// EventObjectReopened is `scientific_object.reopened` (:23). It exists
	// in the vocabulary and this analysis covers it for the same reason it
	// covers a new version: a reopened object is a changed upstream.
	EventObjectReopened = "scientific_object.reopened"
	// EventAssetVersionPublished is `research_asset.version_published`
	// (:26), written by the asset publish command. docs/18 §5's "new
	// version" on the asset landing point.
	EventAssetVersionPublished = "research_asset.version_published"

	// EventImpactDetected is the carrier of the alert this package
	// produces: `dependency.impact_detected` (:29), registered in the
	// Dependency group of docs/18:17. It is NOT a trigger for itself — the
	// analysis does not cascade on its own output, which would make one
	// change walk the graph forever.
	EventImpactDetected = "dependency.impact_detected"
)

// triggerSubjectReader reads one trigger payload. ok is false when the
// payload does not name a subject this analysis can resolve, which is a
// data outcome rather than an error: the log is append-only and a payload
// written by an older producer must be counted, not crash the pass.
type triggerSubjectReader func(payload []byte) (Subject, bool)

// triggerSpec is one trigger: what its payload says, and which KIND of
// thing it says it about.
//
// The kind is declared beside the reader rather than left implicit in it
// because the store's candidate scan has to make the same distinction in
// SQL — it reads `payload->>'object_id'` for the object triggers and
// `payload->>'asset_version_id'` for the asset one, and a trigger listed
// on the wrong side would be an event the scan never picks up. Declaring
// the kind here gives the SQL its two parameter arrays from the same table
// the Go side resolves subjects with;
// TestTriggerSubjectKindMatchesPayloads pins the declaration to what the
// readers actually return, so the two cannot drift.
type triggerSpec struct {
	kind SubjectKind
	read triggerSubjectReader
}

// triggerTable is the whole trigger decision, in one place: event type →
// how its subject is read. The map is the only place in this package that
// decides WHICH changes trigger an analysis; the walk below it decides
// only what an analysis of one subject finds.
//
// The object triggers read `object_id`, which every object-side event
// carries (internal/application/rsg/events.go, internal/application/
// aborts/events.go). They do NOT read a version id: the walk is over the
// object (see the package comment), so a payload that names no version —
// or names a version that has since been superseded by a newer one on the
// same object — still resolves.
//
// The abort payload carries two version numbers, `version_no` (the new
// version that records the abort) and `aborted_version_no` (the version
// that was aborted). The SUBJECT is the aborted version: that is the thing
// someone depended on and the thing whose dependents are now stale. The
// recording version is this change's own bookkeeping, and naming it would
// send every alert after the wrong object.
var triggerTable = map[string]triggerSpec{
	EventObjectVersionCreated: {SubjectObject, objectTrigger("version_no")},
	EventObjectAborted:        {SubjectObject, objectTrigger("aborted_version_no")},
	EventObjectReopened:       {SubjectObject, objectTrigger("version_no")},
	EventAssetVersionPublished: {SubjectAssetVersion, func(payload []byte) (Subject, bool) {
		var p struct {
			AssetVersionID string `json:"asset_version_id"`
		}
		if err := json.Unmarshal(payload, &p); err != nil || p.AssetVersionID == "" {
			return Subject{}, false
		}
		return Subject{Kind: SubjectAssetVersion, ID: p.AssetVersionID}, true
	}},
}

// objectTrigger reads an object-side trigger payload: the object id, plus
// the version number named by field (0 when the payload does not carry
// one, which is not a failure — the walk does not need it).
func objectTrigger(versionField string) triggerSubjectReader {
	return func(payload []byte) (Subject, bool) {
		var p struct {
			ObjectID string `json:"object_id"`
		}
		if err := json.Unmarshal(payload, &p); err != nil || p.ObjectID == "" {
			return Subject{}, false
		}
		// The field is read through a RawMessage and then as a number, NOT
		// by unmarshalling the whole payload into map[string]json.Number:
		// every real payload also carries STRING fields (object_id at
		// least), and decoding a string into a json.Number fails the whole
		// unmarshal — so the version number would silently come back as
		// zero on every object event, and the alert would say "this object
		// changed" instead of naming the version. The field itself is still
		// required to be a JSON number: a producer that sent "4" as a
		// string is a producer whose message this analysis will not guess
		// at, and the object-level subject below is unaffected.
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			// The object id resolved and the rest of the payload did not:
			// the subject is still known, so the analysis runs and the alert
			// simply says less. An unreadable version number is not a reason
			// to skip a dependent.
			return Subject{Kind: SubjectObject, ID: p.ObjectID}, true
		}
		no := 0
		if raw, ok := fields[versionField]; ok {
			var n json.Number
			if err := json.Unmarshal(raw, &n); err == nil {
				if v, err := n.Int64(); err == nil && v > 0 {
					no = int(v)
				}
			}
		}
		return Subject{Kind: SubjectObject, ID: p.ObjectID, VersionNo: no}, true
	}
}

// TriggerTypes returns every event type the analysis runs on, sorted. The
// store's candidate scan is parameterised by it, so "which events does the
// analysis consume" has one definition.
func TriggerTypes() []string {
	out := make([]string, 0, len(triggerTable))
	for t := range triggerTable {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Triggers reports whether eventType is a trigger.
func Triggers(eventType string) bool {
	_, ok := triggerTable[eventType]
	return ok
}

// ObjectTriggerTypes returns the trigger event types whose subject is a
// scientific object, sorted: the triggers whose payload names an
// object_id. The store's candidate scan is parameterised by it, so the
// SQL's shape test ("does this event carry an object_id?") and the Go
// resolver's are the same split of the same table.
func ObjectTriggerTypes() []string { return triggerTypesOf(SubjectObject) }

// AssetTriggerTypes returns the trigger event types whose subject is a
// published asset version, sorted — the triggers whose payload names an
// asset_version_id, and the second landing point of the candidate scan.
func AssetTriggerTypes() []string { return triggerTypesOf(SubjectAssetVersion) }

// triggerTypesOf collects the trigger types declaring one subject kind.
// It is unexported because the two names above are the vocabulary: a third
// caller asking for a kind would be a third landing point, and that is a
// change to make in the trigger table, not at a call site.
func triggerTypesOf(kind SubjectKind) []string {
	out := make([]string, 0, len(triggerTable))
	for t, spec := range triggerTable {
		if spec.kind == kind {
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveSubject reads the subject out of one trigger event's payload.
// ok is false when the payload names nothing this analysis can walk from;
// the caller counts those rather than dropping them silently.
func ResolveSubject(eventType string, payload []byte) (Subject, bool) {
	spec, ok := triggerTable[eventType]
	if !ok {
		return Subject{}, false
	}
	s, ok := spec.read(payload)
	if !ok {
		return Subject{}, false
	}
	if s.ID == "" {
		return Subject{}, false
	}
	return s, true
}

// KnownSubjectKinds returns the subject kinds the walk can start from,
// sorted — derived from the trigger table, so it is a statement about which
// triggers exist rather than a second list to keep in step with them.
//
// It is what a test pins the store's per-kind queries to: the store resolves
// a subject's project with one statement per kind, and a trigger added for a
// kind nothing can resolve would be a trigger that silently never fires.
// Derived, that hole shows up as a kind in this list with no statement
// behind it; listed by hand, it would show up as nothing at all.
func KnownSubjectKinds() []SubjectKind {
	seen := map[SubjectKind]bool{}
	for _, spec := range triggerTable {
		seen[spec.kind] = true
	}
	out := make([]SubjectKind, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
