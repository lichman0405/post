package events

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The webhook signing protocol (T1006, docs/22 §9): every delivery carries
// an HMAC-SHA256 signature over "timestamp.body" plus a Unix-seconds
// timestamp. The timestamp makes replayed deliveries fail closed: a
// consumer that tolerates only a bounded clock skew rejects both a
// captured delivery replayed later (too old) and a signature minted for a
// time far ahead of the receiver's clock (too new). The signature binds
// the timestamp to the body, so neither may be changed independently.
//
// Headers (stable, documented names):
//
//	X-POST-Signature: sha256=<64 lowercase hex chars>
//	X-POST-Timestamp: <unix seconds>
//	X-POST-Event:     <event type>
//	X-POST-Event-Id:  <research event id, stable across retries>
//	X-POST-Delivery:  <delivery id, stable across attempts>
//
// Event-Id and Delivery-Id are the consumer's dedupe keys: at-least-once
// delivery (docs/18) means a consumer may see one event more than once,
// and a delivery id is re-sent verbatim on retries so an idempotent
// consumer can recognize the repeat.

// SignatureHeader / TimestampHeader / EventHeader / EventIDHeader /
// DeliveryIDHeader are the delivery request headers, named here so the
// deliverer and every consumer share one vocabulary.
const (
	SignatureHeader  = "X-POST-Signature"
	TimestampHeader  = "X-POST-Timestamp"
	EventHeader      = "X-POST-Event"
	EventIDHeader    = "X-POST-Event-Id"
	DeliveryIDHeader = "X-POST-Delivery"
)

// signaturePrefix is the algorithm tag a signature carries. Parsing and
// rendering both funnel through one constant so a drift between them is
// impossible.
const signaturePrefix = "sha256="

// DefaultMaxTimestampSkew is the replay window both sides tolerate: a
// timestamp more than this far from the verifier's clock is refused —
// older means a replayed capture, newer means a forged future timestamp or
// badly skewed clocks (5 minutes is the GitHub-shaped default; consumers
// may widen it if their clocks are loosely synchronized).
const DefaultMaxTimestampSkew = 5 * time.Minute

// signatureString renders the exact string the HMAC covers:
// "<unix-seconds>.<raw body>". The dot separator cannot occur in the
// timestamp, so the split is unambiguous.
func signatureString(timestamp int64, body []byte) string {
	return strconv.FormatInt(timestamp, 10) + "." + string(body)
}

// Sign computes the delivery signature for a timestamp/body pair under
// secret: hex-encoded HMAC-SHA256 with the sha256= prefix, as the
// SignatureHeader carries it. The same bytes the verifier compares.
func Sign(secret []byte, timestamp int64, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signatureString(timestamp, body)))
	return signaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// BadSignature / ErrReplay / ErrTimestampSkew / ErrMalformedSignature are
// the verification failures a consumer maps to 401/403. Callers test with
// errors.Is — never by string comparison.
var (
	// ErrBadSignature: the signature does not match secret+timestamp+body
	// (wrong secret, tampered body, tampered timestamp, or tampered
	// signature).
	ErrBadSignature = fmt.Errorf("events: webhook signature mismatch")
	// ErrReplay: the timestamp is older than the allowed skew — a captured
	// delivery replayed later fails here.
	ErrReplay = fmt.Errorf("events: webhook timestamp outside allowed window (replay?)")
	// ErrTimestampSkew: the timestamp is newer than the allowed skew —
	// either forged future timestamps or the sender's clock is far ahead.
	ErrTimestampSkew = fmt.Errorf("events: webhook timestamp too far in the future")
	// ErrMalformedSignature: the signature header does not even have the
	// protocol shape (prefix/hex).
	ErrMalformedSignature = fmt.Errorf("events: malformed webhook signature")
)

// VerifySignature checks sig against secret over timestamp/body within
// maxSkew. Order of checks (fail closed, cheapest first): the timestamp
// window, the signature shape, and only then the constant-time HMAC
// comparison — a verifier must never accept a replay on a weak signature
// path, and must never leak which part failed through timing beyond the
// constant-time comparison itself.
func VerifySignature(secret []byte, timestamp int64, body []byte, sig string, maxSkew time.Duration) error {
	now := time.Now().Unix()
	age := now - timestamp
	if age > int64(maxSkew/time.Second) {
		return ErrReplay
	}
	if age < -int64(maxSkew/time.Second) {
		return ErrTimestampSkew
	}
	want, err := parseSignature(sig)
	if err != nil {
		return err
	}
	got := signBytes(secret, timestamp, body)
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return ErrBadSignature
	}
	return nil
}

// parseSignature validates the "sha256=<hex>" shape and decodes the hex.
// A decoded MAC of the wrong length is malformed too — HMAC-SHA256 is
// always 32 bytes, so a wrong-length tag can never match and failing it
// here keeps the timing-sensitive comparison for real candidates only.
func parseSignature(sig string) ([]byte, error) {
	hexPart, ok := strings.CutPrefix(sig, signaturePrefix)
	if !ok || hexPart == "" || hexPart != strings.ToLower(hexPart) {
		return nil, ErrMalformedSignature
	}
	out := make([]byte, hex.DecodedLen(len(hexPart)))
	if _, err := hex.Decode(out, []byte(hexPart)); err != nil {
		return nil, ErrMalformedSignature
	}
	if len(out) != sha256.Size {
		return nil, ErrMalformedSignature
	}
	return out, nil
}

// signBytes is Sign without the prefix rendering — the raw MAC bytes for
// the constant-time comparison.
func signBytes(secret []byte, timestamp int64, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signatureString(timestamp, body)))
	return mac.Sum(nil)
}
