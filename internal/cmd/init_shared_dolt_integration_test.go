//go:build integration

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestIntegrationSchedulerInitArgsKeepPortArgv(t *testing.T) {
	base := []string{"init", "--quiet", "--non-interactive", "--skip-hooks", "--skip-agents", "--prefix", "gt"}
	tests := []struct {
		name string
		port string
		want []string
	}{
		{"no port keeps the bare argv", "", base},
		{"a port adds the external server flags", "41234", append(append([]string{}, base...), "--server", "--external", "--server-port", "41234")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := schedulerInitArgs("gt", tt.port); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("schedulerInitArgs(gt, %q) = %q, want %q", tt.port, got, tt.want)
			}
		})
	}
}

// doltPortEnvKeys are the variables any one of which points bd at a Dolt
// server; the guard refuses an init only when all three are empty
// (gt-x0dmc). schedulerDoltPort carries the same list in
// scheduler_integration_test.go, which the gt-x0dmc guard owns.
var doltPortEnvKeys = []string{"GT_DOLT_PORT", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_PORT"}

// TestIntegrationDoltInitRefusesWithoutPort pins the guard the init helpers
// run before any git or bd work: with no port variable set it refuses, and
// any one of the three set lets the init proceed.
func TestIntegrationDoltInitRefusesWithoutPort(t *testing.T) {
	t.Run("no port set refuses", func(t *testing.T) {
		for _, key := range doltPortEnvKeys {
			t.Setenv(key, "")
		}
		err := doltInitPortError()
		if err == nil || !strings.Contains(err.Error(), "refusing bd init") {
			t.Fatalf("doltInitPortError() = %v, want a refusal", err)
		}
	})

	for _, key := range doltPortEnvKeys {
		t.Run(key+" set allows", func(t *testing.T) {
			for _, other := range doltPortEnvKeys {
				t.Setenv(other, "")
			}
			t.Setenv(key, "41234")
			if err := doltInitPortError(); err != nil {
				t.Fatalf("doltInitPortError() with %s set = %v, want nil", key, err)
			}
		})
	}
}

// The child process the test below spawns reads these: childInitHelperEnv
// says it was spawned for this probe, and the fake bd it puts first on PATH
// records that it ran by creating the file named in childFakeBDMarkerEnv.
const (
	childInitHelperEnv   = "GT_TEST_INIT_HELPER"
	childFakeBDMarkerEnv = "GT_TEST_FAKE_BD_MARKER"
)

// TestIntegrationDoltInitGuardStartsNoProcess proves end to end what
// TestIntegrationDoltInitRefusesWithoutPort pins on the guard alone: with no
// port set, initBeadsDBWithPrefix fails before any bd process starts
// (gt-wtzzc). The child runs the real helper with no port and a fake bd first
// on PATH, so a bd it reached would leave the marker file behind.
func TestIntegrationDoltInitGuardStartsNoProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake bd is a shell script")
	}
	bin, err := os.Executable()
	if err != nil {
		t.Fatalf("cannot resolve test binary: %v", err)
	}
	fakeBin := t.TempDir()
	script := "#!/bin/sh\ntouch \"$" + childFakeBDMarkerEnv + "\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}

	// Both Docker settings a host can present: a child that fell through to
	// the harness would start a container with the opt-in and fail without
	// it, instead of refusing.
	for _, docker := range []string{"", "1"} {
		name := "GT_TEST_DOCKER unset"
		if docker != "" {
			name = "GT_TEST_DOCKER=" + docker
		}
		t.Run(name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "bd-started")
			cmd := exec.Command(bin, "-test.run="+childRunPattern, "-test.v", "-test.count=1")
			cmd.Env = childInitEnv(fakeBin, marker, docker)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("the child exited 0; a bd init with no Dolt port set must fail:\n%s", out)
			}
			if !strings.Contains(string(out), "--- FAIL: "+childTestName) {
				t.Fatalf("the child never ran %s, so it proved nothing:\n%s", childTestName, out)
			}
			if !strings.Contains(string(out), "refusing bd init") {
				t.Fatalf("the child failed without the guard's refusal, so the guard did not stop it:\n%s", out)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("bd ran (the fake bd created %s) with no Dolt port set:\n%s", marker, out)
			}
		})
	}
}

// childInitEnv is the child's environment: it must inherit no port (the guard
// stands down when one is set) and no Docker opt-in (the harness would start
// a container), and it must find the fake bd before any real one.
func childInitEnv(fakeBin, marker, docker string) []string {
	env := append([]string{}, os.Environ()...)
	for _, key := range doltPortEnvKeys {
		env = beads.StripEnvKey(env, key)
	}
	for _, key := range []string{"GT_TEST_DOCKER", "PATH"} {
		env = beads.StripEnvKey(env, key)
	}
	env = append(env,
		childNoContainerEnv+"=1",
		childInitHelperEnv+"=1",
		childFakeBDMarkerEnv+"="+marker,
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	if docker != "" {
		env = append(env, "GT_TEST_DOCKER="+docker)
	}
	return env
}

// TestIntegrationHelperInitWithoutPortChild is spawned by
// TestIntegrationDoltInitGuardStartsNoProcess. In the package's own run it
// would init a real bd DB against the container, which proves the opposite of
// what the parent asserts, so it runs only in the spawned child.
func TestIntegrationHelperInitWithoutPortChild(t *testing.T) {
	if os.Getenv(childInitHelperEnv) != "1" {
		t.Skip("not invoked as the no-port init child")
	}
	initBeadsDBWithPrefix(t, t.TempDir(), "zz")
}
