package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// envKeyRe is the accepted KEY shape in a KEY=VALUE line.
var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ParseEnvFile reads a minimal KEY=VALUE environment file. The format is
// deliberately strict and boring:
//
//   - one KEY=VALUE per line; blank lines and lines whose first non-space
//     character is '#' are ignored (no inline comments — '#' is a legal
//     character inside a value);
//   - KEY must match [A-Za-z_][A-Za-z0-9_]*;
//   - an optional surrounding pair of matching single or double quotes is
//     stripped from the value (no escape processing inside quotes);
//   - trailing '\r' is stripped (CRLF files);
//   - a duplicated key keeps the last value.
//
// Anything else is an error naming the file and line: a broken layer file
// must fail loudly, never be half-applied.
func ParseEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseEnvBytes(string(data), path)
}

// ParseEnvBytes parses environment-file content; path is used only in error
// messages (pass "" to omit it).
func ParseEnvBytes(content, path string) (map[string]string, error) {
	values := make(map[string]string)
	for i, raw := range strings.Split(content, "\n") {
		lineNo := i + 1
		line := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			// Never echo the raw line. A line without '=' is often a
			// credential-bearing URL pasted on its own, and echoing it put
			// the password straight into the error message.
			return nil, envFileError(path, lineNo,
				fmt.Sprintf("not a KEY=VALUE line (line begins %q); "+
					"every non-comment line must be KEY=VALUE",
					truncateRedacted(trimmed)))
		}
		key = strings.TrimSpace(key)
		if !envKeyRe.MatchString(key) {
			return nil, envFileError(path, lineNo,
				fmt.Sprintf("invalid KEY %q (must match [A-Za-z_][A-Za-z0-9_]*)",
					truncateRedacted(key)))
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			first, last := value[0], value[len(value)-1]
			if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	return values, nil
}

func envFileError(path string, line int, msg string) error {
	where := "env file"
	if path != "" {
		where = path
	}
	return fmt.Errorf("%s:%d: %s", where, line, msg)
}
