package slot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultRunTimeout is how long a run waits for the slot when its caller does
// not cap the wait: the default of `gt slot run --timeout`.
const DefaultRunTimeout = 60 * time.Minute

// acquiredFormat is the line every holder gets the moment it holds the slot.
// Script plugins outside Go read it to tell a command that never ran from one
// that ran and failed — the rebuild plugin greps for it to decide between
// deferring (nothing was built) and escalating a failure — so it is a string
// to keep stable; TestAcquiredFormat pins it (gt-kox0).
const acquiredFormat = "Container-gate slot acquired (role=%s, waited %s, slot %d/%d).\n"

// RunOptions is one command to run with the container-gate slot held.
type RunOptions struct {
	// Role is the holder identity: it is recorded in the owner file and the
	// slot history, and it scopes the reentrant fast path (see Acquire).
	// Callers working on one item pass one stable role; RunRole resolves the
	// CLI's --role default.
	Role string
	// Timeout caps the wait for a free slot. Zero or less waits forever.
	Timeout time.Duration
	// Pool is the town's container-gate pool.
	Pool Pool
	// Nice is the niceness the command runs at. A negative value resolves
	// from Role (gate roles run at normal priority, everyone else at
	// defaultNonGateNice); 0 disables the nice(1) wrapper.
	Nice int
	// Args is the command and its arguments. Leading VAR=value tokens are
	// environment assignments, as env(1) reads them (SplitEnvPrefix).
	Args []string
	// Path is the caller's own PATH. A command naming no PATH of its own is
	// resolved against it, and the child runs under it.
	Path string
	// Env reads the caller's environment, which the child inherits. It is
	// read after the slot is taken, so the child carries the reentrant marker
	// the hold armed (ReentrantEnvVar) and a wrap nested in it rides the hold
	// (gt-tuiy).
	Env func() []string
	// Stdin, Stdout and Stderr are the child's, and the acquire line is
	// written to Stdout. A nil Stdout or Stderr discards that stream.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// gate is the gate the hold is taken from; nil means the package's own.
	// A test injects one whose runtime never reaches the host's docker CLI,
	// where a stray gate container on a shared host would stall the acquire
	// for the whole timeout.
	gate *Gate
}

// Run acquires the container-gate slot for opts.Role, runs opts.Args under it,
// and reports the command's exit code. It is the engine of `gt slot run`; the
// cobra command that calls it is a thin wrapper that resolves the town and
// exits with the code.
//
// A non-nil error means the command could not be validated or started at all;
// the exit code is meaningful only when err is nil. The hold is always
// released before Run returns, recording the command's exit status on the way
// (gt-dc81) — a failing suite is the one an operator is most likely to look
// up, and a release skipped by os.Exit would leave its hold open forever.
func Run(townRoot string, opts RunOptions) (int, error) {
	// Resolve and validate the command before taking the slot: the gate is the
	// merge path's critical section, and a command that cannot run must not hold
	// it (gt-f4xe). Leading VAR=value tokens are env(1) assignments, so the
	// program is looked up under the PATH the child will see (gt-18nx).
	envAssigns, cmdArgs := SplitEnvPrefix(opts.Args)
	program, err := resolveCommand(envAssigns, cmdArgs, opts.Path)
	if err != nil {
		return 0, fmt.Errorf("gt slot run: %w", err)
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = io.Discard
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	fmt.Fprintf(stdout, "Waiting for container-gate slot (role=%s)...\n", opts.Role)
	pool := opts.Pool.normalized()
	gate := opts.gate
	if gate == nil {
		gate = NewGate()
	}
	h, err := gate.AcquirePool(townRoot, opts.Role, opts.Timeout, pool)
	if err != nil {
		return 0, fmt.Errorf("acquiring container-gate slot: %w", err)
	}
	// The wait is reported even when it was negligible: a queued invocation and
	// the one it queued behind only read as a pair (gt-dc81).
	fmt.Fprintf(stdout, acquiredFormat,
		opts.Role, h.WaitedFor.Round(time.Second), h.Index, pool.Slots)

	// A polecat's own suite is optional verification where a gate-class holder
	// is the merge path's critical section, and three concurrent suites
	// tripled the merge gate (gt-93m1) — so non-gate holders run under
	// nice(1) unless --nice says otherwise.
	niceness := runNiceness(opts.Role, opts.Nice)
	if niceness > 0 {
		fmt.Fprintf(stdout, "Running at nice %d (non-gate holder; --nice 0 to disable).\n", niceness)
	}
	sub := childCommand(program, cmdArgs, niceWrapper(niceness))
	if len(envAssigns) > 0 {
		sub.Env = runEnv(opts.Env(), envAssigns)
	}
	sub.Stdin = opts.Stdin
	sub.Stdout = stdout
	sub.Stderr = stderr

	// Forward to the child every signal that would otherwise end this process
	// — SIGINT today, and SIGTERM and SIGHUP, which is what a daemon or tmux
	// kill of the wrapper sends. The child shuts its containers down on SIGINT;
	// a wrapper that died under them instead would hand the kernel back the
	// flock while the suite it started kept running with no slot held, and the
	// next acquirer would be granted the slot the gate exists to protect
	// (gt-1j5rj). The hold is released by the ReleaseWithExit below, after the
	// child has exited, or by the kernel if this process is killed outright.
	//
	// Delivery is registered before the child starts — so a signal arriving
	// first waits in the channel rather than ending the process — while the
	// relay itself starts with the child, and so reads a process that exists.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	var relay <-chan struct{}
	defer func() {
		// Stop delivery before closing: Stop returns only once the signal
		// package's delivery has quiesced, and a delivery to a closed channel
		// panics. A relay that was started has exited by the time this returns.
		signal.Stop(sigCh)
		close(sigCh)
		if relay != nil {
			<-relay
		}
	}()

	// Start and Wait rather than Run, so the relay can be handed the child's
	// process — reading it from the child while Run sets it is a data race.
	if err := sub.Start(); err != nil {
		_ = h.Release()
		return 0, fmt.Errorf("running %s: %w", cmdArgs[0], err)
	}
	relay = relayInterrupts(sigCh, sub.Process)

	runErr := sub.Wait()
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code := exitErr.ExitCode()
			_ = h.ReleaseWithExit(code)
			return code, nil
		}
		_ = h.Release()
		return 0, fmt.Errorf("running %s: %w", cmdArgs[0], runErr)
	}
	_ = h.ReleaseWithExit(0)
	return 0, nil
}

// relayInterrupts forwards every signal received on sigCh to child as an
// interrupt — the signal a wrapped suite shuts its containers down with — and
// reports the channel it has finished on. The caller ends the relay by closing
// sigCh.
func relayInterrupts(sigCh <-chan os.Signal, child *os.Process) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range sigCh {
			_ = child.Signal(os.Interrupt)
		}
	}()
	return done
}

// RunRole is the role `gt slot run` runs under when --role is omitted. Split
// out from Run so it's testable without the full workspace/exec harness.
//
// An explicit flagRole always wins, unchanged. An omitted one usually means
// a nested wrap that has no way to know its ancestor's exact role string
// (gt-cet2, gt-tuiy): default to riding that ancestor's hold via
// InheritedRole instead of a per-invocation placeholder that is
// guaranteed to mismatch it and contend for the full --timeout — the
// deadlock class gt-tuiy exists to prevent. A top-level invocation with no
// ancestor hold at all still falls back to the old unique-pid role, so two
// unrelated unnamed invocations never collide with each other.
func RunRole(flagRole, townRoot string, inheritedRole func(townRoot string) (string, bool)) string {
	if flagRole != "" {
		return flagRole
	}
	if inherited, ok := inheritedRole(townRoot); ok {
		return inherited
	}
	return fmt.Sprintf("pid-%d", os.Getpid())
}

// envAssignmentRe matches a leading environment assignment token as env(1)
// accepts it: an identifier, "=", and any value (possibly empty).
var envAssignmentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// SplitEnvPrefix peels leading VAR=value tokens off args, returning them and
// the remaining command. A token that merely contains "=" later in the word
// (e.g. "--flag=value") is part of the command, not an assignment; splitting
// stops at the first non-assignment token.
func SplitEnvPrefix(args []string) (envAssigns, cmdArgs []string) {
	i := 0
	for i < len(args) && envAssignmentRe.MatchString(args[i]) {
		i++
	}
	return args[:i], args[i:]
}

// resolveCommand resolves the program Run will exec, rejecting a command it
// must not take a gate slot for: one naming no program at all (only VAR=value
// assignments), or one whose program cannot be exec'd. It is called before
// AcquirePool so the refusal costs nothing (gt-f4xe). ambientPath is the
// caller's own PATH.
func resolveCommand(envAssigns, cmdArgs []string, ambientPath string) (string, error) {
	if len(cmdArgs) == 0 {
		return "", fmt.Errorf("no command after environment assignment(s) %v", envAssigns)
	}
	return lookPathForSlot(cmdArgs[0], childPath(envAssigns, ambientPath))
}

// childPath is the PATH the child is run with, and so the one its program is
// resolved against: the assigned PATH= when the leading assignments set one,
// ambientPath otherwise. runEnv builds the child's environment out of those
// same assignments, so this is the PATH the child — and the nice(1) wrapper
// resolving the program inside it — searches.
func childPath(envAssigns []string, ambientPath string) string {
	for i := len(envAssigns) - 1; i >= 0; i-- {
		if path, ok := strings.CutPrefix(envAssigns[i], "PATH="); ok {
			return path
		}
	}
	return ambientPath
}

// lookPathForSlot resolves name against an explicit PATH the way exec.LookPath
// resolves against the ambient one, including an entry naming the current
// directory — empty or "." — and a relative result refused with exec.ErrDot.
// exec.LookPath offers no way to substitute a PATH but does resolve a name
// containing a separator directly, so joining each entry and delegating keeps
// exec.LookPath's own answer to "is this an executable file".
func lookPathForSlot(name, pathEnv string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		resolved, err := exec.LookPath(name)
		if err != nil {
			// Deliberately not exec.LookPath's own error: it reads well for a
			// bare name and poorly for a path, where it blames a $PATH that was
			// never consulted.
			return "", fmt.Errorf("not an executable file: %s", name)
		}
		return resolved, nil
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		candidate := filepath.Join(dir, name)
		if !strings.ContainsRune(candidate, os.PathSeparator) {
			// The entry named the current directory and Join cleaned it away:
			// what is left is a bare name, which exec.LookPath reads as one to
			// search the ambient PATH for, so the entry would resolve gt's own
			// PATH instead (gt-f4xe, gt-h9wh). Writing "./name" holds the
			// search in the entry, and the relative result the ErrDot check
			// below refuses is then this entry's file, not another's.
			candidate = "." + string(os.PathSeparator) + name
		}
		resolved, err := exec.LookPath(candidate)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(resolved) {
			// A candidate always carries a separator, so this resolution is
			// exec.LookPath's path branch: it answers only "is this an
			// executable file", never the ErrDot check its bare-name branch
			// applies. What would run is whatever the working directory holds
			// under this name at exec time (gt-8p4f).
			return "", &exec.Error{Name: name, Err: exec.ErrDot}
		}
		return resolved, nil
	}
	return "", fmt.Errorf("executable file not found in $PATH: %s", name)
}

// defaultNonGateNice is the nice(1) increment for non-gate slot holders.
const defaultNonGateNice = 10

// runNiceness resolves the niceness for a holder: an explicit --nice wins;
// otherwise gate-class roles (IsGateRole) run at normal priority and everyone
// else at defaultNonGateNice.
func runNiceness(role string, flag int) int {
	if flag >= 0 {
		return flag
	}
	if IsGateRole(role) {
		return 0
	}
	return defaultNonGateNice
}

// niceWrapper is the nice(1) prefix for a holder at the given niceness: nil
// when niceness is 0 or no nice binary exists, in which case the caller runs
// the command itself.
func niceWrapper(niceness int) []string {
	if niceness <= 0 {
		return nil
	}
	nicePath, err := exec.LookPath("nice")
	if err != nil {
		return nil
	}
	return []string{nicePath, "-n", strconv.Itoa(niceness)}
}

// childCommand builds the child process for a command resolveCommand has
// already validated, wrapping it in nice(1) when wrapper is non-empty.
// Without a wrapper it execs the resolved program by path: exec.Command would
// resolve a bare argv[0] against gt's own PATH, where the child's assigned
// PATH does not apply, so validation and the exec would disagree (gt-f4xe).
func childCommand(program string, cmdArgs, wrapper []string) *exec.Cmd {
	argv := append(append([]string(nil), wrapper...), cmdArgs...)
	if len(wrapper) == 0 {
		// Args[0] stays the operator's own token; Path is what runs.
		return &exec.Cmd{Path: program, Args: argv}
	}
	// With a wrapper, nice(1) is argv[0] and resolves the program in the
	// child, under the PATH that resolveCommand validated against.
	return exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: args come from the operator's own CLI invocation
}

// runEnv is the environment for the slot's child: environ (this process's
// own), with the operator's leading VAR=value assignments applied over it.
//
// An assigned key's inherited entry is removed rather than left beside the
// assignment, so the child reads the operator's value by construction instead
// of because os/exec dedups the slice it is handed and keeps the last entry.
// That dedup is a rule of the exec path, not of the environment, and this repo
// does not otherwise lean on it (gt-g7ym).
//
// A repeated assignment settles on the last one, as env(1) leaves it.
func runEnv(environ, envAssigns []string) []string {
	env := append([]string(nil), environ...)
	for _, kv := range envAssigns {
		key, _, _ := strings.Cut(kv, "=")
		env = filterEnvKey(env, key)
		env = append(env, kv)
	}
	return env
}

// filterEnvKey removes every entry for key from env.
func filterEnvKey(env []string, key string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if k, _, ok := strings.Cut(entry, "="); ok && k == key {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}
