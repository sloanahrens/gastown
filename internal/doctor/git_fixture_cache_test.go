package doctor

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A git fixture costs one process per git command, and on the loaded host the
// merge gate runs on, a git exec costs 50-400 ms. Several helpers here build
// the same repos for every test that calls them: the same commits, the same
// branches, only the directory differs. cachedGitTree builds each such tree
// once per test binary and gives every caller a file copy of it, the pattern
// internal/cmd's cachedGitFixture and internal/git's fixtures use.
//
// What a copy has to fix up is the template's own absolute path, which git
// writes in a few places: a clone's remote URL (config), a linked worktree's
// gitdir and .git files, FETCH_HEAD and an alternates file. Commit and object
// ids do not depend on the path, so a SHA the template recorded is the copy's
// SHA too.
//
// A copy's commits carry the template's commit times. A check that judges a
// commit's age must not read one from a cached tree: build that repo per test.

// gitTreeTemplate is one built tree, shared by every caller of its key.
type gitTreeTemplate struct {
	once sync.Once
	dir  string
	err  error
}

var gitTreeTemplates sync.Map // key -> *gitTreeTemplate

// cachedGitTree fills dst (created if missing; existing entries are kept) with
// a copy of the tree build lays out in its dir. build runs once per key per
// test binary, in the first caller's goroutine, so it may fail through that
// caller's t, but it must not use t.TempDir or t.Cleanup: the template
// outlives the test that built it. A failed build fails every caller of the
// key.
//
// The key must name everything build depends on: two different builds under
// one key would hand out whichever ran first.
func cachedGitTree(t *testing.T, key, dst string, build func(dir string)) {
	t.Helper()
	v, _ := gitTreeTemplates.LoadOrStore(key, &gitTreeTemplate{})
	tmpl := v.(*gitTreeTemplate)
	tmpl.once.Do(func() {
		// Set before build so a build that exits the goroutine (t.Fatal in a
		// helper) still leaves every later caller an error, not an empty
		// template that looks built.
		tmpl.err = fmt.Errorf("git fixture %q: build did not complete", key)
		// Under the sandbox HOME, which the hermetic harness removes at exit.
		home, err := os.UserHomeDir()
		if err != nil {
			tmpl.err = err
			return
		}
		dir, err := os.MkdirTemp(home, "doctor-git-fixture-")
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
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyGitTree(tmpl.dir, dst); err != nil {
		t.Fatalf("copying git fixture %q: %v", key, err)
	}
}

// copyGitTree copies src into dst (which exists), keeping file modes and
// symlinks, and rewrites src's absolute path to dst in the git metadata files
// that record one.
func copyGitTree(src, dst string) error {
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
		if gitFileRecordsPath(path) {
			data = bytes.ReplaceAll(data, oldRoot, newRoot)
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// gitFileRecordsPath reports whether path is a git metadata file that can hold
// an absolute path: config (remote URLs, core.worktree), a worktree's
// gitdir/commondir, a linked checkout's .git file, FETCH_HEAD, and the object
// store's alternates. Working-tree content is copied as is.
func gitFileRecordsPath(path string) bool {
	switch filepath.Base(path) {
	case "config", "gitdir", "commondir", ".git", "FETCH_HEAD", "alternates":
		return true
	}
	return false
}

// TestCachedGitTreeCopiesAreIndependent pins the copy's one job beyond copying
// bytes: a clone's origin must be the copy's own bare repo. A copy still
// pointing at the template would push into state every later caller starts
// from, and the tests sharing it would pass or fail by run order.
func TestCachedGitTreeCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	build := func(dir string) {
		origin := filepath.Join(dir, "origin.git")
		work := filepath.Join(dir, "work")
		runGit(t, "", "init", "--bare", "-q", origin)
		runGit(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
		runGit(t, "", "clone", "-q", origin, work)
		runGit(t, work, "checkout", "-q", "-b", "main")
		writeFile(t, filepath.Join(work, "a.txt"), "a\n")
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-q", "-m", "base")
		runGit(t, work, "push", "-q", "origin", "main")
	}
	key := "copy self-test " + t.Name()
	first, second := t.TempDir(), t.TempDir()
	cachedGitTree(t, key, first, build)
	cachedGitTree(t, key, second, build)
	base := runGit(t, filepath.Join(first, "work"), "rev-parse", "HEAD")
	if got := runGit(t, filepath.Join(second, "work"), "rev-parse", "origin/main"); got != base {
		t.Fatalf("copies disagree on the template's commit: %s vs %s", got, base)
	}

	work := filepath.Join(first, "work")
	writeFile(t, filepath.Join(work, "b.txt"), "b\n")
	runGit(t, work, "add", "-A")
	runGit(t, work, "commit", "-q", "-m", "only in the first copy")
	runGit(t, work, "push", "-q", "origin", "main")
	pushed := runGit(t, work, "rev-parse", "HEAD")
	if got := runGit(t, filepath.Join(first, "origin.git"), "rev-parse", "main"); got != pushed {
		t.Fatalf("the first copy pushed to a repo other than its own origin.git: its main = %s, want %s", got, pushed)
	}
	runGit(t, filepath.Join(second, "work"), "fetch", "-q", "origin")
	if got := runGit(t, filepath.Join(second, "work"), "rev-parse", "origin/main"); got != base {
		t.Fatalf("the second copy's origin saw the first copy's push: origin/main = %s, want %s", got, base)
	}
}
