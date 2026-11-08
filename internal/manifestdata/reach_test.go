package manifestdata

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

// packagePath is this package's import path, the root of the captured graph.
const packagePath = "github.com/steveyegge/gastown/internal/manifestdata"

// regenerateCommand rebuilds testdata/imports.golist.txt from the package
// directory: the Go toolchain on the first line, then the graph `go list -deps`
// reports for this package.
const regenerateCommand = `{ echo "# $(go env GOVERSION)"; go list -deps -f '{{.ImportPath}}|{{join .Imports " "}}' . ; } > testdata/imports.golist.txt`

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

// goFiles lists the package's non-test Go files, the ones the capture must
// describe. A directory with none fails, since a scan over nothing would pass
// for the wrong reason.
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
		t.Fatalf("%s holds no non-test Go file, so the capture check read nothing", dir)
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

// dependency is one record of the graph `go list -deps` reports: a package's
// import path and the paths its non-test files import.
type dependency struct {
	Path    string
	Imports []string
}

// capturedGraph reads testdata/imports.golist.txt and returns the Go toolchain
// the capture was made with and the dependency graph `go list -deps` reports for
// this package. The unit tier may not run the go tool (docs/testing.md,
// no-build), so the walk reads a capture, not a live `go list`.
// TestLeafHasNoProcessOrSocketReach refuses the capture when its toolchain is
// not the running one, or when its record of this package no longer matches the
// package's files. The capture cannot see a go.mod change to a dependency: a bump
// of yaml.v3 can change its imports and no check here reads go.mod, so
// regenerate after any go.mod change. A toolchain change moves the standard
// library's own reach (os reaches syscall in every program), and the toolchain
// check is what forces its regeneration.
func capturedGraph(t *testing.T, dir string) (string, []dependency) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "testdata", "imports.golist.txt"))
	if err != nil {
		t.Fatalf("reading the captured dependency graph: %v", err)
	}
	toolchain, graph, err := parseCapture(string(data))
	if err != nil {
		t.Fatal(err)
	}
	return toolchain, graph
}

// parseCapture reads a capture: the Go toolchain on its first line, then one
// record per line. It refuses a capture with no toolchain header, a record with
// no "|", or no record at all, since each would let the walk pass for the wrong
// reason.
func parseCapture(data string) (string, []dependency, error) {
	lines := strings.Split(data, "\n")
	toolchain, ok := strings.CutPrefix(lines[0], "# ")
	if !ok || toolchain == "" {
		return "", nil, fmt.Errorf("testdata/imports.golist.txt has no toolchain header on its first line, so it cannot be judged against the running Go; regenerate it with `%s`", regenerateCommand)
	}
	var graph []dependency
	for i, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		path, imports, ok := strings.Cut(line, "|")
		if !ok {
			return "", nil, fmt.Errorf("the captured graph's line %d %q has no \"|\" between the package and its imports", i+2, line)
		}
		graph = append(graph, dependency{Path: path, Imports: strings.Fields(imports)})
	}
	if len(graph) == 0 {
		return "", nil, fmt.Errorf("the captured dependency graph in testdata/imports.golist.txt holds no records, so the walk would pass for the wrong reason")
	}
	return toolchain, graph, nil
}

// toolchainStaleness returns why a capture made with the recorded toolchain
// cannot stand for the running one, or "" when it can.
func toolchainStaleness(recorded, running string) string {
	if recorded == running {
		return ""
	}
	return fmt.Sprintf("testdata/imports.golist.txt was captured with %s but the running toolchain is %s, and a toolchain change moves the standard library's reach; regenerate it with `%s`", recorded, running, regenerateCommand)
}

// graphStaleness returns why graph cannot stand for the package whose non-test
// files import fileImports, or "" when it can. It judges the capture's record of
// this package alone, which is where a new import appears; a dependency's own
// record moves only with go.mod or the Go version, and the walk refuses an
// import the capture does not report.
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
		return fmt.Sprintf("testdata/imports.golist.txt is stale: it records %s importing %q, the package's files import %q; regenerate it with `%s`", root, got, want, regenerateCommand)
	}
	return fmt.Sprintf("testdata/imports.golist.txt is stale: it holds no record of %s; regenerate it with `%s`", root, regenerateCommand)
}

// stdlibImport reports whether path is in the standard library. A standard
// package path has no dot in its first element; a module path has one.
func stdlibImport(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// forbiddenDependency reports whether path gives the leaf a way to run a
// command, open a socket, load a plugin or reach a cluster. syscall and unsafe
// are not on the list: the standard library's os and fmt reach both in every
// program, so a test that forbade them could never pass (overseer decision on
// gt-gg9hv.69, recorded on gt-t5tf9g).
func forbiddenDependency(path string) bool {
	switch {
	case path == "os/exec", path == "net", path == "net/http", path == "plugin", path == "os/user":
		return true
	case strings.HasPrefix(path, "net/"):
		return true
	case strings.HasPrefix(path, "k8s.io/"), strings.HasPrefix(path, "sigs.k8s.io/"):
		return true
	}
	return false
}

// allowedDependency reports whether the leaf may import path: the standard
// library or the YAML encoder, the only module the package imports.
func allowedDependency(path string) bool {
	return path == "gopkg.in/yaml.v3" || stdlibImport(path)
}

// leafFindings walks graph from root and reports every package the leaf reaches
// that is forbidden or outside the standard library and the YAML encoder, and
// every import the graph does not report: an import whose reach is unknown
// could reach a process, so it is refused rather than skipped.
func leafFindings(root string, graph []dependency) []string {
	byPath := make(map[string]dependency, len(graph))
	for _, dep := range graph {
		byPath[dep.Path] = dep
	}
	start, ok := byPath[root]
	if !ok {
		return []string{fmt.Sprintf("go list -deps reported no package %q, so the leaf's reach is unknown", root)}
	}
	var findings []string
	seen := map[string]bool{root: true}
	queue := []dependency{start}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, imp := range node.Imports {
			if forbiddenDependency(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which gives the leaf a process, a socket or a cluster client", node.Path, imp))
				continue
			}
			if !allowedDependency(imp) {
				findings = append(findings, fmt.Sprintf("%s imports %q, which is neither the standard library nor the YAML encoder", node.Path, imp))
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

// TestLeafHasNoProcessOrSocketReach holds the real package to the import rule:
// its dependency set holds no process, socket, plugin, os/user or cluster
// client. It walks a captured `go list -deps` graph, not a live go list, so the
// capture must describe the running toolchain and the package's current files.
// Regenerate the capture from this directory with:
//
//	{ echo "# $(go env GOVERSION)"; go list -deps -f '{{.ImportPath}}|{{join .Imports " "}}' . ; } > testdata/imports.golist.txt
func TestLeafHasNoProcessOrSocketReach(t *testing.T) {
	t.Parallel()
	dir := packageDir(t)
	recorded, graph := capturedGraph(t, dir)
	if stale := toolchainStaleness(recorded, runtime.Version()); stale != "" {
		t.Fatal(stale)
	}
	if stale := graphStaleness(packagePath, graph, packageImports(t, dir)); stale != "" {
		t.Fatal(stale)
	}
	for _, finding := range leafFindings(packagePath, graph) {
		t.Errorf("%s: the leaf imports no process, socket or cluster client and nothing beyond the standard library and the YAML encoder", finding)
	}
}

// TestLeafFindingsReportsReach drives every branch of the walk against graphs
// this test owns: each forbidden package is reported, a third-party import and
// an unknown one are refused, and a standard-library graph reaching syscall and
// unsafe passes.
func TestLeafFindingsReportsReach(t *testing.T) {
	t.Parallel()
	const root = "example.com/leaf"
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
			name:  "a process",
			graph: []dependency{{Path: root, Imports: []string{"os/exec"}}},
			want:  []string{`example.com/leaf imports "os/exec", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "a socket",
			graph: []dependency{{Path: root, Imports: []string{"net"}}},
			want:  []string{`example.com/leaf imports "net", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "an HTTP client",
			graph: []dependency{{Path: root, Imports: []string{"net/http"}}},
			want:  []string{`example.com/leaf imports "net/http", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "a plugin",
			graph: []dependency{{Path: root, Imports: []string{"plugin"}}},
			want:  []string{`example.com/leaf imports "plugin", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "an account lookup",
			graph: []dependency{{Path: root, Imports: []string{"os/user"}}},
			want:  []string{`example.com/leaf imports "os/user", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "a Kubernetes client",
			graph: []dependency{{Path: root, Imports: []string{"k8s.io/client-go/kubernetes"}}},
			want:  []string{`example.com/leaf imports "k8s.io/client-go/kubernetes", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name:  "a controller-runtime client",
			graph: []dependency{{Path: root, Imports: []string{"sigs.k8s.io/controller-runtime"}}},
			want:  []string{`example.com/leaf imports "sigs.k8s.io/controller-runtime", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name: "a forbidden package reached through the YAML encoder",
			graph: []dependency{
				{Path: root, Imports: []string{"gopkg.in/yaml.v3"}},
				{Path: "gopkg.in/yaml.v3", Imports: []string{"net"}},
			},
			want: []string{`gopkg.in/yaml.v3 imports "net", which gives the leaf a process, a socket or a cluster client`},
		},
		{
			name: "a third-party package",
			graph: []dependency{
				{Path: root, Imports: []string{"gopkg.in/yaml.v3"}},
				{Path: "gopkg.in/yaml.v3", Imports: []string{"example.com/x"}},
			},
			want: []string{`gopkg.in/yaml.v3 imports "example.com/x", which is neither the standard library nor the YAML encoder`},
		},
		{
			name:  "an import the graph does not report",
			graph: []dependency{{Path: root, Imports: []string{"fmt"}}},
			want:  []string{`example.com/leaf imports "fmt", which go list -deps did not report, so its reach is unknown`},
		},
		{
			name:  "a graph without the root",
			graph: []dependency{{Path: "fmt"}},
			want:  []string{`go list -deps reported no package "example.com/leaf", so the leaf's reach is unknown`},
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
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := leafFindings(root, tc.graph)
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
	const root = "example.com/leaf"
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
		{"no record of the package", []dependency{{Path: "gopkg.in/yaml.v3"}}, []string{"fmt"}, "holds no record of example.com/leaf"},
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

// TestParseCaptureReadsToolchainAndRecords drives the capture's success branch:
// the toolchain on the first line and each record after it.
func TestParseCaptureReadsToolchainAndRecords(t *testing.T) {
	t.Parallel()
	toolchain, graph, err := parseCapture("# go1.27.1\nexample.com/leaf|fmt gopkg.in/yaml.v3\nfmt|\n")
	if err != nil {
		t.Fatalf("parseCapture refused a well-formed capture: %v", err)
	}
	if toolchain != "go1.27.1" {
		t.Errorf("parseCapture read toolchain %q, want %q", toolchain, "go1.27.1")
	}
	if len(graph) != 2 || graph[0].Path != "example.com/leaf" || len(graph[0].Imports) != 2 {
		t.Errorf("parseCapture read graph %+v, want the leaf with two imports and fmt", graph)
	}
}

// TestParseCaptureRefusesMalformedCapture drives each refusal of the capture
// reader: a capture with no toolchain header, a record with no separator, and a
// capture with no records.
func TestParseCaptureRefusesMalformedCapture(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		data string
		want string
	}{
		{"no toolchain header", "example.com/leaf|fmt\n", "has no toolchain header on its first line"},
		{"an empty file", "", "has no toolchain header on its first line"},
		{"a record with no separator", "# go1.27.1\nexample.com/leaf fmt\n", `line 2 "example.com/leaf fmt" has no "|"`},
		{"no records", "# go1.27.1\n\n", "holds no records"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseCapture(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseCapture returned %v, want an error naming %q", err, tc.want)
			}
		})
	}
}

// TestToolchainStalenessRefusesOtherVersions drives the toolchain check: a
// capture made with the running toolchain passes, and one made with any other
// version is refused with both versions and the regeneration command.
func TestToolchainStalenessRefusesOtherVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		recorded string
		running  string
		refused  bool
	}{
		{"the running toolchain", "go1.27.1", "go1.27.1", false},
		{"an older toolchain", "go1.26.4", "go1.27.1", true},
		{"a newer toolchain", "go1.28.0", "go1.27.1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := toolchainStaleness(tc.recorded, tc.running)
			if !tc.refused {
				if got != "" {
					t.Errorf("toolchainStaleness refused a capture made with the running toolchain: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.recorded) || !strings.Contains(got, tc.running) {
				t.Errorf("toolchainStaleness reported %q, want both %s and %s", got, tc.recorded, tc.running)
			}
			if !strings.Contains(got, "regenerate it with") {
				t.Errorf("toolchainStaleness reported %q, want the regeneration command", got)
			}
		})
	}
}
