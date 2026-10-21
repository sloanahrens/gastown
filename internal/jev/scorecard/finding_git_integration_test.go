//go:build integration

package scorecard

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/jev/dedupe"
)

// gitIn runs git in a fixture repository and returns its trimmed stdout. The
// identity is set on the command line so the fixture needs no user config.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.com",
		"-c", "commit.gpgsign=false"}, args...)
	out, err := exec.Command("git", full...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}

// The git path sizes a landed commit in the rig's repo. A commit the repo lacks
// is ErrNoCommit; a repo git cannot read fails the report; a commit it holds
// counts its changed lines against its first parent.
func TestIntegrationRigDiffsChangedLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	noRepo := t.TempDir()
	if _, err := (rigDiffs{town: noRepo}).ChangedLines(ctx, landedSHA); err == nil || errors.Is(err, ErrNoCommit) {
		t.Errorf("a rig with no git repo gave error %v, want a failure that is not ErrNoCommit", err)
	}

	broken := t.TempDir()
	if err := os.MkdirAll(filepath.Join(broken, dedupe.RigName, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (rigDiffs{town: broken}).ChangedLines(ctx, landedSHA); err == nil || errors.Is(err, ErrNoCommit) {
		t.Errorf("an unreadable repo gave error %v, want a failure that is not ErrNoCommit", err)
	}

	town := t.TempDir()
	repo := filepath.Join(town, dedupe.RigName, "mayor", "rig")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	file := filepath.Join(repo, "a.txt")
	if err := os.WriteFile(file, []byte("1\n2\n3\n4\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "base")
	if err := os.WriteFile(file, []byte("1\n2\n3\n4\nx\ny\nz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "change")
	landed := gitIn(t, repo, "rev-parse", "HEAD")

	d := rigDiffs{town: town}
	if n, err := d.ChangedLines(ctx, landed); err != nil || n != 4 {
		t.Errorf("ChangedLines(landed) = %d, %v; want 4 (one removed, three added)", n, err)
	}
	if _, err := d.ChangedLines(ctx, landedSHA); !errors.Is(err, ErrNoCommit) {
		t.Errorf("ChangedLines(missing commit) error = %v, want ErrNoCommit", err)
	}
}
