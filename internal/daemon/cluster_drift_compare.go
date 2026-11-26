package daemon

import (
	"context"
	"fmt"
)

// clusterDriftOutputLimit caps each stream the drift child writes: 64 KiB, so a
// large compare cannot grow the daemon's memory (gt-jokegc).
const clusterDriftOutputLimit = 64 << 10

// newClusterDriftCompare runs gt town cluster drift through start as a bounded
// child of the binary exe resolves, and answers with parseClusterDriftRun of
// that run. A failed lookup is an error and the child never starts, so the
// unreadable item is raised, never a clear (gt-jokegc).
func newClusterDriftCompare(
	exe func() (string, error),
	start func(ctx context.Context, exe string, args []string, limit int) ([]byte, []byte, int, error),
) func(ctx context.Context) (clusterDriftResult, error) {
	return func(ctx context.Context) (clusterDriftResult, error) {
		path, err := exe()
		if err != nil {
			return clusterDriftResult{}, fmt.Errorf("resolving the daemon's own binary for %s: %w", clusterDriftCommand, err)
		}
		stdout, stderr, exit, runErr := start(ctx, path, []string{"town", "cluster", "drift"}, clusterDriftOutputLimit)
		return parseClusterDriftRun(exit, stdout, stderr, runErr)
	}
}
