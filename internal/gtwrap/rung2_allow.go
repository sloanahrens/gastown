package gtwrap

import "slices"

// rung2Allowlist names the calls that may run under rung 2, the throwaway
// container beside a daemon the probe may have missed. Each entry is an argv
// prefix: a call runs only when its leading arguments equal an entry exactly, so
// `gt status --json` runs and `gt statusx` does not. A flag before the verb matches
// nothing. Every other call is refused with the rung-3 start command. The rule is
// set out in docs/design/gt-in-container.md. A command that starts, stops,
// restarts or repairs the daemon or Dolt never belongs here: the test in
// rung2_allow_test.go fails when one does.
var rung2Allowlist = [][]string{
	{"gt", "status"},
	{"gt", "prime"},
	{"gt", "hook"},
	{"gt", "mail", "inbox"},
	{"bd", "show"},
	{"bd", "list"},
	{"bd", "ready"},
}

// rung2Allows reports whether argv starts with an entry of rung2Allowlist. An
// empty argv matches nothing.
func rung2Allows(argv []string) bool {
	for _, prefix := range rung2Allowlist {
		if len(argv) >= len(prefix) && slices.Equal(argv[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}
