package daemon

import (
	"bytes"
	"context"
	"os/exec"

	"github.com/steveyegge/gastown/internal/util"
)

// gtCall is one gt subprocess the daemon runs: the argv after the binary, the
// working directory and the complete environment (nil inherits the daemon's).
type gtCall struct {
	dir  string
	env  []string
	args []string
}

// gtRunFunc runs one gt call with the binary at gtPath and returns its stdout
// and stderr. The daemon's gt invocations go through one, so tests can record
// and answer them without starting processes. Errors that carry an exit
// status implement interface{ ExitCode() int }, as *exec.ExitError does.
type gtRunFunc func(ctx context.Context, gtPath string, c gtCall) (stdout, stderr []byte, err error)

// runGtProcess is the real gtRunFunc: gt in its own process group, so a
// cancelled ctx kills the whole tree it started.
func runGtProcess(ctx context.Context, gtPath string, c gtCall) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, gtPath, c.args...) //nolint:gosec // G204: gtPath resolved at daemon init, args built internally
	cmd.Dir = c.dir
	cmd.Env = c.env
	util.SetProcessGroup(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
