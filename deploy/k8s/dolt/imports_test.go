package dolt

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// packageDir is the directory of this package's source, found from this file
// rather than from the working directory a test may be run in.
func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller cannot locate this test file")
	}
	return filepath.Dir(file)
}

// goFiles lists the package's non-test Go files, the ones the import check
// judges: a test may import what the builder itself must not.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, name)
	}
	return files
}

// importsOf returns the paths imported by one file, read without building it.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	paths := make([]string, 0, len(f.Imports))
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("%s imports %s, which is not a quoted path", path, imp.Path.Value)
		}
		paths = append(paths, p)
	}
	return paths
}

// forbiddenImport reports whether path would give the builder a way to reach a
// cluster or to run a command: a Kubernetes client, or a process or socket.
func forbiddenImport(path string) bool {
	switch {
	case path == "os/exec":
		return true
	case path == "net", strings.HasPrefix(path, "net/"):
		return true
	case strings.HasPrefix(path, "k8s.io/"), strings.HasPrefix(path, "sigs.k8s.io/"):
		return true
	}
	return false
}
