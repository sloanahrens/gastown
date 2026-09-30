package daemon

import (
	"bytes"
	"os/exec"
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
