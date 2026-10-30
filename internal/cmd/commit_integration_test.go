//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationGtCommitAuthorMatchesSeatIdentity pins that gt commit authors as
// the seat's own identity from the seat alone: its bare agent name and
// <name>@gastown.local, even though the worktree's user.name and user.email name
// someone else. The lease in gt done accepts that address.
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

	fields := strings.Split(strings.TrimSpace(runGit("log", "-1", "--format=%an%x00%ae%x00%cn%x00%ce")), "\x00")
	if len(fields) != 4 {
		t.Fatalf("git log returned %d fields, want author and committer name and email", len(fields))
	}
	seat := seatForCommit("gastown/polecats/coma")
	for i, want := range []string{seat.Name, seat.Email, seat.Name, seat.Email} {
		if fields[i] != want {
			t.Errorf("field %d (author name, author email, committer name, committer email) = %q, want %q", i, fields[i], want)
		}
	}
	if seat.Email != "coma@gastown.local" {
		t.Errorf("seat email = %q, want coma@gastown.local", seat.Email)
	}
}

// TestIntegrationGtCommitWithoutUserEmailCommitsAsTheSeat pins that an agent's
// commit in a worktree with no user.email at all is made, authored as the seat's
// own address: the identity comes from the seat, not from git config. The host's
// global and system config are kept out so no email is inherited.
func TestIntegrationGtCommitWithoutUserEmailCommitsAsTheSeat(t *testing.T) {
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
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "f.txt")

	t.Chdir(dir)
	t.Setenv("GT_ROLE", "gastown/polecats/coma")

	if err := runCommit(commitCmd, []string{"-m", "test: no author email in git config"}); err != nil {
		t.Fatalf("gt commit with no user.email: %v", err)
	}
	if got := strings.TrimSpace(runGit("log", "-1", "--format=%an <%ae>")); got != "coma <coma@gastown.local>" {
		t.Errorf("author = %q, want coma <coma@gastown.local>", got)
	}
}
