package git

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// porcelain is `git status --porcelain -uall` output: an unstaged edit, a
// staged new file, a staged deletion, and two untracked files whose names git
// C-quotes.
const porcelain = " M README.md\nA  added.go\nD  gone.go\n?? \"caf\\303\\251.txt\"\n?? \"weird\\\"quote.txt\"\n"

func TestStatusParsesPorcelain(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"status --porcelain -uall": ok(porcelain),
		"ls-files -v -z":           ok("H README.md\x00H added.go\x00"),
	})
	st, err := newTestGit(t, s).Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Clean {
		t.Error("Clean = true for a dirty tree")
	}
	// The sole leading space of " M" must survive: TrimSpace on the whole
	// output once parsed it as code "M " and path "EADME.md".
	if !reflect.DeepEqual(st.Modified, []string{"README.md"}) {
		t.Errorf("Modified = %q, want [README.md]", st.Modified)
	}
	if !reflect.DeepEqual(st.Added, []string{"added.go"}) || !reflect.DeepEqual(st.Deleted, []string{"gone.go"}) {
		t.Errorf("Added = %q, Deleted = %q", st.Added, st.Deleted)
	}
	// C-quoted paths are unquoted: left quoted they name no real file.
	if want := []string{"café.txt", `weird"quote.txt`}; !reflect.DeepEqual(st.Untracked, want) {
		t.Errorf("Untracked = %q, want %q", st.Untracked, want)
	}
	if want := []string{"added.go", "gone.go"}; !reflect.DeepEqual(st.StagedOnly, want) {
		t.Errorf("StagedOnly = %q, want %q", st.StagedOnly, want)
	}
}

func TestStatusCleanTreeSkipsSkipWorktreeLookup(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"status --porcelain -uall": ok("")})
	st, err := newTestGit(t, s).Status()
	if err != nil || !st.Clean {
		t.Fatalf("Status = %+v, %v; want clean", st, err)
	}
	s.noUnscripted(t)
}

// Files hidden by sparse checkout show as deletions in porcelain but carry
// the skip-worktree bit; they are not real deletions. ls-files -v -z names
// them verbatim, so the lookup by Status()'s unquoted path must hit.
func TestStatusIgnoresSkipWorktreeDeletions(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"status --porcelain -uall": ok(" D \"weird\\\"quote.txt\"\n D \"caf\\303\\251.txt\"\n D plain.txt\n"),
		"ls-files -v -z":           ok("H README.md\x00S café.txt\x00S weird\"quote.txt\x00S plain.txt\x00"),
	})
	g := newTestGit(t, s)
	if got, want := g.skipWorktreeFiles(), map[string]bool{"café.txt": true, `weird"quote.txt`: true, "plain.txt": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("skipWorktreeFiles() = %v, want %v", got, want)
	}
	st, err := g.Status()
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !st.Clean || len(st.Deleted) != 0 {
		t.Fatalf("Status = %+v; skip-worktree deletions must read clean", st)
	}
}

func TestStatusIgnoringSubmodulesAsksGitToIgnoreThem(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"status --porcelain -uall --ignore-submodules=all": ok("")})
	st, err := newTestGit(t, s).StatusIgnoringSubmodules()
	if err != nil || !st.Clean {
		t.Fatalf("StatusIgnoringSubmodules = %+v, %v", st, err)
	}
	s.noUnscripted(t)
}

func TestStatusOnMissingWorkDirReportsMissingDirectoryNotGitBinary(t *testing.T) {
	t.Parallel()
	// A polecat whose worktree directory was removed out from under it
	// (gt-2h6).
	gone := filepath.Join(t.TempDir(), "gone")
	s := newScripted(nil)
	_, err := (&Git{workDir: gone, exec: s.run}).Status()
	if err == nil {
		t.Fatal("expected an error for a missing working directory")
	}
	msg := err.Error()
	if strings.Contains(msg, "fork/exec") || !strings.Contains(msg, "working directory does not exist") || !strings.Contains(msg, gone) {
		t.Errorf("error = %q; want it to name the missing directory", msg)
	}
	if len(s.sent()) != 0 {
		t.Errorf("git was run for a missing directory: %q", s.sent())
	}
}

// A failing command surfaces as a GitError carrying git's raw output for
// the caller to observe (ZFC).
func TestFailedCommandIsGitErrorWithRawOutput(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"rev-parse --abbrev-ref HEAD": fail(128, "fatal: not a git repository (or any of the parent directories): .git\n"),
	})
	_, err := newTestGit(t, s).CurrentBranch()
	var ge *GitError
	if !errors.As(err, &ge) {
		t.Fatalf("error = %T %v, want *GitError", err, err)
	}
	if ge.Command != "rev-parse" || !strings.HasPrefix(ge.Stderr, "fatal: not a git repository") || exitCode(ge) != 128 {
		t.Errorf("GitError = %+v", ge)
	}
	if !strings.Contains(ge.Error(), "not a git repository") {
		t.Errorf("Error() = %q", ge.Error())
	}
}

func TestTopLevelCleansPathAndRejectsEmpty(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"rev-parse --show-toplevel": ok("/srv/repo/sub/..\n")})
	if got, err := newTestGit(t, s).TopLevel(); err != nil || got != "/srv/repo" {
		t.Fatalf("TopLevel = %q, %v; want /srv/repo", got, err)
	}
	s = newScripted(map[string]reply{"rev-parse --show-toplevel": ok("\n")})
	if _, err := newTestGit(t, s).TopLevel(); err == nil {
		t.Fatal("TopLevel with no path printed should fail")
	}
}

// With gitDir set (a bare repo), every call carries --git-dir first.
func TestGitDirPrefixesEveryCall(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"--git-dir=/srv/rig/.repo.git rev-parse HEAD": ok("abc\n")})
	g := &Git{gitDir: "/srv/rig/.repo.git", exec: s.run}
	if got, err := g.Rev("HEAD"); err != nil || got != "abc" {
		t.Fatalf("Rev = %q, %v", got, err)
	}
}

func TestLogGrepSearchesFixedStringOnRef(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{
		"log HEAD --grep=gt-hsg -F -1 --oneline":  ok("1a2b3c4 fix: squash-merged change (gt-hsg)\n"),
		"log HEAD --grep=gt-none -F -1 --oneline": ok(""),
	})
	g := newTestGit(t, s)
	if found, err := g.LogGrep("HEAD", "gt-hsg"); err != nil || !found {
		t.Errorf("LogGrep(gt-hsg) = %v, %v; want true", found, err)
	}
	if found, err := g.LogGrep("HEAD", "gt-none"); err != nil || found {
		t.Errorf("LogGrep(gt-none) = %v, %v; want false", found, err)
	}
}
