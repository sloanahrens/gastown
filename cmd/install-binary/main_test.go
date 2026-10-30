package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// The Makefile must never copy the built binary straight over the installed one
// (gt-0het): that copy is not atomic, so a reader can see a partial binary. The
// recipe is read from the repo, not from the developer's environment.
func TestMakefileNeverCopiesTheBinaryIntoTheInstallDir(t *testing.T) {
	t.Parallel()
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read the Makefile: %v", err)
	}
	direct := regexp.MustCompile(`cp\s+\$\(BUILD_DIR\)/\$\(BINARY\)\s+\$\(INSTALL_DIR\)`)
	if direct.Match(makefile) {
		t.Error("the Makefile copies the built binary directly into the install dir (non-atomic)")
	}
}

// A usage error exits 1 before any file is touched, the script's exit status
// for a missing argument.
func TestRunWithNoArgumentsExitsOneWithUsage(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	if got := run(nil, &stderr); got != 1 {
		t.Fatalf("run() = %d, want 1", got)
	}
	want := "usage: install-binary <built-binary> <install-dir> [name]\n"
	if stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}

// A missing source exits 1 and leaves the install directory uncreated, so the
// refusal happens before any write to the place the binary is installed.
func TestRunWithAMissingSourceLeavesTheInstallDirUncreated(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "install-dir")
	var stderr bytes.Buffer
	if got := run([]string{filepath.Join(dir, "no-such-binary"), dest}, &stderr); got != 1 {
		t.Fatalf("run() = %d, want 1", got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Errorf("install dir %s exists after a refused install (stat: %v)", dest, err)
	}
	if want := "install-binary: source binary not found: " + filepath.Join(dir, "no-such-binary") + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}
}
