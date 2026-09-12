package config

import (
	"strconv"
	"strings"
)

// Problem is one configuration failure: the offending key, what is wrong and
// what to do. Secrets are never part of a Problem.
type Problem struct {
	Key string // environment variable (or POST_ENV for layer problems)
	Msg string // what is wrong
	Fix string // what to do
}

// ConfigError is the aggregate of every configuration problem found in one
// Load call. Failing fast with all problems at once beats one-error-per-run.
type ConfigError struct {
	Problems []Problem
}

func (e *ConfigError) Error() string {
	if e == nil || len(e.Problems) == 0 {
		return "config: unknown error"
	}
	if len(e.Problems) == 1 {
		p := e.Problems[0]
		return "config: " + p.Msg + "; " + p.Fix
	}
	var b strings.Builder
	b.WriteString("config: ")
	b.WriteString(strconv.Itoa(len(e.Problems)))
	b.WriteString(" problems:\n")
	for _, p := range e.Problems {
		b.WriteString("  - ")
		b.WriteString(p.Key)
		b.WriteString(": ")
		b.WriteString(p.Msg)
		b.WriteString("; ")
		b.WriteString(p.Fix)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
