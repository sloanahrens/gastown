//go:build !windows

package testutil

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// applyOpts runs the container options against an empty request, so the
// test can read what dolt.Run would be asked for without starting Docker.
func applyOpts(t *testing.T) testcontainers.GenericContainerRequest {
	t.Helper()
	var req testcontainers.GenericContainerRequest
	for _, o := range doltContainerOpts() {
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
	t.Setenv(DoltTmpfsEnv, "")
	req := applyOpts(t)
	if got := req.Tmpfs[doltDataDir]; got != "rw,size=2g" {
		t.Fatalf("Tmpfs[%s] = %q, want %q", doltDataDir, got, "rw,size=2g")
	}
	if req.Env["DOLT_ROOT_HOST"] != "%" {
		t.Fatalf("DOLT_ROOT_HOST = %q, want %%", req.Env["DOLT_ROOT_HOST"])
	}
}

func TestDoltContainerOpts_TmpfsOptOut(t *testing.T) {
	t.Setenv(DoltTmpfsEnv, "0")
	req := applyOpts(t)
	if _, ok := req.Tmpfs[doltDataDir]; ok {
		t.Fatalf("Tmpfs has %s with %s=0; want it omitted", doltDataDir, DoltTmpfsEnv)
	}
}

// Proves Docker actually mounts tmpfs over the image's declared VOLUME on
// this runtime. Starts and terminates its own container so it never touches
// the package's shared one.
func TestDoltContainer_DataDirIsTmpfs(t *testing.T) {
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Skip("Docker not available, skipping test")
	}
	t.Setenv(DoltTmpfsEnv, "")
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		if isDockerUnavailableErr(err) {
			t.Skipf("Dolt container unavailable: %v", err)
		}
		t.Fatalf("starting Dolt container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Errorf("terminating Dolt container: %v", err)
		}
	})
	code, r, err := ctr.Exec(ctx, []string{"stat", "-f", "-c", "%T", doltDataDir}, tcexec.Multiplexed())
	if err != nil || code != 0 {
		t.Fatalf("stat %s: code=%d err=%v", doltDataDir, code, err)
	}
	out, _ := io.ReadAll(r)
	if fs := strings.TrimSpace(string(out)); fs != "tmpfs" {
		t.Fatalf("%s filesystem = %q, want tmpfs", doltDataDir, fs)
	}
}
