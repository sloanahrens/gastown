package bdshim

import (
	"sort"
	"strings"
	"testing"
)

// oldCommandPath is commandPath as it stood before commandNamespace: the loop
// stepped only through commands the flag tables know. It is the oracle for
// TestCommandPathIsUnchangedOutsideNamespaces, so a change to routing that
// reaches a command the namespace step has nothing to do with fails here.
func oldCommandPath(args []string) string {
	ops := operands("", args)
	if len(ops) == 0 {
		return ""
	}
	path := ops[0]
	if command, ok := aliases[path]; ok {
		path = command
	}
	for _, op := range ops[1:] {
		candidate := path + " " + op
		if command, ok := aliases[candidate]; ok {
			path = command
			continue
		}
		if !knownCommand(candidate) {
			break
		}
		path = candidate
	}
	return path
}

// namespaceOnly returns the names bd documents commands below but that carry
// no flags or operands of their own: the only paths the namespace step may
// change.
func namespaceOnly() map[string]bool {
	out := map[string]bool{}
	add := func(name string) {
		words := strings.Fields(name)
		for i := 1; i < len(words); i++ {
			ns := strings.Join(words[:i], " ")
			// Computed from the tables alone, never from commandNamespace, so a
			// broken commandNamespace cannot empty the list this test checks it
			// against: every prefix of a documented command that is not itself a
			// command is a namespace.
			if !knownCommand(ns) {
				out[ns] = true
			}
		}
	}
	for name := range valueFlags {
		add(name)
	}
	for name := range boolFlags {
		add(name)
	}
	for name := range operandSlots {
		add(name)
	}
	return out
}

// TestCommandPathIsUnchangedOutsideNamespaces drives every command the tables
// know, bare and followed by an id-like operand and by a word no table names,
// and asserts the routed path is what it was before the namespace step. A
// path through a namespace-only name is the one place the step may differ, and
// those are pinned by the next test.
func TestCommandPathIsUnchangedOutsideNamespaces(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	for name := range valueFlags {
		names[name] = true
	}
	for name := range boolFlags {
		names[name] = true
	}
	for name := range operandSlots {
		names[name] = true
	}
	for name := range aliases {
		names[name] = true
	}
	ns := namespaceOnly()
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	checked := 0
	for _, name := range sorted {
		words := strings.Fields(name)
		through := false
		for i := 1; i <= len(words); i++ {
			if ns[strings.Join(words[:i], " ")] {
				through = true
			}
		}
		if through {
			continue
		}
		for _, tail := range [][]string{nil, {"gt-abc"}, {"zzz-not-a-verb"}, {"--json"}, {"gt-abc", "extra"}} {
			args := append(append([]string{}, words...), tail...)
			got, want := commandPath(args), oldCommandPath(args)
			if got != want {
				t.Errorf("commandPath(%q) = %q, was %q before the namespace step", args, got, want)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no command was checked: the tables are empty or every command is namespaced")
	}
}

// TestCommandPathStepsThroughANamespaceOnly pins the one change: a namespace
// word with no flags of its own steps to the documented subcommand, and a word
// that is not a subcommand ends the path at the namespace's parent as before.
func TestCommandPathStepsThroughANamespaceOnly(t *testing.T) {
	t.Parallel()
	ns := namespaceOnly()
	if len(ns) == 0 {
		t.Fatal("the tables document no namespace-only command: the test has nothing to pin")
	}
	names := make([]string, 0, len(ns))
	for name := range ns {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prefix := name + " "
		sub := firstKeyWithPrefix(prefix)
		if sub == "" {
			t.Errorf("namespace %q has no documented subcommand", name)
			continue
		}
		args := strings.Fields(sub)
		if got := commandPath(args); got != sub {
			t.Errorf("commandPath(%q) = %q, want the documented subcommand %q", args, got, sub)
		}
		unknown := append(strings.Fields(name), "zzz-not-a-verb")
		if got := commandPath(unknown); got != name && got != oldCommandPath(unknown) {
			t.Errorf("commandPath(%q) = %q, want it to stop at %q", unknown, got, name)
		}
	}
}

// firstKeyWithPrefix is the alphabetically first documented command below the
// namespace, so the pin does not depend on map order.
func firstKeyWithPrefix(prefix string) string {
	var best string
	consider := func(name string) {
		if strings.HasPrefix(name, prefix) && (best == "" || name < best) {
			best = name
		}
	}
	for name := range valueFlags {
		consider(name)
	}
	for name := range boolFlags {
		consider(name)
	}
	for name := range operandSlots {
		consider(name)
	}
	return best
}
