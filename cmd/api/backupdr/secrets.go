package backupdr

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The critical secrets/config metadata class of the backup (docs/37
// §需要备份).
//
// The document's own parenthesis settles what this class is: "secret 本身按
// secret manager backup policy" — the VALUE of a secret is not this
// backup's business. So the class carries the metadata a restore needs to
// know what to put back and where from: which configuration keys exist,
// which of them carry a credential, and which provenance re-supplies each.
// That is also the only form in which the information is safe to keep: the
// set of key NAMES is not a credential, and it is exactly what an operator
// restoring an environment into new infrastructure has to be told.
//
// Nothing in this file ever reads a value. configKeys below is a table of
// names and provenances; the environment is consulted only for presence
// (os.LookupEnv's boolean), never for the value, and the returned entry
// cannot carry one.

// configKey is one declared configuration key of the dev/production
// environment. The names are the ones internal/config/config.go reads
// (its fieldSpec table), recorded here rather than reflected out of it
// because reflection over another package's unexported table would break
// the moment that table is refactored — and a backup that silently stops
// listing a key is worse than one that lists a name that moved.
type configKey struct {
	Key      string
	Kind     string // "secret" | "config"
	Source   string
	Scope    string
	Critical bool
}

// configKeys is the closed set of critical configuration the drill records
// the metadata of. Every secret-typed field of internal/config is here (the
// four Secret(...) assignments at config.go:365/391/393/410); the rest are
// the non-secret keys a restore has to repoint at new infrastructure.
var configKeys = []configKey{
	{"POST_DB_HOST", "config", "environment", "database", true},
	{"POST_DB_PORT", "config", "environment", "database", true},
	{"POST_DB_USER", "config", "environment", "database", true},
	{"POST_DB_PASSWORD", "secret", "secret manager (dev: .env / compose default)", "database", true},
	{"POST_DB_NAME", "config", "environment", "database", true},
	{"POST_DB_SSLMODE", "config", "environment", "database", true},
	{"POST_BLOB_ENDPOINT", "config", "environment", "blob", true},
	{"POST_BLOB_ACCESS_KEY", "secret", "secret manager (dev: .env / compose default)", "blob", true},
	{"POST_BLOB_SECRET_KEY", "secret", "secret manager (dev: .env / compose default)", "blob", true},
	{"POST_BLOB_BUCKET", "config", "environment", "blob", true},
	{"POST_BLOB_USE_TLS", "config", "environment", "blob", false},
	{"POST_GITEA_BASE_URL", "config", "environment", "git", true},
	{"POST_GITEA_TOKEN", "secret", "secret manager (dev: run-scoped token minted by the drill)", "git", true},
	{"POST_REDIS_ADDR", "config", "environment", "cache", false},
	{"POST_API_ADDR", "config", "environment", "server", false},
	{"POST_MCP_ADDR", "config", "environment", "server", false},
}

// secretValuesPolicy is quoted from docs/37 §需要备份 so the artifact states
// the rule it was built under rather than leaving a reader to infer it.
const secretValuesPolicy = "critical secrets/config metadata（secret 本身按 secret manager backup policy）"

// configMetadata builds the fourth class's record. Presence is read from
// the process environment — the environment the drill itself is running in,
// which is the dev stack's — and only presence: LookupEnv is used for its
// two-value form so the value is never bound to a name in this function.
func configMetadata() ConfigMetadata {
	entries := make([]ConfigEntry, 0, len(configKeys))
	for _, k := range configKeys {
		_, present := os.LookupEnv(k.Key)
		state := "absent"
		if present {
			state = "present"
		}
		entries = append(entries, ConfigEntry{
			Key: k.Key,
			// The kind and the scope are metadata; the presence is metadata.
			// The value is not read, so it cannot be recorded.
			Kind:   k.Kind,
			Source: k.Source,
			Scope:  k.Scope + ":" + state,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return ConfigMetadata{Entries: entries, SecretValuesPolicy: secretValuesPolicy}
}

// SecretHit is one artifact file that contains one of the declared secret
// values.
type SecretHit struct {
	File   string
	Secret string
	// Offset is where in the file the value starts, so a hit can be looked
	// at rather than merely believed.
	Offset int
}

// MinScannableSecretBytes is the shortest declared value the byte scan will
// look for. Below it a "secret" is a substring of ordinary text — an empty
// one matches every file at offset 0 — and the scan would report noise as a
// leak. Dev defaults are all well past this floor.
const MinScannableSecretBytes = 8

// scannableSecretValues splits declared credential values into the ones the
// byte scan really looks for and the number it has to skip as too short.
//
// The split is exposed rather than kept inside the scan because the report
// has to be able to say which set it covered: "searched N values" standing
// for a set that was quietly smaller would be a security-relevant number
// overstating what was actually checked.
func scannableSecretValues(secrets []string) (scanned []string, tooShort int) {
	scanned = make([]string, 0, len(secrets))
	for _, s := range secrets {
		if len(s) < MinScannableSecretBytes {
			tooShort++
			continue
		}
		scanned = append(scanned, s)
	}
	return scanned, tooShort
}

// ScanArtifactsForSecrets walks every file under dir and reports each file
// that contains any of the given secret values.
//
// This is the check that makes the backup's redaction a verified property
// instead of an intention. It is deliberately a plain byte search over
// every artifact — including the dumps, the manifests and the report — so
// a value that reached the artifact by ANY path (a column the redaction
// list forgot, a git remote URL, a config dump, an error message captured
// into the report) is found. The drill refuses to accept a backup whose
// scan is not empty.
//
// It returns hits rather than a bool because the caller's failure message
// has to name the file and the offset; a bare "a secret was found" would
// send an operator looking through a directory tree by hand. The secret
// value itself is never put in the message — only its position.
func ScanArtifactsForSecrets(dir string, secrets []string) ([]SecretHit, error) {
	wanted, _ := scannableSecretValues(secrets)
	var hits []SecretHit
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A git mirror's object store is compressed, so a value could
			// in principle be inside it; the mirrors are scanned as bytes
			// anyway, and .git/hooks holds sample scripts the drill never
			// writes secrets into. Skip nothing: scanning everything is the
			// point of the check.
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range wanted {
			if i := bytes.Index(raw, []byte(secret)); i >= 0 {
				rel, relErr := filepath.Rel(dir, path)
				if relErr != nil {
					rel = path
				}
				hits = append(hits, SecretHit{File: filepath.ToSlash(rel), Secret: secret, Offset: i})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan artifacts in %s: %w", dir, err)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].File != hits[j].File {
			return hits[i].File < hits[j].File
		}
		return hits[i].Offset < hits[j].Offset
	})
	return hits, nil
}

// describeHits renders a hit list for a failure message without quoting any
// secret: the file, the offset and the secret's declared NAME.
func describeHits(hits []SecretHit, names map[string]string) string {
	var parts []string
	seen := map[string]bool{}
	for _, h := range hits {
		name := names[h.Secret]
		if name == "" {
			name = "<undeclared>"
		}
		key := h.File + "|" + name
		if seen[key] {
			continue
		}
		seen[key] = true
		parts = append(parts, fmt.Sprintf("%s contains the value of %s (offset %d)", h.File, name, h.Offset))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
