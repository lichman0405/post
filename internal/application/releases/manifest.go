package releases

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/lichman0405/post/internal/rsg/manifest"
)

// FormatV1 is the release manifest format version this package exports.
// It doubles as the hash algorithm version record (docs/21 §10): the
// canonical-JSON rules and the sha256 digest below are the semantics of
// format "v1" — the same convention as internal/rsg/manifest.
const FormatV1 = "v1"

// ReleaseManifest is one release's immutable snapshot document
// (docs/11 §1): the accepted main state's canonical export embedded in
// full, plus the release-level pins — the policy versions in force, the
// schema versions the snapshot references, and the review/approval record
// — content-addressed by manifest_hash. The field order below is
// canonical: it is the serialization order of the document and — through
// content, which mirrors it minus the two non-semantic fields — of the
// hash input.
type ReleaseManifest struct {
	FormatVersion string `json:"format_version"`
	ProjectID     string `json:"project_id"`
	StateID       string `json:"state_id"`
	// Version is the release version string (unique per project; the
	// releases UNIQUE(project_id, version) guard belongs to the write
	// path, T0606).
	Version string `json:"version"`
	// GeneratedAt is the render timestamp (UTC). Export-time metadata:
	// it moves between renders and is NOT part of manifest_hash.
	GeneratedAt time.Time `json:"generated_at"`
	// State pins the accepted main snapshot: the state's canonical
	// export (internal/rsg/manifest) by its hash-input bytes and its
	// state hash. The export's own generated_at is deliberately not
	// embedded — only the state's hashable content is, so the release
	// document renders identically forever.
	State StatePin `json:"state"`
	// Policy is the rights/policy snapshot: the organization lower bound
	// and the project's stricter overlay, each pinned with its full
	// canonical document (docs/12 §5). Null when no policy version was
	// pinned — the release gate then refuses (release_rights).
	Policy *PolicyPin `json:"policy"`
	// Schemas pins every schema version the snapshot's object versions
	// reference: id, version and the registry's content hash of the
	// exact registered document bytes. Sorted by (id, version).
	Schemas []SchemaPin `json:"schemas"`
	// Reviews is the review/approval record: every review of the
	// research PRs whose proposed states became part of the released
	// main lineage. Sorted by (pull_request_number, proposed_state_id).
	Reviews      []ReviewRecord `json:"reviews"`
	ManifestHash string         `json:"manifest_hash"`
}

// StatePin is the embedded state export: the exact bytes the state's
// state_hash was computed over (internal/rsg/manifest.ContentCanonicalJSON,
// the hash input without generated_at and state_hash), plus that hash.
// A verifier re-derives sha256(content) and compares it to state_hash.
type StatePin struct {
	StateHash string          `json:"state_hash"`
	Content   json.RawMessage `json:"content"`
}

// PolicyPin is the pinned policy snapshot: one version per scope, each
// with its full document. Either side may be absent (null) — e.g. a
// personal project has no organization policy — but a release whose pin
// is entirely absent fails the release gate.
type PolicyPin struct {
	// Organization is the organization's policy version in force (the
	// lower bound); null when the project has no organization or the
	// organization has no policy.
	Organization *PinnedPolicyVersion `json:"organization"`
	// Project is the project's policy version in force (the stricter
	// overlay); null when the project has none.
	Project *PinnedPolicyVersion `json:"project"`
}

// PinnedPolicyVersion is one policy version row pinned in full: its
// identity, its canonical document (sorted keys, compact — the
// domain.Policy.MarshalJSON form) and its authorship.
type PinnedPolicyVersion struct {
	ID        string          `json:"id"`
	Version   string          `json:"version"`
	Document  json.RawMessage `json:"document"`
	CreatedBy string          `json:"created_by"`
	CreatedAt time.Time       `json:"created_at"`
}

// SchemaPin pins one schema version: the ref the object versions carry,
// plus the registry's content hash of the exact registered document bytes
// (schemareg.Schema.ContentHash). id+version is immutable in the registry
// (re-registration with different content is refused), so the ref and the
// hash together pin the content completely.
type SchemaPin struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	ContentHash string `json:"content_hash"`
}

// ReviewRecord is the review history of one research PR whose proposed
// state became part of the released lineage: the PR's number and proposed
// state, and every review row recorded on it, oldest first. The record is
// complete — requests-for-changes and comments are part of the history,
// not just the approvals (the release gate derives approval from the
// record).
type ReviewRecord struct {
	PullRequestNumber int64    `json:"pull_request_number"`
	ProposedStateID   string   `json:"proposed_state_id"`
	Reviews           []Review `json:"reviews"`
}

// Review is one stored review row (canonical table reviews).
type Review struct {
	// ID is the review row id — part of the record so the array has a
	// total deterministic order even when timestamps tie.
	ID         string    `json:"id"`
	ReviewerID string    `json:"reviewer_id"`
	ReviewKind string    `json:"review_kind"`
	Decision   string    `json:"decision"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// ManifestInput is the pinned input set one release manifest renders from.
// Everything in it is recorded content — the state's canonical export,
// the policy version rows, the registry's schema registrations and the
// stored review rows — so the render is a pure function of it.
type ManifestInput struct {
	ProjectID string
	StateID   string
	// Version is the release version string, already validated and
	// trimmed by the service.
	Version     string
	GeneratedAt time.Time
	// State is the state's canonical export (T0206) to embed. Its own
	// generated_at is ignored — only the hashable content is pinned.
	State   *manifest.Manifest
	Policy  *PolicyPin
	Schemas []SchemaPin
	Reviews []ReviewRecord
}

// content is the hash input: the manifest without generated_at (render
// time, different per render by definition) and manifest_hash (the digest
// itself). Its canonical JSON is what manifest_hash is computed over; a
// verifier re-marshals the parsed manifest's semantic fields through it
// and compares digests. Keep the fields in sync with ReleaseManifest
// minus the two omitted ones — TestContentMirrorsDocument pins that.
type content struct {
	FormatVersion string         `json:"format_version"`
	ProjectID     string         `json:"project_id"`
	StateID       string         `json:"state_id"`
	Version       string         `json:"version"`
	State         StatePin       `json:"state"`
	Policy        *PolicyPin     `json:"policy"`
	Schemas       []SchemaPin    `json:"schemas"`
	Reviews       []ReviewRecord `json:"reviews"`
}

// Build assembles the canonical release manifest from the pinned inputs.
// It canonicalizes every embedded document, applies the stable orderings
// and computes manifest_hash. The result marshals to the same bytes on
// every render of the same release — generated_at is the only field that
// moves, and it is not part of the hash (acceptance: manifest
// deterministic).
//
// The embedded state export is verified against its own state hash before
// it is pinned: a manifest whose pin does not re-derive would record a
// hash chain that never closes.
func Build(in ManifestInput) (*ReleaseManifest, error) {
	if in.State == nil {
		return nil, errors.New("releases: manifest input: state export is required")
	}
	if !in.State.VerifyHash() {
		return nil, fmt.Errorf("releases: manifest input: state %s export does not verify against its state hash", in.State.StateID)
	}
	stateContent, err := in.State.ContentCanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("releases: manifest input: state %s export content: %w", in.State.StateID, err)
	}
	policy, err := canonicalPolicyPin(in.Policy)
	if err != nil {
		return nil, fmt.Errorf("releases: manifest input: policy pin: %w", err)
	}

	m := &ReleaseManifest{
		FormatVersion: FormatV1,
		ProjectID:     in.ProjectID,
		StateID:       in.StateID,
		Version:       in.Version,
		GeneratedAt:   in.GeneratedAt.UTC(),
		State: StatePin{
			StateHash: in.State.StateHash,
			Content:   stateContent,
		},
		Policy:  policy,
		Schemas: canonicalSchemas(in.Schemas),
		Reviews: canonicalReviews(in.Reviews),
	}
	digest, err := m.digest()
	if err != nil {
		return nil, err
	}
	m.ManifestHash = digest
	return m, nil
}

// canonicalPolicyPin copies the pin with every document re-encoded into
// canonical JSON (sorted keys, compact, numbers preserved) — the same
// rule the state manifest applies to payloads. A document that is not
// valid JSON is refused: silently dropping bytes would let two different
// stored policies hash alike.
func canonicalPolicyPin(pin *PolicyPin) (*PolicyPin, error) {
	if pin == nil {
		return nil, nil
	}
	out := &PolicyPin{}
	if pin.Organization != nil {
		org := *pin.Organization
		doc, err := canonicalBytes(org.Document)
		if err != nil {
			return nil, fmt.Errorf("organization policy %s: %w", org.ID, err)
		}
		org.Document = doc
		org.CreatedAt = org.CreatedAt.UTC()
		out.Organization = &org
	}
	if pin.Project != nil {
		proj := *pin.Project
		doc, err := canonicalBytes(proj.Document)
		if err != nil {
			return nil, fmt.Errorf("project policy %s: %w", proj.ID, err)
		}
		proj.Document = doc
		proj.CreatedAt = proj.CreatedAt.UTC()
		out.Project = &proj
	}
	return out, nil
}

// canonicalSchemas copies the schema pins in stable order
// ((id, version)) — the render must not depend on the order the caller
// collected the refs in.
func canonicalSchemas(pins []SchemaPin) []SchemaPin {
	out := make([]SchemaPin, len(pins))
	copy(out, pins)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Version < out[j].Version
	})
	return out
}

// canonicalReviews copies the review records in stable order
// ((pull_request_number, proposed_state_id)) with the reviews of each
// record ordered by (created_at, id) — deterministic regardless of the
// order the store returned rows in.
func canonicalReviews(records []ReviewRecord) []ReviewRecord {
	out := make([]ReviewRecord, len(records))
	copy(out, records)
	for i := range out {
		rs := make([]Review, len(out[i].Reviews))
		copy(rs, out[i].Reviews)
		for j := range rs {
			rs[j].CreatedAt = rs[j].CreatedAt.UTC()
		}
		sort.Slice(rs, func(a, b int) bool {
			if !rs[a].CreatedAt.Equal(rs[b].CreatedAt) {
				return rs[a].CreatedAt.Before(rs[b].CreatedAt)
			}
			return rs[a].ID < rs[b].ID
		})
		out[i].Reviews = rs
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PullRequestNumber != out[j].PullRequestNumber {
			return out[i].PullRequestNumber < out[j].PullRequestNumber
		}
		return out[i].ProposedStateID < out[j].ProposedStateID
	})
	return out
}

// canonicalBytes re-encodes a stored JSON document into canonical form
// (sorted keys, no insignificant whitespace, numbers preserved) via the
// state manifest's rule — one canonical encoder for every embedded
// document. The input must be exactly one JSON value; trailing content is
// rejected (see manifest.CanonicalJSON).
func canonicalBytes(raw []byte) (json.RawMessage, error) {
	return manifest.CanonicalJSON(raw)
}

// digest derives manifest_hash from the manifest's content.
func (m *ReleaseManifest) digest() (string, error) {
	b, err := m.ContentCanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("releases: canonical content: %w", err)
	}
	return manifest.Digest(b), nil
}

// ContentCanonicalJSON renders the hash input: the manifest's semantic
// content as canonical JSON, without generated_at and manifest_hash.
// Verifiers re-derive manifest_hash by digesting these bytes.
func (m *ReleaseManifest) ContentCanonicalJSON() ([]byte, error) {
	c := content{
		FormatVersion: m.FormatVersion,
		ProjectID:     m.ProjectID,
		StateID:       m.StateID,
		Version:       m.Version,
		State:         m.State,
		Policy:        m.Policy,
		Schemas:       m.Schemas,
		Reviews:       m.Reviews,
	}
	return json.Marshal(c)
}

// CanonicalJSON renders the full manifest document in its canonical form.
func (m *ReleaseManifest) CanonicalJSON() ([]byte, error) {
	return json.Marshal(m)
}

// VerifyHash reports whether ManifestHash is the digest of the manifest's
// current content — the self-check a consumer runs before trusting an
// exported release manifest.
func (m *ReleaseManifest) VerifyHash() bool {
	digest, err := m.digest()
	if err != nil {
		return false
	}
	return m.ManifestHash == digest
}
