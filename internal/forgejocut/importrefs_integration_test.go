//go:build integration

package forgejocut

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoresync"
)

// gitTestTimeout bounds one git call the test makes. The repositories are local
// file:// ones, so a call that runs this long is hung.
const gitTestTimeout = 60 * time.Second

// gitTest runs git in dir under the test's own environment and returns its
// trimmed stdout. A failed call fails the test with the subcommand named, never
// its output.
func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), gitTestTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(string(out))
}

// refMap is one side's ref listing as a name-to-sha map, so a test states the refs
// it expects without depending on the order git lists them in.
func refMap(t *testing.T, dir, url string) map[string]string {
	t.Helper()
	refs, err := forgejoresync.ParseRefs([]byte(gitTest(t, dir, "ls-remote", "--heads", "--tags", "--refs", url)))
	if err != nil {
		t.Fatalf("parse the listing of %s: %v", url, err)
	}
	m := make(map[string]string, len(refs))
	for _, ref := range refs {
		m[ref.Name] = ref.SHA
	}
	return m
}

// isAncestorTest reports whether a is b or an ancestor of b in the repository at
// dir, through real git.
func isAncestorTest(dir, a, b string) bool {
	if a == b {
		return true
	}
	return exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// TestIntegrationImportFiveOutcomes runs the import with real git against two
// file:// bare repositories, with one ref of each of the five outcomes, and
// proves no Forgejo ref moved to a sha that does not contain the one it held.
func TestIntegrationImportFiveOutcomes(t *testing.T) {
	// The ports are local file:// repositories, so the test's git calls take no
	// identity or config from the operator's environment.
	for name, value := range map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_AUTHOR_NAME":     "toast",
		"GIT_AUTHOR_EMAIL":    "toast@example.invalid",
		"GIT_COMMITTER_NAME":  "toast",
		"GIT_COMMITTER_EMAIL": "toast@example.invalid",
		"GIT_TERMINAL_PROMPT": "0",
	} {
		t.Setenv(name, value)
	}

	root := t.TempDir()
	gh := filepath.Join(root, "github.git")
	fj := filepath.Join(root, "forgejo.git")
	work := filepath.Join(root, "work")
	ghURL, fjURL := "file://"+gh, "file://"+fj

	gitTest(t, root, "init", "--quiet", "--bare", gh)
	gitTest(t, root, "init", "--quiet", "--bare", fj)
	gitTest(t, root, "init", "--quiet", work)
	commit := func(message string) string {
		t.Helper()
		gitTest(t, work, "commit", "--quiet", "--allow-empty", "-m", message)
		return gitTest(t, work, "rev-parse", "HEAD")
	}

	// GitHub's line on main, three commits long.
	c1 := commit("c1")
	c2 := commit("c2")
	c3 := commit("c3")
	// Forgejo's own line, one commit past c1: the ref a push would rewind.
	gitTest(t, work, "checkout", "--quiet", "-b", "stable", c1)
	s2 := commit("forgejo is ahead")
	// One commit off c1 on each side: the ref a push would have to force.
	gitTest(t, work, "checkout", "--quiet", "-b", "forgejo-diverge", c1)
	y1 := commit("forgejo's side")
	gitTest(t, work, "checkout", "--quiet", "-b", "github-diverge", c1)
	x1 := commit("github's side")

	gitTest(t, work, "push", "--quiet", ghURL,
		c3+":refs/heads/main", c2+":refs/heads/feature", c1+":refs/heads/stable",
		x1+":refs/heads/diverge", c1+":refs/tags/v1")
	gitTest(t, work, "push", "--quiet", fjURL,
		c1+":refs/heads/main", s2+":refs/heads/stable",
		y1+":refs/heads/diverge", c1+":refs/tags/v1")
	before := refMap(t, root, fjURL)

	var logs []string
	opts := Options{
		Repo:           "acme/rig",
		GitHubURL:      ghURL,
		ForgejoURL:     "https://hostname.invalid/acme/rig.git",
		AdminGitURL:    fjURL,
		AcceptDiverged: true,
	}
	cutover := &Cutover{
		Opts: &opts,
		Log:  func(line string) { logs = append(logs, line) },
		MkdirTemp: func(_, pattern string) (string, error) {
			return os.MkdirTemp(root, pattern)
		},
	}
	if err := cutover.ImportRefs(); err != nil {
		t.Fatalf("ImportRefs: %v; logs %q", err, logs)
	}

	want := map[string]string{
		"refs/heads/main":    c3,
		"refs/heads/feature": c2,
		"refs/heads/stable":  s2,
		"refs/heads/diverge": y1,
		"refs/tags/v1":       c1,
	}
	after := refMap(t, root, fjURL)
	if !equalMaps(after, want) {
		t.Fatalf("Forgejo refs = %v, want %v", after, want)
	}
	for name, sha := range before {
		if !isAncestorTest(fj, sha, after[name]) {
			t.Fatalf("%s moved backwards: it held %s and now holds %s", name, sha, after[name])
		}
	}

	for _, line := range []string{
		"will import refs/heads/feature (Forgejo has no copy)",
		"will import refs/heads/main (fast-forward)",
		fmt.Sprintf("kept: refs/heads/stable — Forgejo is ahead of GitHub by 1 commits (forgejo %s, github %s)", s2, c1),
		fmt.Sprintf("up to date: refs/tags/v1 is %s on both", c1),
		fmt.Sprintf("diverged: refs/heads/diverge — Forgejo and GitHub have diverged (forgejo %s, github %s)", y1, x1),
		"imported every GitHub ref into acme/rig (5 refs there)",
	} {
		if !slices.Contains(logs, line) {
			t.Fatalf("logs %q do not hold %q", logs, line)
		}
	}

	// The scratch repository is the step's own litter, and a run that leaves it
	// behind fills the town's temp root.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read the test root: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "forgejo-cutover.") {
			t.Fatalf("the scratch directory %s was left behind", entry.Name())
		}
	}
}

// equalMaps reports whether a and b hold the same keys and values.
func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, sha := range a {
		if b[name] != sha {
			return false
		}
	}
	return true
}
