package dependencyimpact

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/events"
)

// fakeStore is the walk's data side, canned: what a change reaches, per
// subject, with no database. The read surface's rules are about what a
// CALLER may be told, so the tests below hold the walk fixed and vary the
// caller — the one thing under test.
type fakeStore struct {
	info       map[Subject]SubjectInfo
	closure    map[string]Analysis
	dependents map[string][]AssetDependent
	infoErr    error
}

func (f *fakeStore) SubjectInfo(_ context.Context, s Subject) (SubjectInfo, error) {
	if f.infoErr != nil {
		return SubjectInfo{}, f.infoErr
	}
	got, ok := f.info[s]
	if !ok {
		return SubjectInfo{}, ErrSubjectNotFound
	}
	return got, nil
}

func (f *fakeStore) ObjectClosure(_ context.Context, objectID string) (Analysis, error) {
	a, ok := f.closure[objectID]
	if !ok {
		return Analysis{Subject: Subject{Kind: SubjectObject, ID: objectID}}, nil
	}
	return a, nil
}

func (f *fakeStore) AssetDependents(_ context.Context, assetVersionID string) ([]AssetDependent, error) {
	return f.dependents[assetVersionID], nil
}

func (f *fakeStore) AnalyzeBatch(_ context.Context, _ int) (Batch, error) { return Batch{}, nil }

// fakeGate answers from a table: the project's standing for this caller.
type fakeGate struct {
	access map[string]ProjectAccess
	err    error
	calls  []string
}

func (g *fakeGate) Access(_ context.Context, _ projects.Reader, projectID string) (ProjectAccess, error) {
	if g.err != nil {
		return ProjectAccess{}, g.err
	}
	g.calls = append(g.calls, projectID)
	return g.access[projectID], nil
}

const (
	upstreamObject  = "11111111-1111-4111-8111-111111111111"
	openProject     = "aaaaaaa1-1111-4111-8111-111111111111"
	closedProject   = "aaaaaaa2-1111-4111-8111-111111111111"
	otherProject    = "aaaaaaa3-1111-4111-8111-111111111111"
	readableObject  = "22222222-2222-4222-8222-222222222222"
	hiddenObject    = "33333333-3333-4333-8333-333333333333"
	secondReadable  = "44444444-4444-4444-8444-444444444444"
	assetVersion    = "55555555-5555-4555-8555-555555555555"
	privateDeclarer = "66666666-6666-4666-8666-666666666666"
)

// TestReadOmitsWhatTheCallerMayNotSee is the privacy rule of the read
// surface: an affected object in a project the caller cannot read is
// DROPPED — no entry, no placeholder, no number.
//
// The assertion is not "the count looks right": it is that the withheld
// project's id appears NOWHERE in the serialized answer, and that the
// answer's shape has no field that could carry a count. That is what makes
// "3 impacts (1 hidden)" impossible to express, which is the disclosure
// docs/23 §5 forbids: knowing that a hidden dependent exists is knowing
// something about a private project.
func TestReadOmitsWhatTheCallerMayNotSee(t *testing.T) {
	store := &fakeStore{
		info: map[Subject]SubjectInfo{
			{Kind: SubjectObject, ID: upstreamObject}: {ProjectID: openProject, Visibility: "public"},
		},
		closure: map[string]Analysis{
			upstreamObject: {
				Subject: Subject{Kind: SubjectObject, ID: upstreamObject},
				Impacts: []Impact{
					{Kind: AffectedObject, ID: readableObject, ProjectID: openProject, ObjectID: readableObject, ObjectType: "experiment", Hops: 1, Directness: Direct},
					{Kind: AffectedObject, ID: hiddenObject, ProjectID: closedProject, ObjectID: hiddenObject, ObjectType: "claim", Hops: 2, Directness: Indirect},
					{Kind: AffectedObject, ID: secondReadable, ProjectID: otherProject, ObjectID: secondReadable, ObjectType: "dataset", Hops: 3, Directness: Indirect},
				},
			},
		},
	}
	gate := &fakeGate{access: map[string]ProjectAccess{
		openProject:  {Visible: true},
		otherProject: {Visible: true},
		// closedProject is absent: the gate answers the zero value, which is
		// exactly what projects.Service.Get answers for a project the reader
		// may not read (ErrProjectNotFound → Visible:false).
	}}
	svc := NewService(store, gate)

	report, err := svc.Read(context.Background(), projects.Reader{UserID: "u1", Authenticated: true},
		Subjects{{Kind: SubjectObject, ID: upstreamObject}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %d, want 1 (a group per subject the caller asked about and may see)", len(report.Groups))
	}
	impacts := report.Groups[0].Impacts
	if len(impacts) != 2 {
		t.Fatalf("impacts = %d (%+v), want 2: the hidden project's dependent must be dropped", len(impacts), impacts)
	}
	if impacts[0].ID != readableObject || impacts[1].ID != secondReadable {
		t.Errorf("impacts = %s/%s, want %s/%s in that order", impacts[0].ID, impacts[1].ID, readableObject, secondReadable)
	}
	// The withheld entity appears NOWHERE — not as an id, not as a name, and
	// not as a count. The serialized form is the strongest available check of
	// "nowhere", because it is what a client actually receives.
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	for _, forbidden := range []string{hiddenObject, closedProject} {
		if bytes.Contains(raw, []byte(forbidden)) {
			t.Errorf("the serialized report contains the withheld id %q: %s", forbidden, raw)
		}
	}
	// Every number in the answer is a property of a row the caller may read
	// (a hop count, a version number). There is no total and no "withheld"
	// field anywhere in the report's own shape.
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	assertNoCountFields(t, generic, "")
}

// assertNoCountFields walks a decoded JSON value and fails on any key that
// looks like a tally. It is deliberately blunt: the rule is about the SHAPE
// of the answer (this repository's Report carries counts nowhere), so a
// field named total/hidden/withheld/dropped/omitted is a design change that
// has to argue with this test, not a value that can be quietly right.
func assertNoCountFields(t *testing.T, v any, path string) {
	t.Helper()
	switch typed := v.(type) {
	case map[string]any:
		for k, child := range typed {
			switch k {
			case "total", "total_count", "count", "hidden", "hidden_count", "withheld", "withheld_count", "dropped", "omitted", "impact_count":
				t.Errorf("report carries %q at %s%s: a count of what was withheld IS the disclosure (docs/23 §5)", k, path, k)
			}
			assertNoCountFields(t, child, path+k+".")
		}
	case []any:
		for i, child := range typed {
			assertNoCountFields(t, child, path)
			_ = i
		}
	}
}

// TestReadHidesTheSubjectItself: a caller may not ask what a change reaches
// when they cannot open the changed thing — the answer would tell them it
// exists. The refusal is the SAME error a nonexistent subject gets, so the
// two cannot be told apart (docs/45 existence hiding).
func TestReadHidesTheSubjectItself(t *testing.T) {
	store := &fakeStore{
		info: map[Subject]SubjectInfo{
			{Kind: SubjectObject, ID: upstreamObject}: {ProjectID: openProject, Visibility: "public"},
			{Kind: SubjectObject, ID: hiddenObject}:   {ProjectID: closedProject, Visibility: "private"},
			{Kind: SubjectObject, ID: secondReadable}: {ProjectID: otherProject, Visibility: "public"},
		},
		closure: map[string]Analysis{},
	}
	gate := &fakeGate{access: map[string]ProjectAccess{
		openProject:  {Visible: true},
		otherProject: {Visible: true},
	}}
	svc := NewService(store, gate)
	reader := projects.Reader{UserID: "u1", Authenticated: true}

	// (a) a subject the caller may not read, (b) a subject that names no row
	// at all: one outcome, and it is not ErrStore (the database is fine) and
	// not a validation error.
	denied, deniedErr := svc.Read(context.Background(), reader, Subjects{{Kind: SubjectObject, ID: hiddenObject}})
	if !IsSubjectNotFound(deniedErr) {
		t.Fatalf("Read of an unreadable subject = %v, want the existence-hiding answer", deniedErr)
	}
	if len(denied.Groups) != 0 {
		t.Errorf("a refused read returned %+v, want nothing", denied)
	}
	missing, missingErr := svc.Read(context.Background(), reader, Subjects{{Kind: SubjectObject, ID: "99999999-9999-4999-8999-999999999999"}})
	if !IsSubjectNotFound(missingErr) {
		t.Fatalf("Read of an unknown subject = %v, want the existence-hiding answer", missingErr)
	}
	if errors.Is(missingErr, ErrStore) {
		t.Error("a hidden subject is reported as a store failure; \"you may not see this\" is not \"the database is unwell\"")
	}
	// The two refusals are the same refusal: same error identity, same
	// (empty) result. A caller cannot tell which of the two happened.
	deniedRaw, _ := json.Marshal(denied)
	missingRaw, _ := json.Marshal(missing)
	if !bytes.Equal(deniedRaw, missingRaw) {
		t.Errorf("a hidden subject answers %s and an unknown one %s: the two must be indistinguishable", deniedRaw, missingRaw)
	}
	if deniedErr.Error() == missingErr.Error() && !IsSubjectNotFound(deniedErr) {
		t.Error("the two refusals share a message but are not the not-found outcome")
	}

	// The whole call fails closed: a list mixing a readable subject with an
	// unreadable one is refused entirely, so the caller cannot probe which
	// entries are real by seeing which ones come back.
	if _, err := svc.Read(context.Background(), reader,
		Subjects{{Kind: SubjectObject, ID: upstreamObject}, {Kind: SubjectObject, ID: hiddenObject}}); !IsSubjectNotFound(err) {
		t.Fatalf("Read of a mixed subject list = %v, want the whole call refused", err)
	}
}

// TestReadRequiresAGate: a service built without the project gate refuses to
// read rather than treating "no gate" as "everyone may read everything".
// The nil port is exactly how that leak would arrive.
func TestReadRequiresAGate(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	if _, err := svc.Read(context.Background(), projects.Reader{}, Subjects{{Kind: SubjectObject, ID: upstreamObject}}); err == nil {
		t.Fatal("Read without a gate returned no error")
	}
}

// TestReadHidesAPrivateAssetDeclarationFromNonMembers is the asset landing
// point's second axis, borrowed from the asset page's own rule
// (assets.ProjectDependencyViewer): a project that declares a dependency
// PRIVATELY is shown to its own members and to nobody else, even when the
// project is public and the caller may read everything in it.
func TestReadHidesAPrivateAssetDeclarationFromNonMembers(t *testing.T) {
	store := &fakeStore{
		info: map[Subject]SubjectInfo{
			{Kind: SubjectAssetVersion, ID: assetVersion}: {ProjectID: openProject, Visibility: "public"},
		},
		dependents: map[string][]AssetDependent{
			assetVersion: {
				{ProjectID: openProject, VisibilityOfUsage: string(events.VisibilityPublic)},
				{ProjectID: privateDeclarer, VisibilityOfUsage: string(events.VisibilityPrivate)},
			},
		},
	}
	gate := &fakeGate{access: map[string]ProjectAccess{
		openProject: {Visible: true},
		// The private declarer's project is PUBLIC and readable — the only
		// thing keeping its declaration off the page is the second axis.
		privateDeclarer: {Visible: true},
	}}
	svc := NewService(store, gate)

	outsider, err := svc.Read(context.Background(), projects.Reader{UserID: "u1", Authenticated: true},
		Subjects{{Kind: SubjectAssetVersion, ID: assetVersion}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := outsider.Groups[0].Impacts; len(got) != 1 || got[0].ID != openProject {
		t.Fatalf("a non-member sees %+v, want only the public declaration in %s", got, openProject)
	}

	gate.access[privateDeclarer] = ProjectAccess{Visible: true, Member: true}
	member, err := svc.Read(context.Background(), projects.Reader{UserID: "u2", Authenticated: true},
		Subjects{{Kind: SubjectAssetVersion, ID: assetVersion}})
	if err != nil {
		t.Fatalf("Read as member: %v", err)
	}
	if got := member.Groups[0].Impacts; len(got) != 2 {
		t.Fatalf("a member of the declaring project sees %+v, want both declarations", got)
	}
}

// TestReadIsAnEmptyAnswerNotAnErrorWhenNothingIsAffected: a subject that
// reaches nothing is an answer the screen renders as "nothing downstream",
// and the group is still present so a client renders a stated absence rather
// than guessing at a missing field.
func TestReadIsAnEmptyAnswerNotAnErrorWhenNothingIsAffected(t *testing.T) {
	store := &fakeStore{
		info:    map[Subject]SubjectInfo{{Kind: SubjectObject, ID: upstreamObject}: {ProjectID: openProject, Visibility: "public"}},
		closure: map[string]Analysis{},
	}
	svc := NewService(store, &fakeGate{access: map[string]ProjectAccess{openProject: {Visible: true}}})
	report, err := svc.Read(context.Background(), projects.Reader{UserID: "u", Authenticated: true},
		Subjects{{Kind: SubjectObject, ID: upstreamObject}})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(report.Groups) != 1 {
		t.Fatalf("groups = %d, want the one subject that was asked about", len(report.Groups))
	}
	if len(report.Groups[0].Impacts) != 0 {
		t.Errorf("impacts = %+v, want none", report.Groups[0].Impacts)
	}
}

// TestAnalyzeSeparatesDirectFromIndirect: the two are different values in
// the returned data, with different hop counts — not one boolean, and not
// one value that happens to differ by a number.
func TestAnalyzeSeparatesDirectAndIndirect(t *testing.T) {
	store := &fakeStore{
		info: map[Subject]SubjectInfo{
			{Kind: SubjectObject, ID: upstreamObject}: {ProjectID: openProject, Visibility: "public"},
		},
		closure: map[string]Analysis{
			upstreamObject: {
				Subject: Subject{Kind: SubjectObject, ID: upstreamObject},
				Impacts: []Impact{
					{Kind: AffectedObject, ID: readableObject, ProjectID: openProject, Hops: 1, Directness: Direct},
					{Kind: AffectedObject, ID: secondReadable, ProjectID: openProject, Hops: 2, Directness: Indirect},
				},
			},
		},
	}
	got, err := NewService(store, nil).Analyze(context.Background(), Subject{Kind: SubjectObject, ID: upstreamObject})
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}
	if len(got.Impacts) != 2 {
		t.Fatalf("impacts = %+v, want 2", got.Impacts)
	}
	first, second := got.Impacts[0], got.Impacts[1]
	if first.Directness == second.Directness {
		t.Fatalf("both impacts carry directness %q; the direct/indirect distinction must be in the data", first.Directness)
	}
	if first.Directness != Direct || first.Hops != 1 {
		t.Errorf("one hop away = %q/%d, want direct/1", first.Directness, first.Hops)
	}
	if second.Directness != Indirect || second.Hops != 2 {
		t.Errorf("two hops away = %q/%d, want indirect/2", second.Directness, second.Hops)
	}
	// The distinction is a VALUE in the payload the analysis emits, not
	// something a consumer derives: the event for each impact carries its
	// own.
	alerts, err := NewService(store, nil).BuildAlerts(context.Background(), Trigger{
		EventID: "evt", EventType: EventObjectAborted, OccurredAt: time.Now().UTC(),
		Subject: Subject{Kind: SubjectObject, ID: upstreamObject},
	}, got)
	if err != nil {
		t.Fatalf("BuildAlerts: %v", err)
	}
	if len(alerts) != 2 {
		t.Fatalf("alerts = %d, want one per impact", len(alerts))
	}
	kinds := map[string]bool{}
	for _, ev := range alerts {
		var payload map[string]any
		if err := json.Unmarshal(ev.Payload, &payload); err != nil {
			t.Fatalf("alert payload: %v", err)
		}
		directness, _ := payload[AlertFieldDirectness].(string)
		kinds[directness] = true
	}
	if !kinds[string(Direct)] || !kinds[string(Indirect)] {
		t.Errorf("alert directness values = %v, want both direct and indirect present", kinds)
	}
}

// TestDirectnessOf: the classification is defined once, and a hop count that
// names no entity classifies as the WEAKER claim.
func TestDirectnessOf(t *testing.T) {
	cases := []struct {
		hops int
		want Directness
	}{
		{1, Direct},
		{2, Indirect},
		{16, Indirect},
		{0, Indirect},
		{-3, Indirect},
	}
	for _, c := range cases {
		if got := DirectnessOf(c.hops); got != c.want {
			t.Errorf("DirectnessOf(%d) = %q, want %q", c.hops, got, c.want)
		}
	}
}

// TestAnalyzeRefusesASubjectWithNoID keeps the walk from running on nothing.
func TestAnalyzeRefusesASubjectWithNoID(t *testing.T) {
	svc := NewService(&fakeStore{}, nil)
	if _, err := svc.Analyze(context.Background(), Subject{Kind: SubjectObject}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Analyze of an empty subject = %v, want ErrValidation", err)
	}
	if _, err := svc.Analyze(context.Background(), Subject{Kind: "nonsense", ID: "x"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Analyze of an unknown subject kind = %v, want ErrValidation", err)
	}
}
