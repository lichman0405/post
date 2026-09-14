package externalref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Ref names one external source whose metadata a refresh wants. It is the
// live identity half (docs/19 §2): the source_type + external_identifier
// pair and the canonical URL.
type Ref struct {
	// SourceType is the external_reference.schema.json source_type enum
	// value (publication, patent, database, standard, vendor, web,
	// other).
	SourceType string
	// ExternalIdentifier is the identifier as the user spelled it;
	// DOI-shaped identifiers are normalized by the fetcher before
	// resolution (domain.NormalizeExternalIdentifier).
	ExternalIdentifier string
	// CanonicalURL is the live URL when the identifier alone does not
	// resolve (optional in V1 — the DOI adapter resolves by identifier).
	CanonicalURL string
}

// UpstreamMetadata is what one fetch learned from the upstream source:
// the metadata document plus the version marker it reports.
type UpstreamMetadata struct {
	// UpstreamVersion is the upstream version marker when the source
	// reports one (the CSL-JSON version field); empty otherwise.
	UpstreamVersion string
	// Metadata is the upstream metadata document as a canonical JSON
	// object (parsed and re-marshaled, so key order is deterministic).
	// It is untrusted upstream data — never instructions (docs/23 §8).
	Metadata json.RawMessage
}

// Fetcher is the upstream metadata port. An adapter implements it against
// one upstream source kind; the V1 adapter is DoiFetcher (DOI content
// negotiation). A fetch that cannot be satisfied — the identifier is not
// resolvable, upstream is unreachable, or the response is not usable
// metadata — fails with one of the package errors below; the refresh is
// aborted, never guessed.
//
// CHANGE-DETECTION CONTRACT: SameContent compares the (Metadata,
// UpstreamVersion) PAIR, never the document bytes alone. An adapter
// whose source reports a version marker separately from the document
// body MUST populate UpstreamVersion: a source whose document is
// unchanged but whose version moved counts as CHANGED and appends a new
// snapshot, exactly as if the document had moved. An adapter whose
// source reports no version leaves the field empty, and empty then
// compares equal only to empty — so the contract costs an adapter
// nothing, but reporting a version it already knows keeps the log from
// ever under-reporting upstream movement.
type Fetcher interface {
	Fetch(ctx context.Context, ref Ref) (UpstreamMetadata, error)
}

// Sentinel errors for the fetch surface. errors.Is matches both the
// sentinel and its wrapped causes.
var (
	// ErrNotResolvable reports a ref whose identifier no adapter can
	// resolve (V1: not DOI-shaped, or the DOI is malformed).
	ErrNotResolvable = errors.New("externalref: identifier is not resolvable by any adapter")
	// ErrUpstreamUnavailable reports an upstream that answered with an
	// error status or could not be reached. The cause carries the
	// transport detail for the log; the message never goes on the wire
	// with dependency detail (docs/22 §5).
	ErrUpstreamUnavailable = errors.New("externalref: upstream metadata source unavailable")
	// ErrInvalidUpstream reports an upstream answer that is not usable
	// metadata (not JSON, not an object, or over the size cap).
	ErrInvalidUpstream = errors.New("externalref: upstream response is not usable metadata")
	// ErrRefusedURL reports a fetch URL the SSRF guard refused
	// (docs/23 §7, docs/54 threat #5).
	ErrRefusedURL = errors.New("externalref: fetch URL refused by the SSRF guard")
)

// maxMetadataBytes bounds one upstream metadata document (CSL-JSON
// records are a few KB; a megabyte is already generous and keeps a
// malicious upstream from making the platform buffer unbounded data).
const maxMetadataBytes = 1 << 20 // 1 MiB

// errf wraps a sentinel with the cause, keeping errors.Is working.
func errf(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}
