//go:build integration

package cmd

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationCloneFixtureTreeMatchesCopy pins the clonefile path (gt-22hdp.58)
// against the file-by-file copy it replaces on darwin: a clone rewrites only
// the template's recorded path files, so a record it missed would leave a
// fixture pointing into the shared template, and every test using that key
// would share state. The two paths must give the same tree up to the root,
// the path-bearing git files must name the clone, and the template must come
// out byte for byte unchanged.
func TestIntegrationCloneFixtureTreeMatchesCopy(t *testing.T) {
	t.Parallel()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(home, "cmd-clone-fixture-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })

	// Every kind of path record git writes: a clone's remote URL (config),
	// a linked worktree's gitdir and .git file, an alternates file, and a
	// symlink into the tree.
	origin := filepath.Join(root, "origin.git")
	work := filepath.Join(root, "work")
	runGitCmd(t, "", "init", "--bare", origin)
	runGitCmd(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	runGitCmd(t, "", "clone", origin, work)
	runGitCmd(t, work, "config", "user.email", "fixture@example.com")
	runGitCmd(t, work, "config", "user.name", "Fixture")
	writeTestFile(t, filepath.Join(work, "a.txt"), "a\n")
	runGitCmd(t, work, "add", "-A")
	runGitCmd(t, work, "commit", "-m", "base")
	runGitCmd(t, work, "push", "origin", "main")
	runGitCmd(t, work, "worktree", "add", "-b", "side", filepath.Join(root, "side"))
	runGitCmd(t, "", "clone", "--shared", origin, filepath.Join(root, "shared"))
	if err := os.Symlink(filepath.Join(root, "work", "a.txt"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	records, err := fixturePathRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &fixtureTemplate{dir: root, pathRecords: records}
	before := snapshotTree(t, root)

	cloneParent := t.TempDir()
	clone, err := cloneFixtureTree(tmpl, cloneParent)
	if err != nil {
		t.Fatal(err)
	}
	// A filesystem without clonefile falls back to the copy (clone ==
	// cloneParent); the comparison below still holds, it just proves less.
	copyRoot := t.TempDir()
	if err := copyFixtureTree(root, copyRoot); err != nil {
		t.Fatal(err)
	}

	if after := snapshotTree(t, root); !equalSnapshots(before, after) {
		t.Fatalf("cloning changed the template:\nbefore %v\nafter  %v", before, after)
	}

	got := normalizedSnapshot(t, clone)
	want := normalizedSnapshot(t, copyRoot)
	if !equalSnapshots(got, want) {
		for rel, w := range want {
			if g, ok := got[rel]; !ok || g != w {
				t.Errorf("%s: clone has %q, copy has %q", rel, g, w)
			}
		}
		for rel := range got {
			if _, ok := want[rel]; !ok {
				t.Errorf("%s: only in the clone", rel)
			}
		}
	}

	// Named independently of fixtureFileRecordsPath, so a file that list
	// forgets still fails here.
	for _, rel := range []string{
		"work/.git/config",
		"work/.git/worktrees/side/gitdir",
		"side/.git",
		"shared/.git/objects/info/alternates",
		"link",
	} {
		path := filepath.Join(clone, rel)
		var data []byte
		if link, err := os.Readlink(path); err == nil {
			data = []byte(link)
		} else if data, err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(root)) {
			t.Errorf("%s still names the template: %q", rel, data)
		}
		if !bytes.Contains(data, []byte(clone)) {
			t.Errorf("%s does not name the clone: %q", rel, data)
		}
	}
	out, err := exec.Command("git", "-C", filepath.Join(clone, "side"), "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatalf("git rev-parse in the cloned worktree: %v", err)
	}
	// git answers with the resolved path (/var is a symlink on macOS).
	realClone, err := filepath.EvalSymlinks(clone)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); !strings.HasPrefix(got, realClone) {
		t.Errorf("the cloned worktree's common dir is %s, want one under %s", got, realClone)
	}
}

// snapshotTree maps every file and symlink under root, relative to it, to its
// contents or link target.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			snap[rel] = "-> " + link
			return err
		}
		data, err := os.ReadFile(path)
		snap[rel] = string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

// normalizedSnapshot is snapshotTree with root's path replaced by a fixed
// token, so two copies of one tree compare equal.
func normalizedSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := snapshotTree(t, root)
	for rel, v := range snap {
		snap[rel] = strings.ReplaceAll(v, root, "<root>")
	}
	return snap
}

func equalSnapshots(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}
