package schemareg

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Schema is one immutable registered schema version. Obtain it via Get or
// Lookup; it is never mutated after registration.
type Schema struct {
	Ref

	// ContentHash is the sha256 hex digest of the exact schema document
	// bytes that were registered (docs/21 §10). It pins the validating
	// content: two registrations with equal id+version but different hashes
	// can never both exist.
	ContentHash string

	raw      []byte
	compiled *jsonschema.Schema
}

// Validate checks doc against this schema. It returns nil when the document
// is valid, *ValidationError when the document violates the schema, and a
// plain error when doc is not valid JSON.
func (s *Schema) Validate(doc []byte) error {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return fmt.Errorf("schemareg: document is not valid JSON: %w", err)
	}
	if err := s.compiled.Validate(v); err != nil {
		return &ValidationError{Ref: s.Ref, err: err}
	}
	return nil
}

// Raw returns a copy of the exact schema document bytes this version was
// registered with (the bytes ContentHash digests). It is how a consumer can
// derive an extension from a registered base schema without re-reading the
// source file — T0213's profile generation reads the base's property
// definitions from here.
func (s *Schema) Raw() []byte {
	out := make([]byte, len(s.raw))
	copy(out, s.raw)
	return out
}

// TypeConst returns the object type this schema governs: the const value
// of its properties.type, when the schema declares one ("" and false
// otherwise). It is how a version row alone can be assembled into its
// entity document: the schema that governs the payload is also the
// authority for the type the payload is of — a payload can never smuggle
// a different type past its own schema.
func (s *Schema) TypeConst() (string, bool) {
	var doc struct {
		Properties struct {
			Type struct {
				Const string `json:"const"`
			} `json:"type"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(s.raw, &doc); err != nil {
		return "", false
	}
	if doc.Properties.Type.Const == "" {
		return "", false
	}
	return doc.Properties.Type.Const, true
}

// compileSchema decodes and compiles doc as a JSON Schema registered at ref.
// resources holds the already-registered schema documents keyed by id; $refs
// in doc resolve against them locally (never over the network). Format
// assertions are enabled so the schemas' format keywords (notably
// "date-time") are enforced rather than annotation-only.
func compileSchema(ref Ref, doc []byte, resources map[string][]byte) (*Schema, error) {
	var v any
	if err := json.Unmarshal(doc, &v); err != nil {
		return nil, fmt.Errorf("schemareg: schema document is not valid JSON: %w", err)
	}
	if id := docID(doc); id != "" && id != ref.ID {
		return nil, fmt.Errorf("schemareg: schema document $id %q does not match the registered id %q", id, ref.ID)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.AssertFormat()
	for url, raw := range resources {
		if url == ref.ID {
			continue // the document being registered is added explicitly below
		}
		var rv any
		if err := json.Unmarshal(raw, &rv); err != nil {
			return nil, fmt.Errorf("schemareg: registered schema %q does not decode: %w", url, err)
		}
		if err := c.AddResource(url, rv); err != nil {
			return nil, fmt.Errorf("schemareg: add resource %q: %w", url, err)
		}
	}
	if err := c.AddResource(ref.ID, v); err != nil {
		return nil, fmt.Errorf("schemareg: schema %s does not parse: %w", ref, err)
	}
	compiled, err := c.Compile(ref.ID)
	if err != nil {
		return nil, fmt.Errorf("schemareg: schema %s is not a valid JSON Schema: %w", ref, err)
	}
	return &Schema{
		Ref:         ref,
		ContentHash: contentHash(doc),
		raw:         doc,
		compiled:    compiled,
	}, nil
}

// docID extracts the $id of a schema document ("" when absent). The caller
// must have decoded doc as valid JSON first; non-object documents simply
// have no $id.
func docID(doc []byte) string {
	var head struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(doc, &head); err != nil {
		return ""
	}
	return head.ID
}

// contentHash is the sha256 hex digest of doc — the registry's integrity
// fingerprint for a schema version.
func contentHash(doc []byte) string {
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:])
}
