package authn_test

import (
	"strings"
	"testing"

	"github.com/lichman0405/post/internal/application/authn"
)

// Each HashPassword call costs ~64 MiB of argon2id work; keep the number of
// hashes here small so the unit suite stays fast.

func TestPasswordHashRoundtrip(t *testing.T) {
	hash, err := authn.HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Errorf("hash %q lacks the expected PHC parameter prefix", hash)
	}
	ok, err := authn.VerifyPassword(hash, "correct-horse-battery-staple")
	if err != nil || !ok {
		t.Fatalf("VerifyPassword(correct) = %v, %v; want true, nil", ok, err)
	}
	ok, err = authn.VerifyPassword(hash, "correct-horse-battery-staplf")
	if err != nil || ok {
		t.Errorf("VerifyPassword(one-char-off) = %v, %v; want false, nil", ok, err)
	}
}

func TestPasswordHashesAreSalted(t *testing.T) {
	a, err := authn.HashPassword("same-password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	b, err := authn.HashPassword("same-password-123")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if a == b {
		t.Error("two hashes of one password are identical — salt missing")
	}
	ok, err := authn.VerifyPassword(a, "same-password-123")
	if err != nil || !ok {
		t.Errorf("VerifyPassword(salted a) = %v, %v", ok, err)
	}
	ok, err = authn.VerifyPassword(b, "same-password-123")
	if err != nil || !ok {
		t.Errorf("VerifyPassword(salted b) = %v, %v", ok, err)
	}
}

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	// Never panic, never accept; a hostile stored value fails closed.
	malformed := []string{
		"",
		"garbage",
		"$argon2id$v=99$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=0,t=0,p=0$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=1048577,t=3,p=2$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$argon2id$v=19$m=65536,t=3,p=2$short$short",
		"$argon2id$v=19$m=65536,t=3,p=2$!!!not-base64!!!$!!!!",
		"$pbkdf2-sha256$v=1$i=600000$c2FsdHNhbHRzYWx0c2FsdA$a2V5a2V5a2V5a2V5a2V5a2V5a2V5a2V5",
		"$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy", // bcrypt
	}
	for _, encoded := range malformed {
		ok, err := authn.VerifyPassword(encoded, "irrelevant-password")
		if ok {
			t.Errorf("VerifyPassword(%q) accepted — malformed hashes must fail closed", encoded)
		}
		if err == nil {
			t.Errorf("VerifyPassword(%q) returned no error for malformed hash", encoded)
		}
	}
}
