package notifications

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/events"
)

// T1005 unit suite for the digest sender. The database-backed suite proves
// the pipeline against live rows; THIS one pins what a real database makes
// hard to reach on purpose — the gate's branches, the per-subscriber failure
// isolation, and the rule that nothing is marked delivered that was not
// sent.
//
// The fake stores count their calls: a sender that marked rows delivered
// before the transport answered, or that rendered an item from a value the
// claim returned instead of the live state, would behave identically
// against a happy database and differently here.

const (
	testUserID = "3f8a1c62-9b4d-4f1e-8a77-0c2d5e6f7a80"
	testMail   = "digest@example.com"
)

// fakeDigestStore is the DigestStore port over in-memory answers. Every
// read is scriptable, so each case can put the sender in front of exactly
// the state it is supposed to decide on.
type fakeDigestStore struct {
	claimed    []events.EmailDelivery
	claimErr   error
	states     map[string]events.EmailDeliveryState
	levels     map[string]events.AudienceLevel
	labels     map[string]string
	stateErr   error
	audienceEr error
	labelErr   error
	markErr    error

	exhaustedCalls []int
	withdrawn      []string
	marked         []string
	recorded       []time.Time
	recordedFor    []string
	claimCalls     int
}

func (s *fakeDigestStore) WithdrawExhaustedEmailDeliveries(_ context.Context, maxAttempts int) (int, error) {
	s.exhaustedCalls = append(s.exhaustedCalls, maxAttempts)
	return 0, nil
}

func (s *fakeDigestStore) ClaimEmailDeliveries(_ context.Context, _, _ int) ([]events.EmailDelivery, error) {
	s.claimCalls++
	return s.claimed, s.claimErr
}

func (s *fakeDigestStore) EmailDeliveryState(_ context.Context, id string) (events.EmailDeliveryState, error) {
	if s.stateErr != nil {
		return events.EmailDeliveryState{}, s.stateErr
	}
	return s.states[id], nil
}

func (s *fakeDigestStore) TargetAudienceFor(_ context.Context, t events.Target, _ string) (events.AudienceLevel, error) {
	if s.audienceEr != nil {
		return "", s.audienceEr
	}
	return s.levels[t.Type+":"+t.ID], nil
}

func (s *fakeDigestStore) TargetLabel(_ context.Context, t events.Target) (string, error) {
	if s.labelErr != nil {
		return "", s.labelErr
	}
	return s.labels[t.Type+":"+t.ID], nil
}

func (s *fakeDigestStore) MarkEmailDelivered(_ context.Context, ids []string) (int, error) {
	if s.markErr != nil {
		return 0, s.markErr
	}
	s.marked = append(s.marked, ids...)
	return len(ids), nil
}

func (s *fakeDigestStore) WithdrawEmailDelivery(_ context.Context, id string) error {
	s.withdrawn = append(s.withdrawn, id)
	return nil
}

func (s *fakeDigestStore) RecordDigestSent(_ context.Context, userID string, at time.Time) error {
	s.recordedFor = append(s.recordedFor, userID)
	s.recorded = append(s.recorded, at)
	return nil
}

// fakeMailer records what the sender handed it and can refuse.
type fakeMailer struct {
	sent  []Mail
	err   error
	calls int
}

func (m *fakeMailer) Send(_ context.Context, mail Mail) error {
	m.calls++
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, mail)
	return nil
}

func senderTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// pendingDelivery is one claimable row: pending, authorized, live.
func pendingDelivery(id, userID, targetID string) events.EmailDelivery {
	return events.EmailDelivery{
		ID:             id,
		UserID:         userID,
		SubscriptionID: "sub-" + id,
		EventID:        "evt-" + id,
		EventType:      "state.committed",
		Target:         events.Target{Type: events.TargetTypeProject, ID: targetID},
		CreatedAt:      time.Date(2026, 9, 16, 9, 0, 0, 0, time.UTC),
		Attempts:       1,
		Email:          testMail,
	}
}

// sendableState is the state that passes every branch of the gate.
func sendableState(visibility string, occurredAt time.Time) events.EmailDeliveryState {
	return events.EmailDeliveryState{
		Found:            true,
		Status:           events.SubscriptionDeliveryPending,
		SubscriptionLive: true,
		EventVisibility:  visibility,
		OccurredAt:       occurredAt,
	}
}

func newTestSender(store DigestStore, mailer Mailer, opts ...SenderOption) *Sender {
	opts = append([]SenderOption{WithSenderLogger(senderTestLogger())}, opts...)
	return NewSender(store, mailer, opts...)
}

// TestSenderNoTransportRefuses: without a transport NOTHING may be marked
// delivered. The fail-closed direction matters more than the error itself:
// a sender that "sent" into a nil transport would empty the queue of
// messages nobody ever received.
func TestSenderNoTransportRefuses(t *testing.T) {
	store := &fakeDigestStore{claimed: []events.EmailDelivery{pendingDelivery("d1", testUserID, testEventProject)}}
	s := newTestSender(store, nil)

	sent, err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("RunOnce with no transport = nil error, want a refusal")
	}
	if sent != 0 {
		t.Errorf("sent = %d, want 0", sent)
	}
	if store.claimCalls != 0 {
		t.Errorf("the claim ran %d times without a transport, want 0: nothing may be claimed that cannot be sent", store.claimCalls)
	}
	if len(store.marked) != 0 {
		t.Errorf("marked delivered %v with no transport", store.marked)
	}
}

// TestSenderSendsAuthorizedDigest is the happy path plus the anchor: one
// message per subscriber carrying every claimed delivery, the rows marked
// delivered, and the send time recorded as the next interval's anchor.
func TestSenderSendsAuthorizedDigest(t *testing.T) {
	occurred := time.Date(2026, 9, 16, 8, 30, 0, 0, time.UTC)
	first := pendingDelivery("d1", testUserID, testEventProject)
	second := pendingDelivery("d2", testUserID, testEventProject)
	second.EventType = "asset.published"
	second.CreatedAt = first.CreatedAt.Add(time.Minute)

	store := &fakeDigestStore{
		claimed: []events.EmailDelivery{first, second},
		states: map[string]events.EmailDeliveryState{
			// The second row's event happened at a DIFFERENT time from when
			// its delivery row was created — the case that tells a digest
			// dated by the event from one dated by the queue.
			"d1": sendableState(events.VisibilityPrivate, occurred),
			"d2": sendableState(events.VisibilityPublic, occurred.Add(time.Hour)),
		},
		levels: map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
		labels: map[string]string{events.TargetTypeProject + ":" + testEventProject: "Project Zephyr"},
	}
	mailer := &fakeMailer{}
	clock := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	s := newTestSender(store, mailer, WithSenderClock(func() time.Time { return clock }),
		WithSenderBaseURL("http://web.test"))

	sent, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 2 {
		t.Errorf("sent = %d, want 2", sent)
	}
	if len(mailer.sent) != 1 {
		t.Fatalf("messages = %d, want 1 — one digest per subscriber per pass, not one per event", len(mailer.sent))
	}
	mail := mailer.sent[0]
	if mail.To != testMail {
		t.Errorf("To = %q, want the account's address", mail.To)
	}
	// Both items are in the SAME message, in claim order, each dated by its
	// own event — not by the row's created_at (which is an hour apart from
	// the second event).
	if !strings.Contains(mail.Text, "2026-09-16T08:30:00Z") {
		t.Errorf("the first item is not dated by its event's occurred_at:\n%s", mail.Text)
	}
	if !strings.Contains(mail.Text, "2026-09-16T09:30:00Z") {
		t.Errorf("the second item is not dated by its event's occurred_at (it was rendered from the claim's "+
			"created_at):\n%s", mail.Text)
	}
	if strings.Count(mail.Text, "Project Zephyr") != 2 {
		t.Errorf("the label is not on both lines:\n%s", mail.Text)
	}
	if !strings.Contains(mail.Text, "http://web.test/projects/"+testEventProject) {
		t.Errorf("the item link is missing or wrong:\n%s", mail.Text)
	}
	if !strings.Contains(mail.Text, "http://web.test/notifications") {
		t.Errorf("the footer link is missing:\n%s", mail.Text)
	}

	if len(store.marked) != 2 || store.marked[0] != "d1" || store.marked[1] != "d2" {
		t.Errorf("marked = %v, want both claimed ids", store.marked)
	}
	if len(store.recorded) != 1 || !store.recorded[0].Equal(clock) {
		t.Errorf("the digest anchor = %v, want [%v] — the cadence window is measured from the SEND", store.recorded, clock)
	}
	if len(store.exhaustedCalls) != 1 || store.exhaustedCalls[0] != events.MaxEmailDeliveryAttempts {
		t.Errorf("exhausted sweep called with %v, want one call with MaxEmailDeliveryAttempts (%d)",
			store.exhaustedCalls, events.MaxEmailDeliveryAttempts)
	}
}

// TestSenderGateWithdrawsRevokedAccess is the acceptance criterion at the
// unit level: a delivery whose subscriber LOST access between the fan-out
// and the send is withdrawn and never rendered. The message must not name
// the target, and no row may be marked delivered.
func TestSenderGateWithdrawsRevokedAccess(t *testing.T) {
	cases := []struct {
		name  string
		state events.EmailDeliveryState
		level events.AudienceLevel
	}{
		{
			name:  "the project turned private under a public-level follower",
			state: sendableState(events.VisibilityPrivate, time.Now()),
			level: events.AudiencePublic,
		},
		{
			name:  "the membership is gone",
			state: sendableState(events.VisibilityPrivate, time.Now()),
			level: events.AudienceNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := pendingDelivery("d1", testUserID, testEventProject)
			store := &fakeDigestStore{
				claimed: []events.EmailDelivery{d},
				states:  map[string]events.EmailDeliveryState{"d1": tc.state},
				levels:  map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: tc.level},
				labels:  map[string]string{events.TargetTypeProject + ":" + testEventProject: "Project Zephyr"},
			}
			mailer := &fakeMailer{}
			s := newTestSender(store, mailer, WithSenderBaseURL("http://web.test"))

			sent, err := s.RunOnce(context.Background())
			if err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if sent != 0 || mailer.calls != 0 {
				t.Errorf("sent = %d, transport calls = %d, want 0 and 0: the revoked subscriber receives nothing",
					sent, mailer.calls)
			}
			if len(store.withdrawn) != 1 || store.withdrawn[0] != "d1" {
				t.Errorf("withdrawn = %v, want [d1]: the row must leave the queue, not sit pending for the "+
					"next pass to retry", store.withdrawn)
			}
			if len(store.marked) != 0 {
				t.Errorf("marked = %v, want none", store.marked)
			}
			if len(store.recorded) != 0 {
				t.Errorf("the digest anchor was recorded (%v) although nothing was sent", store.recorded)
			}
		})
	}
}

// TestSenderGateSkipsResolvedRows: a row that is no longer pending is
// somebody else's outcome — leave it alone. Withdrawing it would overwrite
// what actually happened to it (delivered, or cancelled by its owner).
func TestSenderGateSkipsResolvedRows(t *testing.T) {
	cases := []struct {
		name  string
		state events.EmailDeliveryState
	}{
		{"already delivered by another sender", events.EmailDeliveryState{Found: true, Status: events.SubscriptionDeliveryDelivered, SubscriptionLive: true, EventVisibility: events.VisibilityPublic}},
		{"cancelled by the owner's unsubscribe", events.EmailDeliveryState{Found: true, Status: events.SubscriptionDeliveryCancelled, SubscriptionLive: false, EventVisibility: events.VisibilityPublic}},
		{"the row is gone", events.EmailDeliveryState{Found: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeDigestStore{
				claimed: []events.EmailDelivery{pendingDelivery("d1", testUserID, testEventProject)},
				states:  map[string]events.EmailDeliveryState{"d1": tc.state},
				levels:  map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
			}
			mailer := &fakeMailer{}
			s := newTestSender(store, mailer)

			if _, err := s.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if mailer.calls != 0 {
				t.Errorf("transport calls = %d, want 0", mailer.calls)
			}
			if len(store.withdrawn) != 0 {
				t.Errorf("withdrawn = %v, want none: the row is not ours to resolve any more", store.withdrawn)
			}
			if len(store.marked) != 0 {
				t.Errorf("marked = %v, want none", store.marked)
			}
		})
	}
}

// TestSenderGateFailsClosedOnStoreErrors: "could not check" must never read
// as "allowed" (and must not read as "withdraw" either — the delivery is
// still the subscriber's, the next pass re-checks).
func TestSenderGateFailsClosedOnStoreErrors(t *testing.T) {
	cases := []struct {
		name  string
		store *fakeDigestStore
	}{
		{"the delivery state cannot be read", &fakeDigestStore{stateErr: errors.New("connection reset")}},
		{"the audience cannot be resolved", &fakeDigestStore{audienceEr: errors.New("connection reset")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.store.claimed = []events.EmailDelivery{pendingDelivery("d1", testUserID, testEventProject)}
			tc.store.states = map[string]events.EmailDeliveryState{"d1": sendableState(events.VisibilityPublic, time.Now())}
			mailer := &fakeMailer{}
			s := newTestSender(tc.store, mailer)

			sent, err := s.RunOnce(context.Background())
			if err == nil {
				t.Fatal("RunOnce = nil error, want the failure reported so the pass is retried")
			}
			if sent != 0 || mailer.calls != 0 {
				t.Errorf("sent = %d, transport calls = %d, want 0 and 0", sent, mailer.calls)
			}
			if len(tc.store.withdrawn) != 0 {
				t.Errorf("withdrawn = %v, want none: an unreadable state is not a decision", tc.store.withdrawn)
			}
			if len(tc.store.marked) != 0 {
				t.Errorf("marked = %v, want none", tc.store.marked)
			}
		})
	}
}

// TestSenderTransportFailureLeavesRowsPending: the at-least-once contract.
// A refused send marks nothing delivered, records no anchor, and does not
// stop the other subscribers.
func TestSenderTransportFailureLeavesRowsPending(t *testing.T) {
	failing := pendingDelivery("d1", testUserID, testEventProject)
	ok := pendingDelivery("d2", "11111111-2222-3333-4444-555555555555", testEventProject)
	ok.Email = "other@example.com"

	store := &fakeDigestStore{
		claimed: []events.EmailDelivery{failing, ok},
		states: map[string]events.EmailDeliveryState{
			"d1": sendableState(events.VisibilityPublic, time.Now()),
			"d2": sendableState(events.VisibilityPublic, time.Now()),
		},
		levels: map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
		labels: map[string]string{events.TargetTypeProject + ":" + testEventProject: "Project Zephyr"},
	}

	// A transport that refuses ONE address — the failure is per subscriber.
	mailer := &partialMailer{refuse: testMail}
	s := newTestSender(store, mailer)

	sent, err := s.RunOnce(context.Background())
	if err == nil {
		t.Error("RunOnce = nil error although one subscriber's send failed, want the failure surfaced")
	}
	if sent != 1 {
		t.Errorf("sent = %d, want 1: the other subscriber's digest still went out", sent)
	}
	if len(store.marked) != 1 || store.marked[0] != "d2" {
		t.Errorf("marked = %v, want only the delivered row", store.marked)
	}
	if len(store.recordedFor) != 1 || store.recordedFor[0] != ok.UserID {
		t.Errorf("anchors recorded for %v, want only the subscriber actually sent", store.recordedFor)
	}
}

// partialMailer refuses one address and accepts the rest.
type partialMailer struct {
	refuse string
	sent   []Mail
	calls  int
}

func (m *partialMailer) Send(_ context.Context, mail Mail) error {
	m.calls++
	if mail.To == m.refuse {
		return errors.New("transport refused")
	}
	m.sent = append(m.sent, mail)
	return nil
}

// TestSenderWithdrawsAddresslessAccounts: an account with no address cannot
// be delivered to by any transport, so the row leaves the queue with the
// reason rather than being retried until it exhausts its attempts.
func TestSenderWithdrawsAddresslessAccounts(t *testing.T) {
	d := pendingDelivery("d1", testUserID, testEventProject)
	d.Email = ""
	store := &fakeDigestStore{
		claimed: []events.EmailDelivery{d},
		states:  map[string]events.EmailDeliveryState{"d1": sendableState(events.VisibilityPublic, time.Now())},
		levels:  map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
	}
	mailer := &fakeMailer{}
	s := newTestSender(store, mailer)

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if mailer.calls != 0 {
		t.Errorf("transport calls = %d, want 0", mailer.calls)
	}
	if len(store.withdrawn) != 1 || store.withdrawn[0] != "d1" {
		t.Errorf("withdrawn = %v, want [d1]", store.withdrawn)
	}
}

// TestSenderGroupsBySubscriber: two subscribers, two messages; one
// subscriber with two events, one message. The grouping is what makes a
// digest a digest.
func TestSenderGroupsBySubscriber(t *testing.T) {
	other := "11111111-2222-3333-4444-555555555555"
	a := pendingDelivery("d1", testUserID, testEventProject)
	b := pendingDelivery("d2", other, testEventProject)
	b.Email = "other@example.com"
	c := pendingDelivery("d3", testUserID, testEventProject)
	c.CreatedAt = a.CreatedAt.Add(time.Minute)

	store := &fakeDigestStore{
		claimed: []events.EmailDelivery{a, b, c},
		states: map[string]events.EmailDeliveryState{
			"d1": sendableState(events.VisibilityPublic, time.Now()),
			"d2": sendableState(events.VisibilityPublic, time.Now()),
			"d3": sendableState(events.VisibilityPublic, time.Now()),
		},
		levels: map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
		labels: map[string]string{events.TargetTypeProject + ":" + testEventProject: "Project Zephyr"},
	}
	mailer := &fakeMailer{}
	s := newTestSender(store, mailer)

	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(mailer.sent) != 2 {
		t.Fatalf("messages = %d, want 2 (one per subscriber)", len(mailer.sent))
	}
	byTo := map[string]Mail{}
	for _, m := range mailer.sent {
		byTo[m.To] = m
	}
	if n := strings.Count(byTo[testMail].Text, "- 20"); n != 2 {
		t.Errorf("the first subscriber's digest carries %d items, want 2:\n%s", n, byTo[testMail].Text)
	}
	if n := strings.Count(byTo["other@example.com"].Text, "- 20"); n != 1 {
		t.Errorf("the second subscriber's digest carries %d items, want 1", n)
	}
}

// TestSenderLabelFailureStillNotifies: the audience gate passed, so the
// event is the subscriber's; a label that could not be read is a worse
// rendering, not a reason to drop the notification.
func TestSenderLabelFailureStillNotifies(t *testing.T) {
	store := &fakeDigestStore{
		claimed:  []events.EmailDelivery{pendingDelivery("d1", testUserID, testEventProject)},
		states:   map[string]events.EmailDeliveryState{"d1": sendableState(events.VisibilityPublic, time.Now())},
		levels:   map[string]events.AudienceLevel{events.TargetTypeProject + ":" + testEventProject: events.AudienceMember},
		labelErr: errors.New("connection reset"),
	}
	mailer := &fakeMailer{}
	s := newTestSender(store, mailer)

	sent, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if sent != 1 || len(mailer.sent) != 1 {
		t.Fatalf("sent = %d, messages = %d, want a message anyway", sent, len(mailer.sent))
	}
	if !strings.Contains(mailer.sent[0].Text, testEventProject) {
		t.Errorf("the item does not fall back to the target's identity:\n%s", mailer.sent[0].Text)
	}
}

// TestSenderNoClaimIsSilent: an empty queue costs nothing — no message, no
// anchor, no error.
func TestSenderNoClaimIsSilent(t *testing.T) {
	store := &fakeDigestStore{}
	mailer := &fakeMailer{}
	s := newTestSender(store, mailer)

	sent, err := s.RunOnce(context.Background())
	if err != nil || sent != 0 {
		t.Fatalf("RunOnce = (%d, %v), want (0, nil)", sent, err)
	}
	if mailer.calls != 0 || len(store.recorded) != 0 {
		t.Errorf("an empty claim produced transport calls=%d anchors=%v", mailer.calls, store.recorded)
	}
}

// testEventProject is the target id the unit cases use.
const testEventProject = "0b6f5b1e-6f4a-4a1f-9d3e-2c7a5b8e4d10"
