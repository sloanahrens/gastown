package slot

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
)

// ContainerRuntime is the Docker surface the gate needs: the gate-container
// listing Acquire, Status and Reap judge, the removal Reap and Acquire's
// orphan sweep perform, and the VM capacity `gt doctor` reports. The real
// implementation shells out to the docker CLI (DockerRuntime); a test hands a
// Gate a fake instead, so no unit test reaches the host's docker.
type ContainerRuntime interface {
	// List returns the raw `docker ps` lines (one docker JSON record each, see
	// dockerPSFormat) for every running container matching
	// gateContainerPatterns. A non-nil error means the check could not be
	// performed and must be read as "unknown", never as "no containers
	// running", except the isDaemonUnreachable case Acquire distinguishes.
	List() ([]string, error)
	// Remove force-removes one container by id.
	Remove(id string) error
	// Info reports the Docker VM's CPU and memory bound.
	Info() (VMInfo, error)
}

// VMInfo is the Docker VM's CPU/memory bound, as `docker info` reports it.
type VMInfo struct {
	NCPU     int
	MemBytes int64
}

// DockerRuntime returns the ContainerRuntime backed by the host's docker CLI.
func DockerRuntime() ContainerRuntime { return dockerCLI{} }

// dockerCLI is the real ContainerRuntime.
type dockerCLI struct{}

// List runs `docker ps`, bounded by dockerPSTimeout.
func (dockerCLI) List() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dockerPSTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "--format", dockerPSFormat).Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) {
			// docker binary itself is missing — this host can never run a
			// container-backed suite, so there is nothing to detect. This is
			// distinct from an *exec.ExitError (docker installed but the
			// daemon is unreachable), which IS treated as unknown below.
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("docker ps did not respond within %s: %w", dockerPSTimeout, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			// Fold the docker CLI's own stderr into the error text so
			// isDaemonUnreachable can tell "daemon refused the connection"
			// (safe to infer nothing is running) apart from other exit
			// failures like a permission-denied socket (not safe to infer
			// anything) — exec.ExitError.Error() alone is just "exit status
			// N" and loses that distinction.
			return nil, fmt.Errorf("docker ps failed: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, err
	}
	return matchGateContainers(string(out)), nil
}

// Remove runs `docker rm -f`, bounded by dockerRmTimeout.
func (dockerCLI) Remove(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), dockerRmTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "rm", "-f", id).CombinedOutput() //nolint:gosec // G204: fixed args, id comes from docker's own listing
	if err != nil {
		if msg := strings.TrimSpace(string(out)); msg != "" {
			return errors.New(msg)
		}
		return err
	}
	return nil
}

// Info runs `docker info` for the VM's vCPU count and memory total.
func (dockerCLI) Info() (VMInfo, error) {
	out, err := exec.Command("docker", "info", "--format", "{{.NCPU}} {{.MemTotal}}").Output() //nolint:gosec // G204: fixed args, no user input
	if err != nil {
		return VMInfo{}, err
	}
	return parseVMInfo(string(out))
}

// parseVMInfo parses `docker info --format "{{.NCPU}} {{.MemTotal}}"` output.
func parseVMInfo(out string) (VMInfo, error) {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) != 2 {
		return VMInfo{}, fmt.Errorf("unexpected docker info output: %q", out)
	}
	ncpu, err := strconv.Atoi(fields[0])
	if err != nil {
		return VMInfo{}, fmt.Errorf("parsing NCPU: %w", err)
	}
	memBytes, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return VMInfo{}, fmt.Errorf("parsing MemTotal: %w", err)
	}
	return VMInfo{NCPU: ncpu, MemBytes: memBytes}, nil
}

// defaultRuntime is the runtime the package-level functions (Acquire, Status,
// Reap, ...) use. It is replaced only by the deprecated
// Set*ForTest shims below, for the tests of packages that have not yet moved
// to injecting a Gate.
var defaultRuntime = DockerRuntime()

// legacyRuntime reads defaultRuntime at every call, so a shim that swaps it
// while a package-level call is in flight is seen exactly as the old
// package-variable stubs were.
type legacyRuntime struct{}

func (legacyRuntime) List() ([]string, error) { return defaultRuntime.List() }
func (legacyRuntime) Remove(id string) error  { return defaultRuntime.Remove(id) }
func (legacyRuntime) Info() (VMInfo, error)   { return defaultRuntime.Info() }

// funcRuntime overrides some of a base runtime's methods.
type funcRuntime struct {
	base   ContainerRuntime
	list   func() ([]string, error)
	remove func(id string) error
}

func (r funcRuntime) List() ([]string, error) {
	if r.list != nil {
		return r.list()
	}
	return r.base.List()
}

func (r funcRuntime) Remove(id string) error {
	if r.remove != nil {
		return r.remove(id)
	}
	return r.base.Remove(id)
}

func (r funcRuntime) Info() (VMInfo, error) { return r.base.Info() }

// SetContainerListerForTest overrides the listing the package-level functions
// use, for tests in OTHER packages that drive Acquire through their own code
// (internal/cmd's batch-slot tests via acquireBatchGateSlot, the daemon's
// main-branch runner, the witness) and must never shell out to the real
// docker CLI: a stray dolt/testcontainers/ryuk container on a shared Gas Town
// host would otherwise make Acquire poll for the full batchSlotTimeout and
// hang the whole test binary (gt-tuiy attempt 4, CRITICAL). Returns a restore
// func the caller must invoke (typically via t.Cleanup) — the override is
// process-wide state shared by every test in the binary.
//
// Each call replaces the whole default runtime and its restore puts back the
// runtime it replaced, so overrides nest: restore them in reverse order (LIFO,
// as t.Cleanup does), or an earlier restore resurrects a later override.
//
// Deprecated: inject a runtime with NewGate(WithRuntime(...)) instead. Kept
// until the packages above convert to the unit-test rules.
func SetContainerListerForTest(fn func() ([]string, error)) (restore func()) {
	prev := defaultRuntime
	defaultRuntime = funcRuntime{base: prev, list: fn}
	return func() { defaultRuntime = prev }
}

// SetContainerRemoverForTest overrides the removal the package-level
// functions use, for tests in other packages that drive Reap and must not
// delete a real container. Returns a restore func the caller must invoke
// (typically via t.Cleanup). Like SetContainerListerForTest, it replaces the
// whole default runtime, so restores must run in reverse order (LIFO).
//
// Deprecated: inject a runtime with NewGate(WithRuntime(...)) instead. Kept
// until internal/doctor converts to the unit-test rules.
func SetContainerRemoverForTest(fn func(id string) error) (restore func()) {
	prev := defaultRuntime
	defaultRuntime = funcRuntime{base: prev, remove: fn}
	return func() { defaultRuntime = prev }
}

// procEnv is the process environment the reentrant marker (ReentrantEnvVar)
// travels through: a holder arms it for the processes it spawns, and a
// descendant reads what it inherited.
type procEnv interface {
	Getenv(key string) string
	Setenv(key, value string)
	Unsetenv(key string)
}

// osEnv is the real process environment.
type osEnv struct{}

func (osEnv) Getenv(key string) string { return os.Getenv(key) }

// Setenv and Unsetenv mutate the process environment on purpose: the marker is
// a wire format to descendants, which inherit it at fork(2) from whatever a
// holder's children are spawned with — `gt slot run`, the daemon's gate
// commands, and every other spawn site pass the default environment. Handing
// the marker to each spawn site explicitly would change who inherits it.
func (osEnv) Setenv(key, value string) {
	//testpolicy:allow prod-no-setenv — the reentrant marker is inherited by every child a holder spawns (see ReentrantEnvVar)
	_ = os.Setenv(key, value)
}

func (osEnv) Unsetenv(key string) {
	//testpolicy:allow prod-no-setenv — a released holder must stop handing the reentrant marker to children it spawns next
	_ = os.Unsetenv(key)
}

// ownerProbe is how the gate judges a container's owner labels against this
// host's process table (see owner_labels.go).
type ownerProbe struct {
	// hostname is the host the owner labels are compared against.
	hostname func() (string, error)
	// gone reports whether no process with this pid exists on this host. It
	// must answer true only when that is certain: a true answer licenses
	// deleting the pid's containers (see processGone).
	gone func(pid int) bool
	// startToken reads a live pid's start time; ok is false when it cannot
	// be read (see processStartToken).
	startToken func(pid int) (string, bool)
}

// hostOwnerProbe is the real probe.
func hostOwnerProbe() ownerProbe {
	return ownerProbe{hostname: os.Hostname, gone: processGone, startToken: processStartToken}
}

// Gate is the container-gate slot with its collaborators injected: the Docker
// runtime it checks, the clock its waits run on, and the poll interval between
// attempts. The package-level functions (Acquire, Status, Reap, ...) are a
// Gate with every default: the docker CLI, the real clock, DefaultPollInterval,
// the process environment, and stderr for diagnostics.
type Gate struct {
	runtime      ContainerRuntime
	clock        clockwork.Clock
	pollInterval time.Duration

	// env carries the reentrant marker; pid is this process's id as the
	// marker and the owner files record it.
	env procEnv
	pid int

	owner ownerProbe

	// probeOut carries the gate's `docker ps` and telemetry diagnostics, and
	// debrisOut its verdicts on containers it walks past or removes.
	probeOut  io.Writer
	debrisOut io.Writer
}

// Option configures a Gate.
type Option func(*Gate)

// WithRuntime sets the Docker runtime the gate checks.
func WithRuntime(r ContainerRuntime) Option { return func(g *Gate) { g.runtime = r } }

// WithClock sets the clock the gate's waits and timestamps run on.
func WithClock(c clockwork.Clock) Option { return func(g *Gate) { g.clock = c } }

// WithPollInterval sets how often a timed Acquire retries after a blocked
// pass. Values <= 0 keep DefaultPollInterval.
func WithPollInterval(d time.Duration) Option {
	return func(g *Gate) {
		if d > 0 {
			g.pollInterval = d
		}
	}
}

// NewGate returns a Gate with every default, overridden by opts.
func NewGate(opts ...Option) *Gate {
	g := &Gate{
		runtime:      legacyRuntime{},
		clock:        clockwork.NewRealClock(),
		pollInterval: DefaultPollInterval,
		env:          osEnv{},
		pid:          os.Getpid(),
		owner:        hostOwnerProbe(),
		probeOut:     os.Stderr,
		debrisOut:    os.Stderr,
	}
	for _, opt := range opts {
		opt(g)
	}
	return g
}
