// Package memstore provides in-memory implementations of the authn ports
// (UserStore, SessionStore, RateLimiter) and the profile port
// (profile.ProfileStore). It exists for two consumers: development with
// zero infrastructure and the test suites (unit + e2e), where the
// adapters' behaviour is exact but the storage is ephemeral.
//
// NOT for production identity storage: POST canonical truth is PostgreSQL
// (CLAUDE.md §8); the production adapters are the pgx credential store and
// the Redis session/rate-limit stores in internal/persistence.
package memstore

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/application/profile"
	"github.com/lichman0405/post/internal/domain"
)

// Users is an in-memory UserStore + profile.ProfileStore: one identity
// space with the profile content attached, mirroring how production keeps
// users and profiles side by side in PostgreSQL. Email lookups run on the
// normalized form; the store is safe for concurrent use.
type Users struct {
	mu       sync.Mutex
	byEmail  map[string]authn.UserRecord
	byID     map[string]authn.UserRecord
	byHandle map[string]string // handle -> user id
	bio      map[string]string // user id -> bio
	nextID   int
}

// NewUsers builds an empty in-memory user store.
func NewUsers() *Users {
	return &Users{
		byEmail:  map[string]authn.UserRecord{},
		byID:     map[string]authn.UserRecord{},
		byHandle: map[string]string{},
		bio:      map[string]string{},
		nextID:   1,
	}
}

// Seed inserts a ready-made record (test/dev bootstrap).
func (s *Users) Seed(rec authn.UserRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byEmail[domain.NormalizeEmail(rec.User.Email)] = rec
	s.byID[rec.User.ID] = rec
	s.byHandle[rec.User.Handle] = rec.User.ID
	if _, ok := s.bio[rec.User.ID]; !ok {
		s.bio[rec.User.ID] = ""
	}
}

// freshID returns a deterministic id shaped like a uuid v4 text form, so
// tests stay reproducible while the shape matches the production uuid
// column.
func (s *Users) freshID() string {
	s.nextID++
	return freshUUID(s.nextID)
}

// freshUUID builds a deterministic uuid-v4-shaped id from a counter value
// (the production uuid column's shape), shared by every memstore identity
// space so ids stay reproducible across tests.
func freshUUID(n int) string {
	var b [16]byte
	for i := range b {
		b[i] = byte(n * (i + 1))
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// FindByEmail implements authn.UserStore.
func (s *Users) FindByEmail(_ context.Context, email string) (authn.UserRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byEmail[domain.NormalizeEmail(email)]
	if !ok {
		return authn.UserRecord{}, authn.ErrUserNotFound
	}
	return rec, nil
}

// GetByID implements authn.UserStore.
func (s *Users) GetByID(_ context.Context, id string) (authn.UserRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[id]
	if !ok {
		return authn.UserRecord{}, authn.ErrUserNotFound
	}
	return rec, nil
}

// CreateWithPassword implements authn.UserStore.
func (s *Users) CreateWithPassword(_ context.Context, email, passwordHash, handle, displayName string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = domain.NormalizeEmail(email)
	if _, ok := s.byEmail[email]; ok {
		return domain.User{}, authn.ErrEmailTaken
	}
	user := domain.User{
		ID:          s.freshID(),
		Handle:      uniqueHandle(s.byID, handle),
		Email:       email,
		DisplayName: displayName,
		CreatedAt:   time.Now().UTC(),
	}
	rec := authn.UserRecord{User: user, PasswordHash: passwordHash}
	s.byEmail[email] = rec
	s.byID[user.ID] = rec
	s.byHandle[user.Handle] = user.ID
	s.bio[user.ID] = ""
	return user, nil
}

// CreateOIDC implements authn.UserStore.
func (s *Users) CreateOIDC(_ context.Context, email, handle, displayName string) (domain.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	email = domain.NormalizeEmail(email)
	if _, ok := s.byEmail[email]; ok {
		return domain.User{}, authn.ErrEmailTaken
	}
	user := domain.User{
		ID:          s.freshID(),
		Handle:      uniqueHandle(s.byID, handle),
		Email:       email,
		DisplayName: displayName,
		CreatedAt:   time.Now().UTC(),
	}
	rec := authn.UserRecord{User: user}
	s.byEmail[email] = rec
	s.byID[user.ID] = rec
	s.byHandle[user.Handle] = user.ID
	s.bio[user.ID] = ""
	return user, nil
}

// GetByUserID implements profile.ProfileStore.
func (s *Users) GetByUserID(_ context.Context, userID string) (domain.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[userID]
	if !ok {
		return domain.Profile{}, profile.ErrNotFound
	}
	return domain.Profile{User: rec.User, Bio: s.bio[userID]}, nil
}

// GetByHandle implements profile.ProfileStore. Callers pass the
// normalized handle (the service normalizes).
func (s *Users) GetByHandle(_ context.Context, handle string) (domain.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byHandle[handle]
	if !ok {
		return domain.Profile{}, profile.ErrNotFound
	}
	rec := s.byID[id]
	return domain.Profile{User: rec.User, Bio: s.bio[id]}, nil
}

// Update implements profile.ProfileStore. The same uniqueness rule as
// creation applies: a handle held by another identity is ErrHandleTaken.
func (s *Users) Update(_ context.Context, userID string, upd profile.Update) (domain.Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.byID[userID]
	if !ok {
		return domain.Profile{}, profile.ErrNotFound
	}
	if upd.Handle != nil {
		if other, taken := s.byHandle[*upd.Handle]; taken && other != userID {
			return domain.Profile{}, profile.ErrHandleTaken
		}
		delete(s.byHandle, rec.User.Handle)
		rec.User.Handle = *upd.Handle
		s.byHandle[rec.User.Handle] = userID
	}
	if upd.DisplayName != nil {
		rec.User.DisplayName = *upd.DisplayName
	}
	if upd.Bio != nil {
		s.bio[userID] = *upd.Bio
	}
	s.byID[userID] = rec
	s.byEmail[domain.NormalizeEmail(rec.User.Email)] = rec
	return domain.Profile{User: rec.User, Bio: s.bio[userID]}, nil
}

// uniqueHandle de-duplicates a handle against existing users with a
// deterministic counter suffix (bob, bob-2, bob-3...).
func uniqueHandle(byID map[string]authn.UserRecord, want string) string {
	taken := map[string]bool{}
	for _, rec := range byID {
		taken[rec.User.Handle] = true
	}
	if !taken[want] {
		return want
	}
	for i := 2; ; i++ {
		candidate := want + "-" + itoa(i)
		if !taken[candidate] {
			return candidate
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// Sessions is an in-memory SessionStore with expiry sweeping.
type Sessions struct {
	mu      sync.Mutex
	entries map[string]authn.Session
}

// NewSessions builds an empty session store.
func NewSessions() *Sessions {
	return &Sessions{entries: map[string]authn.Session{}}
}

// Create implements authn.SessionStore.
func (s *Sessions) Create(_ context.Context, sess authn.Session, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[sess.Token] = sess
	return nil
}

// Get implements authn.SessionStore.
func (s *Sessions) Get(_ context.Context, token string) (authn.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.entries[token]
	if !ok {
		return authn.Session{}, authn.ErrSessionNotFound
	}
	if time.Now().After(sess.ExpiresAt) {
		delete(s.entries, token)
		return authn.Session{}, authn.ErrSessionNotFound
	}
	return sess, nil
}

// Delete implements authn.SessionStore.
func (s *Sessions) Delete(_ context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, token)
	return nil
}

// Limiter is an in-memory fixed-window RateLimiter.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*window
}

type window struct {
	start time.Time
	count int
}

// NewLimiter builds an empty limiter.
func NewLimiter() *Limiter {
	return &Limiter{buckets: map[string]*window{}}
}

// Check implements authn.RateLimiter.
func (l *Limiter) Check(_ context.Context, bucket string, limit int, windowDur time.Duration) (bool, time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w, ok := l.buckets[bucket]
	if !ok || now.Sub(w.start) >= windowDur {
		w = &window{start: now}
		l.buckets[bucket] = w
	}
	if w.count >= limit {
		retry := windowDur - now.Sub(w.start)
		if retry < 0 {
			retry = 0
		}
		return false, retry, nil
	}
	w.count++
	return true, 0, nil
}

// FrozenLimiter returns a limiter that always allows (tests that must not
// trip limits), and ErrLimiter returns one that always errors (fail-closed
// tests). Both implement authn.RateLimiter.
type alwaysLimiter struct{}

func (alwaysLimiter) Check(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return true, 0, nil
}

// AllowAll returns a RateLimiter that never blocks.
func AllowAll() authn.RateLimiter { return alwaysLimiter{} }

// ErrLimiter returns a RateLimiter that always fails.
type errLimiter struct{}

func (errLimiter) Check(context.Context, string, int, time.Duration) (bool, time.Duration, error) {
	return false, 0, errors.New("limiter unavailable")
}

// Broken returns a RateLimiter that always errors.
func Broken() authn.RateLimiter { return errLimiter{} }

var _ authn.UserStore = (*Users)(nil)
var _ profile.ProfileStore = (*Users)(nil)
var _ authn.SessionStore = (*Sessions)(nil)
var _ authn.RateLimiter = (*Limiter)(nil)
