package tiersweep

import (
	"path"
	"regexp"
	"sort"
	"strings"
)

// Source is the git and file seam discovery reads through. LsFiles lists the
// tracked files under dir that match pattern, as git ls-files prints them, and
// ReadFile reads one file, as os.ReadFile does. Tests inject a fake.
type Source interface {
	LsFiles(dir, pattern string) ([]string, error)
	ReadFile(name string) ([]byte, error)
}

// integrationTag matches a //go:build line that names the integration tag. It
// is the regexp the script's tagged_dirs greps for, applied per line: a
// "!integration" tag does not match, and "integration_other" does.
var integrationTag = regexp.MustCompile(`(?m)^//go:build.*(^|[^!])integration`)

// NestedModules returns the directory of each go.mod below the root, as the
// Makefile's NESTED_MODULES finds them. A file is a module when its final
// element is exactly go.mod, so extra/notgo.mod is not one. A module whose
// path starts with testdata/ or vendor/, or has /testdata/ or /vendor/ in it,
// is left out. The result is sorted bytewise and holds each directory once.
func NestedModules(files []string) []string {
	var dirs []string
	for _, f := range files {
		if path.Base(f) != "go.mod" || inTestdataOrVendor(f) {
			continue
		}
		if dir := path.Dir(f); dir != "." {
			dirs = append(dirs, dir)
		}
	}
	return sortedUnique(dirs)
}

// inTestdataOrVendor reports whether a repo-relative path lies under a testdata
// or vendor directory at any depth, including the root.
func inTestdataOrVendor(f string) bool {
	return strings.HasPrefix(f, "testdata/") || strings.Contains(f, "/testdata/") ||
		strings.HasPrefix(f, "vendor/") || strings.Contains(f, "/vendor/")
}

// TaggedDirs returns the directories of the test files that carry the
// integration build tag, as ./dir (./ for the module root). Only *_test.go
// files count, and one under /testdata/ does not. A root-level testdata/ file
// is kept: the script's grep -v /testdata/ needs something before the slash,
// and the port keeps that gap, pinned by a test.
//
// A file that read cannot open is kept too. Dropping it would skip its
// package's integration tests without a word, so the tier runs the package
// and the failure shows.
//
// The result is sorted bytewise and holds each directory once.
func TaggedDirs(files []string, read func(string) ([]byte, error)) []string {
	var dirs []string
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") || strings.Contains(f, "/testdata/") {
			continue
		}
		data, err := read(f)
		if err == nil && !integrationTag.Match(data) {
			continue
		}
		if dir := path.Dir(f); dir == "." {
			dirs = append(dirs, "./")
		} else {
			dirs = append(dirs, "./"+dir)
		}
	}
	return sortedUnique(dirs)
}

// InsideModule reports whether dir is one of modules or below one. Both
// arguments may be in either form: NestedModules prints a bare directory
// (tracker) and TaggedDirs prints ./tracker, and both name the same place.
// The root (./ or .) is inside no module, because NestedModules never lists
// it. So ./tracker2 is not inside tracker, and ./tracker/sub is.
func InsideModule(dir string, modules []string) bool {
	d := cleanDir(dir)
	if d == "." {
		return false
	}
	for _, m := range modules {
		mc := cleanDir(m)
		if d == mc || strings.HasPrefix(d, mc+"/") {
			return true
		}
	}
	return false
}

// cleanDir puts a directory in the bare form NestedModules prints: no leading
// ./, no trailing slash, and path.Clean applied. The root comes back as ".".
func cleanDir(p string) string {
	return path.Clean(strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/"))
}

// DockerPackages returns the packages a docker.txt lists, in file order, each
// as ./pkg. A # starts a comment that runs to the end of its line. Blank and
// comment-only lines are skipped, and of a line's fields only the first counts.
func DockerPackages(text string) []string {
	var pkgs []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if fields := strings.Fields(line); len(fields) > 0 {
			pkgs = append(pkgs, "./"+fields[0])
		}
	}
	return pkgs
}

// sortedUnique sorts items bytewise, as LC_ALL=C sort does, and drops repeats.
func sortedUnique(items []string) []string {
	sort.Strings(items)
	var out []string
	for i, s := range items {
		if i == 0 || s != items[i-1] {
			out = append(out, s)
		}
	}
	return out
}
