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
// stdin is what the command reads as its standard input, nil for every command
// but the keychain unlock: its password is handed over on stdin so it never
// appears in the argv ps shows to every user on the machine (gt-4ucnp).
//
// It is a distinct seam from Contract.Exec, which runs a command with no
// working directory and returns its combined output: the contract's escalation
// and the gateway only ask whether that worked, while these commands run in the
// checkout being built and their answer is a status (gt-tpg0p.11).
type Execute func(dir, name string, stdin []byte, args ...string) (stdout, stderr []byte, code int)

// run runs one command through the seam with nothing on its stdin, and for
// real when none was set.
func (e Execute) run(dir, name string, args ...string) (stdout, stderr []byte, code int) {
	return e.runInput(dir, name, nil, args...)
}

// runInput runs one command through the seam, feeding it stdin.
func (e Execute) runInput(dir, name string, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	if e == nil {
		return RealExecute(dir, name, stdin, args...)
	}
	return e(dir, name, stdin, args...)
}

// RealExecute runs the command for real, with stdin as its standard input (nil
// leaves it at /dev/null, as a shell's unredirected command has it). A command
// that never started has status 126 when it could not be executed and 127 when
// it was not found, the two numbers bash reports, so a caller reads every
// answer as one status.
func RealExecute(dir, name string, stdin []byte, args ...string) (stdout, stderr []byte, code int) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
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
// found". errors.Is rather than os.IsPermission, because exec can report a
// permission denial inside its own error, which os.IsPermission does not look
// inside and would read as not found (gt-tpg0p.11). A bare name is 127 either
// way: the PATH search skips a file it cannot execute and finds nothing, which
// is bash's answer for the same case.
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
