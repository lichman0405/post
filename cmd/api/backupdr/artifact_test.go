package backupdr

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The artifact rules: what a backup refuses to write, what it refuses to
// write over, and what it refuses to be silent about.

// TestBackupManifestHashDetectsATamperedClass: the manifest is
// content-addressed, so a class edited inside the artifact after the seal
// no longer verifies. Without this, "which backup is this" would be answered
// by whatever the file happens to say.
func TestBackupManifestHashDetectsATamperedClass(t *testing.T) {
	m := BackupManifest{
		FormatVersion:     FormatVersion,
		SnapshotTimestamp: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC),
		Source:            Env{PostgresURL: "postgres://redacted", BlobBucket: "b", GiteaOwner: "o"},
		Postgres:          DumpRecord{TableCount: 2, RowCount: 10},
		Blobs:             []BlobRecord{{StorageKey: "k", ContentHash: strings.Repeat("a", 64), SizeBytes: 3, File: "blobs/x"}},
		GitRepositories:   []GitRecord{{Coordinate: "o/r", DefaultRef: "refs/heads/main", Refs: []GitRef{{Name: "refs/heads/main", SHA: "abc"}}}},
		ConfigMetadata:    ConfigMetadata{Entries: []ConfigEntry{{Key: "POST_DB_HOST", Kind: "config"}}},
	}
	if err := m.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !m.VerifyHash() {
		t.Fatal("a freshly sealed manifest does not verify")
	}

	// The four classes are each covered: changing any one of them breaks
	// the seal, so the digest is not silently over a subset.
	cases := map[string]func(*BackupManifest){
		"postgres":        func(m *BackupManifest) { m.Postgres.RowCount = 11 },
		"s3 blobs":        func(m *BackupManifest) { m.Blobs[0].SizeBytes = 4 },
		"gitea":           func(m *BackupManifest) { m.GitRepositories[0].Refs[0].SHA = "def" },
		"config metadata": func(m *BackupManifest) { m.ConfigMetadata.Entries[0].Key = "POST_DB_PASSWORD" },
		"snapshot instant": func(m *BackupManifest) {
			m.SnapshotTimestamp = m.SnapshotTimestamp.Add(time.Second)
		},
	}
	for name, breakIt := range cases {
		tampered := m
		breakIt(&tampered)
		if tampered.VerifyHash() {
			t.Fatalf("editing the %s class left the manifest verifying — the digest does not cover it", name)
		}
	}
}

// TestParseBackupManifestRefusesAnUnsealedOne: a restored environment whose
// backup instant cannot be established is not "restored to some moment",
// it is restored to an unknown one — and docs/37 §一致性 requires the
// snapshot timestamp, so a manifest without one is refused rather than
// defaulted.
func TestParseBackupManifestRefusesAnUnsealedOne(t *testing.T) {
	good := BackupManifest{
		FormatVersion:     FormatVersion,
		SnapshotTimestamp: time.Now().UTC().Truncate(time.Millisecond),
	}
	raw, err := canonicalJSON(good)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseBackupManifest(raw); err == nil {
		t.Fatal("a manifest with no hash was accepted")
	}

	if err := good.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	raw, err = canonicalJSON(good)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseBackupManifest(raw); err != nil {
		t.Fatalf("a sealed manifest was refused: %v", err)
	}

	// The instant is required, and its absence is named.
	noInstant := BackupManifest{FormatVersion: FormatVersion}
	if err := noInstant.Seal(); err != nil {
		t.Fatalf("seal: %v", err)
	}
	raw, err = canonicalJSON(noInstant)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseBackupManifest(raw); err == nil ||
		!strings.Contains(err.Error(), "snapshot timestamp") {
		t.Fatalf("a manifest with no snapshot timestamp was accepted or misreported: %v", err)
	}
}

// TestRedactionRefusesAColumnTheDumpDoesNotCarry is the control for the
// credential redaction. A redaction entry whose column is absent means the
// list and the schema have drifted apart, and the one thing the pass must
// never do then is carry on — the value it was told to remove would be
// written into the artifact unredacted.
func TestRedactionRefusesAColumnTheDumpDoesNotCarry(t *testing.T) {
	block := copyBlock{
		Table:   "users",
		Header:  "COPY public.users (id, email) FROM stdin;",
		Columns: []string{"id", "email"},
		Rows:    [][]string{{"1", "a@example.com"}},
	}
	err := block.redact([]Redaction{{Table: "users", Column: "password_hash", Reason: "test"}})
	if err == nil {
		t.Fatal("a redaction naming a column the dump does not carry was silently skipped — the value would have been written")
	}
	if !strings.Contains(err.Error(), "password_hash") {
		t.Fatalf("the refusal does not name the column: %v", err)
	}
}

// TestRedactionReplacesTheValueAndKeepsTheRest is the other half: the
// column IS replaced, every other field is byte-identical, and the row
// count is untouched. A redaction that dropped rows or shifted columns
// would produce a dump that restores wrongly in a way nothing else checks.
func TestRedactionReplacesTheValueAndKeepsTheRest(t *testing.T) {
	block := copyBlock{
		Table:   "users",
		Header:  "COPY public.users (id, email, password_hash, name) FROM stdin;",
		Columns: []string{"id", "email", "password_hash", "name"},
		Rows: [][]string{
			{"1", "a@example.com", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$aGFzaA", "Alice"},
			{"2", "b@example.com", "$argon2id$v=19$m=65536,t=3,p=4$c2FsdA$b3RoZXI", "Bob"},
		},
	}
	if err := block.redact([]Redaction{{Table: "users", Column: "password_hash", Reason: "test"}}); err != nil {
		t.Fatalf("redact: %v", err)
	}
	rendered := string(block.render())
	if strings.Contains(rendered, "argon2id") {
		t.Fatalf("the credential value survived the redaction:\n%s", rendered)
	}
	if strings.Count(rendered, RedactionPlaceholder) != 2 {
		t.Fatalf("want one placeholder per row:\n%s", rendered)
	}
	for _, want := range []string{"a@example.com", "b@example.com", "Alice", "Bob", "\\.\n"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("redaction also removed %q:\n%s", want, rendered)
		}
	}
}

// TestSplitCopyBlocksRefusesAnUnterminatedBlock: a COPY block that never
// ends would mean the rest of the dump was swallowed into it, and a
// redaction that ran over a truncated block would be redacting the wrong
// bytes. The parser refuses instead.
func TestSplitCopyBlocksRefusesAnUnterminatedBlock(t *testing.T) {
	_, err := splitCopyBlocks([]byte("COPY public.users (id) FROM stdin;\n1\n2\n"))
	if err == nil {
		t.Fatal("an unterminated COPY block was accepted")
	}
	if !strings.Contains(err.Error(), "never terminated") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

// TestSplitCopyBlocksReadsHeaderColumns: the parser must recover the exact
// column list, because the redaction addresses columns by index into it.
func TestSplitCopyBlocksReadsHeaderColumns(t *testing.T) {
	blocks, err := splitCopyBlocks([]byte(
		"COPY public.users (id, \"name\", email) FROM stdin;\n1\tA\tx@y\n\\.\n" +
			"COPY public.blobs (id, content_hash) FROM stdin;\n2\tabc\n\\.\n"))
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2", len(blocks))
	}
	if got := strings.Join(blocks[0].Columns, ","); got != "id,name,email" {
		t.Fatalf("columns = %q", got)
	}
	if blocks[0].Table != "users" || blocks[1].Table != "blobs" {
		t.Fatalf("tables = %q, %q", blocks[0].Table, blocks[1].Table)
	}
}

// TestScanArtifactsForSecretsFindsAPlantedValue is the control for the
// fourth class's whole acceptance criterion. Before the scan can be trusted
// to say "no credential value is in the artifact", it has to be shown
// capable of saying the opposite — so a value is planted and must be found,
// with the file and offset named and the value itself never echoed.
func TestScanArtifactsForSecretsFindsAPlantedValue(t *testing.T) {
	dir := t.TempDir()
	const planted = "minio_dev_pw-do-not-ship-this"
	if err := os.MkdirAll(filepath.Join(dir, "postgres", "data"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"clean":true}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	leak := filepath.Join(dir, "postgres", "data", "users.copy")
	if err := os.WriteFile(leak, []byte("COPY public.users (id, password_hash) FROM stdin;\n1\t"+planted+"\n\\.\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	hits, err := ScanArtifactsForSecrets(dir, []string{planted, "some-other-value-not-present"})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits, want 1: %v", len(hits), hits)
	}
	if hits[0].File != "postgres/data/users.copy" {
		t.Fatalf("hit file = %q, want the file that carries it", hits[0].File)
	}
	if hits[0].Offset <= 0 {
		t.Fatalf("hit offset = %d, want the position so it can be looked at", hits[0].Offset)
	}

	// The failure message names the file and the declared NAME, never the
	// value: a leak report that quotes the secret is a second leak.
	msg := describeHits(hits, map[string]string{planted: "POST_BLOB_SECRET_KEY"})
	if !strings.Contains(msg, "users.copy") || !strings.Contains(msg, "POST_BLOB_SECRET_KEY") {
		t.Fatalf("the report does not name the file and the key: %s", msg)
	}
	if strings.Contains(msg, planted) {
		t.Fatalf("the report quoted the credential value: %s", msg)
	}

	// And the clean case: with the leak gone the scan reports nothing, so
	// the assertion above is about the value and not about the directory.
	if err := os.Remove(leak); err != nil {
		t.Fatalf("remove: %v", err)
	}
	hits, err = ScanArtifactsForSecrets(dir, []string{planted})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the scan found a value that is not there: %v", hits)
	}
}

// TestConfigMetadataCarriesNoValue: the fourth class is metadata ONLY
// (docs/37 §需要备份). The check is on the rendered document, so a value
// added to the struct later is caught where it would actually leak —
// in the artifact.
func TestConfigMetadataCarriesNoValue(t *testing.T) {
	const probe = "POST_GITEA_TOKEN"
	os.Setenv(probe, "a-very-recognisable-dev-token-value")
	t.Cleanup(func() { os.Unsetenv(probe) })

	meta := configMetadata()
	raw, err := canonicalJSON(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "a-very-recognisable-dev-token-value") {
		t.Fatalf("the fourth class's document carries a credential value:\n%s", raw)
	}
	var found *ConfigEntry
	for i := range meta.Entries {
		if meta.Entries[i].Key == probe {
			found = &meta.Entries[i]
		}
	}
	if found == nil {
		t.Fatalf("%s is not recorded at all — a restore would not know it has to be re-supplied", probe)
	}
	if found.Kind != "secret" {
		t.Fatalf("%s is recorded as %q, want secret", probe, found.Kind)
	}
	if !strings.Contains(found.Scope, "present") {
		t.Fatalf("%s's presence is not recorded (%q) — the metadata a restore needs", probe, found.Scope)
	}
}

// TestEnsureArtifactsDirRefusesARepoPath: a restore drill writes dumps and
// blob copies. The one place they must never be is somewhere a `git add -A`
// would reach, so the drill refuses a location inside the repository that is
// not the ignored default.
func TestEnsureArtifactsDirRefusesARepoPath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "cmd"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := EnsureArtifactsDirOutsideRepo(filepath.Join(root, "cmd", "artifacts"), root); err == nil {
		t.Fatal("an artifact directory inside the repository tree was accepted")
	}
	if err := EnsureArtifactsDirOutsideRepo(root, root); err == nil {
		t.Fatal("the repository root itself was accepted as an artifact directory")
	}
	// The ignored default is the one location inside the tree that is
	// allowed, because .gitignore covers it.
	if err := EnsureArtifactsDirOutsideRepo(filepath.Join(root, DefaultArtifactsRelPath, "run-1"), root); err != nil {
		t.Fatalf("the ignored default was refused: %v", err)
	}
	// And outside the tree entirely, anything goes.
	outside := t.TempDir()
	if err := EnsureArtifactsDirOutsideRepo(filepath.Join(outside, "artifacts"), root); err != nil {
		t.Fatalf("a directory outside the repository was refused: %v", err)
	}

	// A RELATIVE path names the same location as its absolute spelling, so
	// it has to be refused the same way: the guard resolves it before
	// comparing. The shell wrapper around this guard once compared only
	// absolute paths, and `--artifacts ./cmd/api/artifacts` walked straight
	// past it; this pins the Go half so the two cannot drift apart again.
	t.Chdir(root)
	if err := EnsureArtifactsDirOutsideRepo(filepath.Join(".", "cmd", "artifacts"), root); err == nil {
		t.Fatal("a relative artifact path inside the repository was accepted")
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "artifacts")); err == nil {
		t.Fatal("the refused relative path was created anyway")
	}
}

// TestDrillReportCarriesNoRPOClaim: the run's own document must state that
// this round's acceptance is the process and that no recovery-time
// objective is claimed. docs/37 §RPO/RTO is explicit that V1 validates the
// process; a report that left the field out would let a reader infer
// otherwise.
func TestDrillReportCarriesNoRPOClaim(t *testing.T) {
	rep := DrillReport{FormatVersion: FormatVersion, RunID: "r", RoundAcceptance: RoundAcceptanceV1}
	raw, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := doc["rpo_claim"]
	if !ok {
		t.Fatal("the report has no rpo_claim field at all — its absence is indistinguishable from an omission")
	}
	if v != "" {
		t.Fatalf("rpo_claim = %v, want empty: this round claims no RPO", v)
	}
	if doc["repairs_applied"] != float64(0) {
		t.Fatalf("repairs_applied = %v, want 0", doc["repairs_applied"])
	}
	if !strings.Contains(RoundAcceptanceV1, "流程能跑通") {
		t.Fatalf("the round's acceptance quote lost the document's own words: %s", RoundAcceptanceV1)
	}
}

// TestDefaultArtifactDirIsActuallyGitignored pins the OTHER half of the rule
// the test above states.
//
// EnsureArtifactsDirOutsideRepo accepts exactly one location inside the
// working tree, and the reason it may is written in its own comment: the
// repository's .gitignore covers it. That reason was, until this test, a
// claim about a file nothing checked — the Go constant and the ignore entry
// are two halves of one rule in two different languages, and moving either
// one silently would make .backup-dr/ a directory a `git add -A` reaches,
// with the dump, the blob copies and the git mirror inside it.
//
// The probe is `git check-ignore`, asked twice:
//
//   - the default artifact directory must BE ignored;
//   - a tracked directory (cmd/) must NOT be, which is what makes the first
//     answer a measurement rather than a constant "yes".
func TestDefaultArtifactDirIsActuallyGitignored(t *testing.T) {
	root, err := gitRoot()
	if err != nil {
		// A source tree without git cannot answer the question, and a test
		// that "passed" by not asking it would be the exact green light this
		// package exists to refuse.
		t.Skipf("backupdr: not a git working tree (%v) — this test asks git whether .gitignore "+
			"covers %s, and without git there is no .gitignore to ask about", err, DefaultArtifactsRelPath)
	}

	ignored := func(rel string) bool {
		t.Helper()
		cmd := exec.Command("git", "-C", root, "check-ignore", "-q", "--", rel)
		err := cmd.Run()
		var exitErr *exec.ExitError
		switch {
		case err == nil:
			return true
		case errors.As(err, &exitErr) && exitErr.ExitCode() == 1:
			return false
		default:
			t.Fatalf("git check-ignore %s: %v", rel, err)
		}
		return false
	}

	// The negative control first, so a failure of the positive check cannot
	// be mistaken for "check-ignore matched everything".
	if ignored("cmd") {
		t.Fatal("git says cmd/ is ignored — check-ignore is not discriminating, so the check below proves nothing")
	}
	probe := DefaultArtifactsRelPath + "/drill-probe"
	if !ignored(probe) {
		t.Fatalf("%s is NOT gitignored, but EnsureArtifactsDirOutsideRepo accepts it inside the working tree "+
			"on the grounds that it is — a backup dump, blob copies and a git mirror are one `git add -A` "+
			"from a commit. Add %s/ to .gitignore, or change DefaultArtifactsRelPath and the ignore entry together.",
			probe, DefaultArtifactsRelPath)
	}
}

// gitRoot returns the working tree's root, or an error when there is not one.
func gitRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TestManifestHashMirrorsDocument pins the one place a backup manifest's
// digest can silently stop covering something.
//
// The digest is taken over a sibling struct — the document minus
// manifest_hash, because a digest cannot cover itself — and that
// construction has a failure mode nothing else catches: a field added to
// the document and forgotten in the digest input is a field an edit could
// change WITHOUT moving the digest, which is precisely what
// TestBackupManifestHashDetectsATamperedClass assumes cannot happen. The
// two field lists are compared here by name, so the omission fails at test
// time instead of at the point somebody is relying on the artifact's seal.
func TestManifestHashMirrorsDocument(t *testing.T) {
	cases := []struct {
		name    string
		doc     any
		input   any
		omitted string
	}{
		{"BackupManifest", BackupManifest{}, backupHashInput{}, "manifest_hash"},
		{"RestoreManifest", RestoreManifest{}, restoreHashInput{}, "manifest_hash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := jsonFieldNames(tc.doc)
			input := jsonFieldNames(tc.input)
			if !doc[tc.omitted] {
				t.Fatalf("the document has no %s field, so the digest does not omit the digest "+
					"(the comparison below would be vacuous)", tc.omitted)
			}
			delete(doc, tc.omitted)
			for name := range doc {
				if !input[name] {
					t.Fatalf("%s is in the document but NOT in the digest input: it can be changed "+
						"without moving the digest, and the manifest's seal would not notice", name)
				}
			}
			for name := range input {
				if !doc[name] {
					t.Fatalf("%s is in the digest input but NOT in the document: the digest covers "+
						"something a reader of the artifact cannot see, so it cannot be reproduced", name)
				}
			}
		})
	}
}

// jsonFieldNames returns the JSON names a struct serialises to.
func jsonFieldNames(v any) map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		name := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}
