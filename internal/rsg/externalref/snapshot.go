package externalref

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Snapshot pins what upstream looked like at one access (docs/19 §2:
// accessed_at, upstream version, metadata snapshot, hash). It is the
// pre-store value the refresher produces; once the caller persists it as
// an external_reference_snapshots row it is IMMUTABLE — the database
// rejects UPDATE/DELETE of snapshot rows (migrations 00014/00015) and
// the refresher only ever produces new snapshots, so nothing in this
// domain can rewrite one that exists.
type Snapshot struct {
	// ExternalReferenceID names the live identity this snapshot pins.
	ExternalReferenceID string
	// AccessedAt is the server time upstream was observed (never
	// caller-supplied: the builder stamps it).
	AccessedAt time.Time
	// UpstreamVersion is the upstream version marker when the source
	// reports one; empty otherwise.
	UpstreamVersion string
	// Metadata is the upstream metadata document as canonical JSON (an
	// object). Untrusted upstream data, never instructions (docs/23
	// §8).
	Metadata json.RawMessage
	// SnapshotHash is the sha256 hex digest of Metadata's canonical
	// bytes — the builder's deterministic fingerprint, used for change
	// detection between refreshes. NOTE the stored row's snapshot_hash
	// is DERIVED BY THE DATABASE from the jsonb-canonical metadata text
	// (migration 00045): where jsonb key ordering differs from Go's,
	// the two digests differ in spelling, not in meaning — both are
	// deterministic functions of the same document, and the database
	// value is the authoritative one (docs/23 §3).
	SnapshotHash string
}

// BuildSnapshot turns one fetch result into a pinned snapshot: it
// re-canonicalizes the metadata (parse + marshal, so equal documents
// always produce equal bytes and equal hashes) and stamps the server
// time. now is a seam for tests; nil means time.Now. The document must
// be a JSON OBJECT — the same shape the 00045 DB guard demands — so a
// snapshot always pins a metadata document, never a scalar or null.
func BuildSnapshot(externalReferenceID string, meta UpstreamMetadata, now func() time.Time) (Snapshot, error) {
	var doc map[string]any
	if err := json.Unmarshal(meta.Metadata, &doc); err != nil {
		return Snapshot{}, errf(ErrInvalidUpstream, "upstream metadata is not a JSON document: %v", err)
	}
	if doc == nil {
		// "null" unmarshals into a nil map without error; it is not a
		// document.
		return Snapshot{}, errf(ErrInvalidUpstream, "upstream metadata is null, not a JSON object")
	}
	canonical, err := json.Marshal(doc)
	if err != nil {
		return Snapshot{}, errf(ErrInvalidUpstream, "re-encoding upstream metadata: %v", err)
	}
	clock := now
	if clock == nil {
		clock = time.Now
	}
	return Snapshot{
		ExternalReferenceID: externalReferenceID,
		AccessedAt:          clock().UTC(),
		UpstreamVersion:     meta.UpstreamVersion,
		Metadata:            canonical,
		SnapshotHash:        digest(canonical),
	}, nil
}

// digest is the builder's fingerprint: sha256 hex over the canonical
// metadata bytes (see Snapshot.SnapshotHash for the DB relationship).
func digest(metadata []byte) string {
	sum := sha256.Sum256(metadata)
	return hex.EncodeToString(sum[:])
}

// SameContent reports whether two snapshots pin the same upstream state:
// the (Metadata, UpstreamVersion) PAIR is compared — the canonical
// metadata bytes after re-canonicalization (so a head row read back from
// the database in jsonb-canonical form and a freshly built snapshot
// compare equal whenever the documents they hold are equal — byte
// spellings never cause a false "changed"), and the upstream version
// marker as a whole value. A source whose document is unchanged but
// whose version marker moved is therefore CHANGED and appends: the
// version marker is part of what upstream reported, and an adapter that
// reports it separately from the document must still move the log (see
// the Fetcher contract).
func SameContent(a, b Snapshot) bool {
	var da, db map[string]any
	if json.Unmarshal(a.Metadata, &da) != nil || json.Unmarshal(b.Metadata, &db) != nil {
		return false
	}
	ca, errA := json.Marshal(da)
	cb, errB := json.Marshal(db)
	if errA != nil || errB != nil {
		return false
	}
	return string(ca) == string(cb) && a.UpstreamVersion == b.UpstreamVersion
}
