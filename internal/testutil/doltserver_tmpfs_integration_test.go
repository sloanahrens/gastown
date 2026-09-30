//go:build integration && !windows

package testutil

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

// Proves Docker actually mounts tmpfs over the image's declared VOLUME on
// this runtime. Starts and terminates its own container so it never touches
// the package's shared one.
func TestIntegrationDoltContainerDataDirIsTmpfs(t *testing.T) {
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Fatal(dockerMissingMsg)
	}
	t.Setenv(DoltTmpfsEnv, "")
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
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
