//go:build integration

package version

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/buildsrc"
)

// These tests run the staleness check against the checkout buildsrc.Ensure
// makes from a bare repo, with real git: the detached HEAD and the refs the
// check reads are the ones a rebuild sees.

// detachedGit runs git in dir with a fixed identity and returns its trimmed
// output. A git failure fails the test with the call's output.
func detachedGit(t *testing.T, dir string, args ...string) string {
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

// detachedRig makes a town whose gastown rig has a bare repo with two commits
// on main. It returns the town root, the bare repo, a scratch clone that pushes
// to it, and main's tip.
func detachedRig(t *testing.T) (townRoot, bare, work, tip string) {
	t.Helper()
	townRoot = t.TempDir()
	bare = filepath.Join(townRoot, "gastown", ".repo.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	detachedGit(t, townRoot, "init", "-q", "--bare", bare)
	work = t.TempDir()
	detachedGit(t, work, "init", "-q")
	detachedGit(t, work, "checkout", "-q", "-b", "main")
	detachedCommit(t, work, "a.go")
	detachedCommit(t, work, "b.go")
	detachedGit(t, work, "push", "-q", bare, "main")
	return townRoot, bare, work, detachedGit(t, work, "rev-parse", "HEAD")
}

// detachedCommit writes file and commits it in dir, returning the new commit.
func detachedCommit(t *testing.T, dir, file string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	detachedGit(t, dir, "add", "-A")
	detachedGit(t, dir, "commit", "-q", "-m", "c-"+file)
	return detachedGit(t, dir, "rev-parse", "HEAD")
}

// ensureBuildCheckout returns the gastown rig's job-owned checkout, as
// buildsrc.Ensure hands it to the rebuild.
func ensureBuildCheckout(t *testing.T, townRoot string) string {
	t.Helper()
	dir, err := buildsrc.Ensure(context.Background(), townRoot, "gastown", "")
	if err != nil {
		t.Fatalf("buildsrc.Ensure: %v", err)
	}
	return dir
}

// TestIntegrationCheckStaleDetachedBuildCheckout runs checkStale over the
// checkout buildsrc.Ensure returns: on main at main's tip and at an ancestor
// of it, off main at a commit past the tip and on another branch.
func TestIntegrationCheckStaleDetachedBuildCheckout(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		mutate   func(t *testing.T, dir string)
		wantMain bool
	}{
		{name: "detached at main's tip is on main", mutate: func(*testing.T, string) {}, wantMain: true},
		{
			name: "detached at an ancestor of main is on main",
			mutate: func(t *testing.T, dir string) {
				detachedGit(t, dir, "checkout", "-q", "--detach", "HEAD~1")
			},
			wantMain: true,
		},
		{
			name: "detached past main's tip is off main",
			mutate: func(t *testing.T, dir string) {
				detachedCommit(t, dir, "off.go")
			},
		},
		{
			name: "checkout on another branch is off main",
			mutate: func(t *testing.T, dir string) {
				detachedGit(t, dir, "checkout", "-q", "-b", "feature")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			townRoot, _, _, tip := detachedRig(t)
			dir := ensureBuildCheckout(t, townRoot)
			tc.mutate(t, dir)

			c := checker{git: realGit, commit: tip}
			info := c.checkStale(dir)
			if info.Error != nil {
				t.Fatalf("checkStale: %v", info.Error)
			}
			if info.OnMainBranch != tc.wantMain {
				t.Fatalf("OnMainBranch = %v, want %v (CompareRef %q)", info.OnMainBranch, tc.wantMain, info.CompareRef)
			}
		})
	}
}

// TestIntegrationCheckStaleFreshDetachedReadsBareMainWithoutNetwork pins the
// live check on a build checkout with its origin remote pointed at nowhere: a
// fetch would fail, so a verdict that is still given reads the bare repo's main
// alone. A binary at the bare main is fresh; one behind it is stale.
func TestIntegrationCheckStaleFreshDetachedReadsBareMainWithoutNetwork(t *testing.T) {
	t.Parallel()
	townRoot, bare, work, _ := detachedRig(t)
	dir := ensureBuildCheckout(t, townRoot)
	detachedGit(t, dir, "remote", "set-url", "origin", filepath.Join(townRoot, "unreachable.git"))

	detachedCommit(t, work, "c.go")
	detachedGit(t, work, "push", "-q", bare, "main")
	newTip := detachedGit(t, work, "rev-parse", "HEAD")
	if got := ensureBuildCheckout(t, townRoot); got != dir {
		t.Fatalf("Ensure moved the checkout: %s, want %s", got, dir)
	}
	if head := detachedGit(t, dir, "rev-parse", "HEAD"); head != newTip {
		t.Fatalf("Ensure left HEAD at %s, want the new tip %s", head, newTip)
	}

	c := checker{git: realGit, commit: newTip}
	info := c.checkStaleFresh(dir)
	if info.Error != nil || info.Skipped {
		t.Fatalf("checkStaleFresh: error %v, skipped %q", info.Error, info.SkipReason)
	}
	if !info.OnMainBranch || info.CompareRef != "origin/main" {
		t.Fatalf("OnMainBranch = %v, CompareRef %q; want the detached tip read against origin/main", info.OnMainBranch, info.CompareRef)
	}
	if info.IsStale {
		t.Fatalf("a binary at the bare repo's main reads stale: %+v", info)
	}

	// The bare main moves on. The checkout is not re-ensured, so the verdict has
	// to come from the bare repo itself, which is the only place the new tip is.
	detachedCommit(t, work, "d.go")
	detachedGit(t, work, "push", "-q", bare, "main")
	behind := checker{git: realGit, commit: newTip}.checkStaleFresh(dir)
	if behind.Error != nil || behind.Skipped {
		t.Fatalf("checkStaleFresh after the bare main moved: error %v, skipped %q", behind.Error, behind.SkipReason)
	}
	if !behind.IsStale {
		t.Fatalf("a binary at %s reads fresh after the bare main moved on", newTip)
	}
}
