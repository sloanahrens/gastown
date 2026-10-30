package tiersweep

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"testing"
)

// fakeSource is the Source a test injects. LsFiles returns the tracked files
// whose final element matches the pattern, as the git pathspec does for the
// patterns the code passes (*go.mod, *_test.go); ReadFile returns the scripted
// contents, and an error for a name with none.
type fakeSource struct {
	tracked  []string
	contents map[string]string
}

func (f *fakeSource) LsFiles(_, pattern string) ([]string, error) {
	var out []string
	for _, file := range f.tracked {
		if ok, err := path.Match(pattern, path.Base(file)); err != nil {
			return nil, err
		} else if ok {
			out = append(out, file)
		}
	}
	return out, nil
}

func (f *fakeSource) ReadFile(name string) ([]byte, error) {
	text, ok := f.contents[name]
	if !ok {
		return nil, fmt.Errorf("read %s: no scripted contents", name)
	}
	return []byte(text), nil
}

// equalList fails the test when got and want differ. A nil and an empty list
// are the same answer: no entries.
func equalList(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNestedModulesTable: a go.mod below the root is a module; the root one,
// the ones under testdata/ or vendor/ at any depth, and a notgo.mod are not.
func TestNestedModulesTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		tracked []string
		want    []string
	}{
		{
			name: "nested modules listed sorted, root and excluded ones left out",
			tracked: []string{
				"tracker/store/go.mod", "tracker/go.mod", "a/b/go.mod", "go.mod",
				"testdata/x/go.mod", "vendor/x/go.mod", "a/testdata/go.mod",
				"extra/notgo.mod", "x/vendor/go.mod",
			},
			want: []string{"a/b", "tracker", "tracker/store"},
		},
		{
			name:    "a module listed twice is listed once",
			tracked: []string{"x/go.mod", "x/go.mod"},
			want:    []string{"x"},
		},
		{
			name:    "a root go.mod alone has no nested module",
			tracked: []string{"go.mod", "README.md"},
			want:    nil,
		},
		{
			name:    "a file only named like go.mod is not a module",
			tracked: []string{"a/go.mod.bak", "a/notgo.mod"},
			want:    nil,
		},
		{
			name:    "no files, no modules",
			tracked: nil,
			want:    nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := &fakeSource{tracked: tc.tracked}
			files, err := src.LsFiles(".", "*go.mod")
			if err != nil {
				t.Fatal(err)
			}
			equalList(t, NestedModules(files), tc.want)
		})
	}
}

// TestNestedModulesLsFilesPattern: the listing passes its pattern through, so
// a tracked file the pattern does not name never reaches NestedModules. The
// fake holds a non-matching file that an ignored pattern would return.
func TestNestedModulesLsFilesPattern(t *testing.T) {
	t.Parallel()
	src := &fakeSource{tracked: []string{"a/go.mod", "a/main.go", "a/go.sum"}}
	got, err := src.LsFiles(".", "*go.mod")
	if err != nil {
		t.Fatal(err)
	}
	equalList(t, got, []string{"a/go.mod"})
}

// TestTaggedDirsTable: a test file whose //go:build line names integration is
// tagged, and the regexp decides which lines name it. A root-level testdata/
// file is kept, which is pinned behaviour.
func TestTaggedDirsTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		tracked  []string
		contents map[string]string
		want     []string
	}{
		{
			name:     "a plain integration tag counts",
			tracked:  []string{"tracker/store/x_test.go"},
			contents: map[string]string{"tracker/store/x_test.go": "//go:build integration\n\npackage x\n"},
			want:     []string{"./tracker/store"},
		},
		{
			name:     "a negated tag does not count",
			tracked:  []string{"a/x_test.go"},
			contents: map[string]string{"a/x_test.go": "//go:build !integration\n"},
			want:     nil,
		},
		{
			name:     "integration_other still matches the regexp",
			tracked:  []string{"a/x_test.go"},
			contents: map[string]string{"a/x_test.go": "//go:build integration_other\n"},
			want:     []string{"./a"},
		},
		{
			name:     "integration inside a tag expression counts",
			tracked:  []string{"a/x_test.go"},
			contents: map[string]string{"a/x_test.go": "//go:build linux || integration\n"},
			want:     []string{"./a"},
		},
		{
			name:     "a build line not at the start of its line does not count",
			tracked:  []string{"a/x_test.go"},
			contents: map[string]string{"a/x_test.go": "package x\n // go:build integration\n"},
			want:     nil,
		},
		{
			name:     "a file at the module root gives ./",
			tracked:  []string{"root_test.go"},
			contents: map[string]string{"root_test.go": "//go:build integration\n"},
			want:     []string{"./"},
		},
		{
			name:     "a file under /testdata/ does not count",
			tracked:  []string{"a/testdata/x_test.go"},
			contents: map[string]string{"a/testdata/x_test.go": "//go:build integration\n"},
			want:     nil,
		},
		{
			name:     "a root-level testdata/ file is included (pinned)",
			tracked:  []string{"testdata/x_test.go"},
			contents: map[string]string{"testdata/x_test.go": "//go:build integration\n"},
			want:     []string{"./testdata"},
		},
		{
			name:     "a tagged file that is not a _test.go file does not count",
			tracked:  []string{"a/helper.go"},
			contents: map[string]string{"a/helper.go": "//go:build integration\n"},
			want:     nil,
		},
		{
			name:    "a file that cannot be read keeps its directory",
			tracked: []string{"a/x_test.go"},
			want:    []string{"./a"},
		},
		{
			name:    "directories sorted bytewise and listed once",
			tracked: []string{"b/x_test.go", "a/b/y_test.go", "a/z_test.go", "a/w_test.go"},
			contents: map[string]string{
				"b/x_test.go":   "//go:build integration\n",
				"a/b/y_test.go": "//go:build integration\n",
				"a/z_test.go":   "//go:build integration\n",
				"a/w_test.go":   "//go:build integration\n",
			},
			want: []string{"./a", "./a/b", "./b"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := &fakeSource{tracked: tc.tracked, contents: tc.contents}
			files, err := src.LsFiles(".", "*_test.go")
			if err != nil {
				t.Fatal(err)
			}
			equalList(t, TaggedDirs(files, src.ReadFile), tc.want)
		})
	}
}

// TestInsideModule feeds InsideModule the list NestedModules returns for a
// file list holding a nested module, a root module and a root package, so the
// two helpers are checked against each other's real output. A module's own
// directory and anything below it are inside, in either path form; a sibling
// that shares its prefix is not, and the root is never inside.
func TestInsideModule(t *testing.T) {
	t.Parallel()
	fixture := []string{"tracker/go.mod", "go.mod", "tracker/store/x_test.go", "cmd/gt/main.go"}
	nested := NestedModules(fixture)
	equalList(t, nested, []string{"tracker"})

	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{name: "nested module, ./ form", dir: "./tracker/store", want: true},
		{name: "nested module, bare form", dir: "tracker/store", want: true},
		{name: "nested module, trailing slash", dir: "tracker/store/", want: true},
		{name: "the module directory itself", dir: "./tracker", want: true},
		{name: "sibling sharing a prefix", dir: "./tracker2", want: false},
		{name: "root package", dir: "./cmd/gt", want: false},
		{name: "root, ./ form", dir: "./", want: false},
		{name: "root, dot form", dir: ".", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := InsideModule(tc.dir, nested); got != tc.want {
				t.Errorf("InsideModule(%q, NestedModules) = %v, want %v", tc.dir, got, tc.want)
			}
		})
	}
	t.Run("hand-written ./-prefixed modules", func(t *testing.T) {
		t.Parallel()
		modules := []string{"./tracker"}
		if !InsideModule("tracker/store", modules) || !InsideModule("./tracker/store", modules) {
			t.Error("a package below a ./-prefixed module is not inside it")
		}
		if InsideModule("./tracker2", modules) {
			t.Error("./tracker2 is inside ./tracker")
		}
	})
	t.Run("no modules, nothing inside", func(t *testing.T) {
		t.Parallel()
		if InsideModule("./tracker", nil) {
			t.Error("InsideModule with no modules = true, want false")
		}
	})
}

// TestDockerPackages: comments, blank lines, trailing comments and extra
// fields are handled as the script's sed and awk handle them.
func TestDockerPackages(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		text string
		want []string
	}{
		{
			name: "one package per line, in order",
			text: "internal/beads\ninternal/testutil\n",
			want: []string{"./internal/beads", "./internal/testutil"},
		},
		{
			name: "comment-only and blank lines are skipped",
			text: "# header\n\n   \n# another\ninternal/beads\n",
			want: []string{"./internal/beads"},
		},
		{
			name: "a trailing comment is stripped",
			text: "internal/beads   # the Dolt helpers\n",
			want: []string{"./internal/beads"},
		},
		{
			name: "only the first field of a line counts",
			text: "internal/cmd extra words here\n",
			want: []string{"./internal/cmd"},
		},
		{
			name: "a tab-indented package is read",
			text: "\tinternal/daemon\n",
			want: []string{"./internal/daemon"},
		},
		{
			name: "empty text lists nothing",
			text: "",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			equalList(t, DockerPackages(tc.text), tc.want)
		})
	}
}

// TestDockerPackagesFixture reads the checked-in docker.txt fixture, which mixes
// every case above in one file, and expects its packages in file order.
func TestDockerPackagesFixture(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("testdata/docker.txt")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"./internal/beads", "./internal/testutil", "./internal/cmd", "./internal/daemon"}
	equalList(t, DockerPackages(string(data)), want)
}
