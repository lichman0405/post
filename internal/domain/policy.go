package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The organization/project policy domain (T0603, docs/12 §5): organization
// policy is the minimum governance requirement; a project policy may only be
// stricter, never silently looser. Policies are versioned: policy_versions
// is append-only (migrations 00014/00015 reject UPDATE/DELETE/TRUNCATE at
// the database itself), so a policy never mutates in place — a change is a
// new version row, and old versions stay queryable forever (acceptance:
// 旧 policy version 可查询). Releases pin the policy_version_id that was in
// force when they were built (releases.policy_version_id, T0605).
//
// The policy document (policy_versions.policy_json) is a flat set of named
// rules. The V1 rule keys are the docs/12 §5 examples, each with a defined
// strictness order; every other key follows identity comparison (only the
// identical value is provably non-relaxing — the fail-closed default of
// this codebase).

// Rule keys of the V1 policy vocabulary (docs/12 §5: "main protected、
// release min reviewers、raw data retention、public asset IP review、
// required schema profile"). Unknown keys are storable and mergeable via
// identity comparison, so later tasks (T0604 reviewer routing) can extend
// the vocabulary without a migration.
const (
	// RuleMainProtected requires every change to frozen main to pass
	// review-and-merge gates (bool: true is stricter than false).
	RuleMainProtected = "main_protected"
	// RuleReleaseMinReviewers is the minimum number of approving reviews a
	// release merge must carry (int: larger is stricter).
	RuleReleaseMinReviewers = "release_min_reviewers"
	// RuleRawDataRetentionDays is the minimum raw-data retention in days
	// (int: larger is stricter).
	RuleRawDataRetentionDays = "raw_data_retention_days"
	// RulePublicAssetIPReview requires an IP review before a public asset
	// is published (bool: true is stricter than false).
	RulePublicAssetIPReview = "public_asset_ip_review"
	// RuleRequiredSchemaProfiles lists the schema profiles every accepted
	// object version must satisfy (list of profile ids: a superset is
	// stricter).
	RuleRequiredSchemaProfiles = "required_schema_profiles"
)

// RuleKind is the value shape a rule key expects.
type RuleKind int

const (
	// RuleKindUnknown: no registered semantics — strictness is identity.
	RuleKindUnknown RuleKind = iota
	RuleKindBool
	RuleKindInt
	RuleKindStringList
)

// RuleKindOf returns the registered value kind of a rule key.
func RuleKindOf(key string) RuleKind {
	switch key {
	case RuleMainProtected, RulePublicAssetIPReview:
		return RuleKindBool
	case RuleReleaseMinReviewers, RuleRawDataRetentionDays:
		return RuleKindInt
	case RuleRequiredSchemaProfiles:
		return RuleKindStringList
	}
	return RuleKindUnknown
}

// MaxPolicyRuleKeyLen bounds a rule key (generous but finite).
const MaxPolicyRuleKeyLen = 64

// MaxPolicyRuleCount bounds the number of rules per policy document.
const MaxPolicyRuleCount = 64

// ValidPolicyRuleKey reports whether key has the policy rule shape: 1..64
// characters of [a-z0-9_] (a machine-readable key, not free text).
func ValidPolicyRuleKey(key string) bool {
	if key == "" || len(key) > MaxPolicyRuleKeyLen {
		return false
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

// Policy is the policy document stored in policy_versions.policy_json: a
// flat set of named rules. Values are raw JSON so the engine can compare
// them exactly and later rule vocabularies can use any JSON shape.
type Policy struct {
	Rules map[string]json.RawMessage
}

// EmptyPolicy returns a policy with no rules (the neutral lower bound: an
// organization without a policy constrains nothing).
func EmptyPolicy() Policy { return Policy{Rules: map[string]json.RawMessage{}} }

// Empty reports whether the policy sets no rules.
func (p Policy) Empty() bool { return len(p.Rules) == 0 }

// Rule returns the raw value of one rule and whether it is set.
func (p Policy) Rule(key string) (json.RawMessage, bool) {
	v, ok := p.Rules[key]
	return v, ok
}

// PolicyFromJSON parses a policy document. The shape is strict: a JSON
// object whose member names are valid rule keys and whose values are any
// non-null JSON. Registered keys must carry the registered value kind.
func PolicyFromJSON(data []byte) (Policy, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Policy{}, fmt.Errorf("policy: invalid JSON: %w", err)
	}
	p := Policy{Rules: raw}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// MarshalJSON renders the policy document deterministically: rule keys
// sorted, null values rejected (a rule is a requirement; a JSON null
// value means "no value", which is the same as omitting the rule and must
// be written as such).
func (p Policy) MarshalJSON() ([]byte, error) {
	keys := make([]string, 0, len(p.Rules))
	for k := range p.Rules {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		v := p.Rules[k]
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("policy: rule %q has a null value (omit the rule instead)", k)
		}
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// Validate reports whether the policy document is well-formed: valid rule
// keys, bounded size, non-null values, and registered keys carrying their
// registered value kind. Malformed policies are refused at write time —
// the evaluator and the merge never have to handle them.
func (p Policy) Validate() error {
	if len(p.Rules) > MaxPolicyRuleCount {
		return fmt.Errorf("policy: at most %d rules allowed", MaxPolicyRuleCount)
	}
	for key, v := range p.Rules {
		if !ValidPolicyRuleKey(key) {
			return fmt.Errorf("policy: rule key %q is invalid (1..64 characters of a-z, 0-9, _)", key)
		}
		trimmed := bytes.TrimSpace(v)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return fmt.Errorf("policy: rule %q must carry a JSON value (omit the rule instead of null)", key)
		}
		if _, err := parseRuleValue(key, v); err != nil {
			return err
		}
	}
	return nil
}

// parseRuleValue decodes a rule value into its registered kind, reporting
// a type mismatch. Unknown keys accept any JSON value.
func parseRuleValue(key string, v json.RawMessage) (any, error) {
	switch RuleKindOf(key) {
	case RuleKindBool:
		var b bool
		if err := json.Unmarshal(v, &b); err != nil {
			return nil, fmt.Errorf("policy: rule %q must be a boolean", key)
		}
		return b, nil
	case RuleKindInt:
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, fmt.Errorf("policy: rule %q must be an integer", key)
		}
		return n, nil
	case RuleKindStringList:
		var list []string
		if err := json.Unmarshal(v, &list); err != nil {
			return nil, fmt.Errorf("policy: rule %q must be a list of strings", key)
		}
		return list, nil
	default:
		var anyValue any
		if err := json.Unmarshal(v, &anyValue); err != nil {
			return nil, fmt.Errorf("policy: rule %q: %w", key, err)
		}
		return anyValue, nil
	}
}

// Strictness orders two values of one rule key.
type Strictness int

const (
	// StrictnessIncomparable: no registered order and the values differ —
	// the project value is not provably non-relaxing, so the write is
	// refused (fail closed).
	StrictnessIncomparable Strictness = iota
	// StrictnessWeaker: the candidate relaxes the baseline.
	StrictnessWeaker
	// StrictnessEqual: identical requirements.
	StrictnessEqual
	// StrictnessStricter: the candidate is stricter than the baseline.
	StrictnessStricter
)

// CompareRule orders candidate against baseline for one rule key.
// Registered keys use their documented order (bool: true stricter; int:
// larger stricter; string list: superset stricter). Unregistered keys
// compare by canonical identity: the same value is StrictnessEqual, any
// different value is StrictnessIncomparable (a project must not silently
// rewrite an unknown rule — the engine cannot prove the rewrite is not a
// relaxation).
func CompareRule(key string, baseline, candidate json.RawMessage) (Strictness, error) {
	switch RuleKindOf(key) {
	case RuleKindBool:
		b, err := ruleBool(key, baseline)
		if err != nil {
			return 0, err
		}
		c, err := ruleBool(key, candidate)
		if err != nil {
			return 0, err
		}
		switch {
		case c == b:
			return StrictnessEqual, nil
		case c:
			return StrictnessStricter, nil
		default:
			return StrictnessWeaker, nil
		}
	case RuleKindInt:
		b, err := ruleInt(key, baseline)
		if err != nil {
			return 0, err
		}
		c, err := ruleInt(key, candidate)
		if err != nil {
			return 0, err
		}
		switch {
		case c == b:
			return StrictnessEqual, nil
		case c > b:
			return StrictnessStricter, nil
		default:
			return StrictnessWeaker, nil
		}
	case RuleKindStringList:
		b, err := ruleStringList(key, baseline)
		if err != nil {
			return 0, err
		}
		c, err := ruleStringList(key, candidate)
		if err != nil {
			return 0, err
		}
		bSet, cSet := stringSet(b), stringSet(c)
		switch {
		case len(bSet) == len(cSet) && setContainsAll(bSet, cSet):
			return StrictnessEqual, nil
		case setContainsAll(cSet, bSet):
			return StrictnessStricter, nil
		case setContainsAll(bSet, cSet):
			return StrictnessWeaker, nil
		default:
			return StrictnessIncomparable, nil
		}
	default:
		if jsonEqual(baseline, candidate) {
			return StrictnessEqual, nil
		}
		return StrictnessIncomparable, nil
	}
}

func ruleBool(key string, v json.RawMessage) (bool, error) {
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false, fmt.Errorf("policy: rule %q is not a boolean: %w", key, err)
	}
	return b, nil
}

func ruleInt(key string, v json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		return 0, fmt.Errorf("policy: rule %q is not an integer: %w", key, err)
	}
	return n, nil
}

func ruleStringList(key string, v json.RawMessage) ([]string, error) {
	var list []string
	if err := json.Unmarshal(v, &list); err != nil {
		return nil, fmt.Errorf("policy: rule %q is not a list of strings: %w", key, err)
	}
	return list, nil
}

func stringSet(list []string) map[string]bool {
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set
}

func setContainsAll(superset, subset map[string]bool) bool {
	for s := range subset {
		if !superset[s] {
			return false
		}
	}
	return true
}

// jsonEqual reports whether two JSON values are canonically identical
// (byte-equal after decoding/re-encoding, so member order never matters).
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, errA := json.Marshal(av)
	bb, errB := json.Marshal(bv)
	return errA == nil && errB == nil && bytes.Equal(ab, bb)
}

// PolicyViolation is one rule where a project policy would relax the
// organization lower bound (docs/12 §5: "Project policy 可更严格，不能静默
// 放宽").
type PolicyViolation struct {
	Key          string
	OrgValue     json.RawMessage
	ProjectValue json.RawMessage
	Relation     Strictness // StrictnessWeaker or StrictnessIncomparable
}

// MergeViolations is the error ValidateProjectPolicy returns: every
// violating rule in one error so a client sees the whole conflict at
// once, not one rule per retry. Error renders the violation details only
// — the service wraps it with ErrProjectRelaxesOrg, whose message already
// carries the sentence, so the two must not repeat each other.
type MergeViolations struct {
	Violations []PolicyViolation
}

func (e *MergeViolations) Error() string {
	parts := make([]string, len(e.Violations))
	for i, v := range e.Violations {
		parts[i] = fmt.Sprintf("%s: %s", v.Key, v.Relation)
	}
	return strings.Join(parts, ", ")
}

func (e *MergeViolations) Is(target error) bool {
	_, ok := target.(*MergeViolations)
	return ok
}

// ViolationString renders a Strictness value in the error text above.
func (s Strictness) String() string {
	switch s {
	case StrictnessWeaker:
		return "weaker than the organization rule"
	case StrictnessIncomparable:
		return "not provably at least as strict as the organization rule"
	case StrictnessEqual:
		return "equal"
	case StrictnessStricter:
		return "stricter"
	}
	return "unknown"
}

// ValidateProjectPolicy reports every rule where project relaxes the org
// lower bound (weaker or incomparable). Rules the org does not set are
// always allowed — a project adding requirements can never relax. The
// error is *MergeViolations when anything violates; nil when the project
// policy is valid against the org policy.
func ValidateProjectPolicy(org, project Policy) error {
	var violations []PolicyViolation
	for key, pv := range project.Rules {
		ov, ok := org.Rules[key]
		if !ok {
			continue
		}
		rel, err := CompareRule(key, ov, pv)
		if err != nil {
			// A value that does not parse was already rejected by
			// Validate at write time; treat it as incomparable (refuse).
			rel = StrictnessIncomparable
		}
		if rel == StrictnessWeaker || rel == StrictnessIncomparable {
			violations = append(violations, PolicyViolation{
				Key:          key,
				OrgValue:     ov,
				ProjectValue: pv,
				Relation:     rel,
			})
		}
	}
	if len(violations) > 0 {
		// Deterministic order: the same conflict renders the same error
		// message every time (map iteration order is random).
		sort.Slice(violations, func(i, j int) bool { return violations[i].Key < violations[j].Key })
		return &MergeViolations{Violations: violations}
	}
	return nil
}

// MergeEffective computes the effective policy of a project: the
// organization policy overlaid by the project policy, where the org is
// the lower bound. For every rule the org sets, the effective value is
// the stricter of the two — and when they are not comparable (the org
// policy moved after the project version was written, or an unknown rule
// was rewritten), the org value wins: the lower bound is authoritative.
// Rules only the project sets carry the project value; rules only the org
// sets carry the org value. This function is total: evaluation never
// fails on a merge.
func MergeEffective(org, project Policy) Policy {
	effective := make(map[string]json.RawMessage, len(org.Rules)+len(project.Rules))
	for key, ov := range org.Rules {
		pv, ok := project.Rules[key]
		if !ok {
			effective[key] = ov
			continue
		}
		rel, err := CompareRule(key, ov, pv)
		if err == nil && (rel == StrictnessStricter || rel == StrictnessEqual) {
			effective[key] = pv
		} else {
			effective[key] = ov
		}
	}
	for key, pv := range project.Rules {
		if _, ok := org.Rules[key]; !ok {
			effective[key] = pv
		}
	}
	return Policy{Rules: effective}
}

// ValidPolicyVersion reports whether version has the policy version shape:
// 1..64 characters of letters, digits, '.', '_' or '-' (e.g. "1", "v1",
// "2026-01-01"). Versions are per-scope unique (migration 00033) and
// informational: releases pin the row id, not the string.
func ValidPolicyVersion(version string) bool {
	v := strings.TrimSpace(version)
	if v == "" || len(v) > 64 {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// PolicyScope names the owner of a policy version line: exactly one of
// OrganizationID / ProjectID is set (the canonical XOR CHECK on
// policy_versions).
type PolicyScope struct {
	// OrganizationID identifies the organization; "" for project scope.
	OrganizationID string
	// ProjectID identifies the project; "" for organization scope.
	ProjectID string
}

// Valid reports whether exactly one side of the scope is set.
func (s PolicyScope) Valid() bool {
	return (s.OrganizationID == "") != (s.ProjectID == "")
}

// PolicyVersion is one stored policy version row (canonical table
// policy_versions — append-only; the row never changes).
type PolicyVersion struct {
	// ID is the uuid v4 text form of the version row.
	ID string
	// Scope is the owning organization XOR project.
	Scope PolicyScope
	// Version is the human-facing version string, unique per scope.
	Version string
	// Policy is the parsed policy document.
	Policy Policy
	// CreatedBy is the user id of the writer.
	CreatedBy string
	CreatedAt time.Time
}

// EffectivePolicy is the evaluation view of one project: the organization
// lower bound, the project's own policy, and their merge.
type EffectivePolicy struct {
	// Org is the organization's current policy version; nil when the
	// organization (or the personal project) has none.
	Org *PolicyVersion
	// Project is the project's current policy version; nil when none.
	Project *PolicyVersion
	// Effective is Org overlaid by Project (MergeEffective).
	Effective Policy
}

// ErrUnknownRule: the evaluator was asked about a rule key with no
// registered semantics. Enforcement sites asking for unknown rules is a
// programming error, not a policy state — the caller must either use a
// registered key or decide the semantics itself. Callers treat this as
// default deny.
var ErrUnknownRule = errors.New("policy: unknown rule")
