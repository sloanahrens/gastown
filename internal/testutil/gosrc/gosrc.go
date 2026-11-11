// Package gosrc is the shared read of the checkout's Go source for the tests that
// scan it. Several tests in one package each walk the same directories, read
// every file and parse it; on a runner with a cold file cache and few cores that
// repeated work was most of the package's wall time. A Tree reads each file once
// and parses it once per mode, and every scan after the first is served from
// memory.
//
// The checkout does not change while the tests run, so a Tree is a snapshot taken
// lazily. A scan must not edit what Parse returns. A test that builds its own
// fixture tree uses the standard library, not this package.
//
// The package imports nothing from the module, so any package's tests can use
// it, including the ones testutil itself imports.
package gosrc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Tree is a lazily read, shared view of the Go files under one root.
type Tree struct {
	root string
	fset *token.FileSet

	mu    sync.Mutex
	files map[string]*file
	dirs  map[string]*dirList
}

type file struct {
	once sync.Once
	src  []byte
	err  error

	mu    sync.Mutex
	trees map[parser.Mode]*parsed
}

type parsed struct {
	once sync.Once
	ast  *ast.File
	err  error
}

type dirList struct {
	once    sync.Once
	entries []fs.DirEntry
	err     error
}

var (
	mu    sync.Mutex
	trees = map[string]*Tree{}
)

// For returns the shared Tree of root. Two calls with the same root return the
// same Tree.
func For(root string) *Tree {
	root = filepath.Clean(root)
	mu.Lock()
	defer mu.Unlock()
	t := trees[root]
	if t == nil {
		t = &Tree{root: root, fset: token.NewFileSet(), files: map[string]*file{}, dirs: map[string]*dirList{}}
		trees[root] = t
	}
	return t
}

// Root is the directory the Tree was made for.
func (t *Tree) Root() string { return t.root }

// FileSet is the position table of every file Parse returned.
func (t *Tree) FileSet() *token.FileSet { return t.fset }

func (t *Tree) file(path string) *file {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.files[path]
	if f == nil {
		f = &file{trees: map[parser.Mode]*parsed{}}
		t.files[path] = f
	}
	return f
}

// ReadDir lists dir once. The slice is shared: do not edit it.
func (t *Tree) ReadDir(dir string) ([]fs.DirEntry, error) {
	t.mu.Lock()
	d := t.dirs[dir]
	if d == nil {
		d = &dirList{}
		t.dirs[dir] = d
	}
	t.mu.Unlock()
	d.once.Do(func() { d.entries, d.err = os.ReadDir(dir) })
	return d.entries, d.err
}

// Source returns the bytes of path, read once. The slice is shared: do not edit it.
func (t *Tree) Source(path string) ([]byte, error) {
	f := t.file(path)
	f.once.Do(func() { f.src, f.err = os.ReadFile(path) }) //nolint:gosec // G304: a path the caller names under the checkout
	return f.src, f.err
}

// Parse returns the syntax tree of path in mode, parsed once per mode from the
// bytes Source returns. The tree is shared: do not edit it.
func (t *Tree) Parse(path string, mode parser.Mode) (*ast.File, error) {
	src, err := t.Source(path)
	if err != nil {
		return nil, err
	}
	f := t.file(path)
	f.mu.Lock()
	p := f.trees[mode]
	if p == nil {
		p = &parsed{}
		f.trees[mode] = p
	}
	f.mu.Unlock()
	p.once.Do(func() { p.ast, p.err = parser.ParseFile(t.fset, path, src, mode) })
	return p.ast, p.err
}

// Walk is filepath.WalkDir over root, with each directory's listing read once
// for the Tree. It follows no symlinks, as WalkDir does.
func (t *Tree) Walk(start string, fn fs.WalkDirFunc) error {
	info, err := os.Lstat(start)
	if err != nil {
		return fn(start, nil, err)
	}
	return t.walk(start, fs.FileInfoToDirEntry(info), fn)
}

func (t *Tree) walk(path string, d fs.DirEntry, fn fs.WalkDirFunc) error {
	if err := fn(path, d, nil); err != nil || !d.IsDir() {
		if err == fs.SkipDir && d.IsDir() {
			err = nil
		}
		return err
	}
	entries, err := t.ReadDir(path)
	if err != nil {
		if err = fn(path, d, err); err != nil {
			if err == fs.SkipDir {
				err = nil
			}
			return err
		}
	}
	for _, e := range entries {
		if err := t.walk(filepath.Join(path, e.Name()), e, fn); err != nil {
			if err == fs.SkipDir {
				break
			}
			return err
		}
	}
	return nil
}
