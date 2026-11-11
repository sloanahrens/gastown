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
