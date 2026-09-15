package assets

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// PID is an asset's persistent identifier: 26 lowercase Crockford
// base32 characters (128 bits of randomness, no padding), URL-safe and
// unambiguous in print (the Crockford alphabet drops i, l, o, u).
//
// A pid is generated once, at asset creation, and then never changes:
// it is not derived from the slug, the owning organization, the title,
// or any other revisable attribute, so renaming the asset or
// transferring it between organizations leaves its identity — and the
// persistent URLs built from it — untouched (acceptance: asset ID 不随
// slug/org 变化). The database carries the same invariant:
// research_assets.pid is NOT NULL, UNIQUE, and CHECK-constrained to
// this exact shape (migration 00064), so no writer can smuggle a
// mutable-derived identifier past the application.
type PID string

// pidAlphabet is the Crockford base32 set, lowercase: 0-9 and a-z
// without i, l, o, u.
const pidAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// pidLen is the fixed pid length: 16 random bytes = 128 bits, encoded
// in base32 = ceil(128/5) = 26 characters, no padding.
const pidLen = 26

// NewPID returns a fresh random persistent identifier, generated with
// crypto/rand (stdlib — identity generation must not depend on any
// external service, and the application layer never invents its own
// encoding). The only error is an exhausted entropy source.
//
// Encoding: the 128 bits are read MSB-first as 5-bit groups, exactly
// like RFC 4648 base32 without padding — the final group holds the last
// 3 bits zero-extended. Each group i spans the bit positions 5i..5i+4;
// a 16-bit window starting at the group's byte keeps the extraction
// branch-free (the window's low byte is zero past the buffer, which is
// the padding rule itself).
func NewPID() (PID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("assets: generate pid: %w", err)
	}
	var sb strings.Builder
	sb.Grow(pidLen)
	for i := 0; i < pidLen; i++ {
		bit := i * 5
		byteIdx := bit / 8
		shift := uint(bit % 8) // the group's start inside its byte
		var win uint16 = uint16(b[byteIdx]) << 8
		if byteIdx+1 < len(b) {
			win |= uint16(b[byteIdx+1])
		}
		// The group's highest bit sits at window position 15-shift, so
		// the group is the 5 bits starting there: window >> (11-shift).
		sb.WriteByte(pidAlphabet[(win>>(11-shift))&0x1f])
	}
	return PID(sb.String()), nil
}

// Valid reports whether p has the exact pid shape: 26 lowercase
// Crockford base32 characters. It is the Go mirror of the database's
// research_assets_pid_format CHECK.
func (p PID) Valid() bool {
	return ValidPID(string(p))
}

// ValidPID reports whether s has the exact pid shape. Uppercase input
// is rejected: pids are emitted lowercase, and accepting case variants
// of one identity creates lookalike ambiguity — exactly what the
// Crockford alphabet exists to avoid.
func ValidPID(s string) bool {
	if len(s) != pidLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'h', r >= 'j' && r <= 'k', r >= 'm' && r <= 'n', r >= 'p' && r <= 't', r >= 'v' && r <= 'z':
		default:
			return false
		}
	}
	return true
}
