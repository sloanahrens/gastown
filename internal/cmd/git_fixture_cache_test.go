package cmd

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

// A git fixture costs this package one process per git command, and on the
// loaded host the merge gate runs on a git exec costs 50-400 ms. Many helpers
// build the same repos for every test that calls them: the same commits, the
// same branches, only the directory differs. cachedGitFixture builds each such
// tree once per test binary and gives every caller a file copy of it, the
// pattern refinery's testGitRepo (86889ec) and internal/git's fixtures use.
//
// What a copy has to fix up is the template's own absolute path, which git
// writes in a few places: a clone's remote URL (config), a linked worktree's
// gitdir and .git files, and an alternates file. Commit and object ids do not
// depend on the path, so a SHA the template recorded is the copy's SHA too.

// fixtureTemplate is one built fixture, shared by every caller of its key.
type fixtureTemplate struct {
	once sync.Once
	dir  string // the template's root, under the sandbox home
	meta any    // what the build returned beside the tree, paths relative to dir
	err  error
	// pathRecords lists the files and symlinks under dir, relative to it,
	// that hold dir's absolute path: what a clone of the tree has to rewrite.
	pathRecords []string
}

var fixtureTemplates sync.Map // key -> *fixtureTemplate

// cachedGitFixture returns a fresh copy of the tree build makes in its dir,
// and the value build returned with every template path in its strings
// rewritten to the copy. build runs once per key per test binary, in the
// first caller's goroutine, so it may use that caller's t to run git and
// report (runGitCmd and friends) but never for t.TempDir or t.Cleanup: the
// template outlives the test that built it. A failed build fails every
// caller of the key.
//
// The key must name everything build depends on: two different builds under
// one key would hand out whichever ran first.
func cachedGitFixture[M any](t *testing.T, key string, build func(dir string) (M, error)) (string, M) {
	t.Helper()
	v, _ := fixtureTemplates.LoadOrStore(key, &fixtureTemplate{})
	tmpl := v.(*fixtureTemplate)
	tmpl.once.Do(func() {
		// Set before build so a build that exits the goroutine (t.Fatal in
		// a helper) still leaves every later caller an error, not an empty
		// template that looks built.
		tmpl.err = fmt.Errorf("git fixture %q: build did not complete", key)
		home, err := os.UserHomeDir()
		if err != nil {
			tmpl.err = err
			return
		}
		dir, err := os.MkdirTemp(home, "cmd-git-fixture-")
		if err != nil {
			tmpl.err = err
			return
		}
		m, err := build(dir)
		if err != nil {
			tmpl.err = fmt.Errorf("git fixture %q: %w", key, err)
			return
		}
		records, err := fixturePathRecords(dir)
		if err != nil {
			tmpl.err = fmt.Errorf("git fixture %q: %w", key, err)
			return
		}
		tmpl.dir, tmpl.meta, tmpl.pathRecords, tmpl.err = dir, m, records, nil
	})
	if tmpl.err != nil {
		t.Fatal(tmpl.err)
	}
	dst, err := cloneFixtureTree(tmpl, t.TempDir())
	if err != nil {
		t.Fatalf("copying git fixture %q: %v", key, err)
	}
	return dst, rewriteFixtureMeta(tmpl.meta, tmpl.dir, dst).(M)
}

// cloneFixtureTree gives the caller its own copy of tmpl's tree inside
// parent and returns the copy's root. Where the filesystem can clone a whole
// tree in one call (APFS clonefile), it does that and rewrites only the
// template's recorded path files; the template's files and directories then
// cost this package one syscall instead of an open, read and write each. The
// file-by-file copy, into parent itself, is the fallback everywhere else.
func cloneFixtureTree(tmpl *fixtureTemplate, parent string) (string, error) {
	dst := filepath.Join(parent, "fixture")
	cloned, err := cloneTree(tmpl.dir, dst)
	if err != nil {
		return "", err
	}
	if !cloned {
		if err := copyFixtureTree(tmpl.dir, parent); err != nil {
			return "", err
		}
		return parent, nil
	}
	oldRoot, newRoot := []byte(tmpl.dir), []byte(dst)
	for _, rel := range tmpl.pathRecords {
		path := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return "", err
			}
			if err := os.Remove(path); err != nil {
				return "", err
			}
			if err := os.Symlink(string(bytes.ReplaceAll([]byte(link), oldRoot, newRoot)), path); err != nil {
				return "", err
			}
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		// Write through a fresh file: the clone shares its blocks with the
		// template until written, and a rename keeps the template's copy
		// untouched even if the write fails part way.
		tmp := path + ".fixture-rewrite"
		if err := os.WriteFile(tmp, bytes.ReplaceAll(data, oldRoot, newRoot), info.Mode().Perm()); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, path); err != nil {
			return "", err
		}
	}
	return dst, nil
}

// fixturePathRecords lists, relative to root, the symlinks and the git
// metadata files (fixtureFileRecordsPath) under root that contain root's
// absolute path.
func fixturePathRecords(root string) ([]string, error) {
	var records []string
	needle := []byte(root)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if strings.Contains(link, root) {
				records = append(records, rel)
			}
			return nil
		}
		if !fixtureFileRecordsPath(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, needle) {
			records = append(records, rel)
		}
		return nil
	})
	return records, err
}

// copyFixtureTree copies src into dst (which exists), keeping file modes and
// symlinks, and rewrites src's absolute path to dst in the git metadata files
// that record one.
func copyFixtureTree(src, dst string) error {
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
		if fixtureFileRecordsPath(path) {
			data = bytes.ReplaceAll(data, oldRoot, newRoot)
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
}

// fixtureFileRecordsPath reports whether path is a git metadata file that can
// hold an absolute path: config (remote URLs, core.worktree), a worktree's
// gitdir/commondir, a linked checkout's .git file, FETCH_HEAD, and the object
// store's alternates. Working-tree content is copied as is.
func fixtureFileRecordsPath(path string) bool {
	switch filepath.Base(path) {
	case "config", "gitdir", "commondir", ".git", "FETCH_HEAD", "alternates":
		return true
	}
	return false
}

// rewriteFixtureMeta returns m with oldRoot replaced by newRoot in every
// string it holds: a string, a []string, or a type that rewrites itself
// (fixtureMetaRewriter). Other types are returned unchanged, so a fixture
// whose metadata holds paths must use one of those.
func rewriteFixtureMeta(m any, oldRoot, newRoot string) any {
	switch v := m.(type) {
	case string:
		return strings.ReplaceAll(v, oldRoot, newRoot)
	case []string:
		out := make([]string, len(v))
		for i, x := range v {
			out[i] = strings.ReplaceAll(x, oldRoot, newRoot)
		}
		return out
	case fixtureMetaRewriter:
		return v.rewriteRoot(oldRoot, newRoot)
	}
	return m
}

// fixtureMetaRewriter is implemented by a fixture's metadata struct so its
// path fields follow the copy.
type fixtureMetaRewriter interface {
	rewriteRoot(oldRoot, newRoot string) any
}

// cachedGitFixtureStrings is cachedGitFixture for the common helper shape: a
// build that lays its repos out under dir and returns their paths (and any
// other strings, such as a branch or a SHA). The copy's paths come back in
// the same order.
func cachedGitFixtureStrings(t *testing.T, key string, build func(dir string) []string) []string {
	t.Helper()
	_, out := cachedGitFixture(t, key, func(dir string) ([]string, error) {
		return build(dir), nil
	})
	return out
}

// TestCachedGitFixtureCopiesAreIndependent pins the copy's one job beyond
// copying bytes: a clone's origin must be the copy's own bare repo. A copy
// still pointing at the template would push into state every later caller
// starts from, and the tests sharing it would pass or fail by run order.
func TestCachedGitFixtureCopiesAreIndependent(t *testing.T) {
	t.Parallel()
	build := func(dir string) []string {
		origin := filepath.Join(dir, "origin.git")
		work := filepath.Join(dir, "work")
		runGitCmd(t, "", "init", "--bare", origin)
		runGitCmd(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
		runGitCmd(t, "", "clone", origin, work)
		runGitCmd(t, work, "config", "user.email", "fixture@example.com")
		runGitCmd(t, work, "config", "user.name", "Fixture")
		writeTestFile(t, filepath.Join(work, "a.txt"), "a\n")
		runGitCmd(t, work, "add", "-A")
		runGitCmd(t, work, "commit", "-m", "base")
		runGitCmd(t, work, "push", "origin", "main")
		return []string{work, revParse(t, work, "HEAD")}
	}
	key := "fixture-cache-self-test " + t.Name()
	first := cachedGitFixtureStrings(t, key, build)
	second := cachedGitFixtureStrings(t, key, build)
	if first[0] == second[0] {
		t.Fatalf("two callers got the same directory %s", first[0])
	}
	if first[1] != second[1] || first[1] != revParse(t, first[0], "origin/main") {
		t.Fatalf("copies disagree on the template's commit: %v, %v", first, second)
	}

	writeTestFile(t, filepath.Join(first[0], "b.txt"), "b\n")
	runGitCmd(t, first[0], "add", "-A")
	runGitCmd(t, first[0], "commit", "-m", "only in the first copy")
	runGitCmd(t, first[0], "push", "origin", "main")

	pushed := revParse(t, first[0], "HEAD")
	if got := revParse(t, filepath.Join(filepath.Dir(first[0]), "origin.git"), "main"); got != pushed {
		t.Fatalf("the first copy pushed to a repo other than its own origin.git: its origin main = %s, want %s", got, pushed)
	}
	runGitCmd(t, second[0], "fetch", "origin")
	if got := revParse(t, second[0], "origin/main"); got != second[1] {
		t.Fatalf("the second copy's origin saw the first copy's push: origin/main = %s, want %s", got, second[1])
	}
}
