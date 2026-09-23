package testdb

import "testing"

// The value grammar of RequireE2EDBEnv is the one part of this package a caller
// cannot see from the outside: RequireDB only ever asks it "is a database
// required here?". It is pinned because narrowing it back to "1 and only 1"
// would restore exactly the failure the variable exists to remove — a job that
// meant to demand a database, wrote true, and got a suite that quietly skipped
// its two database journeys and printed ok.
//
// tests/e2e's TestE2EDBGuardDecidesBothWays covers the behaviour end to end
// with the value CI actually sets (1) and with the variable unset; these are
// the spellings in between.
func TestRequiredDBValueGrammar(t *testing.T) {
	cases := []struct {
		value string
		want  string
	}{
		{"", ""},         // unset
		{"0", ""},        // explicitly off
		{" 0 ", ""},      // explicitly off, padded
		{"1", "1"},       // what CI sets
		{" 1 ", "1"},     // padded: environments pad
		{"true", "true"}, // a value nobody meant as "off"
		{"yes", "yes"},   // ditto
	}
	for _, tc := range cases {
		t.Run("value="+tc.value, func(t *testing.T) {
			t.Setenv(RequireE2EDBEnv, tc.value)
			if got := requiredDB(); got != tc.want {
				t.Errorf("requiredDB() with %s=%q = %q, want %q", RequireE2EDBEnv, tc.value, got, tc.want)
			}
		})
	}
}
