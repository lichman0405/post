package backupdr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"time"
)

// The drill's recorded forms: what a backup claims to hold, what a restore
// claims to have produced, and what a reconciliation found.
//
// Every one of these is content-addressed the way the rest of the platform
// content-addresses a document — sha256 over the canonical JSON of the
// value, the convention internal/assets states (Manifest.Hash,
// internal/assets/manifest.go) and the release manifest repeats
// (internal/application/releases/manifest.go). Canonical here means:
// struct field order (Go marshals struct fields in declaration order, and
// encoding/json sorts map keys), so a re-marshal of a parsed manifest
// reproduces the bytes the digest was taken over. VerifyHash is the
// reading half and is what makes a tampered manifest artifact detectable
// rather than merely different.

// FormatVersion is the drill format this package writes. It records the
// digest convention as well as the document shape, the same way
// releases.FormatV1 does.
const FormatVersion = "backupdr-v1"

// maxObjectBytes bounds one blob the drill will read or write. A drill
// target is a test environment; the bound exists so a misconfigured
// endpoint holding something enormous fails loudly instead of exhausting
// the machine.
const maxObjectBytes = 64 << 20 // 64 MiB

// Artifact layout inside a backup directory. Every path the drill writes
// is one of these, so a backup directory is self-describing.
const (
	fileManifest    = "manifest.json"
	fileSchemaSQL   = "postgres/schema.sql"
	fileConfigMeta  = "config-metadata.json"
	dirTableData    = "postgres/data"
	fileSequences   = "postgres/data/_sequences.sql"
	dirBlobs        = "blobs"
	dirGitMirrors   = "git"
	fileRestore     = "restore.json"
	fileReport      = "report.json"
	fileDumpNoteTxt = "postgres/README.txt"
)

// sha256Hex is the digest convention shared with internal/assets
// (sha256Hex, internal/assets/manifest.go) and internal/rsg/manifest: a
// lowercase hex sha256 of the exact bytes, no prefix and no truncation.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonicalJSON renders a value to the bytes its digest is taken over.
// Struct field order is the declaration order; map keys are sorted by
// encoding/json. HTML escaping is off so a value containing "&" or "<"
// hashes to the same digest on every path (encoding/json's default
// escaping would otherwise make the digest depend on which side rendered
// it).
func canonicalJSON(v any) ([]byte, error) {
	var buf jsonBuffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	out := buf.bytes()
	// Encode appends a newline; drop it so the digest is over the document
	// and not over the document plus a terminator.
	for len(out) > 0 && (out[len(out)-1] == '\n' || out[len(out)-1] == '\r') {
		out = out[:len(out)-1]
	}
	return out, nil
}

type jsonBuffer struct{ b []byte }

func (j *jsonBuffer) Write(p []byte) (int, error) { j.b = append(j.b, p...); return len(p), nil }
func (j *jsonBuffer) bytes() []byte               { return j.b }

// Env is one environment's coordinates: where its data lives. The drill
// holds two of these — the source it backs up and the target it restores
// into — and the whole point of the "empty environment" requirement is
// that the target shares nothing with the source but the infrastructure
// the stack runs on.
type Env struct {
	// PostgresURL is the database the class covers. It is the URL the drill
	// CONNECTS with, so it carries the role's password; see MarshalJSON for
	// why the credential never reaches a document.
	PostgresURL string `json:"postgres_url_redacted"`
	// BlobEndpoint/Bucket name the S3-compatible store the blobs live in.
	BlobEndpoint string `json:"blob_endpoint"`
	BlobBucket   string `json:"blob_bucket"`
	// GiteaBaseURL is the Git infrastructure the repositories live in.
	GiteaBaseURL string `json:"gitea_base_url"`
	// GiteaOwner is the organization the drill's repositories are under.
	// The drill only ever touches repositories it created in this owner.
	GiteaOwner string `json:"gitea_owner"`
}

// envDocument is the wire shape of Env: the same fields, without the
// MarshalJSON method (which would otherwise recurse).
type envDocument struct {
	PostgresURL  string `json:"postgres_url_redacted"`
	BlobEndpoint string `json:"blob_endpoint"`
	BlobBucket   string `json:"blob_bucket"`
	GiteaBaseURL string `json:"gitea_base_url"`
	GiteaOwner   string `json:"gitea_owner"`
}

// MarshalJSON renders an environment with the database credential removed.
//
// The redaction lives HERE, at the one point where an Env becomes a
// document, rather than at the call site. A backup manifest and a drill
// report are artifacts that get copied, mailed and archived, and a caller
// who had to remember to pre-redact would eventually not — the URL a caller
// holds has to be the working one, because the same value is what pg_dump
// and pgx connect with. So the field is honest for use and redacted for
// record, and the artifact scan checks the result rather than trusting it.
//
// The field name still says `redacted`, so a reader of the artifact knows
// the value they are looking at cannot be connected with.
func (e Env) MarshalJSON() ([]byte, error) {
	return json.Marshal(envDocument{
		PostgresURL:  redactURLCredential(e.PostgresURL),
		BlobEndpoint: e.BlobEndpoint,
		BlobBucket:   e.BlobBucket,
		GiteaBaseURL: e.GiteaBaseURL,
		GiteaOwner:   e.GiteaOwner,
	})
}

// redactURLCredential removes the userinfo from a URL, keeping everything
// else. A URL that does not parse is returned as the empty string rather
// than as itself: an unparseable URL is exactly the case where a credential
// could be hiding in a shape this function does not recognise, and a
// document that says "there was a URL" is better than one that says what it
// was.
func redactURLCredential(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.User != nil {
		u.User = url.User(u.User.Username())
	}
	return u.String()
}

// DumpRecord is the Postgres class's record. The dump is a logical one —
// the schema verbatim, and per-table data with the credential-bearing
// columns redacted — so the record names both halves and the redaction, and
// a reader can tell exactly what the artifact does and does not hold.
type DumpRecord struct {
	SchemaFile      string        `json:"schema_file"`
	SchemaSHA256    string        `json:"schema_sha256"`
	DataDir         string        `json:"data_dir"`
	TableCount      int           `json:"table_count"`
	RowCount        int           `json:"row_count"`
	Tables          []TableRecord `json:"tables"`
	RedactedColumns []Redaction   `json:"redacted_columns"`
	// SequenceFile is the part of the dump that is neither schema nor rows:
	// the sequence positions (pg_dump's `SELECT pg_catalog.setval(...)`
	// statements). It is recorded as its own artifact because a table
	// restored without its sequence's position collides on the next insert,
	// which is a restore that looks complete until the environment is used.
	SequenceFile   string `json:"sequence_file,omitempty"`
	SequenceSHA256 string `json:"sequence_sha256,omitempty"`
}

// TableRecord is one table's slice of the Postgres class.
type TableRecord struct {
	Name     string   `json:"name"`
	File     string   `json:"file"`
	Rows     int      `json:"rows"`
	SHA256   string   `json:"sha256"`
	Redacted []string `json:"redacted_columns"`
}

// Redaction names one credential-bearing column the backup does NOT carry
// the values of. Why it is excluded is quoted from the migration that
// introduced the column, because a redaction whose justification lives only
// in this package is a rule nobody can audit.
type Redaction struct {
	Table  string `json:"table"`
	Column string `json:"column"`
	Reason string `json:"reason"`
}

// BlobRecord is one object of the S3 blobs/manifests class. The artifact
// stores the object's bytes under its own content hash (ContentHash), so
// the backup's blob store is content-addressed too and a corrupt copy is
// detectable inside the backup, before any restore is attempted. The
// mapping back to the platform is StorageKey — the object key the blobs
// row names (docs/17 §3: blob identity is content hash + blob id, and the
// storage key is where the bytes live).
type BlobRecord struct {
	StorageKey  string `json:"storage_key"`
	ContentHash string `json:"content_hash"`
	SizeBytes   int64  `json:"size_bytes"`
	File        string `json:"file"`
}

// GitRef is one ref of one repository at backup time: the full ref name and
// the commit it pointed at. This is the Git half of the "DB ↔ Git refs"
// axis — the value the restored refs are compared against.
type GitRef struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
}

// GitRecord is one repository of the Gitea class: its coordinate, its
// mirror bundle inside the artifact, and every ref it had. The refs are
// recorded, not just the bundle, because the axis the document names
// compares refs — a bundle whose refs were never listed would only prove
// that some repository was copied.
type GitRecord struct {
	Coordinate string   `json:"coordinate"`
	MirrorPath string   `json:"mirror_path"`
	DefaultRef string   `json:"default_ref"`
	Refs       []GitRef `json:"refs"`
}

// ConfigEntry is one item of the critical secrets/config metadata class.
// It records that a configuration key EXISTS and where its value comes
// from — never the value. Source is the provenance a restore needs to know
// where to get the value again (an environment variable, the Gitea service
// account, the secret manager); the drill can name them all without
// learning any of them.
type ConfigEntry struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Scope  string `json:"scope"`
}

// ConfigMetadata is the fourth class: the metadata half of it, which per
// docs/37 §需要备份 is the whole of what this backup carries ("secret 本身
// 按 secret manager backup policy").
type ConfigMetadata struct {
	Entries []ConfigEntry `json:"entries"`
	// SecretValuesPolicy is quoted from docs/37 §需要备份 so the artifact
	// states the rule it was built under.
	SecretValuesPolicy string `json:"secret_values_policy"`
}

// BackupManifest is the artifact that names what a backup holds. It is
// written FIRST and rewritten as each class completes, and the restore
// refuses to run without it.
type BackupManifest struct {
	FormatVersion string `json:"format_version"`
	// SnapshotTimestamp is the instant the backup was taken (docs/37
	// §一致性: "备份需记录 snapshot timestamp"). It is one instant for the
	// whole backup, taken once at the start, not one per class: "restored
	// to which moment" has to name a single moment to be an answer.
	SnapshotTimestamp time.Time      `json:"snapshot_timestamp"`
	Source            Env            `json:"source"`
	Postgres          DumpRecord     `json:"postgres"`
	Blobs             []BlobRecord   `json:"blobs"`
	GitRepositories   []GitRecord    `json:"git_repositories"`
	ConfigMetadata    ConfigMetadata `json:"config_metadata"`
	ManifestHash      string         `json:"manifest_hash,omitempty"`
}

// hashInput renders the manifest's digest input: the document without
// manifest_hash (the digest itself), the same construction
// releases.content uses. Keep the field list in sync with BackupManifest
// minus the one omitted — TestManifestHashMirrorsDocument pins that.
type backupHashInput struct {
	FormatVersion     string         `json:"format_version"`
	SnapshotTimestamp time.Time      `json:"snapshot_timestamp"`
	Source            Env            `json:"source"`
	Postgres          DumpRecord     `json:"postgres"`
	Blobs             []BlobRecord   `json:"blobs"`
	GitRepositories   []GitRecord    `json:"git_repositories"`
	ConfigMetadata    ConfigMetadata `json:"config_metadata"`
}

// hash derives the manifest digest over the hash input.
func (m BackupManifest) hash() (string, error) {
	raw, err := canonicalJSON(backupHashInput{
		FormatVersion:     m.FormatVersion,
		SnapshotTimestamp: m.SnapshotTimestamp.UTC(),
		Source:            m.Source,
		Postgres:          m.Postgres,
		Blobs:             m.Blobs,
		GitRepositories:   m.GitRepositories,
		ConfigMetadata:    m.ConfigMetadata,
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

// Seal computes and sets ManifestHash. Called once every class has landed.
func (m *BackupManifest) Seal() error {
	h, err := m.hash()
	if err != nil {
		return err
	}
	m.ManifestHash = h
	return nil
}

// VerifyHash reports whether ManifestHash is the digest of this manifest's
// content. A manifest artifact that fails this is not a slightly different
// backup, it is one that was altered after it was sealed.
func (m BackupManifest) VerifyHash() bool {
	if m.ManifestHash == "" {
		return false
	}
	h, err := m.hash()
	if err != nil {
		return false
	}
	return h == m.ManifestHash
}

// ParseBackupManifest reads and verifies a stored manifest document.
func ParseBackupManifest(raw []byte) (*BackupManifest, error) {
	var m BackupManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("backup manifest: parse: %w", err)
	}
	if m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("backup manifest: format_version %q, want %q", m.FormatVersion, FormatVersion)
	}
	if m.SnapshotTimestamp.IsZero() {
		return nil, fmt.Errorf("backup manifest: no snapshot timestamp (docs/37 §一致性 requires one)")
	}
	if !m.VerifyHash() {
		return nil, fmt.Errorf("backup manifest: manifest_hash does not cover the document (tampered or truncated)")
	}
	return &m, nil
}

// Rebinding records one environment coordinate the restore had to point at
// new infrastructure. A shared dev stack cannot host the source's
// organization a second time, so the drill creates its own and says so here
// rather than leaving a reader to guess why the coordinates differ. What is
// rebound is an ADDRESS (which owner, which bucket); what is deliberately
// not rebound is anything the axes compare — ref names and commit SHAs,
// blob content hashes, release manifest digests all come through unchanged.
type Rebinding struct {
	Class string `json:"class"`
	From  string `json:"from"`
	To    string `json:"to"`
	Why   string `json:"why"`
}

// RestoreManifest records what the restore produced: which empty target it
// filled, from which backup instant, and every coordinate it rebound on the
// way. Its SnapshotTimestamp is the backup's, carried forward — this is the
// document that makes "restored to which moment" answerable.
type RestoreManifest struct {
	FormatVersion     string    `json:"format_version"`
	SnapshotTimestamp time.Time `json:"snapshot_timestamp"`
	Target            Env       `json:"target"`
	// Emptiness is the evidence that the target really was empty before
	// the restore wrote to it. The drill refuses to restore into a target
	// it could not prove empty, because "restored into an empty
	// environment" is the acceptance criterion and a target with residual
	// data would satisfy the words while disproving the claim.
	Emptiness []EmptinessCheck `json:"emptiness_checks"`
	// Tables and Rows are counted IN THE TARGET after the load, not copied
	// from the backup manifest: they are what the restored environment
	// holds, measured with the same exact-count query the emptiness proof
	// ran against the same target before the restore.
	Tables       int         `json:"tables_restored"`
	Rows         int         `json:"rows_restored"`
	Blobs        int         `json:"blobs_restored"`
	GitRepos     int         `json:"git_repositories_restored"`
	Rebindings   []Rebinding `json:"rebindings"`
	ManifestHash string      `json:"manifest_hash,omitempty"`
}

// EmptinessCheck is one proof that a target coordinate held no data.
type EmptinessCheck struct {
	Class    string `json:"class"`
	Subject  string `json:"subject"`
	Observed string `json:"observed"`
	Empty    bool   `json:"empty"`
}

type restoreHashInput struct {
	FormatVersion     string           `json:"format_version"`
	SnapshotTimestamp time.Time        `json:"snapshot_timestamp"`
	Target            Env              `json:"target"`
	Emptiness         []EmptinessCheck `json:"emptiness_checks"`
	Tables            int              `json:"tables_restored"`
	Rows              int              `json:"rows_restored"`
	Blobs             int              `json:"blobs_restored"`
	GitRepos          int              `json:"git_repositories_restored"`
	Rebindings        []Rebinding      `json:"rebindings"`
}

func (r RestoreManifest) hash() (string, error) {
	raw, err := canonicalJSON(restoreHashInput{
		FormatVersion:     r.FormatVersion,
		SnapshotTimestamp: r.SnapshotTimestamp.UTC(),
		Target:            r.Target,
		Emptiness:         r.Emptiness,
		Tables:            r.Tables,
		Rows:              r.Rows,
		Blobs:             r.Blobs,
		GitRepos:          r.GitRepos,
		Rebindings:        r.Rebindings,
	})
	if err != nil {
		return "", err
	}
	return sha256Hex(raw), nil
}

// Seal computes and sets ManifestHash.
func (r *RestoreManifest) Seal() error {
	h, err := r.hash()
	if err != nil {
		return err
	}
	r.ManifestHash = h
	return nil
}

// VerifyHash reports whether ManifestHash covers this document.
func (r RestoreManifest) VerifyHash() bool {
	if r.ManifestHash == "" {
		return false
	}
	h, err := r.hash()
	if err != nil {
		return false
	}
	return h == r.ManifestHash
}

// ParseRestoreManifest reads and verifies a stored restore document.
func ParseRestoreManifest(raw []byte) (*RestoreManifest, error) {
	var r RestoreManifest
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("restore manifest: parse: %w", err)
	}
	if r.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("restore manifest: format_version %q, want %q", r.FormatVersion, FormatVersion)
	}
	if r.SnapshotTimestamp.IsZero() {
		return nil, fmt.Errorf("restore manifest: no snapshot timestamp (the restored-to instant is unrecorded)")
	}
	if !r.VerifyHash() {
		return nil, fmt.Errorf("restore manifest: manifest_hash does not cover the document")
	}
	return &r, nil
}
