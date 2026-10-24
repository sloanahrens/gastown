//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
)

// TestIntegrationGtCommitAuthorMatchesSeatIdentity pins the identity gt commit
// writes to the one gt done's rework lease compares (git.SeatIdentityFor): the
// seat's bare agent name and the worktree's user.email. gt commit used to
// author the compound address at the agent email domain, which the lease read
// as another seat's and refused (gt-tip6u).
func TestIntegrationGtCommitAuthorMatchesSeatIdentity(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runGit("init", "-q")
	// The host's configured identity differs from the seat's in both halves, so
	// only the seat's own decision can produce the expected author.
	runGit("config", "user.name", "Host Human")
	runGit("config", "user.email", "seat@test.com")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "f.txt")

	t.Chdir(dir)
	t.Setenv("GT_ROLE", "gastown/polecats/coma")
	t.Setenv("GIT_AUTHOR_NAME", "coma")

	if err := runCommit(commitCmd, []string{"-m", "test: author identity"}); err != nil {
		t.Fatalf("gt commit: %v", err)
	}

	fields := strings.Split(strings.TrimSpace(runGit("log", "-1", "--format=%an%x00%ae")), "\x00")
	if len(fields) != 2 {
		t.Fatalf("git log returned %d fields, want author name and email", len(fields))
	}
	seat := git.SeatIdentityFor("gastown/polecats/coma", "seat@test.com")
	if fields[0] != seat.Name || fields[1] != seat.Email {
		t.Errorf("author = %q <%s>, want the seat identity %q <%s>", fields[0], fields[1], seat.Name, seat.Email)
	}
}

// TestIntegrationAuthorEmailUnsetIsNotAnError pins which AuthorEmail failures
// gt commit treats as "no email set" (the fallback path) and which it reports:
// git config exits 1 when user.email is unset and 128 when the config cannot
// be read, and only the first is the fallback.
func TestIntegrationAuthorEmailUnsetIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	runGit("init", "-q")
	// Keep the host's global and system config out of the answer.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	global := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.NewGit(dir).AuthorEmail(); !authorEmailUnset(err) {
		t.Errorf("unset user.email: authorEmailUnset(%v) = false, want true", err)
	}

	if err := os.WriteFile(global, []byte("[user\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := git.NewGit(dir).AuthorEmail(); err == nil || authorEmailUnset(err) {
		t.Errorf("unreadable config: authorEmailUnset(%v) = true, want false", err)
	}
}
