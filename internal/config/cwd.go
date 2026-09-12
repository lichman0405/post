package config

import (
	"os"
	"path/filepath"
	"strings"
)

// LoadFromCwd loads the validated configuration from the process environment
// plus an optional .env.<layer> layer file in the working directory. It is
// the single entry point every cmd/ binary uses, so no Go process can start
// on a guessed configuration.
//
// The layer-file contract matches the Python adapter and the web loader:
// a file is only loaded when POST_ENV is set and the file is named
// .env.<layer> for that layer. A file for another layer is ignored here —
// Load refuses cross-layer fallback outright. Process-environment values
// override file values inside Load.
func LoadFromCwd() (*Config, error) {
	layer := strings.TrimSpace(os.Getenv(EnvLayer))
	if layer == "" {
		// No layer, so no layer file can be selected. Load still reports the
		// missing POST_ENV problem; passing no files is the only correct call.
		return (Loader{}).Load()
	}
	var files []string
	candidate := filepath.Join(".", ".env."+layer)
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		files = append(files, candidate)
	}
	return (Loader{}).Load(files...)
}
