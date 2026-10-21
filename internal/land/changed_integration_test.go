//go:build integration

package land

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestIntegrationChangedPackagesNamesNonASCIIAndQuotedFiles runs the
// name-status command the producers run, against a real branch whose only
// changes are a non-ASCII file and a file with quotes in its name. Without -z
// git octal-quotes such a name and drops the .go suffix, so the package went
// unselected (gt-fz8xk); with -z both packages are named.
func TestIntegrationChangedPackagesNamesNonASCIIAndQuotedFiles(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "internal/cmd/base.go", "package cmd\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	base := gitT(t, dir, "rev-parse", "HEAD")

	gitT(t, dir, "checkout", "-q", "-b", "branch")
	if err := os.MkdirAll(filepath.Join(dir, "internal", "quote"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeT(t, dir, "internal/cmd/café_test.go", "package cmd\n")
	writeT(t, dir, `internal/quote/say "hi".go`, "package quote\n")
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "branch")

	// Run the argument list exactly as changedpkgs and testsel do, and keep
	// the output byte for byte: the NUL separators are the point.
	cmd := exec.Command("git", append([]string{"-C", dir}, NameStatusArgs(base+"...HEAD")...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git diff: %v", err)
	}

	hasGo := func(d string) bool { return PackageDirHasGo(dir, d) }
	got, err := ChangedPackages(string(out), hasGo)
	if err != nil {
		t.Fatalf("ChangedPackages: %v", err)
	}
	if want := []string{"internal/cmd", "internal/quote"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedPackages = %v, want %v", got, want)
	}

	paths, err := ChangedPaths(string(out))
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if want := []string{"internal/cmd/café_test.go", `internal/quote/say "hi".go`}; !reflect.DeepEqual(paths, want) {
		t.Errorf("ChangedPaths = %q, want %q", paths, want)
	}
}
