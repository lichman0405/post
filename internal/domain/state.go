package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// ProjectState is one immutable node of a project's RSG state history
// (canonical table project_states, docs/07 §5): a state snapshot identified
// by its content hash. Every semantic write produces a new state whose
// ParentStateID names the state it was built on, so the per-branch chain
// (branches.base_state_id → parent_state_id → …) is the traceable history
// the acceptance criteria require. The snapshot's member rows — scientific
// object versions, relation versions, evidence, blob attachments — carry
// the state id they were created in (their state_id column), so a state's
// content is always enumerable from the id alone.
//
// Rows are never updated or deleted: the database rejects it itself
// (migrations 00014/00015) and the repository surface offers no update
// path (docs/21 §4).
type ProjectState struct {
	// ID is the uuid v4 text form (matches the project_states.id uuid
	// column).
	ID string
	// ProjectID is the research boundary the state belongs to.
	ProjectID string
	// BranchID names the research branch the state was committed on; nil
	// for the project's genesis state, which predates any branch
	// (state_commits.branch_id is NOT NULL, so the genesis root has no
	// commit of its own — see CreateInitialState).
	BranchID *string
	// ParentStateID names the state this one was built on; nil only for
	// the genesis root.
	ParentStateID *string
	// StateHash is the content address of the state: sha256 over the
	// canonical JSON of {parent_state_id, operations} (ComputeStateHash).
	// Unique per project, so identical transitions collide instead of
	// duplicating a state.
	StateHash string
	// GitCommitSHA pins the GitProvider commit this state corresponds to
	// when the transition came through git_compat; nil otherwise.
	GitCommitSHA *string
	// ManifestVersion is the manifest format version this state was
	// written under (the rsg-manifest schema's format_version; T0206
	// exports the full manifest).
	ManifestVersion string
	CreatedAt       time.Time
}

// StateVia is the channel a state commit was made through
// (state_commits.via CHECK). The six values are the canonical set of
// docs/15 §5: web UI, the REST API, MCP tools, Claude Code, the
// git-compatibility path and internal system actors (background workers,
// provisioning).
type StateVia string

const (
	ViaWeb        StateVia = "web"
	ViaAPI        StateVia = "api"
	ViaMCP        StateVia = "mcp"
	ViaClaudeCode StateVia = "claude_code"
	ViaGitCompat  StateVia = "git_compat"
	ViaSystem     StateVia = "system"
)

// ValidStateVia reports whether v is one of the six canonical channels.
func ValidStateVia(v StateVia) bool {
	switch v {
	case ViaWeb, ViaAPI, ViaMCP, ViaClaudeCode, ViaGitCompat, ViaSystem:
		return true
	}
	return false
}

// StateOperationKind names one semantic write in a state commit's operation
// summary (docs/09 §2: a commit lists objects created/updated/aborted,
// relations changed, files attached, schema/policy refs, actor/via,
// message). The vocabulary covers the canonical write kinds that exist in
// the V1 data model; an unknown kind is rejected before storage, so the
// operation_summary of every stored commit is always parseable and
// interpretable.
type StateOperationKind string

const (
	// OperationObjectVersionCreated: a new scientific object version row
	// was written (create object + v1 or a next version).
	OperationObjectVersionCreated StateOperationKind = "object_version_created"
	// OperationRelationVersionCreated: a new relation version row was
	// written (create relation + v1 or a next version).
	OperationRelationVersionCreated StateOperationKind = "relation_version_created"
	// OperationEvidenceAsserted: a new evidence assertion was written.
	OperationEvidenceAsserted StateOperationKind = "evidence_asserted"
	// OperationBlobAttached: a blob was attached to an object version.
	OperationBlobAttached StateOperationKind = "blob_attached"
	// OperationPolicyApplied: a visibility/rights policy reference was
	// applied to a version or state member.
	OperationPolicyApplied StateOperationKind = "policy_applied"
	// OperationSchemaReferenceSet: a schema reference was set on a
	// version (schema_id/schema_version of an object version).
	OperationSchemaReferenceSet StateOperationKind = "schema_reference_set"
)

// ValidStateOperationKind reports whether k is one of the canonical
// operation kinds.
func ValidStateOperationKind(k StateOperationKind) bool {
	switch k {
	case OperationObjectVersionCreated, OperationRelationVersionCreated,
		OperationEvidenceAsserted, OperationBlobAttached,
		OperationPolicyApplied, OperationSchemaReferenceSet:
		return true
	}
	return false
}

// StateOperation is one entry of a state commit's operation summary: the
// semantic write the transition consists of, in the order it happened.
// Fields are exported with fixed json tags because the marshaled form is
// stored verbatim (state_commits.operation_summary) and feeds
// ComputeStateHash: the field order below is the canonical serialization.
type StateOperation struct {
	// Kind names the write (StateOperationKind).
	Kind StateOperationKind `json:"kind"`
	// EntityID names the entity the write acted on: the object id for
	// object_version_created, the relation id for relation_version_created,
	// the assertion id for evidence_asserted, the version id for
	// blob_attached, the policy id for policy_applied, the schema $id for
	// schema_reference_set.
	EntityID string `json:"entity_id"`
	// VersionNo is the version number the write created, 0 when the kind
	// has no version dimension (evidence, blob, policy, schema).
	VersionNo int `json:"version_no,omitempty"`
	// Detail carries kind-specific context (e.g. the relation type for a
	// relation version); nil or an empty object when none. Content is
	// stored exactly as given.
	Detail json.RawMessage `json:"detail,omitempty"`
}

// StateCommit is the immutable record of one RSG state transition
// (canonical table state_commits, docs/03: "一次 RSG state transition 的
// 不可变记录"): who did it, through which channel, why (message), what it
// consisted of (operation summary), from which base state to which result
// state. Every semantic write lands in exactly one commit — the commit row
// and the member rows it describes are written in one transaction, so a
// traceable transition can never be observed without its members and vice
// versa.
type StateCommit struct {
	// ID is the uuid v4 text form of the commit row.
	ID string
	// ProjectID is the research boundary the transition belongs to.
	ProjectID string
	// BranchID names the branch the transition happened on. NOT NULL in
	// the canonical schema: the genesis root has no commit (a state
	// commit without a branch cannot be represented, so the empty root is
	// created by CreateInitialState instead).
	BranchID string
	// BaseStateID names the state the transition was built on (the
	// expected branch head at commit time); nil only when the branch had
	// no head yet.
	BaseStateID *string
	// ResultStateID names the state the transition produced (the commit's
	// own snapshot node).
	ResultStateID string
	// ActorID names the user who caused the transition. System-internal
	// transitions still name a resolved user (e.g. the project owner for
	// provisioning writes).
	ActorID string
	// Via is the channel the transition came through.
	Via StateVia
	// Message is the human-written commit message; never empty.
	Message string
	// OperationSummary is the ordered list of semantic operations the
	// transition consists of, as canonical JSON ([]StateOperation).
	OperationSummary json.RawMessage
	CreatedAt        time.Time
}

// ComputeStateHash derives the content address of a state from its
// transition content: sha256 hex over the canonical JSON of
// {"parent_state_id": "<uuid|null>", "operations": [StateOperation…]}.
//
// Canonical means deterministic across runs: fields in fixed order (this
// function builds the bytes from the structs, never from caller JSON),
// no insignificant whitespace, a nil parent rendered as null (not absent),
// operations in the order they happened. Two commits with the same parent
// and the same operations therefore always derive the same hash, which
// collides on project_states (UNIQUE(project_id, state_hash)) instead of
// silently duplicating a state — the store reports that collision as
// ErrStateExists. T0206's manifest hash extends this content address to
// the full snapshot; this hash stays the V1 state identity.
func ComputeStateHash(parentStateID *string, ops []StateOperation) (string, error) {
	payload := struct {
		ParentStateID *string          `json:"parent_state_id"`
		Operations    []StateOperation `json:"operations"`
	}{parentStateID, ops}
	// A nil slice marshals to null; canonical form is the empty array,
	// and the genesis hash is computed from an explicit empty slice.
	if payload.Operations == nil {
		payload.Operations = []StateOperation{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
