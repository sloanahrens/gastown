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

// TestStaticGateListerSelectsByPattern: the pattern keeps the names it selects, in package then
// file order, and go list runs with the Gate line's tags and packages (gt-002qkd).
func TestStaticGateListerSelectsByPattern(t *testing.T) {
	t.Parallel()
	const out = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"]}
{"ImportPath":"x/b","Dir":"/w/b","TestGoFiles":["b_test.go"]}
`
	dirs := map[string]fstest.MapFS{
		"/w/a": {"a_test.go": {Data: []byte("package a\n\nfunc TestAlpha(t *testing.T) {}\nfunc TestBeta(t *testing.T) {}\n")}},
		"/w/b": {"b_test.go": {Data: []byte("package b\n\nfunc TestAlphaTwo(t *testing.T) {}\nfunc BenchmarkAlpha(b *testing.B) {}\n")}},
	}
	tests := []struct {
		pattern string
		want    []string
	}{
		{"Alpha", []string{"TestAlpha", "TestAlphaTwo", "BenchmarkAlpha"}},
		{"TestBeta|Two", []string{"TestBeta", "TestAlphaTwo"}},
		{"", []string{"TestAlpha", "TestBeta", "TestAlphaTwo", "BenchmarkAlpha"}},
		{"Nothing", nil},
	}
	for _, tc := range tests {
		var args []string
		run := func(_ string, _ []string, a ...string) ([]byte, error) {
			args = a
			return []byte(out), nil
		}
		lister := newStaticGateLister(run, func() []string { return nil }, func(dir string) fs.FS { return dirs[dir] })
		got, err := lister("/w", GateRun{Packages: []string{"./a", "./b"}, Pattern: tc.pattern, Tags: "integration"})
		if err != nil {
			t.Fatalf("lister(%q) error = %v", tc.pattern, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("lister(%q) = %q, want %q", tc.pattern, got, tc.want)
		}
		if !slices.Contains(args, "integration") || !slices.Contains(args, "./b") {
			t.Errorf("go list args = %q, want the Gate line's tags and packages", args)
		}
	}
}

// TestStaticGateListerListsANameOnceInAStableOrder: a name two packages declare is listed once, at
// its first position, the same on every run (gt-002qkd).
func TestStaticGateListerListsANameOnceInAStableOrder(t *testing.T) {
	t.Parallel()
	const out = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"]}
{"ImportPath":"x/a_test","Dir":"/w/a_ext","XTestGoFiles":["ext_test.go"]}
`
	dirs := map[string]fstest.MapFS{
		"/w/a":     {"a_test.go": {Data: []byte("package a\n\nfunc TestShared(t *testing.T) {}\nfunc TestOnlyA(t *testing.T) {}\n")}},
		"/w/a_ext": {"ext_test.go": {Data: []byte("package a_test\n\nfunc TestOnlyB(t *testing.T) {}\nfunc TestShared(t *testing.T) {}\n")}},
	}
	run := func(string, []string, ...string) ([]byte, error) { return []byte(out), nil }
	lister := newStaticGateLister(run, func() []string { return nil }, func(dir string) fs.FS { return dirs[dir] })
	want := []string{"TestShared", "TestOnlyA", "TestOnlyB"}
	gr := GateRun{Packages: []string{"./a", "./a_test"}, Pattern: "Test"}
	first, err := lister("/w", gr)
	if err != nil {
		t.Fatalf("lister() error = %v", err)
	}
	second, err := lister("/w", gr)
	if err != nil {
		t.Fatalf("lister() error = %v", err)
	}
	if !slices.Equal(first, want) || !slices.Equal(second, want) {
		t.Errorf("lister() runs = %q and %q, want %q both times", first, second, want)
	}
}

// TestStaticGateListerErrors: a go list that fails, an unreadable test file and a -run pattern
// that does not compile each end the call with its error and no names (gt-002qkd).
func TestStaticGateListerErrors(t *testing.T) {
	t.Parallel()
	errList := errors.New("list failed")
	const listed = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"]}
`
	const noTests = `{"ImportPath":"x/a","Dir":"/w/a"}
`
	listing := func(out string) gateListCommand {
		return func(string, []string, ...string) ([]byte, error) { return []byte(out), nil }
	}
	good := fstest.MapFS{"a_test.go": {Data: []byte("package a\n\nfunc TestA(t *testing.T) {}\n")}}
	tests := []struct {
		name    string
		run     gateListCommand
		openFS  fstest.MapFS
		pattern string
		wantErr error
	}{
		{"go list fails", func(string, []string, ...string) ([]byte, error) { return nil, errList }, good, "TestA", errList},
		{"file unreadable", listing(listed), fstest.MapFS{}, "TestA", fs.ErrNotExist},
		{"pattern does not compile", listing(listed), good, "[", nil},
		{"pattern does not compile and no name is listed", listing(noTests), good, "[", nil},
	}
	for _, tc := range tests {
		lister := newStaticGateLister(tc.run, func() []string { return nil }, func(string) fs.FS { return tc.openFS })
		got, err := lister("/w", GateRun{Packages: []string{"./a"}, Pattern: tc.pattern})
		if err == nil || got != nil {
			t.Errorf("%s: lister() = %q, %v; want no names and an error", tc.name, got, err)
		}
		if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: lister() error = %v, want it to wrap %v", tc.name, err, tc.wantErr)
		}
	}
}
