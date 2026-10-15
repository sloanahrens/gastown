//go:build darwin

package hostcpu

import (
	"context"
	"os/exec"
)

// topOutput runs top twice and returns both samples. macOS reports the CPU
// usage since boot in the first one, so only the second carries a reading of
// now; -n 0 skips the process table, which nothing here reads.
func topOutput(ctx context.Context) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/bin/top", "-l", "2", "-n", "0", "-s", "1").Output()
}
