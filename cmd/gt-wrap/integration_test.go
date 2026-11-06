//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationWrapperRunsGTInTheDaemon builds the wrapper as gt and runs
// `gt status --line` through it against the live container: the one place the
// docker target is real.
//
// Docker being down is the only reason to skip. Every other refusal, the
// wrapper's doctor included, is a failure of this checkout and must not read as
// "no Docker here".
func TestIntegrationWrapperRunsGTInTheDaemon(t *testing.T) {
	bin := buildWrapper(t)
	env := wrapperEnv(t)
	if out, err := dockerInfo(t, env); err != nil {
		t.Skipf("Docker is down, so the live daemon container cannot be reached: %v\n%s", err, out)
	}
	if out, err := runWrapper(bin, env, doctorFlag); err != nil {
		t.Fatalf("the wrapper's doctor = %v\n%s", err, out)
	}
	out, err := runWrapper(bin, env, "status", "--line")
	if err != nil {
		t.Fatalf("gt status --line = %v\n%s", err, out)
	}
	line := strings.TrimSpace(out)
	if line == "" {
		t.Fatal("gt status --line printed no line")
	}
	if strings.Contains(line, "\n") {
		t.Fatalf("gt status --line printed more than one line:\n%s", line)
	}
}

// buildWrapper builds the wrapper the test runs. CGO_ENABLED=0 keeps it static
// and keeps the build off the cgo path: the wrapper's dependency graph holds no
// cgo package, so the icu4c headers AGENTS.md warns about for a bare `go build`
// are not on its way (they reach cmd/gt through its dolt dependencies).
func buildWrapper(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "gt")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the wrapper: %v\n%s", err, out)
	}
	return bin
}

// dockerInfo asks the engine the wrapper reaches whether it answers. It is the
// test's only skip condition: the wrapper's own doctor is then a check on this
// checkout, not the judge of whether to run the test.
func dockerInfo(t *testing.T, env []string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "docker", "info")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// wrapperEnv is the environment the wrapper runs with: the live town named so
// the compose file is found, a temp HOME so no operator config is read, and
// PATH so the docker target finds the docker client. The hermetic harness's
// forbidden-root variable is left out, so the gt inside the container resolves
// the town this test runs in.
func wrapperEnv(t *testing.T) []string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get the working directory: %v", err)
	}
	town := ""
	for {
		if _, err := os.Stat(filepath.Join(dir, townMarker)); err == nil {
			town = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	if town == "" {
		t.Skipf("no town root above %s, so there is no compose file to run against", dir)
	}
	return []string{
		"HOME=" + t.TempDir(),
		"GT_TOWN_ROOT=" + town,
		"PATH=" + os.Getenv("PATH"),
	}
}

// runWrapper runs the built wrapper and returns its combined output and how it
// ended.
func runWrapper(bin string, env []string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...) //nolint:gosec // G204: bin is the wrapper this test just built, and args are fixed
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return string(out), err
}
