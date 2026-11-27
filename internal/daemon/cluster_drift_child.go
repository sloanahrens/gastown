package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// limitedBuffer is an io.Writer that keeps the first limit bytes written and
// drops the rest. A write past the limit still reports its full length and no
// error, so the child process writing to it never sees a short write, and the
// memory held never grows past limit bytes.
type limitedBuffer struct {
	limit     int
	buf       []byte
	truncated bool
}

// Write keeps what fits under the limit and notes when it drops the rest.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	full := len(p)
	room := b.limit - len(b.buf)
	if room < 0 {
		room = 0
	}
	if len(p) > room {
		b.truncated = true
		p = p[:room]
	}
	b.buf = append(b.buf, p...)
	return full, nil
}

// Bytes returns the kept bytes.
func (b *limitedBuffer) Bytes() []byte { return b.buf }

// Truncated reports whether any byte was dropped.
func (b *limitedBuffer) Truncated() bool { return b.truncated }

// startDriftChild runs exe with args as a child of the daemon and returns what
// it wrote to each stream, kept to the first limit bytes, and its exit code. The
// program and arguments are the caller's and are not run through a shell; the
// deadline is the caller's, set on ctx. A child that did not start or that ctx
// ended returns -1 and an error; a child that ran and exited returns its code
// and no error, however large, once driftChildWaitDelay has passed if a
// grandchild still held its output pipes. Nothing here reads the output.
func startDriftChild(ctx context.Context, exe string, args []string, limit int) (stdout, stderr []byte, exit int, err error) {
	out, errOut := &limitedBuffer{limit: limit}, &limitedBuffer{limit: limit}
	cmd := driftChildCmd(exec.CommandContext(ctx, exe, args...)) //nolint:gosec // G204: a fixed program and fixed arguments from the caller, never a shell
	cmd.Stdout, cmd.Stderr = out, errOut
	runErr := cmd.Run()
	exit, err = driftChildOutcome(runErr, ctx.Err(), cmd.ProcessState != nil && cmd.ProcessState.Success())
	return out.Bytes(), errOut.Bytes(), exit, err
}

// driftChildWaitDelay bounds how long Wait keeps reading the output pipes after
// the child itself has exited. A grandchild that inherited the pipes outlives
// the child, and without this bound Run blocks until the grandchild ends,
// however long that is (gt-i9dyt0).
const driftChildWaitDelay = 3 * time.Second

// driftChildCmd gives a command the drift child's process-group settings. The
// child leads its own process group (Setpgid), so the cancel below reaches a
// grandchild the child started; a deadline that killed only the child would
// leave a grandchild holding the output pipes, and Run would wait on it past
// the deadline.
func driftChildCmd(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// The group kill ends a child the deadline interrupts; a child that exits
	// on its own leaves its grandchild holding the pipes, so bound the wait for
	// them as well (gt-i9dyt0).
	cmd.WaitDelay = driftChildWaitDelay
	cmd.Cancel = func() error {
		// exec calls Cancel only after a successful Start, so cmd.Process is
		// the child there. A command that never started has nothing to end:
		// report it as already done, not as interrupted, and never hand a nil
		// Process to the kill below.
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return driftChildCancel(cmd.Process.Pid, syscall.Kill, cmd.Process.Kill)
	}
	return cmd
}

// driftChildCancel ends the child and every process in its group: it sends
// SIGKILL to the group led by pid (a negative pid names the group) so a
// grandchild the child started dies with it, and falls back to killing the
// child alone when the group signal fails.
func driftChildCancel(pid int, signal func(int, syscall.Signal) error, kill func() error) error {
	if err := signal(-pid, syscall.SIGKILL); err != nil {
		return kill()
	}
	return nil
}

// driftChildOutcome maps a finished run to the child's exit code. No run error
// is 0. A run error after the context ended is -1 with the context error: the
// deadline or a cancel ended the child. A successful exit whose output pipes a
// grandchild held past WaitDelay is 0 with no error: exec reports ErrWaitDelay
// for it, and that is not the child's answer. An error that carries an exit
// code is that code with no error, because a non-zero exit is the child's
// answer. Any other run error is -1 with that error: the child did not start.
func driftChildOutcome(runErr, ctxErr error, exitedZero bool) (int, error) {
	if runErr == nil {
		return 0, nil
	}
	if ctxErr != nil {
		return -1, fmt.Errorf("drift child ended by its context: %w", ctxErr)
	}
	if exitedZero && errors.Is(runErr, exec.ErrWaitDelay) {
		return 0, nil
	}
	if code := exitCodeOf(runErr); code >= 0 {
		return code, nil
	}
	return -1, runErr
}
