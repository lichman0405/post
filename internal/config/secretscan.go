package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The secret scan is the T0004 gate for "no secret material in committed
// example files". It is a real check with a demonstrated failing mode: the
// detector is exercised against planted positive fixtures in
// secretscan_test.go, and the repository sweep below runs on every
// `go test ./...` (and therefore `make check`).

// deniedDevCredentials are the dev-only credential values the local infra
// stack (docker-compose.yml, infra/docker) uses. They are real committed
// secrets for the *dev* stack, so they must never be copied verbatim into an
// .env.example: an example file may document the shape, not the value.
var deniedDevCredentials = []string{
	"postgres_dev_pw", "gitea_dev_pw", "minio_dev_pw", "postadmin_dev_pw",
}

// secretKeyRe matches assignment keys that name a secret.
var secretKeyRe = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key)`)

// placeholderValues are the only "values" an example file may pair with a
// secret-shaped key. Anything else on the right-hand side is a finding.
var placeholderValues = []string{
	"", "change-me", "change_me", "changeme", "replace-me", "replace_me",
	"example", "placeholder", "your-password", "your_password", "your-token",
	"your_token", "your-secret", "your_secret", "your-api-key", "your_api_key",
	"xxx", "***", "****", "<change-me>", "<secret>", "<token>", "<password>",
}

// urlCredRe matches userinfo with a concrete password embedded in a URL.
var urlCredRe = regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`)

// Finding is one secret-shaped value located in a scanned file.
type Finding struct {
	File string
	Line int
	What string // what matched, without the secret value itself
}

func (f Finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.File, f.Line, f.What)
}

// ScanExampleContent inspects one example-file content (the committed
// .env.example form). The secret value itself is never part of the finding.
func ScanExampleContent(content, path string) []Finding {
	var findings []Finding
	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSpace(raw)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// 1. Credential-bearing URLs (scheme://user:password@host).
		if loc := urlCredRe.FindStringIndex(line); loc != nil {
			findings = append(findings, Finding{
				File: path, Line: lineNo,
				What: "credential-bearing URL in example file",
			})
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)

		if !secretKeyRe.MatchString(key) {
			continue
		}
		// 2. Denied dev credentials: never copy the real dev value. The
		// finding names the key but not the denied value — findings are
		// output, and no secret may appear in any output.
		for _, denied := range deniedDevCredentials {
			if value == denied {
				findings = append(findings, Finding{
					File: path, Line: lineNo,
					What: fmt.Sprintf("real dev credential (deny-list) assigned to %s in an example file", key),
				})
			}
		}
		// 3. Any concrete value that is not an allowed placeholder.
		allowed := false
		for _, ph := range placeholderValues {
			if strings.EqualFold(value, ph) {
				allowed = true
				break
			}
		}
		if !allowed && len(value) >= 4 {
			findings = append(findings, Finding{
				File: path, Line: lineNo,
				What: fmt.Sprintf("secret-shaped value for %s (not a documented placeholder)", key),
			})
		}
	}
	return findings
}

// ScanRepoExampleFiles sweeps the repository for committed example files
// (*.env.example) and returns every secret-shaped value found in them.
func ScanRepoExampleFiles(root string) ([]Finding, error) {
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".next", ".venv", "__pycache__", "out":
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Base(path) != ".env.example" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		findings = append(findings, ScanExampleContent(string(data), path)...)
		return nil
	})
	return findings, err
}
