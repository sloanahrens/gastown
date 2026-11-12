//go:build integration

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestIntegrationWrapperRunsGTInTheDaemon builds the wrapper as gt and runs
// `gt status --line` through it against the daemon pod of the control cluster:
// the one place the built binary, its config file and the exec path are real
// together. The kubernetes target's own integration test drives the target type
// directly and never builds the wrapper.
//
// It reads a live cluster, so it runs only when WRAP_TEST_CLUSTER=1. The name
// is not that test's GT_TEST_K8S because this package's TestMain runs the
// hermetic harness, which removes every GT_ variable, so a GT_ gate could never
// open here. Past that gate every refusal, the wrapper's doctor included, is a
// failure of this checkout and must not read as "no cluster here".
func TestIntegrationWrapperRunsGTInTheDaemon(t *testing.T) {
	if os.Getenv("WRAP_TEST_CLUSTER") != "1" {
		t.Skip("WRAP_TEST_CLUSTER is not 1, so the control cluster is not reached")
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("WRAP_TEST_CLUSTER is 1 but KUBECONFIG is empty: the test's HOME is a temp directory, so the cluster client finds no kubeconfig unless KUBECONFIG names one")
	}
	bin := buildWrapper(t)
	env := append(wrapperEnv(t), "KUBECONFIG="+kubeconfig)
	if out, stderr, err := runWrapper(bin, env, doctorFlag); err != nil {
		t.Fatalf("the wrapper's doctor = %v\n%s%s", err, out, stderr)
	}
	// The line is on stdout alone: the wrapper may note on stderr that the
	// caller's directory is outside the town.
	out, stderr, err := runWrapper(bin, env, "status", "--line")
	if err != nil {
		t.Fatalf("gt status --line = %v\n%s%s", err, out, stderr)
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

// wrapperEnv is the environment the wrapper runs with: the live town named so
// the tool finds it, a temp HOME holding a config file for the kubernetes target
// so no operator config is read, and PATH so the target finds the cluster
// client. The hermetic harness's forbidden-root variable is left out, so the gt
// in the pod resolves the town this test runs in.
func wrapperEnv(t *testing.T) []string {
	t.Helper()
	// The town is the one this checkout sits in, found from this source file:
	// the hermetic harness moves the working directory into a sandbox town.
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find this source file")
	}
	dir := filepath.Dir(file)
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
		t.Skipf("no town root above %s, so there is no town to run against", dir)
	}
	home := t.TempDir()
	configDir := filepath.Join(home, ".config", "gt")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("create the config directory: %v", err)
	}
	config := `{"target":"kubernetes","context":"colima-gt-control","namespace":"gt","deployment":"daemon","container":"daemon"}` + "\n"
	if err := os.WriteFile(filepath.Join(configDir, "wrap.json"), []byte(config), 0o644); err != nil {
		t.Fatalf("write the target config file: %v", err)
	}
	return []string{
		"HOME=" + home,
		"GT_TOWN_ROOT=" + town,
		"PATH=" + os.Getenv("PATH"),
	}
}

// runWrapper runs the built wrapper and returns its standard output, its
// standard error and how it ended.
func runWrapper(bin string, env []string, args ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(bin, args...) //nolint:gosec // G204: bin is the wrapper this test just built, and args are fixed
	cmd.Env = env
	var out, errOut strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}
