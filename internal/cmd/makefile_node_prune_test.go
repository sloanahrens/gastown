package cmd

import (
	"strings"
	"testing"
)

// nodePruneDefault is the opt-in's unset default: a build nobody asked to
// prune leaves the sandbox nodes alone (gt-06ni7).
const nodePruneDefault = "POLE_NODE_PRUNE ?= 0"

// nodePruneGuard is the conditional the prune call sits in: an exported or
// command-line =1 is the ask, and nothing else is.
const nodePruneGuard = "ifeq ($(POLE_NODE_PRUNE),1)"

// TestMakefileRunsTheNodePruneOnlyWhenAsked pins both halves of the opt-in: the
// unset default is off, and the recipe calls the prune only under =1
// (gt-pkl4q). The shell cases run the recipe; this one reads it, so a default
// flipped to 1 or a call that escaped its guard fails here.
func TestMakefileRunsTheNodePruneOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	lines := strings.Split(readRepoMakefile(t), "\n")
	if !hasMakefileLine(lines, nodePruneDefault) {
		t.Fatalf("the Makefile declares no %q, so a default build's node prune has no off default", nodePruneDefault)
	}
	calls := 0
	for i, line := range lines {
		if !strings.HasPrefix(line, "\t") || !strings.Contains(line, "polecat-node-prune") {
			continue
		}
		calls++
		if guard := enclosingConditional(lines, i); guard != nodePruneGuard {
			t.Errorf("line %d calls polecat-node-prune outside %q, so a build that did not ask prunes the nodes:\n\t%s",
				i+1, nodePruneGuard, line)
		}
	}
	if calls == 0 {
		t.Fatalf("the Makefile calls polecat-node-prune nowhere, so this test pinned nothing")
	}
}

// hasMakefileLine reports whether src has a line equal to want, ignoring
// surrounding space.
func hasMakefileLine(lines []string, want string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// enclosingConditional returns the conditional the recipe line at i sits in, or
// "" when the nearest non-recipe line above it is not one. The Makefile's
// conditionals nest one level, so that line is the answer.
func enclosingConditional(lines []string, i int) string {
	for j := i - 1; j >= 0; j-- {
		if strings.HasPrefix(lines[j], "\t") || strings.TrimSpace(lines[j]) == "" {
			continue
		}
		line := strings.TrimSpace(lines[j])
		for _, open := range []string{"ifeq ", "ifneq ", "ifdef ", "ifndef "} {
			if strings.HasPrefix(line, open) {
				return line
			}
		}
		return ""
	}
	return ""
}
