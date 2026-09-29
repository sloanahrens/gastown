package beads

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// bdCall is one bd subprocess: the argv after "bd", the working directory,
// the complete environment and optional stdin.
type bdCall struct {
	dir  string
	env  []string // the whole environment; nil inherits the process's (plain calls only)
	args []string
	// stdin is piped to bd when non-nil.
	stdin []byte
	// plain marks a call built the way CommandWithEnv builds one: the
	// caller's environment as given, bd in the caller's process group, no
	// OTEL variables added.
	plain bool
}

// bdRunFunc runs one bd call and returns its stdout and stderr. Every bd
// invocation a *Beads makes goes through one, including the --allow-stale
// capability probe, so the package's own tests can record and answer them
// without starting processes. Errors that carry an exit status implement
// interface{ ExitCode() int }, as *exec.ExitError does.
type bdRunFunc func(ctx context.Context, c bdCall) (stdout, stderr []byte, err error)

// runBDProcess is the real bdRunFunc: it runs bd from PATH.
func runBDProcess(ctx context.Context, c bdCall) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	err := newBDProcess(ctx, c, &stdout, &stderr).Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// newBDProcess builds the subprocess for c: a plain call the way
// CommandWithEnv built one (newPlainBDCmd), any other the routed way
// (newBDCmd).
func newBDProcess(ctx context.Context, c bdCall, stdout, stderr *bytes.Buffer) *exec.Cmd {
	if c.plain {
		return newPlainBDCmd(ctx, c, stdout, stderr)
	}
	return newBDCmd(ctx, c.dir, c.env, c.stdin, c.args, stdout, stderr)
}

// newPlainBDCmd builds a plain call's bd subprocess the way CommandWithEnv
// builds one: bd from PATH in c.dir with exactly c.env (nil inherits the
// process environment, with PWD set to the directory), in the caller's
// process group, nothing added.
func newPlainBDCmd(ctx context.Context, c bdCall, stdout, stderr *bytes.Buffer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bd", c.args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = c.dir
	cmd.Env = c.env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if c.stdin != nil {
		cmd.Stdin = bytes.NewReader(c.stdin)
	}
	return cmd
}

// runner returns the bd seam, falling back to the real bd for a Beads built
// without one.
func (b *Beads) runner() bdRunFunc {
	if b.exec == nil {
		return runBDProcess
	}
	return b.exec
}

// allowStaleArgs prepends --allow-stale to args when bd accepts it. The real
// bd is probed once per binary path (BdSupportsAllowStaleWithEnv); an
// injected runner is asked on every call, with the same probe argv, so a test
// decides the answer and never reaches the bd on PATH.
func (b *Beads) allowStaleArgs(env, args []string) []string {
	if b.exec == nil {
		return MaybePrependAllowStaleWithEnv(env, args)
	}
	ctx, cancel := context.WithTimeout(context.Background(), resolveBdAllowStaleProbeTimeout())
	defer cancel()
	stdout, stderr, err := b.exec(ctx, bdCall{dir: b.workDir, env: env, args: []string{"--allow-stale", "version"}})
	out := strings.TrimSpace(string(stdout) + string(stderr))
	if err == nil && out != "" && !strings.Contains(out, "unknown flag") {
		return append([]string{"--allow-stale"}, args...)
	}
	return args
}

// NewPlain returns a Beads that runs bd in dir with exactly env (nil
// inherits the process environment), the way CommandWithEnv builds a bd
// command. None of the routing policy New applies is added: no BEADS_DIR pin,
// no --allow-stale probe, no subprocess deadline, no retries, no prefix
// routing. It exists so callers that managed bd's environment themselves
// can move from raw argv onto typed methods without changing what bd sees.
func NewPlain(dir string, env []string) *Beads {
	b := newBeads(beadsFields{workDir: dir, noRoute: true})
	b.plain = true
	b.plainEnv = env
	return b
}

// WithTimeout returns a copy of a plain wrapper whose bd calls are killed
// after d. It panics on a wrapper not built by NewPlain.
func (b *Beads) WithTimeout(d time.Duration) *Beads {
	if !b.plain {
		panic("beads: WithTimeout on a wrapper not built by NewPlain")
	}
	c := NewPlain(b.workDir, b.plainEnv)
	c.exec = b.exec
	c.plainTimeout = d
	return c
}

// CLIError is a failed bd call made by a plain wrapper (NewPlain). It keeps
// bd's output apart, because callers of the raw commands it replaces read
// stderr or combined output themselves.
type CLIError struct {
	Args   []string
	Stdout []byte
	Stderr []byte
	Err    error
}

func (e *CLIError) Error() string {
	if msg := strings.TrimSpace(string(e.Stderr)); msg != "" {
		return fmt.Sprintf("bd %s: %s", strings.Join(e.Args, " "), msg)
	}
	return fmt.Sprintf("bd %s: %v", strings.Join(e.Args, " "), e.Err)
}

// Output is bd's stdout followed by its stderr, trimmed: what the
// CombinedOutput of the raw command reported.
func (e *CLIError) Output() string {
	return strings.TrimSpace(string(e.Stdout) + string(e.Stderr))
}

// Unwrap exposes the process error, and ErrNotFound when bd said the issue
// was not found, as wrapError does for the policy path.
func (e *CLIError) Unwrap() []error {
	errs := []error{e.Err}
	stderr := string(e.Stderr)
	if strings.Contains(stderr, "not found") || strings.Contains(stderr, "no issue found") {
		errs = append(errs, ErrNotFound)
	}
	return errs
}

// runPlain runs one bd call for a plain wrapper.
func (b *Beads) runPlain(stdin []byte, args []string) ([]byte, error) {
	ctx := context.Background()
	if b.plainTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, b.plainTimeout)
		defer cancel()
	}
	stdout, stderr, err := b.runner()(ctx, bdCall{dir: b.workDir, env: b.plainEnv, args: args, stdin: stdin, plain: true})
	if err != nil {
		return stdout, &CLIError{Args: args, Stdout: stdout, Stderr: stderr, Err: SubprocessFailureError(ctx, b.plainTimeout, err)}
	}
	return stdout, nil
}
