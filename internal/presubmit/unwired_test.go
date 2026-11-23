package presubmit

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// listedFiles answers FileLister from a fixed list, so the judge reads only the module it is given.
func listedFiles(paths ...string) FileLister {
	return func() ([]string, error) { return paths, nil }
}

// TestUnwired judges which added exports it reports: the declaration line added, a caller in
// another file, a caller below it in the same file, a recursive call, a comment or string that
// spells the name, a method, an unexported name, a function on the base, a test file and a nested
// module (gt-46ezdn).
func TestUnwired(t *testing.T) {
	t.Parallel()
	const (
		declFn   = "package x\n\nfunc Foo() {}\n"
		selfCall = "package x\n\nfunc Foo() { Foo() }\n"
		thisCall = "package x\n\nfunc Foo() {}\n\nfunc use() { Foo() }\n"
		onBase   = "package x\n\nfunc Foo() {\n\tx := 1\n}\n"
	)
	nested := func(p string) bool { return strings.HasPrefix(p, "plugins/") }
	tests := []struct {
		name   string
		diff   string
		files  map[string]string
		list   []string
		nested func(string) bool
		want   []string // path:line name, sorted
		judged int
	}{
		{"an export no file names is unwired", waitDiff("x.go", declFn, 3),
			map[string]string{"x.go": declFn}, []string{"x.go"}, nil, []string{"x.go:3 Foo"}, 1},
		{"a caller in another file wires it", waitDiff("x.go", declFn, 3),
			map[string]string{"x.go": declFn, "y.go": "package x\n\nfunc use() { Foo() }\n"}, []string{"x.go", "y.go"}, nil, nil, 1},
		{"a caller below it in the same file wires it", waitDiff("x.go", thisCall, 3),
			map[string]string{"x.go": thisCall}, []string{"x.go"}, nil, nil, 1},
		{"a recursive call inside its own declaration does not wire it", waitDiff("x.go", selfCall, 3),
			map[string]string{"x.go": selfCall}, []string{"x.go"}, nil, []string{"x.go:3 Foo"}, 1},
		{"a comment or string that spells the name does not wire it", waitDiff("x.go", declFn, 3),
			map[string]string{"x.go": declFn, "y.go": "package y\n\n// Foo is named here.\nvar s = \"Foo\"\n"}, []string{"x.go", "y.go"}, nil, []string{"x.go:3 Foo"}, 1},
		{"a caller in a nested module does not wire it", waitDiff("x.go", declFn, 3),
			map[string]string{"x.go": declFn, "plugins/p/y.go": "package p\n\nfunc use() { Foo() }\n"}, []string{"x.go", "plugins/p/y.go"}, nested, []string{"x.go:3 Foo"}, 1},
		{"two added exports report the one no file names", waitDiff("x.go", "package x\n\nfunc Foo() {}\n\nfunc Bar() {}\n\nfunc use() { Bar() }\n", 3, 5),
			map[string]string{"x.go": "package x\n\nfunc Foo() {}\n\nfunc Bar() {}\n\nfunc use() { Bar() }\n"}, []string{"x.go"}, nil, []string{"x.go:3 Foo"}, 2},
		{"an unexported function is not judged", waitDiff("x.go", "package x\n\nfunc foo() {}\n", 3),
			map[string]string{"x.go": "package x\n\nfunc foo() {}\n"}, []string{"x.go"}, nil, nil, 0},
		{"a method is not judged", waitDiff("x.go", "package x\n\ntype T struct{}\n\nfunc (T) Foo() {}\n", 5),
			map[string]string{"x.go": "package x\n\ntype T struct{}\n\nfunc (T) Foo() {}\n"}, []string{"x.go"}, nil, nil, 0},
		{"a function whose name line is on the base is not judged", waitDiff("x.go", onBase, 4),
			map[string]string{"x.go": onBase}, []string{"x.go"}, nil, nil, 0},
		{"a function in a test file is not judged", waitDiff("x_test.go", declFn, 3),
			map[string]string{"x_test.go": declFn}, []string{"x_test.go"}, nil, nil, 0},
		{"an export added in a nested module is not judged", waitDiff("plugins/p/x.go", declFn, 3),
			map[string]string{"plugins/p/x.go": declFn}, []string{"plugins/p/x.go"}, nested, nil, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			res, err := JudgeUnwired(strings.NewReader(tt.diff), listedFiles(tt.list...), waitFiles(tt.files), tt.nested)
			if err != nil {
				t.Fatalf("JudgeUnwired: %v", err)
			}
			var got []string
			for _, f := range res.Findings {
				got = append(got, f.Path+":"+strconv.Itoa(f.Line)+" "+f.Name)
			}
			if !slices.Equal(got, tt.want) || res.Judged != tt.judged {
				t.Errorf("reported %v with %d judged, want %v with %d", got, res.Judged, tt.want, tt.judged)
			}
		})
	}
}

// TestUnwiredRefusesWhatItCannotJudge: every input the judge needs and cannot read fails it, so a
// branch it could not read never reports as one with no unwired export (gt-46ezdn).
func TestUnwiredRefusesWhatItCannotJudge(t *testing.T) {
	t.Parallel()
	const declFn = "package x\n\nfunc Foo() {}\n"
	tests := []struct {
		name  string
		diff  string
		list  FileLister
		files map[string]string
		want  string
	}{
		{"an unreadable hunk header", "+++ b/x.go\n@@ nonsense @@\n+package x\n", listedFiles("x.go"), nil, "unreadable hunk header"},
		{"a changed path git quoted", "+++ \"b/x.go\"\n@@ -0,0 +1,1 @@\n+package x\n", listedFiles("x.go"), nil, "quoted a changed path"},
		{"a changed file that cannot be read", waitDiff("x.go", declFn, 3), listedFiles("x.go"), map[string]string{}, "reading x.go"},
		{"a changed file that does not parse", waitDiff("x.go", declFn, 3), listedFiles("x.go"), map[string]string{"x.go": "package x\nfunc (\n"}, "x.go:"},
		{"a module file that cannot be read", waitDiff("x.go", declFn, 3), listedFiles("y.go"), map[string]string{"x.go": declFn}, "reading y.go"},
		{"a module file that does not parse", waitDiff("x.go", declFn, 3), listedFiles("x.go", "y.go"), map[string]string{"x.go": declFn, "y.go": "package y\nfunc Foo(\n"}, "y.go:"},
		{"a module that lists no Go file", waitDiff("x.go", declFn, 3), listedFiles(), map[string]string{"x.go": declFn}, "lists no Go file"},
		{"a module list that cannot be read", waitDiff("x.go", declFn, 3), func() ([]string, error) { return nil, errors.New("list failed") }, map[string]string{"x.go": declFn}, "listing the module's Go files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := JudgeUnwired(strings.NewReader(tt.diff), tt.list, waitFiles(tt.files), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("JudgeUnwired error = %v, want one naming %q", err, tt.want)
			}
		})
	}
}
