//go:build linux

package hoststat

import (
	"context"
	"os/exec"
)

// topOutput runs top twice in batch mode and returns both samples. Its first
// sample reports the CPU usage since boot, so only the second carries a
// reading of now.
func topOutput(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "top", "-b", "-n", "2", "-d", "1").Output()
}
