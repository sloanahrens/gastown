//go:build integration

package repoguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestIntegrationMakeLintRunsRepoGuards pins the line make lint runs for the
// repository guards: the Go command, and no shell script. make -n prints the
// recipe and executes none of it, so nothing is linted or built. make runs in a
// clean environment (PATH only), so an inherited MAKEFLAGS cannot change the
// recipe (gt-tpg0p.108).
func TestIntegrationMakeLintRunsRepoGuards(t *testing.T) {
	cmd := exec.Command("make", "-n", "lint")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n lint: %v\n%s", err, out)
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		lines = append(lines, strings.TrimSpace(line))
	}
	if !slices.Contains(lines, "go run ./cmd/repo-guards") {
		t.Errorf("make lint does not run the repo guards as go run ./cmd/repo-guards; printed:\n%s", out)
	}
	if strings.Contains(string(out), "repo-guards.sh") {
		t.Errorf("make lint still names scripts/repo-guards.sh; printed:\n%s", out)
	}
}
