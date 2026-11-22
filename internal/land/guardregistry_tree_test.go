package land

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
)

// scanningExemptionsPath is the exemption list's committed path, read from the
// repo root.
const scanningExemptionsPath = "internal/land/testdata/unregistered-scanners.txt"

// treeScanningRegistryFailures returns what guardRegistryFailures reports for
// the tree at repo, whose exemption list is scanningExemptionsPath. An input it
// cannot read or parse is an error, never an empty exemption map.
func treeScanningRegistryFailures(repo fs.FS, registry []treeWideGuard) ([]string, error) {
	text, err := fs.ReadFile(repo, scanningExemptionsPath)
	if err != nil {
		return nil, fmt.Errorf("reading the exemption file: %w", err)
	}
	exempt, err := parseExemptions(string(text))
	if err != nil {
		return nil, fmt.Errorf("parsing the exemption file: %w", err)
	}
	found, err := ScanningTests(repo)
	if err != nil {
		return nil, fmt.Errorf("scanning the source tree: %w", err)
	}
	return guardRegistryFailures(found, registry, exempt), nil
}

// treeScanningRegistryFailure renders the gate's failure text for lines, one
// line per finding, and "" when the tree passes.
func treeScanningRegistryFailure(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return "tree-scanning tests are neither registered nor exempt:\n" + strings.Join(lines, "\n")
}

// TestEveryTreeScanningTestIsRegistered fails the landing gate when the real
// tree holds a test that scans the source tree and is neither a treeWideGuards
// entry nor a line of internal/land/testdata/unregistered-scanners.txt, so the
// next scanner like TestImageRegistryBaseNotPrependedInSource cannot land
// unregistered (gt-yboxur). The fixture rows pin the failure text and the
// refusals the gate takes on an input it cannot read.
func TestEveryTreeScanningTestIsRegistered(t *testing.T) {
	t.Parallel()
	const exemptHeader = "# <pkg> <TestName> # <reason>\n"
	const scan = "package p\n\nfunc TestScan(t *testing.T) { filepath.WalkDir(\"..\", nil) }\n"
	finding := func(pkg string) string {
		return pkg + " TestScan scans the source tree but is not in treeWideGuards (internal/land/changed.go): add it to the entry for " + pkg + " with the paths it reads as judges, or, if it only walks a fixture, list it in the exemption file with a reason"
	}
	// tree holds the exemption list, when exempt is not empty, and one
	// scanning test per package name.
	tree := func(exempt string, pkgs ...string) fstest.MapFS {
		m := fstest.MapFS{}
		if exempt != "" {
			m[scanningExemptionsPath] = &fstest.MapFile{Data: []byte(exempt)}
		}
		for _, pkg := range pkgs {
			m[pkg+"/x_test.go"] = &fstest.MapFile{Data: []byte(scan)}
		}
		return m
	}
	registered := []treeWideGuard{{pkg: "internal/b", tests: []string{"TestScan"}}}
	for _, tc := range []struct {
		name     string
		repo     fs.FS
		registry []treeWideGuard
		wantMsg  string
		wantErr  string
	}{
		{name: "the repo as committed", repo: os.DirFS("../.."), registry: treeWideGuards},
		{name: "an unregistered scanner is one finding line", repo: tree(exemptHeader, "internal/b"), wantMsg: "tree-scanning tests are neither registered nor exempt:\n" + finding("internal/b")},
		{name: "a registered scanner passes", repo: tree(exemptHeader, "internal/b"), registry: registered},
		{name: "an exempt scanner passes", repo: tree(exemptHeader+"internal/b TestScan # walks a fixture\n", "internal/b")},
		{name: "one line per offender", repo: tree(exemptHeader, "internal/b", "internal/c"), wantMsg: "tree-scanning tests are neither registered nor exempt:\n" + finding("internal/b") + "\n" + finding("internal/c")},
		{name: "a missing exemption file is an error", repo: tree("", "internal/b"), wantErr: "reading the exemption file"},
		{name: "an exemption line without a reason is an error", repo: tree("internal/b TestScan\n", "internal/b"), wantErr: "parsing the exemption file"},
		{name: "an unparseable test file is an error", repo: fstest.MapFS{scanningExemptionsPath: {Data: []byte(exemptHeader)}, "internal/b/x_test.go": {Data: []byte("package p\nfunc TestScan( {\n")}}, wantErr: "scanning the source tree"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, err := treeScanningRegistryFailures(tc.repo, tc.registry)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("treeScanningRegistryFailures = %v", err)
			}
			if msg := treeScanningRegistryFailure(lines); msg != tc.wantMsg {
				t.Errorf("failure text = %q, want %q", msg, tc.wantMsg)
			}
		})
	}
}
