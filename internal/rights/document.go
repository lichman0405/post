package rights

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// DocumentVersion is the only rights document version this package
// understands. It is the version specs/policies/rights-template.yaml
// carries, and it is a roadmap for a format change rather than a label:
// a stored document with any other version is refused by Parse (a
// RIGHTS_UNSUPPORTED_VERSION) instead of being read as if it were this
// one, because the fields that changed are exactly the ones a lenient
// reader would silently misread.
const DocumentVersion = 1

// MaxAgreementRefLen and MaxNotesLen bound the two free-text fields.
//
// The agreement ref is a reference, not the agreement: docs/12 §4 wants a
// custom agreement REF (docs/38 §3: the platform may store or link the
// agreement, and the engine only executes explicitly coded approval and
// access rules). 512 characters holds a URL, an object-store key or a
// document id with room to spare.
//
// Notes is the one place a publisher may write a sentence. 2000 characters
// is a paragraph, not a document — an agreement that needs more than that
// belongs in the referenced agreement, where a human can read the whole of
// it.
//
// Both bounds are in CHARACTERS, not bytes, and every rule below counts
// them with utf8.RuneCountInString. The unit is not decoration: the
// documents this package stores are written by people working in Chinese
// and other non-Latin scripts, where one character is three or four bytes,
// so a byte-counted bound would refuse a 700-character note while the error
// message told the publisher it had 2100 characters. What the field may
// hold and what the message reports are the same quantity, and it is the
// one a person counts.
const (
	MaxAgreementRefLen = 512
	MaxNotesLen        = 2000
)

// Document is the machine-readable rights declaration of one published
// research asset version or knowledge publication, in the shape
// specs/policies/rights-template.yaml fixes: a standard license id, a
// custom agreement reference, the usage declarations, and the two access
// axes (metadata visibility vs blob data access).
//
// The JSON field names are part of the model, not a transport detail: this
// object IS the stored rights_json document (research_asset_versions,
// knowledge_publications — migrations 00010, 00066), and the same JSON is
// what a future rights endpoint serves and the web UI renders. The struct
// field order matches the template's order, so a marshalled document reads
// like the file it comes from.
//
// A field is present-or-null rather than empty-or-absent on purpose: null
// standard_license_id is the template's own "no standard license named",
// which is a different statement from a license id of "" (refused) — and
// neither of them is a statement about what a reuser may do, which is what
// the usage block is for.
type Document struct {
	// Version is the document format version; must be DocumentVersion.
	Version int `json:"version"`

	// StandardLicenseID is the standard license the publisher named, in
	// the license ecosystem's own id (a standard license id, docs/12 §4).
	// nil = none named. Shape-checked by LicenseID.Valid; whether the id
	// names a real license is not decided here.
	StandardLicenseID *string `json:"standard_license_id"`

	// CustomAgreementRef is the reference to the custom agreement that
	// carries terms this document does not encode — a URL, an
	// object-store key, a document id. nil = none named. A non-nil ref
	// is stored verbatim; this platform does not read the agreement, and
	// nothing in the engine parses or enforces its text (docs/38 §3).
	CustomAgreementRef *string `json:"custom_agreement_ref"`

	// Usage is what the declaration says about each use of the asset.
	Usage Usage `json:"usage"`

	// Visibility is metadata visibility and blob access, kept apart.
	Visibility Visibility `json:"visibility"`

	// Notes is an optional human sentence attached to the declaration —
	// the place for the context the machine fields cannot carry ("the
	// agreement covers the 2026 release only"). nil = no note.
	Notes *string `json:"notes"`
}

// New returns the default document: the values
// specs/policies/rights-template.yaml shows, used verbatim. Everything
// starts "unspecified" except the two axes the template fixes —
// attribution required (credit is a POST invariant, and the template
// states it) and data access restricted (docs/12 §3: the fail-closed
// default; widening is an explicit, audited act) — with metadata
// visibility following the project policy and no patent grant stated.
//
// The returned document validates (TestNewMatchesRightsTemplate pins it),
// so a publish path may start here and change only what the publisher
// declares.
func New() Document {
	return Document{
		Version: DocumentVersion,
		Usage: Usage{
			CommercialUse:  PermissionUnspecified,
			Derivatives:    PermissionUnspecified,
			Redistribution: PermissionUnspecified,
			ModelTraining:  PermissionUnspecified,
			Attribution:    AttributionRequired,
			PatentGrant:    PatentGrantNone,
		},
		Visibility: Visibility{
			Metadata:   MetadataProjectPolicy,
			DataAccess: DataAccessRestricted,
		},
	}
}

// Validate returns nil when the document may be stored and served, or an
// error joining one *ValidationError per broken rule.
//
// Every rule here is a shape rule — the document says something this
// package can hold: a version it understands, values from the fixed
// vocabularies, a well-formed license id, a formed agreement ref and
// metadata token, a bounded note. There is exactly one cross-field rule,
// and it is textual rather than legal: patent_grant "see_agreement" names
// an agreement that must exist in the same document, because "see the
// agreement" with no agreement in sight is a declaration pointing at
// nothing.
func (d Document) Validate() error {
	errs := []error{validateVersion(d.Version), validateLicense(d.StandardLicenseID)}
	errs = append(errs, validateAgreementRef(d.CustomAgreementRef), d.Usage.Validate(), d.Visibility.Validate())
	if d.Usage.PatentGrant == PatentGrantSeeAgreement && d.CustomAgreementRef == nil {
		errs = append(errs, &ValidationError{
			Code:  CodePatentGrantWithoutAgreement,
			Field: "usage.patent_grant",
			Detail: "see_agreement names no agreement: custom_agreement_ref is null, " +
				"and a patent grant that is only stated in an unnamed agreement cannot be read",
		})
	}
	errs = append(errs, validateNotes(d.Notes))
	return joinErrors(errs...)
}

// Marshal returns the canonical encoding of the document — the bytes that
// belong in a rights_json column.
//
// It validates first: a caller cannot persist a document this package
// would refuse to read back. The only other failure json.Marshal has is
// unreachable for this struct, so an error from Marshal is always a
// validation error (errors.As reaches the *ValidationError).
func (d Document) Marshal() ([]byte, error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(d)
}

// Parse decodes and validates a stored rights document.
//
// Unknown fields are REFUSED rather than ignored. The rights document is a
// record of what a publisher declared, and a reader that drops a field it
// does not recognise would hand back a document that still validates while
// missing a declaration — the quietest way to lose one. The cost is
// fail-closed on forward compatibility: a document written by a newer
// version of this package is refused (RIGHTS_MALFORMED_DOCUMENT) until
// this package learns its fields, which is the intended direction for a
// format whose version field exists to make that visible.
//
// The empty object is not a document: it has no version, so it fails with
// RIGHTS_UNSUPPORTED_VERSION. Rows written before 00066 hold whatever the
// writing path chose (no application path has ever written one — the
// publish command is T0705), and this parser does not invent a meaning for
// a document that states nothing.
func Parse(raw []byte) (Document, error) {
	var d Document
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Document{}, &ValidationError{
			Code:   CodeMalformedDocument,
			Detail: "not one JSON object of the rights format: " + err.Error(),
		}
	}
	// A second value after the document is not "extra data this parser
	// ignores" — it is bytes whose meaning is unknown, in the column
	// that is supposed to hold exactly one document.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Document{}, &ValidationError{
			Code:   CodeMalformedDocument,
			Detail: "trailing content after the document",
		}
	}
	if err := d.Validate(); err != nil {
		return Document{}, err
	}
	return d, nil
}

func validateVersion(v int) error {
	if v == DocumentVersion {
		return nil
	}
	return &ValidationError{
		Code:  CodeUnsupportedVersion,
		Field: "version",
		Detail: "expected the document version this platform reads (" +
			itoa(DocumentVersion) + "), got " + itoa(v),
	}
}

func validateLicense(id *string) error {
	if id == nil {
		return nil
	}
	if ValidLicenseID(*id) {
		return nil
	}
	return &ValidationError{
		Code:  CodeInvalidLicenseID,
		Field: "standard_license_id",
		Detail: "expected a well-formed license id (1.." + itoa(MaxLicenseIDLen) +
			" characters, e.g. MIT or CC-BY-4.0), got " + quote(*id),
	}
}

func validateAgreementRef(ref *string) error {
	if ref == nil {
		return nil
	}
	if ValidAgreementRef(*ref) {
		return nil
	}
	return &ValidationError{
		Code:  CodeInvalidAgreementRef,
		Field: "custom_agreement_ref",
		Detail: "expected a reference to the agreement (1.." + itoa(MaxAgreementRefLen) +
			" printable characters, no surrounding space), got " + quote(*ref),
	}
}

// ValidAgreementRef reports whether s may be stored as a custom agreement
// reference: non-blank, at most MaxAgreementRefLen characters, no
// surrounding space, and no control character.
//
// It says nothing about where the reference points. A reference to an
// agreement that does not exist is storable, because the platform does not
// resolve agreement refs (docs/38 §3: the platform records and links, the
// engine enforces only explicitly coded rules) — a future publish gate may
// resolve it, and that gate's rule belongs with the gate.
func ValidAgreementRef(s string) bool {
	if s == "" || utf8.RuneCountInString(s) > MaxAgreementRefLen || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validateNotes(notes *string) error {
	// One count, used by both the rule and the message, so the bound the
	// message states and the bound the rule applies cannot drift apart.
	length := 0
	if notes != nil {
		length = utf8.RuneCountInString(*notes)
	}
	if notes == nil || length <= MaxNotesLen {
		return nil
	}
	return &ValidationError{
		Code:   CodeNotesTooLong,
		Field:  "notes",
		Detail: "expected at most " + itoa(MaxNotesLen) + " characters, got " + itoa(length),
	}
}

// joinErrors drops the nil rules and returns nil, the single error, or an
// errors.Join of all of them — every broken rule is reported in one pass,
// so a caller fixing a document sees the whole list rather than one field
// per attempt.
func joinErrors(errs ...error) error {
	var kept []error
	for _, err := range errs {
		if err != nil {
			kept = append(kept, err)
		}
	}
	switch len(kept) {
	case 0:
		return nil
	case 1:
		return kept[0]
	default:
		return errors.Join(kept...)
	}
}

// quote renders a value the way Go source would, so a detail line shows
// the exact bytes a rule refused — including the invisible ones (a
// trailing space, an empty string).
func quote(s string) string { return strconv.Quote(s) }

// itoa renders a bound or a version in a detail line.
func itoa(n int) string { return strconv.Itoa(n) }
