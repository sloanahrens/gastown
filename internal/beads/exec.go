package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/steveyegge/gastown/internal/util"
)

// SubprocessEnvMode describes how a bd subprocess should target Dolt and
// whether it may mutate state. New raw bd call sites should use this helper so
// target selection and side-effect suppression stay centralized.
type SubprocessEnvMode int

const (
	ReadOnlyRouting SubprocessEnvMode = iota
	MutationRouting
	ReadOnlyPinned
	MutationPinned
)

// Cmd is an exec.Cmd running bd in machine mode. Every way of collecting bd's
// stdout hands back the payload the call printed before machine mode
// (LegacyPayload), so a caller reads the same bytes whichever bd answered:
// Output and CombinedOutput, and Run, Start and Wait with cmd.Stdout set to a
// buffer or any writer that is not an *os.File. The envelope's typed error
// stays on the failure path, where the error-kind readers (bd_failure.go) want
// it.
//
// Two shapes get the raw envelope: a cmd.Stdout that is an *os.File, which the
// child writes to directly, and StdoutPipe. A caller that wires either decodes
// the envelope itself, or drops machine mode with WithoutMachineEnv when it
// prints for a person.
type Cmd struct {
	*exec.Cmd
	unwrap *unwrapWriter
}

// unwrapWriter holds what bd writes until the command ends, then passes the
// payload on: the envelope is one JSON document, so it cannot be unwrapped
// while it is still arriving.
type unwrapWriter struct {
	dst  io.Writer
	args []string
	buf  bytes.Buffer
}

func (w *unwrapWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }

// flush writes the unwrapped payload on success and the output as it came on
// failure.
func (w *unwrapWriter) flush(ok bool) {
	out := w.buf.Bytes()
	if ok {
		out = LegacyPayload(w.args, out)
	}
	_, _ = w.dst.Write(out)
}

// Start starts the command. A writer in cmd.Stdout is replaced by the
// unwrapper until Wait.
func (c *Cmd) Start() error {
	if _, isFile := c.Stdout.(*os.File); c.Stdout != nil && !isFile && c.unwrap == nil {
		c.unwrap = &unwrapWriter{dst: c.Stdout, args: c.Args[1:]}
		c.Stdout = c.unwrap
	}
	err := c.Cmd.Start()
	if err != nil {
		c.restoreStdout()
	}
	return err
}

// Wait waits for the command and delivers the unwrapped stdout to the writer
// the caller set.
func (c *Cmd) Wait() error {
	err := c.Cmd.Wait()
	if c.unwrap != nil {
		c.unwrap.flush(err == nil)
		c.restoreStdout()
	}
	return err
}

// Run starts the command and waits for it.
func (c *Cmd) Run() error {
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

func (c *Cmd) restoreStdout() {
	if c.unwrap != nil {
		c.Stdout = c.unwrap.dst
		c.unwrap = nil
	}
}

// Output runs the command and returns its stdout, unwrapped from the machine
// envelope on success. A failure returns stdout untouched: its envelope holds
// the typed error kind.
func (c *Cmd) Output() ([]byte, error) {
	out, err := c.Cmd.Output()
	if err != nil {
		return out, err
	}
	return LegacyPayload(c.Args[1:], out), nil
}

// CombinedOutput runs the command and returns stdout then stderr. On success
// stdout is unwrapped from the envelope; on failure stderr, bd's prose,
// comes first and the envelope after it.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	if c.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	if err := c.Cmd.Run(); err != nil {
		return append(stderr.Bytes(), stdout.Bytes()...), err
	}
	return append(LegacyPayload(c.Args[1:], stdout.Bytes()), stderr.Bytes()...), nil
}

// Command builds a bd command with the shared Gas Town bd environment policy.
func Command(dir, fallbackBeadsDir string, mode SubprocessEnvMode, args ...string) *Cmd {
	cmd := exec.Command("bd", args...) //nolint:gosec // G204: args are constructed internally
	ConfigureCommand(cmd, dir, fallbackBeadsDir, mode)
	return &Cmd{Cmd: cmd}
}

// CommandContext builds a context-bound bd command with the shared Gas Town bd
// environment policy.
func CommandContext(ctx context.Context, dir, fallbackBeadsDir string, mode SubprocessEnvMode, args ...string) *Cmd {
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: args are constructed internally
	ConfigureCommand(cmd, dir, fallbackBeadsDir, mode)
	return &Cmd{Cmd: cmd}
}

// CommandContextBounded is CommandContext plus a real deadline: cmd.Cancel
// kills the whole process group (not just the direct bd child) when ctx
// expires, and cmd.WaitDelay bounds how long Wait can then block on a
// descendant that escaped the group and kept an output pipe open. Use this
// instead of CommandContext whenever the caller's ctx timeout must actually
// hold — CommandContext's ConfigureCommand applies SetDetachedProcessGroup,
// which has no Cancel hook, so on timeout it kills only bd itself and Wait
// can still hang on a runaway grandchild (e.g. an interactive credential
// prompt bd or a helper it spawned is blocked on) — the hang gt-7itep traced
// to this package's DefaultBdCli having no deadline at all.
func CommandContextBounded(ctx context.Context, dir, fallbackBeadsDir string, mode SubprocessEnvMode, args ...string) *Cmd {
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = policyEnv(cmd.Args[1:], fallbackBeadsDir, mode)
	cmd.WaitDelay = SubprocessKillGrace
	util.SetProcessGroup(cmd)
	return &Cmd{Cmd: cmd}
}

// CommandContextWithBin is CommandContext for a caller that resolves and caches
// bd's path itself.
func CommandContextWithBin(ctx context.Context, bin, dir, fallbackBeadsDir string, mode SubprocessEnvMode, args ...string) *Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin/args are constructed internally
	ConfigureCommand(cmd, dir, fallbackBeadsDir, mode)
	return &Cmd{Cmd: cmd}
}

// ConfigureCommand applies the shared bd subprocess policy to an existing
// command. This is for callers that need a custom bd path.
func ConfigureCommand(cmd *exec.Cmd, dir, fallbackBeadsDir string, mode SubprocessEnvMode) {
	cmd.Dir = dir
	cmd.Env = policyEnv(cmd.Args[1:], fallbackBeadsDir, mode)
	util.SetDetachedProcessGroup(cmd)
}

// CommandWithEnv builds a bd command for a caller that supplies its own dir and
// env, without the environment policy Command applies; a nil env inherits the
// parent's environment and takes PWD from dir, as exec.Command does.
func CommandWithEnv(dir string, env []string, args ...string) *Cmd {
	cmd := exec.Command("bd", args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = machineEnvForCall(dir, env, args)
	return &Cmd{Cmd: cmd}
}

// CommandContextWithEnv is the context-bound counterpart to CommandWithEnv.
func CommandContextWithEnv(ctx context.Context, dir string, env []string, args ...string) *Cmd {
	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: args are constructed internally
	cmd.Dir = dir
	cmd.Env = machineEnvForCall(dir, env, args)
	return &Cmd{Cmd: cmd}
}

// CommandWithPath is CommandWithEnv for a caller that resolves and caches bd's
// path itself; for the same with the environment policy applied, use
// CommandContextWithBin.
func CommandWithPath(bin, dir string, env []string, args ...string) *Cmd {
	cmd := exec.Command(bin, args...) //nolint:gosec // G204: bin/args are constructed internally
	cmd.Dir = dir
	cmd.Env = machineEnvForCall(dir, env, args)
	return &Cmd{Cmd: cmd}
}

// CommandContextWithPath is the context-bound counterpart to CommandWithPath.
func CommandContextWithPath(ctx context.Context, bin, dir string, env []string, args ...string) *Cmd {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin/args are constructed internally
	cmd.Dir = dir
	cmd.Env = machineEnvForCall(dir, env, args)
	return &Cmd{Cmd: cmd}
}

// policyEnv is the environment EnvForSubprocessMode builds from the parent's,
// less machine mode for the exempt verbs (machineExempt).
func policyEnv(args []string, fallbackBeadsDir string, mode SubprocessEnvMode) []string {
	env := EnvForSubprocessMode(os.Environ(), fallbackBeadsDir, mode)
	if machineExempt(args) {
		env = WithoutMachineEnv(env)
	}
	return env
}

func EnvForSubprocessMode(base []string, fallbackBeadsDir string, mode SubprocessEnvMode) []string {
	switch mode {
	case ReadOnlyRouting:
		return BuildReadOnlyRoutingBDEnv(base, fallbackBeadsDir)
	case MutationRouting:
		return BuildMutationRoutingBDEnv(base, fallbackBeadsDir)
	case ReadOnlyPinned:
		return BuildReadOnlyPinnedBDEnv(base, fallbackBeadsDir)
	case MutationPinned:
		return BuildMutationPinnedBDEnv(base, fallbackBeadsDir)
	default:
		return BuildMutationRoutingBDEnv(base, fallbackBeadsDir)
	}
}

func SubprocessModeForArgs(args []string) SubprocessEnvMode {
	if ArgsAreReadOnly(args) {
		return ReadOnlyRouting
	}
	return MutationRouting
}

// SubprocessModeForCall returns the subprocess policy for a bd argv run from
// workDir, the workspace directory that policy is built from, and an error for
// a workspace that cannot name the database bd must open.
func SubprocessModeForCall(workDir string, args []string) (SubprocessEnvMode, string, error) {
	beadsDir := ResolveBeadsDir(workDir)
	if routableBeadIDInArgs(workDir, args) != "" {
		// The ID selects the database; naming one here would override routing.
		return SubprocessModeForArgs(args), beadsDir, nil
	}
	if _, err := os.Stat(beadsDir); err != nil {
		// No workspace here to name. bd resolves the nearest .beads above its
		// cwd, which is how a directory inside a worktree, or a town-level
		// agent's own directory, reaches the database that owns it.
		return SubprocessModeForArgs(args), beadsDir, nil
	}
	// Routing reads its target from an ID in argv, so an ID-less call has
	// nothing to route and bd falls back to its built-in default "beads" when
	// the workspace it resolves names no dolt_database — a silent read of a
	// database no rig owns (gt-170zk). Naming the workspace gt already
	// resolved keeps the read where the call was placed.
	if DatabaseNameFromMetadata(beadsDir) == "" {
		return 0, "", fmt.Errorf("%w: bd %s in %s",
			ErrNoConfiguredDatabase, strings.Join(args, " "), beadsDir)
	}
	if ArgsAreReadOnly(args) {
		return ReadOnlyPinned, beadsDir, nil
	}
	return MutationPinned, beadsDir, nil
}

// routableBeadIDInArgs returns the first argument bd's prefix routing can
// resolve from workDir's town, or "" when nothing in argv selects a database.
func routableBeadIDInArgs(workDir string, args []string) string {
	townRoot := FindTownRoot(workDir)
	if townRoot == "" {
		return ""
	}
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") || !IsBeadIDToken(arg) {
			continue
		}
		if GetRigPathForPrefix(townRoot, ExtractPrefix(arg)) != "" {
			return arg
		}
	}
	return ""
}

var bdBoolGlobalFlags = map[string]bool{
	"--allow-stale": true,
	"--help":        true,
	"--json":        true,
	"--profile":     true,
	"--quiet":       true,
	"--verbose":     true,
	"--version":     true,
	"-V":            true,
	"-h":            true,
	"-q":            true,
	"-v":            true,
}

var bdTargetSelectorFlags = map[string]bool{
	"--db":        true,
	"--directory": true,
	"--global":    true,
	"--sandbox":   true,
	"-C":          true,
}

// BDSubcommandIndex returns the argv index of bd's subcommand after recognized
// bd global flags. Unknown leading flags fail closed so proxy allowlists cannot
// be bypassed by treating command flags as globals.
func BDSubcommandIndex(argv []string) (int, bool) {
	if len(argv) < 2 || argv[0] != "bd" {
		return 0, false
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			return 0, false
		}
		if !strings.HasPrefix(arg, "-") {
			return i, true
		}
		if _, _, ok := strings.Cut(arg, "="); ok {
			return 0, false
		}
		if bdBoolGlobalFlags[arg] {
			continue
		}
		return 0, false
	}
	return 0, false
}

// HasBDTargetSelectorFlag reports whether argv contains bd globals that can
// override the database or working directory selected by Gas Town. The proxy
// rejects these even after the subcommand because bd accepts globals anywhere.
func HasBDTargetSelectorFlag(argv []string) bool {
	if len(argv) == 0 || argv[0] != "bd" {
		return false
	}
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			return false
		}
		name := arg
		if cut, _, ok := strings.Cut(arg, "="); ok {
			name = cut
		}
		if bdTargetSelectorFlags[name] {
			return true
		}
	}
	return false
}
