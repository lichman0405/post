package validation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rsg/schemareg"
)

const (
	hypSchemaID   = schemareg.CanonicalNamespace + "hypothesis.schema.json"
	assetSchemaID = schemareg.CanonicalNamespace + "research-asset-version.schema.json"
)

func newTestValidator(t *testing.T) *Validator {
	t.Helper()
	reg, err := schemareg.New()
	if err != nil {
		t.Fatalf("schemareg.New: %v", err)
	}
	return NewValidator(reg)
}

// payloadHash is the sha256 of the stored payload bytes — the same
// computation the store performs at insert (docs/23 §3).
func payloadHash(p json.RawMessage) string {
	sum := sha256.Sum256(p)
	return hex.EncodeToString(sum[:])
}

// summaryJSONRaw renders an operation summary the way the store persists it
// (canonical []StateOperation JSON).
func summaryJSONRaw(ops ...domain.StateOperation) json.RawMessage {
	b, _ := json.Marshal(ops)
	return b
}

// completeHypothesisPayload satisfies the hypothesis schema (statement,
// question_id required) and the hypothesis row of the docs/08 domain-field
// table (hypothesis_type, scope).
func completeHypothesisPayload() json.RawMessage {
	b, _ := json.Marshal(map[string]any{
		"hypothesis_type": "mechanistic",
		"question_id":     "q-1",
		"scope":           map[string]any{"detail": "probe"},
		"statement":       "probe",
	})
	return b
}

// completeChain builds a two-state branch chain (base genesis outside the
// branch set, then st-1 -> st-2) with one complete hypothesis member that
// passes every main-gate check.
func completeChain() Snapshot {
	branch := "br-1"
	now := time.Now().UTC()
	st1 := domain.ProjectState{ID: "st-1", ProjectID: "proj-1", BranchID: &branch, ParentStateID: strPtr("st-gen"), StateHash: "h1", CreatedAt: now}
	st2 := domain.ProjectState{ID: "st-2", ProjectID: "proj-1", BranchID: &branch, ParentStateID: strPtr("st-1"), StateHash: "h2", CreatedAt: now}
	c1 := domain.StateCommit{
		ID: "c-1", ProjectID: "proj-1", BranchID: branch,
		BaseStateID: strPtr("st-gen"), ResultStateID: "st-1",
		ActorID: "alice", Via: domain.ViaAPI, Message: "first",
		OperationSummary: summaryJSONRaw(domain.StateOperation{Kind: domain.OperationObjectVersionCreated, EntityID: "object-1", VersionNo: 1}),
		CreatedAt:        now,
	}
	c2 := domain.StateCommit{
		ID: "c-2", ProjectID: "proj-1", BranchID: branch,
		BaseStateID: strPtr("st-1"), ResultStateID: "st-2",
		ActorID: "alice", Via: domain.ViaAPI, Message: "second",
		OperationSummary: summaryJSONRaw(domain.StateOperation{Kind: domain.OperationPolicyApplied, EntityID: "pol-1"}),
		CreatedAt:        now,
	}
	payload := completeHypothesisPayload()
	mv := domain.ScientificObjectVersion{
		ID: "v-1", ObjectID: "object-1", VersionNo: 1, StateID: "st-1",
		BranchID: &branch, SchemaID: hypSchemaID, SchemaVersion: "1",
		Title: "H1", LifecycleState: domain.LifecycleActive,
		Payload: payload, VisibilityPolicyID: strPtr("pol-1"),
		IntegrityHash: payloadHash(payload),
		CreatedBy:     "alice", CreatedAt: now,
	}
	return Snapshot{
		ProjectID:      "proj-1",
		BranchID:       branch,
		Head:           &st2,
		States:         []domain.ProjectState{st1, st2},
		Commits:        []domain.StateCommit{c1, c2},
		ObjectVersions: []domain.ScientificObjectVersion{mv},
	}
}

// strPtr is a one-line &s helper for the nullable domain fields.
func strPtr(s string) *string { return &s }

// failuresFor returns the failed results of one check.
func failuresFor(r Report, check CheckID) []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Check == check && !res.Passed {
			out = append(out, res)
		}
	}
	return out
}

// mustBlock asserts the report is blocked and that check is among the
// blocking failures.
func mustBlock(t *testing.T, r Report, check CheckID) Result {
	t.Helper()
	if !r.Blocked() {
		t.Fatalf("verdict = %s, want blocked; explanation:\n%s", r.Verdict, r.Explanation)
	}
	for _, res := range r.BlockingFailures() {
		if res.Check == check {
			return res
		}
	}
	t.Fatalf("check %s is not among the blocking failures: %+v", check, r.BlockingFailures())
	return Result{}
}

// TestGateSpecsLadderIsMonotone pins the ladder's two properties: the
// blocking set only grows along gateOrder, and every consecutive pair
// differs (StrictnessDiff non-empty). It is the structural guarantee
// behind "不同 gate 严格度不同且可解释".
func TestGateSpecsLadderIsMonotone(t *testing.T) {
	for i := 0; i < len(gateOrder)-1; i++ {
		prev, next := gateOrder[i], gateOrder[i+1]
		if !StricterThan(next, prev) {
			t.Errorf("StricterThan(%s, %s) = false", next, prev)
		}
		diff, err := StrictnessDiff(prev, next)
		if err != nil {
			t.Fatalf("StrictnessDiff(%s, %s): %v", prev, next, err)
		}
		if len(diff) == 0 {
			t.Errorf("StrictnessDiff(%s, %s) is empty — consecutive gates must differ in strictness", prev, next)
		}
		blockingOf := func(g Gate) map[CheckID]bool {
			set := make(map[CheckID]bool)
			for _, spec := range GateSpecs(g) {
				if spec.Severity == SeverityBlocking {
					set[spec.Check] = true
				}
			}
			return set
		}
		for check := range blockingOf(prev) {
			if !blockingOf(next)[check] {
				t.Errorf("%s blocks on %s but the stricter %s does not", prev, check, next)
			}
		}
	}
	// Per-check severity monotonicity: a check may only climb the ladder,
	// never slide back from blocking to warning.
	worst := make(map[CheckID]int) // 0 = absent, 1 = warning, 2 = blocking
	for _, g := range gateOrder {
		seen := make(map[CheckID]bool)
		for _, spec := range GateSpecs(g) {
			if spec.Check == "" || spec.Why == "" {
				t.Errorf("gate %s: check %q has empty check id or why", g, spec.Check)
			}
			sev := 1
			if spec.Severity == SeverityBlocking {
				sev = 2
			} else if spec.Severity != SeverityWarning {
				t.Errorf("gate %s check %s: unknown severity %q", g, spec.Check, spec.Severity)
			}
			if seen[spec.Check] {
				t.Errorf("gate %s declares check %s twice", g, spec.Check)
			}
			seen[spec.Check] = true
			if sev < worst[spec.Check] {
				t.Errorf("check %s is %s at %s but was stricter earlier on the ladder", spec.Check, spec.Severity, g)
			}
			worst[spec.Check] = sev
		}
	}
}

func TestStrictnessDiffContents(t *testing.T) {
	tests := []struct {
		from, to Gate
		want     []string
	}{
		{GateDraft, GatePR, []string{
			"schema_typed: warning -> blocking",
			"authoritative_fields: warning -> blocking",
			"required_fields: added (warning)",
			"state_linkage: warning -> blocking",
			"commit_linkage: warning -> blocking",
			"actor_consistency: added (warning)",
			"chain_integrity: added (warning)",
			"branch_empty: added (warning)",
		}},
		{GatePR, GateMain, []string{
			"required_fields: warning -> blocking",
			"actor_consistency: warning -> blocking",
			"payload_integrity: warning -> blocking",
			"chain_integrity: warning -> blocking",
		}},
		{GateMain, GateRelease, []string{
			"release_review: added (blocking)",
			"release_rights: added (blocking)",
			"release_from_main: added (blocking)",
		}},
		{GateRelease, GateAsset, []string{
			"asset_source_pinned: added (blocking)",
			"asset_version_pinned: added (blocking)",
			"asset_contributors: added (blocking)",
			"asset_rights: added (blocking)",
			"asset_visibility: added (blocking)",
			"asset_dependency_pins: added (blocking)",
			"asset_integrity_hash: added (blocking)",
			"asset_metadata: added (blocking)",
			"asset_schema: added (blocking)",
		}},
	}
	for _, tt := range tests {
		diff, err := StrictnessDiff(tt.from, tt.to)
		if err != nil {
			t.Fatalf("StrictnessDiff(%s, %s): %v", tt.from, tt.to, err)
		}
		if len(diff) != len(tt.want) {
			t.Errorf("StrictnessDiff(%s, %s) = %v, want %v", tt.from, tt.to, diff, tt.want)
			continue
		}
		for i, line := range tt.want {
			if diff[i] != line {
				t.Errorf("StrictnessDiff(%s, %s)[%d] = %q, want %q", tt.from, tt.to, i, diff[i], line)
			}
		}
	}
}

func TestStrictnessDiffRejectsNonStricterPairs(t *testing.T) {
	for _, pair := range [][2]Gate{{GateMain, GatePR}, {GateDraft, GateDraft}, {GatePR, GatePR}} {
		if _, err := StrictnessDiff(pair[0], pair[1]); err == nil {
			t.Errorf("StrictnessDiff(%s, %s) = nil error, want rejection", pair[0], pair[1])
		}
	}
}

func TestExplainGateDiffer(t *testing.T) {
	texts := make(map[Gate]string)
	for _, g := range gateOrder {
		texts[g] = ExplainGate(g)
	}
	if !strings.Contains(texts[GatePR], "stage 2 of 5") {
		t.Errorf("ExplainGate(pr) lacks its stage: %s", texts[GatePR])
	}
	if !strings.Contains(texts[GatePR], "[blocking] schema_typed") {
		t.Errorf("ExplainGate(pr) lacks the promoted schema_typed check")
	}
	if !strings.Contains(texts[GateMain], "stage 3 of 5") {
		t.Errorf("ExplainGate(main) lacks its stage")
	}
	if !strings.Contains(texts[GateRelease], "release_review") {
		t.Errorf("ExplainGate(release) lacks release_review")
	}
	if !strings.Contains(texts[GateAsset], "asset_schema") {
		t.Errorf("ExplainGate(asset) lacks asset_schema")
	}
	for a := range texts {
		for b := range texts {
			if a != b && texts[a] == texts[b] {
				t.Errorf("ExplainGate(%s) == ExplainGate(%s) — the gates must be explainably different", a, b)
			}
		}
	}
}

func TestGateHelpers(t *testing.T) {
	for i, g := range gateOrder {
		stage, ok := Stage(g)
		if !ok || stage != i+1 {
			t.Errorf("Stage(%s) = %d, %v; want %d, true", g, stage, ok, i+1)
		}
		if !ValidGate(g) {
			t.Errorf("ValidGate(%s) = false", g)
		}
		if parsed, err := ParseGate(string(g)); err != nil || parsed != g {
			t.Errorf("ParseGate(%q) = %q, %v", g, parsed, err)
		}
	}
	if _, ok := Stage("gold"); ok {
		t.Error("Stage(\"gold\") claimed a stage")
	}
	if ValidGate("gold") {
		t.Error("ValidGate(\"gold\") = true")
	}
	if _, err := ParseGate("gold"); err == nil {
		t.Error("ParseGate(\"gold\") = nil error")
	}
	commit := CommitGates()
	want := []Gate{GateDraft, GatePR, GateMain}
	if len(commit) != len(want) {
		t.Fatalf("CommitGates() = %v, want %v", commit, want)
	}
	for i := range want {
		if commit[i] != want[i] {
			t.Fatalf("CommitGates()[%d] = %s, want %s", i, commit[i], want[i])
		}
	}
	for _, g := range want {
		if !ValidCommitGate(g) {
			t.Errorf("ValidCommitGate(%s) = false", g)
		}
	}
	for _, g := range []Gate{GateRelease, GateAsset, "gold"} {
		if ValidCommitGate(g) {
			t.Errorf("ValidCommitGate(%s) = true", g)
		}
	}
}

func TestVerdicts(t *testing.T) {
	v := newTestValidator(t)

	// A complete chain passes the strictest commit gate with no warnings.
	if r := v.Validate(GateMain, completeChain()); r.Verdict != VerdictPass {
		t.Errorf("main over complete chain: verdict = %s, want pass; explanation:\n%s", r.Verdict, r.Explanation)
	}

	// A draft lacking a required schema field warns but passes.
	draft := completeChain()
	draft.ObjectVersions[0].Payload = json.RawMessage(`{"statement":"probe"}`)
	r := v.Validate(GateDraft, draft)
	if r.Verdict != VerdictPassWithWarn {
		t.Errorf("draft over incomplete hypothesis: verdict = %s, want pass_with_warnings; explanation:\n%s", r.Verdict, r.Explanation)
	}
	if len(failuresFor(r, CheckSchemaTyped)) == 0 {
		t.Error("schema_typed did not report the missing question_id at draft")
	}

	// The same shape at pr blocks.
	if r := v.Validate(GatePR, draft); !r.Blocked() {
		t.Errorf("pr over incomplete hypothesis: verdict = %s, want blocked", r.Verdict)
	}
}

func TestUnknownGateBlocks(t *testing.T) {
	v := newTestValidator(t)
	r := v.Validate("gold", completeChain())
	if !r.Blocked() {
		t.Fatalf("unknown gate verdict = %s, want blocked", r.Verdict)
	}
	if len(r.Results) != 1 || r.Results[0].Check != CheckID("gate") {
		t.Fatalf("unknown gate results = %+v, want a single gate result", r.Results)
	}
}

func TestGateBlockedErrorCode(t *testing.T) {
	reportWith := func(fails ...Result) Report {
		r := Report{Gate: GatePR}
		r.Results = fails
		r.finalize()
		return r
	}
	tests := []struct {
		name string
		rep  Report
		code string
	}{
		{"schema-only", reportWith(Result{Check: CheckSchemaTyped, Severity: SeverityBlocking, Passed: false, Why: "w"}), "SCHEMA_VALIDATION_FAILED"},
		{"schema-known", reportWith(Result{Check: CheckSchemaKnown, Severity: SeverityBlocking, Passed: false, Why: "w"}), "SCHEMA_VALIDATION_FAILED"},
		{"asset-schema", reportWith(Result{Check: CheckAssetSchema, Severity: SeverityBlocking, Passed: false, Why: "w"}), "SCHEMA_VALIDATION_FAILED"},
		{"provenance-only", reportWith(Result{Check: CheckPayloadIntegrity, Severity: SeverityBlocking, Passed: false, Why: "w"}), "PROVENANCE_INCOMPLETE"},
		{"provenance-linkage", reportWith(Result{Check: CheckStateLinkage, Severity: SeverityBlocking, Passed: false, Why: "w"}), "PROVENANCE_INCOMPLETE"},
		{"mixed", reportWith(
			Result{Check: CheckSchemaTyped, Severity: SeverityBlocking, Passed: false, Why: "w"},
			Result{Check: CheckPayloadIntegrity, Severity: SeverityBlocking, Passed: false, Why: "w"},
		), "RSG_VALIDATION_FAILED"},
		{"other-family", reportWith(Result{Check: CheckRequiredFields, Severity: SeverityBlocking, Passed: false, Why: "w"}), "RSG_VALIDATION_FAILED"},
	}
	for _, tt := range tests {
		err := &GateBlockedError{Report: tt.rep}
		if got := err.Code(); got != tt.code {
			t.Errorf("%s: Code() = %s, want %s", tt.name, got, tt.code)
		}
		if !strings.Contains(err.Error(), "blocked") {
			t.Errorf("%s: Error() = %q lacks the verdict", tt.name, err.Error())
		}
	}
}

func TestSchemaKnownBlocksUnknownSchema(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].SchemaID = "https://example.com/unknown.schema.json"
	r := v.Validate(GateDraft, snap)
	mustBlock(t, r, CheckSchemaKnown)
	if len(failuresFor(r, CheckSchemaCore)) == 0 || len(failuresFor(r, CheckSchemaTyped)) == 0 {
		t.Error("dependent schema checks should refuse an unregistered schema too, not cascade into noise")
	}
	if !strings.Contains(r.Explanation, "schema_known") {
		t.Errorf("explanation does not name schema_known:\n%s", r.Explanation)
	}
}

func TestAuthoritativeFieldsWarningVsBlocking(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"id":"sneaky","statement":"s","question_id":"q"}`)

	draft := v.Validate(GateDraft, snap)
	if draft.Verdict != VerdictPassWithWarn {
		t.Errorf("draft verdict = %s, want pass_with_warnings", draft.Verdict)
	}
	if res := failuresFor(draft, CheckAuthoritativeFields); len(res) != 1 || !strings.Contains(res[0].Detail, "id") {
		t.Errorf("authoritative_fields at draft = %+v, want one warning naming the smuggled id", res)
	}

	pr := v.Validate(GatePR, snap)
	got := mustBlock(t, pr, CheckAuthoritativeFields)
	if !strings.Contains(got.Detail, "id") {
		t.Errorf("authoritative_fields detail = %q, want it to name the smuggled field", got.Detail)
	}
}

func TestIdentityFields(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].Title = "  "
	r := v.Validate(GateDraft, snap)
	got := mustBlock(t, r, CheckIdentityFields)
	if !strings.Contains(got.Detail, "title") {
		t.Errorf("identity_fields detail = %q, want it to name the missing title", got.Detail)
	}
}

func TestRequiredFieldsWarningVsBlocking(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"statement":"s","question_id":"q"}`)

	pr := v.Validate(GatePR, snap)
	if pr.Blocked() {
		t.Fatalf("pr verdict = %s, want pass_with_warnings (required_fields is a warning at pr)", pr.Verdict)
	}
	if res := failuresFor(pr, CheckRequiredFields); len(res) != 1 || !strings.Contains(res[0].Detail, "hypothesis_type") || !strings.Contains(res[0].Detail, "scope") {
		t.Errorf("required_fields at pr = %+v, want one warning naming hypothesis_type and scope", res)
	}

	main := v.Validate(GateMain, snap)
	got := mustBlock(t, main, CheckRequiredFields)
	if !strings.Contains(got.Detail, "hypothesis_type") || !strings.Contains(got.Detail, "scope") {
		t.Errorf("required_fields at main = %q, want both missing fields named", got.Detail)
	}
}

func TestPayloadIntegrityWarningVsBlocking(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].IntegrityHash = strings.Repeat("0", 64)

	if r := v.Validate(GatePR, snap); r.Blocked() {
		t.Fatalf("pr verdict = %s, want pass_with_warnings (payload_integrity is a warning at pr)", r.Verdict)
	}
	mustBlock(t, v.Validate(GateMain, snap), CheckPayloadIntegrity)
}

func TestStateLinkageBlocksUnlinkedMember(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].StateID = "st-unknown"
	mustBlock(t, v.Validate(GatePR, snap), CheckStateLinkage)
}

func TestCommitLinkageBothDirections(t *testing.T) {
	v := newTestValidator(t)

	// Forward: a member no commit names.
	unnamed := completeChain()
	unnamed.Commits[0].OperationSummary = summaryJSONRaw(domain.StateOperation{Kind: domain.OperationPolicyApplied, EntityID: "pol-1"})
	got := mustBlock(t, v.Validate(GatePR, unnamed), CheckCommitLinkage)
	if !strings.Contains(got.Detail, "object-1") {
		t.Errorf("commit_linkage detail = %q, want the unnamed member named", got.Detail)
	}

	// Reverse: an operation whose member row is missing (the real member
	// stays named, so the forward direction passes).
	dangling := completeChain()
	dangling.Commits[0].OperationSummary = summaryJSONRaw(
		domain.StateOperation{Kind: domain.OperationObjectVersionCreated, EntityID: "object-1", VersionNo: 1},
		domain.StateOperation{Kind: domain.OperationObjectVersionCreated, EntityID: "object-9", VersionNo: 1},
	)
	got = mustBlock(t, v.Validate(GateMain, dangling), CheckCommitLinkage)
	if !strings.Contains(got.Detail, "object-9") {
		t.Errorf("commit_linkage detail = %q, want the dangling operation named", got.Detail)
	}
}

func TestActorConsistencyWarningVsBlocking(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.ObjectVersions[0].CreatedBy = "mallory"

	if r := v.Validate(GatePR, snap); r.Blocked() {
		t.Fatalf("pr verdict = %s, want pass_with_warnings (actor_consistency is a warning at pr)", r.Verdict)
	}
	got := mustBlock(t, v.Validate(GateMain, snap), CheckActorConsistency)
	if !strings.Contains(got.Detail, "mallory") {
		t.Errorf("actor_consistency detail = %q, want both actors named", got.Detail)
	}
}

func TestChainIntegrityFailures(t *testing.T) {
	v := newTestValidator(t)

	t.Run("fork", func(t *testing.T) {
		snap := completeChain()
		branch := "br-1"
		stX := domain.ProjectState{ID: "st-x", ProjectID: "proj-1", BranchID: &branch, ParentStateID: strPtr("st-gen"), StateHash: "hx", CreatedAt: time.Now()}
		snap.States = append(snap.States, stX)
		got := mustBlock(t, v.Validate(GateMain, snap), CheckChainIntegrity)
		if !strings.Contains(got.Detail, "st-x") {
			t.Errorf("chain_integrity detail = %q, want the forked state named", got.Detail)
		}
	})

	t.Run("gap", func(t *testing.T) {
		snap := completeChain()
		snap.States[1].ParentStateID = strPtr("st-missing")
		mustBlock(t, v.Validate(GateMain, snap), CheckChainIntegrity)
	})

	t.Run("cycle", func(t *testing.T) {
		snap := completeChain()
		snap.States[0].ParentStateID = strPtr("st-2") // st-1 -> st-2 -> st-1
		mustBlock(t, v.Validate(GateMain, snap), CheckChainIntegrity)
	})

	t.Run("commit edge mismatch", func(t *testing.T) {
		snap := completeChain()
		snap.Commits[0].BaseStateID = strPtr("st-other")
		mustBlock(t, v.Validate(GateMain, snap), CheckChainIntegrity)
	})
}

func TestBranchEmptyWarns(t *testing.T) {
	v := newTestValidator(t)
	snap := Snapshot{ProjectID: "proj-1", BranchID: "br-empty"}
	r := v.Validate(GatePR, snap)
	if r.Verdict != VerdictPassWithWarn {
		t.Fatalf("empty branch at pr: verdict = %s, want pass_with_warnings", r.Verdict)
	}
	res := failuresFor(r, CheckBranchEmpty)
	if len(res) != 1 || !strings.Contains(res[0].Detail, "no states") {
		t.Errorf("branch_empty = %+v, want one warning about the empty branch", res)
	}
}

func TestReleaseGateRequiresFacts(t *testing.T) {
	v := newTestValidator(t)
	r := v.Validate(GateRelease, completeChain())
	mustBlock(t, r, CheckReleaseReview)
	mustBlock(t, r, CheckReleaseRights)
	got := mustBlock(t, r, CheckReleaseFromMain)
	if !strings.Contains(got.Detail, "release facts were not provided") {
		t.Errorf("release_from_main detail = %q, want the facts-missing explanation", got.Detail)
	}
}

func TestReleaseGatePassesWithFacts(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	snap.Release = &ReleaseFacts{ReviewApproved: true, RightsSnapshot: true, FromMainBranch: true}
	r := v.Validate(GateRelease, snap)
	if r.Blocked() {
		t.Fatalf("release over complete chain + facts: verdict = %s, want pass; explanation:\n%s", r.Verdict, r.Explanation)
	}
}

func TestAssetGate(t *testing.T) {
	v := newTestValidator(t)

	validDoc := json.RawMessage(`{"asset_id":"asset-1","version":"1.0.0","asset_type":"dataset","origin_refs":["https://example.com/source"],"rights":{"license":"CC-BY-4.0"},"integrity_hash":"` + strings.Repeat("a", 64) + `"}`)

	emptySnap := Snapshot{ProjectID: "proj-1", BranchID: "br-1"}

	// Without facts every asset check fails.
	r := v.Validate(GateAsset, emptySnap)
	got := mustBlock(t, r, CheckAssetSourcePinned)
	if !strings.Contains(got.Detail, "asset facts were not provided") {
		t.Errorf("asset_source_pinned detail = %q, want the facts-missing explanation", got.Detail)
	}
	mustBlock(t, r, CheckAssetSchema)

	// Complete facts over an empty chain: only the branch_empty warning
	// remains. The asset gate also runs the release checks (an asset
	// descends from a release), so the release facts are supplied too.
	emptySnap.Asset = &AssetFacts{
		SourcePinned: true, VersionPinned: true, Contributors: true,
		Rights: true, Visibility: true, DependencyPins: true,
		IntegrityHash: true, Metadata: true,
		Document: validDoc,
		Ref:      schemareg.Ref{ID: assetSchemaID, Version: "1"},
	}
	emptySnap.Release = &ReleaseFacts{ReviewApproved: true, RightsSnapshot: true, FromMainBranch: true}
	r = v.Validate(GateAsset, emptySnap)
	if r.Verdict != VerdictPassWithWarn {
		t.Fatalf("asset gate with complete facts: verdict = %s, want pass_with_warnings; explanation:\n%s", r.Verdict, r.Explanation)
	}
	for _, res := range r.Warnings() {
		if res.Check != CheckBranchEmpty {
			t.Errorf("unexpected warning %s: %s", res.Check, res.Detail)
		}
	}

	// A document violating the asset schema blocks.
	emptySnap.Asset.Document = json.RawMessage(`{"asset_id":"asset-1"}`)
	r = v.Validate(GateAsset, emptySnap)
	res := mustBlock(t, r, CheckAssetSchema)
	if !strings.Contains(res.Detail, "origin_refs") {
		t.Errorf("asset_schema detail = %q, want the missing required field named", res.Detail)
	}
}

func TestReportSerialization(t *testing.T) {
	v := newTestValidator(t)
	r := v.Validate(GatePR, completeChain())
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	var wire struct {
		Gate        string   `json:"gate"`
		Verdict     string   `json:"verdict"`
		Results     []Result `json:"results"`
		Explanation string   `json:"explanation"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}
	if wire.Gate != "pr" || wire.Verdict != VerdictPass || wire.Explanation == "" {
		t.Errorf("wire report = %+v", wire)
	}
	if len(wire.Results) == 0 {
		t.Error("wire report has no results")
	}
	for _, res := range wire.Results {
		if res.Why == "" {
			t.Errorf("result %s has no why — every report must explain itself", res.Check)
		}
	}
}

// TestAssembledDocumentUsesRowFacts pins the assembly rule the typed-schema
// check depends on: the version row is authoritative, the payload only
// contributes content fields (docs/23 §3).
func TestAssembledDocumentUsesRowFacts(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	// The payload tries to claim a different title and lifecycle; the
	// assembled document must keep the row's facts.
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"statement":"s","question_id":"q","title":"FAKE","lifecycle_state":"aborted","project_id":"other"}`)
	snap.ObjectVersions[0].IntegrityHash = payloadHash(snap.ObjectVersions[0].Payload)
	snap.ObjectVersions[0].LifecycleState = domain.LifecycleAborted
	r := v.Validate(GateDraft, snap)
	// authoritative_fields fires for the smuggled keys (draft: warning);
	// schema_typed must still pass on the row facts, so nothing blocks.
	if r.Blocked() {
		t.Fatalf("verdict = %s, want pass_with_warnings; explanation:\n%s", r.Verdict, r.Explanation)
	}
	for _, res := range r.Results {
		if res.Check == CheckSchemaTyped && !res.Passed {
			t.Errorf("schema_typed failed on the smuggled payload: %s", res.Detail)
		}
	}
}

// specAt returns the gate's spec for one check (the test-side lookup of
// the declared rationale the reports must render).
func specAt(gate Gate, check CheckID) Spec {
	for _, s := range gateSpecs[gate] {
		if s.Check == check {
			return s
		}
	}
	return Spec{}
}

// extHypothesisSchemaDoc is a runtime extension schema whose id does NOT
// name the type it governs: only properties.type.const says "hypothesis".
// It is the shape T0201's Register accepts (namespaced id, draft 2020-12).
func extHypothesisSchemaDoc() string {
	return `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://acme.example/schemas/hypothesis-ext.schema.json",
  "title": "Hypothesis Extension",
  "type": "object",
  "required": ["id", "type", "version", "project_id", "title", "lifecycle_state", "schema_ref", "created_by", "created_at"],
  "properties": {
    "id": {"type": "string"},
    "type": {"const": "hypothesis"},
    "version": {"type": "integer", "minimum": 1},
    "project_id": {"type": "string"},
    "title": {"type": "string"},
    "lifecycle_state": {"type": "string"},
    "schema_ref": {"type": "object"},
    "created_by": {"type": "string"},
    "created_at": {"type": "string"},
    "statement": {"type": "string"},
    "question_id": {"type": "string"},
    "hypothesis_type": {"type": "string"},
    "scope": {"type": "object"}
  },
  "additionalProperties": true
}`
}

// untypedSchemaDoc declares no properties.type.const at all: its objects
// have no authoritative type to map to the docs/08 table.
func untypedSchemaDoc() string {
	return `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://acme.example/schemas/untyped.schema.json",
  "title": "Untyped Extension",
  "type": "object",
  "additionalProperties": true
}`
}

// TestRequiredFieldsUsesAuthoritativeTypeForExtensionSchemas pins the
// blocking defect: the required-field decision must come from the schema's
// const (registry TypeConst), never from the id's label. An extension
// schema whose id looks like "<type>.schema.json" but whose const says
// "hypothesis" must still be held to the hypothesis fields — a hypothesis
// missing hypothesis_type and scope may not pass a blocking gate.
func TestRequiredFieldsUsesAuthoritativeTypeForExtensionSchemas(t *testing.T) {
	v := newTestValidator(t)
	extID := "https://acme.example/schemas/hypothesis-ext.schema.json"
	if _, err := v.reg.Register(extID, "1", []byte(extHypothesisSchemaDoc())); err != nil {
		t.Fatalf("register extension schema: %v", err)
	}
	snap := completeChain()
	snap.ObjectVersions[0].SchemaID = extID
	snap.ObjectVersions[0].SchemaVersion = "1"
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"statement":"probe","question_id":"q-1"}`)

	// At pr the missing hypothesis fields warn (the severity ladder is
	// per check, not per schema id).
	pr := v.Validate(GatePR, snap)
	if res := failuresFor(pr, CheckRequiredFields); len(res) != 1 ||
		!strings.Contains(res[0].Detail, "hypothesis_type") || !strings.Contains(res[0].Detail, "scope") {
		t.Errorf("required_fields at pr for the extension schema = %+v, want one warning naming hypothesis_type and scope", res)
	}

	// At main the same missing fields block — the label "hypothesis-ext"
	// would have matched no docs/08 row and silently passed the gate.
	got := mustBlock(t, v.Validate(GateMain, snap), CheckRequiredFields)
	if !strings.Contains(got.Detail, "hypothesis_type") || !strings.Contains(got.Detail, "scope") {
		t.Errorf("required_fields at main = %q, want both missing hypothesis fields named", got.Detail)
	}
}

// TestRequiredFieldsFailClosedWithoutAuthoritativeType pins the other half
// of the rule: a registered schema that declares no type const cannot be
// mapped to the docs/08 table, so the check fails with an explicit reason
// instead of silently passing — the blocking gate never grades what it
// cannot type.
func TestRequiredFieldsFailClosedWithoutAuthoritativeType(t *testing.T) {
	v := newTestValidator(t)
	untypedID := "https://acme.example/schemas/untyped.schema.json"
	if _, err := v.reg.Register(untypedID, "1", []byte(untypedSchemaDoc())); err != nil {
		t.Fatalf("register untyped schema: %v", err)
	}
	snap := completeChain()
	snap.ObjectVersions[0].SchemaID = untypedID
	snap.ObjectVersions[0].SchemaVersion = "1"
	got := mustBlock(t, v.Validate(GateMain, snap), CheckRequiredFields)
	if !strings.Contains(got.Detail, "no authoritative type") {
		t.Errorf("required_fields detail = %q, want the missing-const reason", got.Detail)
	}
}

// TestExplanationRendersSpecWhy pins the "可解释" acceptance criterion: a
// failed check's rationale is the Spec.Why text itself, rendered into the
// report's explanation — not just the check id. Removing the why from the
// renderer (report.go) must fail this test.
func TestExplanationRendersSpecWhy(t *testing.T) {
	v := newTestValidator(t)
	snap := completeChain()
	// question_id is missing: the data-driven schema_typed check fails on
	// the assembled document, and its spec's rationale must be readable in
	// the explanation.
	snap.ObjectVersions[0].Payload = json.RawMessage(`{"statement":"probe"}`)

	spec := specAt(GatePR, CheckSchemaTyped)
	if spec.Why == "" {
		t.Fatal("test bug: the pr schema_typed spec has no why")
	}
	pr := v.Validate(GatePR, snap)
	fails := failuresFor(pr, CheckSchemaTyped)
	if len(fails) == 0 {
		t.Fatalf("test bug: schema_typed must fail for the incomplete payload; verdict = %s", pr.Verdict)
	}
	if !strings.Contains(fails[0].Detail, "question_id") {
		t.Errorf("schema_typed detail = %q, want the missing field named", fails[0].Detail)
	}
	if !strings.Contains(pr.Explanation, spec.Why) {
		t.Errorf("explanation does not render the schema_typed why %q:\n%s", spec.Why, pr.Explanation)
	}
}
