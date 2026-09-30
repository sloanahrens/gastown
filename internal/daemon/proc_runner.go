package daemon

import (
	"bytes"
	"os/exec"

	"github.com/steveyegge/gastown/internal/beads"
)

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

// runCmd runs a subprocess the daemon built through its execCmd seam.
func (d *Daemon) runCmd(cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	return runWith(d.execCmd, cmd)
}

// combinedOutput is cmd.CombinedOutput through the daemon's execCmd seam.
func (d *Daemon) combinedOutput(cmd *exec.Cmd) ([]byte, error) {
	return combinedOutputWith(d.execCmd, cmd)
}

// bdOutput is (*beads.Cmd).Output through the daemon's execCmd seam: stdout,
// unwrapped from bd's machine envelope on success and untouched on failure.
func (d *Daemon) bdOutput(cmd *beads.Cmd) ([]byte, error) {
	return bdOutputWith(d.execCmd, cmd)
}

// bdOutputWith is (*beads.Cmd).Output through run.
func bdOutputWith(run cmdRunFunc, cmd *beads.Cmd) ([]byte, error) {
	out, _, err := bdRunWith(run, cmd)
	return out, err
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
