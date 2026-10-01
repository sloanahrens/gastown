package land

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// TestPolicyPackage is the package that enforces the repo's test rules.
const TestPolicyPackage = "internal/testpolicy"

// ChangedPackages maps `git diff --name-status -M` output to the Go package
// directories whose tests the change can affect, sorted and relative to the
// repo root ("internal/land", never "./internal/land").
//
// A .go file names its own directory. A file of any other kind (testdata, an
// embedded template, a fixture) names the nearest ancestor directory that
// holds Go files, because that package is the one that reads it; a file with
// no such ancestor, such as docs/x.md, names nothing. Both sides of a rename
// and a deleted file count, so a package that lost a file is still tested. A
// directory with no Go files left is dropped: `go test` of a deleted package
// is an error, not a pass.
//
// A branch that adds, modifies or renames to any _test.go file also names
// TestPolicyPackage: its scan rules (no sleep polls, no env or parallel misuse)
// judge test files across the whole tree, so the package that changed is not
// the one that fails. Deleting a test file cannot break a rule and does not
// add it.
//
// hasGo reports whether a repo-relative directory holds a non-test or test
// .go file; tests replace it, PackageDirHasGo is the real one.
func ChangedPackages(nameStatus string, hasGo func(dir string) bool) []string {
	seen := map[string]bool{}
	for _, line := range strings.Split(nameStatus, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) < 2 {
			continue
		}
		// fields[0] is the status (M, A, D, R100, C75 ...); the rest are
		// paths: one, or old and new for a rename or copy.
		for _, file := range fields[1:] {
			if dir := owningPackage(file, hasGo); dir != "" {
				seen[dir] = true
			}
		}
		// The last path is the file as it stands after the change.
		if !strings.HasPrefix(fields[0], "D") && strings.HasSuffix(path.Clean(filepath.ToSlash(fields[len(fields)-1])), "_test.go") && hasGo(TestPolicyPackage) {
			seen[TestPolicyPackage] = true
		}
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs
}

// owningPackage is the package directory file belongs to, or "".
func owningPackage(file string, hasGo func(dir string) bool) string {
	file = path.Clean(filepath.ToSlash(file))
	dir := path.Dir(file)
	if strings.HasSuffix(file, ".go") {
		if hasGo(dir) {
			return dir
		}
		return ""
	}
	for ; dir != "." && dir != "/"; dir = path.Dir(dir) {
		if hasGo(dir) {
			return dir
		}
	}
	return ""
}

// PackageDirHasGo reports whether repoRoot/dir holds a .go file directly.
func PackageDirHasGo(repoRoot, dir string) bool {
	entries, err := os.ReadDir(filepath.Join(repoRoot, filepath.FromSlash(dir)))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
