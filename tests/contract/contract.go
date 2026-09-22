package contract

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ContractOp is one operation the API contract declares: an HTTP method plus a
// path, with the server prefix already joined on.
//
// The join is the whole reason this type exists. specs/api/openapi.yaml spells
// its paths RELATIVE to servers.url (/api/v1); the tree registers ABSOLUTE
// patterns. Compare the two without joining and the gate compares spellings
// rather than interfaces — every route would look undocumented and every
// contract entry unmounted, which is a gate that is red for a reason that is
// not about the product.
type ContractOp struct {
	Method string `json:"method"`
	// Path is absolute, e.g. /api/v1/projects/{projectId}/main:freeze.
	Path string `json:"path"`
	// Relative is the path as the contract spells it, kept so a reader can
	// check the join rather than trust it.
	Relative string `json:"relative"`
	Summary  string `json:"summary,omitempty"`
}

// Contract is a parsed specs/api/openapi.yaml, reduced to what the gate reads.
type Contract struct {
	// ServerPrefix is servers[0].url, e.g. /api/v1.
	ServerPrefix string
	Ops          []ContractOp
	// Paths is the number of entries in `paths`, for reconciling this tool's
	// reading against a hand count.
	Paths int
}

// httpMethods are the keys of an OpenAPI path item that are operations.
var httpMethods = map[string]string{
	"get": "GET", "put": "PUT", "post": "POST", "delete": "DELETE",
	"options": "OPTIONS", "head": "HEAD", "patch": "PATCH", "trace": "TRACE",
}

// ReadContract parses an OpenAPI document. Only servers[0].url, the keys of
// `paths` and the operation keys under each are read; everything else in the
// document is the contract's business, not the gate's.
func ReadContract(path string) (*Contract, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var doc struct {
		Servers []struct {
			URL string `yaml:"url"`
		} `yaml:"servers"`
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(doc.Servers) == 0 || doc.Servers[0].URL == "" {
		return nil, fmt.Errorf("%s declares no servers[0].url: without it a relative path cannot be joined to an absolute mux pattern, and comparing the two spellings is not a comparison", path)
	}
	prefix := strings.TrimSuffix(doc.Servers[0].URL, "/")
	out := &Contract{ServerPrefix: prefix, Paths: len(doc.Paths)}
	for rel, item := range doc.Paths {
		methods := make([]string, 0, len(item))
		for key := range item {
			if m, ok := httpMethods[strings.ToLower(key)]; ok {
				methods = append(methods, m)
			}
		}
		sort.Strings(methods)
		for _, m := range methods {
			out.Ops = append(out.Ops, ContractOp{
				Method:   m,
				Path:     prefix + rel,
				Relative: rel,
				Summary:  summaryOf(item),
			})
		}
	}
	sort.Slice(out.Ops, func(i, j int) bool {
		if out.Ops[i].Path != out.Ops[j].Path {
			return out.Ops[i].Path < out.Ops[j].Path
		}
		return out.Ops[i].Method < out.Ops[j].Method
	})
	return out, nil
}

// summaryOf returns the first operation summary in a path item, chosen by
// sorted method key so the value does not depend on map iteration order.
func summaryOf(item map[string]any) string {
	keys := make([]string, 0, len(item))
	for key := range item {
		if _, isOp := httpMethods[strings.ToLower(key)]; isOp {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	op, ok := item[keys[0]].(map[string]any)
	if !ok {
		return ""
	}
	s, _ := op["summary"].(string)
	return s
}
