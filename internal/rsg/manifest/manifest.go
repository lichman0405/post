// Package manifest is the canonical export format of one RSG state
// (task T0206): the open manifest of docs/07 §4, whose state_hash
// content-addresses the project's research-state graph as of that state.
//
// Canonical serialization means the same bytes every time:
//
//   - fixed field order (the struct declaration order below — the manifest
//     schema specs/schemas/rsg-manifest.schema.json mirrors it);
//   - compact JSON, no insignificant whitespace;
//   - payloads re-encoded from their stored jsonb text into canonical form:
//     object keys sorted, numbers preserved exactly (json.Number), so rows
//     holding semantically identical jsonb in different spellings hash
//     alike;
//   - stable ordering: object versions by (object_id, version_no, id),
//     relation versions by (relation_id, version_no, id), schema/policy
//     refs and blob refs sorted by their keys;
//   - timestamps normalized to UTC.
//
// The state_hash is sha256 over the canonical JSON of the manifest's
// semantic content — every field above except generated_at (export-time,
// varies per export) and state_hash itself. Exporting the same state twice
// therefore derives the same hash; any semantic change (a different object
// version, relation version, payload, blob or git ref) moves it. The hash
// algorithm is pinned by FormatV1 ("v1"): the project_states row records
// manifest_version, so a future algorithm is a new format version, never a
// silent rewrite of old hashes (docs/21 §10).
//
// Purity invariant: a state's hash is a pure function of the state's own
// recorded content — the state row plus its lineage's member rows. Build
// therefore takes no mutable input: the git ref is the state's recorded
// commit sha (project_states.git_commit_sha), and nothing is read from the
// mutable project row (whose git repository id may change after the state
// was written — exporting through it would let a later project update move
// an earlier state's hash). The state row and the object/relation version
// rows are guarded append-only by migrations 00014/00015; blob attachment
// membership and blobs.content_hash are not guarded in place, but no writer
// mutates them today (AttachBlob only inserts with a state_id; the only
// blobs UPDATE touches integrity_state, which the manifest does not read) —
// extending the append-only guard to them is a recorded follow-up.
//
// The manifest covers the state's full lineage: every scientific object
// version and relation version whose state_id is the state itself or one of
// its ancestors (the per-branch state chain), plus the blob attachments
// whose own state_id AND owning version are in the lineage — the complete
// versioned graph, not just the transition's direct members. A version
// created on a forked branch is not part of the ancestor branch's
// manifests, and vice versa; likewise an attachment created in a later
// state is not part of earlier states' manifests.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/lichman0405/post/internal/domain"
)

// FormatV1 is the manifest format version this package exports. It doubles
// as the hash algorithm version record (docs/21 §10): the canonical-JSON
// rules and the sha256 digest above are the semantics of format "v1", and
// states record it in project_states.manifest_version when they are
// committed (internal/application/rsg writes the same value).
const FormatV1 = "v1"

// HashAlgorithm is the digest algorithm of the state_hash field. It is
// prefixed to the hex digest so a manifest self-describes how to verify it.
const HashAlgorithm = "sha256"

// Manifest is one state's export document. The field order below is
// canonical: it is the serialization order of the manifest document and —
// through content, which mirrors it minus the two non-semantic fields — of
// the hash input.
type Manifest struct {
	FormatVersion    string            `json:"format_version"`
	ProjectID        string            `json:"project_id"`
	StateID          string            `json:"state_id"`
	GeneratedAt      time.Time         `json:"generated_at"`
	ObjectVersions   []ObjectVersion   `json:"object_versions"`
	RelationVersions []RelationVersion `json:"relation_versions"`
	SchemaRefs       []string          `json:"schema_refs"`
	PolicyRefs       []string          `json:"policy_refs"`
	BlobRefs         []BlobRef         `json:"blob_refs"`
	// GitRef is the state's recorded GitProvider commit sha
	// (project_states.git_commit_sha); null when the state has none.
	GitRef    *string `json:"git_ref"`
	StateHash string  `json:"state_hash"`
}

// content is the hash input: the manifest without generated_at (export
// time, different per export by definition) and state_hash (the digest
// itself). Its canonical JSON is what the state_hash is computed over; a
// verifier re-marshals the parsed manifest's semantic fields through it and
// compares digests. Keep the fields in sync with Manifest minus the two
// omitted ones — TestManifestContentMirrorsDocument pins that.
type content struct {
	FormatVersion    string            `json:"format_version"`
	ProjectID        string            `json:"project_id"`
	StateID          string            `json:"state_id"`
	ObjectVersions   []ObjectVersion   `json:"object_versions"`
	RelationVersions []RelationVersion `json:"relation_versions"`
	SchemaRefs       []string          `json:"schema_refs"`
	PolicyRefs       []string          `json:"policy_refs"`
	BlobRefs         []BlobRef         `json:"blob_refs"`
	GitRef           *string           `json:"git_ref"`
}

// ObjectVersion is one scientific object version row in the manifest: the
// append-only version-log row plus the container's object_type, so the
// manifest names the object's scientific type without a second table
// (docs/07 §2: nodes are identified by object_type + version).
type ObjectVersion struct {
	// ID is the version row id (scientific_object_versions.id).
	ID string `json:"id"`
	// ObjectID names the container object the version belongs to.
	ObjectID string `json:"object_id"`
	// ObjectType is the object's scientific type from the container row.
	ObjectType string `json:"object_type"`
	// VersionNo is the 1-based position in the object's version log.
	VersionNo int `json:"version_no"`
	// StateID is the state the version was created in.
	StateID string `json:"state_id"`
	// BranchID is the research branch the version was created on; null
	// when the version predates branch resolution.
	BranchID *string `json:"branch_id"`
	// SchemaRef pins the JSON Schema governing Payload.
	SchemaRef SchemaRef `json:"schema_ref"`
	Title     string    `json:"title"`
	// LifecycleState is the version's lifecycle position.
	LifecycleState string `json:"lifecycle_state"`
	// Payload is the versioned scientific content in canonical form.
	Payload json.RawMessage `json:"payload"`
	// VisibilityPolicyID pins the rights policy of this version; null
	// inherits the project default.
	VisibilityPolicyID *string `json:"visibility_policy_id"`
	// IntegrityHash is the stored payload digest (sha256 of the jsonb text
	// form, docs/23 §3).
	IntegrityHash string    `json:"integrity_hash"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
}

// SchemaRef names one registered JSON Schema version (the schemareg.Ref
// halves stored on the version row).
type SchemaRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

// RelationVersion is one relation version row in the manifest: the typed,
// version-pinned edge (docs/07 §3).
type RelationVersion struct {
	ID string `json:"id"`
	// RelationID names the container edge.
	RelationID string `json:"relation_id"`
	// VersionNo is the 1-based position in the edge's version log.
	VersionNo int `json:"version_no"`
	// StateID is the state the version was created in.
	StateID string `json:"state_id"`
	// RelationType is the edge's type from the relation catalog (docs/44).
	RelationType string `json:"relation_type"`
	// SourceObjectVersionID pins the source endpoint to one exact object
	// version.
	SourceObjectVersionID string `json:"source_object_version_id"`
	// TargetObjectVersionID pins the target endpoint to one exact object
	// version.
	TargetObjectVersionID string `json:"target_object_version_id"`
	// Payload is the edge's scope/metadata in canonical form.
	Payload       json.RawMessage `json:"payload"`
	IntegrityHash string          `json:"integrity_hash"`
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

// BlobRef is one blob attachment of the snapshot's object versions: the
// blob id and its content hash (the blobs.content_hash the bytes were
// uploaded under). The bytes themselves live in S3/MinIO — the manifest
// pins the reference, never the content (invariant 7).
type BlobRef struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// Snapshot is the raw read surface one manifest is built from: the state's
// lineage rows as the persistence layer returns them (payloads in their
// stored form; Build canonicalizes).
type Snapshot struct {
	ObjectVersions   []ObjectVersion
	RelationVersions []RelationVersion
	BlobRefs         []BlobRef
}

// Build assembles the canonical manifest of state from the snapshot rows
// and the export timestamp. It canonicalizes every payload, applies the
// stable orderings, derives the schema/policy refs, renders the git ref
// (the state's own recorded commit sha, or null) and computes the
// state_hash. The result marshals to the same bytes on every export of the
// same state — generated_at is the only field that moves between exports,
// and it is not part of the hash.
//
// Build takes no project or other mutable input on purpose: a state's hash
// is a pure function of the state's recorded content (the package doc's
// purity invariant), so every hash input below is either a state-row field
// or a lineage member row.
func Build(state domain.ProjectState, snap Snapshot, generatedAt time.Time) (*Manifest, error) {
	if state.ManifestVersion != "" && state.ManifestVersion != FormatV1 {
		return nil, fmt.Errorf("manifest: state %s was written under manifest format %q, this exporter speaks %q", state.ID, state.ManifestVersion, FormatV1)
	}
	m := &Manifest{
		FormatVersion:    FormatV1,
		ProjectID:        state.ProjectID,
		StateID:          state.ID,
		GeneratedAt:      generatedAt.UTC(),
		ObjectVersions:   make([]ObjectVersion, 0, len(snap.ObjectVersions)),
		RelationVersions: make([]RelationVersion, 0, len(snap.RelationVersions)),
		SchemaRefs:       []string{},
		PolicyRefs:       []string{},
		BlobRefs:         make([]BlobRef, 0, len(snap.BlobRefs)),
	}
	for _, v := range snap.ObjectVersions {
		payload, err := CanonicalJSON(v.Payload)
		if err != nil {
			return nil, fmt.Errorf("manifest: object version %s payload is not valid JSON: %w", v.ID, err)
		}
		v.Payload = payload
		v.CreatedAt = v.CreatedAt.UTC()
		m.ObjectVersions = append(m.ObjectVersions, v)
	}
	for _, v := range snap.RelationVersions {
		payload, err := CanonicalJSON(v.Payload)
		if err != nil {
			return nil, fmt.Errorf("manifest: relation version %s payload is not valid JSON: %w", v.ID, err)
		}
		v.Payload = payload
		v.CreatedAt = v.CreatedAt.UTC()
		m.RelationVersions = append(m.RelationVersions, v)
	}
	m.BlobRefs = append(m.BlobRefs, snap.BlobRefs...)

	// Stable ordering: the manifest's arrays sort by their canonical keys
	// regardless of the order the store returned the rows in.
	sort.Slice(m.ObjectVersions, func(i, j int) bool {
		a, b := m.ObjectVersions[i], m.ObjectVersions[j]
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		if a.VersionNo != b.VersionNo {
			return a.VersionNo < b.VersionNo
		}
		return a.ID < b.ID
	})
	sort.Slice(m.RelationVersions, func(i, j int) bool {
		a, b := m.RelationVersions[i], m.RelationVersions[j]
		if a.RelationID != b.RelationID {
			return a.RelationID < b.RelationID
		}
		if a.VersionNo != b.VersionNo {
			return a.VersionNo < b.VersionNo
		}
		return a.ID < b.ID
	})
	sort.Slice(m.BlobRefs, func(i, j int) bool { return m.BlobRefs[i].ID < m.BlobRefs[j].ID })
	m.SchemaRefs = deriveSchemaRefs(m.ObjectVersions)
	m.PolicyRefs = derivePolicyRefs(m.ObjectVersions)

	// GitRef is the state's own recorded GitProvider commit
	// (project_states.git_commit_sha, set by the git-compat path); null when
	// the state has none. Only the state row contributes — the project's
	// repository id is mutable project data and is deliberately not part of
	// the manifest (purity invariant: the hash never depends on data outside
	// the state's recorded content).
	if state.GitCommitSHA != nil && *state.GitCommitSHA != "" {
		m.GitRef = state.GitCommitSHA
	}

	digest, err := m.digest()
	if err != nil {
		return nil, err
	}
	m.StateHash = digest
	return m, nil
}

// digest derives the state_hash from the manifest's content.
func (m *Manifest) digest() (string, error) {
	b, err := m.ContentCanonicalJSON()
	if err != nil {
		return "", fmt.Errorf("manifest: canonical content: %w", err)
	}
	return Digest(b), nil
}

// ContentCanonicalJSON renders the hash input: the manifest's semantic
// content as canonical JSON, without generated_at and state_hash. Verifiers
// re-derive the state_hash by digesting these bytes.
func (m *Manifest) ContentCanonicalJSON() ([]byte, error) {
	c := content{
		FormatVersion:    m.FormatVersion,
		ProjectID:        m.ProjectID,
		StateID:          m.StateID,
		ObjectVersions:   m.ObjectVersions,
		RelationVersions: m.RelationVersions,
		SchemaRefs:       m.SchemaRefs,
		PolicyRefs:       m.PolicyRefs,
		BlobRefs:         m.BlobRefs,
		GitRef:           m.GitRef,
	}
	return json.Marshal(c)
}

// CanonicalJSON renders the full manifest document in its canonical form.
func (m *Manifest) CanonicalJSON() ([]byte, error) {
	return json.Marshal(m)
}

// VerifyHash reports whether StateHash is the digest of the manifest's
// current content — the self-check a consumer runs before trusting an
// imported manifest.
func (m *Manifest) VerifyHash() bool {
	digest, err := m.digest()
	if err != nil {
		return false
	}
	return m.StateHash == digest
}

// Digest renders a content digest in the manifest's hash spelling:
// "<algorithm>:<hex>", so the algorithm is recorded with the digest
// (docs/21 §10).
func Digest(content []byte) string {
	sum := sha256.Sum256(content)
	return HashAlgorithm + ":" + hex.EncodeToString(sum[:])
}

// CanonicalJSON re-encodes stored JSON (a jsonb payload's text form) into
// canonical form: object keys sorted, no insignificant whitespace, numbers
// preserved exactly. jsonb already normalizes keys and whitespace on
// insert, but its key order is first-write order — two rows holding the
// same object in different key orders hash differently unless the payload
// is re-encoded through the canonical rule here.
//
// The input must be exactly one JSON value: trailing content after the
// first value (a second value or garbage) is rejected — this function feeds
// the state hash, and silently dropping bytes would let two different
// stored payloads hash alike.
func CanonicalJSON(raw []byte) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON value after the first value")
		}
		return nil, fmt.Errorf("trailing content after JSON value: %w", err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// deriveSchemaRefs lists the unique schema references of the object
// versions as sorted "id@version" strings.
func deriveSchemaRefs(objs []ObjectVersion) []string {
	seen := make(map[string]bool, len(objs))
	refs := make([]string, 0, len(objs))
	for _, o := range objs {
		ref := o.SchemaRef.ID + "@" + o.SchemaRef.Version
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	return refs
}

// derivePolicyRefs lists the unique visibility policy ids pinned by the
// object versions, sorted.
func derivePolicyRefs(objs []ObjectVersion) []string {
	seen := make(map[string]bool, len(objs))
	refs := make([]string, 0, len(objs))
	for _, o := range objs {
		if o.VisibilityPolicyID == nil {
			continue
		}
		if !seen[*o.VisibilityPolicyID] {
			seen[*o.VisibilityPolicyID] = true
			refs = append(refs, *o.VisibilityPolicyID)
		}
	}
	sort.Strings(refs)
	return refs
}
