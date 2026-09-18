package assets

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Task T0707: the project-side read of asset_dependencies — "which fixed
// asset versions does this project depend on, and how" (docs/42 §Project
// Assets). BuildProjectDependencies decides what may be rendered; the store
// hands it every row, private ones included, so this is where the two
// disclosure rules are actually enforced and therefore where they are
// tested.
//
// The rules (see project_dependency.go):
//
//  1. the used version must be one the network can open — public, in a
//     public project (mayLinkVersion) — for EVERY viewer, members included,
//     because the entity it protects belongs to another project;
//  2. a non-member sees only rows whose visibility_of_usage is public.
//
// Both fail closed and neither counts anything: a dropped row leaves no
// entry, no placeholder and no number (docs/23 §5, issue #238 class).

const (
	// depUsedPID / depOtherPID are the pids of the used versions.
	depUsedPID  PID = "01j9z6k3m4n5p6q7r8s9t0v1w7"
	depOtherPID PID = "01j9z6k3m4n5p6q7r8s9t0v1w8"
)

// depTime is the fixture's recording instant, deliberately NOT in UTC: the
// model promises the rendered instant in UTC, and a fixture in UTC could not
// tell that promise from a no-op.
var depTime = time.Date(2026, 9, 15, 20, 30, 0, 0, time.FixedZone("CST", 8*3600))

// depRow builds one stored row as the reader hands it over: the pin already
// resolved to the canonical form, both visibility axes present.
func depRow(t *testing.T, pin string, versionVisibility, projectVisibility Visibility, dependencyType string, usageVisibility Visibility, title string) ProjectDependencyState {
	t.Helper()
	if !DependencyPin(pin).Valid() {
		t.Fatalf("the fixture names %q, which is not a canonical pin", pin)
	}
	return ProjectDependencyState{
		Pin:                      pin,
		Title:                    title,
		Type:                     TypeDataset,
		VersionVisibility:        versionVisibility,
		VersionProjectVisibility: projectVisibility,
		DependencyType:           dependencyType,
		VisibilityOfUsage:        usageVisibility,
		CreatedAt:                depTime,
	}
}

// depPin builds a canonical pin for the fixture.
func depPin(t *testing.T, pid PID, version string) string {
	t.Helper()
	pin, ok := NewDependencyPin(pid, version)
	if !ok {
		t.Fatalf("NewDependencyPin(%q, %q) refused a valid pair", pid, version)
	}
	return string(pin)
}

// depState is the fixture: four declarations of one project —
//
//	depUsedPID@1.0   public version, public project, public usage
//	depUsedPID@2.0   public version, public project, PRIVATE usage
//	depUsedPID@0.9   PRIVATE version, private usage    (unopenable)
//	depOtherPID@7.1  public version, PRIVATE project   (unopenable)
//
// — so every combination the two rules distinguish is present at once, and
// a rule that leaked would leak into a list that also has a legitimate entry
// to hide behind.
func depState(t *testing.T) []ProjectDependencyState {
	t.Helper()
	return []ProjectDependencyState{
		depRow(t, depPin(t, depUsedPID, "1.0"), VisibilityPublic, VisibilityPublic, string(DependencyTypeDependsOn), VisibilityPublic, "Public Use Of A Public Version"),
		depRow(t, depPin(t, depUsedPID, "2.0"), VisibilityPublic, VisibilityPublic, string(DependencyTypeDependsOn), VisibilityPrivate, "Private Declaration Of A Public Version"),
		depRow(t, depPin(t, depUsedPID, "0.9"), VisibilityPrivate, VisibilityPublic, string(DependencyTypeDependsOn), VisibilityPrivate, "Private Version Of Someone Elses Asset"),
		depRow(t, depPin(t, depOtherPID, "7.1"), VisibilityPublic, VisibilityPrivate, string(DependencyTypeDependsOn), VisibilityPublic, "Public Version Of A Private Project"),
	}
}

// TestProjectDependenciesNonMemberSeesPublicDeclarationsOnly: rule 2. A
// non-member reading a public project (docs/12 §2) sees the declarations
// that are public and nothing else — and the withheld ones leave no trace:
// not an entry, not a blank, not a count, and not their strings anywhere in
// the serialized answer.
func TestProjectDependenciesNonMemberSeesPublicDeclarationsOnly(t *testing.T) {
	got := BuildProjectDependencies(depState(t), ProjectDependencyViewer{})
	if len(got.Dependencies) != 1 {
		t.Fatalf("a non-member saw %d dependencies, want exactly the one public declaration: %+v", len(got.Dependencies), got.Dependencies)
	}
	entry := got.Dependencies[0]
	if entry.Pin != DependencyPin(depPin(t, depUsedPID, "1.0")) {
		t.Errorf("the rendered entry is %q, want the project's public declaration", entry.Pin)
	}
	if entry.VisibilityOfUsage != VisibilityPublic {
		t.Errorf("a non-member was shown a %q declaration", entry.VisibilityOfUsage)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	// The withheld rows' own strings, as they really are in the fixture:
	// the private declaration (a public version used privately), the private
	// version, and the private project's asset. A leak of any of them is a
	// leak of a practice or of another project's identity.
	for _, secret := range []string{
		depPin(t, depUsedPID, "2.0"),
		depPin(t, depUsedPID, "0.9"),
		depPin(t, depOtherPID, "7.1"),
		"Private Declaration Of A Public Version",
		"Private Version Of Someone Elses Asset",
		"Public Version Of A Private Project",
	} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the non-member's answer mentions %q: %s", secret, raw)
		}
	}
}

// TestProjectDependenciesMemberSeesItsOwnPrivateDeclarations: rule 2's other
// half. A member may read the project itself, so the project's private
// declarations of publicly readable versions are shown to them — and that is
// the ONLY thing the member bit buys: rule 1 still drops the unopenable
// versions below.
func TestProjectDependenciesMemberSeesItsOwnPrivateDeclarations(t *testing.T) {
	got := BuildProjectDependencies(depState(t), ProjectDependencyViewer{Member: true})
	pins := make([]string, 0, len(got.Dependencies))
	for _, d := range got.Dependencies {
		pins = append(pins, string(d.Pin))
	}
	want := []string{depPin(t, depUsedPID, "1.0"), depPin(t, depUsedPID, "2.0")}
	if len(pins) != len(want) {
		t.Fatalf("a member saw %v, want exactly %v", pins, want)
	}
	for i := range want {
		if pins[i] != want[i] {
			t.Fatalf("a member saw %v, want exactly %v", pins, want)
		}
	}
	if got.Dependencies[1].VisibilityOfUsage != VisibilityPrivate {
		t.Errorf("the member's own private declaration is rendered %q", got.Dependencies[1].VisibilityOfUsage)
	}
}

// TestProjectDependenciesDropsVersionsTheNetworkCannotOpen: rule 1, and the
// member bit does not buy it. A version the network cannot open may not be
// named — its pin is another entity's identity and its title is that
// entity's display name — so both axes are exercised (a private version; a
// public version of an asset in a private project) for both viewers.
func TestProjectDependenciesDropsVersionsTheNetworkCannotOpen(t *testing.T) {
	unopenable := []string{depPin(t, depUsedPID, "0.9"), depPin(t, depOtherPID, "7.1")}
	for _, viewer := range []ProjectDependencyViewer{{}, {Member: true}} {
		got := BuildProjectDependencies(depState(t), viewer)
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatalf("marshal the answer: %v", err)
		}
		for _, pin := range unopenable {
			for _, d := range got.Dependencies {
				if string(d.Pin) == pin {
					t.Errorf("viewer %+v was shown %q, a version the network cannot open: %+v", viewer, pin, d)
				}
			}
			if strings.Contains(string(raw), pin) {
				t.Errorf("viewer %+v was answered text mentioning %q: %s", viewer, pin, raw)
			}
		}
	}
}

// TestProjectDependenciesRendersTheCanonicalPinAndItsURL: the entry names one
// exact version (the pin, verbatim) and where it can be opened — the version
// page, which exists for every rendered entry because rule 1 already
// required the network to be able to open it.
func TestProjectDependenciesRendersTheCanonicalPinAndItsURL(t *testing.T) {
	got := BuildProjectDependencies(depState(t), ProjectDependencyViewer{})
	entry := got.Dependencies[0]
	if want := AssetVersionURL(depUsedPID, "1.0"); entry.URL != want {
		t.Errorf("URL = %q, want %q", entry.URL, want)
	}
	if entry.Type != TypeDataset {
		t.Errorf("the used asset's type = %q, want the stored %q", entry.Type, TypeDataset)
	}
	if entry.Title != "Public Use Of A Public Version" {
		t.Errorf("the used asset's title = %q, want the stored title", entry.Title)
	}
	if !entry.CreatedAt.Equal(depTime) {
		t.Errorf("created_at = %v, want the instant %v", entry.CreatedAt, depTime)
	}
	if entry.CreatedAt.Location() != time.UTC {
		t.Errorf("created_at is rendered in %v, want UTC", entry.CreatedAt.Location())
	}
}

// TestProjectDependenciesImpactAnalysisFollowsTheCatalog: the answer carries
// the citation/dependency distinction as a boolean so a client does not have
// to re-derive the vocabulary. depends_on is true, references is false
// (docs/19 §3, catalog.go:42-45), and a stored value outside the vocabulary
// is rendered verbatim with the fail-closed false.
func TestProjectDependenciesImpactAnalysisFollowsTheCatalog(t *testing.T) {
	for _, tc := range []struct {
		stored string
		want   bool
	}{
		{string(DependencyTypeDependsOn), true},
		{string(DependencyTypeReferences), false},
		{"reuses", false}, // a repository state this platform cannot produce
		{"", false},
	} {
		row := depRow(t, depPin(t, depUsedPID, "1.0"), VisibilityPublic, VisibilityPublic, tc.stored, VisibilityPublic, "Title")
		got := BuildProjectDependencies([]ProjectDependencyState{row}, ProjectDependencyViewer{})
		if len(got.Dependencies) != 1 {
			t.Fatalf("stored type %q: got %d entries, want the one row rendered", tc.stored, len(got.Dependencies))
		}
		entry := got.Dependencies[0]
		if entry.ImpactAnalysis != tc.want {
			t.Errorf("stored type %q: impact_analysis = %v, want %v", tc.stored, entry.ImpactAnalysis, tc.want)
		}
		// The stored value is rendered as what it says, whatever it says:
		// the reader is not the gate.
		if string(entry.DependencyType) != tc.stored {
			t.Errorf("stored type %q was rendered as %q", tc.stored, entry.DependencyType)
		}
	}
}

// TestProjectDependenciesOfAProjectWithNothingIsEmptyAndNotNil: a project
// that has declared nothing has an answered empty list, never nil and never
// an error — the caller renders a stated absence.
func TestProjectDependenciesOfAProjectWithNothingIsEmptyAndNotNil(t *testing.T) {
	got := BuildProjectDependencies(nil, ProjectDependencyViewer{Member: true})
	if got.Dependencies == nil {
		t.Fatal("the answer's list is nil, want an empty list")
	}
	if len(got.Dependencies) != 0 {
		t.Errorf("an empty read rendered %+v", got.Dependencies)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal the answer: %v", err)
	}
	// The list must survive serialization as a list: "dependencies":null is
	// a missing field to a client, not an empty project.
	if !strings.Contains(string(raw), `"dependencies":[]`) {
		t.Errorf("an empty answer serializes as %s, want an empty list", raw)
	}
}

// TestProjectDependenciesDropsAMalformedPin: the stored pin is the join of
// two stored columns, and a pair that is not a canonical pin cannot name a
// version a reader could open. It is dropped rather than rendered half-named
// — and, again, not counted.
func TestProjectDependenciesDropsAMalformedPin(t *testing.T) {
	good := depRow(t, depPin(t, depUsedPID, "1.0"), VisibilityPublic, VisibilityPublic, string(DependencyTypeDependsOn), VisibilityPublic, "Good")
	bad := good
	bad.Pin = string(depUsedPID) + "@>=1.0" // not a pin: a range
	bad.Title = "Malformed Pin Row"
	got := BuildProjectDependencies([]ProjectDependencyState{bad, good}, ProjectDependencyViewer{Member: true})
	if len(got.Dependencies) != 1 {
		t.Fatalf("got %d entries, want only the well-formed row: %+v", len(got.Dependencies), got.Dependencies)
	}
	if got.Dependencies[0].Title != "Good" {
		t.Errorf("the wrong row survived: %+v", got.Dependencies[0])
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "Malformed Pin Row") {
		t.Errorf("the malformed row left a trace: %s", raw)
	}
}

// TestProjectDependenciesKeepsTheStoredOrder: the answer is the reader's
// order (created_at, then the pin), untouched. A model that sorted would be
// a second ordering rule the store's query also implements, and the two
// would drift.
func TestProjectDependenciesKeepsTheStoredOrder(t *testing.T) {
	rows := depState(t)
	got := BuildProjectDependencies(rows, ProjectDependencyViewer{Member: true})
	if len(got.Dependencies) != 2 {
		t.Fatalf("got %d entries, want 2", len(got.Dependencies))
	}
	if got.Dependencies[0].Pin != DependencyPin(rows[0].Pin) || got.Dependencies[1].Pin != DependencyPin(rows[1].Pin) {
		t.Errorf("the rendered order %q, %q is not the stored order %q, %q",
			got.Dependencies[0].Pin, got.Dependencies[1].Pin, rows[0].Pin, rows[1].Pin)
	}
}
