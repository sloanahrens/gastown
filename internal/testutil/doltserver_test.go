//go:build !windows

package testutil

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go/modules/dolt"
)

func TestIsDockerUnavailableErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "rootless", err: errors.New("testcontainers docker unavailable: rootless Docker not found"), want: true},
		{name: "daemon", err: errors.New("Cannot connect to the Docker daemon at unix:///var/run/docker.sock"), want: true},
		{name: "ordinary", err: errors.New("pulling image failed"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDockerUnavailableErr(tt.err); got != tt.want {
				t.Fatalf("isDockerUnavailableErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// portNotMappedErr is the startup failure gt-jvve is about: the container is up
// and serving, but Docker's inspect of its published bindings has no port.
func portNotMappedErr() error {
	return fmt.Errorf("starting Dolt container: initialize: port %q not found", doltContainerPort)
}

// stubPortWaitSleep replaces the wait's sleep for one test, so a test can run
// the whole poll budget without spending it.
func stubPortWaitSleep(sleep func(time.Duration)) func() {
	previous := portWaitSleep
	portWaitSleep = sleep
	return func() { portWaitSleep = previous }
}

// A container that answers on the first lookup is not delayed at all.
func TestWaitForMappedPortImmediate(t *testing.T) {
	sleeps := 0
	defer stubPortWaitSleep(func(time.Duration) { sleeps++ })()

	lookups := 0
	port, err := waitForMappedPort(context.Background(), func(context.Context, string) (string, error) {
		lookups++
		return "55015", nil
	})
	if err != nil || port != "55015" {
		t.Fatalf("waitForMappedPort = (%q, %v), want (55015, nil)", port, err)
	}
	if lookups != 1 || sleeps != 0 {
		t.Fatalf("lookups = %d, sleeps = %d, want 1 lookup and no sleep", lookups, sleeps)
	}
}

// The lookup failing for a few polls and then answering is the race the wait
// exists for: the startup must ride it out.
func TestWaitForMappedPortWaitsOutTransientLookups(t *testing.T) {
	sleeps := 0
	defer stubPortWaitSleep(func(time.Duration) { sleeps++ })()

	const transient = 3
	lookups := 0
	port, err := waitForMappedPort(context.Background(), func(_ context.Context, containerPort string) (string, error) {
		lookups++
		if lookups <= transient {
			return "", fmt.Errorf("initialize: port %q not found", containerPort)
		}
		return "55016", nil
	})
	if err != nil || port != "55016" {
		t.Fatalf("waitForMappedPort = (%q, %v), want (55016, nil)", port, err)
	}
	if lookups != transient+1 || sleeps != transient {
		t.Fatalf("lookups = %d, sleeps = %d, want %d and %d", lookups, sleeps, transient+1, transient)
	}
}

// A port that never appears fails the wait at the budget, reporting the lookup
// error itself rather than "timed out".
func TestWaitForMappedPortGivesUpAtBudget(t *testing.T) {
	sleeps := 0
	defer stubPortWaitSleep(func(time.Duration) { sleeps++ })()

	lookups := 0
	port, err := waitForMappedPort(context.Background(), func(_ context.Context, containerPort string) (string, error) {
		lookups++
		return "", fmt.Errorf("port %q not found", containerPort)
	})
	if port != "" {
		t.Fatalf("port = %q, want empty", port)
	}
	if err == nil || !strings.Contains(err.Error(), `port "3306/tcp" not found`) {
		t.Fatalf("err = %v, want the last lookup error", err)
	}
	if lookups != mappedPortPolls || sleeps != mappedPortPolls-1 {
		t.Fatalf("lookups = %d, sleeps = %d, want %d and %d", lookups, sleeps, mappedPortPolls, mappedPortPolls-1)
	}
}

// fakeContainerStart scripts container starts and records what the retry does
// with each failed one, so no Docker is involved.
type fakeContainerStart struct {
	t        *testing.T
	ctr      *dolt.DoltContainer
	failures []error // one per start, in order; nil is a successful start
	logTail  string
	logErr   error

	attempts int
	settles  int
	reaps    int
	sleeps   []time.Duration
}

func (f *fakeContainerStart) hooks() containerStartHooks {
	return containerStartHooks{
		start: func(context.Context) (*dolt.DoltContainer, error) {
			if f.attempts >= len(f.failures) {
				f.t.Fatalf("start called %d times, %d scripted", f.attempts+1, len(f.failures))
			}
			err := f.failures[f.attempts]
			f.attempts++
			return f.ctr, err
		},
		settle: func(context.Context, *dolt.DoltContainer) { f.settles++ },
		reap:   func(*dolt.DoltContainer) { f.reaps++ },
		logs:   func(context.Context, *dolt.DoltContainer) (string, error) { return f.logTail, f.logErr },
		sleep:  func(d time.Duration) { f.sleeps = append(f.sleeps, d) },
	}
}

func TestRetryContainerStartSucceedsFirstAttempt(t *testing.T) {
	f := &fakeContainerStart{t: t, ctr: &dolt.DoltContainer{}, failures: []error{nil}}

	ctr, err := retryContainerStart(context.Background(), f.hooks())
	if err != nil || ctr != f.ctr {
		t.Fatalf("retryContainerStart = (%v, %v), want the started container and no error", ctr, err)
	}
	if f.attempts != 1 || f.settles != 0 || f.reaps != 0 || len(f.sleeps) != 0 {
		t.Fatalf("attempts = %d, settles = %d, reaps = %d, sleeps = %v; want a single clean start",
			f.attempts, f.settles, f.reaps, f.sleeps)
	}
}

// A port that was not mapped yet is waited out and retried, and the container
// the failed attempt left behind is reaped rather than leaked.
func TestRetryContainerStartRetriesUnmappedPort(t *testing.T) {
	f := &fakeContainerStart{t: t, ctr: &dolt.DoltContainer{}, failures: []error{portNotMappedErr(), nil}}

	ctr, err := retryContainerStart(context.Background(), f.hooks())
	if err != nil || ctr != f.ctr {
		t.Fatalf("retryContainerStart = (%v, %v), want the replacement container and no error", ctr, err)
	}
	if f.attempts != 2 || f.settles != 1 || f.reaps != 1 {
		t.Fatalf("attempts = %d, settles = %d, reaps = %d, want 2, 1, 1", f.attempts, f.settles, f.reaps)
	}
	if len(f.sleeps) != 1 || f.sleeps[0] != startupRetryDelay {
		t.Fatalf("sleeps = %v, want [%s]", f.sleeps, startupRetryDelay)
	}
}

// The reaper still removing the previous container is retried too, and needs no
// port wait: that failure says nothing about this container's bindings.
func TestRetryContainerStartRetriesReaperRemoving(t *testing.T) {
	reaperErr := errors.New("unexpected container status \"removing\"")
	f := &fakeContainerStart{t: t, ctr: &dolt.DoltContainer{}, failures: []error{reaperErr, nil}}

	if _, err := retryContainerStart(context.Background(), f.hooks()); err != nil {
		t.Fatalf("retryContainerStart: %v", err)
	}
	if f.attempts != 2 || f.settles != 0 || f.reaps != 1 {
		t.Fatalf("attempts = %d, settles = %d, reaps = %d, want 2, 0, 1", f.attempts, f.settles, f.reaps)
	}
}

// The retry is bounded, and when it runs out the failure names the container's
// own output instead of ending at the port lookup.
func TestRetryContainerStartExhaustedReportsLogs(t *testing.T) {
	f := &fakeContainerStart{
		t:        t,
		ctr:      &dolt.DoltContainer{},
		failures: []error{portNotMappedErr(), portNotMappedErr(), portNotMappedErr()},
		logTail:  "Server ready. Accepting connections.",
	}

	ctr, err := retryContainerStart(context.Background(), f.hooks())
	if ctr != nil {
		t.Fatalf("ctr = %v, want nil after every attempt failed", ctr)
	}
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want the startup failure", err)
	}
	if !strings.Contains(err.Error(), "Server ready. Accepting connections.") {
		t.Fatalf("err = %v, want the container log tail attached", err)
	}
	if f.attempts != startupAttempts || f.settles != startupAttempts || f.reaps != startupAttempts {
		t.Fatalf("attempts = %d, settles = %d, reaps = %d, want %d of each",
			f.attempts, f.settles, f.reaps, startupAttempts)
	}
	if len(f.sleeps) != startupAttempts-1 || f.sleeps[0] != startupRetryDelay || f.sleeps[1] != 2*startupRetryDelay {
		t.Fatalf("sleeps = %v, want [%s %s]", f.sleeps, startupRetryDelay, 2*startupRetryDelay)
	}
}

// An error the retry does not recognise is reported at once, still with the
// container's logs.
func TestRetryContainerStartDoesNotRetryOtherFailures(t *testing.T) {
	f := &fakeContainerStart{
		t:        t,
		ctr:      &dolt.DoltContainer{},
		failures: []error{errors.New("pull access denied for dolthub/dolt-sql-server")},
		logTail:  "no matching manifest",
	}

	_, err := retryContainerStart(context.Background(), f.hooks())
	if err == nil || !strings.Contains(err.Error(), "pull access denied") ||
		!strings.Contains(err.Error(), "no matching manifest") {
		t.Fatalf("err = %v, want the pull failure with its logs", err)
	}
	if f.attempts != 1 || f.reaps != 0 || len(f.sleeps) != 0 {
		t.Fatalf("attempts = %d, reaps = %d, sleeps = %v, want one attempt", f.attempts, f.reaps, f.sleeps)
	}
}

// A failure that never produced a container (Docker unreachable) is reported
// verbatim: there is no log to read, and the callers match on this text to skip
// rather than fail.
func TestRetryContainerStartReportsStartFailureWithoutContainer(t *testing.T) {
	dockerErr := errors.New("testcontainers docker unavailable: rootless Docker not found")
	f := &fakeContainerStart{t: t, failures: []error{dockerErr}}

	_, err := retryContainerStart(context.Background(), f.hooks())
	if !errors.Is(err, dockerErr) {
		t.Fatalf("err = %v, want %v", err, dockerErr)
	}
	if err.Error() != dockerErr.Error() {
		t.Fatalf("err = %q, want it unchanged", err)
	}
}

func TestWithContainerLogs(t *testing.T) {
	base := portNotMappedErr()
	tests := []struct {
		name    string
		tail    string
		logErr  error
		want    []string
		notWant string
	}{
		{
			name: "tail attached",
			tail: "Server ready. Accepting connections.",
			want: []string{`port "3306/tcp" not found`, "container logs", "Server ready"},
		},
		{
			name:   "logs unavailable",
			logErr: errors.New("no such container"),
			want:   []string{"container logs unavailable", "no such container"},
		},
		{
			name:    "logs empty",
			tail:    "  \n\n",
			want:    []string{"container logs empty"},
			notWant: "---",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := withContainerLogs(base, tt.tail, tt.logErr)
			if !errors.Is(err, base) {
				t.Fatalf("err = %v, want it to wrap the startup failure", err)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want %q in it", err, want)
				}
			}
			if tt.notWant != "" && strings.Contains(err.Error(), tt.notWant) {
				t.Errorf("err = %v, want no %q in it", err, tt.notWant)
			}
		})
	}
}

func TestIsRetriableContainerStartErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "unmapped port", err: portNotMappedErr(), want: true},
		{name: "reaper removing", err: errors.New("unexpected container status \"removing\""), want: true},
		{name: "missing image", err: errors.New("pull access denied for dolthub/dolt-sql-server"), want: false},
		{name: "init failure", err: errors.New("initialize: error creating database gt_test: connection refused"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetriableContainerStartErr(tt.err); got != tt.want {
				t.Fatalf("isRetriableContainerStartErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
