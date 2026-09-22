package contract

import (
	"fmt"
	"os"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Exemption is one entry of specs/api/openapi-exemptions.yaml. The field names
// are the file's, verbatim (method/path/decision/reason/follow_up): this tool
// reads that file, it does not define its own copy, and a shape the Supervisor
// writes must be the shape the tool reads.
type Exemption struct {
	Method   string `yaml:"method" json:"method"`
	Path     string `yaml:"path" json:"path"`
	Decision string `yaml:"decision" json:"decision"`
	Reason   string `yaml:"reason" json:"reason"`
	FollowUp string `yaml:"follow_up" json:"follow_up,omitempty"`
}

// ExemptionList is a parsed exemption file.
type ExemptionList struct {
	Version    int         `yaml:"version"`
	Exemptions []Exemption `yaml:"exemptions"`
	// Path is where it was read from, carried so every report says which file
	// it consulted rather than leaving the reader to assume.
	Path string `yaml:"-" json:"-"`

	// The file's own prose rules are checked too, because the header states
	// them as rules and a tool that ignores the file it claims to read is
	// reading something else. Verb-suffix catch-alls belong to the contract,
	// not here: parking one hides a real endpoint behind a shape the contract
	// cannot express.
	DocSaysNoCatchAlls bool `yaml:"-"`
}

// ReadExemptions parses the exemption list. It returns an error only when the
// file cannot be read or parsed: an entry with an unresolvable citation is a
// FINDING (the gate's red), not a parse failure, because the file is written
// by a human and its defects belong in the report.
func ReadExemptions(path string) (*ExemptionList, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc ExemptionList
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	doc.Path = path
	sort.SliceStable(doc.Exemptions, func(i, j int) bool {
		if doc.Exemptions[i].Path != doc.Exemptions[j].Path {
			return doc.Exemptions[i].Path < doc.Exemptions[j].Path
		}
		return doc.Exemptions[i].Method < doc.Exemptions[j].Method
	})
	return &doc, nil
}

// IsCatchAll reports whether an entry's path is a Go catch-all (`{x...}`),
// which the exemption file's header forbids here. It is the same predicate the
// comparison uses, deliberately: the entry is judged by the shape that matters
// (the path's last segment), not by a second spelling of the rule that could
// drift from it.
func (e Exemption) IsCatchAll() bool { return IsCatchAll(e.Path) }
