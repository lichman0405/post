package assets

import (
	"strings"
	"testing"
)

// Task T0702 required test "asset validators": the dependency pin — the
// form a version records what it was built against (docs/11 §5
// Reference/Use: a pin names an exact Asset Version).

// anotherPID is a second well-formed pid, distinct from samplePID, for
// cross-asset pin cases.
const anotherPID PID = "01j9z6k3m4n5p6q7r8s9t0v1w3"

func TestDependencyPinRoundTrip(t *testing.T) {
	pin, ok := NewDependencyPin(samplePID, "1.0")
	if !ok {
		t.Fatalf("NewDependencyPin(%q, %q) refused a valid pair", samplePID, "1.0")
	}
	if want := DependencyPin(string(samplePID) + "@1.0"); pin != want {
		t.Fatalf("NewDependencyPin = %q, want %q", pin, want)
	}
	if !pin.Valid() {
		t.Errorf("a pin built by NewDependencyPin must satisfy Valid: %q", pin)
	}
	pid, version, ok := ParseDependencyPin(string(pin))
	if !ok || pid != samplePID || version != "1.0" {
		t.Errorf("ParseDependencyPin(%q) = (%q, %q, %v), want (%q, %q, true)", pin, pid, version, ok, samplePID, "1.0")
	}

	// The label is trimmed at construction and parse, the way
	// ValidVersionLabel trims: a label with surrounding whitespace names
	// the version it names.
	trimmed, ok := NewDependencyPin(samplePID, " 2.0 ")
	if !ok || trimmed != DependencyPin(string(samplePID)+"@2.0") {
		t.Errorf("NewDependencyPin with a padded label = (%q, %v), want the trimmed canonical pin", trimmed, ok)
	}
}

// TestDependencyPinIsAnExactVersionOnly is the rule docs/11 §5 turns on:
// a pin names one immutable version, so every floating form is refused —
// not by a policy the caller is trusted to apply, but because the string
// cannot be built and does not parse.
func TestDependencyPinIsAnExactVersionOnly(t *testing.T) {
	refused := []string{
		"",                                          // nothing
		string(samplePID),                           // an asset, no version
		"@" + "1.0",                                 // a version, no asset
		string(samplePID) + "@",                     // an empty label
		string(samplePID) + "@@1.0",                 // two separators, empty label half
		string(samplePID) + "@1.0@2.0",              // a second separator inside the label
		string(samplePID) + "@^1.0",                 // a caret range
		string(samplePID) + "@>=1.0",                // a comparison range
		string(samplePID) + "@~1.0",                 // a tilde range
		string(samplePID) + "@1.0 || 2.0",           // a union
		string(samplePID) + "@*",                    // a wildcard
		string(samplePID) + "@1 .0",                 // a space inside the label
		string(samplePID) + "@../1.0",               // path traversal games in the label
		strings.ToUpper(string(samplePID)) + "@1.0", // an uppercase pid is not a pid
		string(samplePID) + "@" + strings.Repeat("9", MaxVersionLen+1), // label over the bound
		"release:1.0", // the misreading of a pin as an origin ref
	}
	for _, s := range refused {
		if DependencyPin(s).Valid() {
			t.Errorf("DependencyPin(%q).Valid() = true; a pin is pid@version, and nothing else", s)
		}
		if _, _, ok := ParseDependencyPin(s); ok {
			t.Errorf("ParseDependencyPin(%q) accepted a non-pin", s)
		}
	}

	accepted := []string{
		string(samplePID) + "@1",
		string(samplePID) + "@1.0.0",
		string(samplePID) + "@v2-rc1",
		string(samplePID) + "@2026-09-15",
		string(samplePID) + "@1_0",
	}
	for _, s := range accepted {
		pin := DependencyPin(s)
		if !pin.Valid() {
			t.Errorf("DependencyPin(%q).Valid() = false, want true", s)
		}
		if pid, _, ok := ParseDependencyPin(s); !ok || pid != samplePID {
			t.Errorf("ParseDependencyPin(%q) = (%q, %v), want the pid half", s, pid, ok)
		}
	}
}

// TestValidateDependencyPinsEmptyListIsValid pins the deliberate decision
// that having no dependencies is not a violation: the checklist item is
// about how dependencies are RECORDED, not about having them.
//
// It is asserted rather than left to a zero-value default because the
// opposite rule — "every version must pin at least one dependency" —
// would be a product requirement no specification states, and this test is
// where that claim is either held or broken.
func TestValidateDependencyPinsEmptyListIsValid(t *testing.T) {
	for _, pins := range [][]DependencyPin{nil, {}} {
		if errs := validateDependencyPins(pins); len(errs) != 0 {
			t.Errorf("validateDependencyPins(%v) = %v, want no refusals", pins, codesOf(validationErrors(joinErrors(errs...))))
		}
	}
}

func TestValidateDependencyPinsReportsEveryMalformedPinInOnePass(t *testing.T) {
	pins := []DependencyPin{
		"latest", // no pid, no separator
		DependencyPin(string(anotherPID) + "@1.0"), // the one good pin
		"", // empty
	}
	errs := validateDependencyPins(pins)
	if len(errs) != 2 {
		t.Fatalf("validateDependencyPins produced %d refusals %v, want 2 — one per malformed pin", len(errs), errs)
	}
	got := validationErrors(joinErrors(errs...))
	if got[0].Field != "dependency_pins[0]" || got[1].Field != "dependency_pins[2]" {
		t.Errorf("refusals name %v, want the two malformed indices", fieldsOf(got))
	}
	for _, e := range got {
		if e.Code != CodeInvalidDependencyPin {
			t.Errorf("%s has code %s, want %s", e.Field, e.Code, CodeInvalidDependencyPin)
		}
	}
}

func TestValidateDependencyPinsRefusesDuplicatesAndNamesTheFirst(t *testing.T) {
	first, _ := NewDependencyPin(samplePID, "1.0")
	second, _ := NewDependencyPin(anotherPID, "1.0")
	pins := []DependencyPin{first, second, first, first}
	errs := validationErrors(joinErrors(validateDependencyPins(pins)...))
	if len(errs) != 2 {
		t.Fatalf("validateDependencyPins produced %d refusals %v, want 2 (the second and third repeat)", len(errs), codesOf(errs))
	}
	if errs[0].Field != dependencyPinPath(2) || errs[1].Field != dependencyPinPath(3) {
		t.Errorf("refusals name %v, want [%s %s]", fieldsOf(errs), dependencyPinPath(2), dependencyPinPath(3))
	}
	for _, e := range errs {
		if e.Code != CodeDuplicateDependencyPin {
			t.Errorf("%s has code %s, want %s", e.Field, e.Code, CodeDuplicateDependencyPin)
		}
		if !strings.Contains(e.Detail, "index 0") {
			t.Errorf("%s detail %q does not name where the pin was first seen", e.Field, e.Detail)
		}
	}
}

// TestValidateSelfPin pins the one pin rule that needs the version's own
// identity: depending on yourself is a cycle of one. A pin on ANOTHER
// version of the same asset is an ordinary dependency and must stay
// allowed — a benchmark's v2 may well be built against its own v1.
func TestValidateSelfPin(t *testing.T) {
	self, _ := NewDependencyPin(samplePID, "1.0")
	other, _ := NewDependencyPin(anotherPID, "1.0")
	ownEarlier, _ := NewDependencyPin(samplePID, "0.9")

	cases := []struct {
		name    string
		pins    []DependencyPin
		wantIdx int
	}{
		{"empty", nil, -1},
		{"another asset", []DependencyPin{other}, -1},
		{"an earlier version of the same asset", []DependencyPin{ownEarlier}, -1},
		{"the version itself", []DependencyPin{self}, 0},
		{"the version itself, second", []DependencyPin{other, self}, 1},
		{"a malformed pin is not a self pin", []DependencyPin{"not-a-pin"}, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			errs := validateSelfPin(c.pins, samplePID, "1.0")
			if c.wantIdx < 0 {
				if len(errs) != 0 {
					t.Fatalf("validateSelfPin(%v) = %v, want no refusal", c.pins, errs)
				}
				return
			}
			if len(errs) != 1 {
				t.Fatalf("validateSelfPin(%v) = %v, want one refusal", c.pins, errs)
			}
			got := validationErrors(joinErrors(errs...))[0]
			if got.Code != CodeSelfDependencyPin || got.Field != dependencyPinPath(c.wantIdx) {
				t.Errorf("self pin = %s at %s, want %s at %s", got.Code, got.Field, CodeSelfDependencyPin, dependencyPinPath(c.wantIdx))
			}
		})
	}
}

// TestValidateSelfPinWithoutAUsableIdentity pins the guard: when the
// candidate's own pid is not a pid there is nothing to compare against,
// and the self-pin check must stay silent rather than refuse every pin —
// the candidate's own pid failure is the gate's to report.
func TestValidateSelfPinWithoutAUsableIdentity(t *testing.T) {
	malformed := []DependencyPin{"not-a-pin"}
	if errs := validateSelfPin(malformed, PID("not-a-pid"), "1.0"); len(errs) != 0 {
		t.Errorf("validateSelfPin without a usable identity = %v, want no refusal", errs)
	}
}
