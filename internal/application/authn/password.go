package authn

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Password hash parameters (OWASP-recommended argon2id for 2026): 64 MiB
// memory, 3 iterations, 2 lanes. Parameters are embedded in the encoded
// hash so they can be raised later without breaking old hashes — Verify
// always uses the parameters stored in the hash itself.
const (
	argonMemory      = 64 * 1024 // KiB
	argonIterations  = 3
	argonParallelism = 2
	argonSaltLen     = 16
	argonKeyLen      = 32
	// Reject hostile stored values that would make verification run
	// unbounded memory/time (the encoding is attacker-controllable input
	// once the database is, so fail closed on absurd parameters).
	argonMaxMemory = 1024 * 1024 // KiB
)

// HashPassword returns the argon2id PHC-style encoding of password:
//
//	$argon2id$v=19$m=65536,t=3,p=2$<salt-b64>$<hash-b64>
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("authn: password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword reports whether password matches the encoded hash.
// It rejects malformed encodings (error) and wrong passwords (false),
// never panics on hostile stored values.
func VerifyPassword(encoded, password string) (bool, error) {
	params, salt, key, err := decodeHash(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, params.iterations, params.memory, params.parallelism, uint32(len(key)))
	return subtle.ConstantTimeCompare(got, key) == 1, nil
}

type hashParams struct {
	iterations  uint32
	memory      uint32 // KiB
	parallelism uint8
}

func decodeHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// ["", "argon2id", "v=19", "m=...,t=...,p=...", salt, key]
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return hashParams{}, nil, nil, errors.New("authn: unrecognized password hash encoding")
	}
	if parts[2] != fmt.Sprintf("v=%d", argon2.Version) {
		return hashParams{}, nil, nil, errors.New("authn: unsupported argon2 version")
	}
	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.iterations, &p.parallelism); err != nil {
		return hashParams{}, nil, nil, fmt.Errorf("authn: bad hash parameters: %w", err)
	}
	if p.memory < 8 || p.iterations < 1 || p.parallelism < 1 || p.memory > argonMaxMemory {
		return hashParams{}, nil, nil, errors.New("authn: hash parameters out of safe range")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return hashParams{}, nil, nil, errors.New("authn: bad hash salt")
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return hashParams{}, nil, nil, errors.New("authn: bad hash key")
	}
	return p, salt, key, nil
}

// dummyHash is a precomputed argon2id hash of a fixed random string with
// the current production parameters. Login verifies the submitted password
// against it when the account does not exist, so "unknown email" and
// "wrong password" perform identical work (enumeration resistance via
// timing).
var dummyHash = func() string {
	h, err := HashPassword("post-dummy-password-for-timing-2026")
	if err != nil {
		panic(err) // never: HashPassword fails only on broken rand source
	}
	return h
}()
