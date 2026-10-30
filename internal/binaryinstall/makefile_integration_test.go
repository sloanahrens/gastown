//go:build integration

package binaryinstall

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationMakeInstallLocalRecipe pins the line make install-local runs
// to swap the built binary in: the Go command, the built binary, the install
// directory, then the binary name. make -n prints the recipes and executes
// none of them, so nothing is built, copied or signed. make runs in a clean
// environment (PATH only), so an inherited MAKEFLAGS or HOME cannot change the
// line.
func TestIntegrationMakeInstallLocalRecipe(t *testing.T) {
	buildDir := t.TempDir()
	installDir := t.TempDir()
	args := []string{"-n", "--no-print-directory", "install-local", "BUILD_DIR=" + buildDir, "INSTALL_DIR=" + installDir}
	cmd := exec.Command("make", args...)
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	want := "go run ./cmd/install-binary " + filepath.Join(buildDir, "gt") + " " + installDir + " gt"
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "go run ./cmd/install-binary") {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 || lines[0] != want {
		t.Errorf("make install-local printed the install lines %q\nwant exactly one: %q", lines, want)
	}
}
