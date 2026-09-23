package overlap

import (
	"bytes"
	"context"
	"os/exec"
	"time"

	"github.com/steveyegge/gastown/internal/util"
)

// RealRunStep is the production SuiteRunner: it runs step.Cmd through a
// shell in dir, in its own process group, so ctx cancellation (an overlap
// timeout) kills the whole process tree instead of leaving a shell's child
// processes running past the join — the same reason runGate does this for
// the batch and single-MR Go gate path (gt-6t43).
func RealRunStep(dir string) SuiteRunner {
	return func(ctx context.Context, step SuiteStep) StepResult {
		start := time.Now()
		// Trust boundary: step.Cmd comes from the rig's own formula vars
		// (operator-controlled), not from a submitted branch. Shell execution
		// is intentional, matching runGate's own gate commands.
		cmd := exec.CommandContext(ctx, "sh", "-c", step.Cmd) //nolint:gosec // G204: step.Cmd is from trusted rig config
		util.SetProcessGroup(cmd)
		cmd.Dir = dir
		var out bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &out

		err := cmd.Run()
		elapsed := time.Since(start)
		if err == nil {
			return StepResult{Success: true, ExitCode: 0, Output: capOutput(out.String()), Elapsed: elapsed}
		}
		exitCode := -1
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
		output := out.String()
		if exitCode == -1 {
			// Launch failure or ctx cancellation, not a process exit — record
			// the error so the caller can tell a timeout from a real failure.
			if output != "" {
				output += "\n"
			}
			output += err.Error()
		}
		return StepResult{Success: false, ExitCode: exitCode, Output: capOutput(output), Elapsed: elapsed}
	}
}

// maxCapturedOutput bounds how much of a step's combined stdout/stderr rides
// in the --json result: a full `make test` dump would otherwise land whole
// in the refinery agent's context on every rejection. Matches the tail the
// old run-tests formula step already took (`tail -40`) in spirit, sized in
// bytes since a step's output is not line-oriented in general.
const maxCapturedOutput = 8000

// capOutput keeps the tail of output — the end of a build/test log is where
// the actual failure is — prefixed with a marker so a caller can tell the
// log was cut rather than short by coincidence.
func capOutput(output string) string {
	if len(output) <= maxCapturedOutput {
		return output
	}
	return "...[truncated]...\n" + output[len(output)-maxCapturedOutput:]
}
