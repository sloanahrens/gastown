package done

import (
	"errors"
	"io/fs"
	"slices"
	"testing"
	"testing/fstest"
)

// TestPackageTestNamesListsEveryPackage: the names of two packages come back in package then
// file order, each file read through openDir from its package's Dir, with the environment
// environ returns; the only command run is go list (gt-icmj4w).
func TestPackageTestNamesListsEveryPackage(t *testing.T) {
	t.Parallel()
	const out = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"],"XTestGoFiles":["a_x_test.go"]}
{"ImportPath":"x/b","Dir":"/w/b","TestGoFiles":["b_test.go"]}
`
	dirs := map[string]fstest.MapFS{
		"/w/a": {
			"a_test.go":   {Data: []byte("package a\n\nfunc TestA(t *testing.T) {}\n")},
			"a_x_test.go": {Data: []byte("package a_test\n\nfunc TestAX(t *testing.T) {}\n")},
		},
		"/w/b": {
			"b_test.go": {Data: []byte("package b\n\nfunc TestB(t *testing.T) {}\nfunc BenchmarkB(b *testing.B) {}\n")},
		},
	}
	var calls [][]string
	var gotEnv []string
	run := func(_ string, env []string, args ...string) ([]byte, error) {
		calls = append(calls, args)
		gotEnv = env
		return []byte(out), nil
	}
	environ := func() []string { return []string{"GO111MODULE=on"} }
	openDir := func(dir string) fs.FS { return dirs[dir] }

	got, err := packageTestNames(run, environ, openDir, "/w", "", []string{"./a", "./b"})
	if err != nil {
		t.Fatalf("packageTestNames() error = %v", err)
	}
	want := []string{"TestA", "TestAX", "TestB", "BenchmarkB"}
	if !slices.Equal(got, want) {
		t.Errorf("packageTestNames() = %q, want %q", got, want)
	}
	if len(calls) != 1 || calls[0][0] != "list" {
		t.Errorf("commands run = %q, want exactly one go list", calls)
	}
	if !slices.Equal(gotEnv, []string{"GO111MODULE=on"}) {
		t.Errorf("go list got %d env entries, want the one environ returned", len(gotEnv))
	}
}

// TestPackageTestNamesRefusesAFailedStep: a go list that fails, a package go list cannot load,
// a test file that cannot be read and one that does not parse each return an error and no
// names, so a partial listing is never read as a package with no tests (gt-icmj4w).
func TestPackageTestNamesRefusesAFailedStep(t *testing.T) {
	t.Parallel()
	const listed = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"]}
`
	const notLoaded = `{"ImportPath":"x/a","Dir":"/w/a","Error":{"Err":"no Go files"}}
`
	ok := func(out string) gateListCommand {
		return func(string, []string, ...string) ([]byte, error) { return []byte(out), nil }
	}
	tests := []struct {
		name   string
		run    gateListCommand
		openFS fstest.MapFS
	}{
		{"go list fails", func(string, []string, ...string) ([]byte, error) { return nil, errors.New("exit status 1") }, nil},
		{"package not loaded", ok(notLoaded), nil},
		{"file unreadable", ok(listed), fstest.MapFS{}},
		{"file unparseable", ok(listed), fstest.MapFS{"a_test.go": {Data: []byte("package a\nfunc TestA( {\n")}}},
	}
	for _, tc := range tests {
		openDir := func(string) fs.FS { return tc.openFS }
		got, err := packageTestNames(tc.run, func() []string { return nil }, openDir, "/w", "", []string{"./a"})
		if err == nil || got != nil {
			t.Errorf("%s: packageTestNames() = %q, %v; want no names and an error", tc.name, got, err)
		}
	}
}
