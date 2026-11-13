//go:build integration

package metrics

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The default workspace finder reads the process's own working directory and
// environment, which the unit tier forbids (docs/testing.md: a test that needs
// the process moves to the integration tier). These cases drive the production
// default from a fixture town the test changes into.

// TestIntegrationRealEnvWorkspaceFinderDefault drives the production default:
// with no WorkspaceRoot and no injected Getenv, the wiring resolves the town
// the process runs in. Removing the default makes the town Home/gt, as it was
// in the daemon pod before gt-kjm3g8.
func TestIntegrationRealEnvWorkspaceFinderDefault(t *testing.T) {
	town := t.TempDir()
	townFile(t, town, "mayor/town.json", `{}`)
	t.Chdir(town)
	// With no injected Getenv the wiring reads the process's TOWN_METRICS_TOWN,
	// which would outrank the workspace; clear it so the case is the workspace
	// level and not the host's setting.
	t.Setenv(envTown, "")
	runner := &realRunner{answer: realGit(town)}
	real, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Home: "/home", Files: seamFiles{}.seam(), Run: runner.run,
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	// The finder starts from the process's own working directory, so compare
	// against the directory it reads rather than the path t.TempDir named: a
	// host that reaches the same directory by another path (a symlinked temp
	// root) makes those two strings differ.
	want, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if real.Town != want {
		t.Errorf("Town = %q, want the working directory's workspace %s", real.Town, want)
	}
}

// TestIntegrationRealEnvInjectedGetenvIgnoresTheWorkingDirectory: an injected
// Getenv is the caller's whole view of the host, so the default workspace
// finder must not read the process's own working directory and environment
// behind it. Without the guard a test injecting an empty environment would
// still resolve the workspace the test binary runs in.
func TestIntegrationRealEnvInjectedGetenvIgnoresTheWorkingDirectory(t *testing.T) {
	town := t.TempDir()
	townFile(t, town, "mayor/town.json", `{}`)
	t.Chdir(town)
	runner := &realRunner{answer: realGit(town)}
	real, err := RealEnv(context.Background(), RealEnvOptions{
		Today: "smoke", Home: "/home", Getenv: noEnv, Files: seamFiles{}.seam(), Run: runner.run,
	})
	if err != nil {
		t.Fatalf("RealEnv: %v", err)
	}
	if want := filepath.Join("/home", "gt"); real.Town != want {
		t.Errorf("Town = %q, want the home fallback %s: an injected Getenv must not read the host's cwd", real.Town, want)
	}
}
