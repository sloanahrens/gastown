package done

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// goListFixture is go list's output for three packages: both test file kinds, none, in-package only.
const goListFixture = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"],"XTestGoFiles":["a_x_test.go"]}
{"ImportPath":"x/b","Dir":"/w/b"}
{"ImportPath":"x/c","Dir":"/w/c","TestGoFiles":["c_test.go"]}
`

// fakeGoList answers with out and err and records the argv in argv.
func fakeGoList(out string, err error, argv *[]string) gateListCommand {
	return func(_ string, _ []string, args ...string) ([]byte, error) {
		*argv = args
		return []byte(out), err
	}
}

// TestGoListTestFilesArgvAndDecode: the argv is the go list line, with -tags only when the
// Gate line names tags; each package's files are TestGoFiles then XTestGoFiles with its Dir (gt-y9uww1).
func TestGoListTestFilesArgvAndDecode(t *testing.T) {
	t.Parallel()
	const flags = "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Error"
	packages := []string{"./a", "./b", "./c"}
	want := []gateTestFiles{
		{Dir: "/w/a", Files: []string{"a_test.go", "a_x_test.go"}},
		{Dir: "/w/b"},
		{Dir: "/w/c", Files: []string{"c_test.go"}},
	}
	tests := []struct {
		tags     string
		wantArgv []string
	}{
		{"", []string{"list", "-e", flags, "./a", "./b", "./c"}},
		{"integration", []string{"list", "-e", "-tags", "integration", flags, "./a", "./b", "./c"}},
	}
	for _, tc := range tests {
		var argv []string
		got, err := goListTestFiles(fakeGoList(goListFixture, nil, &argv), nil, "/w", tc.tags, packages)
		if err != nil {
			t.Fatalf("tags %q: goListTestFiles() = %v, want the listing", tc.tags, err)
		}
		if !slices.Equal(argv, tc.wantArgv) {
			t.Errorf("tags %q: go list argv = %q, want %q", tc.tags, argv, tc.wantArgv)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("tags %q: goListTestFiles() = %+v, want %+v", tc.tags, got, want)
		}
	}
}

// TestGoListTestFilesPackageError: a package go list could not load carries an Error and no
// files; it is refused by name, the first such package wins, and no listing comes back, so
// a missing package is never read as a package with no tests (gt-ehy53r).
func TestGoListTestFilesPackageError(t *testing.T) {
	t.Parallel()
	const out = `{"ImportPath":"x/a","Dir":"/w/a","TestGoFiles":["a_test.go"]}
{"ImportPath":"x/missing","Dir":"/w/missing","Error":{"Err":"no required module provides package x/missing"}}
{"ImportPath":"x/later","Dir":"/w/later","Error":{"Err":"second failure"}}
`
	var argv []string
	got, err := goListTestFiles(fakeGoList(out, nil, &argv), nil, "/w", "", []string{"./a", "./missing", "./later"})
	if err == nil || got != nil {
		t.Fatalf("goListTestFiles() = %+v, %v; want no listing and an error", got, err)
	}
	for _, want := range []string{"x/missing", "no required module provides package x/missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "x/later") {
		t.Errorf("error = %q, names a later package; the first Error package must win", err)
	}
}

// TestGoListTestFilesRefusesABadRun: a go list that fails, or prints output that does not
// decode, returns an error and no listing, so a partial answer is never read as clean (gt-y9uww1).
func TestGoListTestFilesRefusesABadRun(t *testing.T) {
	t.Parallel()
	tests := map[string]gateListCommand{
		"command fails":   fakeGoList(goListFixture, errors.New("exit status 1"), new([]string)),
		"output bad json": fakeGoList("{not json", nil, new([]string)),
	}
	for name, run := range tests {
		got, err := goListTestFiles(run, nil, "/w", "", []string{"./a"})
		if err == nil || got != nil {
			t.Errorf("%s: goListTestFiles() = %+v, %v; want no listing and an error", name, got, err)
		}
	}
}
