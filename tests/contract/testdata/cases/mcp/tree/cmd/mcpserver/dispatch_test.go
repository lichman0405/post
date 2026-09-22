package main

import "testing"

// TestRegister proves a name mentioned only in a test is not a wiring: the
// scan skips _test.go for the same reason the route enumerator does.
func TestRegister(t *testing.T) {
	register("alpha.three", nil)
}
