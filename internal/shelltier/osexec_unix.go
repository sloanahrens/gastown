//go:build unix

package shelltier

import (
	"context"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// killWaitDelay bounds how long Wait waits for a child that still holds the log
// pipe after the script exits or is killed. It is longer than
// util.ProcessGroupKillGrace, so a group that ignores SIGTERM has been
// SIGKILLed before Wait gives up on it.
const killWaitDelay = 5 * time.Second

// newCommand builds the command for c without starting it: it runs in c.Dir
// with c.Env added to the inherited environment, under ctx, and Wait is
// bounded by killWaitDelay.
func newCommand(ctx context.Context, c Command) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	util.SetProcessGroup(cmd)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.WaitDelay = killWaitDelay
	return cmd
}

// startProcess runs c with os/exec and returns what cmd.Run returned. The
// group is killed by util.SetProcessGroup (internal/util/exec_unix.go:33), which
// sets Setpgid and installs the Cancel hook that signals the whole group, so
// the budget's kill reaches the scripts and tools the command started, not only
// the shell that was named.
func startProcess(ctx context.Context, c Command, log io.Writer) error {
	cmd := newCommand(ctx, c)
	cmd.Stdout = log
	cmd.Stderr = log
	return cmd.Run()
}
