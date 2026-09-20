package domain

import (
	"errors"
	"fmt"
)

// The Activity sources (T0607). docs/26 §1 splits the platform's records
// into three kinds that must never be merged into one log: application
// logs, the domain audit log, and research events. The Activity page reads
// the second and the third — the governance record of who did what to a
// project (audit_log, T0110) and the research events the platform emits
// about it (research_events, T1001) — and this type is the reader's name
// for which of the two a row came from, and for which of the two a page
// asked to see.
//
// A row's source is an ENVELOPE fact, not a rendering one: it says which
// registry the row was read from, so a reader can tell a governance action
// from a research event even when they spell the same dotted name
// (scientific_object.aborted is both an audit action and an event type —
// the coincidence of two registries that internal/domain/audit.go records
// beside ActionPullRequestMerged; the two rows are different rows, written
// by different paths, and the Activity page shows both).
//
// The two feeds are never merged into one identity: a governance row's id
// is an audit_log id, a research row's id is a research_events id, and the
// source is what tells them apart when the (occurred_at, id) keyset
// cursor of one page covers both.
type ActivitySource string

const (
	// ActivitySourceGovernance is an audit_log row: a high-risk action
	// recorded with its actor, channel and correlation id (docs/26 §5).
	ActivitySourceGovernance ActivitySource = "governance"
	// ActivitySourceResearch is a research_events row: a subscribable
	// domain event (docs/26 §1), carrying the event type, its visibility
	// and its payload.
	ActivitySourceResearch ActivitySource = "research"
)

// ErrUnknownActivitySource is returned for a source value outside the two
// above. It is a wire-input failure, so the transport renders it as the
// validation error every other bad query parameter gets — never as "no
// such rows", which is what an unknown filter silently answered as an
// empty filter would look like.
var ErrUnknownActivitySource = errors.New("domain: unknown activity source")

// ParseActivitySource validates a source filter as it arrives from the
// client. "" is the absence of a filter — the page shows both registries —
// and is returned as the zero value, not as an error.
func ParseActivitySource(s string) (ActivitySource, error) {
	switch s {
	case "":
		return "", nil
	case string(ActivitySourceGovernance):
		return ActivitySourceGovernance, nil
	case string(ActivitySourceResearch):
		return ActivitySourceResearch, nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownActivitySource, s)
	}
}

// IncludesGovernance reports whether a page filtered by this source reads
// the audit registry (the absence of a filter reads both).
func (s ActivitySource) IncludesGovernance() bool {
	return s == "" || s == ActivitySourceGovernance
}

// IncludesResearch reports whether a page filtered by this source reads
// the research event registry (the absence of a filter reads both).
func (s ActivitySource) IncludesResearch() bool {
	return s == "" || s == ActivitySourceResearch
}
