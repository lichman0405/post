package backupdr

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSecretScanCountsWhatItSearched: values_searched is a security-relevant
// number, and the scan cannot look for a value shorter than
// MinScannableSecretBytes. The split has to account for the whole declared
// set, so that "searched N values" never stands for a set that was quietly
// larger — the shape where a report overstates the work behind it.
func TestSecretScanCountsWhatItSearched(t *testing.T) {
	declared := []string{"", "short", "postgres_dev_pw", "long-enough-to-hunt"}
	scanned, tooShort := scannableSecretValues(declared)
	if tooShort != 2 {
		t.Fatalf("skipped %d values as too short, want 2 (the empty string and \"short\", both below %d bytes)",
			tooShort, MinScannableSecretBytes)
	}
	if len(scanned)+tooShort != len(declared) {
		t.Fatalf("the split accounts for %d searched + %d skipped values, but %d were declared",
			len(scanned), tooShort, len(declared))
	}
	for _, s := range scanned {
		if len(s) < MinScannableSecretBytes {
			t.Fatalf("%q is below the floor but the scan was going to look for it", s)
		}
	}
}

// TestSecretScanFloorIsLoadBearing: the floor is what keeps the scan honest
// in both directions. A value it does search for has to be found, and a
// value below the floor has to be left alone — an empty declared value
// matches every artifact at offset 0, which would report the whole artifact
// set as a leak.
func TestSecretScanFloorIsLoadBearing(t *testing.T) {
	dir := t.TempDir()
	body := []byte("this artifact mentions supersecretvalue in passing\n")
	if err := os.WriteFile(filepath.Join(dir, "artifact.txt"), body, 0o644); err != nil {
		t.Fatalf("write the artifact: %v", err)
	}

	hits, err := ScanArtifactsForSecrets(dir, []string{"supersecretvalue"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 1 || hits[0].Offset != 23 {
		t.Fatalf("the scan found %+v; it was looking for a value the artifact contains at offset 23", hits)
	}

	hits, err = ScanArtifactsForSecrets(dir, []string{"", "super"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the scan reported %+v for values below the %d-byte floor — an empty value would have "+
			"reported every artifact", hits, MinScannableSecretBytes)
	}
}
