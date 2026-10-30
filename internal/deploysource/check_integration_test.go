//go:build integration

package deploysource

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRealRepoCases runs the check against throwaway repositories:
// the cases scripts/check-deploy-source_test.sh built, one repository state
// each, through ExecGit rather than a fake. It covers what a fake cannot —
// git's own answers for an equal commit, a detached HEAD, a branch with no
// upstream, the .beads pathspec — and the porcelain parsing.
func TestIntegrationRealRepoCases(t *testing.T) {
	// The user's git config must not change an answer: the check and the
	// fixtures both run with the global and system files switched off.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	cases := []struct {
		name        string
		setup       func(t *testing.T, bare, work string)
		opts        Options
		wantPass    bool
		wantRefusal string
		wantOutput  string
		// verify gets originBefore, the origin/main the work tree held before
		// the check ran.
		verify func(t *testing.T, work, out, originBefore string)
	}{
		{
			name:     "HEAD equal to origin/main passes",
			wantPass: true,
		},
		{
			name: "HEAD behind origin/main passes and warns",
			setup: func(t *testing.T, _, work string) {
				gitIn(t, work, "reset", "--hard", "HEAD~1")
			},
			wantPass:   true,
			wantOutput: "behind origin/main",
		},
		{
			name: "an unmerged local commit is refused",
			setup: func(t *testing.T, _, work string) {
				commitUnmerged(t, work)
			},
			wantRefusal: "is not merged into origin/main",
		},
		{
			name: "a detached HEAD on an unmerged commit is refused",
			setup: func(t *testing.T, _, work string) {
				commitUnmerged(t, work)
				gitIn(t, work, "checkout", "--detach", "HEAD")
			},
			wantRefusal: "is not merged into origin/main",
		},
		{
			name: "a branch without an upstream at a merged commit passes",
			setup: func(t *testing.T, _, work string) {
				gitIn(t, work, "checkout", "-b", "feature", "--no-track", "origin/main")
			},
			wantPass: true,
		},
		{
			name: "a branch without an upstream on an unmerged commit is refused",
			setup: func(t *testing.T, _, work string) {
				gitIn(t, work, "checkout", "-b", "feature", "--no-track")
				commitUnmerged(t, work)
			},
			wantRefusal: "is not merged into origin/main",
		},
		{
			name: "ALLOW_UNMERGED overrides the refusal loudly",
			setup: func(t *testing.T, _, work string) {
				commitUnmerged(t, work)
			},
			opts:       Options{AllowUnmerged: true},
			wantPass:   true,
			wantOutput: "UNMERGED",
		},
		{
			name: "a tracked modification outside .beads refuses",
			setup: func(t *testing.T, _, work string) {
				writeFile(t, filepath.Join(work, "file"), "dirty")
			},
			wantRefusal: "tracked files outside .beads/ are modified",
		},
		{
			name: ".beads edits and untracked files pass",
			setup: func(t *testing.T, _, work string) {
				writeFile(t, filepath.Join(work, ".beads", "issues.jsonl"), "y\n")
				writeFile(t, filepath.Join(work, "untracked.txt"), "junk\n")
			},
			wantPass: true,
		},
		{
			// git status without -z C-quotes this path, so only the -z read
			// sees it under .beads/.
			name: "a .beads edit to a path git would C-quote passes",
			setup: func(t *testing.T, _, work string) {
				writeFile(t, filepath.Join(work, ".beads", "a b é.jsonl"), "changed\n")
			},
			wantPass: true,
		},
		{
			name: "a main that moved is not fetched under SKIP_UPDATE_CHECK",
			setup: func(t *testing.T, bare, work string) {
				other := filepath.Join(t.TempDir(), "other")
				gitIn(t, "", "clone", "--quiet", bare, other)
				writeFile(t, filepath.Join(other, "file"), "c")
				gitIn(t, other, "commit", "-am", "three")
				gitIn(t, other, "push", "--quiet", "origin", "main")
			},
			opts:     Options{SkipUpdateCheck: true},
			wantPass: true,
			verify: func(t *testing.T, work, _, originBefore string) {
				if got := gitIn(t, work, "rev-parse", "origin/main"); got != originBefore {
					t.Errorf("origin/main moved to %s; SKIP_UPDATE_CHECK must not fetch (gt-9jax)", got)
				}
			},
		},
		{
			name: "SKIP_UPDATE_CHECK does not skip the merged check",
			setup: func(t *testing.T, _, work string) {
				commitUnmerged(t, work)
			},
			opts:        Options{SkipUpdateCheck: true},
			wantRefusal: "is not merged into origin/main",
		},
		{
			name:        "a main ref that does not resolve is refused",
			opts:        Options{MainRef: "origin/nope"},
			wantRefusal: "cannot resolve origin/nope",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Each row gets its own origin, so a row that pushes to it cannot
			// change what a later row sees.
			bare := newOrigin(t)
			work := cloneOrigin(t, bare)
			if tc.setup != nil {
				tc.setup(t, bare, work)
			}
			originBefore := gitIn(t, work, "rev-parse", "origin/main")
			var w bytes.Buffer
			err := Run(ExecGit{Dir: work}, tc.opts, &w)
			out := w.String()
			switch {
			case tc.wantPass && err != nil:
				t.Fatalf("Run = %v, want nil (output %q)", err, out)
			case !tc.wantPass:
				var refusal *Refusal
				if !errors.As(err, &refusal) {
					t.Fatalf("Run = %v, want a *Refusal (output %q)", err, out)
				}
				if !strings.Contains(refusal.Reason, tc.wantRefusal) {
					t.Errorf("refusal = %q, want it to contain %q", refusal.Reason, tc.wantRefusal)
				}
			}
			if tc.wantOutput != "" && !strings.Contains(out, tc.wantOutput) {
				t.Errorf("output = %q, want it to contain %q", out, tc.wantOutput)
			}
			if tc.verify != nil {
				tc.verify(t, work, out, originBefore)
			}
		})
	}
}

// newOrigin builds a bare repository with two commits on main.
func newOrigin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bare := filepath.Join(dir, "origin.git")
	gitIn(t, "", "init", "--bare", "-b", "main", bare)
	seed := filepath.Join(dir, "seed")
	gitIn(t, "", "init", "-b", "main", seed)
	if err := os.MkdirAll(filepath.Join(seed, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(seed, "file"), "a")
	writeFile(t, filepath.Join(seed, ".beads", "issues.jsonl"), "x\n")
	writeFile(t, filepath.Join(seed, ".beads", "a b é.jsonl"), "x\n")
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-m", "one")
	writeFile(t, filepath.Join(seed, "file"), "b")
	gitIn(t, seed, "commit", "-am", "two")
	gitIn(t, seed, "remote", "add", "origin", bare)
	gitIn(t, seed, "push", "--quiet", "origin", "main")
	return bare
}

// cloneOrigin clones bare into a fresh directory: a clean checkout at the tip
// of main with origin/main present.
func cloneOrigin(t *testing.T, bare string) string {
	t.Helper()
	work := filepath.Join(t.TempDir(), "work")
	gitIn(t, "", "clone", "--quiet", bare, work)
	return work
}

// commitUnmerged adds a commit to work that no remote has.
func commitUnmerged(t *testing.T, work string) {
	t.Helper()
	writeFile(t, filepath.Join(work, "file"), "unmerged")
	gitIn(t, work, "commit", "-am", "unmerged")
}

// gitIn runs git, with a throwaway identity, in dir ("" is the process's
// working directory) and fails the test when git does.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{
		"-c", "user.email=test@example.invalid",
		"-c", "user.name=test",
		"-c", "commit.gpgsign=false",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeFile writes content to path, failing the test when it cannot.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
