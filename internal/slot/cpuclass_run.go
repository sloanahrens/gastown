package slot

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// defaultCPUClassNice is the niceness a CPU-class build runs at: the same
// increment `gt slot run` gives non-gate suites, so a crew build yields the
// host's cores to the daemon and bd when they want them.
const defaultCPUClassNice = 10

// defaultCPUClassP is the go parallelism the CPU class adds to GOFLAGS. Go
// sizes -p from the host's core count, so one crew build legitimately starts
// 24 compile processes; four is what a shared host can afford beside its
// neighbors.
const defaultCPUClassP = "-p=4"

// CPURunOptions is one command to run with a CPU-class slot held.
type CPURunOptions struct {
	// Role is the holder identity recorded in the slot's owner file and its
	// wait messages (CPUBuildRole's answer for a build).
	Role string
	// Timeout caps the wait for a free slot. Zero or less waits forever.
	Timeout time.Duration
	// Size is the class size; values below 1 mean DefaultCPUClassSize.
	Size int
	// Args is the command and its arguments. Leading VAR=value tokens are
	// environment assignments, as env(1) reads them (SplitEnvPrefix).
	Args []string
	// Path is the caller's own PATH, used to resolve a command that names no
	// PATH of its own.
	Path string
	// Dir is the child's working directory; "" keeps the wrapper's.
	Dir string
	// Environ reads the caller's environment, which the child inherits with
	// the CPU class's GOFLAGS adjustment applied. Nil reads os.Environ.
	Environ func() []string
	// Stdin, Stdout and Stderr are the child's. The slot's own wait and grant
	// lines go to Stderr, never Stdout: a wrapped `go test -json` must not have
	// its stream broken by a notice.
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	// gate is the gate the slot is taken from; nil means the package's own.
	gate *Gate
}

// CPUExecRequest is one command prepared for the executor: the program
// resolved to a path, the argv the caller wrote, the child's environment, and
// the priority the child must run at.
type CPUExecRequest struct {
	Program string
	Args    []string
	Env     []string
	Dir     string
	// Nice is the niceness to set before the command starts (setpriority on
	// the process that spawns it, which the child inherits; 0 leaves the
	// inherited priority).
	Nice int
	// SetPriority applies Nice; nil means setCPUClassPriority, and a test
	// injects a recorder so it never changes the test process's niceness.
	SetPriority func(int) error
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
}

// CPUExecutor runs a prepared CPU-class command and reports its exit code. A
// non-nil error means the command could not be started at all; the exit code
// is meaningful only when err is nil. The real one is runCPUExec; a test
// injects a fake (WithCPUExecutor) so no unit test starts a process.
type CPUExecutor func(CPUExecRequest) (int, error)

// RunCPU acquires a CPU-class slot for opts.Role, runs opts.Args under it, and
// reports the command's exit code. It is the engine of the cpuslot wrapper that
// the gate and presubmit recipes call from a crew checkout; the command that
// invokes it decides whether the checkout is crew (CPUBuildRole).
//
// The command is resolved before the slot is taken, so a command that cannot
// run does not hold a slot. The hold is always released before RunCPU returns,
// recording nothing else: the class is a load bound, and a build's own exit
// code is the verdict.
func RunCPU(townRoot string, opts CPURunOptions) (int, error) {
	envAssigns, cmdArgs := SplitEnvPrefix(opts.Args)
	program, err := resolveCommand(envAssigns, cmdArgs, opts.Path)
	if err != nil {
		return 0, fmt.Errorf("resolving the command: %w", err)
	}

	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	size := normalizedCPUClassSize(opts.Size)
	gate := opts.gate
	if gate == nil {
		gate = NewGate()
	}
	exec := gate.execCPU
	if exec == nil {
		exec = runCPUExec
	}

	fmt.Fprintf(stderr, "Waiting for CPU-class build slot (role=%s, size=%d)...\n", opts.Role, size)
	h, err := gate.AcquireCPU(townRoot, opts.Role, opts.Timeout, size)
	if err != nil {
		return 0, fmt.Errorf("acquiring a CPU-class build slot: %w", err)
	}
	fmt.Fprintf(stderr, "CPU-class build slot acquired (role=%s, waited %s, slot %d/%d).\n",
		opts.Role, h.WaitedFor.Round(time.Second), h.Index, size)

	environ := os.Environ
	if opts.Environ != nil {
		environ = opts.Environ
	}
	code, runErr := exec(CPUExecRequest{
		Program: program,
		Args:    cmdArgs,
		Env:     cpuClassEnv(environ(), envAssigns),
		Dir:     opts.Dir,
		Nice:    defaultCPUClassNice,
		Stdin:   opts.Stdin,
		Stdout:  opts.Stdout,
		Stderr:  stderr,
	})
	_ = h.Release()
	if runErr != nil {
		return 0, runErr
	}
	return code, nil
}

// RunCommandUnslotted runs args exactly as the shell would have, for a tree the
// CPU class does not cover: no slot, no priority change and GOFLAGS untouched,
// so the wrapper is invisible to a polecat's or the landing worker's build.
// Leading VAR=value assignments apply, and the program is resolved against the
// PATH the child will see.
func RunCommandUnslotted(args []string, pathEnv string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	envAssigns, cmdArgs := SplitEnvPrefix(args)
	program, err := resolveCommand(envAssigns, cmdArgs, pathEnv)
	if err != nil {
		return 0, err
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return runCPUExec(CPUExecRequest{
		Program: program,
		Args:    cmdArgs,
		Env:     runEnv(os.Environ(), envAssigns),
		Stdin:   stdin,
		Stdout:  stdout,
		Stderr:  stderr,
	})
}

// cpuClassEnv is the child's environment: the caller's, with the leading
// assignments applied, and GOFLAGS carrying -p=4 unless the caller already set
// -p. An unset GOFLAGS gets the value alone.
func cpuClassEnv(environ, envAssigns []string) []string {
	env := runEnv(environ, envAssigns)
	for _, entry := range env {
		v, ok := strings.CutPrefix(entry, "GOFLAGS=")
		if !ok {
			continue
		}
		if goflagsSetsP(v) {
			return env
		}
		return append(filterEnvKey(env, "GOFLAGS"), "GOFLAGS="+strings.TrimSpace(v+" "+defaultCPUClassP))
	}
	return append(env, "GOFLAGS="+defaultCPUClassP)
}

// goflagsSetsP reports whether GOFLAGS already names a -p setting.
func goflagsSetsP(v string) bool {
	for _, f := range strings.Fields(v) {
		if f == "-p" || strings.HasPrefix(f, "-p=") {
			return true
		}
	}
	return false
}

// runCPUExec is the real CPUExecutor: it sets this process's niceness to
// req.Nice (children inherit it), starts the command, forwards interrupts to it
// and reports its exit code.
//
// A priority that cannot be set is reported and the build runs anyway: nicing
// is the class's politeness toward its neighbors, not a correctness guard, and
// failing a crew's build over it would be a worse trade than one unslotted
// build.
func runCPUExec(req CPUExecRequest) (int, error) {
	if req.Nice > 0 {
		setPriority := req.SetPriority
		if setPriority == nil {
			setPriority = setCPUClassPriority
		}
		if err := setPriority(req.Nice); err != nil {
			stderr := req.Stderr
			if stderr == nil {
				stderr = io.Discard
			}
			fmt.Fprintf(stderr, "gt slot: could not lower this build's priority to nice %d (%v); running at the inherited priority\n", req.Nice, err)
		}
	}
	cmd := &exec.Cmd{
		Path:   req.Program,
		Args:   req.Args,
		Env:    req.Env,
		Dir:    req.Dir,
		Stdin:  req.Stdin,
		Stdout: req.Stdout,
		Stderr: req.Stderr,
	}

	// Same forwarding as `gt slot run` (gt-1j5rj): a wrapper killed by a signal
	// must take the build with it, or the kernel drops the slot's flock while
	// the build keeps loading the host.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	var relay <-chan struct{}
	defer func() {
		signal.Stop(sigCh)
		close(sigCh)
		if relay != nil {
			<-relay
		}
	}()

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("running %s: %w", req.Args[0], err)
	}
	relay = relayInterrupts(sigCh, cmd.Process)
	return exitResult(req.Args[0], cmd.Wait())
}

// exitResult is what a finished command reports: its exit code, or the error
// that stopped it from finishing. A child that exited reports its code; any
// other wait error — a write to the caller's own stream that failed, a wait the
// kernel refused — is the wrapper's failure, and the build must not read as
// run.
func exitResult(program string, waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 0, fmt.Errorf("running %s: %w", program, waitErr)
}
