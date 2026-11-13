package update

import (
	"context"
	"fmt"
	"strings"
)

// RemoteMainSHA is the tip of origin's main, read with ls-remote through the
// rig's bare repository at bareRepo. It never fetches. Both the run's plan and
// the daemon's failed-run hold read main through it, so they compare the same
// tip. A checkout's own origin/main ref lags whenever nothing has fetched since
// main moved, so it is not read. An empty answer or a failed ls-remote is an
// error, never a blank sha.
func RemoteMainSHA(ctx context.Context, run RunFunc, bareRepo string) (string, error) {
	out, exit, err := run(ctx, "", nil, "git", "--git-dir", bareRepo, "ls-remote", "origin", "refs/heads/main")
	if err != nil {
		return "", fmt.Errorf("running git ls-remote for main in %s: %w", bareRepo, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("git ls-remote for main in %s exited %d", bareRepo, exit)
	}
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("git ls-remote for main in %s named no ref", bareRepo)
	}
	return fields[0], nil
}
