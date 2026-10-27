package installer

import (
	"bytes"
	"errors"
	"io/fs"
	"os/exec"
)

// Execute runs one external command from dir and reports its two streams and
// the status it exited with. The install sections need both halves of that: the
// build makes one decision on the output (a slot line, an "already at HEAD")
// and another on the status (build-failed against slot-busy), and the smoke
// check parses stdout as JSON with stderr discarded, as the script's
// `2>/dev/null` does.
//
// It is a distinct seam from Contract.Exec, which runs a command with no
// working directory and returns its combined output: the contract's escalation
// and the gateway only ask whether that worked, while these commands run in the
// checkout being built and their answer is a status (gt-tpg0p.11).
type Execute func(dir, name string, args ...string) (stdout, stderr []byte, code int)

// run runs one command through the seam, and for real when none was set.
func (e Execute) run(dir, name string, args ...string) (stdout, stderr []byte, code int) {
	if e == nil {
		return RealExecute(dir, name, args...)
	}
	return e(dir, name, args...)
}

// RealExecute runs the command for real. A command that never started has
// status 126 when it could not be executed and 127 when it was not found, the
// two numbers bash reports, so a caller reads every answer as one status.
func RealExecute(dir, name string, args ...string) (stdout, stderr []byte, code int) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), errOut.Bytes(), 0
	}
	return out.Bytes(), errOut.Bytes(), statusOf(err)
}

// statusOf is the status bash gives a command that never ran: a command that
// did run has its own, 126 is "found but could not be executed" and 127 is "not
// found". The permission check is errors.Is rather than os.IsPermission because
// a path that exists and is not executable fails the way exec reports it, and
// os.IsPermission does not look inside exec's error, so that path would have
// read as not found (gt-tpg0p.11). A bare name is 127 either way: bash searches
// PATH past a file it cannot execute and finds nothing, which is what Go's
// lookup reports too.
func statusOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	if errors.Is(err, fs.ErrPermission) {
		return 126
	}
	return 127
}
