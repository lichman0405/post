package main

import (
	"fmt"
	"strings"
)

// flagSpec declares one accepted flag. takesValue flags consume the next
// argument when no =value form is given.
type flagSpec struct {
	name       string
	takesValue bool
}

// parseFlags parses args into a flag value map and the ordered positional
// arguments. Flags may appear before or after positionals (docs/61 shows
// `rddev task reject T0204 --reason-file rejection.md`). Both --name VALUE
// and --name=VALUE are accepted; "--" ends flag parsing.
func parseFlags(args []string, specs ...flagSpec) (map[string]string, []string, error) {
	vals := map[string]string{}
	var pos []string
	known := map[string]bool{}
	valueFlags := map[string]bool{}
	for _, s := range specs {
		known[s.name] = true
		if s.takesValue {
			valueFlags[s.name] = true
		}
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		name, val, isFlag := strings.Cut(a, "=")
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
			continue
		}
		if !isFlag {
			name = a
		}
		if !known[name] {
			return nil, nil, fmt.Errorf("unknown flag: %s", name)
		}
		if isFlag && !valueFlags[name] {
			return nil, nil, fmt.Errorf("flag %s does not take a value", name)
		}
		if !isFlag && valueFlags[name] {
			if i+1 >= len(args) {
				return nil, nil, fmt.Errorf("flag %s requires a value", name)
			}
			i++
			val = args[i]
		}
		vals[name] = val
	}
	return vals, pos, nil
}

// wantsHelp reports whether args ask for help (before flag parsing).
func wantsHelp(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			return true
		}
	}
	return false
}
