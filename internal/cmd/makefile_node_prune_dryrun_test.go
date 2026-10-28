package cmd

import (
	"fmt"
	"strings"
	"testing"
)

// TestMakefilePrunesTheNodesAfterTheImageBuild pins where the opt-in sits in
// polecat-image's recipe: one prune call, after the docker build, so nothing
// prunes the sandbox nodes for an image that build has not made yet (gt-grnzj).
//
// TestMakefileRunsTheNodePruneOnlyWhenAsked pins the other half of the opt-in,
// the unset default and the guard. Reading the recipe keeps both halves in the
// unit tier, which starts no tool (docs/testing.md, "no-subprocess"), and the
// position is in the text whichever way a later guard is written: ifneq and
// filter answer here as ifeq does.
func TestMakefilePrunesTheNodesAfterTheImageBuild(t *testing.T) {
	t.Parallel()
	lines := strings.Split(readRepoMakefile(t), "\n")
	start, end, ok := makefileTargetBlock(lines, "polecat-image")
	if !ok {
		t.Fatalf("the Makefile declares no polecat-image target, so this test pinned nothing")
	}
	build := -1
	var prunes []int
	for i := start + 1; i < end; i++ {
		if !strings.HasPrefix(lines[i], "\t") {
			continue
		}
		if build < 0 && strings.Contains(lines[i], "docker build") {
			build = i
			continue
		}
		if strings.Contains(lines[i], "polecat-node-prune") {
			prunes = append(prunes, i)
		}
	}
	if build < 0 {
		t.Fatalf("polecat-image runs no docker build between lines %d and %d, so this test pinned nothing: if the recipe moved, move this test with it", start+1, end)
	}
	if len(prunes) != 1 {
		calls := make([]string, len(prunes))
		for i, at := range prunes {
			calls[i] = fmt.Sprintf("line %d: %s", at+1, lines[at])
		}
		t.Fatalf("polecat-image calls polecat-node-prune on %d recipe lines, want one: with a second call the build line no longer orders the prune that runs:\n\t%s", len(prunes), strings.Join(calls, "\n\t"))
	}
	if prunes[0] < build {
		t.Errorf("polecat-node-prune runs at line %d, before the docker build at line %d, so it prunes the nodes for an image that is not built yet:\n\t%s", prunes[0]+1, build+1, lines[prunes[0]])
	}
}

// makefileTargetBlock returns the half-open line range of target's block: its
// definition line and every line after it up to the next target definition, so
// the range covers the recipe and the conditionals make allows between its
// lines.
func makefileTargetBlock(lines []string, target string) (start, end int, ok bool) {
	start = -1
	for i, line := range lines {
		if !isMakefileTargetLine(line) {
			continue
		}
		name, _, _ := strings.Cut(line, ":")
		if strings.TrimSpace(name) == target && start < 0 {
			start = i
			continue
		}
		if start >= 0 {
			return start, i, true
		}
	}
	if start < 0 {
		return 0, 0, false
	}
	return start, len(lines), true
}

// isMakefileTargetLine reports whether line defines a target: it starts in
// column 0, carries a colon, and is not an assignment (a '=' before the colon)
// or a comment.
func isMakefileTargetLine(line string) bool {
	if line == "" || line[0] == '\t' || line[0] == ' ' || line[0] == '#' {
		return false
	}
	name, _, found := strings.Cut(line, ":")
	return found && !strings.Contains(name, "=")
}
