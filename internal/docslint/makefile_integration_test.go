//go:build integration

package docslint

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestIntegrationMakeDocsLintRecipe runs `make -n docs-lint` from the repository
// root and checks the recipe is the Go command, so `make lint` (which depends on
// the docs-lint target) runs the command and not a script that is gone. make -n
// executes no recipe line; the test reads the Makefile and writes nothing.
func TestIntegrationMakeDocsLintRecipe(t *testing.T) {
	cmd := exec.Command("make", "-n", "--no-print-directory", "docs-lint")
	cmd.Dir = "../.."
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n docs-lint: %v\n%s", err, out)
	}
	if got, want := strings.TrimSpace(string(out)), "go run ./cmd/docs-lint"; got != want {
		t.Errorf("make -n docs-lint printed %q, want %q", got, want)
	}
	if _, err := os.Stat("../../scripts/docs-lint.sh"); !os.IsNotExist(err) {
		t.Errorf("scripts/docs-lint.sh is still there (stat error %v)", err)
	}
}
