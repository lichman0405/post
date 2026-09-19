package rsg

import (
	"errors"
	"fmt"
)

// Sentinel errors the RSG service produces itself (docs/45: the wire
// carries codes, never dependency detail). Outcomes owned by the
// underlying services — a missing object, a lost version compare-and-swap,
// a blocked validation gate, a branch state conflict — pass through
// unchanged (their packages define the canonical sentinels and wire
// codes), so the transport maps each outcome once, wherever it arose.
var (
	// ErrForbidden: the authorization engine refused the action (a denied
	// matrix verdict, or a conditional form this site does not resolve).
	// The caller is refused before any object/relation lookup, so the
	// refusal never discloses whether the target exists (docs/45).
	ErrForbidden = errors.New("rsg: forbidden")
	// ErrValidation: an input fails the domain shape rules (an unknown
	// object type, a payload that is not a JSON object, an expected_version
	// below 1, a missing endpoint, ...).
	ErrValidation = errors.New("rsg: validation failed")
	// ErrStore: an adapter or policy engine failed (cause kept for the log).
	ErrStore = errors.New("rsg: store failure")
)

// EvidenceRefUnavailableError reports that a version an evidence assertion
// names cannot be used at that end of the edge. It is ONE outcome for every
// reason the answer is no, because a caller must not be able to tell "you
// may not" from "it is not there" (docs/45, existence hiding):
//
//   - the target version does not exist;
//   - the target version carries no publication (docs/10 §7 is about the
//     evidence a PUBLISHED knowledge object carries);
//   - the target is published but its audience is the owning project's
//     members only, and the assertion comes from another project (发布不等于
//     公开);
//   - the cited version does not exist, or belongs to another project (an
//     assertion cites the asserting project's own evidence).
type EvidenceRefUnavailableError struct {
	// Side is "target" or "evidence".
	Side string
	// VersionID is the object version the write named at that end.
	VersionID string
}

// Error implements error. The message names the side and the version, never
// which of the reasons applied.
func (e *EvidenceRefUnavailableError) Error() string {
	return fmt.Sprintf("rsg: evidence %s version %s is not available for an assertion here (it does not exist, or it is not a version this caller may pin there)", e.Side, e.VersionID)
}

// Code is the stable wire code of this outcome (docs/45).
func (e *EvidenceRefUnavailableError) Code() string { return CodeEvidenceRefUnavailable }

// Wire codes (docs/45). The shared outcomes reuse the canonical code
// strings of the owning packages (projects, branches, states, sciobjects,
// relations, rsg/validation); the codes below are this package's own.
const (
	CodeValidation  = "VALIDATION_FAILED"
	CodeUnavailable = "SERVICE_UNAVAILABLE"
	// CodeForbidden matches the profile surface's AUTH_FORBIDDEN string
	// (docs/45): one stable code for every matrix refusal.
	CodeForbidden = "AUTH_FORBIDDEN"
	// CodeEvidenceRefUnavailable is the evidence write's version-reference
	// outcome (EvidenceRefUnavailableError). It is its own code rather than
	// the relation surface's OBJECT_VERSION_NOT_FOUND because "a version
	// exists but is not usable as THIS end of an evidence assertion" has no
	// equivalent on that surface — folding the two together would leave a
	// client unable to tell "you named the wrong version" from "there is no
	// such version", which is the distinction the code vocabulary exists
	// for (docs/45).
	CodeEvidenceRefUnavailable = "EVIDENCE_REF_UNAVAILABLE"
)
