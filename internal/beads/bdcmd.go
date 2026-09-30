package beads

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/util"
)

// BdCmd is a builder for constructing bd exec.Command calls.
// It provides a fluent API for configuring environment variables,
// working directory, and I/O settings common to bd CLI invocations.
type BdCmd struct {
	args       []string
	dir        string
	env        []string
	stdin      io.Reader
	stderr     io.Writer
	autoCommit bool
	allowStale bool
	gtRoot     string
	beadsDir   string
	routing    bool
	// timeout overrides resolveBdCmdTimeout when positive. Tests set it so a
	// timeout path takes milliseconds instead of GT_BD_TIMEOUT_SEC's whole
	// seconds.
	timeout time.Duration
	// run answers the call in process instead of starting bd (Via). nil is
	// the bd on PATH.
	run BDRunner
}

// NewBdCmd creates a new bd command builder with the given arguments.
// The command will execute "bd" with the provided arguments.
//
// Example:
//
//	err := NewBdCmd("show", beadID, "--json").
//	    Dir(workDir).
//	    Run()
func NewBdCmd(args ...string) *BdCmd {
	return &BdCmd{
		args:   args,
		env:    os.Environ(),
		stderr: os.Stderr,
	}
}

// Via sends the call to run instead of the bd on PATH; nil keeps bd. Tests
// pass an in-process bd so they need no stub script on PATH.
func (b *BdCmd) Via(run BDRunner) *BdCmd {
	b.run = run
	return b
}

// runVia answers the call through b.run, with stdout unwrapped from the
// machine envelope as Cmd unwraps it.
func (b *BdCmd) runVia(ctx context.Context) (stdout, stderr []byte, err error) {
	args := b.resolvedArgs()
	var stdin []byte
	if b.stdin != nil {
		if stdin, err = io.ReadAll(b.stdin); err != nil {
			return nil, nil, err
		}
	}
	stdout, stderr, err = b.run(ctx, BDCall{Dir: b.dir, Env: b.buildEnv(), Args: args, Stdin: stdin})
	if err == nil {
		stdout = LegacyPayload(args, stdout)
	}
	return stdout, stderr, err
}

// WithAutoCommit sets BD_DOLT_AUTO_COMMIT=on in the environment.
// This is used for sequential dependent bd calls where each call
// needs to see the changes from previous calls.
func (b *BdCmd) WithAutoCommit() *BdCmd {
	b.autoCommit = true
	return b
}

// AllowStale requests bd's stale-read bypass when the installed bd supports it.
// Unsupported bd versions silently omit the flag so callers can share one
// compatibility path instead of hardcoding version-specific arguments.
func (b *BdCmd) AllowStale() *BdCmd {
	b.allowStale = true
	return b
}

// WithGTRoot adds GT_ROOT=root to the environment.
// This is required for bd to find town-level formulas and configuration.
func (b *BdCmd) WithGTRoot(root string) *BdCmd {
	b.gtRoot = root
	return b
}

// WithBeadsDir sets BEADS_DIR explicitly in the environment.
// This prevents inherited BEADS_DIR from the parent process from causing
// bd to write to the wrong database. The dir should be the resolved
// .beads directory path (e.g., from ResolveBeadsDir).
func (b *BdCmd) WithBeadsDir(dir string) *BdCmd {
	b.beadsDir = dir
	return b
}

// Dir sets the working directory for the command. When a directory is provided,
// bd is also pinned to that directory's resolved .beads database unless
// WithBeadsDir supplies a more specific database.
func (b *BdCmd) Dir(dir string) *BdCmd {
	b.dir = dir
	return b
}

// StripBeadsDir removes any inherited BEADS_DIR from the environment.
// Use this when the command relies on Dir() for routing and an inherited
// BEADS_DIR would incorrectly override the resolved database. If Dir() is set,
// buildEnv will still add an explicit BEADS_DIR for that directory; this method
// only strips inherited values from the parent process.
func (b *BdCmd) StripBeadsDir() *BdCmd {
	b.env = StripEnvKey(b.env, "BEADS_DIR")
	return b
}

// WithRouting strips inherited bd target selectors and does not pin BEADS_DIR,
// allowing bd prefix routing to choose the target database. Dir still sets cwd.
func (b *BdCmd) WithRouting() *BdCmd {
	b.routing = true
	b.env = StripEnvKey(b.env, "BEADS_DIR")
	return b
}

// Stderr sets the stderr writer for the command.
// Defaults to os.Stderr if not set.
func (b *BdCmd) Stderr(w io.Writer) *BdCmd {
	b.stderr = w
	return b
}

// Stdin sets the stdin reader for the command.
func (b *BdCmd) Stdin(r io.Reader) *BdCmd {
	b.stdin = r
	return b
}

// WithEnv replaces the base environment the command starts from. It is for
// callers that hold a routing environment of their own (the daemon) instead
// of the process's; the default is os.Environ().
func (b *BdCmd) WithEnv(env []string) *BdCmd {
	b.env = append([]string{}, env...)
	return b
}

// buildEnv constructs the final environment slice based on configured options.
func (b *BdCmd) buildEnv() []string {
	env := append([]string{}, b.env...)

	// Add GT_ROOT if specified.
	// Filter existing entries first for the same reason as above.
	if b.gtRoot != "" {
		env = StripEnvKey(env, "GT_ROOT")
		env = append(env, "GT_ROOT="+b.gtRoot)
	}

	mode := MutationRouting
	if ArgsAreReadOnly(b.args) && !b.autoCommit {
		mode = ReadOnlyRouting
	}

	beadsDir := ""
	if b.beadsDir != "" {
		beadsDir = b.beadsDir
		if mode == ReadOnlyRouting {
			mode = ReadOnlyPinned
		} else {
			mode = MutationPinned
		}
	} else if b.dir != "" {
		beadsDir = ResolveBeadsDir(b.dir)
		if !b.routing {
			if mode == ReadOnlyRouting {
				mode = ReadOnlyPinned
			} else {
				mode = MutationPinned
			}
		}
	}

	return EnvForSubprocessMode(env, beadsDir, mode)
}

// Build returns the configured command, in machine mode. The embedded
// exec.Cmd can be customized before execution; Cmd documents which
// ways of collecting stdout hand back the unwrapped payload.
func (b *BdCmd) Build() *Cmd {
	args := b.resolvedArgs()
	cmd := CommandWithEnv(b.dir, b.buildEnv(), args...)
	cmd.Stdin = b.stdin
	cmd.Stderr = b.stderr
	return cmd
}

func resolveBdCmdTimeout() time.Duration {
	if v := os.Getenv("GT_BD_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return constants.BdCommandTimeout
}

// deadline is the time budget for one run of this command.
func (b *BdCmd) deadline() time.Duration {
	if b.timeout > 0 {
		return b.timeout
	}
	return resolveBdCmdTimeout()
}

func (b *BdCmd) buildContextCommand(ctx context.Context) *Cmd {
	args := b.resolvedArgs()
	cmd := CommandContextWithEnv(ctx, b.dir, b.buildEnv(), args...)
	util.SetProcessGroup(cmd.Cmd)
	cmd.Stdin = b.stdin
	cmd.Stderr = b.stderr
	return cmd
}

func (b *BdCmd) wrapTimeout(err error, deadline time.Duration) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		return fmt.Errorf("%s timed out after %v: %w", b.argsDesc(), deadline, err)
	}
	return err
}

func (b *BdCmd) wrapCommandError(ctx context.Context, err error, deadline time.Duration) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%s timed out after %v: %w", b.argsDesc(), deadline, err)
	}
	return b.wrapTimeout(err, deadline)
}

func (b *BdCmd) argsDesc() string {
	desc := "bd"
	if len(b.args) > 0 {
		desc += " " + b.args[0]
	}
	if len(b.args) > 1 {
		desc += fmt.Sprintf(" ... (%d args)", len(b.args))
	}
	if b.beadsDir != "" {
		desc += fmt.Sprintf(" beads_dir=%s", b.beadsDir)
	}
	if b.dir != "" {
		desc += fmt.Sprintf(" cwd=%s", b.dir)
	}
	return desc
}

// resolvedArgs returns the final args, normalizing requested stale-read support
// to bd's global flag position when supported and stripping it when unsupported.
func (b *BdCmd) resolvedArgs() []string {
	filtered := make([]string, 0, len(b.args))
	requestedAllowStale := b.allowStale
	for _, a := range b.args {
		if a == "--allow-stale" {
			requestedAllowStale = true
			continue
		}
		filtered = append(filtered, a)
	}
	if !requestedAllowStale {
		return b.args
	}
	// An in-process bd has no version to probe; it gets the flag.
	if b.run != nil || BdSupportsAllowStaleWithEnv(b.buildEnv()) {
		return append([]string{"--allow-stale"}, filtered...)
	}
	return filtered
}

// Run builds and runs the command, returning any error.
// This is a convenience method equivalent to Build().Run().
func (b *BdCmd) Run() error {
	deadline := b.deadline()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if b.run != nil {
		_, stderr, err := b.runVia(ctx)
		if b.stderr != nil {
			_, _ = b.stderr.Write(stderr)
		}
		return b.wrapCommandError(ctx, err, deadline)
	}
	return b.wrapCommandError(ctx, b.buildContextCommand(ctx).Run(), deadline)
}

// Output builds and runs the command, returning stdout and any error.
// This is a convenience method equivalent to Build().Output().
// Note: Output() captures stdout but Stderr must still be configured
// separately if you want to capture stderr instead of it going to os.Stderr.
func (b *BdCmd) Output() ([]byte, error) {
	deadline := b.deadline()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if b.run != nil {
		out, stderr, err := b.runVia(ctx)
		if b.stderr != nil {
			_, _ = b.stderr.Write(stderr)
		}
		return out, b.wrapCommandError(ctx, err, deadline)
	}
	out, err := b.buildContextCommand(ctx).Output()
	return out, b.wrapCommandError(ctx, err, deadline)
}

// CombinedOutput builds and runs the command, returning combined stdout+stderr.
// This overrides the configured Stderr writer to capture both streams.
// Useful for including command output in error messages.
func (b *BdCmd) CombinedOutput() ([]byte, error) {
	deadline := b.deadline()
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	if b.run != nil {
		out, stderr, err := b.runVia(ctx)
		if err != nil {
			return append(stderr, out...), b.wrapCommandError(ctx, err, deadline)
		}
		return append(out, stderr...), nil
	}
	args := b.resolvedArgs()
	cmd := CommandContextWithEnv(ctx, b.dir, b.buildEnv(), args...)
	util.SetProcessGroup(cmd.Cmd)
	cmd.Stdin = b.stdin
	out, err := cmd.CombinedOutput()
	return out, b.wrapCommandError(ctx, err, deadline)
}
