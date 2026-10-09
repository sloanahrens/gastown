package daemon

import (
	"bytes"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/util"
)

// daemonCmdWaitDelay bounds how long exec waits on a daemon-launched
// command's output pipes once the command itself is gone. A child that
// inherited the pipe holds the copy open and the group kill cannot reach it:
// either the command had already exited, so exec had nothing left to cancel,
// or the child left the group. Unbounded, the caller's buffer copy — and the
// single-flight guard behind it — outlive the command by as long as that
// child lives (gt-7uyfc).
const daemonCmdWaitDelay = 5 * time.Second

// boundDaemonCmd gives a daemon-launched command a process group of its own
// and the pipe WaitDelay above, so a context deadline reaches the whole tree
// and the wait on its output is bounded either way. cmd must come from
// exec.CommandContext, as SetProcessGroup requires.
//
// runCmd and combinedOutput are the daemon's own seams, and this runs in
// both: the commands built for them are bounded here rather than at each
// call site (gt-7uyfc).
func boundDaemonCmd(cmd *exec.Cmd) *exec.Cmd {
	util.SetProcessGroup(cmd)
	cmd.WaitDelay = daemonCmdWaitDelay
	return cmd
}

// cmdRunFunc runs a subprocess the daemon has built — argv, directory and
// environment already set — and returns its stdout and stderr. The daemon's
// gt, bd and dolt invocations go through one, so tests can record and answer
// them without starting processes. Errors that carry an exit status implement
// interface{ ExitCode() int }, as *exec.ExitError does.
type cmdRunFunc func(cmd *exec.Cmd) (stdout, stderr []byte, err error)

// runCmdProcess is the real cmdRunFunc: it runs cmd, capturing whichever of
// stdout and stderr the caller left unset.
func runCmdProcess(cmd *exec.Cmd) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	if cmd.Stdout == nil {
		cmd.Stdout = &stdout
	}
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// runWith runs cmd through run, or for real when run is nil.
func runWith(run cmdRunFunc, cmd *exec.Cmd) ([]byte, []byte, error) {
	if run == nil {
		return runCmdProcess(cmd)
	}
	return run(cmd)
}

// combinedOutputWith runs cmd through run with stdout and stderr interleaved
// into one buffer, as cmd.CombinedOutput does.
func combinedOutputWith(run cmdRunFunc, cmd *exec.Cmd) ([]byte, error) {
	var b bytes.Buffer
	cmd.Stdout = &b
	cmd.Stderr = &b
	_, _, err := runWith(run, cmd)
	return b.Bytes(), err
}

// runCmd runs a subprocess the daemon built through its execCmd seam, in a
// process group of its own and with a bounded wait on its output (gt-7uyfc).
func (d *Daemon) runCmd(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	return runWith(d.execCmd, boundDaemonCmd(cmd))
}

// combinedOutput is cmd.CombinedOutput through the daemon's execCmd seam, in
// a process group of its own and with a bounded wait on its output (gt-7uyfc).
func (d *Daemon) combinedOutput(cmd *exec.Cmd) ([]byte, error) {
	return combinedOutputWith(d.execCmd, boundDaemonCmd(cmd))
}

// bdRunWith is (*beads.Cmd).Run through run, with stdout and stderr
// captured: stdout is unwrapped from bd's machine envelope on success and
// left as bd printed it on failure, whose envelope holds the typed error.
func bdRunWith(run cmdRunFunc, cmd *beads.Cmd) (stdout, stderr []byte, err error) {
	stdout, stderr, err = runWith(run, cmd.Cmd)
	if err != nil {
		return stdout, stderr, err
	}
	return beads.LegacyPayload(cmd.Args[1:], stdout), stderr, nil
}
