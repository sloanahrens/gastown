package mail

import (
	"bytes"
	"context"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	// bdReadTimeout is the timeout for bd read operations (list, show, query).
	// 60s accommodates concurrent agent load where multiple bd processes compete
	// for Dolt locks and memory (was 30s, caused signal:killed under contention).
	bdReadTimeout = 60 * time.Second
	// bdWriteTimeout is the timeout for bd write operations (create, close, label, reopen).
	bdWriteTimeout = 60 * time.Second
)

// bdError represents an error from running a bd command.
// It wraps the underlying error and includes the stderr output for inspection.
type bdError struct {
	Err    error
	Stderr string
}

// Error implements the error interface.
func (e *bdError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "unknown bd error"
}

// Unwrap returns the underlying error for errors.Is/As compatibility.
func (e *bdError) Unwrap() error {
	return e.Err
}

// ContainsError checks if the stderr message contains the given substring.
func (e *bdError) ContainsError(substr string) bool {
	return strings.Contains(e.Stderr, substr)
}

// bdCall is one bd invocation as a bdRunner sees it: the argv after "bd",
// the working directory and the whole environment.
type bdCall struct {
	Dir  string
	Env  []string
	Args []string
}

// bdRunner runs one bd invocation under ctx. Mail keeps its own raw bd
// runner rather than beads' typed client: every mail call carries a
// caller's deadline (bdReadCtx, bdWriteCtx), so a hung bd cannot pin the
// sender or the reader, and runs in its own process group so the deadline
// kills bd's children too. A failure that should read as a bd exit status
// implements interface{ ExitCode() int }.
type bdRunner func(ctx context.Context, c bdCall) (stdout, stderr []byte, err error)

// runBdCommand executes a bd command with a context timeout and proper environment setup.
// ctx controls the deadline/timeout for the subprocess.
// workDir is the directory to run the command in.
// beadsDir is the BEADS_DIR environment variable value.
// extraEnv contains additional environment variables to set (e.g., "BD_IDENTITY=...").
// Returns stdout bytes on success, or a *bdError on failure.
func runBdCommand(ctx context.Context, run bdRunner, args []string, workDir, beadsDir string, extraEnv ...string) (_ []byte, retErr error) {

	// Remove stale dolt-server.pid before spawning bd. A stale PID file causes
	// bd to connect to port 3307 which may be occupied by a different Dolt server
	// serving different databases, resulting in hangs until the read timeout kills it.
	beads.CleanStaleDoltServerPID(beadsDir)

	// bd v0.59+ requires --flat for list --json to produce JSON output.
	// Without it, bd returns human-readable tree format that fails JSON parsing.
	// The mail package calls bd directly (not via beads.Run), so it needs its
	// own injection. (GH#2746)
	args = beads.InjectFlatForListJSON(args)

	if run == nil {
		run = runBdProcess
	}
	// cmd.Environ() carries PWD=workDir, which bd's own file discovery reads.
	env := bdSubprocessEnv(beads.CommandContextWithEnv(ctx, workDir, nil).Environ(), beadsDir, beads.ArgsAreReadOnly(args), extraEnv)

	stdout, stderr, runErr := run(ctx, bdCall{Dir: workDir, Env: env, Args: args})

	// If bd doesn't support --flat (< v0.59), retry without it.
	// Same fallback pattern as beads.Run. (GH#2746)
	if runErr != nil && strings.Contains(string(stderr), "unknown flag: --flat") {
		retryArgs := make([]string, 0, len(args))
		for _, a := range args {
			if a != "--flat" {
				retryArgs = append(retryArgs, a)
			}
		}
		stdout, stderr, runErr = run(ctx, bdCall{Dir: workDir, Env: env, Args: retryArgs})
	}

	if runErr != nil {
		return nil, &bdError{
			Err:    runErr,
			Stderr: strings.TrimSpace(string(stderr)),
		}
	}

	return stdout, nil
}

// runBdProcess is the real bdRunner: bd on PATH, in its own detached
// process group, with exactly the environment c carries.
func runBdProcess(ctx context.Context, c bdCall) ([]byte, []byte, error) {
	cmd := beads.CommandContextWithEnv(ctx, c.Dir, c.Env, c.Args...)
	util.SetDetachedProcessGroup(cmd.Cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// firstArg returns args[0] or "" when the slice is empty.
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func bdSubprocessEnv(baseEnv []string, beadsDir string, readOnly bool, extraEnv []string) []string {
	base := append(append([]string{}, baseEnv...), extraEnv...)
	mode := beads.MutationRouting
	if readOnly {
		mode = beads.ReadOnlyRouting
	}
	if beadsDir != "" {
		if readOnly {
			mode = beads.ReadOnlyPinned
		} else {
			mode = beads.MutationPinned
		}
	}
	return beads.EnvForSubprocessMode(base, beadsDir, mode)
}

// bdReadCtx returns a context with the standard bd read timeout.
//
//nolint:gosec // The cancel function is returned to callers, who are responsible for invoking it.
func bdReadCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), bdReadTimeout)
	return ctx, cancel
}

// bdWriteCtx returns a context with the standard bd write timeout.
//
//nolint:gosec // The cancel function is returned to callers, who are responsible for invoking it.
func bdWriteCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), bdWriteTimeout)
	return ctx, cancel
}
