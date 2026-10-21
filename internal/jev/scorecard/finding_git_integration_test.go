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

// PrefetchLines sizes the commits of a window in two git calls, and ChangedLines
// then answers for each exactly as the call-per-commit path does: the changed
// lines against the first parent for an ordinary commit and a merge, abbreviated
// names, ErrNoCommit for a commit the repo lacks, and the same failure for a
// root commit.
func TestIntegrationRigDiffsPrefetchMatchesPerCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	town := t.TempDir()
	repo := filepath.Join(town, dedupe.RigName, "mayor", "rig")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	write("a.txt", "1\n2\n3\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "root")
	root := gitIn(t, repo, "rev-parse", "HEAD")
	write("a.txt", "1\n2\n3\n4\n5\n")
	gitIn(t, repo, "commit", "-q", "-am", "grow")
	grow := gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "checkout", "-q", "-b", "side")
	write("b.txt", "x\ny\nz\nw\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "side work")
	gitIn(t, repo, "checkout", "-q", "main")
	write("a.txt", "1\n2\n3\n4\n5\n6\n")
	gitIn(t, repo, "commit", "-q", "-am", "main work")
	gitIn(t, repo, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	merge := gitIn(t, repo, "rev-parse", "HEAD")
	write("bin.dat", "\x00\x01\x02")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", "binary")
	binary := gitIn(t, repo, "rev-parse", "HEAD")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "empty")
	empty := gitIn(t, repo, "rev-parse", "HEAD")

	commits := []string{grow, merge, binary, empty, root, grow[:9], merge[:7], "0123abc4567def"}
	// The sizes the call-per-commit path reports are the reference.
	plain := rigDiffs{town: town}
	type answer struct {
		n   int
		err string
	}
	ask := func(d rigDiffs, commit string) answer {
		n, err := d.ChangedLines(ctx, commit)
		if err != nil {
			if errors.Is(err, ErrNoCommit) {
				return answer{err: "no commit"}
			}
			return answer{err: "failed"}
		}
		return answer{n: n}
	}
	want := map[string]answer{}
	for _, c := range commits {
		want[c] = ask(plain, c)
	}
	if want[grow] != (answer{n: 2}) || want[merge].n == 0 || want[root].err != "failed" {
		t.Fatalf("the reference answers are not the fixture's: %+v", want)
	}

	sizes := &diffSizes{}
	batched := rigDiffs{town: town, sizes: sizes}
	batched.PrefetchLines(ctx, commits)
	for _, c := range commits {
		if got := ask(batched, c); got != want[c] {
			t.Errorf("ChangedLines(%s) after PrefetchLines = %+v, per-commit path = %+v", c, got, want[c])
		}
	}
	for _, c := range []string{grow, merge, binary, empty, grow[:9], merge[:7]} {
		if _, ok := sizes.lines[c]; !ok {
			t.Errorf("PrefetchLines left %s to a git call of its own", c)
		}
	}
	if !sizes.absent["0123abc4567def"] {
		t.Errorf("PrefetchLines did not record the missing commit as absent")
	}
}
