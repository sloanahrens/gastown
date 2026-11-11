package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A town whose directives path is a file cannot take the deploy: the directory
// it must create is refused, and the refusal names that directory.
func TestDeployCentralDirectivesRefusesADirectivesPathThatIsAFile(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
	}}}}
	town := t.TempDir()
	dir := CentralDirectiveDir(town)
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write the blocking file: %v", err)
	}

	_, err := deployFrom(town, commit, buildCommitOf(""), src, realDirFS())
	if err == nil || !strings.Contains(err.Error(), dir) {
		t.Fatalf("deployFrom error = %v, want one naming %s", err, dir)
	}
}

// A directive whose name is a directory cannot be replaced by the write, so the
// deploy stops there and says which file it was writing.
func TestDeployCentralDirectivesRefusesADirectiveThatIsADirectory(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
	}}}}
	town := t.TempDir()
	blocker := filepath.Join(CentralDirectiveDir(town), "a.md")
	if err := os.MkdirAll(blocker, 0o755); err != nil {
		t.Fatalf("create the blocking directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(blocker, "inside"), []byte("x"), 0o644); err != nil {
		t.Fatalf("fill the blocking directory: %v", err)
	}

	_, err := deployFrom(town, commit, buildCommitOf(""), src, realDirFS())
	if err == nil || !strings.Contains(err.Error(), "writing") || !strings.Contains(err.Error(), blocker) {
		t.Fatalf("deployFrom error = %v, want one saying it could not write %s", err, blocker)
	}
}

// A deployed directory that cannot be listed fails the deploy after the writes,
// and the error names the directory. The listing is injected to fail, so the
// unit tier needs no permission bits; the commit's file already holds its bytes,
// so nothing is written first.
func TestDeployCentralDirectivesReportsAnUnlistableDirectivesDir(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
	}}}}
	town := t.TempDir()
	dir := CentralDirectiveDir(town)
	seedDirectives(t, town, map[string]string{"a.md": "A"})
	fsys := realDirFS()
	fsys.readDir = func(string) ([]fs.DirEntry, error) { return nil, errors.New("injected listing failure") }

	written, err := deployFrom(town, commit, buildCommitOf(""), src, fsys)
	if err == nil || !strings.Contains(err.Error(), "reading "+dir) {
		t.Fatalf("deployFrom error = %v, want one saying it could not read %s", err, dir)
	}
	if len(written) != 0 {
		t.Errorf("written = %v, want nothing: a.md already held its bytes", baseNames(written))
	}
}

// A stale deployed *.md that cannot be removed fails the deploy and names it.
// The removal is injected to fail; the stale file is the one left behind.
func TestDeployCentralDirectivesReportsAStaleFileThatCannotBeRemoved(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
	}}}}
	town := t.TempDir()
	seedDirectives(t, town, map[string]string{"a.md": "A", "stale.md": "gone"})
	fsys := realDirFS()
	fsys.remove = func(string) error { return errors.New("injected removal failure") }

	_, err := deployFrom(town, commit, buildCommitOf(""), src, fsys)
	if err == nil || !strings.Contains(err.Error(), "stale.md") {
		t.Fatalf("deployFrom error = %v, want one naming stale.md", err)
	}
}

// The git-backed source asks git for the commit, its directives tree and each
// file; a scripted git stands in so the unit tier runs no git.
type scriptedGit struct {
	catFileErr error
	lsTree     []byte
	lsTreeErr  error
	blobs      map[string]string
}

func (g scriptedGit) run(_ string, args ...string) ([]byte, error) {
	switch args[0] {
	case "cat-file":
		if args[1] == "-e" {
			return nil, g.catFileErr
		}
		return []byte(g.blobs[args[2]]), nil
	case "ls-tree":
		return g.lsTree, g.lsTreeErr
	}
	return nil, fmt.Errorf("unexpected git %s", args[0])
}

// The git source keeps the blobs named *.md from the tree listing and marks the
// symlinks; a tree, a non-md blob and a malformed record are not directives.
func TestDeployCentralDirectivesGitSourceListsTheMarkdownBlobs(t *testing.T) {
	t.Parallel()
	g := scriptedGit{
		lsTree: []byte("100644 blob h1\ta.md\x00120000 blob h2\tlink.md\x00100644 blob h3\tnotes.txt\x00040000 tree h4\tsub.md\x00"),
		blobs:  map[string]string{"2222222:directives/a.md": "A"},
	}
	src := gitDirectiveSource{repo: "/x.git", run: g.run}

	entries, err := src.listDirectives("2222222")
	if err != nil {
		t.Fatalf("listDirectives: %v", err)
	}
	want := []directiveEntry{{name: "a.md"}, {name: "link.md", symlink: true}}
	if !reflect.DeepEqual(entries, want) {
		t.Errorf("listDirectives = %v, want %v", entries, want)
	}
	data, err := src.readDirective("2222222", "a.md")
	if err != nil || string(data) != "A" {
		t.Errorf("readDirective = %q, %v, want A", data, err)
	}
}

// Each git failure the source meets is an error that names the commit: a commit
// the repo lacks, a commit with no directives tree, and a tree listing that
// cannot be parsed.
func TestDeployCentralDirectivesGitSourceRefusesAFailingGit(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	tests := []struct {
		name string
		g    scriptedGit
		call func(gitDirectiveSource) error
	}{
		{
			name: "a commit the repo lacks",
			g:    scriptedGit{catFileErr: errors.New("exit status 128")},
			call: func(s gitDirectiveSource) error { return s.hasCommit(commit) },
		},
		{
			name: "a commit with no directives tree",
			g:    scriptedGit{lsTreeErr: errors.New("exit status 128")},
			call: func(s gitDirectiveSource) error { _, err := s.listDirectives(commit); return err },
		},
		{
			name: "a tree listing with a record that has no name",
			g:    scriptedGit{lsTree: []byte("100644 blob h1 a.md\x00")},
			call: func(s gitDirectiveSource) error { _, err := s.listDirectives(commit); return err },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := gitDirectiveSource{repo: "/x.git", run: tc.g.run}
			err := tc.call(src)
			if err == nil || !strings.Contains(err.Error(), commit) {
				t.Errorf("error = %v, want one naming commit %s", err, commit)
			}
		})
	}
}
