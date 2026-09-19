package contribution

import (
	"strings"
	"time"
)

// Credit attribution and credit disputes (T0809; docs/13 §2, §3).
//
// docs/13 §2 states the attribution half verbatim: "Asset/Finding/Release
// 可声明 creators/major contributors/method designer 等高层 credit。它可以
// 被纠正，但 correction 作为新 event，旧 attribution 和 dispute history
// 保留。" docs/13 §3 states the dispute half: "Contributor 可发起 dispute，
// 附带 ledger evidence；Maintainer/Organization governance 处理。不得直接
// 改原事件。Dispute 本身不参与公共 reputation 排名直到 resolution."
//
// # What this file owns, and what it deliberately does not
//
// It owns the VOCABULARY the two sentences need and nothing else: which
// credit roles a declaration may name, which targets a credit or a dispute
// is about and how such a target is spelled, and which states a dispute's
// current-state row may hold. It holds no scoring, no weighting and no
// derivation of credit from the ledger — docs/13 §4 forbids a single
// reputation score, §6 refuses raw counts as quality proxies, and CLAUDE.md
// §9 invariant 13 forbids a Truth Score. A declaration records what a
// person declared; it is not computed from anything.
//
// Storage lives in migration 00103 (credit_attribution_statements and
// credit_attribution_parties, both append-only by the 00014 row guard and
// the 00015 statement guard) and in 00011's credit_disputes current-state
// row; the transaction lives in the store
// (internal/persistence/credit_store.go) and the command in
// internal/application/credit.
//
// # The credit role vocabulary is two values, and the omission is a decision
//
// docs/13 §2 names three roles and leaves the list open with 等: creators /
// major contributors / method designer. This vocabulary admits exactly the
// two the task requires and REFUSES every other value — including
// "method_designer" and anything the 等 could later name. That refusal is
// deliberate (fail closed on the undecided end of an open list) and it is
// the same set migration 00103's CHECK admits, so the Go vocabulary and the
// database cannot disagree: widening either is the change that ratifies a
// product decision, and widening the CHECK is a migration.
//
// Both spellings are already in this repository and neither is invented:
// "creator" is asset_version_parties' role (00082, from docs/11 §6) and is
// spelled identically here so the two surfaces cannot disagree about the
// word, and "major_contributor" is the task requirement's own term.
//
// # The target ref shape is the ledger's, not a second one
//
// A credit declaration and a dispute both address one target with the
// canonical "kind:value" text ref the repository already spells in
// research_asset_versions.origin_refs (00064) and in the ledger's
// object_refs. The kinds are docs/13 §2's own three — asset, release,
// finding — and each value is the identity that kind is addressed by:
// an asset's pid (the identity a citation resolves, not its row id), a
// release's row id, a finding's row id.
//
// # The dispute state machine is docs/13 §3's, and it is terminal
//
// A dispute is born open, and it closes exactly once, as resolved or
// rejected (00011's CHECK). "Dispute 本身不参与公共 reputation 排名直到
// resolution" is why the state matters at all; "不得直接改原事件" is why a
// closed dispute is never re-opened or re-decided here — a later claim is a
// new dispute, the same rule §2 applies to an attribution correction.
// Migration 00103's credit_dispute_state_guard enforces exactly this at the
// database, for every write path.

// CreditRole is one role a high-level credit declaration may name for a
// party (credit_attribution_parties.role CHECK).
type CreditRole string

const (
	// CreditRoleCreator is "creator": the person or organization that
	// wrote the thing. The spelling is asset_version_parties' own
	// (migration 00082, docs/11 §6), not a second opinion about the word.
	CreditRoleCreator CreditRole = "creator"
	// CreditRoleMajorContributor is "major contributor": a party whose
	// contribution the declarer holds to be major. What makes a
	// contribution "major" is a scientific-semantics decision the specs
	// do not make (docs/13 §2's list ends in 等), so nothing here computes
	// or infers it: the role records a declaration someone made, and the
	// declarer is recorded beside it (credit_attribution_statements.
	// recorded_by).
	CreditRoleMajorContributor CreditRole = "major_contributor"
)

// creditRoles is the vocabulary in declaration order — the same set
// migration 00103's CHECK admits.
var creditRoles = []CreditRole{CreditRoleCreator, CreditRoleMajorContributor}

// ValidCreditRole reports whether r is one of the two admitted roles. A
// role outside them is refused rather than stored: an unratified credit
// title in an append-only table is a row no later reader can repair.
func ValidCreditRole(r CreditRole) bool {
	for _, v := range creditRoles {
		if v == r {
			return true
		}
	}
	return false
}

// CreditRoles returns the vocabulary, in declaration order. A copy: the
// caller cannot reorder or extend the set the database admits.
func CreditRoles() []CreditRole {
	out := make([]CreditRole, len(creditRoles))
	copy(out, creditRoles)
	return out
}

// CreditTargetKind names what a credit declaration or a dispute is about
// (credit_attribution_statements.target_kind CHECK; docs/13 §2's
// "Asset/Finding/Release").
type CreditTargetKind string

const (
	// CreditTargetAsset is a research asset, addressed by its persistent
	// identifier (research_assets.pid) — the identity a citation resolves,
	// not the row id.
	CreditTargetAsset CreditTargetKind = "asset"
	// CreditTargetRelease is a release (releases.id).
	CreditTargetRelease CreditTargetKind = "release"
	// CreditTargetFinding is a finding (findings.version_id).
	CreditTargetFinding CreditTargetKind = "finding"
)

// creditTargetKinds is the vocabulary in declaration order — the same set
// migration 00103's CHECK admits.
var creditTargetKinds = []CreditTargetKind{CreditTargetAsset, CreditTargetRelease, CreditTargetFinding}

// ValidCreditTargetKind reports whether k is one of the three targets
// docs/13 §2 names. Anything else — a scientific object, a pull request, a
// project — is refused: the sentence lists three, and an open-ended read
// of it would let a declaration attach credit to a target nothing defines
// the credit semantics of.
func ValidCreditTargetKind(k CreditTargetKind) bool {
	for _, v := range creditTargetKinds {
		if v == k {
			return true
		}
	}
	return false
}

// NewCreditTargetRef renders the canonical "kind:value" ref a declaration
// or a dispute stores in target_ref, reporting false when the pair is not
// a reference at all: an unknown kind, an empty value, or a value carrying
// the separator (which would make the ref parse as a different kind).
//
// The same shape migration 00103's credit_attribution_statements_target_ref_kind
// CHECK requires, so a ref this function refuses cannot be stored by
// another path either.
func NewCreditTargetRef(kind CreditTargetKind, value string) (string, bool) {
	if !ValidCreditTargetKind(kind) {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" || strings.Contains(value, ":") {
		return "", false
	}
	return string(kind) + ":" + value, true
}

// ParseCreditTargetRef splits a canonical ref into its kind and value. ok
// is false for a ref that is not "kind:value" with a known kind and a
// non-empty value — the same predicate the writing command applies before
// it stores one, so a reader may trust every ref it can parse.
//
// A value carrying surrounding whitespace is refused rather than trimmed:
// the function's contract is that a ref it accepts round-trips through
// NewCreditTargetRef unchanged, and silently normalizing would let two
// spellings of one ref mean the same thing to the parser while comparing
// unequal everywhere they are stored.
func ParseCreditTargetRef(ref string) (CreditTargetKind, string, bool) {
	kind, value, found := strings.Cut(ref, ":")
	if !found {
		return "", "", false
	}
	k := CreditTargetKind(kind)
	if !ValidCreditTargetKind(k) || value == "" || strings.Contains(value, ":") || value != strings.TrimSpace(value) {
		return "", "", false
	}
	return k, value, true
}

// DisputeState is one state of a credit dispute's current-state row
// (credit_disputes.state CHECK, 00011).
type DisputeState string

const (
	// DisputeStateOpen: the dispute has been raised and is not decided.
	// It is the only state a dispute can be born in (00103's
	// credit_dispute_state_guard), because a dispute that started decided
	// would have no opening and the credit.dispute_opened event the
	// ledger records could not exist.
	DisputeStateOpen DisputeState = "open"
	// DisputeStateResolved: governance decided for the claim.
	DisputeStateResolved DisputeState = "resolved"
	// DisputeStateRejected: governance decided against the claim. It is a
	// distinct state rather than an absence, because "the claim was heard
	// and refused" and "the claim is still open" are different facts
	// about the credit.
	DisputeStateRejected DisputeState = "rejected"
)

// ValidDisputeState reports whether s is one of the three states 00011's
// CHECK admits.
func ValidDisputeState(s DisputeState) bool {
	return s == DisputeStateOpen || s == DisputeStateResolved || s == DisputeStateRejected
}

// Terminal reports whether s is a decided state — one a dispute never
// leaves. Both terminals are decisions: resolved and rejected differ in
// outcome, not in finality (docs/13 §3's dispute is decided once; a later
// claim is a new dispute).
func (s DisputeState) Terminal() bool {
	return s == DisputeStateResolved || s == DisputeStateRejected
}

// CreditAttribution is one credit declaration as read back: the target it
// is about, the parties it names per role, who declared it and when.
//
// It is the row of credit_attribution_statements joined with its
// credit_attribution_parties children. A target has a CHAIN of these
// (ordinal 1, 2, 3 …) and nothing ever revises one: the current credit for
// a target is its greatest-ordinal statement, and every earlier one stays
// readable — which is what docs/13 §2's "它可以被纠正，但 correction 作为
// 新 event，旧 attribution ... 保留" asks for, and what the acceptance
// "修正不改 ledger 原事件" is checked through.
type CreditAttribution struct {
	// ID is the credit_attribution_statements row id.
	ID string
	// ProjectID is the research boundary the declaration belongs to.
	ProjectID string
	// TargetKind and TargetRef name what the declaration is about; Ref is
	// the canonical "kind:value" form the column stores.
	TargetKind CreditTargetKind
	TargetRef  string
	// Ordinal is this declaration's position in the target's chain, 1 for
	// the first. It is a per-target sequence, not a timestamp: two
	// declarations in one transaction share now() exactly, and "the
	// declaration this one corrects" would otherwise be a tie-break nobody
	// declared.
	Ordinal int
	// Parties are the declared parties, in the order the declarer sent
	// them, per role.
	Parties []CreditParty
	// RecordedBy is the actor whose request declared the credit, and
	// RecordedAt when the row was written. A declaration is attributable
	// or it is not stored (the column is NOT NULL).
	RecordedBy string
	RecordedAt time.Time
}

// CreditParty is one declared party with the role it is declared under:
// which kind of identity, which row of it, and where in the declaring
// caller's list it stood.
//
// The party is named by kind AND id for the reason asset_version_parties
// is (00082's header): the id points into users for one kind and
// organizations for the other, so it cannot be a single-column foreign
// key, and the writing command resolves it against the table its kind
// names before storing it. The kind and the id are the identity; Handle
// and DisplayName are what a surface renders, filled by a reader that
// resolved them and empty when it did not.
type CreditParty struct {
	Party PartyRef
	// Role is the credit role this party is declared under.
	Role CreditRole
	// Position is the party's index in the declarer's list for its role.
	Position int
	// Handle is a user's handle or an organization's slug, and
	// DisplayName the matching display name — filled from the table the
	// kind names when a read resolved them, never guessed from the other
	// kind's shape.
	Handle      string
	DisplayName string
}

// PartyRef is a party identity: which kind, and which row. It is the pair
// credit_attribution_parties stores as (party_kind, party_id), and the
// same pair assetrights.PartyIdentity carries — kept as its own small type
// here so this package does not depend on an application package for a
// two-field identity.
type PartyRef struct {
	// Kind is one of the identity kinds ("user", "organization"). A
	// project is not a credit party: docs/13 §2 names people-shaped roles,
	// and the originating project is already a column on research_assets.
	Kind string
	// ID is the uuid of the row kind names, in text form.
	ID string
}

// CreditDispute is one credit dispute: the current-state row 00011 created
// and migration 00103 guards, plus the ledger evidence the claim carries.
//
// The row's STATE is what a surface reads; the HISTORY is not in this
// struct and never will be — it is the credit.dispute_opened /
// credit.dispute_resolved events (specs/events/event-types.yaml) that the
// Contribution Ledger projects into the append-only contribution_events,
// and closing a dispute writes an event rather than editing a record of
// one. See internal/application/credit's doc comment.
type CreditDispute struct {
	// ID is the credit_disputes row id.
	ID string
	// ProjectID is the research boundary the dispute was raised in. 00011
	// leaves the column nullable; nothing this package drives writes a
	// dispute without one, because the project is what the actor's
	// membership — and therefore the authorization — is resolved against.
	ProjectID string
	// OpenedBy is the user who raised the dispute.
	OpenedBy string
	// TargetKind and TargetRef name the credit the dispute is about, in
	// the same canonical shape a declaration uses and against the same
	// three kinds (ParseCreditTargetRef).
	TargetKind CreditTargetKind
	TargetRef  string
	// Claim is the dispute's claim: what the opener says is wrong with the
	// credit, in their words. Immutable once written (00103's guard) —
	// docs/13 §3's "不得直接改原事件" is exactly this, a resolution
	// answers the claim and never edits it.
	Claim string
	// State is the dispute's current state.
	State DisputeState
	// Resolution is governance's decision, empty while the dispute is
	// open and non-empty once it is closed.
	Resolution string
	// OpenedAt is when the dispute was raised, ResolvedAt when it was
	// closed (nil while open).
	OpenedAt   time.Time
	ResolvedAt *time.Time
	// EvidenceRefs are the ledger references the claim points at, in the
	// canonical "kind:value" form — docs/13 §3's "附带 ledger evidence".
	// 00011's table has no column for them and this task may not add one
	// to a table its own catalog test pins, so they travel with the
	// credit.dispute_opened event, which research_events keeps verbatim;
	// a read that wants them reads the ledger.
	EvidenceRefs []string
}

// The audit action names these commands write. They live HERE rather than
// in internal/domain/audit.go, where every other action name lives,
// because internal/domain is not in this task's writable scope; the
// placement is named in the task RESULT as a follow-up (the constant and
// its doc travel, nothing else).
//
// The names follow that file's `<subject>.<verb-past>` convention. The two
// dispute names spell the same dotted names as the research EVENTS the same
// commands emit (credit.dispute_opened / credit.dispute_resolved,
// specs/events/event-types.yaml) — the coincidence of two separate
// registries internal/domain/audit.go records beside
// ActionPullRequestMerged and ActionProjectMainFrozen: subscribers route on
// events, the Activity page reads audit actions, and neither list is
// derived from the other. credit.attribution_declared has no event beside
// it: the event vocabulary is the spec's and lists no declaration event, so
// a declaration that needs a new name must get it from the spec rather than
// from this file.
const (
	// AuditActionCreditAttributionDeclared: one row per credit declaration
	// written, in the same transaction as the append-only statement row it
	// records. It is the record of WHO declared which credit for which
	// target — the declaration rows say what the credit now is, and this
	// says who said so.
	AuditActionCreditAttributionDeclared = "credit.attribution_declared"
	// AuditActionCreditDisputeOpened: one row per dispute raised, in the
	// same transaction as the credit_disputes row itself. docs/26 §5 lists
	// credit dispute among the high-risk actions whose audit must be
	// append-only, which audit_log already is by trigger (00014/00015).
	AuditActionCreditDisputeOpened = "credit.dispute_opened"
	// AuditActionCreditDisputeResolved: one row per dispute closed, whether
	// it was resolved or rejected — the two outcomes are one act (governance
	// deciding a claim) and the outcome itself is on the row and in the
	// audit Metadata, beside the credit.dispute_resolved event.
	AuditActionCreditDisputeResolved = "credit.dispute_resolved"
)
