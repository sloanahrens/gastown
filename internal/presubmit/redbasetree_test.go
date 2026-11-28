package presubmit

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// treeFiles answers RedBaseTree's file calls from memory and records them, so the unit tier writes no file.
type treeFiles struct {
	content  map[string][]byte
	readErr  error
	mkdirErr error
	writeErr error
	dirs     []string
	writes   map[string][]byte
}

func (f *treeFiles) read(p string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	return f.content[p], nil
}

func (f *treeFiles) mkdir(dir string) error {
	f.dirs = append(f.dirs, dir)
	return f.mkdirErr
}

func (f *treeFiles) write(p string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.writes == nil {
		f.writes = map[string][]byte{}
	}
	f.writes[p] = append([]byte(nil), data...)
	return nil
}

// worktreeRemoveCalls counts the git worktree remove calls a stub git recorded.
func worktreeRemoveCalls(g *worktreeGit) int {
	n := 0
	for _, c := range g.calls {
		if len(c) == 5 && c[0] == "worktree" && c[1] == "remove" {
			n++
		}
	}
	return n
}

// TestRedBaseTree: each named file's HEAD content is written into the tree under its own path, its parent directory is made, and the returned removal removes the tree (gt-mc0uv9).
func TestRedBaseTree(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tree := filepath.Join(dir, "red-base")
	g := &worktreeGit{mergeBase: "abc123"}
	files := []string{"internal/a/a_test.go", "internal/b/nested/c_test.go", "root_test.go"}
	f := &treeFiles{content: map[string][]byte{
		"internal/a/a_test.go":        []byte("package a\n"),
		"internal/b/nested/c_test.go": []byte("package nested\n"),
		"root_test.go":                []byte("package root\n"),
	}}

	got, remove, err := RedBaseTree(g.run, f.read, f.mkdir, f.write, "main", dir, files)
	if err != nil {
		t.Fatalf("RedBaseTree: %v", err)
	}
	if got != tree {
		t.Errorf("tree = %q, want %q", got, tree)
	}
	for _, p := range files {
		want := filepath.Join(tree, filepath.FromSlash(p))
		if string(f.writes[want]) != string(f.content[p]) {
			t.Errorf("wrote %q to %q, want %q", string(f.writes[want]), want, string(f.content[p]))
		}
		if parent := filepath.Dir(want); !slices.Contains(f.dirs, parent) {
			t.Errorf("dirs = %v, want a make of %q", f.dirs, parent)
		}
	}
	if err := remove(); err != nil {
		t.Fatalf("remove: %v", err)
	}
	wantRemove := []string{"worktree", "remove", "--force", "--end-of-options", tree}
	if last := g.calls[len(g.calls)-1]; !slices.Equal(last, wantRemove) {
		t.Errorf("last call = %v, want %v", last, wantRemove)
	}
}

// TestRedBaseTreeRefusals holds one row per refusal: each returns an error with no tree and no removal, removing the worktree exactly when it existed (gt-mc0uv9).
func TestRedBaseTreeRefusals(t *testing.T) {
	t.Parallel()
	readErr := errors.New("no head content")
	mkdirErr := errors.New("read-only tree")
	writeErr := errors.New("disk full")
	tests := []struct {
		name       string
		base       string
		files      []string
		git        worktreeGit
		readErr    error
		mkdirErr   error
		writeErr   error
		wantErr    string
		wantRemove int
	}{
		{"a failed worktree is refused", "", []string{"a_test.go"}, worktreeGit{}, nil, nil, nil, "empty base", 0},
		{"an empty file path is refused", "main", []string{""}, worktreeGit{mergeBase: "abc"}, nil, nil, nil, "empty file path", 1},
		{"an absolute file path is refused", "main", []string{"/etc/passwd"}, worktreeGit{mergeBase: "abc"}, nil, nil, nil, "is absolute", 1},
		{"a file path that leaves the tree is refused", "main", []string{"../a_test.go"}, worktreeGit{mergeBase: "abc"}, nil, nil, nil, "leaves the tree", 1},
		{"a file path that cleans to the tree root is refused", "main", []string{"a/.."}, worktreeGit{mergeBase: "abc"}, nil, nil, nil, "leaves the tree", 1},
		{"a failed read is refused", "main", []string{"a_test.go"}, worktreeGit{mergeBase: "abc"}, readErr, nil, nil, "reading a_test.go", 1},
		{"a failed parent-directory make is refused", "main", []string{"internal/a_test.go"}, worktreeGit{mergeBase: "abc"}, nil, mkdirErr, nil, "making", 1},
		{"a failed write is refused", "main", []string{"a_test.go"}, worktreeGit{mergeBase: "abc"}, nil, nil, writeErr, "writing", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := tt.git
			f := &treeFiles{readErr: tt.readErr, mkdirErr: tt.mkdirErr, writeErr: tt.writeErr}
			tree, remove, err := RedBaseTree(g.run, f.read, f.mkdir, f.write, tt.base, t.TempDir(), tt.files)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one naming %q", err, tt.wantErr)
			}
			if tree != "" {
				t.Errorf("tree = %q, want empty", tree)
			}
			if remove != nil {
				t.Errorf("remove is non-nil, want nil on refusal")
			}
			if got := worktreeRemoveCalls(&g); got != tt.wantRemove {
				t.Errorf("worktree remove calls = %d, want %d", got, tt.wantRemove)
			}
		})
	}
}

// TestRedBaseTreeRemovalFailureJoined: when a copy fails and the worktree removal fails too, the returned error carries both (gt-mc0uv9).
func TestRedBaseTreeRemovalFailureJoined(t *testing.T) {
	t.Parallel()
	readErr := errors.New("no head content")
	rmErr := errors.New("worktree locked")
	g := &worktreeGit{mergeBase: "abc", removeErr: rmErr}
	f := &treeFiles{readErr: readErr}

	tree, remove, err := RedBaseTree(g.run, f.read, f.mkdir, f.write, "main", t.TempDir(), []string{"a_test.go"})
	if err == nil {
		t.Fatal("error = nil, want the read and removal errors")
	}
	if !errors.Is(err, readErr) || !errors.Is(err, rmErr) {
		t.Errorf("error = %v, want one joining %v and %v", err, readErr, rmErr)
	}
	if tree != "" || remove != nil {
		t.Errorf("tree = %q, remove nil = %t, want empty and nil", tree, remove == nil)
	}
}
