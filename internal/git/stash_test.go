package git

import (
	"reflect"
	"strings"
	"testing"
)

// stashList is `git stash list` for a repository whose worktrees stashed on
// main, on develop (a commit message containing "on fix:"), on a detached
// HEAD, and on polecat-branch with a custom message.
const stashList = "stash@{0}: On (no branch): own-wip\n" +
	"stash@{1}: WIP on develop: 1a2b3c4 work on fix: edge case\n" +
	"stash@{2}: On polecat-branch: wt-stash\n" +
	"stash@{3}: WIP on main: c18603f remove file\n" +
	"stash@{4}: On main: main-stash\n"

func stashGit(t *testing.T, branch string) (*Git, *scripted) {
	t.Helper()
	s := newScripted(map[string]reply{
		"stash list":                  ok(stashList),
		"rev-parse --abbrev-ref HEAD": ok(branch + "\n"),
	})
	return newTestGit(t, s), s
}

// Stashes are repo-wide; each worktree counts only those labeled with its
// own branch, in either message form git writes.
func TestStashCountFiltersByBranch(t *testing.T) {
	t.Parallel()
	for branch, want := range map[string]int{
		"main":           2, // "WIP on main:" and "On main:"
		"polecat-branch": 1,
		"develop":        1,
		"fix":            0, // "on fix:" in develop's commit message is not a label
		"HEAD":           1, // detached: only "(no branch)" stashes
	} {
		g, _ := stashGit(t, branch)
		if got, err := g.StashCount(); err != nil || got != want {
			t.Errorf("StashCount on %s = %d, %v; want %d", branch, got, err, want)
		}
		if got, err := g.StashCountAll(); err != nil || got != 5 {
			t.Errorf("StashCountAll on %s = %d, %v; want 5", branch, got, err)
		}
	}
}

// When the branch cannot be read, the filter matches everything: counting a
// sibling's stash is the safe error.
func TestStashCountUnknownBranchCountsAll(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"stash list":                  ok(stashList),
		"rev-parse --abbrev-ref HEAD": fail(128, "fatal: ambiguous argument 'HEAD': unknown revision or path not in the working tree.\n"),
	})
	if got, err := newTestGit(t, s).StashCount(); err != nil || got != 5 {
		t.Fatalf("StashCount = %d, %v; want 5", got, err)
	}
}

func TestStashListForBranch(t *testing.T) {
	t.Parallel()
	g, _ := stashGit(t, "main")
	entries, err := g.StashListForBranch()
	if err != nil {
		t.Fatal(err)
	}
	want := []StashEntry{
		{Ref: "stash@{3}", Message: "WIP on main: c18603f remove file"},
		{Ref: "stash@{4}", Message: "On main: main-stash"},
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("StashListForBranch = %+v, want %+v", entries, want)
	}

	g, _ = stashGit(t, "HEAD")
	if entries, _ := g.StashListForBranch(); len(entries) != 1 || !strings.Contains(entries[0].Message, "own-wip") {
		t.Fatalf("detached StashListForBranch = %+v, want just own-wip", entries)
	}

	s := newScripted(map[string]reply{"stash list": ok("")})
	if entries, err := newTestGit(t, s).StashListForBranch(); err != nil || entries != nil {
		t.Fatalf("no stashes = %+v, %v", entries, err)
	}
}

func TestStashPop(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"stash pop stash@{0}": ok(""),
		"stash pop stash@{9}": fail(1, "error: stash@{9} is not a valid reference\n"),
	})
	g := newTestGit(t, s)
	s.on("-C "+g.workDir+" rev-parse --show-toplevel", ok(g.workDir+"\n"))
	if err := g.StashPop("stash@{0}"); err != nil {
		t.Fatalf("StashPop: %v", err)
	}
	if err := g.StashPop("stash@{9}"); err == nil || !strings.Contains(err.Error(), "git stash pop stash@{9}") {
		t.Fatalf("StashPop of a bad ref = %v", err)
	}
	if err := g.StashPop(""); err == nil {
		t.Fatal(`StashPop("") should error`)
	}
}
