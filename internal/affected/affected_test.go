package affected

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// fakeRunner answers the selector's commands from canned output and records
// each argv it is asked, so the unit tier starts no process.
type fakeRunner struct {
	diff    []byte
	list    []byte
	diffErr error
	listErr error
	calls   [][]string
}

func (f *fakeRunner) run(argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	if len(argv) == 0 {
		return nil, errors.New("fake runner: empty argv")
	}
	switch argv[0] {
	case "git":
		return f.diff, f.diffErr
	case "go":
		return f.list, f.listErr
	}
	return nil, fmt.Errorf("fake runner: unexpected program %q", argv[0])
}

// names renders `git diff --name-only -z` output: every path, NUL-terminated.
func names(files ...string) []byte {
	var out []byte
	for _, file := range files {
		out = append(out, file...)
		out = append(out, 0)
	}
	return out
}

// fixtureRoot is the module directory the captured graph names: its Dir fields
// were rewritten to it, so the fixture travels with the repository.
const fixtureRoot = "/fixture"

// listGraph is a real `go list -deps -test -json ./...` capture of the module
// under testdata/modsrc, filtered to the main module's own records, with each
// Dir rewritten to fixtureRoot. That module holds a leaf a; b imports a; e
// imports b; c's tests import a from the package and f's tests import it from
// an external test package; d and tools depend on nothing. Driving the graph
// from the capture, not a hand-written stream, is what proves the selector
// reads the record shapes go list actually writes.
func listGraph(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golist.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

// dependentsOfA is every package the graph says a change to internal/a
// reaches: a itself, b and e through imports, and c and f through their tests.
var dependentsOfA = []string{
	"example.com/m/internal/a",
	"example.com/m/internal/b",
	"example.com/m/internal/c",
	"example.com/m/internal/e",
	"example.com/m/internal/f",
}

func TestAffectedReverseClosureSelectsDependents(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		diff []byte
		want []string
	}{
		{"changed leaf", names("internal/a/a.go"), dependentsOfA},
		{"leaf and a file beside it", names("internal/a/a.go", "internal/a/data.json"), dependentsOfA},
		{"file in a directory below the package", names("internal/a/sub/data.json"), dependentsOfA},
		{"changed package no test imports", names("internal/d/d.go"), []string{"example.com/m/internal/d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, list: listGraph(t)}
			sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if sel.All {
				t.Fatalf("Select = ALL, want %v", tc.want)
			}
			if !slices.Equal(sel.Packages, tc.want) {
				t.Errorf("Select = %v, want %v", sel.Packages, tc.want)
			}
		})
	}
}

// TestAffectedEmptyDiffSelectsNothing pins the state the command prints NONE
// for: an empty diff is not a read failure, and it is not ALL either.
func TestAffectedEmptyDiffSelectsNothing(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if sel.All {
		t.Fatal("Select = ALL, want no packages")
	}
	if len(sel.Packages) != 0 {
		t.Errorf("Select = %v, want no packages", sel.Packages)
	}
	if len(r.calls) != 1 {
		t.Errorf("ran %d commands, want only git diff: %q", len(r.calls), r.calls)
	}
}

// TestAffectedOutsideModuleRootIsAll covers a selector started below the module
// root: go list's ./... would cover the subtree alone, so the closure would
// lose the importers outside it. The selector must fail wide rather than return
// that short list.
func TestAffectedOutsideModuleRootIsAll(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{diff: names("internal/a/a.go"), list: listGraph(t)}
	sel, err := Select(r.run, filepath.Join(fixtureRoot, "internal", "a"), "origin/main", "HEAD")
	if !sel.All {
		t.Fatalf("Select = %v, want ALL", sel.Packages)
	}
	if err == nil {
		t.Error("Select error = nil, want the module root mismatch named")
	}
}

func TestAffectedSameDirResolvesSymlinks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if !sameDir(link, dir) {
		t.Errorf("sameDir(%q, %q) = false, want true", link, dir)
	}
	if sameDir(dir, filepath.Join(dir, "sub")) {
		t.Errorf("sameDir(%q, %q) = true, want false", dir, filepath.Join(dir, "sub"))
	}
}

func TestAffectedAsksForTheDiffAndTheGraph(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{diff: names("internal/a/a.go"), list: listGraph(t)}
	if _, err := Select(r.run, fixtureRoot, "origin/main", "HEAD"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	want := [][]string{
		{"git", "diff", "--name-only", "--no-renames", "-z", "--end-of-options", "origin/main...HEAD"},
		{"go", "list", "-deps", "-test", "-json", "./..."},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Errorf("called %q, want %q", r.calls, want)
	}
}

func TestAffectedWidensToAll(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		diff    []byte
		wantErr bool
	}{
		{"go.mod", names("go.mod"), false},
		{"go.sum", names("go.sum"), false},
		{"Makefile", names("Makefile"), false},
		{".forgejo workflow", names(".forgejo/workflows/gate.yml"), false},
		{"file outside every package", names("docs/guide.md"), true},
		{"testdata file", names("internal/a/testdata/fixture.json"), false},
		{"go file in a directory the go tool skips", names("_gen/tool.go"), true},
		{"build input beside a package file", names("internal/d/d.go", "go.sum"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, list: listGraph(t)}
			sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
			if !sel.All {
				t.Fatalf("Select = %v, want ALL", sel.Packages)
			}
			if tc.wantErr != (err != nil) {
				t.Errorf("Select error = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

func TestAffectedUnreadableDiffIsAll(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		diff    []byte
		diffErr error
		list    []byte
		listErr error
	}{
		{name: "git diff fails", diffErr: errors.New("git diff failed")},
		{name: "go list fails", diff: names("internal/a/a.go"), listErr: errors.New("go list failed")},
		{name: "diff is not NUL-terminated", diff: []byte("internal/a/a.go")},
		{name: "diff holds an empty path", diff: []byte("internal/a/a.go\x00\x00")},
		{name: "go list output is not JSON", diff: names("internal/a/a.go"), list: []byte("not json")},
		{
			name: "go list reports a package error",
			diff: names("internal/a/a.go"),
			list: []byte(`{"ImportPath": "example.com/m/internal/a", "Error": {"Err": "build constraints exclude all Go files"}}`),
		},
		{
			name: "go list names no main module",
			diff: names("internal/a/a.go"),
			list: []byte(`{"ImportPath": "example.com/m/internal/a", "Dir": "/repo/internal/a"}`),
		},
		{
			name: "the main module record names no directory",
			diff: names("internal/a/a.go"),
			list: []byte(`{"ImportPath": "example.com/m/internal/a", "Dir": "/repo/internal/a", "Module": {"Path": "example.com/m", "Main": true}}`),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, diffErr: tc.diffErr, list: tc.list, listErr: tc.listErr}
			sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
			if !sel.All {
				t.Fatalf("Select = %v, want ALL", sel.Packages)
			}
			if err == nil {
				t.Error("Select error = nil, want the unreadable input named")
			}
		})
	}
}

func TestAffectedUnplacedFileIsSelected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		diff []byte
		want []string
	}{
		{"new package directory", names("internal/newpkg/new.go"), []string{"example.com/m/internal/newpkg"}},
		{"new file in the module root", names("root.go"), []string{"example.com/m"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, list: listGraph(t)}
			sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if sel.All {
				t.Fatalf("Select = ALL, want %v", tc.want)
			}
			if !slices.Equal(sel.Packages, tc.want) {
				t.Errorf("Select = %v, want %v", sel.Packages, tc.want)
			}
		})
	}
}
