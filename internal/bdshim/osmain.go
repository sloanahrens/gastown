package bdshim

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

// handlers is the tracker verb registry. This skeleton ships none, so every
// tracker verb is refused rather than falling back to bd.real; X3-X6 of
// gt-p85pw fill it from this package.
var handlers = map[string]Handler{}

// OsMain is the production entry: this process's argv, environment and working
// directory, and bd.real beside the running binary.
func OsMain(args []string) int {
	return Main(osEnv(args))
}

// osEnv is the process surface a real run reads.
func osEnv(args []string) Env {
	return processEnv(args, os.Getenv, os.Getwd, os.Executable, runBdReal, os.Stderr)
}

// processEnv wires Main to the process's reads and to the verb registry. The
// caller passes each source: osEnv passes the real ones, and a test passes
// fakes for the same wiring with a town of its own.
func processEnv(args []string, getenv func(string) string, getwd func() (string, error), executable func() (string, error), run func(string, []string) error, stderr io.Writer) Env {
	return Env{
		Args:       args,
		Getenv:     getenv,
		Getwd:      getwd,
		Executable: executable,
		Run:        run,
		Handlers:   handlers,
		Stderr:     stderr,
	}
}

// runBdReal runs bd.real and reports how it ended, leaving bd's own output to
// the streams it was given.
func runBdReal(path string, argv []string) error {
	err := realCommand(path, argv).Run()
	if err == nil {
		return nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return err // bd.real never started
	}
	if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return childStatus{code: -1, signal: int(ws.Signal())}
	}
	return childStatus{code: exit.ExitCode()}
}

// realCommand is the bd.real process: bd's argv, the inherited environment
// (Cmd.Env left nil) and the inherited streams, so the command reaches bd
// exactly as it reached the shim.
func realCommand(path string, argv []string) *exec.Cmd {
	cmd := exec.Command(path, argv...) //nolint:gosec // the path is bd.real beside this binary
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd
}

// childStatus is how bd.real ended, as the status Main returns: a shell's
// 128+signal for a signal death, so a caller reaches the same conclusion as
// with bd itself.
type childStatus struct {
	code   int // the child's exit code, -1 when a signal ended it
	signal int // the signal that ended the child, 0 for a plain exit
}

// ExitCode is the status the shim exits with.
func (c childStatus) ExitCode() int {
	switch {
	case c.signal != 0:
		return 128 + c.signal
	case c.code < 0:
		return 1 // no status to report: a shim failure, never a pass
	}
	return c.code
}

// Error reports the status childStatus carries.
func (c childStatus) Error() string {
	return fmt.Sprintf("%s exited %d", realName, c.ExitCode())
}
