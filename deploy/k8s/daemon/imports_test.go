package daemon

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
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
// judges: a test may import what the builder itself must not. A directory with
// none fails, since a scan over nothing would pass for the wrong reason.
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
	if len(files) == 0 {
		t.Fatalf("%s holds no non-test Go file, so the import check read nothing", dir)
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

// manifestPackages are the packages holding the two manifests this builder
// renders from: the daemon's secrets and the town root's ownership split.
var manifestPackages = []string{
	"github.com/steveyegge/gastown/internal/templates",
	"github.com/steveyegge/gastown/internal/townstate",
}

// allowedImport reports whether path is one the renderer may reach: the
// standard library, the YAML encoder its goldens are written with, or a
// manifest package.
func allowedImport(path string) bool {
	if slices.Contains(manifestPackages, path) {
		return true
	}
	if path == "gopkg.in/yaml.v3" {
		return true
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// forbiddenImport reports whether path would give the renderer a way to reach a
// cluster or to run a command, whatever the allow list says of the rest of the
// standard library: a process, a socket, a dynamic plugin or direct memory
// access, or a Kubernetes client.
func forbiddenImport(path string) bool {
	switch {
	case path == "os/exec", path == "syscall", path == "plugin", path == "unsafe":
		return true
	case path == "net", strings.HasPrefix(path, "net/"):
		return true
	case strings.HasPrefix(path, "k8s.io/"), strings.HasPrefix(path, "sigs.k8s.io/"):
		return true
	}
	return false
}

// scanImports reports every import in dir's non-test Go files that the allow
// list refuses or the forbidden set names, one finding per import. It returns
// the findings instead of failing, so a fixture that must produce some drives
// this same scan.
func scanImports(t *testing.T, dir string) []string {
	t.Helper()
	var findings []string
	for _, file := range goFiles(t, dir) {
		for _, imp := range importsOf(t, filepath.Join(dir, file)) {
			if !allowedImport(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which is neither the standard library, the YAML encoder nor a manifest package", file, imp))
				continue
			}
			if forbiddenImport(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which reaches a process, a socket or a cluster", file, imp))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

func TestImportsAreRestricted(t *testing.T) {
	t.Parallel()
	for _, finding := range scanImports(t, packageDir(t)) {
		t.Errorf("%s: the renderer reaches no cluster, command or third-party package", finding)
	}
}

// TestImportScanReportsForbiddenImports drives the failing branch: a fixture
// importing a client, a process, a socket and a third-party package must be
// reported, and the clean file and the test file beside it must not.
func TestImportScanReportsForbiddenImports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFixture(t, filepath.Join(dir, "builder.go"), `package fixture

import (
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/templates"
)

var (
	_ = fmt.Sprint
	_ = yaml.Marshal
	_ = templates.SecretFile{Name: "x"}
)
`)
	writeFixture(t, filepath.Join(dir, "apply.go"), `package fixture

import (
	_ "k8s.io/client-go/kubernetes"
	_ "net/http"
	_ "os/exec"
	_ "sigs.k8s.io/controller-runtime"
	_ "unsafe"
	_ "example.com/client"
)
`)
	writeFixture(t, filepath.Join(dir, "apply_test.go"), `package fixture

import _ "os/exec"
`)

	want := []string{
		`apply.go imports "example.com/client", which is neither the standard library, the YAML encoder nor a manifest package`,
		`apply.go imports "k8s.io/client-go/kubernetes", which is neither the standard library, the YAML encoder nor a manifest package`,
		`apply.go imports "net/http", which reaches a process, a socket or a cluster`,
		`apply.go imports "os/exec", which reaches a process, a socket or a cluster`,
		`apply.go imports "sigs.k8s.io/controller-runtime", which is neither the standard library, the YAML encoder nor a manifest package`,
		`apply.go imports "unsafe", which reaches a process, a socket or a cluster`,
	}
	if got := scanImports(t, dir); !slices.Equal(got, want) {
		t.Errorf("the scan reported %q, want %q", got, want)
	}
}

// writeFixture writes one file of the fixture package.
func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
