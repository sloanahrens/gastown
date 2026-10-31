package affected

import (
	"errors"
	"fmt"
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

// listGraph is a `go list -deps -test -json` stream for a module of seven
// packages: b imports a, e imports b, and the tests of c and f import a, while
// d, tools and the module root hold no dependency. c's test is in its own
// package, which go list reports as the variant and the test binary; f's is an
// external test package, whose variant is named after the parent in ForTest.
// The fixture fails if a test-only import stops being an edge, and if the
// external test package f_test is read as a package of its own.
const listGraph = `{
	"ImportPath": "example.com/m/internal/a",
	"Dir": "/repo/internal/a",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/b",
	"Dir": "/repo/internal/b",
	"Deps": ["example.com/m/internal/a", "fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/c",
	"Dir": "/repo/internal/c",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/c [example.com/m/internal/c.test]",
	"Dir": "/repo/internal/c",
	"Deps": ["example.com/m/internal/a", "fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/c.test",
	"Dir": "/repo/internal/c",
	"Deps": ["example.com/m/internal/a", "example.com/m/internal/c [example.com/m/internal/c.test]"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/d",
	"Dir": "/repo/internal/d",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/e",
	"Dir": "/repo/internal/e",
	"Deps": ["example.com/m/internal/b", "fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/f",
	"Dir": "/repo/internal/f",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/f_test [example.com/m/internal/f.test]",
	"Dir": "/repo/internal/f",
	"ForTest": "example.com/m/internal/f",
	"Deps": ["example.com/m/internal/a", "fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/internal/f.test",
	"Dir": "/repo/internal/f",
	"Deps": ["example.com/m/internal/f_test [example.com/m/internal/f.test]", "fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
{
	"ImportPath": "example.com/m/tools",
	"Dir": "/repo/tools",
	"Deps": ["fmt"],
	"Module": {"Path": "example.com/m", "Dir": "/repo", "Main": true}
}
`

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
		{"no change", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{diff: tc.diff, list: []byte(listGraph)}
			sel, err := Select(r.run, "origin/main", "HEAD")
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

func TestAffectedAsksForTheDiffAndTheGraph(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{diff: names("internal/a/a.go"), list: []byte(listGraph)}
	if _, err := Select(r.run, "origin/main", "HEAD"); err != nil {
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
			r := &fakeRunner{diff: tc.diff, list: []byte(listGraph)}
			sel, err := Select(r.run, "origin/main", "HEAD")
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
			sel, err := Select(r.run, "origin/main", "HEAD")
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
			r := &fakeRunner{diff: tc.diff, list: []byte(listGraph)}
			sel, err := Select(r.run, "origin/main", "HEAD")
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
