package assets

import "strings"

// DependencyPin names one exact asset version another version depends on:
// the pinned asset's pid and its version label, in the canonical text form
// pid@version ("01j9z6k3m4n5p6q7r8s9t0v1w2@1.0").
//
// The pin is an exact version or it is not a pin: docs/11 §5 separates
// Reference/Use — 引用固定 Asset Version — from a floating reference, and
// docs/11 §3 keeps "dependency pin" on the publish checklist for exactly
// that reason. There is no range, no "latest", no alias: a pid names one
// asset and a label names one immutable version of it (CLAUDE.md
// invariant 5), so the pair identifies published bytes forever. A
// dependency that names only an asset — meaning "some version, I did not
// record which" — is not expressible here, deliberately: it could not be
// reproduced, and impact analysis (docs/11 §5) could not tell which
// downstream projects a new version affects.
//
// The text form is used rather than a JSON object because a pid and a
// version label share no character ("@" is in neither alphabet —
// pidAlphabet drops it, versionLabelCharset drops it), so the split is
// unambiguous and the pin stays one comparable, sortable string, the way
// an origin ref does.
type DependencyPin string

// NewDependencyPin builds the canonical pin of one asset version. It
// reports false when the pid or the label is outside its shape — a pin
// that names something that cannot exist is refused at construction
// rather than discovered later.
func NewDependencyPin(pid PID, version string) (DependencyPin, bool) {
	if !ValidPID(string(pid)) || !ValidVersionLabel(version) {
		return "", false
	}
	return DependencyPin(string(pid) + "@" + strings.TrimSpace(version)), true
}

// ParseDependencyPin splits a canonical pin into the pinned asset's pid
// and version label. It is the inverse of NewDependencyPin: ok is true
// exactly when the string has the canonical shape.
func ParseDependencyPin(s string) (pid PID, version string, ok bool) {
	left, right, found := strings.Cut(s, "@")
	if !found || !ValidPID(left) || !ValidVersionLabel(right) {
		return "", "", false
	}
	return PID(left), strings.TrimSpace(right), true
}

// Valid reports whether p has the canonical pin shape: a well-formed pid,
// a version label that could be stored, and no further "@" (a second "@"
// lands in the label half, which the label charset refuses).
func (p DependencyPin) Valid() bool {
	_, _, ok := ParseDependencyPin(string(p))
	return ok
}

// validateDependencyPins checks the document rules of a pin list: every
// pin canonical and no pin listed twice. The returned errors are in list
// order, so a report reads top to bottom.
//
// An EMPTY list is valid. "dependency pin" on the publish checklist
// (docs/11 §3) is about how dependencies are recorded, not about having
// them: a protocol that needs nothing from another asset has nothing to
// pin, and refusing it for that would invent a requirement the spec does
// not state. What is refused is a dependency that is not pinned — a
// malformed entry, or the same version listed twice (which one is the real
// dependency then?).
func validateDependencyPins(pins []DependencyPin) []error {
	var errs []error
	seen := make(map[DependencyPin]int, len(pins))
	for i, pin := range pins {
		if !pin.Valid() {
			errs = append(errs, &ValidationError{
				Code:  CodeInvalidDependencyPin,
				Field: dependencyPinPath(i),
				Detail: "expected the canonical pin form pid@version (pid = 26 Crockford base32 characters, " +
					"version = 1..64 of [A-Za-z0-9._-]), got " + quote(string(pin)),
			})
			continue
		}
		if first, dup := seen[pin]; dup {
			errs = append(errs, &ValidationError{
				Code:   CodeDuplicateDependencyPin,
				Field:  dependencyPinPath(i),
				Detail: "the same version is pinned again at index " + itoa(first) + ": " + quote(string(pin)),
			})
			continue
		}
		seen[pin] = i
	}
	return errs
}

// validateSelfPin checks the one pin rule that needs the version's own
// identity, which the document does not carry: no pin may name the very
// version being published. Depending on itself is a cycle of one — the
// published version could never be resolved against a version of itself
// that does not exist yet, and impact analysis would walk back into the
// version it started from (docs/11 §5).
func validateSelfPin(pins []DependencyPin, self PID, version string) []error {
	if !ValidPID(string(self)) {
		// Without a known identity there is nothing to compare against;
		// the candidate's own pid failure is reported by the gate.
		return nil
	}
	selfVersion := strings.TrimSpace(version)
	for i, pin := range pins {
		pid, pinnedVersion, ok := ParseDependencyPin(string(pin))
		if ok && pid == self && pinnedVersion == selfVersion {
			return []error{&ValidationError{
				Code:   CodeSelfDependencyPin,
				Field:  dependencyPinPath(i),
				Detail: "the version pins itself (" + quote(string(pin)) + "); a dependency is another published version",
			}}
		}
	}
	return nil
}

// dependencyPinPath renders the JSON path of one pin, so an error names
// the element a caller must fix.
func dependencyPinPath(i int) string { return "dependency_pins[" + itoa(i) + "]" }
