package authn_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lichman0405/post/internal/application/authn"
	"github.com/lichman0405/post/internal/domain"
	"github.com/lichman0405/post/internal/persistence/memstore"
)

// testService builds the service over memory adapters.
func testService(t *testing.T, users *memstore.Users) (*authn.Service, *memstore.Sessions, *memstore.Limiter) {
	t.Helper()
	sessions := memstore.NewSessions()
	limiter := memstore.NewLimiter()
	cfg := authn.Config{
		WebOrigin:          "http://127.0.0.1:3000",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 5,
		LoginLimitPerIP:    100,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   100,
	}
	return authn.NewService(users, sessions, limiter, nil, nil, cfg), sessions, limiter
}

func seedPasswordUser(t *testing.T, users *memstore.Users, email, password, handle string) string {
	t.Helper()
	hash, err := authn.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	user, err := users.CreateWithPassword(context.Background(), email, hash, handle, handle)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user.ID
}

func TestSignupLoginLogoutSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	svc, sessions, _ := testService(t, users)

	// Signup creates the account and a live session.
	result, err := svc.Signup(ctx, "Alice@Example.com ", "correct-horse-battery", "alice", "Alice", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if result.User.Email != "alice@example.com" {
		t.Errorf("email normalized: %q, want alice@example.com", result.User.Email)
	}
	if result.Session.Token == "" || result.Session.CSRFToken == "" {
		t.Fatal("signup must return a session with token and csrf token")
	}

	// The session resolves to the user.
	user, sess, err := svc.Authenticate(ctx, result.Session.Token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if user.ID != result.User.ID || sess.CSRFToken != result.Session.CSRFToken {
		t.Error("authenticated session does not match signup session")
	}

	// Login with the same credentials mints a fresh session.
	login, err := svc.Login(ctx, "ALICE@example.com", "correct-horse-battery", "10.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if login.Session.Token == result.Session.Token {
		t.Error("login must issue a new session token")
	}

	// Logout revokes.
	if err := svc.Logout(ctx, login.Session.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, _, err := svc.Authenticate(ctx, login.Session.Token); !errors.Is(err, authn.ErrSessionNotFound) {
		t.Errorf("Authenticate after logout = %v, want authn.ErrSessionNotFound", err)
	}

	// Logout is idempotent.
	if err := svc.Logout(ctx, login.Session.Token); err != nil {
		t.Errorf("second Logout = %v, want nil", err)
	}

	// Empty token is "no session", never an error.
	if _, _, err := svc.Authenticate(ctx, ""); !errors.Is(err, authn.ErrSessionNotFound) {
		t.Errorf("Authenticate(\"\") = %v, want authn.ErrSessionNotFound", err)
	}

	_ = sessions
}

// TestLoginEnumerationProtection is the acceptance criterion "账号枚举防护":
// an unknown email and a wrong password must produce the SAME error
// (identical code, message, wrapped sentinel) and burn the same argon2id
// work.
func TestLoginEnumerationProtection(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	seedPasswordUser(t, users, "known@example.com", "right-password-123", "known")
	svc, _, _ := testService(t, users)

	_, errWrongPassword := svc.Login(ctx, "known@example.com", "wrong-password-456", "10.0.0.1")
	_, errUnknownEmail := svc.Login(ctx, "nobody@example.com", "wrong-password-456", "10.0.0.1")

	if !errors.Is(errWrongPassword, authn.ErrInvalidCredentials) {
		t.Fatalf("wrong password error = %v, want authn.ErrInvalidCredentials", errWrongPassword)
	}
	if !errors.Is(errUnknownEmail, authn.ErrInvalidCredentials) {
		t.Fatalf("unknown email error = %v, want authn.ErrInvalidCredentials", errUnknownEmail)
	}
	if errWrongPassword.Error() != errUnknownEmail.Error() {
		t.Errorf("enumeration leak: wrong-password %q vs unknown-email %q", errWrongPassword, errUnknownEmail)
	}
}

// TestLoginEnumerationTimingIsUniform is the timing half of the criterion:
// both paths must run the same argon2id work. The assertion is generous
// (CI noise) but a missing dummy-hash would differ by ~2 orders of
// magnitude.
func TestLoginEnumerationTimingIsUniform(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	seedPasswordUser(t, users, "known@example.com", "right-password-123", "known")
	svc, _, _ := testService(t, users)

	measure := func(email string) time.Duration {
		start := time.Now()
		_, _ = svc.Login(ctx, email, "some-password-123", "10.0.0.1")
		return time.Since(start)
	}
	var known, unknown time.Duration
	for i := 0; i < 3; i++ {
		known += measure("known@example.com")
		unknown += measure("nobody@example.com")
	}
	ratio := float64(unknown) / float64(known)
	if ratio > 3 || ratio < 0.3 {
		t.Errorf("timing ratio unknown/known = %.2f, want roughly 1 (dummy hash must match real work)", ratio)
	}
}

func TestLoginRejectsDisabledAccountIdentically(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	id := seedPasswordUser(t, users, "gone@example.com", "right-password-123", "gone")
	rec, _ := users.GetByID(ctx, id)
	disabledAt := time.Now().Add(-time.Hour)
	rec.User.DisabledAt = &disabledAt
	users.Seed(rec)
	svc, _, _ := testService(t, users)

	_, err := svc.Login(ctx, "gone@example.com", "right-password-123", "10.0.0.1")
	if !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Errorf("disabled account login = %v, want authn.ErrInvalidCredentials (same as any failure)", err)
	}
	// An existing session of a disabled account must stop authenticating.
	users2 := memstore.NewUsers()
	seedPasswordUser(t, users2, "active@example.com", "right-password-123", "active")
	svc2, _, _ := testService(t, users2)
	login, err := svc2.Login(ctx, "active@example.com", "right-password-123", "10.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	rec2, _ := users2.GetByID(ctx, login.User.ID)
	rec2.User.DisabledAt = &disabledAt
	users2.Seed(rec2)
	if _, _, err := svc2.Authenticate(ctx, login.Session.Token); !errors.Is(err, authn.ErrSessionNotFound) {
		t.Errorf("disabled account session = %v, want authn.ErrSessionNotFound", err)
	}
}

// TestLoginEnumerationTimingUniformForOIDCAndDisabled extends the timing
// criterion to the other account shapes: an OIDC-only account (empty
// password hash) and a disabled account must burn the same argon2id work
// as an unknown email. A fast path here lets an attacker distinguish
// "registered OIDC/disabled account" from "no account" by timing alone —
// the M1 regression (measured ~7000x before the fix).
func TestLoginEnumerationTimingUniformForOIDCAndDisabled(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	seedPasswordUser(t, users, "password@example.com", "right-password-123", "pw")
	if _, err := users.CreateOIDC(ctx, "oidc-only@example.com", "oidc-only", "OIDC Only"); err != nil {
		t.Fatalf("seed OIDC-only user: %v", err)
	}
	disabledID := seedPasswordUser(t, users, "disabled@example.com", "right-password-123", "disabled")
	rec, _ := users.GetByID(ctx, disabledID)
	disabledAt := time.Now().Add(-time.Hour)
	rec.User.DisabledAt = &disabledAt
	users.Seed(rec)
	svc, _, _ := testService(t, users)

	measure := func(email, password string) time.Duration {
		start := time.Now()
		_, _ = svc.Login(ctx, email, password, "10.0.0.1")
		return time.Since(start)
	}
	paths := map[string]func() time.Duration{
		"unknown email":  func() time.Duration { return measure("nobody@example.com", "some-password-123") },
		"oidc-only":      func() time.Duration { return measure("oidc-only@example.com", "some-password-123") },
		"disabled":       func() time.Duration { return measure("disabled@example.com", "right-password-123") },
		"wrong password": func() time.Duration { return measure("password@example.com", "wrong-password-456") },
	}
	sums := map[string]time.Duration{}
	for name, m := range paths {
		for i := 0; i < 3; i++ {
			sums[name] += m()
		}
	}
	// Every path must land in the same band as the unknown-email baseline
	// (the real regression is ~7000x, so a generous 0.3-3 band is safe).
	baseline := sums["unknown email"]
	for name, sum := range sums {
		if ratio := float64(sum) / float64(baseline); ratio > 3 || ratio < 0.3 {
			t.Errorf("timing ratio %s/unknown-email = %.2f, want roughly 1 (every failure must burn the same KDF work)", name, ratio)
		}
	}
}

func TestLoginRateLimitPerEmail(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	seedPasswordUser(t, users, "bruteforced@example.com", "right-password-123", "bf")
	svc, _, _ := testService(t, users) // limit 5 per email per minute

	var lastErr error
	for i := 0; i < 5; i++ {
		_, lastErr = svc.Login(ctx, "bruteforced@example.com", "wrong-password-"+string(rune('a'+i))+"-xxxxxxx", "10.0.0.1")
		if lastErr != nil && errors.Is(lastErr, authn.ErrInvalidCredentials) {
			continue
		}
	}
	// Attempt 6 trips the limiter.
	_, err := svc.Login(ctx, "bruteforced@example.com", "wrong-password-9999", "10.0.0.1")
	var rateErr *authn.RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("6th attempt = %v, want authn.RateLimitError", err)
	}
	if rateErr.RetryAfter <= 0 || rateErr.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want (0, 1m]", rateErr.RetryAfter)
	}
	// Even the correct password is refused while limited (the limit is on
	// attempts, not on failures — it must not be an oracle either).
	_, err = svc.Login(ctx, "bruteforced@example.com", "right-password-123", "10.0.0.1")
	if !errors.As(err, &rateErr) {
		t.Errorf("correct password while limited = %v, want authn.RateLimitError", err)
	}
}

func TestLoginFailsClosedWhenLimiterErrors(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	seedPasswordUser(t, users, "x@example.com", "right-password-123", "x")
	cfg := authn.Config{SessionTTL: time.Hour, LoginLimitPerEmail: 5, LoginLimitPerIP: 100, LoginWindow: time.Minute}
	svc := authn.NewService(users, memstore.NewSessions(), memstore.Broken(), nil, nil, cfg)

	_, err := svc.Login(ctx, "x@example.com", "right-password-123", "10.0.0.1")
	if !errors.Is(err, authn.ErrUnavailable) {
		t.Errorf("login with broken limiter = %v, want authn.ErrUnavailable (fail closed)", err)
	}
}

func TestSignupValidationAndDuplicateEmail(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	svc, _, _ := testService(t, users)

	if _, err := svc.Signup(ctx, "not-an-email", "long-enough-password", "", "", "10.0.0.1"); !errors.Is(err, authn.ErrValidation) {
		t.Errorf("bad email = %v, want authn.ErrValidation", err)
	}
	if _, err := svc.Signup(ctx, "a@b.co", "short", "", "", "10.0.0.1"); !errors.Is(err, authn.ErrValidation) {
		t.Errorf("short password = %v, want authn.ErrValidation", err)
	}
	if _, err := svc.Signup(ctx, "a@b.co", "long-enough-password", "", "", "10.0.0.1"); err != nil {
		t.Fatalf("valid signup: %v", err)
	}
	_, err := svc.Signup(ctx, "A@B.CO", "another-password-123", "", "", "10.0.0.1")
	if !errors.Is(err, authn.ErrEmailTaken) {
		t.Errorf("duplicate email (case-variant) = %v, want authn.ErrEmailTaken", err)
	}
}

// TestHandleDerivation: an omitted handle derives from the email local
// part; collisions resolve deterministically.
func TestHandleDerivationAndCollision(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	svc, _, _ := testService(t, users)

	first, err := svc.Signup(ctx, "bob.smith@example.com", "long-enough-password", "", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if first.User.Handle != "bob-smith" {
		t.Errorf("derived handle = %q, want bob-smith", first.User.Handle)
	}
	second, err := svc.Signup(ctx, "bob.smith@other.example", "long-enough-password", "", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup 2: %v", err)
	}
	if second.User.Handle == first.User.Handle {
		t.Errorf("colliding derived handle not de-duplicated: both %q", first.User.Handle)
	}
	// Display name defaults to the derived handle (pre-dedupe); the
	// stored handle stays unique.
	if second.User.DisplayName != "bob-smith" {
		t.Errorf("display name = %q, want derived handle bob-smith", second.User.DisplayName)
	}
}

func TestSessionExpiry(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	svc, sessions, _ := testService(t, users)

	result, err := svc.Signup(ctx, "t@example.com", "long-enough-password", "", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	// Rewrite the session with an already-expired timestamp (the store
	// enforces expiry on Get).
	sess := result.Session
	sess.ExpiresAt = time.Now().Add(-time.Second)
	if err := sessions.Create(ctx, sess, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Authenticate(ctx, result.Session.Token); !errors.Is(err, authn.ErrSessionNotFound) {
		t.Errorf("expired session = %v, want authn.ErrSessionNotFound", err)
	}
}

func TestNewTokenShape(t *testing.T) {
	a, err := authn.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, err := authn.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if a == b {
		t.Error("two tokens must differ")
	}
	if len(a) != 43 { // 32 bytes base64url without padding
		t.Errorf("token length = %d, want 43", len(a))
	}
	if strings.ContainsAny(a, "+/=") {
		t.Errorf("token %q contains non-url-safe characters", a)
	}
}

func TestOIDCLoginLinksOnVerifiedEmailOnly(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	sessions := memstore.NewSessions()
	cfg := authn.Config{
		SessionTTL: time.Hour,
		OIDC:       authn.OIDCConfig{Enabled: true, Issuer: "https://fake", ClientID: "c", RedirectPath: "/cb"},
	}
	provider := &stubOIDC{claims: authn.OIDCClaims{
		Subject: "sub-1", Email: "Oidc@Example.com", EmailVerified: true,
		PreferredUsername: "oidc-alice", Name: "Alice O",
	}}
	svc := authn.NewService(users, sessions, memstore.AllowAll(), provider, nil, cfg)

	result, err := svc.OIDCLogin(ctx, "code", "https://api/cb")
	if err != nil {
		t.Fatalf("OIDCLogin: %v", err)
	}
	if result.User.Email != "oidc@example.com" {
		t.Errorf("email = %q, want normalized oidc@example.com", result.User.Email)
	}
	if result.User.Handle != "oidc-alice" {
		t.Errorf("handle = %q, want oidc-alice", result.User.Handle)
	}

	// Second login with the same email reuses the account (no duplicate).
	again, err := svc.OIDCLogin(ctx, "code2", "https://api/cb")
	if err != nil {
		t.Fatalf("OIDCLogin again: %v", err)
	}
	if again.User.ID != result.User.ID {
		t.Error("second OIDC login must reuse the account with the same email")
	}

	// Unverified email: refused, never linked.
	provider.claims = authn.OIDCClaims{Subject: "sub-2", Email: "other@example.com", EmailVerified: false}
	_, err = svc.OIDCLogin(ctx, "code3", "https://api/cb")
	if !errors.Is(err, authn.ErrOIDCEmailNotVerified) {
		t.Errorf("unverified email = %v, want authn.ErrOIDCEmailNotVerified", err)
	}
	if _, err := users.FindByEmail(ctx, "other@example.com"); !errors.Is(err, authn.ErrUserNotFound) {
		t.Error("unverified OIDC login must not create an account")
	}

	// Disabled OIDC config: not configured error.
	svcDisabled := authn.NewService(users, sessions, memstore.AllowAll(), provider, nil, authn.Config{})
	if _, err := svcDisabled.OIDCLogin(ctx, "code", "https://api/cb"); !errors.Is(err, authn.ErrOIDCNotConfigured) {
		t.Errorf("disabled OIDC = %v, want authn.ErrOIDCNotConfigured", err)
	}
}

// stubOIDC is a programmable OIDCProvider for service tests.
type stubOIDC struct {
	claims authn.OIDCClaims
}

func (s *stubOIDC) AuthorizeURL(_ context.Context, _, state string) (string, error) {
	return "https://fake/authorize?state=" + state, nil
}

func (s *stubOIDC) ExchangeCode(context.Context, string, string) (authn.OIDCClaims, error) {
	return s.claims, nil
}

// TestOIDCAuthorizeURLDisabled answers authn.ErrOIDCNotConfigured when no
// provider is wired.
func TestOIDCAuthorizeURLDisabled(t *testing.T) {
	svc, _, _ := testService(t, memstore.NewUsers())
	_, _, err := svc.OIDCAuthorizeURL(context.Background(), "https://api/cb")
	if !errors.Is(err, authn.ErrOIDCNotConfigured) {
		t.Errorf("authorize-url without provider = %v, want authn.ErrOIDCNotConfigured", err)
	}
}

// TestSignupRateLimitPerIP: signup burns a full argon2id KDF plus an
// INSERT per call, so it gets its own per-IP budget (M2 — before the fix
// signup was unlimited). The limit is on attempts, and another IP is
// unaffected.
func TestSignupRateLimitPerIP(t *testing.T) {
	ctx := context.Background()
	cfg := authn.Config{
		SessionTTL: time.Hour, LoginLimitPerEmail: 5, LoginLimitPerIP: 100,
		LoginWindow: time.Minute, SignupLimitPerIP: 3,
	}
	svc := authn.NewService(memstore.NewUsers(), memstore.NewSessions(), memstore.NewLimiter(), nil, nil, cfg)

	for i := 0; i < 3; i++ {
		email := fmt.Sprintf("s%d@example.com", i)
		if _, err := svc.Signup(ctx, email, "long-enough-password", "", "", "10.0.0.1"); err != nil {
			t.Fatalf("signup %d: %v", i+1, err)
		}
	}
	_, err := svc.Signup(ctx, "s3@example.com", "long-enough-password", "", "", "10.0.0.1")
	var rateErr *authn.RateLimitError
	if !errors.As(err, &rateErr) {
		t.Fatalf("4th signup = %v, want authn.RateLimitError", err)
	}
	if rateErr.RetryAfter <= 0 || rateErr.RetryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want (0, 1m]", rateErr.RetryAfter)
	}
	// The bucket is per IP: another IP keeps its own budget.
	if _, err := svc.Signup(ctx, "s4@example.com", "long-enough-password", "", "", "10.9.9.9"); err != nil {
		t.Errorf("signup from another IP = %v, want nil (per-IP bucket)", err)
	}
}

// TestSignupFailsClosedWhenLimiterErrors: like login, a broken limiter
// refuses signup (the policy state is unknowable — fail closed).
func TestSignupFailsClosedWhenLimiterErrors(t *testing.T) {
	ctx := context.Background()
	cfg := authn.Config{
		SessionTTL: time.Hour, LoginLimitPerEmail: 5, LoginLimitPerIP: 100,
		LoginWindow: time.Minute, SignupLimitPerIP: 3,
	}
	svc := authn.NewService(memstore.NewUsers(), memstore.NewSessions(), memstore.Broken(), nil, nil, cfg)

	_, err := svc.Signup(ctx, "x@example.com", "long-enough-password", "", "", "10.0.0.1")
	if !errors.Is(err, authn.ErrUnavailable) {
		t.Errorf("signup with broken limiter = %v, want authn.ErrUnavailable (fail closed)", err)
	}
}

// TestUserSuppliedHandleValidation: a user-supplied handle is normalized
// (trim + lowercase) and validated against the identity charset [a-z0-9-_]
// — the same shape derived handles produce. Anything else is a validation
// error, not a silent rewrite; existing stored handles keep working
// (reads never re-validate).
func TestUserSuppliedHandleValidation(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	svc, _, _ := testService(t, users)

	// Uppercase is normalized down.
	result, err := svc.Signup(ctx, "carol@example.com", "long-enough-password", "Carol-1", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if result.User.Handle != "carol-1" {
		t.Errorf("normalized handle = %q, want carol-1", result.User.Handle)
	}
	// The default display name is the normalized handle.
	if result.User.DisplayName != "carol-1" {
		t.Errorf("default display name = %q, want carol-1", result.User.DisplayName)
	}
	// A valid charset handle passes verbatim.
	result, err = svc.Signup(ctx, "dave@example.com", "long-enough-password", "dave_2", "", "10.0.0.1")
	if err != nil || result.User.Handle != "dave_2" {
		t.Errorf("valid handle = %+v (%v)", result.User, err)
	}
	// Invalid charset is rejected, never silently rewritten.
	for _, bad := range []string{"Bad Handle!", "дave", "dave@x", "dave/x", "-dave", "dave-"} {
		if _, err := svc.Signup(ctx, "eve@example.com", "long-enough-password", bad, "", "10.0.0.1"); !errors.Is(err, authn.ErrValidation) {
			t.Errorf("handle %q = %v, want authn.ErrValidation", bad, err)
		}
	}
	// Too long is rejected.
	if _, err := svc.Signup(ctx, "frank@example.com", "long-enough-password", strings.Repeat("a", 65), "", "10.0.0.1"); !errors.Is(err, authn.ErrValidation) {
		t.Errorf("65-char handle = %v, want authn.ErrValidation", err)
	}
}

// TestDomainEmailValidation pins the identity-shape rules.
func TestDomainEmailValidation(t *testing.T) {
	valid := []string{"a@b.co", "Alice@Example.com", "a.b+c@sub.example.org"}
	invalid := []string{"", "plain", "a@", "@b.co", "a b@c.co", "a@b@c", strings.Repeat("a", 300) + "@b.co"}
	for _, email := range valid {
		if !domain.ValidEmail(email) {
			t.Errorf("ValidEmail(%q) = false, want true", email)
		}
	}
	for _, email := range invalid {
		if domain.ValidEmail(email) {
			t.Errorf("ValidEmail(%q) = true, want false", email)
		}
	}
	if domain.NormalizeEmail("  ALICE@Example.COM ") != "alice@example.com" {
		t.Error("NormalizeEmail must lowercase and trim")
	}
}

// fakeAuditRecorder captures the auth audit entries in memory (unit fake
// for the AuditRecorder port; the pgx adapter is exercised in the
// integration suite).
type fakeAuditRecorder struct {
	entries []domain.AuditEntry
	err     error
}

func (f *fakeAuditRecorder) Record(_ context.Context, e domain.AuditEntry) error {
	f.entries = append(f.entries, e)
	return f.err
}

// TestAuthEventsAreRecorded pins the audit contract of the auth service
// (T0110): signup, login success, login failure (known actor for a known
// account, no actor for an unknown one) and logout each append one entry
// with actor/via/target, and the correlation id flows from the request
// context.
func TestAuthEventsAreRecorded(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	knownID := seedPasswordUser(t, users, "known@example.com", "right-password-123", "known")
	rec := &fakeAuditRecorder{}
	sessions := memstore.NewSessions()
	cfg := authn.Config{
		WebOrigin:          "http://127.0.0.1:3000",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 5,
		LoginLimitPerIP:    100,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   100,
	}
	svc := authn.NewService(users, sessions, memstore.NewLimiter(), nil, rec, cfg)

	reqCtx := domain.WithRequestInfo(ctx, domain.RequestInfo{
		CorrelationID: "corr-123",
	})

	// Signup records auth.account.signup with the new user as actor.
	signup, err := svc.Signup(reqCtx, "fresh@example.com", "long-enough-password", "fresh", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	// Login success records auth.login.success.
	login, err := svc.Login(reqCtx, "known@example.com", "right-password-123", "10.0.0.1")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	// Wrong password for a known account records a failure naming the account.
	if _, err := svc.Login(reqCtx, "known@example.com", "wrong-password-456", "10.0.0.1"); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("wrong-password login = %v, want ErrInvalidCredentials", err)
	}
	// Unknown email records a failure with no actor.
	if _, err := svc.Login(reqCtx, "nobody@example.com", "whatever-password", "10.0.0.1"); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("unknown-email login = %v, want ErrInvalidCredentials", err)
	}
	// Logout records auth.logout with the actor from the request context.
	logoutCtx := domain.WithRequestInfo(ctx, domain.RequestInfo{
		ActorID:       knownID,
		Via:           domain.ViaSession,
		CorrelationID: "corr-logout",
	})
	if err := svc.Logout(logoutCtx, login.Session.Token); err != nil {
		t.Fatalf("Logout: %v", err)
	}

	if len(rec.entries) != 5 {
		t.Fatalf("recorded %d entries, want 5: %+v", len(rec.entries), rec.entries)
	}
	signupEntry := rec.entries[0]
	if signupEntry.Action != domain.ActionAuthSignup || signupEntry.ActorID != signup.User.ID ||
		signupEntry.Via != domain.ViaPassword || signupEntry.TargetRef != "user:"+signup.User.ID {
		t.Errorf("signup entry = %+v", signupEntry)
	}
	if signupEntry.CorrelationID != "corr-123" {
		t.Errorf("signup correlation id = %q, want corr-123", signupEntry.CorrelationID)
	}
	if signupEntry.AfterSummary.(map[string]any)["handle"] != "fresh" {
		t.Errorf("signup after_summary = %+v", signupEntry.AfterSummary)
	}

	loginEntry := rec.entries[1]
	if loginEntry.Action != domain.ActionAuthLoginSuccess || loginEntry.ActorID != knownID ||
		loginEntry.Via != domain.ViaPassword {
		t.Errorf("login success entry = %+v", loginEntry)
	}

	failKnown := rec.entries[2]
	if failKnown.Action != domain.ActionAuthLoginFailed || failKnown.ActorID != knownID {
		t.Errorf("known-account failure entry = %+v", failKnown)
	}
	if failKnown.Metadata.(map[string]any)["reason"] != "invalid_credentials" {
		t.Errorf("failure metadata = %+v", failKnown.Metadata)
	}

	failUnknown := rec.entries[3]
	if failUnknown.Action != domain.ActionAuthLoginFailed || failUnknown.ActorID != "" {
		t.Errorf("unknown-account failure entry = %+v", failUnknown)
	}

	logoutEntry := rec.entries[4]
	if logoutEntry.Action != domain.ActionAuthLogout || logoutEntry.ActorID != knownID ||
		logoutEntry.Via != domain.ViaSession || logoutEntry.CorrelationID != "corr-logout" {
		t.Errorf("logout entry = %+v", logoutEntry)
	}
}

// TestDisabledAccountLoginRecordsAccountDisabledReason pins the single
// failure-reason vocabulary (review M3): a disabled account's password
// login records metadata reason "account_disabled" — the same word the
// disabled-account OIDC branch records — so downstream analysis of
// disabled-account attempts sees one vocabulary across channels.
func TestDisabledAccountLoginRecordsAccountDisabledReason(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	id := seedPasswordUser(t, users, "disabled@example.com", "right-password-123", "disabled")
	rec, _ := users.GetByID(ctx, id)
	disabledAt := time.Now().Add(-time.Hour)
	rec.User.DisabledAt = &disabledAt
	users.Seed(rec)
	auditRec := &fakeAuditRecorder{}
	cfg := authn.Config{
		WebOrigin:          "http://127.0.0.1:3000",
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 5,
		LoginLimitPerIP:    100,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   100,
	}
	svc := authn.NewService(users, memstore.NewSessions(), memstore.NewLimiter(), nil, auditRec, cfg)

	if _, err := svc.Login(ctx, "disabled@example.com", "right-password-123", "10.0.0.1"); !errors.Is(err, authn.ErrInvalidCredentials) {
		t.Fatalf("disabled login = %v, want ErrInvalidCredentials (the wire answer is unchanged)", err)
	}
	if len(auditRec.entries) != 1 {
		t.Fatalf("recorded %d entries, want 1", len(auditRec.entries))
	}
	e := auditRec.entries[0]
	if e.Action != domain.ActionAuthLoginFailed || e.ActorID != id || e.Via != domain.ViaPassword {
		t.Errorf("entry = %+v, want auth.login.failed naming the disabled account", e)
	}
	if e.Metadata.(map[string]any)["reason"] != "account_disabled" {
		t.Errorf("reason = %v, want account_disabled", e.Metadata)
	}
}

// TestAuthAuditRecordingIsBestEffort: an audit store failure must never
// change the auth outcome (the log write is not the action) — it is
// logged, not returned.
func TestAuthAuditRecordingIsBestEffort(t *testing.T) {
	ctx := context.Background()
	users := memstore.NewUsers()
	rec := &fakeAuditRecorder{err: errors.New("audit store down")}
	cfg := authn.Config{
		SessionTTL:         time.Hour,
		LoginLimitPerEmail: 5,
		LoginLimitPerIP:    100,
		LoginWindow:        time.Minute,
		SignupLimitPerIP:   100,
	}
	svc := authn.NewService(users, memstore.NewSessions(), memstore.NewLimiter(), nil, rec, cfg)

	_, err := svc.Signup(ctx, "alice@example.com", "long-enough-password", "alice", "", "10.0.0.1")
	if err != nil {
		t.Fatalf("Signup with failing audit recorder = %v, want success", err)
	}
	if len(rec.entries) != 1 {
		t.Errorf("recorded %d entries, want 1 (the failed one)", len(rec.entries))
	}
}
