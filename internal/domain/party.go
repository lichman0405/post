package domain

import "strings"

// The party vocabulary (T0711): who an asset's governance roles name.
//
// docs/11 §6 is the whole specification — "分离 Rights Holder、Custodian、
// Maintainer、Creator、Contributor、Originating Project" — and the model's one
// hard rule is that a party is recorded as WHICH KIND of identity it is AND
// WHICH ROW, never as an id whose kind the reader has to guess. owner ruling
// L3-20260916-1 (tasks/decisions.md) states it for the rights holder: 权利人
// 可以是组织；人和组织分开记 — a record must distinguish a natural person from
// an organization rather than putting both in one id column.
//
// So a Party is the pair, and nothing in this package (or in the storage
// that mirrors it, infra/migrations/00082) admits a member id without a
// kind. The three kinds are the three identity tables the platform already
// has — 00002's users and organizations, and 00003's projects — and the
// vocabulary is closed: a kind that names none of them is refused rather
// than stored, because a reader that met one could not resolve it to
// anything.

// PartyKind names which identity table a party's ID is a row of. It is
// stored beside the id (asset_version_parties.party_kind,
// asset_rights_holder_events.holder_kind), and a stored pair is the only
// form in which a party exists.
type PartyKind string

const (
	// PartyUser is a natural person: a row of users (00002).
	PartyUser PartyKind = "user"
	// PartyOrganization is an organization: a row of organizations
	// (00002). It is a party in its own right and never a stand-in for
	// one of its members.
	PartyOrganization PartyKind = "organization"
	// PartyProject is a research project: a row of projects (00003). It is
	// what docs/11 §6 calls the Originating Project — neither a person nor
	// an organization, which is why the kind is not folded into either.
	PartyProject PartyKind = "project"
)

// Valid reports whether k is one of the three kinds.
func (k PartyKind) Valid() bool {
	switch k {
	case PartyUser, PartyOrganization, PartyProject:
		return true
	}
	return false
}

// ParsePartyKind reads a stored or caller-supplied kind. ok is false for
// anything outside the closed vocabulary — an unknown kind has no table to
// resolve against, so it is refused rather than carried.
func ParsePartyKind(s string) (PartyKind, bool) {
	k := PartyKind(strings.TrimSpace(s))
	return k, k.Valid()
}

// PartyIdentityKinds are the kinds a person-shaped identity may take: the
// two 00002 tables. It is the rights holder's vocabulary — a holder is a
// person or an organization (L3-20260916-1) — and it is NOT the whole
// PartyKind set, deliberately: docs/11 §6 lists Originating Project as its
// own role, and "a project holds the rights" is the conflation that
// separation exists to prevent.
func (k PartyKind) IsIdentity() bool {
	return k == PartyUser || k == PartyOrganization
}

// Party is one party: which kind of identity, and which row of it. Both
// fields are required; a Party with either half missing names nothing.
type Party struct {
	Kind PartyKind
	ID   string
}

// Valid reports whether the party names a kind AND an id.
func (p Party) Valid() bool {
	return p.Kind.Valid() && strings.TrimSpace(p.ID) != ""
}

// IsIdentity reports whether the party is a person or an organization (see
// PartyKind.IsIdentity).
func (p Party) IsIdentity() bool { return p.Kind.IsIdentity() }

// Role is one of the six governance roles of docs/11 §6, named exactly as
// the spec names them. They are six distinct values and not one "owner"
// column: Creator and Contributor are different facts about a version
// (authorship and participation, CLAUDE.md invariant 14 keeps the
// contribution ledger append-only and credit correctable), Rights Holder is
// ownership of the asset (and changes), and Originating Project is a
// project — not a party that can be a person.
type Role string

const (
	// RoleCreator is an author of the published version (docs/11 §3's
	// "creators/contributors" publish-gate item).
	RoleCreator Role = "creator"
	// RoleContributor is a participant in the version who is not its
	// author. It is a different role, not a lesser creator: the publish
	// declaration names both, and a contributor never becomes a creator by
	// being the only party recorded.
	RoleContributor Role = "contributor"
	// RoleCustodian is the party responsible for keeping the artifact
	// (docs/11 §6).
	RoleCustodian Role = "custodian"
	// RoleMaintainer is the party maintaining it (docs/11 §6).
	RoleMaintainer Role = "maintainer"
	// RoleRightsHolder is the party that holds the asset's rights. Unlike
	// the four above it is a fact about the ASSET, it CHANGES by an
	// append-only governance event, and it is stored in its own chain
	// (asset_rights_holder_events) rather than on a version.
	RoleRightsHolder Role = "rights_holder"
	// RoleOriginatingProject is where the asset came from: a project, read
	// from research_assets.origin_project_id (00010). It is stored there
	// and not here, so that one fact has one source.
	RoleOriginatingProject Role = "originating_project"
)

// Valid reports whether r is one of the six roles.
func (r Role) Valid() bool {
	switch r {
	case RoleCreator, RoleContributor, RoleCustodian, RoleMaintainer,
		RoleRightsHolder, RoleOriginatingProject:
		return true
	}
	return false
}

// VersionRoles are the roles that are facts about a published VERSION (the
// four docs/11 §6 names besides the rights holder and the originating
// project). They are the roles asset_version_parties admits: a version's
// credits are part of its permanent record, while ownership belongs to the
// asset and evolves by appending events.
func VersionRoles() []Role {
	return []Role{RoleCreator, RoleContributor, RoleCustodian, RoleMaintainer}
}

// IsVersionRole reports whether r is stored per version.
func (r Role) IsVersionRole() bool {
	switch r {
	case RoleCreator, RoleContributor, RoleCustodian, RoleMaintainer:
		return true
	}
	return false
}
