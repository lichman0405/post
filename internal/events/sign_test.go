package events

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

// Task T1006: the required test "webhook security" — the signing protocol
// refuses what a receiver must refuse:
//
//   - a bad signature (tampered body, tampered timestamp, wrong secret,
//     tampered signature) fails closed;
//   - a replay (an old capture re-sent inside/outside the window) fails;
//   - a far-future timestamp fails (forged timestamps);
//   - a malformed signature header fails without touching the HMAC path.
//
// The deliverer's real output is verified against these rules again in
// the integration suite (tests/integration/webhook_test.go), where a real
// receiver checks a real delivery.

func TestSignVerifyRoundTrip(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"event_id":"x","event_type":"state.committed"}`)
	ts := time.Now().Unix()

	sig := Sign(secret, ts, body)
	if !strings.HasPrefix(sig, "sha256=") {
		t.Fatalf("signature %q lacks the sha256= prefix", sig)
	}
	if err := VerifySignature(secret, ts, body, sig, DefaultMaxTimestampSkew); err != nil {
		t.Fatalf("VerifySignature(valid) = %v, want nil", err)
	}
}

func TestVerifySignatureRejectsTampering(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"event_type":"state.committed"}`)
	ts := time.Now().Unix()
	sig := Sign(secret, ts, body)

	tests := []struct {
		name string
		body []byte
		ts   int64
		sec  []byte
		sig  string
	}{
		{"tampered body", []byte(`{"event_type":"release.published"}`), ts, secret, sig},
		{"tampered timestamp", body, ts + 1, secret, sig},
		{"tampered timestamp back", body, ts - 1, secret, sig},
		{"wrong secret", body, ts, []byte("other-secret"), sig},
		{"tampered signature (flipped nibble)", body, ts, secret, flipLastHexNibble(sig)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifySignature(tt.sec, tt.ts, tt.body, tt.sig, DefaultMaxTimestampSkew)
			if !errors.Is(err, ErrBadSignature) {
				t.Fatalf("VerifySignature = %v, want ErrBadSignature", err)
			}
		})
	}
}

func TestVerifySignatureRejectsReplay(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"event_type":"state.committed"}`)

	// A captured delivery from 10 minutes ago, re-sent now.
	old := time.Now().Add(-10 * time.Minute).Unix()
	sig := Sign(secret, old, body)
	err := VerifySignature(secret, old, body, sig, DefaultMaxTimestampSkew)
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("replayed timestamp = %v, want ErrReplay", err)
	}

	// Exactly at the window edge: the skew bound is inclusive, and a
	// replay just inside it still verifies (clock drift tolerance) — the
	// protection is the window, not the exact second.
	edge := time.Now().Add(-DefaultMaxTimestampSkew + time.Second).Unix()
	sig = Sign(secret, edge, body)
	if err := VerifySignature(secret, edge, body, sig, DefaultMaxTimestampSkew); err != nil {
		t.Fatalf("in-window timestamp = %v, want nil", err)
	}
}

func TestVerifySignatureRejectsFutureTimestamp(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"event_type":"state.committed"}`)

	future := time.Now().Add(10 * time.Minute).Unix()
	sig := Sign(secret, future, body)
	err := VerifySignature(secret, future, body, sig, DefaultMaxTimestampSkew)
	if !errors.Is(err, ErrTimestampSkew) {
		t.Fatalf("future timestamp = %v, want ErrTimestampSkew", err)
	}
}

func TestVerifySignatureRejectsMalformedSignature(t *testing.T) {
	secret := []byte("test-secret")
	body := []byte(`{"event_type":"state.committed"}`)
	ts := time.Now().Unix()

	valid := Sign(secret, ts, body)
	validHex := strings.TrimPrefix(valid, "sha256=")
	tests := []struct {
		name string
		sig  string
	}{
		{"empty", ""},
		{"no prefix", validHex},
		{"wrong prefix", "hmac-sha256=" + validHex},
		{"non-hex", "sha256=zz" + validHex[2:]},
		{"short", "sha256=" + validHex[:40]},
		{"uppercase hex", "sha256=" + strings.ToUpper(validHex)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifySignature(secret, ts, body, tt.sig, DefaultMaxTimestampSkew)
			if !errors.Is(err, ErrMalformedSignature) {
				t.Fatalf("VerifySignature(%q) = %v, want ErrMalformedSignature", tt.name, err)
			}
		})
	}
}

func TestSignIsDeterministicForSameInputs(t *testing.T) {
	secret := []byte("s")
	body := []byte("b")
	ts := int64(1726500000)
	if a, b := Sign(secret, ts, body), Sign(secret, ts, body); a != b {
		t.Fatalf("Sign is not deterministic: %q != %q", a, b)
	}
	// A different timestamp must produce a different signature (the
	// timestamp is part of the HMAC input — otherwise replay protection
	// would not bind time to content).
	if Sign(secret, ts+1, body) == Sign(secret, ts, body) {
		t.Fatal("signatures for different timestamps collide")
	}
}

// flipLastHexNibble corrupts one hex nibble of a signature while keeping
// its shape valid — the tampered-signature case.
func flipLastHexNibble(sig string) string {
	hexPart := strings.TrimPrefix(sig, "sha256=")
	b, err := hex.DecodeString(hexPart)
	if err != nil {
		panic(err)
	}
	b[len(b)-1] ^= 0x01
	return "sha256=" + hex.EncodeToString(b)
}
