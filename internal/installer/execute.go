package installer

import (
	"bytes"
	"errors"
	"os"
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
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.Bytes(), errOut.Bytes(), exitErr.ExitCode()
	}
	if os.IsPermission(err) {
		return out.Bytes(), errOut.Bytes(), 126
	}
	return out.Bytes(), errOut.Bytes(), 127
}
