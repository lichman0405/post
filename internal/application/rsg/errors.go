package rsg

import (
	"errors"
	"fmt"

	"github.com/lichman0405/post/internal/rsg/semantics"
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

// LiteratureEvidenceUnitUnnamedError reports a literature assertion that
// names no evidence unit at all: evidence_type = 'literature' with an empty
// reasoning note. docs/10 §6 forbids exactly that shape ("a DOI may not
// support a claim directly"), and docs/19 §4 writes the same rule from the
// reference side ("Evidence Assertion 指向具体 location/excerpt/figure/table/
// dataset/method").
//
// # Why this is a refusal when the semantic check only warns
//
// internal/rsg/semantics.CheckEvidenceAssertion WARNS about this shape, and
// keeps warning: whether a note names a SUFFICIENT unit is a scientific call
// the check must not make for the author (evidence_assertion.go:26-28,
// docs/10 §4: V1 不自动赋数值权重; CLAUDE.md §9.12). That split is untouched —
// this type is the CALLER's policy, applied by the write path, over the one
// predicate the check does own: "is the place empty".
//
// The two questions are not the same question, and the difference is the
// whole rule. A blank note names no unit under any reading, so no scientific
// judgement is needed to refuse it: there is nothing there to be judged. A
// non-empty note is accepted whatever it says — a weak location that a DOI
// would not stand behind is the AUTHOR's and the REVIEWER's call, never this
// command's (the assertion is stored with its relation and its review_state,
// never folded into a verdict).
type LiteratureEvidenceUnitUnnamedError struct {
	// Hint is the semantic check's own advisory — the one this refusal
	// promotes. Carrying it (rather than restating the guidance) is what
	// keeps the advisory from being dropped on the way out: the refusal
	// message IS the hint message, so the author still reads what to write.
	Hint semantics.Hint
}

// Error implements error. The message is the promoted advisory, prefixed the
// way the other refusal on this surface is (GateBlockedError): the caller is
// told the write was refused and what the next step is (docs/45), and nothing
// about the store, the versions or the project leaks with it.
func (e *LiteratureEvidenceUnitUnnamedError) Error() string {
	return "the write was refused: " + e.Hint.Message
}

// Code is the stable wire code of this outcome (docs/45). It IS the semantic
// check's own code for the condition — one token for one condition, whether a
// caller meets it as the advisory or, on this write path, as the refusal.
// Inventing a second spelling for the same fact is how two vocabularies come
// to disagree about it, so this constant is read, never re-written.
func (e *LiteratureEvidenceUnitUnnamedError) Code() string {
	return semantics.HintLiteratureEvidenceUnitUnnamed
}

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
