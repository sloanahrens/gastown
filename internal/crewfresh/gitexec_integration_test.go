//go:build integration

package crewfresh

import (
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier, which drives real git, under the
// hermetic harness: it gives the temporary repositories a git identity and
// keeps them off the live town.
func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

// gitT runs git in dir and returns its trimmed output, failing the test when
// git refuses.
func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed git subcommand built by the test
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// writeT writes body to dir/name.
func writeT(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// cloneFixture is the shape NewGit is proved on: a bare origin, the seed clone
// that pushes to it, and the clone Run is asked about.
type cloneFixture struct {
	origin string
	seed   string
	clone  string
}

// newCloneFixture builds that shape under t.TempDir, origin seeded with main
// and the clone taken from it.
func newCloneFixture(t *testing.T) cloneFixture {
	t.Helper()
	root := t.TempDir()
	f := cloneFixture{
		origin: filepath.Join(root, "origin.git"),
		seed:   filepath.Join(root, "seed"),
		clone:  filepath.Join(root, "clone"),
	}
	gitT(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	gitT(t, root, "init", "-q", "-b", "main", f.seed)
	gitT(t, f.seed, "remote", "add", "origin", f.origin)
	writeT(t, f.seed, "readme.txt", "v1\n")
	gitT(t, f.seed, "add", "readme.txt")
	gitT(t, f.seed, "commit", "-q", "-m", "origin: seed")
	gitT(t, f.seed, "push", "-q", "origin", "main")
	gitT(t, root, "clone", "-q", f.origin, f.clone)
	return f
}

// advanceOrigin pushes count commits to origin's main, each a new version of
// readme.txt, so the clone has that many to catch up.
func (f cloneFixture) advanceOrigin(t *testing.T, count int) {
	t.Helper()
	for i := range count {
		writeT(t, f.seed, "readme.txt", fmt.Sprintf("v%d\n", i+2))
		gitT(t, f.seed, "commit", "-q", "-am", fmt.Sprintf("origin: v%d", i+2))
		gitT(t, f.seed, "push", "-q", "origin", "main")
	}
}

// snapshot is everything a move would change: HEAD, the checked-out branch and
// every file outside .git, with its content.
type snapshot struct {
	head   string
	branch string
	files  map[string]string
}

// takeSnapshot reads dir's git state and files, so a test can prove a Run left
// them exactly as they were. .git is skipped: a fetch writes refs there without
// moving anything the clone's user sees.
func takeSnapshot(t *testing.T, dir string) snapshot {
	t.Helper()
	s := snapshot{
		head:   gitT(t, dir, "rev-parse", "HEAD"),
		branch: gitT(t, dir, "rev-parse", "--abbrev-ref", "HEAD"),
		files:  map[string]string{},
	}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		s.files[rel] = string(body)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", dir, err)
	}
	return s
}

// changedFiles names the files whose content differs between before and after,
// or that exist on only one side.
func changedFiles(before, after map[string]string) []string {
	paths := make([]string, 0, len(before)+len(after))
	for p := range before {
		paths = append(paths, p)
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	var changed []string
	for _, p := range paths {
		if before[p] != after[p] {
			changed = append(changed, p)
		}
	}
	return changed
}

// Three commits pushed to origin land on a clean main, and the working tree
// comes with them (gt-vpj84).
func TestIntegrationNewGitFastForwardsACleanMain(t *testing.T) {
	f := newCloneFixture(t)
	f.advanceOrigin(t, 3)

	report, err := Run(NewGit(f.clone), time.Minute)
	if err != nil {
		t.Fatalf("Run = error %v, want nil", err)
	}
	if report.Action != ActionFastForward {
		t.Errorf("Report.Action = %d, want ActionFastForward (message %q)", report.Action, report.Message)
	}
	if report.Behind != 3 {
		t.Errorf("Report.Behind = %d, want 3", report.Behind)
	}
	if want, got := gitT(t, f.origin, "rev-parse", "main"), gitT(t, f.clone, "rev-parse", "HEAD"); got != want {
		t.Errorf("clone HEAD = %s, want origin/main %s", got, want)
	}
	if got := gitT(t, f.clone, "status", "--porcelain"); got != "" {
		t.Errorf("clone status = %q, want a clean working tree", got)
	}
	body, err := os.ReadFile(filepath.Join(f.clone, "readme.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "v4\n"; string(body) != want {
		t.Errorf("clone readme.txt = %q, want %q", body, want)
	}
}

// A failed fetch is reported by the command that failed, never by git's own
// output: a remote URL can carry a credential in its path, so the error must
// not repeat it (gt-vpj84).
func TestIntegrationNewGitErrorsDoNotRepeatGitsOutput(t *testing.T) {
	f := newCloneFixture(t)
	const secret = "token-do-not-print-4f2a"
	gitT(t, f.clone, "remote", "set-url", "origin", filepath.Join(t.TempDir(), secret, "missing.git"))

	err := NewGit(f.clone).Fetch(time.Minute)
	if err == nil {
		t.Fatal("Fetch = nil error, want the failed fetch")
	}
	if want := "git fetch origin main"; !strings.Contains(err.Error(), want) {
		t.Errorf("Fetch error = %q, want it to name %q", err, want)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("Fetch error repeats git's output, which names the remote path %q: %q", secret, err)
	}
}

// A clone with local changes, on another branch, or with its own commit on
// main is left exactly where it was, HEAD and files alike (gt-vpj84).
func TestIntegrationNewGitMovesNothingWhenDirtyOrOnABranchOrDiverged(t *testing.T) {
	cases := []struct {
		name  string
		setUp func(t *testing.T, f cloneFixture)
	}{
		{
			name: "dirty",
			setUp: func(t *testing.T, f cloneFixture) {
				writeT(t, f.clone, "readme.txt", "uncommitted\n")
			},
		},
		{
			name: "on another branch",
			setUp: func(t *testing.T, f cloneFixture) {
				gitT(t, f.clone, "checkout", "-q", "-b", "crew/work")
			},
		},
		{
			name: "diverged",
			setUp: func(t *testing.T, f cloneFixture) {
				writeT(t, f.clone, "local.txt", "local work\n")
				gitT(t, f.clone, "add", "local.txt")
				gitT(t, f.clone, "commit", "-q", "-m", "local: work")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCloneFixture(t)
			f.advanceOrigin(t, 2)
			tc.setUp(t, f)
			before := takeSnapshot(t, f.clone)

			report, err := Run(NewGit(f.clone), time.Minute)
			if err != nil {
				t.Fatalf("Run = error %v, want nil", err)
			}
			if report.Action != ActionNone {
				t.Errorf("Report.Action = %d, want ActionNone (message %q)", report.Action, report.Message)
			}
			after := takeSnapshot(t, f.clone)
			if after.head != before.head {
				t.Errorf("HEAD moved from %s to %s", before.head, after.head)
			}
			if after.branch != before.branch {
				t.Errorf("branch moved from %q to %q", before.branch, after.branch)
			}
			if !maps.Equal(after.files, before.files) {
				t.Errorf("files changed: %v", changedFiles(before.files, after.files))
			}
		})
	}
}
