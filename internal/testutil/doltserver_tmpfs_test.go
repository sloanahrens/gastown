//go:build !windows

package testutil

import (
	"slices"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// applyOpts runs the container options against an empty request, so the
// test can read what testcontainers.Run would be asked for without starting
// Docker.
func applyOpts(t *testing.T, env environment) testcontainers.GenericContainerRequest {
	t.Helper()
	var req testcontainers.GenericContainerRequest
	for _, o := range doltContainerOptsIn(env) {
		if err := o.Customize(&req); err != nil {
			t.Fatalf("Customize: %v", err)
		}
	}
	return req
}

// Every Dolt commit fsyncs its journal; on the Docker Desktop VM disk that is
// the dominant per-migration cost. The data dir must be a RAM mount unless
// the caller opted out (claude-yfj design, stage 3).
func TestDoltContainerOpts_TmpfsByDefault(t *testing.T) {
	t.Parallel()
	req := applyOpts(t, newMapEnv(DoltTmpfsEnv+"="))
	if got := req.Tmpfs[doltDataDir]; got != "rw,size=2g" {
		t.Fatalf("Tmpfs[%s] = %q, want %q", doltDataDir, got, "rw,size=2g")
	}
	if req.Env["DOLT_ROOT_HOST"] != "%" {
		t.Fatalf("DOLT_ROOT_HOST = %q, want %%", req.Env["DOLT_ROOT_HOST"])
	}
}

// The dolt-sql-server image's entrypoint logs "Server ready" (dolt's own line)
// and then runs its init SQL through `dolt sql` under `set -e`. A test that
// changes the catalog meanwhile makes that SQL fail, the entrypoint exit 1,
// and the container die under the suite: 2 of 3 containers in a probe that
// churned CREATE/DROP DATABASE from "Server ready", 0 of 3 once the start
// waited for the entrypoint's last line. The start must wait for both lines,
// each under its own deadline, on the published Dolt port.
func TestDoltContainerOpts_WaitsForEntrypointInit(t *testing.T) {
	t.Parallel()
	req := applyOpts(t, newMapEnv())
	logs := waitedForLogs(req.WaitingFor)
	for _, want := range []string{doltServerReadyLog, doltEntrypointDoneLog} {
		if !slices.Contains(logs, want) {
			t.Fatalf("container start waits for logs %q, want %q among them", logs, want)
		}
	}
	if !slices.Contains(req.ExposedPorts, doltContainerPort) {
		t.Fatalf("ExposedPorts = %q, want %s", req.ExposedPorts, doltContainerPort)
	}
}

// waitedForLogs lists every log line a wait strategy waits for.
func waitedForLogs(s wait.Strategy) []string {
	switch w := s.(type) {
	case *wait.LogStrategy:
		return []string{w.Log}
	case *wait.MultiStrategy:
		var logs []string
		for _, inner := range w.Strategies {
			logs = append(logs, waitedForLogs(inner)...)
		}
		return logs
	}
	return nil
}

func TestDoltContainerOpts_TmpfsOptOut(t *testing.T) {
	t.Parallel()
	req := applyOpts(t, newMapEnv(DoltTmpfsEnv+"=0"))
	if _, ok := req.Tmpfs[doltDataDir]; ok {
		t.Fatalf("Tmpfs has %s with %s=0; want it omitted", doltDataDir, DoltTmpfsEnv)
	}
}
