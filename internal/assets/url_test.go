package assets

import (
	"strings"
	"testing"
)

// The persistent URL scheme: every URL is built from the pid alone.
// These tests pin the exact shapes and — the acceptance criterion in
// URL form — that nothing mutable ever appears in them.

func TestPersistentURLShapes(t *testing.T) {
	pid, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		got  string
		want string
	}{
		{AssetURL(pid), "/assets/" + string(pid)},
		{AssetVersionURL(pid, "v1.0"), "/assets/" + string(pid) + "/v1.0"},
		{AssetAPIPath(pid), "/api/v1/assets/" + string(pid)},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("URL = %q, want %q", tc.got, tc.want)
		}
	}
}

// There is deliberately no unit test named ...StableAcrossSlugAndOrgChanges
// here. The acceptance criterion — the URL is byte-identical across a
// slug rename and an organization transfer — is a statement about an
// asset whose mutable metadata changed while its pid did not, and
// nothing in this package can express that state change: every builder
// here takes a pid and a label, so any unit test of it compares two
// calls with the same argument (the first T0701 delivery had exactly
// that: it looped over slugs, called AssetURL(pid) twice and could not
// fail for any input). The criterion is pinned where the state change
// actually happens, against a real PostgreSQL, by
// tests/integration/asset_core_test.go TestAssetCorePID — it UPDATEs the
// slug and the owning organization and re-reads the pid and the URL.

func TestAPIAssetIDFromPath(t *testing.T) {
	pid, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := APIAssetIDFromPath("/api/v1/assets/" + string(pid)); !ok || got != pid {
		t.Errorf("APIAssetIDFromPath(valid) = (%q, %v), want (%q, true)", got, ok, pid)
	}
	rejects := []string{
		"",
		"/api/v1/assets/",
		"/api/v1/assets/my-dataset-slug",        // a slug is not a pid
		"/api/v1/assets/" + string(pid) + "/v1", // deeper routes are not the asset path
		"/projects/" + string(pid),
		"/assets/" + string(pid),
	}
	for _, p := range rejects {
		if got, ok := APIAssetIDFromPath(p); ok {
			t.Errorf("APIAssetIDFromPath(%q) = (%q, true), want false", p, got)
		}
	}
}

func TestVersionLabelIsAURLSegment(t *testing.T) {
	pid, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	// Every label ValidVersionLabel accepts must produce a stable bare
	// URL segment: the two rules are one contract.
	for _, label := range []string{"1", "v1", "1.0", "2026-09-15", "v2.0.1-rc.1", "1_0"} {
		if !ValidVersionLabel(label) {
			t.Fatalf("label %q must be valid", label)
		}
		url := AssetVersionURL(pid, label)
		if strings.ContainsAny(url, " \t?&#%") {
			t.Errorf("AssetVersionURL(%q) = %q contains characters that need URL encoding", label, url)
		}
	}
}
