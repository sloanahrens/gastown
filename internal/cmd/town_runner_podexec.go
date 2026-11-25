package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"unicode/utf8"

	"github.com/steveyegge/gastown/internal/report"
)

// podExecStderrTail is how much of a failing child's stderr an error carries: the
// last bytes, scrubbed. Raw captured output in an error is the om rule's 11th
// finding, so the tail is bounded here and passed through report.Scrub.
const podExecStderrTail = 2048

// productionPodExec is the real podExec: it runs argv with exec.CommandContext,
// stdin set from the bytes, and returns what the child printed on stdout, kept
// apart from stderr, and its exit status. Stderr is never parsed and never mixed
// into the reply: update.RunFunc returns both combined, and a kubectl warning
// there would corrupt the one-word replies of the landing-hold scripts. It is
// kept apart, and only its last podExecStderrTail bytes, scrubbed, reach an error.
//
// A child that exits non-zero is its status and the stdout so far with an error
// naming the command, its status and its stderr tail, so a refused hold or a
// kubectl failure says why. A command that cannot start, a child killed by the
// context and a child killed by a signal (no exit status) are errors naming the
// command, with the stderr tail when there is one. On success the stderr is
// dropped and the reply is clean.
func productionPodExec(ctx context.Context, argv []string, stdin []byte) ([]byte, int, error) {
	if len(argv) == 0 {
		return nil, 0, errors.New("no argv to run")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	stderr := &tailWriter{max: podExecStderrTail + utf8.UTFMax}
	cmd.Stderr = stderr
	err := cmd.Run()
	status := -1
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		status = exit.ExitCode()
	}
	return podExecOutcome(ctx, argv[0], stdout.Bytes(), stderr.buf, status, err)
}

// tailWriter keeps the last max bytes written to it, so a child that writes
// without end cannot grow the daemon's memory.
type tailWriter struct {
	max int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = append([]byte(nil), w.buf[len(w.buf)-w.max:]...)
	}
	return len(p), nil
}

// stderrTail is the last podExecStderrTail bytes of b, moved forward to a
// character boundary if the cut would split one, with credential-shaped runs
// replaced and line breaks turned to spaces (report.Scrub).
func stderrTail(b []byte) string {
	if len(b) > podExecStderrTail {
		b = b[len(b)-podExecStderrTail:]
	}
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return report.Scrub(string(b))
}

// podExecFailure is err with the stderr tail after it, when there is one.
func podExecFailure(stderr []byte, err error) error {
	if tail := stderrTail(stderr); tail != "" {
		return fmt.Errorf("%w: %s", err, tail)
	}
	return err
}

// podExecOutcome turns what running the child ended with into productionPodExec's
// answer. It takes the error and the exit status (-1 when the child has none) as
// values, so each branch is driven without starting a process.
func podExecOutcome(ctx context.Context, name string, stdout, stderr []byte, status int, err error) ([]byte, int, error) {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout, 0, nil
	case ctx.Err() != nil:
		return nil, 0, podExecFailure(stderr, fmt.Errorf("running %s: %w", name, ctx.Err()))
	case status > 0:
		return stdout, status, podExecFailure(stderr, fmt.Errorf("%s exited %d", name, status))
	case errors.As(err, &exit):
		return nil, 0, podExecFailure(stderr, fmt.Errorf("running %s: %w", name, err))
	default:
		return nil, 0, podExecFailure(stderr, fmt.Errorf("starting %s: %w", name, err))
	}
}
