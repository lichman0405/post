package assets

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Task T0709 required test "asset page tests" (this file) and "asset browse
// tests" (browse_test.go).
//
// docs/42 §Asset Page fixes the page's content verbatim, in one line:
//
//	PID/version、type、origin、rights、creators、metadata、dependencies、
//	lineage、used/derived public links、versions、network events
//
// Eleven items. TestPageRendersEveryItemOfTheSpec walks that list — the
// table in this file is written from the sentence, block by block, not from
// the struct — and fails when one of them is not on the page, so "the page
// has roughly all of it" cannot pass for having all of it.
//
// The rest of the file is about the other half of the same requirement: what
// a page must NOT say. docs/23 §5 keeps private objects and their counts out
// of public answers, docs/12 §2 says what a public project is, and the
// disclosure T0712 closed on the preview surface (issue #238) is the same
// one reached through a page — a project's identity, its visibility, its
// asset's title, leaked one field at a time. Every test below is a negative
// one where it can be: it builds the state that WOULD leak if the rule were
// dropped and asserts the leak is absent.

// The fixtures below are deliberately the shape a real reader produces:
// several versions of one asset, a project, users, pins, refs, lineage
// edges, usages and events, with the private ones mixed in.

const (
	// pageProject is the asset's originating project; pageForeignProject is
	// another project whose private entity a usage or an edge points at.
	pageProject        = "11111111-1111-4111-8111-111111111111"
	pageForeignProject = "22222222-2222-4222-8222-222222222222"
	// pageReleaseRef IDs the release one origin ref of the fixture names.
	pageReleaseRef = "33333333-3333-4333-8333-333333333333"
	// pagePrivatePinPID is the pid of the asset a dependency pin names: a
	// private version of somebody else's asset.
	pagePrivatePinPID PID = "01j9z6k3m4n5p6q7r8s9t0v1w4"
	// pageHiddenProjectPID is the pid of the asset a second dependency pin and
	// a second lineage edge name: a PUBLIC version of an asset whose project
	// is private. The version is visible; the page it would be opened through
	// is not, because that page runs the project's read gate first — the case
	// that tells the version axis and the project axis of the link rule apart.
	pageHiddenProjectPID PID = "01j9z6k3m4n5p6q7r8s9t0v1w5"
	// pageVersions are the row ids of the fixture's three versions.
	pageV1 = "aaaaaaaa-1111-4111-8111-aaaaaaaaaaaa"
	pageV2 = "aaaaaaaa-2222-4222-8222-aaaaaaaaaaaa"
	pageV3 = "aaaaaaaa-3333-4333-8333-aaaaaaaaaaaa"
	// pageUserID is the publisher of every fixture version.
	pageUserID = "44444444-4444-4444-8444-444444444444"
)

// pageTime is the base publication instant; the fixture's versions are
// published a minute apart so the "newest visible" order is unambiguous.
var pageTime = time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)

// pageManifest builds a stored dataset manifest that the platform's own
// parser accepts: the type's required metadata (RequiredMetadata — the
// manifest rules are not this test's to restate, so the required keys are
// taken from the table) with the given extra keys, and the given pins.
func pageManifest(t *testing.T, extra Metadata, pins ...DependencyPin) json.RawMessage {
	t.Helper()
	metadata := Metadata{
		"purpose":       "screen MOF water stability",
		"data_type":     "tabular",
		"quality_notes": "curated",
		"blob_ids":      []any{"blobs/open-1"},
		"access_level":  "restricted",
	}
	for _, field := range RequiredMetadata(TypeDataset) {
		if _, ok := metadata[field.Key]; !ok {
			t.Fatalf("the fixture does not declare the required dataset key %q", field.Key)
		}
	}
	for k, v := range extra {
		metadata[k] = v
	}
	doc, err := json.Marshal(Manifest{
		Version:        ManifestFormatVersion,
		AssetType:      TypeDataset,
		Metadata:       metadata,
		DependencyPins: pins,
	})
	if err != nil {
		t.Fatalf("render manifest: %v", err)
	}
	pageManifestParses(t, doc)
	return doc
}

// pageManifestParses is the guard the fixture's other tests rely on: a
// manifest this file builds must be one the platform's parser reads, or the
// page would render empty blocks for a reason that has nothing to do with
// what the test is about.
func pageManifestParses(t *testing.T, raw json.RawMessage) {
	t.Helper()
	if _, err := ParseManifest(raw); err != nil {
		t.Fatalf("the fixture's manifest is not readable by ParseManifest: %v", err)
	}
}

// pageRights is a stored rights document, in the shape the panel renders.
func pageRights(t *testing.T) json.RawMessage {
	t.Helper()
	return json.RawMessage(`{"license":{"id":"CC-BY-4.0"},"visibility":{"metadata":"public","data_access":"restricted"}}`)
}

// pageVersion builds one stored version row.
func pageVersion(t *testing.T, id, label string, visibility Visibility, publishedAt time.Time, metadata Metadata, pins ...DependencyPin) PageVersionState {
	t.Helper()
	return PageVersionState{
		ID:            id,
		Version:       label,
		Visibility:    visibility,
		IntegrityHash: "sha256:" + strings.Repeat("a", 64),
		PublishedBy:   pageUserID,
		PublishedAt:   publishedAt,
		Manifest:      pageManifest(t, metadata, pins...),
		RightsJSON:    pageRights(t),
		OriginRefs:    []string{"project:" + pageProject, "release:" + pageReleaseRef},
	}
}

// pageState is the fixture state: one dataset in the public project
// pageProject, with three versions —
//
//	1.0  public,   published first
//	2.0  public,   published second      (the render for a non-member)
//	3.0  private,  published third       (the render for a member)
//
// — one dependency pin that resolves to a version the network can open and
// THREE that resolve to versions it cannot (another asset's private version,
// and a public version of an asset in a private project), one public and
// three hidden usages, one edge to an openable counterpart and two to
// versions that are not, and one public and one private event.
func pageState(t *testing.T, projectVisibility Visibility) PageState {
	t.Helper()
	publicPin, _ := NewDependencyPin(anotherPID, "1.0")
	privatePin, _ := NewDependencyPin(pagePrivatePinPID, "0.9")
	hiddenProjectPin, _ := NewDependencyPin(pageHiddenProjectPID, "7.1")
	selfPin, _ := NewDependencyPin(samplePID, "1.0")
	return PageState{
		Asset: PageAssetState{
			ID:              "55555555-5555-4555-8555-555555555555",
			PID:             samplePID,
			Type:            TypeDataset,
			Title:           "MOF water-stability screening set",
			Slug:            "mof-water-stability",
			OriginProjectID: pageProject,
			CreatedAt:       pageTime,
		},
		Project: PageProjectState{
			ID:         pageProject,
			Name:       "Open MOF Lab",
			Slug:       "open-lab",
			Visibility: projectVisibility,
		},
		Versions: []PageVersionState{
			pageVersion(t, pageV1, "1.0", VisibilityPublic, pageTime,
				Metadata{"instruments": []any{"XRD"}}),
			pageVersion(t, pageV2, "2.0", VisibilityPublic, pageTime.Add(time.Minute),
				Metadata{"method": "GCMC"}, publicPin, privatePin, hiddenProjectPin, selfPin),
			pageVersion(t, pageV3, "3.0", VisibilityPrivate, pageTime.Add(2*time.Minute),
				Metadata{"review_notes": "internal only"}),
		},
		Users: map[string]PageUser{
			pageUserID: {UserID: pageUserID, Handle: "privacy-alice", DisplayName: "Alice"},
		},
		Pins: map[DependencyPin]PagePinState{
			publicPin: {
				Pin: publicPin, Resolved: true,
				Visibility: VisibilityPublic, ProjectVisibility: VisibilityPublic,
				Title: "A public benchmark", Type: TypeBenchmark, PID: anotherPID, Version: "1.0",
			},
			privatePin: {
				Pin: privatePin, Resolved: true,
				Visibility: VisibilityPrivate, ProjectVisibility: VisibilityPublic,
				Title: "Secret zeolite dataset", Type: TypeDataset, PID: pagePrivatePinPID, Version: "0.9",
			},
			hiddenProjectPin: {
				Pin: hiddenProjectPin, Resolved: true,
				Visibility: VisibilityPublic, ProjectVisibility: VisibilityPrivate,
				Title: "Hidden-project dataset", Type: TypeDataset, PID: pageHiddenProjectPID, Version: "7.1",
			},
			selfPin: {
				Pin: selfPin, Resolved: true,
				Visibility: VisibilityPublic, ProjectVisibility: projectVisibility,
				Title: "MOF water-stability screening set", Type: TypeDataset, PID: samplePID, Version: "1.0",
			},
		},
		Refs: map[OriginRef]StoredRef{
			OriginRef("project:" + pageProject): {
				Ref: OriginRef("project:" + pageProject), Resolved: true,
				ProjectID: pageProject, ProjectVisibility: projectVisibility,
			},
			OriginRef("release:" + pageReleaseRef): {
				Ref: OriginRef("release:" + pageReleaseRef), Resolved: true,
				ProjectID: pageProject, ProjectVisibility: projectVisibility,
			},
		},
		Lineage: []PageLineageState{
			{
				Relation: "derived_from",
				Parent:   PageLineageEnd{VersionID: pageV1, PID: samplePID, Version: "1.0", Visibility: VisibilityPublic, ProjectVisibility: projectVisibility, Title: "MOF water-stability screening set"},
				Child:    PageLineageEnd{VersionID: pageV2, PID: samplePID, Version: "2.0", Visibility: VisibilityPublic, ProjectVisibility: projectVisibility, Title: "MOF water-stability screening set"},
			},
			{
				// The counterpart end is another asset's PRIVATE version.
				Relation: "forked_from",
				Parent:   PageLineageEnd{VersionID: pageV2, PID: samplePID, Version: "2.0", Visibility: VisibilityPublic, ProjectVisibility: projectVisibility, Title: "MOF water-stability screening set"},
				Child:    PageLineageEnd{VersionID: "aaaaaaaa-4444-4444-8444-aaaaaaaaaaaa", PID: pagePrivatePinPID, Version: "9.9", Visibility: VisibilityPrivate, ProjectVisibility: VisibilityPrivate, Title: "Secret zeolite fork"},
			},
			{
				// The counterpart end is a PUBLIC version of an asset whose
				// project is private: the version axis passes, the project
				// axis is what drops the edge.
				Relation: "supersedes",
				Parent:   PageLineageEnd{VersionID: "aaaaaaaa-5555-4555-8555-aaaaaaaaaaaa", PID: pageHiddenProjectPID, Version: "8.8", Visibility: VisibilityPublic, ProjectVisibility: VisibilityPrivate, Title: "Hidden-project fork"},
				Child:    PageLineageEnd{VersionID: pageV2, PID: samplePID, Version: "2.0", Visibility: VisibilityPublic, ProjectVisibility: projectVisibility, Title: "MOF water-stability screening set"},
			},
		},
		Usages: []PageUsageState{
			{
				AssetVersionID: pageV2, ProjectID: pageForeignProject,
				ProjectName: "Open Benchmark Suite", ProjectSlug: "open-benchmarks",
				ProjectVisibility: VisibilityPublic, VisibilityOfUsage: VisibilityPublic,
				DependencyType: "reuses", CreatedAt: pageTime,
			},
			{
				AssetVersionID: pageV2, ProjectID: "66666666-6666-4666-8666-666666666666",
				ProjectName: "Secret Zeolite Lab", ProjectSlug: "secret-lab",
				ProjectVisibility: VisibilityPrivate, VisibilityOfUsage: VisibilityPublic,
				DependencyType: "reuses", CreatedAt: pageTime.Add(time.Second),
			},
			{
				AssetVersionID: pageV2, ProjectID: "77777777-7777-4777-8777-777777777777",
				ProjectName: "Hidden Usage Lab", ProjectSlug: "hidden-usage",
				ProjectVisibility: VisibilityPublic, VisibilityOfUsage: VisibilityPrivate,
				DependencyType: "reuses", CreatedAt: pageTime.Add(2 * time.Second),
			},
		},
		Events: []PageEventState{
			{
				Type: "research_asset.version_published", Visibility: VisibilityPublic,
				ActorID: pageUserID, AssetVersion: "1.0", OccurredAt: pageTime,
			},
			{
				Type: "research_asset.version_published", Visibility: VisibilityPrivate,
				ActorID: pageUserID, AssetVersion: "3.0", OccurredAt: pageTime.Add(2 * time.Minute),
			},
			{
				// A PUBLIC event naming a PRIVATE version: what a public
				// project produces when it publishes a private version (the
				// event's visibility is the project's at publish time, the
				// version's is its own). The event axis passes on its own;
				// the version axis is what must withhold this row.
				Type: "research_asset.version_published", Visibility: VisibilityPublic,
				ActorID: pageUserID, AssetVersion: "3.0", OccurredAt: pageTime.Add(3 * time.Minute),
			},
			{
				// A public event naming a version that does not exist. No
				// caller may judge it, so no caller may be told it: the
				// fail-closed direction.
				Type: "research_asset.version_published", Visibility: VisibilityPublic,
				ActorID: pageUserID, AssetVersion: "6.6", OccurredAt: pageTime.Add(4 * time.Minute),
			},
		},
	}
}

// watchedStrings are the fixture's private entities: every string that must
// never appear in a page rendered for a non-member. Naming them in one place
// is what makes the negative assertions below mechanical rather than
// remembered.
var watchedStrings = []string{
	// the private version and its contents
	"3.0",
	"internal only",
	// the other project's private entities
	string(pagePrivatePinPID),
	"Secret zeolite dataset",
	"Secret zeolite fork",
	"Secret Zeolite Lab",
	"secret-lab",
	"Hidden Usage Lab",
	"hidden-usage",
	"0.9",
	"9.9",
	// the label of a public event that names no stored version
	"6.6",
	// the asset whose project is private, named by a pin and by a lineage edge
	string(pageHiddenProjectPID),
	"Hidden-project dataset",
	"Hidden-project fork",
	"7.1",
	"8.8",
}

// assertNoPrivateLeak marshals a rendered page and fails when any watched
// string appears in the bytes.
//
// It checks the SERIALIZED page rather than the Go fields, because the wire
// is what leaks: a field a client is not meant to read is still a
// disclosure if it is on the wire, and a struct assertion would pass on a
// page whose JSON tags published it anyway.
func assertNoPrivateLeak(t *testing.T, label string, page AssetPage) {
	t.Helper()
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatalf("%s: marshal page: %v", label, err)
	}
	body := string(raw)
	for _, secret := range watchedStrings {
		if strings.Contains(body, secret) {
			t.Errorf("%s: rendered page leaks %q: %s", label, secret, body)
		}
	}
}

// TestPageRendersEveryItemOfTheSpec is the acceptance test for the eleven
// items of docs/42 §Asset Page: it walks the spec's own list, in its own
// order, and each entry states which part of the rendered page answers it.
func TestPageRendersEveryItemOfTheSpec(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}

	items := []struct {
		item  string // the spec's own words
		match func() bool
		why   string
	}{
		{"PID", func() bool { return page.Asset.PID == samplePID }, "the asset block carries the persistent identifier"},
		{"version", func() bool {
			return page.Version.Version == "2.0" && page.Version.URL == "/assets/"+string(samplePID)+"/2.0"
		}, "the version block carries the label and its persistent URL"},
		{"type", func() bool { return page.Asset.Type == TypeDataset }, "the asset block carries the closed V1 type"},
		{"origin", func() bool { return len(page.Origin) > 0 }, "the origin block resolves the version's refs"},
		{"rights", func() bool { return len(page.Rights) > 0 }, "the rights block carries the stored rights document"},
		{"creators", func() bool { return len(page.Creators) > 0 }, "the creators block names a credited party"},
		{"metadata", func() bool { return len(page.Metadata) > 0 }, "the metadata block renders the manifest's metadata"},
		{"dependencies", func() bool { return len(page.Dependencies) > 0 }, "the dependencies block renders the manifest's pins"},
		{"lineage", func() bool { return len(page.Lineage) > 0 }, "the lineage block renders a fork/derive edge"},
		{"used/derived public links", func() bool { return len(page.UsedBy) > 0 }, "the used_by block renders a public usage"},
		{"versions", func() bool { return len(page.Versions) > 0 }, "the versions block lists the visible versions"},
		{"network events", func() bool { return len(page.Events) > 0 }, "the events block renders the research events"},
	}
	for _, entry := range items {
		if !entry.match() {
			t.Errorf("docs/42 §Asset Page item %q is missing from the rendered page (%s)", entry.item, entry.why)
		}
	}
}

// TestPageVersionAndPIDAreNotConfused pins the two halves of "PID/version"
// apart: the pid is the asset's permanent identity and the label is the
// version's, they are different fields, and the version URL carries both.
func TestPageVersionAndPIDAreNotConfused(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if page.Asset.PID != samplePID {
		t.Errorf("asset pid = %q, want %q", page.Asset.PID, samplePID)
	}
	if page.Version.Version == string(page.Asset.PID) {
		t.Fatalf("the version block carries the pid (%q) instead of a version label", page.Version.Version)
	}
	if want := "/assets/" + string(samplePID) + "/" + page.Version.Version; page.Version.URL != want {
		t.Errorf("version url = %q, want %q (pid first, then the label)", page.Version.URL, want)
	}
	// Every version of the list carries its own URL, so two versions of one
	// asset are distinguishable — and none of them is the asset URL.
	seen := map[string]bool{}
	for _, v := range page.Versions {
		if seen[v.URL] {
			t.Errorf("two version rows share the URL %q", v.URL)
		}
		seen[v.URL] = true
		if v.URL == AssetURL(page.Asset.PID) {
			t.Errorf("version %q renders the ASSET url, not the version url", v.Version)
		}
		if !strings.HasSuffix(v.URL, "/"+v.Version) {
			t.Errorf("version url %q does not end in its label %q", v.URL, v.Version)
		}
	}
}

// TestPageRendersTheIntegrityHashOfTheRenderedVersion: the hash is part of
// the rendered version, it is the stored value, and two versions of one asset
// carry their own — the item exists so a reader can check that what it was
// served is what was published, which only works if the hash follows the
// version.
func TestPageRendersTheIntegrityHashOfTheRenderedVersion(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	// Give the two public versions different hashes: the fixture's are equal,
	// and a page that rendered "the asset's hash" would pass either way.
	state.Versions[0].IntegrityHash = "sha256:" + strings.Repeat("1", 64)
	state.Versions[1].IntegrityHash = "sha256:" + strings.Repeat("2", 64)

	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if want := state.Versions[1].IntegrityHash; page.Version.IntegrityHash != want {
		t.Errorf("rendered hash = %q, want the 2.0 row's %q", page.Version.IntegrityHash, want)
	}
	byLabel := map[string]string{}
	for _, v := range page.Versions {
		byLabel[v.Version] = v.IntegrityHash
	}
	if byLabel["1.0"] == byLabel["2.0"] {
		t.Errorf("both versions render the same hash (%q): the versions list must distinguish them", byLabel["1.0"])
	}
	if want := state.Versions[0].IntegrityHash; byLabel["1.0"] != want {
		t.Errorf("1.0 hash = %q, want %q", byLabel["1.0"], want)
	}
}

// TestPageAnonymousSeesPublicVersionOnly: a caller who is not a member of the
// asset's project renders the newest PUBLIC version, and the private one is
// not on the page at all — not as a row, not as a count, not as a string.
func TestPageAnonymousSeesPublicVersionOnly(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if page.Version.Version != "2.0" {
		t.Errorf("anonymous renders version %q, want the newest public one 2.0", page.Version.Version)
	}
	if page.Version.Visibility != VisibilityPublic {
		t.Errorf("rendered visibility = %q, want public", page.Version.Visibility)
	}
	if len(page.Versions) != 2 {
		t.Errorf("anonymous sees %d versions, want the 2 public ones", len(page.Versions))
	}
	for _, v := range page.Versions {
		if v.Visibility != VisibilityPublic {
			t.Errorf("anonymous versions list contains a %q version: %+v", v.Visibility, v)
		}
	}
	assertNoPrivateLeak(t, "anonymous", page)
}

// TestPageMemberSeesPrivateVersions: a member of the asset's own project
// renders the newest version including the private ones. The page does not
// hide a project's own work from that project — and it says which version is
// private rather than pretending the list is complete.
func TestPageMemberSeesPrivateVersions(t *testing.T) {
	state := pageState(t, VisibilityPrivate)
	page, ok := BuildPage(state, PageViewer{Member: true}, "")
	if !ok {
		t.Fatal("BuildPage refused a member reading their own project's asset")
	}
	if page.Version.Version != "3.0" {
		t.Errorf("member renders version %q, want the newest 3.0", page.Version.Version)
	}
	if page.Version.Visibility != VisibilityPrivate {
		t.Errorf("rendered visibility = %q, want private", page.Version.Visibility)
	}
	if len(page.Versions) != 3 {
		t.Errorf("member sees %d versions, want all 3", len(page.Versions))
	}
	// The private project may be named: for this caller, and only this one.
	if page.Asset.OriginProject == nil || page.Asset.OriginProject.ID != pageProject {
		t.Errorf("member's page does not name their own project: %+v", page.Asset.OriginProject)
	}
	// The same membership is what the origin block needs: the refs point
	// into that project, so the member — and only the member — gets them,
	// still resolved and still carrying their identity.
	if len(page.Origin) != 2 {
		t.Fatalf("member's origin = %+v, want both refs of the version", page.Origin)
	}
	for _, o := range page.Origin {
		if o.ProjectID != pageProject || o.Link == "" {
			t.Errorf("member's origin ref %q = %+v, want the project named and linked", o.Ref, o)
		}
	}
}

// TestPageWithholdsAPrivateProjectFromNonMembers is the identity rule on the
// page: a private project's id, name, slug and visibility are withheld from a
// non-member even though the asset itself is public.
func TestPageWithholdsAPrivateProjectFromNonMembers(t *testing.T) {
	state := pageState(t, VisibilityPrivate)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused a public version of an asset in a private project")
	}
	if page.Asset.OriginProject != nil {
		t.Errorf("a non-member's page names the private project: %+v", page.Asset.OriginProject)
	}
	raw, _ := json.Marshal(page)
	for _, secret := range []string{pageProject, "Open MOF Lab", "open-lab", `"visibility":"private"`} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("page leaks the private project's %q: %s", secret, raw)
		}
	}
	// The version's origin refs point INTO that project, and a ref's own
	// bytes name the entity it references ("project:<uuid>",
	// "release:<uuid>") — so they are dropped whole rather than rendered
	// with their identity fields blanked. The block is empty, which states
	// an absence; a list of blanked refs would state a count.
	if len(page.Origin) != 0 {
		t.Errorf("origin = %+v, want the block empty: every ref of this version names the private project", page.Origin)
	}
}

// TestPageNeverRendersACountOfHiddenEntities is docs/23 §5's counting rule:
// the page renders no number that would let a reader infer how many private
// things there are. The fixture has exactly one hidden usage, one hidden
// lineage edge and one hidden event, and the page's own lists carry the
// public ones only — so the sizes themselves are the leak, and the assertion
// is on the sizes.
func TestPageNeverRendersACountOfHiddenEntities(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if len(page.UsedBy) != 1 {
		t.Errorf("used_by has %d entries, want only the public usage (the fixture also has a private-project usage and a private-usage row)", len(page.UsedBy))
	}
	if len(page.Events) != 1 {
		t.Errorf("events has %d entries, want only the public event", len(page.Events))
	}
	// Both lineage edges touch version 2.0: the derived_from edge to the
	// public 1.0 is rendered, the forked_from edge to another asset's
	// private 9.9 is not.
	for _, edge := range page.Lineage {
		if edge.Version != "1.0" {
			t.Errorf("lineage renders an edge to %q, want only the public counterpart 1.0", edge.Version)
		}
	}
	if len(page.Lineage) != 1 {
		t.Errorf("lineage has %d entries, want 1 (the public edge only)", len(page.Lineage))
	}
	assertNoPrivateLeak(t, "anonymous", page)
}

// TestPageUsageWithoutAPublicProjectIsHidden: a usage row may declare itself
// public while the USING project is private. The row's declaration is about
// the usage, not about the project's existence (docs/12 §2 keeps a private
// project's identity out of a public answer), so the entry is withheld and
// not counted.
func TestPageUsageWithoutAPublicProjectIsHidden(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if len(page.UsedBy) != 1 {
		t.Fatalf("used_by = %+v, want exactly the one public-project usage", page.UsedBy)
	}
	got := page.UsedBy[0]
	if got.ProjectID != pageForeignProject || got.ProjectName != "Open Benchmark Suite" {
		t.Errorf("used_by entry = %+v, want the public project %q", got, "Open Benchmark Suite")
	}
	// The usage the page DOES render carries the public project's identity
	// — that is what it is for.
	raw, _ := json.Marshal(page)
	if !strings.Contains(string(raw), "Open Benchmark Suite") {
		t.Errorf("the public usage's project name is missing from the page: %s", raw)
	}
}

// TestPageDependencyPinsToVersionsTheNetworkCannotOpenAreDropped: a pin's
// own bytes are "<pid>@<version>", so a pin the network cannot open names an
// asset and a version nobody may read. Both axes are checked here — the
// private version, and the public version of an asset in a private project
// (whose page answers 404 through the project gate) — and the entry is
// DROPPED rather than rendered blank: a blanked entry would be the count
// docs/23 §5 forbids.
func TestPageDependencyPinsToVersionsTheNetworkCannotOpenAreDropped(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	byPin := map[string]PageDependency{}
	for _, d := range page.Dependencies {
		byPin[d.Pin] = d
	}
	for _, hidden := range []string{
		string(pagePrivatePinPID) + "@0.9",    // private version
		string(pageHiddenProjectPID) + "@7.1", // public version, private project
	} {
		if entry, present := byPin[hidden]; present {
			t.Errorf("the page renders the pin %q, which names a version the network cannot open: %+v", hidden, entry)
		}
	}
	// What IS rendered is what the reader can go and read, with its identity
	// and its URL.
	publicPin := string(anotherPID) + "@1.0"
	if got := byPin[publicPin]; got.Title != "A public benchmark" || got.Type != TypeBenchmark ||
		!got.Public || !got.Resolved || got.URL != AssetVersionURL(anotherPID, "1.0") {
		t.Errorf("public pin = %+v, want the pinned asset's title, type and version URL", got)
	}
	// The manifest declares four pins; two name versions the network can
	// open (the benchmark, and this asset's own 1.0 — the same public project
	// the reader is already reading). The block is a list of names, not a
	// list with holes in it, so its SIZE is asserted, not only its content:
	// three entries would mean one hidden pin is still being counted.
	if len(page.Dependencies) != 2 {
		t.Errorf("dependencies = %+v, want exactly the 2 openable pins", page.Dependencies)
	}
	assertNoPrivateLeak(t, "anonymous", page)
}

// TestPageLineageToAVersionTheNetworkCannotOpenIsOmitted: the same rule on
// the lineage block, and the same two axes — a counterpart version that is
// private, and a counterpart that is public inside a private project. The
// edge is omitted entirely and the block's size is asserted, because an edge
// that is "there but hidden" is the count.
func TestPageLineageToAVersionTheNetworkCannotOpenIsOmitted(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	for _, edge := range page.Lineage {
		if edge.PID != samplePID {
			t.Errorf("lineage renders an edge to %q, which names a version the network cannot open: %+v", edge.PID, edge)
		}
	}
	if len(page.Lineage) != 1 {
		t.Errorf("lineage = %+v, want only the edge to this asset's public 1.0", page.Lineage)
	}
	if page.Lineage[0].Version != "1.0" || page.Lineage[0].Relation != "derived_from" {
		t.Errorf("lineage entry = %+v, want the derived_from edge to 1.0", page.Lineage[0])
	}
	assertNoPrivateLeak(t, "anonymous", page)
}

// TestPageAnonymouslyRendersNothingWhenEveryVersionIsPrivate: the page
// answers ok=false, which the transport turns into the same 404 an unknown
// pid gets. A caller who may not see anything must not be able to tell
// "this asset exists and is entirely private" from "no such asset".
func TestPageAnonymouslyRendersNothingWhenEveryVersionIsPrivate(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	for i := range state.Versions {
		state.Versions[i].Visibility = VisibilityPrivate
	}
	if _, ok := BuildPage(state, PageViewer{}, ""); ok {
		t.Error("BuildPage rendered a page for an asset whose every version is private")
	}
	// The same state renders for a member: the refusal is about the viewer,
	// not about the state.
	if _, ok := BuildPage(state, PageViewer{Member: true}, ""); !ok {
		t.Error("BuildPage refused a member of the asset's own project")
	}
}

// TestPageVersionRequestIsHonouredOrRefused covers the second half of the
// version rule: asking for one version renders THAT version, and asking for
// one the viewer may not see is refused exactly as an asset with nothing
// visible — never "that version exists but is private".
func TestPageVersionRequestIsHonouredOrRefused(t *testing.T) {
	state := pageState(t, VisibilityPublic)

	page, ok := BuildPage(state, PageViewer{}, "1.0")
	if !ok {
		t.Fatal("BuildPage refused the public version 1.0")
	}
	if page.Version.Version != "1.0" {
		t.Errorf("requested 1.0, rendered %q", page.Version.Version)
	}
	for _, v := range page.Versions {
		if v.Current != (v.Version == "1.0") {
			t.Errorf("versions list marks %q current=%v, want only 1.0 current", v.Version, v.Current)
		}
	}

	if _, ok := BuildPage(state, PageViewer{}, "3.0"); ok {
		t.Error("BuildPage rendered the private version 3.0 for a non-member")
	}
	if _, ok := BuildPage(state, PageViewer{}, "99.0"); ok {
		t.Error("BuildPage rendered a version that does not exist")
	}
	if _, ok := BuildPage(state, PageViewer{Member: true}, "3.0"); !ok {
		t.Error("BuildPage refused a member's request for their own private version")
	}
}

// TestPageIsDeterministic: the same state and viewer render identical bytes.
// The page is cacheable public data and a citation target; a page whose
// blocks were ordered by map iteration would differ between two reads of
// one state.
func TestPageIsDeterministic(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	first, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	second, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("two renders of one state differ:\n%s\n%s", a, b)
	}
	// The metadata block is ordered by key — the stored canonical order —
	// rather than by Go's map order.
	var prev string
	for _, m := range first.Metadata {
		if prev != "" && m.Key < prev {
			t.Errorf("metadata %q follows %q: the block is not ordered by key", m.Key, prev)
		}
		prev = m.Key
	}
}

// TestPageRendersNoBlobURL is the page half of docs/17 §5 (every download
// checks project/object/blob access) and of the task's own rule that a
// restricted blob is not downloadable. The page renders no blob URL at all:
// the platform has no blob download route, so a URL here would be a link
// nobody serves, and the manifest's blob ids stay where the publisher
// declared them — inside metadata.
func TestPageRendersNoBlobURL(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	raw, _ := json.Marshal(page)
	body := string(raw)
	for _, forbidden := range []string{"/blobs/", "/download", "/raw", "blob_url", "blob_urls", "download_url"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the page contains %q, but no blob download route exists: %s", forbidden, body)
		}
	}
	// The declared blob id IS on the page, as metadata — which is where the
	// manifest declares it, and what docs/17 §3 means by "public metadata".
	if !strings.Contains(body, "blobs/open-1") {
		t.Errorf("the manifest's declared blob id is missing from the metadata block: %s", body)
	}
}

// TestPageCreatorNamesItsRole: the creators block names what the credit is.
// The platform stores no creator list, so the party it can answer for is the
// publisher — and rendering that under a "creator" heading without saying so
// would credit the wrong party.
func TestPageCreatorNamesItsRole(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if len(page.Creators) != 1 {
		t.Fatalf("creators = %+v, want the one publisher", page.Creators)
	}
	got := page.Creators[0]
	if got.Role != CreatorRolePublisher {
		t.Errorf("creator role = %q, want %q", got.Role, CreatorRolePublisher)
	}
	if got.Handle != "privacy-alice" {
		t.Errorf("creator handle = %q, want the resolved user", got.Handle)
	}
}

// TestPageUnresolvableUserRendersNoIdentity: a version whose publisher could
// not be resolved renders no creators entry — an id is not an identity, and
// printing one would be a half-answer a client would render as a name.
func TestPageUnresolvableUserRendersNoIdentity(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	state.Users = map[string]PageUser{}
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if len(page.Creators) != 0 {
		t.Errorf("creators = %+v, want none when the user row was not resolved", page.Creators)
	}
	if page.Version.PublishedBy != nil {
		t.Errorf("published_by = %+v, want nil when the user row was not resolved", page.Version.PublishedBy)
	}
	raw, _ := json.Marshal(page)
	if strings.Contains(string(raw), pageUserID) {
		t.Errorf("the page leaks the unresolved user id: %s", raw)
	}
}

// TestPageEventsRespectTheirOwnVisibility: an event's stored visibility is
// the publishing project's at publish time, and the page obeys the event's
// own answer rather than the asset's.
func TestPageEventsRespectTheirOwnVisibility(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	for _, e := range page.Events {
		if e.Version == "3.0" {
			t.Errorf("a non-member's page renders the private event: %+v", e)
		}
	}
	member, ok := BuildPage(pageState(t, VisibilityPublic), PageViewer{Member: true}, "")
	if !ok {
		t.Fatal("BuildPage refused a member")
	}
	if len(member.Events) != 3 {
		t.Errorf("member sees %d events, want the three the version guards admit (the unresolvable label is withheld from everybody)", len(member.Events))
	}
}

// TestPageEventsNameNoVersionTheVersionsBlockWithholds is the guard the
// events block needs in addition to the event's own visibility: the fixture
// holds a PUBLIC event naming the PRIVATE version 3.0 — what a public
// project produces when it publishes a private version, the event's
// visibility being the project's at publish time — and a public event
// naming a label no version row carries.
//
// The rule the test pins is one a page cannot be read as violating: every
// version an event names is one the versions block renders. Rendering the
// event instead would put the private version's label, publish instant and
// publisher on the network's page while the same page answers the
// indistinguishable 404 for that version's own address.
func TestPageEventsNameNoVersionTheVersionsBlockWithholds(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}

	// The invariant, stated as the invariant: the two blocks are read from
	// one answer, so the labels they name must be a subset of each other.
	if !assertEventsNameRenderedVersions(t, "a non-member's page", page) {
		t.FailNow()
	}
	for _, e := range page.Events {
		if e.Version == "3.0" || e.Version == "6.6" {
			t.Errorf("the events block renders %+v: the version it names is not one this caller may see", e)
		}
	}
	// The private version is still withheld by the versions block, and the
	// event that would have named it is gone: neither block names it, which
	// is the whole point (one block must not contradict the other).
	if len(page.Versions) != 2 {
		t.Errorf("the versions block = %+v, want the two public versions", page.Versions)
	}
	assertNoPrivateLeak(t, "the non-member's page with a public event about a private version", page)

	// A member may see the private version, so the public event naming it is
	// theirs to read — the guard narrows nothing a member could already see.
	member, ok := BuildPage(pageState(t, VisibilityPublic), PageViewer{Member: true}, "")
	if !ok {
		t.Fatal("BuildPage refused a member")
	}
	if !assertEventsNameRenderedVersions(t, "a member's page", member) {
		t.FailNow()
	}
	named := map[string]bool{}
	for _, e := range member.Events {
		named[e.Version] = true
	}
	if !named["3.0"] {
		t.Errorf("the member's events = %+v, want the event about the private 3.0 they may see", member.Events)
	}
	// The unresolvable label is withheld from the member too: it names no
	// version at all, so no caller's view can be right about it.
	if named["6.6"] {
		t.Errorf("the member's events = %+v, want the unresolvable label withheld from every viewer", member.Events)
	}
}

// assertEventsNameRenderedVersions fails when the events block names a
// version the versions block does not render, and reports whether it held.
// It reads the RENDERED page rather than the state, because the claim is
// about what two blocks of one page tell one reader.
func assertEventsNameRenderedVersions(t *testing.T, label string, page AssetPage) bool {
	t.Helper()
	rendered := map[string]bool{}
	for _, v := range page.Versions {
		rendered[v.Version] = true
	}
	ok := true
	for _, e := range page.Events {
		if e.Version == "" {
			continue
		}
		if !rendered[e.Version] {
			t.Errorf("%s: the events block names version %q, which the versions block does not render: %+v",
				label, e.Version, e)
			ok = false
		}
	}
	return ok
}

// TestPageRightsIsTheStoredDocument: the rights block is the stored bytes,
// echoed rather than re-encoded, so what the panel renders is what the
// publisher published.
func TestPageRightsIsTheStoredDocument(t *testing.T) {
	state := pageState(t, VisibilityPublic)
	page, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	var doc map[string]any
	if err := json.Unmarshal(page.Rights, &doc); err != nil {
		t.Fatalf("rights block is not the stored document: %v", err)
	}
	if _, ok := doc["license"]; !ok {
		t.Errorf("rights block = %s, want the stored document with its license", page.Rights)
	}
	// An unresolved rights document renders as null rather than as an empty
	// object: "the reader did not answer" is not "there is no declaration".
	state.Versions[1].RightsJSON = nil
	empty, ok := BuildPage(state, PageViewer{}, "")
	if !ok {
		t.Fatal("BuildPage refused an asset with a public version")
	}
	if len(empty.Rights) != 0 {
		t.Errorf("rights = %s, want null when the document was not read", empty.Rights)
	}
}
