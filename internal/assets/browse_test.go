package assets

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// Task T0709 required test "asset browse tests" (page_test.go holds the
// page's).
//
// The browse read is docs/11 §2's hub list, and it makes two claims a test
// has to hold it to:
//
//   - it is an index of the TYPES the platform has — the closed V1 set of
//     four (Type, AllTypes, research_assets.asset_type's CHECK) — and its
//     filter answers for exactly that set;
//   - it is a PUBLIC index: it lists assets with something public behind
//     them, it names a project only when that project is public, and it
//     renders no number that would let a reader count what it does not
//     list (docs/23 §5).
//
// Both halves are asserted against the SERIALIZED list where the leak
// would be (assertBrowseNoLeak), not only against the Go fields: a field a
// client is not meant to read is still a disclosure if it is on the wire.

// browseRow builds one reader row, with a public project unless the caller
// passes another visibility.
func browseRow(pid PID, t Type, title string, visibility Visibility, publishedAt time.Time, publicVersions int) BrowseRowState {
	return BrowseRowState{
		PID:                     pid,
		Type:                    t,
		Title:                   title,
		Slug:                    strings.ToLower(strings.ReplaceAll(title, " ", "-")),
		OriginProjectID:         pageProject,
		OriginProjectName:       "Open MOF Lab",
		OriginProjectSlug:       "open-lab",
		OriginProjectVisibility: visibility,
		PublicVersions:          publicVersions,
		LatestVersion:           "1.0",
		LatestPublishedAt:       publishedAt,
	}
}

// browseRows is the fixture: one asset of each of the four V1 types, so a
// filter that dropped one or answered for a type that does not exist is
// visible, with the publication instants a minute apart and the pids
// deliberately NOT in publication order (the list's order is the reader's
// state, not the row order).
func browseRows() []BrowseRowState {
	return []BrowseRowState{
		browseRow(samplePID, TypeDataset, "MOF water-stability screening set", VisibilityPublic, pageTime, 2),
		browseRow(anotherPID, TypeBenchmark, "A public benchmark", VisibilityPublic, pageTime.Add(2*time.Minute), 1),
		browseRow("01j9z6k3m4n5p6q7r8s9t0v1w6", TypeProtocol, "Synthesis protocol", VisibilityPublic, pageTime.Add(time.Minute), 3),
		browseRow("01j9z6k3m4n5p6q7r8s9t0v1w7", TypeMaterialCollection, "Zeolite collection", VisibilityPublic, pageTime.Add(3*time.Minute), 1),
	}
}

// browsePrivateRow is one row with a PRIVATE project behind it, carrying
// the identity watchedStrings already lists as private ("Hidden Usage Lab"
// / "hidden-usage", and pageForeignProject's uuid) so one list of secrets
// covers the page's surface and this one's.
func browsePrivateRow(pid PID, t Type, title string, publishedAt time.Time, publicVersions int) BrowseRowState {
	row := browseRow(pid, t, title, VisibilityPrivate, publishedAt, publicVersions)
	row.OriginProjectID = pageForeignProject
	row.OriginProjectName = "Hidden Usage Lab"
	row.OriginProjectSlug = "hidden-usage"
	return row
}

// assertBrowseNoLeak marshals a rendered list and fails when any watched
// string appears in the bytes.
//
// It searches for the PRIVATE project's identity and not for the public
// one's: the public project is rendered by design and appears in the same
// list, so asserting its absence would assert the opposite of the rule.
func assertBrowseNoLeak(t *testing.T, label string, list BrowseList) {
	t.Helper()
	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("%s: marshal list: %v", label, err)
	}
	body := string(raw)
	for _, secret := range append([]string{pageForeignProject, `"visibility":"private"`}, watchedStrings...) {
		if strings.Contains(body, secret) {
			t.Errorf("%s: rendered list leaks %q: %s", label, secret, body)
		}
	}
}

// TestBrowseOffersTheClosedTypeSet: the list carries the four V1 types —
// the same set the CHECK constraint admits and Type parses — so a client's
// filter control is built from the platform's set rather than a copy of
// it, and every offered type is one the filter accepts.
func TestBrowseOffersTheClosedTypeSet(t *testing.T) {
	list := BuildBrowse(nil, nil)
	want := []Type{TypeDataset, TypeProtocol, TypeMaterialCollection, TypeBenchmark}
	if len(AllTypes()) != len(want) {
		t.Fatalf("AllTypes() = %v, want the four V1 types %v", AllTypes(), want)
	}
	if len(list.Types) != len(want) {
		t.Fatalf("list.Types = %v, want the four V1 types", list.Types)
	}
	for i, ty := range want {
		if list.Types[i] != ty {
			t.Errorf("list.Types[%d] = %q, want %q", i, list.Types[i], ty)
		}
		// Every offered type is one the filter accepts: a control built from
		// this list cannot ask for something the transport refuses.
		got, ok := ParseBrowseFilter(string(ty))
		if !ok || got == nil || *got != ty {
			t.Errorf("ParseBrowseFilter(%q) = %v, %v, want the type itself", ty, got, ok)
		}
	}
}

// TestBrowseFiltersByType: each of the four types selects its own rows and
// nothing else. The filter is applied in the MODEL as well as in the query
// (BuildBrowse), so this test builds one unfiltered row set and asks for
// each type in turn — the state a store that ignored the filter would hand
// over.
func TestBrowseFiltersByType(t *testing.T) {
	rows := browseRows()
	for _, want := range AllTypes() {
		filter := want
		list := BuildBrowse(rows, &filter)
		if list.Type == nil || *list.Type != want {
			t.Fatalf("list.Type = %v, want the filter that produced it (%q)", list.Type, want)
		}
		if len(list.Assets) != 1 {
			t.Fatalf("filter %q selected %d assets, want 1: %+v", want, len(list.Assets), list.Assets)
		}
		if list.Assets[0].Type != want {
			t.Errorf("filter %q selected a %q asset", want, list.Assets[0].Type)
		}
	}
	// Unfiltered lists all four, and the filter is not silently invented.
	all := BuildBrowse(rows, nil)
	if all.Type != nil {
		t.Errorf("list.Type = %v, want nil for an unfiltered list", all.Type)
	}
	if len(all.Assets) != 4 {
		t.Errorf("unfiltered list has %d assets, want all 4: %+v", len(all.Assets), all.Assets)
	}
}

// TestParseBrowseFilterRefusesWhatItCannotAnswer: an absent filter is not a
// filter, the four V1 types are accepted, and anything else is reported
// false — never silently ignored, because an ignored filter would return an
// empty list, and "there are no such assets" is a claim this platform
// cannot make about a type it does not have.
func TestParseBrowseFilterRefusesWhatItCannotAnswer(t *testing.T) {
	if filter, ok := ParseBrowseFilter(""); !ok || filter != nil {
		t.Errorf("ParseBrowseFilter(\"\") = %v, %v, want nil, true", filter, ok)
	}
	for _, bad := range []string{"Dataset", "DATASET", "model", "publication", "datasets", "bench", "benchmark_data", "material-collection"} {
		if filter, ok := ParseBrowseFilter(bad); ok || filter != nil {
			t.Errorf("ParseBrowseFilter(%q) = %v, %v, want nil, false", bad, filter, ok)
		}
	}
	// Surrounding whitespace is NOT a refusal: the platform's own type
	// parser trims it (internal/assets.ParseType, which the publish gate
	// also reads), and the browse filter inheriting that is one answer to
	// "what is this type called", not two.
	if filter, ok := ParseBrowseFilter(" benchmark "); !ok || filter == nil || *filter != TypeBenchmark {
		t.Errorf("ParseBrowseFilter(\" benchmark \") = %v, %v, want the benchmark type", filter, ok)
	}
}

// TestBrowseNamesAProjectOnlyWhenItIsPublic: a private project may publish
// an asset (docs/12 §2), and the asset page may name that project to its
// own members — this list may name it to nobody, because naming it per
// reader would mean one authorization question per row about one private
// project per row. The identity is withheld, and the row carries nothing
// else about the project either.
func TestBrowseNamesAProjectOnlyWhenItIsPublic(t *testing.T) {
	rows := []BrowseRowState{
		browseRow(samplePID, TypeDataset, "A public project's dataset", VisibilityPublic, pageTime, 1),
		browsePrivateRow(anotherPID, TypeDataset, "A private project's dataset", pageTime.Add(time.Minute), 1),
	}
	list := BuildBrowse(rows, nil)
	if len(list.Assets) != 2 {
		t.Fatalf("list has %d assets, want both: %+v", len(list.Assets), list.Assets)
	}
	byTitle := map[string]BrowseItem{}
	for _, item := range list.Assets {
		byTitle[item.Title] = item
	}
	public := byTitle["A public project's dataset"]
	if public.OriginProject == nil || public.OriginProject.Name != "Open MOF Lab" {
		t.Errorf("public project's row = %+v, want the project named", public.OriginProject)
	}
	private := byTitle["A private project's dataset"]
	if private.OriginProject != nil {
		t.Errorf("private project's row names the project: %+v", private.OriginProject)
	}
	// The asset itself is listed with what is public about it: its own
	// identity, its type and its version URL. Only the project is withheld.
	if private.PID != anotherPID || private.Type != TypeDataset || private.LatestURL != AssetVersionURL(anotherPID, "1.0") {
		t.Errorf("private project's row = %+v, want the asset's own public facts", private)
	}
	assertBrowseNoLeak(t, "anonymous", list)
}

// TestBrowseNeverRendersATotalItCannotSee is docs/23 §5's counting rule on
// the index: the only number a row carries is the count of PUBLIC versions
// — the versions a reader can go and open one by one — and the item's own
// wire shape is asserted, so a total (or a "hidden" count) cannot be added
// later without this test failing.
func TestBrowseNeverRendersATotalItCannotSee(t *testing.T) {
	rows := []BrowseRowState{browseRow(samplePID, TypeDataset, "MOF water-stability screening set", VisibilityPublic, pageTime, 2)}
	list := BuildBrowse(rows, nil)
	raw, err := json.Marshal(list.Assets[0])
	if err != nil {
		t.Fatalf("marshal item: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("unmarshal item: %v", err)
	}
	got := make([]string, 0, len(fields))
	for k := range fields {
		got = append(got, k)
	}
	sort.Strings(got)
	want := []string{
		"latest_published_at", "latest_url", "latest_version",
		"origin_project", "pid", "public_versions", "slug", "title", "type", "url",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("browse item fields = %v, want exactly %v (no total, no hidden count)", got, want)
	}
	if list.Assets[0].PublicVersions != 2 {
		t.Errorf("public_versions = %d, want the 2 public ones", list.Assets[0].PublicVersions)
	}
	// A row that claims no public version is dropped rather than rendered as
	// an asset with nothing behind it: the list is the index of what the
	// network can open.
	empty := BuildBrowse([]BrowseRowState{browseRow(anotherPID, TypeDataset, "All private", VisibilityPublic, pageTime, 0)}, nil)
	if len(empty.Assets) != 0 {
		t.Errorf("list = %+v, want no row for an asset with no public version", empty.Assets)
	}
}

// TestBrowseIsOrderedNewestFirstAndDeterministic: the hub's order is the
// network's publication order, and two reads of one state render identical
// bytes — the list is a public index, so it cannot depend on the order a
// store happened to return rows in.
func TestBrowseIsOrderedNewestFirstAndDeterministic(t *testing.T) {
	rows := browseRows()
	first := BuildBrowse(rows, nil)

	// The same rows in a different order render the same list.
	shuffled := make([]BrowseRowState, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		shuffled = append(shuffled, rows[i])
	}
	second := BuildBrowse(shuffled, nil)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("two renders of one state differ:\n%s\n%s", a, b)
	}

	// Newest first, and no asset appears twice.
	seen := map[PID]bool{}
	var prev time.Time
	for i, item := range first.Assets {
		if seen[item.PID] {
			t.Fatalf("asset %q is listed twice", item.PID)
		}
		seen[item.PID] = true
		if i > 0 && item.LatestPublishedAt.After(prev) {
			t.Errorf("asset %q (%s) follows a newer one (%s): the list is not newest first",
				item.PID, item.LatestPublishedAt, prev)
		}
		prev = item.LatestPublishedAt
	}

	// Ties are broken by pid, so the order is total rather than merely
	// sorted: two assets published in one transaction still have an order
	// both reads agree on.
	tied := []BrowseRowState{
		browseRow("01j9z6k3m4n5p6q7r8s9t0v1w9", TypeDataset, "Tied B", VisibilityPublic, pageTime, 1),
		browseRow("01j9z6k3m4n5p6q7r8s9t0v1w8", TypeDataset, "Tied A", VisibilityPublic, pageTime, 1),
	}
	ordered := BuildBrowse(tied, nil)
	if len(ordered.Assets) != 2 || ordered.Assets[0].Title != "Tied A" {
		t.Errorf("ties render as %+v, want the lower pid first", ordered.Assets)
	}
}

// TestBrowseRowsCarryThePersistentURLs: every row's URL is the asset's
// persistent address and its latest_url is that version's — two different
// strings, so a reader cannot land on "the asset" thinking it asked for a
// version, and the pair is what makes the newest version reachable in one
// click from the hub.
func TestBrowseRowsCarryThePersistentURLs(t *testing.T) {
	rows := browseRows()
	list := BuildBrowse(rows, nil)
	for _, item := range list.Assets {
		if item.URL != "/assets/"+string(item.PID) {
			t.Errorf("item %q url = %q, want the asset's persistent URL", item.PID, item.URL)
		}
		if item.LatestURL != "/assets/"+string(item.PID)+"/"+item.LatestVersion {
			t.Errorf("item %q latest_url = %q, want the version's persistent URL", item.PID, item.LatestURL)
		}
		if item.URL == item.LatestURL {
			t.Errorf("item %q renders one URL for the asset and the version", item.PID)
		}
	}
}
