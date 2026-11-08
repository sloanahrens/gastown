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

// manifestPackages is the package holding the manifests this builder renders
// from: the daemon's secrets and the town root's ownership split. It imports
// the standard library and the YAML module only, so the walk continues through
// it and covers every package the renderer reaches.
var manifestPackages = []string{
	"github.com/steveyegge/gastown/internal/manifestdata",
}

// allowedImport reports whether path is one the renderer may import: the
// standard library, the YAML encoder the renderer writes with (the sibling
// packages' rule, recorded on gt-gg9hv.69), or a manifest package.
func allowedImport(path string) bool {
	if slices.Contains(manifestPackages, path) || path == "gopkg.in/yaml.v3" {
		return true
	}
	return stdlibImport(path)
}

// stdlibImport reports whether path is in the standard library. A standard
// package path has no dot in its first element; a module path has one.
func stdlibImport(path string) bool {
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

// reachForbidden reports whether path, reached through the dependencies of the
// renderer rather than imported by it, gives a way to run a command, open a
// socket, load a plugin or talk to a cluster. It is forbiddenImport without
// syscall and unsafe, and only a standard-library node may use it: os, fmt and
// reflect reach both in every program, so refusing them there would refuse
// everything. The renderer's own files and every non-standard-library package
// it reaches are held to the full forbiddenImport list.
func reachForbidden(path string) bool {
	switch {
	case path == "os/exec", path == "plugin":
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
				findings = append(findings, fmt.Sprintf("%s imports %q, which gives the renderer a process, a socket or a cluster client", file, imp))
			}
		}
	}
	sort.Strings(findings)
	return findings
}

// packagePath is this package's import path: the module path plus its
// directory, and the root record of the captured dependency graph.
const packagePath = "github.com/steveyegge/gastown/deploy/k8s/daemon"

// dependency is one record of the graph `go list -deps` reports: a package's
// import path and the paths its non-test files import.
type dependency struct {
	Path    string
	Imports []string
}

// capturedGraph reads testdata/imports.golist.txt, the dependency graph
// `go list -deps -f '{{.ImportPath}}|{{join .Imports " "}}' .` reports for
// this package. The unit tier may not run the go tool (docs/testing.md,
// no-build), so the graph is a capture; TestImportsAreRestricted refuses the
// capture as stale when its record of this package no longer matches the
// package's files, rather than walking an old graph. Regenerate it from this
// directory with:
//
//	go list -deps -f '{{.ImportPath}}|{{join .Imports " "}}' . > testdata/imports.golist.txt
func capturedGraph(t *testing.T, dir string) []dependency {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "testdata", "imports.golist.txt"))
	if err != nil {
		t.Fatalf("reading the captured dependency graph: %v", err)
	}
	var graph []dependency
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		path, imports, ok := strings.Cut(line, "|")
		if !ok {
			t.Fatalf("the captured graph's line %d %q has no \"|\" between the package and its imports", i+1, line)
		}
		graph = append(graph, dependency{Path: path, Imports: strings.Fields(imports)})
	}
	if len(graph) == 0 {
		t.Fatalf("the captured dependency graph in testdata/imports.golist.txt holds no records, so the walk would pass for the wrong reason")
	}
	return graph
}

// graphStaleness returns why graph cannot stand for the package whose non-test
// files import fileImports, or "" when it can. It judges the capture's record of
// this package alone, which is where a change to the renderer appears. A
// dependency's record (the YAML module's today) moves only with go.mod, so a
// go.mod change needs the capture regenerated even when this check passes.
func graphStaleness(root string, graph []dependency, fileImports []string) string {
	want := slices.Clone(fileImports)
	sort.Strings(want)
	for _, dep := range graph {
		if dep.Path != root {
			continue
		}
		got := slices.Clone(dep.Imports)
		sort.Strings(got)
		if slices.Equal(got, want) {
			return ""
		}
		return fmt.Sprintf("testdata/imports.golist.txt is stale: it records %s importing %q, the package's files import %q; regenerate it with `go list -deps -f '{{.ImportPath}}|{{join .Imports \" \"}}' . > testdata/imports.golist.txt`", root, got, want)
	}
	return fmt.Sprintf("testdata/imports.golist.txt is stale: it holds no record of %s; regenerate it with `go list -deps -f '{{.ImportPath}}|{{join .Imports \" \"}}' . > testdata/imports.golist.txt`", root)
}

// packageImports returns the sorted, unique imports of dir's non-test Go files,
// the imports the captured graph's record of the package must hold.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	var imports []string
	for _, file := range goFiles(t, dir) {
		imports = append(imports, importsOf(t, filepath.Join(dir, file))...)
	}
	sort.Strings(imports)
	return slices.Compact(imports)
}

// transitiveFindings walks graph from root and reports every package the
// renderer reaches, directly or through its dependencies, that is neither the
// standard library, the YAML encoder nor a manifest package, and every
// forbidden package it reaches through any path.
//
// The walk passes through internal/manifestdata like any other dependency: it
// imports the standard library and the YAML module only, so a forbidden package
// reached through it is the renderer's too. It does NOT stop at the standard
// library: a standard package that reaches os/exec or net (reachForbidden) is a
// finding, so the renderer cannot gain a process or a socket through, say, a
// standard package that imports one. syscall and unsafe are reachable from fmt
// alone, so a standard-library node may import them; the renderer and every
// other package may not (forbiddenImport). Anything else reached outside those
// is a finding, and so is an import the graph does not report, standard library
// included: an import whose reach is unknown could reach a process or a
// cluster, so it is refused rather than skipped.
func transitiveFindings(root string, graph []dependency) []string {
	byPath := make(map[string]dependency, len(graph))
	for _, dep := range graph {
		byPath[dep.Path] = dep
	}
	start, ok := byPath[root]
	if !ok {
		return []string{fmt.Sprintf("go list -deps reported no package %q, so the renderer's reach is unknown", root)}
	}
	var findings []string
	seen := map[string]bool{root: true}
	queue := []dependency{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, imp := range node.Imports {
			if !allowedImport(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which is neither the standard library, the YAML encoder nor a manifest package", node.Path, imp))
				continue
			}
			forbidden := forbiddenImport
			if stdlibImport(node.Path) && node.Path != root {
				forbidden = reachForbidden
			}
			if forbidden(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which gives the renderer a process, a socket or a cluster client", node.Path, imp))
				continue
			}
			if seen[imp] {
				continue
			}
			dep, ok := byPath[imp]
			if !ok {
				findings = append(findings, fmt.Sprintf("%s imports %q, which go list -deps did not report, so its reach is unknown", node.Path, imp))
				continue
			}
			seen[imp] = true
			queue = append(queue, dep)
		}
	}
	sort.Strings(findings)
	return findings
}

func TestImportsAreRestricted(t *testing.T) {
	t.Parallel()
	dir := packageDir(t)
	for _, finding := range scanImports(t, dir) {
		t.Errorf("%s: the renderer imports no cluster client, command or third-party package beyond the YAML encoder", finding)
	}
	graph := capturedGraph(t, dir)
	if stale := graphStaleness(packagePath, graph, packageImports(t, dir)); stale != "" {
		t.Fatal(stale)
	}
	for _, finding := range transitiveFindings(packagePath, graph) {
		t.Errorf("%s: the renderer reaches no cluster client, command or third-party package beyond the YAML encoder", finding)
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

	"github.com/steveyegge/gastown/internal/manifestdata"
)

var (
	_ = fmt.Sprint
	_ = yaml.Marshal
	_ = manifestdata.SecretFile{Name: "x"}
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
		`apply.go imports "net/http", which gives the renderer a process, a socket or a cluster client`,
		`apply.go imports "os/exec", which gives the renderer a process, a socket or a cluster client`,
		`apply.go imports "sigs.k8s.io/controller-runtime", which is neither the standard library, the YAML encoder nor a manifest package`,
		`apply.go imports "unsafe", which gives the renderer a process, a socket or a cluster client`,
	}
	if got := scanImports(t, dir); !slices.Equal(got, want) {
		t.Errorf("the scan reported %q, want %q", got, want)
	}
}

// TestTransitiveFindingsReportsReach drives every branch of the walk against a
// graph this test owns: each refusal is reported, and the manifest packages'
// own reach, the standard library's and a clean graph report nothing.
func TestTransitiveFindingsReportsReach(t *testing.T) {
	t.Parallel()
	const root = "example.com/renderer"
	tests := []struct {
		name  string
		graph []dependency
		want  []string
	}{
		{
			name: "a clean graph",
			graph: []dependency{
				{Path: root, Imports: []string{"fmt", "gopkg.in/yaml.v3"}},
				{Path: "gopkg.in/yaml.v3", Imports: []string{"fmt"}},
				{Path: "fmt"},
			},
		},
		{
			name: "a package outside the standard library, the YAML encoder and the manifests",
			graph: []dependency{
				{Path: root, Imports: []string{"gopkg.in/yaml.v3"}},
				{Path: "gopkg.in/yaml.v3", Imports: []string{"example.com/apply"}},
			},
			want: []string{`gopkg.in/yaml.v3 imports "example.com/apply", which is neither the standard library, the YAML encoder nor a manifest package`},
		},
		{
			name: "a forbidden package reached around the manifest packages",
			graph: []dependency{
				{Path: root, Imports: []string{"gopkg.in/yaml.v3"}},
				{Path: "gopkg.in/yaml.v3", Imports: []string{"net", "os/exec", "syscall"}},
				{Path: "syscall"},
			},
			want: []string{
				`gopkg.in/yaml.v3 imports "net", which gives the renderer a process, a socket or a cluster client`,
				`gopkg.in/yaml.v3 imports "os/exec", which gives the renderer a process, a socket or a cluster client`,
				`gopkg.in/yaml.v3 imports "syscall", which gives the renderer a process, a socket or a cluster client`,
			},
		},
		{
			name: "a process or a socket reached through the standard library",
			graph: []dependency{
				{Path: root, Imports: []string{"fmt"}},
				{Path: "fmt", Imports: []string{"text/template"}},
				{Path: "text/template", Imports: []string{"os/exec", "net/http"}},
				{Path: "os/exec", Imports: []string{"os"}},
				{Path: "net/http", Imports: []string{"net"}},
			},
			want: []string{
				`text/template imports "net/http", which gives the renderer a process, a socket or a cluster client`,
				`text/template imports "os/exec", which gives the renderer a process, a socket or a cluster client`,
			},
		},
		{
			name: "the renderer's own import of syscall",
			graph: []dependency{
				{Path: root, Imports: []string{"syscall"}},
				{Path: "syscall", Imports: []string{"unsafe"}},
			},
			want: []string{`example.com/renderer imports "syscall", which gives the renderer a process, a socket or a cluster client`},
		},
		{
			name: "a standard package the graph does not report",
			graph: []dependency{
				{Path: root, Imports: []string{"fmt"}},
			},
			want: []string{`example.com/renderer imports "fmt", which go list -deps did not report, so its reach is unknown`},
		},
		{
			name: "a forbidden package reached through the manifest package",
			graph: []dependency{
				{Path: root, Imports: []string{"github.com/steveyegge/gastown/internal/manifestdata"}},
				{Path: "github.com/steveyegge/gastown/internal/manifestdata", Imports: []string{"gopkg.in/yaml.v3", "net", "os/exec"}},
				{Path: "gopkg.in/yaml.v3"},
			},
			want: []string{
				`github.com/steveyegge/gastown/internal/manifestdata imports "net", which gives the renderer a process, a socket or a cluster client`,
				`github.com/steveyegge/gastown/internal/manifestdata imports "os/exec", which gives the renderer a process, a socket or a cluster client`,
			},
		},
		{
			name: "syscall and unsafe reached through the standard library",
			graph: []dependency{
				{Path: root, Imports: []string{"fmt"}},
				{Path: "fmt", Imports: []string{"os", "reflect"}},
				{Path: "os", Imports: []string{"syscall"}},
				{Path: "syscall", Imports: []string{"unsafe"}},
				{Path: "reflect", Imports: []string{"unsafe"}},
				{Path: "unsafe"},
			},
		},
		{
			name: "a dependency the graph does not report",
			graph: []dependency{
				{Path: root, Imports: []string{"gopkg.in/yaml.v3"}},
			},
			want: []string{`example.com/renderer imports "gopkg.in/yaml.v3", which go list -deps did not report, so its reach is unknown`},
		},
		{
			name: "a graph without the root",
			graph: []dependency{
				{Path: "gopkg.in/yaml.v3", Imports: []string{"fmt"}},
			},
			want: []string{`go list -deps reported no package "example.com/renderer", so the renderer's reach is unknown`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := transitiveFindings(root, tc.graph)
			if !slices.Equal(got, tc.want) {
				t.Errorf("the walk reported %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCapturedGraphRefusesStaleness drives the guard on the capture: a graph
// whose record of this package matches the files passes, and one that differs or
// holds no record is refused with the command that regenerates it.
func TestCapturedGraphRefusesStaleness(t *testing.T) {
	t.Parallel()
	const root = "example.com/renderer"
	graph := []dependency{{Path: root, Imports: []string{"fmt", "io"}}}
	tests := []struct {
		name        string
		graph       []dependency
		fileImports []string
		want        string
	}{
		{"a graph that matches the files", graph, []string{"io", "fmt"}, ""},
		{"an import the files do not have", graph, []string{"fmt"}, `importing ["fmt" "io"]`},
		{"an import the graph lacks", graph, []string{"fmt", "io", "os"}, `files import ["fmt" "io" "os"]`},
		{"no record of the package", []dependency{{Path: "gopkg.in/yaml.v3"}}, []string{"fmt"}, "holds no record of example.com/renderer"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := graphStaleness(root, tc.graph, tc.fileImports)
			if tc.want == "" {
				if got != "" {
					t.Errorf("graphStaleness refused a graph that matches the files: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("graphStaleness reported %q, want a finding naming %q", got, tc.want)
			}
			if !strings.Contains(got, "regenerate it with") {
				t.Errorf("graphStaleness reported %q, want the regeneration command", got)
			}
		})
	}
}

// writeFixture writes one file of the fixture package.
func writeFixture(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
