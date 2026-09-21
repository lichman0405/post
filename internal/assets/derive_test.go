package assets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/policy"
	"github.com/lichman0405/post/internal/application/projects"
	"github.com/lichman0405/post/internal/authz"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/rights"
)

// The derive command's unit surface (T0708). What lives here is what can be
// decided without a database: the relation vocabulary, the three-valued
// rights verdict over a parent's stored declaration, the parent read ruler's
// agreement with the page's own, the request-shape validation, the
// authorization step, and the replay rules. The transaction itself — the
// rows, the atomicity and the concurrency — is the integration test's
// (tests/integration/asset_lineage_test.go), because a fake store cannot
// show that two requests with one key wrote one row.

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// childManifest is a valid manifest of the child version, built from the
// type table rather than written out (manifestFor, manifest_test.go) so a
// field added to a type's required metadata does not make this stale.
func childManifest(t *testing.T) json.RawMessage {
	t.Helper()
	b, err := manifestFor(TypeDataset).CanonicalJSON()
	if err != nil {
		t.Fatalf("render the fixture manifest: %v", err)
	}
	return b
}

// rightsDocument renders one rights declaration with the named derivatives
// value, through the package that owns the format: a document the platform
// itself could never read would make every case below an unreadable-document
// case instead of the case it names.
func rightsDocument(t *testing.T, derivatives rights.Permission) json.RawMessage {
	t.Helper()
	doc := rights.New()
	doc.Usage.Derivatives = derivatives
	b, err := doc.Marshal()
	if err != nil {
		t.Fatalf("marshal the fixture rights document: %v", err)
	}
	return b
}

// parentRef is a canonical parent reference for fixtures.
const parentRef = ParentVersionRef(samplePID + "@v1")

// fakeMembers answers the membership question from a table.
type fakeMembers struct {
	role *domain.ProjectRole
	err  error
	// calls counts every resolution: the security property is as much about
	// WHEN this is asked as about what it answers.
	calls int
}

// GetMembership answers a membership row when a role is set, and
// projects.ErrMemberNotFound when it is not: that is the port's own contract
// for "not a member" and for "no such project" alike, so a nil role here
// exercises the authenticated-non-member path rather than panicking.
func (m *fakeMembers) GetMembership(_ context.Context, projectID, userID string) (domain.ProjectMembership, error) {
	m.calls++
	if m.err != nil {
		return domain.ProjectMembership{}, m.err
	}
	if m.role == nil {
		return domain.ProjectMembership{}, projects.ErrMemberNotFound
	}
	return domain.ProjectMembership{ProjectID: projectID, UserID: userID, Role: *m.role}, nil
}

// fakePolicies answers the policy-in-force read from a table, and counts the
// calls: the governance step's POSITION in the order — after the decision
// about the actor, after a replay has been answered — is asserted with that
// count, not by reading the source.
type fakePolicies struct {
	policy domain.Policy
	err    error
	calls  int
}

func (p *fakePolicies) EffectivePolicy(_ context.Context, _ domain.User, _ string) (domain.EffectivePolicy, error) {
	p.calls++
	if p.err != nil {
		return domain.EffectivePolicy{}, p.err
	}
	return domain.EffectivePolicy{Effective: p.policy}, nil
}

// fakeRules answers the rule query from a table and records what it was asked.
// The zero value is the permissive answer for a project that sets nothing:
// Found=false is "the policy does not mention the rule", which the publish's
// own step permits (only an explicit true refuses).
type fakeRules struct {
	decision policy.Decision
	err      error
	asked    []policy.Query
}

func (r *fakeRules) Evaluate(_ context.Context, _ domain.Policy, q policy.Query) (policy.Decision, error) {
	r.asked = append(r.asked, q)
	if r.err != nil {
		return policy.Decision{}, r.err
	}
	return r.decision, nil
}

// fakeStore is the derive store: it records what the command asked of it and
// answers from a table. Every call is counted, because "the refusal happened
// before anything was read" is asserted by these counts.
type fakeStore struct {
	lookup    *DerivedAsset
	lookupErr error
	derived   DerivedAsset
	deriveErr error

	lookups int
	derives int
	lastReq DeriveRequest
}

func (s *fakeStore) LookupDerivation(_ context.Context, _ string, _ string) (*DerivedAsset, error) {
	s.lookups++
	return s.lookup, s.lookupErr
}

func (s *fakeStore) Derive(_ context.Context, req DeriveRequest) (DerivedAsset, error) {
	s.derives++
	s.lastReq = req
	if s.deriveErr != nil {
		return DerivedAsset{}, s.deriveErr
	}
	out := s.derived
	if out.AssetPID == "" {
		out = DerivedAsset{
			ID: "version-id", AssetID: "asset-id", AssetPID: string(req.Candidate.AssetPID),
			Version: req.Candidate.Version, Visibility: req.Candidate.Visibility,
			Manifest: req.Candidate.Manifest, RightsJSON: req.Candidate.RightsJSON,
			ParentAssetPID: parentAssetPID(req.Parent), ParentVersion: parentVersion(req.Parent),
			Relation: req.Relation,
		}
	}
	return out, nil
}

// deriveParams is the smallest request that passes validation: everything
// below changes exactly one field of it, so a refusal is about that field.
func deriveParams(t *testing.T) DeriveParams {
	t.Helper()
	pin, ok := NewDependencyPin(samplePID, "v1")
	if !ok {
		t.Fatalf("fixture pin is not canonical")
	}
	return DeriveParams{
		ProjectID:  "11111111-1111-1111-1111-111111111111",
		Parent:     string(pin),
		Relation:   string(RelationForkedFrom),
		AssetType:  string(TypeDataset),
		Version:    "v1",
		Manifest:   childManifest(t),
		Rights:     rightsDocument(t, rights.PermissionAllowed),
		Visibility: string(VisibilityPublic),
		Title:      "A derived dataset",
		Slug:       "a-derived-dataset",
	}
}

// ownerMembers is the membership port answering "owner of the target
// project" — the one matrix class that allows publish_private_to_public in
// V1, and therefore the only actor that reaches the steps after
// authorization.
func ownerMembers() *fakeMembers {
	role := domain.ProjectRoleOwner
	return &fakeMembers{role: &role}
}

// ownerCommand wires a command whose actor is an OWNER of the target project
// (the one matrix class that allows publish_private_to_public in V1), over
// the given store.
func ownerCommand(t *testing.T, store *fakeStore) *DeriveCommand {
	t.Helper()
	role := domain.ProjectRoleOwner
	return NewDeriveCommand(DeriveDeps{
		Members:  &fakeMembers{role: &role},
		Policies: &fakePolicies{},
		Rules:    &fakeRules{},
		Store:    store,
		Authz:    authz.NewMatrixEngine(),
		NewPID:   func() (PID, error) { return samplePID, nil },
	})
}

func deriveActor() DeriveActor {
	return DeriveActor{User: domain.User{ID: "22222222-2222-2222-2222-222222222222"}}
}

// parentAssetPID and parentVersion split a fixture ref without going through
// the parser, so a test that is asserting about the EDGE it built is not
// also depending on ParseParentVersionRef being right.
func parentAssetPID(ref ParentVersionRef) string {
	pid, _, _ := strings.Cut(string(ref), "@")
	return pid
}

func parentVersion(ref ParentVersionRef) string {
	_, v, _ := strings.Cut(string(ref), "@")
	return v
}

// ---------------------------------------------------------------------------
// the relation vocabulary
// ---------------------------------------------------------------------------

// TestDeriveRelationVocabulary pins the names and the exclusions. The set is
// the 00010 CHECK's (forked_from, derived_from, supersedes) minus
// supersedes: a supersession does not create an identity (docs/11 §7), so it
// is not this command's, and the name is not offered as accepted-and-ignored
// either — it is refused by the parser.
func TestDeriveRelationVocabulary(t *testing.T) {
	for _, r := range []struct {
		raw string
		rel DeriveRelation
	}{{"forked_from", RelationForkedFrom}, {"derived_from", RelationDerivedFrom}} {
		got, ok := ParseDeriveRelation(r.raw)
		if !ok {
			t.Errorf("ParseDeriveRelation(%q) refused an admissible relation", r.raw)
			continue
		}
		if got != r.rel {
			t.Errorf("ParseDeriveRelation(%q) = %q, want %q", r.raw, got, r.rel)
		}
		if !got.Valid() {
			t.Errorf("%q.Valid() = false, want true", got)
		}
		if string(got) != r.raw {
			t.Errorf("%q renders back as %q: the stored value is the requested one", r.raw, got)
		}
	}

	for _, raw := range []string{
		"supersedes",     // a supersession does not create an identity
		"",               //
		"forked",         //
		"Forked_From",    // the vocabulary is lower-case; a case-tolerant parser
		"DERIVED_FROM",   // would store a value the CHECK refuses
		"forked-from",    // the CHECK's values use underscores
		"depends_on",     // a dependency is not a lineage edge
		"derived_from_x", //
	} {
		if got, ok := ParseDeriveRelation(raw); ok {
			t.Errorf("ParseDeriveRelation(%q) accepted %q; the vocabulary is exactly the two identity-creating relations", raw, got)
		}
	}

	// Surrounding whitespace is the caller's formatting, not a different
	// relation: the padded form parses, and what it parses TO is the canonical
	// value — the one the column's CHECK accepts. The stored relation is never
	// the caller's string as sent.
	for _, padded := range []string{" forked_from ", "\tderived_from\n"} {
		got, ok := ParseDeriveRelation(padded)
		if !ok {
			t.Errorf("ParseDeriveRelation(%q) refused a padded but admissible relation", padded)
			continue
		}
		if got != DeriveRelation(strings.TrimSpace(padded)) {
			t.Errorf("ParseDeriveRelation(%q) = %q, want the canonical %q", padded, got, strings.TrimSpace(padded))
		}
	}

	all := AllDeriveRelations()
	if len(all) != 2 {
		t.Fatalf("AllDeriveRelations() has %d entries, want 2", len(all))
	}
	for _, r := range all {
		if r == DeriveRelation("supersedes") {
			t.Error("AllDeriveRelations() offers supersedes; it must not be reachable through this command")
		}
	}
}

// TestDeriveRelationRefusalNamesBothOptions: a client that sent supersedes
// must be able to read the two names it may send instead.
func TestDeriveRelationRefusalNamesBothOptions(t *testing.T) {
	cmd := ownerCommand(t, &fakeStore{})
	in := deriveParams(t)
	in.Relation = "supersedes"
	_, err := cmd.Derive(context.Background(), deriveActor(), in)
	if !errors.Is(err, ErrDeriveValidation) {
		t.Fatalf("supersedes was not refused as a validation failure: %v", err)
	}
	for _, want := range []string{"forked_from", "derived_from"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err.Error(), want)
		}
	}
}

// ---------------------------------------------------------------------------
// the parent reference
// ---------------------------------------------------------------------------

// TestParentVersionRefShape: the edge pins a VERSION, and only the canonical
// pid@version form is accepted. An asset id or a slug must not slide in.
func TestParentVersionRefShape(t *testing.T) {
	pid := string(samplePID)
	valid := []string{pid + "@v1", pid + "@2026-01-02", pid + "@v1.2.3-rc1"}
	for _, raw := range valid {
		if !ParentVersionRef(raw).Valid() {
			t.Errorf("ParentVersionRef(%q).Valid() = false, want true", raw)
		}
		gotPID, version, ok := ParseParentVersionRef(raw)
		if !ok || string(gotPID) != pid || version != parentVersion(ParentVersionRef(raw)) {
			t.Errorf("ParseParentVersionRef(%q) = (%q, %q, %v)", raw, gotPID, version, ok)
		}
	}

	invalid := []string{
		"",                           // nothing
		pid,                          // an ASSET id: the parent end must be a version
		"@v1",                        // a label with no asset
		"a-slug@v1",                  // a slug is not a pid
		pid + "@",                    // a version with no label
		pid + "@v1@v2",               // a second @ lands in the label half
		strings.ToUpper(pid) + "@v1", // the pid alphabet is lower-case
		pid + "@v 1",                 // the label charset is [A-Za-z0-9._-]
	}
	for _, raw := range invalid {
		if ParentVersionRef(raw).Valid() {
			t.Errorf("ParentVersionRef(%q).Valid() = true, want false", raw)
		}
		if _, _, ok := ParseParentVersionRef(raw); ok {
			t.Errorf("ParseParentVersionRef(%q) accepted a non-canonical ref", raw)
		}
	}

	// NewParentVersionRef is the constructor the store's edge-resolution uses
	// in reverse; it must agree with the parser on every case above.
	for _, raw := range valid {
		pid, version, _ := ParseParentVersionRef(raw)
		ref, ok := NewParentVersionRef(pid, version)
		if !ok || ref != ParentVersionRef(raw) {
			t.Errorf("NewParentVersionRef(%q, %q) = (%q, %v), want (%q, true)", pid, version, ref, ok, raw)
		}
	}
	if _, ok := NewParentVersionRef(PID("not-a-pid"), "v1"); ok {
		t.Error("NewParentVersionRef accepted a malformed pid")
	}
}

// ---------------------------------------------------------------------------
// the rights verdict: three values, none collapsed
// ---------------------------------------------------------------------------

// TestRequireDerivableAllowed: the parent's publisher said yes. No
// confirmation is required, a supplied one is recorded and decides nothing,
// and Permission reports the declaration's own value — the verdict is an
// account of what was read, not a boolean.
func TestRequireDerivableAllowed(t *testing.T) {
	raw := rightsDocument(t, rights.PermissionAllowed)

	verdict, err := RequireDerivable(raw, "")
	if err != nil {
		t.Fatalf("an `allowed` declaration was refused: %v", err)
	}
	if verdict.Permission != rights.PermissionAllowed {
		t.Errorf("Permission = %q, want %q", verdict.Permission, rights.PermissionAllowed)
	}
	if verdict.ConfirmationRequired {
		t.Error("ConfirmationRequired = true for an `allowed` declaration: nothing was confirmed, the declaration granted it")
	}
	if verdict.Confirmation != "" {
		t.Errorf("Confirmation = %q, want the empty string", verdict.Confirmation)
	}

	// A confirmation supplied anyway is accepted and BOUND to the permission
	// that was actually read. Refusing it would require a client to know the
	// parent's declaration before asking, which is the game of guessing the
	// policy this field exists to avoid; dropping it silently would record a
	// confirmation nobody read.
	verdict, err = RequireDerivable(raw, "  I have read the terms  ")
	if err != nil {
		t.Fatalf("a confirmation beside an `allowed` declaration was refused: %v", err)
	}
	if verdict.Permission != rights.PermissionAllowed {
		t.Errorf("Permission = %q, want %q", verdict.Permission, rights.PermissionAllowed)
	}
	if verdict.Confirmation != "I have read the terms" {
		t.Errorf("Confirmation = %q, want the trimmed sentence", verdict.Confirmation)
	}
	if verdict.ConfirmationRequired {
		t.Errorf("ConfirmationRequired = true: the declaration permitted it, the confirmation decided nothing")
	}
}

// TestRequireDerivableRestrictedRefusesEvenWithAConfirmation is the
// anti-collapse case the task names: a confirmation is a caller's acceptance
// of a declaration that says NOTHING, not a licence to override one that says
// no. If the two "not allowed" values were ever collapsed or a confirmation
// ever overrode an explicit prohibition, this is the test that fails.
func TestRequireDerivableRestrictedRefusesEvenWithAConfirmation(t *testing.T) {
	raw := rightsDocument(t, rights.PermissionRestricted)

	for _, confirmation := range []string{"", "I accept", strings.Repeat("x", MaxConfirmationLen)} {
		verdict, err := RequireDerivable(raw, confirmation)
		if err == nil {
			t.Fatalf("confirmation %q: an explicitly `restricted` parent permitted a derivation (verdict %+v)", confirmation, verdict)
		}
		var refusal *RightsRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("confirmation %q: refusal is %T, want *RightsRefusal", confirmation, err)
		}
		if refusal.Code() != CodeDeriveDerivativesRestricted {
			t.Errorf("confirmation %q: code = %q, want %q — a prohibited derivation must not be reported as an unspecified one",
				confirmation, refusal.Code(), CodeDeriveDerivativesRestricted)
		}
		if !errors.Is(err, ErrDeriveRightsRefused) {
			t.Errorf("confirmation %q: errors.Is(err, ErrDeriveRightsRefused) = false", confirmation)
		}
		if verdict.Permission != "" {
			t.Errorf("confirmation %q: a refused verdict carries Permission %q; it is meaningful only when err is nil", confirmation, verdict.Permission)
		}
	}
}

// TestRequireDerivableUnspecifiedRequiresAConfirmation: the third value is
// neither permission nor prohibition. Without a confirmation it is refused
// with a code of its own (so a client can tell "not declared" from "declared
// no"), and with one it is permitted and reports that the confirmation is
// what permitted it.
func TestRequireDerivableUnspecifiedRequiresAConfirmation(t *testing.T) {
	raw := rightsDocument(t, rights.PermissionUnspecified)

	for _, blank := range []string{"", "   ", "\t\n"} {
		verdict, err := RequireDerivable(raw, blank)
		if err == nil {
			t.Fatalf("confirmation %q: an `unspecified` parent permitted a derivation without one", blank)
		}
		var refusal *RightsRefusal
		if !errors.As(err, &refusal) {
			t.Fatalf("confirmation %q: refusal is %T, want *RightsRefusal", blank, err)
		}
		// NOT the restricted code: silence is not a prohibition, and a client
		// told "restricted" would stop asking.
		if refusal.Code() != CodeDeriveDerivativesUnspecified {
			t.Errorf("confirmation %q: code = %q, want %q", blank, refusal.Code(), CodeDeriveDerivativesUnspecified)
		}
		if !strings.Contains(refusal.Reason, DerivativesConfirmationField) {
			t.Errorf("confirmation %q: refusal %q does not name the field that resolves it (%s)",
				blank, refusal.Reason, DerivativesConfirmationField)
		}
		if verdict.Permission != "" {
			t.Errorf("confirmation %q: a refused verdict carries Permission %q", blank, verdict.Permission)
		}
	}

	verdict, err := RequireDerivable(raw, "  I accept responsibility  ")
	if err != nil {
		t.Fatalf("an `unspecified` parent with a confirmation was refused: %v", err)
	}
	if verdict.Permission != rights.PermissionUnspecified {
		t.Errorf("Permission = %q, want %q: the verdict reports what the document said, not what the confirmation changed",
			verdict.Permission, rights.PermissionUnspecified)
	}
	if !verdict.ConfirmationRequired {
		t.Error("ConfirmationRequired = false: the confirmation is the whole of what permitted this derivation")
	}
	if verdict.Confirmation != "I accept responsibility" {
		t.Errorf("Confirmation = %q, want the trimmed sentence", verdict.Confirmation)
	}
}

// TestRequireDerivableConfirmationBound: the confirmation is a sentence in an
// audit row, not an essay. The bound is checked for the value where it is the
// deciding rule; the command checks it for all values (TestDeriveValidation*).
func TestRequireDerivableConfirmationBound(t *testing.T) {
	raw := rightsDocument(t, rights.PermissionUnspecified)

	if _, err := RequireDerivable(raw, strings.Repeat("x", MaxConfirmationLen)); err != nil {
		t.Errorf("a confirmation of exactly %d characters was refused: %v", MaxConfirmationLen, err)
	}
	_, err := RequireDerivable(raw, strings.Repeat("x", MaxConfirmationLen+1))
	if err == nil {
		t.Fatalf("a confirmation of %d characters was accepted", MaxConfirmationLen+1)
	}
	var refusal *RightsRefusal
	if !errors.As(err, &refusal) || refusal.Code() != CodeDeriveValidationFailed {
		t.Fatalf("over-long confirmation: got %v, want a *RightsRefusal with code %q", err, CodeDeriveValidationFailed)
	}
	// Counting is in CHARACTERS, the unit a person counts: a 512-rune
	// sentence of multi-byte characters is inside the bound, and a bound
	// counted in bytes would refuse it.
	if _, err := RequireDerivable(raw, strings.Repeat("好", MaxConfirmationLen)); err != nil {
		t.Errorf("a confirmation of %d multi-byte characters (%d bytes) was refused: %v",
			MaxConfirmationLen, MaxConfirmationLen*3, err)
	}
}

// TestRequireDerivableRefusesUnreadableRights: an unreadable policy is not a
// permissive one. Every shape below is refused BEFORE the three values are
// considered, so none of them can land in a permissive branch — including the
// empty object, which is the shape the pre-T0705 seed rows carry and which
// internal/rights deliberately does not give a meaning to.
func TestRequireDerivableRefusesUnreadableRights(t *testing.T) {
	// A document that is valid apart from one unrecognised field: the parser
	// refuses unknown fields precisely so that a declaration this build does
	// not know is not silently dropped and then read as silence.
	unknownField := []byte(`{"version":1,"usage":{"derivatives":"allowed"},"unheard_of":"x"}`)

	cases := []struct {
		name string
		raw  []byte
	}{
		{"nil bytes", nil},
		{"empty bytes", []byte{}},
		{"not json", []byte("derivatives: allowed")},
		{"a json array", []byte(`["allowed"]`)},
		{"a json string", []byte(`"allowed"`)},
		{"the empty object", []byte(`{}`)},
		{"a version this build does not know", []byte(`{"version":99,"usage":{"derivatives":"allowed"}}`)},
		{"an unknown field", unknownField},
		{"two documents", []byte(`{"version":1}{"version":1}`)},
		{"a fourth derivatives value", []byte(`{"version":1,"usage":{"derivatives":"maybe"}}`)},
		{"a derivatives value of the wrong type", []byte(`{"version":1,"usage":{"derivatives":true}}`)},
		{"truncated", []byte(`{"version":1,"usage":{"derivatives":"all`)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The confirmation is supplied, so a permissive branch would be
			// reachable if the unreadable case fell through to the
			// unspecified one. It must not: unreadable is refused first.
			verdict, err := RequireDerivable(tc.raw, "I accept")
			if err == nil {
				t.Fatalf("unreadable rights (%s) permitted a derivation: verdict %+v", tc.name, verdict)
			}
			var refusal *RightsRefusal
			if !errors.As(err, &refusal) {
				t.Fatalf("refusal is %T, want *RightsRefusal", err)
			}
			if refusal.Code() != CodeDeriveParentRightsUnreadable {
				t.Errorf("code = %q, want %q", refusal.Code(), CodeDeriveParentRightsUnreadable)
			}
			if refusal.Err == nil {
				t.Error("Err is nil: the parse failure behind an unreadable-document refusal is kept for the log")
			}
		})
	}
}

// TestRequireDerivableDoesNotTrimTheDeclarationsOwnValue: the verdict reads
// the value internal/rights parsed, which is one of the three. Whitespace in
// the stored document is the parser's business, and a verdict that normalised
// it here would be a second interpretation of the document.
func TestRequireDerivableDoesNotCollapseAllowedAndUnspecified(t *testing.T) {
	allowed, err := RequireDerivable(rightsDocument(t, rights.PermissionAllowed), "")
	if err != nil {
		t.Fatalf("allowed: %v", err)
	}
	unspecified, err := RequireDerivable(rightsDocument(t, rights.PermissionUnspecified), "I accept")
	if err != nil {
		t.Fatalf("unspecified + confirmation: %v", err)
	}

	if allowed.Permission == unspecified.Permission {
		t.Fatalf("both verdicts report Permission %q: the two declarations are not one", allowed.Permission)
	}
	if allowed.ConfirmationRequired == unspecified.ConfirmationRequired {
		t.Errorf("ConfirmationRequired is %v for both: a declaration that granted the use and one that said nothing about it are not the same fact",
			allowed.ConfirmationRequired)
	}
}

// ---------------------------------------------------------------------------
// the parent read ruler
// ---------------------------------------------------------------------------

// TestParentReadableMatchesThePageRuler: "readable ⇒ derivable" is the whole
// of the rule, so the derivation's ruler is asserted TWICE — against the table
// the asset page's read gate implies, and against the page's own functions,
// called here on the same inputs.
//
// The second assertion is the one that matters over time: a future edit that
// made ParentReadable a copy of the ruler (rather than a call to it) would
// still pass the table and fail this, which is exactly the failure the rule
// exists to prevent.
func TestParentReadableMatchesThePageRuler(t *testing.T) {
	const projectID = "11111111-1111-1111-1111-111111111111"

	cases := []struct {
		name              string
		projectVisibility Visibility
		versionVisibility Visibility
		member            bool
		want              bool
	}{
		{"public project, public version, stranger", VisibilityPublic, VisibilityPublic, false, true},
		{"public project, private version, stranger", VisibilityPublic, VisibilityPrivate, false, false},
		{"public project, private version, member", VisibilityPublic, VisibilityPrivate, true, true},
		{"public project, public version, member", VisibilityPublic, VisibilityPublic, true, true},
		{"private project, public version, stranger", VisibilityPrivate, VisibilityPublic, false, false},
		{"private project, public version, member", VisibilityPrivate, VisibilityPublic, true, true},
		{"private project, private version, stranger", VisibilityPrivate, VisibilityPrivate, false, false},
		{"private project, private version, member", VisibilityPrivate, VisibilityPrivate, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParentReadable(projectID, tc.projectVisibility, tc.versionVisibility, tc.member)
			if got != tc.want {
				t.Fatalf("ParentReadable = %v, want %v", got, tc.want)
			}
			viewer := PageViewer{Member: tc.member}
			ruler := mayRenderOwnProject(PageProjectState{ID: projectID, Visibility: tc.projectVisibility}, viewer) &&
				memberMaySee(PageVersionState{Visibility: tc.versionVisibility}, viewer)
			if got != ruler {
				t.Errorf("ParentReadable = %v but the page's own ruler = %v over the same inputs: two answers to \"may this caller read this version\"", got, ruler)
			}
		})
	}
}

// TestParentReadableRefusesAnUnnamedProject: the project state's id is not
// decoration — the page refuses to render a project it cannot name, and a
// derivation whose parent has no project is a row the platform does not have.
func TestParentReadableRefusesAnUnnamedProject(t *testing.T) {
	for _, member := range []bool{false, true} {
		if ParentReadable("", VisibilityPublic, VisibilityPublic, member) {
			t.Errorf("member=%v: a parent with no project was readable", member)
		}
	}
}

// ---------------------------------------------------------------------------
// request shape
// ---------------------------------------------------------------------------

// TestDeriveValidationRefusesBeforeAnyRead: every shape refusal fires before
// the membership is resolved, before the ledger is read and before the
// transaction runs. The counts are the assertion — a validation that ran
// after an authorization would be a refusal a stranger could use to tell a
// project that exists from one that does not.
func TestDeriveValidationRefusesBeforeAnyRead(t *testing.T) {
	longSlug := strings.Repeat("s", MaxSlugLen+1)
	longTitle := strings.Repeat("t", MaxTitleLen+1)
	longConfirmation := strings.Repeat("c", MaxConfirmationLen+1)

	cases := []struct {
		name     string
		change   func(*DeriveParams)
		mentions string
	}{
		{"no project", func(p *DeriveParams) { p.ProjectID = "  " }, "project_id"},
		{"no relation", func(p *DeriveParams) { p.Relation = "" }, "relation"},
		{"relation outside the vocabulary", func(p *DeriveParams) { p.Relation = "supersedes" }, "supersedes"},
		{"no parent", func(p *DeriveParams) { p.Parent = "" }, "parent"},
		{"parent is an asset id, not a version", func(p *DeriveParams) { p.Parent = string(samplePID) }, "parent"},
		{"parent is a slug", func(p *DeriveParams) { p.Parent = "a-slug@v1" }, "parent"},
		{"a named asset", func(p *DeriveParams) { p.AssetPID = string(samplePID) }, "asset_id"},
		{"unknown asset type", func(p *DeriveParams) { p.AssetType = "widget" }, "asset_type"},
		{"no version", func(p *DeriveParams) { p.Version = "" }, "version"},
		{"a version label with a slash", func(p *DeriveParams) { p.Version = "v1/2" }, "version"},
		{"unknown visibility", func(p *DeriveParams) { p.Visibility = "internal" }, "visibility"},
		{"no title", func(p *DeriveParams) { p.Title = "   " }, "title"},
		{"no slug", func(p *DeriveParams) { p.Slug = "" }, "slug"},
		{"an over-long slug", func(p *DeriveParams) { p.Slug = longSlug }, "slug"},
		{"an over-long title", func(p *DeriveParams) { p.Title = longTitle }, "title"},
		{"an over-long confirmation", func(p *DeriveParams) { p.Confirmation = longConfirmation }, DerivativesConfirmationField},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			cmd := ownerCommand(t, store)
			in := deriveParams(t)
			tc.change(&in)

			_, err := cmd.Derive(context.Background(), deriveActor(), in)
			if !errors.Is(err, ErrDeriveValidation) {
				t.Fatalf("got %v, want ErrDeriveValidation", err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("refusal %q does not name %q", err.Error(), tc.mentions)
			}
			if store.lookups != 0 || store.derives != 0 {
				t.Errorf("the store was called for a malformed request (lookups=%d derives=%d)", store.lookups, store.derives)
			}
			if members, ok := cmd.members.(*fakeMembers); ok && members.calls != 0 {
				t.Errorf("the membership was resolved for a malformed request (%d calls)", members.calls)
			}
		})
	}
}

// TestDeriveValidationAcceptsTheSmallestRequest: the control for the table
// above — the unmodified fixture reaches the store. Without it, every case
// above would pass on a command that refused everything.
func TestDeriveValidationAcceptsTheSmallestRequest(t *testing.T) {
	store := &fakeStore{}
	cmd := ownerCommand(t, store)
	in := deriveParams(t)

	stored, err := cmd.Derive(context.Background(), deriveActor(), in)
	if err != nil {
		t.Fatalf("the fixture request was refused: %v", err)
	}
	if store.derives != 1 {
		t.Fatalf("store.Derive calls = %d, want 1", store.derives)
	}
	if stored.Version != "v1" {
		t.Errorf("stored version = %q, want %q", stored.Version, "v1")
	}
}

// ---------------------------------------------------------------------------
// the agent backstop
// ---------------------------------------------------------------------------

// TestDeriveAgentBackstopRefusesAndReadsNothing: the domain backstop is the
// first thing Derive does, from the actor value alone. It is assertable
// independently of the matrix (which denies the agent column of
// publish_private_to_public anyway) precisely because it must survive a
// governance edit of that cell.
func TestDeriveAgentBackstopRefusesAndReadsNothing(t *testing.T) {
	store := &fakeStore{}
	cmd := ownerCommand(t, store)
	in := deriveParams(t)

	_, err := cmd.Derive(context.Background(), DeriveActor{User: domain.User{ID: "agent-user"}, IsAgent: true}, in)
	if !errors.Is(err, ErrDeriveAgentNotPermitted) {
		t.Fatalf("got %v, want ErrDeriveAgentNotPermitted", err)
	}
	var refusal *DeriveAgentNotPermittedError
	if !errors.As(err, &refusal) {
		t.Fatalf("refusal is %T, want *DeriveAgentNotPermittedError", err)
	}
	if refusal.Code() != CodeDeriveAgentDenied {
		t.Errorf("code = %q, want %q", refusal.Code(), CodeDeriveAgentDenied)
	}
	if store.lookups != 0 || store.derives != 0 {
		t.Errorf("the store was called for an agent actor (lookups=%d derives=%d)", store.lookups, store.derives)
	}
	if members := cmd.members.(*fakeMembers); members.calls != 0 {
		t.Errorf("the membership was resolved for an agent actor (%d calls)", members.calls)
	}
}

// ---------------------------------------------------------------------------
// authorization
// ---------------------------------------------------------------------------

// TestDeriveAuthorizationIsThePublishDecision walks the target project's
// membership roles through the real matrix engine. The action is
// publish_private_to_public (the same cell the publish resolves, no new
// action), whose V1 column values are: owner allow, maintainer conditional
// (a condition with no specification resolves to a refusal), and deny for
// everyone else.
func TestDeriveAuthorizationIsThePublishDecision(t *testing.T) {
	role := func(r domain.ProjectRole) *domain.ProjectRole { return &r }

	cases := []struct {
		name string
		role *domain.ProjectRole
		err  error
	}{
		{"owner", role(domain.ProjectRoleOwner), nil},
		{"maintainer (conditional, unspecified condition)", role(domain.ProjectRoleMaintainer), ErrDeriveForbidden},
		{"contributor", role(domain.ProjectRoleContributor), ErrDeriveForbidden},
		{"viewer", role(domain.ProjectRoleViewer), ErrDeriveForbidden},
		{"authenticated non-member", nil, ErrDeriveForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			cmd := NewDeriveCommand(DeriveDeps{
				Members:  &fakeMembers{role: tc.role},
				Policies: &fakePolicies{},
				Rules:    &fakeRules{},
				Store:    store,
				Authz:    authz.NewMatrixEngine(),
				NewPID:   func() (PID, error) { return samplePID, nil },
			})
			_, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t))
			if tc.err == nil {
				if err != nil {
					t.Fatalf("an owner was refused: %v", err)
				}
				if store.derives != 1 {
					t.Fatalf("store.Derive calls = %d, want 1", store.derives)
				}
				return
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("got %v, want %v", err, tc.err)
			}
			// The denial precedes every lookup on the parent: a caller that
			// may not derive here learns nothing about which versions exist.
			if store.lookups != 0 || store.derives != 0 {
				t.Errorf("the store was called for a denied actor (lookups=%d derives=%d)", store.lookups, store.derives)
			}
		})
	}
}

// TestDeriveAuthorizationHidesAProjectItCannotFind: an unknown membership is
// not a disclosed absence — projects.ErrMemberNotFound is the answer for both
// "no such project" and "not a member", and it resolves to the same forbidden
// the role table above produces. Only the project surface's own deliberate
// not-found is relayed as a not-found.
func TestDeriveAuthorizationAnswersForAnUnknownProject(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want error
	}{
		{"unknown project or non-member", projects.ErrMemberNotFound, ErrDeriveForbidden},
		{"invisible project", projects.ErrProjectNotFound, ErrDeriveProjectNotFound},
		{"membership store failure", errors.New("connection refused"), ErrDeriveStore},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			cmd := NewDeriveCommand(DeriveDeps{
				Members:  &fakeMembers{err: tc.err},
				Policies: &fakePolicies{},
				Rules:    &fakeRules{},
				Store:    store,
				Authz:    authz.NewMatrixEngine(),
				NewPID:   func() (PID, error) { return samplePID, nil },
			})
			_, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if store.lookups != 0 || store.derives != 0 {
				t.Errorf("the store was called (lookups=%d derives=%d)", store.lookups, store.derives)
			}
		})
	}
}

// TestDeriveRefusesWhenAnAdapterIsMissing: a command wired without a port is
// a wiring bug, and it answers a failure rather than proceeding as if the
// missing check had passed.
func TestDeriveRefusesWhenAnAdapterIsMissing(t *testing.T) {
	for _, tc := range []struct {
		name string
		deps DeriveDeps
	}{
		{"no membership port", DeriveDeps{Store: &fakeStore{}, Authz: authz.NewMatrixEngine()}},
		{"no matrix engine", DeriveDeps{Store: &fakeStore{}, Members: &fakeMembers{}}},
		// The governance step is a REFUSAL when it cannot be made, and a
		// half-wired command is not a command that skips it: the zero-value
		// port must produce a failure, never a silent permit.
		{"no policy port", DeriveDeps{Store: &fakeStore{}, Members: ownerMembers(), Authz: authz.NewMatrixEngine(), Rules: &fakeRules{}}},
		{"no rule evaluator", DeriveDeps{Store: &fakeStore{}, Members: ownerMembers(), Authz: authz.NewMatrixEngine(), Policies: &fakePolicies{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := NewDeriveCommand(tc.deps)
			if _, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t)); !errors.Is(err, ErrDeriveStore) {
				t.Fatalf("got %v, want ErrDeriveStore", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the governance step: the publish's own rule, on the derive path too
// ---------------------------------------------------------------------------

// TestDeriveAppliesThePublishPolicyRule: a PUBLIC derivation asks the policy
// in force the same question the publish asks (domain.RulePublicAssetIPReview)
// and answers a refusal when it is set. Without this step the same document,
// the same project and the same actor would be refused through
// POST .../assets:publish and permitted through POST ...:derive — a route
// around a governance requirement, opened by adding a second write path to
// the same gate.
//
// The four answers are the publish's four, and this test asserts the
// agreement for each: unreadable (error from the port) refuses, unevaluable
// (error from the evaluator) refuses, an explicit true refuses, and
// absent/false permits. The two refusals are what "an unreadable policy is
// not a permissive one" means on this path.
func TestDeriveAppliesThePublishPolicyRule(t *testing.T) {
	cases := []struct {
		name     string
		policies *fakePolicies
		rules    *fakeRules
		wantErr  bool
	}{
		{"rule absent", &fakePolicies{}, &fakeRules{}, false},
		{"rule false", &fakePolicies{}, &fakeRules{decision: policy.Decision{Found: true, Bool: false}}, false},
		{"rule true", &fakePolicies{}, &fakeRules{decision: policy.Decision{Found: true, Bool: true}}, true},
		{"policy unreadable", &fakePolicies{err: errors.New("policy store unavailable")}, &fakeRules{}, true},
		{"rule unevaluable", &fakePolicies{}, &fakeRules{err: errors.New("malformed policy document")}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			cmd := NewDeriveCommand(DeriveDeps{
				Members:  ownerMembers(),
				Policies: tc.policies,
				Rules:    tc.rules,
				Store:    store,
				Authz:    authz.NewMatrixEngine(),
				NewPID:   func() (PID, error) { return samplePID, nil },
			})
			_, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t))
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("a policy that does not forbid it refused the derivation: %v", err)
				}
				if store.derives != 1 {
					t.Fatalf("store.Derive calls = %d, want 1", store.derives)
				}
				return
			}
			if !errors.Is(err, ErrDerivePolicyRefused) {
				t.Fatalf("got %v, want ErrDerivePolicyRefused", err)
			}
			var refused *DerivePolicyRefusedError
			if !errors.As(err, &refused) {
				t.Fatalf("the refusal does not carry its report: %v", err)
			}
			if refused.Rule != domain.RulePublicAssetIPReview {
				t.Errorf("rule = %q, want %q", refused.Rule, domain.RulePublicAssetIPReview)
			}
			// The refusal happens BEFORE the store: a governance refusal is
			// not a transaction that rolled back, it is a transaction that
			// never opened, so nothing was written to roll back.
			if store.derives != 0 {
				t.Errorf("the store ran for a refused derivation (derives=%d)", store.derives)
			}
		})
	}
}

// TestDeriveAsksThePolicyOnlyForAPublicTarget: the rule is about publishing
// a PUBLIC version, so a private derivation — which widens nothing — does not
// read the policy at all. Asserted by the port's call count rather than by
// reading the branch: a private derivation that quietly consulted the policy
// would refuse private work in a project with the rule set, which is not what
// the rule says.
func TestDeriveAsksThePolicyOnlyForAPublicTarget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		visibility Visibility
		wantReads  int
	}{
		{"public target", VisibilityPublic, 1},
		{"private target", VisibilityPrivate, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies := &fakePolicies{}
			// The rule is SET and FALSE, not absent: a policy that mentions
			// the rule is the one whose read the count below can observe. A
			// permitted derivation through a rule that is true does not
			// exist, so this is the only value that lets the public case
			// reach the store at all.
			rules := &fakeRules{decision: policy.Decision{Found: true, Bool: false}}
			cmd := NewDeriveCommand(DeriveDeps{
				Members:  ownerMembers(),
				Policies: policies,
				Rules:    rules,
				Store:    &fakeStore{},
				Authz:    authz.NewMatrixEngine(),
				NewPID:   func() (PID, error) { return samplePID, nil },
			})
			in := deriveParams(t)
			in.Visibility = string(tc.visibility)
			if _, err := cmd.Derive(context.Background(), deriveActor(), in); err != nil {
				t.Fatalf("derivation refused: %v", err)
			}
			if policies.calls != tc.wantReads {
				t.Errorf("policy reads = %d, want %d", policies.calls, tc.wantReads)
			}
			wantQueries := tc.wantReads
			if len(rules.asked) != wantQueries {
				t.Errorf("rule queries = %d, want %d", len(rules.asked), wantQueries)
			}
			for _, q := range rules.asked {
				if q.Rule != domain.RulePublicAssetIPReview {
					t.Errorf("asked about %q, want %q", q.Rule, domain.RulePublicAssetIPReview)
				}
			}
		})
	}
}

// TestDeriveGovernanceRunsAfterAuthorizationAndAfterAReplay pins WHERE the
// step sits, which is the part a later edit is most likely to move.
//
// After authorization, because EffectivePolicy resolves the project's policy
// through the project read gate: asking it first would make the policy port
// the thing that decides whether a stranger learns which projects exist.
// After a replay, because a replay is a READ of a decision already made —
// re-deciding it would turn a successful derivation into a refusal the day
// after the project enables the rule, and an idempotent retry would stop
// being idempotent.
func TestDeriveGovernanceRunsAfterAuthorizationAndAfterAReplay(t *testing.T) {
	t.Run("a denied actor does not reach the policy port", func(t *testing.T) {
		policies := &fakePolicies{}
		rules := &fakeRules{}
		cmd := NewDeriveCommand(DeriveDeps{
			Members:  &fakeMembers{}, // no role: not a member
			Policies: policies,
			Rules:    rules,
			Store:    &fakeStore{},
			Authz:    authz.NewMatrixEngine(),
			NewPID:   func() (PID, error) { return samplePID, nil },
		})
		if _, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t)); !errors.Is(err, ErrDeriveForbidden) {
			t.Fatalf("got %v, want ErrDeriveForbidden", err)
		}
		if policies.calls != 0 || len(rules.asked) != 0 {
			t.Errorf("the policy was read for a denied actor (reads=%d queries=%d)", policies.calls, len(rules.asked))
		}
	})

	t.Run("a replay is answered without reading the policy", func(t *testing.T) {
		key := "replay-key"
		// The ledger entry the replay answers with, and the policy the
		// project has ENABLED since the derivation happened: the rule being
		// true is what makes this case decisive. If the replay re-decided,
		// yesterday's successful derivation would be refused today and an
		// idempotent retry would stop being idempotent.
		stored := DerivedAsset{
			ID: "version-id", AssetID: "asset-id", AssetPID: "01j9z6k3m4n5p6q7r8s9t0v1w2x",
			Version: "v1", Relation: RelationForkedFrom,
			ParentAssetPID: parentAssetPID(parentRef), ParentVersion: parentVersion(parentRef),
		}
		store := &fakeStore{lookup: &stored}
		policies := &fakePolicies{}
		rules := &fakeRules{decision: policy.Decision{Found: true, Bool: true}}
		cmd := NewDeriveCommand(DeriveDeps{
			Members:  ownerMembers(),
			Policies: policies,
			Rules:    rules,
			Store:    store,
			Authz:    authz.NewMatrixEngine(),
			NewPID:   func() (PID, error) { return samplePID, nil },
		})
		in := deriveParams(t)
		in.IdempotencyKey = &key
		got, err := cmd.Derive(context.Background(), deriveActor(), in)
		if err != nil {
			t.Fatalf("the replay was refused: %v", err)
		}
		if got.AssetPID != stored.AssetPID {
			t.Errorf("replay returned %q, want the ledger's %q", got.AssetPID, stored.AssetPID)
		}
		if policies.calls != 0 || len(rules.asked) != 0 {
			t.Errorf("the policy was read for a replay (reads=%d queries=%d)", policies.calls, len(rules.asked))
		}
		if store.derives != 0 {
			t.Errorf("a replay ran the store's transaction (derives=%d)", store.derives)
		}
	})
}

// TestDerivePolicyRefusalCarriesItsReport: the refusal names the rule and the
// answer that produced it, because a client — and an operator reading the log
// — has to be able to tell "your project requires an IP review" from "your
// policy could not be read", and the wire carries one code for both on
// purpose (the publish's RIGHTS_POLICY_BLOCKS_ACTION is about the ACTION
// being blocked, and both answers block it).
func TestDerivePolicyRefusalCarriesItsReport(t *testing.T) {
	cause := errors.New("policy store unavailable")
	cmd := NewDeriveCommand(DeriveDeps{
		Members:  ownerMembers(),
		Policies: &fakePolicies{err: cause},
		Rules:    &fakeRules{},
		Store:    &fakeStore{},
		Authz:    authz.NewMatrixEngine(),
		NewPID:   func() (PID, error) { return samplePID, nil },
	})
	_, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t))
	var refused *DerivePolicyRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("got %v, want a *DerivePolicyRefusedError", err)
	}
	if !errors.Is(refused, cause) {
		t.Errorf("the refusal does not wrap its cause: %v", refused.Unwrap())
	}
	if refused.Code() != CodeDerivePolicyRefused {
		t.Errorf("code = %q, want %q", refused.Code(), CodeDerivePolicyRefused)
	}
	if !errors.Is(err, ErrDerivePolicyRefused) {
		t.Errorf("the refusal is not the sentinel: %v", err)
	}
	if refused.Reason == "" {
		t.Error("the refusal carries no reason")
	}
	if mapDeriveError(err) != err {
		t.Error("mapDeriveError does not pass the sentinel through, so the transport cannot branch on it")
	}
}

// ---------------------------------------------------------------------------
// replay
// ---------------------------------------------------------------------------

// TestDeriveReplayIsARead: an Idempotency-Key that already derived answers
// with the ledger's account and runs no transaction. The ledger's parent,
// relation and version are what come back — not the request echoed — so a
// client can check what the repository holds.
func TestDeriveReplayIsARead(t *testing.T) {
	stored := DerivedAsset{
		ID: "version-id", AssetID: "asset-id", AssetPID: "01j9z6k3m4n5p6q7r8s9t0v1w2x",
		Version: "v1", Relation: RelationForkedFrom,
		ParentAssetPID: parentAssetPID(parentRef), ParentVersion: parentVersion(parentRef),
	}
	store := &fakeStore{lookup: &stored}
	cmd := ownerCommand(t, store)
	key := "one-key"
	in := deriveParams(t)
	in.IdempotencyKey = &key

	got, err := cmd.Derive(context.Background(), deriveActor(), in)
	if err != nil {
		t.Fatalf("the replay was refused: %v", err)
	}
	if store.derives != 0 {
		t.Errorf("store.Derive calls = %d: a replay is a READ", store.derives)
	}
	if store.lookups != 1 {
		t.Errorf("store.LookupDerivation calls = %d, want 1", store.lookups)
	}
	if got.ID != stored.ID || got.AssetPID != stored.AssetPID || got.Version != stored.Version ||
		got.Relation != stored.Relation || got.ParentAssetPID != stored.ParentAssetPID ||
		got.ParentVersion != stored.ParentVersion {
		t.Errorf("replay answered %+v, want the ledger's account %+v", got, stored)
	}
}

// TestDeriveReplayConflict: a key that derived something else may not answer
// for this request. The three compared fields are the parent, the relation
// and the child's version — each case would otherwise hand the caller a row
// it did not ask for and could not tell apart.
func TestDeriveReplayConflict(t *testing.T) {
	base := func() DerivedAsset {
		return DerivedAsset{
			ID: "version-id", AssetID: "asset-id", AssetPID: "01j9z6k3m4n5p6q7r8s9t0v1w2x",
			Version: "v1", Relation: RelationForkedFrom,
			ParentAssetPID: parentAssetPID(parentRef), ParentVersion: parentVersion(parentRef),
		}
	}
	otherParent := string(samplePID)[:25] + "z@v1"

	cases := []struct {
		name   string
		change func(*DerivedAsset)
	}{
		{"a different parent version", func(d *DerivedAsset) {
			pid, version, _ := ParseParentVersionRef(otherParent)
			d.ParentAssetPID, d.ParentVersion = string(pid), version
		}},
		{"a different parent asset", func(d *DerivedAsset) {
			d.ParentAssetPID = "01j9z6k3m4n5p6q7r8s9t0v1w2y"
		}},
		{"a different relation", func(d *DerivedAsset) { d.Relation = RelationDerivedFrom }},
		{"a different child version", func(d *DerivedAsset) { d.Version = "v2" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stored := base()
			tc.change(&stored)
			store := &fakeStore{lookup: &stored}
			cmd := ownerCommand(t, store)
			key := "one-key"
			in := deriveParams(t)
			in.IdempotencyKey = &key

			_, err := cmd.Derive(context.Background(), deriveActor(), in)
			if !errors.Is(err, ErrDeriveIdempotencyConflict) {
				t.Fatalf("got %v, want ErrDeriveIdempotencyConflict", err)
			}
			if store.derives != 0 {
				t.Errorf("store.Derive calls = %d: a conflicting key must not derive again", store.derives)
			}
		})
	}
}

// TestDeriveReplayDoesNotCompareTheMintedPID: the child's pid is minted per
// request, so comparing it would turn every repeat of one derivation into a
// conflict — exactly the case an Idempotency-Key exists for.
func TestDeriveReplayDoesNotCompareTheMintedPID(t *testing.T) {
	stored := DerivedAsset{
		ID: "version-id", AssetID: "asset-id", AssetPID: "01j9z6k3m4n5p6q7r8s9t0v1w2x",
		Version: "v1", Relation: RelationForkedFrom,
		ParentAssetPID: parentAssetPID(parentRef), ParentVersion: parentVersion(parentRef),
	}
	store := &fakeStore{lookup: &stored}
	cmd := ownerCommand(t, store) // mints samplePID, which differs from the stored pid
	key := "one-key"
	in := deriveParams(t)
	in.IdempotencyKey = &key

	if _, err := cmd.Derive(context.Background(), deriveActor(), in); err != nil {
		t.Fatalf("a repeat with a freshly minted pid was refused: %v", err)
	}
}

// TestDeriveLookupFailureIsAStoreFailure: a ledger that could not be read is
// not an empty ledger. Proceeding would derive a second time under a key that
// may already have derived.
func TestDeriveLookupFailureIsAStoreFailure(t *testing.T) {
	store := &fakeStore{lookupErr: errors.New("connection refused")}
	cmd := ownerCommand(t, store)
	key := "one-key"
	in := deriveParams(t)
	in.IdempotencyKey = &key

	_, err := cmd.Derive(context.Background(), deriveActor(), in)
	if !errors.Is(err, ErrDeriveStore) {
		t.Fatalf("got %v, want ErrDeriveStore", err)
	}
	if store.derives != 0 {
		t.Errorf("store.Derive calls = %d: an unreadable ledger must not be treated as an empty one", store.derives)
	}
}

// ---------------------------------------------------------------------------
// what the command hands the store
// ---------------------------------------------------------------------------

// TestDeriveMintsAFreshIdentity: every request mints a pid of its own, the
// candidate carries it, and the caller cannot supply one. Two requests
// produce two pids — the newness of the identity is the point of the command.
func TestDeriveMintsAFreshIdentity(t *testing.T) {
	first := PID(string(samplePID)[:25] + "0")
	second := PID(string(samplePID)[:25] + "1")
	next := 0
	role := domain.ProjectRoleOwner
	minted := []PID{first, second}
	store := &fakeStore{}
	cmd := NewDeriveCommand(DeriveDeps{
		Members:  &fakeMembers{role: &role},
		Policies: &fakePolicies{},
		Rules:    &fakeRules{},
		Store:    store,
		Authz:    authz.NewMatrixEngine(),
		NewPID:   func() (PID, error) { p := minted[next]; next++; return p, nil },
	})

	for i, want := range minted {
		key := "key-" + string(rune('a'+i))
		in := deriveParams(t)
		in.IdempotencyKey = &key
		if _, err := cmd.Derive(context.Background(), deriveActor(), in); err != nil {
			t.Fatalf("derivation %d: %v", i, err)
		}
		if got := string(store.lastReq.Candidate.AssetPID); got != string(want) {
			t.Errorf("derivation %d stored pid %q, want the freshly minted %q", i, got, want)
		}
	}
	if minted[0] == minted[1] {
		t.Fatal("fixture is broken: the two minted pids are equal")
	}
}

// TestDerivePIDGeneratorFailureIsAStoreFailure: a pid that could not be
// minted is a failure of this service, and the request is refused rather than
// stored under an empty identity.
func TestDerivePIDGeneratorFailureIsAStoreFailure(t *testing.T) {
	role := domain.ProjectRoleOwner
	store := &fakeStore{}
	cmd := NewDeriveCommand(DeriveDeps{
		Members:  &fakeMembers{role: &role},
		Policies: &fakePolicies{},
		Rules:    &fakeRules{},
		Store:    store,
		Authz:    authz.NewMatrixEngine(),
		NewPID:   func() (PID, error) { return "", errors.New("entropy unavailable") },
	})
	if _, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t)); !errors.Is(err, ErrDeriveStore) {
		t.Fatalf("got %v, want ErrDeriveStore", err)
	}
	if store.derives != 0 {
		t.Error("the store was called without a pid")
	}

	// A generator that returns something that is not a pid is the same
	// failure: an identity the platform cannot name is not stored.
	bad := NewDeriveCommand(DeriveDeps{
		Members:  &fakeMembers{role: &role},
		Policies: &fakePolicies{},
		Rules:    &fakeRules{},
		Store:    &fakeStore{},
		Authz:    authz.NewMatrixEngine(),
		NewPID:   func() (PID, error) { return PID("not-a-pid"), nil },
	})
	if _, err := bad.Derive(context.Background(), deriveActor(), deriveParams(t)); !errors.Is(err, ErrDeriveStore) {
		t.Fatalf("a malformed minted pid: got %v, want ErrDeriveStore", err)
	}
}

// TestDeriveHandsTheStoreTheDecisionsItHasMade: the request the store
// receives carries the parent as a parsed ref, the relation, the trimmed
// confirmation, the audit row keyed to the parent and relation, and the
// declared usages of the child's manifest. The store is the component that
// re-reads the parent's declaration and the current state; everything above
// is this command's to decide and it decides it before the transaction.
func TestDeriveHandsTheStoreTheDecisionsItHasMade(t *testing.T) {
	pin, _ := NewDependencyPin(samplePID, "pinned-v1")
	manifest := manifestFor(TypeDataset)
	manifest.DependencyPins = []DependencyPin{pin}
	rawManifest, err := manifest.CanonicalJSON()
	if err != nil {
		t.Fatalf("render the fixture manifest: %v", err)
	}

	store := &fakeStore{}
	cmd := ownerCommand(t, store)
	key := "a-key"
	in := deriveParams(t)
	in.Manifest = rawManifest
	in.Confirmation = "   I accept responsibility   "
	in.Visibility = string(VisibilityPrivate)
	in.IdempotencyKey = &key

	if _, err := cmd.Derive(context.Background(), deriveActor(), in); err != nil {
		t.Fatalf("derivation: %v", err)
	}

	req := store.lastReq
	if req.Parent != parentRef {
		t.Errorf("request parent = %q, want %q", req.Parent, parentRef)
	}
	if req.Relation != RelationForkedFrom {
		t.Errorf("request relation = %q, want %q", req.Relation, RelationForkedFrom)
	}
	if req.Confirmation != "I accept responsibility" {
		t.Errorf("request confirmation = %q, want the trimmed sentence", req.Confirmation)
	}
	if req.IdempotencyKey == nil || *req.IdempotencyKey != "a-key" {
		t.Errorf("request idempotency key = %v, want %q", req.IdempotencyKey, "a-key")
	}
	if string(req.Candidate.Version) != "v1" || req.Candidate.Visibility != VisibilityPrivate {
		t.Errorf("candidate = version %q visibility %q", req.Candidate.Version, req.Candidate.Visibility)
	}

	// The usages are the manifest's pins, at the CHILD's visibility: the
	// declaration is the child project's, and its visibility is the child
	// version's (PublishedUsages).
	if len(req.Usages) != 1 {
		t.Fatalf("usages = %+v, want one declaration for the one pin", req.Usages)
	}
	if req.Usages[0].Pin != pin {
		t.Errorf("usage pin = %q, want %q", req.Usages[0].Pin, pin)
	}
	if req.Usages[0].Type != DependencyTypeDependsOn {
		t.Errorf("usage type = %q, want %q", req.Usages[0].Type, DependencyTypeDependsOn)
	}
	if req.Usages[0].VisibilityOfUsage != VisibilityPrivate {
		t.Errorf("usage visibility = %q, want the child version's %q", req.Usages[0].VisibilityOfUsage, VisibilityPrivate)
	}

	// The audit row: the action is this command's own, and the parent and
	// relation are in the summary rather than only in the edge — a reader of
	// the audit trail must see WHAT was derived from what without a join.
	if req.Audit.Action != ActionAssetDerived {
		t.Errorf("audit action = %q, want %q", req.Audit.Action, ActionAssetDerived)
	}
	if req.Audit.ActorID != deriveActor().User.ID {
		t.Errorf("audit actor = %q, want %q", req.Audit.ActorID, deriveActor().User.ID)
	}
	if req.Audit.ProjectID != in.ProjectID {
		t.Errorf("audit project = %q, want the TARGET project %q", req.Audit.ProjectID, in.ProjectID)
	}
	summary, ok := req.Audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("audit summary is %T, want a map the jsonb column can hold", req.Audit.AfterSummary)
	}
	if got := summary["parent"]; got != string(parentRef) {
		t.Errorf("audit summary parent = %v, want %q", got, parentRef)
	}
	if got := summary["relation"]; got != string(RelationForkedFrom) {
		t.Errorf("audit summary relation = %v, want %q", got, RelationForkedFrom)
	}
}

// TestDeriveAuditSummaryDoesNotCarryTheConfirmation: the command does not put
// the confirmation in the summary it builds — the STORE adds it, because the
// store is the component that read the parent's declaration and therefore the
// only one that knows whether the confirmation was what permitted anything.
// Two writers of one key would make the row depend on which ran last.
func TestDeriveAuditSummaryDoesNotCarryTheConfirmation(t *testing.T) {
	store := &fakeStore{}
	cmd := ownerCommand(t, store)
	in := deriveParams(t)
	in.Confirmation = "I accept"

	if _, err := cmd.Derive(context.Background(), deriveActor(), in); err != nil {
		t.Fatalf("derivation: %v", err)
	}
	summary, ok := store.lastReq.Audit.AfterSummary.(map[string]any)
	if !ok {
		t.Fatalf("audit summary is %T, want a map", store.lastReq.Audit.AfterSummary)
	}
	for _, key := range []string{"derivatives_declared", "derivatives_confirmation", "derivatives_confirmation_required"} {
		if _, present := summary[key]; present {
			t.Errorf("the command wrote %q into the audit summary; the store owns the three derivatives_* keys", key)
		}
	}
}

// ---------------------------------------------------------------------------
// error mapping
// ---------------------------------------------------------------------------

// TestMapDeriveErrorKeepsTheSentinelAndWrapsTheRest: the transport branches on
// the sentinels, so every outcome this package names must survive mapping, and
// anything else must become a store failure rather than an unrecognised error
// the transport would answer 500 for.
func TestMapDeriveErrorKeepsTheSentinelAndWrapsTheRest(t *testing.T) {
	sentinels := []error{
		ErrDeriveValidation, ErrDeriveAgentNotPermitted, ErrDeriveForbidden,
		ErrDeriveProjectNotFound, ErrDeriveParentNotFound, ErrDeriveRightsRefused,
		ErrDeriveAssetExists, ErrDeriveVersionImmutable, ErrDeriveIdempotencyConflict,
		ErrDeriveRefused, ErrDeriveStore,
	}
	for _, sentinel := range sentinels {
		got := mapDeriveError(sentinel)
		if !errors.Is(got, sentinel) {
			t.Errorf("mapDeriveError(%v) = %v, want the sentinel", sentinel, got)
		}
	}
	// A sentinel the store wrapped with detail is kept wrapped, not flattened:
	// the transport matches by errors.Is, and the log keeps the detail.
	wrapped := fmt.Errorf("%w: no row for pid@version", ErrDeriveParentNotFound)
	got := mapDeriveError(wrapped)
	if !errors.Is(got, ErrDeriveParentNotFound) {
		t.Errorf("mapDeriveError(%v) = %v, want the wrapped sentinel", wrapped, got)
	}
	if !strings.Contains(got.Error(), "no row for pid@version") {
		t.Errorf("mapDeriveError(%v) = %v, want the wrapping kept", wrapped, got)
	}
	if got := mapDeriveError(errors.New("some driver failure")); !errors.Is(got, ErrDeriveStore) {
		t.Errorf("mapDeriveError(unknown) = %v, want ErrDeriveStore", got)
	}
	if got := mapDeriveError(nil); got != nil {
		t.Errorf("mapDeriveError(nil) = %v, want nil", got)
	}
}

// TestDeriveStoreFailureIsMapped: a store that failed is reported as a store
// failure, never as a refusal about the caller's request.
func TestDeriveStoreFailureIsMapped(t *testing.T) {
	store := &fakeStore{deriveErr: errors.New("connection refused")}
	cmd := ownerCommand(t, store)
	if _, err := cmd.Derive(context.Background(), deriveActor(), deriveParams(t)); !errors.Is(err, ErrDeriveStore) {
		t.Fatalf("got %v, want ErrDeriveStore", err)
	}
}
