//go:build integration

package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestIntegrationMakePolecatCachesRefusesUnplacedSeedJob runs the real
// `make polecat-caches` with neither POLE_CACHE_SANDBOX nor
// POLE_CACHE_PLAIN_CLUSTER set. The recipe's first line refuses before its Go
// command runs, so the refusal is the only output and make exits 2. Without
// that guard the recipe falls through to `go run`, which prints something else
// and fails, so the refusal text is what the test checks.
func TestIntegrationMakePolecatCachesRefusesUnplacedSeedJob(t *testing.T) {
	cmd := exec.Command("make", "--no-print-directory", "polecat-caches")
	cmd.Dir = "../.."
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("make polecat-caches: want exit 2, got %v\n%s", err, out)
	}
	for _, want := range []string{"POLE_CACHE_SANDBOX is empty", "POLE_CACHE_PLAIN_CLUSTER=1"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("make polecat-caches output lacks %q:\n%s", want, out)
		}
	}
}
