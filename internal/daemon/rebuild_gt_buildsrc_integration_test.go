//go:build integration

package daemon

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/buildsrc"
)

// These tests run the rebuild preflight against real git: the checkout is the
// one buildsrc.Ensure makes from a bare repo, so the branch check reads what a
// rebuild would read. The daemon's unit tier runs no git (testpolicy/gitfree.txt).

// buildsrcGit runs git in dir with a fixed identity and returns its trimmed
// output. A git failure fails the test with the call's output.
func buildsrcGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// buildsrcRig makes a town whose gastown rig has a bare repo with two commits
// on main, and returns the town root and main's tip.
func buildsrcRig(t *testing.T) (townRoot, tip string) {
	t.Helper()
	townRoot = t.TempDir()
	bare := filepath.Join(townRoot, "gastown", ".repo.git")
	mkdirs(t, filepath.Dir(bare))
	buildsrcGit(t, townRoot, "init", "-q", "--bare", bare)
	work := t.TempDir()
	buildsrcGit(t, work, "init", "-q")
	buildsrcGit(t, work, "checkout", "-q", "-b", "main")
	writeFile(t, filepath.Join(work, "a.go"), "package a\n")
	buildsrcGit(t, work, "add", "-A")
	buildsrcGit(t, work, "commit", "-q", "-m", "first")
	writeFile(t, filepath.Join(work, "b.go"), "package a\n")
	buildsrcGit(t, work, "add", "-A")
	buildsrcGit(t, work, "commit", "-q", "-m", "second")
	buildsrcGit(t, work, "push", "-q", bare, "main")
	return townRoot, buildsrcGit(t, work, "rev-parse", "HEAD")
}

// TestIntegrationRebuildGTOnMainRealBuildCheckout drives the preflight's branch
// check against the checkout buildsrc.Ensure returns: accepted at main's tip,
// refused at any other commit and on another branch, and an error (so the cycle
// defers) when origin/main cannot be read.
func TestIntegrationRebuildGTOnMainRealBuildCheckout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(t *testing.T, dir string)
		wantOK  bool
		wantErr bool
		wantWhy string
	}{
		{name: "detached at main's tip is accepted", mutate: func(*testing.T, string) {}, wantOK: true},
		{
			name: "detached at another commit is refused",
			mutate: func(t *testing.T, dir string) {
				buildsrcGit(t, dir, "commit", "-q", "--allow-empty", "-m", "off main")
			},
			wantWhy: "not on main branch (detached at ",
		},
		{
			name: "checkout on another branch is refused",
			mutate: func(t *testing.T, dir string) {
				buildsrcGit(t, dir, "checkout", "-q", "-b", "feature")
			},
			wantWhy: "not on main branch (on feature)",
		},
		{
			// The bare main moves on and the checkout's origin/main is refreshed to
			// it (the fetch Ensure runs), but HEAD stays at the old tip: refused.
			name: "bare main moved past the checkout is refused",
			mutate: func(t *testing.T, dir string) {
				bare, ok := buildsrc.BareRepoOf(dir)
				if !ok {
					t.Fatalf("%s is not a build checkout", dir)
				}
				old := buildsrcGit(t, dir, "rev-parse", "HEAD")
				next := buildsrcGit(t, dir, "commit-tree", old+"^{tree}", "-p", old, "-m", "next")
				buildsrcGit(t, dir, "push", "-q", bare, next+":refs/heads/main")
				buildsrcGit(t, dir, "fetch", "-q", "--no-tags", "--", bare, "+refs/heads/main:refs/remotes/origin/main")
			},
			wantWhy: "not on main branch (detached at ",
		},
		{
			name: "detached with origin/main unreadable defers",
			mutate: func(t *testing.T, dir string) {
				buildsrcGit(t, dir, "update-ref", "-d", "refs/remotes/origin/main")
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot, tip := buildsrcRig(t)
			dir, err := buildsrc.Ensure(context.Background(), townRoot, "gastown", "")
			if err != nil {
				t.Fatalf("buildsrc.Ensure: %v", err)
			}
			if head := buildsrcGit(t, dir, "rev-parse", "HEAD"); head != tip {
				t.Fatalf("Ensure left HEAD at %s, want main's tip %s", head, tip)
			}
			tc.mutate(t, dir)

			d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}}
			onMain, why, err := d.rebuildGTOnMain(dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("rebuildGTOnMain: error %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if onMain != tc.wantOK {
				t.Fatalf("onMain = %v (why %q), want %v", onMain, why, tc.wantOK)
			}
			if tc.wantWhy != "" && !strings.HasPrefix(why, tc.wantWhy) {
				t.Fatalf("why = %q, want prefix %q", why, tc.wantWhy)
			}
		})
	}
}
