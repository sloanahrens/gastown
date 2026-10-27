package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A stat error that is not absence on a legacy directive file is an error the
// read reports, not an absent file (gt-80rnu). The legacy town directory is a
// regular file here, so stat of a file beneath it fails with ENOTDIR, not
// ErrNotExist; a reader that treated that as absence would render no directive
// and pass.
func TestDirectiveLegacyStatErrorFailsClosed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.WriteFile(filepath.Join(town, "directives"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRoleDirectives("polecat", town, "alpha"); err == nil {
		t.Fatal("ReadRoleDirectives returned no error for a legacy stat failure that is not absence")
	}
}

// directoryJoin matches a filepath.Join whose argument list ends in the string
// "directives", or a string concatenation ending in "/directives": the two ways a
// directory path is built from parts.
var directoryJoin = regexp.MustCompile(`filepath\.Join\([^)]*"directives"\s*[,)]|"/directives"`)

// TestNoDirectivePathBuiltOutsideConfig: every directive path is built by the
// config package (directivePaths, CentralDirectiveDir, RigDirectiveDir and the
// helpers beside them), so a Go file outside it that joins a path ending in
// "directives" fails here. The allowlist starts empty (gt-80rnu).
func TestNoDirectivePathBuiltOutsideConfig(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	var offenders []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			if d.IsDir() {
				if filepath.ToSlash(rel) == "internal/config" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for i, line := range strings.Split(string(data), "\n") {
				if directoryJoin.MatchString(line) {
					offenders = append(offenders, filepath.ToSlash(rel)+":"+strconv.Itoa(i+1))
				}
			}
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
	}
	if len(offenders) > 0 {
		t.Errorf("directive paths built outside internal/config (use config.DirectivePaths, DirectiveEditPath or DirectiveTreeRels): %s", strings.Join(offenders, ", "))
	}
}
