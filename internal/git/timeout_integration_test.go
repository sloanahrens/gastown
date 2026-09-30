//go:build integration

package git_test

import (
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// TestIntegrationWithTimeoutKillsRealGit runs real git under a deadline it
// cannot meet: the call fails with an error matching ErrTimedOut, and the
// same repository answers normally without the deadline.
func TestIntegrationWithTimeoutKillsRealGit(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	g := git.NewGit(dir)
	if _, err := g.WithTimeout(time.Nanosecond).Status(); !errors.Is(err, git.ErrTimedOut) {
		t.Fatalf("Status under a 1ns deadline = %v; want ErrTimedOut", err)
	}
	if _, err := g.Status(); err != nil {
		t.Fatalf("Status without a deadline: %v", err)
	}
}
