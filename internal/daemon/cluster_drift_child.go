package daemon

import (
	"context"
	"fmt"
	"os/exec"
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
// and no error, however large. Nothing here reads the output.
func startDriftChild(ctx context.Context, exe string, args []string, limit int) (stdout, stderr []byte, exit int, err error) {
	out, errOut := &limitedBuffer{limit: limit}, &limitedBuffer{limit: limit}
	cmd := exec.CommandContext(ctx, exe, args...) //nolint:gosec // G204: a fixed program and fixed arguments from the caller, never a shell
	cmd.Stdout, cmd.Stderr = out, errOut
	exit, err = driftChildOutcome(cmd.Run(), ctx.Err())
	return out.Bytes(), errOut.Bytes(), exit, err
}

// driftChildOutcome maps a finished run to the child's exit code. No run error
// is 0. A run error after the context ended is -1 with the context error: the
// deadline or a cancel ended the child. An error that carries an exit code is
// that code with no error, because a non-zero exit is the child's answer. Any
// other run error is -1 with that error: the child did not start.
func driftChildOutcome(runErr, ctxErr error) (int, error) {
	if runErr == nil {
		return 0, nil
	}
	if ctxErr != nil {
		return -1, fmt.Errorf("drift child ended by its context: %w", ctxErr)
	}
	if code := exitCodeOf(runErr); code >= 0 {
		return code, nil
	}
	return -1, runErr
}
