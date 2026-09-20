package domain_test

import (
	"errors"
	"testing"

	"github.com/lichman0405/post/internal/domain"
)

// TestParseActivitySource: the source filter has exactly three acceptable
// inputs — the empty filter (both registries) and the two registry names.
// The negative cases matter more than the positive ones: a client that
// mistypes a filter must be refused, never answered with a subset it did
// not ask for (Governance spelled with a capital G would otherwise read as
// "no filter" if the parse fell back, or as "unknown" if it did not).
func TestParseActivitySource(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   domain.ActivitySource
		wantOK bool
	}{
		{in: "", want: "", wantOK: true},
		{in: "governance", want: domain.ActivitySourceGovernance, wantOK: true},
		{in: "research", want: domain.ActivitySourceResearch, wantOK: true},
		{in: "Governance"},
		{in: "RESEARCH"},
		{in: "audit"},  // the registry's table name, not its Activity name
		{in: "events"}, // the other registry's table name
		{in: "all"},    // there is no "all": the absence of the param is all
		{in: " research"},
		{in: "research "},
	} {
		got, err := domain.ParseActivitySource(tc.in)
		if tc.wantOK {
			if err != nil {
				t.Errorf("ParseActivitySource(%q) = %v, want %q", tc.in, err, tc.want)
				continue
			}
			if got != tc.want {
				t.Errorf("ParseActivitySource(%q) = %q, want %q", tc.in, got, tc.want)
			}
			continue
		}
		if !errors.Is(err, domain.ErrUnknownActivitySource) {
			t.Errorf("ParseActivitySource(%q) = %v, want ErrUnknownActivitySource", tc.in, err)
		}
		if got != "" {
			t.Errorf("ParseActivitySource(%q) returned %q beside its error; a refused filter must not also carry a value", tc.in, got)
		}
	}
}

// TestActivitySourceIncludes: the two branches a page reads. The empty
// source reads BOTH — that is what makes "no filter" different from either
// filter — and each named source reads exactly its own branch.
func TestActivitySourceIncludes(t *testing.T) {
	for _, tc := range []struct {
		source               domain.ActivitySource
		governance, research bool
	}{
		{source: "", governance: true, research: true},
		{source: domain.ActivitySourceGovernance, governance: true},
		{source: domain.ActivitySourceResearch, research: true},
	} {
		if got := tc.source.IncludesGovernance(); got != tc.governance {
			t.Errorf("%q.IncludesGovernance() = %v, want %v", tc.source, got, tc.governance)
		}
		if got := tc.source.IncludesResearch(); got != tc.research {
			t.Errorf("%q.IncludesResearch() = %v, want %v", tc.source, got, tc.research)
		}
	}
}
