package update

import (
	"context"
	"fmt"
	"strings"
)

// RemoteMainSHA is the tip of origin's main, read with ls-remote in repoRoot.
// It never fetches. Both the run's plan and the daemon's failed-run hold read
// main through it, so they compare the same tip; the checkout's own origin/main
// ref lags whenever nothing has fetched since it moved. An empty answer or a
// failed ls-remote is an error, never a blank sha.
func RemoteMainSHA(ctx context.Context, run RunFunc, repoRoot string) (string, error) {
	out, exit, err := run(ctx, "", nil, "git", "-C", repoRoot, "ls-remote", "origin", "refs/heads/main")
	if err != nil {
		return "", fmt.Errorf("running git ls-remote for main in %s: %w", repoRoot, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("git ls-remote for main in %s exited %d", repoRoot, exit)
	}
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", fmt.Errorf("git ls-remote for main in %s named no ref", repoRoot)
	}
	return fields[0], nil
}
