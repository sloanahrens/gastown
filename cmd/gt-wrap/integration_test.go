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
// docker target is real. The wrapper's doctor runs first, and a ladder with no
// rung skips the test with the doctor's report, since there is then no
// container to run the tool in.
func TestIntegrationWrapperRunsGTInTheDaemon(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "gt")
	build := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the wrapper: %v\n%s", err, out)
	}
	env := wrapperEnv(t)
	if out, err := runWrapper(bin, env, doctorFlag); err != nil {
		t.Skipf("the wrapper's doctor reports no usable rung, so there is no container to run the tool in: %v\n%s", err, out)
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
