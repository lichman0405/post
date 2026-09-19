package events

import (
	"encoding/json"
	"errors"
	"testing"
)

// Task T1002 unit suite: the subscription vocabulary and the pure rules the
// pipeline is built from. The database-backed half — the audience rule
// against live rows, the fan-out's delivery and withdrawal, the
// unsubscribe's atomicity — is tests/integration/subscription_test.go.

func TestValidateTarget(t *testing.T) {
	const uuid = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	const pid = "01j9z6k3m4n5p6q7r8s9t0v1w2"
	cases := []struct {
		name string
		in   Target
		ok   bool
	}{
		{"project by uuid", Target{TargetTypeProject, uuid}, true},
		{"asset by pid", Target{TargetTypeAsset, pid}, true},
		{"knowledge by pid", Target{TargetTypeKnowledge, pid}, true},
		{"user by uuid", Target{TargetTypeUser, uuid}, true},
		{"organization by uuid", Target{TargetTypeOrganization, uuid}, true},
		{"unknown type", Target{"planet", uuid}, false},
		{"empty type", Target{"", uuid}, false},
		// The id's shape is per type: an asset and a published knowledge
		// object are addressed by their pid and nothing else, and the row-id
		// targets by a uuid. Getting this backwards is what would turn the
		// audience queries' ::uuid casts into a runtime error — and for
		// knowledge it is the bug T0901 fixed: a publication's row uuid never
		// leaves the process, so a target demanding one could never be
		// created.
		{"asset addressed by uuid", Target{TargetTypeAsset, uuid}, false},
		{"knowledge addressed by uuid", Target{TargetTypeKnowledge, uuid}, false},
		{"project addressed by pid", Target{TargetTypeProject, pid}, false},
		{"knowledge pid with an excluded letter", Target{TargetTypeKnowledge, "01j9z6k3m4n5p6q7r8s9t0v1wL"}, false},
		{"knowledge pid one character short", Target{TargetTypeKnowledge, pid[:25]}, false},
		{"empty id", Target{TargetTypeProject, ""}, false},
		{"upper-case uuid", Target{TargetTypeProject, "3F8A1C62-9B4D-4F1E-8A77-0C2D5E6F7A80"}, false},
		{"asset pid with an excluded letter", Target{TargetTypeAsset, "01j9z6k3m4n5p6q7r8s9t0v1wL"}, false},
		{"asset pid one character short", Target{TargetTypeAsset, pid[:25]}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateTarget(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("ValidateTarget(%+v) = %v, want nil", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateTarget(%+v) = nil, want an error", tc.in)
			}
			if err != nil && !errors.Is(err, ErrTargetShape) {
				t.Errorf("ValidateTarget(%+v) = %v, want ErrTargetShape", tc.in, err)
			}
		})
	}
}

func TestValidateChannels(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		ok   bool
	}{
		{"web only", []string{ChannelWeb}, true},
		{"email only", []string{ChannelEmail}, true},
		{"both, in order", []string{ChannelWeb, ChannelEmail}, true},
		{"both, reversed", []string{ChannelEmail, ChannelWeb}, true},
		{"nil", nil, false},
		{"empty", []string{}, false},
		{"unknown channel", []string{"webhook"}, false},
		{"empty value", []string{""}, false},
		{"duplicate", []string{ChannelWeb, ChannelWeb}, false},
		{"three values", []string{ChannelWeb, ChannelEmail, ChannelWeb}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateChannels(tc.in)
			if tc.ok && err != nil {
				t.Fatalf("ValidateChannels(%v) = %v, want nil", tc.in, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("ValidateChannels(%v) = nil, want an error", tc.in)
			}
		})
	}
}

// TestDelivers pins the public/private rule as a table over the whole
// cross-product, because every combination is a decision someone could get
// wrong in the other direction: the two "member" rows are the ones a
// fail-closed mistake would break, and the two "public" rows are the ones
// a leak would break.
func TestDelivers(t *testing.T) {
	cases := []struct {
		level      AudienceLevel
		visibility string
		want       bool
	}{
		{AudienceMember, VisibilityPublic, true},
		{AudienceMember, VisibilityPrivate, true},
		{AudiencePublic, VisibilityPublic, true},
		{AudiencePublic, VisibilityPrivate, false},
		{AudienceNone, VisibilityPublic, false},
		{AudienceNone, VisibilityPrivate, false},
		// An unrecognised level is not a relationship: fail closed.
		{AudienceLevel("root"), VisibilityPublic, false},
		{AudienceLevel(""), VisibilityPublic, false},
		// An unrecognised visibility is not public: fail closed.
		{AudiencePublic, "internal", false},
		{AudiencePublic, "", false},
	}
	for _, tc := range cases {
		if got := Delivers(tc.level, tc.visibility); got != tc.want {
			t.Errorf("Delivers(%q, %q) = %v, want %v", tc.level, tc.visibility, got, tc.want)
		}
	}
}

// TestWithdrawFlags pins the two booleans the fan-out derives from Delivers
// and hands to the withdrawal statement (cancelUndeliverableDeliveries'
// $4/$5). The statement applies them to the visibility of the event each
// queued row points at, so the pairs ARE the rule for what may be
// withdrawn. Each combination is a decision that could be wrong in the
// dangerous direction, which is why they are pinned rather than derived
// here as well.
func TestWithdrawFlags(t *testing.T) {
	cases := []struct {
		level         AudienceLevel
		wantNoPrivate bool // withdraw queued PRIVATE events
		wantNoPublic  bool // withdraw queued PUBLIC events
	}{
		{AudienceMember, false, false}, // keeps everything: nothing to withdraw
		{AudiencePublic, true, false},  // loses what only a member may see
		{AudienceNone, true, true},     // loses everything, delivered or not
	}
	for _, tc := range cases {
		noPrivate := !Delivers(tc.level, VisibilityPrivate)
		noPublic := !Delivers(tc.level, VisibilityPublic)
		if noPrivate != tc.wantNoPrivate || noPublic != tc.wantNoPublic {
			t.Errorf("level %q: withdraw flags (private %v, public %v), want (%v, %v)",
				tc.level, noPrivate, noPublic, tc.wantNoPrivate, tc.wantNoPublic)
		}
		// The same flags decide the event in hand: whatever may be
		// withdrawn may not be delivered, so the two halves of the
		// decision cannot contradict each other.
		for _, visibility := range []string{VisibilityPublic, VisibilityPrivate} {
			withdraw := noPublic
			if visibility != VisibilityPublic {
				withdraw = noPrivate
			}
			if withdraw == Delivers(tc.level, visibility) {
				t.Errorf("level %q, %s event: withdraw = %v but delivers = %v — a row would be "+
					"delivered and withdrawn by the same rule", tc.level, visibility, withdraw, Delivers(tc.level, visibility))
			}
		}
	}
}

func TestSubscribesTo(t *testing.T) {
	cases := []struct {
		name      string
		filters   []string
		eventType string
		want      bool
	}{
		{"empty list admits everything", nil, "state.committed", true},
		{"empty slice admits everything", []string{}, "anything.at.all", true},
		{"exact match", []string{"state.committed"}, "state.committed", true},
		{"one of several", []string{"branch.created", "state.committed"}, "state.committed", true},
		{"miss", []string{"branch.created"}, "state.committed", false},
		// The match is exact, not a prefix: a filter is an event type, and
		// treating it as a namespace would silently widen what a subscriber
		// asked for.
		{"a prefix is not a match", []string{"state"}, "state.committed", false},
		{"a longer filter is not a match", []string{"state.committed.extra"}, "state.committed", false},
		{"case matters", []string{"STATE.COMMITTED"}, "state.committed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SubscribesTo(tc.filters, tc.eventType); got != tc.want {
				t.Errorf("SubscribesTo(%v, %q) = %v, want %v", tc.filters, tc.eventType, got, tc.want)
			}
		})
	}
}

func TestEventTargets(t *testing.T) {
	const (
		actor     = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
		project   = "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3"
		assetPID  = "01j9z6k3m4n5p6q7r8s9t0v1w2"
		otherUUID = "aa11bb22-cc33-4d44-8e55-ff6677889900"
	)
	cases := []struct {
		name      string
		eventType string
		actor     string
		project   string
		payload   string
		want      []Target
	}{
		{
			"a project event addresses the project and its actor",
			"state.committed", actor, project, `{}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			"no project means no project target",
			"user.profile_updated", actor, "", `{}`,
			[]Target{{TargetTypeUser, actor}},
		},
		{
			"no actor means no user target",
			"organization.created", "", "", `{}`,
			nil,
		},
		{
			"a version publish addresses the asset its payload names",
			EventTypeAssetVersionPublished, actor, project, `{"asset_id":"` + assetPID + `"}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}, {TargetTypeAsset, assetPID}},
		},
		{
			// A malformed payload key is skipped, not guessed at: the
			// mapping is closed, and an absent or unshaped id addresses
			// nothing rather than addressing a target nobody can resolve.
			"a publish with no asset id addresses only the envelope",
			EventTypeAssetVersionPublished, actor, project, `{}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			"a publish whose asset id is not a pid addresses no asset",
			EventTypeAssetVersionPublished, actor, project, `{"asset_id":"` + otherUUID + `"}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			"a payload key that is not a target key addresses nothing extra",
			EventTypeAssetVersionPublished, actor, project, `{"object_id":"` + assetPID + `"}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			"an unparseable payload addresses only the envelope",
			EventTypeAssetVersionPublished, actor, project, `{not json`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			// A published knowledge object is addressed by its pid, exactly
			// as an asset is: the producer writes the pid
			// (knowledge_publish_store.go knowledgeVersionPublishedPayload)
			// and the public read resolves one BY pid.
			"a knowledge publish addresses the publication its payload names",
			EventTypeKnowledgeVersionPublished, actor, project, `{"publication_id":"` + assetPID + `"}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}, {TargetTypeKnowledge, assetPID}},
		},
		{
			// The publication's ROW uuid is not its identity: it never leaves
			// the process, so an event carrying one addresses no knowledge
			// target.
			"a knowledge publish whose publication id is a uuid addresses no knowledge target",
			EventTypeKnowledgeVersionPublished, actor, project, `{"publication_id":"` + otherUUID + `"}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
		{
			"a knowledge publish with no publication id addresses only the envelope",
			EventTypeKnowledgeVersionPublished, actor, project, `{}`,
			[]Target{{TargetTypeProject, project}, {TargetTypeUser, actor}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EventTargets(tc.eventType, tc.actor, tc.project, []byte(tc.payload))
			if len(got) != len(tc.want) {
				t.Fatalf("EventTargets = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("EventTargets[%d] = %+v, want %+v (full: %+v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// TestEventTargetsEveryTargetIsValidatable is the invariant the fan-out
// depends on: every target EventTargets produces passes ValidateTarget, so
// the audience queries can cast what they are handed. The one target the
// mapping cannot validate from the envelope — organization, resolved from
// the schema by the fan-out — is not produced here at all.
func TestEventTargetsEveryTargetIsValidatable(t *testing.T) {
	payloads := []string{
		`{}`,
		`{"asset_id":"01j9z6k3m4n5p6q7r8s9t0v1w2"}`,
		`{"asset_id":"nonsense"}`,
		`{"asset_id":""}`,
		`{"publication_id":"01j9z6k3m4n5p6q7r8s9t0v1w2"}`,
		`{"publication_id":"3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"}`,
		`{"publication_id":"nonsense"}`,
	}
	for _, e := range []string{"state.committed", EventTypeAssetVersionPublished, EventTypeKnowledgeVersionPublished} {
		for _, p := range payloads {
			for _, targets := range [][]Target{
				EventTargets(e, "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80", "7c1b2d33-4e55-4a66-9b77-8c99d0e1f2a3", []byte(p)),
				EventTargets(e, "", "", []byte(p)),
			} {
				for _, target := range targets {
					if err := ValidateTarget(target); err != nil {
						t.Errorf("EventTargets(%s, %s) produced %+v: %v", e, p, target, err)
					}
					// The organization is the fan-out's to resolve from the
					// schema, so it is the one target type that must NOT
					// appear here.
					if target.Type == TargetTypeOrganization {
						t.Errorf("EventTargets(%s, %s) produced an organization target; the "+
							"organization is the fan-out's to resolve", e, p)
					}
				}
			}
		}
	}
}

// TestSubscriptionTargetRoundTrip pins Target() against the columns it
// renders: the fan-out hands the result straight to the audience queries,
// so a subscription whose Target() lost a field would resolve the wrong
// relationship.
func TestSubscriptionTargetRoundTrip(t *testing.T) {
	sub := Subscription{TargetType: TargetTypeAsset, TargetID: "01j9z6k3m4n5p6q7r8s9t0v1w2"}
	if got := sub.Target(); got != (Target{TargetTypeAsset, sub.TargetID}) {
		t.Fatalf("Target() = %+v", got)
	}
	if err := ValidateTarget(sub.Target()); err != nil {
		t.Fatalf("a subscription's own target does not validate: %v", err)
	}
}

// TestSubscriptionPayloadJSONIsClosed guards the one place a raw payload is
// read: an event whose payload is valid JSON but not an object must not
// panic or invent a target.
func TestSubscriptionPayloadJSONIsClosed(t *testing.T) {
	for _, payload := range []string{`null`, `[]`, `"a string"`, `42`, `{}`} {
		if !json.Valid([]byte(payload)) {
			t.Fatalf("fixture %q is not valid JSON", payload)
		}
		if got := EventTargets(EventTypeAssetVersionPublished, "", "", []byte(payload)); len(got) != 0 {
			t.Errorf("EventTargets(..., %s) = %+v, want no targets", payload, got)
		}
	}
}
