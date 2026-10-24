//go:build integration

package cmd

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// makefileBranchStamp captures the shell expression the Makefile runs to stamp
// internal/cmd.Branch.
var makefileBranchStamp = regexp.MustCompile(`(?m)^BRANCH := \$\(shell (.*)\)\s*$`)

// TestIntegrationResolveBranchMakefileStampOnDetachedHead runs the Makefile's
// BRANCH expression in a real repository (gt-t2fom). On a branch it prints the
// branch name; on a detached HEAD it must print nothing, so the binary reports
// no branch instead of the literal "HEAD".
func TestIntegrationResolveBranchMakefileStampOnDetachedHead(t *testing.T) {
	t.Parallel()
	m := makefileBranchStamp.FindStringSubmatch(readRepoMakefile(t))
	if m == nil {
		t.Fatal("Makefile has no BRANCH := $(shell ...) line; if the stamp moved, move this test with it")
	}
	expr := m[1]

	repo := t.TempDir()
	gitInRepo(t, repo, "init", "-q")
	gitInRepo(t, repo, "-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")

	gitInRepo(t, repo, "checkout", "-q", "-b", "polecat/test/gt-t2fom")
	if got := stampFromShell(t, repo, expr); got != "polecat/test/gt-t2fom" {
		t.Fatalf("BRANCH expression on a branch = %q, want the branch name; the expression no longer reads the branch", got)
	}

	gitInRepo(t, repo, "checkout", "-q", "--detach")
	if got := stampFromShell(t, repo, expr); got != "" {
		t.Errorf("BRANCH expression on a detached HEAD = %q, want empty: an empty stamp lets the binary report no branch", got)
	}
}

// gitInRepo runs git in dir with the user's global and system config ignored,
// so the fixture does not depend on the developer's machine.
func gitInRepo(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// stampFromShell evaluates a Makefile $(shell ...) body the way make does: the
// output is its stdout with the trailing newline removed, and the exit status
// is ignored.
func stampFromShell(t *testing.T, dir, expr string) string {
	t.Helper()
	cmd := exec.Command("sh", "-c", expr)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("running %q in %s: %v", expr, dir, err)
	}
	return strings.TrimSuffix(string(out), "\n")
}
