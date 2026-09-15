package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
)

// ManifestFormatVersion is the only manifest format version this package
// reads and writes. It is a roadmap for a format change rather than a
// label: a stored document carrying any other version is refused by
// ParseManifest (ASSET_UNSUPPORTED_MANIFEST_VERSION) instead of being read
// as if it were this one, because the fields that changed are exactly the
// ones a lenient reader would silently misread.
const ManifestFormatVersion = 1

// Manifest is the version manifest document of one published research
// asset version: the JSON object stored in
// research_asset_versions.manifest (jsonb, migration 00010). It carries
// the two publish-checklist items of docs/11 §3 that belong to the version
// document rather than to a column of the row — the asset type's required
// metadata and the version's dependency pins. The other checklist items
// live in columns of their own (origin_refs, rights_json, visibility,
// integrity_hash) and are checked by the publish gate beside this
// document, not inside it.
//
// The JSON field names are part of the model, not a transport detail: this
// object IS the stored manifest, the same bytes the hash covers and the
// same document a future asset page renders. The struct field order is the
// canonical serialization order.
//
// One document, four assets: the asset type decides which metadata the
// version must carry (RequiredMetadata) instead of the type deciding the
// document's Go type. A per-type struct per asset type would put the same
// envelope (format version, type, pins) in four places, and the manifest
// column holds one jsonb document whatever the type is — the reader has to
// dispatch on the declared type either way.
type Manifest struct {
	// Version is the manifest format version; must be
	// ManifestFormatVersion.
	Version int `json:"version"`
	// AssetType is the asset type this version belongs to. It must be one
	// of the four V1 types, and the publish gate additionally requires it
	// to equal the type of the asset the version is published under —
	// otherwise the metadata would be held to another type's required
	// table.
	AssetType Type `json:"asset_type"`
	// Metadata is the version's metadata block: every key RequiredMetadata
	// demands for this asset type, plus any further keys the publisher
	// declares. See Metadata for the value shapes it may hold.
	Metadata Metadata `json:"metadata"`
	// DependencyPins are the exact published versions this version was
	// built against (docs/11 §5). Empty when it depends on no other
	// asset; a pin that is present must be exact (DependencyPin).
	DependencyPins []DependencyPin `json:"dependency_pins"`
}

// Metadata is the manifest's metadata block: a JSON object of declared
// metadata keys. The type's required keys (RequiredMetadata) are a FLOOR,
// not a closed set — a publisher may declare more (instrument ids, a
// DOI-shaped reference, domain-specific fields), and the extra keys are
// part of the document and therefore part of the hash the integrity hash
// covers. What is closed is the set of value SHAPES: see
// classifyMetadataValue.
type Metadata map[string]any

// MetadataKind is the JSON shape one metadata key must have.
type MetadataKind string

const (
	// KindText: a non-blank JSON string.
	KindText MetadataKind = "text"
	// KindTextList: a non-empty JSON array of non-blank strings — a list
	// of references, steps or metric names.
	KindTextList MetadataKind = "text_list"
	// KindTextObject: a non-empty JSON object whose values are non-blank
	// strings or non-empty arrays of them — one level deep, which is what
	// a parameter block needs ({"temperature": "300 K"}) and all the
	// canonical form admits.
	KindTextObject MetadataKind = "text_object"
	// KindEnum: a non-blank JSON string from a fixed vocabulary
	// (MetadataField.Values).
	KindEnum MetadataKind = "enum"
)

// MetadataField declares one required metadata key of an asset type's
// manifest: the key, the shape its value must have, and — for KindEnum —
// the vocabulary it must come from.
type MetadataField struct {
	// Key is the metadata key, as it appears in the document.
	Key string
	// Kind is the value shape the key must have.
	Kind MetadataKind
	// Values is the Key's vocabulary when Kind is KindEnum; nil otherwise.
	Values []string
}

// requiredMetadata is the per-asset-type required metadata table — the
// four manifest shapes of the task, in one place.
//
// Dataset and Protocol are the domain-field lists docs/08 gives those two
// object types (§Dataset: purpose、data type、row/item summary、
// schema/profile、blob refs、derived_from、quality notes、access level;
// §Protocol: purpose、domain、steps/parameters、inputs/outputs、
// equipment/software requirements、version notes), reduced to the fields a
// published version must declare and named with the object schemas' own
// property names (specs/schemas/dataset.schema.json requires purpose and
// fixes access_level to open|restricted; specs/schemas/protocol.schema.json
// requires purpose). The same names appear in the gate ladder's per-object
// required-field table (internal/rsg/validation typeRequiredFields,
// "dataset" and "protocol"): a published dataset asset is a dataset, and a
// field the PR gate demands before the object reaches main cannot be
// optional once it is published.
//
// Material Collection and Benchmark have no object type and no docs/08
// field list — V1 publishes them as assets without a scientific object
// behind them — so their fields are chosen here and are the one part of
// this table that is not quoted from a specification:
//
//   - Material Collection: a curated set of materials. Its members
//     (docs/08 §Material: material_ref、batch/sample id、…) are named by
//     member_refs, the reason they belong together is the collection's own
//     scientific statement (selection_criteria), and the party accountable
//     for the physical material is the custodian docs/11 §6 separates
//     from the rights holder.
//   - Benchmark: an evaluation setup. Its task names what is being
//     evaluated, metrics the quantities it is scored on, and dataset_refs
//     the datasets it scores against.
//
// Every type also requires purpose, the one field docs/08 gives both
// listed object types: a published asset has to state why it exists.
//
// Required for every type; not sufficient for any: a required key present
// with an empty or wrongly shaped value fails the same way an absent one
// does (ASSET_METADATA_INVALID_VALUE), because an empty declaration
// declares nothing.
var requiredMetadata = map[Type][]MetadataField{
	TypeDataset: {
		{Key: "purpose", Kind: KindText},
		{Key: "data_type", Kind: KindText},
		{Key: "blob_ids", Kind: KindTextList},
		{Key: "access_level", Kind: KindEnum, Values: []string{"open", "restricted"}},
		{Key: "quality_notes", Kind: KindText},
	},
	TypeProtocol: {
		{Key: "purpose", Kind: KindText},
		{Key: "domain", Kind: KindText},
		{Key: "steps", Kind: KindTextList},
		{Key: "parameters", Kind: KindTextObject},
		{Key: "requirements", Kind: KindTextList},
	},
	TypeMaterialCollection: {
		{Key: "purpose", Kind: KindText},
		{Key: "member_refs", Kind: KindTextList},
		{Key: "selection_criteria", Kind: KindText},
		{Key: "custodian", Kind: KindText},
	},
	TypeBenchmark: {
		{Key: "purpose", Kind: KindText},
		{Key: "task", Kind: KindText},
		{Key: "metrics", Kind: KindTextList},
		{Key: "dataset_refs", Kind: KindTextList},
	},
}

// RequiredMetadata returns the required metadata fields of an asset type,
// in declaration order (stable — it is the order a form or an error list
// renders them in). An unknown type has no requirements; the caller that
// validates a document has already refused the type itself.
func RequiredMetadata(t Type) []MetadataField {
	fields := requiredMetadata[t]
	out := make([]MetadataField, len(fields))
	for i, f := range fields {
		out[i] = MetadataField{Key: f.Key, Kind: f.Kind, Values: append([]string(nil), f.Values...)}
	}
	return out
}

// Validate returns nil when the document may be stored and published, or
// an error joining one *ValidationError per broken rule — every broken
// rule in one pass, so a caller fixing a manifest sees the whole list
// rather than one field per attempt.
//
// Every rule here is a document rule: the format version, the asset type,
// the metadata the type requires, and the shape of the dependency pins.
// The rules that need the version's own identity (a pin naming the
// version itself) belong to the publish gate, which is the only caller
// that knows it.
func (m Manifest) Validate() error {
	if m.Version != ManifestFormatVersion {
		return &ValidationError{
			Code:  CodeUnsupportedManifestVersion,
			Field: "version",
			Detail: "expected the manifest format version this platform reads (" +
				itoa(ManifestFormatVersion) + "), got " + itoa(m.Version),
		}
	}
	if !m.AssetType.Valid() {
		return &ValidationError{
			Code:  CodeUnknownAssetType,
			Field: "asset_type",
			Detail: "expected one of the four V1 asset types (dataset, protocol, material_collection, benchmark), got " +
				quote(string(m.AssetType)),
		}
	}
	errs := m.validateMetadata()
	errs = append(errs, validateDependencyPins(m.DependencyPins)...)
	return joinErrors(errs...)
}

// validateMetadata checks the metadata block: every required key of the
// asset type present and of its declared shape, and every extra key of a
// shape the canonical form can hold.
func (m Manifest) validateMetadata() []error {
	var errs []error
	required := make(map[string]bool, len(requiredMetadata[m.AssetType]))
	for _, f := range RequiredMetadata(m.AssetType) {
		required[f.Key] = true
		value, present := m.Metadata[f.Key]
		// null is not a declaration: it reads as "the key is there" only
		// to a reader that never checks the value, which is the reader
		// this check exists to stop.
		if !present || value == nil {
			errs = append(errs, &ValidationError{
				Code:  CodeMissingMetadata,
				Field: metadataPath(f.Key),
				Detail: "the " + string(m.AssetType) + " manifest must declare it (" + string(f.Kind) +
					"); a published version whose required metadata is absent is not discoverable or citable",
			})
			continue
		}
		if err := validateMetadataValue(f, value); err != nil {
			errs = append(errs, err)
		}
	}
	for _, key := range sortedKeys(m.Metadata) {
		if required[key] {
			continue
		}
		// An extra key is allowed — the required set is a floor — but it
		// is part of the document, so it must be a value the canonical
		// form holds: the same three shapes, with no declared kind to
		// match.
		value := m.Metadata[key]
		if value == nil {
			errs = append(errs, &ValidationError{
				Code:   CodeInvalidMetadataValue,
				Field:  metadataPath(key),
				Detail: "null is not a declared value; omit the key instead",
			})
			continue
		}
		if _, ok := classifyMetadataValue(value); !ok {
			errs = append(errs, &ValidationError{
				Code:   CodeInvalidMetadataValue,
				Field:  metadataPath(key),
				Detail: "expected " + metadataShapeDescription() + ", got " + describeJSON(value),
			})
		}
	}
	return errs
}

// validateMetadataValue checks one required field's value against the
// shape the field declares.
func validateMetadataValue(f MetadataField, value any) error {
	kind, ok := classifyMetadataValue(value)
	if !ok {
		return &ValidationError{
			Code:   CodeInvalidMetadataValue,
			Field:  metadataPath(f.Key),
			Detail: "expected " + kindDescription(f.Kind) + ", got " + describeJSON(value),
		}
	}
	if f.Kind == KindEnum {
		s, _ := value.(string)
		for _, allowed := range f.Values {
			if s == allowed {
				return nil
			}
		}
		return &ValidationError{
			Code:   CodeInvalidMetadataValue,
			Field:  metadataPath(f.Key),
			Detail: "expected one of " + strings.Join(f.Values, "|") + ", got " + quote(s),
		}
	}
	if kind != f.Kind {
		return &ValidationError{
			Code:   CodeInvalidMetadataValue,
			Field:  metadataPath(f.Key),
			Detail: "expected " + kindDescription(f.Kind) + ", got " + kindDescription(kind),
		}
	}
	return nil
}

// classifyMetadataValue reports which of the three supported shapes a
// decoded JSON value has, and false when it has none of them.
//
// The shapes are closed on purpose. A metadata value ends up in the
// manifest's canonical bytes, which the version's integrity hash covers
// and which must be identical before and after the jsonb round trip;
// JSON numbers are where that breaks first (jsonb normalises them as
// numeric, Go decodes them as float64, and the two re-serialise
// differently), so "published metadata is text" is the rule that keeps
// the hash verifiable. Booleans and nested structures are refused for the
// same reason — they are also not statements a required field needs: a
// declaration is text, a list of them, or a small block of them.
func classifyMetadataValue(v any) (MetadataKind, bool) {
	switch value := v.(type) {
	case string:
		if strings.TrimSpace(value) == "" {
			return "", false
		}
		return KindText, true
	case []any:
		if len(value) == 0 {
			return "", false
		}
		for _, item := range value {
			s, ok := item.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return "", false
			}
		}
		return KindTextList, true
	case map[string]any:
		if len(value) == 0 {
			return "", false
		}
		for _, item := range value {
			if _, ok := classifyMetadataValue(item); !ok {
				return "", false
			}
			if _, nested := item.(map[string]any); nested {
				return "", false
			}
		}
		return KindTextObject, true
	}
	return "", false
}

// ParseManifest decodes and validates a stored manifest document.
//
// Unknown top-level fields are REFUSED rather than ignored — the same rule
// internal/rights.Parse applies to the rights document and for the same
// reason: a reader that drops a field it does not recognise hands back a
// document that still validates while missing a declaration, which is the
// quietest way to lose one. The cost is fail-closed forward
// compatibility, which is the intended direction for a format whose
// version field exists to make that visible.
func ParseManifest(raw []byte) (Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, &ValidationError{
			Code:   CodeMalformedManifest,
			Detail: "not one JSON object of the manifest format: " + err.Error(),
		}
	}
	// A second value after the document is not "extra data this parser
	// ignores" — it is bytes whose meaning is unknown, in the column that
	// is supposed to hold exactly one document.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Manifest{}, &ValidationError{
			Code:   CodeMalformedManifest,
			Detail: "trailing content after the document",
		}
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// CanonicalJSON renders the document's canonical bytes: the struct in
// field order, metadata keys sorted (encoding/json sorts map keys), and
// absent collections rendered as the empty ones they mean — so a manifest
// built with a nil map hashes exactly like the same manifest built with an
// empty one.
//
// These are the bytes the integrity hash covers and the bytes that belong
// in the manifest column. They are stable across a jsonb round trip: the
// document holds only strings, arrays of strings and one-level objects of
// them (classifyMetadataValue), which jsonb preserves exactly, so a
// verifier that re-parses the stored document and re-renders it gets the
// same bytes back.
func (m Manifest) CanonicalJSON() ([]byte, error) {
	return json.Marshal(m.normalized())
}

// Hash returns the sha256 hex digest of the canonical bytes: the value a
// published version's integrity_hash carries. The publish gate compares
// the candidate's integrity hash to it, so a stored version's hash always
// covers the manifest published with it.
func (m Manifest) Hash() (string, error) {
	b, err := m.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return sha256Hex(b), nil
}

// ManifestHash derives the canonical hash of a stored manifest document's
// bytes: parse, validate, re-render canonically, digest. It is the
// verification half of Manifest.Hash — a reader holding the bytes in the
// manifest column (or in a field of a version row) derives the hash the
// same way the publisher did.
func ManifestHash(raw []byte) (string, error) {
	m, err := ParseManifest(raw)
	if err != nil {
		return "", err
	}
	return m.Hash()
}

// normalized returns a copy whose absent collections are the empty ones
// they mean, so the canonical bytes do not depend on how the value was
// built (nil map vs empty map, nil slice vs empty slice).
func (m Manifest) normalized() Manifest {
	out := m
	if out.Metadata == nil {
		out.Metadata = Metadata{}
	}
	if out.DependencyPins == nil {
		out.DependencyPins = []DependencyPin{}
	}
	return out
}

// sortedKeys returns the map's keys in ascending order — the order a
// metadata error list and the canonical bytes both read in.
func sortedKeys(m Metadata) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// metadataPath renders the JSON path of one metadata key.
func metadataPath(key string) string { return "metadata." + key }

// kindDescription names a shape the way an error message says it, so a
// refusal states what would have been accepted.
func kindDescription(kind MetadataKind) string {
	switch kind {
	case KindText:
		return "a non-blank string"
	case KindTextList:
		return "a non-empty array of non-blank strings"
	case KindTextObject:
		return "a non-empty object of strings and arrays of strings"
	case KindEnum:
		return "one of the declared values"
	}
	return string(kind)
}

// metadataShapeDescription names the shapes any metadata value may have.
func metadataShapeDescription() string {
	return "a non-blank string, a non-empty array of non-blank strings, or a non-empty object of them"
}

// describeJSON names the JSON kind of a value that failed a shape rule —
// "null", "a number", "a boolean", "an empty array" — so the message says
// what was found without echoing unbounded content.
func describeJSON(v any) string {
	switch value := v.(type) {
	case nil:
		return "null"
	case string:
		if strings.TrimSpace(value) == "" {
			return "a blank string"
		}
		return "a string"
	case []any:
		if len(value) == 0 {
			return "an empty array"
		}
		return "an array"
	case map[string]any:
		if len(value) == 0 {
			return "an empty object"
		}
		return "an object"
	case bool:
		return "a boolean"
	case float64:
		return "a number"
	}
	return "a value of an unsupported kind"
}

// sha256Hex is the digest form every stored hash in this repository uses
// (payload integrity hashes, release manifest hashes): lowercase hex.
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
