package polecat

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every git command a fixture runs is a process, and on the loaded host the
// merge gate runs on, a process start costs 50-400 ms of mostly system time.
// Many helpers here build the same repos for every test that calls them: the
// same commits and branches, only the directory differs. cachedGitFixture
// builds each such tree once per test binary and hands every caller a file
// copy of it (the pattern of internal/cmd's cachedGitFixture and refinery's
// testGitRepo).
//
// A copy has to fix up the template's own absolute path wherever git wrote
// it: a clone's remote URL and core settings (config), a linked worktree's
// gitdir and .git files, FETCH_HEAD and alternates. Object ids do not depend
// on the path, so a SHA recorded in the template is the copy's SHA too.
//
// Commit times are the template's: a test that needs a commit of a given age
// makes that commit itself. No polecat code reads a commit's date.

type gitFixtureTemplate struct {
	once sync.Once
	dir  string
	err  error
}

var gitFixtureTemplates sync.Map // key -> *gitFixtureTemplate

// cachedGitFixture returns a fresh copy (a new t.TempDir) of the tree build
// lays out under its dir. build runs once per key per test binary, in the
// first caller's goroutine: it may run git through that caller's t, but must
// not use t.TempDir or t.Cleanup, because the template outlives that test. A
// failed build fails every caller of the key. The key must name everything
// build depends on.
func cachedGitFixture(t *testing.T, key string, build func(dir string)) string {
	t.Helper()
	v, _ := gitFixtureTemplates.LoadOrStore(key, &gitFixtureTemplate{})
	tmpl := v.(*gitFixtureTemplate)
	tmpl.once.Do(func() {
		// Set first, so a build that exits its goroutine (t.Fatal) leaves
		// every later caller an error rather than an empty template.
		tmpl.err = fmt.Errorf("git fixture %q: build did not complete", key)
		home, err := os.UserHomeDir()
		if err != nil {
			tmpl.err = err
			return
		}
		dir, err := os.MkdirTemp(home, "polecat-git-fixture-")
		if err != nil {
			tmpl.err = err
			return
		}
		build(dir)
		tmpl.dir, tmpl.err = dir, nil
	})
	if tmpl.err != nil {
		t.Fatal(tmpl.err)
	}
	dst := t.TempDir()
	if err := copyGitFixture(tmpl.dir, dst); err != nil {
		t.Fatalf("copying git fixture %q: %v", key, err)
	}
	return dst
}

// copyGitFixture copies src into the existing dst, keeping modes and
// symlinks, and rewrites src's path to dst in the git files that record one.
func copyGitFixture(src, dst string) error {
	oldRoot, newRoot := []byte(src), []byte(dst)
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			if rel == "." {
				return nil
			}
			return os.Mkdir(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(string(bytes.ReplaceAll([]byte(link), oldRoot, newRoot)), target)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s: not a regular file, directory or symlink", path)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		switch filepath.Base(path) {
		case "config", "gitdir", "commondir", ".git", "FETCH_HEAD", "alternates":
			data = bytes.ReplaceAll(data, oldRoot, newRoot)
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// TestCachedGitFixtureCopiesAreIndependent pins what a copy must be beyond
// its bytes: its clone's origin is the copy's own bare repo, so a push from
// one copy is invisible to the template and to every other copy.
func TestCachedGitFixtureCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	build := func(dir string) {
		origin := filepath.Join(dir, "origin.git")
		work := filepath.Join(dir, "work")
		runGit(t, dir, "init", "--bare", "--initial-branch=main", origin)
		runGit(t, dir, "clone", origin, work)
		runGit(t, work, "config", "user.email", "fixture@example.com")
		runGit(t, work, "config", "user.name", "Fixture")
		seedFile(t, filepath.Join(work, "a.txt"), "a\n")
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-m", "base")
		runGit(t, work, "push", "origin", "main")
	}
	key := "self-test " + t.Name()
	first := cachedGitFixture(t, key, build)
	second := cachedGitFixture(t, key, build)
	if first == second {
		t.Fatalf("two callers got the same directory %s", first)
	}
	if url := gitProbeOutput(t, filepath.Join(first, "work"), "remote", "get-url", "origin"); url != filepath.Join(first, "origin.git") {
		t.Fatalf("copy's origin = %q, want its own %q", url, filepath.Join(first, "origin.git"))
	}

	work := filepath.Join(first, "work")
	seedFile(t, filepath.Join(work, "b.txt"), "b\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-m", "only in the first copy")
	runGit(t, work, "push", "origin", "main")
	if a, b := gitProbeOutput(t, filepath.Join(first, "origin.git"), "rev-parse", "main"),
		gitProbeOutput(t, filepath.Join(second, "origin.git"), "rev-parse", "main"); a == b {
		t.Fatalf("a push to the first copy's origin moved the second copy's origin to %s", b)
	}
}

// TestCanonicalWithPolecatsCopyHoldsNoTemplatePath pins that a copied rig
// with worktrees names only itself: a linked worktree records absolute paths
// (its .git file, the gitdir under .git/worktrees), and a copy still pointing
// at the template would let one test's git writes reach every later test's
// starting state.
func TestCanonicalWithPolecatsCopyHoldsNoTemplatePath(t *testing.T) {
	t.Parallel()
	mgr, _, _, added := setupCanonicalWithPolecats(t, false, "alpha")
	town := filepath.Dir(mgr.rig.Path)
	data, err := os.ReadFile(filepath.Join(town, "polecats.json"))
	if err != nil {
		t.Fatal(err)
	}
	tmplRoot, _, _ := bytes.Cut(data, []byte("\n"))
	if len(tmplRoot) == 0 || bytes.Equal(tmplRoot, []byte(town)) {
		t.Fatalf("template root %q unusable for this check (copy at %s)", tmplRoot, town)
	}
	err = filepath.WalkDir(town, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Base(path) == "polecats.json" {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, tmplRoot) {
			t.Errorf("%s still names the template %s", path, tmplRoot)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rigReal, err := filepath.EvalSymlinks(mgr.rig.Path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(gitProbeOutput(t, added["alpha"].ClonePath, "rev-parse", "--git-common-dir"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, rigReal+string(filepath.Separator)) {
		t.Errorf("alpha's common git dir = %q, want one inside the copied rig %s", got, rigReal)
	}
}
