//go:build integration

package forgejoresync

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// gitIn runs git with -C dir, the test's identity set, and returns its trimmed stdout. A git
// call that fails fails the test with the subcommand named, never its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=toast", "GIT_AUTHOR_EMAIL=toast@example.invalid",
		"GIT_COMMITTER_NAME=toast", "GIT_COMMITTER_EMAIL=toast@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(string(out))
}

// refsOf reads the branches and tags of the repository at url through git ls-remote, the
// listing the run compares.
func refsOf(t *testing.T, dir, url string) []Ref {
	t.Helper()
	refs, err := ParseRefs([]byte(gitIn(t, dir, "ls-remote", "--heads", "--tags", "--refs", url)))
	if err != nil {
		t.Fatalf("parse listing of %s: %v", url, err)
	}
	return refs
}

// TestIntegrationPruningPushConvergesRealRefs runs the flow with real git against two file://
// bare repositories. GitHub is ahead on main, Forgejo's main has diverged from GitHub's history,
// Forgejo holds a branch GitHub deleted, and both carry the tag. After the run Forgejo must hold
// exactly GitHub's refs: the forced push settles the diverged main and the prune deletes the
// stale branch, which no fake can show.
func TestIntegrationPruningPushConvergesRealRefs(t *testing.T) {
	root := t.TempDir()
	gh := filepath.Join(root, "github.git")
	fj := filepath.Join(root, "forgejo.git")
	work := filepath.Join(root, "work")
	ghURL, fjURL := "file://"+gh, "file://"+fj

	gitIn(t, root, "init", "--quiet", "--bare", gh)
	gitIn(t, root, "init", "--quiet", "--bare", fj)
	gitIn(t, root, "init", "--quiet", work)
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "-m", "base")
	base := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "tag", "v1.0.0", base)
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "-m", "github ahead")
	ahead := gitIn(t, work, "rev-parse", "HEAD")
	gitIn(t, work, "checkout", "--quiet", "-b", "forgejo-side", base)
	gitIn(t, work, "commit", "--quiet", "--allow-empty", "-m", "forgejo diverged")
	diverged := gitIn(t, work, "rev-parse", "HEAD")

	gitIn(t, work, "push", "--quiet", ghURL,
		ahead+":refs/heads/main", base+":refs/heads/feature-a", "refs/tags/v1.0.0:refs/tags/v1.0.0")
	gitIn(t, work, "push", "--quiet", fjURL,
		diverged+":refs/heads/main", diverged+":refs/heads/stale-branch",
		base+":refs/heads/feature-a", "refs/tags/v1.0.0:refs/tags/v1.0.0")
	if !slices.ContainsFunc(refsOf(t, root, fjURL), func(ref Ref) bool { return ref.Name == "refs/heads/stale-branch" }) {
		t.Fatal("setup: Forgejo does not hold the stale branch before the run")
	}

	events := []string{}
	fake := forgejotest.New()
	fake.AddRepo(liftRepo)
	fake.SeedRule(liftRepo, liftRule("main"))
	var logs []string
	resync := &Resync{
		Opts: &Options{
			Repo:       liftRepo,
			APIURL:     forgejotest.APIRoot + forgejotest.APIPath,
			MainBranch: "main",
			GitHubURL:  ghURL,
			ForgejoURL: fjURL,
			BackupDir:  filepath.Join(root, "backups"),
		},
		HTTP: &http.Client{Transport: &liftTransport{fake: fake, events: &events}},
		Getenv: func(name string) string {
			if name == restoreTokenEnv {
				return forgejotest.AdminToken
			}
			return ""
		},
		ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		Now:      func() time.Time { return liftNow },
		Log:      func(line string) { logs = append(logs, line) },
		MkdirTemp: func(_, pattern string) (string, error) {
			return os.MkdirTemp(root, pattern)
		},
		Provision: func(context.Context, *forgejoprov.Options, forgejoprov.Env) int {
			return forgejoprov.ExitOK
		},
	}
	if rc := resync.Run(); rc != ExitOK {
		t.Fatalf("Run = %d, want 0; logs %q", rc, logs)
	}

	got, want := refsOf(t, root, fjURL), refsOf(t, root, ghURL)
	if !slices.Equal(got, want) {
		t.Fatalf("Forgejo refs = %v, want GitHub's %v", got, want)
	}
	if !slices.Contains(logs, "refs match: Forgejo holds every GitHub ref (3 refs)") {
		t.Fatalf("logs %q do not report the match", logs)
	}
	backups, err := os.ReadDir(filepath.Join(root, "backups"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v (err %v), want the one lifted rule", backups, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read test root: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "forgejo-resync.") {
			t.Fatalf("scratch directory %s was left behind", e.Name())
		}
	}
}
