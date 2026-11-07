package sweeprunner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/config"
)

// stepTailLines is how many lines of a failed step's output its failure
// message carries: enough for the error a copy or apply prints, without the
// whole output of a long step.
const stepTailLines = 8

// stepRetryPause is how long a Step marked Retry waits before its second
// attempt, so a source a live writer was changing has settled.
const stepRetryPause = 5 * time.Second

// redactedValue stands for a token-looking value the output of a failed step
// carried, so the failure message never quotes a credential (gt-d6hfcc).
const redactedValue = "[redacted]"

// StepRunFunc runs one command the caller built and returns its stdout and
// stderr. The daemon passes its own command seam, so tests answer the steps
// without a cluster.
type StepRunFunc func(cmd *exec.Cmd) (stdout, stderr []byte, err error)

// RunSteps runs steps in order, stopping at the first that fails, and returns
// their output and that step's exit status. A step marked Retry is attempted
// twice, with stepRetryPause on clock between the attempts; a step that fails
// both times reports both attempts. A failed attempt's message carries the
// last stepTailLines lines of its output, with token-looking values redacted.
func RunSteps(ctx context.Context, clock clockwork.Clock, run StepRunFunc, steps []Step) (string, int) {
	var b strings.Builder
	for _, step := range steps {
		if len(step.Argv) == 0 {
			continue
		}
		attempts := 1
		if step.Retry {
			attempts = 2
		}
		for attempt := 1; attempt <= attempts; attempt++ {
			if attempt > 1 {
				select {
				case <-clock.After(stepRetryPause):
				case <-ctx.Done():
				}
			}
			cmd := exec.CommandContext(ctx, step.Argv[0], step.Argv[1:]...) //nolint:gosec // G204: the argv a Backend rendered from a validated Spec, run directly and never through a shell
			if step.Stdin != nil {
				cmd.Stdin = bytes.NewReader(step.Stdin)
			}
			stdout, stderr, err := run(cmd)
			if err == nil {
				b.Write(stdout)
				b.Write(stderr)
				break
			}
			output := slices.Concat(stdout, stderr)
			if step.MissingSource(output) {
				// The source this step copies is absent, which the step declares
				// is not a failure: a run that wrote no logs leaves /out/logs
				// absent (gt-4lj2q). Any other failure still stops the run.
				b.Write(stdout)
				b.Write(stderr)
				break
			}
			code := exitStatus(err)
			if step.Retry {
				fmt.Fprintf(&b, "sweep-run: %s failed on attempt %d of %d (exit %d): %v\n", strings.Join(step.Argv, " "), attempt, attempts, code, err)
			} else {
				fmt.Fprintf(&b, "sweep-run: %s failed (exit %d): %v\n", strings.Join(step.Argv, " "), code, err)
			}
			if tail := failureTail(output); tail != "" {
				b.WriteString(tail + "\n")
			}
			if attempt == attempts {
				return b.String(), code
			}
		}
	}
	return b.String(), 0
}

// exitStatus is the exit status an error carries, or -1 when it carries none:
// the step never ran, which is not the same as a command that ran and failed.
func exitStatus(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

// failureTail is the last stepTailLines lines of out, each with its
// token-looking values redacted; empty when out is.
func failureTail(out []byte) string {
	text := strings.TrimRight(string(out), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) > stepTailLines {
		lines = lines[len(lines)-stepTailLines:]
	}
	for i, line := range lines {
		lines[i] = redactLine(line)
	}
	return strings.Join(lines, "\n")
}

// redactLine is line with each token-looking value replaced: a KEY=value word
// or a value after a "KEY:" word whose key names a credential, and a bare word
// that starts like a provider token. The test is the one the config secrets
// guard applies (config.LooksLikeSecret), so output and config agree on what
// a token is. A line with no such value is returned as it was.
func redactLine(line string) string {
	fields := strings.Fields(line)
	changed := false
	for i, field := range fields {
		prev := ""
		if i > 0 {
			prev = fields[i-1]
		}
		switch {
		case strings.Contains(field, "="):
			key, value, _ := strings.Cut(field, "=")
			if config.LooksLikeSecret(key, value) {
				fields[i] = key + "=" + redactedValue
				changed = true
			}
		case strings.HasSuffix(prev, ":") && config.LooksLikeSecret(strings.TrimSuffix(prev, ":"), field):
			fields[i] = redactedValue
			changed = true
		case config.LooksLikeSecret("", field):
			fields[i] = redactedValue
			changed = true
		}
	}
	if !changed {
		return line
	}
	return strings.Join(fields, " ")
}
