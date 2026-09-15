package assets

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/validation"
)

// PublishCandidate is everything a publish would store for one asset
// version, in one value: the version document (manifest), the rights
// document, and the row fields the version carries besides them. It is the
// input of Gate, and the shape the publish command (T0705) assembles from
// the publisher's request and the project's accepted state.
//
// The two documents are handed over as the BYTES that would be stored, not
// as parsed values: what a publish gate has to be able to refuse is the
// document that would actually land in the column — badly formed JSON, an
// unknown field, a format version this platform does not read — and a
// candidate that arrived already parsed could no longer be asked that
// question.
type PublishCandidate struct {
	// AssetPID is the published asset's persistent identifier: the asset
	// the new version belongs to (research_assets.pid, migration 00064).
	AssetPID PID
	// AssetType is the asset's type — the table the manifest's required
	// metadata is taken from.
	AssetType Type
	// Version is the immutable version label this publish would store
	// (UNIQUE(asset_id, version)).
	Version string
	// Manifest is the manifest document, as it would be stored in
	// research_asset_versions.manifest.
	Manifest json.RawMessage
	// RightsJSON is the rights document, as it would be stored in
	// research_asset_versions.rights_json (internal/rights).
	RightsJSON json.RawMessage
	// OriginRefs are the version's provenance pins (docs/11 §3: where the
	// published content came from), in the canonical kind:value form of
	// OriginRef.
	OriginRefs []string
	// Visibility is the published version's coarse visibility axis.
	Visibility Visibility
	// IntegrityHash is the hash the version is published under; it must
	// cover the manifest document (GateResult.ManifestJSON / Manifest.Hash).
	IntegrityHash string
	// CreatorIDs are the users the version credits (docs/11 §3:
	// creators/contributors; CLAUDE.md invariant 14).
	CreatorIDs []string
}

// GateResult is what a candidate that passed the gate yields: the facts the
// asset gate of the validation ladder reads, plus the canonical forms of
// the two documents, ready to be written to the row.
type GateResult struct {
	// Facts are the asset-gate facts (internal/rsg/validation.AssetFacts)
	// derived from the candidate. Every fact is true exactly when its
	// check found nothing to refuse — Gate returns one *ValidationError
	// per false fact (the Document and Ref fields are the caller's: they
	// carry the research-asset-version entity document, which the publish
	// command renders from the row it is about to write).
	Facts validation.AssetFacts
	// Manifest is the parsed manifest document.
	Manifest Manifest
	// ManifestJSON is the manifest's canonical bytes — the bytes to store
	// in the manifest column. They are the bytes Facts.IntegrityHash
	// covers, so storing them keeps the row verifiable.
	ManifestJSON []byte
	// Rights is the parsed rights document.
	Rights rights.Document
}

// Gate evaluates one publish candidate against the asset gate of the
// progressive validation ladder (docs/11 §3, internal/rsg/validation
// GateAsset) and returns the facts those checks read.
//
// It returns an error joining one *ValidationError per rule the candidate
// breaks — nil when it breaks none — and the facts in both cases: a
// candidate refused for missing provenance still yields the facts a
// preview needs (T0704 lists exactly these as blockers), and a caller that
// wants the ladder's own verdict hands Facts to the validation engine,
// which declares the severities and the reasons the ladder renders.
//
// The import of internal/rsg/validation is deliberate: the asset gate's
// fact vocabulary has exactly one definition — validation.AssetFacts, the
// struct its asset checks read — and this package fills that struct
// rather than a second one that would drift from it. The dependency
// direction is one-way (validation knows nothing about assets), and the
// engine is pure: no adapter, no infrastructure.
//
// Which fact a rule feeds, and why:
//
//   - source pinned: the origin refs are non-empty, every ref is canonical
//     (KindProject/KindRelease/KindState/KindObjectVersion), and at least
//     one of them names a release or an accepted state. docs/11 §3 wants
//     "source accepted state/release" — a version whose provenance stops
//     at "this project" pins no accepted research state, and the published
//     bytes could not be traced back to a reviewed transition.
//   - version pinned: the version label is one this platform can store as
//     the immutable identity of the version (ValidVersionLabel) — the
//     label is what every later reference and URL resolves through, so a
//     version without one is not a version.
//   - dependency pins: the manifest's pins are canonical, none is listed
//     twice, and none names the version being published itself. An empty
//     list passes: having no dependencies is not a violation of "pin your
//     dependencies" (docs/11 §5 Dependency).
//   - rights: the stored rights document parses and validates
//     (rights.Parse). A candidate with no rights document, with an empty
//     object, or with a document this platform does not read is refused —
//     the asset would be published without a declaration anyone can act
//     on, which is what "rights/license must be set" (docs/11 §3) exists
//     to stop. What the document must SAY is its own model's business: a
//     declaration naming no standard license is a valid statement
//     (internal/rights.Document allows it deliberately), so it is not this
//     gate's place to demand one.
//   - visibility: public or private, the two values the column admits.
//   - metadata: the manifest parses, validates (format version, the
//     asset type's required metadata, the pins) and declares the same
//     asset type as the candidate. The last rule is the quiet one: the
//     required metadata table is chosen by the manifest's own asset_type,
//     so a benchmark manifest published under a dataset asset would be
//     held to the wrong table and its metadata would mean nothing.
//   - integrity hash: the hash is a sha256 hex digest AND equals the
//     canonical manifest's digest (Manifest.Hash). The shape rule alone
//     would accept any 64 hex characters, including one that covers a
//     different document; the equality rule is what makes "the asset
//     carries its hash" (docs/11 §3) mean the published version cannot be
//     edited without the hash moving. When the manifest does not parse
//     there is nothing to verify against, and the manifest's own refusal
//     is the reported reason (the fact stays false).
//   - contributors: at least one creator id, none of them blank.
//
// The identity rules — the pid is a real pid, the version label is
// storable, the manifest declares a type — are preconditions rather than
// ladder facts: a candidate that fails one is not a publishable version at
// all, so Gate refuses it without a fact of its own.
func Gate(c PublishCandidate) (GateResult, error) {
	var res GateResult
	var errs []error

	if !ValidPID(string(c.AssetPID)) {
		errs = append(errs, &ValidationError{
			Code:   CodeInvalidPID,
			Field:  "asset_pid",
			Detail: "expected a persisted asset's pid (26 Crockford base32 characters), got " + quote(string(c.AssetPID)),
		})
	}

	if ValidVersionLabel(c.Version) {
		res.Facts.VersionPinned = true
	} else {
		errs = append(errs, &ValidationError{
			Code:  CodeInvalidVersionLabel,
			Field: "version",
			Detail: "expected an immutable version label (1.." + itoa(MaxVersionLen) +
				" characters of [A-Za-z0-9._-], not \".\" or \"..\"), got " + quote(c.Version),
		})
	}

	errs = append(errs, c.validateProvenance(&res)...)

	rightsDoc, rightsErr := rights.Parse(c.RightsJSON)
	if rightsErr != nil {
		code := CodeInvalidRights
		if len(bytes.TrimSpace(c.RightsJSON)) == 0 {
			code = CodeMissingRights
		}
		errs = append(errs, &ValidationError{
			Code:   code,
			Field:  "rights_json",
			Detail: "the version must carry a rights declaration: " + rightsErr.Error(),
		})
	} else {
		res.Rights = rightsDoc
		res.Facts.Rights = true
	}

	if c.Visibility.Valid() {
		res.Facts.Visibility = true
	} else {
		errs = append(errs, &ValidationError{
			Code:   CodeInvalidVisibility,
			Field:  "visibility",
			Detail: "expected one of public|private, got " + quote(string(c.Visibility)),
		})
	}

	manifest, canonical, manifestErr := parseManifestBytes(c.Manifest)
	if manifestErr != nil {
		errs = append(errs, manifestErr)
	} else {
		res.Manifest = manifest
		res.ManifestJSON = canonical
		switch {
		case manifest.AssetType != c.AssetType:
			errs = append(errs, &ValidationError{
				Code:  CodeManifestTypeMismatch,
				Field: "asset_type",
				Detail: "the manifest declares " + quote(string(manifest.AssetType)) +
					" while the version is published under " + quote(string(c.AssetType)) +
					"; the required metadata would be taken from the wrong table",
			})
		default:
			res.Facts.Metadata = true
		}
		if selfErrs := validateSelfPin(manifest.DependencyPins, c.AssetPID, c.Version); len(selfErrs) > 0 {
			errs = append(errs, selfErrs...)
		} else {
			res.Facts.DependencyPins = true
		}
	}

	if vErr := c.validateIntegrityHash(canonical); vErr != nil {
		errs = append(errs, vErr)
	} else if len(canonical) > 0 {
		res.Facts.IntegrityHash = true
	}

	if validCreatorIDs(c.CreatorIDs) {
		res.Facts.Contributors = true
	} else {
		errs = append(errs, &ValidationError{
			Code:   CodeNoContributors,
			Field:  "creator_ids",
			Detail: "expected at least one creator id, none blank; got " + describeCreatorIDs(c.CreatorIDs),
		})
	}

	return res, joinErrors(errs...)
}

// validateProvenance checks the origin refs and sets the source-pinned fact.
func (c PublishCandidate) validateProvenance(res *GateResult) []error {
	if len(c.OriginRefs) == 0 {
		return []error{&ValidationError{
			Code:   CodeMissingProvenance,
			Field:  "origin_refs",
			Detail: "expected at least one provenance pin (project, release, state or object_version); a version with no origin cannot be traced to what was published",
		}}
	}
	var errs []error
	for i, ref := range c.OriginRefs {
		if !OriginRef(ref).Valid() {
			errs = append(errs, &ValidationError{
				Code:   CodeInvalidProvenanceRef,
				Field:  "origin_refs[" + itoa(i) + "]",
				Detail: "expected the canonical kind:value form with a uuid value, got " + quote(ref),
			})
		}
	}
	if len(errs) > 0 {
		return errs
	}
	if !c.pinsAcceptedSource() {
		return []error{&ValidationError{
			Code:   CodeUnpinnedSource,
			Field:  "origin_refs",
			Detail: "expected a release or state origin pin (docs/11 §3: source accepted state/release); the refs name " + describeOriginKinds(c.OriginRefs),
		}}
	}
	res.Facts.SourcePinned = true
	return nil
}

// pinsAcceptedSource reports whether the origin refs pin the accepted
// state or release the version was published from.
func (c PublishCandidate) pinsAcceptedSource() bool {
	for _, ref := range c.OriginRefs {
		kind, _, ok := ParseOriginRef(ref)
		if !ok {
			continue
		}
		if kind == KindRelease || kind == KindState {
			return true
		}
	}
	return false
}

// validateIntegrityHash checks the candidate's hash: well formed, and —
// when the manifest parsed, so there is something to cover — the digest of
// the manifest's canonical bytes.
func (c PublishCandidate) validateIntegrityHash(canonical []byte) error {
	if !isSHA256Hex(c.IntegrityHash) {
		return &ValidationError{
			Code:   CodeInvalidIntegrityHash,
			Field:  "integrity_hash",
			Detail: "expected a sha256 hex digest (64 lowercase hex characters), got " + quote(c.IntegrityHash),
		}
	}
	if len(canonical) == 0 {
		// The manifest did not parse: its refusal is already reported, and
		// there is no canonical document to compare the hash against.
		return nil
	}
	want := sha256Hex(canonical)
	if c.IntegrityHash != want {
		return &ValidationError{
			Code:   CodeIntegrityHashMismatch,
			Field:  "integrity_hash",
			Detail: "the hash " + quote(c.IntegrityHash) + " does not cover the manifest being published (canonical manifest digest " + want + ")",
		}
	}
	return nil
}

// parseManifestBytes parses the candidate's manifest document and returns
// it with its canonical bytes. A candidate with no manifest at all is a
// malformed one, not a candidate without optional metadata: the column is
// NOT NULL and every publish carries a manifest.
//
// The two whole-document refusals raised HERE name the candidate field
// they are about ("manifest", the column the bytes would be stored in),
// the way the rights refusals name "rights_json"; the bytes never became
// a document, so there is no path inside one to point at. A refusal
// ParseManifest itself raises keeps the path INSIDE the document
// ("version", "metadata.data_type"), because that is where the publisher
// has to look.
func parseManifestBytes(raw json.RawMessage) (Manifest, []byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return Manifest{}, nil, &ValidationError{
			Code:   CodeMalformedManifest,
			Field:  "manifest",
			Detail: "the manifest document is empty; every published version carries one",
		}
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return Manifest{}, nil, err
	}
	canonical, err := m.CanonicalJSON()
	if err != nil {
		return Manifest{}, nil, &ValidationError{
			Code:   CodeMalformedManifest,
			Field:  "manifest",
			Detail: "the manifest does not render to its canonical form: " + err.Error(),
		}
	}
	return m, canonical, nil
}

// validCreatorIDs reports whether the version credits at least one user,
// with no blank entry. An id's SHAPE is deliberately not checked here: the
// column is a uuid, so the database refuses a non-uuid, and the id's
// existence is the caller's read to make (a gate that cannot look anything
// up cannot decide it).
func validCreatorIDs(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			return false
		}
	}
	return true
}

// describeCreatorIDs renders the creator list for a refusal without
// echoing unbounded content.
func describeCreatorIDs(ids []string) string {
	switch {
	case len(ids) == 0:
		return "none"
	case strings.TrimSpace(ids[0]) == "":
		return "a blank first entry"
	default:
		return itoa(len(ids)) + " entries, one of them blank"
	}
}

// describeOriginKinds names the kinds the refs do use, so the refusal says
// what was found instead of only what was wanted.
func describeOriginKinds(refs []string) string {
	kinds := make([]string, 0, len(refs))
	for _, ref := range refs {
		if kind, _, ok := ParseOriginRef(ref); ok {
			kinds = append(kinds, string(kind))
		}
	}
	if len(kinds) == 0 {
		return "no readable kind"
	}
	return strings.Join(kinds, ",")
}

// isSHA256Hex reports the digest form every stored hash in this repository
// uses: lowercase hex, 64 characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}
