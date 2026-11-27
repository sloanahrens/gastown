package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// clusterDriftOutputLimit caps each stream the drift child writes: 64 KiB, so a
// large compare cannot grow the daemon's memory (gt-jokegc).
const clusterDriftOutputLimit = 64 << 10

// clusterDriftRunBound is the deadline on one drift run. A child that hangs past
// it is ended by its context and answers an error, so the heartbeat is never
// held and the unreadable item is raised, never a clear (gt-ime6b8).
const clusterDriftRunBound = 60 * time.Second

// newClusterDriftCompare runs gt town cluster drift through start as a bounded
// child of the binary exe resolves, and answers with parseClusterDriftRun of
// that run. The child's context carries clusterDriftRunBound as its deadline. A
// failed lookup is an error and the child never starts, so the unreadable item
// is raised, never a clear (gt-jokegc, gt-ime6b8).
func newClusterDriftCompare(
	exe func() (string, error),
	start func(ctx context.Context, exe string, args []string, limit int) ([]byte, []byte, int, error),
) func(ctx context.Context) (clusterDriftResult, error) {
	return func(ctx context.Context) (clusterDriftResult, error) {
		path, err := exe()
		if err != nil {
			return clusterDriftResult{}, fmt.Errorf("resolving the daemon's own binary for %s: %w", clusterDriftCommand, err)
		}
		runCtx, cancel := context.WithTimeout(ctx, clusterDriftRunBound)
		defer cancel()
		stdout, stderr, exit, runErr := start(runCtx, path, []string{"town", "cluster", "drift"}, clusterDriftOutputLimit)
		res, err := parseClusterDriftRun(exit, stdout, stderr, runErr)
		return res, clusterDriftNoGrantError(err)
	}
}

// clusterDriftNoGrantError names the missing drift read grant in a run's error
// when its text says the daemon was forbidden the cluster or a binary the run
// needs was not found (gt-udasjo). The match is case-insensitive so kubectl's
// "(Forbidden)" counts too. Only the first line of the error is kept. A nil
// error, the no-recorded-render error and any other error pass through unchanged.
func clusterDriftNoGrantError(err error) error {
	if err == nil || errors.Is(err, errNoRecordedRender) {
		return err
	}
	text := err.Error()
	lower := strings.ToLower(text)
	if !strings.Contains(lower, "forbidden") && !strings.Contains(lower, "executable file not found") {
		return err
	}
	first, _, _ := strings.Cut(text, "\n")
	return fmt.Errorf("the daemon cannot read the control cluster yet (the drift read grant is not applied): %s", first)
}
