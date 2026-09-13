package audit

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

// Cursor is the keyset position of an Activity page: the (occurred_at, id)
// of the last row served. The log is append-only, so this pair is stable —
// rows never move, pages never drift (an offset-based page would shift
// while new events land).
type Cursor struct {
	OccurredAt time.Time
	ID         string
}

// EncodeCursor renders the cursor opaquely (base64url of "RFC3339Nano|id")
// so clients treat it as a token. Cursors come from the server only;
// clients never construct them.
func EncodeCursor(c Cursor) string {
	raw := c.OccurredAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses and validates a cursor token: exactly one separator,
// an RFC3339Nano timestamp and a canonical uuid id. Anything malformed —
// bad base64, wrong shape, empty parts, unparseable time, non-uuid id —
// answers ErrValidation, never a store round-trip. The uuid strictness is
// a hard requirement, not cosmetics: the persistence layer's cursor-id
// parse must never see a value it could panic on (review B1 — a forged
// cursor=ts|zzz once panicked the handler).
func DecodeCursor(s string) (Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: invalid cursor", ErrValidation)
	}
	// Exactly one separator: Cut splits on the FIRST "|", so a second one
	// (an id smuggling the separator, like "a|b") is malformed, not an id.
	ts, id, ok := strings.Cut(string(b), "|")
	if !ok || ts == "" || id == "" || strings.Contains(id, "|") {
		return Cursor{}, fmt.Errorf("%w: invalid cursor", ErrValidation)
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return Cursor{}, fmt.Errorf("%w: invalid cursor", ErrValidation)
	}
	if !canonicalUUID(id) {
		return Cursor{}, fmt.Errorf("%w: invalid cursor", ErrValidation)
	}
	return Cursor{OccurredAt: t, ID: id}, nil
}

// canonicalUUID reports whether s is the canonical uuid text form
// (8-4-4-4-12 lowercase hex — what EncodeCursor emits and the database
// returns). Cursors are server-issued tokens, so anything not exactly this
// shape is malformed; the strictness is also what guarantees the
// persistence layer only ever parses valid cursor ids.
func canonicalUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		switch i {
		case 8, 13, 18, 23:
			if s[i] != '-' {
				return false
			}
		default:
			c := s[i]
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}
