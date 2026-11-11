package testpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckoutFilesAreParsedOnce: two scans of a checkout directory share one
// parse of each file, so the dozen policy tests read each source file once.
func TestCheckoutFilesAreParsedOnce(t *testing.T) {
	t.Parallel()
	src := sourcesFor(".")
	if !src.cached {
		t.Fatalf("the package directory is not cached")
	}
	a, err := src.parse("scan.go")
	if err != nil {
		t.Fatal(err)
	}
	b, err := sourcesFor(".").parse("scan.go")
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Errorf("scan.go was parsed twice")
	}
	if ok, err := src.matches(".", "scan.go"); err != nil || !ok {
		t.Errorf("matches(scan.go) = %v, %v; want true, nil", ok, err)
	}
}

// TestFixtureDirectoriesAreReadFresh: a directory under the temporary directory
// is a test's own fixture and is never cached, so a file a test rewrites between
// two scans is read again.
func TestFixtureDirectoriesAreReadFresh(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "a_test.go")
	if sourcesFor(dir).cached {
		t.Fatalf("a fixture directory is cached")
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package a\n")
	first, err := sourcesFor(dir).parse(path)
	if err != nil {
		t.Fatal(err)
	}
	write("package a\n\nfunc F() {}\n")
	second, err := sourcesFor(dir).parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Decls) != 0 || len(second.Decls) != 1 {
		t.Errorf("decls before and after the rewrite = %d, %d; want 0, 1", len(first.Decls), len(second.Decls))
	}
}

// TestAFileWhoseBuildConstraintCannotBeReadFailsTheScan: every scan that asks for a
// file's build constraints returns the failure naming the file, never a scan of
// fewer files.
func TestAFileWhoseBuildConstraintCannotBeReadFailsTheScan(t *testing.T) {
	t.Parallel()
	// An unterminated expression in a //go:build line is a constraint go/build refuses.
	const bad = "//go:build (\n\npackage bad\n"
	for name, tc := range map[string]struct {
		file string
		scan func(dir string) error
	}{
		"ScanDir":               {"bad_test.go", func(d string) error { _, err := ScanDir(d); return err }},
		"StartsContainers":      {"bad_test.go", func(d string) error { _, err := StartsContainers(d); return err }},
		"unitTierTestFiles":     {"bad_test.go", func(d string) error { _, _, err := unitTierTestFiles(d); return err }},
		"exportedEntryWrappers": {"bad.go", func(d string) error { _, err := exportedEntryWrappers(d); return err }},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte(bad), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := tc.scan(dir); err == nil || !strings.Contains(err.Error(), "bad") {
				t.Errorf("err = %v, want one naming the file", err)
			}
		})
	}
}

// TestCacheableUnderTellsTheCheckoutFromAFixture: a directory is cached when it is
// inside the checkout, whether or not the checkout sits under the temporary
// directory; a fixture is read fresh. The more specific of the checkout root and
// the temporary directory decides.
func TestCacheableUnderTellsTheCheckoutFromAFixture(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		root  string
		temps []string
		dir   string
		want  bool
	}{
		"checkout beside the temp dir":                 {"/work/repo", []string{"/tmp"}, "/work/repo/internal/a", true},
		"fixture under the temp dir":                   {"/work/repo", []string{"/tmp"}, "/tmp/TestA/001", false},
		"checkout under the temp dir (sweep, CI)":      {"/tmp/ws/repo", []string{"/tmp"}, "/tmp/ws/repo/internal/a", true},
		"fixture beside a checkout under the temp dir": {"/tmp/ws/repo", []string{"/tmp"}, "/tmp/TestA/001", false},
		"temp dir is the checkout's parent":            {"/tmp/ws/repo", []string{"/tmp/ws"}, "/tmp/ws/repo/internal/a", true},
		"temp dir nested inside the checkout":          {"/r", []string{"/r/.tmp"}, "/r/.tmp/TestA/001", false},
		"temp dir is the checkout root itself":         {"/work/repo", []string{"/work/repo"}, "/work/repo/internal/a", false},
		"checkout dir with a nested temp dir":          {"/r", []string{"/r/.tmp"}, "/r/internal/a", true},
		"temp dir spelled two ways":                    {"/work/repo", []string{"/var/f", "/private/var/f"}, "/private/var/f/TestA/001", false},
		"no checkout root found":                       {"", []string{"/tmp"}, "/work/repo", false},
		"a sibling that shares the root's prefix":      {"/work/repo", []string{"/tmp"}, "/work/repo2/a", false},
	} {
		if got := cacheableUnder(tc.root, tc.temps, tc.dir); got != tc.want {
			t.Errorf("%s: cacheableUnder(%q, %q, %q) = %v, want %v", name, tc.root, tc.temps, tc.dir, got, tc.want)
		}
	}
}

// TestModuleRootIsTheNearestGoModAbove: the checkout root is the directory of the
// nearest go.mod at or above the working directory, so a nested module is its own
// root. The walk runs over a fresh temporary tree, and stops at the nearest go.mod
// inside it, so it never reads the host's directories.
func TestModuleRootIsTheNearestGoModAbove(t *testing.T) {
	t.Parallel()
	tree := t.TempDir()
	outer := filepath.Join(tree, "repo")
	inner := filepath.Join(outer, "tools")
	deep := filepath.Join(inner, "internal", "a")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{outer, inner} {
		if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		dir  string
		want string
	}{
		"the module itself":          {outer, outer},
		"a package under the module": {filepath.Join(outer, "cmd"), outer},
		"a nested module wins":       {deep, inner},
	} {
		if got := moduleRootAbove(tc.dir); got != tc.want {
			t.Errorf("%s: moduleRootAbove(%q) = %q, want %q", name, tc.dir, got, tc.want)
		}
	}
}
