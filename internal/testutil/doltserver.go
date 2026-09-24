//go:build !windows

package testutil

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql" // required by testcontainers Dolt module
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/dolt"
)

// DoltDockerImage is the Docker image used for Dolt test containers.
// DOLT_ROOT_HOST=% tells the entrypoint to create root@'%' (available
// since Dolt 1.46.0), which lets testcontainers connect via TCP.
const DoltDockerImage = "dolthub/dolt-sql-server:2.0.7"

// doltContainerPort is the port the image serves on, and the container port
// every lookup in this file asks Docker to map.
const doltContainerPort = "3306/tcp"

// DoltTmpfsEnv opts a run out of the tmpfs data dir ("0" = keep data on the
// VM disk), for a Docker runtime that cannot mount tmpfs over the image's
// declared volume.
const DoltTmpfsEnv = "GT_TEST_DOLT_TMPFS"

// doltDataDir is the image's data directory (its declared VOLUME).
const doltDataDir = "/var/lib/dolt"

const (
	// startupAttempts is how many containers one startup may burn before its
	// failure is reported. A failed attempt is replaced rather than waited on:
	// dolt.Run's initialize (database and user creation) runs after the port
	// lookup, so a container whose lookup failed was never initialized.
	startupAttempts = 3
	// startupRetryDelay is the backoff before the second startup attempt; it
	// doubles per attempt.
	startupRetryDelay = 2 * time.Second
	// mappedPortPollInterval is the gap between mapped-port lookups.
	mappedPortPollInterval = 500 * time.Millisecond
	// mappedPortPolls is how many lookups a container that testcontainers
	// reports as running gets before its startup is called failed. Sixty at
	// 500ms is a 30s budget, counted in polls rather than read off the clock
	// so the wait is testable without real time passing.
	mappedPortPolls = 60
	// startupLogTailLines is how much container output a startup failure
	// carries into its error message.
	startupLogTailLines = 40
)

// portLookup returns the host port Docker published for a container port.
type portLookup func(ctx context.Context, containerPort string) (string, error)

// doltPortLookup returns ctr's published Dolt port.
func doltPortLookup(ctr *dolt.DoltContainer) portLookup {
	return func(ctx context.Context, containerPort string) (string, error) {
		p, err := ctr.MappedPort(ctx, containerPort)
		if err != nil {
			return "", err
		}
		return p.Port(), nil
	}
}

// portWaitSleep is time.Sleep behind a variable so the wait's tests do not
// spend its budget in real time.
var portWaitSleep = time.Sleep

// waitForMappedPort polls lookup until Docker answers with a host port for the
// container's Dolt port, returning the last lookup error once the polls run out.
//
// The lookup inspects the container's published bindings, and Docker can answer
// it with no binding for a container it reports as running and serving: the
// gates saw three tests in two packages die on `port "3306/tcp" not found`
// while a concurrent container-backed suite held the Docker VM (gt-jvve). The
// wait turns that window into a delay instead of a failed suite.
func waitForMappedPort(ctx context.Context, lookup portLookup) (string, error) {
	var lastErr error
	for poll := range mappedPortPolls {
		port, err := lookup(ctx, doltContainerPort)
		if err == nil {
			return port, nil
		}
		lastErr = err
		if poll < mappedPortPolls-1 {
			portWaitSleep(mappedPortPollInterval)
		}
	}
	return "", lastErr
}

var (
	doltCtr     *dolt.DoltContainer
	doltCtrOnce sync.Once
	doltCtrErr  error
	doltCtrPort string
	dockerOnce  sync.Once
	dockerAvail bool
)

// isDockerAvailable returns true if the Docker daemon is reachable.
// The result is cached after the first call.
// DockerTestsEnv opts the container-backed tests in. Every entry point that
// would start a Dolt/testcontainers container skips unless it is "1", so an
// ordinary `go test ./internal/cmd -run TestFoo` compiles and runs in seconds
// without touching Docker or the container-gate slot. `make test` (the
// refinery gate and the daemon's main-branch patrol) sets it; a polecat that
// wants the container tests sets it explicitly and runs under gt slot run.
// Measured 2026-09-18 (gt-pxlg): five polecats spent ~81 of 185 agent-minutes
// queued on the slot for filtered runs Go finished in 4-17s, because any run
// of a container-bearing package had to take the slot whether or not the
// filter selected a container test.
const DockerTestsEnv = "GT_TEST_DOCKER"

// DockerTestsEnabled reports whether container-backed tests may run in this
// process.
func DockerTestsEnabled() bool {
	return os.Getenv(DockerTestsEnv) == "1"
}

const dockerTestsSkipMsg = "container-backed tests are opt-in: set " + DockerTestsEnv + "=1 (make test does) and run under gt slot run"

func isDockerAvailable() bool {
	dockerOnce.Do(func() {
		dockerAvail = exec.Command("docker", "info").Run() == nil
	})
	return dockerAvail
}

// isReaperRemovingErr returns true if the error is a transient "removing"
// status from the testcontainers Ryuk reaper. This happens when a previous
// test run's reaper container is still being cleaned up by Docker.
func isReaperRemovingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "unexpected container status") &&
		strings.Contains(err.Error(), "removing")
}

func isDockerUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rootless docker not found") ||
		strings.Contains(msg, "cannot connect to the docker daemon") ||
		strings.Contains(msg, "no docker host")
}

func runDoltContainer(ctx context.Context) (ctr *dolt.DoltContainer, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("testcontainers docker unavailable: %v", r)
		}
	}()

	return dolt.Run(ctx, DoltDockerImage, doltContainerOpts()...)
}

// doltContainerOpts is every option a test Dolt container starts with. The
// data dir is tmpfs by default: each bd init DOLT_COMMITs 66 migrations, and
// on the Docker Desktop VM disk every commit's fsync reaches the host SSD
// (claude-yfj). A container's data is ~18 MB; the 2g cap bounds a runaway.
func doltContainerOpts() []testcontainers.ContainerCustomizer {
	opts := []testcontainers.ContainerCustomizer{
		// WithEnv must precede dolt.WithDatabase: dolt.WithDatabase writes
		// req.Env without a nil check, so it needs the map already set.
		testcontainers.WithEnv(map[string]string{"DOLT_ROOT_HOST": "%"}),
		dolt.WithDatabase("gt_test"),
	}
	if os.Getenv(DoltTmpfsEnv) != "0" {
		opts = append(opts, testcontainers.WithTmpfs(map[string]string{doltDataDir: "rw,size=2g"}))
	}
	return opts
}

// isPortNotMappedErr reports whether err is the inspect-backed port lookup
// answering that Docker has no binding for the container's Dolt port.
func isPortNotMappedErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found") &&
		strings.Contains(err.Error(), doltContainerPort)
}

// isRetriableContainerStartErr reports whether a failed container start is one
// a replacement attempt clears: the reaper still removing the previous
// container's status, or a port binding Docker had not published yet
// (gt-jvve).
func isRetriableContainerStartErr(err error) bool {
	return isReaperRemovingErr(err) || isPortNotMappedErr(err)
}

// settleDoltContainer waits for ctr's published port so the daemon that
// answers the lookup is the one that starts the replacement.
func settleDoltContainer(ctx context.Context, ctr *dolt.DoltContainer) {
	if ctr == nil {
		return
	}
	_, _ = waitForMappedPort(ctx, doltPortLookup(ctr))
}

// reapFailedContainer stops a container whose startup failed, so it does not
// hold a host port and its share of the shared Docker VM while the next
// attempt runs. The termination error is dropped because the startup failure
// is the one worth reporting, and the reaper collects what this misses
// (gt-p98h).
func reapFailedContainer(ctr *dolt.DoltContainer) {
	if ctr == nil {
		return
	}
	_ = testcontainers.TerminateContainer(ctr)
}

// containerLogTail returns the last startupLogTailLines of ctr's output, which
// is what a startup failure is missing when it reports only the port lookup
// that raced the container (gt-jvve).
func containerLogTail(ctx context.Context, ctr *dolt.DoltContainer) (string, error) {
	if ctr == nil {
		return "", errors.New("the failed start left no container to read")
	}
	logs, err := ctr.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = logs.Close() }()

	var lines []string
	scanner := bufio.NewScanner(logs)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(lines) > startupLogTailLines {
		lines = lines[len(lines)-startupLogTailLines:]
	}
	return strings.Join(lines, "\n"), nil
}

// withContainerLogs appends a container log tail to a startup failure. A tail
// that could not be read is named rather than dropped, so a failure that has
// no logs says so instead of looking like a failure that had none to give.
func withContainerLogs(err error, tail string, logErr error) error {
	switch {
	case logErr != nil:
		return fmt.Errorf("%w (container logs unavailable: %v)", err, logErr)
	case strings.TrimSpace(tail) == "":
		return fmt.Errorf("%w (container logs empty)", err)
	}
	return fmt.Errorf("%w\n--- container logs (last %d lines) ---\n%s", err, startupLogTailLines, tail)
}

// containerStartError attaches ctr's log tail to a startup failure whose
// container outlived it.
func containerStartError(ctx context.Context, ctr *dolt.DoltContainer, err error) error {
	tail, logErr := containerLogTail(ctx, ctr)
	return withContainerLogs(err, tail, logErr)
}

// containerStartHooks is one container start plus what the retry does with a
// failed one. Fields rather than direct calls so the wait and retry path is
// testable with a fake (gt-jvve); the production set is in
// runDoltContainerWithRetry.
type containerStartHooks struct {
	start  func(ctx context.Context) (*dolt.DoltContainer, error)
	settle func(ctx context.Context, ctr *dolt.DoltContainer)
	reap   func(ctr *dolt.DoltContainer)
	logs   func(ctx context.Context, ctr *dolt.DoltContainer) (string, error)
	sleep  func(time.Duration)
}

// retryContainerStart starts one Dolt container, retrying the failures
// isRetriableContainerStartErr names, and reports the last failure with that
// container's logs attached.
func retryContainerStart(ctx context.Context, hooks containerStartHooks) (*dolt.DoltContainer, error) {
	delay := startupRetryDelay
	var lastCtr *dolt.DoltContainer
	var lastErr error

	for attempt := range startupAttempts {
		ctr, err := hooks.start(ctx)
		if err == nil {
			return ctr, nil
		}
		lastCtr, lastErr = ctr, err
		if !isRetriableContainerStartErr(err) {
			break
		}
		if isPortNotMappedErr(err) {
			hooks.settle(ctx, ctr)
		}
		hooks.reap(ctr)
		if attempt < startupAttempts-1 {
			hooks.sleep(delay)
			delay *= 2
		}
	}

	if lastCtr == nil {
		return nil, lastErr
	}
	tail, logErr := hooks.logs(ctx, lastCtr)
	return nil, withContainerLogs(lastErr, tail, logErr)
}

// runDoltContainerWithRetry starts a Dolt container, retrying the transient
// startup failures up to startupAttempts times.
func runDoltContainerWithRetry(ctx context.Context) (*dolt.DoltContainer, error) {
	return retryContainerStart(ctx, containerStartHooks{
		start:  runDoltContainer,
		settle: settleDoltContainer,
		reap:   reapFailedContainer,
		logs:   containerLogTail,
		sleep:  time.Sleep,
	})
}

// startSharedDoltContainer starts the shared Dolt container and sets
// GT_DOLT_PORT and BEADS_DOLT_PORT process-wide.
func startSharedDoltContainer() {
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		doltCtrErr = fmt.Errorf("starting Dolt container: %w", err)
		return
	}

	p, err := waitForMappedPort(ctx, doltPortLookup(ctr))
	if err != nil {
		doltCtrErr = containerStartError(ctx, ctr, fmt.Errorf("getting mapped port: %w", err))
		_ = testcontainers.TerminateContainer(ctr)
		return
	}

	doltCtr = ctr
	doltCtrPort = p
	os.Setenv("GT_DOLT_PORT", doltCtrPort)    //nolint:tenv // intentional process-wide env
	os.Setenv("BEADS_DOLT_PORT", doltCtrPort) //nolint:tenv // intentional process-wide env
	os.Setenv("GT_TEST_EXTERNAL_DOLT", "1")   //nolint:tenv // integration tests reuse this container
	// This container is always ephemeral and test-only (never production), so
	// declare it a dedicated test server. Without this, bd refuses to connect
	// the testdb_* databases minted by isolated Init() calls (gt-uq28):
	// "set BEADS_TEST_SERVER=1 on a dedicated test server". Process-wide
	// because callers reaching this port through a non-isolated beads client
	// (e.g. refinery's Manager, which inherits os.Environ() directly) need it
	// too, not just testutil.RequireDoltContainer's direct callers.
	os.Setenv("BEADS_TEST_SERVER", "1") //nolint:tenv // intentional process-wide env
}

// StartIsolatedDoltContainer starts a per-test Dolt container and returns the
// mapped host port. GT_DOLT_PORT is set via t.Setenv (scoped to the test).
// The container is terminated automatically when the test finishes.
func StartIsolatedDoltContainer(t *testing.T) string {
	t.Helper()
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Skip("Docker not available, skipping test")
	}

	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		if isDockerUnavailableErr(err) {
			t.Skipf("Dolt container unavailable: %v", err)
		}
		t.Fatalf("starting Dolt container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			// Fail loud: a swallowed termination error is exactly how
			// containers leak and quietly exhaust the shared Docker VM
			// (gt-p98h, gt-n5g6).
			t.Errorf("terminating Dolt container: %v", err)
		}
	})

	port, err := waitForMappedPort(ctx, doltPortLookup(ctr))
	if err != nil {
		t.Fatalf("getting mapped port: %v", containerStartError(ctx, ctr, err))
	}

	portStr := port
	t.Setenv("GT_DOLT_PORT", portStr)
	t.Setenv("BEADS_TEST_SERVER", "1")
	return portStr
}

// EnsureDoltContainerForTestMain starts a shared Dolt container for use in
// TestMain functions. Call TerminateDoltContainer() after m.Run() to clean up.
// Sets both GT_DOLT_PORT and BEADS_DOLT_PORT process-wide.
//
// The only caller is StartHermetic (WithDolt), which logs the error and
// continues with the port variables left poisoned; every package that opts
// in then skips its container tests on the empty port (daemon, convoy) or
// via RequireDoltContainer (cmd). TestStartHermetic_WithDoltWithoutOptIn
// pins that contract.
func EnsureDoltContainerForTestMain() error {
	if !DockerTestsEnabled() {
		return fmt.Errorf("%s", dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		return fmt.Errorf("Docker not available")
	}

	doltCtrOnce.Do(startSharedDoltContainer)
	return doltCtrErr
}

// RequireDoltContainer ensures a shared Dolt container is running. Skips the
// test if Docker is not available.
func RequireDoltContainer(t *testing.T) {
	t.Helper()
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Skip("Docker not available, skipping test")
	}

	doltCtrOnce.Do(startSharedDoltContainer)
	if doltCtrErr != nil {
		if isDockerUnavailableErr(doltCtrErr) {
			t.Skipf("Dolt container unavailable: %v", doltCtrErr)
		}
		t.Fatalf("Dolt container setup failed: %v", doltCtrErr)
	}
}

// DoltContainerAddr returns the address (host:port) of the Dolt container.
func DoltContainerAddr() string {
	return "127.0.0.1:" + doltCtrPort
}

// DoltContainerPort returns the mapped host port of the Dolt container.
func DoltContainerPort() string {
	return doltCtrPort
}

// TerminateDoltContainer stops and removes the shared Dolt container.
// Called from TestMain after m.Run(). Returns the termination error instead
// of swallowing it: a container that fails to terminate keeps running and
// holding memory on the shared Docker VM until something notices (gt-p98h,
// gt-n5g6 - a fleet of hours-old leaked containers is exactly how the VM's
// 7.6GiB got exhausted town-wide, undetected because this used to discard
// the error).
func TerminateDoltContainer() error {
	if doltCtr == nil {
		return nil
	}
	err := testcontainers.TerminateContainer(doltCtr)
	doltCtr = nil
	return err
}
