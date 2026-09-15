package assets

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/rights"
	"github.com/lichman0405/post/internal/rsg/validation"
)

// Publication impact preview (T0704): the read-only answer to "if this
// publish were executed, who would see what?".
//
// docs/23 §4 makes private→public the highest-risk operation of the
// platform and lists what it must carry: a human explicit action, an
// impact preview, rights validation, no hidden private dependency leak,
// and an audit event. The action, the audit event and the widening itself
// belong to the publish command (T0705); this file is the preview — the
// half that runs BEFORE the decision, and therefore the half that must not
// change anything. Nothing in this file writes: it reads a state value and
// returns a result.
//
// The preview takes the same value the publish gate takes (PublishCandidate:
// the asset, its type, the version document, the rights document, the
// origin refs, the target visibility, the hash and the creators) plus the
// state the repository is in right now. It runs the SAME gate (Gate,
// docs/11 §3) over the candidate — the publish checklist has one enforcer,
// and a preview that decided publishability its own way would be a second
// one — and then answers the part the gate cannot: what the publication
// would point at. The gate sees the candidate's bytes; only a reader of
// current state can see that a pinned version is still private, or that a
// referenced blob is still restricted.
//
// The output is deliberately the five lists the question has:
//
//	objects  — the object versions the publication would carry, with the
//	           visibility each has today (docs/11 §2: an object published as
//	           an asset becomes a network-reusable object);
//	metadata — the manifest's metadata block, key by key: what a public asset
//	           page renders (docs/11 §4);
//	blobs    — the blobs the manifest names, and the access each has today
//	           (docs/17 §5: every download checks project/object/blob access;
//	           public metadata is not open download);
//	refs     — the origin refs (docs/11 §3 provenance pins), each resolved to
//	           the project it points into and that project's visibility;
//	private_dependencies — the things the publication leans on that the
//	           network cannot see today, named one by one (docs/23 §4: no
//	           hidden private dependency leak);
//
// plus the candidate's own refusals (the gate's, plus the resolutions that
// failed) and the rights declaration's refusals, which are the fifth item
// of the task's own list.
//
// Why "named one by one" is the point: docs/11 §5 separates a Dependency —
// what a project needs to reproduce or run, which takes part in impact
// analysis — from a Reference, which does not. A dependency on a private
// version is exactly the thing that must not ride along quietly into a
// public publication: the published document is immutable and public, so
// the moment the row is written, the pin, the ref or the blob id it names
// is public too, and no later action can take that back.
//
// Determinism: same candidate + same state ⇒ byte-identical result. Every
// list below is built in a stated order (the document's own declaration
// order for pins, refs and blob ids; ascending key order for metadata),
// nothing iterates a Go map into output, and the blocker lists keep the
// order their producing rules ran in.

// PreviewRequest is one proposed publish, as the preview sees it: the
// project it would be published in (assets are published out of projects,
// docs/11 §2) and the candidate itself, in the exact shape the publish gate
// takes.
type PreviewRequest struct {
	// ProjectID is the project the publish would happen in — the route's
	// project, and the one a ref into it is judged against.
	ProjectID string
	// Candidate is the proposed publish.
	Candidate PublishCandidate
}

// CurrentState is everything the preview needs to know about the repository
// right now: the resolutions of the things the candidate names. A reader
// produces it (the Postgres adapter in cmd/api/assetshttp) and ImpactPreview
// consumes it, which is the same separation internal/rsg/validation draws
// between facts and the reader that assembled them.
//
// A nil Asset, an absent pin, a blob that is not listed: those are the
// answers "the pid names no asset", "the pin resolves to no stored
// version", "the blob id names no blob". They are NOT errors — they are the
// state, and the preview reports them as such. A canonical ref the state
// carries no entry for IS an error (ErrIncompleteState): a reader is
// required to answer for every ref it was asked about, so one that drops a
// ref fails loudly instead of having its silence reported as "unresolved".
type CurrentState struct {
	// Asset is the asset the candidate's pid names, or nil when it names no
	// stored asset.
	Asset *StoredAsset
	// Pins are the dependency pins that resolve to a stored asset version.
	// A pin that is not listed here resolves to nothing.
	Pins []StoredPin
	// Refs holds one entry per canonical origin ref of the candidate, in
	// any order (ImpactPreview matches them by ref).
	Refs []StoredRef
	// Blobs are the referred blobs that exist, by blob id.
	Blobs []StoredBlob
}

// StoredAsset is the asset row a pid resolves to.
type StoredAsset struct {
	// ID is the internal uuid of the research_assets row.
	ID string
	// PID is the persistent identifier the candidate names.
	PID PID
	// Type is the asset's declared type: the type every version of it is
	// published as.
	Type Type
	// Title is the asset's display title.
	Title string
	// OriginProjectID is the project the asset belongs to
	// (research_assets.origin_project_id, NOT NULL): the project whose
	// versions may be published under it.
	OriginProjectID string
}

// StoredPin is one dependency pin that resolves to a stored version, with
// the visibility that version has right now (research_asset_versions.
// visibility).
type StoredPin struct {
	// Pin is the canonical pid@version the manifest declared.
	Pin DependencyPin
	// Visibility is the pinned version's current visibility.
	Visibility Visibility
}

// StoredRef is the resolution of one origin ref.
//
// Resolved says whether the ref names a row at all. ProjectID and
// ProjectVisibility are the project the referenced entity lives in and that
// project's visibility (projects.visibility, the V1 Public/Private preset
// of docs/12 §2): every V1 origin kind — project, release, state,
// object_version — is an entity of exactly one project, which is what makes
// "is this reference public?" answerable for all four kinds the same way.
type StoredRef struct {
	// Ref is the canonical origin ref this entry answers for.
	Ref OriginRef
	// Resolved is true when the ref names a stored row.
	Resolved bool
	// ProjectID is the project the referenced entity belongs to ("" while
	// unresolved).
	ProjectID string
	// ProjectVisibility is that project's visibility ("" while unresolved).
	ProjectVisibility Visibility
	// Object carries the object version's own fields when the ref is of
	// kind object_version and resolved.
	Object *StoredObject
}

// StoredObject is the object version an object_version ref points at.
type StoredObject struct {
	// ObjectVersionID is the scientific_object_versions row id (the ref's
	// own value).
	ObjectVersionID string
	// ObjectID is the scientific_objects row the version belongs to.
	ObjectID string
	// Title is the version's title, as the network would render it.
	Title string
}

// StoredBlob is one blob the manifest names, with the access it has right
// now. Access is the blob's attachment access (blob_attachments.
// access_level): the vocabulary internal/rights uses for the data-access
// axis, and the storage layer's own (docs/17 §3: a blob records its access
// policy; §5: every download checks it).
type StoredBlob struct {
	// BlobID is the blobs row id, in text form.
	BlobID string
	// Access is open when the blob is openly attached, restricted
	// otherwise — including a blob with no attachment at all: fail-closed,
	// because a blob nothing attaches is not reachable through any object.
	Access rights.DataAccess
}

// ImpactPreview is the whole answer: what the publication would expose, what it
// leans on that is not exposed, and why it cannot execute.
type ImpactPreview struct {
	// ProjectID is the project the publish was previewed in.
	ProjectID string `json:"project_id"`
	// Version is the version label the publish would store.
	Version string `json:"version"`
	// TargetVisibility is the visibility the publish would store — the value
	// that decides whether anything here becomes public at all.
	TargetVisibility Visibility `json:"target_visibility"`
	// Asset is the asset the version would be published under.
	Asset PreviewAsset `json:"asset"`
	// Facts is the publish checklist state (validation.AssetFacts, the facts
	// the ladder's asset gate reads), fact by fact.
	Facts PreviewFacts `json:"facts"`
	// Publishable is true when the publication could execute and expose
	// nothing hidden: no publish blocker, no rights blocker, and no blocking
	// private dependency.
	//
	// It is a verdict about this preview, NOT a permission. Authorization,
	// the explicit human action and the audit event belong to the publish
	// command (docs/23 §4, T0705); this flag says only "nothing this
	// repository can see today makes this publication unsafe".
	Publishable bool `json:"publishable"`
	// Objects are the object versions the publication would carry.
	Objects []PreviewObject `json:"objects"`
	// Metadata is the manifest's metadata block, ascending by key.
	Metadata []PreviewMetadata `json:"metadata"`
	// Blobs are the blobs the manifest names, in the order it names them.
	Blobs []PreviewBlob `json:"blobs"`
	// Refs are the origin refs, in the order the candidate declares them.
	Refs []PreviewRef `json:"refs"`
	// Dependencies are the dependency pins, in the order the manifest
	// declares them, each with the visibility its pinned version has today.
	//
	// Every pin is listed, not only the private ones, and that is the point
	// of the list: "no hidden private dependency leak" (docs/23 §4) is a
	// claim about all of them, and a reader can only check it by seeing that
	// each pin was resolved and what it resolved to. A pin that resolves to
	// nothing is listed too, with an empty visibility: it is the case where
	// the platform has nothing to report and the pin must not pass for a
	// public one.
	Dependencies []PreviewDependency `json:"dependencies"`
	// PrivateDependencies are the referenced things that are not public
	// today, named one by one: the pins and refs of the two lists above,
	// plus the blob ids.
	PrivateDependencies []PrivateDependency `json:"private_dependencies"`
	// PublishBlockers are the candidate's own refusals: the checks of
	// docs/11 §3 the publish gate finds broken, plus the resolutions that
	// failed (an unknown asset, a ref or pin that names no row).
	PublishBlockers []Blocker `json:"publish_blockers"`
	// RightsBlockers are what the rights declaration itself says — the
	// rights model's own refusals (internal/rights.ValidationError).
	RightsBlockers []Blocker `json:"rights_blockers"`
}

// PreviewAsset is the asset the version would be published under.
type PreviewAsset struct {
	// PID is the persistent identifier the candidate named.
	PID PID `json:"pid"`
	// AssetType is the type the candidate declared.
	AssetType Type `json:"asset_type"`
	// Resolved is true when the pid names a stored asset.
	Resolved bool `json:"resolved"`
	// Title is the stored asset's title ("" while unresolvable).
	Title string `json:"title"`
	// OriginProjectID is the project the stored asset belongs to ("" while
	// unresolvable).
	OriginProjectID string `json:"origin_project_id"`
}

// PreviewFacts mirrors validation.AssetFacts on the wire: the same facts,
// with the field names the rest of the API uses. It is a second structure
// on purpose — the ladder's type is an engine input with no transport tags,
// and putting tags on it would make a wire contract out of a validation
// struct — and the two are held together by a test that fails when a fact
// is added to the ladder and not here.
type PreviewFacts struct {
	SourcePinned   bool `json:"source_pinned"`
	VersionPinned  bool `json:"version_pinned"`
	Contributors   bool `json:"contributors"`
	Rights         bool `json:"rights"`
	Visibility     bool `json:"visibility"`
	DependencyPins bool `json:"dependency_pins"`
	IntegrityHash  bool `json:"integrity_hash"`
	Metadata       bool `json:"metadata"`
}

// PreviewObject is one object version the publication would carry, and the
// visibility it has today.
type PreviewObject struct {
	// ObjectVersionID is the scientific object version's row id.
	ObjectVersionID string `json:"object_version_id"`
	// ObjectID is the object the version belongs to.
	ObjectID string `json:"object_id"`
	// ProjectID is the project the object lives in.
	ProjectID string `json:"project_id"`
	// Title is the version's title: what a public page would show.
	Title string `json:"title"`
	// CurrentVisibility is the visibility of the object's project today.
	CurrentVisibility Visibility `json:"current_visibility"`
}

// PreviewMetadata is one metadata key of the manifest, with the value that
// would become public with it.
type PreviewMetadata struct {
	// Key is the metadata key.
	Key string `json:"key"`
	// Value is the declared value, verbatim.
	Value any `json:"value"`
}

// PreviewBlob is one blob the manifest names, with the access it has today
// and the access the version's documents promise for it.
//
// The two are different statements and the preview shows both, because
// "who would see what" needs them apart: CurrentAccess is the storage
// layer's answer (can the bytes be fetched today?), DeclaredAccess is what
// the published documents say (will a reader be told the bytes are open?).
// docs/17 §5 keeps exactly that separation — public metadata is not open
// download — and the pair is what makes the one blob contradiction
// visible when the two disagree.
type PreviewBlob struct {
	// BlobID is the identifier the manifest declared.
	BlobID string `json:"blob_id"`
	// Resolved is true when the identifier names a stored blob. A manifest
	// blob id is a declared string (specs/schemas/dataset.schema.json:
	// blob_ids is an array of strings), so an id this platform cannot look
	// up is reported rather than refused — it may be a foreign reference
	// (a DOI, a storage key), which is a valid declaration.
	Resolved bool `json:"resolved"`
	// CurrentAccess is the blob's access today: open when the blob is
	// openly attached, restricted otherwise, unknown while unresolved.
	CurrentAccess string `json:"current_access"`
	// DeclaredAccess is what the version being published promises about
	// its data: open when either document declares it, restricted
	// otherwise (the fail-closed default of internal/rights.New and of the
	// dataset schema's access_level).
	DeclaredAccess string `json:"declared_access"`
}

// Evaluation of a blob's access, as PreviewBlob.CurrentAccess renders it.
const (
	// BlobAccessOpen: some attachment of the blob records access_level =
	// 'open' (blob_attachments.access_level). That is what this value says
	// and all it says: it is a NECESSARY, not a sufficient, reading of
	// docs/17 §5's download gate, because the object's and the project's
	// access policy take no part in it (docs/12 §2: a private project's
	// blobs are invisible by default) — a blob openly attached only inside
	// a private project renders as open here. What the preview refuses on
	// this axis is the narrower contradiction: documents promising open
	// data access for bytes no attachment calls open.
	BlobAccessOpen = string(rights.DataAccessOpen)
	// BlobAccessRestricted: no attachment of the blob records 'open', so
	// nothing in the state says the bytes may be fetched. Fail-closed on
	// the one axis this value decides.
	BlobAccessRestricted = string(rights.DataAccessRestricted)
	// BlobAccessUnknown: the identifier names no stored blob, so the
	// platform has no access to report.
	BlobAccessUnknown = "unknown"
)

// PreviewRef is one origin ref of the candidate, resolved.
type PreviewRef struct {
	// Ref is the canonical ref, or the raw string when it is not canonical.
	Ref string `json:"ref"`
	// Kind is the ref's origin kind, "" when the ref is not canonical.
	Kind string `json:"kind"`
	// Resolved is true when the ref names a stored row.
	Resolved bool `json:"resolved"`
	// ProjectID is the project the referenced entity lives in ("" while
	// unresolved).
	ProjectID string `json:"project_id,omitempty"`
	// CurrentVisibility is that project's visibility ("" while unresolved).
	CurrentVisibility string `json:"current_visibility,omitempty"`
}

// PreviewDependency is one dependency pin of the manifest, resolved.
type PreviewDependency struct {
	// Pin is the canonical pid@version the manifest declared.
	Pin string `json:"pin"`
	// Resolved is true when the pin names a stored asset version.
	Resolved bool `json:"resolved"`
	// CurrentVisibility is the pinned version's visibility today, empty
	// while unresolved.
	CurrentVisibility string `json:"current_visibility,omitempty"`
}

// PrivateDependency is one referenced thing that is not public today, named:

// the pin, the ref or the blob id, the class it belongs to, and whether it
// blocks this publication.
//
// Blocking is about THIS publication and about nothing else. A private→public
// publication that names a private thing makes that name public inside an
// immutable document, so the default is fail-closed and it blocks. Three
// cases do not block, each for a stated reason:
//
//   - the candidate's target visibility is private: nothing becomes public,
//     so nothing leaks. The dependency is still named — it is what the
//     version leans on — but it blocks nothing.
//   - the ref points into the PUBLISHING project itself, and the publication
//     is public. The version schema requires at least one origin ref
//     (specs/schemas/research-asset-version.schema.json: origin_refs,
//     minItems 1) and docs/12 §2 sanctions exactly this combination: a
//     private project may explicitly publish an asset. A version carries the
//     object versions of its own project, so refusing this would make
//     publishing a private project's own work impossible. A ref into ANOTHER
//     private project is the case the rule exists for, and it blocks.
//   - a restricted blob under a rights declaration that also says
//     restricted. That combination is docs/55's RESTRICTED_BLOB — public
//     metadata, restricted data — and docs/17 §5 says public metadata is not
//     open download, so the publication claims nothing it cannot deliver.
type PrivateDependency struct {
	// Kind is the class of the named thing: "asset_version" (a dependency
	// pin), "blob" (a manifest blob id), or one of the four origin kinds
	// (project, release, state, object_version) for an origin ref.
	Kind string `json:"kind"`
	// Ref is the name the publication would carry: the canonical
	// pid@version of a pin, the canonical ref of an origin ref, the blob id
	// of a blob.
	Ref string `json:"ref"`
	// Blocking is true when this publication must not execute while this
	// dependency is in the state it is in.
	Blocking bool `json:"blocking"`
	// Detail is the sentence a human reads: what was found, and why it
	// blocks or does not.
	Detail string `json:"detail"`
}

// The dependency classes of PrivateDependency.Kind that are not origin kinds.
const (
	// DependencyAssetVersion: a dependency pin (docs/11 §5 Dependency: what
	// the version needs, which takes part in impact analysis).
	DependencyAssetVersion = "asset_version"
	// DependencyBlob: a blob id the manifest's metadata names.
	DependencyBlob = "blob"
)

// Blocker is one refusal, in the shape internal/assets.ValidationError and
// internal/rights.ValidationError share: a stable code, the field path it
// applies to, and a sentence naming what was found. The two blocker lists of
// ImpactPreview keep them apart by which model refused.
type Blocker struct {
	// Code is the stable refusal code: ASSET_* or PREVIEW_* for the
	// candidate, RIGHTS_* for the rights declaration.
	Code string `json:"code"`
	// Field is the JSON path of the offending field ("" for a rule about the
	// candidate or a document as a whole).
	Field string `json:"field"`
	// Detail names what was found.
	Detail string `json:"detail"`
}

// The preview's own refusals: the ones no document rule can make, because
// they need the current state. They are PREVIEW_* rather than ASSET_* for
// that reason — an ASSET_* code is a rule about the candidate's own bytes.
const (
	// CodePreviewAssetUnknown: the candidate's pid names no stored asset, so
	// there is no asset for the version to be published under.
	CodePreviewAssetUnknown = "PREVIEW_ASSET_UNKNOWN"
	// CodePreviewAssetTypeMismatch: the stored asset's type differs from the
	// type the candidate declares.
	CodePreviewAssetTypeMismatch = "PREVIEW_ASSET_TYPE_MISMATCH"
	// CodePreviewAssetProjectMismatch: the stored asset belongs to another
	// project than the one the publish is previewed in (docs/11 §2: assets
	// are published out of projects; a fork creates a new asset identity,
	// docs/11 §5).
	CodePreviewAssetProjectMismatch = "PREVIEW_ASSET_PROJECT_MISMATCH"
	// CodePreviewRefUnresolved: an origin ref names no stored row, so the
	// published document's provenance would point at nothing (docs/11 §3:
	// source accepted state/release).
	CodePreviewRefUnresolved = "PREVIEW_ORIGIN_REF_UNRESOLVED"
	// CodePreviewPinUnresolved: a dependency pin names no stored version, so
	// the published document would depend on a version nobody can resolve
	// (docs/11 §5).
	CodePreviewPinUnresolved = "PREVIEW_DEPENDENCY_UNRESOLVED"
)

// ErrIncompleteState reports a state that does not answer for every
// canonical origin ref of the candidate: a reader dropped one, and the
// preview will not guess whether the ref exists.
var ErrIncompleteState = errors.New("assets: preview state does not cover every origin ref")

// ImpactPreview builds the impact preview of one proposed publish against the
// current state.
//
// It returns an error only when the state cannot answer for the candidate
// (ErrIncompleteState) — everything else, including a candidate the gate
// refuses outright, comes back as an ImpactPreview with the refusals listed and the
// impact computed from whatever the documents do say. A preview exists for a
// human deciding whether to proceed, and "this cannot execute, here is why,
// and here is what it would have exposed anyway" is the answer that decision
// needs.
func Preview(req PreviewRequest, state CurrentState) (ImpactPreview, error) {
	c := req.Candidate
	out := ImpactPreview{
		ProjectID:           req.ProjectID,
		Version:             c.Version,
		TargetVisibility:    c.Visibility,
		Asset:               previewAsset(req.Candidate, state),
		Objects:             []PreviewObject{},
		Metadata:            []PreviewMetadata{},
		Blobs:               []PreviewBlob{},
		Refs:                []PreviewRef{},
		Dependencies:        []PreviewDependency{},
		PrivateDependencies: []PrivateDependency{},
		PublishBlockers:     []Blocker{},
		RightsBlockers:      []Blocker{},
	}

	// The checklist runs first and unchanged: Gate is the one enforcer of
	// docs/11 §3, and it returns its facts whether or not it refused.
	gateRes, gateErr := Gate(c)
	out.Facts = previewFacts(gateRes.Facts)

	// The rights document is read twice on purpose. Gate refuses a candidate
	// whose rights document does not parse (with its own ASSET_* code); the
	// rights model refuses it with RIGHTS_* codes and the path INSIDE the
	// document. The rights list carries the second, so a rights refusal is
	// reported once, by the model that owns the vocabulary, and the gate's
	// ASSET_INVALID_RIGHTS / ASSET_MISSING_RIGHTS is left out of the publish
	// list rather than repeated.
	rightsDoc, rightsErr := rights.Parse(c.RightsJSON)
	out.RightsBlockers = append(out.RightsBlockers, previewRightsBlockers(c.RightsJSON, rightsErr)...)

	manifest := gateRes.Manifest
	out.Metadata = previewMetadata(manifest)
	out.PublishBlockers = append(out.PublishBlockers, gateBlockers(gateErr)...)
	out.PublishBlockers = append(out.PublishBlockers, previewAssetBlockers(req, state)...)

	// Refs into a private project are the class docs/23 §4 is about; a
	// publication that stays private widens nothing.
	blockingTarget := c.Visibility == VisibilityPublic

	refState, err := indexRefs(c.OriginRefs, state.Refs)
	if err != nil {
		return ImpactPreview{}, err
	}
	for _, raw := range c.OriginRefs {
		st, canonical := refState[OriginRef(raw)]
		if !canonical {
			// Not a canonical ref: the gate refused its shape, so there is
			// nothing to resolve and nothing to look up.
			out.Refs = append(out.Refs, PreviewRef{Ref: raw})
			continue
		}
		out.Refs = append(out.Refs, previewRef(raw, st))
		if !st.Resolved {
			out.PublishBlockers = append(out.PublishBlockers, Blocker{
				Code:  CodePreviewRefUnresolved,
				Field: "origin_refs",
				Detail: "the ref " + quote(raw) + " names no stored entity; a published version's provenance " +
					"must resolve (docs/11 §3: source accepted state/release)",
			})
		} else if st.Object != nil {
			out.Objects = appendObject(out.Objects, *st.Object, st.ProjectID, st.ProjectVisibility)
		}
		if dep, named := refDependency(raw, st, req.ProjectID, blockingTarget); named {
			out.PrivateDependencies = append(out.PrivateDependencies, dep)
		}
	}

	// The dependency pins (docs/11 §5): what the version needs in order to be
	// reproduced, and the thing docs/23 §4's "no hidden private dependency
	// leak" is mostly about.
	pins := indexPins(state.Pins)
	for _, pin := range manifest.DependencyPins {
		st, resolved := pins[pin]
		entry := PreviewDependency{Pin: string(pin), Resolved: resolved}
		if resolved {
			entry.CurrentVisibility = string(st.Visibility)
		}
		out.Dependencies = append(out.Dependencies, entry)
		switch {
		case !resolved:
			out.PublishBlockers = append(out.PublishBlockers, Blocker{
				Code:  CodePreviewPinUnresolved,
				Field: "dependency_pins",
				Detail: "the pin " + quote(string(pin)) + " names no stored asset version; a published version " +
					"depends on versions that resolve (docs/11 §5)",
			})
			out.PrivateDependencies = append(out.PrivateDependencies, PrivateDependency{
				Kind:     DependencyAssetVersion,
				Ref:      string(pin),
				Blocking: true,
				Detail:   "the pinned version does not exist, so nobody can resolve this dependency",
			})
		case st.Visibility != VisibilityPrivate:
			// A visible pin is a dependency like any other: it is part of
			// what the published version needs, and it is already visible.
			continue
		case blockingTarget:
			out.PrivateDependencies = append(out.PrivateDependencies, PrivateDependency{
				Kind:     DependencyAssetVersion,
				Ref:      string(pin),
				Blocking: true,
				Detail: "the pinned asset version is private, so a public publication would point the network " +
					"at a version it cannot read (docs/23 §4: no hidden private dependency leak)",
			})
		default:
			out.PrivateDependencies = append(out.PrivateDependencies, PrivateDependency{
				Kind:   DependencyAssetVersion,
				Ref:    string(pin),
				Detail: "the pinned asset version is private; this publication would not widen it",
			})
		}
	}

	// The blobs the manifest names, and the access each has today.
	blobState := indexBlobs(state.Blobs)
	promised := declaredBlobAccess(manifest, rightsDoc)
	for _, id := range DeclaredBlobIDs(manifest) {
		blob, resolved := blobState[id]
		access := BlobAccessUnknown
		if resolved {
			access = string(blob.Access)
		}
		out.Blobs = append(out.Blobs, PreviewBlob{
			BlobID:         id,
			Resolved:       resolved,
			CurrentAccess:  access,
			DeclaredAccess: promised,
		})
		if resolved && access == BlobAccessOpen {
			continue
		}
		dep := PrivateDependency{
			Kind: DependencyBlob,
			Ref:  id,
			Detail: "the manifest names this blob and it is not openly attached, so its bytes stay behind the " +
				"download gate (docs/17 §5: public metadata is not open download)",
		}
		switch {
		case !blockingTarget:
			dep.Detail = "the manifest names this blob; this publication would not widen it"
		case promised == BlobAccessOpen:
			// The one case in which a blob blocks: the rights declaration
			// states open data access for blobs that are not open. The
			// version document is immutable, so the statement could never be
			// corrected afterwards.
			dep.Blocking = true
			dep.Detail = "the rights declaration states data_access=open while this blob is not open, so the " +
				"published version would state an access its own data does not have"
		}
		out.PrivateDependencies = append(out.PrivateDependencies, dep)
	}

	out.Publishable = len(out.PublishBlockers) == 0 && len(out.RightsBlockers) == 0 && !anyBlocking(out.PrivateDependencies)
	return out, nil
}

// previewAsset renders the asset half of the preview. It reports what the
// candidate named and what the state resolved it to; whether that is a
// refusal is previewAssetBlockers' business.
func previewAsset(c PublishCandidate, state CurrentState) PreviewAsset {
	out := PreviewAsset{PID: c.AssetPID, AssetType: c.AssetType}
	if state.Asset == nil {
		return out
	}
	out.Resolved = true
	out.Title = state.Asset.Title
	out.OriginProjectID = state.Asset.OriginProjectID
	return out
}

// previewAssetBlockers reports the three refusals that need the stored asset
// row: the pid names no asset, the asset is of another type than the
// candidate declares, or the asset belongs to another project than the one
// being published in.
//
// The third is the one worth stating: research_assets.origin_project_id is
// NOT NULL and docs/11 §2 publishes objects "out of a project", so a version
// of an asset is published in the asset's own project. A different project
// wants a different identity — that is what Fork/Derive is for (docs/11 §5:
// 创建新的 Asset/Object identity).
func previewAssetBlockers(req PreviewRequest, state CurrentState) []Blocker {
	c := req.Candidate
	if state.Asset == nil {
		return []Blocker{{
			Code:  CodePreviewAssetUnknown,
			Field: "asset_pid",
			Detail: "the pid " + quote(string(c.AssetPID)) + " names no stored asset; a version is published " +
				"under an asset that exists (docs/11 §2)",
		}}
	}
	var out []Blocker
	if state.Asset.Type != c.AssetType {
		out = append(out, Blocker{
			Code:  CodePreviewAssetTypeMismatch,
			Field: "asset_type",
			Detail: "the stored asset is a " + quote(string(state.Asset.Type)) + " while the candidate declares " +
				quote(string(c.AssetType)) + "; every version of an asset is of that asset's type",
		})
	}
	if state.Asset.OriginProjectID != req.ProjectID {
		out = append(out, Blocker{
			Code:  CodePreviewAssetProjectMismatch,
			Field: "project_id",
			Detail: "the stored asset belongs to project " + quote(state.Asset.OriginProjectID) + " while this " +
				"publish is previewed in " + quote(req.ProjectID) + "; an asset's versions are published in the " +
				"asset's own project (docs/11 §2)",
		})
	}
	return out
}

// previewRef renders one resolved ref.
func previewRef(raw string, st StoredRef) PreviewRef {
	kind, _, _ := ParseOriginRef(raw)
	out := PreviewRef{Ref: raw, Kind: string(kind), Resolved: st.Resolved}
	if st.Resolved {
		out.ProjectID = st.ProjectID
		out.CurrentVisibility = string(st.ProjectVisibility)
	}
	return out
}

// appendObject adds one carried object version to the objects list, once.
// The same object version referenced twice is one object — a repeated ref
// says nothing new about who sees what — and the first occurrence keeps the
// position, so the list follows the candidate's own order.
func appendObject(seen []PreviewObject, obj StoredObject, projectID string, visibility Visibility) []PreviewObject {
	for _, o := range seen {
		if o.ObjectVersionID == obj.ObjectVersionID {
			return seen
		}
	}
	return append(seen, PreviewObject{
		ObjectVersionID:   obj.ObjectVersionID,
		ObjectID:          obj.ObjectID,
		ProjectID:         projectID,
		Title:             obj.Title,
		CurrentVisibility: visibility,
	})
}

// refDependency reports whether one origin ref is something the publication
// leans on that the network cannot see today, and renders it. See
// PrivateDependency for why a ref into the publishing project does not block
// while a ref into another private project does.
//
// An unresolved ref is named whichever the target visibility is: a
// provenance pin that names nothing is a broken immutable document rather
// than a disclosure, and the fail-closed answer to "can this execute?" is
// no either way.
func refDependency(raw string, st StoredRef, publishingProject string, blockingTarget bool) (PrivateDependency, bool) {
	kind, _, _ := ParseOriginRef(raw)
	dep := PrivateDependency{Kind: string(kind), Ref: raw}
	if !st.Resolved {
		dep.Blocking = true
		dep.Detail = "the ref names no stored entity, so the published provenance would point at nothing " +
			"(docs/11 §3: source accepted state/release)"
		return dep, true
	}
	if st.ProjectVisibility != VisibilityPrivate {
		return PrivateDependency{}, false
	}
	switch {
	case !blockingTarget:
		dep.Detail = "the ref points into the private project " + quote(st.ProjectID) +
			"; this publication would not widen it"
	case st.ProjectID == publishingProject:
		dep.Detail = "the ref points into the private publishing project itself; a project may explicitly " +
			"publish an asset from its own private state (docs/12 §2), so this is what the version carries, " +
			"not a dependency on somebody else's private work"
	default:
		dep.Blocking = true
		dep.Detail = "the ref points into the private project " + quote(st.ProjectID) +
			", which is not the publishing project, so a public publication would disclose another project's " +
			"private entity (docs/23 §4: no hidden private dependency leak)"
	}
	return dep, true
}

// indexRefs matches every canonical ref of the candidate to the state's
// answer for it. A canonical ref the state does not answer for is
// ErrIncompleteState — the reader's contract, checked here rather than
// assumed.
func indexRefs(refs []string, state []StoredRef) (map[OriginRef]StoredRef, error) {
	byRef := make(map[OriginRef]StoredRef, len(state))
	for _, st := range state {
		byRef[st.Ref] = st
	}
	for _, raw := range refs {
		if _, _, ok := ParseOriginRef(raw); !ok {
			// Not canonical: the gate reports the shape, and there is no
			// entity it could name.
			continue
		}
		if _, ok := byRef[OriginRef(raw)]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrIncompleteState, raw)
		}
	}
	return byRef, nil
}

// indexPins indexes the resolved pins by their canonical form.
func indexPins(pins []StoredPin) map[DependencyPin]StoredPin {
	out := make(map[DependencyPin]StoredPin, len(pins))
	for _, p := range pins {
		out[p.Pin] = p
	}
	return out
}

// indexBlobs indexes the resolved blobs by blob id.
func indexBlobs(blobs []StoredBlob) map[string]StoredBlob {
	out := make(map[string]StoredBlob, len(blobs))
	for _, b := range blobs {
		out[b.BlobID] = b
	}
	return out
}

// previewMetadata renders the manifest's metadata block, ascending by key.
// The keys are sorted rather than left in Go's map order so that two
// previews of the same document render identically; the canonical manifest
// bytes sort them the same way (Manifest.CanonicalJSON), so what the preview
// shows is the order the stored document reads in.
func previewMetadata(m Manifest) []PreviewMetadata {
	keys := sortedKeys(m.Metadata)
	out := make([]PreviewMetadata, 0, len(keys))
	for _, k := range keys {
		out = append(out, PreviewMetadata{Key: k, Value: m.Metadata[k]})
	}
	return out
}

// blobIDsKey is the metadata key that names blobs. It is the dataset object
// schema's own field (specs/schemas/dataset.schema.json: blob_ids, an array
// of strings) and the key T0702's required-metadata table carries for a
// dataset, so a published version that names bytes names them here. It is
// read for EVERY asset type, not only dataset: the type's required set is a
// floor, and a protocol that declares its sample data the same way is
// declaring the same thing.
const blobIDsKey = "blob_ids"

// dataAccessKey is the dataset manifest's own access declaration. It is a
// required metadata key of the dataset type (requiredMetadata,
// specs/schemas/dataset.schema.json: access_level, open|restricted), so a
// published dataset states its data access twice: once here and once in
// the rights document's visibility.data_access (internal/rights). Both are
// statements in the immutable published documents, so both are read.
const dataAccessKey = "access_level"

// declaredBlobAccess returns what the version's documents promise about
// its data: open when EITHER of them says so, restricted otherwise.
//
// Two documents can make the promise — the rights declaration's data
// access axis, which is the model that owns the vocabulary
// (rights.DataAccessOpen), and the manifest's own access_level metadata,
// which the dataset schema requires as open|restricted — and a reader must
// take the more open of the two, because that is what a reuser is told
// either way. A publication whose promise is open while the bytes are not
// is the one contradiction the blob rule refuses.
func declaredBlobAccess(m Manifest, doc rights.Document) string {
	if doc.Visibility.DataAccess == rights.DataAccessOpen {
		return BlobAccessOpen
	}
	if level, ok := m.Metadata[dataAccessKey].(string); ok && level == BlobAccessOpen {
		return BlobAccessOpen
	}
	return BlobAccessRestricted
}

// DeclaredBlobIDs returns the blob identifiers a manifest declares, in the
// order it declares them, without repeats. Both shapes the metadata rule
// admits for a blob list are read — the array of strings the dataset schema
// specifies, and a bare string (the key is required for one type only, so a
// publisher on another type may write it as a single reference).
//
// It is exported because the state reader needs the same list: the preview
// renders the blobs a manifest names, and the reader has to look up exactly
// those (cmd/api/assetshttp). Two implementations of "which ids does this
// document name" would be two answers to one question, and the one that
// disagreed would be the one nobody read.
func DeclaredBlobIDs(m Manifest) []string {
	value, ok := m.Metadata[blobIDsKey]
	if !ok {
		return nil
	}
	var raw []string
	switch v := value.(type) {
	case string:
		raw = []string{v}
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok {
				raw = append(raw, s)
			}
		}
	default:
		return nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// previewFacts copies the ladder's facts into the transport shape.
func previewFacts(f validation.AssetFacts) PreviewFacts {
	return PreviewFacts{
		SourcePinned:   f.SourcePinned,
		VersionPinned:  f.VersionPinned,
		Contributors:   f.Contributors,
		Rights:         f.Rights,
		Visibility:     f.Visibility,
		DependencyPins: f.DependencyPins,
		IntegrityHash:  f.IntegrityHash,
		Metadata:       f.Metadata,
	}
}

// previewRightsBlockers renders what the rights declaration itself refuses.
//
// An absent document is named with the assets package's own
// ASSET_MISSING_RIGHTS, because that is the rule the publish checklist
// states ("rights/license must be set", docs/11 §3) and internal/rights has
// no code for "there is no document to read". Every other refusal is the
// rights model's own, with its own code and its field path inside the
// document — prefixed with "rights", which is the request field the document
// arrives in, so the path a client gets is the path into its own body.
func previewRightsBlockers(raw []byte, err error) []Blocker {
	if err == nil {
		return nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return []Blocker{{
			Code:  CodeMissingRights,
			Field: "rights",
			Detail: "the version must carry a rights declaration: a reusable asset without rights cannot be " +
				"reused lawfully (docs/11 §3)",
		}}
	}
	var ve *rights.ValidationError
	if errors.As(err, &ve) {
		return []Blocker{{Code: ve.Code, Field: rightsFieldPath(ve.Field), Detail: ve.Detail}}
	}
	return []Blocker{{Code: rights.CodeMalformedDocument, Field: "rights", Detail: err.Error()}}
}

// rightsFieldPath prefixes a rights document path with the request field it
// arrives in ("" becomes "rights", the document as a whole).
func rightsFieldPath(field string) string {
	if field == "" {
		return "rights"
	}
	return "rights." + field
}

// gateBlockers renders Gate's refusals as blockers, in the order the rules
// appended them (Gate joins one *ValidationError per broken rule, and a
// preview list needs all of them rather than the first one an errors.As
// happens to find).
//
// The two rights refusals are left out: the assets gate says "the rights
// document does not parse" (ASSET_MISSING_RIGHTS / ASSET_INVALID_RIGHTS),
// which internal/rights says again with its own RIGHTS_* code and the path
// INSIDE the document. Reporting both would list one broken declaration
// twice, and the rights model's version is the one that names the field the
// publisher has to fix — so the rights list carries it and this list does
// not repeat it.
func gateBlockers(err error) []Blocker {
	out := []Blocker{}
	walk(err, func(ve *ValidationError) {
		if ve.Code == CodeInvalidRights || ve.Code == CodeMissingRights {
			return
		}
		out = append(out, Blocker{Code: ve.Code, Field: ve.Field, Detail: ve.Detail})
	})
	return out
}

// walk calls fn for every *ValidationError inside err, in tree order. A
// joined error is descended into rather than reported as a whole: the
// refusal list a human reads is the list of rules, not the list of joins
// that carried them.
func walk(err error, fn func(*ValidationError)) {
	if err == nil {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, inner := range joined.Unwrap() {
			walk(inner, fn)
		}
		return
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		fn(ve)
	}
}

// anyBlocking reports whether any dependency blocks its publication.
func anyBlocking(deps []PrivateDependency) bool {
	for _, d := range deps {
		if d.Blocking {
			return true
		}
	}
	return false
}
