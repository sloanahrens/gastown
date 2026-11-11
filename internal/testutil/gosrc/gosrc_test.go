package gosrc

import (
	"errors"
	"go/parser"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func writeTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"a.go":          "package a\n",
		"sub/b.go":      "package b\n\nfunc F() {}\n",
		"sub/skip/c.go": "package c\n",
	} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// TestForSharesOneTreePerRoot: two calls for a root return the same tree, and a
// file is read and parsed once per mode.
func TestForSharesOneTreePerRoot(t *testing.T) {
	t.Parallel()
	root := writeTree(t)
	a, b := For(root), For(root+string(filepath.Separator))
	if a != b {
		t.Fatalf("For returned two trees for one root")
	}
	p := filepath.Join(root, "sub", "b.go")
	first, err := a.Parse(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := b.Parse(p, 0)
	if first != second {
		t.Errorf("one file in one mode was parsed twice")
	}
	imports, err := a.Parse(p, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if imports == first {
		t.Errorf("two modes shared one tree")
	}
	if len(first.Decls) != 1 || len(imports.Decls) != 0 {
		t.Errorf("decls full = %d, imports-only = %d; want 1, 0", len(first.Decls), len(imports.Decls))
	}
}

// TestParseReportsAFileItCannotRead: an unreadable path is an error, not an empty tree.
func TestParseReportsAFileItCannotRead(t *testing.T) {
	t.Parallel()
	tr := For(t.TempDir())
	if _, err := tr.Parse(filepath.Join(tr.Root(), "missing.go"), 0); err == nil {
		t.Errorf("a missing file parsed without an error")
	}
	if _, err := tr.Source(filepath.Join(tr.Root(), "missing.go")); err == nil {
		t.Errorf("a missing file read without an error")
	}
}

// TestWalkVisitsWhatWalkDirVisits: the tree's walk yields the same paths in the same
// order as filepath.WalkDir, honours SkipDir, and passes a listing error to fn.
func TestWalkVisitsWhatWalkDirVisits(t *testing.T) {
	t.Parallel()
	root := writeTree(t)
	var want, got []string
	skip := func(path string, d fs.DirEntry) error {
		if d.IsDir() && d.Name() == "skip" {
			return fs.SkipDir
		}
		return nil
	}
	if err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		want = append(want, p)
		return skip(p, d)
	}); err != nil {
		t.Fatal(err)
	}
	if err := For(root).Walk(root, func(p string, d fs.DirEntry, err error) error {
		got = append(got, p)
		return skip(p, d)
	}); err != nil {
		t.Fatal(err)
	}
	if !sort.StringsAreSorted(got) && len(got) != len(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	if len(got) != len(want) {
		t.Fatalf("walk = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("walk[%d] = %s, want %s", i, got[i], want[i])
		}
	}
	var sawErr error
	missing := filepath.Join(root, "nope")
	_ = For(root).Walk(missing, func(p string, d fs.DirEntry, err error) error {
		sawErr = err
		return err
	})
	if !errors.Is(sawErr, fs.ErrNotExist) {
		t.Errorf("a missing start passed %v to fn, want not-exist", sawErr)
	}
}

// TestWalkPassesAListingErrorToFnAndStopsOnItsError: a directory that cannot be
// listed is handed to fn; fn's error ends the walk, fn's nil lets it go on, and an
// error from a visited entry ends the walk with that error.
func TestWalkPassesAListingErrorToFnAndStopsOnItsError(t *testing.T) {
	t.Parallel()
	root := writeTree(t)
	sub := filepath.Join(root, "sub")
	boom := errors.New("cannot list")

	broken := func() *Tree {
		tr := For(t.TempDir()) // a Tree of its own: the listing cache is per Tree
		d := &dirList{err: boom}
		d.once.Do(func() {})
		tr.dirs[sub] = d
		return tr
	}

	// fn returns the listing error: the walk ends with it.
	var listed []string
	err := broken().Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			listed = append(listed, p)
		}
		return err
	})
	if !errors.Is(err, boom) || len(listed) != 1 || listed[0] != sub {
		t.Errorf("err = %v, listed = %v; want the listing error for %s", err, listed, sub)
	}

	// fn answers a listing error with nil: the walk goes on and ends clean.
	if err := broken().Walk(root, func(p string, d fs.DirEntry, err error) error { return nil }); err != nil {
		t.Errorf("a tolerated listing error ended the walk with %v", err)
	}

	// fn fails on a file: the walk returns that error.
	stop := errors.New("stop")
	err = For(root).Walk(root, func(p string, d fs.DirEntry, err error) error {
		if filepath.Base(p) == "b.go" {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) {
		t.Errorf("an error from a visited file ended the walk with %v, want %v", err, stop)
	}
}
