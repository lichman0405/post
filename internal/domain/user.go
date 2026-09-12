package domain

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// User is the POST person identity (docs/04 §1): it exists across
// organizations and projects and is never deleted — history only refers to
// it (CLAUDE.md §9.8, ADR-007). DisabledAt marks a disabled account:
// authentication must reject it, history must keep it.
type User struct {
	// ID is the uuid v4 text form (matches the users.id uuid column).
	ID          string
	Handle      string
	Email       string
	DisplayName string
	CreatedAt   time.Time
	DisabledAt  *time.Time
}

// NewUserID returns a random uuid v4 in canonical text form, generated
// with crypto/rand (stdlib — the application layer must not decide the
// storage representation, and stores generate their own ids server-side;
// this exists for stores and tests that need a client-side id).
func NewUserID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("domain: user id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return formatUUID(b), nil
}

// formatUUID renders 16 bytes in the canonical 8-4-4-4-12 text form.
func formatUUID(b [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Disabled reports whether the account is currently disabled.
func (u User) Disabled() bool {
	return u.DisabledAt != nil && !u.DisabledAt.After(time.Now())
}

// NormalizeEmail canonicalizes an email address for storage and lookup:
// lowercased and trimmed. Identity decisions must always run on the
// normalized form so alice@example.com and ALICE@example.com are one
// account (T0101 L1: case-insensitive email identity).
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// ValidEmail reports whether email has the minimal shape of an address:
// non-empty after normalization, a single '@' with a non-empty local part
// and a non-empty domain, no whitespace anywhere. It is deliberately
// permissive otherwise — delivery concerns (MX records etc.) are out of
// scope for V1 — but rejects input that could never be an address.
func ValidEmail(email string) bool {
	n := NormalizeEmail(email)
	if n == "" || len(n) > 254 {
		return false
	}
	if strings.ContainsAny(n, " \t\r\n") {
		return false
	}
	at := strings.Index(n, "@")
	if at <= 0 || at == len(n)-1 || strings.Contains(n[at+1:], "@") {
		return false
	}
	if strings.HasPrefix(n, "@") || strings.Contains(n, "..") {
		return false
	}
	return true
}

// ValidPassword enforces the V1 password policy (T0101 L1): at least 10
// characters, at most 256 bytes. The upper bound exists so a hostile
// client cannot make the server run the password KDF on megabytes of input.
func ValidPassword(password string) bool {
	return len(password) >= 10 && len(password) <= 256
}

// NormalizeHandle canonicalizes a user-supplied handle: trimmed and
// lowercased — the identity charset is [a-z0-9-_], exactly what derived
// handles produce. Normalization runs on signup input only, never on
// reads: handles stored before this rule keep working verbatim.
func NormalizeHandle(handle string) string {
	return strings.ToLower(strings.TrimSpace(handle))
}

// ValidHandle reports whether handle has the identity shape after
// normalization: 1..64 characters of [a-z0-9-_], no leading/trailing '-'
// (the same restriction derived handles get). Anything else is rejected at
// signup, never silently rewritten.
func ValidHandle(handle string) bool {
	n := NormalizeHandle(handle)
	if n == "" || len(n) > 64 {
		return false
	}
	if strings.HasPrefix(n, "-") || strings.HasSuffix(n, "-") {
		return false
	}
	for _, r := range n {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}
