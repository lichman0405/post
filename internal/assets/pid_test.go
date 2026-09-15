package assets

import (
	"strings"
	"testing"
)

// The pid is the asset's persistent identity: random, fixed-shape, and
// independent of every mutable attribute. These tests pin the shape,
// the alphabet, the entropy source and the rejection of anything that
// could have been derived from a slug or an organization.

func TestNewPIDShapeAndAlphabet(t *testing.T) {
	for i := 0; i < 256; i++ {
		p, err := NewPID()
		if err != nil {
			t.Fatalf("NewPID() error: %v", err)
		}
		if !p.Valid() {
			t.Fatalf("NewPID() = %q does not satisfy its own Valid", p)
		}
		if len(p) != pidLen {
			t.Fatalf("NewPID() length = %d, want %d", len(p), pidLen)
		}
		for _, r := range p {
			if !strings.ContainsRune(pidAlphabet, r) {
				t.Fatalf("NewPID() = %q contains %q outside the Crockford alphabet", p, r)
			}
		}
	}
}

func TestNewPIDIsRandom(t *testing.T) {
	seen := make(map[PID]struct{}, 4096)
	for i := 0; i < 4096; i++ {
		p, err := NewPID()
		if err != nil {
			t.Fatalf("NewPID() error: %v", err)
		}
		if _, dup := seen[p]; dup {
			t.Fatalf("NewPID() collision after %d draws: %q", i+1, p)
		}
		seen[p] = struct{}{}
	}
}

func TestValidPID(t *testing.T) {
	pid, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	valid := []string{
		string(pid),
		// 26 characters that include BOTH endpoints of every range of the
		// alphabet (0-9, a-h, j-k, m-n, p-t, v-z) — a range narrowed by
		// mistake fails here, not only on the generated pid. The claim is
		// checked, not asserted: pidRangeEndpoints below requires every
		// endpoint to appear in this sample, because the previous version
		// of this comment said "both endpoints of every range" over a
		// sample that had no 'n'.
		"0123456789abcdefghjkmnptvz",
		"00000000000000000000000000",
	}
	// pidRangeEndpoints is the alphabet as the ranges ValidPID is written
	// in: the rune switch in pid.go lists them as ranges, so every
	// endpoint must appear in the sample above or that range's coverage
	// rests on a random draw.
	pidRangeEndpoints := []rune{'0', '9', 'a', 'h', 'j', 'k', 'm', 'n', 'p', 't', 'v', 'z'}
	for _, endpoint := range pidRangeEndpoints {
		if !strings.ContainsRune(valid[1], endpoint) {
			t.Errorf("the valid sample %q does not cover %q, an endpoint of a range of the alphabet: the comment's coverage claim is false",
				valid[1], endpoint)
		}
	}
	for _, s := range valid {
		if len(s) != pidLen {
			t.Errorf("valid sample %q is %d chars, want %d", s, len(s), pidLen)
		}
		for _, r := range s {
			if !strings.ContainsRune(pidAlphabet, r) {
				t.Errorf("valid sample %q contains %q, outside the alphabet", s, r)
			}
		}
		if !ValidPID(s) {
			t.Errorf("ValidPID(%q) = false, want true", s)
		}
	}

	// ValidPID has two rules — the fixed length and the Crockford
	// alphabet — and each group below can only be refused by the rule it
	// names. Membership is asserted, not assumed: an off-length case must
	// be entirely alphabet-legal (or the alphabet would be doing the
	// refusing), and a bad-character case must be exactly pidLen long (or
	// the length check would). Without those assertions a case drifts
	// into testing the wrong rule silently — which is how the first T0701
	// delivery left the alphabet rule with no test at all.
	offLength := []struct{ s, why string }{
		{"", "empty"},
		{"abc", "3 chars"},
		{"0123456789abcdefghjkmnpqr", "25 chars"},
		{"0123456789abcdefghjkmnpqrsv", "27 chars"},
	}
	for _, c := range offLength {
		if len(c.s) == pidLen {
			t.Errorf("offLength case %q (%s) is exactly pidLen: it must be refused for its length", c.s, c.why)
		}
		for _, r := range c.s {
			if !strings.ContainsRune(pidAlphabet, r) {
				t.Errorf("offLength case %q (%s) contains %q, outside the alphabet: only the length rule may refuse it",
					c.s, c.why, r)
			}
		}
		if ValidPID(c.s) {
			t.Errorf("ValidPID(%q) = true, want false (%s)", c.s, c.why)
		}
	}
	badChar := []struct{ s, why string }{
		{"0123456789abcdefghjkmnpqri", "'i' is not in the alphabet"},
		{"0123456789abcdefghjkmnpqrl", "'l' is not in the alphabet"},
		{"0123456789abcdefghjkmnpqro", "'o' is not in the alphabet"},
		{"0123456789abcdefghjkmnpqru", "'u' is not in the alphabet"},
		{"0123456789abcdefghjkmnpqrS", "uppercase is not emitted"},
		{"0123456789abcdefghjkmnpqr ", "space"},
		{"0123456789abcdefghjkmnpqr-", "'-'"},
		{"0123456789abcdefghjkmnpqr_", "'_'"},
		{"asset_0123456789abcdefghjk", "prefixed slug-like strings are not pids"},
		{"my-dataset-version-1-2026-", "a slug is not a pid"},
	}
	for _, c := range badChar {
		if len(c.s) != pidLen {
			t.Errorf("badChar case %q (%s) is %d chars, want %d: only the alphabet rule may refuse it",
				c.s, c.why, len(c.s), pidLen)
		}
		if ValidPID(c.s) {
			t.Errorf("ValidPID(%q) = true, want false (%s)", c.s, c.why)
		}
	}
}

// The acceptance criterion in miniature: a pid is not derived from the
// slug or the organization — the two are generated independently, so a
// pid contains nothing of either, and nothing about either input can
// ever change it.
func TestPIDIsNotDerivedFromSlugOrOrg(t *testing.T) {
	slug := "my-dataset"
	org := "acme-labs"
	p1, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	p2, err := NewPID()
	if err != nil {
		t.Fatal(err)
	}
	if p1 == p2 {
		t.Fatal("two pids generated back to back must differ (randomness, not derivation)")
	}
	if p1 == PID(slug) || p1 == PID(org) {
		t.Fatalf("pid %q must not equal a slug or an organization name", p1)
	}
	if strings.Contains(string(p1), slug) || strings.Contains(string(p1), org) {
		t.Fatalf("pid %q must contain nothing of the slug %q or the org %q", p1, slug, org)
	}
	if !p1.Valid() {
		t.Fatalf("pid %q invalid", p1)
	}
}
