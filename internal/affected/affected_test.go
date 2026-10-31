package affected

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeRunner answers the selector's commands from canned output and records
// each argv it is asked, so the unit tier starts no process. The two git diffs
// are told apart by the range: the first names base...head, the second the head
// the working tree is compared against.
type fakeRunner struct {
	diff      []byte // git diff base...head
	treeDiff  []byte // git diff head, the working tree against head
	untracked []byte // git ls-files --others
	list      []byte // go list
	calls     [][]string

	diffErr      error
	treeDiffErr  error
	untrackedErr error
	listErr      error
}

func (f *fakeRunner) run(argv []string) ([]byte, error) {
	f.calls = append(f.calls, slices.Clone(argv))
	if len(argv) == 0 {
		return nil, errors.New("fake runner: empty argv")
	}
	if argv[0] == "go" {
		return f.list, f.listErr
	}
	if argv[0] != "git" || len(argv) < 2 {
		return nil, fmt.Errorf("fake runner: unexpected command %q", argv)
	}
	switch argv[1] {
	case "diff":
		if strings.Contains(argv[len(argv)-1], "...") {
			return f.diff, f.diffErr
		}
		return f.treeDiff, f.treeDiffErr
	case "ls-files":
		return f.untracked, f.untrackedErr
	}
	return nil, fmt.Errorf("fake runner: unexpected git command %q", argv[1])
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
// Dir rewritten to fixtureRoot. Regenerate it from the repository root with:
//
//	cd internal/affected/testdata/modsrc &&
//	  go list -deps -test -json ./... |
//	  jq -c 'select(.Module.Main)' |
//	  sed "s|$(pwd)|/fixture|g" > ../golist.json
//
// The module holds the root package m, a leaf a; b imports a; e imports b; c's
// internal test and f's external test import a; d and tools depend on nothing.
// g has an internal test and an external test that imports h, which imports g,
// so go list reports h rebuilt for g's test binary as "h [g.test]" with ForTest
// naming g: the recompiled-dependency shape, which a hand-written stream would
// not think to include.
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
		{"a file beside the changed package", names("internal/a/a.go", "internal/a/data.json"), dependentsOfA},
		{"changed package no test imports", names("internal/d/d.go"), []string{"example.com/m/internal/d"}},
		{"changed package a test imports", names("internal/h/h.go"), []string{"example.com/m/internal/g", "example.com/m/internal/h"}},
		{"changed package an import reaches", names("internal/g/g.go"), []string{"example.com/m/internal/g", "example.com/m/internal/h"}},
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

func TestAffectedAsksForTheDiffTheTreeAndTheGraph(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{diff: names("internal/a/a.go"), list: listGraph(t)}
	if _, err := Select(r.run, fixtureRoot, "origin/main", "HEAD"); err != nil {
		t.Fatalf("Select: %v", err)
	}
	want := [][]string{
		{"git", "diff", "--name-only", "--no-renames", "-z", "--end-of-options", "origin/main...HEAD"},
		{"git", "diff", "--name-only", "-z", "--end-of-options", "HEAD"},
		{"git", "ls-files", "--others", "-z", "--", "*.go", "go.mod", "go.sum"},
		{"go", "list", "-deps", "-test", "-json", "./..."},
	}
	if !slices.EqualFunc(r.calls, want, slices.Equal) {
		t.Errorf("called %q, want %q", r.calls, want)
	}
}

// TestAffectedWorkingTreeOutsideHeadIsAll covers the graph being read from the
// working tree while the diff ends at head: a tree that is not head's would
// resolve the diff against the wrong import edges, so it must fail wide.
func TestAffectedWorkingTreeOutsideHeadIsAll(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		treeDiff  []byte
		untracked []byte
		treeErr   error
		otherErr  error
	}{
		{name: "tracked change since head", treeDiff: names("internal/a/a.go")},
		{name: "untracked go file", untracked: names("internal/a/extra.go")},
		{name: "diff against head fails", treeErr: errors.New("git diff failed")},
		{name: "untracked files cannot be listed", otherErr: errors.New("git ls-files failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			diff := names("internal/a/a.go")
			r := &fakeRunner{diff: diff, treeDiff: tc.treeDiff, untracked: tc.untracked, list: listGraph(t), treeDiffErr: tc.treeErr, untrackedErr: tc.otherErr}
			sel, err := Select(r.run, fixtureRoot, "origin/main", "HEAD")
			if !sel.All {
				t.Fatalf("Select = %v, want ALL", sel.Packages)
			}
			if err == nil {
				t.Error("Select error = nil, want the tree state named")
			}
		})
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
		{"file below a package directory", names("internal/a/sub/data.json"), true},
		{"file in a directory the go tool skips", names(".github/workflows/x.yml"), true},
		{"testdata file", names("internal/a/testdata/fixture.json"), false},
		{"testdata file outside every package", names("testdata/fixture.json"), false},
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
		{"file of the root package", names("root.go"), []string{"example.com/m"}},
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
