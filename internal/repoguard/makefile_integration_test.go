//go:build integration

package repoguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// makeLintTimeout bounds the make -n lint call, so a wedged make cannot hold
// the integration tier. A healthy make -n finishes in well under a second.
var makeLintTimeout = 60 * time.Second

// makeLintRecipe runs make -n lint in dir and returns what it printed. The
// call is cut off at makeLintTimeout; the returned error wraps
// context.DeadlineExceeded when the bound fires.
func makeLintRecipe(dir string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), makeLintTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-n", "lint")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return out, fmt.Errorf("make -n lint: %w", ctx.Err())
	}
	return out, err
}

// TestIntegrationMakeLintRunsRepoGuards pins the line make lint runs for the
// repository guards: the Go command, and no shell script. make -n prints the
// recipe and executes none of it, so nothing is linted or built. make runs in a
// clean environment (PATH only), so an inherited MAKEFLAGS cannot change the
// recipe (gt-tpg0p.108).
func TestIntegrationMakeLintRunsRepoGuards(t *testing.T) {
	out, err := makeLintRecipe(filepath.Join("..", ".."))
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

// TestIntegrationMakeLintBoundsItsDuration pins that the make call stops at its
// deadline. A deadline that has already passed fails the call before make
// starts, so the test needs no sleep and no fake make on PATH. Without the
// bound in makeLintRecipe, the call would succeed and this test fails.
func TestIntegrationMakeLintBoundsItsDuration(t *testing.T) {
	saved := makeLintTimeout
	makeLintTimeout = time.Nanosecond
	t.Cleanup(func() { makeLintTimeout = saved })
	_, err := makeLintRecipe(filepath.Join("..", ".."))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("make -n lint under an expired deadline: got %v, want context.DeadlineExceeded", err)
	}
}
