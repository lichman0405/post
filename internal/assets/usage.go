package assets

// This file is the recording rule for asset_dependencies (migration 00010):
// the row that says "this project uses that fixed asset version". Until this
// task the table had a reader (the asset page's used/derived public links,
// internal/assets/page.go) and no writer at all, so the platform could not
// answer "who uses this version" about anything it had stored.
//
// # What the table records, and which direction it points
//
// asset_dependencies(project_id, asset_version_id, dependency_type,
// visibility_of_usage, created_at) is keyed by the USING project and the USED
// version. It is therefore the reverse index of the asset page's
// dependencies block: the page renders what one version PINS (from its own
// manifest, Direction 1 — "本资产依赖谁"), and this table answers who uses
// one version (Direction 2 — "谁公开用了本资产", docs/42 §Asset Page's
// "used/derived public links"). docs/42_PAGE_SPECS.md:19 names both items,
// and the two directions are carried by two different stores on purpose: a
// pin is a fact about the publisher's document, a usage is a fact about a
// project's practice, and a project may use a version it never wrote into a
// manifest of its own — the table is the only place the second fact exists.
//
// # Where the recording happens
//
// A project DECLARES the versions it uses when it publishes: the manifest's
// dependency_pins (docs/11 §3, "dependency pin" on the publish checklist) are
// the project's own statement of the exact versions its work was built
// against, and the publish is the governed, audited, owner-level write
// (docs/23 §4) that carries that statement into the repository. So the
// publish records the usages its manifest declares, in the SAME transaction
// as the version row: a publish that fails leaves no usage behind, and a
// usage can never name a version row that does not exist — which is also why
// a pin that resolves to nothing records nothing (there is no row id to point
// at; the pin itself is still rendered as unresolved by the page, which is a
// fact about the publisher's document).
//
// The command computes the declarations (internal/application/assetpublish)
// and the store writes exactly what it was handed — the same split the
// publish already uses for its audit row and its candidate: the application
// layer decides, the transaction writes, and no rule about what may be
// recorded lives inside a SQL statement.
//
// # The type of a recorded usage
//
// Every pin is a dependency, never a citation. A pin names an exact version
// the project's own work was rebuilt from (internal/assets/dependency.go:
// "The pin is an exact version or it is not a pin"), and docs/11 §5 puts
// exactly that under Dependency — "项目复现/运行所需，参与 impact analysis"
// — rather than under Reference/Use. A manifest has no field that means
// "background knowledge": nothing in the document distinguishes a version
// that was consulted from one that was used as an input, so a citation is
// not expressible here and this rule does not invent one. The vocabulary
// admits both values (DependencyType) and the readers render whichever a
// stored row carries; a citation path, if V1 ever grows one, is a new
// declaration with a product decision behind it.
//
// # The visibility of a recorded usage
//
// visibility_of_usage is the PUBLISHED VERSION'S OWN visibility, and that is
// a derivation rather than a choice: the usage is the project's declaration,
// the declaration is carried by the version document (its manifest pins are
// inside the hashed bytes), and the document's visibility is the version's
// (migration 00010, research_asset_versions.visibility). A version published
// publicly declares its uses publicly; a version published privately declares
// them privately, and the row says so.
//
// It is also the only value the platform can take without a new input: the
// publish request's shape is the contract's (specs/api/openapi.yaml), so
// there is no field a client could set, and a value defaulted from nothing
// would be a disclosure nobody declared.
//
// This is a public-ness DECLARATION, not permission to render: the asset
// page keeps its own second condition on top of it (the using project must be
// public too — internal/assets.PageUsage), so a private project's public
// usage of someone else's version stays off that page, and no count of such
// rows is rendered anywhere. docs/23 §5 is the rule; V1 takes the option that
// line offers — "V1 可直接不显示 private count" — and renders no private
// count at all.

// UsageDeclaration is one asset_dependencies row a version declares when it
// is published: the project's use of one exact published version. It is a
// DECLARATION rather than a stored row — the pin still has to be resolved to
// the version row's id inside the writing transaction, and a declaration
// whose pin resolves to nothing is dropped there rather than written.
type UsageDeclaration struct {
	// Pin is the exact version used, in the canonical pid@version form
	// (DependencyPin). A declaration always names a version: "some version
	// of that asset" is not a pin and could not be recorded here.
	Pin DependencyPin
	// Type is how the project uses it. It is always a value of the
	// vocabulary (PublishedUsages produces DependencyTypeDependsOn);
	// carrying the type rather than letting the store assume one keeps the
	// decision in this package, where the rule and its citations are.
	Type DependencyType
	// VisibilityOfUsage is the declaration's own visibility: the published
	// version's (see the file comment).
	VisibilityOfUsage Visibility
}

// PublishedUsages returns the usages one publish declares: every dependency
// pin the version's manifest names, as a depends_on usage of the publishing
// project, at the version's own visibility.
//
// The list is DEDUPLICATED by pin. The publish gate refuses a manifest that
// lists one pin twice (validateDependencyPins, CodeDuplicateDependencyPin),
// so this is the second line rather than the first: a declaration list that
// named one version twice would write one row twice, and the caller would
// have to know the row's primary key to know that it is harmless.
//
// An EMPTY pin list yields an empty list, never nil: a version that depends
// on nothing declares no usages, and that is a fact ("this project uses
// nothing here") rather than an absence.
func PublishedUsages(pins []DependencyPin, visibility Visibility) []UsageDeclaration {
	out := make([]UsageDeclaration, 0, len(pins))
	seen := make(map[DependencyPin]bool, len(pins))
	for _, pin := range pins {
		if seen[pin] {
			continue
		}
		seen[pin] = true
		out = append(out, UsageDeclaration{
			Pin:               pin,
			Type:              DependencyTypeDependsOn,
			VisibilityOfUsage: visibility,
		})
	}
	return out
}
