package assets

import "time"

// The project-side read of the same table usage.go writes: "which fixed
// asset versions does this project depend on, and how" — docs/42 §Project
// Assets (published/dependencies) read against docs/42 §Asset Page, whose
// "dependencies" and "used/derived public links" name the two directions of
// one table.
//
// The two directions are read by two surfaces, and the split is deliberate.
// The asset page answers "who publicly uses THIS version" to the network, and
// its "dependencies" block answers "what does THIS version pin" from the
// version's own manifest. This read answers the third question — "what does
// THIS project use" — which neither of those can: they are keyed by an asset,
// and a project's dependency list is keyed by the project. It is the
// direction ROADMAP T0707 states as the feature ("project uses fixed asset
// version"), and it is where a project's own PRIVATE declarations are
// readable at all (the page renders a usage only when the using project is
// public, which for a private project is never).
//
// # Who may see which row
//
// Two rules, both borrowed rather than invented, and both fail-closed:
//
//  1. The used version must be one the network can open — public, in a
//     public project (mayLinkVersion, the rule the asset page's dependency
//     and lineage blocks already run). A row whose used version fails it is
//     DROPPED, not blanked: the pin's bytes are the identity of another
//     entity and its title is that entity's display name, so "there is a
//     dependency here, on something you may not see" would be exactly the
//     count docs/23 §5 forbids. The rule holds for every reader, members
//     included, because the entity it protects belongs to another project —
//     being inside the project that does the using says nothing about the
//     project being used.
//
//  2. The row's own visibility_of_usage. A non-member reading a public
//     project (docs/12 §2: a public project is readable by anyone) sees the
//     dependencies the project declared PUBLIC and nothing else — a private
//     usage is the project's own business, and rendering it would put an
//     unannounced practice on a public surface. A MEMBER sees its private
//     ones too: they are the project's own declarations, and a member may
//     already read the project itself.
//
// Neither rule counts anything: a dropped row leaves no entry, no
// placeholder and no number, so a reader cannot infer how many were dropped.

// ProjectDependencyState is one asset_dependencies row of one project, with
// the used version resolved: the raw answer, private rows included, which
// BuildProjectDependencies decides about.
type ProjectDependencyState struct {
	// Pin is the used version's canonical pid@version identity, built from
	// the stored rows (research_assets.pid and research_asset_versions.
	// version) rather than parsed out of a document.
	Pin   string
	Title string
	Type  Type
	// VersionVisibility is the used VERSION's stored visibility and
	// VersionProjectVisibility its asset's originating project's: both axes
	// of mayLinkVersion, which is what rule 1 above needs.
	VersionVisibility        Visibility
	VersionProjectVisibility Visibility
	// DependencyType is the stored dependency_type, verbatim. The column has
	// no CHECK (dependency_type.go: the vocabulary lives in Go), so a stored
	// value outside the vocabulary is possible and is rendered as what it
	// says — the reader is not the gate, and a repository state the platform
	// cannot produce is not a reason to hide a row.
	DependencyType string
	// VisibilityOfUsage is the stored visibility_of_usage: the using
	// project's own declaration (usage.go).
	VisibilityOfUsage Visibility
	CreatedAt         time.Time
}

// ProjectDependencyViewer is who is reading the project's dependency list,
// as the two rules need it: a member of the project, or not. It is
// deliberately one bit wide — the project read gate has already answered
// "may this caller read the project at all" (a caller who may not is
// answered the existence-hiding 404 by the transport), and the only question
// left is whether the caller is inside it.
type ProjectDependencyViewer struct {
	Member bool
}

// ProjectDependency is one rendered entry: a fixed asset version this project
// depends on, how it depends on it, and where the version can be read.
type ProjectDependency struct {
	// Pin is the exact version used, canonical pid@version (DependencyPin):
	// the same identity form the asset page renders a pin in, and the reason
	// a dependency is a statement about one immutable version rather than
	// about an asset.
	Pin   DependencyPin `json:"pin"`
	Title string        `json:"title,omitempty"`
	Type  Type          `json:"type,omitempty"`
	// URL is the used version's persistent web page (/assets/{pid}/{version},
	// internal/assets/url.go). It is present because the entry is rendered
	// only for a version the network can open (rule 1): a URL that answered
	// 404 would be a worse disclosure than none.
	URL string `json:"url,omitempty"`
	// DependencyType is the recorded kind — one of the two the vocabulary
	// admits, for every row this platform writes (usage.go).
	DependencyType DependencyType `json:"dependency_type"`
	// ImpactAnalysis is whether an upstream change to the used version must
	// trigger re-analysis of this project: the catalog's DependencyInference
	// answer for DependencyType (docs/19 §3, dependency_type.go), so a client
	// that has to act on the citation/dependency distinction reads it instead
	// of re-deriving the vocabulary.
	ImpactAnalysis bool `json:"impact_analysis"`
	// VisibilityOfUsage is the recorded visibility, which for a member's own
	// project is what tells a public declaration from an internal one.
	VisibilityOfUsage Visibility `json:"visibility_of_usage"`
	CreatedAt         time.Time  `json:"created_at"`
}

// ProjectDependencies is the read's whole answer. The list is always present
// — empty when the project has no dependency the viewer may see — so a client
// renders a stated absence rather than guessing at a missing field.
type ProjectDependencies struct {
	Dependencies []ProjectDependency `json:"dependencies"`
}

// BuildProjectDependencies renders one project's dependency list for one
// viewer. See the file comment for the two rules; entries that fail either
// are dropped without a trace, and the result is always a list (empty when
// the project depends on nothing the viewer may see), never nil.
func BuildProjectDependencies(rows []ProjectDependencyState, viewer ProjectDependencyViewer) ProjectDependencies {
	out := make([]ProjectDependency, 0, len(rows))
	for _, row := range rows {
		if !viewer.Member && row.VisibilityOfUsage != VisibilityPublic {
			continue
		}
		if !mayLinkVersion(row.VersionVisibility, row.VersionProjectVisibility) {
			continue
		}
		pid, version, ok := ParseDependencyPin(row.Pin)
		if !ok {
			// A stored pair that is not a canonical pin cannot name a version
			// this repository holds (every stored label is a valid label and
			// every stored pid is a valid pid), so there is nothing a reader
			// could open. Dropped rather than rendered with half an identity.
			continue
		}
		out = append(out, ProjectDependency{
			Pin:               DependencyPin(row.Pin),
			Title:             row.Title,
			Type:              row.Type,
			URL:               AssetVersionURL(pid, version),
			DependencyType:    DependencyType(row.DependencyType),
			ImpactAnalysis:    DependencyType(row.DependencyType).TriggersImpactAnalysis(),
			VisibilityOfUsage: row.VisibilityOfUsage,
			CreatedAt:         row.CreatedAt.UTC(),
		})
	}
	return ProjectDependencies{Dependencies: out}
}
