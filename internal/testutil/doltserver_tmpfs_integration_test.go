//go:build integration && !windows

package testutil

import (
	"context"
	"io"
	"strings"
	"testing"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// Proves Docker actually mounts tmpfs over the image's declared VOLUME on
// this runtime, on the scratch container (every test container starts with
// the same options).
func TestIntegrationDoltContainerDataDirIsTmpfs(t *testing.T) {
	if getenv(procEnv{}, DoltTmpfsEnv) == "0" {
		t.Skip(DoltTmpfsEnv + "=0 opted this run out of tmpfs")
	}
	LeaseScratchDoltContainer(t)
	ctx := context.Background()
	ctr := scratchDolt.ctr
	code, r, err := ctr.Exec(ctx, []string{"stat", "-f", "-c", "%T", doltDataDir}, tcexec.Multiplexed())
	if err != nil || code != 0 {
		t.Fatalf("stat %s: code=%d err=%v", doltDataDir, code, err)
	}
	out, _ := io.ReadAll(r)
	if fs := strings.TrimSpace(string(out)); fs != "tmpfs" {
		t.Fatalf("%s filesystem = %q, want tmpfs", doltDataDir, fs)
	}
}
