// Package main is a synthetic MCP server: one tool is registered through a call
// (a dispatch site), one is named in a bare literal (a mention, which is not a
// dispatch), and one appears only in a test. The reconciliation has to tell
// those three apart, and the fourth case — a tool the tree dispatches and the
// catalogue does not declare — has to be reported too.
package main

import "fmt"

// register is the one call a tool can be wired through.
func register(string, any) {}

// dispatch stands for a wiring the catalogue does not know about.
func dispatch(string) {}

func main() {
	register("alpha.one", nil)
	// named, never wired
	fmt.Println("alpha.two")
	dispatch("gamma.one")
}
