package assets

import "strings"

// The asset metadata of docs/11 §4 — the layer the document makes
// revisable and explicitly separates from the scientific version:
//
//	Scientific Version 不可变；描述、keywords、cover、contact、
//	documentation 等 Asset Metadata 可独立 revision，保留 audit，
//	不产生新的 scientific version。
//
// Three things in that sentence are structural, and this file is the
// first two:
//
//   - the revision is INDEPENDENT of the version stream. Nothing here
//     touches a research_asset_versions row, and nothing here is a
//     version: AssetMetadata is a value of the ASSET
//     (research_assets, migration 00125), keyed by the asset's pid, not
//     by any version label. Revising it cannot produce a version —
//     there is no version field to move.
//   - the revision is AUDITED. That half is not in this file: the audit
//     row commits in the same transaction as the write
//     (internal/application/assetmetadata, the shape
//     internal/application/projects/settings.go set).
//   - the asset's IDENTITY is not metadata. PID, Type and
//     OriginProjectID are deliberately absent below; docs/11 §2 makes
//     the pid the value the network sees and the URLs are built from,
//     and it never changes. AssetMetadata carries a PID field so a
//     caller can name the asset it is about, and nothing that could
//     change it.
//
// # Titles and slugs, and why the bounds look familiar
//
// Title and Slug are metadata too — the manifest and the asset row have
// always held them, and Asset calls Slug "display-level, revisable
// metadata (docs/11 §4)" in as many words. Their bounds (SlugMaxLen,
// TitleMaxLen) are the SAME numbers the publish command applies when it
// creates an asset (internal/application/assetpublish.MaxSlugLen /
// MaxTitleLen), because both write the same column: a value a publish
// would refuse to create must not be reachable through a revision
// either. The publish's two constants are ALIASES of these rather than
// copies of the numbers, so the two surfaces cannot drift; the equality
// is asserted in internal/application/assetmetadata, the only package
// that may import both.
//
// # What the validators are, and are not
//
// A list-shaped field is VALID when it holds at most MaxItems entries,
// each non-blank after trimming and at most MaxItemLen characters. That
// is the whole rule, and it is deliberately narrower than "well-formed":
//
//   - no charset, no URL or e-mail shape for contact or documentation.
//     This build stores the reference the publisher declared and never
//     dereferences it, so asserting a shape would refuse a DOI, an
//     internal repository path or a handle that is a perfectly good
//     reference.
//   - no uniqueness rule. Duplicate keywords are meaningless, but
//     "meaningless" is not the same as "not storable", and inventing a
//     refusal here would be inventing a product rule (CLAUDE.md §5:
//     L3 stays the owner's).
//   - no ordering rule. The stored order is the caller's declaration
//     order, the same way asset_version_parties.position preserves one
//     (migration 00082), so a list reads back item by item as declared.
//
// The bounds themselves are this task's L1 choice of SIZE, not a
// statement about content: a description short enough to be a description
// rather than a document, a keyword short enough to be a term. They are
// enforced on the write path, not as CHECK constraints — the same
// division the manifest's metadata block uses (00067 stores the document,
// internal/assets decides the field tables).

// The metadata bounds. Title and Slug carry the publish's numbers; the
// rest are this surface's.
const (
	// SlugMaxLen and TitleMaxLen bound the two display fields the asset
	// row has always held. They are the values
	// internal/application/assetpublish enforces at create time (its
	// MaxSlugLen / MaxTitleLen alias these), because a create and a
	// revision write one column.
	SlugMaxLen  = 128
	TitleMaxLen = 256

	// MaxDescriptionLen bounds the description. 4000 is the bound the
	// project purpose already uses (domain.ValidProjectPurpose): the same
	// kind of prose field on the same kind of governed row, and two
	// answers to "how long may a description be" is one more than the
	// product has.
	MaxDescriptionLen = 4000

	// MaxKeywords and MaxKeywordLen bound the keyword list. Keywords are
	// terms rather than prose — a search facet, not a sentence.
	MaxKeywords   = 32
	MaxKeywordLen = 64

	// MaxContacts and MaxContactLen bound the contact list. A contact is
	// one reachable reference, and a handful of them is what "how do I
	// reach whoever is responsible" needs.
	MaxContacts   = 16
	MaxContactLen = 320

	// MaxDocumentation and MaxDocumentationLen bound the documentation
	// list. Documentation entries are references, which are longer than
	// keywords and shorter than prose.
	MaxDocumentation    = 32
	MaxDocumentationLen = 2048
)

// AssetMetadata is one asset's revisable metadata (docs/11 §4): the
// values of research_assets.description, .keywords, .contact,
// .documentation and the title/slug display fields, as one value the
// revision surface reads, validates and writes.
//
// PID names the asset the metadata belongs to. It is carried here so a
// caller holds the identity together with the values — the revision's
// result has to name the asset it just revised, and the URL that identity
// resolves through is built from it (AssetURL) — and it is never a field
// a request can set: the pid in a revision request comes from the
// request's target, not from its body.
//
// CoverBlobID is RESERVED and is not writable in this build. docs/11 §4
// lists cover among the revisable metadata, so the field exists and names
// the blob row it would point at; the blob surface has no way to serve
// those bytes (no signed URL, no TTL, no download route), so a revision
// that tried to set it is refused by name rather than stored
// (internal/application/assetmetadata.ErrCoverNotSupported). Empty means
// "no cover", which is every asset's state today.
type AssetMetadata struct {
	// PID is the asset's persistent identifier (migration 00064). The
	// asset is named by it and by nothing else: it does not change when
	// any field below does.
	PID PID
	// Title is the human-facing label.
	Title string
	// Slug is display-level, revisable metadata — see Asset.Slug. It
	// never appears in an identity or a persistent URL.
	Slug string
	// Description is the asset's prose description; "" means none.
	Description string
	// Keywords are the asset's terms, in declaration order; empty means
	// none.
	Keywords []string
	// Contact are references to whoever is responsible for the asset, in
	// declaration order; empty means none recorded.
	Contact []string
	// Documentation are references to where the asset is documented, in
	// declaration order; empty means none recorded.
	Documentation []string
	// CoverBlobID is the reserved cover slot; empty means no cover. See
	// the type doc — nothing in this build writes it.
	CoverBlobID string
}

// ValidMetadataSlug reports whether the raw slug is storable after
// trimming: non-empty and at most SlugMaxLen characters.
//
// The rule is the publish command's create-time rule exactly (it trims,
// then bounds), and deliberately no more. A slug is display-level
// metadata that never enters a URL (Asset.Slug: it "never appears in an
// identity or a persistent URL"), so there is no path-segment or charset
// constraint to keep — asserting one would refuse a value the publish
// would have created.
func ValidMetadataSlug(slug string) bool {
	s := strings.TrimSpace(slug)
	return s != "" && len(s) <= SlugMaxLen
}

// ValidMetadataTitle reports whether the raw title is storable after
// trimming: non-empty and at most TitleMaxLen characters. The rule is the
// publish command's create-time rule, for the same reason
// ValidMetadataSlug states.
func ValidMetadataTitle(title string) bool {
	t := strings.TrimSpace(title)
	return t != "" && len(t) <= TitleMaxLen
}

// ValidMetadataDescription reports whether the raw description is
// storable: at most MaxDescriptionLen characters. An EMPTY description is
// valid — it is how a caller clears one, and it is the state of every
// asset that never had one (migration 00125 stores "" rather than NULL).
// The value is measured as given rather than trimmed, because whitespace
// inside a description is content; the write path stores what the caller
// declared.
func ValidMetadataDescription(description string) bool {
	return len(description) <= MaxDescriptionLen
}

// ValidMetadataList reports whether a list-shaped metadata field is
// storable: at most maxItems entries, each non-blank after trimming and
// at most maxItemLen characters. An empty list is valid — it is how a
// caller clears the field.
//
// nil and an empty slice are the same value here, and that is the point
// rather than a convenience: a revision that clears a field and one that
// sends nothing for a field it is clearing are the same request, and the
// stored column is '{}' either way (migration 00125). See the file doc
// for what this rule deliberately does not check.
func ValidMetadataList(items []string, maxItems, maxItemLen int) bool {
	if len(items) > maxItems {
		return false
	}
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" || len(trimmed) > maxItemLen {
			return false
		}
	}
	return true
}
